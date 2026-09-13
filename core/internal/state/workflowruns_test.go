package state

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// The workflow plan's durable half (EPIC L, #808).
//
// Two things are being proven here, and the second is the one that matters
// most for #808:
//
//   - a plan is committed in ONE transaction, run row and step rows
//     together, because "durably committed before any hook may execute"
//     is a claim about all of it. A crash between the run and its steps
//     would leave a run whose spool a later recovery pass would read as
//     the authority on an empty plan.
//   - the schema has no column a resolved secret could live in. That is
//     asserted against the table definitions themselves rather than
//     against a write path, because a write path that does not persist
//     something today is a write path somebody can extend tomorrow, and a
//     column that does not exist cannot be filled in by accident.

func testWorkflowPlan(runID string) WorkflowPlan {
	started := time.Date(2026, 9, 13, 2, 0, 0, 0, time.UTC)

	return WorkflowPlan{
		Run: WorkflowRun{
			RunID:            runID,
			BackupSetID:      "production/postgres-primary",
			State:            "pending",
			StartedAt:        started,
			BackupStatus:     "unknown",
			CleanupStatus:    "unknown",
			WorkflowStatus:   "pending",
			RecoveryState:    "none",
			ResolvedPlanHash: strings.Repeat("a", 64),
			ScriptSpoolRef:   "/var/lib/backupd/workflow-runs/" + runID,
		},
		Steps: []WorkflowStep{
			{
				StepID:       "0000~global~before~10-mount.local.sh",
				Order:        0,
				Scope:        "global",
				Phase:        "before",
				ScriptName:   "10-mount.local.sh",
				ScriptSHA256: strings.Repeat("b", 64),
				ScriptSize:   42,
				Target:       "local",
				State:        "pending",
				Timeout:      2 * time.Minute,
				SpoolRef:     "/var/lib/backupd/workflow-runs/" + runID + "/scripts/0000~global~before~10-mount.local.sh",
			},
			{
				StepID:                 "0001~set~before~20-quiesce.remote.sh",
				Order:                  1,
				Scope:                  "set",
				Phase:                  "before",
				ScriptName:             "20-quiesce.remote.sh",
				ScriptSHA256:           strings.Repeat("c", 64),
				ScriptSize:             99,
				Target:                 "remote",
				ExecutionConnectionRef: "production/postgres-primary",
				State:                  "pending",
				Timeout:                30 * time.Second,
				SpoolRef:               "/var/lib/backupd/workflow-runs/" + runID + "/scripts/0001~set~before~20-quiesce.remote.sh",
			},
		},
	}
}

func TestCommitWorkflowPlanRoundTripsEveryField(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	want := testWorkflowPlan("run-1")

	if err := j.CommitWorkflowPlan(ctx, want); err != nil {
		t.Fatalf("CommitWorkflowPlan: %v", err)
	}

	gotRun, err := j.WorkflowRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if !reflect.DeepEqual(gotRun, want.Run) {
		t.Errorf("the run came back changed.\n got: %+v\nwant: %+v", gotRun, want.Run)
	}

	gotSteps, err := j.WorkflowSteps(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowSteps: %v", err)
	}
	if len(gotSteps) != len(want.Steps) {
		t.Fatalf("read back %d steps, want %d", len(gotSteps), len(want.Steps))
	}
	for i := range want.Steps {
		if !reflect.DeepEqual(gotSteps[i], want.Steps[i]) {
			t.Errorf("step %d came back changed.\n got: %+v\nwant: %+v", i, gotSteps[i], want.Steps[i])
		}
	}
}

// Steps come back in plan order, not insertion or rowid order, because the
// order is the plan and a read that returned them shuffled would let a
// recovery pass run an "after" hook before a "before" one.
func TestWorkflowStepsComeBackInPlanOrder(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := testWorkflowPlan("run-1")

	// Insert them in the wrong order deliberately: a read that relied on
	// the rowid would pass against an ordered insert and fail here.
	plan.Steps[0], plan.Steps[1] = plan.Steps[1], plan.Steps[0]

	if err := j.CommitWorkflowPlan(ctx, plan); err != nil {
		t.Fatalf("CommitWorkflowPlan: %v", err)
	}

	steps, err := j.WorkflowSteps(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowSteps: %v", err)
	}

	for i, s := range steps {
		if s.Order != i {
			t.Errorf("step at index %d records order %d; the plan's order is the execution order and a read must not reorder it", i, s.Order)
		}
	}
}

