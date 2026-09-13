package workflowrun

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/state"
	"github.com/backupdproject/backupd/core/internal/workflow"
)

// Resuming an interrupted run's cleanup, and the things that only go
// wrong once the hooks actually produce output or the resume actually
// takes a while.
//
// The fakes in this package are silent and instant by default, which is
// why several of these failures were invisible: a hook that prints
// nothing never reaches the log recorder, and a resume that returns
// immediately never outlives its own bound.

// interruptDuringBackup stops the process while the backup itself is
// running, which is the one position where backup_status is "running" on
// disk.
func interruptDuringBackup(t *testing.T, h *harness, plan workflow.Plan) {
	t.Helper()

	reached := false

	func() {
		defer func() {
			if recovered := recover(); recovered != nil && recovered != errCrash {
				panic(recovered)
			}
		}()

		_, _ = h.engine.Run(context.Background(), RunRequest{
			Plan:        plan,
			BackupSetID: plan.BackupSetID(),
			Backup: func(context.Context) error {
				reached = true

				panic(errCrash)
			},
		})
	}()

	if !reached {
		t.Fatal("the run never got as far as its backup")
	}
}

// crashOn panics from a chosen recovery write, so that the process stops
// in the middle of a sequence the ordinary suites only ever see complete.
//
// Reconcile, ResumeCleanup and AcknowledgeRecovery each make several
// durable writes, and the crash-injection suite counts only the ones a
// RUN makes -- so every position in these three was untested.
type crashOn struct {
	Store

	resolve        bool
	reconciliation bool

	crashed bool
}

func (c *crashOn) ResolveWorkflowRecovery(ctx context.Context, runID string, to workflow.State, at time.Time) error {
	if c.resolve {
		c.crashed = true

		panic(errCrash)
	}

	return c.Store.ResolveWorkflowRecovery(ctx, runID, to, at)
}

func (c *crashOn) ApplyWorkflowReconciliation(ctx context.Context, rec state.WorkflowReconciliation) error {
	if c.reconciliation {
		c.crashed = true

		panic(errCrash)
	}

	return c.Store.ApplyWorkflowReconciliation(ctx, rec)
}

// crashOnFinalAdvance stops the process on the run's SUMMARY write: the
// one advance that carries a finish time. It is the position that
// produces the second kind of reconciliation -- a run whose work is
// demonstrably over and whose row never landed.
type crashOnFinalAdvance struct {
	Store

	crashed bool
}

func (c *crashOnFinalAdvance) AdvanceWorkflowRun(ctx context.Context, runID string, adv state.WorkflowRunAdvance) error {
	if adv.FinishedAt != nil {
		c.crashed = true

		panic(errCrash)
	}

	return c.Store.AdvanceWorkflowRun(ctx, runID, adv)
}

func crashing(t *testing.T, h *harness, store Store, run func()) {
	t.Helper()

	h.engine.Store = store

	defer func() {
		recovered := recover()
		if recovered != nil && recovered != errCrash {
			panic(recovered)
		}

		h.engine.Store = h.store
	}()

	run()
}

