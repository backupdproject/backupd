package workflowexec

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// --- the envelope under a real bash ---------------------------------------
//
// envelope_test.go proves the payload's TEXT: it parses the payload back
// the way a shell reads single-quoted literals and insists the result is
// exactly the assignments this product intended. That is the right shape
// for a unit test and it is blind to one whole class of mistake, because
// nothing in it ever runs a shell. A hand-written literal parser agrees
// with itself; it cannot tell whether bash agrees with it, and it cannot
// observe a shell option at all -- "this product injects no set -e" is a
// claim about what a hook OBSERVES, and the only witness to that is a
// hook.
//
// So these three tests drive the real interpreter, with exactly BashArgs
// taken from the package rather than retyped, a hostile parent
// environment on cmd.Env, and every assertion read out of the hook's own
// output.

// realBashMarker frames the values a hook prints back.
//
// The corpus deliberately contains newlines, quotes and backslashes, so a
// value cannot be delimited by any of those. A fixed nonce that appears
// nowhere in the corpus can be: the framing is a line of its own before
// the bytes and a line of its own after them, which stays unambiguous for
// a value that is itself several lines long.
const realBashMarker = "9f2c1d4e"

// realBash finds the interpreter, in the same candidate order and by the
// same rule as hostrunner's hostBashForFake.
//
// This one skips rather than failing, because workflowexec is a pure
// encoding package whose other tests need no shell: a host with no bash
// still gets a meaningful result out of the rest of the file. On darwin
// and on every linux distribution this product supports one of these
// paths exists, so a skip here is a report about the machine and never
// the normal outcome.
func realBash(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"/bin/bash", "/usr/bin/bash", "/usr/local/bin/bash", "/opt/homebrew/bin/bash"} {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	t.Skip("this host has none of /bin/bash, /usr/bin/bash, /usr/local/bin/bash or /opt/homebrew/bin/bash, so the envelope cannot be run through a real interpreter here")

	return ""
}

// runUnderRealBash feeds stdin to `bash --noprofile --norc -s` with parent
// as its whole environment.
//
// parent is passed as cmd.Env rather than added to os.Environ(), because
// the property under test is what a hook sees when the environment it
// STARTED from was hostile: inheriting the test runner's own environment
// would leave the result depending on whoever ran `go test`.
func runUnderRealBash(t *testing.T, parent []string, stdin []byte) (string, string, int) {
	t.Helper()

	cmd := exec.Command(realBash(t), BashArgs...)
	cmd.Env = parent
	cmd.Stdin = strings.NewReader(string(stdin))
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// A hook that exits nonzero is an ordinary observation here, so only
	// a bash that could not be started at all is a broken fixture.
	var exit *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exit) {
		t.Fatalf("running the envelope under bash: %v\nstderr:\n%s", err, stderr.String())
	}

	return stdout.String(), stderr.String(), cmd.ProcessState.ExitCode()
}

// hostileParent is the environment an SSH exec channel or a careless
// service manager can realistically hand this envelope: an account's own
// variables, an sshd's, and a PATH nobody chose.
func hostileParent(extra ...string) []string {
	parent := []string{
		"HOME=/home/somebody",
		"USER=somebody",
		"LOGNAME=somebody",
		"LANG=tr_TR.UTF-8",
		"MAIL=/var/mail/somebody",
		"SSH_CONNECTION=10.0.0.1 52342 10.0.0.2 22",
		"SSH_CLIENT=10.0.0.1 52342 22",
		"PATH=/opt/somebody/bin",
		"LD_PRELOAD=/opt/somebody/lib/hook.so",
		"IFS=:",
	}

	return append(parent, extra...)
}

