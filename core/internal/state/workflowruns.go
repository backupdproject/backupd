package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/secretref"
	"github.com/backupdproject/backupd/core/internal/workflow"
)

// The workflow plan's durable side (EPIC L, #808): one run row per backup
// cycle that had hooks, one step row per script that cycle decided to run.
//
// This package STORES the vocabulary as plain strings and does not invent
// it: state, scope, phase, target and the three statuses are columns of
// text, for the reason types.go states about the FR-10 lifecycle
// vocabulary -- the vocabulary belongs to internal/workflow, and a second
// opinion about it in a lower layer is how two vocabularies drift apart.
//
// It does not follow that the write path may take the vocabulary on trust,
// and the first version of this file made exactly that mistake: it
// accepted a plan of plain strings and checked only that they were
// non-empty. "state" of "whatever", a step whose target said local and
// whose script name said remote, a timeout of zero -- all committed, all
// read back by a recovery pass after a restart, and a recovery pass has no
// second chance to notice. So CommitWorkflowPlan takes the DOMAIN types
// and calls their own Validate before the transaction opens: the
// vocabulary is still internal/workflow's, and this journal refuses to
// store anything that package would not have produced.
//
// What this file adds on top is STRUCTURE -- the fields without which a
// row is silently useless, and the atomicity of a plan -- and
// 0012_workflow_runs.sql carries the argument for the shape.
//
// The one rule worth reading before changing anything here: a plan is
// committed in ONE transaction, run row and step rows together. "Durably
// committed before any hook may execute" is a claim about all of it, and a
// crash between the run and its steps would leave a run row pointing at a
// script spool with no steps in it -- which a recovery pass reads as "this
// run had nothing to do", while the spool on disk says otherwise.

// workflowScriptsDirName is the subdirectory of a run's spool that holds
// the captured scripts. internal/workflow creates it; this is the copy the
// cross-check below compares against, and the two are pinned together by
// TestTheSpoolLayoutThisJournalChecksIsTheOneWorkflowCreates.
const workflowScriptsDirName = "scripts"

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

	// Bypassed is #811's history field: this run's hooks were
	// deliberately skipped. Written once, with the plan; see
	// 0013_workflow_lifecycle.sql for why there is no write that turns
	// it on later.
	Bypassed bool
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

	State string

	// Timeout is the resolved per-step bound, stored in NANOSECONDS.
	// Whole seconds were what this column first held, and 500ms became
	// 0: a bound that had been configured and then silently was not.
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

