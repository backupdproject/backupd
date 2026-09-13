package hostrunner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The container launch, asserted where it can be asserted without a
// daemon: the ARGUMENT VECTOR this runner builds, and the refusals it
// produces when the host cannot run a container at all.
//
// Everything in this file is about what the runner does. What happens
// INSIDE a real container -- that the environment arrives byte for byte,
// that there is no PTY, that no docker socket is reachable, that a
// TERM-ignoring process still dies -- is proven against a real daemon in
// core/tests/containerhooks, because a fake would only ever prove that
// this repository's idea of docker is self-consistent. The tier rule
// (core/internal/testtier) is what keeps the two apart: nothing in this
// package may need a daemon.

// hookSpec is a launch spec of the shape Execute builds, for the argv
// assertions.
func hookSpec(t *testing.T) launchSpec {
	t.Helper()
	root := t.TempDir()
	return launchSpec{
		name:       "backupd-hook-run1-step1-0a1b2c3d",
		runID:      "run1",
		stepID:     "step1",
		workDir:    filepath.Join(root, "work"),
		scriptPath: filepath.Join(root, "step1.script"),
		envNames:   []string{"BACKUPD_WORK_DIR", "PGPASSWORD"},
	}
}

// testContainer is a proven capability with nothing proven: the fields a
// successful preflight would have filled in, so the argv builder can be
// asserted on its own.
func testContainer() Container {
	return Container{
		Docker:        "/usr/bin/docker",
		ServerVersion: "27.0.0",
		Image:         DefaultHookImage,
		ImageID:       "sha256:0123456789ab",
		Bash:          Bash{Path: DefaultHookBash, Version: "5.2.37(1)-release"},
		Network:       DefaultHookNetwork,
		User:          "1000:1000",
		platform:      "linux/arm64",
	}
}

// argvHas reports whether argv carries flag followed by value.
func argvHas(argv []string, flag, value string) bool {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) && argv[i+1] == value {
			return true
		}
	}
	return false
}

func argvIndex(argv []string, want string) int {
	for i, a := range argv {
		if a == want {
			return i
		}
	}
	return -1
}

// TestHookArgs_NeverMountsTheDockerSocket is the acceptance criterion that
// cannot be softened. A hook container with the docker socket in it is
// root on the host for whatever an operator dropped in a directory, which
// is strictly worse than the shell this whole design exists to avoid.
//
// It is asserted over the WHOLE vector rather than over the mount flags,
// because there are several ways to hand a container the socket -- a bind
// mount, a --mount spec, a --volume-from, an environment variable naming a
// tcp daemon -- and a test that only read the ones this build happens to
// emit would go quiet the moment the shape changed.
func TestHookArgs_NeverMountsTheDockerSocket(t *testing.T) {
	argv := testContainer().hookArgs(hookSpec(t))
	for _, a := range argv {
		if strings.Contains(a, "docker.sock") || strings.Contains(a, "/var/run/docker") {
			t.Fatalf("the hook container is handed the docker socket by %q: argv %v", a, argv)
		}
		if strings.Contains(a, "--privileged") || strings.Contains(a, "--pid=host") {
			t.Fatalf("the hook container is handed the host by %q", a)
		}
	}
}

