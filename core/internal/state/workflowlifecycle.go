package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/workflow"
)

// The durable transitions the five-stage workflow lifecycle runs on
// (EPIC L, #811), and the one discipline they all share.
//
// # Every write asks the domain whether it is legal first
//
// internal/workflow owns the two transition graphs (State.CanFollowForRun
// and CanFollowForStep) and the obligation's (ObligationState.CanFollow).
// This file reads the CURRENT value out of the row, asks the graph, and
// refuses anything it does not allow -- inside the same transaction as
// the write, so the check and the write cannot be separated by another
// connection.
//
// That is not defensive decoration, it is the whole reason a crash is
// tractable. "A crash between any two durable transitions reconciles
// deterministically" is a claim about a FINITE set of positions, and it
// only holds if no write could have put a row somewhere the graph does
// not admit. A journal that accepted any state for any row would make
// the reconciliation a guess about which of the writes had happened.
//
// # There is no write here that clears an outstanding recovery
//
// AdvanceWorkflowRun refuses to lower a run's recovery state, and that is
// the structural half of #811's requirement that --skip-workflow-scripts
// (L6) cannot clear recovery_required. A flag that skips hooks reaches
// this journal through the ordinary advance, and the ordinary advance
// cannot say "and consider the interruption dealt with". The two calls
// that can are ResolveWorkflowRecovery, which requires every obligation
// to have reached a settled state first, and the acknowledgement path,
// which requires an actor and a reason it writes down.
//
// # Times are the caller's, not this layer's
//
// Every transition takes the moment as a parameter. The engine's clock is
// injectable so that the crash-injection tests are deterministic, and a
// journal that stamped rows with time.Now() would make half of each
// record untestable and the other half disagree with the step's own
// timestamps by whatever the write cost.

// ErrWorkflowObligationNotFound is returned when a run has no obligation
// for the scope named. It is a value for ErrWorkflowRunNotFound's reason:
// no caller needs structure out of it.
var ErrWorkflowObligationNotFound = errors.New("state: workflow cleanup obligation not found")

// ErrRecoveryOutstanding is returned by ResolveWorkflowRecovery when a
// scope of the run still cannot be accounted for.
//
// A sentinel rather than a sentence to match on, because the caller that
// branches on it is deciding whether a resume-cleanup finished the job or
// left the backup set blocked -- and getting that wrong in the permissive
// direction would report a machine as put back on the strength of a
// changed error message.
var ErrRecoveryOutstanding = errors.New("state: this workflow run still has a cleanup obligation that cannot be accounted for")

// execQuerier is the half of *sql.DB and *sql.Tx this file uses, so that
// every transition is one function usable both on its own and inside the
// reconciliation's single transaction.
//
// Without it, ApplyWorkflowReconciliation would be a second
// implementation of the same three writes -- which is exactly the place a
// second opinion about a transition graph would hide.
type execQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// StartWorkflowStep records that a step is RUNNING, before its bytes
// reach a runner.
//
// The ordering is #811's requirement and it is the caller's to honour;
// what this function guarantees is the other half: the write is refused
// unless the step is pending, so a second attempt at a step whose first
// attempt is in flight cannot make the journal say the step started
// twice. A step recorded as running that this process then dies under is
// what the reconciliation reads as interrupted, and an interrupted step
// is the only thing that makes a scope's obligation unaccountable rather
// than merely unfinished.
func (j *Journal) StartWorkflowStep(ctx context.Context, runID, stepID string, at time.Time) error {
	if at.IsZero() {
		return fmt.Errorf("state: starting workflow step %q of run %q with no time; when a hook started is what a recovery pass reads", stepID, runID)
	}

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin start workflow step %q: %w", stepID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	if err := startWorkflowStep(ctx, tx, runID, stepID, at); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit start of workflow step %q: %w", stepID, err)
	}

	return nil
}

func startWorkflowStep(ctx context.Context, q execQuerier, runID, stepID string, at time.Time) error {
	current, _, err := currentStepState(ctx, q, runID, stepID)
	if err != nil {
		return err
	}

	if err := current.CanFollowForStep(workflow.StateRunning); err != nil {
		return fmt.Errorf("state: workflow step %q of run %q cannot start: %w", stepID, runID, err)
	}

	_, err = q.ExecContext(ctx,
		`UPDATE workflow_steps SET state = ?, started_at = ? WHERE run_id = ? AND step_id = ?`,
		string(workflow.StateRunning), formatTime(at), runID, stepID)
	if err != nil {
		return fmt.Errorf("state: starting workflow step %q of run %q: %w", stepID, runID, err)
	}

	return nil
}

