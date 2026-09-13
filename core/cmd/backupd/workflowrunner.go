package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/backupdproject/backupd/core/cliecho"
	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/hostrunner"
)

// `workflow-runner`: the one command in this binary that is meant to be
// run OUTSIDE the container (#809).
//
// Everything else here talks to a deployment. This talks to a host. The
// canonical runtime is distroless, read-only, non-root and has no shell
// (container/compose.yaml, docs/runtime-contract.md), so a `.local.sh`
// hook -- which means "run this on the machine backupd is installed on"
// -- cannot be executed by the engine at all. internal/hostrunner's
// package doc lays out the four ways that sentence could be satisfied and
// why three of them are the same mistake; this is the fourth, and it is a
// separate process on the host that the engine reaches over one Unix
// socket.
//
// It is in THIS binary, rather than in a second one, for the reason the
// version refusal exists: the engine and the runner are one program cut
// in half by a socket, and a runner built from a different tree is a
// mismatch nothing could detect. Shipping them as one artifact makes
// "version-pinned to the installed release" a property of the file on
// disk rather than a step in an installer somebody may skip.
//
// # Why the directories are flags and not configuration
//
// The runner's paths are HOST paths: <prefix>/run, <prefix>/workspace and
// <prefix>/secrets as they exist on the machine, not as they appear
// inside the container. The same config.yaml is read from both sides --
// the engine sees /data/state and /etc/backupd/config, a shell on the
// host sees <prefix>/state and <prefix>/config -- so a field in that file
// could not be right in both places. route.go makes exactly this argument
// about where the engine's address comes from, and this is the same fact
// from the other end.
//
// --config is still accepted and still does something: it carries the
// deployment's configured bound on one script's size, so the runner
// refuses what the engine's own planner would have refused. A deployment
// that has not been configured yet (#176's first-run state) has no
// config.yaml at all, and the runner starts anyway with the documented
// default, because refusing to start would mean a fresh install could
// never reach the point of having one.

// workflowRunnerVerb is one `workflow-runner` subcommand.
type workflowRunnerVerb struct {
	// operand is the usage fragment after the verb, for the refusal
	// text. Empty for both verbs today: they take flags only.
	operand string
	run     func(args []string) int
}

// workflowRunnerVerbs is every verb `workflow-runner` dispatches.
//
// A table rather than a switch, for mediumVerbs' reason:
// TestUsage_EveryRegisteredCommandIsPinned reads main.go's map, which has
// one entry for `workflow-runner` and cannot see a level below it, so a
// verb added here with no line in usage() would be dispatchable,
// undiscoverable and pinned by nothing. TestUsage_NamesEveryWorkflowRunnerVerb
// holds this table against usage().
var workflowRunnerVerbs = map[string]workflowRunnerVerb{
	"serve":  {run: workflowRunnerServe},
	"status": {run: workflowRunnerStatus},
}

// workflowRunnerVerbNames returns the verbs in a stable order, for the
// usage guard and for refusal text.
func workflowRunnerVerbNames() []string {
	names := make([]string, 0, len(workflowRunnerVerbs))
	for verb := range workflowRunnerVerbs {
		names = append(names, verb)
	}
	sort.Strings(names)
	return names
}

func cmdWorkflowRunner(args []string) int {
	if len(args) == 0 {
		return usageError("workflow-runner needs a verb: %s", strings.Join(workflowRunnerVerbNames(), ", "))
	}
	verb, rest := args[0], args[1:]
	entry, ok := workflowRunnerVerbs[verb]
	if !ok {
		return usageError("workflow-runner has no %q verb. It has %s", verb, strings.Join(workflowRunnerVerbNames(), ", "))
	}
	_ = entry.operand
	return entry.run(rest)
}

// runnerDirs are the three host directories every verb needs, and the one
// place their flags are declared.
type runnerDirs struct {
	runtimeDir   *string
	workspaceDir *string
	secretsDir   *string
}

func runnerDirFlags(fs interface {
	String(name string, value string, usage string) *string
}) runnerDirs {
	return runnerDirs{
		runtimeDir:   fs.String("runtime-dir", "", "the host directory holding the runner socket, normally <prefix>/run. It is the only directory the engine's container mounts, so nothing else lives in it"),
		workspaceDir: fs.String("workspace-dir", "", "the runner-private host directory holding the per-step working directories, normally <prefix>/workspace. It is mounted into no container"),
		secretsDir:   fs.String("secrets-dir", "", "the host directory holding this installation's workflow-runner credential, normally <prefix>/secrets"),
	}
}

