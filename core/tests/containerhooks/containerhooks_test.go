// Package containerhooks_test is #865's evidence, against a real Docker
// daemon and a real ephemeral container per hook.
//
// It is a machine-tier suite because its central claims cannot be made
// anywhere else. "The hook cannot reach the docker socket", "there is no
// PTY", "the rootfs is read-only", "the network is none", "a
// TERM-ignoring process still dies with the container", "an engine that
// disconnects leaves no container behind" are statements about what the
// DAEMON does with the flags this product passes. A fake could only ever
// prove that this repository agrees with itself: core/internal/hostrunner's
// own unit tests assert what the runner ASKS FOR, through a stand-in
// client that records it, and everything here is measured against the
// real thing.
//
// Hermetic in the one way that matters for a suite that starts
// containers: every container the runner creates carries
// hostrunner.LabelHook, this suite removes anything carrying it on the
// way IN as well as on the way out, and the way-in sweep is what a
// SIGKILLed previous run needs -- a t.Cleanup cannot run after a kill.
// See core/tests/dockerlease for the incident that argument comes from.
package containerhooks_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/hostrunner"
	"github.com/backupdproject/backupd/core/tests/machines"
)

// hookLabel selects every container this product's host runner created.
const hookLabel = hostrunner.LabelHook + "=1"

// capability proves the container capability against the real daemon and
// returns an executor wired to it, plus a layout under a short root.
//
// It sweeps leftovers first, for the reason in the package doc, and it
// FAILS rather than skipping when docker is not usable: this suite is
// #865's evidence, and a suite that skipped itself on the machine where
// the feature does not work would report ok for the one deployment shape
// that is broken. The gate runs it on a host with a daemon (see
// scripts/ci-local.sh); a developer without one gets a failure that says
// what is missing.
func capability(t *testing.T) (*hostrunner.Executor, hostrunner.Layout) {
	t.Helper()

	image := machines.EnsureHookImage(t, hostrunner.DefaultHookImage)
	if removed := machines.RemoveContainersWithLabel(t, hookLabel); removed > 0 {
		t.Logf("removed %d hook container(s) a previous run left behind", removed)
	}
	t.Cleanup(func() { machines.RemoveContainersWithLabel(t, hookLabel) })

	container, err := hostrunner.ProveContainerCapability(context.Background(), hostrunner.ContainerConfig{
		Image: image,
		User:  fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid()),
	})
	if err != nil {
		t.Fatalf("this host cannot run workflow hooks in containers, so #865 has no evidence here: %v", err)
	}

	runnerLayout := layout(t)
	return &hostrunner.Executor{
		Layout:    runnerLayout,
		Container: container,
		Grace:     2 * time.Second,
	}, runnerLayout
}

// layout is a runner layout under a SHORT root, for hostrunner's socket
// length reason, and one this suite owns entirely.
func layout(t *testing.T) hostrunner.Layout {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "bdch")
	if err != nil {
		t.Fatalf("preparing a short temporary root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	l := hostrunner.Layout{
		RuntimeDir:   filepath.Join(root, "run"),
		WorkspaceDir: filepath.Join(root, "workspace"),
		SecretsDir:   filepath.Join(root, "secrets"),
	}
	for _, dir := range []string{l.RuntimeDir, l.WorkspaceDir, l.SecretsDir} {
		if err := hostrunner.EnsureDir(dir); err != nil {
			t.Fatalf("preparing %s: %v", dir, err)
		}
	}
	return l
}

// collector keeps every chunk, for the assertions about the two streams.
type collector struct {
	chunks []hostrunner.Chunk
}

func (c *collector) Chunk(chunk hostrunner.Chunk) error {
	c.chunks = append(c.chunks, chunk)
	return nil
}

func (c *collector) text(stream hostrunner.StreamID) string {
	var b strings.Builder
	for _, chunk := range c.chunks {
		if chunk.Stream == stream {
			b.Write(chunk.Data)
		}
	}
	return b.String()
}

