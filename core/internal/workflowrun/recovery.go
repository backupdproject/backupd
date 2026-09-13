package workflowrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/state"
	"github.com/backupdproject/backupd/core/internal/workflow"
)

// Startup reconciliation, resume-cleanup and acknowledgement: the three
// things that can happen to a run this process did not see the end of.
//
// # Nothing replays
//
// #811 says it twice and it is the rule the rest of this file exists to
// keep: a daemon that comes back up does not re-run anybody's hooks.
// It cannot know what the interrupted step got as far as doing, so
// running the "after" stage unprompted would be unwinding a state nobody
// has looked at, and running the "before" stage again would be doing the
// side effect twice. What it does instead is write down that the run
// needs recovery, block the backup set, and say so where an operator will
// see it.
//
// # Erring toward recovery_required is a property of the transition graph
//
// The reconciliation is a pure function of the rows it found
// (reconcileRun), and for every non-terminal run it produces the same
// three conclusions: a step that was running is interrupted, a step that
// was pending was never run, and any scope whose obligation is not
// settled requires recovery. The global scope's obligation is committed
// eligible in the same transaction that creates the run, so EVERY
// interrupted run has at least one unsettled scope -- which is why "a
// crash between any two durable transitions errs toward
// recovery_required" holds wherever the crash landed, rather than for the
// positions somebody thought of.
//
// # Why the refusal is in memory and the truth is on disk
//
// The durable answer to "does this set have an unresolved interruption"
// is a query. Asking it on every run would put a journal read on the
// zero-hook path, which #811 requires to stay as cheap as it is today. So
// Reconcile loads the answer once -- at the only moment it can change
// without this process doing it, which is a restart -- and every write
// that changes it afterwards goes through this file, under the same lock.
// Run refuses until Reconcile has run, so the map is never merely empty
// by omission.

// RecoveryHold is one reason a backup set is blocked: a run that was
// interrupted, and the scope of it that cannot be accounted for.
type RecoveryHold struct {
	RunID       string
	BackupSetID model.BackupSetID

	// Scope is the nested scope whose cleanup is outstanding.
	Scope workflow.Scope

	// EnteredAt is when that scope was entered, which is the answer to
	// the first question an operator asks: how long has this machine
	// been left like this.
	EnteredAt time.Time

	// SpoolRef is the run's retained script spool, so the operator
	// surfaces can say what a resume-cleanup would execute.
	SpoolRef string
}

// ReconcileReport is what one startup pass found.
type ReconcileReport struct {
	// Interrupted names the runs this pass moved to recovery_required.
	Interrupted []string

	// Holds is every outstanding hold after the pass, including the ones
	// that were already recorded before this process started.
	Holds []RecoveryHold
}

// Reconcile brings the journal into line with the fact that this process
// has just started, and returns what is outstanding.
//
// It is idempotent: a run it has already moved to recovery_required is no
// longer in a non-terminal state, so a second pass finds nothing to do
// and reports the same holds.
func (e *Engine) Reconcile(ctx context.Context, at time.Time) (ReconcileReport, error) {
	if e.Store == nil {
		return ReconcileReport{}, errors.New("workflowrun: this engine has no journal to reconcile")
	}
	if at.IsZero() {
		at = e.clock()
	}

	open, err := e.Store.WorkflowRunsInStates(ctx, openRunStates()...)
	if err != nil {
		return ReconcileReport{}, err
	}

	var report ReconcileReport

	for _, run := range open {
		// A run whose set is LOCKED is a run this process is executing
		// right now, not one it found lying about. Reconcile is a
		// startup pass by design, but nothing structural stops it being
		// called again -- a health endpoint, a second daemon component,
		// a test -- and a pass that reconciled a live run would mark
		// its in-flight step interrupted, move its obligations to
		// recovery_required, and block the set underneath a backup that
		// is still running. Skipping it is the only safe answer: the
		// run is going to record its own outcome, and if it dies first
		// the NEXT startup finds it with no lock held.
		if e.setIsLocked(run.BackupSetID) {
			continue
		}

		steps, err := e.Store.WorkflowSteps(ctx, run.RunID)
		if err != nil {
			return ReconcileReport{}, err
		}
		obligations, err := e.Store.WorkflowCleanupObligations(ctx, run.RunID)
		if err != nil {
			return ReconcileReport{}, err
		}

		rec := reconcileRun(run, steps, obligations, at)

		if err := e.Store.ApplyWorkflowReconciliation(ctx, rec); err != nil {
			return ReconcileReport{}, fmt.Errorf("workflowrun: reconciling interrupted workflow run %q: %w", run.RunID, err)
		}

		// Reported as interrupted only when that is what it IS. The
		// other branch of reconcileRun is a run whose work was
		// demonstrably finished and whose summary row was missing --
		// which this pass FINALIZED rather than interrupted, and
		// reporting it as an interruption would put a run nobody needs
		// to act on into the one list an operator reads to find out what
		// broke.
		if rec.Run.To == workflow.StateRecoveryRequired {
			report.Interrupted = append(report.Interrupted, run.RunID)
		}
	}

	if err := e.reconcileRecoveryAxis(ctx, at); err != nil {
		return ReconcileReport{}, err
	}

	holds, err := e.loadHolds(ctx)
	if err != nil {
		return ReconcileReport{}, err
	}
	report.Holds = holds

	e.mu.Lock()
	e.reconciled = true
	e.mu.Unlock()

	// Warned for EVERY outstanding hold, not only the ones this pass
	// created. A health warning is raised while the condition holds, and
	// the condition here is "a machine may still be quiesced" -- which is
	// as true on the third restart as it was on the first, and is
	// precisely when an operator is most likely to be looking.
	e.warn(ctx, holds)

	return report, nil
}

