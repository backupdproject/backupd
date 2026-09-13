package state

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/secretref"
	"github.com/backupdproject/backupd/core/internal/workflow"
)

// Recovery, and the two claims that have to hold together for it to be
// worth anything:
//
//   - a run interrupted before its last hook is recovered with the
//     environment it was PLANNED with, not the one today's config file
//     describes. An operator who edits (or deletes) a variable while the
//     daemon is down has not changed what the interrupted run finishes
//     with, because that run's decision was made and recorded;
//   - and no resolved secret material is anywhere in the bytes that made
//     that possible.
//
// The second is what makes the first non-obvious. The easy way to make a
// plan recoverable is to persist everything the run needs, which for a
// secret-backed variable is the credential -- and #808's contract is that
// this product never writes one down. So what is durable is the LOCATION,
// and recovery resolves it again at execution time, through
// internal/secretref's custody rules, exactly as the original run did.

// planWithEnv is testWorkflowPlan with a different environment, for the
// tests that are about the environment rather than about the plan.
func planWithEnv(t *testing.T, runID string, vars []workflow.EnvVar) WorkflowPlan {
	t.Helper()

	env, err := workflow.NewEnvironment(vars)
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}

	plan := testWorkflowPlan(runID)
	plan.Env = env

	return plan
}

func TestRecoverWorkflowPlanRebuildsThePlannedEnvironment(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	// The environment as it was when the run was planned: a literal, an
	// empty literal, a file-backed secret, an env-backed secret and a
	// command-backed secret whose argv has to survive intact.
	planned := []workflow.EnvVar{
		{Name: "PGHOST", Value: "db.internal"},
		{Name: "PGOPTIONS", Value: ""},
		{Name: "PGPASSWORD", Secret: secretref.Ref{File: "/etc/backupd/secrets/db.pw"}},
		{Name: "API_TOKEN", Secret: secretref.Ref{Env: "BACKUPD_TEST_TOKEN"}},
		{Name: "VAULT_PW", Secret: secretref.Ref{Command: []string{"vault", "read", "-field=password", "secret/prod/db"}}},
	}

	plan := planWithEnv(t, "run-1", planned)

	if err := j.CommitWorkflowPlan(ctx, plan); err != nil {
		t.Fatalf("CommitWorkflowPlan: %v", err)
	}

	// The config edit that happens while the daemon is down. Nothing in
	// this test reads a config file -- that is the point: the recovery
	// path has no access to one, so if the environment were not durable
	// there would be nothing here to reconstruct it from.
	recovered, err := j.RecoverWorkflowPlan(ctx, "run-1")
	if err != nil {
		t.Fatalf("RecoverWorkflowPlan: %v", err)
	}

	if !reflect.DeepEqual(recovered.Env().Vars(), plan.Env.Vars()) {
		t.Errorf("the recovered environment is not the planned one.\n got: %+v\nwant: %+v",
			recovered.Env().Vars(), plan.Env.Vars())
	}

	// And the rest of the plan came back too, or the comparison above is
	// about an environment attached to nothing.
	if recovered.RunID() != "run-1" {
		t.Errorf("RecoverWorkflowPlan returned run %q", recovered.RunID())
	}
	if got, want := len(recovered.Steps()), len(plan.Steps); got != want {
		t.Errorf("the recovered plan has %d steps, want %d", got, want)
	}
	if recovered.ResolvedPlanHash() != plan.Run.ResolvedPlanHash {
		t.Errorf("the recovered plan hash is %q, want %q", recovered.ResolvedPlanHash(), plan.Run.ResolvedPlanHash)
	}
	if got := recovered.Steps()[1].Timeout; got != plan.Steps[1].Timeout {
		t.Errorf("a recovered step's timeout is %s, want %s", got, plan.Steps[1].Timeout)
	}
}

// A run with no environment at all is a real configuration -- a
// deployment that declares hook directories and no variables -- and it
// must recover as the ZERO environment rather than as a refusal or as the
// sanitized baseline. #808's first acceptance criterion is that a set
// which configures nothing behaves as it did before this package existed.
func TestRecoverWorkflowPlanAcceptsARunWithNoEnvironment(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := testWorkflowPlan("run-1")
	plan.Env = workflow.Environment{}

	if err := j.CommitWorkflowPlan(ctx, plan); err != nil {
		t.Fatalf("CommitWorkflowPlan: %v", err)
	}

	recovered, err := j.RecoverWorkflowPlan(ctx, "run-1")
	if err != nil {
		t.Fatalf("RecoverWorkflowPlan: %v", err)
	}
	if got := recovered.Env().Names(); len(got) != 0 {
		t.Errorf("a run planned with no environment recovered with %v", got)
	}
}