// request builds a well-formed execute request, with the size and hash a
// truthful engine sends.
func request(runID, stepID, body string) hostrunner.Request {
	sum := sha256.Sum256([]byte(body))
	return hostrunner.Request{
		Op:           hostrunner.OpExecute,
		RunID:        runID,
		StepID:       stepID,
		Script:       []byte(body),
		ScriptSize:   int64(len(body)),
		ScriptSHA256: hex.EncodeToString(sum[:]),
		TimeoutMS:    60_000,
	}
}

// TestAHookRunsInsideTheImageAndStreamsBothStreamsBack is the acceptance
// criterion, measured rather than asserted.
//
// "Inside the image" is observable in one way that cannot be faked from
// the host: the hook reads a file that exists in the hook image and not
// on the machine running this test, and reports the bash it is running
// under -- which is the image's, at the path the capability probe found,
// not whatever the host has.
func TestAHookRunsInsideTheImageAndStreamsBothStreamsBack(t *testing.T) {
	exec, _ := capability(t)
	out := &collector{}

	body := `printf 'os=%s\n' "$(cat /etc/alpine-release 2>/dev/null || echo none)"
printf 'bash=%s\n' "$BASH"
printf 'version=%s\n' "$BASH_VERSION"
printf 'hostname=%s\n' "$(hostname)"
echo to-stderr >&2
`
	result, err := exec.Execute(context.Background(), request("run-1", "step-1", body), out)
	if err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("the hook did not succeed: %+v\nstderr %q", result, out.text(hostrunner.StreamStderr))
	}

	stdout := out.text(hostrunner.StreamStdout)
	if strings.Contains(stdout, "os=none") {
		t.Errorf("the hook did not run inside the hook image: it could not read a file that only exists there:\n%s", stdout)
	}
	if !strings.Contains(stdout, "bash="+exec.Container.Bash.Path+"\n") {
		t.Errorf("the hook ran under a different interpreter from the one the preflight proved (%s):\n%s", exec.Container.Bash.Path, stdout)
	}
	hostname, _ := os.Hostname()
	if hostname != "" && strings.Contains(stdout, "hostname="+hostname+"\n") {
		t.Errorf("the hook reports this machine's own hostname, so it ran on the host rather than in a container:\n%s", stdout)
	}

	// The two streams, apart, and numbered from one counter.
	if got := strings.TrimSpace(out.text(hostrunner.StreamStderr)); got != "to-stderr" {
		t.Errorf("stderr carried %q, so the launch merged the streams -- which is what a PTY would do", got)
	}
	seen := map[uint64]bool{}
	var last uint64
	for _, chunk := range out.chunks {
		if chunk.Seq == 0 || seen[chunk.Seq] || chunk.Seq <= last {
			t.Errorf("chunk sequence %d is not a single monotonic series across both streams", chunk.Seq)
		}
		seen[chunk.Seq] = true
		last = chunk.Seq
	}
	if result.Chunks == 0 {
		t.Error("the result reports no chunks, so a consumer cannot tell a truncated stream from a silent hook")
	}
}

