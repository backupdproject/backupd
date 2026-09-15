// Package sshexecintegration_test is #810's evidence, against a real sshd
// with three accounts that differ only in what the server will let them
// run: an ordinary shell account, an internal-sftp-forced account, and a
// forced-command account.
//
// It is a machine-tier suite because the central claim cannot be made
// anywhere else. "An SFTP credential is not an exec credential" is a
// statement about what OpenSSH does with a ForceCommand, and a fake would
// only ever prove that this repository's own idea of that behaviour is
// self-consistent. Everything asserted here -- the refusals, the absence of
// a PTY, the byte fidelity of a hostile environment value, the lack of
// residue, and which terminations can be confirmed -- is measured against
// the server.
package sshexecintegration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/remoteexec"
	"github.com/retnd/retnd/core/internal/transport"
	"github.com/retnd/retnd/core/internal/transport/rclone"
	"github.com/retnd/retnd/core/internal/workflowexec"
	"github.com/retnd/retnd/core/tests/machines"
)

// seededArtifact is the byte content of the artifact the SFTP-only account
// keeps, seeded and then transferred back so the "still backs up" half of
// the capability boundary is a transfer rather than a directory listing.
const seededArtifact = "artifact-bytes"

// connectionFor builds the execution connection for one of the fixture's
// accounts. It goes through the exported Connection rather than through
// config resolution on purpose: what this suite is about is the CHANNEL,
// and internal/remoteexec's own tests cover resolution without a container.
func connectionFor(t *testing.T, h *machines.ExecHost, user string) remoteexec.Connection {
	t.Helper()

	return remoteexec.Connection{
		Ref:  "fixture/" + user,
		Kind: remoteexec.KindDeclared,
		Source: transport.Source{
			Type:       "sftp",
			Host:       h.Host,
			Port:       h.Port,
			User:       user,
			KeyFile:    h.KeyFile,
			KnownHosts: h.KnownHostsFile,
		},
	}
}

func dial(t *testing.T, h *machines.ExecHost, conn remoteexec.Connection) *remoteexec.Client {
	t.Helper()

	client, err := remoteexec.Dial(h.Context(), conn)
	if err != nil {
		t.Fatalf("Dial as %s: %v", conn.Source.User, err)
	}
	t.Cleanup(func() { _ = client.Close() })

	return client
}

// sink collects the capture. It is mutex-guarded because the two streams
// are copied by two goroutines and because the tests below READ it while
// the hook is still running -- waiting for a hook's own "started" line is
// how they synchronise on the remote process existing, instead of sleeping
// for a duration that is either flaky or slow.
type sink struct {
	mu     sync.Mutex
	chunks []workflowexec.Chunk
}

func (s *sink) Chunk(c workflowexec.Chunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks = append(s.chunks, workflowexec.Chunk{Stream: c.Stream, Seq: c.Seq, Data: append([]byte(nil), c.Data...)})

	return nil
}

func (s *sink) all() []workflowexec.Chunk {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]workflowexec.Chunk(nil), s.chunks...)
}

func (s *sink) text(stream workflowexec.StreamID) string {
	var b strings.Builder
	for _, c := range s.all() {
		if c.Stream == stream {
			b.Write(c.Data)
		}
	}

	return b.String()
}

// awaitOutput blocks until the hook has printed marker on stdout, which is
// the hook itself saying it is running. Every test that has to act on a
// LIVE remote process synchronises on this rather than on a sleep: a sleep
// long enough to be reliable on a loaded machine is a sleep that makes the
// suite slow, and a shorter one tests whatever happened to have started.
// It reports rather than fails fatally, because two of its callers wait on
// behalf of a goroutine and t.Fatalf from one of those stops nothing.
func awaitOutput(t *testing.T, s *sink, marker string, within time.Duration) bool {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if strings.Contains(s.text(workflowexec.StreamStdout), marker) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("the hook never printed %q within %s, so it was never observed running and this test proves nothing:\n%s",
		marker, within, s.text(workflowexec.StreamStdout))

	return false
}

// awaitGone waits for a process matching marker to disappear from the
// remote host's process table, and says what is still there if it does not.
func awaitGone(t *testing.T, h *machines.ExecHost, marker string, within time.Duration) {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !strings.Contains(h.Inside(t, "ps", "-A", "-o", "args="), marker) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("%q is still running on the remote host after %s:\n%s", marker, within, h.Inside(t, "ps", "-A", "-o", "args="))
}

// --- the capability boundary ---------------------------------------------

