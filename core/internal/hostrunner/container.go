package hostrunner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The ephemeral container every local hook runs in (#865), and the four
// things that make it a containment boundary rather than a `docker run`
// call.
//
// # 1. There is no other way to run a hook
//
// This runner has ONE execution path and it goes through a container.
// There is no host-bash fallback, no "degraded mode", no flag that turns
// the container off -- because the value of the boundary is exactly the
// set of cases in which it holds, and a fallback makes that set "whenever
// the daemon happened to answer". A host that cannot run a container
// cannot run a local hook, and says so at startup (see
// ProveContainerCapability) rather than at 3am in the middle of a backup.
//
// The alternative was considered and is worse in the specific way that
// matters: an operator whose hook quietly ran on the NAS itself, with the
// service account's whole filesystem visible, because the hook image had
// been garbage-collected, would have no way to notice. Every property in
// this file would be true of their deployment on paper and false in fact.
//
// # 2. The capability is PROVEN, once, before anything is served
//
// docker on PATH is not the capability. A daemon that answers `version`
// is not the capability. The capability is "bytes this process sent ran
// inside a container on this host", and the only way to ask that question
// is to require an answer nothing else can produce: the probe prints a
// marker that exists in this repository, from inside a container started
// with the SAME hardening flags a hook gets, and reports what it found
// there. core/internal/remoteexec's preflight makes the identical
// argument for the SSH side, and for the identical reason -- a
// forced-command account also exits 0 having run somebody else's program.
//
// Running the probe with the same flags is the part that is easy to get
// wrong. A probe launched with a plain `docker run` proves nothing about
// a launch that also asks for --read-only, --cap-drop ALL, a tmpfs and a
// non-root user: any one of those can be unsupported by an older daemon
// or a storage driver, and discovering it at hook time turns a
// configuration problem into a failed backup.
//
// # 3. The envelope is the one core/internal/workflowexec describes
//
// Fixed bash, --noprofile --norc, no PTY, nothing injected, the
// environment delivered as values the shell never parses, stdout and
// stderr apart with one sequence counter, `bash -n` as its own refusal.
// The container changes WHERE that happens and nothing about WHAT it is:
// the image's own entrypoint is overridden so a hook cannot end up inside
// somebody else's wrapper, and the environment travels as `--env NAME`
// with the value read from this process's own block, so no value ever
// appears in the host's process list.
//
// # 4. Termination is the container's, and it is proved
//
// Killing a hook is killing its container, by the name this runner minted
// for it: SIGTERM to the container's process, a grace period, SIGKILL,
// then a LOOK -- and the look asks the daemon whether the container still
// exists rather than whether a process does. That is strictly stronger
// than the process-group signalling this replaced: a container's cgroup
// takes everything the hook started with it, including the child that
// ignores SIGTERM and the one that changed its process group.
//
// The lease is unchanged and is why the removal is explicit: a dropped
// engine connection stops AND removes the container, so an engine that
// died mid-hook leaves no running container and no stopped one.

// ErrContainer is every refusal about running hooks in containers: no
// docker, no daemon, no image, a probe that did not come back.
var ErrContainer = errors.New("hostrunner: this host cannot run workflow hooks in containers")

// DefaultHookImage is the image a hook runs in unless an operator names
// another.
//
// A PINNED patch version of the official `bash` image, not `bash:latest`
// and not `alpine`. Three reasons, in order of how much they cost when
// they are wrong: a floating tag makes a hook's interpreter change under
// a deployment that changed nothing, which is the failure mode this
// product spends a whole preflight on avoiding; `alpine` has no bash at
// all, so the envelope's fixed interpreter would not exist; and a
// digest-only reference would be unreadable in the one place an operator
// meets it, which is the refusal telling them to pull it.
//
// It is ~15 MB and carries bash 5.2 plus busybox, which is the floor for
// a hook that does anything at all (`date`, `mkdir`, `tar`). An operator
// whose hook needs pg_dump configures their own image; that is what
// --hook-image is for.
const DefaultHookImage = "bash:5.2.37-alpine3.21"

// DefaultHookBash is where bash is inside DefaultHookImage. The official
// image installs it under /usr/local, not /bin.
const DefaultHookBash = "/usr/local/bin/bash"

// DefaultHookNetwork is the network a hook container joins.
//
// `none`, which is a product decision and the one most likely to be
// argued with. A local hook's job is to quiesce something on THIS
// machine: flush a database to disk, unmount a snapshot, touch a file.
// Nothing in that needs a network, and a hook with the default bridge can
// reach every service on the NAS's LAN -- which is the blast radius of a
// script an operator copied off a forum. A hook that genuinely needs to
// send a notification gets --hook-network, once, visibly, for the whole
// deployment.
const DefaultHookNetwork = "none"

// HookTmpfsSize is how much a hook may write to /tmp.
//
// The rootfs is read-only, so without this a hook calling mktemp fails
// with a message about a read-only filesystem that names nothing anybody
// would connect to a container flag. 64 MiB is enough for the scratch
// files a quiesce hook writes and small enough that a runaway `yes >
// /tmp/x` hits a bound instead of the host's memory: a tmpfs is RAM.
//
// Anything larger belongs in RETND_WORK_DIR, which is on disk, is
// mounted read-write, and is the directory this runner cleans up.
const HookTmpfsSize = "64m"

// HookPidsLimit bounds how many processes one hook may have at once. It
// is a guard against a fork bomb in an operator's script rather than a
// resource policy: 512 is far past anything a real hook does and far
// under what it takes to make a NAS unresponsive.
const HookPidsLimit = 512

// containerProbeMarker is the line the capability probe prints first and
// nothing else on the host can produce.
//
// It exists because "docker ran" is not the capability. A `docker` on
// PATH that is a wrapper script, an image whose entrypoint prints a
// banner and exits 0, a daemon that answers version and cannot start a
// container: all three look like success. So the question asked is "did
// MY bytes run in a container", which cannot be answered by accident.
const containerProbeMarker = "backupd-container-probe-ok"

// containerNamePrefix begins the name of every container this runner
// owns. Termination addresses a container by NAME rather than by id
// because the name is minted here, before the container exists, which
// means a launch that never reported an id is still a container this
// runner can stop.
const containerNamePrefix = "backupd-hook-"

// LabelHook, LabelRun and LabelStep mark a hook container as this
// runner's, and say which step it is running.
//
// They are what makes an operator's `docker ps` legible -- a container
// called backupd-hook-... with a run id on it, rather than an anonymous
// alpine -- and what a sweep of leftovers from a killed runner can select
// on without ever touching a container it did not create.
const (
	LabelHook = "backupd.workflow-hook"
	LabelRun  = "backupd.workflow-run"
	LabelStep = "backupd.workflow-step"

	// LabelInstance is the one label that is unique to a SINGLE launch,
	// and it is what makes termination possible rather than merely
	// legible.
	//
	// The other three describe a step; this one describes an attempt at
	// it. It goes on the container when the container is CREATED, so
	// `docker ps --filter label=...` finds this launch's container even
	// when nothing here ever observed its name being registered --
	// which is exactly the state a cancel landing inside a creation
	// leaves behind, and the state in which a container used to be able
	// to run on with nothing watching it (see Executor.reconcile).
	LabelInstance = "backupd.workflow-instance"
)

// dockerProbeTimeout bounds the preflight's own docker calls. Generous,
// because pulling nothing on a loaded NAS still means talking to a daemon
// that may be busy starting the engine's own container, and bounded
// because a preflight that hangs is a runner that never reports why.
const dockerProbeTimeout = 60 * time.Second

// dockerControlTimeout bounds the small control-plane calls made while a
// step is running or ending: kill, remove, and the existence probe.
//
// Short, and separate from dockerProbeTimeout on purpose: these run on
// the termination path, where the thing being waited for is an answer
// about a container that is already being torn down. A control call that
// hung here would hold up the refusal or the certainty rather than the
// hook.
const dockerControlTimeout = 20 * time.Second

// DockerCandidates are the paths searched, in order, when no docker
// binary is configured.
//
// PATH is deliberately not consulted, for BashCandidates' reason one
// boundary further out: PATH here would be this process's own, which a
// service manager sets, and a runner whose container runtime can change
// with a unit file edit is a runner whose capability proof is about a
// different binary from the one that runs the hook. An operator whose
// docker is elsewhere configures the path.
func DockerCandidates() []string {
	return []string{
		"/usr/bin/docker",
		"/usr/local/bin/docker",
		// Synology DSM's package manager installs the client here, and
		// it is the platform this product is most often installed on.
		"/usr/local/docker/bin/docker",
		// A developer's macOS host, so the same preflight runs on the
		// machine the tests are written on.
		"/opt/homebrew/bin/docker",
	}
}

// Bash is the interpreter a hook runs under, as the capability probe
// found it INSIDE the hook image.
//
// It describes the image rather than the host, which is the whole change
// #865 makes to this field: the host's own bash is not consulted, not
// searched for and not required, because no hook ever runs on it. What
// `status` reports is what a hook actually gets.
type Bash struct {
	// Path is the absolute path inside the image.
	Path string

	// Version is what that bash reported for itself.
	Version string
}

// Mount is one host path a hook container may see, at the same path it
// has on the host.
//
// IDENTITY mounts, always, and that is a decision about meaning rather
// than convenience: RETND_WORK_DIR, a path in a hook's log line and the
// engine's own record of where a dump landed are then all one string. A
// remapped mount would make "the hook wrote /work/dump.sql" a sentence
// nobody can act on.
type Mount struct {
	// Path is the absolute host path, which is also the container path.
	Path string

	// ReadOnly is the default for an operator-configured mount.
	ReadOnly bool
}

// ParseMount reads one --hook-mount value: an absolute path, optionally
// suffixed with :ro or :rw. A bare path is read-only.
//
// Read-only by default because the common hook reads its source tree and
// the destructive case is worth typing. The refusals below are the whole
// policy surface an operator can get wrong, so each one says what is
// wrong rather than that something is.
func ParseMount(spec string) (Mount, error) {
	path, mode := spec, ""
	if i := strings.LastIndex(spec, ":"); i >= 0 {
		if suffix := spec[i+1:]; suffix == "ro" || suffix == "rw" {
			path, mode = spec[:i], suffix
		}
	}
	if !filepath.IsAbs(path) {
		return Mount{}, fmt.Errorf("%w: the hook mount %q is not an absolute path, and a relative one would mean whatever directory this process happens to be in", ErrContainer, spec)
	}
	if mode == "" && strings.Contains(filepath.Base(spec), ":") {
		return Mount{}, fmt.Errorf("%w: the hook mount %q ends in a mode this runner does not know; write :ro or :rw, or neither for read-only", ErrContainer, spec)
	}
	clean := filepath.Clean(path)
	if strings.Contains(filepath.Base(clean), "docker.sock") || strings.Contains(clean, "/docker.sock") {
		return Mount{}, fmt.Errorf("%w: %q is a docker socket, and a hook container that can reach the docker daemon is root on this host for whatever an operator put in a hook script. This runner will not mount one under any name", ErrContainer, spec)
	}
	if socket := reachesDaemonSocket(clean); socket != "" {
		return Mount{}, fmt.Errorf("%w: the hook mount %q contains the docker socket %s, so a hook could reach the docker daemon through it -- which is root on this host for whatever an operator put in a hook script. Name the directory the hook actually needs, not one above the socket", ErrContainer, spec, socket)
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return Mount{}, fmt.Errorf("%w: the hook mount %q cannot be read: %v", ErrContainer, clean, err)
	}
	if info.Mode()&os.ModeSocket != 0 {
		return Mount{}, fmt.Errorf("%w: the hook mount %q is a socket, and this runner mounts only directories and regular files", ErrContainer, clean)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Mount{}, fmt.Errorf("%w: the hook mount %q is a symbolic link, so what a hook could reach through it is whatever the link says today. Name the directory itself", ErrContainer, clean)
	}
	return Mount{Path: clean, ReadOnly: mode != "rw"}, nil
}

// daemonSocketPaths are the Unix sockets no hook container may be able to
// reach, whether by name or by way of a directory they are in.
//
// Three entries rather than one, because "the docker socket" is not one
// path. /var/run/docker.sock is the documented location and is a SYMLINK
// on every modern Linux, where the real file is /run/docker.sock -- so a
// check that knew only the first would accept `--hook-mount /run`, which
// is the same socket by the path the kernel actually uses. And a
// deployment that moved the endpoint (rootless Docker under
// XDG_RUNTIME_DIR) names it in DOCKER_HOST, which is the endpoint this
// runner's own client talks to.
func daemonSocketPaths() []string {
	paths := []string{"/var/run/docker.sock", "/run/docker.sock"}
	if host := strings.TrimSpace(os.Getenv("DOCKER_HOST")); strings.HasPrefix(host, "unix://") {
		if path := strings.TrimPrefix(host, "unix://"); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

// reachesDaemonSocket reports the daemon socket a mount of clean would
// expose, or "" when it exposes none.
//
// It is a question about TREES rather than about names, which is the
// whole reason it exists: the name check above refuses
// /var/run/docker.sock and every alias of it, and refuses nothing at all
// about `--hook-mount /run:rw`, which mounts the directory the socket
// lives in and hands the same file to the same script by a path nobody
// typed. A mount that is the socket's directory, or anything above it, is
// the socket.
//
// Both sides are resolved through the links that are actually there,
// because on a Linux host /var/run, /run and a bind-mounted runtime
// directory are routinely three names for one place, and a purely lexical
// comparison would accept two of them.
func reachesDaemonSocket(clean string) string {
	mounts := []string{clean}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil && resolved != clean {
		mounts = append(mounts, resolved)
	}
	for _, socket := range daemonSocketPaths() {
		socket = filepath.Clean(socket)
		sockets := []string{socket}
		if resolved, err := filepath.EvalSymlinks(filepath.Dir(socket)); err == nil {
			if real := filepath.Join(resolved, filepath.Base(socket)); real != socket {
				sockets = append(sockets, real)
			}
		}
		for _, candidate := range sockets {
			for _, mount := range mounts {
				if mount == candidate || containsPath(mount, candidate) {
					return candidate
				}
			}
		}
	}
	return ""
}

// containsPath reports whether dir is an ancestor of path. Both are
// expected clean and absolute.
func containsPath(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// arg renders this mount as a --volume value.
func (m Mount) arg() string {
	spec := m.Path + ":" + m.Path
	if m.ReadOnly {
		spec += ":ro"
	}
	return spec
}

// String is the operator-facing rendering, for `status`.
func (m Mount) String() string {
	if m.ReadOnly {
		return m.Path + " (read-only)"
	}
	return m.Path + " (read-write)"
}

// ContainerConfig is what an operator configured, before anything is
// proven about it.
type ContainerConfig struct {
	// Docker is an absolute path to the docker client, or empty to
	// search DockerCandidates.
	Docker string

	// Image is the hook image reference. Empty takes DefaultHookImage.
	Image string

	// Bash is the interpreter path INSIDE that image. Empty takes
	// DefaultHookBash.
	Bash string

	// Network is the container network. Empty takes DefaultHookNetwork.
	Network string

	// User is `uid:gid` inside the container. Empty takes this process's
	// own, which is what makes the files a hook writes into its working
	// directory belong to the account that later removes them.
	User string

	// Mounts are the host paths an operator declared a hook may see,
	// beyond the per-step working directory and the script.
	Mounts []Mount
}

// Container is the PROOF a successful preflight leaves behind, plus
// everything a launch needs.
//
// It is a value rather than a handle because there is nothing to hold
// open: each hook is its own `docker run`. What it carries is what was
// established -- which client, which daemon, which image, which
// interpreter inside it -- so that a launch cannot be assembled from
// facts nobody checked.
type Container struct {
	// Docker is the absolute path to the client this runner proved.
	Docker string

	// ServerVersion is what that client's daemon reported.
	ServerVersion string

	// Image is the hook image reference, and ImageID the id it resolved
	// to at preflight. The id is recorded so that an operator can tell
	// "the tag moved under this deployment" from "the hook changed".
	Image   string
	ImageID string

	// Bash is the interpreter inside the image, as the probe found it.
	Bash Bash

	// Network and User are the launch's own policy, proven usable by the
	// probe rather than assumed.
	Network string
	User    string

	// Mounts are the operator-configured host paths.
	Mounts []Mount

	// clientArgs are the global docker flags this runner passes before
	// every subcommand: the config directory, and the daemon endpoint
	// when the runner's own environment names one.
	//
	// They are FLAGS rather than environment, and that is the reason
	// this field exists. A hook's environment becomes the client
	// process's environment (see hookArgs: --env NAME reads the value
	// from it), so a DOCKER_HOST an operator set in workflows.environment
	// would otherwise redirect this runner's own client to a daemon of
	// their choosing. env.go deletes DOCKER_* from every hook
	// environment as well; this is the other half, and either one alone
	// would be a single mistake away from being the only one.
	clientArgs []string

	// platform is the daemon's own os/arch, as it reported it. It is
	// kept so the refusal below can name both sides of a mismatch.
	platform string
}

// Available reports whether this runner has a proven container
// capability. An Executor without one refuses to execute; there is no
// other path.
func (c Container) Available() bool { return c.Docker != "" && c.Image != "" && c.Bash.Path != "" }

// ProveContainerCapability establishes, once, that this host can run a
// hook in a container -- and refuses in a way nobody can mistake for a
// hook failure when it cannot.
//
// The order is the content. Each step is a different fault with a
// different remedy, and collapsing them into "containers do not work
// here" sends an operator to reinstall Docker over a group membership:
//
//  1. the client binary exists and is executable, at a path this process
//     did not learn from PATH;
//  2. the daemon answers, as THIS account -- the overwhelmingly common
//     NAS fault is a client that works and an account that is not in the
//     docker group;
//  3. the hook user is not root, because a container writing root-owned
//     files into the per-step working directory breaks the cleanup this
//     runner promises, and re-privileges every hook at once;
//  4. the hook network is not one of the two modes that would undo the
//     isolation the default gives -- the host's own namespace, or
//     another container's;
//  5. the configured mounts are paths, not sockets, not links, and not
//     directories the docker socket is in;
//  6. the image is present. It is NOT pulled: a preflight that reached
//     for the network would turn "the image is missing" into a five
//     minute hang on a NAS with no route out, and an operator who
//     chooses an image chooses when to fetch it;
//  7. a probe container runs OUR bytes, under the same hardening a hook
//     gets, and reports the interpreter, the uid and the absence of a
//     terminal from inside.
func ProveContainerCapability(ctx context.Context, cfg ContainerConfig) (Container, error) {
	c := Container{
		Image:   firstNonEmpty(cfg.Image, DefaultHookImage),
		Network: firstNonEmpty(cfg.Network, DefaultHookNetwork),
		User:    cfg.User,
		Mounts:  cfg.Mounts,
		Bash:    Bash{Path: firstNonEmpty(cfg.Bash, DefaultHookBash)},
	}
	if c.User == "" {
		c.User = fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid())
	}

	docker, err := findDocker(cfg.Docker)
	if err != nil {
		return Container{}, err
	}
	c.Docker = docker
	c.clientArgs = dockerClientArgs(os.Environ())

	version, err := c.output(ctx, dockerProbeTimeout,
		c.argv("version", "--format", "{{.Server.Version}} {{.Server.Os}}/{{.Server.Arch}}")...)
	if err != nil {
		return Container{}, daemonRefusal(docker, err)
	}
	c.ServerVersion, c.platform = cutFields(strings.TrimSpace(version))

	if err := refuseRootHookUser(c.User); err != nil {
		return Container{}, err
	}
	if err := refuseHookNetwork(c.Network); err != nil {
		return Container{}, err
	}
	for i := range c.Mounts {
		// Re-validated here, not only where the flag was parsed: a path
		// that was a directory when the unit file was written can be a
		// symbolic link by the time the runner starts, and this is the
		// last moment before it becomes a mount.
		mount, err := ParseMount(c.Mounts[i].spec())
		if err != nil {
			return Container{}, err
		}
		c.Mounts[i] = mount
	}

	imageFacts, err := c.output(ctx, dockerProbeTimeout,
		c.argv("image", "inspect", "--format", "{{.Id}} {{.Os}}/{{.Architecture}}", c.Image)...)
	if err != nil {
		return Container{}, containerRefusal(fmt.Sprintf("the hook image %s is not on this host, and this runner does not pull it for you -- a preflight that reached for the network would hang on a NAS with no route out. Fetch it once with `docker pull %s`, or name an image you have with --hook-image: %v",
			c.Image, c.Image, err))
	}
	var imagePlatform string
	c.ImageID, imagePlatform = cutFields(strings.TrimSpace(imageFacts))
	if err := c.refusePlatformMismatch(imagePlatform); err != nil {
		return Container{}, err
	}

	probe, err := c.runProbe(ctx)
	if err != nil {
		return Container{}, err
	}
	if path := probe["bash_path"]; path != "" {
		c.Bash.Path = path
	}
	c.Bash.Version = probe["bash_version"]
	if c.Bash.Version == "" {
		return Container{}, containerRefusal(fmt.Sprintf("the probe container ran in %s and its shell reported no version, so this runner cannot say which interpreter a hook would get", c.Image))
	}
	return c, nil
}

// spec renders a mount back into the value ParseMount reads, so the
// re-validation above goes through exactly one parser.
func (m Mount) spec() string {
	if m.ReadOnly {
		return m.Path + ":ro"
	}
	return m.Path + ":rw"
}

// findDocker fixes the client binary for this process's lifetime.
//
// A configured path that does not work is a REFUSAL rather than a
// fallback to the search, for FindBash's reason as it was: an operator who
// named a binary and silently got a different one is in the worst of both
// worlds.
func findDocker(configured string) (string, error) {
	if configured != "" {
		if !filepath.IsAbs(configured) {
			return "", containerRefusal(fmt.Sprintf("the configured docker client %q is not an absolute path", configured))
		}
		if err := executableFile(configured); err != nil {
			return "", containerRefusal(fmt.Sprintf("the configured docker client %s cannot be used: %v", configured, err))
		}
		return configured, nil
	}
	for _, candidate := range DockerCandidates() {
		if err := executableFile(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", containerRefusal(fmt.Sprintf("no docker client was found at any of %s, and this runner runs every local hook in a container. Install Docker, or name the client with --docker",
		strings.Join(DockerCandidates(), ", ")))
}

// executableFile reports whether path is a regular, executable file,
// following links (a packaged docker client is routinely a link).
func executableFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("it is not a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("it is mode %#o, which is not executable", info.Mode().Perm())
	}
	return nil
}

// refusePlatformMismatch refuses a hook image built for a different
// architecture from the daemon running it.
//
// It looks like pedantry and is two real faults at once. A NAS operator
// who pulled an amd64 image onto an arm64 box gets a hook that either
// fails with a message about an exec format error or runs under
// emulation -- ten times slower, on the machine where a backup window is
// already tight, and with a libc nobody tested. And the docker client
// WARNS about it on stderr for every single launch, which this runner
// captures as the hook's own stderr: an operator reading their hook's
// log would find a line their script never printed, in a stream this
// product promises is the hook's.
//
// The remedy is one command, so the refusal is that command.
func (c Container) refusePlatformMismatch(imagePlatform string) error {
	if c.platform == "" || imagePlatform == "" {
		// A daemon or an image that did not report a platform is not a
		// mismatch this runner can claim. Refusing on missing evidence
		// would fail a deployment for the format of a template answer.
		return nil
	}
	if imagePlatform == c.platform {
		return nil
	}
	return containerRefusal(fmt.Sprintf("the hook image %s is built for %s and this host's daemon runs %s. Under emulation a hook is an order of magnitude slower and its libc is untested here, and the docker client prints a warning about it into every hook's stderr, which this runner streams to the operator as the hook's own output. Fetch the right one with `docker pull --platform %s %s`",
		c.Image, imagePlatform, c.platform, c.platform, c.Image))
}

// cutFields splits "first second" into its two words, tolerating an
// answer with only one.
func cutFields(s string) (string, string) {
	first, second, _ := strings.Cut(s, " ")
	return first, strings.TrimSpace(second)
}

// refuseRootHookUser is RefuseRoot's rule for the account INSIDE the
// container. See ProveContainerCapability's third step.
//
// The uid is parsed as a NUMBER rather than compared as a string, and
// that is not pedantry about `00`: a --hook-user this runner refused to
// read as root and the kernel read as root would be a hook running as
// root with a preflight that said it was not, which is worse than either
// answer on its own. The probe's own uid assertion is the backstop; this
// is the refusal that names the flag to fix.
func refuseRootHookUser(user string) error {
	uid, _, _ := strings.Cut(user, ":")
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return containerRefusal("no uid was resolved for the hook container, and a container with no --user runs as whatever the image declares, which is root in most images")
	}
	if numeric, err := strconv.ParseInt(uid, 10, 64); err == nil && numeric == 0 || uid == "root" {
		return containerRefusal("a hook container would run as root inside, which writes root-owned files into the per-step working directory this runner later removes, and re-privileges every hook in the deployment at once. Name an unprivileged uid:gid with --hook-user")
	}
	return nil
}

// refuseHookNetwork refuses the two network modes that would quietly
// undo the isolation --network none exists for.
//
// `host` and `container:<id>` are not "a bigger network". They put a hook
// INSIDE a namespace somebody else owns: with `host` a script an operator
// copied off a forum reaches every service on this machine, including the
// ones bound to 127.0.0.1 -- which on a NAS is where the admin interface
// lives, and in this deployment is where the engine's own listeners are.
// `container:<id>` makes what a hook can reach a property of whatever
// that container happens to be today, which is a fact no preflight here
// can establish and none of this runner's hardening can constrain.
//
// A named docker network is allowed, because that is a boundary an
// operator DEFINED and can look at. The default is still `none`.
func refuseHookNetwork(network string) error {
	mode := strings.TrimSpace(network)
	if mode == "host" {
		return containerRefusal("a hook network of `host` puts the hook container in this machine's own network namespace, where a hook script reaches every service on the host -- including the ones bound to 127.0.0.1, which is where a NAS keeps its admin interface and this deployment keeps its own listeners. A local hook quiesces something on this machine and needs no network to do it: leave --hook-network at `none`, or name a docker network")
	}
	if mode == "container" || strings.HasPrefix(mode, "container:") {
		return containerRefusal("a hook network of `" + mode + "` puts the hook container inside another container's network namespace, so what a hook can reach becomes a property of whatever that container is today rather than of anything this runner proved. Leave --hook-network at `none`, or name a docker network")
	}
	return nil
}

// daemonRefusal turns a failed `docker version` into the sentence whose
// remedy matches. The permission-denied shape is the one worth detecting:
// on a NAS the client is installed, the socket is there, and the account
// is not in the docker group.
func daemonRefusal(docker string, err error) error {
	detail := err.Error()
	if strings.Contains(strings.ToLower(detail), "permission denied") {
		return containerRefusal(fmt.Sprintf("the Docker daemon refused this account: %v. Add the account this runner runs as to the docker group and restart it. That membership is root-equivalent on this host, which is why it belongs to the runner and to nothing else -- the engine container never gets it", detail))
	}
	return containerRefusal(fmt.Sprintf("the Docker daemon is not reachable through %s: %v. Local hooks run in containers, so a deployment with hook scripts needs a running daemon this account can reach (docker group)", docker, detail))
}

func containerRefusal(sentence string) error {
	return &Failure{Code: CodeContainerUnavailable, Message: ErrContainer.Error() + ": " + sentence}
}

// runProbe starts one probe container and parses its key=value lines.
//
// It goes through the same hardening as a hook and through the same
// entrypoint override, so what it proves is the capability a hook needs
// rather than the capability a plain `docker run` has. What it does NOT
// carry is a hook environment or any mount: the probe is about the
// runtime, and a probe that needed the step's directories could not run
// before one exists.
func (c Container) runProbe(ctx context.Context) (map[string]string, error) {
	id, err := mintAuxIdentity("probe")
	if err != nil {
		return nil, err
	}
	// The probe is a `--rm` run and normally removes itself. This is for
	// the launch that does not get that far: a probe cancelled, or timed
	// out, between the daemon creating the container and starting it
	// leaves a Created container that --rm never reaps, because --rm is
	// a promise about an exit. The name and the label are minted before
	// the launch precisely so that this can find it.
	defer c.reconcileInstance(id.token)
	out, err := c.output(ctx, dockerProbeTimeout, c.probeArgs(id)...)
	if err != nil {
		return nil, containerRefusal(fmt.Sprintf("a probe container in %s did not run: %v. That is the capability a local hook needs, so this runner will not serve", c.Image, err))
	}
	probe := map[string]string{}
	marker := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == containerProbeMarker {
			marker = true
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			probe[key] = value
		}
	}
	if !marker {
		return nil, containerRefusal(fmt.Sprintf("a container in %s ran and did not print %s, so what ran was not the bytes this runner sent. An image whose entrypoint prints something and exits, or a docker client that is a wrapper script, looks exactly like this: %.400q",
			c.Image, containerProbeMarker, out))
	}
	if probe["tty"] != "no" {
		return nil, containerRefusal(fmt.Sprintf("the probe container found a terminal on its output (tty=%q). A hook with a PTY has its two streams merged and can block forever on a prompt, so this runner will not run one", probe["tty"]))
	}
	if uid := probe["uid"]; uid == "0" {
		return nil, containerRefusal("the probe container ran as uid 0 despite --user, so this daemon is not applying the hardening this runner depends on")
	}
	return probe, nil
}