// setIsLocked reports whether a backup set has a run in flight in THIS
// process, which is what makes a non-terminal run row a live run rather
// than a leftover.
//
// It takes the set id as the journal spells it, because that is what the
// row carries; an id the journal holds and this package cannot parse is
// not a set anything here holds a lock for, and it is not this function's
// business to refuse it (the reconciliation will, or the run will).
func (e *Engine) setIsLocked(setID string) bool {
	set, err := model.ParseBackupSetID(setID)
	if err != nil {
		return false
	}

	_, busy := e.locks().Holder(set)

	return busy
}

// reconcileRecoveryAxis brings the runs whose recovery is unsettled into
// line with their own obligations, in whichever of the two directions the
// rows justify.
//
// Both directions exist because settling a recovery, and declaring one,
// take TWO writes and nothing can make them one: the obligations move
// first and the run's axis second, because the second write is the one
// that reads the first (ResolveWorkflowRecovery refuses while anything
// is unsettled). A process that dies in between -- or a journal that
// refuses one of the pair -- leaves a run and its scopes disagreeing,
// and recovery_required is excluded from openRunStates precisely because
// it is the state the reconciliation WRITES, so the ordinary pass never
// looks at these again.
//
//   - every scope settled and the run still saying recovery_required:
//     an acknowledgement (or a resume) that got its obligations written
//     and died before the resolution. Nothing is owed and nobody is
//     needed, and leaving it would keep a backup set blocked on a hold
//     that no longer exists for as long as the deployment lives. So the
//     resolution is finished, in the direction the rows already justify:
//     a scope that failed leaves the run cleanup_failed rather than
//     recovered, which is the verdict the resume itself would have
//     written.
//   - the run saying recovery_required with a scope still at eligible or
//     running: the other half, and the more dangerous one. That
//     obligation is unsettled, so something IS owed -- but it is not in
//     the state the per-set refusal and the health warning scan for
//     (ObligationsRequiringRecovery), so the set would not be blocked
//     and no surface would mention it. The scope is moved to
//     recovery_required, which is what the run already says about
//     itself.
func (e *Engine) reconcileRecoveryAxis(ctx context.Context, at time.Time) error {
	runs, err := e.Store.WorkflowRunsRequiringRecovery(ctx)
	if err != nil {
		return err
	}

	for _, run := range runs {
		// A live resume-cleanup holds the set's lock and is in the
		// middle of exactly this sequence. Leaving it alone is the same
		// rule the open-run pass follows.
		if e.setIsLocked(run.BackupSetID) {
			continue
		}

		obligations, err := e.Store.WorkflowCleanupObligations(ctx, run.RunID)
		if err != nil {
			return err
		}
		if len(obligations) == 0 {
			continue
		}

		settled, failed := true, false
		for _, o := range obligations {
			if !o.State.Settled() {
				settled = false
			}
			if o.State == workflow.ObligationFailed {
				failed = true
			}
		}

		if !settled {
			if err := e.markScopesOutstanding(ctx, run, obligations, at); err != nil {
				return err
			}

			continue
		}

		to := workflow.StateRecovered
		if failed {
			to = workflow.StateCleanupFailed
		}

		if err := e.Store.ResolveWorkflowRecovery(ctx, run.RunID, to, at); err != nil {
			return fmt.Errorf("workflowrun: settling the recovery of workflow run %q, whose every scope is accounted for: %w", run.RunID, err)
		}

		if err := e.settleStatuses(ctx, run.RunID, failed); err != nil {
			return err
		}
	}

	return nil
}

// markScopesOutstanding moves a scope that is owed but not VISIBLY owed
// into recovery_required. See reconcileRecoveryAxis's second case.
func (e *Engine) markScopesOutstanding(ctx context.Context, run state.WorkflowRun, obligations []workflow.CleanupObligation, at time.Time) error {
	for _, o := range obligations {
		if o.State.Settled() || o.State.RequiresRecovery() {
			continue
		}

		if err := e.Store.AdvanceWorkflowCleanupObligation(ctx, state.WorkflowObligationAdvance{
			RunID: run.RunID,
			Scope: o.Scope,
			To:    workflow.ObligationRecoveryRequired,
			At:    at,
		}); err != nil {
			return fmt.Errorf("workflowrun: recording that the %s scope of workflow run %q is outstanding: %w", o.Scope, run.RunID, err)
		}
	}

	return nil
}

// settleStatuses closes the three-status axes of a run whose recovery has
// just been resolved.
//
// ResolveWorkflowRecovery writes the run's STATE and its recovery axis
// and deliberately nothing else -- it is the one call that may settle a
// recovery, and widening it to the statuses would widen what that
// privilege covers. So the statuses are written here, and they have to
// be: a run left at workflow_status=running and cleanup_status=unknown is
// a terminal row that every history surface reads as still in flight.
//
// The workflow's verdict is failed either way. A run that needed a
// recovery is a run that did not do what it set out to do, whoever
// finished it.
func (e *Engine) settleStatuses(ctx context.Context, runID string, cleanupFailed bool) error {
	cleanup := workflow.StatusSuccess
	if cleanupFailed {
		cleanup = workflow.StatusFailed
	}

	if err := e.Store.AdvanceWorkflowRun(ctx, runID, state.WorkflowRunAdvance{
		CleanupStatus:  cleanup,
		WorkflowStatus: workflow.StatusFailed,
	}); err != nil {
		return fmt.Errorf("workflowrun: recording the statuses of resolved workflow run %q: %w", runID, err)
	}

	return nil
}