// WorkflowStepOutcome is everything a step's terminal transition records.
//
// ExitCode is a pointer and its absence is meaningful: #810's and #811's
// shared requirement is that transport loss is never reported as a known
// exit code, so a status nobody observed leaves it nil. This type is
// where that is enforced rather than documented -- an outcome that is not
// StateSuccess or StateFailed and carries a code is refused, because
// those are the only two states that mean "a process exited and we saw
// the status".
type WorkflowStepOutcome struct {
	// State is the terminal state. Any of workflow's step states that
	// the step's current state may legally move to.
	State workflow.State

	// FinishedAt is when the step reached it. Required.
	FinishedAt time.Time

	// ExitCode is the observed status, or nil when nobody observed one.
	ExitCode *int

	// TerminationConfirmed records that this product PROVED what it
	// killed is gone, rather than that it sent a signal and moved on.
	TerminationConfirmed bool

	// StdoutLogRef and StderrLogRef point at the captured output.
	StdoutLogRef string
	StderrLogRef string
}

// FinishWorkflowStep records a step's terminal state.
func (j *Journal) FinishWorkflowStep(ctx context.Context, runID, stepID string, out WorkflowStepOutcome) error {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin finish workflow step %q: %w", stepID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	if err := finishWorkflowStep(ctx, tx, runID, stepID, out); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit finish of workflow step %q: %w", stepID, err)
	}

	return nil
}

func finishWorkflowStep(ctx context.Context, q execQuerier, runID, stepID string, out WorkflowStepOutcome) error {
	if out.FinishedAt.IsZero() {
		return fmt.Errorf("state: finishing workflow step %q of run %q with no time", stepID, runID)
	}

	if out.ExitCode != nil && out.State != workflow.StateSuccess && out.State != workflow.StateFailed {
		return fmt.Errorf(
			"state: workflow step %q of run %q is being recorded as %q with exit code %d; only a step that exited and whose status this product observed has one, and reporting a killed, cancelled or lost step's outcome as an exit code is the confusion #810 refuses",
			stepID, runID, out.State, *out.ExitCode)
	}

	current, started, err := currentStepState(ctx, q, runID, stepID)
	if err != nil {
		return err
	}

	if err := current.CanFollowForStep(out.State); err != nil {
		return fmt.Errorf("state: workflow step %q of run %q cannot finish: %w", stepID, runID, err)
	}

	// A step that never STARTED does not get a finish time, whatever the
	// caller passed.
	//
	// This is not tidiness. A step is reached from pending by being
	// skipped or refused, and internal/workflow's Step.Validate refuses
	// a record that finished without having started -- so a row carrying
	// the pair is a row RecoverWorkflowPlan will not read back, which
	// makes the whole run unrecoverable because one of its steps was
	// deliberately not run. The decision's time is the run's own finish
	// time; this column is when the STEP ended, and it did not.
	finishedAt := any(formatTime(out.FinishedAt))
	if !started {
		finishedAt = nil
	}

	_, err = q.ExecContext(ctx,
		`UPDATE workflow_steps
		    SET state = ?, finished_at = ?, exit_code = ?, termination_confirmed = ?,
		        stdout_log_ref = ?, stderr_log_ref = ?
		  WHERE run_id = ? AND step_id = ?`,
		string(out.State), finishedAt, out.ExitCode, out.TerminationConfirmed,
		out.StdoutLogRef, out.StderrLogRef, runID, stepID)
	if err != nil {
		return fmt.Errorf("state: finishing workflow step %q of run %q: %w", stepID, runID, err)
	}

	return nil
}

