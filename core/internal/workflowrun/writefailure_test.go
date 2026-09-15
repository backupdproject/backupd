package workflowrun

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/workflow"
)

// A durable write that does not LAND, as opposed to a process that stops
// (crash_test.go).
//
// The two are different claims and this file is the second one. A crash
// leaves the engine no chance to be wrong: whatever was written is
// written, and the reconciliation reads it. A refused write leaves the
// engine running, holding a belief about the journal that is now false,
// and what has to hold is that it does not carry on as though the write
// had succeeded -- because every one of those beliefs is the permission
// slip for a side effect. A full disk, a database locked by a hung
// reader, a transition the graph refuses: all three arrive here.

// The backup-set scope cannot be recorded as entered, so NOTHING in that
// scope runs -- including the backup, which is a side effect inside it.
//
// The failure this is about produced side effects with no cleanup. The
// engine failed the run and carried straight on into the stage: the
// set-before hooks quiesced a database and the backup read it, inside a
// scope whose obligation still said "never eligible" -- which is settled,
// so the reconciliation has nothing to do with it, no hold is raised, and
// the next run goes ahead over a machine nobody put back.
func TestAScopeThatCannotBeRecordedAsEnteredRunsNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, fullTree())

	store := refusing(h.store).obligation(workflow.ScopeSet, workflow.ObligationEligible)

	backupRan := false
	res, err := h.runWith(t, store, tr.snapshot(t, "run-1"), &backupRan)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if store.failures() == 0 {
		t.Fatal("the write this test is about was never attempted")
	}

	// The global scope's hooks ran and were unwound; nothing in the
	// backup-set scope was dispatched, and the backup did not happen.
	want := []string{
		"local:10-mount.local.sh",
		"local:20-second.local.sh",
		"local:90-unmount.local.sh",
	}
	if got := h.rec.dispatched(); !reflect.DeepEqual(got, want) {
		t.Errorf("dispatched\n\t%v\nwant\n\t%v", got, want)
	}
	if backupRan {
		t.Error("the backup ran inside a scope the journal says was never entered")
	}

	// The journal says what happened: the set scope was never entered,
	// its steps were skipped rather than left pending, and the run
	// failed.
	if got := obligationOf(t, h.store, "run-1", workflow.ScopeSet).State; got != workflow.ObligationNeverEligible {
		t.Errorf("the set obligation is %q, want never_eligible", got)
	}
	for _, name := range []string{"10-quiesce.remote.sh", "10-resume.remote.sh"} {
		if got := stateOf(t, h.store, "run-1", name).State; got != string(workflow.StateSkipped) {
			t.Errorf("%s is %q, want skipped", name, got)
		}
	}
	if res.State != workflow.StateFailed {
		t.Errorf("the run ended %q, want failed", res.State)
	}
	if res.BackupStatus != workflow.StatusSkipped {
		t.Errorf("backup status is %q, want skipped", res.BackupStatus)
	}
}

// A scope whose cleanup cannot even be recorded as STARTING leaves the
// run needing recovery, and the set blocked.
//
// This is the other half of the same bug and the worse half. The after
// steps do not run, so the scope is not put back -- and the old code
// reported cleanup_status=success and wrote a TERMINAL run row over an
// obligation still sitting at "eligible". A terminal run is one the
// startup pass never looks at again, so that obligation would never
// become a hold: the machine stays quiesced and every surface says the
// run merely failed.
func TestAScopeWhoseCleanupCannotStartLeavesTheRunInRecovery(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	store := refusing(h.store).obligation(workflow.ScopeGlobal, workflow.ObligationRunning)

	backupRan := false
	res, err := h.runWith(t, store, tr.snapshot(t, "run-1"), &backupRan)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if store.failures() == 0 {
		t.Fatal("the write this test is about was never attempted")
	}

	if !backupRan {
		t.Error("the backup did not run; this test is about the cleanup, not the backup")
	}

	// The cleanup did not happen and the result says so, rather than
	// reporting the success of a stage that never started.
	if res.CleanupStatus != workflow.StatusFailed {
		t.Errorf("cleanup status is %q, want failed", res.CleanupStatus)
	}
	if res.State != workflow.StateRecoveryRequired {
		t.Errorf("the run ended %q, want recovery_required", res.State)
	}
	if !res.RecoveryOutstanding {
		t.Error("the result does not report the outstanding recovery")
	}

	// The run row is NOT terminal and carries the recovery axis, which
	// is what keeps the spool and makes the hold real.
	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.State != string(workflow.StateRecoveryRequired) {
		t.Fatalf("the journal records the run as %q, want recovery_required", run.State)
	}
	if run.RecoveryState != string(workflow.RecoveryRequired) {
		t.Errorf("the run's recovery state is %q, want required", run.RecoveryState)
	}
	if run.FinishedAt != nil {
		t.Error("a run that still needs a person carries a finish time")
	}
	if got := obligationOf(t, h.store, "run-1", workflow.ScopeGlobal).State; got != workflow.ObligationRecoveryRequired {
		t.Errorf("the global obligation is %q, want recovery_required", got)
	}

	// And the set is refused, in this process, without a restart.
	if sets := h.engine.SuspendedBackupSets(); len(sets) != 1 || sets[0] != tr.setID {
		t.Fatalf("the suspended sets are %v, want just %s", sets, tr.setID)
	}
	_, err = h.engine.Run(context.Background(), RunRequest{
		Plan:        tr.snapshot(t, "run-2"),
		BackupSetID: tr.setID,
		Backup:      func(context.Context) error { return nil },
	})
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("the next run of the set returned %v, want ErrRecoveryRequired", err)
	}
}