// TestTheEnvelopeHoldsInsideTheContainer is the shared execution envelope
// (core/internal/workflowexec), asserted from INSIDE a real container --
// which is the only place several of these can be asserted at all.
//
// Every line of it is a property #865 promised to carry unchanged:
// nothing injected, no PTY, the sanitized baseline, and the hostile
// values arriving byte for byte. The values are hashed in the container
// and compared here, because a comparison of the text would be a
// comparison of two things this test wrote, while a sha256 taken on the
// far side cannot be accidentally right.
func TestTheEnvelopeHoldsInsideTheContainer(t *testing.T) {
	exec, _ := capability(t)
	out := &collector{}

	values := map[string]string{
		"BACKUPD_TEST_SUBSTITUTION": "$(touch /tmp/pwned)`touch /tmp/pwned`",
		"BACKUPD_TEST_QUOTES":       `he said "hi"; rm -rf /; '\''`,
		"BACKUPD_TEST_NEWLINE":      "first\nsecond\ttab\\",
		"BACKUPD_TEST_UTF8":         "café — 日本語 — Ω — 🔒",
	}
	names := []string{
		"BACKUPD_TEST_SUBSTITUTION",
		"BACKUPD_TEST_QUOTES",
		"BACKUPD_TEST_NEWLINE",
		"BACKUPD_TEST_UTF8",
	}
	vars := make([]hostrunner.EnvVar, 0, len(names)+5)
	for _, name := range names {
		vars = append(vars, hostrunner.EnvVar{Name: name, Value: values[name]})
	}
	// The five names that must not survive: four that make bash execute
	// something the plan never captured, and one that would point the
	// runner's own docker client somewhere else.
	vars = append(vars,
		hostrunner.EnvVar{Name: "BASH_ENV", Value: "/tmp/preamble.sh"},
		hostrunner.EnvVar{Name: "ENV", Value: "/tmp/preamble.sh"},
		hostrunner.EnvVar{Name: "SHELLOPTS", Value: "xtrace"},
		hostrunner.EnvVar{Name: "BASHOPTS", Value: "expand_aliases"},
		hostrunner.EnvVar{Name: "DOCKER_HOST", Value: "tcp://attacker.example:2375"},
	)

	body := `printf 'flags=%s\n' "$-"
for name in ` + strings.Join(names, " ") + `; do
  printf '%s=%s\n' "$name" "$(printf '%s' "${!name}" | sha256sum | cut -d' ' -f1)"
done
printenv | sed -n 's/^\(BASH_ENV\|ENV\|SHELLOPTS\|BASHOPTS\|DOCKER_HOST\)=.*/present=\1/p'
printf 'shellopts=%s\n' "$SHELLOPTS"
printf 'bashopts=%s\n' "$BASHOPTS"
if [[ -t 0 || -t 1 || -t 2 ]]; then printf 'tty=yes\n'; else printf 'tty=no\n'; fi
printf 'pwned=%s\n' "$([ -e /tmp/pwned ] && echo yes || echo no)"
`
	req := request("run-env", "step-env", body)
	req.Env = hostrunner.EnvSet{Vars: vars}

	result, err := exec.Execute(context.Background(), req, out)
	if err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("the hook did not succeed: %+v\nstderr %q", result, out.text(hostrunner.StreamStderr))
	}
	stdout := out.text(hostrunner.StreamStdout)

	for _, name := range names {
		sum := sha256.Sum256([]byte(values[name]))
		want := name + "=" + hex.EncodeToString(sum[:]) + "\n"
		if !strings.Contains(stdout, want) {
			t.Errorf("the hook read a different %s than the engine sent.\nwant %s got:\n%s", name, want, stdout)
		}
	}
	if strings.Contains(stdout, "present=") {
		t.Errorf("a variable that must never reach a hook is in its environment:\n%s", stdout)
	}
	if strings.Contains(stdout, "pwned=yes") {
		t.Error("a value was interpreted by a shell on its way in, so the environment is being rendered as text somewhere rather than delivered to the daemon")
	}
	if !strings.Contains(stdout, "tty=no") {
		t.Error("the hook has a terminal on one of its descriptors. A PTY merges the two streams and lets a hook block forever on a prompt")
	}
	for _, injected := range []string{"e", "u", "x"} {
		for _, line := range strings.Split(stdout, "\n") {
			if flags, ok := strings.CutPrefix(line, "flags="); ok && strings.Contains(flags, injected) {
				t.Errorf("bash was started with %q: this runner injected a shell option into somebody else's script", flags)
			}
		}
	}
	// SHELLOPTS and BASHOPTS are set by bash ITSELF in every shell, so
	// their presence as shell variables is not the failure -- their
	// VALUE being the operator's is, because that is what changes how
	// every captured byte is interpreted before the first line runs.
	if strings.Contains(stdout, "xtrace") || strings.Contains(stdout, "expand_aliases") {
		t.Errorf("an operator's shell option reached bash's startup inside the container:\n%s", stdout)
	}
	want := []string{"BASHOPTS", "BASH_ENV", "DOCKER_HOST", "ENV", "SHELLOPTS"}
	if strings.Join(result.DroppedEnvNames, ",") != strings.Join(want, ",") {
		t.Errorf("the result reports %v as dropped rather than %v, so the operator is never told", result.DroppedEnvNames, want)
	}
}

