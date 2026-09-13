package workflowrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/obs"
	"github.com/backupdproject/backupd/core/internal/state"
	"github.com/backupdproject/backupd/core/internal/workflow"
	"github.com/backupdproject/backupd/core/internal/workflowexec"
)

// DefaultCleanupTimeout bounds the whole cleanup stage of a run that is
// being torn down.
//
// It is SEPARATE from the per-step timeouts and from the run's own
// cancellation on purpose (#811): the moment a run is cancelled or times
// out is exactly the moment its "after" hooks matter most, so cleanup
// cannot inherit the deadline that just expired. Two minutes is enough
// for an unmount and a database resume and short enough that a shutdown
// is not held open by a hook that will never return.
const DefaultCleanupTimeout = 2 * time.Minute

// ErrRecoveryRequired is returned when a backup set has a workflow run
// whose interruption has not been dealt with.
var ErrRecoveryRequired = errors.New("workflowrun: this backup set has a workflow run that needs recovery")

// ErrNotReconciled is returned by Run before Reconcile has been called.
//
// It is a refusal rather than an implicit reconciliation because the
// startup pass is what decides which sets are blocked, and an engine that
// reconciled lazily on first use would run one backup with an empty
// answer to that question -- which is the one run that must not happen.
var ErrNotReconciled = errors.New("workflowrun: this engine has not reconciled the journal yet")

// Engine runs the five-stage lifecycle.
//
// One engine per deployment. It holds the set locks and the in-process
// record of which sets are blocked, both of which are per deployment, and
// neither of which means anything if there are two.
type Engine struct {
	// Store is the journal. Required.
	Store Store

	// Local and Remote dispatch a step by its target. Either may be nil
	// in a deployment that cannot have the corresponding hooks, and a
	// plan that needs a missing one is refused before anything runs
	// rather than halfway through a stage.
	Local  Executor
	Remote Executor

	// Now is the clock. Required in tests for determinism; defaults to
	// time.Now.
	Now func() time.Time

	// Locks is the per-set lock table. A nil Locks gets one on first
	// use, so the ordinary wiring is three fields.
	Locks *SetLocks

	// Logs fans a run's output out to followers. Optional: a nil Broker
	// means nothing is watching, which is the common case and costs
	// nothing.
	Logs *Broker

	// Logger carries the run and step ids into the existing correlation
	// model (obs.Logger.Begin and Action.End pair a start with its
	// outcome by action id). Optional.
	Logger *obs.Logger

	// Redactor is the deployment's endpoint redactor. Each step's own
	// resolved secret values are layered onto it for the duration of
	// that step and dropped afterwards.
	Redactor *obs.Redactor

	// CleanupTimeout bounds a torn-down run's cleanup stage. Zero takes
	// DefaultCleanupTimeout.
	CleanupTimeout time.Duration

	// StepOutputBytes bounds how much of one step's output is
	// persisted. Zero takes DefaultStepOutputBytes.
	StepOutputBytes int64

	mu          sync.Mutex
	reconciled  bool
	recoveryFor map[model.BackupSetID][]RecoveryHold
}

// RunRequest is one run of one backup set.
type RunRequest struct {
	// Plan is the snapshot of what this run executes, from
	// workflow.Snapshot. A zero Plan is a set with no workflow
	// configured at all, and takes the path that existed before this
	// package did.
	Plan workflow.Plan

	// BackupSetID is the set. It must agree with the plan's, and it is
	// required even for a zero plan, which has no set to ask.
	BackupSetID model.BackupSetID

	// Backup is today's backup-set operation, unchanged. It is called
	// once, between the "before" and "after" stages, and its error is
	// recorded rather than interpreted.
	Backup func(ctx context.Context) error

	// Facts are the deployment's own answers to the built-ins this
	// package cannot know: BACKUPD_SOURCE_HOST, BACKUPD_SOURCE_PATH,
	// BACKUPD_DESTINATION. Anything that is not one of
	// workflow.BuiltinEnvNames is refused by the environment model
	// rather than passed through.
	Facts map[string]string

	// Bypassed records that this run's hooks were deliberately skipped
	// (L6's --skip-workflow-scripts). It is written on the run row as
	// history and has no effect on anything here: an engine handed a
	// plan runs it.
	Bypassed bool
}