// TestHookArgs_CarriesTheHardeningTheDesignPromises holds the launch to
// #865's list. Each flag is here because its absence is invisible: a
// container that runs with the default capability set, a writable rootfs
// or the default bridge network works perfectly for every hook anybody
// tests with, and is the containment the issue promised, missing.
func TestHookArgs_CarriesTheHardeningTheDesignPromises(t *testing.T) {
	c := testContainer()
	argv := c.hookArgs(hookSpec(t))

	for _, want := range []struct{ flag, value string }{
		{"--network", "none"},
		{"--security-opt", "no-new-privileges"},
		{"--cap-drop", "ALL"},
		{"--user", "1000:1000"},
		{"--entrypoint", DefaultHookBash},
	} {
		if !argvHas(argv, want.flag, want.value) {
			t.Errorf("%s %s is missing: argv %v", want.flag, want.value, argv)
		}
	}
	// The platform is fixed at preflight and named on every launch, so
	// a DOCKER_DEFAULT_PLATFORM in whatever environment the client
	// inherits cannot decide which architecture somebody's hook runs
	// under -- nor make the client print a warning about it into the
	// hook's own stderr.
	if !argvHas(argv, "--platform", "linux/arm64") {
		t.Errorf("the launch does not name the platform the preflight proved: argv %v", argv)
	}
	if argvIndex(argv, "--read-only") < 0 {
		t.Errorf("--read-only is missing: argv %v", argv)
	}
	// And NOT --rm, which is a property of this vector rather than an
	// omission from it. A container the daemon removes the moment it
	// exits is a container whose exit status cannot be inspected
	// afterwards -- and the hook's own status, 125 included, is read off
	// the daemon precisely because the client cannot report it. The
	// removal is explicit and happens after the status has been taken
	// (Executor.reconcile).
	if argvIndex(argv, "--rm") >= 0 {
		t.Errorf("the hook container is created with --rm, so the daemon may remove it before its exit status is read: argv %v", argv)
	}
	if argvIndex(argv, "create") != 0 {
		t.Errorf("the launch is not a `docker create`, so the container's identity does not exist before its process does: argv %v", argv)
	}
	if argvIndex(argv, "--tmpfs") < 0 {
		t.Error("there is no writable /tmp, so a hook that uses mktemp fails for a reason the read-only rootfs does not explain")
	}
	if argvIndex(argv, "--pids-limit") < 0 {
		t.Error("nothing bounds the number of processes a hook may start")
	}
}

// TestHookArgs_RunsTheCapturedBytesUnderTheProvenBashAndNothingElse is the
// envelope, in argv form: the image's own entrypoint is replaced by the
// bash the preflight proved, and the only arguments after the image are
// --noprofile --norc and the script.
//
// The entrypoint override is the part that looks redundant and is not. The
// official bash image declares `docker-entrypoint.sh` as its entrypoint,
// so a launch that merely named bash in the command would run somebody
// else's wrapper with bash's path as its first argument -- an envelope
// nobody wrote and this product cannot describe.
func TestHookArgs_RunsTheCapturedBytesUnderTheProvenBashAndNothingElse(t *testing.T) {
	c := testContainer()
	spec := hookSpec(t)
	argv := c.hookArgs(spec)

	image := argvIndex(argv, c.Image)
	if image < 0 {
		t.Fatalf("the image is not in the vector: %v", argv)
	}
	got := argv[image+1:]
	want := []string{"--noprofile", "--norc", spec.scriptPath}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the command after the image is %v, want %v: anything else is a shell option this product injected", got, want)
	}
	if !argvHas(argv, "--entrypoint", c.Bash.Path) {
		t.Fatal("the image's own entrypoint is not overridden, so a hook runs inside whatever wrapper the image declares")
	}
	for _, injected := range []string{"-e", "-u", "-o", "pipefail", "-x", "set"} {
		if argvIndex(got, injected) >= 0 {
			t.Fatalf("%q is injected into the hook's shell", injected)
		}
	}
}

// TestHookArgs_CarriesEnvironmentNamesAndNeverValues is why the launch
// passes `--env NAME` rather than `--env NAME=VALUE`.
//
// A value on the command line is a value in the host's process list:
// `ps` on a NAS is readable by every account, and the values here are
// repository passphrases and database passwords. Naming the variable and
// letting the client read the value out of its own environment -- which is
// this process's environment, built by ProcessEnv -- keeps the value in a
// place only the daemon socket sees.
func TestHookArgs_CarriesEnvironmentNamesAndNeverValues(t *testing.T) {
	c := testContainer()
	spec := hookSpec(t)
	argv := c.hookArgs(spec)

	for _, name := range spec.envNames {
		if !argvHas(argv, "--env", name) {
			t.Errorf("%s is not forwarded into the container: argv %v", name, argv)
		}
	}
	for _, a := range argv {
		if strings.Contains(a, "=") && strings.HasPrefix(a, "PGPASSWORD=") {
			t.Fatalf("an environment VALUE reached the command line: %q", a)
		}
	}
}