// probeScript is the fixed capability probe: the marker first, so a
// truncated answer is still recognisable, then facts as key=value lines.
//
// Bash builtins only -- printf, $BASH, $BASH_VERSION, $EUID and [[ -t ]]
// -- so the probe reports on the SHELL rather than on whether the image
// happens to carry coreutils. An image with bash and nothing else is a
// perfectly good hook image and must pass this.
const probeScript = `printf '%s\n' ` + containerProbeMarker + `
printf 'bash_path=%s\n' "$BASH"
printf 'bash_version=%s\n' "$BASH_VERSION"
printf 'uid=%s\n' "$EUID"
if [[ -t 1 ]]; then printf 'tty=yes\n'; else printf 'tty=no\n'; fi
`

// hardening is the flag list every container this runner starts is given,
// hook and probe alike.
//
// One function, so the probe cannot prove a capability the hook launch
// does not use. Each flag's argument is in the doc above the constant it
// comes from; what is worth stating here is what is NOT in the list: no
// --privileged, no --pid=host, no --volume of any socket, no
// --cap-add. This runner has no configuration that can add one.
func (c Container) hardening() []string {
	args := []string{}
	if c.platform != "" {
		// The platform is FIXED at preflight and named on every launch,
		// for the reason the bash path is: a hook must not run under an
		// architecture that depends on the client's environment.
		//
		// DOCKER_DEFAULT_PLATFORM is the specific thing this closes.
		// It is set on developer machines and on cross-building CI
		// hosts, and it changes which variant of a multi-arch tag a
		// `docker run` resolves -- so without this flag the image the
		// preflight inspected and the image a hook runs in could be
		// different architectures, and the client would say so by
		// printing a warning into the hook's own stderr on every
		// launch. Naming it makes the launch deterministic and the
		// warning impossible.
		args = append(args, "--platform", c.platform)
	}
	return append(args,
		"--network", c.Network,
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
		"--read-only",
		"--tmpfs", "/tmp:rw,nosuid,nodev,size="+HookTmpfsSize,
		"--pids-limit", strconv.Itoa(HookPidsLimit),
		"--user", c.User,
		// The image's own entrypoint is replaced rather than trusted:
		// the official bash image declares a wrapper, and a hook that
		// ran inside somebody else's wrapper would not be running the
		// envelope this product documents.
		"--entrypoint", c.Bash.Path,
	)
}