// currentStepState reads one step's state and whether it ever started, or
// reports that the step is not one this journal has.
//
// A missing step is a refusal rather than a no-op update, and the
// difference matters: an UPDATE that matched no row returns success, so an
// engine working from a plan the journal does not hold would run a whole
// workflow and record none of it.
func currentStepState(ctx context.Context, q execQuerier, runID, stepID string) (st workflow.State, started bool, err error) {
	var (
		raw       string
		startedAt sql.NullString
	)

	err = q.QueryRowContext(ctx,
		`SELECT state, started_at FROM workflow_steps WHERE run_id = ? AND step_id = ?`,
		runID, stepID).Scan(&raw, &startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("state: workflow run %q has no step %q: %w", runID, stepID, ErrWorkflowRunNotFound)
	}
	if err != nil {
		return "", false, fmt.Errorf("state: reading workflow step %q of run %q: %w", stepID, runID, err)
	}

	return workflow.State(raw), startedAt.Valid, nil
}

// WorkflowRunAdvance is one transition of a run.
//
// Every field is optional except the ones the caller sets, and the zero
// value of each means "leave it alone" rather than "set it to the zero
// value". That is unusual enough to state: a run's three statuses move
// independently -- a backup that has succeeded while the cleanup is still
// running is the normal case -- so an advance that wrote every column
// would make the caller restate facts it is not changing, which is how
// the backup status gets clobbered by the code that meant to record the
// cleanup's.
type WorkflowRunAdvance struct {
	// To is the run's new state, or "" to leave it where it is.
	To workflow.State

	// The three statuses. "" leaves one unchanged. They are separate
	// fields end to end, which is #811's requirement and the reason
	// there is no single "status" here.
	BackupStatus   workflow.Status
	CleanupStatus  workflow.Status
	WorkflowStatus workflow.Status

	// RecoveryState moves the second axis. It may only be RAISED by this
	// call; see AdvanceWorkflowRun.
	RecoveryState workflow.RecoveryState

	// FinishedAt closes the run. Nil leaves it open.
	FinishedAt *time.Time
}

func (a WorkflowRunAdvance) isEmpty() bool {
	return a.To == "" && a.BackupStatus == "" && a.CleanupStatus == "" &&
		a.WorkflowStatus == "" && a.RecoveryState == "" && a.FinishedAt == nil
}

// AdvanceWorkflowRun applies one transition to a run.
//
// It refuses to LOWER the recovery axis, which is the structural half of
// #811's "--skip-workflow-scripts must be unable to clear
// recovery_required": a run whose recovery is outstanding can be advanced
// through every other column -- its state, its three statuses, its finish
// time -- and there is no argument to this function that settles the
// recovery. ResolveWorkflowRecovery is the only call that does, and it
// checks the obligations first.
func (j *Journal) AdvanceWorkflowRun(ctx context.Context, runID string, adv WorkflowRunAdvance) error {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin advance workflow run %q: %w", runID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	if err := advanceWorkflowRun(ctx, tx, runID, adv); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit advance of workflow run %q: %w", runID, err)
	}

	return nil
}

func advanceWorkflowRun(ctx context.Context, q execQuerier, runID string, adv WorkflowRunAdvance) error {
	if adv.isEmpty() {
		return fmt.Errorf("state: advancing workflow run %q changes nothing; a write that records no fact is a caller that has lost track of what it observed", runID)
	}

	current, currentRecovery, err := currentRunState(ctx, q, runID)
	if err != nil {
		return err
	}

	if adv.To != "" {
		if err := current.CanFollowForRun(adv.To); err != nil {
			return fmt.Errorf("state: workflow run %q cannot advance: %w", runID, err)
		}
	}

	if err := checkRecoveryRaise(runID, currentRecovery, adv.RecoveryState); err != nil {
		return err
	}

	for _, s := range []struct {
		what  string
		value workflow.Status
	}{
		{"backup status", adv.BackupStatus},
		{"cleanup status", adv.CleanupStatus},
		{"workflow status", adv.WorkflowStatus},
	} {
		if s.value != "" && !s.value.Valid() {
			return fmt.Errorf("state: workflow run %q cannot be given %s %q", runID, s.what, s.value)
		}
	}

	_, err = q.ExecContext(ctx,
		`UPDATE workflow_runs
		    SET state           = COALESCE(NULLIF(?, ''), state),
		        backup_status   = COALESCE(NULLIF(?, ''), backup_status),
		        cleanup_status  = COALESCE(NULLIF(?, ''), cleanup_status),
		        workflow_status = COALESCE(NULLIF(?, ''), workflow_status),
		        recovery_state  = COALESCE(NULLIF(?, ''), recovery_state),
		        finished_at     = COALESCE(?, finished_at)
		  WHERE run_id = ?`,
		string(adv.To), string(adv.BackupStatus), string(adv.CleanupStatus),
		string(adv.WorkflowStatus), string(adv.RecoveryState), formatTimePtr(adv.FinishedAt), runID)
	if err != nil {
		return fmt.Errorf("state: advancing workflow run %q: %w", runID, err)
	}

	return nil
}