// TestHookArgs_MountsTheStepsOwnPathsAndNothingElse is the mount policy:
// the per-step working directory read-write, the runner's private copy of
// the script read-only, and no other path unless an operator configured
// one.
//
// The mounts are IDENTITY mounts -- the same path inside as outside -- so
// that BACKUPD_WORK_DIR means one thing. A hook that logs where it wrote a
// dump logs a path the operator can then find, and the engine's own record
// of the working directory is the record of the place the bytes are.
func TestHookArgs_MountsTheStepsOwnPathsAndNothingElse(t *testing.T) {
	c := testContainer()
	spec := hookSpec(t)
	argv := c.hookArgs(spec)

	if !argvHas(argv, "--volume", spec.workDir+":"+spec.workDir) {
		t.Errorf("the per-step working directory is not mounted read-write at its own path: argv %v", argv)
	}
	if !argvHas(argv, "--volume", spec.scriptPath+":"+spec.scriptPath+":ro") {
		t.Errorf("the captured script is not mounted read-only at its own path: argv %v", argv)
	}
	if !argvHas(argv, "--workdir", spec.workDir) {
		t.Errorf("the hook does not start in its own working directory: argv %v", argv)
	}

	var mounts []string
	for i, a := range argv {
		if a == "--volume" {
			mounts = append(mounts, argv[i+1])
		}
	}
	if len(mounts) != 2 {
		t.Fatalf("mounts = %v, want exactly the working directory and the script", mounts)
	}
}

// TestHookArgs_CarriesAnOperatorsConfiguredMountsReadOnlyByDefault covers
// the one way a hook reaches anything else on the host.
//
// The paths come from the RUNNER's configuration, never from the request,
// and that is a security property rather than an ergonomic choice: the
// protocol has no field that can hold a path (see protocol.go), so the
// engine -- or anything else that reached the socket -- cannot choose what
// a hook container can see. Read-only unless the operator wrote `:rw`,
// because a hook that only reads its source tree is the common case and
// the destructive one is worth typing.
func TestHookArgs_CarriesAnOperatorsConfiguredMountsReadOnlyByDefault(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "photos")
	dest := filepath.Join(dir, "dumps")
	for _, d := range []string{source, dest} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	ro, err := ParseMount(source)
	if err != nil {
		t.Fatalf("parsing a bare path: %v", err)
	}
	rw, err := ParseMount(dest + ":rw")
	if err != nil {
		t.Fatalf("parsing an explicit rw path: %v", err)
	}
	if ro.ReadOnly != true || rw.ReadOnly != false {
		t.Fatalf("mount modes = %+v %+v, want the bare path read-only", ro, rw)
	}

	c := testContainer()
	c.Mounts = []Mount{ro, rw}
	argv := c.hookArgs(hookSpec(t))
	if !argvHas(argv, "--volume", source+":"+source+":ro") {
		t.Errorf("the configured source path is not mounted read-only: %v", argv)
	}
	if !argvHas(argv, "--volume", dest+":"+dest) {
		t.Errorf("the configured destination path is not mounted read-write: %v", argv)
	}
}

func TestParseMount_Refusals(t *testing.T) {
	existing := t.TempDir()
	socket := filepath.Join(existing, "docker.sock")

	for _, tc := range []struct{ name, spec, want string }{
		{"a relative path", "workflows:ro", "absolute"},
		{"an unknown mode", existing + ":rx", "ro"},
		{"the docker socket", "/var/run/docker.sock", "docker"},
		{"a docker socket under another name", socket, "docker"},
		{"nothing at all", "", "absolute"},
		// The directory the socket is IN, which is the refusal the name
		// check does not make: `--hook-mount /run:rw` hands the same
		// file to the same script by a path nobody typed. Both spellings
		// are covered because /var/run is a symlink to /run on every
		// modern Linux and neither is the other lexically.
		{"the directory the socket is in", "/run:rw", "docker socket"},
		{"the directory the socket is in, by its other name", "/var/run", "docker socket"},
		// And the ancestor case, which is the same argument one level
		// out: a mount of / contains every socket on the host.
		{"the root of the filesystem", "/", "docker socket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseMount(tc.spec)
			if err == nil {
				t.Fatalf("%q was accepted as a hook mount", tc.spec)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal does not say why: %v", err)
			}
		})
	}
}

// --- the capability preflight ---------------------------------------------
//
// Every refusal below is asserted to be a REFUSAL and asserted not to
// leave a usable Container behind, because the whole point of the
// preflight is that there is nothing to fall back to: a runner that could
// not prove the capability must not start, and #865 names "never a silent
// fall back to host bash" as the criterion.