// probeArgs is the capability probe's launch: the hardening, no mounts, no
// environment, and the probe script on the command line -- which is safe
// for this one script because it is a constant in this repository and
// carries no value from anywhere.
//
// It carries a name and this runner's labels for the reason a hook
// launch does: an UNNAMED container is one nothing can address after the
// client that started it has gone, and a probe is started with a context
// that can be cancelled.
func (c Container) probeArgs(id containerIdentity) []string {
	args := append([]string{}, c.clientArgs...)
	args = append(args, "run", "--rm",
		"--name", id.name,
		"--label", LabelHook+"=1",
		"--label", LabelInstance+"="+id.token,
	)
	args = append(args, c.hardening()...)
	args = append(args, c.Image, "--noprofile", "--norc", "-c", probeScript)
	return args
}

// launchSpec is one hook container's inputs.
type launchSpec struct {
	name       string
	token      string
	runID      string
	stepID     string
	workDir    string
	scriptPath string
	envNames   []string
}

// hookArgs builds the `docker create` for one step.
//
// CREATE rather than RUN, and that is the difference the whole
// termination story rests on. `docker run` is create-and-start behind one
// exit status, which costs two things this runner needs: the container's
// name and labels do not exist on the daemon's side until some
// unobservable moment inside that call, so a cancel landing there leaves
// an object nothing here can address; and the client's exit status then
// carries both "the container could not be created" and "the hook ran
// and exited with this status", which are not the same answer and are
// the same number (125). Creating first makes the identity a fact before
// the process is, and leaves the hook's own status to be read off the
// daemon, which is the only party that has it.
//
// There is no --rm either, for the second half of the same reason: a
// container the daemon removes the moment it exits is a container whose
// exit status cannot be inspected afterwards. The removal is explicit
// (Container.remove, Executor.reconcile) and happens after the status
// has been read.
//
// Three properties are asserted about this vector by tests that would be
// meaningless anywhere else, so they are stated here as well:
//
//   - no environment VALUE appears in it. `--env NAME` tells the client to
//     read the value from its own environment (see Container.create,
//     which sets it to ProcessEnv's block), and that keeps repository
//     passphrases out of the host's process list, which on a NAS is
//     readable by every account.
//   - the only mounts are the step's own working directory (read-write),
//     the runner's private copy of the script (read-only), and whatever
//     the OPERATOR configured. The request cannot add one: the protocol
//     has no field that holds a path.
//   - the command after the image is exactly --noprofile --norc and the
//     script. No shell option this product invented.
func (c Container) hookArgs(spec launchSpec) []string {
	args := append([]string{}, c.clientArgs...)
	args = append(args,
		"create",
		"--name", spec.name,
		"--label", LabelHook+"=1",
		"--label", LabelRun+"="+spec.runID,
		"--label", LabelStep+"="+spec.stepID,
		"--label", LabelInstance+"="+spec.token,
	)
	args = append(args, c.hardening()...)
	for _, name := range spec.envNames {
		args = append(args, "--env", name)
	}
	args = append(args,
		"--volume", spec.workDir+":"+spec.workDir,
		"--volume", spec.scriptPath+":"+spec.scriptPath+":ro",
	)
	for _, mount := range c.Mounts {
		args = append(args, "--volume", mount.arg())
	}
	args = append(args, "--workdir", spec.workDir)
	args = append(args, c.Image, "--noprofile", "--norc", spec.scriptPath)
	return args
}

