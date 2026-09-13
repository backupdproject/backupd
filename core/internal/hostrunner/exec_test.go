package hostrunner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestExecute_RunsTheCapturedBytesOnThisHost is #809's first acceptance
// criterion, reduced to the one thing it actually claims.
//
// A hook must run on the HOST -- the machine backupd is installed on --
// rather than inside the distroless engine container, which has no shell
// at all. Inside this process the observable form of that claim is that
// the hook sees this process's own hostname and can read a file only this
// host has.
func TestExecute_RunsTheCapturedBytesOnThisHost(t *testing.T) {
	exec := testExecutor(t)
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
}

// TestExecute_RefusesBytesWhoseHashDoesNotMatchTheClaim is the second
// verification of the same fact, and the reason it is done twice.
//
// internal/workflow verified these bytes against the plan when it opened
// them. Between that check and this one lies a socket, and a check on the
// far side of a boundary is not a check on this side of it.
func TestExecute_RefusesBytesWhoseHashDoesNotMatchTheClaim(t *testing.T) {
	exec := testExecutor(t)
	req := scriptRequest("run-1", "step-1", "echo hello\n")
	req.Script = []byte("rm -rf /\n")

	_, err := exec.Execute(context.Background(), req, &collector{})
	if err == nil {
		t.Fatal("bytes that do not hash to the declared sha256 were executed")
	}
	if !IsCode(err, CodeScriptMismatch) {
		t.Fatalf("the refusal is not a script_mismatch, so the engine cannot tell it from a hook that failed: %v", err)
	}
	assertNothingLeftBehind(t, exec.Layout, "run-1", "step-1")
}

// TestExecute_RefusesBytesWhoseSizeDoesNotMatchTheClaim covers the
// truncation case specifically: bytes that are a prefix of the real
// script still parse, still run, and do half of what the operator wrote.
func TestExecute_RefusesBytesWhoseSizeDoesNotMatchTheClaim(t *testing.T) {
	exec := testExecutor(t)
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
// process creates, chmods and later removes recursively. A run id of
// "../../../etc" is not a hypothetical attack, it is what a bug in a
// caller looks like.
func TestExecute_RefusesAnIdThatWouldEscapeTheRuntimeDirectory(t *testing.T) {
	exec := testExecutor(t)

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
// question without running anything.
func TestExecute_RefusesBytesBashCannotParse(t *testing.T) {
	exec := testExecutor(t)
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
	exec := testExecutor(t)
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
func TestExecute_TheHookNeverSeesTheCursedFour(t *testing.T) {
	exec := testExecutor(t)
	out := &collector{}

	req := scriptRequest("run-1", "step-1", "printenv | sort\n")
	req.Env = EnvSet{Vars: []EnvVar{
		{Name: "BASH_ENV", Value: "/tmp/preamble.sh"},
		{Name: "ENV", Value: "/tmp/preamble.sh"},
		{Name: "SHELLOPTS", Value: "xtrace"},
		{Name: "BASHOPTS", Value: "expand_aliases"},
		{Name: "PGHOST", Value: "db.example"},
	}}

	result, err := exec.Execute(context.Background(), req, out)
	if err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	env := out.text(StreamStdout)

	for _, name := range []string{"BASH_ENV=", "ENV=", "BASHOPTS="} {
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
	if want := []string{"BASHOPTS", "BASH_ENV", "ENV", "SHELLOPTS"}; strings.Join(result.DroppedEnvNames, ",") != strings.Join(want, ",") {
		t.Errorf("the result reports %v as dropped rather than %v, so the operator is never told", result.DroppedEnvNames, want)
	}
}

// TestExecute_GivesTheHookAPrivateWorkingDirectoryAndTakesItAway covers
// three of #809's requirements at once, because they are one behaviour:
// the directory exists, only this account can read it, and it is gone
// afterwards.
func TestExecute_GivesTheHookAPrivateWorkingDirectoryAndTakesItAway(t *testing.T) {
	exec := testExecutor(t)
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
// execute.
func TestExecute_TheHookCannotRewriteTheScriptBashIsReading(t *testing.T) {
	exec := testExecutor(t)
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
}

// TestExecute_StdoutAndStderrStayApartAndShareOneCounter is the capture
// contract agreed with core/internal/remoteexec (#810).
//
// Apart, because the journal records them as two references and a merged
// text cannot be split again. One counter, because keeping them apart
// otherwise loses the only thing a human reading a failed hook wants:
// whether the warning came before or after the line that looks like the
// cause.
func TestExecute_StdoutAndStderrStayApartAndShareOneCounter(t *testing.T) {
	exec := testExecutor(t)
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

// TestExecute_TimeoutKillsWhatTheHookStartedAndNotJustTheHook is the
// process-group claim, and the child is the whole point of it.
//
// Killing the hook kills the hook. A hook that ran `pg_dump | gzip > x`
// leaves both halves running, still holding the database connection the
// timeout existed to release. One signal to the negated process group id
// reaches everything the hook started, with no window in which a new
// child appears between a walk and a kill.
func TestExecute_TimeoutKillsWhatTheHookStartedAndNotJustTheHook(t *testing.T) {
	exec := testExecutor(t)
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
		t.Errorf("the runner could not prove the process group was gone: %q", result.TerminationCertainty)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("the timeout took %s to take effect", elapsed)
	}

	waitForFile(t, filepath.Join(evidence, "started"), 5*time.Second)
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(filepath.Join(evidence, "child-survived")); err == nil {
		t.Error("the background process the hook started outlived the kill, so only the hook's own pid was signalled and a pipeline would still be holding whatever it held")
	}
	assertNothingLeftBehind(t, exec.Layout, "run-1", "step-1")
}

// TestExecute_CancellationTerminatesTheGroupAndSaysSo covers the explicit
// cancel and the lease expiry, which are the same kill with two
// different stories. The cause is what tells them apart in the journal.
func TestExecute_CancellationTerminatesTheGroupAndSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  State
	}{
		{"an operator or the engine cancelling", context.Canceled, StateCanceled},
		{"the engine's lease expiring", errLeaseExpired, StateLeaseExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := testExecutor(t)
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
		})
	}
}

// TestExecute_RefusesASecondRunOfTheSameStepInPlace is what the script
// file's O_EXCL buys.
//
// The same run and step executing twice at once means two hooks, two
// process groups and one working directory, and the second one silently
// overwriting the first is exactly the kind of thing that is discovered
// months later in a dump that is half of one database and half of
// another.
func TestExecute_RefusesASecondRunOfTheSameStepInPlace(t *testing.T) {
	exec := testExecutor(t)
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

// TestFindBash_RefusesAConfiguredInterpreterThatIsNotOne checks the
// preflight refuses rather than falling back.
//
// An operator who named an interpreter and silently got a different one
// is in the worst of both worlds: their hooks work until the day the
// other one is removed.
func TestFindBash_RefusesAConfiguredInterpreterThatIsNotOne(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-bash")

	_, err := FindBash(context.Background(), missing)
	if err == nil {
		t.Fatal("a configured interpreter that does not exist was accepted, so the search silently supplied a different shell")
	}
	if !errors.Is(err, ErrBash) {
		t.Fatalf("the refusal is not an ErrBash: %v", err)
	}

	if _, err := FindBash(context.Background(), "bash"); err == nil {
		t.Fatal("a relative interpreter path was accepted, which makes what runs depend on where this process was started")
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