func TestProveContainerCapability_RefusesAMissingDockerBinary(t *testing.T) {
	_, err := ProveContainerCapability(context.Background(), ContainerConfig{
		Docker: filepath.Join(t.TempDir(), "no-docker-here"),
		Image:  DefaultHookImage,
		User:   "1000:1000",
	})
	if err == nil {
		t.Fatal("a runner with no docker proved a container capability")
	}
	if !IsCode(err, CodeContainerUnavailable) {
		t.Fatalf("the refusal is not a container-capability refusal, so nothing upstream can tell it from a hook failure: %v", err)
	}
	if !strings.Contains(err.Error(), "no-docker-here") {
		t.Fatalf("the refusal does not name the path it looked at: %v", err)
	}
}

func TestProveContainerCapability_RefusesAnUnreachableDaemon(t *testing.T) {
	docker := writeFakeDocker(t, fakeDocker{versionFails: "permission denied while trying to connect to the Docker daemon socket"})

	_, err := ProveContainerCapability(context.Background(), ContainerConfig{
		Docker: docker,
		Image:  DefaultHookImage,
		User:   "1000:1000",
	})
	if !IsCode(err, CodeContainerUnavailable) {
		t.Fatalf("an unreachable daemon is not a container-capability refusal: %v", err)
	}
	// The single most common shape on the deployments this product
	// targets: the CLI is installed and the account is not in the docker
	// group. A refusal that says "docker failed" sends an operator to
	// reinstall Docker over a group membership.
	if !strings.Contains(err.Error(), "docker group") {
		t.Fatalf("a permission-denied daemon does not get the group hint: %v", err)
	}
}

