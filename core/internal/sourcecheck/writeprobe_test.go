package sourcecheck

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/backupdproject/backupd/core/internal/transport"
)

// Issue #852: whether a source may be WRITTEN to is a seventh thing this
// check proves, and it proves it by doing it.
//
// The three properties held below are the whole contract, and each of them
// is a way the feature could be built wrong:
//
//   - A refused write is a POSTURE, not a failure. A check that reported
//     not-OK for a read-only account would refuse the deployment
//     docs/ssh-setup.md recommends.
//   - Writable is a MACHINE-READABLE answer, never a sentence a surface
//     has to parse. The UI disables a control on it and the service
//     refuses a delete-enabling write on it.
//   - The underlying error never reaches the report. Same FR-33 rule as
//     every other step here.

// probeDeps is depsFor plus one write-probe outcome, so a test states only
// the thing it is about.
func probeDeps(t *testing.T, hostKey ssh.PublicKey, addr string, probeErr error) Deps {
	t.Helper()
	deps := depsFor("SHA256:aClientKeyFingerprint", trustedKeysOf(hostKey), knownHostsVerifier(t, addr, hostKey), 3)
	deps.ProbeWrite = func(context.Context) error { return probeErr }
	return deps
}

func probeTarget(host string, port int) Target {
	return Target{
		BackupSetID: "api-server/var-backups",
		Host:        host, Port: port,
		User:       "backups",
		RemotePath: "/var/backups",
	}
}

// TestRun_WriteProbeProvesWritable is the positive control: a full
// write-and-remove round trip is what makes delete-from-source available,
// and the report has to say so in a field rather than only in prose.
func TestRun_WriteProbeProvesWritable(t *testing.T) {
	_, pub := clientKey(t)
	host, port, hostKey := inProcessSSH(t, pub)

	report := Run(context.Background(), probeTarget(host, port), probeDeps(t, hostKey, addrOf(host, port), nil))

	if !report.OK {
		t.Fatalf("a source that answers everything and accepts a probe write reported not OK: %+v", report.Checks)
	}
	if !report.Writable {
		t.Error("Writable is false after a probe write that succeeded; this is the field the UI enables delete-from-source on")
	}
	c := checkFor(t, report, StepWriteProbe)
	if c.Outcome != Passed {
		t.Fatalf("write_probe outcome %q, want %q: %s", c.Outcome, Passed, c.Detail)
	}
	if !strings.Contains(c.Detail, "remove") && !strings.Contains(c.Detail, "removed") {
		t.Errorf("write_probe detail does not say the probe was removed again, which is half of what was proven: %q", c.Detail)
	}
}

// TestRun_RefusedWriteIsReadOnlyAndNotAFailure is the acceptance criterion
// that matters most: the recommended hardened source account may well be
// read-only, and a connection test that called that a failure would be
// refusing a supported deployment.
func TestRun_RefusedWriteIsReadOnlyAndNotAFailure(t *testing.T) {
	_, pub := clientKey(t)
	host, port, hostKey := inProcessSSH(t, pub)

	refused := transport.NewError(transport.PermissionDenied, "probe_source_write", errors.New("sftp: permission denied"))
	report := Run(context.Background(), probeTarget(host, port), probeDeps(t, hostKey, addrOf(host, port), refused))

	if !report.OK {
		t.Fatalf("a read-only source reported the whole connection test as failed; read-only is a supported posture, not a broken connection: %+v", report.Checks)
	}
	if report.Writable {
		t.Fatal("Writable is true after a probe write the server refused")
	}
	c := checkFor(t, report, StepWriteProbe)
	if c.Outcome != Passed {
		t.Errorf("write_probe outcome %q, want %q: the step ran and answered, and only its answer was no", c.Outcome, Passed)
	}
	if !strings.Contains(c.Detail, "read-only") {
		t.Errorf("write_probe detail does not name the posture an operator now has: %q", c.Detail)
	}
	if strings.Contains(c.Detail, "permission denied") {
		t.Errorf("write_probe detail carries the underlying error's own text, which FR-33 keeps off this surface: %q", c.Detail)
	}
	if _, stopped := report.StoppedAt(); stopped {
		t.Error("StoppedAt reports a failed step for a source that is merely read-only")
	}
}