// TestAHookUnderARealBashSeesNoShellOptionThisProductNeverAskedForAndAClosedStdin
// is the executable half of the consensus row "never inject
// set -e/-u/pipefail": the hook reports $- and `set -o`, and then
// DEMONSTRATES the same thing the way an operator would meet it.
//
// The reported-state half alone would be a weak assertion, because a
// future payload could set an option in a subshell, or export an option
// through SHELLOPTS, and still print a clean `set -o` at the top. So each
// option is also proved by consequence: a failing command that the script
// survives (no errexit), a reference to a name that was never set which
// does not abort (no nounset), and a pipeline whose left-hand side fails
// but whose status is zero (no pipefail). The bug this catches is somebody
// adding `set -euo pipefail` to the payload "for safety", which silently
// changes what every captured hook script MEANS without changing a byte
// of it.
//
// The second half of the test is the hook's standard input, which belongs
// in the same place because it is the other property only a running shell
// can show: a hook that reads stdin gets EOF and not the remainder of its
// own script.
func TestAHookUnderARealBashSeesNoShellOptionThisProductNeverAskedForAndAClosedStdin(t *testing.T) {
	t.Parallel()

	script := []byte(strings.Join([]string{
		`printf 'dash=%s\n' "$-"`,
		`printf 'options-begin\n'`,
		`set -o`,
		`printf 'options-end\n'`,
		// No errexit: a failing command is the hook's business, not the
		// envelope's, so the next line must still run.
		`false`,
		`printf 'survived-a-failing-command\n'`,
		// No nounset: an unset reference expands to nothing.
		`printf 'unset-reference=[%s]\n' "$THIS_NAME_WAS_NEVER_SET"`,
		`printf 'survived-an-unset-reference\n'`,
		// No pipefail: only the right-hand side decides the status.
		`false | true`,
		`printf 'pipeline-status=%s\n' "$?"`,
		`printf 'reached-the-end\n'`,
		"",
	}, "\n"))

	payload, err := StdinPayload([]string{"PLAN_VALUE=present"}, script)
	if err != nil {
		t.Fatalf("StdinPayload: %v", err)
	}
	stdout, stderr, code := runUnderRealBash(t, hostileParent(), payload)

	if code != 0 {
		t.Errorf("the hook exited %d; stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, want := range []string{
		"survived-a-failing-command",
		"survived-an-unset-reference",
		"reached-the-end",
		"unset-reference=[]",
		"pipeline-status=0",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the hook never printed %q, so an option this product must not inject was in effect; stdout:\n%s\nstderr:\n%s", want, stdout, stderr)
		}
	}

	// $- is bash's own summary of the single-letter options. Only the four
	// this product refuses to inject are checked: h, B and s are on for
	// every non-interactive bash and asserting their absence would be
	// asserting a bug.
	dash := valueOfReportedLine(t, stdout, "dash=")
	for letter, option := range map[string]string{"e": "errexit", "u": "nounset", "x": "xtrace"} {
		if strings.Contains(dash, letter) {
			t.Errorf("$- is %q, so %s is on inside the hook", dash, option)
		}
	}

	// And the long form, which is the only place pipefail appears at all.
	options := reportedOptions(t, stdout)
	for _, option := range []string{"errexit", "nounset", "pipefail", "xtrace"} {
		state, reported := options[option]
		if !reported {
			// A parse that found nothing would pass every check below,
			// which is the one way this assertion could stop meaning
			// anything.
			t.Errorf("the hook's `set -o` output never mentioned %s, so this test is not reading it; stdout:\n%s", option, stdout)

			continue
		}
		if state != "off" {
			t.Errorf("%s is %s inside the hook; a hook's bytes run exactly as captured", option, state)
		}
	}

	// --- and the hook's own standard input --------------------------------
	//
	// The payload ends in `eval "$__backupd_script" 0</dev/null`, so a
	// hook that reads stdin must see EOF. The bug this catches is a
	// payload that runs the captured bytes with bash's own script stream
	// still attached: a `read` then competes with bash's parser for the
	// same bytes, which is the measured failure payload.go documents.
	readScript := []byte(strings.Join([]string{
		`IFS= read -r line`,
		`printf 'read-status=%s\n' "$?"`,
		`printf 'read-line=[%s]\n' "$line"`,
		`printf 'read-end\n'`,
		"",
	}, "\n"))

	readPayload, err := StdinPayload(nil, readScript)
	if err != nil {
		t.Fatalf("StdinPayload: %v", err)
	}
	stdout, stderr, code = runUnderRealBash(t, hostileParent(), readPayload)
	if code != 0 {
		t.Errorf("the reading hook exited %d; stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "read-line=[]") || !hasReportedLine(stdout, "read-end") {
		t.Errorf("the hook's read did not return an empty line at EOF, so its stdin was not the closed one the host runner also gives it; stdout:\n%s", stdout)
	}
	if status := valueOfReportedLine(t, stdout, "read-status="); status == "0" {
		t.Errorf("the hook's read SUCCEEDED, so something was readable on its stdin; stdout:\n%s", stdout)
	}

	// The control: the same bytes fed to bash raw on its own stdin -- the
	// encoding this envelope deliberately does not use -- and the read
	// takes the NEXT LINE OF THE SCRIPT as its data. Measured here, so
	// that the check above is an observation about the envelope rather
	// than a restatement of how `read` behaves at EOF: the raw run has no
	// read-status line of its own, because the read swallowed it, and
	// reports a read-line holding that swallowed line instead. The
	// prefixes are matched line by line for exactly that reason -- the
	// swallowed text contains "read-status=" as DATA.
	rawStdout, _, _ := runUnderRealBash(t, hostileParent(), readScript)
	if hasReportedLine(rawStdout, "read-status=") {
		t.Errorf("feeding the script raw on bash's own stdin lost nothing, so the literal-and-eval framing this envelope pays for is no longer load-bearing and payload.go's reasoning should be revisited; stdout:\n%s", rawStdout)
	}
	if !hasReportedLine(rawStdout, "read-line=[printf ") {
		t.Errorf("the raw control did not feed a line of the script to the read, so it proves nothing about what the envelope prevents; stdout:\n%s", rawStdout)
	}
}

// TestAHookUnderARealBashReceivesHostileValuesByteIdenticalAndNeverRunsThem
// is the injection contract executed rather than parsed.
//
// The corpus is envelope_test.go's shape -- command substitution,
// backticks, embedded quotes, newlines, a backslash, a value that looks
// like another variable -- with every payload pointed at a canary under
// t.TempDir() and a value that is a whole shell command. Two things are
// asserted, and they fail for different bugs: the values come back byte
// for byte (a quoting bug that merely mangles a value, which is how a
// password with an apostrophe in it stops working), and the canary does
// not exist (a quoting bug that lets a value become syntax).
//
// The control at the end is what keeps the canary assertion honest. A
// canary that could never be created would make "the canary is absent"
// true of any envelope at all, so a value of the same shape is also run
// through a deliberately UNQUOTED assignment, and that canary has to
// appear.
//
// Every payload in the corpus is a touch or a redirection into
// t.TempDir(). None of them is destructive, deliberately: a test whose
// corpus would wreck the machine if the defence regressed is a test
// nobody can afford to run, and "it created a file it should not have
// been able to create" is the same evidence.
func TestAHookUnderARealBashReceivesHostileValuesByteIdenticalAndNeverRunsThem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	canary := filepath.Join(dir, "canary")
	builtinCanary := filepath.Join(dir, "builtin-canary")
	controlCanary := filepath.Join(dir, "control-canary")

	hostile := map[string]string{
		"V1":  "$(touch " + canary + ")",
		"V2":  "`touch " + canary + "`",
		"V3":  "'; touch " + canary + "; '",
		"V4":  "x' && touch " + canary + " && echo '",
		"V5":  "$V1",
		"V6":  `a\'b`,
		"V7":  "a\nexport EVIL=yes\n",
		"V8":  "';\ntouch " + canary + "\n'",
		"V9":  "$(: > " + builtinCanary + ")",
		"V10": "cd /; touch " + canary + "; printf pwned",
		"V11": `tab	quote" dollar$ bang! backslash\ done`,
	}

	names := make([]string, 0, len(hostile))
	for name := range hostile {
		names = append(names, name)
	}
	sort.Strings(names)

	var environ []string
	var script strings.Builder
	for _, name := range names {
		environ = append(environ, name+"="+hostile[name])
		script.WriteString(frameValue(name))
	}
	script.WriteString("printf 'reached-the-end\\n'\n")

	payload, err := StdinPayload(environ, []byte(script.String()))
	if err != nil {
		t.Fatalf("StdinPayload: %v", err)
	}
	stdout, stderr, code := runUnderRealBash(t, hostileParent(), payload)

	if code != 0 || !strings.Contains(stdout, "reached-the-end") {
		t.Fatalf("the hook exited %d without reaching its end; stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, name := range names {
		got, ok := framedValue(stdout, name)
		if !ok {
			t.Errorf("the hook printed no framed value for %s; stdout:\n%s", name, stdout)

			continue
		}
		if got != hostile[name] {
			t.Errorf("%s arrived as %q, want %q", name, got, hostile[name])
		}
	}
	// Nothing inside a single-quoted literal is interpreted, so nothing in
	// the corpus can have run.
	for _, path := range []string{canary, builtinCanary} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s exists, so a value in the corpus escaped its literal and executed", filepath.Base(path))
		}
	}

	// The control: the same kind of value, assigned WITHOUT the quoting,
	// really does run. This is what proves the canary above is reachable
	// and the assertion therefore falsifiable.
	control := []byte("export CONTROL=$(touch " + controlCanary + ")\nprintf 'control-ran\\n'\n")
	if _, stderr, code := runUnderRealBash(t, hostileParent("PATH=/usr/bin:/bin"), control); code != 0 {
		t.Fatalf("the control payload exited %d, so the canary mechanism cannot be trusted; stderr:\n%s", code, stderr)
	}
	if _, err := os.Stat(controlCanary); err != nil {
		t.Fatalf("the unquoted control did NOT create %s (%v), so this test's canary proves nothing about the quoted corpus", filepath.Base(controlCanary), err)
	}
}

