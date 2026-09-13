package hostrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestExecute_RunsTheCapturedBytesInAContainerOnThisHost is #865's first
// acceptance criterion, reduced to the two things it claims that can be
// claimed without a daemon.
//
// The hook runs on the HOST -- the machine backupd is installed on --
// rather than inside the distroless engine container, which has no shell
// at all. And it gets there through a container this runner created and
// then started: the call log shows both, with this step's container name
// on them, and nothing was executed any other way.
func TestExecute_RunsTheCapturedBytesInAContainerOnThisHost(t *testing.T) {
	exec, state := testExecutor(t)
	witness := filepath.Join(t.TempDir(), "only-on-this-host")
	if err := os.WriteFile(witness, []byte("present"), 0o600); err != nil {
		t.Fatalf("writing the witness file: %v", err)
	}

	out := &collector{}
	result, err := exec.Execute(context.Background(),
		scriptRequest("run-1", "step-1", "cat "+witness+"\n"), out)
	if err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("the hook did not succeed: %+v, stderr %q", result, out.text(StreamStderr))
	}
	if got := out.text(StreamStdout); got != "present" {
		t.Errorf("the hook could not read a file that exists only on this host, so it did not run here: %q", got)
	}

	// One creation and one attached start for this step, and the step's
	// own name on both: the launch is the explicit lifecycle rather than
	// a `docker run`, which is what lets the exit status below be the
	// hook's own (see Executor.observeExit).
	created := dockerCallsWithVerb(t, state, "create", containerNamePrefix+"run-1-step-1-")
	if len(created) != 1 {
		t.Fatalf("the hook's container was not created exactly once for this step: %v", dockerCalls(t, state))
	}
	started := dockerCallsWithVerb(t, state, "start", containerNamePrefix+"run-1-step-1-")
	if len(started) != 1 {
		t.Fatalf("the created container was not started with this process attached: %v", dockerCalls(t, state))
	}
}

// TestExecute_RefusesBytesWhoseHashDoesNotMatchTheClaim is the second
// verification of the same fact, and the reason it is done twice.
//
// internal/workflow verified these bytes against the plan when it opened
// them. Between that check and this one lies a socket, and a check on the
// far side of a boundary is not a check on this side of it.
func TestExecute_RefusesBytesWhoseHashDoesNotMatchTheClaim(t *testing.T) {
	exec, state := testExecutor(t)
	req := scriptRequest("run-1", "step-1", "echo hello\n")
	req.Script = []byte("rm -rf /\n")

	_, err := exec.Execute(context.Background(), req, &collector{})
	if err == nil {
		t.Fatal("bytes that do not hash to the declared sha256 were executed")
	}
	if !IsCode(err, CodeScriptMismatch) {
		t.Fatalf("the refusal is not a script_mismatch, so the engine cannot tell it from a hook that failed: %v", err)
	}
	// The CREATE verb, not the name prefix: this runner's own probe and
	// syntax-check containers carry the same prefix, so a search for it
	// would find them and this assertion would be about nothing.
	if created := dockerCallsWithVerb(t, state, "create"); len(created) != 0 {
		t.Errorf("a hook container was created for bytes that were refused: %v", created)
	}
	assertNothingLeftBehind(t, exec.Layout, "run-1", "step-1")
}

// TestExecute_RefusesBytesWhoseSizeDoesNotMatchTheClaim covers the
// truncation case specifically: bytes that are a prefix of the real
// script still parse, still run, and do half of what the operator wrote.
func TestExecute_RefusesBytesWhoseSizeDoesNotMatchTheClaim(t *testing.T) {
	exec, _ := testExecutor(t)
	req := scriptRequest("run-1", "step-1", "echo hello\n")
	req.ScriptSize = req.ScriptSize + 1

	_, err := exec.Execute(context.Background(), req, &collector{})
	if !IsCode(err, CodeScriptMismatch) {
		t.Fatalf("a size that disagrees with the bytes was not refused as a script_mismatch: %v", err)
	}
}

// TestExecute_RefusesAnIdThatWouldEscapeTheRuntimeDirectory is the
// traversal case.
//
// These ids arrive over a socket and become directory names that this
// process creates, chmods and later removes recursively -- and, since
// #865, part of a container NAME this runner later signals by. A run id
// of "../../../etc" is not a hypothetical attack, it is what a bug in a
// caller looks like.
func TestExecute_RefusesAnIdThatWouldEscapeTheRuntimeDirectory(t *testing.T) {
	exec, _ := testExecutor(t)

	for _, id := range []string{"../escape", "run/nested", "..", "", "-flag", "run\x00id"} {
		req := scriptRequest(id, "step-1", "echo hello\n")
		_, err := exec.Execute(context.Background(), req, &collector{})
		if err == nil {
			t.Errorf("the run id %q was accepted as a directory name under the runtime directory", id)
			continue
		}
		if !IsCode(err, CodeRefused) {
			t.Errorf("the run id %q was refused for the wrong reason: %v", id, err)
		}
	}
}