// The environment is committed in the SAME transaction as the run and its
// steps. A run whose variables were written separately could be read back
// half-planned, and a hook executed with a different environment than the
// one the plan hash covers is a hook nothing can account for.
func TestARefusedPlanLeavesNoEnvironmentBehind(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := testWorkflowPlan("run-1")
	plan.Steps[1].Target = "somewhere" // refused after the run and the steps would have been written

	if err := j.CommitWorkflowPlan(ctx, plan); err == nil {
		t.Fatal("a plan with an unusable step was committed")
	}

	var rows int
	if err := j.db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_run_env WHERE run_id = ?`, "run-1").Scan(&rows); err != nil {
		t.Fatalf("counting the environment rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d environment rows survived a refused commit", rows)
	}
}

// #808's "no resolved secret in any persisted artifact", asserted against
// the DATABASE FILE rather than against the schema.
//
// The schema guard (TestWorkflowSchemaHasNoColumnASecretCouldLiveIn) pins
// the column set, which is the structural claim. This is the empirical
// one: a real secret is resolved through the real resolver, so the
// sentinel genuinely exists in this process's memory as a credential, and
// then every byte SQLite wrote -- the database, its write-ahead log and
// its shared-memory index -- is searched for it.
//
// The resolution is not decoration. A test that never resolved anything
// would pass against a product that persists credentials it was never
// given, which is no test at all.
func TestNoResolvedSecretReachesAnyPersistedByte(t *testing.T) {
	t.Parallel()

	const sentinel = "sentinel-9f3a1c-do-not-persist-this-value"

	j, dbPath := openJournal(t)
	ctx := context.Background()

	secretFile := filepath.Join(t.TempDir(), "db.pw")
	if err := os.WriteFile(secretFile, []byte(sentinel+"\n"), 0o600); err != nil {
		t.Fatalf("writing the secret fixture: %v", err)
	}

	ref := secretref.Ref{File: secretFile}

	plan := planWithEnv(t, "run-1", []workflow.EnvVar{
		{Name: "PGHOST", Value: "db.internal"},
		{Name: "PGPASSWORD", Secret: ref},
	})

	// Resolve it for real, so the material is in play.
	resolved, err := plan.Env.Resolve(ctx, map[string]string{})
	if err != nil {
		t.Fatalf("resolving the plan's environment: %v", err)
	}

	found := false
	for _, kv := range resolved.Environ() {
		if strings.Contains(kv, sentinel) {
			found = true
		}
	}
	if !found {
		t.Fatal("the fixture's secret did not resolve to the sentinel, so searching the journal for it proves nothing")
	}

	if err := j.CommitWorkflowPlan(ctx, plan); err != nil {
		t.Fatalf("CommitWorkflowPlan: %v", err)
	}

	// Force everything out of SQLite's own buffers before reading the
	// files: a sentinel still in a page cache is one this test would miss.
	if _, err := j.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpointing the journal: %v", err)
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := dbPath + suffix

		blob, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}

		if bytes.Contains(blob, []byte(sentinel)) {
			t.Errorf("%s contains the resolved secret. A credential in the journal is a credential in every backup of the journal, in every support bundle and in every `strings` an operator runs on it; what is durable is the LOCATION a secret comes from", path)
		}
	}

	// The location, on the other hand, IS durable -- and has to be, or a
	// recovery could not resolve the secret again. This is the assertion
	// that keeps the search above from passing because nothing was
	// written at all.
	blob, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("reading %s: %v", dbPath, err)
	}
	if !bytes.Contains(blob, []byte(secretFile)) {
		t.Errorf("the journal does not contain the secret's LOCATION (%s), so this run cannot be recovered and the search above proved nothing", secretFile)
	}

	recovered, err := j.RecoverWorkflowPlan(ctx, "run-1")
	if err != nil {
		t.Fatalf("RecoverWorkflowPlan: %v", err)
	}

	again, err := recovered.Env().Resolve(ctx, map[string]string{})
	if err != nil {
		t.Fatalf("resolving a recovered environment: %v", err)
	}

	same := false
	for _, kv := range again.Environ() {
		if kv == "PGPASSWORD="+sentinel {
			same = true
		}
	}
	if !same {
		t.Error("a recovered run resolves a different secret than the run that was planned; the location is stored so that this is the one thing that does not change")
	}
}

// The recovery index and RecoveryState.Settled are one decision written in
// two languages, and they have to agree: a state the Go side calls
// unsettled and the index does not is a run the recovery pass scan never
// sees, which is a hook that ran, was interrupted, and is never finished.
func TestRecoveryIndexMatchesTheUnsettledStates(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	var ddl string
	if err := j.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_workflow_runs_unsettled'`,
	).Scan(&ddl); err != nil {
		t.Fatalf("reading the index definition: %v", err)
	}

	for _, state := range workflow.RecoveryStates() {
		named := strings.Contains(ddl, "'"+string(state)+"'")
		settled := state.Settled()

		if named == settled {
			t.Errorf("recovery state %q is %s in Go and %s in the index predicate:\n\t%s\n\n"+
				"The index is what the recovery pass scans. A state Go calls unsettled and the index omits is a run "+
				"nothing ever finishes; one the index carries and Go calls settled is every historical run in the "+
				"deployment landing in a partial index that exists to stay small.",
				state, settledWord(settled), settledWord(!named), ddl)
		}
	}
}