// RunResult is what happened, in the shape the history surfaces need.
type RunResult struct {
	RunID string

	// State is the run's terminal state, and it is EMPTY for a backup
	// set with no workflow configured: that path produces no run, so
	// there is no run state to report and inventing one would put a
	// workflow verdict on a backup that had none.
	State workflow.State

	// The three statuses, separate end to end.
	BackupStatus   workflow.Status
	CleanupStatus  workflow.Status
	WorkflowStatus workflow.Status

	// FailedStep is the step id that caused the workflow to fail, or ""
	// -- the FIRST one, because that is the one an operator has to
	// understand; everything after it is a consequence.
	FailedStep string

	// ScriptCount is how many scripts the plan declared, Duration is how
	// long the whole run took, and Bypassed is the history flag.
	ScriptCount int
	Duration    time.Duration
	Bypassed    bool

	// Steps is each step's outcome in plan order.
	Steps []StepResult

	// BackupErr is what the backup operation returned, unchanged.
	BackupErr error

	// RecoveryOutstanding is set when this run ended, or was resumed,
	// with a scope that still cannot be accounted for. The set stays
	// blocked.
	RecoveryOutstanding bool
}

// StepResult is one step's outcome.
type StepResult struct {
	StepID     string
	ScriptName string
	Scope      workflow.Scope
	Phase      workflow.Phase
	Target     workflow.Target
	State      workflow.State
	Outcome    StepOutcome
	Duration   time.Duration
}

// Run executes one backup set's whole workflow: the five stages, the
// obligations, the logs and the durable record of all of it.
//
// The zero-hook path is first and deliberately short. A set with no
// workflow configured produces no run row, no spool, no obligation and no
// log: it calls the backup between a lock and an unlock, which is what
// #811's "adds no meaningful overhead to today's run" means. The lock is
// taken even then, because two concurrent runs of one set were never
// acceptable.
func (e *Engine) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := e.checkReady(req); err != nil {
		return RunResult{}, err
	}

	set := req.BackupSetID
	if !req.Plan.IsZero() {
		if set.IsZero() {
			set = req.Plan.BackupSetID()
		}
		if set != req.Plan.BackupSetID() {
			return RunResult{}, fmt.Errorf("workflowrun: this run names backup set %s and its plan names %s", set, req.Plan.BackupSetID())
		}
	}

	if err := e.checkNotBlocked(set); err != nil {
		return RunResult{}, err
	}

	release, err := e.locks().Acquire(set, req.Plan.RunID())
	if err != nil {
		return RunResult{}, err
	}
	defer release()

	if req.Plan.IsZero() {
		return RunResult{BackupErr: req.Backup(ctx)}, nil
	}

	return e.runPlanned(ctx, req, set)
}