// TestExecute_RefusesBytesBashCannotParse is the `bash -n` preflight, and
// the code it is refused with matters as much as the refusal.
//
// "This hook does not parse" has to be distinguishable from "this hook
// failed", because #809 requires `.local.sh` validation to fail BEFORE a
// backup starts -- which is only possible if the engine can ask the
// question without running anything. Since #865 the check runs in the
// hook IMAGE rather than on the host, so the shell that judges the
// syntax is the shell that will run it.
func TestExecute_RefusesBytesBashCannotParse(t *testing.T) {
	exec, state := testExecutor(t)
	marker := filepath.Join(t.TempDir(), "ran")

	// The first line is valid and has an effect. If the preflight is not
	// happening, bash runs it before meeting the unterminated `if`, and
	// the marker appears.
	body := fmt.Sprintf("touch %s\nif true\n", marker)

	_, err := exec.Execute(context.Background(), scriptRequest("run-1", "step-1", body), &collector{})
	if err == nil {
		t.Fatal("a script bash cannot parse was executed")
	}
	if !IsCode(err, CodeSyntax) {
		t.Fatalf("a script that does not parse was not refused as a syntax error: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("the script's first line ran, so the syntax check happened after execution rather than before it")
	}
	// The check is a container of its own, and no hook container was
	// ever created: a runner that ran the check inside the hook's own
	// launch would have had to start the hook to find out.
	if created := dockerCallsWithVerb(t, state, "create"); len(created) != 0 {
		t.Errorf("a hook container was created for a script that does not parse: %v", created)
	}
}

// TestExecute_ASyntaxCheckFailureIsNotAnExecFailure is the distinction
// that the container adds a new way to get wrong.
//
// `docker run` answers 125 when it could not create the container and
// 126/127 when it could not run the entrypoint. All three are non-zero
// exits from the same command that reports a syntax error with a
// non-zero exit, so a runner that read "exit status" as "bash refused
// the script" would tell an operator to fix a hook that is fine --
// while the thing that is actually broken (their image, their daemon)
// goes unmentioned.
func TestExecute_ASyntaxCheckFailureIsNotAnExecFailure(t *testing.T) {
	// A stand-in client that cannot create a container: exit 125 with
	// docker's own wording, which is what an absent image or a bad
	// mount produces in a deployment.
	state := fakeDockerState(t)
	client := filepath.Join(state, "docker")
	body := "#!" + hostBashForFake(t) + "\nprintf 'docker: Error response from daemon: no such image\\n' >&2\nexit 125\n"
	if err := os.WriteFile(client, []byte(body), 0o700); err != nil {
		t.Fatalf("writing the stand-in client: %v", err)
	}

	container := Container{
		Docker: client,
		Image:  "stand-in/hook:test",
		Bash:   Bash{Path: hostBashForFake(t), Version: "stand-in"},
	}
	err := container.SyntaxCheck(context.Background(), []byte("echo hello\n"))
	if err == nil {
		t.Fatal("a check that could not be run was reported as a script that parses")
	}
	if IsCode(err, CodeSyntax) {
		t.Fatalf("a container that could not be created was reported as a syntax error, so an operator is told to edit a correct script: %v", err)
	}
	if !IsCode(err, CodeInternal) {
		t.Fatalf("the failure is neither a syntax refusal nor an internal one, so nothing downstream can branch on it: %v", err)
	}
}

// TestExecute_InjectsNoShellOptions is the promise that the bytes an
// operator wrote are the bytes bash is given.
//
// A runner that helpfully added `set -e` would change the meaning of
// every hook already written, invisibly: the file the author is reading
// would no longer describe what runs. The same goes for -u, pipefail and
// -x, and -x additionally prints every command -- including the
// expansion of a variable holding a credential -- into the captured
// stderr this product stores.
func TestExecute_InjectsNoShellOptions(t *testing.T) {
	exec, _ := testExecutor(t)
	out := &collector{}

	body := `printf 'flags=%s\n' "$-"
printf 'pipefail=%s\n' "$(set -o | grep pipefail | awk '{print $2}')"
false
printf 'still-running\n'
`
	result, err := exec.Execute(context.Background(), scriptRequest("run-1", "step-1", body), out)
	if err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	stdout := out.text(StreamStdout)

	flags := ""
	for _, line := range strings.Split(stdout, "\n") {
		if rest, ok := strings.CutPrefix(line, "flags="); ok {
			flags = rest
		}
	}
	if flags == "" {
		t.Fatalf("the hook did not report its shell flags at all: %q / stderr %q", stdout, out.text(StreamStderr))
	}
	if strings.ContainsAny(flags, "eux") {
		t.Errorf("bash was started with %q: this runner injected errexit, nounset or xtrace into somebody else's script", flags)
	}
	if !strings.Contains(stdout, "pipefail=off") {
		t.Errorf("pipefail is not off, so a pipeline in an existing hook now fails where it used to succeed: %q", stdout)
	}
	if !strings.Contains(stdout, "still-running") {
		t.Errorf("the script stopped at a failing command, which is errexit behaviour the operator did not ask for: %q", stdout)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Errorf("the hook's own exit status was changed by the envelope: %+v", result)
	}
}

// TestExecute_TheHookNeverSeesTheCursedFour is the end-to-end form of the
// environment claim: not that ProcessEnv filters them, but that bash
// cannot see them.
//
// The DOCKER_ name is #865's addition to the same rule, and the reason it
// belongs beside the other four: the block this runner builds becomes the
// docker CLIENT's environment (that is how `--env NAME` gets a value
// without putting it on a command line), so a DOCKER_HOST in a hook's
// environment would aim this runner's own client at a daemon somebody
// else chose.
func TestExecute_TheHookNeverSeesTheCursedFour(t *testing.T) {
	exec, _ := testExecutor(t)
	out := &collector{}

	req := scriptRequest("run-1", "step-1", "printenv | sort\n")
	req.Env = EnvSet{Vars: []EnvVar{
		{Name: "BASH_ENV", Value: "/tmp/preamble.sh"},
		{Name: "ENV", Value: "/tmp/preamble.sh"},
		{Name: "SHELLOPTS", Value: "xtrace"},
		{Name: "BASHOPTS", Value: "expand_aliases"},
		{Name: "DOCKER_HOST", Value: "tcp://attacker.example:2375"},
		{Name: "PGHOST", Value: "db.example"},
	}}

	result, err := exec.Execute(context.Background(), req, out)
	if err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	env := out.text(StreamStdout)

	for _, name := range []string{"BASH_ENV=", "ENV=", "BASHOPTS=", "DOCKER_HOST="} {
		if strings.Contains("\n"+env, "\n"+name) {
			t.Errorf("%s reached the hook's environment:\n%s", name, env)
		}
	}
	// SHELLOPTS is set by bash ITSELF in every shell, so its presence is
	// not the failure -- its VALUE being the operator's is.
	if strings.Contains(env, "SHELLOPTS=xtrace") {
		t.Errorf("the operator's SHELLOPTS reached bash's startup, changing how every captured byte is interpreted:\n%s", env)
	}
	if !strings.Contains(env, "PGHOST=db.example") {
		t.Errorf("a configured variable did not reach the hook:\n%s", env)
	}
	want := []string{"BASHOPTS", "BASH_ENV", "DOCKER_HOST", "ENV", "SHELLOPTS"}
	if strings.Join(result.DroppedEnvNames, ",") != strings.Join(want, ",") {
		t.Errorf("the result reports %v as dropped rather than %v, so the operator is never told", result.DroppedEnvNames, want)
	}
}

// TestExecute_GivesTheHookAPrivateWorkingDirectoryAndTakesItAway covers
// three of #809's requirements at once, because they are one behaviour:
// the directory exists, only this account can read it, and it is gone
// afterwards.
//
// Since #865 it is also the mount: BACKUPD_WORK_DIR is the same string
// inside the container as outside, because the launch mounts the
// directory at its own path.
func TestExecute_GivesTheHookAPrivateWorkingDirectoryAndTakesItAway(t *testing.T) {
	exec, _ := testExecutor(t)
	out := &collector{}

	// $PWD is compared with -ef rather than as text: macOS resolves
	// /var to /private/var, so a string comparison would be asserting
	// which symbolic links the platform happens to have rather than
	// which directory the hook started in.
	body := `printf 'work=%s\n' "$BACKUPD_WORK_DIR"
printf 'pwd-is-work=%s\n' "$([ "$PWD" -ef "$BACKUPD_WORK_DIR" ] && echo yes || echo no)"
printf 'mode=%s\n' "$(stat -c %a "$BACKUPD_WORK_DIR" 2>/dev/null || stat -f %Lp "$BACKUPD_WORK_DIR")"
echo evidence > "$BACKUPD_WORK_DIR/dump.sql"
`
	if _, err := exec.Execute(context.Background(), scriptRequest("run-1", "step-1", body), out); err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	stdout := out.text(StreamStdout)

	wantDir, err := exec.Layout.StepWorkDir("run-1", "step-1")
	if err != nil {
		t.Fatalf("deriving the expected working directory: %v", err)
	}
	if !strings.Contains(stdout, "work="+wantDir+"\n") {
		t.Errorf("BACKUPD_WORK_DIR is not the per-step directory this runner created (%s):\n%s", wantDir, stdout)
	}
	if !strings.Contains(stdout, "pwd-is-work=yes\n") {
		t.Errorf("the hook did not START in its own working directory, so a script writing a relative path writes it somewhere nobody cleans up:\n%s", stdout)
	}
	if !strings.Contains(stdout, "mode=700\n") {
		t.Errorf("the working directory is not 0700. On the platforms this product targets the service account shares a group with other users, so anything group-readable is a hook's database dump other accounts can read:\n%s", stdout)
	}
	assertNothingLeftBehind(t, exec.Layout, "run-1", "step-1")
}

// TestExecute_TheHookCannotRewriteTheScriptBashIsReading is why the
// script file is a sibling of the working directory rather than a file
// inside it.
//
// bash reads a script incrementally. A script inside the directory its
// own hook is writing into is a script whose second half can be replaced
// while the first half is still running -- which would make the sha256
// this runner just verified a statement about bytes that no longer
// execute. The container adds the second lock: the script is mounted
// read-only, so even the same uid cannot write it.
func TestExecute_TheHookCannotRewriteTheScriptBashIsReading(t *testing.T) {
	exec, state := testExecutor(t)
	out := &collector{}

	body := `ls -a "$BACKUPD_WORK_DIR"
`
	if _, err := exec.Execute(context.Background(), scriptRequest("run-1", "step-1", body), out); err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	for _, entry := range strings.Fields(out.text(StreamStdout)) {
		if entry != "." && entry != ".." {
			t.Errorf("the hook's working directory is not empty: it contains %q, and anything this runner puts in there is something the hook can edit", entry)
		}
	}

	scriptPath, err := exec.Layout.StepScriptPath("run-1", "step-1")
	if err != nil {
		t.Fatalf("deriving the script path: %v", err)
	}
	if mounted := dockerCallsMatching(t, state, "--volume "+scriptPath+":"+scriptPath+":ro"); len(mounted) == 0 {
		t.Errorf("the captured script was not mounted read-only:\n%s", strings.Join(dockerCalls(t, state), "\n"))
	}
}

// TestExecute_StdoutAndStderrStayApartAndShareOneCounter is the capture
// contract agreed with core/internal/remoteexec (#810).
//
// Apart, because the journal records them as two references and a merged
// text cannot be split again. One counter, because keeping them apart
// otherwise loses the only thing a human reading a failed hook wants:
// whether the warning came before or after the line that looks like the
// cause.
//
// The launch is what makes this possible in the container era: no PTY is
// requested anywhere, so docker demultiplexes the two streams onto the
// client's own two pipes. A `-t` would merge them irrecoverably, which is
// why the capability probe refuses a terminal.
func TestExecute_StdoutAndStderrStayApartAndShareOneCounter(t *testing.T) {
	exec, _ := testExecutor(t)
	out := &collector{}

	body := `echo to-stdout
echo to-stderr >&2
`
	if _, err := exec.Execute(context.Background(), scriptRequest("run-1", "step-1", body), out); err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	if got := strings.TrimSpace(out.text(StreamStdout)); got != "to-stdout" {
		t.Errorf("stdout carried %q", got)
	}
	if got := strings.TrimSpace(out.text(StreamStderr)); got != "to-stderr" {
		t.Errorf("stderr carried %q", got)
	}

	seen := map[uint64]bool{}
	var last uint64
	for _, chunk := range out.chunks {
		if chunk.Seq == 0 {
			t.Errorf("a chunk was delivered with sequence 0, so there is no way to order it")
		}
		if seen[chunk.Seq] {
			t.Errorf("sequence %d was used twice, so the two streams are being numbered independently and the numbers mean nothing across them", chunk.Seq)
		}
		if chunk.Seq <= last {
			t.Errorf("chunk %d was delivered after chunk %d: the numbering is done under a lock and the delivery is not, so the order the number records is not the order anything receives", chunk.Seq, last)
		}
		seen[chunk.Seq] = true
		last = chunk.Seq
	}
}

// TestExecute_TimeoutStopsTheContainerAndEverythingInIt is the
// termination claim, and the child is the whole point of it.
//
// Killing the hook kills the hook. A hook that ran `pg_dump | gzip > x`
// leaves both halves running, still holding the database connection the
// timeout existed to release. Stopping the CONTAINER takes everything the
// hook started with it, with no walk of a process tree and no window in
// which a new child appears between the walk and the kill.
func TestExecute_TimeoutStopsTheContainerAndEverythingInIt(t *testing.T) {
	exec, state := testExecutor(t)
	evidence := t.TempDir()

	body := fmt.Sprintf(`( sleep 3; touch %s/child-survived ) >/dev/null 2>&1 &
touch %s/started
sleep 30
`, evidence, evidence)

	req := scriptRequest("run-1", "step-1", body)
	req.TimeoutMS = 300

	started := time.Now()
	result, err := exec.Execute(context.Background(), req, &collector{})
	if err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	if result.State != StateTimedOut {
		t.Fatalf("a hook that outlived its timeout was reported as %q", result.State)
	}
	if result.ExitCode != nil {
		t.Errorf("a killed hook was given exit code %d. Nil and 0 are not the same answer, and internal/workflow's journal stores this one.", *result.ExitCode)
	}
	if result.TerminationCertainty != CertaintyConfirmed {
		t.Errorf("the runner could not prove the container was gone: %q", result.TerminationCertainty)
	}
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Errorf("the timeout took %s to take effect", elapsed)
	}
	if signals := dockerCallsMatching(t, state, "kill", "--signal=TERM"); len(signals) == 0 {
		t.Errorf("the container was never signalled, so a hook that traps SIGTERM to release a lock never got the chance:\n%s", strings.Join(dockerCalls(t, state), "\n"))
	}
	if records := containerRecords(t, state); len(records) != 0 {
		t.Errorf("the killed container is still there: %v", records)
	}

	waitForFile(t, filepath.Join(evidence, "started"), 5*time.Second)
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(filepath.Join(evidence, "child-survived")); err == nil {
		t.Error("the background process the hook started outlived the container, so only the hook's own process was signalled and a pipeline would still be holding whatever it held")
	}
	assertNothingLeftBehind(t, exec.Layout, "run-1", "step-1")
}

