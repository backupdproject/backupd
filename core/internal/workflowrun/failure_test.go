package workflowrun

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/backupdproject/backupd/core/internal/workflow"
	"github.com/backupdproject/backupd/core/internal/workflowexec"
)

// The failure matrix (#811's Failure semantics section), one row per way
// a run can go wrong.
//
// Every row asserts the same four things, because they are the four an
// operator acts on and they are separate fields end to end: what got
// dispatched, what the backup did, what the cleanup did, and what the
// workflow's verdict was. A test that only checked the verdict would
// pass against an engine that reported a failed cleanup as a failed
// backup.

type failureRow struct {
	name string

	// tree is what is configured, and fail names the script that does
	// not succeed, with the outcome it produces.
	scripts map[stage][]string
	fail    string
	outcome func(context.Context, StepRequest) (StepOutcome, error)
	backup  func(context.Context) error

	wantDispatched []string
	wantState      workflow.State
	wantBackup     workflow.Status
	wantCleanup    workflow.Status
	wantWorkflow   workflow.Status
	wantFailedName string
}

func fullTree() map[stage][]string {
	return map[stage][]string{
		globalBefore: {"10-mount.local.sh", "20-second.local.sh"},
		setBefore:    {"10-quiesce.remote.sh"},
		setAfter:     {"10-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	}
}

func TestFailureMatrix(t *testing.T) {
	t.Parallel()

	rows := []failureRow{
		{
			// A "before" failure marks the step failed, stops the later
			// "before" steps, SKIPS the backup, enters eligible cleanup
			// and fails the workflow. The backup-set scope was never
			// entered, so its "after" stage is not eligible and does not
			// run: only the global one does.
			name:    "global before fails",
			scripts: fullTree(),
			fail:    "10-mount.local.sh",
			wantDispatched: []string{
				"local:10-mount.local.sh",
				"local:90-unmount.local.sh",
			},
			wantState:      workflow.StateFailed,
			wantBackup:     workflow.StatusSkipped,
			wantCleanup:    workflow.StatusSuccess,
			wantWorkflow:   workflow.StatusFailed,
			wantFailedName: "10-mount.local.sh",
		},
		{
			// A backup-set "before" failure makes BOTH "after" stages
			// eligible, because the backup-set scope was entered before
			// that step ran.
			name:    "set before fails",
			scripts: fullTree(),
			fail:    "10-quiesce.remote.sh",
			wantDispatched: []string{
				"local:10-mount.local.sh",
				"local:20-second.local.sh",
				"remote:10-quiesce.remote.sh",
				"remote:10-resume.remote.sh",
				"local:90-unmount.local.sh",
			},
			wantState:      workflow.StateFailed,
			wantBackup:     workflow.StatusSkipped,
			wantCleanup:    workflow.StatusSuccess,
			wantWorkflow:   workflow.StatusFailed,
			wantFailedName: "10-quiesce.remote.sh",
		},
		{
			// A backup failure still runs the eligible "after" steps:
			// whatever the "before" hooks did to the machine has to be
			// undone whether or not the backup worked.
			name:    "backup fails",
			scripts: fullTree(),
			backup:  func(context.Context) error { return errors.New("the repository is full") },
			wantDispatched: []string{
				"local:10-mount.local.sh",
				"local:20-second.local.sh",
				"remote:10-quiesce.remote.sh",
				"backup",
				"remote:10-resume.remote.sh",
				"local:90-unmount.local.sh",
			},
			wantState:    workflow.StateFailed,
			wantBackup:   workflow.StatusFailed,
			wantCleanup:  workflow.StatusSuccess,
			wantWorkflow: workflow.StatusFailed,
		},
		{
			// An "after" failure is recorded, the LATER cleanup
			// continues, and the workflow fails even though the backup
			// succeeded. Those three facts in one row are why the
			// statuses are three fields.
			name:    "set after fails",
			scripts: fullTree(),
			fail:    "10-resume.remote.sh",
			wantDispatched: []string{
				"local:10-mount.local.sh",
				"local:20-second.local.sh",
				"remote:10-quiesce.remote.sh",
				"backup",
				"remote:10-resume.remote.sh",
				"local:90-unmount.local.sh",
			},
			wantState:      workflow.StateCleanupFailed,
			wantBackup:     workflow.StatusSuccess,
			wantCleanup:    workflow.StatusFailed,
			wantWorkflow:   workflow.StatusFailed,
			wantFailedName: "10-resume.remote.sh",
		},
		{
			name:    "global after fails",
			scripts: fullTree(),
			fail:    "90-unmount.local.sh",
			wantDispatched: []string{
				"local:10-mount.local.sh",
				"local:20-second.local.sh",
				"remote:10-quiesce.remote.sh",
				"backup",
				"remote:10-resume.remote.sh",
				"local:90-unmount.local.sh",
			},
			wantState:      workflow.StateCleanupFailed,
			wantBackup:     workflow.StatusSuccess,
			wantCleanup:    workflow.StatusFailed,
			wantWorkflow:   workflow.StatusFailed,
			wantFailedName: "90-unmount.local.sh",
		},
		{
			// A step this product killed for outliving its bound is
			// StateTimedOut and not StateFailed: a script that returns 1
			// decided something, and one that ran out of time is usually
			// waiting on something that never came.
			name:    "set before times out",
			scripts: fullTree(),
			fail:    "10-quiesce.remote.sh",
			outcome: func(context.Context, StepRequest) (StepOutcome, error) {
				return StepOutcome{
					Disposition: DispositionTimedOut,
					Certainty:   workflowexec.TerminationConfirmed,
				}, nil
			},
			wantDispatched: []string{
				"local:10-mount.local.sh",
				"local:20-second.local.sh",
				"remote:10-quiesce.remote.sh",
				"remote:10-resume.remote.sh",
				"local:90-unmount.local.sh",
			},
			wantState:      workflow.StateTimedOut,
			wantBackup:     workflow.StatusSkipped,
			wantCleanup:    workflow.StatusSuccess,
			wantWorkflow:   workflow.StatusFailed,
			wantFailedName: "10-quiesce.remote.sh",
		},
		{
			// A lost SSH session: the outcome was never observed, so
			// there is no exit code, and the eligible cleanup still runs.
			name:    "remote session is lost",
			scripts: fullTree(),
			fail:    "10-quiesce.remote.sh",
			outcome: func(context.Context, StepRequest) (StepOutcome, error) {
				return StepOutcome{
					Disposition: DispositionTransportLost,
					Detail:      "the remote session ended without reporting a status",
				}, nil
			},
			wantDispatched: []string{
				"local:10-mount.local.sh",
				"local:20-second.local.sh",
				"remote:10-quiesce.remote.sh",
				"remote:10-resume.remote.sh",
				"local:90-unmount.local.sh",
			},
			wantState:      workflow.StateFailed,
			wantBackup:     workflow.StatusSkipped,
			wantCleanup:    workflow.StatusSuccess,
			wantWorkflow:   workflow.StatusFailed,
			wantFailedName: "10-quiesce.remote.sh",
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			tr := newTree(t, row.scripts)

			if row.fail != "" {
				outcome := row.outcome
				if outcome == nil {
					outcome = failing(1)
				}
				h.local.outcomes[row.fail] = outcome
				h.remote.outcomes[row.fail] = outcome
			}

			backup := row.backup
			if backup == nil {
				backup = func(context.Context) error { return nil }
			}

			res, err := h.engine.Run(context.Background(), RunRequest{
				Plan:        tr.snapshot(t, "run-1"),
				BackupSetID: tr.setID,
				Backup: func(ctx context.Context) error {
					h.rec.mu.Lock()
					h.rec.calls = append(h.rec.calls, "backup")
					h.rec.mu.Unlock()

					return backup(ctx)
				},
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if got := h.rec.dispatched(); !reflect.DeepEqual(got, row.wantDispatched) {
				t.Errorf("dispatched\n\t%v\nwant\n\t%v", got, row.wantDispatched)
			}
			if res.State != row.wantState {
				t.Errorf("the run ended %q, want %q", res.State, row.wantState)
			}
			if res.BackupStatus != row.wantBackup {
				t.Errorf("backup status is %q, want %q", res.BackupStatus, row.wantBackup)
			}
			if res.CleanupStatus != row.wantCleanup {
				t.Errorf("cleanup status is %q, want %q", res.CleanupStatus, row.wantCleanup)
			}
			if res.WorkflowStatus != row.wantWorkflow {
				t.Errorf("workflow status is %q, want %q", res.WorkflowStatus, row.wantWorkflow)
			}

			if row.wantFailedName != "" {
				step := stateOf(t, h.store, "run-1", row.wantFailedName)
				if res.FailedStep != step.StepID {
					t.Errorf("the run names %q as its failed step, want %q", res.FailedStep, step.StepID)
				}
			}

			// The journal agrees with the result, which is what every
			// surface above this actually reads.
			run, err := h.store.WorkflowRun(context.Background(), "run-1")
			if err != nil {
				t.Fatalf("WorkflowRun: %v", err)
			}
			if run.State != string(row.wantState) {
				t.Errorf("the journal records the run as %q, want %q", run.State, row.wantState)
			}
			if run.BackupStatus != string(row.wantBackup) {
				t.Errorf("the journal records backup status %q, want %q", run.BackupStatus, row.wantBackup)
			}
			if run.CleanupStatus != string(row.wantCleanup) {
				t.Errorf("the journal records cleanup status %q, want %q", run.CleanupStatus, row.wantCleanup)
			}
		})
	}
}

// The exit code is the one field transport loss must never reach. A step
// whose session died has no status, and nil is how "nobody saw one" is
// spelled -- 0 would read as success and 1 as a script's own decision.
func TestALostSessionRecordsNoExitCode(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{setBefore: {"10-quiesce.remote.sh"}})

	h.remote.outcomes["10-quiesce.remote.sh"] = func(context.Context, StepRequest) (StepOutcome, error) {
		return StepOutcome{Disposition: DispositionTransportLost}, nil
	}

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	step := stateOf(t, h.store, "run-1", "10-quiesce.remote.sh")
	if step.State != string(workflow.StateFailed) {
		t.Errorf("the lost step is %q, want failed", step.State)
	}
	if step.ExitCode != nil {
		t.Errorf("the lost step recorded exit code %d; transport loss is never a known exit code", *step.ExitCode)
	}
}

// An adapter that claims an exit code alongside anything other than "the
// process exited" is not believed. This is the engine's own half of the
// rule, so a future adapter cannot introduce the confusion by mistake.
func TestAnOutcomeClaimingAnExitCodeItCannotHaveIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{setBefore: {"10-quiesce.remote.sh"}})

	code := 0
	h.remote.outcomes["10-quiesce.remote.sh"] = func(context.Context, StepRequest) (StepOutcome, error) {
		return StepOutcome{Disposition: DispositionTransportLost, ExitCode: &code}, nil
	}

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	step := stateOf(t, h.store, "run-1", "10-quiesce.remote.sh")
	if step.ExitCode != nil {
		t.Errorf("an outcome claiming a status it cannot have was recorded as exit code %d", *step.ExitCode)
	}
	if step.State != string(workflow.StateFailed) {
		t.Errorf("the step is %q, want failed", step.State)
	}
}