func (d runnerDirs) layout() (hostrunner.Layout, error) {
	layout := hostrunner.Layout{
		RuntimeDir:   *d.runtimeDir,
		WorkspaceDir: *d.workspaceDir,
		SecretsDir:   *d.secretsDir,
	}
	if err := layout.Validate(); err != nil {
		return hostrunner.Layout{}, err
	}
	return layout, nil
}

// containerFlags are the hook-container knobs, declared in one place
// because `serve` and any future verb that has to describe the runtime
// must describe the same one.
type containerFlags struct {
	docker  *string
	image   *string
	bash    *string
	network *string
	user    *string
	mounts  *hookMountList
}

// hookMountList collects a repeatable --hook-mount.
//
// Repeatable rather than comma-separated, because these are PATHS: a
// comma is a legal character in a directory name on every filesystem
// this product runs on, and a flag that split on one would be a flag
// that mounts two wrong directories for somebody's "photos,raw" folder.
type hookMountList struct {
	mounts []hostrunner.Mount
}

func (l *hookMountList) String() string {
	rendered := make([]string, 0, len(l.mounts))
	for _, m := range l.mounts {
		rendered = append(rendered, m.String())
	}
	return strings.Join(rendered, ", ")
}

func (l *hookMountList) Set(value string) error {
	mount, err := hostrunner.ParseMount(value)
	if err != nil {
		return err
	}
	l.mounts = append(l.mounts, mount)
	return nil
}

func containerFlagSet(fs *flag.FlagSet) containerFlags {
	mounts := &hookMountList{}
	fs.Var(mounts, "hook-mount", "a host path a hook may see inside its container, as PATH or PATH:ro or PATH:rw (read-only by default). Repeat for more than one. The per-step working directory and the captured script are mounted for you; nothing else is")
	return containerFlags{
		docker:  fs.String("docker", "", "absolute path to the docker client; empty searches the documented candidates"),
		image:   fs.String("hook-image", hostrunner.DefaultHookImage, "the image every local hook runs in. It must already be present: this runner refuses rather than pulling"),
		bash:    fs.String("hook-bash", hostrunner.DefaultHookBash, "the absolute path of bash INSIDE the hook image"),
		network: fs.String("hook-network", hostrunner.DefaultHookNetwork, "the docker network a hook container joins; `none` gives a hook no network at all"),
		user:    fs.String("hook-user", "", "uid:gid a hook runs as inside its container; empty takes this process's own, which is what makes the files a hook writes removable afterwards"),
		mounts:  mounts,
	}
}

func (c containerFlags) config() hostrunner.ContainerConfig {
	return hostrunner.ContainerConfig{
		Docker:  *c.docker,
		Image:   *c.image,
		Bash:    *c.bash,
		Network: *c.network,
		User:    *c.user,
		Mounts:  c.mounts.mounts,
	}
}