// TestExecute_EscalatesToAKillWhenTheHookIgnoresTheStop is the grace
// period doing its job and then ending.
//
// A hook that traps SIGTERM is the reason the signal is sent first at
// all: it releases a lock, unmounts a snapshot, finishes a write. A hook
// that traps it and never exits is the reason the grace period is
// enforced rather than waited on, and the escalation is asserted from the
// call log because the two signals are the whole behaviour.
func TestExecute_EscalatesToAKillWhenTheHookIgnoresTheStop(t *testing.T) {
	exec, state := testExecutor(t)
	exec.Grace = 300 * time.Millisecond
	evidence := t.TempDir()

	body := fmt.Sprintf(`trap '' TERM
touch %s/started
while :; do sleep 0.05; done
`, evidence)

	req := scriptRequest("run-kill", "step-kill", body)
	req.TimeoutMS = 400

	result, err := exec.Execute(context.Background(), req, &collector{})
	if err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	if result.State != StateTimedOut {
		t.Fatalf("the hook was reported as %q rather than timed out", result.State)
	}
	calls := dockerCalls(t, state)
	term, kill := -1, -1
	for i, call := range calls {
		if strings.HasPrefix(call, "kill ") && strings.Contains(call, "--signal=TERM") && term < 0 {
			term = i
		}
		if strings.HasPrefix(call, "kill ") && strings.Contains(call, "--signal=KILL") && kill < 0 {
			kill = i
		}
	}
	if term < 0 || kill < 0 || !(term < kill) {
		t.Fatalf("the escalation did not happen in order (TERM at %d, KILL at %d):\n%s", term, kill, strings.Join(calls, "\n"))
	}
	if result.TerminationCertainty != CertaintyConfirmed {
		t.Errorf("the runner reported %q for a container it killed and removed", result.TerminationCertainty)
	}
	if records := containerRecords(t, state); len(records) != 0 {
		t.Errorf("a container survived the step: %v", records)
	}
	assertNothingLeftBehind(t, exec.Layout, "run-kill", "step-kill")
}