// Even when the recovery_required write fails TOO, nothing closes over
// the scope.
//
// This is the doubly-unlucky case: the scope's cleanup cannot be started
// and the engine cannot record that the scope needs a person either, so
// the journal still says "eligible" -- entered, nothing recorded. What
// has to hold is that the run does not become terminal, because a
// terminal run is one the startup pass never looks at again and that
// obligation would never become a hold. The run is left non-terminal and
// the next restart raises it, which is the same answer a crash in this
// position gets. The journal refuses the alternative from its own side
// as well (state's TestNoRunReachesATerminalStateWithAScopeUnaccountedFor).
func TestBothObligationWritesFailingStillLeavesTheScopeVisible(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})

	store := refusing(h.store).
		obligation(workflow.ScopeGlobal, workflow.ObligationRunning).
		obligation(workflow.ScopeGlobal, workflow.ObligationRecoveryRequired)

	backupRan := false
	if _, err := h.runWith(t, store, tr.snapshot(t, "run-1"), &backupRan); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if store.failures() != 2 {
		t.Fatalf("%d writes were refused, want both of them", store.failures())
	}

	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if workflow.State(run.State).Terminal() {
		t.Errorf("the run is terminal (%q) with its global obligation at %q",
			run.State, obligationOf(t, h.store, "run-1", workflow.ScopeGlobal).State)
	}
	if got := obligationOf(t, h.store, "run-1", workflow.ScopeGlobal).State; got.Settled() {
		t.Errorf("the global obligation is %q, which reports the scope as accounted for", got)
	}

	// Which is exactly the state a restart turns into a hold.
	restarted := restart(t, h)
	if len(restarted.RecoveryHolds()) == 0 {
		t.Error("the restart found nothing outstanding for a scope nobody accounted for")
	}
}