// startArgs runs the created container with this process attached to it.
//
// --attach and no -i: the two streams come back on the client's own two
// pipes, demultiplexed by the daemon because the container has no PTY
// (see the probe's tty assertion), and the hook's stdin stays closed.
func (c Container) startArgs(name string) []string {
	return c.argv("start", "--attach", name)
}

// syntaxArgs runs `bash -n` on the captured bytes INSIDE the hook image.
//
// Inside, not on the host, and that is the whole reason this is a
// container call rather than two lines with the host's bash: the shell
// that judges a script's syntax has to be the shell that will run it. A
// script written for bash 5 does not parse on 3.2, and an operator whose
// hook image carries a different bash from the NAS would otherwise be
// told their script is fine and watch it fail half way through.
//
// The bytes go in on stdin, with no mount and no network: a syntax check
// has no working directory to own and nothing to reach.
func (c Container) syntaxArgs(id containerIdentity) []string {
	args := append([]string{}, c.clientArgs...)
	args = append(args, "run", "--rm", "--interactive",
		"--name", id.name,
		"--label", LabelHook+"=1",
		"--label", LabelInstance+"="+id.token,
	)
	args = append(args, c.hardening()...)
	args = append(args, c.Image, "--noprofile", "--norc", "-n")
	return args
}

