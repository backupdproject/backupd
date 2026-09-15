package state

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/workflow"
)

// The durable transitions #811 runs the five-stage lifecycle on, held to
// the one property the crash-safety argument needs from this layer: a
// transition that is not legal is REFUSED rather than written, so a
// journal read back after a restart can only ever hold a position the
// engine could have put it in.

func commitTestPlan(t *testing.T, j *Journal, runID string) WorkflowPlan {
	t.Helper()

	plan := testWorkflowPlan(runID)

	if err := j.CommitWorkflowPlan(context.Background(), plan); err != nil {
		t.Fatalf("CommitWorkflowPlan: %v", err)
	}

	return plan
}

func stepIDs(t *testing.T, plan WorkflowPlan) []string {
	t.Helper()

	ids := make([]string, 0, len(plan.Steps))
	for _, s := range plan.Steps {
		ids = append(ids, s.ID)
	}
	if len(ids) < 2 {
		t.Fatalf("the fixture plan has %d steps; these tests need at least two", len(ids))
	}

	return ids
}

func TestWorkflowStepRunsThenReachesATerminalState(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := commitTestPlan(t, j, "run-1")
	ids := stepIDs(t, plan)

	started := plan.Run.StartedAt.Add(time.Second)
	if err := j.StartWorkflowStep(ctx, "run-1", ids[0], started); err != nil {
		t.Fatalf("StartWorkflowStep: %v", err)
	}

	steps, err := j.WorkflowSteps(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowSteps: %v", err)
	}
	if got := steps[0].State; got != string(workflow.StateRunning) {
		t.Errorf("the started step reads %q, want %q", got, workflow.StateRunning)
	}
	if steps[0].StartedAt == nil || !steps[0].StartedAt.Equal(started) {
		t.Errorf("the started step's start time is %v, want %s", steps[0].StartedAt, started)
	}

	code := 0
	if err := j.FinishWorkflowStep(ctx, "run-1", ids[0], WorkflowStepOutcome{
		State:        workflow.StateSuccess,
		FinishedAt:   started.Add(2 * time.Second),
		ExitCode:     &code,
		StdoutLogRef: "run-1/" + ids[0] + "/stdout",
	}); err != nil {
		t.Fatalf("FinishWorkflowStep: %v", err)
	}

	steps, err = j.WorkflowSteps(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowSteps: %v", err)
	}
	if got := steps[0].State; got != string(workflow.StateSuccess) {
		t.Errorf("the finished step reads %q, want %q", got, workflow.StateSuccess)
	}
	if steps[0].ExitCode == nil || *steps[0].ExitCode != 0 {
		t.Errorf("the finished step's exit code is %v, want 0", steps[0].ExitCode)
	}
	if steps[0].StdoutLogRef == "" {
		t.Error("the finished step records no stdout log reference")
	}
}

// The refusals that make the durable record trustworthy. Each one is a
// write this journal must not accept, and the reason is the same in every
// case: a recovery pass has no way to tell a row that was written out of
// order from one that describes what really happened.
func TestWorkflowStepTransitionsAreRefusedWhenIllegal(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := commitTestPlan(t, j, "run-1")
	ids := stepIDs(t, plan)
	at := plan.Run.StartedAt.Add(time.Second)

	// A step cannot finish without having started, except by being
	// skipped: a "success" with no start is a step nothing observed.
	if err := j.FinishWorkflowStep(ctx, "run-1", ids[0], WorkflowStepOutcome{
		State: workflow.StateSuccess, FinishedAt: at,
	}); err == nil {
		t.Error("a pending step was allowed to succeed without ever having run")
	}

	// Skipping one that never started is exactly what an abandoned stage
	// does, and it is allowed.
	if err := j.FinishWorkflowStep(ctx, "run-1", ids[1], WorkflowStepOutcome{
		State: workflow.StateSkipped, FinishedAt: at,
	}); err != nil {
		t.Fatalf("skipping a pending step was refused: %v", err)
	}

	// And a skipped step is terminal: nothing re-opens it.
	if err := j.StartWorkflowStep(ctx, "run-1", ids[1], at); err == nil {
		t.Error("a skipped step was allowed to start running")
	}

	if err := j.StartWorkflowStep(ctx, "run-1", ids[0], at); err != nil {
		t.Fatalf("StartWorkflowStep: %v", err)
	}
	if err := j.StartWorkflowStep(ctx, "run-1", ids[0], at); err == nil {
		t.Error("a running step was allowed to start a second time")
	}

	// An exit code on an outcome nobody observed a status for is the one
	// refusal #810 and #811 share: transport loss is never a known exit
	// code, and this is the layer that cannot be talked round.
	code := 0
	if err := j.FinishWorkflowStep(ctx, "run-1", ids[0], WorkflowStepOutcome{
		State: workflow.StateInterrupted, FinishedAt: at, ExitCode: &code,
	}); err == nil {
		t.Error("an interrupted step was allowed to carry an exit code")
	}

	if err := j.FinishWorkflowStep(ctx, "run-1", "no-such-step", WorkflowStepOutcome{
		State: workflow.StateSuccess, FinishedAt: at,
	}); err == nil {
		t.Error("a step this journal does not have was allowed to finish")
	}
}