func (e *Engine) checkReady(req RunRequest) error {
	switch {
	case e.Store == nil:
		return errors.New("workflowrun: this engine has no journal, and a run nothing records cannot be recovered")
	case req.Backup == nil:
		return errors.New("workflowrun: this run has no backup operation, so the five stages would wrap nothing")
	case req.BackupSetID.IsZero() && req.Plan.IsZero():
		return errors.New("workflowrun: this run names no backup set")
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.reconciled {
		return fmt.Errorf("%w: Reconcile establishes which sets have an unresolved interruption, and a run started before that answer exists is a backup taken over a machine that may still be quiesced", ErrNotReconciled)
	}

	return nil
}

func (e *Engine) locks() *SetLocks {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.Locks == nil {
		e.Locks = NewSetLocks()
	}

	return e.Locks
}

func (e *Engine) clock() time.Time {
	if e.Now == nil {
		return time.Now()
	}

	return e.Now()
}

// runner is one run's whole state, so that the stage loop reads as the
// sequence it is rather than as fifteen parameters.
type runner struct {
	engine *Engine
	req    RunRequest
	plan   workflow.Plan
	set    model.BackupSetID
	runID  string
	steps  []workflow.Step
	logs   *logRecorder

	startedAt time.Time

	// jctx is the run's context with its CANCELLATION removed, and every
	// durable write in this file uses it rather than ctx.
	//
	// This is not a convenience. A cancelled run still has to record
	// what its steps DID -- a step killed by a cancellation is
	// StateCanceled, which is an outcome this product observed -- and a
	// journal write on the cancelled context fails, leaving the step
	// recorded as "running". The next startup then reads that as an
	// interruption whose outcome nobody knows and blocks the backup set
	// for recovery, which is completely wrong about a run somebody
	// cancelled on purpose. Execution uses ctx, so a cancellation still
	// stops hooks; bookkeeping uses jctx, so what they did is still
	// written down.
	jctx context.Context

	// The three statuses, which move independently. They are the values
	// handed to every hook as it starts, so they are the run's live
	// state and not a summary computed at the end.
	backupStatus   workflow.Status
	cleanupStatus  workflow.Status
	workflowStatus workflow.Status

	// entered says which scopes have had their obligation durably
	// raised to eligible. It is the in-memory shadow of the journal, and
	// the journal is what a restart reads.
	entered map[workflow.Scope]bool

	results    []StepResult
	byID       map[string]int
	failedStep string
	hookFailed bool
	canceled   bool
	timedOut   bool
	cleanupBad bool
	backupErr  error

	// recovering marks a runner built by ResumeCleanup rather than by a
	// run. It is what puts BACKUPD_RECOVERY=1 in front of a hook and
	// what keeps a resumed cleanup from re-deciding the run's own
	// timeout and cancellation state.
	recovering bool
}

func (e *Engine) runPlanned(ctx context.Context, req RunRequest, set model.BackupSetID) (RunResult, error) {
	startedAt := e.clock()
	plan := req.Plan

	r := &runner{
		engine:         e,
		req:            req,
		plan:           plan,
		set:            set,
		runID:          plan.RunID(),
		steps:          plan.Steps(),
		startedAt:      startedAt,
		backupStatus:   workflow.StatusUnknown,
		cleanupStatus:  workflow.StatusUnknown,
		workflowStatus: workflow.StatusRunning,
		entered:        map[workflow.Scope]bool{},
		byID:           map[string]int{},
		jctx:           context.WithoutCancel(ctx),
	}

	if err := r.checkExecutors(); err != nil {
		return RunResult{}, err
	}

	r.logs = &logRecorder{
		store:  e.Store,
		broker: e.Logs,
		runID:  r.runID,
		now:    e.clock,
		bound:  e.StepOutputBytes,
		ctx:    context.WithoutCancel(ctx),
	}

	for i, s := range r.steps {
		r.byID[s.ID] = i
		r.results = append(r.results, StepResult{
			StepID:     s.ID,
			ScriptName: s.ScriptName,
			Scope:      s.Scope,
			Phase:      s.Phase,
			Target:     s.Target,
			State:      workflow.StatePending,
		})
	}

	// The plan, its steps and both obligations, in one transaction,
	// before a byte of any hook runs. The global scope is committed
	// ALREADY eligible: the run has begun, so that scope is entered, and
	// this transaction is the only write that is unambiguously before
	// the run's first side effect.
	if err := e.Store.CommitWorkflowPlan(ctx, state.WorkflowPlan{
		Run:         r.runRecord(),
		Steps:       r.steps,
		Env:         plan.Env(),
		Obligations: r.initialObligations(),
	}); err != nil {
		return RunResult{}, err
	}

	r.entered[workflow.ScopeGlobal] = true

	action := e.begin(ctx, r)

	if err := e.Store.AdvanceWorkflowRun(ctx, r.runID, state.WorkflowRunAdvance{
		To:             workflow.StateRunning,
		WorkflowStatus: workflow.StatusRunning,
	}); err != nil {
		return RunResult{}, err
	}

	r.execute(ctx)

	res, err := r.finish()
	e.end(ctx, action, res)

	return res, err
}

// checkExecutors refuses a plan this engine cannot dispatch, before
// anything runs.
//
// #809's requirement is that a local hook's validation fails BEFORE the
// backup starts, and this is the same argument applied to the wiring: a
// plan with a remote step and no remote executor must not discover that
// after the "before" stage has already quiesced a database.
func (r *runner) checkExecutors() error {
	for _, s := range r.steps {
		if _, err := r.engine.executorFor(s.Target); err != nil {
			return fmt.Errorf("workflowrun: run %q cannot be executed: %w", r.runID, err)
		}
	}

	return nil
}

func (e *Engine) executorFor(target workflow.Target) (Executor, error) {
	switch target {
	case workflow.TargetLocal:
		if e.Local == nil {
			return nil, errors.New("this deployment has no host workflow runner, and a NAME.local.sh has nowhere to run")
		}

		return e.Local, nil
	case workflow.TargetRemote:
		if e.Remote == nil {
			return nil, errors.New("this deployment has no remote executor, and a NAME.remote.sh has nowhere to run")
		}

		return e.Remote, nil
	default:
		return nil, fmt.Errorf("%q is not a target this engine can dispatch to", target)
	}
}

func (r *runner) runRecord() workflow.Run {
	return workflow.Run{
		ID:               r.runID,
		BackupSetID:      r.set,
		State:            workflow.StatePending,
		StartedAt:        r.startedAt,
		BackupStatus:     r.backupStatus,
		CleanupStatus:    r.cleanupStatus,
		WorkflowStatus:   workflow.StatusUnknown,
		RecoveryState:    workflow.RecoveryNone,
		ResolvedPlanHash: r.plan.ResolvedPlanHash(),
		ScriptSpoolRef:   r.plan.ScriptSpoolRef(),
		Bypassed:         r.req.Bypassed,
	}
}

// initialObligations is #811's nested rule, stated once: the global scope
// is entered when the run begins, the backup-set scope is not.
func (r *runner) initialObligations() []workflow.CleanupObligation {
	started := r.startedAt

	return []workflow.CleanupObligation{
		{
			RunID:       r.runID,
			Scope:       workflow.ScopeGlobal,
			BackupSetID: r.set,
			State:       workflow.ObligationEligible,
			EnteredAt:   &started,
		},
		{
			RunID:       r.runID,
			Scope:       workflow.ScopeSet,
			BackupSetID: r.set,
			State:       workflow.ObligationNeverEligible,
		},
	}
}

// execute is the five stages, and the shape of it IS the failure matrix.
//
// Read it as three questions. Did global-before succeed? If not, the
// backup-set scope is never entered, its "before" steps are skipped and
// so is the backup. Did backup-set-before succeed? If not, the backup is
// skipped -- but the scope WAS entered, so both "after" stages are owed.
// Then cleanup runs whatever is owed, in unwind order, whatever happened
// above it.
func (r *runner) execute(ctx context.Context) {
	globalBefore := r.runStage(ctx, workflow.ScopeGlobal, workflow.PhaseBefore)

	switch {
	case !globalBefore:
		// The backup-set scope is never entered. Its "before" steps are
		// recorded as skipped rather than left pending, so the plan read
		// back afterwards accounts for every step it declared.
		r.skipStage(workflow.ScopeSet, workflow.PhaseBefore)
		r.skipBackup()
	default:
		r.enterSetScope()

		if r.runStage(ctx, workflow.ScopeSet, workflow.PhaseBefore) {
			r.runBackup(ctx)
		} else {
			r.skipBackup()
		}
	}

	r.runCleanup(ctx)
}

// enterSetScope is the durable write that makes the backup-set scope's
// "after" stage owed, and it happens BEFORE the first side-effecting
// command in that scope -- which is either the first backup-set "before"
// hook or, if there are none, the backup itself.
func (r *runner) enterSetScope() {
	if r.entered[workflow.ScopeSet] {
		return
	}

	if err := r.engine.Store.AdvanceWorkflowCleanupObligation(r.jctx, state.WorkflowObligationAdvance{
		RunID: r.runID,
		Scope: workflow.ScopeSet,
		To:    workflow.ObligationEligible,
		At:    r.engine.clock(),
	}); err != nil {
		// A scope that cannot be recorded as entered must not be
		// entered. Failing the run here leaves the journal saying the
		// set scope was never entered, which is true: nothing in it has
		// run.
		r.fail("", fmt.Errorf("recording that the backup-set scope was entered: %w", err))

		return
	}

	r.entered[workflow.ScopeSet] = true
}

// runStage runs one stage's steps in plan order and reports whether the
// stage passed.
//
// A step that fails stops the STAGE: the steps behind it are skipped,
// because a "before" stage is a sequence an operator wrote in an order,
// and running step three after step two failed is running it in a state
// nobody designed.
func (r *runner) runStage(ctx context.Context, scope workflow.Scope, phase workflow.Phase) bool {
	ok := true

	for _, s := range r.steps {
		if s.Scope != scope || s.Phase != phase {
			continue
		}

		if !ok || r.stopping(ctx) {
			r.skipStep(s)

			continue
		}

		if !r.runStep(ctx, s, false) {
			ok = false
		}
	}

	return ok
}

// stopping reports whether the run is being torn down, which is the point
// at which no NEW before-or-backup work may start.
func (r *runner) stopping(ctx context.Context) bool {
	if r.canceled || r.timedOut {
		return true
	}

	select {
	case <-ctx.Done():
		r.noteCancellation(ctx.Err())

		return true
	default:
		return false
	}
}

// noteCancellation reads a context error, and ONLY a context error.
//
// Anything else is an ordinary failure. The first version of this treated
// every non-nil error as a cancellation, which made a backup that failed
// because the repository was full come out as a cancelled run -- two
// completely different operator situations, one of which is nobody's
// fault.
func (r *runner) noteCancellation(err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		r.timedOut = true
	case errors.Is(err, context.Canceled):
		r.canceled = true
	}
}