// TestExecute_CancellationStopsAndRemovesTheContainerAndSaysSo covers the
// explicit cancel and the lease expiry, which are the same kill with two
// different stories. The cause is what tells them apart in the journal.
//
// The removal is the part #865 added to this test, and it is the lease
// guarantee's second half: an engine that died mid-hook must leave no
// running container AND no stopped one. A leftover would take the step's
// container name with it, so the next attempt at the same step could not
// even start.
func TestExecute_CancellationStopsAndRemovesTheContainerAndSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  State
	}{
		{"an operator or the engine cancelling", context.Canceled, StateCanceled},
		{"the engine's lease expiring", errLeaseExpired, StateLeaseExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec, state := testExecutor(t)
			evidence := t.TempDir()
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)

			body := fmt.Sprintf("touch %s/started\nsleep 30\n", evidence)
			go func() {
				waitForFileEventually(filepath.Join(evidence, "started"), 5*time.Second)
				cancel(tc.cause)
			}()

			result, err := exec.Execute(ctx, scriptRequest("run-1", "step-1", body), &collector{})
			if err != nil {
				t.Fatalf("running the hook: %v", err)
			}
			if result.State != tc.want {
				t.Errorf("the outcome is %q rather than %q, so the journal cannot tell an operator's cancel from an engine that died", result.State, tc.want)
			}
			if result.TerminationCertainty != CertaintyConfirmed {
				t.Errorf("termination was not proved: %q", result.TerminationCertainty)
			}
			if records := containerRecords(t, state); len(records) != 0 {
				t.Errorf("the hook's container outlived the step: %v. An engine that went away must leave no runaway and no leftover", records)
			}
			if removed := dockerCallsWithVerb(t, state, "rm", containerNamePrefix); len(removed) == 0 {
				t.Errorf("the container was never removed by name:\n%s", strings.Join(dockerCalls(t, state), "\n"))
			}
		})
	}
}

// TestExecute_RefusesASecondRunOfTheSameStepInPlace is what the script
// file's O_EXCL buys.
//
// The same run and step executing twice at once means two hooks, two
// containers and one working directory, and the second one silently
// overwriting the first is exactly the kind of thing that is discovered
// months later in a dump that is half of one database and half of
// another.
func TestExecute_RefusesASecondRunOfTheSameStepInPlace(t *testing.T) {
	exec, _ := testExecutor(t)
	evidence := t.TempDir()

	body := fmt.Sprintf("touch %s/started\nsleep 5\n", evidence)
	first := make(chan error, 1)
	go func() {
		_, err := exec.Execute(context.Background(), scriptRequest("run-1", "step-1", body), &collector{})
		first <- err
	}()
	waitForFile(t, filepath.Join(evidence, "started"), 5*time.Second)

	_, err := exec.Execute(context.Background(), scriptRequest("run-1", "step-1", "echo second\n"), &collector{})
	if err == nil {
		t.Fatal("a second execution of the same run and step was started over the top of the first")
	}
	if !IsCode(err, CodeInternal) && !IsCode(err, CodeBusy) {
		t.Errorf("the second execution failed for an unrelated reason: %v", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("the first execution did not finish cleanly: %v", err)
	}
}

// assertNothingLeftBehind checks a step left no working directory and no
// script file.
func assertNothingLeftBehind(t *testing.T, layout Layout, runID, stepID string) {
	t.Helper()

	workDir, err := layout.StepWorkDir(runID, stepID)
	if err == nil {
		if _, statErr := os.Stat(workDir); statErr == nil {
			t.Errorf("the per-step working directory %s survived the step", workDir)
		}
	}
	scriptPath, err := layout.StepScriptPath(runID, stepID)
	if err == nil {
		if _, statErr := os.Stat(scriptPath); statErr == nil {
			t.Errorf("the captured script %s survived the step, so a copy of an operator's hook is accumulating under the runtime directory", scriptPath)
		}
	}
}

func waitForFileEventually(path string, within time.Duration) {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestExecute_FollowsNoSymbolicLinkUnderTheWorkspace is the traversal
// case the id validation does NOT cover.
//
// ValidID stops a run id from being "../../etc". It says nothing about
// what is already AT <workflow>/<run id>: a symbolic link planted there
// points a path-based MkdirAll, OpenFile and RemoveAll at whatever it
// names, and the process doing the creating and the recursive removing
// is this one. The workspace is deliberately outside every container
// mount so that nothing the engine can write is on this path at all, and
// this is the second half of that argument -- the descriptors, so that a
// link which somehow appeared is refused rather than followed.
//
// Both directions are covered: a link out of the workspace, and a link
// that stays inside it. Neither is something this design ever creates.
func TestExecute_FollowsNoSymbolicLinkUnderTheWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		external bool
	}{
		{"a link out of the workspace", true},
		{"a link to a sibling inside the workspace", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor, _ := testExecutor(t)
			root := executor.Layout.WorkflowRoot()
			if err := EnsureDir(root); err != nil {
				t.Fatalf("preparing the workflow root: %v", err)
			}

			target := filepath.Join(t.TempDir(), "somewhere-else")
			if !tc.external {
				target = filepath.Join(root, "another-run")
			}
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatalf("preparing the link's target: %v", err)
			}
			precious := filepath.Join(target, "precious")
			if err := os.WriteFile(precious, []byte("evidence"), 0o600); err != nil {
				t.Fatalf("preparing the file the link points at: %v", err)
			}

			link := filepath.Join(root, "run-planted")
			if err := os.Symlink(target, link); err != nil {
				t.Fatalf("planting the symbolic link: %v", err)
			}

			_, err := executor.Execute(context.Background(),
				scriptRequest("run-planted", "step-1", "echo hello\n"), &collector{})
			if err == nil {
				t.Fatal("a step whose run directory was a symbolic link ran, so this runner created a working directory and an executable file somewhere it did not choose -- and then handed that path to the daemon as a bind mount")
			}

			entries, readErr := os.ReadDir(target)
			if readErr != nil {
				t.Fatalf("reading the link's target: %v", readErr)
			}
			if len(entries) != 1 {
				t.Errorf("the runner created %d entries through the symbolic link: %v", len(entries)-1, entries)
			}

			// The cleanup half. Creating through a link is one failure;
			// a best-effort RemoveAll following the same link is the
			// one that deletes somebody's directory.
			executor.cleanupStep("run-planted", "step-1")
			if _, err := os.Stat(precious); err != nil {
				t.Errorf("the cleanup removed %s through the symbolic link: %v", precious, err)
			}
		})
	}
}