// TestTheContainmentIsWhatItClaims is the hardening, measured from inside
// rather than read off the argument vector.
//
// Each of these is a flag that works perfectly for every hook anybody
// tests with when it is MISSING, which is exactly why it is worth a
// round trip to the daemon: the containment either holds here or it holds
// nowhere.
func TestTheContainmentIsWhatItClaims(t *testing.T) {
	exec, _ := capability(t)
	out := &collector{}

	body := `printf 'uid=%s\n' "$(id -u)"
printf 'socket=%s\n' "$(ls /var/run/docker.sock 2>/dev/null || ls /run/docker.sock 2>/dev/null || echo absent)"
printf 'rootfs=%s\n' "$(touch /forbidden 2>/dev/null && echo writable || echo read-only)"
printf 'tmp=%s\n' "$(touch /tmp/scratch 2>/dev/null && echo writable || echo read-only)"
printf 'workdir=%s\n' "$(touch "$BACKUPD_WORK_DIR/dump" 2>/dev/null && echo writable || echo read-only)"
printf 'network=%s\n' "$( (exec 3<>/dev/tcp/1.1.1.1/443) >/dev/null 2>&1 && echo reachable || echo none)"
printf 'privileged=%s\n' "$(cat /proc/self/status 2>/dev/null | awk '/CapEff/ {print $2}')"
`
	if _, err := exec.Execute(context.Background(), request("run-hard", "step-hard", body), out); err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	stdout := out.text(hostrunner.StreamStdout)

	for _, want := range []string{
		// The acceptance criterion that cannot be softened.
		"socket=absent",
		"rootfs=read-only",
		// And the two directories a hook legitimately needs.
		"tmp=writable",
		"workdir=writable",
		"network=none",
	} {
		if !strings.Contains(stdout, want+"\n") {
			t.Errorf("%s is not what the hardening promises:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "uid=0\n") {
		t.Errorf("the hook ran as root inside its container:\n%s", stdout)
	}
	// cap_drop ALL means an effective capability set of zero. The line
	// is absent on a daemon whose kernel does not publish it, which is
	// not this product's business to assert about.
	for _, line := range strings.Split(stdout, "\n") {
		if caps, ok := strings.CutPrefix(line, "privileged="); ok && caps != "" && strings.Trim(caps, "0") != "" {
			t.Errorf("the hook holds capabilities (CapEff %s) despite --cap-drop ALL", caps)
		}
	}
}

// TestTerminationStopsAndRemovesTheContainerEvenWhenTheHookIgnoresIt is
// the runaway this runner exists to prevent, in the shape that used to
// need a process-group argument.
//
// The hook traps SIGTERM and keeps a child running. Nothing inside will
// stop on request, so the only thing that ends it is the container being
// killed -- and the two assertions are that this runner PROVED it
// (confirmed certainty) and that the daemon no longer knows about the
// container.
func TestTerminationStopsAndRemovesTheContainerEvenWhenTheHookIgnoresIt(t *testing.T) {
	exec, _ := capability(t)

	body := `trap '' TERM
( trap '' TERM; while :; do sleep 0.2; done ) &
printf 'ready\n'
while :; do sleep 0.2; done
`
	req := request("run-kill", "step-kill", body)
	req.TimeoutMS = 2_000

	// Watched while it runs, so "the container is gone afterwards" is a
	// change rather than a fact that was always true.
	seen := make(chan int, 1)
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if ids := machines.ContainersWithLabel(t, hostrunner.LabelRun+"=run-kill"); len(ids) > 0 {
				seen <- len(ids)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		seen <- 0
	}()

	result, err := exec.Execute(context.Background(), req, &collector{})
	if err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	if count := <-seen; count == 0 {
		t.Fatal("no container carrying this step's run label was ever visible, so this test is not watching the hook it thinks it is")
	}
	if result.State != hostrunner.StateTimedOut {
		t.Errorf("a hook that outlived its timeout was reported as %q", result.State)
	}
	if result.TerminationCertainty != hostrunner.CertaintyConfirmed {
		t.Errorf("termination was reported as %q for a container the daemon removed", result.TerminationCertainty)
	}
	if ids := machines.ContainersWithLabel(t, hostrunner.LabelRun+"=run-kill"); len(ids) != 0 {
		t.Errorf("the hook's container survived the step: %v. A hook that traps SIGTERM would now run until this host is rebooted", ids)
	}
	if !result.WorkDirRemoved {
		t.Error("the working directory was kept after a termination that WAS proved")
	}
}

// TestAnEngineThatDisconnectsLeavesNoContainerBehind is the lease, end to
// end, over a real socket and against a real daemon.
//
// This is the failure the lease exists for: an engine dies during a
// `before` hook that has quiesced a database, and nothing is left to time
// the hook out or to run the matching `after` hook. The half #865 adds is
// the second one -- no LEFTOVER container either, because a stopped
// container holding this step's name is a step that can never run again.
func TestAnEngineThatDisconnectsLeavesNoContainerBehind(t *testing.T) {
	exec, runnerLayout := capability(t)

	token := strings.Repeat("c", hostrunner.MinTokenLength)
	if err := os.WriteFile(runnerLayout.TokenPath(), []byte(token), 0o600); err != nil {
		t.Fatalf("writing the credential: %v", err)
	}

	server, err := hostrunner.NewServer(hostrunner.Config{
		Layout:    runnerLayout,
		Version:   "containerhooks-test",
		Token:     []byte(token),
		Container: exec.Container,
		Grace:     2 * time.Second,
		EUID:      os.Geteuid(),
		Username:  hostrunner.CurrentUsername(os.Geteuid()),
	})
	if err != nil {
		t.Fatalf("preparing the runner: %v", err)
	}
	if err := server.Listen(); err != nil {
		t.Fatalf("listening: %v", err)
	}
	serveCtx, stopServing := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(serveCtx) }()
	t.Cleanup(func() {
		stopServing()
		<-served
		_ = server.Close()
	})

	client := hostrunner.Client{SocketPath: runnerLayout.SocketPath(), Version: "containerhooks-test", Token: token}

	// The engine's connection, cancelled mid-hook: the runner reads that
	// as the lease expiring, which is the same path an engine crash
	// takes.
	engineCtx, killEngine := context.WithCancel(context.Background())
	executed := make(chan error, 1)
	go func() {
		_, err := client.Execute(engineCtx, hostrunner.ExecuteRequest{
			RunID:   "run-lease",
			StepID:  "step-lease",
			Script:  []byte("trap '' TERM\nprintf 'ready\\n'\nwhile :; do sleep 0.2; done\n"),
			Timeout: 5 * time.Minute,
		}, &collector{})
		executed <- err
	}()

	deadline := time.Now().Add(60 * time.Second)
	for {
		if ids := machines.ContainersWithLabel(t, hostrunner.LabelRun+"=run-lease"); len(ids) > 0 {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("the hook's container never appeared, so there is nothing to lose the lease over")
		}
		time.Sleep(100 * time.Millisecond)
	}

	killEngine()
	<-executed

	// The runner terminates the container on its own timeline, so this
	// waits for it rather than asserting the instant after the
	// disconnect. What must not happen is it never happening.
	deadline = time.Now().Add(90 * time.Second)
	for {
		ids := machines.ContainersWithLabel(t, hostrunner.LabelRun+"=run-lease")
		if len(ids) == 0 {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the hook's container is still there ninety seconds after the engine went away: %v. That is the runaway the lease exists to prevent, plus a container name this step can never reuse", ids)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestACapabilityRefusalNamesTheMissingPieceAndRunsNothing is the
// preflight's own evidence, against the real daemon.
//
// An image that is not on this host is the ordinary way this fails, and
// what matters is the shape of the answer: a refusal with a code the
// engine can branch on, naming the thing to do about it -- and never a
// hook that ran somewhere else instead.
func TestACapabilityRefusalNamesTheMissingPieceAndRunsNothing(t *testing.T) {
	_, err := hostrunner.ProveContainerCapability(context.Background(), hostrunner.ContainerConfig{
		Image: "backupd.invalid/no-such-hook-image:0",
		User:  fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid()),
	})
	if err == nil {
		t.Fatal("a capability was proved for an image that is not on this host")
	}
	if !hostrunner.IsCode(err, hostrunner.CodeContainerUnavailable) {
		t.Fatalf("the refusal is not a container-capability refusal, so nothing upstream can tell it from a hook that failed: %v", err)
	}
	if !strings.Contains(err.Error(), "docker pull backupd.invalid/no-such-hook-image:0") {
		t.Fatalf("the refusal does not say how to fix it: %v", err)
	}
	// And the runner that holds that refusal cannot be served at all,
	// which is where "never a silent fall back to host bash" is
	// structural rather than documented.
	_, serverErr := hostrunner.NewServer(hostrunner.Config{
		Layout:   layout(t),
		Version:  "containerhooks-test",
		Token:    []byte(strings.Repeat("d", hostrunner.MinTokenLength)),
		EUID:     os.Geteuid(),
		Username: "test",
	})
	if serverErr == nil {
		t.Fatal("a runner with no container capability was created, so a deployment with no Docker would serve hooks by some other means")
	}
}

// TestASyntaxErrorIsRefusedByTheImagesOwnBash keeps the distinction the
// engine branches on, across the boundary that now carries it.
//
// The check runs in a container of its own, in the hook IMAGE, so the
// shell that judges the script is the shell that would run it. What must
// not happen is a container-runtime failure being reported as a script
// that does not parse, or the reverse.
func TestASyntaxErrorIsRefusedByTheImagesOwnBash(t *testing.T) {
	exec, _ := capability(t)

	if err := exec.Container.SyntaxCheck(context.Background(), []byte("if true\n")); err == nil {
		t.Fatal("a script that does not parse was accepted")
	} else if !hostrunner.IsCode(err, hostrunner.CodeSyntax) {
		t.Fatalf("the refusal is not a syntax error: %v", err)
	}
	if err := exec.Container.SyntaxCheck(context.Background(), []byte("printf 'hello\\n'\n")); err != nil {
		t.Fatalf("a script that parses was refused: %v", err)
	}

	// And the same through Execute, where nothing must be created.
	out := &collector{}
	_, err := exec.Execute(context.Background(), request("run-syn", "step-syn", "printf 'first\\n'\nif true\n"), out)
	if !hostrunner.IsCode(err, hostrunner.CodeSyntax) {
		t.Fatalf("Execute did not refuse the unparseable script as a syntax error: %v", err)
	}
	if ids := machines.ContainersWithLabel(t, hostrunner.LabelRun+"=run-syn"); len(ids) != 0 {
		t.Errorf("a hook container was created for a script that does not parse: %v", ids)
	}
}