// TestAnSFTPOnlyAccountIsRefusedExecAndStillBacksUp is the acceptance
// criterion this whole issue turns on, and it is one test because the two
// halves are only interesting together: the same credential, at the same
// moment, is a valid backup transport and an invalid workflow executor.
func TestAnSFTPOnlyAccountIsRefusedExecAndStillBacksUp(t *testing.T) {
	h := machines.Start(t).ExecHost(t)

	// The transport half first, so a failure here cannot be mistaken for
	// the exec refusal having broken backups.
	seeded := h.Inside(t, "sh", "-c",
		"mkdir -p /home/"+machines.SFTPOnlyUser+"/artifacts && "+
			"printf '"+seededArtifact+"' > /home/"+machines.SFTPOnlyUser+"/artifacts/dump.tar && "+
			"chown -R "+machines.SFTPOnlyUser+" /home/"+machines.SFTPOnlyUser+"/artifacts && echo seeded")
	if !strings.Contains(seeded, "seeded") {
		t.Fatalf("could not seed an artifact for the transport half: %q", seeded)
	}

	adapter := rclone.New()
	source := transport.Source{
		ID:         "sftp-only-source",
		Type:       "sftp",
		Host:       h.Host,
		Port:       h.Port,
		User:       machines.SFTPOnlyUser,
		KeyFile:    h.KeyFile,
		KnownHosts: h.KnownHostsFile,
		Root:       "artifacts",
	}
	artifacts, err := adapter.List(h.Context(), source)
	if err != nil {
		t.Fatalf("the SFTP-only account could not list its artifacts, so this test cannot say anything about exec: %v", err)
	}
	if len(artifacts) != 1 || !strings.HasSuffix(artifacts[0].Path, "dump.tar") {
		t.Fatalf("the transport half listed %+v, want one dump.tar", artifacts)
	}

	// Listing is not backing up. The claim is that this credential still
	// TRANSFERS, so the artifact is pulled down and compared byte for
	// byte with what was seeded: a source that could be listed and not
	// read would pass a listing assertion and fail every backup.
	local := filepath.Join(t.TempDir(), "dump.tar.partial")
	if _, err := adapter.CopyToLocal(h.Context(), source, artifacts[0].Path, local); err != nil {
		t.Fatalf("the SFTP-only account could not transfer its artifact, which is the half of this test that says backups keep working: %v", err)
	}
	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatalf("reading the transferred artifact: %v", err)
	}
	if string(got) != seededArtifact {
		t.Fatalf("the transferred artifact is %q, want %q", got, seededArtifact)
	}

	// The exec half: the same host, the same key, the same account.
	client, dialErr := remoteexec.Dial(h.Context(), connectionFor(t, h, machines.SFTPOnlyUser))
	if dialErr != nil {
		t.Fatalf("the SFTP-only account failed to AUTHENTICATE, which is a different fact from failing to exec: %v", dialErr)
	}
	defer func() { _ = client.Close() }()

	_, err = client.Preflight(h.Context(), []byte("printf 'the hook ran\\n'\n"))
	if err == nil {
		t.Fatal("an internal-sftp-forced account passed the exec preflight; the transfer credential was assumed to grant shell exec")
	}
	if !errors.Is(err, remoteexec.ErrExecCapability) {
		t.Errorf("the refusal is not an ErrExecCapability, so a caller cannot tell it from a broken connection: %v", err)
	}
	for _, want := range []string{"SFTP-only", "artifact backup over this credential keeps working"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q, and an operator has to be told which capability is missing:\n%v", want, err)
		}
	}
}

// TestAForcedCommandAccountIsRefusedExec is the harder half of the same
// boundary: this account accepts the exec request and exits 0. Only the
// probe's own marker tells "my script ran" from "something ran".
//
// The refusal is asserted against what the FIXTURE's forced command
// actually printed, and against the status it actually exited with. A
// refusal that only said "forced command" would be produced for any
// marker-less answer -- a nologin shell, a broken account, an sshd that
// said nothing at all -- so deleting this fixture's ForceCommand would
// leave the test passing while it stopped testing forced commands.
func TestAForcedCommandAccountIsRefusedExec(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	client := dial(t, h, connectionFor(t, h, machines.ForcedCommandUser))

	_, err := client.Preflight(h.Context(), []byte("printf 'the hook ran\\n'\n"))
	if err == nil {
		t.Fatal("a forced-command account passed the exec preflight, so a hook would have been reported as run while the account's own program ran instead")
	}
	if !errors.Is(err, remoteexec.ErrExecCapability) {
		t.Errorf("the refusal is not an ErrExecCapability: %v", err)
	}
	if !strings.Contains(err.Error(), "forced command") {
		t.Errorf("the refusal does not name the forced command: %v", err)
	}
	// The evidence: somebody else's program produced output of its own,
	// and the session it ran in SUCCEEDED. Both halves are what makes
	// this failure silent without a capability probe.
	if !strings.Contains(err.Error(), machines.ForcedCommandOutput) {
		t.Errorf("the refusal does not carry what the account actually ran and printed (%q), so it is the generic marker-less refusal rather than evidence about this account: %v",
			machines.ForcedCommandOutput, err)
	}
	if !strings.Contains(err.Error(), "exited 0") {
		t.Errorf("the refusal does not record that the session SUCCEEDED, which is the whole reason a forced command is dangerous: %v", err)
	}
}

// TestAForcedCommandAccountCannotRunAHookAtAll is the same fact stated
// where it matters most: not on the preflight, which a caller could
// forget, but on the one exported way to run anything.
//
// A step here must never come back as exit 0. The account accepts the exec
// request, runs its own program and succeeds, so an execution path that
// trusted "the request was accepted" would report every remote hook on
// this account as having run perfectly while none of them ever ran.
func TestAForcedCommandAccountCannotRunAHookAtAll(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	client := dial(t, h, connectionFor(t, h, machines.ForcedCommandUser))

	marker := "/tmp/forced-command-hook-should-not-have-run"
	res, s, err := runHook(t, h, client, remoteexec.Request{
		Token:   "backupd-exec-forced-run",
		Script:  []byte("touch " + marker + "\nprintf 'the hook ran\\n'\n"),
		Timeout: 60 * time.Second,
	})

	if err == nil {
		t.Fatal("a step on a forced-command account was reported as having run")
	}
	if !errors.Is(err, remoteexec.ErrExecCapability) {
		t.Errorf("the refusal is not an ErrExecCapability: %v", err)
	}
	if res.ExitCode != nil {
		t.Errorf("the step reported exit code %d, which is the forced command's status and not a hook's", *res.ExitCode)
	}
	if out := s.text(workflowexec.StreamStdout); strings.Contains(out, "the hook ran") {
		t.Errorf("the hook's own output was captured on an account that cannot run one: %q", out)
	}
	if listing := h.Inside(t, "ls", marker); !strings.Contains(listing, "No such file") {
		t.Errorf("the hook's first line ran on a forced-command account: %q", listing)
	}
}

