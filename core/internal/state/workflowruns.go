package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The workflow plan's durable side (EPIC L, #808): one run row per backup
// cycle that had hooks, one step row per script that cycle decided to run.
//
// This package interprets none of it. state, scope, phase, target and the
// three statuses are all plain strings here for the reason types.go states
// about the FR-10 lifecycle vocabulary: the vocabulary belongs to
// internal/workflow, and a second opinion about it in a lower layer is how
// two vocabularies drift apart. What this file enforces is STRUCTURE --
// the fields without which a row is silently useless, and the atomicity
// of a plan -- and 0012_workflow_runs.sql carries the argument for the
// shape.
//
// The one rule worth reading before changing anything here: a plan is
// committed in ONE transaction, run row and step rows together. "Durably
// committed before any hook may execute" is a claim about all of it, and a
// crash between the run and its steps would leave a run row pointing at a
// script spool with no steps in it -- which a recovery pass reads as "this
// run had nothing to do", while the spool on disk says otherwise.

// ErrWorkflowRunNotFound is returned by every read that names a run id
// with no row behind it. It is a value, like this journal's other
// refusals, because no caller needs structure out of it -- only the
// ability to tell it from a run that is there.
var ErrWorkflowRunNotFound = errors.New("state: workflow run not found")

// WorkflowRun is one workflow run's row, read back exactly as stored.
//
// Every vocabulary field is a plain string. See this file's preamble.
type WorkflowRun struct {
	RunID string

	// BackupSetID is the rendered model.BackupSetID ("source/set"). It is
	// a string here because this package does not import a set identity
	// to re-derive; the caller parses it back with
	// model.ParseBackupSetID if it needs the parts.
	BackupSetID string

	State     string
	StartedAt time.Time

	// FinishedAt is nil while the run is open. Nil is "still running",
	// never "finished at the zero time".
	FinishedAt *time.Time

	BackupStatus   string
	CleanupStatus  string
	WorkflowStatus string
	RecoveryState  string

	ResolvedPlanHash string
	ScriptSpoolRef   string
}

// WorkflowStep is one step's row.
type WorkflowStep struct {
	StepID string

	// Order is the step's position in the plan, counted from zero across
	// every stage. It is what WorkflowSteps orders by, because the order
	// is the plan.
	Order int

	Scope string
	Phase string

	ScriptName   string
	ScriptSHA256 string
	ScriptSize   int64
	Target       string

	ExecutionConnectionRef string

	State   string
	Timeout time.Duration

	StartedAt  *time.Time
	FinishedAt *time.Time

	// ExitCode is nil unless a process exited and this product observed
	// the status. Nil and 0 are not the same answer.
	ExitCode *int

	TerminationConfirmed bool

	StdoutLogRef string
	StderrLogRef string

	SpoolRef string
}

// WorkflowPlan is a run and the complete, ordered set of steps it will
// execute: the unit CommitWorkflowPlan writes, because it is the unit
// that has to be durable before a hook runs.
type WorkflowPlan struct {
	Run   WorkflowRun
	Steps []WorkflowStep
}

// CommitWorkflowPlan durably records one run and all of its steps in a
// single transaction.
//
// It is the only write that creates a run, and it refuses a run id this
// journal already has. A replay is NOT resolved to the existing row the
// way PlaceSnapshotHold resolves a repeated hold id: a hold is one
// caller's idempotent request about an existing snapshot, while this is a
// plan, and two plans for one run id means either the steps would be
// duplicated or the run's start time would move -- and the start time is
// what a recovery pass reads to decide how long somebody has been waiting.
//
// Every structural refusal happens before the transaction opens, so a
// refused plan has touched nothing.
func (j *Journal) CommitWorkflowPlan(ctx context.Context, plan WorkflowPlan) error {
	if err := validateWorkflowPlan(plan); err != nil {
		return err
	}

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin commit workflow plan %q: %w", plan.Run.RunID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	r := plan.Run

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workflow_runs
		   (run_id, backup_set_id, state, started_at, finished_at,
		    backup_status, cleanup_status, workflow_status, recovery_state,
		    resolved_plan_hash, script_spool_ref)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RunID, r.BackupSetID, r.State, formatTime(r.StartedAt), formatTimePtr(r.FinishedAt),
		r.BackupStatus, r.CleanupStatus, r.WorkflowStatus, r.RecoveryState,
		r.ResolvedPlanHash, r.ScriptSpoolRef,
	); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf(
				"state: workflow run %q is already recorded, and a run's plan is written once: committing a second one would either duplicate its steps or move the start time a recovery pass reads",
				r.RunID)
		}

		return fmt.Errorf("state: recording workflow run %q: %w", r.RunID, err)
	}

	for _, s := range plan.Steps {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_steps
			   (run_id, step_id, step_order, scope, phase, script_name, script_sha256,
			    script_size, target, execution_connection_ref, state, timeout_seconds,
			    started_at, finished_at, exit_code, termination_confirmed,
			    stdout_log_ref, stderr_log_ref, spool_ref)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.RunID, s.StepID, s.Order, s.Scope, s.Phase, s.ScriptName, s.ScriptSHA256,
			s.ScriptSize, s.Target, s.ExecutionConnectionRef, s.State, int64(s.Timeout/time.Second),
			formatTimePtr(s.StartedAt), formatTimePtr(s.FinishedAt), s.ExitCode, s.TerminationConfirmed,
			s.StdoutLogRef, s.StderrLogRef, s.SpoolRef,
		); err != nil {
			return fmt.Errorf("state: recording workflow step %q of run %q: %w", s.StepID, r.RunID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit workflow plan %q: %w", r.RunID, err)
	}

	return nil
}