func settledWord(settled bool) string {
	if settled {
		return "settled"
	}

	return "unsettled"
}

// A run whose stored rows no longer describe a plan this product could
// have built is refused at RECOVERY, not executed.
//
// This is the other end of the write path's validation: the rows were
// legal when they were written, and a database is a file another process
// can edit. Every refusal here is internal/workflow's, reached through
// RecoverPlan, which is the point -- there is no second, weaker validation
// for the recovery path.
func TestRecoverWorkflowPlanRefusesRowsThatAreNoLongerAPlan(t *testing.T) {
	t.Parallel()

	cases := []struct {
		what    string
		corrupt string
		args    []any
		mustSay string
	}{
		{
			what:    "a step whose target was changed under it",
			corrupt: `UPDATE workflow_steps SET target = 'remote', execution_connection_ref = 'x' WHERE step_order = 0 AND run_id = ?`,
			mustSay: "says it runs",
		},
		{
			what:    "a spooled script moved out of the run's own spool",
			corrupt: `UPDATE workflow_steps SET spool_ref = '/tmp/elsewhere/script' WHERE step_order = 0 AND run_id = ?`,
			mustSay: "spool holds it at",
		},
		{
			what:    "a state this domain does not have",
			corrupt: `UPDATE workflow_steps SET state = 'nearly' WHERE step_order = 0 AND run_id = ?`,
			mustSay: "step state",
		},
		{
			what:    "a timeout that rounds to nothing",
			corrupt: `UPDATE workflow_steps SET timeout_nanos = 0 WHERE step_order = 0 AND run_id = ?`,
			mustSay: "no spelling of",
		},
		{
			what:    "an environment variable carrying both a literal and a secret",
			corrupt: `UPDATE workflow_run_env SET literal_value = 'x', secret_file = '/k' WHERE run_id = ? AND name = 'PGHOST'`,
			mustSay: "both a literal value and a secret reference",
		},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()

			j, _ := openJournal(t)
			ctx := context.Background()

			if err := j.CommitWorkflowPlan(ctx, testWorkflowPlan("run-1")); err != nil {
				t.Fatalf("CommitWorkflowPlan: %v", err)
			}

			if _, err := j.db.ExecContext(ctx, tc.corrupt, "run-1"); err != nil {
				t.Fatalf("corrupting the rows: %v", err)
			}

			_, err := j.RecoverWorkflowPlan(ctx, "run-1")
			if err == nil {
				t.Fatalf("RecoverWorkflowPlan accepted %s", tc.what)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("RecoverWorkflowPlan said:\n\t%v\nwant it to contain %q", err, tc.mustSay)
			}
		})
	}

	// The positive control.
	j, _ := openJournal(t)
	ctx := context.Background()

	if err := j.CommitWorkflowPlan(ctx, testWorkflowPlan("run-1")); err != nil {
		t.Fatalf("CommitWorkflowPlan: %v", err)
	}
	if _, err := j.RecoverWorkflowPlan(ctx, "run-1"); err != nil {
		t.Fatalf("an uncorrupted plan was refused, so every row above proves nothing: %v", err)
	}
}