func TestAnExecCapableAccountPassesPreflight(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	client := dial(t, h, connectionFor(t, h, machines.ExecUser))

	capability, err := client.Preflight(h.Context(), []byte("printf 'ok\\n'\nfor i in 1 2; do printf '%s\\n' \"$i\"; done\n"))
	if err != nil {
		t.Fatalf("an ordinary shell account was refused: %v", err)
	}
	report := capability.Report()

	if report.User != machines.ExecUser {
		t.Errorf("the far side says the session is %q, want %q", report.User, machines.ExecUser)
	}
	if report.BashPath != machines.RemoteBashPath {
		t.Errorf("bash path = %q", report.BashPath)
	}
	if !strings.HasPrefix(report.BashVersion, "5.") && !strings.HasPrefix(report.BashVersion, "4.") {
		t.Errorf("bash version = %q, which does not look like a bash version", report.BashVersion)
	}
	if report.PTY {
		t.Error("the session got a terminal, which merges stdout and stderr into one unrecoverable stream")
	}
	if len(report.Contamination) != 0 {
		t.Errorf("a clean account reported contamination: %v", report.Contamination)
	}
	if !report.ScriptSyntaxChecked {
		t.Error("the captured bytes were not parsed on the target, so a script that cannot run there would be found out half way through")
	}
	if report.HostKeyFingerprint == "" {
		t.Error("no host identity was recorded, and an audit line naming a host is only worth reading if the identity was checked")
	}
}

// TestAMissingBashIsRefusedRatherThanSubstituted is a technical requirement
// stated as a prohibition: never silently substitute /bin/sh, another
// shell, or a different credential.
func TestAMissingBashIsRefusedRatherThanSubstituted(t *testing.T) {
	h := machines.Start(t).ExecHost(t)

	conn := connectionFor(t, h, machines.ExecUser)
	conn.BashPath = "/opt/definitely-not-here/bash"
	client := dial(t, h, conn)

	_, err := client.Preflight(h.Context(), []byte("printf 'ok\\n'\n"))
	if err == nil {
		t.Fatal("a connection naming a bash that is not there passed the preflight, so something else ran the hook")
	}
	if !errors.Is(err, remoteexec.ErrExecCapability) {
		t.Errorf("the refusal is not an ErrExecCapability: %v", err)
	}
	if !strings.Contains(err.Error(), conn.BashPath) {
		t.Errorf("the refusal does not name the path that is missing: %v", err)
	}
	if !strings.Contains(err.Error(), "never substitutes") {
		t.Errorf("the refusal does not state the substitution rule: %v", err)
	}
}

// TestASyntaxErrorIsCaughtOnTheTargetBeforeTheHookRuns proves the refusal
// AND that nothing ran: the script's first line would have left a file.
func TestASyntaxErrorIsCaughtOnTheTargetBeforeTheHookRuns(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	client := dial(t, h, connectionFor(t, h, machines.ExecUser))

	marker := "/tmp/syntax-check-should-not-have-run"
	script := []byte("touch " + marker + "\nif true; then\nprintf 'unreachable\\n'\n")

	_, err := client.Preflight(h.Context(), script)
	if err == nil {
		t.Fatal("a script that does not parse passed the preflight")
	}
	if !errors.Is(err, remoteexec.ErrExecCapability) {
		t.Errorf("the refusal is not an ErrExecCapability: %v", err)
	}
	if !strings.Contains(err.Error(), "does not parse") {
		t.Errorf("the refusal does not say the script would not parse: %v", err)
	}
	if listing := h.Inside(t, "ls", marker); !strings.Contains(listing, "No such file") {
		t.Errorf("the script's first line ran during a syntax check: %q", listing)
	}
}

// TestAHostKeyPolicyViolationRefusesBeforeTheHookRuns is the control that
// has to fail closed. The fixture's decoy known_hosts pins a real key this
// machine does not have, which is the shape of both a rotated host key and
// a machine in the middle.
func TestAHostKeyPolicyViolationRefusesBeforeTheHookRuns(t *testing.T) {
	h := machines.Start(t).ExecHost(t)

	conn := connectionFor(t, h, machines.ExecUser)
	conn.Source.KnownHosts = h.BadKnownHostsFile

	_, err := remoteexec.Dial(h.Context(), conn)
	if err == nil {
		t.Fatal("a host offering an unpinned key was accepted, and a hook is arbitrary code handed to whatever answered")
	}
	if !errors.Is(err, remoteexec.ErrHostKeyPolicy) {
		t.Errorf("a host-key mismatch was not reported as a host-key policy failure, so a retry could paper over it: %v", err)
	}

	// And the positive control, or the test above would pass for a client
	// that simply could not connect at all.
	good := dial(t, h, connectionFor(t, h, machines.ExecUser))
	if _, err := good.Preflight(h.Context(), []byte("printf 'ok\\n'\n")); err != nil {
		t.Fatalf("the same connection with the real pinned keys was refused: %v", err)
	}
}

// --- the envelope ---------------------------------------------------------

func runHook(t *testing.T, h *machines.ExecHost, client *remoteexec.Client, req remoteexec.Request) (remoteexec.Result, *sink, error) {
	t.Helper()

	s := &sink{}
	req.Sink = s
	if req.Token == "" {
		req.Token = "backupd-exec-" + strings.ReplaceAll(t.Name(), "/", ".")
	}
	res, err := client.Run(h.Context(), req)

	return res, s, err
}