// WorkflowPlan is a run, the complete ordered set of steps it will
// execute, and the environment it will execute them with: the unit
// CommitWorkflowPlan writes, because it is the unit that has to be durable
// before a hook runs.
//
// They are DOMAIN types, not this package's row shapes. A journal that
// accepted the row shapes here would be a journal whose write path cannot
// tell a plan from a set of strings; see this file's preamble.
type WorkflowPlan struct {
	Run   workflow.Run
	Steps []workflow.Step

	// Env is the plan's UNRESOLVED environment: literals as the operator
	// wrote them and secrets as LOCATIONS. It is persisted with the run
	// because a recovery after a config edit has to finish the run that
	// was planned, not the run today's configuration would plan. See
	// 0012_workflow_runs.sql's argument on workflow_run_env, and note
	// what it forbids: the value a location resolves to is never written
	// anywhere.
	Env workflow.Environment

	// Obligations are the run's cleanup obligations, one per scope, in
	// the state the run BEGINS in: the global scope already eligible
	// (the run has begun, so that scope is entered), the backup-set
	// scope not yet (#811's nested rule -- it is entered only once
	// global-before has succeeded).
	//
	// They are committed in the same transaction as the run and its
	// steps, and that is the point rather than tidiness. "The obligation
	// is durably written before the first side-effecting command in a
	// scope" is the claim the whole crash-safety argument rests on, and
	// for the global scope the first side-effecting command is the run's
	// first hook -- so the only write that is unambiguously before it is
	// the one that makes the run exist at all.
	Obligations []workflow.CleanupObligation
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
		return fmt.Errorf("state: begin commit workflow plan %q: %w", plan.Run.ID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	r := plan.Run

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workflow_runs
		   (run_id, backup_set_id, state, started_at, finished_at,
		    backup_status, cleanup_status, workflow_status, recovery_state,
		    resolved_plan_hash, script_spool_ref, bypassed)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.BackupSetID.String(), string(r.State), formatTime(r.StartedAt), formatTimePtr(r.FinishedAt),
		string(r.BackupStatus), string(r.CleanupStatus), string(r.WorkflowStatus), string(r.RecoveryState),
		r.ResolvedPlanHash, r.ScriptSpoolRef, r.Bypassed,
	); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf(
				"state: workflow run %q is already recorded, and a run's plan is written once: committing a second one would either duplicate its steps or move the start time a recovery pass reads",
				r.ID)
		}

		return fmt.Errorf("state: recording workflow run %q: %w", r.ID, err)
	}

	for _, s := range plan.Steps {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_steps
			   (run_id, step_id, step_order, scope, phase, script_name, script_sha256,
			    script_size, target, execution_connection_ref, state, timeout_nanos,
			    started_at, finished_at, exit_code, termination_confirmed,
			    stdout_log_ref, stderr_log_ref, spool_ref)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, s.ID, s.Order, string(s.Scope), string(s.Phase), s.ScriptName, s.ScriptSHA256,
			s.ScriptSize, string(s.Target), s.ExecutionConnectionRef, string(s.State), int64(s.Timeout),
			formatTimePtr(s.StartedAt), formatTimePtr(s.FinishedAt), s.ExitCode, s.TerminationConfirmed,
			s.StdoutLogRef, s.StderrLogRef, s.SpoolRef,
		); err != nil {
			return fmt.Errorf("state: recording workflow step %q of run %q: %w", s.ID, r.ID, err)
		}
	}

	// The environment goes in the SAME transaction as the run and its
	// steps, for the reason the steps do: a recovery pass that found a
	// run with steps and no environment could not tell "this run had no
	// variables" from "this run's variables were not written", and one of
	// those is a hook executed with a different environment than the one
	// the plan was hashed over.
	for i, v := range plan.Env.Vars() {
		literal, file, env, command, err := envColumns(v)
		if err != nil {
			return fmt.Errorf("state: recording the environment of workflow run %q: %w", r.ID, err)
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_run_env
			   (run_id, position, name, literal_value, secret_file, secret_env, secret_command)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.ID, i, v.Name, literal, file, env, command,
		); err != nil {
			return fmt.Errorf("state: recording environment variable %q of workflow run %q: %w", v.Name, r.ID, err)
		}
	}

	// The obligations go in the SAME transaction, for the reason the
	// steps and the environment do, and for one more that is specific to
	// them: an obligation is the record that a scope has been entered,
	// and this transaction is what makes the run exist. A crash between
	// the two would leave a run with a first hook about to execute and
	// no record that the scope it executes in is owed a cleanup.
	for _, o := range plan.Obligations {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_cleanup_obligations
			   (run_id, scope, backup_set_id, state, entered_at, started_at, finished_at,
			    acknowledged_at, acknowledged_by, acknowledge_reason)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, string(o.Scope), o.BackupSetID.String(), string(o.State),
			formatTimePtr(o.EnteredAt), formatTimePtr(o.StartedAt), formatTimePtr(o.FinishedAt),
			formatTimePtr(o.AcknowledgedAt), o.AcknowledgedBy, o.AcknowledgeReason,
		); err != nil {
			return fmt.Errorf("state: recording the %s cleanup obligation of workflow run %q: %w", o.Scope, r.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit workflow plan %q: %w", r.ID, err)
	}

	return nil
}

// envColumns renders one environment entry as the four columns that carry
// it, or refuses one that cannot be stored.
//
// literal_value is NULL for a secret-backed variable and the literal
// (including "") otherwise, because an empty literal is a real
// configuration and a column that could not tell it from "no literal"
// would turn it into a secret with no source.
//
// The argv is JSON rather than a joined string: an argument may contain
// any byte, so any separator chosen here would be one two different argvs
// could come back through as the same command. That is the same mistake
// the plan hash made before it was length-prefixed, one layer down.
func envColumns(v workflow.EnvVar) (literal any, file, env, command string, err error) {
	if !v.IsSecret() {
		return v.Value, "", "", "", nil
	}

	if len(v.Secret.Command) != 0 {
		encoded, mErr := json.Marshal(v.Secret.Command)
		if mErr != nil {
			return nil, "", "", "", fmt.Errorf("encoding the argv of %s: %w", v.Name, mErr)
		}

		command = string(encoded)
	}

	return nil, v.Secret.File, v.Secret.Env, command, nil
}