// checkRecoveryRaise holds the recovery axis to one direction.
//
// RecoveryNone -> RecoveryRequired is what a reconciliation writes;
// RecoveryRequired -> RecoveryInProgress is what a resume-cleanup writes.
// The reverse is refused here and possible only through
// ResolveWorkflowRecovery, which is the one call that looks at whether
// the obligations were actually accounted for.
func checkRecoveryRaise(runID string, current, next workflow.RecoveryState) error {
	if next == "" {
		return nil
	}
	if !next.Valid() {
		return fmt.Errorf("state: workflow run %q cannot be given recovery state %q", runID, next)
	}

	if current.Settled() || !next.Settled() {
		return nil
	}

	return fmt.Errorf(
		"state: workflow run %q has recovery state %q and this write would settle it as %q; an ordinary advance cannot decide that an interruption has been dealt with -- a resume-cleanup that accounted for every obligation, or an acknowledgement with an audit reason, is what settles one, and that is what keeps a flag which skips hooks from clearing a recovery it knows nothing about",
		runID, current, next)
}

func currentRunState(ctx context.Context, q execQuerier, runID string) (workflow.State, workflow.RecoveryState, error) {
	var state, recovery string

	err := q.QueryRowContext(ctx,
		`SELECT state, recovery_state FROM workflow_runs WHERE run_id = ?`, runID).Scan(&state, &recovery)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("state: workflow run %q: %w", runID, ErrWorkflowRunNotFound)
	}
	if err != nil {
		return "", "", fmt.Errorf("state: reading workflow run %q: %w", runID, err)
	}

	return workflow.State(state), workflow.RecoveryState(recovery), nil
}

// WorkflowObligationAdvance is one transition of one scope's cleanup
// obligation.
type WorkflowObligationAdvance struct {
	RunID string
	Scope workflow.Scope

	// To is the obligation's new state.
	To workflow.ObligationState

	// At is the moment, and which column it lands in follows from To: the
	// scope's entry time, the cleanup's start, its finish, or the
	// acknowledgement. One parameter rather than four, because a caller
	// that could set them independently could record a cleanup that
	// finished before the scope was entered.
	At time.Time

	// AcknowledgedBy and AcknowledgeReason are required for
	// ObligationAcknowledged and refused for everything else.
	AcknowledgedBy    string
	AcknowledgeReason string
}

// AdvanceWorkflowCleanupObligation applies one transition to one scope's
// obligation.
//
// The transition is checked against internal/workflow's graph and the
// resulting ROW is then validated as a whole
// (workflow.CleanupObligation.Validate), which is what catches the
// combinations a graph cannot express: an acknowledgement with no reason,
// a cleanup that finished before it started, an entered scope with no
// entry time.
func (j *Journal) AdvanceWorkflowCleanupObligation(ctx context.Context, adv WorkflowObligationAdvance) error {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin advance cleanup obligation of run %q: %w", adv.RunID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	if err := advanceObligation(ctx, tx, adv); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit advance of cleanup obligation of run %q: %w", adv.RunID, err)
	}

	return nil
}