// TestAHookRunsWithSeparateStreamsAndOneSequence is the capture contract
// measured end to end: two logical streams, one counter, and an exit code
// the hook actually chose.
func TestAHookRunsWithSeparateStreamsAndOneSequence(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	client := dial(t, h, connectionFor(t, h, machines.ExecUser))

	// The sleeps are what make the interleaving assertion meaningful:
	// without them the three writes can arrive in one packet each way and
	// "which came first" would be untestable rather than merely unproven.
	script := []byte("printf 'out-first\\n'\nsleep 0.3\nprintf 'err-middle\\n' >&2\nsleep 0.3\nprintf 'out-last\\n'\nexit 5\n")

	res, s, err := runHook(t, h, client, remoteexec.Request{Script: script, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.ExitCode == nil {
		t.Fatal("no exit status was observed for a hook that exited normally")
	}
	if *res.ExitCode != 5 {
		t.Errorf("exit = %d, want 5", *res.ExitCode)
	}
	if res.Certainty != workflowexec.TerminationNotRequested {
		t.Errorf("certainty = %q for a step nothing asked to stop", res.Certainty)
	}

	if got := s.text(workflowexec.StreamStdout); got != "out-first\nout-last\n" {
		t.Errorf("stdout = %q, want only the stdout writes", got)
	}
	if got := s.text(workflowexec.StreamStderr); got != "err-middle\n" {
		t.Errorf("stderr = %q, want only the stderr write", got)
	}

	var lastSeq uint64
	var errSeq, lastOutSeq uint64
	for _, c := range s.chunks {
		if c.Seq <= lastSeq {
			t.Fatalf("sequence numbers are not increasing across the streams: %d after %d", c.Seq, lastSeq)
		}
		lastSeq = c.Seq
		switch {
		case c.Stream == workflowexec.StreamStderr:
			errSeq = c.Seq
		case strings.Contains(string(c.Data), "out-last"):
			lastOutSeq = c.Seq
		}
	}
	if errSeq == 0 || lastOutSeq == 0 {
		t.Fatalf("the streams did not arrive as separate chunks: %+v", s.chunks)
	}
	if errSeq > lastOutSeq {
		t.Error("the stderr write is sequenced after the last stdout write, so the interleaving a reader recovers is wrong")
	}
}

// TestHostileEnvironmentValuesArriveByteForByteAndAreNeverInterpreted is
// the injection proof, and it is asserted with a hash rather than by eye:
// the remote side digests what it received and the test compares that to a
// digest computed here, so a value that lost a byte, gained a quote or got
// expanded cannot pass.
func TestHostileEnvironmentValuesArriveByteForByteAndAreNeverInterpreted(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	client := dial(t, h, connectionFor(t, h, machines.ExecUser))

	values := map[string]string{
		"SUBST":     "$(touch /tmp/pwned-subst)",
		"BACKTICK":  "`touch /tmp/pwned-backtick`",
		"QUOTES":    `he said "it's fine" and 'left'`,
		"NEWLINES":  "first line\nsecond line\nthird\n",
		"UTF8":      "ünïcødé ✓ — 日本語",
		"BACKSLASH": `C:\path\to\nowhere \' \\ \n`,
		"SEMICOLON": "x; touch /tmp/pwned-semi; echo",
		"EMPTY":     "",
		"DOLLAR":    "$HOME $PATH ${IFS}",
	}

	var environ []string
	names := make([]string, 0, len(values))
	for name, value := range values {
		environ = append(environ, name+"="+value)
		names = append(names, name)
	}

	// The hook prints one "NAME sha256" line per variable, computed from
	// the bytes the variable actually holds on the far side.
	var script strings.Builder
	for _, name := range names {
		fmt.Fprintf(&script, "printf '%%s ' %s; printf '%%s' \"$%s\" | sha256sum | cut -d' ' -f1\n", name, name)
	}

	res, s, err := runHook(t, h, client, remoteexec.Request{
		Script:  []byte(script.String()),
		Environ: environ,
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("the hook did not succeed: exit=%v stderr=%q", res.ExitCode, s.text(workflowexec.StreamStderr))
	}

	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(s.text(workflowexec.StreamStdout)), "\n") {
		if name, digest, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			got[name] = strings.TrimSpace(digest)
		}
	}
	for name, value := range values {
		sum := sha256.Sum256([]byte(value))
		want := hex.EncodeToString(sum[:])
		if got[name] != want {
			t.Errorf("%s arrived with digest %q, want %q -- the value did not survive byte for byte", name, got[name], want)
		}
	}

	// Nothing in any of those values may have executed.
	listing := h.Inside(t, "ls", "-A", "/tmp")
	for _, pwned := range []string{"pwned-subst", "pwned-backtick", "pwned-semi"} {
		if strings.Contains(listing, pwned) {
			t.Errorf("an environment value was interpreted as shell syntax: /tmp holds %s\n%s", pwned, listing)
		}
	}
}

// TestNoEnvironmentValueReachesTheRemoteProcessList is the acceptance
// criterion about the process list, measured from inside the remote host
// while the hook is still running. The remote command line is readable by
// every account on that machine.
func TestNoEnvironmentValueReachesTheRemoteProcessList(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	client := dial(t, h, connectionFor(t, h, machines.ExecUser))

	const secret = "correct-horse-battery-staple-810"

	done := make(chan struct{})
	var res remoteexec.Result
	var runErr error
	var s *sink
	go func() {
		defer close(done)
		res, s, runErr = runHook(t, h, client, remoteexec.Request{
			Token:   "backupd-exec-processlist",
			Script:  []byte("printf 'started\\n'\nsleep 4\n"),
			Environ: []string{"PGPASSWORD=" + secret},
			Timeout: 60 * time.Second,
		})
	}()

	// Sample the remote process table while the hook holds the channel.
	var table string
	for range 20 {
		time.Sleep(300 * time.Millisecond)
		table = h.Inside(t, "ps", "-A", "-o", "args=")
		if strings.Contains(table, "sleep 4") {
			break
		}
	}
	if !strings.Contains(table, "sleep 4") {
		t.Fatalf("the hook was never visible in the remote process table, so this test proved nothing:\n%s", table)
	}

	if strings.Contains(table, secret) {
		t.Errorf("a secret environment value is in the remote process list:\n%s", table)
	}
	if strings.Contains(table, "PGPASSWORD") {
		t.Errorf("an environment variable name is in the remote process list:\n%s", table)
	}
	if !strings.Contains(table, "backupd-exec-processlist") {
		t.Errorf("the step token is NOT in the process list, so the reaper would have nothing to find:\n%s", table)
	}

	<-done
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("the hook did not succeed: %v %q", res.ExitCode, s.text(workflowexec.StreamStderr))
	}
}