func TestProveContainerCapability_RefusesAnAbsentImage(t *testing.T) {
	docker := writeFakeDocker(t, fakeDocker{imageMissing: true})

	_, err := ProveContainerCapability(context.Background(), ContainerConfig{
		Docker: docker,
		Image:  "example.invalid/nothing:1",
		User:   "1000:1000",
	})
	if !IsCode(err, CodeContainerUnavailable) {
		t.Fatalf("a missing hook image is not a container-capability refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "docker pull example.invalid/nothing:1") {
		t.Fatalf("the refusal does not say how to fix it: %v", err)
	}
}

// TestProveContainerCapability_RefusesAnImageBuiltForAnotherArchitecture
// is the mismatch that produces two separate faults and looks like a
// cosmetic one.
//
// An amd64 hook image on an arm64 NAS either fails with an exec format
// error or runs under emulation -- an order of magnitude slower, on the
// machine where the backup window is already tight. And the docker client
// prints a warning about it on stderr for every launch, which this runner
// streams to the operator as the HOOK's stderr: a line in their hook's
// log that their script never wrote.
func TestProveContainerCapability_RefusesAnImageBuiltForAnotherArchitecture(t *testing.T) {
	docker := writeFakeDocker(t, fakeDocker{imagePlatform: "linux/loong64"})

	_, err := ProveContainerCapability(context.Background(), ContainerConfig{
		Docker: docker,
		Image:  DefaultHookImage,
		Bash:   hostBashForFake(t),
		User:   "1000:1000",
	})
	if !IsCode(err, CodeContainerUnavailable) {
		t.Fatalf("a hook image for another architecture is not a container-capability refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "docker pull --platform") {
		t.Fatalf("the refusal does not say how to fix it: %v", err)
	}
}

// TestProveContainerCapability_RefusesAProbeThatDoesNotEmitOurMarker is the
// local half of #810's argument, and the reason the probe exists at all.
//
// "docker ran" is not the capability. An image whose entrypoint prints
// something and exits 0, a `docker` on PATH that is a wrapper script, a
// daemon that answers version and cannot start a container -- all of them
// look like success from the outside. The capability is "MY bytes ran in a
// container", and the only way to ask that is to require an answer nothing
// else can produce.
func TestProveContainerCapability_RefusesAProbeThatDoesNotEmitOurMarker(t *testing.T) {
	docker := writeFakeDocker(t, fakeDocker{probeSays: "hello from somebody else\n"})

	_, err := ProveContainerCapability(context.Background(), ContainerConfig{
		Docker: docker,
		Image:  DefaultHookImage,
		User:   "1000:1000",
	})
	if !IsCode(err, CodeContainerUnavailable) {
		t.Fatalf("a probe that never ran our bytes is not a container-capability refusal: %v", err)
	}
	if !strings.Contains(err.Error(), containerProbeMarker) {
		t.Fatalf("the refusal does not name the evidence it wanted: %v", err)
	}
}

// TestProveContainerCapability_RefusesAProbeThatFoundATerminal asserts the
// no-PTY property from INSIDE the container, which is the only place it
// can be asserted: "we did not pass -t" is a statement about this client,
// and a PTY changes what a hook is (it merges the two streams and makes an
// interactive prompt block forever instead of failing).
func TestProveContainerCapability_RefusesAProbeThatFoundATerminal(t *testing.T) {
	docker := writeFakeDocker(t, fakeDocker{probeSays: containerProbeMarker + "\nbash_path=/usr/local/bin/bash\nbash_version=5.2.37\nuid=1000\ntty=yes\n"})

	_, err := ProveContainerCapability(context.Background(), ContainerConfig{
		Docker: docker,
		Image:  DefaultHookImage,
		User:   "1000:1000",
	})
	if !IsCode(err, CodeContainerUnavailable) {
		t.Fatalf("a container with a terminal on its output is not a capability refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("the refusal does not say what it found: %v", err)
	}
}

// TestProveContainerCapability_RefusesAHookThatWouldRunAsRootInside is
// RefuseRoot's rule, one boundary further in.
//
// A container running as uid 0 writes root-owned files into the per-step
// working directory, which this runner then cannot remove -- so the
// "removed after the step" guarantee quietly becomes a directory that
// accumulates -- and it re-privileges every hook in the deployment with
// one flag, which is the switch RefuseRoot exists to not have.
func TestProveContainerCapability_RefusesAHookThatWouldRunAsRootInside(t *testing.T) {
	docker := writeFakeDocker(t, fakeDocker{})

	_, err := ProveContainerCapability(context.Background(), ContainerConfig{
		Docker: docker,
		Image:  DefaultHookImage,
		User:   "0:0",
	})
	if !IsCode(err, CodeContainerUnavailable) {
		t.Fatalf("a root hook user is not a capability refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "root") {
		t.Fatalf("the refusal does not name the problem: %v", err)
	}
}

// TestProveContainerCapability_ReportsWhatItProved is the other half: a
// successful preflight is a set of FACTS an operator can read, for the
// same reason `status` reports the bash path -- "which bash ran my hook"
// and "which image did it run in" are the first two questions anybody
// asks, and neither is answerable from the outside.
func TestProveContainerCapability_ReportsWhatItProved(t *testing.T) {
	docker := writeFakeDocker(t, fakeDocker{})

	c, err := ProveContainerCapability(context.Background(), ContainerConfig{
		Docker: docker,
		Image:  DefaultHookImage,
		// The stand-in client really execs this, so it has to be a
		// path that exists; in a deployment it is a path inside the
		// image. What the test is about is that the reported facts come
		// from the PROBE rather than from the configuration.
		Bash: hostBashForFake(t),
		User: "1000:1000",
	})
	if err != nil {
		t.Fatalf("proving the capability against a working daemon: %v", err)
	}
	if c.Docker != docker || c.Image != DefaultHookImage || c.ImageID == "" {
		t.Fatalf("the capability does not carry what it proved: %+v", c)
	}
	if c.Bash.Path == "" || c.Bash.Version == "" {
		t.Fatalf("the in-image interpreter was not reported: %+v", c.Bash)
	}
	if !strings.HasPrefix(c.Bash.Version, "3.") && !strings.HasPrefix(c.Bash.Version, "4.") && !strings.HasPrefix(c.Bash.Version, "5.") {
		t.Errorf("bash version = %q, which is not something a bash reported about itself", c.Bash.Version)
	}
	if c.Network != DefaultHookNetwork {
		t.Fatalf("network = %q, want the default %q", c.Network, DefaultHookNetwork)
	}
}

// TestExecute_RefusesWithoutAProvenCapabilityAndRunsNoHostBash is the
// mutation-proof test for the criterion this whole issue turns on.
//
// An executor with no proven container must refuse. It must not resolve a
// host bash, it must not run the script, and the witness below is how that
// is observed rather than asserted: a hook that ran anywhere at all would
// have created the file, so a future "fall back to the host if docker is
// missing" makes this test fail.
func TestExecute_RefusesWithoutAProvenCapabilityAndRunsNoHostBash(t *testing.T) {
	witness := filepath.Join(t.TempDir(), "the-hook-ran")
	e := &Executor{Layout: testLayout(t)}

	_, err := e.Execute(context.Background(),
		scriptRequest("run-1", "step-1", "printf ran > '"+witness+"'\n"), &collector{})
	if err == nil {
		t.Fatal("a runner with no container capability executed a hook")
	}
	if !IsCode(err, CodeContainerUnavailable) {
		t.Fatalf("the refusal is not a container-capability refusal: %v", err)
	}
	if _, statErr := os.Stat(witness); statErr == nil {
		t.Fatal("the hook RAN: this runner fell back to executing a script outside a container, which is the one thing #865 forbids")
	}
}

// TestProveContainerCapability_RefusesANetworkThatUndoesTheIsolation is
// the two --hook-network values that are not "a bigger network".
//
// `host` puts the hook in this machine's own network namespace, where a
// script an operator copied off a forum reaches every service on the
// host -- including the ones bound to 127.0.0.1, which on a NAS is the
// admin interface and in this deployment is the engine's own listeners.
// `container:<id>` makes what a hook can reach a property of whatever
// that container is today, which is a fact no preflight here can
// establish. Both are a reduction in isolation that no other flag in
// this runner can produce, so both are refused rather than documented.
func TestProveContainerCapability_RefusesANetworkThatUndoesTheIsolation(t *testing.T) {
	docker := writeFakeDocker(t, fakeDocker{})

	for _, network := range []string{"host", "container:backupd-engine", "container"} {
		t.Run(network, func(t *testing.T) {
			_, err := ProveContainerCapability(context.Background(), ContainerConfig{
				Docker:  docker,
				Image:   DefaultHookImage,
				Bash:    hostBashForFake(t),
				User:    "1000:1000",
				Network: network,
			})
			if err == nil {
				t.Fatalf("--hook-network %s was accepted, and with it every service on this host", network)
			}
			if !IsCode(err, CodeContainerUnavailable) {
				t.Fatalf("the refusal is not a container-capability refusal: %v", err)
			}
			if !strings.Contains(err.Error(), "none") {
				t.Fatalf("the refusal does not name the remedy: %v", err)
			}
		})
	}

	// A named network is a boundary an operator DEFINED, so it is
	// allowed: this is a refusal of two namespace modes, not of
	// networking.
	if _, err := ProveContainerCapability(context.Background(), ContainerConfig{
		Docker:  docker,
		Image:   DefaultHookImage,
		Bash:    hostBashForFake(t),
		User:    "1000:1000",
		Network: "backupd-hooks",
	}); err != nil {
		t.Fatalf("a named docker network was refused as well, which is a refusal of networking rather than of the host namespace: %v", err)
	}
}

// TestRefuseRootHookUser_ReadsTheUidAsANumber is the spelling the string
// comparison missed.
//
// The kernel reads `00` and ` 0` as uid 0; a check that compared the text
// to "0" read them as somebody else. A hook this runner said was
// unprivileged and the daemon ran as root is worse than either answer on
// its own, which is why the refusal is early and the probe's own uid
// assertion is only the backstop.
func TestRefuseRootHookUser_ReadsTheUidAsANumber(t *testing.T) {
	for _, user := range []string{"0", "0:0", "00", "00:00", " 0", "0 ", "+0", "-0", "root", "root:root"} {
		if err := refuseRootHookUser(user); err == nil {
			t.Errorf("--hook-user %q was accepted, and it is uid 0 to the kernel", user)
		}
	}
	if err := refuseRootHookUser(""); err == nil {
		t.Error("a hook container with no --user was accepted, and an image declares root in most cases")
	}
	for _, user := range []string{"1000:1000", "501:20", "10:0", "nobody"} {
		if err := refuseRootHookUser(user); err != nil {
			t.Errorf("--hook-user %q was refused as root: %v", user, err)
		}
	}
}
