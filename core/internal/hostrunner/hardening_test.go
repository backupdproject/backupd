package hostrunner

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Every container vector this runner launches, held to #865's hardening
// at the ARGUMENT VECTOR level -- not just the hook one.
//
// There are four launches in this package and they are all here:
//
//   - hookArgs, the step's own `docker create`;
//   - probeArgs, the capability probe's `docker run --rm`;
//   - syntaxArgs, the `bash -n` check's `docker run --rm`;
//   - startArgs, which starts a container hookArgs already created and
//     therefore carries no hardening of its own.
//
// The table below IS the assertion, and a fifth vector added later must
// be added to it. The gap this closes is specific: hardening() is one
// function, but each vector assembles its own flags around it, so a probe
// or a syntax check launched without that call would prove a capability
// -- "a container with a read-only rootfs, no capabilities and a non-root
// user starts on this host" -- that the hook launch does not actually
// ask for, and every existing test would stay green. container_test.go
// asserts the hook vector; nothing asserted the other three.
//
// Two forbidden rows look redundant against the preflight and are not.
// refuseHookNetwork rejects `host` and `container:<id>`, and
// refuseRootHookUser rejects uid 0, but hardening() renders c.Network and
// c.User VERBATIM into the argv -- so the argv row is the backstop for
// any future path that builds a Container without going through
// ProveContainerCapability (a test fixture, a config reload, a
// deserialised capability). The refusal and the rendering are two
// different places to get this wrong.

// launchVector is one of this runner's docker invocations, with what its
// shape is supposed to be.
type launchVector struct {
	// what names the vector in a failure, in the terms an operator would
	// use rather than the function's.
	what string
	argv []string

	// subcommand is the first non-client token, and ephemeral says
	// whether the daemon may remove the container when it exits.
	subcommand string
	ephemeral  bool

	// hardened is false only for startArgs, whose container was already
	// created with the flags.
	hardened bool
}

// launchVectors builds all four out of one proven capability, so what is
// asserted is the argv a real preflight's Container produces rather than
// a literal assembled in this file.
func launchVectors(t *testing.T) (Container, []launchVector) {
	t.Helper()

	container, _ := fakeCapability(t, fakeDocker{})

	probe, err := mintAuxIdentity("probe")
	if err != nil {
		t.Fatalf("minting the probe container's identity: %v", err)
	}
	syntax, err := mintAuxIdentity("syntax")
	if err != nil {
		t.Fatalf("minting the syntax container's identity: %v", err)
	}
	hook, err := mintContainerIdentity("run1", "step1")
	if err != nil {
		t.Fatalf("minting the hook container's identity: %v", err)
	}

	root := t.TempDir()
	spec := launchSpec{
		name:       hook.name,
		token:      hook.token,
		runID:      "run1",
		stepID:     "step1",
		workDir:    filepath.Join(root, "work"),
		scriptPath: filepath.Join(root, "step1.script"),
		envNames:   []string{"BACKUPD_WORK_DIR", "PGPASSWORD"},
	}

	return container, []launchVector{
		{what: "the hook launch", argv: container.hookArgs(spec), subcommand: "create", ephemeral: false, hardened: true},
		{what: "the capability probe", argv: container.probeArgs(probe), subcommand: "run", ephemeral: true, hardened: true},
		{what: "the syntax check", argv: container.syntaxArgs(syntax), subcommand: "run", ephemeral: true, hardened: true},
		{what: "the start of a created hook container", argv: container.startArgs(hook.name), subcommand: "start", ephemeral: false, hardened: false},
	}
}