// A run whose own "I have started" write fails still unwinds the scope
// the plan already committed as entered.
//
// The plan's transaction is what makes the global scope eligible, so by
// the time this write is attempted there is a promise on the record. The
// first version returned the error here, leaving a run row at "pending"
// with that promise outstanding -- an open run with an owed cleanup that
// nothing looks at until the next restart, manufactured by the code whose
// whole job is to avoid that shape.
func TestARunThatCannotRecordItsOwnStartStillUnwinds(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	store := refusing(h.store).runAdvance(workflow.StateRunning)

	backupRan := false
	res, err := h.runWith(t, store, tr.snapshot(t, "run-1"), &backupRan)
	if err == nil {
		t.Fatal("the engine reported success for a run it could not record as started")
	}
	if !errors.Is(err, errWriteFailed) {
		t.Fatalf("Run returned %v, want the write failure", err)
	}

	// No "before" hook ran and the backup did not happen: the run never
	// started, and starting hooks for a run the journal does not say is
	// running is the side effect with no record.
	if backupRan {
		t.Error("the backup ran for a run that could not be recorded as started")
	}
	if got := stateOf(t, h.store, "run-1", "10-mount.local.sh").State; got != string(workflow.StateSkipped) {
		t.Errorf("the before hook is %q, want skipped", got)
	}

	// The global scope WAS entered by the plan's own transaction, so its
	// cleanup ran and the obligation is settled.
	if got := h.rec.dispatched(); !reflect.DeepEqual(got, []string{"local:90-unmount.local.sh"}) {
		t.Errorf("dispatched %v, want just the global cleanup", got)
	}
	if got := obligationOf(t, h.store, "run-1", workflow.ScopeGlobal).State; got != workflow.ObligationSuccess {
		t.Errorf("the global obligation is %q, want success", got)
	}

	// And the run is CLOSED rather than left open for a restart to find.
	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if !workflow.State(run.State).Terminal() {
		t.Errorf("the run is %q, which a later startup reads as an interruption", run.State)
	}
	if run.FinishedAt == nil {
		t.Error("the closed run has no finish time")
	}
	if res.State != workflow.StateFailed {
		t.Errorf("the result says %q, want failed", res.State)
	}

	restarted := restart(t, h)
	if holds := restarted.RecoveryHolds(); len(holds) != 0 {
		t.Errorf("a restart found %d holds for a run that unwound everything it entered: %+v", len(holds), holds)
	}
}