// TestExecute_RefusesToMountAWorkingDirectoryThatBecameALink is the
// container-era half of the same argument, and it is a different check
// from the one above.
//
// paths.go creates the step's directories through descriptors that
// cannot be walked out of the workspace. What is handed to the DAEMON,
// though, is a string -- and the daemon resolves it itself, as root, with
// none of that discipline. A link that appeared between the creation and
// the launch would therefore be a mount of wherever it points, read-write,
// into a container running somebody's script.
func TestExecute_RefusesToMountAWorkingDirectoryThatBecameALink(t *testing.T) {
	executor, state := testExecutor(t)
	elsewhere := t.TempDir()

	// The step is prepared the way Execute prepares one, and then its
	// working directory is replaced by a link, which is the window this
	// check closes.
	if _, _, err := executor.Layout.prepareStep("run-swap", "step-swap", []byte("echo hello\n")); err != nil {
		t.Fatalf("preparing the step: %v", err)
	}
	workDir, err := executor.Layout.StepWorkDir("run-swap", "step-swap")
	if err != nil {
		t.Fatalf("deriving the working directory: %v", err)
	}
	if err := os.Remove(workDir); err != nil {
		t.Fatalf("removing the real working directory: %v", err)
	}
	if err := os.Symlink(elsewhere, workDir); err != nil {
		t.Fatalf("planting the link: %v", err)
	}

	if err := refuseLink(workDir); err == nil {
		t.Fatal("a working directory that is a symbolic link was accepted as a bind mount source")
	} else if !IsCode(err, CodeRefused) {
		t.Fatalf("the refusal is not a refusal: %v", err)
	}
	if created := dockerCallsWithVerb(t, state, "create"); len(created) != 0 {
		t.Errorf("a hook container was created anyway: %v", created)
	}
}

// TestExecute_KeepsTheWorkingDirectoryWhenTerminationCannotBeProved is
// the other side of the certainty invariant, on the path that used to
// ignore it.
//
// A daemon that will not let go of the container -- one that is wedged,
// or restarting, or whose task is in uninterruptible I/O -- is the case
// "unconfirmed" exists for. Something may still be writing in the
// working directory, so removing it would turn "a hook survived its own
// kill" into "half a dump, in a directory nobody can find", which is
// precisely the forensic case this answer is reported for.
func TestExecute_KeepsTheWorkingDirectoryWhenTerminationCannotBeProved(t *testing.T) {
	container, state := fakeCapability(t, fakeDocker{sticky: true})
	// The confirmation window is what this test waits out, so it is
	// shortened rather than endured: the behaviour under assertion is
	// the ANSWER after the window, not its length.
	executor := &Executor{
		Layout:        testLayout(t),
		Container:     container,
		Grace:         100 * time.Millisecond,
		ConfirmWindow: 500 * time.Millisecond,
	}
	evidence := t.TempDir()

	body := fmt.Sprintf("touch %s/started\nsleep 30\n", evidence)
	req := scriptRequest("run-keep", "step-keep", body)
	req.TimeoutMS = 300

	result, err := executor.Execute(context.Background(), req, &collector{})
	if err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	if result.TerminationCertainty != CertaintyUnconfirmed {
		t.Fatalf("a container the daemon still knows about was reported as %q", result.TerminationCertainty)
	}
	if result.WorkDirRemoved {
		t.Error("the result claims the working directory was removed after a termination that could not be proved")
	}
	workDir, dirErr := executor.Layout.StepWorkDir("run-keep", "step-keep")
	if dirErr != nil {
		t.Fatalf("deriving the working directory: %v", dirErr)
	}
	if _, statErr := os.Stat(workDir); statErr != nil {
		t.Errorf("%s was removed after a termination this runner could not prove: %v. Something may still be writing in there, and the operator has nothing left to look at", workDir, statErr)
	}
	if records := containerRecords(t, state); len(records) == 0 {
		t.Error("this test asserted nothing: the stand-in daemon did let go of the container, so the unconfirmed path was never taken")
	}
}

// TestExecute_ADaemonThatCannotBeAskedIsNotAConfirmedKill is the one
// asymmetry in the certainty logic, and the one most easily written the
// wrong way round.
//
// `docker inspect` on a missing container and `docker inspect` against a
// dead daemon both fail. A runner that read "the question failed" as "the
// container is gone" would record every termination taken during a daemon
// restart as a clean kill -- which is the exact case where a hook is most
// likely to still be running.
func TestExecute_ADaemonThatCannotBeAskedIsNotAConfirmedKill(t *testing.T) {
	container, _ := fakeCapability(t, fakeDocker{daemonGoneAfterStart: true})
	executor := &Executor{
		Layout:        testLayout(t),
		Container:     container,
		Grace:         100 * time.Millisecond,
		ConfirmWindow: 500 * time.Millisecond,
	}

	req := scriptRequest("run-blind", "step-blind", "sleep 30\n")
	req.TimeoutMS = 300

	result, err := executor.Execute(context.Background(), req, &collector{})
	if err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	if result.TerminationCertainty != CertaintyUnconfirmed {
		t.Fatalf("a termination nobody could ask about was reported as %q", result.TerminationCertainty)
	}
}

// TestExecute_RemovesTheWorkingDirectoryWhenOnlyTheStreamWasLost is the
// contrast that keeps the two tests above honest.
//
// A lost stream is not by itself a reason to keep a directory: the hook
// ran to completion and nothing was signalled, so there is nothing that
// could still be writing. A "keep it whenever anything failed" rule would
// pass those tests and accumulate a working directory per failed
// connection forever.
func TestExecute_RemovesTheWorkingDirectoryWhenOnlyTheStreamWasLost(t *testing.T) {
	executor, _ := testExecutor(t)
	sink := SinkFunc(func(Chunk) error { return errors.New("the engine is no longer reading") })

	_, err := executor.Execute(context.Background(),
		scriptRequest("run-lost", "step-lost", "echo streaming\n"), sink)
	if err == nil {
		t.Fatal("a step whose output never reached the engine was reported as a clean run")
	}
	assertNothingLeftBehind(t, executor.Layout, "run-lost", "step-lost")
}

