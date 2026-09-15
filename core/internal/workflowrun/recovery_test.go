package workflowrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/state"
	"github.com/retnd/retnd/core/internal/workflow"
)

// Recovery: what happens to a run this process did not see the end of,
// and the two ways out of it (#811's Recovery section).

// crashAfterStarting stops the process immediately after the named step
// has been durably recorded as running.
//
// A named step rather than a write count, because these suites are about
// a specific SITUATION -- a hook that was in flight when the machine went
// down -- and a count would move whenever the engine's write sequence
// did.
type crashAfterStarting struct {
	Store

	step    string
	crashed bool
}

func (c *crashAfterStarting) StartWorkflowStep(ctx context.Context, runID, stepID string, at time.Time) error {
	if err := c.Store.StartWorkflowStep(ctx, runID, stepID, at); err != nil {
		return err
	}

	if strings.Contains(stepID, c.step) {
		c.crashed = true

		panic(errCrash)
	}

	return nil
}

// interrupt runs a workflow and stops the process while the named script
// is running, the way a power cut would.
func interrupt(t *testing.T, h *harness, plan workflow.Plan, script string) {
	t.Helper()

	crash := &crashAfterStarting{Store: h.store, step: script}
	h.engine.Store = crash

	defer func() {
		recovered := recover()
		if recovered != nil && recovered != errCrash {
			panic(recovered)
		}

		h.engine.Store = h.store

		if !crash.crashed {
			t.Fatalf("the run finished without ever starting %s", script)
		}
	}()

	_, _ = h.engine.Run(context.Background(), RunRequest{
		Plan:        plan,
		BackupSetID: plan.BackupSetID(),
		Backup:      func(context.Context) error { return nil },
	})
}

// restart is a new engine on the same journal, which is what a daemon
// coming back up is.
func restart(t *testing.T, h *harness) *Engine {
	t.Helper()

	e := &Engine{Store: h.store, Local: h.local, Remote: h.remote, Now: h.clock.Now}
	if _, err := e.Reconcile(context.Background(), h.clock.Now()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	return e
}

func TestAnInterruptedRunBlocksTheSetAndResumesFromItsCapturedBytes(t *testing.T) {
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

	// The operator edits /workflows while the daemon is down: the
	// cleanup script is rewritten and the mount script is deleted. None
	// of it may change what the recovery executes.
	rewritten := filepath.Join(tr.dirs[setAfter], "30-resume.remote.sh")
	if err := os.WriteFile(rewritten, []byte("#!/usr/bin/env bash\nrm -rf /\n"), 0o600); err != nil {
		t.Fatalf("rewriting the cleanup script: %v", err)
	}
	if err := os.Remove(filepath.Join(tr.dirs[globalBefore], "10-mount.local.sh")); err != nil {
		t.Fatalf("deleting a script: %v", err)
	}

	engine := restart(t, h)

	// 1. The run requires recovery, and the interrupted step is recorded
	//    as interrupted rather than failed: nobody observed its status.
	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.State != string(workflow.StateRecoveryRequired) {
		t.Fatalf("the interrupted run is %q, want recovery_required", run.State)
	}
	if got := stateOf(t, h.store, "run-1", "20-quiesce.remote.sh"); got.State != string(workflow.StateInterrupted) {
		t.Errorf("the in-flight step is %q, want interrupted", got.State)
	}

	// 2. The set is suspended, an ordinary new run is refused, and the
	//    refusal says which run and which scope.
	if sets := engine.SuspendedBackupSets(); len(sets) != 1 || sets[0] != tr.setID {
		t.Errorf("the suspended sets are %v, want just %s", sets, tr.setID)
	}

	_, err = engine.Run(context.Background(), RunRequest{
		Plan:        tr.snapshot(t, "run-2"),
		BackupSetID: tr.setID,
		Backup:      func(context.Context) error { return nil },
	})
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("a new run of a blocked set returned %v, want ErrRecoveryRequired", err)
	}
	if !strings.Contains(err.Error(), "run-1") {
		t.Errorf("the refusal does not name the run that caused it: %v", err)
	}

	// 3. The spool is retained, because it is what the resume executes.
	if _, err := os.Stat(run.ScriptSpoolRef); err != nil {
		t.Fatalf("the interrupted run's spool is gone: %v", err)
	}

	// 4. resume-cleanup runs ONLY the eligible after stages, out of the
	//    captured bytes, with the recovery environment.
	h.rec.mu.Lock()
	h.rec.calls = nil
	h.rec.bodies = nil
	h.rec.mu.Unlock()

	res, err := engine.ResumeCleanup(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ResumeCleanup: %v", err)
	}

	want := []string{"remote:30-resume.remote.sh", "local:90-unmount.local.sh"}
	if got := h.rec.dispatched(); !reflect.DeepEqual(got, want) {
		t.Errorf("the resume dispatched\n\t%v\nwant\n\t%v", got, want)
	}

	h.rec.mu.Lock()
	bodies := append([]string(nil), h.rec.bodies...)
	h.rec.mu.Unlock()

	for _, body := range bodies {
		if strings.Contains(body, "rm -rf /") {
			t.Fatal("the resume executed the script as it is in /workflows NOW, not the bytes the run captured")
		}
	}
	if !strings.Contains(bodies[0], "echo 30-resume.remote.sh") {
		t.Errorf("the resumed cleanup ran %q, want the captured bytes", bodies[0])
	}

	env := h.rec.envOf(t, "30-resume.remote.sh")
	if env["BACKUPD_RECOVERY"] != "1" {
		t.Errorf("a resumed cleanup hook saw BACKUPD_RECOVERY=%q, want 1", env["BACKUPD_RECOVERY"])
	}
	if env["BACKUPD_CLEANUP_REASON"] != workflow.CleanupReasonInterruptedRun {
		t.Errorf("a resumed cleanup hook saw BACKUPD_CLEANUP_REASON=%q, want %q",
			env["BACKUPD_CLEANUP_REASON"], workflow.CleanupReasonInterruptedRun)
	}

	// The ordinary environment is the one the run was PLANNED with, read
	// back out of the journal rather than out of today's configuration.
	if env["BACKUPD_RUN_ID"] != "run-1" {
		t.Errorf("the resumed hook was told run id %q", env["BACKUPD_RUN_ID"])
	}

	// 5. The run is recovered, its recovery is settled, and the set runs
	//    again.
	if res.State != workflow.StateRecovered {
		t.Errorf("the resumed run is %q, want recovered", res.State)
	}
	if res.RecoveryOutstanding {
		t.Error("the resumed run still reports an outstanding recovery")
	}

	run, err = h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.State != string(workflow.StateRecovered) {
		t.Errorf("the journal records the resumed run as %q, want recovered", run.State)
	}
	if run.RecoveryState != string(workflow.RecoveryResolved) {
		t.Errorf("the resumed run's recovery state is %q, want resolved", run.RecoveryState)
	}
	for _, scope := range workflow.Scopes() {
		if got := obligationOf(t, h.store, "run-1", scope).State; got != workflow.ObligationSuccess {
			t.Errorf("after the resume the %s obligation is %q, want success", scope, got)
		}
	}

	if len(engine.SuspendedBackupSets()) != 0 {
		t.Errorf("the set is still suspended after a completed resume: %v", engine.SuspendedBackupSets())
	}
	if _, err := engine.Run(context.Background(), RunRequest{
		Plan:        tr.snapshot(t, "run-3"),
		BackupSetID: tr.setID,
		Backup:      func(context.Context) error { return nil },
	}); err != nil {
		t.Fatalf("a run after a completed resume was refused: %v", err)
	}
}

