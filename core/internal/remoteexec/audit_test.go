package remoteexec

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/transport"
	"github.com/retnd/retnd/core/internal/workflowexec"
)

type countingSink struct {
	mu     sync.Mutex
	chunks []workflowexec.Chunk
}

func (s *countingSink) Chunk(c workflowexec.Chunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks = append(s.chunks, workflowexec.Chunk{Stream: c.Stream, Seq: c.Seq, Data: append([]byte(nil), c.Data...)})

	return nil
}

func (s *countingSink) text(stream workflowexec.StreamID) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var b strings.Builder
	for _, c := range s.chunks {
		if c.Stream == stream {
			b.Write(c.Data)
		}
	}

	return b.String()
}

func (s *countingSink) all() []workflowexec.Chunk {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]workflowexec.Chunk(nil), s.chunks...)
}

// TestAuditCarriesEveryFactAndNoCredential is #810's audit requirement read
// as two halves. The first is a list of facts, and a missing one makes the
// line useless months later. The second is what must never be there, and it
// is tested against material that WAS in play for the step -- a key path, a
// passphrase, a secret environment value -- rather than against an invented
// string, because the only interesting failure is one of those leaking.
func TestAuditCarriesEveryFactAndNoCredential(t *testing.T) {
	t.Parallel()

	conn := Connection{
		Ref:  "db-hooks",
		Kind: KindDeclared,
		Source: transport.Source{
			Type:       "sftp",
			Host:       "db.example.com",
			User:       "backupd-hooks",
			KeyFile:    "/etc/backupd/hooks.key",
			KnownHosts: "/etc/backupd/known_hosts",
		},
	}
	req := Request{
		Token:        "backupd-exec-run1-0002",
		BackupSet:    "production/db",
		StepID:       "0002~set~before~10-quiesce.remote.sh",
		ScriptName:   "10-quiesce.remote.sh",
		ScriptSHA256: "3a7bd3e2360a3d29eea436fcfb7e44c735d117c42d1c1835420b6b9942dd4f1b",
		Environ:      []string{"PGPASSWORD=super-secret-value"},
	}
	exit := 0
	res := Result{
		ExitCode:           &exit,
		User:               "backupd-hooks",
		HostKeyFingerprint: "SHA256:abc123",
		StartedAt:          time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC),
		FinishedAt:         time.Date(2026, 9, 13, 10, 0, 4, 0, time.UTC),
		Certainty:          workflowexec.TerminationNotRequested,
	}

	line := NewAudit(conn, req, res, nil).String()

	for _, want := range []string{
		"backup_set=production/db",
		"step=0002~set~before~10-quiesce.remote.sh",
		"script=10-quiesce.remote.sh",
		"sha256=3a7bd3e2360a3d29eea436fcfb7e44c735d117c42d1c1835420b6b9942dd4f1b",
		"connection=db-hooks",
		"connection_kind=declared",
		"host=db.example.com",
		"host_key=SHA256:abc123",
		"ssh_user=backupd-hooks",
		"started=2026-09-13T10:00:00Z",
		"finished=2026-09-13T10:00:04Z",
		"exit=0",
		"termination=not-requested",
		"outcome=completed",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the audit line is missing %q:\n%s", want, line)
		}
	}

	for _, forbidden := range []string{
		"super-secret-value",
		"PGPASSWORD",
		"/etc/backupd/hooks.key",
		"/etc/backupd/known_hosts",
	} {
		if strings.Contains(line, forbidden) {
			t.Errorf("the audit line carries %q, which is credential material or its location:\n%s", forbidden, line)
		}
	}
}