// openRunStates is the set of run states that mean "this process was in
// the middle of something".
//
// Derived from the vocabulary rather than listed, so a run state added
// later is enrolled or not by State.Terminal -- the same decision the
// spool-retention rule and the partial index are made of -- instead of by
// somebody remembering this line. StateRecoveryRequired is excluded
// because it is the state this pass WRITES: a run already there has been
// reconciled and must not be reconciled again.
func openRunStates() []workflow.State {
	var out []workflow.State
	for _, s := range workflow.RunStates() {
		if s.Terminal() || s == workflow.StateRecoveryRequired {
			continue
		}
		out = append(out, s)
	}

	return out
}

// reconcileRun is the reconciliation DECISION, as a pure function of what
// was found.
//
// Pure on purpose: it is the thing the crash-injection suite calls with
// the rows a killed run really left behind, and a decision tangled up
// with the writes that apply it could only be tested by crashing and
// looking at the result, which proves the pair rather than the rule.
//
// There are exactly two answers, and which one applies is decided by the
// obligations rather than by where the crash landed:
//
//   - some scope is not accounted for: the run requires recovery. Every
//     step that was running becomes interrupted, every step that never
//     started becomes skipped, and every unsettled scope becomes
//     recovery_required. This is the case that blocks the backup set.
//   - every scope is settled: the run is OVER, and the only thing
//     missing is the row that says so. That is not a recovery, and
//     treating it as one would block a set whose machine was
//     demonstrably put back -- the obligations say so -- because the
//     process happened to die on its last write.
//
// The second case is safe because of what makes it reachable: a scope
// only settles when its cleanup reached a terminal recorded state, so a
// run with every scope settled has no interrupted step anywhere in it.
func reconcileRun(run state.WorkflowRun, steps []state.WorkflowStep, obligations []workflow.CleanupObligation, at time.Time) state.WorkflowReconciliation {
	rec := state.WorkflowReconciliation{RunID: run.RunID}

	outstanding := false
	for _, o := range obligations {
		if !o.State.Settled() {
			outstanding = true
		}
	}

	if !outstanding {
		rec.Run = finishedRunAdvance(run, steps, obligations, at)

		return rec
	}

	rec.Run = state.WorkflowRunAdvance{
		To:            workflow.StateRecoveryRequired,
		RecoveryState: workflow.RecoveryRequired,
	}

	// A backup this process was in the MIDDLE of is not still running,
	// whatever the row says. Nobody is going to observe its outcome, so
	// it goes to unknown -- the vocabulary's "nobody knows yet", which
	// is exactly the fact -- rather than being left at running, which
	// claims work is in flight in a process that no longer exists and
	// which a resumed "after" hook would be told through
	// BACKUPD_BACKUP_STATUS. A hook deciding whether to roll something
	// back on the strength of "the backup is still going" is the concrete
	// damage.
	if workflow.Status(run.BackupStatus) == workflow.StatusRunning {
		rec.Run.BackupStatus = workflow.StatusUnknown
	}

	outstandingScope := map[workflow.Scope]bool{}
	for _, o := range obligations {
		if !o.State.Settled() {
			outstandingScope[o.Scope] = true
		}
	}

	for _, s := range steps {
		switch workflow.State(s.State) {
		case workflow.StateRunning:
			// Nobody observed this step's exit status and nobody ever
			// will. That is not a failure -- a failure is a script that
			// decided something -- it is an outcome that does not exist,
			// and it is the fact that makes the scope unaccountable
			// rather than merely unfinished.
			rec.Steps = append(rec.Steps, state.WorkflowStepReconciliation{
				StepID: s.StepID, State: workflow.StateInterrupted, At: at,
			})
		case workflow.StatePending:
			// A "before" step that never started is over: the run is
			// not going to continue, and nothing of that step happened.
			// Recorded as skipped so the plan read back afterwards
			// accounts for every step it declared.
			//
			// An "after" step of an OUTSTANDING scope is the opposite.
			// It is still OWED -- it is the whole thing a
			// resume-cleanup exists to run -- so it stays pending.
			// Marking it skipped would make it terminal, and a terminal
			// step cannot be run: the recovery would have nothing left
			// to do and would report a machine as put back that nothing
			// had touched.
			if workflow.Phase(s.Phase) == workflow.PhaseAfter && outstandingScope[workflow.Scope(s.Scope)] {
				continue
			}

			rec.Steps = append(rec.Steps, state.WorkflowStepReconciliation{
				StepID: s.StepID, State: workflow.StateSkipped, At: at,
			})
		}
	}

	for _, o := range obligations {
		if o.State.Settled() || o.State.RequiresRecovery() {
			continue
		}

		rec.Obligations = append(rec.Obligations, state.WorkflowObligationAdvance{
			RunID: run.RunID,
			Scope: o.Scope,
			To:    workflow.ObligationRecoveryRequired,
			At:    at,
		})
	}

	return rec
}