// TestNothingIsLeftOnTheRemoteHost is the no-upload, no-residue criterion,
// and it is asserted by SEARCHING for the script rather than by diffing a
// directory listing.
//
// The distinction is the difference between a test that means something and
// one that fails for somebody else's reasons: this fixture's container runs
// under an x86-64 emulator that creates ~/.cache/rosetta on first exec, so a
// before/after listing of the home directory reports a change on every run
// that has nothing to do with this product. What #810 actually requires is
// that no script was uploaded and nothing of the step was left behind, and
// that is exactly what a content search answers.
//
// The hook carries a marker AND uses a here-document, which is the one
// ordinary shell construct bash implements with a temporary file. Both the
// envelope's own payload and the hook's here-document have to be gone.
func TestNothingIsLeftOnTheRemoteHost(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	client := dial(t, h, connectionFor(t, h, machines.ExecUser))

	const marker = "residue-marker-30bd1a6e"
	home := "/home/" + machines.ExecUser
	tmpBefore := h.Inside(t, "ls", "-A", "/tmp")

	res, s, err := runHook(t, h, client, remoteexec.Request{
		Script:  []byte("printf '" + marker + "\\n'\ncat <<'EOF'\n" + marker + " in a here-document\nEOF\n"),
		Environ: []string{"MARKED=" + marker},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("the hook failed: %v %q", res.ExitCode, s.text(workflowexec.StreamStderr))
	}
	if !strings.Contains(s.text(workflowexec.StreamStdout), marker) {
		t.Fatalf("the hook did not run, so this test proved nothing: %q", s.text(workflowexec.StreamStdout))
	}

	// The script bytes, the environment value and the here-document text
	// all carry the marker. None of them may exist on that host.
	found := h.Inside(t, "grep", "-rl", marker, "/tmp", "/var/tmp", home, "/dev/shm")
	for _, line := range strings.Split(found, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "No such file") {
			continue
		}
		t.Errorf("the step left something behind on the remote host: %s", line)
	}

	if after := h.Inside(t, "ls", "-A", "/tmp"); after != tmpBefore {
		t.Errorf("/tmp changed across a hook run.\nbefore: %q\nafter:  %q", tmpBefore, after)
	}
}