// A run this journal does not have is ErrWorkflowRunNotFound rather than
// an empty plan, because "there is nothing to recover" and "this run had
// nothing to do" are different answers and only one of them is safe to act
// on.
func TestRecoverWorkflowPlanDistinguishesAMissingRun(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)

	_, err := j.RecoverWorkflowPlan(context.Background(), "never-existed")
	if err == nil {
		t.Fatal("RecoverWorkflowPlan invented a plan for a run this journal does not have")
	}
	if !isNotFound(err) {
		t.Errorf("RecoverWorkflowPlan returned %v, want an ErrWorkflowRunNotFound", err)
	}
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrWorkflowRunNotFound.Error())
}

// A step whose spooled script is not inside its run's own spool is
// refused at COMMIT, not only at recovery.
//
// The run record and the steps come from two different pieces of the
// caller -- a run starts before its first hook does -- so the write path
// is the one place the two halves can be checked against each other. A
// plan committed with another run's spool paths is a run that would
// execute another run's scripts, and workflow.RecoverPlan would refuse to
// read it back, which is a run that can never be recovered either.
func TestCommitWorkflowPlanRefusesAStepSpooledOutsideItsRun(t *testing.T) {
	t.Parallel()

	j, _ := openJournal(t)
	ctx := context.Background()

	plan := testWorkflowPlan("run-1")
	plan.Steps[0].SpoolRef = "/var/lib/backupd/workflow-runs/some-other-run/scripts/" + plan.Steps[0].ID

	err := j.CommitWorkflowPlan(ctx, plan)
	if err == nil {
		t.Fatal("a step spooled under another run was committed; that run would execute bytes that belong to a different plan")
	}
	if !strings.Contains(err.Error(), "spool holds it at") {
		t.Errorf("CommitWorkflowPlan said:\n\t%v\nwant it to name where this run's spool holds the script", err)
	}
}

// The scripts-directory name is a constant in two packages, and they have
// to be the same constant: this journal checks a step's spool path against
// its own copy, and internal/workflow is what creates the directory. A
// rename on one side would make every commit fail -- or, worse, make the
// check pass against a path nothing writes to.
func TestTheSpoolLayoutThisJournalChecksIsTheOneWorkflowCreates(t *testing.T) {
	t.Parallel()

	plan := testWorkflowPlan("run-1")

	recovered, err := workflow.RecoverPlan(workflow.RecoveredPlan{
		RunID:            plan.Run.ID,
		BackupSetID:      plan.Run.BackupSetID,
		Steps:            plan.Steps,
		Env:              plan.Env,
		ResolvedPlanHash: plan.Run.ResolvedPlanHash,
		ScriptSpoolRef:   plan.Run.ScriptSpoolRef,
	})
	if err != nil {
		t.Fatalf("the spool paths this journal accepts are not the ones internal/workflow derives: %v", err)
	}
	if len(recovered.Steps()) != len(plan.Steps) {
		t.Errorf("the recovered plan has %d steps, want %d", len(recovered.Steps()), len(plan.Steps))
	}
}

// A guard on the fixture rather than on the product: the plan every test
// in this file commits has to be one the domain accepts, or the refusal
// tables are all measuring the fixture.
func TestTheWorkflowFixtureIsAPlanTheDomainAccepts(t *testing.T) {
	t.Parallel()

	plan := testWorkflowPlan("run-1")

	if err := plan.Run.Validate(); err != nil {
		t.Fatalf("the fixture's run: %v", err)
	}
	for i, s := range plan.Steps {
		if err := s.Validate(); err != nil {
			t.Fatalf("the fixture's step %d: %v", i, err)
		}
	}
	if plan.Run.StartedAt.After(time.Now()) {
		t.Error("the fixture starts in the future")
	}
	if _, err := model.ParseBackupSetID(plan.Run.BackupSetID.String()); err != nil {
		t.Errorf("the fixture's backup set id does not read back: %v", err)
	}
}