// finishedRunAdvance is the verdict for a run whose work is demonstrably
// over and whose summary row never landed.
//
// It is derived from the rows and nothing else, which is the one thing it
// can be: the engine's own finish() has facts these rows do not carry --
// whether the run was cancelled by an operator or timed out -- and those
// facts died with the process. So this is deliberately narrower than
// finish(): it decides WHETHER the run succeeded, which the steps do
// answer, and does not invent a reason it ended that nobody recorded. A
// run that was cancelled at its last write therefore reconciles to
// "failed" rather than to "canceled", which understates the story and
// never overstates the outcome.
func finishedRunAdvance(run state.WorkflowRun, steps []state.WorkflowStep, obligations []workflow.CleanupObligation, at time.Time) state.WorkflowRunAdvance {
	cleanupFailed := false
	anyFailed := workflow.Status(run.BackupStatus) == workflow.StatusFailed

	for _, s := range steps {
		switch workflow.State(s.State) {
		case workflow.StateSuccess, workflow.StateSkipped:
		default:
			anyFailed = true
			if workflow.Phase(s.Phase) == workflow.PhaseAfter {
				cleanupFailed = true
			}
		}
	}

	plannedCleanup, eligibleCleanup := 0, 0
	for _, s := range steps {
		if workflow.Phase(s.Phase) != workflow.PhaseAfter {
			continue
		}
		plannedCleanup++

		for _, o := range obligations {
			if o.Scope == workflow.Scope(s.Scope) && o.State != workflow.ObligationNeverEligible {
				eligibleCleanup++
			}
		}
	}

	for _, o := range obligations {
		if o.State == workflow.ObligationFailed {
			cleanupFailed = true
			anyFailed = true
		}
	}

	cleanupStatus := workflow.StatusSuccess
	switch {
	case cleanupFailed:
		cleanupStatus = workflow.StatusFailed
	case plannedCleanup > 0 && eligibleCleanup == 0:
		cleanupStatus = workflow.StatusSkipped
	}

	st := workflow.StateSuccess
	workflowStatus := workflow.StatusSuccess
	switch {
	case cleanupFailed:
		st = workflow.StateCleanupFailed
		workflowStatus = workflow.StatusFailed
	case anyFailed:
		st = workflow.StateFailed
		workflowStatus = workflow.StatusFailed
	}

	finished := at

	adv := state.WorkflowRunAdvance{
		To:             st,
		CleanupStatus:  cleanupStatus,
		WorkflowStatus: workflowStatus,
		FinishedAt:     &finished,
	}

	// A backup row still reading "running" belongs to a process that no
	// longer exists, on either branch of the reconciliation. Nobody is
	// going to observe its outcome, so it goes to the vocabulary's
	// "nobody knows yet" rather than being left claiming work is in
	// flight -- which is what a history surface would show forever and
	// what a hook would be told through BACKUPD_BACKUP_STATUS.
	if workflow.Status(run.BackupStatus) == workflow.StatusRunning {
		adv.BackupStatus = workflow.StatusUnknown
	}

	return adv
}

// loadHolds reads every outstanding hold out of the journal and installs
// it as this engine's refusal set.
//
// It asks for the OBLIGATIONS rather than for the runs, which is the
// query the partial index was built for and the one that costs nothing
// on the overwhelmingly common answer: a deployment with nothing
// outstanding reads zero rows and makes no second query. The run is
// looked up only for a set that is actually blocked, and only once per
// run, because the hold has to be able to say which spool a
// resume-cleanup would execute.
func (e *Engine) loadHolds(ctx context.Context) ([]RecoveryHold, error) {
	obligations, err := e.Store.ObligationsRequiringRecovery(ctx)
	if err != nil {
		return nil, err
	}

	var holds []RecoveryHold
	bySet := map[model.BackupSetID][]RecoveryHold{}
	spools := map[string]string{}

	for _, o := range obligations {
		spool, known := spools[o.RunID]
		if !known {
			run, err := e.Store.WorkflowRun(ctx, o.RunID)
			if err != nil {
				return nil, err
			}
			spool = run.ScriptSpoolRef
			spools[o.RunID] = spool
		}

		hold := RecoveryHold{
			RunID:       o.RunID,
			BackupSetID: o.BackupSetID,
			Scope:       o.Scope,
			SpoolRef:    spool,
		}
		if o.EnteredAt != nil {
			hold.EnteredAt = *o.EnteredAt
		}

		holds = append(holds, hold)
		bySet[o.BackupSetID] = append(bySet[o.BackupSetID], hold)
	}

	e.mu.Lock()
	e.recoveryFor = bySet
	e.mu.Unlock()

	return holds, nil
}

// RecoveryHolds returns every outstanding hold, which is what a health
// report and an activity warning are built from.
//
// Sorted, by run and then by scope. The holds are kept in a map keyed by
// backup set, and ranging a map is a different order every call: a health
// endpoint that reordered its own list between two scrapes, or an
// activity warning whose two lines swapped, is a surface an operator
// cannot diff -- and diffing two health reports is how somebody finds out
// whether anything changed.
func (e *Engine) RecoveryHolds() []RecoveryHold {
	e.mu.Lock()
	defer e.mu.Unlock()

	var out []RecoveryHold
	for _, holds := range e.recoveryFor {
		out = append(out, holds...)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].RunID != out[j].RunID {
			return out[i].RunID < out[j].RunID
		}

		return out[i].Scope < out[j].Scope
	})

	return out
}

// SuspendedBackupSets returns the sets whose scheduled and manual runs
// are being refused, in a stable order for RecoveryHolds' reason.
//
// It is the read a scheduler makes before a tick and a manual submission
// makes before it starts: both are refused for the same reason and must
// not be able to disagree about which sets are affected.
func (e *Engine) SuspendedBackupSets() []model.BackupSetID {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make([]model.BackupSetID, 0, len(e.recoveryFor))
	for set := range e.recoveryFor {
		out = append(out, set)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })

	return out
}