// runStep is the whole of one step: resolve the environment, open the
// captured bytes, record it running, dispatch it, record what happened.
//
// recovering says whether these bytes are being run to unwind an
// INTERRUPTED run rather than this one, which changes exactly one thing:
// the environment the hook is handed (BACKUPD_RECOVERY and
// BACKUPD_CLEANUP_REASON). Everything else about running a step -- the
// durable "running" write, the redaction, the bound, the outcome mapping
// -- is identical, because a recovery that executed hooks through a
// second path would be a second set of guarantees.
func (r *runner) runStep(ctx context.Context, step workflow.Step, recovering bool) bool {
	idx := r.byID[step.ID]

	script, err := r.plan.OpenScript(step.ID)
	if err != nil {
		r.finishStep(step, StepOutcome{
			Disposition: DispositionNotAttempted,
			Detail:      err.Error(),
		}, workflow.StateFailed, 0)

		return false
	}

	resolved, err := r.plan.Env().Resolve(ctx, r.builtins(step, recovering))
	if err != nil {
		r.finishStep(step, StepOutcome{
			Disposition: DispositionNotAttempted,
			Detail:      err.Error(),
		}, workflow.StateFailed, 0)

		return false
	}

	executor, err := r.engine.executorFor(step.Target)
	if err != nil {
		r.finishStep(step, StepOutcome{
			Disposition: DispositionNotAttempted,
			Detail:      err.Error(),
		}, workflow.StateFailed, 0)

		return false
	}

	startedAt := r.engine.clock()

	// Durably running BEFORE the bytes reach a runner. Everything about
	// recovery follows from this one ordering: a step the journal says
	// is running, in a process that then dies, is a step whose outcome
	// is unknown -- and a step the journal never recorded is a side
	// effect nothing will ever look for.
	if err := r.engine.Store.StartWorkflowStep(r.jctx, r.runID, step.ID, startedAt); err != nil {
		r.fail(step.ID, fmt.Errorf("recording that step %q started: %w", step.ID, err))

		return false
	}
	r.results[idx].State = workflow.StateRunning

	// The step's own secret material is layered onto the deployment's
	// redactor for the duration of this step and dropped afterwards, so
	// a hook that prints its own credential does not write it into the
	// journal or stream it to a browser.
	sink := r.logs.stepSink(step.ID, r.engine.Redactor.WithValues(resolved.SecretValues()...))

	stepCtx, cancel := context.WithTimeout(ctx, step.Timeout)
	outcome, execErr := executor.ExecuteStep(stepCtx, StepRequest{
		RunID:   r.runID,
		Step:    step,
		Script:  script,
		Environ: resolved.Environ(),
		Timeout: step.Timeout,
		Sink:    sink,
	})
	cancel()

	closeErr := sink.close()

	switch {
	case execErr != nil:
		outcome = StepOutcome{Disposition: DispositionNotAttempted, Detail: execErr.Error()}
	case closeErr != nil:
		// A step whose output could not be recorded is a step this
		// product cannot account for, and reporting it as captured is
		// worse than failing it (workflowexec.Sink's own contract).
		outcome = StepOutcome{Disposition: DispositionNotAttempted, Detail: closeErr.Error()}
	default:
		if err := outcome.validate(); err != nil {
			outcome = StepOutcome{Disposition: DispositionTransportLost, Detail: err.Error()}
		}
	}

	nextState := outcome.state()

	// A step killed because the run was being torn down is recorded as
	// cancelled or timed out rather than failed, whatever the adapter
	// made of the signal: the reason it stopped is this product's own
	// decision and an audit has to be able to attribute it.
	if nextState == workflow.StateFailed || nextState == workflow.StateCanceled {
		switch {
		case r.timedOut && outcome.Disposition != DispositionExited:
			nextState = workflow.StateTimedOut
		case r.canceled && outcome.Disposition != DispositionExited:
			nextState = workflow.StateCanceled
		}
	}

	r.finishStep(step, outcome, nextState, r.engine.clock().Sub(startedAt))

	return nextState == workflow.StateSuccess
}