// TestAHookUnderARealBashLosesTheStartupVariablesTheEnvelopeCanRemoveAndNothingElse
// is the honest version of the "sanitize BASH_ENV/ENV/SHELLOPTS/BASHOPTS"
// row, split into the half the envelope guarantees and the half it
// documents as somebody else's refusal.
//
// What the envelope guarantees, and what this test asserts: inside a hook,
// BASH_ENV and ENV are unset whatever the parent exported, and anything a
// sourced BASH_ENV managed to export is gone as well -- so the hook's own
// children cannot be redirected by a file this product did not choose.
//
// What the envelope does NOT guarantee, and what this test therefore
// asserts as an observation rather than a protection: bash acts on both
// variables BEFORE the payload's first byte is read. --norc does not
// suppress BASH_ENV (it is read by every non-interactive bash), so the
// file really is sourced, and an inherited SHELLOPTS is applied as shell
// options at startup, after which SHELLOPTS is readonly and the payload's
// unset cannot take the option away. Refusing such a host is
// core/internal/remoteexec's job and is done in its preflight:
// contaminationOf reports BASH_ENV or ENV merely being set, and reports a
// SHELLOPTS or BASHOPTS carrying an option that changes what a script
// means. This test asserts the observable state of that split -- the
// sourced file ran, the inherited xtrace is still on -- so that a future
// change which actually closed either half turns this red and the claim
// gets rewritten deliberately instead of drifting.
//
// BASHOPTS is set in the parent for completeness but nothing is asserted
// about it: it does not exist before bash 4.1, so any assertion would be
// about which bash the machine has rather than about this envelope.
func TestAHookUnderARealBashLosesTheStartupVariablesTheEnvelopeCanRemoveAndNothingElse(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sourced := filepath.Join(dir, "bash-env-ran")
	startup := filepath.Join(dir, "startup.sh")
	// Written with a redirection rather than touch so the file needs no
	// PATH: at startup the parent's PATH is whatever the hostile
	// environment said, and this canary must not depend on it.
	body := "export EVIL_FROM_BASH_ENV=yes\n: > " + sourced + "\n"
	if err := os.WriteFile(startup, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the startup file: %v", err)
	}

	script := []byte(strings.Join([]string{
		`printf 'bash_env=[%s]\n' "${BASH_ENV-UNSET}"`,
		`printf 'env=[%s]\n' "${ENV-UNSET}"`,
		`printf 'evil=[%s]\n' "${EVIL_FROM_BASH_ENV-UNSET}"`,
		`printf 'ssh_connection=[%s]\n' "${SSH_CONNECTION-UNSET}"`,
		`printf 'dash=%s\n' "$-"`,
		`printf 'plan=[%s]\n' "$PLAN_VALUE"`,
		`printf 'reached-the-end\n'`,
		"",
	}, "\n"))

	payload, err := StdinPayload([]string{"PLAN_VALUE=present"}, script)
	if err != nil {
		t.Fatalf("StdinPayload: %v", err)
	}
	parent := hostileParent(
		"BASH_ENV="+startup,
		"ENV="+startup,
		"SHELLOPTS=xtrace",
		"BASHOPTS=extglob",
	)
	stdout, stderr, code := runUnderRealBash(t, parent, payload)

	if code != 0 || !strings.Contains(stdout, "reached-the-end") {
		t.Fatalf("the hook exited %d without reaching its end; stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	// The guaranteed half.
	for _, line := range []string{"bash_env=[UNSET]", "env=[UNSET]", "evil=[UNSET]", "ssh_connection=[UNSET]"} {
		if !strings.Contains(stdout, line) {
			t.Errorf("the hook did not report %q, so a variable the envelope promises to remove survived into it; stdout:\n%s", line, stdout)
		}
	}
	// Even on a host this contaminated, the plan still arrives intact:
	// that is the whole point of clearing before exporting rather than
	// exporting on top of what was there.
	if !strings.Contains(stdout, "plan=[present]") {
		t.Errorf("the resolved plan did not reach the hook on a contaminated host; stdout:\n%s", stdout)
	}

	// The half that is the preflight's, recorded as what really happens.
	if _, err := os.Stat(sourced); err != nil {
		t.Errorf("bash did not source BASH_ENV (%v). That is a stronger protection than this envelope claims: --norc does not suppress BASH_ENV, so if this is now true the claim in payload.go and remoteexec's contaminationOf refusal should be revisited", err)
	}
	if dash := valueOfReportedLine(t, stdout, "dash="); !strings.Contains(dash, "x") {
		t.Errorf("$- is %q, so the SHELLOPTS=xtrace this test inherited is NOT in effect. The envelope does not claim to un-apply one -- SHELLOPTS is applied at startup and readonly afterwards -- so if it is gone, something changed and remoteexec's preflight refusal is no longer the only defence", dash)
	}
	// The inherited option is visible in the hook's own diagnostics, which
	// is the operator-facing consequence the preflight exists to prevent.
	if !strings.Contains(stderr, "+ printf") {
		t.Errorf("the inherited xtrace produced no trace on stderr, so this test is not observing the startup-applied option at all; stderr:\n%s", stderr)
	}
}

// --- reading a hook's report ----------------------------------------------

// frameValue is the one line of hook script that prints a variable's bytes
// between two markers.
//
// printf '%s' and not echo: echo interprets backslashes in some shells and
// eats a leading -n, and the whole question here is whether the bytes
// arrived unchanged.
func frameValue(name string) string {
	return "printf '<<<" + realBashMarker + ":" + name + "\\n'\n" +
		`printf '%s' "$` + name + "\"\n" +
		"printf '\\n>>>" + realBashMarker + ":" + name + "\\n'\n"
}

// framedValue recovers what frameValue printed. The closing marker is
// searched for as a whole line, and the newline that introduces it is the
// one frameValue added, so a value that itself ends in a newline survives
// the round trip.
func framedValue(stdout, name string) (string, bool) {
	open := "<<<" + realBashMarker + ":" + name + "\n"
	shut := "\n>>>" + realBashMarker + ":" + name + "\n"
	start := strings.Index(stdout, open)
	if start < 0 {
		return "", false
	}
	rest := stdout[start+len(open):]
	end := strings.Index(rest, shut)
	if end < 0 {
		return "", false
	}

	return rest[:end], true
}

// valueOfReportedLine returns the remainder of the first line beginning
// with prefix.
func valueOfReportedLine(t *testing.T, stdout, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if after, ok := strings.CutPrefix(line, prefix); ok {
			return after
		}
	}
	t.Fatalf("the hook printed no %q line; stdout:\n%s", prefix, stdout)

	return ""
}

// hasReportedLine reports whether any WHOLE line begins with prefix.
//
// Line-anchored rather than a plain substring search, because the text a
// misencoded payload leaks into a hook is the hook's own script, and that
// text contains the markers this test looks for.
func hasReportedLine(stdout, prefix string) bool {
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}

	return false
}

// reportedOptions parses the `set -o` table the hook printed between its
// own markers into name -> "on"/"off".
func reportedOptions(t *testing.T, stdout string) map[string]string {
	t.Helper()

	_, after, found := strings.Cut(stdout, "options-begin\n")
	if !found {
		t.Fatalf("the hook printed no option table; stdout:\n%s", stdout)
	}
	table, _, found := strings.Cut(after, "options-end\n")
	if !found {
		t.Fatalf("the hook's option table has no end marker, so it was cut short; stdout:\n%s", stdout)
	}

	options := map[string]string{}
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		options[fields[0]] = fields[1]
	}

	return options
}