// validateWorkflowPlan refuses a plan that would be silently useless once
// written.
//
// It stops at the first problem rather than collecting, unlike
// config.Validate: the caller here is this product's own code assembling a
// plan it controls, so a refusal is a bug report and the first one is the
// whole diagnosis.
//
// It checks STRUCTURE only. Whether "pending" is a state a step may be in
// is internal/workflow's question, and Step.Validate there is what answers
// it; duplicating the vocabulary in this layer would be a second authority
// on it.
func validateWorkflowPlan(plan WorkflowPlan) error {
	r := plan.Run

	switch {
	case r.RunID == "":
		return errors.New("state: committing a workflow plan requires a run id; it is how the plan is found again after a restart, and it is the spool directory's name")
	case r.BackupSetID == "":
		return fmt.Errorf("state: workflow run %q names no backup set; a run not attributable to a set cannot be retained, recovered or reported", r.RunID)
	case r.State == "":
		return fmt.Errorf("state: workflow run %q has no state", r.RunID)
	case r.StartedAt.IsZero():
		return fmt.Errorf("state: workflow run %q has no start time; how long a run has been open is what a recovery pass reads", r.RunID)
	case r.ResolvedPlanHash == "":
		return fmt.Errorf("state: workflow run %q has no resolved plan hash; the hash is what makes a plan auditable and a recovery reproducible", r.RunID)
	case r.ScriptSpoolRef == "":
		return fmt.Errorf("state: workflow run %q has no script spool; execution reads scripts from the spool and nowhere else, so a run without one can run nothing", r.RunID)
	}

	seenIDs := map[string]bool{}
	seenOrders := map[int]bool{}

	for i, s := range plan.Steps {
		switch {
		case s.StepID == "":
			return fmt.Errorf("state: workflow run %q has a step at index %d with no step id; the step id is the spooled script's filename", r.RunID, i)
		case s.ScriptName == "":
			return fmt.Errorf("state: workflow step %q of run %q names no script", s.StepID, r.RunID)
		case s.ScriptSHA256 == "":
			return fmt.Errorf("state: workflow step %q of run %q has no script hash; the hash is what proves the spooled bytes are the ones that passed validation", s.StepID, r.RunID)
		case s.Target == "":
			return fmt.Errorf("state: workflow step %q of run %q does not say where it runs", s.StepID, r.RunID)
		case s.State == "":
			return fmt.Errorf("state: workflow step %q of run %q has no state", s.StepID, r.RunID)
		case s.SpoolRef == "":
			return fmt.Errorf("state: workflow step %q of run %q has no spooled script; execution reads from the spool and nowhere else", s.StepID, r.RunID)
		case s.Order < 0:
			return fmt.Errorf("state: workflow step %q of run %q has order %d", s.StepID, r.RunID, s.Order)
		case seenIDs[s.StepID]:
			return fmt.Errorf("state: workflow run %q has two steps with id %q, which would share one spooled script file", r.RunID, s.StepID)
		case seenOrders[s.Order]:
			return fmt.Errorf("state: workflow run %q has two steps claiming order %d; the order IS the plan, so two steps in one position is a plan with no defined execution sequence", r.RunID, s.Order)
		}

		seenIDs[s.StepID] = true
		seenOrders[s.Order] = true
	}

	return nil
}