// finishStep records one step's terminal state, durably and in memory.
func (r *runner) finishStep(step workflow.Step, outcome StepOutcome, st workflow.State, took time.Duration) {
	idx := r.byID[step.ID]

	r.results[idx].State = st
	r.results[idx].Outcome = outcome
	r.results[idx].Duration = took

	out := state.WorkflowStepOutcome{
		State:                st,
		FinishedAt:           r.engine.clock(),
		TerminationConfirmed: outcome.Certainty == workflowexec.TerminationConfirmed,
	}

	// An exit code is recorded only when a process exited and this
	// product saw the status. The journal refuses the alternative, and
	// the adapter's outcome already carries nil in every other case;
	// this is the third place the same rule is applied, because it is
	// the rule #810 and #811 both name explicitly.
	if outcome.Disposition == DispositionExited {
		out.ExitCode = outcome.ExitCode
	}

	if err := r.engine.Store.FinishWorkflowStep(r.jctx, r.runID, step.ID, out); err != nil {
		r.fail(step.ID, fmt.Errorf("recording the outcome of step %q: %w", step.ID, err))

		return
	}

	if st != workflow.StateSuccess && st != workflow.StateSkipped {
		r.noteFailure(step, st)
	}
}

// noteFailure records which step broke the run and which status it broke.
//
// An "after" step's failure counts against the CLEANUP status and the
// workflow's; a "before" step's or the backup's counts against the
// workflow's. Both fail the workflow, which is #811's rule -- an after
// failure fails the run even when the backup succeeded -- and the two
// statuses stay separate because the operator's next action differs.
func (r *runner) noteFailure(step workflow.Step, st workflow.State) {
	if r.failedStep == "" {
		r.failedStep = step.ID
	}

	if step.Phase == workflow.PhaseAfter {
		r.cleanupBad = true
	}

	r.hookFailed = true

	switch st {
	case workflow.StateTimedOut:
		r.timedOut = true
	case workflow.StateCanceled:
		r.canceled = true
	}
}