// validateWorkflowPlan refuses a plan this journal must not store.
//
// It asks the DOMAIN types their own question first -- Run.Validate and
// Step.Validate, which own the closed vocabularies, the script-name rule,
// the name-against-target agreement and the derived step id -- and then
// adds the two things only a journal can know: that the steps belong to
// the run being committed, and that no two of them would share a row or a
// position.
//
// It stops at the first problem rather than collecting, unlike
// config.Validate: the caller here is this product's own code assembling a
// plan it controls, so a refusal is a bug report and the first one is the
// whole diagnosis.
func validateWorkflowPlan(plan WorkflowPlan) error {
	r := plan.Run

	if err := r.Validate(); err != nil {
		return fmt.Errorf("state: refusing to record this workflow run: %w", err)
	}

	seenIDs := map[string]bool{}
	seenOrders := map[int]bool{}

	for i, s := range plan.Steps {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("state: refusing to record step %d of workflow run %q: %w", i, r.ID, err)
		}

		switch {
		case s.RunID != r.ID:
			return fmt.Errorf(
				"state: step %q belongs to run %q and is being committed as part of run %q; a step attributed to the wrong run is a side effect nothing can place",
				s.ID, s.RunID, r.ID)
		case s.SpoolRef != filepath.Join(r.ScriptSpoolRef, workflowScriptsDirName, s.ID):
			// The run record and the steps are assembled by two
			// different pieces of the caller (a run starts before its
			// first hook does), so this is the one place the two halves
			// can be checked against each other. A step whose spooled
			// script is not inside the run's own spool is a plan
			// somebody has mixed with another one, and it is exactly
			// what workflow.RecoverPlan will refuse to read back --
			// better to refuse the write.
			return fmt.Errorf(
				"state: step %q of run %q names the spooled script %s, and this run's spool holds it at %s",
				s.ID, r.ID, s.SpoolRef, filepath.Join(r.ScriptSpoolRef, workflowScriptsDirName, s.ID))
		case seenIDs[s.ID]:
			return fmt.Errorf("state: workflow run %q has two steps with id %q, which would share one spooled script file", r.ID, s.ID)
		case seenOrders[s.Order]:
			return fmt.Errorf("state: workflow run %q has two steps claiming order %d; the order IS the plan, so two steps in one position is a plan with no defined execution sequence", r.ID, s.Order)
		}

		seenIDs[s.ID] = true
		seenOrders[s.Order] = true
	}

	return validatePlanObligations(plan)
}

// validatePlanObligations refuses an obligation set a run cannot be
// committed with.
//
// The requirement that there is EXACTLY one obligation per scope is the
// load-bearing one. A run missing the set scope's row would be a run
// whose set-scope cleanup can never be marked eligible -- so a crash
// after the backup-set "before" stage had already quiesced something
// would reconcile to "nothing outstanding", which is the one outcome the
// whole record exists to prevent. A run with no obligations at all is
// refused for the same reason rather than treated as "hooks with no
// cleanup", because every scope a run enters is a scope it can be
// interrupted in.
func validatePlanObligations(plan WorkflowPlan) error {
	r := plan.Run

	byScope := map[workflow.Scope]bool{}
	for _, o := range plan.Obligations {
		if err := o.Validate(); err != nil {
			return fmt.Errorf("state: refusing to record a cleanup obligation of workflow run %q: %w", r.ID, err)
		}

		switch {
		case o.RunID != r.ID:
			return fmt.Errorf(
				"state: the %s cleanup obligation of run %q is being committed as part of run %q; an obligation attributed to the wrong run is a promise nothing can discharge",
				o.Scope, o.RunID, r.ID)
		case o.BackupSetID != r.BackupSetID:
			return fmt.Errorf(
				"state: the %s cleanup obligation of run %q names backup set %s and the run names %s; the refusal an obligation drives is per set, so the two disagreeing would block the wrong one",
				o.Scope, r.ID, o.BackupSetID, r.BackupSetID)
		case byScope[o.Scope]:
			return fmt.Errorf("state: workflow run %q has two %s cleanup obligations, which is two answers to whether that scope is owed its cleanup", r.ID, o.Scope)
		}

		byScope[o.Scope] = true
	}

	for _, scope := range workflow.Scopes() {
		if !byScope[scope] {
			return fmt.Errorf(
				"state: workflow run %q is being committed with no %s cleanup obligation; every scope a run can enter needs its row before the run starts, because a scope with no row reconciles after a crash as one that was never entered -- which is how a quiesced database gets reported as clean",
				r.ID, scope)
		}
	}

	return nil
}