// WorkflowRun reads one run back, or returns ErrWorkflowRunNotFound.
func (j *Journal) WorkflowRun(ctx context.Context, runID string) (WorkflowRun, error) {
	row := j.db.QueryRowContext(ctx,
		`SELECT run_id, backup_set_id, state, started_at, finished_at,
		        backup_status, cleanup_status, workflow_status, recovery_state,
		        resolved_plan_hash, script_spool_ref
		   FROM workflow_runs WHERE run_id = ?`, runID)

	var (
		r          WorkflowRun
		startedAt  string
		finishedAt sql.NullString
	)

	err := row.Scan(&r.RunID, &r.BackupSetID, &r.State, &startedAt, &finishedAt,
		&r.BackupStatus, &r.CleanupStatus, &r.WorkflowStatus, &r.RecoveryState,
		&r.ResolvedPlanHash, &r.ScriptSpoolRef)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowRun{}, fmt.Errorf("%w: %s", ErrWorkflowRunNotFound, runID)
	}
	if err != nil {
		return WorkflowRun{}, fmt.Errorf("state: reading workflow run %q: %w", runID, err)
	}

	if r.StartedAt, err = parseTime(startedAt); err != nil {
		return WorkflowRun{}, fmt.Errorf("state: workflow run %q has an unreadable start time %q: %w", runID, startedAt, err)
	}

	if r.FinishedAt, err = parseTimePtr(finishedAt); err != nil {
		return WorkflowRun{}, fmt.Errorf("state: workflow run %q has an unreadable finish time: %w", runID, err)
	}

	return r, nil
}

// WorkflowSteps reads one run's steps back IN PLAN ORDER.
//
// The ordering is the whole contract: the order is the plan, and a read
// that returned rows in insertion or rowid order would let a recovery pass
// run an "after" hook before a "before" one. A run this journal does not
// have returns no steps and no error, because "no steps" is the honest
// answer to "what does this run execute" and WorkflowRun is the call that
// distinguishes a missing run.
func (j *Journal) WorkflowSteps(ctx context.Context, runID string) ([]WorkflowStep, error) {
	rows, err := j.db.QueryContext(ctx,
		`SELECT step_id, step_order, scope, phase, script_name, script_sha256, script_size,
		        target, execution_connection_ref, state, timeout_seconds,
		        started_at, finished_at, exit_code, termination_confirmed,
		        stdout_log_ref, stderr_log_ref, spool_ref
		   FROM workflow_steps WHERE run_id = ? ORDER BY step_order`, runID)
	if err != nil {
		return nil, fmt.Errorf("state: reading the steps of workflow run %q: %w", runID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only

	var out []WorkflowStep

	for rows.Next() {
		var (
			s              WorkflowStep
			timeoutSeconds int64
			startedAt      sql.NullString
			finishedAt     sql.NullString
			exitCode       sql.NullInt64
		)

		if err := rows.Scan(&s.StepID, &s.Order, &s.Scope, &s.Phase, &s.ScriptName, &s.ScriptSHA256,
			&s.ScriptSize, &s.Target, &s.ExecutionConnectionRef, &s.State, &timeoutSeconds,
			&startedAt, &finishedAt, &exitCode, &s.TerminationConfirmed,
			&s.StdoutLogRef, &s.StderrLogRef, &s.SpoolRef); err != nil {
			return nil, fmt.Errorf("state: scanning a step of workflow run %q: %w", runID, err)
		}

		s.Timeout = time.Duration(timeoutSeconds) * time.Second

		if s.StartedAt, err = parseTimePtr(startedAt); err != nil {
			return nil, fmt.Errorf("state: workflow step %q of run %q has an unreadable start time: %w", s.StepID, runID, err)
		}
		if s.FinishedAt, err = parseTimePtr(finishedAt); err != nil {
			return nil, fmt.Errorf("state: workflow step %q of run %q has an unreadable finish time: %w", s.StepID, runID, err)
		}
		if exitCode.Valid {
			code := int(exitCode.Int64)
			s.ExitCode = &code
		}

		out = append(out, s)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: reading the steps of workflow run %q: %w", runID, err)
	}

	return out, nil
}

// formatTimePtr and parseTimePtr are formatTime and parseTime for the
// columns where NULL is a meaningful answer.
//
// They exist because "not yet" and "the zero time" are different facts
// throughout this schema, and a nil check written inline at each of the
// six call sites is six places to get it the wrong way round.
func formatTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}

	return formatTime(*t)
}

func parseTimePtr(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}

	parsed, err := parseTime(s.String)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", s.String, err)
	}

	return &parsed, nil
}