// TestExecute_DeliversValuesToTheHookByteForByte is the environment's
// central claim, asserted from inside the hook rather than about the
// block this package builds.
//
// The values below are the ones a shell would mangle: a command
// substitution, a semicolon and quotes, an embedded newline, and UTF-8.
// If any of them ever reached bash as part of a rendered `export` line --
// which is how every "run this remotely" implementation eventually gets
// written -- the substitution would EXECUTE, and the marker file it
// creates would exist. The launch passes `--env NAME` instead, so the
// value travels from this process's own block to the daemon and is never
// text a shell reads. What the hook sees is what the engine sent, byte
// for byte, and the hash is what says so: a comparison of the text would
// be a comparison of two things this test wrote, while a sha256 taken
// inside the hook cannot be accidentally right.
func TestExecute_DeliversValuesToTheHookByteForByte(t *testing.T) {
	executor, state := testExecutor(t)
	marker := filepath.Join(t.TempDir(), "substitution-ran")

	values := map[string]string{
		"BACKUPD_TEST_SUBSTITUTION": "$(touch " + marker + ")`touch " + marker + "`",
		"BACKUPD_TEST_QUOTES":       `he said "hi"; rm -rf /; '\''`,
		"BACKUPD_TEST_NEWLINE":      "first\nsecond\ttab\\",
		"BACKUPD_TEST_UTF8":         "café — 日本語 — Ω — 🔒",
	}
	names := make([]string, 0, len(values))
	vars := make([]EnvVar, 0, len(values))
	for _, name := range []string{"BACKUPD_TEST_SUBSTITUTION", "BACKUPD_TEST_QUOTES", "BACKUPD_TEST_NEWLINE", "BACKUPD_TEST_UTF8"} {
		names = append(names, name)
		vars = append(vars, EnvVar{Name: name, Value: values[name]})
	}

	// sha256sum on Linux, shasum on macOS: one of the two is on every
	// platform this product supports, and a hook that could find
	// neither would be asserting about the test host rather than about
	// the runner, so it says so.
	body := `sha() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 | cut -d' ' -f1
  else echo "no sha256 tool on this host" >&2; exit 3
  fi
}
for name in ` + strings.Join(names, " ") + `; do
  printf '%s=%s\n' "$name" "$(printf '%s' "${!name}" | sha)"
done
`
	req := scriptRequest("run-env", "step-env", body)
	req.Env = EnvSet{Vars: vars}

	out := &collector{}
	result, err := executor.Execute(context.Background(), req, out)
	if err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("the hook did not succeed: %+v, stderr %q", result, out.text(StreamStderr))
	}

	stdout := out.text(StreamStdout)
	for _, name := range names {
		sum := sha256.Sum256([]byte(values[name]))
		want := name + "=" + hex.EncodeToString(sum[:]) + "\n"
		if !strings.Contains(stdout, want) {
			t.Errorf("the hook read a different %s than the engine sent.\nwant %s got:\n%s", name, want, stdout)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("%s exists: a value was interpreted by a shell somewhere between the request and the hook, so the environment is being rendered as text rather than passed to the daemon", marker)
	}

	// And the other half of the same property: not one of those values
	// is anywhere in the argument vector, because `ps` on a NAS is
	// readable by every account and these are passwords.
	for _, call := range dockerCalls(t, state) {
		for name, value := range values {
			if strings.Contains(call, value) {
				t.Errorf("the value of %s reached a command line: %q", name, call)
			}
		}
	}
}

// TestSyntaxCheck_ACancelledCheckIsNotASyntaxRefusal separates "bash read
// these bytes and refused them" from "this runner stopped waiting".
//
// The check runs under a context, and a cancelled command exits non-zero
// with nothing on stderr -- which looks exactly like a refusal to the
// branch that maps an exit status to CodeSyntax. The runner shutting
// down, or an engine hanging up mid-validation, would therefore tell an
// operator that a perfectly valid hook "does not parse", and that is a
// sentence they will act on by editing a correct script.
func TestSyntaxCheck_ACancelledCheckIsNotASyntaxRefusal(t *testing.T) {
	// A stand-in client that is still running when the context is
	// cancelled, which a real `docker run` of `bash -n` finishes far
	// too quickly to be.
	slow := filepath.Join(t.TempDir(), "slow-docker")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nexec sleep 5\n"), 0o700); err != nil {
		t.Fatalf("writing the stand-in client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	container := Container{
		Docker: slow,
		Image:  "stand-in/hook:test",
		Bash:   Bash{Path: "/bin/bash", Version: "stand-in"},
	}
	err := container.SyntaxCheck(ctx, []byte("echo hello\n"))
	if err == nil {
		t.Fatal("a check that never completed was reported as a script that parses")
	}
	if IsCode(err, CodeSyntax) {
		t.Fatalf("a cancelled check was reported as a syntax error, so a valid hook is refused because the runner was stopping: %v", err)
	}
	if !IsCode(err, CodeInternal) {
		t.Fatalf("the failure is neither a syntax refusal nor an internal one, so nothing downstream can branch on it: %v", err)
	}
}

// --- the container's identity, and the cross-kill it used to allow -------

// TestContainerName_KeepsTheUniqueSuffixWhateverTheIdsAre is the
// truncation bug in its smallest form.
//
// Verify accepts a run id and a step id of MaxIDLength each, so the two
// of them plus the prefix are past docker's limit on their own. A name
// cut at the END loses the random token -- the ONLY unique part of it --
// so two concurrent steps whose ids share a long prefix ended up asking
// the daemon for the same container. That is not a cosmetic collision:
// the second `docker create` fails, and the failed attempt's own cleanup
// then removes the container the first step is still running in.
func TestContainerName_KeepsTheUniqueSuffixWhateverTheIdsAre(t *testing.T) {
	long := strings.Repeat("a", MaxIDLength-6)
	runID := long + "-run-1"
	stepID := long + "-step-2"
	other := long + "-step-3"

	first := containerName(runID, stepID, "0123456789abcdef")
	second := containerName(runID, other, "fedcba9876543210")

	for _, name := range []string{first, second} {
		if len(name) > maxContainerNameLength {
			t.Fatalf("the name is %d bytes, past the %d this runner holds itself to: %q", len(name), maxContainerNameLength, name)
		}
		if !strings.HasPrefix(name, containerNamePrefix) {
			t.Fatalf("the name is not this runner's: %q", name)
		}
	}
	if !strings.HasSuffix(first, "0123456789abcdef") || !strings.HasSuffix(second, "fedcba9876543210") {
		t.Fatalf("the token was truncated away, so the name is derived rather than unique:\n%q\n%q", first, second)
	}
	if first == second {
		t.Fatalf("two different steps got the same container name: %q", first)
	}
	// And the legible part still differs, so an operator reading
	// `docker ps` can tell two long-id steps apart.
	if strings.TrimSuffix(first, "0123456789abcdef") == strings.TrimSuffix(second, "fedcba9876543210") {
		t.Fatalf("the two names differ only in their token: %q vs %q", first, second)
	}
}