// Cancellation, in each of the four phases a run can be cancelled in.
//
// The rule is the same wherever it lands: stop starting new "before" or
// backup work, ask the active work to stop, mark the backup as not having
// run, and STILL attempt the eligible cleanup -- under a bound of its own,
// because the deadline the run just lost is not one the unwinding can
// inherit.
func TestCancellationInEachPhaseStillAttemptsEligibleCleanup(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		cancelDuring   string
		cancelBackup   bool
		wantDispatched []string
		wantBackup     workflow.Status

		// wantCleanup and wantObligations are per case because a
		// cancellation that lands ON a cleanup step is a cleanup that
		// did not finish unwinding, which is a different fact from one
		// that lands before the cleanup started. Both have to be
		// visible, and neither may be flattened into the cancellation.
		wantCleanup    workflow.Status
		wantObligation map[workflow.Scope]workflow.ObligationState
	}{
		{
			name:         "during global before",
			cancelDuring: "10-mount.local.sh",
			// The backup-set scope was never entered, so only the global
			// "after" stage is owed.
			wantDispatched: []string{"local:10-mount.local.sh", "local:90-unmount.local.sh"},
			wantBackup:     workflow.StatusSkipped,
			wantCleanup:    workflow.StatusSuccess,
			wantObligation: map[workflow.Scope]workflow.ObligationState{
				workflow.ScopeGlobal: workflow.ObligationSuccess,
				workflow.ScopeSet:    workflow.ObligationNeverEligible,
			},
		},
		{
			name:         "during set before",
			cancelDuring: "10-quiesce.remote.sh",
			wantDispatched: []string{
				"local:10-mount.local.sh", "local:20-second.local.sh",
				"remote:10-quiesce.remote.sh",
				"remote:10-resume.remote.sh", "local:90-unmount.local.sh",
			},
			wantBackup:  workflow.StatusSkipped,
			wantCleanup: workflow.StatusSuccess,
			wantObligation: map[workflow.Scope]workflow.ObligationState{
				workflow.ScopeGlobal: workflow.ObligationSuccess,
				workflow.ScopeSet:    workflow.ObligationSuccess,
			},
		},
		{
			name:         "during the backup",
			cancelBackup: true,
			wantDispatched: []string{
				"local:10-mount.local.sh", "local:20-second.local.sh",
				"remote:10-quiesce.remote.sh", "backup",
				"remote:10-resume.remote.sh", "local:90-unmount.local.sh",
			},
			wantBackup:  workflow.StatusFailed,
			wantCleanup: workflow.StatusSuccess,
			wantObligation: map[workflow.Scope]workflow.ObligationState{
				workflow.ScopeGlobal: workflow.ObligationSuccess,
				workflow.ScopeSet:    workflow.ObligationSuccess,
			},
		},
		{
			name:         "during set after",
			cancelDuring: "10-resume.remote.sh",
			wantDispatched: []string{
				"local:10-mount.local.sh", "local:20-second.local.sh",
				"remote:10-quiesce.remote.sh", "backup",
				"remote:10-resume.remote.sh", "local:90-unmount.local.sh",
			},
			wantBackup: workflow.StatusSuccess,

			// The cancelled hook is the set scope's own unwinding, so
			// that scope did NOT get put back -- and the global scope's
			// cleanup still ran behind it, which is the rule that later
			// cleanup continues.
			wantCleanup: workflow.StatusFailed,
			wantObligation: map[workflow.Scope]workflow.ObligationState{
				workflow.ScopeGlobal: workflow.ObligationSuccess,
				workflow.ScopeSet:    workflow.ObligationFailed,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			tr := newTree(t, fullTree())

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			stop := func(_ context.Context, _ StepRequest) (StepOutcome, error) {
				cancel()

				return StepOutcome{
					Disposition: DispositionCanceled,
					Certainty:   workflowexec.TerminationConfirmed,
				}, nil
			}
			if tc.cancelDuring != "" {
				h.local.outcomes[tc.cancelDuring] = stop
				h.remote.outcomes[tc.cancelDuring] = stop
			}

			res, err := h.engine.Run(ctx, RunRequest{
				Plan:        tr.snapshot(t, "run-1"),
				BackupSetID: tr.setID,
				Backup: func(ctx context.Context) error {
					h.rec.mu.Lock()
					h.rec.calls = append(h.rec.calls, "backup")
					h.rec.mu.Unlock()

					if tc.cancelBackup {
						cancel()

						return ctx.Err()
					}

					return nil
				},
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if got := h.rec.dispatched(); !reflect.DeepEqual(got, tc.wantDispatched) {
				t.Errorf("dispatched\n\t%v\nwant\n\t%v", got, tc.wantDispatched)
			}
			if res.State != workflow.StateCanceled {
				t.Errorf("the run ended %q, want canceled", res.State)
			}
			if res.BackupStatus != tc.wantBackup {
				t.Errorf("backup status is %q, want %q", res.BackupStatus, tc.wantBackup)
			}

			// The cleanup's own outcome is preserved rather than
			// flattened into the cancellation.
			if res.CleanupStatus != tc.wantCleanup {
				t.Errorf("cleanup status is %q, want %q -- the cancellation must not erase what the cleanup did", res.CleanupStatus, tc.wantCleanup)
			}

			// And every scope's obligation says what became of it,
			// which is what a later startup reads to decide whether
			// this machine was put back.
			for scope, want := range tc.wantObligation {
				if got := obligationOf(t, h.store, "run-1", scope).State; got != want {
					t.Errorf("the %s obligation is %q, want %q", scope, got, want)
				}
			}

			// And what this product PROVED about the termination is on
			// the record, which is the difference between "the hook was
			// killed" and "we stopped waiting for the hook".
			if tc.cancelDuring != "" {
				step := stateOf(t, h.store, "run-1", tc.cancelDuring)
				if !step.TerminationConfirmed {
					t.Errorf("the cancelled step %s does not record a confirmed termination", tc.cancelDuring)
				}
				if step.State != string(workflow.StateCanceled) {
					t.Errorf("the cancelled step is %q, want canceled", step.State)
				}
			}
		})
	}
}

// A cleanup stage does not inherit the deadline the run just lost. The
// context a cleanup step is handed is still live after the run's own
// context has been cancelled, which is what makes "attempt eligible
// cleanup under a separate bounded cleanup timeout" true rather than
// hopeful.
func TestCleanupRunsUnderItsOwnBoundAfterTheRunIsCancelled(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.local.outcomes["10-mount.local.sh"] = func(_ context.Context, _ StepRequest) (StepOutcome, error) {
		cancel()

		return StepOutcome{Disposition: DispositionCanceled}, nil
	}

	var cleanupCtxLive bool
	h.local.outcomes["90-unmount.local.sh"] = func(stepCtx context.Context, _ StepRequest) (StepOutcome, error) {
		cleanupCtxLive = stepCtx.Err() == nil
		deadline, hasDeadline := stepCtx.Deadline()
		if !hasDeadline || deadline.IsZero() {
			t.Error("a cleanup step was handed a context with no deadline; a cleanup that can hang forever holds a shutdown open")
		}

		return exited(0), nil
	}

	if _, err := h.engine.Run(ctx, RunRequest{
		Plan:        tr.snapshot(t, "run-1"),
		BackupSetID: tr.setID,
		Backup:      func(context.Context) error { return nil },
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !cleanupCtxLive {
		t.Error("the cleanup step was handed an already-cancelled context, so the unwinding inherited the deadline the run had just lost")
	}
}