// fail is the "this engine could not do its own bookkeeping" path: a
// journal write that did not land.
//
// It marks the run as failed in memory and lets the sequence carry on to
// the cleanup stage, because the cleanup is the part that matters when
// something has already been quiesced. What it does NOT do is pretend the
// step succeeded.
func (r *runner) fail(stepID string, err error) {
	if r.failedStep == "" {
		r.failedStep = stepID
	}
	r.hookFailed = true

	if r.engine.Logger != nil {
		r.engine.Logger.Error(context.Background(), "workflow_run", err)
	}
}

// skipStage records every step of a stage as skipped.
func (r *runner) skipStage(scope workflow.Scope, phase workflow.Phase) {
	for _, s := range r.steps {
		if s.Scope == scope && s.Phase == phase {
			r.skipStep(s)
		}
	}
}

func (r *runner) skipStep(step workflow.Step) {
	idx := r.byID[step.ID]
	if r.results[idx].State != workflow.StatePending {
		return
	}

	r.results[idx].State = workflow.StateSkipped

	if err := r.engine.Store.FinishWorkflowStep(r.jctx, r.runID, step.ID, state.WorkflowStepOutcome{
		State:      workflow.StateSkipped,
		FinishedAt: r.engine.clock(),
	}); err != nil {
		r.fail(step.ID, fmt.Errorf("recording that step %q was skipped: %w", step.ID, err))
	}
}