// WorkflowRun reads one run back, or returns ErrWorkflowRunNotFound.
func (j *Journal) WorkflowRun(ctx context.Context, runID string) (WorkflowRun, error) {
	row := j.db.QueryRowContext(ctx,
		`SELECT run_id, backup_set_id, state, started_at, finished_at,
		        backup_status, cleanup_status, workflow_status, recovery_state,
		        resolved_plan_hash, script_spool_ref, bypassed
		   FROM workflow_runs WHERE run_id = ?`, runID)

	var (
		r          WorkflowRun
		startedAt  string
		finishedAt sql.NullString
	)

	err := row.Scan(&r.RunID, &r.BackupSetID, &r.State, &startedAt, &finishedAt,
		&r.BackupStatus, &r.CleanupStatus, &r.WorkflowStatus, &r.RecoveryState,
		&r.ResolvedPlanHash, &r.ScriptSpoolRef, &r.Bypassed)
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
		        target, execution_connection_ref, state, timeout_nanos,
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
			s            WorkflowStep
			timeoutNanos int64
			startedAt    sql.NullString
			finishedAt   sql.NullString
			exitCode     sql.NullInt64
		)

		if err := rows.Scan(&s.StepID, &s.Order, &s.Scope, &s.Phase, &s.ScriptName, &s.ScriptSHA256,
			&s.ScriptSize, &s.Target, &s.ExecutionConnectionRef, &s.State, &timeoutNanos,
			&startedAt, &finishedAt, &exitCode, &s.TerminationConfirmed,
			&s.StdoutLogRef, &s.StderrLogRef, &s.SpoolRef); err != nil {
			return nil, fmt.Errorf("state: scanning a step of workflow run %q: %w", runID, err)
		}

		s.Timeout = time.Duration(timeoutNanos)

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

// RecoverWorkflowPlan reads one run back as the plan a recovery pass may
// execute, or returns ErrWorkflowRunNotFound.
//
// This is the read that makes the durable half worth having, and it is
// deliberately NOT "the row shapes with the strings in them". It rebuilds
// the DOMAIN plan and hands it to workflow.RecoverPlan, which re-validates
// every vocabulary value, re-derives every step id, re-checks that each
// step's target agrees with its script name and that each spooled script
// is inside this run's own spool -- and returns a value whose only way to
// reach a script is the capability that verifies the bytes against the
// recorded sha256.
//
// The point is that everything between the commit and here is a file
// another process may have edited. A recovery pass that trusted these rows
// would be a recovery pass that executes whatever the database says, which
// after a restart is the one thing nobody has checked.
//
// The environment comes from workflow_run_env rather than from today's
// configuration, which is the whole reason that table exists: an operator
// who edited or deleted a variable while the daemon was down must not
// change what an interrupted run finishes with.
func (j *Journal) RecoverWorkflowPlan(ctx context.Context, runID string) (workflow.Plan, error) {
	row, err := j.WorkflowRun(ctx, runID)
	if err != nil {
		return workflow.Plan{}, err
	}

	setID, err := model.ParseBackupSetID(row.BackupSetID)
	if err != nil {
		return workflow.Plan{}, fmt.Errorf("state: workflow run %q names the backup set %q, which cannot be read back: %w", runID, row.BackupSetID, err)
	}

	stepRows, err := j.WorkflowSteps(ctx, runID)
	if err != nil {
		return workflow.Plan{}, err
	}

	steps := make([]workflow.Step, 0, len(stepRows))
	for _, s := range stepRows {
		steps = append(steps, workflow.Step{
			ID:                     s.StepID,
			RunID:                  runID,
			Scope:                  workflow.Scope(s.Scope),
			Phase:                  workflow.Phase(s.Phase),
			Order:                  s.Order,
			ScriptName:             s.ScriptName,
			ScriptSHA256:           s.ScriptSHA256,
			ScriptSize:             s.ScriptSize,
			Target:                 workflow.Target(s.Target),
			ExecutionConnectionRef: s.ExecutionConnectionRef,
			Timeout:                s.Timeout,
			SpoolRef:               s.SpoolRef,
			State:                  workflow.State(s.State),
			StartedAt:              s.StartedAt,
			FinishedAt:             s.FinishedAt,
			ExitCode:               s.ExitCode,
			TerminationConfirmed:   s.TerminationConfirmed,
			StdoutLogRef:           s.StdoutLogRef,
			StderrLogRef:           s.StderrLogRef,
		})
	}

	env, err := j.workflowRunEnvironment(ctx, runID)
	if err != nil {
		return workflow.Plan{}, err
	}

	return workflow.RecoverPlan(workflow.RecoveredPlan{
		RunID:            row.RunID,
		BackupSetID:      setID,
		Steps:            steps,
		Env:              env,
		ResolvedPlanHash: row.ResolvedPlanHash,
		ScriptSpoolRef:   row.ScriptSpoolRef,
	})
}