// TestExecute_TwoConcurrentStepsWithLongSharedIdsNeitherCollideNorKillEachOther
// is the same fault measured where it did its damage.
//
// Both steps run at once, both ids are as long as this runner accepts and
// agree for all but their last characters. Pre-fix they minted ONE name:
// the loser's `docker create` failed with a name conflict, and its
// cleanup removed that name's container -- so the winner's hook was
// killed by the other step's failure. Both hooks running to completion is
// the whole assertion.
func TestExecute_TwoConcurrentStepsWithLongSharedIdsNeitherCollideNorKillEachOther(t *testing.T) {
	exec, state := testExecutor(t)
	evidence := t.TempDir()
	long := strings.Repeat("s", MaxIDLength-8)

	type outcome struct {
		step   string
		result Result
		err    error
	}
	outcomes := make(chan outcome, 2)
	for _, step := range []string{long + "-step-1", long + "-step-2"} {
		go func(step string) {
			body := fmt.Sprintf("touch %s/%s.started\nsleep 1\nprintf ok > %s/%s.done\n",
				evidence, step[len(step)-6:], evidence, step[len(step)-6:])
			result, err := exec.Execute(context.Background(),
				scriptRequest(long+"-run", step, body), &collector{})
			outcomes <- outcome{step: step, result: result, err: err}
		}(step)
	}

	for i := 0; i < 2; i++ {
		got := <-outcomes
		if got.err != nil {
			t.Fatalf("%s could not run at all: %v. Two concurrent steps with long ids asked the daemon for the same container", got.step[len(got.step)-6:], got.err)
		}
		if got.result.ExitCode == nil || *got.result.ExitCode != 0 {
			t.Fatalf("%s did not finish: %+v. A step killed by the other step's cleanup looks exactly like this", got.step[len(got.step)-6:], got.result)
		}
	}
	for _, marker := range []string{"step-1.done", "step-2.done"} {
		if _, err := os.Stat(filepath.Join(evidence, marker)); err != nil {
			t.Fatalf("%s was never written, so that hook did not run to the end: %v", marker, err)
		}
	}
	// Two creations, two distinct names: the collision is what the
	// truncation produced, and this is it not happening.
	created := dockerCallsWithVerb(t, state, "create")
	if len(created) != 2 {
		t.Fatalf("there were not two creations: %v", created)
	}
	if names := containerNamesIn(created); len(names) != 2 {
		t.Fatalf("the two steps asked for the same container name: %v", names)
	}
}

// containerNamesIn collects the distinct --name values out of call-log
// lines.
func containerNamesIn(calls []string) map[string]bool {
	names := map[string]bool{}
	for _, call := range calls {
		fields := strings.Fields(call)
		for i, field := range fields {
			if field == "--name" && i+1 < len(fields) {
				names[fields[i+1]] = true
			}
		}
	}
	return names
}

// --- creation, termination and the race between them --------------------

// TestExecute_ACancelDuringCreationLeavesNoRunaway is the create/terminate
// race, reproduced rather than argued about.
//
// The stand-in daemon here accepts the creation and completes it a second
// LATER, from a writer the client's death does not touch, while the
// client itself hangs -- which is what a cancel landing inside a launch
// really looks like on a loaded host. The version of this runner that
// signalled the name it minted, once, got "no such container" twice and
// returned; the container appeared afterwards and nothing on the host
// knew its name.
//
// The reconciliation is by LABEL and watches for the whole confirmation
// window, so the container that appears late is still found, killed and
// removed.
func TestExecute_ACancelDuringCreationLeavesNoRunaway(t *testing.T) {
	container, state := fakeCapability(t, fakeDocker{createLatencySeconds: "1"})
	exec := &Executor{
		Layout:         testLayout(t),
		Container:      container,
		Grace:          200 * time.Millisecond,
		ConfirmWindow:  3 * time.Second,
		ControlTimeout: 5 * time.Second,
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel(errLeaseExpired)
	}()

	started := time.Now()
	result, err := exec.Execute(ctx, scriptRequest("run-late", "step-late", "sleep 30\n"), &collector{})
	if err != nil {
		t.Fatalf("a cancel during creation was reported as a failure rather than as a cancel: %v", err)
	}
	if result.State != StateLeaseExpired {
		t.Fatalf("the outcome is %q rather than the lease expiring, so the journal cannot say why the step ended", result.State)
	}
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("the cancel took %s to be answered", elapsed)
	}
	if result.TerminationCertainty != CertaintyConfirmed {
		t.Errorf("the runner could not prove the late container was gone: %q", result.TerminationCertainty)
	}
	if records := containerRecords(t, state); len(records) != 0 {
		t.Fatalf("a container created after the cancel is still there: %v. That is the runaway: nothing on this host now knows what it is", records)
	}
	if killed := dockerCallsWithVerb(t, state, "rm"); len(killed) == 0 {
		t.Errorf("nothing was ever removed, so the reconciliation never found the container:\n%s", strings.Join(dockerCalls(t, state), "\n"))
	}
	// By label, which is the only question that can find a container
	// whose name this runner never saw registered.
	if byLabel := dockerCallsWithVerb(t, state, "ps", "label="+LabelInstance+"="); len(byLabel) == 0 {
		t.Errorf("the reconciliation never asked the daemon what it had by label:\n%s", strings.Join(dockerCalls(t, state), "\n"))
	}
}

// TestExecute_AWedgedDaemonEndsTheStepWithinTheBoundAndSaysSo is the
// other half of the same requirement: a daemon that has stopped
// answering must cost this runner CERTAINTY, never its ability to
// return.
//
// Every docker call here blocks forever once the hook has started, and
// the attached client blocks with them -- which is the case that used to
// hang: the wait for that client had no bound at all, so a lost lease or
// a shutdown waited for a daemon that was never going to answer. The
// step must end inside the bound, report the termination as unconfirmed
// and keep the working directory, because something may still be running
// in it.
func TestExecute_AWedgedDaemonEndsTheStepWithinTheBoundAndSaysSo(t *testing.T) {
	container, state := fakeCapability(t, fakeDocker{hangAfterStart: true})
	layout := testLayout(t)
	exec := &Executor{
		Layout:         layout,
		Container:      container,
		Grace:          200 * time.Millisecond,
		ConfirmWindow:  700 * time.Millisecond,
		ControlTimeout: 300 * time.Millisecond,
	}

	evidence := t.TempDir()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go func() {
		waitForFileEventually(filepath.Join(evidence, "started"), 5*time.Second)
		cancel(errLeaseExpired)
	}()

	started := time.Now()
	result, err := exec.Execute(ctx,
		scriptRequest("run-wedged", "step-wedged", fmt.Sprintf("touch %s/started\nsleep 30\n", evidence)),
		&collector{})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("the step was reported as a failure rather than as a lost lease: %v", err)
	}
	// Generous, and still a bound: the point is that the answer does not
	// wait for a daemon that never replies, not the exact arithmetic.
	if elapsed > 30*time.Second {
		t.Fatalf("the step took %s against a daemon that never answered, so something on the termination path is unbounded", elapsed)
	}
	if result.State != StateLeaseExpired {
		t.Errorf("the outcome is %q rather than the lease expiring", result.State)
	}
	if result.TerminationCertainty != CertaintyUnconfirmed {
		t.Fatalf("termination against a wedged daemon was reported as %q. Nothing proved the container is gone, and claiming it is is how a runaway gets recorded as a clean kill", result.TerminationCertainty)
	}
	if result.WorkDirRemoved {
		t.Error("the working directory was removed although the termination could not be proved")
	}
	if _, err := os.Stat(stepWorkDirOf(t, layout, "run-wedged", "step-wedged")); err != nil {
		t.Errorf("the working directory of an unconfirmed termination is gone: %v. Something may still be writing in there, and it is the only forensic record of what", err)
	}
	if signals := dockerCallsMatching(t, state, "kill", "--signal=TERM"); len(signals) == 0 {
		t.Errorf("the container was never signalled:\n%s", strings.Join(dockerCalls(t, state), "\n"))
	}
}