// runBackup calls today's backup-set operation and records what it
// returned.
func (r *runner) runBackup(ctx context.Context) {
	r.backupStatus = workflow.StatusRunning
	if err := r.engine.Store.AdvanceWorkflowRun(r.jctx, r.runID, state.WorkflowRunAdvance{
		BackupStatus: workflow.StatusRunning,
	}); err != nil {
		r.fail("", err)
	}

	err := r.req.Backup(ctx)

	switch {
	case err == nil:
		r.backupStatus = workflow.StatusSuccess
	default:
		r.backupStatus = workflow.StatusFailed
		r.noteCancellation(err)
	}

	if err := r.engine.Store.AdvanceWorkflowRun(r.jctx, r.runID, state.WorkflowRunAdvance{
		BackupStatus: r.backupStatus,
	}); err != nil {
		r.fail("", err)
	}

	r.backupErr = err
}

// skipBackup records that the backup did not run.
//
// StatusSkipped rather than StatusFailed, because nothing was attempted:
// a "before" hook refused to let the backup proceed, or the run was being
// torn down. The run's own state carries WHY (failed, cancelled, timed
// out), which is the fact that distinguishes the three.
func (r *runner) skipBackup() {
	r.backupStatus = workflow.StatusSkipped

	if err := r.engine.Store.AdvanceWorkflowRun(r.jctx, r.runID, state.WorkflowRunAdvance{
		BackupStatus: workflow.StatusSkipped,
	}); err != nil {
		r.fail("", err)
	}
}

// runCleanup runs the "after" stages that are owed, in unwind order, and
// discharges each scope's obligation.
//
// It runs under a context with the run's CANCELLATION REMOVED and a bound
// of its own (#811's separate cleanup timeout). That is the point of the
// stage: the moment a run is cancelled or times out is the moment its
// unwinding matters most, so cleanup cannot inherit the deadline that
// just expired -- and abandoning an unmount because somebody pressed
// Ctrl-C twice is how a machine gets left quiesced.
func (r *runner) runCleanup(ctx context.Context) {
	timeout := r.engine.CleanupTimeout
	if timeout <= 0 {
		timeout = DefaultCleanupTimeout
	}

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	r.cleanupStatus = workflow.StatusRunning
	if err := r.engine.Store.AdvanceWorkflowRun(r.jctx, r.runID, state.WorkflowRunAdvance{
		To:            workflow.StateCleanupRunning,
		CleanupStatus: workflow.StatusRunning,
	}); err != nil {
		r.fail("", err)
	}

	// Unwind order: the backup-set scope first, then the global one.
	for _, scope := range []workflow.Scope{workflow.ScopeSet, workflow.ScopeGlobal} {
		r.runCleanupScope(cleanupCtx, scope)
	}

	r.cleanupStatus = r.finalCleanupStatus()
}