// checkNotBlocked is the refusal itself: an ordinary run of a set with an
// unresolved interruption does not start.
//
// It names the run, the scope and when that scope was entered, because
// the operator's next act is either to resume the cleanup or to
// acknowledge it, and both need to know which run they are talking about.
func (e *Engine) checkNotBlocked(set model.BackupSetID) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	holds := e.recoveryFor[set]
	if len(holds) == 0 {
		return nil
	}

	var scopes []string
	for _, h := range holds {
		scopes = append(scopes, string(h.Scope))
	}

	return fmt.Errorf("%w: workflow run %q left the %s scope of %s unaccounted for at %s. Resume its cleanup, or acknowledge it with a reason; a new run would take a backup over a machine this product cannot say was put back",
		ErrRecoveryRequired, holds[0].RunID, strings.Join(scopes, " and "), set,
		holds[0].EnteredAt.UTC().Format(time.RFC3339))
}

// warn raises the visible health warning for an interrupted run.
//
// It reuses the alert event the halts model already uses rather than a
// new one, and it deliberately does NOT write a backup_set_halts row: a
// halt is a standing statement that the manager could not CONNECT to a
// set, its reasons are a closed CHECK constraint in the schema, and
// widening that costs a table rebuild (0002 and 0006 are what it cost
// last time). The durable statement here is the run's own
// recovery_required row, which has a partial index built for exactly this
// query, and presence is the claim in both models.
func (e *Engine) warn(ctx context.Context, holds []RecoveryHold) {
	if e.Logger == nil {
		return
	}

	for _, h := range holds {
		e.Logger.Alert(ctx, "workflow_recovery_required", h.BackupSetID.String(),
			fmt.Sprintf("workflow run %q was interrupted with its %s cleanup outstanding since %s; scheduled and manual runs of this backup set are refused until the cleanup is resumed or acknowledged",
				h.RunID, h.Scope, h.EnteredAt.UTC().Format(time.RFC3339)))
	}
}