// TestShellStartupFilesDoNotChangeAHook is the trust-boundary test. sshd
// runs the fixed command through the account's own login shell, so the
// account's startup files are the last thing before the envelope -- and
// --noprofile --norc is what stops them mattering.
func TestShellStartupFilesDoNotChangeAHook(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	client := dial(t, h, connectionFor(t, h, machines.ExecUser))

	home := "/home/" + machines.ExecUser
	written := h.Inside(t, "sh", "-c",
		"printf 'export CONTAMINATED=yes\\nalias printf=false\\necho contamination-from-bashrc\\n' > "+home+"/.bashrc && "+
			"chown "+machines.ExecUser+" "+home+"/.bashrc && echo written")
	if !strings.Contains(written, "written") {
		t.Fatalf("could not write the contaminating startup file: %q", written)
	}
	t.Cleanup(func() { h.Inside(t, "rm", "-f", home+"/.bashrc") })

	res, s, err := runHook(t, h, client, remoteexec.Request{
		Script:  []byte("printf 'contaminated=[%s]\\n' \"${CONTAMINATED-unset}\"\n"),
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("the hook failed: %v %q", res.ExitCode, s.text(workflowexec.StreamStderr))
	}

	stdout := s.text(workflowexec.StreamStdout)
	if !strings.Contains(stdout, "contaminated=[unset]") {
		t.Errorf("an account startup file reached the hook's environment: %q", stdout)
	}
	if strings.Contains(stdout, "contamination-from-bashrc") {
		t.Errorf("an account startup file's own output landed in the hook's captured stdout: %q", stdout)
	}
}

// TestBASHENVContaminationIsDetectedByThePreflight is the other half, and
// the one --noprofile --norc cannot defend: BASH_ENV is read by bash at
// STARTUP, before any line of the payload runs, so a server that exports it
// has already redirected the shell. The fixture's fourth account has sshd
// itself set it, which is exactly how it happens in the field.
func TestBASHENVContaminationIsDetectedByThePreflight(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	client := dial(t, h, connectionFor(t, h, machines.ContaminatedUser))

	_, err := client.Preflight(h.Context(), []byte("printf 'ok\\n'\n"))
	if err == nil {
		t.Fatal("an account whose server sets BASH_ENV passed the preflight, so a file of somebody else's choosing would have run before the hook")
	}
	if !errors.Is(err, remoteexec.ErrExecCapability) {
		t.Errorf("the refusal is not an ErrExecCapability: %v", err)
	}
	if !strings.Contains(err.Error(), "BASH_ENV") {
		t.Errorf("the refusal does not name what was found: %v", err)
	}
}

// --- termination ----------------------------------------------------------

// TestAForegroundHookIsTerminatedWithConfirmedCertainty is the acceptance
// criterion for the ordinary case: a normal foreground child, stopped on
// timeout, yields confirmed -- and the remote process really is gone.
func TestAForegroundHookIsTerminatedWithConfirmedCertainty(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	client := dial(t, h, connectionFor(t, h, machines.ExecUser))

	const marker = "sleep 611"
	res, _, err := runHook(t, h, client, remoteexec.Request{
		Token:   "backupd-exec-foreground-timeout",
		Script:  []byte("printf 'started\\n'\n" + marker + "\n"),
		Timeout: 3 * time.Second,
	})

	if !errors.Is(err, remoteexec.ErrStepTimeout) {
		t.Fatalf("a step that outran its bound did not report a timeout: %v", err)
	}
	if res.Certainty != workflowexec.TerminationConfirmed {
		t.Errorf("certainty = %q for an ordinary foreground child, want %q (reaper: %s)",
			res.Certainty, workflowexec.TerminationConfirmed, res.Reaper)
	}
	if res.ExitCode != nil {
		t.Errorf("a killed step reported exit code %d; nobody observed a status", *res.ExitCode)
	}

	// Confirmed has to mean what it says.
	for range 10 {
		if !strings.Contains(h.Inside(t, "ps", "-A", "-o", "args="), marker) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Errorf("termination was recorded as confirmed while %q is still running on the remote host:\n%s",
		marker, h.Inside(t, "ps", "-A", "-o", "args="))
}

// TestCancellingAForegroundHookIsAlsoConfirmed covers the other way a step
// is stopped. It is a separate case from the timeout because the cause is
// different -- an operator or a shutdown, not this product's own bound --
// and the journal records them as different states.
func TestCancellingAForegroundHookIsAlsoConfirmed(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	client := dial(t, h, connectionFor(t, h, machines.ExecUser))

	ctx, cancel := context.WithCancel(h.Context())
	defer cancel()

	// The cancellation is fired once the hook has SAID it is running,
	// not after a sleep chosen to be probably long enough: a step
	// cancelled before its remote process exists proves nothing about
	// termination, and it is the outcome a loaded machine produces.
	const marker = "sleep 612"
	s := &sink{}
	go func() {
		awaitOutput(t, s, "started", 60*time.Second)
		cancel()
	}()

	res, err := client.Run(ctx, remoteexec.Request{
		Token:   "backupd-exec-cancelled",
		Script:  []byte("printf 'started\\n'\n" + marker + "\n"),
		Sink:    s,
		Timeout: 5 * time.Minute,
	})

	if !errors.Is(err, remoteexec.ErrStepCanceled) {
		t.Fatalf("a cancelled step did not report cancellation: %v", err)
	}
	if res.Certainty != workflowexec.TerminationConfirmed {
		t.Errorf("certainty = %q, want %q (reaper: %s)", res.Certainty, workflowexec.TerminationConfirmed, res.Reaper)
	}
	if res.ExitCode != nil {
		t.Errorf("a cancelled step reported exit code %d", *res.ExitCode)
	}

	// Confirmed has to mean what it says here too. The certainty is
	// about the remote host, so the remote host is what is asked.
	awaitGone(t, h, marker, 10*time.Second)
}

// TestADeliberatelyDetachedChildIsUnconfirmed is the honest half of the
// termination contract. setsid puts the child in its own session, outside
// the process group the reaper can reach, and it keeps the channel's stdout
// open -- so the session never completes and this product says so instead
// of claiming a clean stop.
func TestADeliberatelyDetachedChildIsUnconfirmed(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	client := dial(t, h, connectionFor(t, h, machines.ExecUser))

	res, _, err := runHook(t, h, client, remoteexec.Request{
		Token:   "backupd-exec-detached",
		Script:  []byte("printf 'detaching\\n'\nsetsid sleep 613 &\nsleep 600\n"),
		Timeout: 3 * time.Second,
	})

	if !errors.Is(err, remoteexec.ErrStepTimeout) {
		t.Fatalf("a step that outran its bound did not report a timeout: %v", err)
	}
	if res.Certainty != workflowexec.TerminationUnconfirmed {
		t.Errorf("certainty = %q for a deliberately detached descendant, want %q. Claiming confirmed here would be the worst case a workflow has: a hook still touching what it was quiescing while the backup proceeds",
			res.Certainty, workflowexec.TerminationUnconfirmed)
	}
	if !strings.Contains(err.Error(), "unconfirmed") {
		t.Errorf("the error does not carry the certainty: %v", err)
	}

	// Clean up the deliberate escapee, so it cannot affect a later test.
	t.Cleanup(func() { h.Inside(t, "pkill", "-f", "sleep 613") })
}

// TestTransportLossIsNotReportedAsAnExitCode closes the connection under a
// running hook, which is what a dropped network does. A known exit code
// here would let "the link went away" be read as "the quiesce script
// reported failure".
func TestTransportLossIsNotReportedAsAnExitCode(t *testing.T) {
	h := machines.Start(t).ExecHost(t)

	client, err := remoteexec.Dial(h.Context(), connectionFor(t, h, machines.ExecUser))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	// The connection is dropped once the hook is demonstrably running,
	// so what this measures is a step losing its transport mid-run
	// rather than a race between Close and the session ever starting.
	s := &sink{}
	go func() {
		awaitOutput(t, s, "started", 60*time.Second)
		_ = client.Close()
	}()

	res, runErr := client.Run(h.Context(), remoteexec.Request{
		Token:   "backupd-exec-transport-loss",
		Script:  []byte("printf 'started\\n'\nsleep 8\n"),
		Sink:    s,
		Timeout: 60 * time.Second,
	})

	if runErr == nil {
		t.Fatal("a connection that went away mid-step was reported as success")
	}
	if !errors.Is(runErr, remoteexec.ErrTransportLoss) {
		t.Errorf("error %v is not an ErrTransportLoss", runErr)
	}
	if res.ExitCode != nil {
		t.Errorf("a lost connection produced exit code %d", *res.ExitCode)
	}
}

// --- the audit line -------------------------------------------------------

// TestTheAuditLineDescribesTheStepAndNoCredential is the per-step audit
// requirement, asserted against a real run so the fields that come from the
// far side (the host identity, the SSH user) are real rather than echoed
// back from the request.
func TestTheAuditLineDescribesTheStepAndNoCredential(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	conn := connectionFor(t, h, machines.ExecUser)
	client := dial(t, h, conn)

	script := []byte("printf 'quiesced\\n'\n")
	sum := sha256.Sum256(script)
	req := remoteexec.Request{
		Token:        "backupd-exec-audit",
		Script:       script,
		Environ:      []string{"PGPASSWORD=audit-secret-value"},
		BackupSet:    "production/db",
		StepID:       "0001~set~before~10-quiesce.remote.sh",
		ScriptName:   "10-quiesce.remote.sh",
		ScriptSHA256: hex.EncodeToString(sum[:]),
		Timeout:      30 * time.Second,
	}

	res, _, err := runHook(t, h, client, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	line := remoteexec.NewAudit(conn, req, res, nil).String()
	for _, want := range []string{
		"backup_set=production/db",
		"script=10-quiesce.remote.sh",
		"sha256=" + hex.EncodeToString(sum[:]),
		"ssh_user=" + machines.ExecUser,
		"host_key=SHA256:",
		"exit=0",
		"outcome=completed",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the audit line is missing %q:\n%s", want, line)
		}
	}
	for _, forbidden := range []string{"audit-secret-value", filepath.Base(h.KeyFile), h.KeyFile} {
		if strings.Contains(line, forbidden) {
			t.Errorf("the audit line carries credential material (%q):\n%s", forbidden, line)
		}
	}
}

// TestAKeyFileTheDeploymentNoLongerOwnsIsRefused proves the exec path did
// not get a weaker version of the transfer path's custody rules. It is here
// rather than only in a unit test because the exec client reads the key
// itself, which the transfer path never does.
func TestAKeyFileTheDeploymentNoLongerOwnsIsRefused(t *testing.T) {
	h := machines.Start(t).ExecHost(t)

	copied := filepath.Join(t.TempDir(), "id_ed25519")
	raw, err := os.ReadFile(h.KeyFile)
	if err != nil {
		t.Fatalf("reading the fixture key: %v", err)
	}
	if err := os.WriteFile(copied, raw, 0o644); err != nil {
		t.Fatalf("writing the widened key: %v", err)
	}

	conn := connectionFor(t, h, machines.ExecUser)
	conn.Source.KeyFile = copied

	if _, err := remoteexec.Dial(h.Context(), conn); err == nil {
		t.Fatal("a world-readable key file was used to open an exec channel")
	}
}

// TestAHookSeesTheResolvedEnvironmentAndNothingTheAccountExported is the
// environment-parity criterion, measured where it can actually be wrong.
//
// An SSH exec channel inherits what the server and the account put there:
// HOME, USER, LOGNAME, LANG, MAIL, and SSH_CONNECTION, which sshd sets for
// every session. A NAME.local.sh gets none of them -- the host runner hands
// execve a block built from workflow.SanitizedBaseline, one variable -- so
// a remote hook that inherited them would mean something different on the
// two executors, which is the one thing a shared envelope exists to stop.
func TestAHookSeesTheResolvedEnvironmentAndNothingTheAccountExported(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	client := dial(t, h, connectionFor(t, h, machines.ExecUser))

	// env -0 rather than `env`, so a value containing a newline cannot
	// look like two variables and an absent variable cannot be spelled by
	// one that merely contains a line break.
	script := []byte("env -0 | tr '\\0' '\\n' | sed -n 's/=.*//p' | sort | sed 's/^/name=/'\n" +
		"printf 'pgdatabase=[%s]\\n' \"${PGDATABASE-unset}\"\n" +
		"printf 'path=[%s]\\n' \"${PATH-unset}\"\n")

	res, s, err := runHook(t, h, client, remoteexec.Request{
		Token:   "backupd-exec-environment",
		Script:  script,
		Environ: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "PGDATABASE=orders"},
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("the hook failed: %v %q", res.ExitCode, s.text(workflowexec.StreamStderr))
	}

	stdout := s.text(workflowexec.StreamStdout)
	names := map[string]bool{}
	for _, line := range strings.Split(stdout, "\n") {
		if name, found := strings.CutPrefix(strings.TrimSpace(line), "name="); found {
			names[name] = true
		}
	}
	if len(names) == 0 {
		t.Fatalf("the hook reported no environment at all, so this test proved nothing:\n%s", stdout)
	}

	// SSH_CONNECTION is the account-and-server variable this cannot be
	// wrong about: nothing in this product ever sets it, sshd always
	// does, and its presence is proof the hook is running in the
	// account's environment rather than the resolved one.
	for _, forbidden := range []string{"SSH_CONNECTION", "SSH_CLIENT", "HOME", "USER", "LOGNAME", "MAIL"} {
		if names[forbidden] {
			t.Errorf("the hook inherited %s from the remote account, so its environment is the account's with this product's layered on top:\n%s",
				forbidden, stdout)
		}
	}
	if !strings.Contains(stdout, "pgdatabase=[orders]") {
		t.Errorf("a declared variable did not reach the hook: %q", stdout)
	}
	if !strings.Contains(stdout, "path=[/usr/local/sbin:") {
		t.Errorf("the resolved PATH did not reach the hook: %q", stdout)
	}
}

// TestTwoStepsWhoseTokensSharePrefixDoNotKillEachOther is the reaper's
// blast radius, measured with two hooks running at the same moment.
//
// Step tokens are generated from a run and a step, so they are related
// strings by construction: "…-before-10" is a prefix of "…-before-10-b". A
// reaper that looked for its token anywhere in a command line would find
// the OTHER step's process group and kill a hook that was running
// perfectly, on a different backup set, while reporting a successful
// termination of this one.
func TestTwoStepsWhoseTokensSharePrefixDoNotKillEachOther(t *testing.T) {
	h := machines.Start(t).ExecHost(t)

	const shortToken = "backupd-exec-prefix"
	const longToken = "backupd-exec-prefix-and-more"

	// Two connections, because one client holds one connection and these
	// two steps genuinely run at the same time.
	victimClient := dial(t, h, connectionFor(t, h, machines.ExecUser))
	doomedClient := dial(t, h, connectionFor(t, h, machines.ExecUser))

	victimSink := &sink{}
	victimDone := make(chan struct{})
	var victimRes remoteexec.Result
	var victimErr error
	go func() {
		defer close(victimDone)
		victimRes, victimErr = victimClient.Run(h.Context(), remoteexec.Request{
			Token:   longToken,
			Script:  []byte("printf 'started\\n'\nsleep 6\nprintf 'survived\\n'\n"),
			Sink:    victimSink,
			Timeout: 120 * time.Second,
		})
	}()
	if !awaitOutput(t, victimSink, "started", 60*time.Second) {
		t.Fatal("the step that is supposed to survive never started")
	}

	// The step that gets stopped carries the SHORTER token, so its
	// reaper is the one whose match could reach the other.
	doomedSink := &sink{}
	_, doomedErr := doomedClient.Run(h.Context(), remoteexec.Request{
		Token:   shortToken,
		Script:  []byte("printf 'started\\n'\nsleep 617\n"),
		Sink:    doomedSink,
		Timeout: 3 * time.Second,
	})
	if !errors.Is(doomedErr, remoteexec.ErrStepTimeout) {
		t.Fatalf("the step that was supposed to be stopped did not time out: %v", doomedErr)
	}

	<-victimDone
	if victimErr != nil {
		t.Fatalf("a hook running normally on another connection was stopped by the other step's reaper: %v", victimErr)
	}
	if victimRes.ExitCode == nil || *victimRes.ExitCode != 0 {
		t.Fatalf("the surviving step's exit status is %v, want 0 -- its process group was signalled by the other step's reaper", victimRes.ExitCode)
	}
	if got := victimSink.text(workflowexec.StreamStdout); !strings.Contains(got, "survived") {
		t.Errorf("the surviving step did not finish its script: %q", got)
	}
}

// TestAGroupIsReapedAfterItsLeaderHasGone is the case the exec-channel
// signal request cannot be the mechanism for.
//
// The hook's leader exits immediately, leaving a child that ignores TERM
// and holds the channel's stdout open -- so the session never completes and
// the step has to be terminated. The token lives on the LEADER's command
// line, which is no longer there, so a reaper that could only match the
// command line would find nothing and report a clean "nothing was running"
// about a process that is still running. What reaches it is the process
// group id, captured while the step was alive, and the KILL that follows
// the TERM the child ignores.
func TestAGroupIsReapedAfterItsLeaderHasGone(t *testing.T) {
	h := machines.Start(t).ExecHost(t)
	client := dial(t, h, connectionFor(t, h, machines.ExecUser))

	const marker = "sleep 618"
	s := &sink{}
	res, err := client.Run(h.Context(), remoteexec.Request{
		Token:   "backupd-exec-orphaned-group",
		Script:  []byte("sh -c 'trap \"\" TERM; printf \"started\\n\"; " + marker + "' &\nsleep 3\nexit 0\n"),
		Sink:    s,
		Timeout: 8 * time.Second,
	})

	if !errors.Is(err, remoteexec.ErrStepTimeout) {
		t.Fatalf("the step did not outrun its bound, so the leader-has-gone case was never reached: %v (exit %v)", err, res.ExitCode)
	}
	if res.Certainty != workflowexec.TerminationConfirmed {
		t.Errorf("certainty = %q, want %q: the step's process group was still reachable by the id captured while it ran (reaper: %s)",
			res.Certainty, workflowexec.TerminationConfirmed, res.Reaper)
	}
	// The reaper has to say it killed a GROUP. "nothing was running" is
	// what a reaper that could only match the vanished leader's command
	// line reports, about a process that is still there.
	if !strings.Contains(res.Reaper, "killed process group") {
		t.Errorf("the reaper found nothing to kill once the leader had gone: %s", res.Reaper)
	}

	// The assertion that cannot be satisfied by bookkeeping: the
	// TERM-ignoring child is gone from the remote host.
	awaitGone(t, h, marker, 15*time.Second)
	t.Cleanup(func() { h.Inside(t, "pkill", "-f", marker) })
}