// TestExecute_AClientThatExitsWhileItsContainerLivesIsNotACleanExit is
// MAJOR 3: the normal path's evidence, which used to be discarded.
//
// The client says the step is over and the container says otherwise -- a
// daemon restart, a dropped transport, a client that was OOM-killed. The
// runner that read the client's exit as the hook's reported a clean
// termination with no certainty at all and DELETED the working
// directory, which is the one place the half-written output of a hook
// that outlived its client is.
//
// Both endings are asserted, because the pair is what keeps either one
// honest: a daemon that lets the container be removed gives a confirmed
// termination, and one that will not gives an unconfirmed one and keeps
// the directory.
func TestExecute_AClientThatExitsWhileItsContainerLivesIsNotACleanExit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sticky    bool
		certainty TerminationCertainty
		kept      bool
	}{
		{"a daemon that lets go", false, CertaintyConfirmed, false},
		{"a daemon that will not let go", true, CertaintyUnconfirmed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			container, _ := fakeCapability(t, fakeDocker{clientExitsEarly: true, sticky: tc.sticky})
			layout := testLayout(t)
			exec := &Executor{
				Layout:         layout,
				Container:      container,
				Grace:          200 * time.Millisecond,
				ConfirmWindow:  500 * time.Millisecond,
				ControlTimeout: 3 * time.Second,
			}

			result, err := exec.Execute(context.Background(),
				scriptRequest("run-orphan", "step-orphan", "sleep 2\n"), &collector{})
			if err != nil {
				t.Fatalf("running the hook: %v", err)
			}
			if result.ExitCode != nil {
				t.Errorf("the hook was given exit code %d, which nothing observed: the client exited and the container was still running", *result.ExitCode)
			}
			if result.TerminationCertainty != tc.certainty {
				t.Fatalf("certainty = %q, want %q", result.TerminationCertainty, tc.certainty)
			}
			_, statErr := os.Stat(stepWorkDirOf(t, layout, "run-orphan", "step-orphan"))
			if tc.kept && statErr != nil {
				t.Errorf("the working directory was removed although the container outlived its client and could not be proved gone: %v", statErr)
			}
			if !tc.kept && statErr == nil {
				t.Error("the working directory survived a termination this runner proved, so the cleanup promise is not kept")
			}
		})
	}
}

// TestExecute_AHookThatExitsWith125IsAHookThatRan is the exit-status
// conflation, which was a lie about the seam rather than a cosmetic
// mistake.
//
// `docker run` answers 125 when it could not create the container AND
// when the container's process returned 125, because the client
// propagates the wait status verbatim. A runner that read the number
// reported a *Failure -- "nothing was attempted" -- for a hook that had
// run, done its work and returned an exit code an operator chose. That
// is a behavioural regression from the host bash this replaced, where
// 125 was just a number, and it is the one fact the workflow engine
// branches on.
func TestExecute_AHookThatExitsWith125IsAHookThatRan(t *testing.T) {
	exec, _ := testExecutor(t)
	evidence := t.TempDir()

	result, err := exec.Execute(context.Background(),
		scriptRequest("run-125", "step-125", fmt.Sprintf("printf ran > %s/it-ran\nexit 125\n", evidence)),
		&collector{})
	if err != nil {
		t.Fatalf("a hook that exited 125 was reported as never having been attempted: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(evidence, "it-ran")); statErr != nil {
		t.Fatalf("the hook did not run at all, so this test is not about what it claims: %v", statErr)
	}
	if result.State != StateExited {
		t.Fatalf("the state is %q rather than exited", result.State)
	}
	if result.ExitCode == nil || *result.ExitCode != 125 {
		t.Fatalf("exit code = %v, want 125: the hook ran and returned it", result.ExitCode)
	}
	if result.TerminationCertainty != CertaintyNotApplicable {
		t.Errorf("a hook that exited on its own was given a termination certainty of %q", result.TerminationCertainty)
	}
}

// TestSyntaxCheck_RemovesTheContainerAutoRemoveDidNotReap is MINOR 7: this
// runner's OWN containers are named, labelled and reconciled too.
//
// The probe and the syntax check are `--rm` runs, and --rm is a promise
// about an EXIT: a run cancelled or timed out between the daemon creating
// the container and starting it leaves a Created container that nothing
// ever reaps. They used to be unnamed children of a CommandContext, so
// there was nothing to reap it BY.
func TestSyntaxCheck_RemovesTheContainerAutoRemoveDidNotReap(t *testing.T) {
	container, state := fakeCapability(t, fakeDocker{autoRemoveFails: true})

	if err := container.SyntaxCheck(context.Background(), []byte("printf ok\n")); err != nil {
		t.Fatalf("checking bytes that parse: %v", err)
	}
	if records := containerRecords(t, state); len(records) != 0 {
		t.Fatalf("a container this runner started is still there: %v. --rm did not happen, and nothing else removed it", records)
	}
	// Named and labelled, which is what made the removal possible.
	named := dockerCallsWithVerb(t, state, "run", containerNamePrefix+"syntax-", "--label "+LabelInstance+"=")
	if len(named) == 0 {
		t.Errorf("the syntax check's container carries no name and no instance label:\n%s", strings.Join(dockerCalls(t, state), "\n"))
	}
	probed := dockerCallsWithVerb(t, state, "run", containerNamePrefix+"probe-", "--label "+LabelInstance+"=")
	if len(probed) == 0 {
		t.Errorf("the capability probe's container carries no name and no instance label:\n%s", strings.Join(dockerCalls(t, state), "\n"))
	}
}

// stepWorkDirOf is StepWorkDir with the id validation already known to
// pass, for the tests that assert on whether the directory survived.
func stepWorkDirOf(t *testing.T, layout Layout, runID, stepID string) string {
	t.Helper()
	dir, err := layout.StepWorkDir(runID, stepID)
	if err != nil {
		t.Fatalf("resolving the step's working directory: %v", err)
	}
	return dir
}