// vectorCarriesFlag reports whether argv names flag at all, written
// either as two tokens or as the single token `--flag=value`.
//
// Both spellings, because docker accepts both and a check that knew only
// the two-token form would miss `--privileged=true` -- which is the
// spelling somebody adds when they are appending to a slice in a loop.
func vectorCarriesFlag(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// vectorFlagValues returns every value argv gives for flag, in either
// spelling. A flag written last with no value yields nothing, which is
// what the required table then reports.
func vectorFlagValues(argv []string, flag string) []string {
	var values []string
	for i, a := range argv {
		switch {
		case a == flag && i+1 < len(argv):
			values = append(values, argv[i+1])
		case strings.HasPrefix(a, flag+"="):
			values = append(values, strings.TrimPrefix(a, flag+"="))
		}
	}
	return values
}

// vectorHasFlagValue reports whether argv gives flag exactly this value.
func vectorHasFlagValue(argv []string, flag, value string) bool {
	return slices.Contains(vectorFlagValues(argv, flag), value)
}

// vectorNonRootUser reports why a --user value is root, or nil when the
// kernel would not read it as root.
//
// The uid is PARSED rather than compared against a list of strings, for
// refuseRootHookUser's reason: "0", "0:0" and "00" are the same account
// and only one of them is the string anybody thinks to exclude. A
// non-numeric value ("root", or a name this test cannot resolve to a
// number) fails too, because a launch whose uid this test cannot read is
// a launch it cannot say is unprivileged.
func vectorNonRootUser(value string) error {
	uid, _, _ := strings.Cut(value, ":")
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return fmt.Errorf("--user %q names no uid", value)
	}
	number, err := strconv.Atoi(uid)
	if err != nil {
		return fmt.Errorf("--user %q does not name a numeric uid, so nothing here can prove the hook is not root", value)
	}
	if number == 0 {
		return fmt.Errorf("--user %q is root inside the container", value)
	}
	return nil
}

// TestEveryLaunchVectorCarriesTheHardeningTheHookVectorIsTestedFor is the
// container-contract regression #812 asks for.
//
// The bug it catches: a vector assembled without hardening() -- or with
// it, before a flag was added to it and to only some callers. A probe
// that starts a container with the default capability set, a writable
// rootfs or the default bridge proves that the DAEMON works and says
// nothing about whether the launch a hook gets works, which is the one
// question the preflight exists to answer.
func TestEveryLaunchVectorCarriesTheHardeningTheHookVectorIsTestedFor(t *testing.T) {
	container, vectors := launchVectors(t)

	for _, vector := range vectors {
		if !vector.hardened {
			continue
		}
		for _, want := range []struct{ flag, value string }{
			{"--cap-drop", "ALL"},
			{"--security-opt", "no-new-privileges"},
			{"--network", DefaultHookNetwork},
			{"--pids-limit", strconv.Itoa(HookPidsLimit)},
			// The entrypoint is the bash the probe FOUND inside the
			// image, not a path from the host: a vector that let the
			// image's own entrypoint stand would run a hook inside
			// somebody else's wrapper.
			{"--entrypoint", container.Bash.Path},
		} {
			if !vectorHasFlagValue(vector.argv, want.flag, want.value) {
				t.Errorf("%s does not carry %s %s: argv %v", vector.what, want.flag, want.value, vector.argv)
			}
		}

		if !vectorCarriesFlag(vector.argv, "--read-only") {
			t.Errorf("%s has a writable rootfs: argv %v", vector.what, vector.argv)
		}

		// /tmp specifically, because the rootfs is read-only: a tmpfs
		// mounted anywhere else leaves mktemp failing with a message
		// about a read-only filesystem that names no flag anybody could
		// act on.
		tmpfs := false
		for _, value := range vectorFlagValues(vector.argv, "--tmpfs") {
			if target, _, _ := strings.Cut(value, ":"); target == "/tmp" {
				tmpfs = true
			}
		}
		if !tmpfs {
			t.Errorf("%s gives the container no writable /tmp: argv %v", vector.what, vector.argv)
		}

		users := vectorFlagValues(vector.argv, "--user")
		if len(users) == 0 {
			t.Errorf("%s names no user, so the container runs as the image's -- which is root in every official image: argv %v", vector.what, vector.argv)
		}
		for _, user := range users {
			if err := vectorNonRootUser(user); err != nil {
				t.Errorf("%s runs privileged: %v: argv %v", vector.what, err, vector.argv)
			}
		}
	}
}