// workflowRunEnvironment reads one run's unresolved environment back, in
// the order it was planned in.
//
// It goes back through workflow.NewEnvironment rather than reconstructing
// the value type directly, so a row set that has been tampered with -- a
// reserved name, a variable carrying both a literal and a secret source,
// two rows for one name -- is refused by the same rules that refused it at
// configuration time. A journal is not a trusted input.
func (j *Journal) workflowRunEnvironment(ctx context.Context, runID string) (workflow.Environment, error) {
	rows, err := j.db.QueryContext(ctx,
		`SELECT name, literal_value, secret_file, secret_env, secret_command
		   FROM workflow_run_env WHERE run_id = ? ORDER BY position`, runID)
	if err != nil {
		return workflow.Environment{}, fmt.Errorf("state: reading the environment of workflow run %q: %w", runID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only

	var vars []workflow.EnvVar

	for rows.Next() {
		var (
			name    string
			literal sql.NullString
			file    string
			env     string
			command string
		)

		if err := rows.Scan(&name, &literal, &file, &env, &command); err != nil {
			return workflow.Environment{}, fmt.Errorf("state: scanning the environment of workflow run %q: %w", runID, err)
		}

		// Both halves are read out and BOTH are handed to the domain
		// type, rather than one being taken as authoritative. A row
		// carrying a literal and a secret source is not a row this write
		// path produces, so reading it as "the literal wins" would be
		// this package quietly resolving a contradiction in a file
		// another process can edit -- and the value a hook then receives
		// is whichever branch happened to be written first.
		// EnvVar.Validate is what refuses it, in the one place that rule
		// lives.
		v := workflow.EnvVar{Name: name}

		if literal.Valid {
			v.Value = literal.String
		}

		if file != "" || env != "" || command != "" {
			var argv []string
			if command != "" {
				if err := json.Unmarshal([]byte(command), &argv); err != nil {
					return workflow.Environment{}, fmt.Errorf(
						"state: the secret command recorded for %s in workflow run %q cannot be read back: %w", name, runID, err)
				}
			}

			v.Secret = secretref.Ref{File: file, Env: env, Command: argv}
		}

		vars = append(vars, v)
	}

	if err := rows.Err(); err != nil {
		return workflow.Environment{}, fmt.Errorf("state: reading the environment of workflow run %q: %w", runID, err)
	}

	if len(vars) == 0 {
		return workflow.Environment{}, nil
	}

	recovered, err := workflow.NewEnvironment(vars)
	if err != nil {
		return workflow.Environment{}, fmt.Errorf("state: the environment recorded for workflow run %q is not one this product would have planned: %w", runID, err)
	}

	return recovered, nil
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