// TestAuditNeverInventsAnExitCode is the pointer's whole purpose, carried
// through to the one place a human reads it. A timed-out step that printed
// exit=0 would read as a hook that succeeded.
func TestAuditNeverInventsAnExitCode(t *testing.T) {
	t.Parallel()

	res := Result{Certainty: workflowexec.TerminationUnconfirmed}
	line := NewAudit(Connection{Ref: "r"}, Request{}, res, fmt.Errorf("%w: bound", ErrStepTimeout)).String()

	if !strings.Contains(line, "exit=unobserved") {
		t.Errorf("a step with no observed status did not say so:\n%s", line)
	}
	if strings.Contains(line, "exit=0") || strings.Contains(line, "exit=-1") {
		t.Errorf("a status was invented:\n%s", line)
	}
	if !strings.Contains(line, "termination=unconfirmed") {
		t.Errorf("the certainty is missing:\n%s", line)
	}
	if !strings.Contains(line, "outcome=timed-out") {
		t.Errorf("the outcome is missing:\n%s", line)
	}
}

func TestAuditOutcomeVocabulary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		err  error
		want string
	}{
		{nil, "completed"},
		{fmt.Errorf("%w: x", ErrStepTimeout), "timed-out"},
		{fmt.Errorf("%w: x", ErrStepCanceled), "canceled"},
		{fmt.Errorf("%w: x", ErrStepSignaled), "signaled"},
		{fmt.Errorf("%w: x", ErrExecCapability), "refused-not-exec-capable"},
		{fmt.Errorf("%w: x", ErrHostKeyPolicy), "refused-host-key-policy"},
		{fmt.Errorf("%w: x", ErrTransportLoss), "transport-loss"},
		{fmt.Errorf("%w: x", ErrConnection), "refused-connection"},
		{errors.New("something else entirely"), "failed"},
	}
	for _, c := range cases {
		if got := outcomeOf(c.err); got != c.want {
			t.Errorf("outcomeOf(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestTransportLossIsNeverAnExitCode is a technical requirement of #810 in
// one assertion: the two failures a reader must never confuse are a hook
// that decided something and a connection that dropped.
func TestTransportLossIsNeverAnExitCode(t *testing.T) {
	t.Parallel()

	c := &Client{conn: Connection{Ref: "r", Source: transport.Source{User: "u"}}, addr: "h:22"}

	res, err := c.finish(Result{}, errors.New("connection reset by peer"))
	if err == nil {
		t.Fatal("a failed channel was reported as success")
	}
	if !errors.Is(err, ErrTransportLoss) {
		t.Errorf("error %v is not an ErrTransportLoss", err)
	}
	if res.ExitCode != nil {
		t.Errorf("a transport loss produced an exit code (%d)", *res.ExitCode)
	}
}

func TestReaperScriptCannotBeSteeredByItsToken(t *testing.T) {
	t.Parallel()

	// The token is quoted rather than interpolated, so even a token that
	// escaped Request.validate could not become syntax. Both defences are
	// asserted because the reaper sends signals: a single quoting mistake
	// here is a remote kill with an attacker-chosen argument.
	script := reaperScript("tok'; kill -9 -1 #", "4321'; kill -9 -1 #")
	if strings.Contains(script, "kill -9 -1 #") && !strings.Contains(script, `'\''`) {
		t.Error("the token reached the reaper script unquoted")
	}
	if !strings.Contains(script, `tok=`) {
		t.Error("the reaper does not assign the token at all")
	}
	// The guards that make a malformed ps line, or a group id read back
	// from the far side, harmless. "kill -TERM -1" would signal every
	// process the account owns.
	for _, guard := range []string{
		`case "$pgid" in ''|*[!0-9]*) continue ;; 0|1) continue ;; esac`,
		`  ''|*[!0-9]*|0|1) ;;`,
	} {
		if !strings.Contains(script, guard) {
			t.Errorf("the reaper is missing the process-group guard %q", guard)
		}
	}
	// No here-document: bash implements one with a temporary file, and
	// this package leaves nothing on the remote host.
	if strings.Contains(script, "<<") {
		t.Error("the reaper uses a here-document, which bash implements with a temporary file on the remote host")
	}
}