// SyntaxCheck parses bytes with the hook image's own `bash -n` and runs
// nothing.
//
// A non-zero exit from bash is a syntax error and is returned as a
// *Failure with CodeSyntax carrying bash's own message, which names the
// line -- the only useful thing anybody can say about a script that does
// not parse. Anything else that went wrong (the daemon, a cancelled
// context, a client that could not start) is CodeInternal, because
// telling an operator that a valid hook "does not parse" is a sentence
// they will act on by editing a correct script.
func (c Container) SyntaxCheck(ctx context.Context, script []byte) error {
	if !c.Available() {
		return noCapability()
	}
	id, err := mintAuxIdentity("syntax")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dockerProbeTimeout)
	defer cancel()

	// Same argument as the probe's: the timeout above and a cancelled
	// validation are both endings that can land between the daemon
	// creating this container and starting it, and --rm does not cover a
	// container that never ran. The reconciliation uses the label rather
	// than this context, which is by then the cancelled one.
	defer c.reconcileInstance(id.token)

	cmd := exec.CommandContext(ctx, c.Docker, c.syntaxArgs(id)...)
	cmd.Stdin = bytes.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return &Failure{Code: CodeInternal, Message: fmt.Sprintf("the syntax check of the captured bytes did not complete: %v. Nothing is known about whether they parse, which is not the same as their not parsing", ctx.Err())}
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return &Failure{Code: CodeInternal, Message: "this runner could not run a syntax check in " + c.Image + ": " + err.Error()}
	}
	// A container that could not be created at all exits 125 from the
	// client, and 126/127 are "the entrypoint could not be run". None of
	// them is bash answering about the script, and reporting one as a
	// syntax error would blame the operator's file for a runtime fault.
	switch exitErr.ExitCode() {
	case 125, 126, 127:
		return &Failure{Code: CodeInternal, Message: fmt.Sprintf("the syntax check could not be run in %s (docker exit %d): %s", c.Image, exitErr.ExitCode(), strings.TrimSpace(stderr.String()))}
	}
	message := strings.TrimSpace(stderr.String())
	if message == "" {
		message = strings.TrimSpace(stdout.String())
	}
	return &Failure{Code: CodeSyntax, Message: "the captured bytes do not parse: " + message}
}