// The atomicity claim. A plan whose last step is unusable must leave NO
// run row behind: a run row pointing at a spool, with no steps, is
// something a recovery pass reads as "this run had nothing to do".
func TestCommitWorkflowPlanIsAllOrNothing(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := testWorkflowPlan("run-1")
	plan.Steps[1].StepID = "" // refused by the write path

	if err := j.CommitWorkflowPlan(ctx, plan); err == nil {
		t.Fatal("a plan with an unusable step was committed")
	}

	if _, err := j.WorkflowRun(ctx, "run-1"); !errors.Is(err, ErrWorkflowRunNotFound) {
		t.Errorf("the run row survived a refused commit (err=%v); a run with no steps is one a recovery pass reads as having had nothing to do", err)
	}

	steps, err := j.WorkflowSteps(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowSteps: %v", err)
	}
	if len(steps) != 0 {
		t.Errorf("%d step rows survived a refused commit", len(steps))
	}

	// The positive control: the same plan, fixed, commits. Without it
	// the assertions above would pass against a CommitWorkflowPlan that
	// never wrote anything at all.
	if err := j.CommitWorkflowPlan(ctx, testWorkflowPlan("run-1")); err != nil {
		t.Fatalf("the corrected plan was refused: %v", err)
	}
	if _, err := j.WorkflowRun(ctx, "run-1"); err != nil {
		t.Fatalf("WorkflowRun after a successful commit: %v", err)
	}
}

// A run id is committed once. A retry that re-committed would either
// duplicate the steps or move the run's start time, and both make the
// plan stop describing what actually ran.
func TestCommitWorkflowPlanRefusesASecondCommitOfOneRun(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	if err := j.CommitWorkflowPlan(ctx, testWorkflowPlan("run-1")); err != nil {
		t.Fatalf("CommitWorkflowPlan: %v", err)
	}

	err := j.CommitWorkflowPlan(ctx, testWorkflowPlan("run-1"))
	if err == nil {
		t.Fatal("the same run was committed twice")
	}
	if !strings.Contains(err.Error(), "run-1") {
		t.Errorf("the refusal must name the run, got: %v", err)
	}

	steps, err := j.WorkflowSteps(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowSteps: %v", err)
	}
	if len(steps) != 2 {
		t.Errorf("the journal holds %d steps for one run, want 2: a re-commit duplicated the plan", len(steps))
	}
}

// The refusal table for a plan this journal must not store. Each row is
// something that would be silently useless once written, and the sentence
// has to say what it costs.
func TestCommitWorkflowPlanRefusesUnusablePlans(t *testing.T) {
	t.Parallel()

	cases := []struct {
		what    string
		mutate  func(p *WorkflowPlan)
		mustSay string
	}{
		{"no run id", func(p *WorkflowPlan) { p.Run.RunID = "" }, "run id"},
		{"no backup set", func(p *WorkflowPlan) { p.Run.BackupSetID = "" }, "backup set"},
		{"no state", func(p *WorkflowPlan) { p.Run.State = "" }, "state"},
		{"no start time", func(p *WorkflowPlan) { p.Run.StartedAt = time.Time{} }, "time"},
		{"no plan hash", func(p *WorkflowPlan) { p.Run.ResolvedPlanHash = "" }, "plan hash"},
		{"no spool", func(p *WorkflowPlan) { p.Run.ScriptSpoolRef = "" }, "spool"},
		{"a step with no id", func(p *WorkflowPlan) { p.Steps[0].StepID = "" }, "step id"},
		{"a step with no script name", func(p *WorkflowPlan) { p.Steps[0].ScriptName = "" }, "script"},
		{"a step with no hash", func(p *WorkflowPlan) { p.Steps[0].ScriptSHA256 = "" }, "hash"},
		{"a step with no spooled script", func(p *WorkflowPlan) { p.Steps[0].SpoolRef = "" }, "spool"},
		{"a step with no state", func(p *WorkflowPlan) { p.Steps[0].State = "" }, "state"},
		{"two steps claiming one order", func(p *WorkflowPlan) { p.Steps[1].Order = p.Steps[0].Order }, "order"},
		{"two steps claiming one id", func(p *WorkflowPlan) { p.Steps[1].StepID = p.Steps[0].StepID }, "step"},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()

			j, _ := openJournal(t)

			plan := testWorkflowPlan("run-1")
			tc.mutate(&plan)

			err := j.CommitWorkflowPlan(context.Background(), plan)
			if err == nil {
				t.Fatalf("CommitWorkflowPlan accepted %s", tc.what)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("CommitWorkflowPlan said:\n\t%v\nwant it to contain %q", err, tc.mustSay)
			}
		})
	}

	// The positive control: the unmutated plan commits.
	j, _ := openJournal(t)
	if err := j.CommitWorkflowPlan(context.Background(), testWorkflowPlan("run-1")); err != nil {
		t.Fatalf("the control plan was refused, so every row above proves nothing: %v", err)
	}
}