// L6's --skip-workflow-scripts, as history.
//
// A bypassed run takes the backup and runs no hooks, and the POINT is the
// row it leaves: bypassed=1, every step skipped, both scopes never
// eligible, cleanup skipped. The flag used to be written only by the
// planned path -- which runs hooks -- so the one caller that actually
// sets it could not produce a run row carrying it, and "was this backup
// taken with the workflow skipped" was unanswerable from history.
func TestABypassedRunTakesTheBackupAndRecordsThatItSkippedTheHooks(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, fullTree())

	backupRan := false
	res, err := h.engine.Run(context.Background(), RunRequest{
		Plan:        tr.snapshot(t, "run-1"),
		BackupSetID: tr.setID,
		Bypassed:    true,
		Backup: func(context.Context) error {
			backupRan = true

			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !backupRan {
		t.Fatal("a bypassed run skipped the backup; it skips the HOOKS")
	}
	if got := h.rec.dispatched(); len(got) != 0 {
		t.Fatalf("a bypassed run dispatched %v", got)
	}

	if !res.Bypassed {
		t.Error("the result does not report the run as bypassed")
	}
	if res.CleanupStatus != workflow.StatusSkipped {
		t.Errorf("cleanup status is %q, want skipped", res.CleanupStatus)
	}
	if res.BackupStatus != workflow.StatusSuccess {
		t.Errorf("backup status is %q, want success", res.BackupStatus)
	}
	if res.ScriptCount != 5 {
		t.Errorf("the run reports %d scripts; the plan declared 5, and what was skipped is what an operator needs to know about", res.ScriptCount)
	}

	// The journal is where L6's history surface reads this from.
	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if !run.Bypassed {
		t.Error("the run row does not record that the hooks were skipped")
	}
	if run.CleanupStatus != string(workflow.StatusSkipped) {
		t.Errorf("the row's cleanup status is %q, want skipped", run.CleanupStatus)
	}
	if !workflow.State(run.State).Terminal() {
		t.Errorf("the bypassed run is %q, which is not over", run.State)
	}

	steps, err := h.store.WorkflowSteps(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowSteps: %v", err)
	}
	if len(steps) != 5 {
		t.Fatalf("the plan committed %d steps, want 5", len(steps))
	}
	for _, s := range steps {
		if s.State != string(workflow.StateSkipped) {
			t.Errorf("step %s is %q, want skipped", s.ScriptName, s.State)
		}
	}

	// Neither scope was entered, so neither is owed a cleanup and
	// nothing about this run can block the set.
	for _, scope := range workflow.Scopes() {
		if got := obligationOf(t, h.store, "run-1", scope).State; got != workflow.ObligationNeverEligible {
			t.Errorf("the %s obligation of a bypassed run is %q, want never_eligible", scope, got)
		}
	}
	if len(h.engine.SuspendedBackupSets()) != 0 {
		t.Errorf("a bypassed run blocked its set: %v", h.engine.SuspendedBackupSets())
	}

	// And a restart concludes nothing about it, which is the property
	// that matters for a run whose hooks were deliberately skipped: it
	// is over, and there is no scope to account for.
	restarted := restart(t, h)
	if holds := restarted.RecoveryHolds(); len(holds) != 0 {
		t.Errorf("a restart raised %d holds for a bypassed run: %+v", len(holds), holds)
	}
}

// A bypassed run is still a run the journal can read back, which is what
// the reconciliation would need if this process died during its backup.
func TestABypassedRunInterruptedDuringItsBackupNeedsNoRecovery(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, fullTree())

	func() {
		defer func() {
			if recovered := recover(); recovered != nil && recovered != errCrash {
				panic(recovered)
			}
		}()

		_, _ = h.engine.Run(context.Background(), RunRequest{
			Plan:        tr.snapshot(t, "run-1"),
			BackupSetID: tr.setID,
			Bypassed:    true,
			Backup:      func(context.Context) error { panic(errCrash) },
		})
	}()

	restarted := restart(t, h)

	// Nothing was entered, so there is nothing to account for: the run
	// is finalized rather than blocked.
	if holds := restarted.RecoveryHolds(); len(holds) != 0 {
		t.Fatalf("a bypassed run interrupted mid-backup left %d holds: %+v", len(holds), holds)
	}

	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if !workflow.State(run.State).Terminal() {
		t.Errorf("the reconciled run is %q", run.State)
	}
	if run.BackupStatus != string(workflow.StatusUnknown) {
		t.Errorf("the backup status of an interrupted backup is %q, want unknown", run.BackupStatus)
	}
}

// An advance the engine makes on a cancelled run's behalf still lands:
// the run's bookkeeping context has the cancellation removed, and the
// plan commit and the start advance are part of that bookkeeping.
//
// The two used to be on the caller's context, so a run cancelled between
// the lock and the first hook left no row at all -- a spool on disk with
// nothing pointing at it -- or a run row stuck at pending.
func TestARunCancelledBeforeItStartsIsStillRecorded(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := h.engine.Run(ctx, RunRequest{
		Plan:        tr.snapshot(t, "run-1"),
		BackupSetID: tr.setID,
		Backup:      func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != workflow.StateCanceled {
		t.Errorf("the run ended %q, want canceled", res.State)
	}

	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("the cancelled run left no journal row: %v", err)
	}
	if !workflow.State(run.State).Terminal() {
		t.Errorf("the cancelled run is %q, which a restart reads as an interruption", run.State)
	}

	// Its cleanup still ran, under its own bound, which is the rule a
	// cancellation must not break.
	if got := h.rec.dispatched(); !reflect.DeepEqual(got, []string{"local:90-unmount.local.sh"}) {
		t.Errorf("dispatched %v, want the global cleanup", got)
	}
	if got := obligationOf(t, h.store, "run-1", workflow.ScopeGlobal).State; got != workflow.ObligationSuccess {
		t.Errorf("the global obligation is %q, want success", got)
	}
}

// A run whose CONTEXT expires -- a deadline rather than a cancellation --
// is timed out, not cancelled, and its cleanup still runs.
//
// The distinction is an operator's: somebody pressed Ctrl-C, or this
// product decided the window was over. Both stop new work; they are not
// the same sentence in a report.
func TestARunWhoseDeadlineExpiresIsTimedOutAndStillUnwinds(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, fullTree())

	ctx, cancel := context.WithDeadline(context.Background(), h.clock.Now().Add(-time.Hour))
	defer cancel()

	res, err := h.engine.Run(ctx, RunRequest{
		Plan:        tr.snapshot(t, "run-1"),
		BackupSetID: tr.setID,
		Backup:      func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.State != workflow.StateTimedOut {
		t.Errorf("the run ended %q, want timed_out", res.State)
	}
	if res.BackupStatus != workflow.StatusSkipped {
		t.Errorf("backup status is %q, want skipped", res.BackupStatus)
	}

	// Every "before" step is skipped rather than left pending, and the
	// global scope's cleanup ran: a cleanup does not inherit the
	// deadline the run has just lost.
	if got := stateOf(t, h.store, "run-1", "10-mount.local.sh").State; got != string(workflow.StateSkipped) {
		t.Errorf("the first before step is %q, want skipped", got)
	}
	if got := h.rec.dispatched(); !reflect.DeepEqual(got, []string{"local:90-unmount.local.sh"}) {
		t.Errorf("dispatched %v, want the global cleanup only", got)
	}
	for _, scope := range workflow.Scopes() {
		o := obligationOf(t, h.store, "run-1", scope)
		if !o.State.Settled() {
			t.Errorf("the %s obligation is %q after a timed-out run", scope, o.State)
		}
	}
}

// An earlier step's TIMEOUT does not relabel a later step's lost session.
//
// One flag used to carry both "the run is being torn down" and "some
// step timed out", so a hook that ran out of its own bound made every
// later step that came back with transport loss get recorded as
// timed_out -- this product claiming it killed something it never
// touched, on a run whose context had not expired at all. An audit that
// says a step was timed out is a claim about what this product did.
func TestAnEarlierTimeoutDoesNotRelabelALaterLostSession(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	h.local.outcomes["10-mount.local.sh"] = func(context.Context, StepRequest) (StepOutcome, error) {
		return StepOutcome{Disposition: DispositionTimedOut}, nil
	}
	h.local.outcomes["90-unmount.local.sh"] = func(context.Context, StepRequest) (StepOutcome, error) {
		return StepOutcome{
			Disposition: DispositionTransportLost,
			Detail:      "the runner's socket went away",
		}, nil
	}

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stateOf(t, h.store, "run-1", "10-mount.local.sh").State; got != string(workflow.StateTimedOut) {
		t.Errorf("the step that ran out of its bound is %q, want timed_out", got)
	}

	lost := stateOf(t, h.store, "run-1", "90-unmount.local.sh")
	if lost.State != string(workflow.StateFailed) {
		t.Errorf("the step whose session was lost is %q, want failed: nothing timed IT out", lost.State)
	}
	if lost.ExitCode != nil {
		t.Errorf("the lost step carries exit code %d", *lost.ExitCode)
	}
}

// The failure matrix says a stage stops at its first failure, and the
// steps behind it are SKIPPED rather than left pending -- which is what
// makes a plan read back after the fact account for every step it
// declared. A pending step in a terminal run is work nothing will ever
// do and nothing will ever report.
func TestAFailedStageSkipsTheStepsBehindIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, fullTree())

	h.local.outcomes["10-mount.local.sh"] = failing(1)

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stateOf(t, h.store, "run-1", "10-mount.local.sh").State; got != string(workflow.StateFailed) {
		t.Errorf("the failing step is %q, want failed", got)
	}

	for _, name := range []string{"20-second.local.sh", "10-quiesce.remote.sh", "10-resume.remote.sh"} {
		got := stateOf(t, h.store, "run-1", name)
		if got.State != string(workflow.StateSkipped) {
			t.Errorf("%s is %q, want skipped", name, got.State)
		}
		if got.StartedAt != nil {
			t.Errorf("%s was skipped and records a start time", name)
		}
	}
}

// Every step of every stage is accounted for, whatever happened: no step
// of a terminal run is left pending. Asserted over the whole failure
// matrix rather than for one row, because "accounts for every step it
// declared" is a property of the plan's record and not of one path
// through it.
func TestNoStepOfATerminalRunIsLeftPending(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		fail string
	}{
		{"nothing fails", ""},
		{"global before fails", "10-mount.local.sh"},
		{"set before fails", "10-quiesce.remote.sh"},
		{"set after fails", "10-resume.remote.sh"},
		{"global after fails", "90-unmount.local.sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			tr := newTree(t, fullTree())

			if tc.fail != "" {
				h.local.outcomes[tc.fail] = failing(1)
				h.remote.outcomes[tc.fail] = failing(1)
			}

			res, err := h.run(t, tr.snapshot(t, "run-1"))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !res.State.Terminal() {
				t.Fatalf("the run ended %q, which is not terminal", res.State)
			}

			steps, err := h.store.WorkflowSteps(context.Background(), "run-1")
			if err != nil {
				t.Fatalf("WorkflowSteps: %v", err)
			}
			for _, s := range steps {
				switch workflow.State(s.State) {
				case workflow.StatePending, workflow.StateRunning:
					t.Errorf("step %s is %q in a terminal run", s.ScriptName, s.State)
				}
			}

			for _, scope := range workflow.Scopes() {
				if o := obligationOf(t, h.store, "run-1", scope); !o.State.Settled() {
					t.Errorf("the %s obligation is %q in a terminal run", scope, o.State)
				}
			}
		})
	}
}