// workflowRunnerServe is the long-running host helper.
func workflowRunnerServe(args []string) int {
	fs, cfgPath := newFlagSet("workflow-runner serve")
	dirs := runnerDirFlags(fs)
	container := containerFlagSet(fs)

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		return usageError("workflow-runner serve takes no arguments")
	}
	layout, err := dirs.layout()
	if err != nil {
		return usageError("%v", err)
	}

	// Unprivileged BY REFUSAL rather than by default. An administrator
	// installed this product, which says nothing about whether they
	// meant every script in a hook directory to run as root. There is no
	// flag that turns this off; sudoers is the operator's tool for a
	// specific privileged command, and it is auditable in a way a switch
	// here would not be.
	if err := hostrunner.RefuseRoot(os.Geteuid()); err != nil {
		return fail(err)
	}

	token, err := hostrunner.LoadToken(layout.TokenPath())
	if err != nil {
		return fail(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The container capability, proved before the socket exists (#865).
	// A host with no docker, no reachable daemon or no hook image does
	// not serve: local hooks run in ephemeral containers and there is no
	// fallback to a shell on this machine. The refusal names which of
	// those four it was, because the remedies are completely different
	// and "containers do not work here" sends an operator to reinstall
	// Docker over a group membership.
	hookContainer, err := hostrunner.ProveContainerCapability(ctx, container.config())
	if err != nil {
		return fail(err)
	}

	server, err := hostrunner.NewServer(hostrunner.Config{
		Layout:        layout,
		Version:       version,
		Token:         token,
		Container:     hookContainer,
		MaxScriptSize: configuredScriptBound(*cfgPath),
		EUID:          os.Geteuid(),
		Username:      hostrunner.CurrentUsername(os.Geteuid()),
	})
	if err != nil {
		return fail(err)
	}
	if err := server.Listen(); err != nil {
		return fail(err)
	}
	defer server.Close()

	// Printed before serving, and to stdout, because a supervisor's log
	// is where an operator looks first when a hook did not run: the
	// socket it would have to reach, the image and interpreter their
	// script will be given, the account it will run as, and every host
	// path it can see are the facts that answer most of those questions
	// without anybody attaching to anything.
	fmt.Printf("%s workflow runner %s\n", cliecho.Binary, version)
	fmt.Printf("socket %s\n", server.SocketPath())
	fmt.Printf("docker %s (server %s)\n", hookContainer.Docker, hookContainer.ServerVersion)
	fmt.Printf("hook image %s (%s)\n", hookContainer.Image, hookContainer.ImageID)
	fmt.Printf("hook bash %s (%s)\n", hookContainer.Bash.Path, hookContainer.Bash.Version)
	fmt.Printf("hook network %s, hook user %s\n", hookContainer.Network, hookContainer.User)
	for _, mount := range hookContainer.Mounts {
		fmt.Printf("hook mount %s\n", mount)
	}
	fmt.Printf("user %s (uid %d)\n", hostrunner.CurrentUsername(os.Geteuid()), os.Geteuid())

	if err := server.Serve(ctx); err != nil {
		return fail(err)
	}
	return exitOK
}

// workflowRunnerStatus is the preflight question asked from a terminal.
//
// The same question the engine asks before it validates a `.local.sh`
// hook, deliberately: an operator debugging "why did my hook not run"
// and the engine deciding whether to start a backup are asking one thing,
// and two implementations of it would be two answers.
func workflowRunnerStatus(args []string) int {
	fs, _ := newFlagSet("workflow-runner status")
	dirs := runnerDirFlags(fs)

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		return usageError("workflow-runner status takes no arguments")
	}
	layout, err := dirs.layout()
	if err != nil {
		return usageError("%v", err)
	}

	token, err := hostrunner.LoadToken(layout.TokenPath())
	if err != nil {
		return fail(err)
	}

	client := hostrunner.Client{SocketPath: layout.SocketPath(), Version: version, Token: string(token)}
	status, err := client.Status(context.Background())
	if err != nil {
		return fail(err)
	}

	fmt.Printf("runner %s\n", status.Version)
	fmt.Printf("socket %s\n", status.SocketPath)
	fmt.Printf("docker %s (server %s)\n", status.DockerPath, status.DockerServerVersion)
	fmt.Printf("hook image %s (%s)\n", status.HookImage, status.HookImageID)
	fmt.Printf("hook bash %s (%s)\n", status.BashPath, status.BashVersion)
	fmt.Printf("hook network %s, hook user %s\n", status.HookNetwork, status.HookUser)
	for _, mount := range status.HookMounts {
		fmt.Printf("hook mount %s\n", mount)
	}
	fmt.Printf("user %s (uid %d)\n", status.User, status.UID)
	fmt.Printf("runtime %s\n", status.RuntimeDir)
	fmt.Printf("workspace %s\n", status.WorkspaceDir)
	if len(status.Active) == 0 {
		fmt.Println("running nothing")
		return exitOK
	}
	fmt.Printf("running %s\n", strings.Join(status.Active, " "))
	return exitOK
}

// configuredScriptBound reads the deployment's own limit on one hook
// script's size, and tolerates a configuration that is not there.
//
// Tolerates, rather than refuses, for the reason in this file's preamble:
// a deployment in #176's first-run state has no config.yaml, and a runner
// that would not start without one could never be provisioned by an
// installer that runs before the first setup. Zero means "no configured
// bound", which internal/hostrunner reads as its own default.
func configuredScriptBound(path string) int64 {
	resolved := config.ResolvePath(path)
	cfg, err := config.Load(resolved)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// A config file that exists and does not parse is worth a
			// word on stderr rather than a silent default: the operator
			// asked for a bound and is not getting it.
			fmt.Fprintf(os.Stderr, "%s: the configured script size bound could not be read from %s, using this build's default: %v\n", cliecho.Binary, resolved, err)
		}
		return 0
	}
	return cfg.Workflows.MaxScriptSizeBytes
}