// TestEveryLaunchVectorRefusesTheFlagsThatWouldUndoTheHardening asserts
// the absences.
//
// The bug it catches: any one of these flags appearing on any vector --
// through a debugging change somebody kept, an operator-configurable
// passthrough, or a future --hook-device. Each of them individually gives
// a hook back something --cap-drop ALL, --read-only and --network none
// were there to take away, and each of them is invisible in every test
// that only asserts the flags that ARE there.
func TestEveryLaunchVectorRefusesTheFlagsThatWouldUndoTheHardening(t *testing.T) {
	_, vectors := launchVectors(t)

	// Namespaces somebody else owns, capabilities coming back, host
	// devices, host kernel parameters and host name resolution.
	forbidden := []struct{ flag, why string }{
		{"--privileged", "a privileged container is root on the host"},
		{"--pid", "the host's pid namespace lets a hook signal every process on the machine"},
		{"--ipc", "the host's ipc namespace exposes another process's shared memory"},
		{"--userns", "a user namespace override undoes the non-root uid"},
		{"--cap-add", "a capability handed back is a capability --cap-drop ALL did not drop"},
		{"--device", "a host device node is direct access to hardware, including the disks"},
		{"--device-cgroup-rule", "a device cgroup rule is the same access by the other spelling"},
		{"--sysctl", "a container that can set kernel parameters can set the host's"},
		{"--add-host", "an injected name makes what a hook resolves a property of this launch"},
	}
	for _, vector := range vectors {
		for _, bad := range forbidden {
			if vectorCarriesFlag(vector.argv, bad.flag) {
				t.Errorf("%s carries %s: %s: argv %v", vector.what, bad.flag, bad.why, vector.argv)
			}
		}

		// unconfined is the one --security-opt value that cancels the
		// no-new-privileges the required table asks for, and it is a
		// value rather than a flag, so the flag check above cannot see
		// it.
		for _, value := range vectorFlagValues(vector.argv, "--security-opt") {
			if strings.Contains(value, "unconfined") {
				t.Errorf("%s asks for --security-opt %s, which turns off the sandbox the other flags configure: argv %v", vector.what, value, vector.argv)
			}
		}

		// refuseHookNetwork rejects both of these at preflight; this is
		// the argv backstop, because hardening() renders c.Network
		// verbatim and a Container built without the preflight would
		// reach here unchecked.
		for _, value := range vectorFlagValues(vector.argv, "--network") {
			if value == "host" {
				t.Errorf("%s joins the host's network namespace, so a hook reaches every service bound to 127.0.0.1 on this machine: argv %v", vector.what, vector.argv)
			}
			if strings.HasPrefix(value, "container:") {
				t.Errorf("%s joins %s, whose reachability is a fact no preflight here can establish: argv %v", vector.what, value, vector.argv)
			}
		}

		// The mounts, against this runner's OWN idea of where the daemon
		// socket is -- the same list ParseMount refuses -- rather than
		// against a literal path, so a deployment whose DOCKER_HOST
		// moved the endpoint is checked against the endpoint it actually
		// has.
		for _, flag := range []string{"--volume", "-v"} {
			for _, value := range vectorFlagValues(vector.argv, flag) {
				source, _, _ := strings.Cut(value, ":")
				if socket := reachesDaemonSocket(filepath.Clean(source)); socket != "" {
					t.Errorf("%s mounts %s, which reaches the daemon socket %s -- that is root on the host: argv %v", vector.what, source, socket, vector.argv)
				}
				for _, field := range strings.Split(value, ":") {
					if field == "/" {
						t.Errorf("%s mounts the host root in %s, so the container boundary is a directory listing: argv %v", vector.what, value, vector.argv)
					}
				}
			}
		}
	}
}