func TestWorkflowRunAdvanceRecordsTheThreeStatusesSeparately(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := commitTestPlan(t, j, "run-1")
	finished := plan.Run.StartedAt.Add(time.Minute)

	if err := j.AdvanceWorkflowRun(ctx, "run-1", WorkflowRunAdvance{
		To: workflow.StateRunning, BackupStatus: workflow.StatusRunning,
	}); err != nil {
		t.Fatalf("AdvanceWorkflowRun: %v", err)
	}

	// Cleanup only ever fails from cleanup_running: a run that reported
	// cleanup_failed without having been recorded as running its cleanup
	// is a run nothing can prove attempted one.
	if err := j.AdvanceWorkflowRun(ctx, "run-1", WorkflowRunAdvance{
		To: workflow.StateCleanupRunning, CleanupStatus: workflow.StatusRunning,
	}); err != nil {
		t.Fatalf("AdvanceWorkflowRun: %v", err)
	}

	// And the scope it entered is accounted for: a cleanup that failed
	// is a cleanup that RAN, so the obligation moves with it. A run
	// cannot be closed while a scope is neither settled nor declared as
	// needing recovery (TestNoRunReachesATerminalStateWithAScopeUnaccountedFor).
	for _, to := range []workflow.ObligationState{workflow.ObligationRunning, workflow.ObligationFailed} {
		if err := j.AdvanceWorkflowCleanupObligation(ctx, WorkflowObligationAdvance{
			RunID: "run-1", Scope: workflow.ScopeGlobal, To: to, At: finished,
		}); err != nil {
			t.Fatalf("AdvanceWorkflowCleanupObligation(%s): %v", to, err)
		}
	}

	// The three statuses are separate fields end to end: a backup that
	// succeeded, a cleanup that failed and a workflow that therefore
	// failed all read back as themselves.
	if err := j.AdvanceWorkflowRun(ctx, "run-1", WorkflowRunAdvance{
		To:             workflow.StateCleanupFailed,
		BackupStatus:   workflow.StatusSuccess,
		CleanupStatus:  workflow.StatusFailed,
		WorkflowStatus: workflow.StatusFailed,
		FinishedAt:     &finished,
	}); err != nil {
		t.Fatalf("AdvanceWorkflowRun: %v", err)
	}

	run, err := j.WorkflowRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.BackupStatus != string(workflow.StatusSuccess) {
		t.Errorf("backup status is %q, want success", run.BackupStatus)
	}
	if run.CleanupStatus != string(workflow.StatusFailed) {
		t.Errorf("cleanup status is %q, want failed", run.CleanupStatus)
	}
	if run.WorkflowStatus != string(workflow.StatusFailed) {
		t.Errorf("workflow status is %q, want failed", run.WorkflowStatus)
	}
	if run.FinishedAt == nil || !run.FinishedAt.Equal(finished) {
		t.Errorf("the run's finish time is %v, want %s", run.FinishedAt, finished)
	}
}