// TestRun_WriteProbeSaysWhenAProbeWasLeftBehind holds the one outcome that
// costs an operator something: the write landed and the removal did not, so
// there is a file of ours on their machine and the report has to say so, by
// the name they can find it under.
func TestRun_WriteProbeSaysWhenAProbeWasLeftBehind(t *testing.T) {
	_, pub := clientKey(t)
	host, port, hostKey := inProcessSSH(t, pub)

	// The sentinel has to be reachable by errors.Is, which is how the
	// wording is chosen; a message match would drift.
	left := transport.NewError(transport.PermissionDenied, "probe_source_write",
		errors.Join(transport.ErrProbeNotRemoved, errors.New("sftp: permission denied")))

	report := Run(context.Background(), probeTarget(host, port), probeDeps(t, hostKey, addrOf(host, port), left))

	if report.Writable {
		t.Fatal("Writable is true for a probe that could not be removed; a source this manager cannot delete from must never have delete-from-source enabled")
	}
	c := checkFor(t, report, StepWriteProbe)
	if !strings.Contains(c.Detail, transport.ProbeObjectPrefix) {
		t.Errorf("write_probe detail does not name the prefix the leftover object can be found under, which is the one thing an operator needs here: %q", c.Detail)
	}
	if !report.OK {
		t.Error("a leftover probe made the whole connection test fail; it is a cleanup an operator has to do, not a connection that does not work")
	}
}

// TestRun_WriteProbeIsSkippedWhenTheListNeverRan keeps the skip discipline
// this package is built on: a probe that never ran must never read as a
// proven answer in either direction.
func TestRun_WriteProbeIsSkippedWhenTheListNeverRan(t *testing.T) {
	_, pub := clientKey(t)
	host, port, hostKey := inProcessSSH(t, pub)

	probed := false
	deps := probeDeps(t, hostKey, addrOf(host, port), nil)
	deps.List = func(context.Context) (int, error) {
		return 0, transport.NewError(transport.Configuration, "list", errors.New("no such directory"))
	}
	deps.ProbeWrite = func(context.Context) error {
		probed = true
		return nil
	}

	report := Run(context.Background(), probeTarget(host, port), deps)

	if probed {
		t.Error("the write probe ran after the remote path could not even be listed; nothing may be written to a path this check has not established")
	}
	if report.Writable {
		t.Error("Writable is true off a probe that never ran")
	}
	c := checkFor(t, report, StepWriteProbe)
	if c.Outcome != Skipped {
		t.Errorf("write_probe outcome %q, want %q", c.Outcome, Skipped)
	}
	if c.Detail == "" {
		t.Error("write_probe was skipped with no reason, so a reader cannot tell why it never ran")
	}
}

// TestRun_WriteProbeObservesTheCauseAndPublishesNone is FR-33 on this
// step: the classified cause goes to the operator's log, and the report
// carries this package's own sentence and nothing else.
func TestRun_WriteProbeObservesTheCauseAndPublishesNone(t *testing.T) {
	_, pub := clientKey(t)
	host, port, hostKey := inProcessSSH(t, pub)

	cause := errors.New("sftp: ssh: /home/deploy/.ssh/id_ed25519: permission denied writing /srv/backups")
	deps := probeDeps(t, hostKey, addrOf(host, port), transport.NewError(transport.PermissionDenied, "probe_source_write", cause))
	var observed []error
	deps.Observe = func(step Step, err error) {
		if step == StepWriteProbe {
			observed = append(observed, err)
		}
	}

	report := Run(context.Background(), probeTarget(host, port), deps)

	if len(observed) != 1 {
		t.Fatalf("the write probe's cause reached Observe %d times, want exactly once: the log is where a path belongs", len(observed))
	}
	if !errors.Is(observed[0], cause) {
		t.Errorf("Observe got %v, want the classified cause itself", observed[0])
	}
	detail := checkFor(t, report, StepWriteProbe).Detail
	for _, leak := range []string{"/home/deploy", "id_ed25519", "/srv/backups"} {
		if strings.Contains(detail, leak) {
			t.Errorf("write_probe detail carries %q from the underlying error; nothing on this surface may (FR-33): %q", leak, detail)
		}
	}
}