func advanceObligation(ctx context.Context, q execQuerier, adv WorkflowObligationAdvance) error {
	if adv.At.IsZero() {
		return fmt.Errorf("state: advancing the %s cleanup obligation of run %q with no time", adv.Scope, adv.RunID)
	}

	current, err := readObligation(ctx, q, adv.RunID, adv.Scope)
	if err != nil {
		return err
	}

	if err := current.State.CanFollow(adv.To); err != nil {
		return fmt.Errorf("state: the %s cleanup obligation of run %q cannot advance: %w", adv.Scope, adv.RunID, err)
	}

	next := current
	next.State = adv.To
	at := adv.At

	switch adv.To {
	case workflow.ObligationEligible:
		next.EnteredAt = &at
	case workflow.ObligationRunning:
		next.StartedAt = &at
	case workflow.ObligationSuccess, workflow.ObligationFailed:
		next.FinishedAt = &at
	case workflow.ObligationAcknowledged:
		next.AcknowledgedAt = &at
		next.AcknowledgedBy = adv.AcknowledgedBy
		next.AcknowledgeReason = adv.AcknowledgeReason
	case workflow.ObligationRecoveryRequired:
		// Nothing new is known. An interruption is precisely the case
		// where there is no outcome to stamp, and inventing a finish
		// time for a cleanup nobody watched would be the record saying
		// something happened at a moment that is only when this process
		// noticed.
	case workflow.ObligationNeverEligible:
		// Unreachable: no transition leads here (obligationTransitions).
		// Stated so that a future edge added to that table fails here
		// rather than writing a row with nothing filled in.
		return fmt.Errorf("state: nothing may move a cleanup obligation back to %q", adv.To)
	}

	if err := next.Validate(); err != nil {
		return fmt.Errorf("state: the %s cleanup obligation of run %q would not be storable: %w", adv.Scope, adv.RunID, err)
	}

	_, err = q.ExecContext(ctx,
		`UPDATE workflow_cleanup_obligations
		    SET state = ?, entered_at = ?, started_at = ?, finished_at = ?,
		        acknowledged_at = ?, acknowledged_by = ?, acknowledge_reason = ?
		  WHERE run_id = ? AND scope = ?`,
		string(next.State), formatTimePtr(next.EnteredAt), formatTimePtr(next.StartedAt),
		formatTimePtr(next.FinishedAt), formatTimePtr(next.AcknowledgedAt),
		next.AcknowledgedBy, next.AcknowledgeReason, adv.RunID, string(adv.Scope))
	if err != nil {
		return fmt.Errorf("state: advancing the %s cleanup obligation of run %q: %w", adv.Scope, adv.RunID, err)
	}

	return nil
}

// obligationColumns is the one column list every obligation read uses.
// Two copies of a ten-column scan is where a column ends up read into the
// wrong field, and every field here is one a refusal is decided on.
const obligationColumns = `run_id, scope, backup_set_id, state, entered_at, started_at,
	        finished_at, acknowledged_at, acknowledged_by, acknowledge_reason`

// scanObligation decodes one row selected with obligationColumns.
// query.go's scanRow (satisfied by *sql.Row and *sql.Rows) is what lets
// the single-row read and the list reads share this one scan.
func scanObligation(row scanRow) (workflow.CleanupObligation, error) {
	var (
		o                                        workflow.CleanupObligation
		scope, setID, state                      string
		entered, started, finished, acknowledged sql.NullString
	)

	if err := row.Scan(&o.RunID, &scope, &setID, &state, &entered, &started,
		&finished, &acknowledged, &o.AcknowledgedBy, &o.AcknowledgeReason); err != nil {
		return workflow.CleanupObligation{}, err
	}

	o.Scope = workflow.Scope(scope)
	o.State = workflow.ObligationState(state)

	setIdentity, err := model.ParseBackupSetID(setID)
	if err != nil {
		return workflow.CleanupObligation{}, fmt.Errorf(
			"state: the %s cleanup obligation of run %q names backup set %q, which cannot be read back: %w",
			scope, o.RunID, setID, err)
	}
	o.BackupSetID = setIdentity

	for _, f := range []struct {
		raw sql.NullString
		dst **time.Time
	}{
		{entered, &o.EnteredAt},
		{started, &o.StartedAt},
		{finished, &o.FinishedAt},
		{acknowledged, &o.AcknowledgedAt},
	} {
		t, err := parseTimePtr(f.raw)
		if err != nil {
			return workflow.CleanupObligation{}, fmt.Errorf(
				"state: the %s cleanup obligation of run %q has an unreadable timestamp: %w", scope, o.RunID, err)
		}
		*f.dst = t
	}

	return o, nil
}

// readObligation reads one scope's obligation back as the domain value.
func readObligation(ctx context.Context, q execQuerier, runID string, scope workflow.Scope) (workflow.CleanupObligation, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+obligationColumns+`
		   FROM workflow_cleanup_obligations
		  WHERE run_id = ? AND scope = ?`, runID, string(scope))

	o, err := scanObligation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return workflow.CleanupObligation{}, fmt.Errorf("state: workflow run %q has no %s cleanup obligation: %w",
			runID, scope, ErrWorkflowObligationNotFound)
	}
	if err != nil {
		return workflow.CleanupObligation{}, err
	}

	return o, nil
}