// The structural half of "--skip-workflow-scripts must be unable to clear
// recovery_required" (#811's technical requirements). There is no write
// on this journal that lowers a run's recovery state: the two calls that
// can are ResumeWorkflowRecovery, which requires every obligation to have
// reached a terminal recorded state, and AcknowledgeWorkflowCleanup,
// which requires an actor and a reason.
func TestNoOrdinaryRunAdvanceCanClearRecoveryRequired(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	commitTestPlan(t, j, "run-1")

	if err := j.AdvanceWorkflowRun(ctx, "run-1", WorkflowRunAdvance{
		To: workflow.StateRecoveryRequired, RecoveryState: workflow.RecoveryRequired,
	}); err != nil {
		t.Fatalf("AdvanceWorkflowRun: %v", err)
	}

	for _, to := range []workflow.RecoveryState{workflow.RecoveryNone, workflow.RecoveryResolved} {
		err := j.AdvanceWorkflowRun(ctx, "run-1", WorkflowRunAdvance{RecoveryState: to})
		if err == nil {
			t.Fatalf("an ordinary run advance cleared recovery_required to %q", to)
		}
		if !strings.Contains(err.Error(), "recovery") {
			t.Errorf("the refusal does not mention recovery: %v", err)
		}
	}

	// It is not merely refused in the return value: the row did not move.
	run, err := j.WorkflowRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.RecoveryState != string(workflow.RecoveryRequired) {
		t.Fatalf("the run's recovery state is %q, want it still required", run.RecoveryState)
	}
}

func TestCleanupObligationsAreCommittedWithThePlanAndAdvanceLegally(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := commitTestPlan(t, j, "run-1")

	obligations, err := j.WorkflowCleanupObligations(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowCleanupObligations: %v", err)
	}
	if len(obligations) != 2 {
		t.Fatalf("the run has %d obligations, want 2: %+v", len(obligations), obligations)
	}
	if obligations[0].Scope != workflow.ScopeGlobal || obligations[0].State != workflow.ObligationEligible {
		t.Errorf("the first obligation is %+v, want the global scope eligible", obligations[0])
	}
	if obligations[1].Scope != workflow.ScopeSet || obligations[1].State != workflow.ObligationNeverEligible {
		t.Errorf("the second obligation is %+v, want the set scope never eligible", obligations[1])
	}
	if obligations[0].BackupSetID != plan.Run.BackupSetID {
		t.Errorf("the obligation names set %s, want %s", obligations[0].BackupSetID, plan.Run.BackupSetID)
	}

	at := plan.Run.StartedAt.Add(time.Second)

	// never_eligible -> eligible is the scope-entry write, and it must
	// record when the scope was entered.
	if err := j.AdvanceWorkflowCleanupObligation(ctx, WorkflowObligationAdvance{
		RunID: "run-1", Scope: workflow.ScopeSet, To: workflow.ObligationEligible, At: at,
	}); err != nil {
		t.Fatalf("entering the set scope: %v", err)
	}

	// eligible -> success is not a legal edge: a cleanup that reports
	// success without having been recorded as running is a cleanup
	// nothing can prove ran.
	if err := j.AdvanceWorkflowCleanupObligation(ctx, WorkflowObligationAdvance{
		RunID: "run-1", Scope: workflow.ScopeSet, To: workflow.ObligationSuccess, At: at,
	}); err == nil {
		t.Error("an eligible obligation was allowed to succeed without running")
	}

	for _, to := range []workflow.ObligationState{workflow.ObligationRunning, workflow.ObligationSuccess} {
		if err := j.AdvanceWorkflowCleanupObligation(ctx, WorkflowObligationAdvance{
			RunID: "run-1", Scope: workflow.ScopeSet, To: to, At: at,
		}); err != nil {
			t.Fatalf("advancing the set obligation to %q: %v", to, err)
		}
	}

	// And a settled obligation is settled: nothing re-opens it.
	if err := j.AdvanceWorkflowCleanupObligation(ctx, WorkflowObligationAdvance{
		RunID: "run-1", Scope: workflow.ScopeSet, To: workflow.ObligationRunning, At: at,
	}); err == nil {
		t.Error("a successful obligation was re-opened")
	}

	obligations, err = j.WorkflowCleanupObligations(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowCleanupObligations: %v", err)
	}
	set := obligations[1]
	if set.State != workflow.ObligationSuccess {
		t.Errorf("the set obligation is %q, want success", set.State)
	}
	if set.EnteredAt == nil || set.StartedAt == nil || set.FinishedAt == nil {
		t.Errorf("the finished obligation does not carry all three timestamps: %+v", set)
	}
}

