package service

import (
	"context"
	"errors"
	"testing"

	"github.com/backupdproject/backupd/core/internal/state"
	"github.com/backupdproject/backupd/core/internal/workflow"
	"github.com/backupdproject/backupd/core/internal/workflowrun"
)

// What a process does when its workflow reconciliation FAILED (#813).
//
// The reconciliation is what decides which backup sets are blocked by an
// interrupted run whose cleanup nobody accounted for. So a failure means
// this process does not know which machines may be sitting quiesced, and
// the moment that answer is unknown is exactly the moment a run must not
// start. A deployment that carried on serving with the lifecycle
// uninstalled had the guard switched off precisely when it was needed:
// the scheduler and the API ran backups directly, with no hooks and no
// recovery check.
//
// The other half is just as deliberate. Reading is NOT gated: the runs,
// the recovery holds and the resume/acknowledge actions stay available,
// because they are how an operator lifts the gate.

// reconcileFailsStore is the journal with its reconciliation read broken,
// and nothing else: every other call goes to the real journal, so a read
// beside the closed gate is a real read rather than an error from a
// stand-in.
type reconcileFailsStore struct {
	workflowrun.Store
	err error
}

func (s reconcileFailsStore) WorkflowRunsInStates(context.Context, ...workflow.State) ([]state.WorkflowRun, error) {
	return nil, s.err
}

// breakReconciliation makes this service's next reconciliation fail the
// way an unreadable journal row does.
func breakReconciliation(t *testing.T, svc *BackupService) {
	t.Helper()

	rt := svc.runtime()
	if rt == nil || rt.engine == nil {
		t.Fatal("this service has no workflow engine, so there is no reconciliation to break")
	}
	rt.engine.Store = reconcileFailsStore{Store: rt.engine.Store, err: errors.New("this journal's workflow rows cannot be read")}
}

func TestAFailedReconciliationRefusesEveryRunAction(t *testing.T) {
	svc, _, _ := openWorkflowConfigService(t, "unused")
	ctx := context.Background()

	breakReconciliation(t, svc)
	if _, err := svc.ReconcileWorkflows(ctx); err == nil {
		t.Fatal("the reconciliation succeeded, so this test is not exercising a failure")
	}

	if err := svc.WorkflowReconcileGate(); !errors.Is(err, ErrWorkflowReconcileIncomplete) {
		t.Fatalf("WorkflowReconcileGate = %v, want ErrWorkflowReconcileIncomplete", err)
	}

	revision := svc.state.Load().revision

	if _, err := svc.SubmitRunCycle(ctx, RunCycleRequest{
		IdempotencyKey: "key-cycle", ConfigRevision: revision, Actor: "operator",
	}); !errors.Is(err, ErrWorkflowReconcileIncomplete) {
		t.Errorf("SubmitRunCycle = %v, want the reconciliation refusal; a deployment whose recovery state is unknown must not start a deployment-wide run", err)
	}

	if _, err := svc.SubmitRunBackupSet(ctx, RunBackupSetRequest{
		IdempotencyKey: "key-set", ConfigRevision: revision, BackupSetID: "production/alpha", Actor: "operator",
	}); !errors.Is(err, ErrWorkflowReconcileIncomplete) {
		t.Errorf("SubmitRunBackupSet = %v, want the reconciliation refusal", err)
	}

	// And no operation row was created for either, which is what makes
	// the refusal a refusal rather than a queued run that will not
	// happen.
	ops, err := svc.ListOperations(ctx, 0)
	if err != nil {
		t.Fatalf("ListOperations: %v", err)
	}
	if len(ops) != 0 {
		t.Errorf("%d durable operation row(s) exist for runs that were refused: %+v", len(ops), ops)
	}
}

// TestAFailedReconciliationSkipsTheScheduledTick is the same gate on the
// path nobody is watching, which is the one that matters most: a tick
// runs unattended, so an unguarded one would back up over a quiesced
// machine at three in the morning.
//
// It asserts through the single-flight lock rather than through a log
// line: a tick that returned without running takes and releases nothing,
// so the lock being free afterwards and no cycle having been recorded is
// the observable fact.
func TestAFailedReconciliationSkipsTheScheduledTick(t *testing.T) {
	svc, _, _ := openWorkflowConfigService(t, "unused")
	ctx := context.Background()

	breakReconciliation(t, svc)
	if _, err := svc.ReconcileWorkflows(ctx); err == nil {
		t.Fatal("the reconciliation succeeded, so this test is not exercising a failure")
	}

	svc.runScheduledCycle(ctx)

	if !svc.runOnce.TryLock() {
		t.Fatal("the skipped tick left the single-flight lock held, so every later run in this process would be refused for the wrong reason")
	}
	svc.runOnce.Unlock()

	// A tick that ran would have recorded an operation-free cycle in the
	// journal's artifact rows and in the feed; the cheapest observable
	// is that nothing was walked at all.
	runs, err := svc.WorkflowRuns(ctx, "", 0)
	if err != nil {
		t.Fatalf("WorkflowRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("the refused tick produced %d workflow run(s): %+v", len(runs), runs)
	}
}

// TestAFailedReconciliationLeavesRecoveryReadable is the other half, and
// without it the gate above would be satisfied by a process that refused
// everything -- which would take away the surface an operator lifts the
// gate from.
func TestAFailedReconciliationLeavesRecoveryReadable(t *testing.T) {
	svc, _, _ := openWorkflowConfigService(t, "unused")
	ctx := context.Background()

	breakReconciliation(t, svc)
	if _, err := svc.ReconcileWorkflows(ctx); err == nil {
		t.Fatal("the reconciliation succeeded, so this test is not exercising a failure")
	}

	if _, err := svc.WorkflowRecovery(ctx); err != nil {
		t.Errorf("WorkflowRecovery = %v, want the holds this process knows about; reading is how the gate gets lifted", err)
	}
	if _, err := svc.WorkflowRuns(ctx, "", 0); err != nil {
		t.Errorf("WorkflowRuns = %v, want the journal's rows", err)
	}
	if _, err := svc.ListBackupSets(ctx); err != nil {
		t.Errorf("ListBackupSets = %v; inspection is not gated", err)
	}
}

// TestASuccessfulReconciliationOpensTheGate keeps the gate from being a
// one-way door: the refusal is a state of this process, so a second
// attempt that works starts backing up again without a restart.
func TestASuccessfulReconciliationOpensTheGate(t *testing.T) {
	svc, _, _ := openWorkflowConfigService(t, "unused")
	ctx := context.Background()

	rt := svc.runtime()
	real := rt.engine.Store
	rt.engine.Store = reconcileFailsStore{Store: real, err: errors.New("this journal's workflow rows cannot be read")}
	if _, err := svc.ReconcileWorkflows(ctx); err == nil {
		t.Fatal("the reconciliation succeeded, so this test is not exercising a failure")
	}

	rt.engine.Store = real
	if _, err := svc.ReconcileWorkflows(ctx); err != nil {
		t.Fatalf("the second reconciliation failed: %v", err)
	}

	if err := svc.WorkflowReconcileGate(); err != nil {
		t.Errorf("WorkflowReconcileGate = %v after a successful pass; the gate must open again", err)
	}
}
