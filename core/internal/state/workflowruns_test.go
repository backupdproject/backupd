package state

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/secretref"
	"github.com/backupdproject/backupd/core/internal/workflow"
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

	setID, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		panic("the fixture's backup set id is not one model accepts: " + err.Error())
	}

	spool := "/var/lib/backupd/workflow-runs/" + runID
	scripts := spool + "/scripts/"

	mount := workflow.StepID(0, workflow.ScopeGlobal, workflow.PhaseBefore, "10-mount.local.sh")
	quiesce := workflow.StepID(1, workflow.ScopeSet, workflow.PhaseBefore, "20-quiesce.remote.sh")

	env, err := workflow.NewEnvironment([]workflow.EnvVar{
		{Name: "PGHOST", Value: "db.internal"},
		{Name: "PGPASSWORD", Secret: secretref.Ref{Command: []string{"vault", "read", "-field=password", "secret/db"}}},
		{Name: "EMPTY", Value: ""},
	})
	if err != nil {
		panic("the fixture's environment is not one workflow accepts: " + err.Error())
	}

	return WorkflowPlan{
		Run: workflow.Run{
			ID:               runID,
			BackupSetID:      setID,
			State:            workflow.StatePending,
			StartedAt:        started,
			BackupStatus:     workflow.StatusUnknown,
			CleanupStatus:    workflow.StatusUnknown,
			WorkflowStatus:   workflow.StatusUnknown,
			RecoveryState:    workflow.RecoveryNone,
			ResolvedPlanHash: strings.Repeat("a", 64),
			ScriptSpoolRef:   spool,
		},
		Steps: []workflow.Step{
			{
				ID:           mount,
				RunID:        runID,
				Order:        0,
				Scope:        workflow.ScopeGlobal,
				Phase:        workflow.PhaseBefore,
				ScriptName:   "10-mount.local.sh",
				ScriptSHA256: strings.Repeat("b", 64),
				ScriptSize:   42,
				Target:       workflow.TargetLocal,
				State:        workflow.StatePending,
				Timeout:      2 * time.Minute,
				SpoolRef:     scripts + mount,
			},
			{
				ID:                     quiesce,
				RunID:                  runID,
				Order:                  1,
				Scope:                  workflow.ScopeSet,
				Phase:                  workflow.PhaseBefore,
				ScriptName:             "20-quiesce.remote.sh",
				ScriptSHA256:           strings.Repeat("c", 64),
				ScriptSize:             99,
				Target:                 workflow.TargetRemote,
				ExecutionConnectionRef: "production/postgres-primary",
				State:                  workflow.StatePending,
				Timeout:                1500 * time.Millisecond,
				SpoolRef:               scripts + quiesce,
			},
		},
		Env: env,
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

	wantRun := WorkflowRun{
		RunID:            want.Run.ID,
		BackupSetID:      want.Run.BackupSetID.String(),
		State:            string(want.Run.State),
		StartedAt:        want.Run.StartedAt,
		BackupStatus:     string(want.Run.BackupStatus),
		CleanupStatus:    string(want.Run.CleanupStatus),
		WorkflowStatus:   string(want.Run.WorkflowStatus),
		RecoveryState:    string(want.Run.RecoveryState),
		ResolvedPlanHash: want.Run.ResolvedPlanHash,
		ScriptSpoolRef:   want.Run.ScriptSpoolRef,
	}
	if !reflect.DeepEqual(gotRun, wantRun) {
		t.Errorf("the run came back changed.\n got: %+v\nwant: %+v", gotRun, wantRun)
	}

	gotSteps, err := j.WorkflowSteps(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowSteps: %v", err)
	}
	if len(gotSteps) != len(want.Steps) {
		t.Fatalf("read back %d steps, want %d", len(gotSteps), len(want.Steps))
	}
	for i, s := range want.Steps {
		wantStep := WorkflowStep{
			StepID:                 s.ID,
			Order:                  s.Order,
			Scope:                  string(s.Scope),
			Phase:                  string(s.Phase),
			ScriptName:             s.ScriptName,
			ScriptSHA256:           s.ScriptSHA256,
			ScriptSize:             s.ScriptSize,
			Target:                 string(s.Target),
			ExecutionConnectionRef: s.ExecutionConnectionRef,
			State:                  string(s.State),
			Timeout:                s.Timeout,
			SpoolRef:               s.SpoolRef,
		}
		if !reflect.DeepEqual(gotSteps[i], wantStep) {
			t.Errorf("step %d came back changed.\n got: %+v\nwant: %+v", i, gotSteps[i], wantStep)
		}
	}

	// The sub-second bound, spelled out: this is the row that was
	// destroyed by a column of whole seconds, and 1500ms is what a
	// deployment that wants a hook killed fast configures.
	if got := gotSteps[1].Timeout; got != 1500*time.Millisecond {
		t.Errorf("a step with a %s timeout came back as %s; a bound stored in whole seconds is a bound that was configured and then silently was not",
			1500*time.Millisecond, got)
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
	plan.Steps[1].ID = "" // refused by the write path

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
		{"no run id", func(p *WorkflowPlan) { p.Run.ID = "" }, "run id"},
		{"no backup set", func(p *WorkflowPlan) { p.Run.BackupSetID = model.BackupSetID{} }, "backup set"},
		{"no state", func(p *WorkflowPlan) { p.Run.State = "" }, "run state"},
		{"no start time", func(p *WorkflowPlan) { p.Run.StartedAt = time.Time{} }, "time"},
		{"no plan hash", func(p *WorkflowPlan) { p.Run.ResolvedPlanHash = "" }, "plan hash"},
		{"no spool", func(p *WorkflowPlan) { p.Run.ScriptSpoolRef = "" }, "spool"},
		{"a step with no id", func(p *WorkflowPlan) { p.Steps[0].ID = "" }, "step id"},
		{"a step with no script name", func(p *WorkflowPlan) { p.Steps[0].ScriptName = "" }, "script"},
		{"a step with no hash", func(p *WorkflowPlan) { p.Steps[0].ScriptSHA256 = "" }, "hash"},
		{"a step with no spooled script", func(p *WorkflowPlan) { p.Steps[0].SpoolRef = "" }, "spool"},
		{"a step with no state", func(p *WorkflowPlan) { p.Steps[0].State = "" }, "step state"},
		{"two steps claiming one order", func(p *WorkflowPlan) { p.Steps[1].Order = p.Steps[0].Order }, "order"},
		{"two steps claiming one id", func(p *WorkflowPlan) { p.Steps[1].ID = p.Steps[0].ID }, "step"},

		// The vocabulary rows. Every one of these is a value the first
		// version of this write path accepted, because it checked only
		// that the string was non-empty -- and every one of them is read
		// back by a recovery pass after a restart, which has no second
		// chance to notice.
		{"a run state this domain does not have", func(p *WorkflowPlan) { p.Run.State = "whatever" }, "run state"},
		{"a step state that is a run state", func(p *WorkflowPlan) { p.Steps[0].State = workflow.StateCleanupRunning }, "step state"},
		{"a scope this domain does not have", func(p *WorkflowPlan) { p.Steps[0].Scope = "host" }, "step scope"},
		{"a phase this domain does not have", func(p *WorkflowPlan) { p.Steps[0].Phase = "during" }, "step phase"},
		{"a target this domain does not have", func(p *WorkflowPlan) { p.Steps[0].Target = "somewhere" }, "step target"},
		{"a status this domain does not have", func(p *WorkflowPlan) { p.Run.BackupStatus = "fine" }, "backup status"},
		{"a recovery state this domain does not have", func(p *WorkflowPlan) { p.Run.RecoveryState = "maybe" }, "recovery state"},
		{
			what: "a step whose target disagrees with its script name",
			mutate: func(p *WorkflowPlan) {
				p.Steps[0].Target = workflow.TargetRemote
				p.Steps[0].ExecutionConnectionRef = "production/postgres-primary"
			},
			mustSay: "says it runs",
		},
		{
			what:    "a step id that is not derived from the step's own fields",
			mutate:  func(p *WorkflowPlan) { p.Steps[0].ID = "0000~global~before~something-else.local.sh" },
			mustSay: "derived",
		},
		{"a step with no timeout", func(p *WorkflowPlan) { p.Steps[0].Timeout = 0 }, "no spelling of"},
		{
			what:    "a step belonging to another run",
			mutate:  func(p *WorkflowPlan) { p.Steps[0].RunID = "some-other-run" },
			mustSay: "belongs to run",
		},
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
			"timeout_nanos",
		},
		"workflow_run_env": {
			"id",
			"literal_value",
			"name",
			"position",
			"run_id",
			"secret_command",
			"secret_env",
			"secret_file",
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
				"If a column was ADDED, say what it holds, and say it against the one rule this schema exists "+
				"to keep: no RESOLVED secret value is ever persisted. A variable's name, its literal value and the "+
				"LOCATION a secret comes from are all things config.yaml already holds in the clear, and they are "+
				"durable here (workflow_run_env) because a recovery after a config edit has to finish the run that "+
				"was planned. The material a location resolves to is not, anywhere, ever -- and "+
				"TestNoResolvedSecretReachesAnyPersistedByte is what searches the database file to prove it. "+
				"If a column was REMOVED, a journal that already has it still has it, and this package cannot read "+
				"a schema it does not describe.",
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
		`INSERT INTO workflow_steps (run_id, step_id, step_order, scope, phase, script_name, script_sha256, script_size, target, state, timeout_nanos, spool_ref)
		 VALUES ('run-does-not-exist', 's', 0, 'set', 'before', 'a.local.sh', 'x', 1, 'local', 'pending', 1, '/x')`,
	); err == nil {
		t.Error("a step for a run this journal does not have was accepted; a plan's steps must not be able to outlive the run they belong to")
	}
}