// ResumeCleanup runs the eligible "after" stages of an interrupted run,
// out of that run's own captured bytes.
//
// Everything about what it executes comes from the journal and the spool,
// never from today's configuration: the plan is rebuilt with
// workflow.RecoverPlan (which re-verifies every script against the sha256
// recorded at snapshot time), the ordinary environment is the one the run
// was PLANNED with, and the secrets are re-resolved from the references
// that plan carried. An operator who edited /workflows or config.yaml
// while the daemon was down has changed nothing about this.
//
// The hooks are told BACKUPD_RECOVERY=1 and
// BACKUPD_CLEANUP_REASON=interrupted_run, because unwinding after a crash
// is a different job from unwinding after a run: the state a script finds
// may be half-applied, and a script that wants to be careful about that
// needs to be able to tell.
func (e *Engine) ResumeCleanup(ctx context.Context, runID string) (RunResult, error) {
	if e.Store == nil {
		return RunResult{}, errors.New("workflowrun: this engine has no journal")
	}

	run, err := e.Store.WorkflowRun(ctx, runID)
	if err != nil {
		return RunResult{}, err
	}
	if workflow.RecoveryState(run.RecoveryState) != workflow.RecoveryRequired {
		return RunResult{}, fmt.Errorf("workflowrun: workflow run %q has recovery state %q, and only a run requiring recovery has a cleanup to resume",
			runID, run.RecoveryState)
	}

	set, err := model.ParseBackupSetID(run.BackupSetID)
	if err != nil {
		return RunResult{}, fmt.Errorf("workflowrun: workflow run %q names backup set %q: %w", runID, run.BackupSetID, err)
	}

	release, err := e.locks().Acquire(set, runID+"/resume")
	if err != nil {
		return RunResult{}, err
	}
	defer release()

	plan, err := e.Store.RecoverWorkflowPlan(ctx, runID)
	if err != nil {
		return RunResult{}, err
	}

	// The facts the run was PLANNED with, out of the journal rather than
	// out of today's configuration, for the reason the environment comes
	// from there: a recovery finishes the run that was planned. Without
	// them an `umount "$BACKUPD_SOURCE_PATH"` in a recovery hook runs
	// with an empty variable.
	facts, err := e.Store.WorkflowRunFacts(ctx, runID)
	if err != nil {
		return RunResult{}, err
	}

	obligations, err := e.Store.WorkflowCleanupObligations(ctx, runID)
	if err != nil {
		return RunResult{}, err
	}
	steps, err := e.Store.WorkflowSteps(ctx, runID)
	if err != nil {
		return RunResult{}, err
	}

	r, err := e.resumeRunner(ctx, run, plan, set, facts, steps)
	if err != nil {
		return RunResult{}, err
	}

	if err := e.Store.AdvanceWorkflowRun(ctx, runID, state.WorkflowRunAdvance{
		To:            workflow.StateCleanupRunning,
		RecoveryState: workflow.RecoveryInProgress,
		CleanupStatus: workflow.StatusRunning,
	}); err != nil {
		return RunResult{}, err
	}

	timeout := e.CleanupTimeout
	if timeout <= 0 {
		timeout = DefaultCleanupTimeout
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	// Unwind order, the same as a run's: the backup-set scope first.
	for _, scope := range []workflow.Scope{workflow.ScopeSet, workflow.ScopeGlobal} {
		o, found := obligationFor(obligations, scope)
		if !found || !o.State.RequiresRecovery() {
			continue
		}

		r.resumeScope(cleanupCtx, scope, steps)
	}

	return e.settleRecovery(ctx, r, run)
}

// resumeRunner is a runner wired for a recovery: the recovered plan, the
// run's own recorded statuses rather than fresh ones, the facts it was
// planned with, and its steps as the journal holds them.
//
// The backup status in particular is READ BACK rather than recomputed: an
// "after" hook running under a recovery is told what the backup actually
// did, which for an interrupted run is usually that it never ran.
func (e *Engine) resumeRunner(ctx context.Context, run state.WorkflowRun, plan workflow.Plan, set model.BackupSetID, facts map[string]string, recorded []state.WorkflowStep) (*runner, error) {
	r := &runner{
		engine:         e,
		req:            RunRequest{BackupSetID: set, Facts: facts},
		plan:           plan,
		set:            set,
		runID:          run.RunID,
		steps:          plan.Steps(),
		startedAt:      run.StartedAt,
		backupStatus:   workflow.Status(run.BackupStatus),
		cleanupStatus:  workflow.StatusRunning,
		workflowStatus: workflow.Status(run.WorkflowStatus),
		entered:        map[workflow.Scope]bool{},
		byID:           map[string]int{},
		jctx:           context.WithoutCancel(ctx),
		recovering:     true,
	}

	// The recorder carries on from where the interrupted run's log got
	// to. Starting at zero collides with the rows that run already
	// wrote, on UNIQUE (run_id, seq), at the FIRST byte a recovery hook
	// produces -- and the engine reads a failed log write as a step
	// whose output could not be recorded, fails it, and reports the
	// cleanup of the run somebody is recovering as failed because of a
	// counter.
	lastSeq, err := e.Store.WorkflowStepLogLastSeq(ctx, run.RunID)
	if err != nil {
		return nil, err
	}

	r.logs = &logRecorder{
		store:  e.Store,
		broker: e.Logs,
		runID:  run.RunID,
		now:    e.clock,
		bound:  e.StepOutputBytes,
		ctx:    context.WithoutCancel(ctx),
		seq:    lastSeq,
	}

	// The steps' states come from the JOURNAL, not from "pending". A
	// resumed run's result is read by the same surfaces a run's is, and
	// seeding every step as pending contradicts the rows: the before
	// steps of an interrupted run are already skipped, interrupted or
	// successful, and reporting them as pending says work is owed that
	// nothing is ever going to do.
	states := map[string]workflow.State{}
	for _, s := range recorded {
		states[s.StepID] = workflow.State(s.State)
	}

	for i, s := range r.steps {
		r.byID[s.ID] = i

		st := states[s.ID]
		if st == "" {
			st = workflow.StatePending
		}

		r.results = append(r.results, StepResult{
			StepID:     s.ID,
			ScriptName: s.ScriptName,
			Scope:      s.Scope,
			Phase:      s.Phase,
			Target:     s.Target,
			State:      st,
		})
	}

	return r, nil
}

// resumeScope runs one scope's outstanding "after" steps and decides what
// the obligation becomes.
//
// The three outcomes are the point:
//
//   - every step of the scope reached success: the obligation is
//     discharged (ObligationSuccess), and the interruption has been dealt
//     with;
//   - a step RAN and failed: the obligation is ObligationFailed. It is
//     settled -- the cleanup was attempted and what happened is on the
//     record -- and the run ends as cleanup_failed, which is visible and
//     final rather than blocking;
//   - a step cannot be run at all because it was INTERRUPTED, so its own
//     outcome is unknown: the obligation goes back to recovery_required.
//     This product will not re-run a hook whose first attempt may have
//     half-applied something, and it will not claim a scope is clean
//     because it ran the steps it could. The only exit left is a person,
//     which is what acknowledgement is for.
//
// Both obligation writes go on r.jctx rather than on the cleanup's
// bounded context, and the pair matters. The bound exists to stop a hook
// running forever; the obligation is this product's own record of what
// that hook was for. A resume that outlived the bound -- a database that
// took three minutes to come back -- would otherwise fail BOTH writes:
// the scope would be left at recovery_required in the journal while this
// process believed it had run the cleanup, and the run would be reported
// as recovered in memory with the hold still on disk. Execution is
// bounded; bookkeeping is not.
func (r *runner) resumeScope(ctx context.Context, scope workflow.Scope, recorded []state.WorkflowStep) {
	r.entered[scope] = true

	if err := r.engine.Store.AdvanceWorkflowCleanupObligation(r.jctx, state.WorkflowObligationAdvance{
		RunID: r.runID,
		Scope: scope,
		To:    workflow.ObligationRunning,
		At:    r.engine.clock(),
	}); err != nil {
		r.fail("", err)

		return
	}

	states := map[string]workflow.State{}
	for _, s := range recorded {
		states[s.StepID] = workflow.State(s.State)
	}

	ok := true
	unaccountable := false

	for _, s := range r.steps {
		if s.Scope != scope || s.Phase != workflow.PhaseAfter {
			continue
		}

		switch states[s.ID] {
		case workflow.StatePending:
			if !r.runStep(ctx, s, true) {
				ok = false
			}
		case workflow.StateSuccess:
			// Already done before the interruption. Re-running an undo
			// that already succeeded is a side effect nobody asked for.
			r.results[r.byID[s.ID]].State = workflow.StateSuccess
		case workflow.StateInterrupted:
			ok = false
			unaccountable = true
			r.results[r.byID[s.ID]].State = workflow.StateInterrupted
		default:
			// Failed, timed out, cancelled or skipped before the
			// interruption: a terminal record, and the graph refuses to
			// re-open it.
			ok = false
			r.results[r.byID[s.ID]].State = states[s.ID]
			r.cleanupBad = true
		}
	}

	to := workflow.ObligationSuccess
	switch {
	case unaccountable:
		to = workflow.ObligationRecoveryRequired
	case !ok:
		to = workflow.ObligationFailed
		r.cleanupBad = true
	}

	if err := r.engine.Store.AdvanceWorkflowCleanupObligation(r.jctx, state.WorkflowObligationAdvance{
		RunID: r.runID,
		Scope: scope,
		To:    to,
		At:    r.engine.clock(),
	}); err != nil {
		r.fail("", err)
	}
}

// settleRecovery closes a resumed run, or leaves it blocked.
//
// ResolveWorkflowRecovery refuses while any obligation is unsettled, so
// this cannot talk the journal out of a recovery it still needs: a scope
// that came back recovery_required leaves the run exactly where it was,
// the set still refused, and the result says so.
//
// Every write here is on r.jctx, for resumeScope's reason: a resume that
// outlived the cleanup bound must still record what it did, or the
// obligation it advanced in memory and the row on disk disagree
// permanently.
func (e *Engine) settleRecovery(ctx context.Context, r *runner, run state.WorkflowRun) (RunResult, error) {
	at := e.clock()

	to := workflow.StateRecovered
	cleanupStatus := workflow.StatusSuccess
	if r.cleanupBad {
		to = workflow.StateCleanupFailed
		cleanupStatus = workflow.StatusFailed
	}

	res := RunResult{
		RunID:          run.RunID,
		BackupStatus:   workflow.Status(run.BackupStatus),
		WorkflowStatus: workflow.StatusFailed,
		CleanupStatus:  cleanupStatus,
		ScriptCount:    len(r.steps),
		Steps:          r.results,
		Bypassed:       run.Bypassed,
		FailedStep:     r.failedStep,
	}

	err := e.Store.ResolveWorkflowRecovery(r.jctx, run.RunID, to, at)
	if err != nil {
		if !errors.Is(err, state.ErrRecoveryOutstanding) {
			return res, err
		}

		res.State = workflow.StateRecoveryRequired
		res.RecoveryOutstanding = true

		// The cleanup did not succeed, whatever the scopes this resume
		// COULD run did: a scope is still owed, so reporting success
		// here would put "cleanup: success" beside "state:
		// recovery_required" on the same row.
		res.CleanupStatus = workflow.StatusFailed
		cleanupStatus = workflow.StatusFailed

		// The run is put BACK to recovery_required, durably, before
		// anything else. ResumeCleanup moved it to cleanup_running with
		// recovery in_progress on the way in, and an incomplete resume
		// that left it there would be a run no second resume can touch:
		// ResumeCleanup refuses anything whose recovery state is not
		// "required", so the only way out would be a restart. That is a
		// backup set blocked until somebody reboots a daemon, for a
		// scope an operator was in the middle of dealing with.
		//
		// cleanup_running -> recovery_required and in_progress ->
		// required are both legal (runTransitions, checkRecoveryRaise):
		// this is the same direction a crash here would have been
		// reconciled in.
		if err := e.Store.AdvanceWorkflowRun(r.jctx, run.RunID, state.WorkflowRunAdvance{
			To:             workflow.StateRecoveryRequired,
			RecoveryState:  workflow.RecoveryRequired,
			CleanupStatus:  cleanupStatus,
			WorkflowStatus: workflow.StatusFailed,
		}); err != nil {
			return res, err
		}

		if _, err := e.loadHolds(r.jctx); err != nil {
			return res, err
		}

		return res, nil
	}

	if err := e.Store.AdvanceWorkflowRun(r.jctx, run.RunID, state.WorkflowRunAdvance{
		CleanupStatus:  cleanupStatus,
		WorkflowStatus: workflow.StatusFailed,
	}); err != nil {
		return res, err
	}

	res.State = to

	if _, err := e.loadHolds(r.jctx); err != nil {
		return res, err
	}

	return res, nil
}

func obligationFor(obligations []workflow.CleanupObligation, scope workflow.Scope) (workflow.CleanupObligation, bool) {
	for _, o := range obligations {
		if o.Scope == scope {
			return o, true
		}
	}

	return workflow.CleanupObligation{}, false
}

// Acknowledgement is a person taking responsibility for a scope this
// product cannot account for.
type Acknowledgement struct {
	// By is who asked. Whatever the surface that took the
	// acknowledgement knows about them; never a credential.
	By string

	// Reason is the audit record, and it is required. An obligation that
	// can be cleared without saying why is one that gets cleared by a
	// script.
	Reason string
}

// AcknowledgeRecovery records that an operator has dealt with an
// interrupted run by hand, and unblocks the backup set.
//
// It is the ONLY exit from recovery_required that is not a cleanup run,
// and everything about it is built so that it cannot be taken by
// accident: the reason is required by the domain type
// (workflow.CleanupObligation.Validate refuses a blank or whitespace
// one), the transition is refused for any scope that is not actually
// outstanding, and the run's recovery axis is settled by the journal only
// after every scope has reached a settled state.
func (e *Engine) AcknowledgeRecovery(ctx context.Context, runID string, ack Acknowledgement) error {
	if e.Store == nil {
		return errors.New("workflowrun: this engine has no journal")
	}
	if strings.TrimSpace(ack.Reason) == "" {
		return errors.New("workflowrun: acknowledging an interrupted run needs a reason, because the reason is the whole difference between an operator who checked the machine and a cleared alarm nobody can explain")
	}
	if strings.TrimSpace(ack.By) == "" {
		return errors.New("workflowrun: acknowledging an interrupted run needs to record who did it")
	}

	run, err := e.Store.WorkflowRun(ctx, runID)
	if err != nil {
		return err
	}
	if workflow.RecoveryState(run.RecoveryState).Settled() {
		return fmt.Errorf("workflowrun: workflow run %q has recovery state %q; there is nothing outstanding to acknowledge", runID, run.RecoveryState)
	}

	set, err := model.ParseBackupSetID(run.BackupSetID)
	if err != nil {
		return fmt.Errorf("workflowrun: workflow run %q names backup set %q: %w", runID, run.BackupSetID, err)
	}

	// The same lock a resume takes, for the same reason. An
	// acknowledgement and a resume-cleanup of one run are two callers
	// walking the same obligations in opposite directions: the resume
	// moves a scope to running and then to its outcome, the
	// acknowledgement moves every unsettled scope to acknowledged, and
	// interleaved they produce a scope acknowledged while its hook is
	// executing -- an audit record saying a person checked a machine
	// that this product was at that moment still unwinding. The lock
	// makes the two serial, and it refuses rather than queues, so an
	// operator is told the resume is in flight.
	release, err := e.locks().Acquire(set, runID+"/acknowledge")
	if err != nil {
		return err
	}
	defer release()

	obligations, err := e.Store.WorkflowCleanupObligations(ctx, runID)
	if err != nil {
		return err
	}

	at := e.clock()

	for _, o := range obligations {
		if o.State.Settled() {
			continue
		}

		if err := e.Store.AdvanceWorkflowCleanupObligation(ctx, state.WorkflowObligationAdvance{
			RunID:             runID,
			Scope:             o.Scope,
			To:                workflow.ObligationAcknowledged,
			At:                at,
			AcknowledgedBy:    ack.By,
			AcknowledgeReason: ack.Reason,
		}); err != nil {
			return err
		}
	}

	if err := e.Store.ResolveWorkflowRecovery(ctx, runID, workflow.StateRecovered, at); err != nil {
		return err
	}

	// The three statuses, closed the same way a resume closes them. A
	// run acknowledged by hand is terminal, and leaving it at
	// workflow_status=running with cleanup_status=unknown puts a row in
	// the history that every surface reads as a run still in flight --
	// for a run whose whole story is that a person finished it.
	//
	// cleanup_status is failed rather than success: nothing this product
	// ran accounted for the scope. The person who acknowledged it said
	// why, and that sentence is on the obligation.
	if err := e.settleStatuses(ctx, runID, true); err != nil {
		return err
	}

	if _, err := e.loadHolds(ctx); err != nil {
		return err
	}

	if e.Logger != nil {
		e.Logger.Alert(ctx, "workflow_recovery_acknowledged", run.BackupSetID,
			fmt.Sprintf("workflow run %q was acknowledged by %s: %s", runID, ack.By, ack.Reason))
	}

	return nil
}

// builtins is the BACKUPD_* block one step is handed.
//
// Every value is a fact this product observed, and the two that only a
// recovery sets are the reason a hook can tell the two jobs apart:
// BACKUPD_RECOVERY is "1" when these bytes are being run to unwind an
// interrupted run rather than a completed one, and
// BACKUPD_CLEANUP_REASON says which. Both are always present -- an unset
// variable and one saying "the normal path" read identically in
// `test -z`, and only one of them is true.
//
// The caller's own facts (source host, source path, destination) are
// layered in first and can therefore not overwrite anything this function
// decides, which matters because the run's identity and its three
// statuses are not things a caller may state on this product's behalf.
func (r *runner) builtins(step workflow.Step, recovering bool) map[string]string {
	out := map[string]string{}

	for name, value := range r.req.Facts {
		out[name] = value
	}

	recovery := "0"
	reason := workflow.CleanupReasonRunCompleted
	if recovering {
		recovery = "1"
		reason = workflow.CleanupReasonInterruptedRun
	}

	out[workflow.ReservedEnvName] = "1"
	out["BACKUPD_RUN_ID"] = r.runID
	out["BACKUPD_BACKUP_SET_ID"] = r.set.String()
	out["BACKUPD_BACKUP_SET_NAME"] = r.set.Set
	out["BACKUPD_PHASE"] = string(step.Phase)
	out["BACKUPD_STEP_ID"] = step.ID
	out["BACKUPD_STEP_NAME"] = step.ScriptName
	out["BACKUPD_STEP_TARGET"] = string(step.Target)
	out["BACKUPD_BACKUP_STATUS"] = string(r.backupStatus)
	out["BACKUPD_CLEANUP_STATUS"] = string(r.cleanupStatus)
	out["BACKUPD_WORKFLOW_STATUS"] = string(r.liveWorkflowStatus())
	out["BACKUPD_STARTED_AT"] = r.startedAt.UTC().Format(time.RFC3339)
	out["BACKUPD_RECOVERY"] = recovery
	out["BACKUPD_CLEANUP_REASON"] = reason

	return out
}

// liveWorkflowStatus is what a hook is told about the stages BEFORE it:
// running while nothing has gone wrong, failed once something has.
//
// Not the run's final verdict, which does not exist yet. A hook that
// reads this is deciding whether to do the careful thing, and "failed"
// is the answer that makes it do so.
func (r *runner) liveWorkflowStatus() workflow.Status {
	if r.hookFailed || r.backupStatus == workflow.StatusFailed {
		return workflow.StatusFailed
	}

	return workflow.StatusRunning
}

// obsAttrs is the attribute set every run-level log line carries, kept in
// one place so a run id is spelled one way across the correlation model.
func obsAttrs(runID, set string, scripts int) []slog.Attr {
	return []slog.Attr{
		slog.String("run_id", runID),
		slog.String("backup_set", set),
		slog.Int("scripts", scripts),
	}
}