// #808's "no resolved secret in any persisted artifact", held against the
// SCHEMA rather than against a write path.
//
// A write path that does not persist a value today is one somebody can
// extend tomorrow; a column that does not exist cannot be filled in by
// accident. So this pins the exact column set of both tables, and the
// failure message says what the reviewer has to think about.
func TestWorkflowSchemaHasNoColumnASecretCouldLiveIn(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	want := map[string][]string{
		"workflow_runs": {
			"backup_set_id",
			"backup_status",
			"cleanup_status",
			"finished_at",
			"id",
			"recovery_state",
			"resolved_plan_hash",
			"run_id",
			"script_spool_ref",
			"started_at",
			"state",
			"workflow_status",
		},
		"workflow_steps": {
			"execution_connection_ref",
			"exit_code",
			"finished_at",
			"id",
			"phase",
			"run_id",
			"scope",
			"script_name",
			"script_sha256",
			"script_size",
			"spool_ref",
			"started_at",
			"state",
			"stderr_log_ref",
			"stdout_log_ref",
			"step_id",
			"step_order",
			"target",
			"termination_confirmed",
			"timeout_seconds",
		},
	}

	for table, wantColumns := range want {
		rows, err := j.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY name`, table)
		if err != nil {
			t.Fatalf("reading %s's columns: %v", table, err)
		}

		var got []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close() //nolint:errcheck // the scan already failed

				t.Fatalf("scanning %s's columns: %v", table, err)
			}
			got = append(got, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("reading %s's columns: %v", table, err)
		}
		rows.Close() //nolint:errcheck // done with it

		sort.Strings(wantColumns)

		if len(got) == 0 {
			t.Fatalf("%s has no columns at all, so this guard compared nothing", table)
		}

		if !reflect.DeepEqual(got, wantColumns) {
			t.Errorf("%s's columns are\n\t%v\nand this guard records\n\t%v\n\n"+
				"If a column was ADDED, say what it holds: this schema deliberately has nowhere to put a hook's "+
				"environment, because a secret-backed variable resolves to material and #808's contract is that no "+
				"resolved value is ever persisted. A variable's NAME and the LOCATION its value comes from live in "+
				"the config file; the plan hash covers them. If a column was REMOVED, a journal that already has it "+
				"still has it, and this package cannot read a schema it does not describe.",
				table, got, wantColumns)
		}
	}
}

// A step for a run this journal does not have is refused by the foreign
// key, which is what stops a plan's steps outliving the run they belong to
// (and, with it, stops a recovery pass finding steps it cannot attribute).
func TestWorkflowStepsCannotOutliveTheirRun(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	if err := j.CommitWorkflowPlan(ctx, testWorkflowPlan("run-1")); err != nil {
		t.Fatalf("CommitWorkflowPlan: %v", err)
	}

	if _, err := j.db.ExecContext(ctx,
		`INSERT INTO workflow_steps (run_id, step_id, step_order, scope, phase, script_name, script_sha256, script_size, target, state, timeout_seconds, spool_ref)
		 VALUES ('run-does-not-exist', 's', 0, 'set', 'before', 'a.local.sh', 'x', 1, 'local', 'pending', 1, '/x')`,
	); err == nil {
		t.Error("a step for a run this journal does not have was accepted; a plan's steps must not be able to outlive the run they belong to")
	}
}
