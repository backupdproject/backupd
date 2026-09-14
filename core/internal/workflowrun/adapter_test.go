package workflowrun

import (
	"errors"
	"fmt"
	"testing"

	"github.com/backupdproject/backupd/core/internal/remoteexec"
)

// TestARemoteStepThatNeverStartedIsNotReportedAsAnOutcomeNobodySaw is the
// diagnostic half of #919. The two dispositions are not interchangeable:
// transport_lost says a hook may have run and half applied its side
// effects with nobody to say what it did, and not_attempted says nothing
// of it ran at all. An operator reading the first about a pre-backup
// quiesce goes looking for a database left mid-flush.
//
// remoteexec's two pre-execution refusals -- ErrConnection (the request or
// the connection was refused before a session was asked for) and
// ErrExecCapability (the account authenticates and may not run a command)
// -- both happen before the exec channel exists. They were reaching the
// err != nil catch-all, which is how #919 spent two milestones being read
// as a flapping network.
func TestARemoteStepThatNeverStartedIsNotReportedAsAnOutcomeNobodySaw(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want Disposition
	}{
		{
			name: "a request refused before any session",
			err:  fmt.Errorf("%w: %q is not a usable step token", remoteexec.ErrConnection, "0002~set~before~10-quiesce.remote.sh"),
			want: DispositionNotAttempted,
		},
		{
			name: "an account that is not exec-capable",
			err:  fmt.Errorf("%w: an SFTP-only account", remoteexec.ErrExecCapability),
			want: DispositionNotAttempted,
		},
		{
			name: "a connection that failed under a running step",
			err:  fmt.Errorf("%w: the exec channel failed", remoteexec.ErrTransportLoss),
			want: DispositionTransportLost,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out := remoteOutcome(remoteexec.Result{}, tc.err)
			if out.Disposition != tc.want {
				t.Errorf("disposition %q, want %q (detail: %s)", out.Disposition, tc.want, out.Detail)
			}
			if out.ExitCode != nil {
				t.Errorf("a step that reported no status carries exit code %d", *out.ExitCode)
			}
			if out.Detail == "" {
				t.Error("the refusal was recorded without the reason, which is the only thing an operator can act on")
			}
			if err := out.validate(); err != nil {
				t.Errorf("the outcome is one the engine refuses: %v", err)
			}
		})
	}
}

// TestATimeoutIsStillATimeoutHoweverItIsWrapped guards the order the
// dispositions are decided in. A step this product stopped is wrapped in
// ErrConnection or ErrExecCapability by nothing, but a step that was
// stopped and then failed to be reaped over a broken connection carries
// both sentinels -- and the one that matters is why the step ended, not
// what the cleanup afterwards could not do.
func TestATimeoutIsStillATimeoutHoweverItIsWrapped(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("%w (the reaper could not run: %w)", remoteexec.ErrStepTimeout, remoteexec.ErrConnection)
	if !errors.Is(err, remoteexec.ErrConnection) {
		t.Fatal("the fixture error does not carry both sentinels, so this test is not about anything")
	}

	if out := remoteOutcome(remoteexec.Result{}, err); out.Disposition != DispositionTimedOut {
		t.Errorf("disposition %q, want %q: a step stopped for outliving its bound ran", out.Disposition, DispositionTimedOut)
	}
}