func TestAcknowledgingACleanupObligationRecordsTheAudit(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := commitTestPlan(t, j, "run-1")
	at := plan.Run.StartedAt.Add(time.Hour)

	if err := j.AdvanceWorkflowCleanupObligation(ctx, WorkflowObligationAdvance{
		RunID: "run-1", Scope: workflow.ScopeGlobal, To: workflow.ObligationRecoveryRequired, At: at,
	}); err != nil {
		t.Fatalf("marking the global obligation as requiring recovery: %v", err)
	}

	// An acknowledgement with no reason is not an acknowledgement.
	if err := j.AdvanceWorkflowCleanupObligation(ctx, WorkflowObligationAdvance{
		RunID: "run-1", Scope: workflow.ScopeGlobal, To: workflow.ObligationAcknowledged,
		At: at, AcknowledgedBy: "operator",
	}); err == nil {
		t.Error("an acknowledgement with no audit reason was accepted")
	}

	if err := j.AdvanceWorkflowCleanupObligation(ctx, WorkflowObligationAdvance{
		RunID: "run-1", Scope: workflow.ScopeGlobal, To: workflow.ObligationAcknowledged,
		At: at, AcknowledgedBy: "operator@example", AcknowledgeReason: "unmounted the share by hand",
	}); err != nil {
		t.Fatalf("acknowledging: %v", err)
	}

	obligations, err := j.WorkflowCleanupObligations(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowCleanupObligations: %v", err)
	}
	got := obligations[0]
	if got.State != workflow.ObligationAcknowledged {
		t.Fatalf("the global obligation is %q, want acknowledged", got.State)
	}
	if got.AcknowledgedBy != "operator@example" || got.AcknowledgeReason != "unmounted the share by hand" {
		t.Errorf("the audit record is %q / %q", got.AcknowledgedBy, got.AcknowledgeReason)
	}
	if got.AcknowledgedAt == nil || !got.AcknowledgedAt.Equal(at) {
		t.Errorf("the acknowledgement time is %v, want %s", got.AcknowledgedAt, at)
	}
}

// One transaction, whatever it takes: a reconciliation that moved a run
// and left its steps alone would be a reconciliation a second restart
// reads as a run with nothing outstanding.
func TestWorkflowReconciliationIsOneTransaction(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := commitTestPlan(t, j, "run-1")
	ids := stepIDs(t, plan)
	at := plan.Run.StartedAt.Add(time.Second)

	if err := j.StartWorkflowStep(ctx, "run-1", ids[0], at); err != nil {
		t.Fatalf("StartWorkflowStep: %v", err)
	}

	// A reconciliation naming one step that cannot move must write
	// nothing at all, including the parts of it that could.
	bad := WorkflowReconciliation{
		RunID: "run-1",
		Run:   WorkflowRunAdvance{To: workflow.StateRecoveryRequired, RecoveryState: workflow.RecoveryRequired},
		Steps: []WorkflowStepReconciliation{
			{StepID: ids[0], State: workflow.StateInterrupted, At: at},
			{StepID: "no-such-step", State: workflow.StateInterrupted, At: at},
		},
	}
	if err := j.ApplyWorkflowReconciliation(ctx, bad); err == nil {
		t.Fatal("a reconciliation naming a step this journal does not have was accepted")
	}

	run, err := j.WorkflowRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.State == string(workflow.StateRecoveryRequired) {
		t.Fatal("the refused reconciliation moved the run anyway, so it is not one transaction")
	}

	good := WorkflowReconciliation{
		RunID: "run-1",
		Run:   WorkflowRunAdvance{To: workflow.StateRecoveryRequired, RecoveryState: workflow.RecoveryRequired},
		Steps: []WorkflowStepReconciliation{{StepID: ids[0], State: workflow.StateInterrupted, At: at}},
		Obligations: []WorkflowObligationAdvance{{
			RunID: "run-1", Scope: workflow.ScopeGlobal, To: workflow.ObligationRecoveryRequired, At: at,
		}},
	}
	if err := j.ApplyWorkflowReconciliation(ctx, good); err != nil {
		t.Fatalf("ApplyWorkflowReconciliation: %v", err)
	}

	run, err = j.WorkflowRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.State != string(workflow.StateRecoveryRequired) || run.RecoveryState != string(workflow.RecoveryRequired) {
		t.Errorf("the reconciled run is %q / %q", run.State, run.RecoveryState)
	}

	steps, err := j.WorkflowSteps(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowSteps: %v", err)
	}
	if steps[0].State != string(workflow.StateInterrupted) {
		t.Errorf("the running step reconciled to %q, want interrupted", steps[0].State)
	}
	if steps[0].ExitCode != nil {
		t.Errorf("the interrupted step carries exit code %d; nobody observed one", *steps[0].ExitCode)
	}
}