// TestEveryLaunchVectorIsTheSubcommandItsTerminationStoryNeeds pins the
// create/start split and the --rm each vector may or may not have.
//
// The bug it catches: a hook launch collapsed back into `docker run`, or
// given --rm. Either one is invisible in a passing hook -- the script
// runs, the output arrives -- and destroys the exit status, because a
// container the daemon removed cannot be inspected and `docker run`'s own
// 125 means both "the hook exited 125" and "the container could not be
// created". The probe and the syntax check are the opposite case: they
// have no status anybody reads, so they MUST self-remove or they leak a
// container per preflight.
func TestEveryLaunchVectorIsTheSubcommandItsTerminationStoryNeeds(t *testing.T) {
	container, vectors := launchVectors(t)

	for _, vector := range vectors {
		// The subcommand is the first token AFTER the global client
		// flags, which docker requires to come first (see Container.argv).
		if len(vector.argv) <= len(container.clientArgs) {
			t.Fatalf("%s is only the global client flags: argv %v", vector.what, vector.argv)
		}
		if got := vector.argv[len(container.clientArgs)]; got != vector.subcommand {
			t.Errorf("%s is a `docker %s`, want `docker %s`: argv %v", vector.what, got, vector.subcommand, vector.argv)
		}
		if got := vectorCarriesFlag(vector.argv, "--rm"); got != vector.ephemeral {
			if vector.ephemeral {
				t.Errorf("%s has no --rm, so every one of them leaves a stopped container behind: argv %v", vector.what, vector.argv)
			} else {
				t.Errorf("%s carries --rm, so the daemon may remove the container before its exit status is read: argv %v", vector.what, vector.argv)
			}
		}
	}
}

// TestEveryLaunchVectorCarriesTheHookLabelSoASweepCanFindIt covers the
// leftovers.
//
// The bug it catches: a vector that creates a container without
// LabelHook. Nothing fails at the time -- the probe or the check runs and
// exits -- but a runner killed between creating one and removing it
// leaves an object no `docker ps --filter label=` of this runner's finds,
// which is the difference between a leftover an operator can sweep and an
// anonymous container they have to identify by hand.
func TestEveryLaunchVectorCarriesTheHookLabelSoASweepCanFindIt(t *testing.T) {
	_, vectors := launchVectors(t)

	for _, vector := range vectors {
		// The start vector addresses a container hookArgs already
		// labelled, so it is the one launch with nothing to label.
		if vector.subcommand == "start" {
			continue
		}
		if !vectorHasFlagValue(vector.argv, "--label", LabelHook+"=1") {
			t.Errorf("%s creates a container carrying no %s label, so a sweep of this runner's leftovers cannot find it: argv %v", vector.what, LabelHook, vector.argv)
		}
		// And a name, for the same reason one step further: a container
		// with no name is one nothing can address once the client that
		// started it has gone.
		if len(vectorFlagValues(vector.argv, "--name")) != 1 {
			t.Errorf("%s does not name its container exactly once: argv %v", vector.what, vector.argv)
		}
	}
}

// TestTheStartVectorCarriesNoHardeningBecauseTheCreateAlreadyFixedIt is
// the inverse of the table above, and it is the row that would catch the
// regression the create/start split exists to prevent.
//
// startArgs must be exactly the global client flags plus `start --attach
// <name>`. Any hardening flag appearing there would mean the two-call
// launch had collapsed back into a single `docker run` -- which is
// precisely the shape whose exit status cannot be told apart from the
// client's own (see hookArgs), and the shape in which a cancel landing
// during creation leaves a container nothing here can address.
func TestTheStartVectorCarriesNoHardeningBecauseTheCreateAlreadyFixedIt(t *testing.T) {
	container, vectors := launchVectors(t)

	var start launchVector
	for _, vector := range vectors {
		if vector.subcommand == "start" {
			start = vector
		}
	}
	if start.argv == nil {
		t.Fatal("the table no longer covers the start vector")
	}

	name := start.argv[len(start.argv)-1]
	want := append(append([]string{}, container.clientArgs...), "start", "--attach", name)
	if !slices.Equal(start.argv, want) {
		t.Fatalf("the start vector is %v, want exactly %v", start.argv, want)
	}

	for _, flag := range []string{"--cap-drop", "--security-opt", "--read-only", "--network", "--user", "--pids-limit", "--tmpfs", "--entrypoint", "--rm", "--volume", "--env"} {
		if vectorCarriesFlag(start.argv, flag) {
			t.Errorf("the start vector carries %s, so the launch is no longer a create followed by a start: argv %v", flag, start.argv)
		}
	}
}
