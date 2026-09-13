package remoteexec

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Audit is one remote step's record, as #810 requires it: enough to answer
// "what ran where, as whom, and what happened", and nothing that could
// answer "with what credential".
//
// It is a type rather than a log call because this package does not own
// where the record goes -- the journal and the activity feed do -- and
// because the fields that must NOT be here are easier to keep out of a
// struct than out of a format string. There is no field for a key, a
// passphrase, an environment value or a script body, and String cannot
// print what the struct cannot hold.
type Audit struct {
	// BackupSet is which set this step belonged to.
	BackupSet string

	// ConnectionRef and ConnectionKind are which connection ran it and
	// which of the two resolution rules produced that connection. The
	// kind matters to a reader: "backup-set-source" says the transfer
	// credential was reused, which is the case that had to be proven
	// exec-capable.
	ConnectionRef  string
	ConnectionKind ConnectionKind

	// Host and User are the remote identity. Host is the endpoint as
	// configured; HostKeyFingerprint is the key it actually presented,
	// which is the only unforgeable half of "which machine was this".
	Host               string
	User               string
	HostKeyFingerprint string

	// StepID, ScriptName and ScriptSHA256 identify what ran. The hash is
	// the whole point of carrying it: it is what lets somebody prove
	// months later that the script in the spool is the script that ran.
	StepID       string
	ScriptName   string
	ScriptSHA256 string

	// StartedAt and FinishedAt bracket the session.
	StartedAt  time.Time
	FinishedAt time.Time

	// ExitCode is nil when no exit status was observed. See Result.
	ExitCode *int

	// Signal is the remote signal that killed the hook, when one did.
	Signal string

	// Certainty is what was proved about termination, verbatim from the
	// Result. It is recorded even when it is empty ("not requested"),
	// because an audit line that omitted it would read as a clean finish.
	Certainty string

	// Outcome is the one-line verdict: "completed", or the sentinel-level
	// reason it did not.
	Outcome string
}

// NewAudit assembles the record from the request that ran and the result
// that came back, so the two cannot drift apart at a call site.
func NewAudit(conn Connection, req Request, res Result, err error) Audit {
	return Audit{
		BackupSet:          req.BackupSet,
		ConnectionRef:      conn.Ref,
		ConnectionKind:     conn.Kind,
		Host:               conn.Source.Host,
		User:               res.User,
		HostKeyFingerprint: res.HostKeyFingerprint,
		StepID:             req.StepID,
		ScriptName:         req.ScriptName,
		ScriptSHA256:       req.ScriptSHA256,
		StartedAt:          res.StartedAt,
		FinishedAt:         res.FinishedAt,
		ExitCode:           res.ExitCode,
		Signal:             res.Signal,
		Certainty:          string(res.Certainty),
		Outcome:            outcomeOf(err),
	}
}

// outcomeOf names the verdict in the vocabulary the sentinels already
// define, so an audit line and a journal state cannot disagree about what
// happened.
func outcomeOf(err error) string {
	switch {
	case err == nil:
		return "completed"
	case errors.Is(err, ErrStepTimeout):
		return "timed-out"
	case errors.Is(err, ErrStepCanceled):
		return "canceled"
	case errors.Is(err, ErrStepSignaled):
		return "signaled"
	case errors.Is(err, ErrExecCapability):
		return "refused-not-exec-capable"
	case errors.Is(err, ErrHostKeyPolicy):
		return "refused-host-key-policy"
	case errors.Is(err, ErrTransportLoss):
		return "transport-loss"
	case errors.Is(err, ErrConnection):
		return "refused-connection"
	default:
		return "failed"
	}
}

// String renders the record as one line an operator can read and a log can
// hold.
//
// The exit code is "unobserved" rather than a number when there is none.
// Printing 0 there, or -1, would be this product inventing a status the
// hook never returned -- which is the same refusal Result.ExitCode's
// pointer exists for, carried through to the one place a human reads it.
func (a Audit) String() string {
	exit := "unobserved"
	if a.ExitCode != nil {
		exit = fmt.Sprintf("%d", *a.ExitCode)
	}
	certainty := a.Certainty
	if certainty == "" {
		certainty = "not-requested"
	}

	fields := []string{
		"backup_set=" + a.BackupSet,
		"step=" + a.StepID,
		"script=" + a.ScriptName,
		"sha256=" + a.ScriptSHA256,
		"connection=" + a.ConnectionRef,
		"connection_kind=" + string(a.ConnectionKind),
		"host=" + a.Host,
		"host_key=" + a.HostKeyFingerprint,
		"ssh_user=" + a.User,
		"started=" + renderTime(a.StartedAt),
		"finished=" + renderTime(a.FinishedAt),
		"exit=" + exit,
		"termination=" + certainty,
		"outcome=" + a.Outcome,
	}
	if a.Signal != "" {
		fields = append(fields, "signal=SIG"+a.Signal)
	}

	return strings.Join(fields, " ")
}

func renderTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}

	return t.UTC().Format(time.RFC3339)
}