// runCleanupScope runs one scope's "after" stage if that scope is owed
// one, and records the obligation's outcome either way.
func (r *runner) runCleanupScope(ctx context.Context, scope workflow.Scope) {
	if !r.entered[scope] {
		// Never eligible: the scope was not entered, so nothing in it
		// has a side effect to undo. Its steps are recorded as skipped
		// and the obligation stays where the plan committed it.
		r.skipStage(scope, workflow.PhaseAfter)

		return
	}

	if err := r.engine.Store.AdvanceWorkflowCleanupObligation(r.jctx, state.WorkflowObligationAdvance{
		RunID: r.runID,
		Scope: scope,
		To:    workflow.ObligationRunning,
		At:    r.engine.clock(),
	}); err != nil {
		r.fail("", err)

		return
	}

	ok := true
	for _, s := range r.steps {
		if s.Scope != scope || s.Phase != workflow.PhaseAfter {
			continue
		}

		// An "after" step's failure does NOT stop the rest of the
		// cleanup: #811's rule is that later cleanup continues. An
		// unmount that fails must not prevent the notification that
		// says so.
		if !r.runStep(ctx, s, false) {
			ok = false
		}
	}

	to := workflow.ObligationSuccess
	if !ok {
		to = workflow.ObligationFailed
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

// finalCleanupStatus is the run's cleanup verdict.
//
// Success for a run whose eligible "after" steps all succeeded, INCLUDING
// a run that had none: the obligation was to account for the scope, and a
// scope with nothing to undo is accounted for. Skipped when "after" steps
// were planned and none of them was eligible, which is the honest answer
// for a global-before failure: there was cleanup to do for the set scope
// and that scope was never entered.
func (r *runner) finalCleanupStatus() workflow.Status {
	if r.cleanupBad {
		return workflow.StatusFailed
	}

	planned, eligible := 0, 0
	for _, s := range r.steps {
		if s.Phase != workflow.PhaseAfter {
			continue
		}
		planned++
		if r.entered[s.Scope] {
			eligible++
		}
	}

	if planned > 0 && eligible == 0 {
		return workflow.StatusSkipped
	}

	return workflow.StatusSuccess
}

// finish records the run's terminal state and its three statuses.
func (r *runner) finish() (RunResult, error) {
	finishedAt := r.engine.clock()

	r.workflowStatus = workflow.StatusSuccess
	if r.hookFailed || r.backupStatus == workflow.StatusFailed || r.cleanupBad {
		r.workflowStatus = workflow.StatusFailed
	}

	// The run's state, in precedence order. Cancellation and timeout win
	// because they are what an operator asked for or what this product
	// decided, and the cleanup's outcome is preserved in its own field
	// either way -- which is #811's "finish with a cancellation status
	// that preserves the cleanup outcome".
	st := workflow.StateSuccess
	switch {
	case r.canceled:
		st = workflow.StateCanceled
	case r.timedOut:
		st = workflow.StateTimedOut
	case r.cleanupBad:
		st = workflow.StateCleanupFailed
	case r.workflowStatus == workflow.StatusFailed:
		st = workflow.StateFailed
	}

	advance := state.WorkflowRunAdvance{
		To:             st,
		BackupStatus:   r.backupStatus,
		CleanupStatus:  r.cleanupStatus,
		WorkflowStatus: r.workflowStatus,
		FinishedAt:     &finishedAt,
	}

	err := r.engine.Store.AdvanceWorkflowRun(r.jctx, r.runID, advance)

	res := RunResult{
		RunID:          r.runID,
		State:          st,
		BackupStatus:   r.backupStatus,
		CleanupStatus:  r.cleanupStatus,
		WorkflowStatus: r.workflowStatus,
		FailedStep:     r.failedStep,
		ScriptCount:    len(r.steps),
		Duration:       finishedAt.Sub(r.startedAt),
		Bypassed:       r.req.Bypassed,
		Steps:          r.results,
		BackupErr:      r.backupErr,
	}

	return res, err
}

// begin and end put the run into the existing correlation model: a start
// carrying an action id, and a completion carrying the same id and an
// outcome, which is what lets a reader pair them and notice a run that
// announced itself and went quiet (obs/action.go).
func (e *Engine) begin(ctx context.Context, r *runner) *obs.Action {
	if e.Logger == nil {
		return nil
	}

	return e.Logger.Begin(ctx, "workflow_run_start", "workflow_run",
		"workflow run started",
		obsAttrs(r.runID, r.set.String(), len(r.steps))...,
	)
}

func (e *Engine) end(ctx context.Context, action *obs.Action, res RunResult) {
	if action == nil {
		return
	}

	attrs := []slog.Attr{
		slog.String("run_id", res.RunID),
		slog.String("workflow_status", string(res.WorkflowStatus)),
		slog.String("backup_status", string(res.BackupStatus)),
		slog.String("cleanup_status", string(res.CleanupStatus)),
	}
	if res.FailedStep != "" {
		attrs = append(attrs, slog.String("failed_step", res.FailedStep))
	}

	if res.WorkflowStatus == workflow.StatusSuccess {
		action.Succeeded(ctx, "workflow run finished", attrs...)

		return
	}

	action.End(ctx, obs.ResultError, "workflow run finished", attrs...)
}