// argv prefixes one docker subcommand with the global client flags.
//
// A separate function because docker requires global flags BEFORE the
// subcommand, and a caller that appended them anywhere else would be
// refused by the client with a message about an unknown flag rather than
// about a daemon.
func (c Container) argv(sub ...string) []string {
	return append(append([]string{}, c.clientArgs...), sub...)
}

// noCapability is the refusal an Executor with no proven container gives.
// It is a *Failure rather than a panic because it is reachable through the
// wire -- a runner is never served without a capability (NewServer
// refuses), so this is the last line of that same argument.
func noCapability() error {
	return containerRefusal("no container capability was proven at startup, so there is nothing to run a hook in. This runner never falls back to executing a hook on the host")
}

// create asks the daemon for one hook container and returns before
// anything in it runs.
//
// The identity this runner minted is on the container from here on: the
// NAME every signal is addressed to, and the unique label every
// reconciliation selects on. That ordering is the point of splitting the
// launch in two -- see hookArgs -- and it is what makes a cancel that
// lands during creation survivable rather than a runaway.
//
// env is the HOOK's block, and this is the one call that gets it: `--env
// NAME` makes the client read each value out of its own environment and
// send it to the daemon over the socket, so the values travel here and
// never appear in the argument vector of this call or of any later one.
// The client's own settings travel as explicit flags for the same reason
// in the other direction (see Container.clientArgs).
//
// The client's stderr is RETURNED rather than folded into an error,
// because it belongs in the step's own stderr where every other launch
// fault's message already is -- and not in a refusal, which would put a
// host path out of an operator's configuration into every record of it.
func (c Container) create(ctx context.Context, timeout time.Duration, env []string, spec launchSpec) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Docker, c.hookArgs(spec)...)
	cmd.Env = env
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return detail, err
	}
	return "", nil
}

