package workflowrun

import (
	"context"
	"time"

	"github.com/backupdproject/backupd/core/internal/state"
	"github.com/backupdproject/backupd/core/internal/workflow"
)

// Store is the durable half of this engine, named as an interface for one
// reason that is worth stating because it is not the usual one.
//
// It is NOT here to allow a fake journal. Every suite in this package
// runs against the real *state.Journal on a real SQLite file, because the
// guarantees being tested are the journal's transitions and a fake would
// be a second, more permissive implementation of exactly the thing under
// test.
//
// It is here so that a test can make a durable write FAIL at a chosen
// point. "A crash between any two durable transitions reconciles
// deterministically" is #811's central claim and it is not assertable
// without the ability to stop the engine mid-sequence and then reconcile
// the real rows that were left behind. A decorator around this interface
// is how that is done (see crash_test.go), and the rows it leaves are the
// real journal's.
//
// The surface is wide because the engine's dependency on the journal
// genuinely is. Narrowing it by grouping calls behind coarser methods
// would move engine logic into the journal, which is the layering this
// product spent 0012's header arguing against.
type Store interface {
	CommitWorkflowPlan(ctx context.Context, plan state.WorkflowPlan) error

	StartWorkflowStep(ctx context.Context, runID, stepID string, at time.Time) error
	FinishWorkflowStep(ctx context.Context, runID, stepID string, out state.WorkflowStepOutcome) error

	AdvanceWorkflowRun(ctx context.Context, runID string, adv state.WorkflowRunAdvance) error
	AdvanceWorkflowCleanupObligation(ctx context.Context, adv state.WorkflowObligationAdvance) error
	ApplyWorkflowReconciliation(ctx context.Context, rec state.WorkflowReconciliation) error
	ResolveWorkflowRecovery(ctx context.Context, runID string, to workflow.State, at time.Time) error

	WorkflowRun(ctx context.Context, runID string) (state.WorkflowRun, error)
	WorkflowSteps(ctx context.Context, runID string) ([]state.WorkflowStep, error)
	WorkflowCleanupObligations(ctx context.Context, runID string) ([]workflow.CleanupObligation, error)
	ObligationsRequiringRecovery(ctx context.Context) ([]workflow.CleanupObligation, error)
	WorkflowRunsInStates(ctx context.Context, states ...workflow.State) ([]state.WorkflowRun, error)
	RecoverWorkflowPlan(ctx context.Context, runID string) (workflow.Plan, error)

	AppendWorkflowStepLog(ctx context.Context, rec workflow.StepLog) error
	WorkflowStepLogsAfter(ctx context.Context, runID string, afterSeq uint64, limit int) ([]workflow.StepLog, error)
}

// The journal is the Store. Asserted here rather than left to the call
// site, so that a method added to this interface fails to compile in this
// package instead of in whatever wires the engine up.
var _ Store = (*state.Journal)(nil)