// WorkflowCleanupObligations reads one run's obligations, global scope
// first.
//
// The order is fixed (by scope name, which puts 'global' before 'set')
// because it is the order the scopes are ENTERED in and the reverse of
// the order they unwind in, and a caller reading them back to decide what
// to run next must not have that depend on insertion order.
func (j *Journal) WorkflowCleanupObligations(ctx context.Context, runID string) ([]workflow.CleanupObligation, error) {
	return j.obligationsWhere(ctx, `run_id = ?`, runID)
}

// ObligationsRequiringRecovery returns every scope, of every run, that
// still needs a person -- which is the read the per-set refusal and the
// health warning are both built on.
//
// The predicate is the Go vocabulary's RequiresRecovery rendered into
// SQL, and idx_workflow_obligations_recovery's partial index uses the
// same literal. TestTheRecoveryIndexMatchesTheStateThatBlocksASet is what
// fails if one moves without the other.
func (j *Journal) ObligationsRequiringRecovery(ctx context.Context) ([]workflow.CleanupObligation, error) {
	return j.obligationsWhere(ctx, `state = ?`, string(workflow.ObligationRecoveryRequired))
}

// obligationsWhere is the list read with a caller-chosen predicate, in
// (run, scope) order.
func (j *Journal) obligationsWhere(ctx context.Context, where string, args ...any) ([]workflow.CleanupObligation, error) {
	rows, err := j.db.QueryContext(ctx,
		`SELECT `+obligationColumns+`
		   FROM workflow_cleanup_obligations
		  WHERE `+where+`
		  ORDER BY run_id, scope`, args...)
	if err != nil {
		return nil, fmt.Errorf("state: reading workflow cleanup obligations: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only

	var out []workflow.CleanupObligation
	for rows.Next() {
		o, err := scanObligation(rows)
		if err != nil {
			return nil, fmt.Errorf("state: reading workflow cleanup obligations: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: reading workflow cleanup obligations: %w", err)
	}

	return out, nil
}

// WorkflowStepReconciliation is one step's part of a reconciliation: the
// state a restart concluded it must be in, and when that was concluded.
type WorkflowStepReconciliation struct {
	StepID string
	State  workflow.State
	At     time.Time
}

// WorkflowReconciliation is everything a restart concluded about ONE run.
//
// It is one value because it has to be one transaction. A reconciliation
// that moved the run and left its steps, or moved the steps and left an
// obligation, would be a reconciliation the NEXT restart reads as a run
// with nothing outstanding -- which is the failure the whole obligation
// record exists to prevent, reintroduced by the code that was supposed to
// close it.
type WorkflowReconciliation struct {
	RunID       string
	Run         WorkflowRunAdvance
	Steps       []WorkflowStepReconciliation
	Obligations []WorkflowObligationAdvance
}

// ApplyWorkflowReconciliation writes one run's whole reconciliation
// atomically, or writes none of it.
//
// Every part goes through the same transition checks an ordinary advance
// does, deliberately: a reconciliation is not privileged, and one that
// could write a state the graph forbids would be a way past every
// guarantee this file makes. What it IS allowed to do that an ordinary
// advance is not, is raise the recovery axis -- which
// AdvanceWorkflowRun's own rule already permits, because that direction
// is the safe one.
func (j *Journal) ApplyWorkflowReconciliation(ctx context.Context, rec WorkflowReconciliation) error {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin reconciling workflow run %q: %w", rec.RunID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	for _, s := range rec.Steps {
		if s.At.IsZero() {
			return fmt.Errorf("state: reconciling step %q of workflow run %q with no time", s.StepID, rec.RunID)
		}
		if err := finishWorkflowStep(ctx, tx, rec.RunID, s.StepID, WorkflowStepOutcome{
			State:      s.State,
			FinishedAt: s.At,
		}); err != nil {
			return err
		}
	}

	for _, o := range rec.Obligations {
		if o.RunID == "" {
			o.RunID = rec.RunID
		}
		if o.RunID != rec.RunID {
			return fmt.Errorf("state: reconciling workflow run %q was handed an obligation for run %q", rec.RunID, o.RunID)
		}
		if err := advanceObligation(ctx, tx, o); err != nil {
			return err
		}
	}

	if !rec.Run.isEmpty() {
		if err := advanceWorkflowRun(ctx, tx, rec.RunID, rec.Run); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit reconciliation of workflow run %q: %w", rec.RunID, err)
	}

	return nil
}

// ResolveWorkflowRecovery settles a run's recovery axis, and it is the
// only call that can.
//
// It refuses unless every one of the run's obligations has reached a
// SETTLED state, which is what makes the refusal structural rather than a
// matter of which caller remembered to check: a resume-cleanup that ran
// every eligible after stage leaves them settled, an acknowledgement
// leaves them acknowledged, and anything else -- including a flag that
// skipped the hooks -- leaves at least one requiring recovery and cannot
// get past this.
func (j *Journal) ResolveWorkflowRecovery(ctx context.Context, runID string, to workflow.State, at time.Time) error {
	if at.IsZero() {
		return fmt.Errorf("state: resolving the recovery of workflow run %q with no time", runID)
	}

	obligations, err := j.WorkflowCleanupObligations(ctx, runID)
	if err != nil {
		return err
	}
	if len(obligations) == 0 {
		return fmt.Errorf("state: workflow run %q has no cleanup obligations, so there is nothing here to have resolved: %w", runID, ErrWorkflowObligationNotFound)
	}

	for _, o := range obligations {
		if !o.State.Settled() {
			return fmt.Errorf(
				"%w: workflow run %q has its %s cleanup obligation at %q, and a run is out of recovery only when every scope it entered has been accounted for -- run the remaining cleanup, or acknowledge it with a reason",
				ErrRecoveryOutstanding, runID, o.Scope, o.State)
		}
	}

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin resolving the recovery of workflow run %q: %w", runID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	current, _, err := currentRunState(ctx, tx, runID)
	if err != nil {
		return err
	}
	if err := current.CanFollowForRun(to); err != nil {
		return fmt.Errorf("state: workflow run %q cannot be resolved: %w", runID, err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE workflow_runs
		    SET state = ?, recovery_state = ?, finished_at = ?
		  WHERE run_id = ?`,
		string(to), string(workflow.RecoveryResolved), formatTime(at), runID); err != nil {
		return fmt.Errorf("state: resolving the recovery of workflow run %q: %w", runID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit the resolution of workflow run %q: %w", runID, err)
	}

	return nil
}

// WorkflowRunsInStates returns every run in one of the given states, in
// start order.
//
// The states are the caller's, spelled in internal/workflow's vocabulary,
// for LocalBytesInUse's reason: which states are "open" is a domain
// question, and a predicate written here would be a second opinion about
// State.Terminal that nothing keeps in step with it.
func (j *Journal) WorkflowRunsInStates(ctx context.Context, states ...workflow.State) ([]WorkflowRun, error) {
	if len(states) == 0 {
		return nil, nil
	}

	placeholders := ""
	args := make([]any, 0, len(states))
	for i, s := range states {
		if i > 0 {
			placeholders += ", "
		}
		placeholders += "?"
		args = append(args, string(s))
	}

	return j.workflowRunsWhere(ctx, `state IN (`+placeholders+`)`, args...)
}

// WorkflowRunsRequiringRecovery returns every run whose recovery axis is
// unsettled, in start order: the startup pass's read, and the one that
// answers "may this set run" once filtered by set.
//
// The predicate names the two unsettled states rather than saying "not
// none", which is what 0012's partial index says as well and for the same
// reason: a state added to the vocabulary later is settled or unsettled
// by somebody's decision, and <> 'none' would enrol it silently.
func (j *Journal) WorkflowRunsRequiringRecovery(ctx context.Context) ([]WorkflowRun, error) {
	return j.workflowRunsWhere(ctx, `recovery_state IN (?, ?)`,
		string(workflow.RecoveryRequired), string(workflow.RecoveryInProgress))
}

func (j *Journal) workflowRunsWhere(ctx context.Context, where string, args ...any) ([]WorkflowRun, error) {
	rows, err := j.db.QueryContext(ctx,
		`SELECT run_id, backup_set_id, state, started_at, finished_at,
		        backup_status, cleanup_status, workflow_status, recovery_state,
		        resolved_plan_hash, script_spool_ref, bypassed
		   FROM workflow_runs
		  WHERE `+where+`
		  ORDER BY started_at, run_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("state: reading workflow runs: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only

	var out []WorkflowRun
	for rows.Next() {
		var (
			r        WorkflowRun
			started  string
			finished sql.NullString
		)

		if err := rows.Scan(&r.RunID, &r.BackupSetID, &r.State, &started, &finished,
			&r.BackupStatus, &r.CleanupStatus, &r.WorkflowStatus, &r.RecoveryState,
			&r.ResolvedPlanHash, &r.ScriptSpoolRef, &r.Bypassed); err != nil {
			return nil, fmt.Errorf("state: reading workflow runs: %w", err)
		}

		if r.StartedAt, err = parseTime(started); err != nil {
			return nil, fmt.Errorf("state: workflow run %q has an unreadable start time: %w", r.RunID, err)
		}
		if r.FinishedAt, err = parseTimePtr(finished); err != nil {
			return nil, fmt.Errorf("state: workflow run %q has an unreadable finish time: %w", r.RunID, err)
		}

		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: reading workflow runs: %w", err)
	}

	return out, nil
}

// AppendWorkflowStepLog records one piece of a step's captured output.
//
// The bytes have already been redacted by the time they reach here (see
// 0013_workflow_lifecycle.sql and obs.StreamFilter): this is the durable
// store, not the place the filtering happens, because a filter applied
// here would be a filter applied per row and unable to catch a needle
// split across two reads of a pipe.
//
// A repeated sequence number is refused by the schema's UNIQUE and
// reported as such, because a follower's cursor is only a cursor if one
// number names one record.
func (j *Journal) AppendWorkflowStepLog(ctx context.Context, rec workflow.StepLog) error {
	if err := rec.Validate(); err != nil {
		return err
	}

	_, err := j.db.ExecContext(ctx,
		`INSERT INTO workflow_step_logs (run_id, step_id, seq, kind, stream, captured_at, payload)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rec.RunID, rec.StepID, int64(rec.Seq), string(rec.Kind), rec.Stream,
		formatTime(rec.CapturedAt), rec.Payload)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("state: workflow run %q already has a log record at sequence %d; one sequence number names one record, because that is what makes a follower's cursor resumable",
				rec.RunID, rec.Seq)
		}

		return fmt.Errorf("state: recording the output of step %q of workflow run %q: %w", rec.StepID, rec.RunID, err)
	}

	return nil
}

// WorkflowStepLogsAfter returns one run's log records with a sequence
// number above afterSeq, oldest first, up to limit of them.
//
// This is the replay a reconnecting follower makes, and the bound is what
// makes it safe to offer: a `set -x` in a loop is a megabyte a second, so
// a read with no limit is a read that serves an incident's worth of
// output in one allocation. A caller pages by passing the last sequence
// it received.
func (j *Journal) WorkflowStepLogsAfter(ctx context.Context, runID string, afterSeq uint64, limit int) ([]workflow.StepLog, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("state: reading the log of workflow run %q needs a positive limit; a read with no bound over a table nothing prunes has no honest meaning", runID)
	}

	rows, err := j.db.QueryContext(ctx,
		`SELECT step_id, seq, kind, stream, captured_at, payload
		   FROM workflow_step_logs
		  WHERE run_id = ? AND seq > ?
		  ORDER BY seq
		  LIMIT ?`, runID, int64(afterSeq), limit)
	if err != nil {
		return nil, fmt.Errorf("state: reading the log of workflow run %q: %w", runID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only

	var out []workflow.StepLog
	for rows.Next() {
		rec := workflow.StepLog{RunID: runID}

		var (
			seq        int64
			kind       string
			capturedAt string
		)

		if err := rows.Scan(&rec.StepID, &seq, &kind, &rec.Stream, &capturedAt, &rec.Payload); err != nil {
			return nil, fmt.Errorf("state: reading the log of workflow run %q: %w", runID, err)
		}

		rec.Seq = uint64(seq)
		rec.Kind = workflow.LogKind(kind)
		if rec.CapturedAt, err = parseTime(capturedAt); err != nil {
			return nil, fmt.Errorf("state: a log record of workflow run %q has an unreadable capture time: %w", runID, err)
		}

		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: reading the log of workflow run %q: %w", runID, err)
	}

	return out, nil
}