// A resumed cleanup writes into the log of the run it is unwinding, and
// that log already has rows in it.
//
// The recorder used to start counting at zero, so the first byte a
// recovery hook produced collided with the interrupted run's own output
// on UNIQUE (run_id, seq). The engine reads a failed log write as a step
// whose output could not be recorded, which fails the step, which fails
// the cleanup -- so a recovery that did everything right reported
// cleanup_failed because of a counter, and the operator was told the
// machine could not be put back.
//
// It was invisible because the fake hooks print nothing.
func TestAResumedCleanupsOutputContinuesTheRunsLogSequence(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setBefore:    {"20-quiesce.remote.sh"},
		setAfter:     {"30-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	// The interrupted run produces output, which is what puts rows in
	// the log for the resume to collide with.
	h.local.outcomes["10-mount.local.sh"] = emitting("mounted /srv/data\n")

	interrupt(t, h, tr.snapshot(t, "run-1"), "20-quiesce.remote.sh")

	before := len(logsOf(t, h, "run-1"))
	if before == 0 {
		t.Fatal("the interrupted run recorded no output, so a resume has nothing to collide with and this test proves nothing")
	}

	engine := restart(t, h)

	// And so do the recovery hooks.
	h.remote.outcomes["30-resume.remote.sh"] = emitting("database resumed\n")
	h.local.outcomes["90-unmount.local.sh"] = emitting("unmounted /srv/data\n")

	res, err := engine.ResumeCleanup(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ResumeCleanup: %v", err)
	}

	if res.State != workflow.StateRecovered {
		t.Errorf("the resumed run is %q, want recovered", res.State)
	}
	if res.CleanupStatus != workflow.StatusSuccess {
		t.Errorf("cleanup status is %q, want success -- the hooks all succeeded", res.CleanupStatus)
	}

	// One sequence, contiguous, across the interruption: a follower's
	// cursor spans a run and its recovery, because it is the run's log.
	recs := logsOf(t, h, "run-1")
	if len(recs) <= before {
		t.Fatalf("the resume recorded nothing: %d records before, %d after", before, len(recs))
	}
	for i, rec := range recs {
		if rec.Seq != uint64(i+1) {
			t.Fatalf("record %d has sequence %d; the resumed recorder did not carry on from %d", i, rec.Seq, before)
		}
	}

	var whole strings.Builder
	for _, rec := range recs {
		whole.Write(rec.Payload)
	}
	for _, want := range []string{"mounted /srv/data", "database resumed", "unmounted /srv/data"} {
		if !strings.Contains(whole.String(), want) {
			t.Errorf("the run's log does not contain %q:\n%s", want, whole.String())
		}
	}
}

// An incomplete resume is recorded as such, DURABLY, and can be resumed
// again.
//
// ResumeCleanup moves the run to cleanup_running with recovery
// in_progress on the way in. A resume that came back with a scope still
// unaccountable used to leave it exactly there -- and ResumeCleanup
// refuses any run whose recovery state is not "required", so the only way
// to try again was to restart the daemon. That is a backup set blocked
// until somebody reboots something, for a scope an operator was in the
// middle of dealing with.
func TestAnIncompleteResumeIsRecordedAndCanBeResumedAgain(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setAfter:     {"30-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	// Interrupted while the set scope's own cleanup hook was running:
	// its outcome is unknown, so nothing may re-run it.
	interrupt(t, h, tr.snapshot(t, "run-1"), "30-resume.remote.sh")

	engine := restart(t, h)

	res, err := engine.ResumeCleanup(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ResumeCleanup: %v", err)
	}
	if !res.RecoveryOutstanding {
		t.Fatal("a scope with an interrupted cleanup step was reported as accounted for")
	}

	// The JOURNAL, not just the result: the run is back at
	// recovery_required with its axis at "required", which is the only
	// state another resume or an acknowledgement can act on.
	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.State != string(workflow.StateRecoveryRequired) {
		t.Errorf("the run row is %q, want recovery_required", run.State)
	}
	if run.RecoveryState != string(workflow.RecoveryRequired) {
		t.Errorf("the run's recovery state is %q, want required", run.RecoveryState)
	}
	if run.CleanupStatus != string(workflow.StatusFailed) {
		t.Errorf("the run's cleanup status is %q; the cleanup did not succeed", run.CleanupStatus)
	}

	// A second resume is accepted rather than refused. It reaches the
	// same conclusion -- an interrupted hook is still unaccountable --
	// which is the point: the run is where an operator can act on it.
	again, err := engine.ResumeCleanup(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("the second ResumeCleanup was refused: %v", err)
	}
	if !again.RecoveryOutstanding {
		t.Error("the second resume claimed the interrupted scope was accounted for")
	}

	// And the acknowledgement still works from there, which is the exit
	// that exists for exactly this scope.
	if err := engine.AcknowledgeRecovery(context.Background(), "run-1", Acknowledgement{
		By:     "operator@example",
		Reason: "checked the database by hand; it is serving",
	}); err != nil {
		t.Fatalf("AcknowledgeRecovery: %v", err)
	}
	if len(engine.SuspendedBackupSets()) != 0 {
		t.Errorf("the set is still suspended: %v", engine.SuspendedBackupSets())
	}
}

// A resumed cleanup whose hook RUNS and fails settles the scope and
// unblocks the set: the cleanup was attempted and what happened is on
// the record, which is a finished story with a bad ending rather than an
// unfinished one.
func TestAResumedCleanupThatFailsSettlesAndUnblocksTheSet(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setBefore:    {"20-quiesce.remote.sh"},
		setAfter:     {"30-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	interrupt(t, h, tr.snapshot(t, "run-1"), "20-quiesce.remote.sh")

	engine := restart(t, h)

	h.remote.outcomes["30-resume.remote.sh"] = failing(1)

	res, err := engine.ResumeCleanup(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ResumeCleanup: %v", err)
	}

	if res.RecoveryOutstanding {
		t.Error("a cleanup that ran and failed was reported as still outstanding")
	}
	if res.State != workflow.StateCleanupFailed {
		t.Errorf("the resumed run is %q, want cleanup_failed", res.State)
	}
	if res.CleanupStatus != workflow.StatusFailed {
		t.Errorf("cleanup status is %q, want failed", res.CleanupStatus)
	}

	// The later cleanup still ran: an unmount that comes after a failed
	// database resume is still worth attempting.
	if got := stateOf(t, h.store, "run-1", "90-unmount.local.sh").State; got != string(workflow.StateSuccess) {
		t.Errorf("the global cleanup is %q; later cleanup must continue", got)
	}

	o := obligationOf(t, h.store, "run-1", workflow.ScopeSet)
	if o.State != workflow.ObligationFailed {
		t.Errorf("the set obligation is %q, want failed", o.State)
	}
	if !o.State.Settled() {
		t.Error("a cleanup that ran and failed left the scope unsettled")
	}

	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.RecoveryState != string(workflow.RecoveryResolved) {
		t.Errorf("the run's recovery state is %q, want resolved", run.RecoveryState)
	}
	if run.WorkflowStatus != string(workflow.StatusFailed) {
		t.Errorf("the run's workflow status is %q, want failed", run.WorkflowStatus)
	}

	// The set runs again: the story is over, badly, and visibly.
	if len(engine.SuspendedBackupSets()) != 0 {
		t.Fatalf("the set is still suspended: %v", engine.SuspendedBackupSets())
	}
	if _, err := engine.Run(context.Background(), RunRequest{
		Plan:        tr.snapshot(t, "run-2"),
		BackupSetID: tr.setID,
		Backup:      func(context.Context) error { return nil },
	}); err != nil {
		t.Fatalf("a run after a failed-but-settled resume was refused: %v", err)
	}
}

// A resume that outlives the cleanup bound still records what it did.
//
// The obligation writes used to go on the bounded cleanup context, so a
// resume that took longer than CleanupTimeout -- a database that needed
// three minutes to come back -- failed BOTH of them: the scope stayed at
// recovery_required on disk while this process believed it had run the
// cleanup, and the set stayed blocked with nothing in the journal
// explaining why. Execution is bounded; bookkeeping is not.
func TestAResumeThatOutlivesItsBoundStillRecordsTheScopesOutcome(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setBefore:    {"20-quiesce.remote.sh"},
		setAfter:     {"30-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	interrupt(t, h, tr.snapshot(t, "run-1"), "20-quiesce.remote.sh")

	engine := restart(t, h)

	// A bound that has already expired by the time the first cleanup
	// step is dispatched, which is what a resume slower than the bound
	// looks like from every write's point of view.
	engine.CleanupTimeout = time.Nanosecond

	timedOut := func(ctx context.Context, _ StepRequest) (StepOutcome, error) {
		if ctx.Err() == nil {
			t.Error("the cleanup step was handed a live context; this test needs the bound to have expired")
		}

		return StepOutcome{Disposition: DispositionTimedOut}, nil
	}
	h.remote.outcomes["30-resume.remote.sh"] = timedOut
	h.local.outcomes["90-unmount.local.sh"] = timedOut

	res, err := engine.ResumeCleanup(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ResumeCleanup: %v", err)
	}

	// Both scopes were attempted and both outcomes are on the record --
	// settled, so the run is over and the set is released. What must NOT
	// happen is the scope being left where it was with this process
	// thinking otherwise.
	for _, scope := range workflow.Scopes() {
		o := obligationOf(t, h.store, "run-1", scope)
		if o.State != workflow.ObligationFailed {
			t.Errorf("the %s obligation is %q, want failed: the cleanup was attempted and timed out", scope, o.State)
		}
		if o.StartedAt == nil {
			t.Errorf("the %s obligation records no start, so nothing says the cleanup was attempted", scope)
		}
	}

	if res.RecoveryOutstanding {
		t.Error("the resume reported an outstanding recovery for scopes it settled")
	}
	if res.State != workflow.StateCleanupFailed {
		t.Errorf("the resumed run is %q, want cleanup_failed", res.State)
	}

	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.RecoveryState != string(workflow.RecoveryResolved) {
		t.Errorf("the run's recovery state is %q, want resolved", run.RecoveryState)
	}
	if len(engine.SuspendedBackupSets()) != 0 {
		t.Errorf("the set is still suspended after a resume that settled every scope: %v", engine.SuspendedBackupSets())
	}
}

// The deployment's facts reach a hook on BOTH paths, and neither path
// lets a caller state a fact this product owns.
//
// The facts are injected per call and persisted nowhere, so a resumed
// hook used to be handed an empty BACKUPD_SOURCE_PATH -- and
// `umount "$BACKUPD_SOURCE_PATH"` with an empty variable is a hook that
// unmounts nothing and exits 0, which is a recovery reporting a machine
// as put back that it never touched.
func TestTheRunsFactsReachItsHooksOnBothTheRunAndTheResumePath(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setBefore:    {"20-quiesce.remote.sh"},
		setAfter:     {"30-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	facts := map[string]string{
		"BACKUPD_SOURCE_HOST": "db.internal",
		"BACKUPD_SOURCE_PATH": "/srv/data",
		"BACKUPD_DESTINATION": "nas:/backups/production",

		// A caller trying to state the run's own identity. The
		// built-ins are this product's facts, not a caller's, and they
		// win unconditionally.
		"BACKUPD_RUN_ID": "forged",
	}

	crash := &crashAfterStarting{Store: h.store, step: "20-quiesce.remote.sh"}
	h.engine.Store = crash

	func() {
		defer func() {
			if recovered := recover(); recovered != nil && recovered != errCrash {
				panic(recovered)
			}

			h.engine.Store = h.store
		}()

		_, _ = h.engine.Run(context.Background(), RunRequest{
			Plan:        tr.snapshot(t, "run-1"),
			BackupSetID: tr.setID,
			Facts:       facts,
			Backup:      func(context.Context) error { return nil },
		})
	}()

	if !crash.crashed {
		t.Fatal("the fixture run never reached the step it was supposed to be interrupted at")
	}

	// The ordinary path.
	env := h.rec.envOf(t, "10-mount.local.sh")
	for name, want := range map[string]string{
		"BACKUPD_SOURCE_HOST": "db.internal",
		"BACKUPD_SOURCE_PATH": "/srv/data",
		"BACKUPD_DESTINATION": "nas:/backups/production",
		"BACKUPD_RUN_ID":      "run-1",
	} {
		if env[name] != want {
			t.Errorf("a hook of the run was told %s=%q, want %q", name, env[name], want)
		}
	}

	// And the resume path, out of the journal: the daemon has restarted,
	// so nothing about these values is still in memory.
	engine := restart(t, h)

	if _, err := engine.ResumeCleanup(context.Background(), "run-1"); err != nil {
		t.Fatalf("ResumeCleanup: %v", err)
	}

	resumed := h.rec.envOf(t, "30-resume.remote.sh")
	for name, want := range map[string]string{
		"BACKUPD_SOURCE_HOST": "db.internal",
		"BACKUPD_SOURCE_PATH": "/srv/data",
		"BACKUPD_DESTINATION": "nas:/backups/production",
		"BACKUPD_RUN_ID":      "run-1",
		"BACKUPD_RECOVERY":    "1",
	} {
		if resumed[name] != want {
			t.Errorf("a resumed hook was told %s=%q, want %q", name, resumed[name], want)
		}
	}
}

// A crash DURING the backup leaves backup_status saying "running" on
// disk, and nobody is ever going to observe that backup's outcome.
//
// Left alone, the row says work is in flight in a process that no longer
// exists -- forever, in every history surface -- and a resumed "after"
// hook is told BACKUPD_BACKUP_STATUS=running, which is the input a
// careful hook uses to decide whether to roll something back.
func TestACrashDuringTheBackupDoesNotLeaveItRunningForever(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setBefore:    {"20-quiesce.remote.sh"},
		setAfter:     {"30-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	interruptDuringBackup(t, h, tr.snapshot(t, "run-1"))

	// Before the restart, the row really does say running: without this
	// the assertion below could pass against a run that never got that
	// far.
	crashed, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if crashed.BackupStatus != string(workflow.StatusRunning) {
		t.Fatalf("the interrupted run's backup status is %q; this test needs it mid-backup", crashed.BackupStatus)
	}

	engine := restart(t, h)

	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.BackupStatus != string(workflow.StatusUnknown) {
		t.Errorf("the reconciled backup status is %q, want unknown: nobody observed that backup's outcome", run.BackupStatus)
	}

	// And the hook that unwinds it is told the same thing.
	if _, err := engine.ResumeCleanup(context.Background(), "run-1"); err != nil {
		t.Fatalf("ResumeCleanup: %v", err)
	}

	env := h.rec.envOf(t, "30-resume.remote.sh")
	if env["BACKUPD_BACKUP_STATUS"] != string(workflow.StatusUnknown) {
		t.Errorf("the recovery hook was told BACKUPD_BACKUP_STATUS=%q, want unknown", env["BACKUPD_BACKUP_STATUS"])
	}
}

// A resumed run's step results come from the JOURNAL.
//
// They used to be seeded as "pending" across the board, so the result an
// operator surface renders for a recovery said every before-step was
// still waiting to run -- for a run that is over, whose steps are
// recorded as successful, skipped and interrupted. The result and the
// rows cannot disagree about that: they are read side by side.
func TestAResumedRunsStepsAreSeededFromTheJournal(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setBefore:    {"20-quiesce.remote.sh"},
		setAfter:     {"30-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	interrupt(t, h, tr.snapshot(t, "run-1"), "20-quiesce.remote.sh")

	engine := restart(t, h)

	res, err := engine.ResumeCleanup(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ResumeCleanup: %v", err)
	}

	got := map[string]workflow.State{}
	for _, s := range res.Steps {
		got[s.ScriptName] = s.State
	}

	want := map[string]workflow.State{
		"10-mount.local.sh":    workflow.StateSuccess,
		"20-quiesce.remote.sh": workflow.StateInterrupted,
		"30-resume.remote.sh":  workflow.StateSuccess,
		"90-unmount.local.sh":  workflow.StateSuccess,
	}
	for name, state := range want {
		if got[name] != state {
			t.Errorf("the resumed result reports %s as %q, want %q", name, got[name], state)
		}
	}

	// And it agrees with the rows, which is the actual claim.
	for name := range want {
		if row := stateOf(t, h.store, "run-1", name); row.State != string(got[name]) {
			t.Errorf("%s is %q in the result and %q in the journal", name, got[name], row.State)
		}
	}
}

// A spool somebody has edited is not executed.
//
// The hash is taken at snapshot time and verified on every open, which is
// what makes a recovered plan safe to run after a restart: the spool has
// been sitting on disk in between. This is that guarantee observed from
// the resume path, which is the one that matters -- a fresh run's bytes
// were just written.
func TestATamperedSpoolIsNotExecutedByAResume(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setBefore:    {"20-quiesce.remote.sh"},
		setAfter:     {"30-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	plan := tr.snapshot(t, "run-1")
	interrupt(t, h, plan, "20-quiesce.remote.sh")

	// The spooled copy of the cleanup script is rewritten in place: not
	// /workflows, which the resume never reads, but the run's own
	// private authority.
	step := stateOf(t, h.store, "run-1", "30-resume.remote.sh")
	if err := os.WriteFile(step.SpoolRef, []byte("#!/usr/bin/env bash\nrm -rf /\n"), 0o600); err != nil {
		t.Fatalf("tampering with the spooled script: %v", err)
	}

	engine := restart(t, h)

	res, err := engine.ResumeCleanup(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ResumeCleanup: %v", err)
	}

	// The tampered step was never dispatched, and the one that was is
	// the untouched global cleanup.
	for _, call := range h.rec.dispatched() {
		if strings.Contains(call, "30-resume.remote.sh") {
			t.Fatal("the resume executed a spooled script whose bytes no longer match the plan")
		}
	}

	failed := stateOf(t, h.store, "run-1", "30-resume.remote.sh")
	if failed.State != string(workflow.StateFailed) {
		t.Errorf("the tampered step is %q, want failed", failed.State)
	}
	if failed.ExitCode != nil {
		t.Errorf("the tampered step carries exit code %d; nothing ran", *failed.ExitCode)
	}

	if res.State != workflow.StateCleanupFailed {
		t.Errorf("the resume ended %q, want cleanup_failed", res.State)
	}
	if got := obligationOf(t, h.store, "run-1", workflow.ScopeSet).State; got != workflow.ObligationFailed {
		t.Errorf("the set obligation is %q, want failed", got)
	}
}

// An acknowledgement closes the run it acknowledges.
//
// It used to leave a terminal row reading workflow_status=running and
// cleanup_status=unknown, which every history surface renders as a run
// still in flight -- for a run whose entire story is that a person
// finished it by hand.
func TestAnAcknowledgementClosesTheRunsStatuses(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setAfter:     {"30-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	interrupt(t, h, tr.snapshot(t, "run-1"), "30-resume.remote.sh")
	engine := restart(t, h)

	if err := engine.AcknowledgeRecovery(context.Background(), "run-1", Acknowledgement{
		By:     "operator@example",
		Reason: "unmounted the share by hand",
	}); err != nil {
		t.Fatalf("AcknowledgeRecovery: %v", err)
	}

	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if !workflow.State(run.State).Terminal() {
		t.Errorf("the acknowledged run is %q", run.State)
	}
	if run.WorkflowStatus != string(workflow.StatusFailed) {
		t.Errorf("the acknowledged run's workflow status is %q, want failed", run.WorkflowStatus)
	}
	if run.CleanupStatus != string(workflow.StatusFailed) {
		t.Errorf("the acknowledged run's cleanup status is %q; nothing this product ran accounted for the scope", run.CleanupStatus)
	}
	if run.FinishedAt == nil {
		t.Error("the acknowledged run has no finish time")
	}

	// A second acknowledgement is refused: there is nothing outstanding,
	// and an audit record for a scope nobody was asked about is worse
	// than an error.
	err = engine.AcknowledgeRecovery(context.Background(), "run-1", Acknowledgement{
		By:     "operator@example",
		Reason: "again",
	})
	if err == nil {
		t.Fatal("a settled run was acknowledged a second time")
	}
	if !strings.Contains(err.Error(), "nothing outstanding") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// An acknowledgement that dies between its obligation writes and its
// resolution is finished by the next startup.
//
// The two writes cannot be one -- the second is the one that reads the
// first -- so this position exists. What it leaves is a run at
// recovery_required with every scope acknowledged: nothing owed, nobody
// needed, and nothing that would ever look at it again, because
// recovery_required is the state the reconciliation writes and therefore
// the one it skips. The backup set would stay blocked on a hold that no
// longer exists.
func TestAnAcknowledgementInterruptedBeforeItsResolutionIsSweptUp(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setAfter:     {"30-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	interrupt(t, h, tr.snapshot(t, "run-1"), "30-resume.remote.sh")
	engine := restart(t, h)

	crash := &crashOn{Store: h.store, resolve: true}
	engine.Store = crash

	func() {
		defer func() {
			if recovered := recover(); recovered != nil && recovered != errCrash {
				panic(recovered)
			}

			engine.Store = h.store
		}()

		_ = engine.AcknowledgeRecovery(context.Background(), "run-1", Acknowledgement{
			By:     "operator@example",
			Reason: "checked by hand",
		})
	}()

	if !crash.crashed {
		t.Fatal("the acknowledgement never reached its resolution")
	}

	// The half-written state: scopes acknowledged, run still blocked.
	for _, scope := range workflow.Scopes() {
		if got := obligationOf(t, h.store, "run-1", scope).State; !got.Settled() {
			t.Fatalf("the %s obligation is %q; this test needs the acknowledgement's own writes to have landed", scope, got)
		}
	}
	stranded, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if stranded.RecoveryState != string(workflow.RecoveryRequired) {
		t.Fatalf("the run's recovery state is %q; this test needs it unresolved", stranded.RecoveryState)
	}

	// The restart finishes the job, in the direction the rows justify.
	restarted := restart(t, h)

	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.RecoveryState != string(workflow.RecoveryResolved) {
		t.Errorf("the run's recovery state is %q, want resolved", run.RecoveryState)
	}
	if !workflow.State(run.State).Terminal() {
		t.Errorf("the swept run is %q", run.State)
	}
	if run.WorkflowStatus != string(workflow.StatusFailed) {
		t.Errorf("the swept run's workflow status is %q, want failed", run.WorkflowStatus)
	}

	if len(restarted.SuspendedBackupSets()) != 0 {
		t.Errorf("the set is still blocked on a hold that no longer exists: %v", restarted.SuspendedBackupSets())
	}
	if _, err := restarted.Run(context.Background(), RunRequest{
		Plan:        tr.snapshot(t, "run-2"),
		BackupSetID: tr.setID,
		Backup:      func(context.Context) error { return nil },
	}); err != nil {
		t.Fatalf("a run of the released set was refused: %v", err)
	}
}

// A crash inside a RESUME leaves the run recoverable, and the second
// resume tells the truth about what it can no longer account for.
func TestACrashDuringAResumeLeavesTheRunRecoverable(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setBefore:    {"20-quiesce.remote.sh"},
		setAfter:     {"30-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	interrupt(t, h, tr.snapshot(t, "run-1"), "20-quiesce.remote.sh")

	engine := restart(t, h)

	// The process dies as the recovery hook is recorded as running:
	// the same position the original interruption was in, one level
	// down.
	crash := &crashAfterStarting{Store: h.store, step: "30-resume.remote.sh"}
	engine.Store = crash

	func() {
		defer func() {
			if recovered := recover(); recovered != nil && recovered != errCrash {
				panic(recovered)
			}

			engine.Store = h.store
		}()

		_, _ = engine.ResumeCleanup(context.Background(), "run-1")
	}()

	if !crash.crashed {
		t.Fatal("the resume never reached the hook it was supposed to be interrupted at")
	}

	restarted := restart(t, h)

	// The set is still blocked and the interrupted recovery hook is
	// recorded as interrupted rather than as anything that reads like an
	// outcome.
	if len(restarted.SuspendedBackupSets()) != 1 {
		t.Fatalf("the set is not blocked after a crashed resume: %v", restarted.SuspendedBackupSets())
	}
	if got := stateOf(t, h.store, "run-1", "30-resume.remote.sh").State; got != string(workflow.StateInterrupted) {
		t.Errorf("the interrupted recovery hook is %q, want interrupted", got)
	}

	// And a further resume will not re-run it: its outcome is unknown,
	// so the only exit left is a person.
	res, err := restarted.ResumeCleanup(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ResumeCleanup: %v", err)
	}
	if !res.RecoveryOutstanding {
		t.Error("a resume claimed a scope whose hook was interrupted twice was accounted for")
	}
	if err := restarted.AcknowledgeRecovery(context.Background(), "run-1", Acknowledgement{
		By:     "operator@example",
		Reason: "verified by hand",
	}); err != nil {
		t.Fatalf("AcknowledgeRecovery: %v", err)
	}
}

// A crash inside the reconciliation itself changes nothing: it is one
// transaction, so the next pass starts from the same rows and reaches
// the same answer.
func TestACrashInsideTheReconciliationIsRetriedFromTheSameRows(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, fullTree())

	interrupt(t, h, tr.snapshot(t, "run-1"), "10-quiesce.remote.sh")

	before := snapshotRun(t, h.store, "run-1")

	crash := &crashOn{Store: h.store, reconciliation: true}
	engine := &Engine{Store: crash, Local: h.local, Remote: h.remote, Now: h.clock.Now}

	func() {
		defer func() {
			if recovered := recover(); recovered != nil && recovered != errCrash {
				panic(recovered)
			}
		}()

		_, _ = engine.Reconcile(context.Background(), h.clock.Now())
	}()

	if !crash.crashed {
		t.Fatal("the reconciliation never reached its write")
	}

	if after := snapshotRun(t, h.store, "run-1"); after.Run.State != before.Run.State {
		t.Errorf("a crashed reconciliation moved the run from %q to %q", before.Run.State, after.Run.State)
	}

	// The retry gets there.
	restarted := restart(t, h)
	if len(restarted.RecoveryHolds()) == 0 {
		t.Fatal("the retried reconciliation raised no hold")
	}

	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.State != string(workflow.StateRecoveryRequired) {
		t.Errorf("the run is %q, want recovery_required", run.State)
	}
}

// Reconcile does not touch a run this process is EXECUTING.
//
// It is a startup pass by design, and nothing structural stops it being
// called again -- a health endpoint, a second component, a test. A pass
// that reconciled a live run would mark its in-flight step interrupted,
// move its obligations to recovery_required and block the set underneath
// a backup that is still running.
func TestReconcileLeavesALiveRunAlone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, fullTree())

	interrupt(t, h, tr.snapshot(t, "run-1"), "10-quiesce.remote.sh")

	before := snapshotRun(t, h.store, "run-1")

	// The set is locked, which is what a run in flight looks like from
	// the outside.
	release, err := h.engine.locks().Acquire(tr.setID, "run-live")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	report, err := h.engine.Reconcile(context.Background(), h.clock.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.Interrupted) != 0 {
		t.Errorf("a reconciliation reported %v as interrupted while its set was locked", report.Interrupted)
	}

	after := snapshotRun(t, h.store, "run-1")
	if after.Run.State != before.Run.State {
		t.Errorf("the live run moved from %q to %q", before.Run.State, after.Run.State)
	}
	for i, o := range after.Obligations {
		if o.State != before.Obligations[i].State {
			t.Errorf("the %s obligation of a live run moved from %q to %q", o.Scope, before.Obligations[i].State, o.State)
		}
	}

	// Once the lock is gone -- which is what a restart is, for a process
	// that holds none -- the same pass does its job.
	release()

	report, err = h.engine.Reconcile(context.Background(), h.clock.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.Interrupted) != 1 || report.Interrupted[0] != "run-1" {
		t.Errorf("the second pass reported %v as interrupted, want run-1", report.Interrupted)
	}
}

// The report's "interrupted" list is the runs that NEED something, not
// every run the pass looked at.
//
// A run whose work was demonstrably finished and whose summary row never
// landed is finalized by the same pass -- that is the other branch of the
// reconciliation, and it is deliberately not a recovery. Reporting it as
// interrupted puts a run nobody has to act on into the one list an
// operator reads to find out what broke.
func TestTheReconcileReportNamesOnlyTheRunsThatNeedRecovery(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	crash := &crashOnFinalAdvance{Store: h.store}

	crashing(t, h, crash, func() {
		_, _ = h.engine.Run(context.Background(), RunRequest{
			Plan:        tr.snapshot(t, "run-1"),
			BackupSetID: tr.setID,
			Backup:      func(context.Context) error { return nil },
		})
	})

	if !crash.crashed {
		t.Fatal("the run never reached its summary write")
	}

	restarted := &Engine{Store: h.store, Local: h.local, Remote: h.remote, Now: h.clock.Now}

	report, err := restarted.Reconcile(context.Background(), h.clock.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(report.Interrupted) != 0 {
		t.Errorf("a run whose every scope was settled was reported as interrupted: %v", report.Interrupted)
	}
	if len(report.Holds) != 0 {
		t.Errorf("a finalized run left %d holds: %+v", len(report.Holds), report.Holds)
	}

	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if !workflow.State(run.State).Terminal() {
		t.Errorf("the finalized run is %q", run.State)
	}
	if run.RecoveryState != string(workflow.RecoveryNone) {
		t.Errorf("a run nobody needs to act on has recovery state %q", run.RecoveryState)
	}
}

// The health surfaces are STABLE: two reads of the same outstanding state
// are the same list in the same order. They come out of a map keyed by
// backup set, and a health report that reordered itself between two
// scrapes is one nobody can diff -- which is how an operator finds out
// whether anything changed.
func TestTheHealthSurfacesAreOrderedTheSameWayTwice(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	// Three sets, each with an interrupted run, so there is something to
	// order.
	for _, name := range []string{"alpha", "beta", "gamma"} {
		tr := newTree(t, map[stage][]string{
			globalBefore: {"10-mount.local.sh"},
			setBefore:    {"20-quiesce.remote.sh"},
			setAfter:     {"30-resume.remote.sh"},
			globalAfter:  {"90-unmount.local.sh"},
		})

		set, err := setNamed(name)
		if err != nil {
			t.Fatalf("NewBackupSetID: %v", err)
		}
		tr.setID = set

		interrupt(t, h, tr.snapshot(t, "run-"+name), "20-quiesce.remote.sh")
	}

	engine := restart(t, h)

	holds := engine.RecoveryHolds()
	sets := engine.SuspendedBackupSets()
	if len(holds) < 3 || len(sets) != 3 {
		t.Fatalf("the fixture produced %d holds over %d sets", len(holds), len(sets))
	}

	for range 5 {
		if got := engine.RecoveryHolds(); !sameHolds(got, holds) {
			t.Fatalf("two reads of the holds disagree:\n\t%+v\n\t%+v", got, holds)
		}

		got := engine.SuspendedBackupSets()
		for i := range sets {
			if got[i] != sets[i] {
				t.Fatalf("two reads of the suspended sets disagree: %v vs %v", got, sets)
			}
		}
	}

	// And the order is the documented one rather than merely repeatable.
	for i := 1; i < len(sets); i++ {
		if sets[i-1].String() > sets[i].String() {
			t.Errorf("the suspended sets are not in name order: %v", sets)
		}
	}
	for i := 1; i < len(holds); i++ {
		if holds[i-1].RunID > holds[i].RunID {
			t.Errorf("the holds are not in run order: %+v", holds)
		}
	}
}

func sameHolds(a, b []RecoveryHold) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// A resume and an acknowledgement of one run cannot interleave.
//
// They walk the same obligations in opposite directions: the resume moves
// a scope to running and then to its outcome, the acknowledgement moves
// every unsettled scope to acknowledged. Interleaved, they produce an
// audit record saying a person checked a machine that this product was at
// that moment still unwinding. The set lock is what makes them serial,
// and it refuses rather than queues, so the operator is told.
func TestAnAcknowledgementIsRefusedWhileAResumeHoldsTheSet(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setAfter:     {"30-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	interrupt(t, h, tr.snapshot(t, "run-1"), "30-resume.remote.sh")
	engine := restart(t, h)

	// The lock a live resume of this run would be holding.
	release, err := engine.locks().Acquire(tr.setID, "run-1/resume")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	err = engine.AcknowledgeRecovery(context.Background(), "run-1", Acknowledgement{
		By:     "operator@example",
		Reason: "assuming it is fine",
	})
	if !errors.Is(err, ErrRunInFlight) {
		t.Fatalf("AcknowledgeRecovery returned %v, want ErrRunInFlight", err)
	}

	release()

	if err := engine.AcknowledgeRecovery(context.Background(), "run-1", Acknowledgement{
		By:     "operator@example",
		Reason: "checked by hand",
	}); err != nil {
		t.Fatalf("AcknowledgeRecovery after the resume finished: %v", err)
	}
}