// containerState is what the daemon says about one container.
type containerState struct {
	// status is the daemon's own word: created, running, exited, dead.
	status string

	// running is that word's boolean, as the daemon reports it.
	running bool

	// exited is the only state in which exitCode means anything: a
	// container whose own process ran and stopped.
	exited bool

	// exitCode is that process's status. It is the HOOK's exit code,
	// which is the whole reason this round trip exists.
	exitCode int
}

// inspectState asks the daemon what became of a container's own process.
//
// It exists because the client cannot be asked. `docker start --attach`
// reports the container's status as its own exit status, so a hook that
// ran and exited 125 is indistinguishable from a client that could not
// reach the daemon -- and reading that number as "the container was
// never created" tells an operator nothing ran when their script ran and
// returned 125, which is the one fact the workflow engine branches on.
// The daemon knows the difference, so the daemon is asked.
//
// `created` is treated as NOT exited even though it carries an exit code
// of 0, because that is the reading which would turn a launch that never
// happened into a hook that succeeded.
func (c Container) inspectState(timeout time.Duration, name string) (containerState, error) {
	out, err := c.output(context.Background(), timeout,
		c.argv("inspect", "--format", "{{.State.Status}} {{.State.Running}} {{.State.ExitCode}}", name)...)
	if err != nil {
		return containerState{}, err
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 3 {
		return containerState{}, fmt.Errorf("the daemon described %s as %.120q, which this runner cannot read", name, out)
	}
	code, err := strconv.Atoi(fields[2])
	if err != nil {
		return containerState{}, fmt.Errorf("the daemon reported %q as the exit status of %s", fields[2], name)
	}
	state := containerState{
		status:   fields[0],
		running:  fields[1] == "true",
		exitCode: code,
	}
	state.exited = !state.running && state.status == "exited"
	return state, nil
}

// containersWithInstance lists every container the daemon still has
// carrying one launch's unique label.
//
// `ps --all --filter label=` rather than a question about a name, and the
// difference is the whole reason this is the function termination uses.
// A name is what this runner BELIEVES it created; the label is what the
// daemon HAS -- including a container whose creation completed after the
// client that asked for it had already been killed, which is the runaway
// this replaces. `ps` also exits 0 with empty output for "nothing
// matches" and non-zero only when the daemon could not answer, so
// "absent" and "unanswerable" are distinguishable, and the error is
// returned separately for exactly that reason: reporting "gone" because
// a question failed is how a runaway gets recorded as a clean kill.
func (c Container) containersWithInstance(timeout time.Duration, token string) ([]string, error) {
	out, err := c.output(context.Background(), timeout,
		c.argv("ps", "--all", "--quiet", "--no-trunc", "--filter", "label="+LabelInstance+"="+token)...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			ids = append(ids, line)
		}
	}
	return ids, nil
}