// Nothing replays on restart. Reconcile records and blocks; it does not
// run a single hook, because it cannot know what the interrupted step got
// as far as doing.
func TestRestartRunsNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, fullTree())

	interrupt(t, h, tr.snapshot(t, "run-1"), "10-quiesce.remote.sh")

	h.rec.mu.Lock()
	h.rec.calls = nil
	h.rec.mu.Unlock()

	restart(t, h)

	if got := h.rec.dispatched(); len(got) != 0 {
		t.Fatalf("a restart executed %v; a daemon coming back up must never replay a hook", got)
	}
}

// An "after" step that was interrupted cannot be accounted for by running
// anything: its own outcome is unknown, so re-running it might apply an
// undo twice and skipping it might leave one half-applied. The obligation
// therefore goes back to recovery_required, and the only exit left is a
// person -- which is what the acknowledgement is for, and why it takes a
// reason.
func TestResumeLeavesAnUnaccountableScopeBlockedUntilAcknowledged(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setAfter:     {"30-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	interrupt(t, h, tr.snapshot(t, "run-1"), "30-resume.remote.sh")

	engine := restart(t, h)

	res, err := engine.ResumeCleanup(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ResumeCleanup: %v", err)
	}
	if !res.RecoveryOutstanding {
		t.Fatal("a scope with an interrupted cleanup step was reported as accounted for")
	}
	if res.State != workflow.StateRecoveryRequired {
		t.Errorf("the resumed run is %q, want recovery_required", res.State)
	}

	// The global scope, whose cleanup COULD be run, was: the resume does
	// what it can and blocks on what it cannot.
	if got := obligationOf(t, h.store, "run-1", workflow.ScopeGlobal).State; got != workflow.ObligationSuccess {
		t.Errorf("the global obligation is %q, want success", got)
	}
	if got := obligationOf(t, h.store, "run-1", workflow.ScopeSet).State; got != workflow.ObligationRecoveryRequired {
		t.Errorf("the set obligation is %q, want it still requiring recovery", got)
	}

	if len(engine.SuspendedBackupSets()) != 1 {
		t.Fatalf("the set is not suspended after an incomplete resume: %v", engine.SuspendedBackupSets())
	}

	// An acknowledgement with no reason, or with nobody's name on it, is
	// not an acknowledgement.
	for _, ack := range []Acknowledgement{
		{By: "operator"},
		{Reason: "checked by hand"},
		{By: "operator", Reason: "   "},
	} {
		if err := engine.AcknowledgeRecovery(context.Background(), "run-1", ack); err == nil {
			t.Errorf("AcknowledgeRecovery accepted %+v", ack)
		}
	}

	if err := engine.AcknowledgeRecovery(context.Background(), "run-1", Acknowledgement{
		By:     "operator@example",
		Reason: "resumed the database by hand and checked the share is unmounted",
	}); err != nil {
		t.Fatalf("AcknowledgeRecovery: %v", err)
	}

	// The audit record is durable, and it names who and why.
	o := obligationOf(t, h.store, "run-1", workflow.ScopeSet)
	if o.State != workflow.ObligationAcknowledged {
		t.Fatalf("the acknowledged obligation is %q", o.State)
	}
	if o.AcknowledgedBy != "operator@example" {
		t.Errorf("the audit record names %q", o.AcknowledgedBy)
	}
	if !strings.Contains(o.AcknowledgeReason, "by hand") {
		t.Errorf("the audit reason is %q", o.AcknowledgeReason)
	}
	if o.AcknowledgedAt == nil {
		t.Error("the audit record has no time on it")
	}

	// And the set runs again.
	run, err := h.store.WorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.RecoveryState != string(workflow.RecoveryResolved) {
		t.Errorf("the acknowledged run's recovery state is %q, want resolved", run.RecoveryState)
	}
	if len(engine.SuspendedBackupSets()) != 0 {
		t.Errorf("the set is still suspended after an acknowledgement: %v", engine.SuspendedBackupSets())
	}
}

// #811's technical requirement, asserted from the direction it will
// actually be attacked from: L6's --skip-workflow-scripts.
//
// A bypassed run is a history fact and nothing more. It cannot start
// while a set is blocked, and there is no write on the journal that
// settles a recovery without either a cleanup that accounted for every
// scope or an acknowledgement with a reason.
func TestSkippingWorkflowScriptsCannotClearRecoveryRequired(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, fullTree())

	interrupt(t, h, tr.snapshot(t, "run-1"), "10-quiesce.remote.sh")
	engine := restart(t, h)

	// A run that declares it is skipping the hooks is refused exactly as
	// an ordinary one is.
	_, err := engine.Run(context.Background(), RunRequest{
		Plan:        tr.snapshot(t, "run-2"),
		BackupSetID: tr.setID,
		Backup:      func(context.Context) error { return nil },
		Bypassed:    true,
	})
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("a bypassed run of a blocked set returned %v, want ErrRecoveryRequired", err)
	}

	// A set with no hooks configured at all is refused too: the block is
	// about the machine the last run left behind, not about what the
	// next run intends to do.
	_, err = engine.Run(context.Background(), RunRequest{
		BackupSetID: tr.setID,
		Backup:      func(context.Context) error { return nil },
	})
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("a hook-less run of a blocked set returned %v, want ErrRecoveryRequired", err)
	}

	// And the journal itself refuses to settle the recovery while a
	// scope is outstanding, whatever a caller asks for.
	err = h.store.ResolveWorkflowRecovery(context.Background(), "run-1", workflow.StateRecovered, h.clock.Now())
	if err == nil {
		t.Fatal("the journal settled a recovery with a scope still outstanding")
	}
	if !errors.Is(err, state.ErrRecoveryOutstanding) {
		t.Errorf("the refusal is not ErrRecoveryOutstanding: %v", err)
	}

	// Nor can an ordinary run advance lower the axis.
	err = h.store.AdvanceWorkflowRun(context.Background(), "run-1", state.WorkflowRunAdvance{
		RecoveryState: workflow.RecoveryNone,
	})
	if err == nil {
		t.Fatal("an ordinary advance cleared an outstanding recovery")
	}
}

// Reconcile is what installs the block, so a run started before it has
// happened is refused rather than allowed through on an empty answer.
func TestARunBeforeReconcileIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, fullTree())

	fresh := &Engine{Store: h.store, Local: h.local, Remote: h.remote, Now: h.clock.Now}

	_, err := fresh.Run(context.Background(), RunRequest{
		Plan:        tr.snapshot(t, "run-1"),
		BackupSetID: tr.setID,
		Backup:      func(context.Context) error { return nil },
	})
	if !errors.Is(err, ErrNotReconciled) {
		t.Fatalf("a run on an unreconciled engine returned %v, want ErrNotReconciled", err)
	}
}

// A run that is not in recovery has no cleanup to resume, and saying so
// is better than running its "after" hooks a second time.
func TestResumeRefusesARunThatIsNotInRecovery(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, fullTree())

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := h.engine.ResumeCleanup(context.Background(), "run-1"); err == nil {
		t.Fatal("ResumeCleanup ran the cleanup of a run that had already finished it")
	}
}