// The two reads a startup reconciliation and a per-set refusal are built
// on. Both go through the vocabulary rather than a SQL predicate, so the
// Go side and the partial index cannot drift apart.
func TestWorkflowRunQueriesFindTheOpenAndTheUnsettled(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := commitTestPlan(t, j, "run-1")
	commitTestPlan(t, j, "run-2")

	if err := j.AdvanceWorkflowRun(ctx, "run-1", WorkflowRunAdvance{To: workflow.StateRunning}); err != nil {
		t.Fatalf("AdvanceWorkflowRun: %v", err)
	}

	open, err := j.WorkflowRunsInStates(ctx, workflow.StateRunning, workflow.StatePending)
	if err != nil {
		t.Fatalf("WorkflowRunsInStates: %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("there are %d open runs, want 2", len(open))
	}
	if open[0].RunID != "run-1" || open[1].RunID != "run-2" {
		t.Errorf("open runs came back as %q, %q; want them in start order", open[0].RunID, open[1].RunID)
	}

	unsettled, err := j.WorkflowRunsRequiringRecovery(ctx)
	if err != nil {
		t.Fatalf("WorkflowRunsRequiringRecovery: %v", err)
	}
	if len(unsettled) != 0 {
		t.Fatalf("a fresh run already requires recovery: %+v", unsettled)
	}

	if err := j.AdvanceWorkflowRun(ctx, "run-1", WorkflowRunAdvance{
		To: workflow.StateRecoveryRequired, RecoveryState: workflow.RecoveryRequired,
	}); err != nil {
		t.Fatalf("AdvanceWorkflowRun: %v", err)
	}

	unsettled, err = j.WorkflowRunsRequiringRecovery(ctx)
	if err != nil {
		t.Fatalf("WorkflowRunsRequiringRecovery: %v", err)
	}
	if len(unsettled) != 1 || unsettled[0].RunID != "run-1" {
		t.Fatalf("the unsettled set is %+v, want just run-1", unsettled)
	}
	if unsettled[0].BackupSetID != plan.Run.BackupSetID.String() {
		t.Errorf("the unsettled run names set %q, want %q", unsettled[0].BackupSetID, plan.Run.BackupSetID)
	}
}

func TestWorkflowStepLogsReplayByCursorWithoutAGap(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := commitTestPlan(t, j, "run-1")
	ids := stepIDs(t, plan)
	at := plan.Run.StartedAt

	// A hook's output is not required to be UTF-8, and a journal that
	// re-encoded it would be editing evidence.
	binary := []byte{0x00, 0xff, 0xfe, 'h', 'i', 0x0a}

	records := []workflow.StepLog{
		{RunID: "run-1", StepID: ids[0], Seq: 1, Kind: workflow.LogOutput, Stream: workflow.LogStreamStdout, CapturedAt: at, Payload: []byte("one\n")},
		{RunID: "run-1", StepID: ids[0], Seq: 2, Kind: workflow.LogOutput, Stream: workflow.LogStreamStderr, CapturedAt: at, Payload: binary},
		{RunID: "run-1", StepID: ids[1], Seq: 3, Kind: workflow.LogOutput, Stream: workflow.LogStreamStdout, CapturedAt: at, Payload: []byte("three\n")},
		{RunID: "run-1", StepID: ids[1], Seq: 4, Kind: workflow.LogTruncated, Stream: workflow.LogStreamStdout, CapturedAt: at, Payload: []byte("output truncated")},
	}
	for _, r := range records {
		if err := j.AppendWorkflowStepLog(ctx, r); err != nil {
			t.Fatalf("AppendWorkflowStepLog(%d): %v", r.Seq, err)
		}
	}

	// A repeated sequence number is refused rather than written, because
	// a follower's cursor is only a cursor if one number names one
	// record.
	if err := j.AppendWorkflowStepLog(ctx, records[0]); err == nil {
		t.Error("a second record claiming sequence 1 was accepted")
	}

	all, err := j.WorkflowStepLogsAfter(ctx, "run-1", 0, 10)
	if err != nil {
		t.Fatalf("WorkflowStepLogsAfter: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("the run has %d log records, want 4", len(all))
	}
	for i, r := range all {
		if r.Seq != uint64(i+1) {
			t.Fatalf("record %d has sequence %d; the replay is not in order", i, r.Seq)
		}
	}
	if !bytes.Equal(all[1].Payload, binary) {
		t.Errorf("the binary payload came back as %q, want %q", all[1].Payload, binary)
	}
	if all[1].Stream != workflow.LogStreamStderr {
		t.Errorf("the second record's stream is %q, want stderr", all[1].Stream)
	}
	if all[3].Kind != workflow.LogTruncated {
		t.Errorf("the fourth record's kind is %q, want the truncation marker", all[3].Kind)
	}

	// A follower that fell behind at 2 replays 3 and 4 and nothing else.
	tail, err := j.WorkflowStepLogsAfter(ctx, "run-1", 2, 10)
	if err != nil {
		t.Fatalf("WorkflowStepLogsAfter: %v", err)
	}
	if len(tail) != 2 || tail[0].Seq != 3 || tail[1].Seq != 4 {
		t.Fatalf("replaying from cursor 2 gave %+v", tail)
	}

	// And the bound is a bound: a follower asking for the whole history
	// of a noisy hook gets a page.
	page, err := j.WorkflowStepLogsAfter(ctx, "run-1", 0, 2)
	if err != nil {
		t.Fatalf("WorkflowStepLogsAfter: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("a limit of 2 returned %d records", len(page))
	}
}

// The partial index the per-set refusal scans names one obligation state
// as a SQL literal, and ObligationState.RequiresRecovery decides the same
// thing in Go. If the two drift, the refusal either scans a state nobody
// blocks on or misses the one that blocks a whole backup set.
func TestTheRecoveryIndexMatchesTheStateThatBlocksASet(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	var ddl string
	if err := j.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_workflow_obligations_recovery'`,
	).Scan(&ddl); err != nil {
		t.Fatalf("reading the index definition: %v", err)
	}

	for _, s := range workflow.ObligationStates() {
		named := strings.Contains(ddl, "'"+string(s)+"'")

		if named != s.RequiresRecovery() {
			t.Errorf("obligation state %q: RequiresRecovery() is %v in Go and the index predicate %s it:\n\t%s\n\n"+
				"This index is what the per-set refusal and the health warning scan. A state Go blocks on and the "+
				"index omits is a quiesced machine nobody is told about; one the index carries and Go does not is "+
				"every historical run in the deployment landing in a partial index that exists to stay small.",
				s, s.RequiresRecovery(), namedWord(named), ddl)
		}
	}
}

func namedWord(named bool) string {
	if named {
		return "names"
	}

	return "omits"
}

// A run does NOT reach a terminal state while a scope of it is still
// unaccounted for, and the graph is what refuses it rather than the
// engine remembering to.
//
// The crash-safety argument is stated as "a run with an unsettled
// obligation is either non-terminal or in recovery", and a crash cannot
// break it: nothing is written at all. A failed WRITE can, and that is
// the reachable case -- the advance that starts a scope's cleanup fails,
// the after steps never run, and the engine's summary write then closes
// the run as though the scope had been accounted for. A terminal run is
// invisible to the reconciliation (it is not an open run) and its
// obligation is therefore never turned into a hold, so the set is
// released with a machine still quiesced. Refusing the write here is
// what makes that unreachable however the caller is written.
func TestNoRunReachesATerminalStateWithAScopeUnaccountedFor(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	commitTestPlan(t, j, "run-1")
	at := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)

	// The global scope is committed eligible: entered, cleanup owed.
	err := j.AdvanceWorkflowRun(ctx, "run-1", WorkflowRunAdvance{
		To:             workflow.StateFailed,
		WorkflowStatus: workflow.StatusFailed,
		FinishedAt:     &at,
	})
	if err == nil {
		t.Fatal("a run was closed as failed with its global cleanup obligation still eligible")
	}
	if !strings.Contains(err.Error(), "global") {
		t.Errorf("the refusal does not name the scope that is outstanding: %v", err)
	}

	run, err := j.WorkflowRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.State != string(workflow.StatePending) {
		t.Errorf("the refused advance moved the run to %q", run.State)
	}

	// The two ways past it are the two that are honest. Either the
	// scope is accounted for...
	if err := j.AdvanceWorkflowCleanupObligation(ctx, WorkflowObligationAdvance{
		RunID: "run-1", Scope: workflow.ScopeGlobal, To: workflow.ObligationRunning, At: at,
	}); err != nil {
		t.Fatalf("AdvanceWorkflowCleanupObligation: %v", err)
	}
	if err := j.AdvanceWorkflowCleanupObligation(ctx, WorkflowObligationAdvance{
		RunID: "run-1", Scope: workflow.ScopeGlobal, To: workflow.ObligationFailed, At: at,
	}); err != nil {
		t.Fatalf("AdvanceWorkflowCleanupObligation: %v", err)
	}
	if err := j.AdvanceWorkflowRun(ctx, "run-1", WorkflowRunAdvance{
		To:             workflow.StateFailed,
		WorkflowStatus: workflow.StatusFailed,
		FinishedAt:     &at,
	}); err != nil {
		t.Fatalf("a run whose every scope is settled was refused a terminal state: %v", err)
	}
}

// ...or the run says out loud that it needs a person, which is
// non-terminal and blocks the set.
func TestARunWithAnOutstandingScopeMayStillSayItNeedsRecovery(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	commitTestPlan(t, j, "run-1")

	if err := j.AdvanceWorkflowRun(ctx, "run-1", WorkflowRunAdvance{
		To:            workflow.StateRecoveryRequired,
		RecoveryState: workflow.RecoveryRequired,
	}); err != nil {
		t.Fatalf("a run with an outstanding scope could not be moved to recovery_required: %v", err)
	}

	// And a run whose recovery is outstanding may be closed as
	// cleanup_failed: the recovery axis carries the fact that a scope is
	// unaccounted for, the spool is retained on that axis
	// (workflow.Run.SpoolRetainable), and the hold is what blocks the
	// set.
	at := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	if err := j.AdvanceWorkflowRun(ctx, "run-1", WorkflowRunAdvance{
		To:            workflow.StateCleanupFailed,
		CleanupStatus: workflow.StatusFailed,
		FinishedAt:    &at,
	}); err != nil {
		t.Fatalf("a run in recovery could not be closed as cleanup_failed: %v", err)
	}
}

// The deployment's own facts -- the source host, the source path, the
// destination -- are persisted WITH the plan, for the same reason the
// environment is: a recovery has to hand a hook the run it is unwinding,
// and those three are not things the journal could derive or today's
// configuration could be trusted for. An unmount of "$RETND_SOURCE_PATH"
// with an empty variable runs against the wrong thing or against nothing.
func TestAPlansFactsRoundTripSeparatelyFromItsEnvironment(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := testWorkflowPlan("run-1")
	plan.Facts = map[string]string{
		"RETND_SOURCE_HOST": "db.internal",
		"RETND_SOURCE_PATH": "/srv/data",
		"RETND_DESTINATION": "nas:/backups/production",
	}

	if err := j.CommitWorkflowPlan(ctx, plan); err != nil {
		t.Fatalf("CommitWorkflowPlan: %v", err)
	}

	facts, err := j.WorkflowRunFacts(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowRunFacts: %v", err)
	}
	if !reflect.DeepEqual(facts, plan.Facts) {
		t.Errorf("the facts came back as %v, want %v", facts, plan.Facts)
	}

	// The CONFIGURED environment is unchanged by their presence: a fact
	// is not a variable an operator wrote, and it must not appear in the
	// environment a recovered plan resolves (where a RETND_ name is
	// refused outright).
	recovered, err := j.RecoverWorkflowPlan(ctx, "run-1")
	if err != nil {
		t.Fatalf("RecoverWorkflowPlan: %v", err)
	}
	if !reflect.DeepEqual(recovered.Env().Vars(), plan.Env.Vars()) {
		t.Errorf("the recovered environment is %+v, want %+v", recovered.Env().Vars(), plan.Env.Vars())
	}

	// A run planned with no facts reads back as none rather than as an
	// error: the zero-fact case is a deployment whose hooks do not ask.
	if err := j.CommitWorkflowPlan(ctx, testWorkflowPlan("run-2")); err != nil {
		t.Fatalf("CommitWorkflowPlan: %v", err)
	}
	if got, err := j.WorkflowRunFacts(ctx, "run-2"); err != nil || len(got) != 0 {
		t.Errorf("a run with no facts read back %v, %v", got, err)
	}
}

// A fact this product does not inject is refused at the write, because
// the read is a hook's environment: a row saying PGPASSWORD is a fact
// would be a variable an operator never configured arriving in a
// recovered hook with whatever somebody put in the database.
func TestAPlansFactsMustBeBuiltinsThisProductInjects(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := testWorkflowPlan("run-1")
	plan.Facts = map[string]string{"PGPASSWORD": "hunter2"}

	err := j.CommitWorkflowPlan(ctx, plan)
	if err == nil {
		t.Fatal("a plan naming an arbitrary variable as a run fact was committed")
	}
	if !strings.Contains(err.Error(), "PGPASSWORD") {
		t.Errorf("the refusal does not name the offending fact: %v", err)
	}

	if _, err := j.WorkflowRun(ctx, "run-1"); err == nil {
		t.Error("the refused plan left a run row behind")
	}
}

// A resumed run appends to the log its interrupted self already wrote, so
// it has to know where that got to: one sequence number names one record
// (UNIQUE (run_id, seq)), and a recorder restarting at zero makes the
// first byte of output a failed insert -- which the engine reads as a
// step whose output could not be recorded.
func TestTheLastLogSequenceIsWhereAResumedRecorderCarriesOnFrom(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := commitTestPlan(t, j, "run-1")
	ids := stepIDs(t, plan)

	if got, err := j.WorkflowStepLogLastSeq(ctx, "run-1"); err != nil || got != 0 {
		t.Fatalf("a run with no output reports last sequence %d, %v; want 0", got, err)
	}

	at := plan.Run.StartedAt.Add(time.Second)
	for seq := uint64(1); seq <= 3; seq++ {
		if err := j.AppendWorkflowStepLog(ctx, workflow.StepLog{
			RunID: "run-1", StepID: ids[0], Seq: seq,
			Kind: workflow.LogOutput, Stream: workflow.LogStreamStdout,
			CapturedAt: at, Payload: []byte("out\n"),
		}); err != nil {
			t.Fatalf("AppendWorkflowStepLog: %v", err)
		}
	}

	got, err := j.WorkflowStepLogLastSeq(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowStepLogLastSeq: %v", err)
	}
	if got != 3 {
		t.Errorf("the last sequence is %d, want 3", got)
	}

	// It is per RUN: another run's output does not move this one's
	// cursor, because the cursor a follower holds is per run.
	commitTestPlan(t, j, "run-2")
	if got, err := j.WorkflowStepLogLastSeq(ctx, "run-2"); err != nil || got != 0 {
		t.Errorf("another run's last sequence is %d, %v; want 0", got, err)
	}
}