// signal sends one signal to a container, addressed by name or by id.
//
// The timeout is the caller's, because these calls happen on the
// termination path and the caller is the one that knows how long the
// whole termination is allowed to take. A control call with no bound of
// its own is how a wedged daemon turns a lost lease into a shutdown that
// never finishes.
func (c Container) signal(timeout time.Duration, target, sig string) error {
	_, err := c.output(context.Background(), timeout, c.argv("kill", "--signal="+sig, target)...)
	return err
}

// remove removes a container, whatever state it is in.
//
// There is no --rm on a hook launch (see hookArgs), so this is not a
// belt-and-braces call any more: it is the removal. The lease guarantee
// is "an engine that went away leaves no runaway AND no leftover", and
// this is the second half of it -- with Executor.reconcile as the part
// that proves it.
func (c Container) remove(timeout time.Duration, target string) error {
	_, err := c.output(context.Background(), timeout, c.argv("rm", "--force", "--volumes", target)...)
	return err
}

// reconcileInstance removes whatever the daemon still has carrying one
// launch's label. It is Executor.reconcile without the certainty: the
// callers are this runner's OWN short-lived containers -- the capability
// probe and the syntax check -- which own no working directory and whose
// outcome no Result reports, so a leftover is a leftover and not a
// forensic question.
//
// Bounded and best effort, and deliberately not using the caller's
// context: the reason it is reached at all is usually that the caller's
// context was cancelled.
func (c Container) reconcileInstance(token string) {
	leftovers, err := c.containersWithInstance(dockerControlTimeout, token)
	if err != nil {
		return
	}
	for _, container := range leftovers {
		_ = c.signal(dockerControlTimeout, container, "KILL")
		_ = c.remove(dockerControlTimeout, container)
	}
}

// output runs one docker argv and returns its stdout, with stderr folded
// into the error because docker says everything useful there.
//
// The argv is the WHOLE vector, global flags included: every caller
// builds it through argv, probeArgs, syntaxArgs or hookArgs, so there is
// no second place that decides where a client flag goes.
//
// Env is deliberately left nil, which inherits this process's own
// environment. That is correct for every call here -- they are the runner
// talking to its own daemon, before any hook environment exists -- and is
// the opposite of Executor.run, which sets it to the hook's block
// precisely so `--env NAME` picks up the hook's values and nothing else.
func (c Container) output(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Docker, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			return stdout.String(), err
		}
		return stdout.String(), fmt.Errorf("%s: %w", detail, err)
	}
	return stdout.String(), nil
}

// dockerClientArgs turns the runner's OWN environment into explicit
// global flags.
//
// Explicit, because the client's environment is about to be the hook's:
// Executor.run sets it to the hook's block so that `--env NAME` can read
// the values from it, and docker honours DOCKER_HOST, DOCKER_CONTEXT and
// DOCKER_API_VERSION from the environment. A flag beats an environment
// variable in docker's own precedence, so resolving them here is what
// makes an operator's `workflows.environment` unable to move this
// runner's daemon endpoint. env.go's deletion of DOCKER_* is the same
// property from the other side.
//
// --host and --context are mutually exclusive in docker; DOCKER_HOST
// wins, which is docker's own order.
func dockerClientArgs(environ []string) []string {
	env := map[string]string{}
	for _, entry := range environ {
		if name, value, ok := strings.Cut(entry, "="); ok {
			env[name] = value
		}
	}
	var args []string
	if config := env["DOCKER_CONFIG"]; config != "" {
		args = append(args, "--config", config)
	} else if home := env["HOME"]; home != "" {
		args = append(args, "--config", filepath.Join(home, ".docker"))
	}
	switch {
	case env["DOCKER_HOST"] != "":
		args = append(args, "--host", env["DOCKER_HOST"])
	case env["DOCKER_CONTEXT"] != "":
		args = append(args, "--context", env["DOCKER_CONTEXT"])
	}
	return args
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
