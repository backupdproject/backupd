package workflowrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/state"
	"github.com/backupdproject/backupd/core/internal/workflow"
)

// Crash injection at every durable transition (#811's central claim).
//
// # What is being asserted
//
// Not "the engine handles an error from the journal". That is a different
// and weaker property. What has to hold is that if this PROCESS stops at
// any point in the sequence of durable writes, the rows left behind
// reconcile to one deterministic answer, and that answer errs toward
// recovery_required.
//
// So the crash is a panic from inside the journal decorator, after the
// Nth write has been allowed through: the engine gets no chance to tidy
// up, exactly as it would not if the machine lost power. The test then
// throws that engine away, builds a NEW one on the same journal file --
// which is what a restart is -- and reconciles.
//
// # Why the loop goes to every N rather than to a chosen few
//
// Because the interesting N is the one nobody thought of. The sequence of
// durable writes in a five-stage run is a dozen long and it changes
// whenever the engine does, so the suite walks all of them and asserts
// the same invariants at each, rather than naming the transitions
// somebody considered load-bearing at the time.

// errCrash is the panic a crashStore raises. A panic rather than an error
// return, deliberately: an error is something the engine handles, and
// what is being modelled here is a process that stops.
var errCrash = errors.New("crash: the process stopped here")

// crashStore allows a fixed number of durable writes and then stops the
// process. Reads are never counted: a restart re-reads everything, so a
// read is not a point a crash can be observed at.
type crashStore struct {
	Store

	mu      sync.Mutex
	allowed int
	writes  int
	crashed bool
}

func (c *crashStore) note() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.writes++
	if c.writes > c.allowed {
		c.crashed = true

		panic(errCrash)
	}
}

func (c *crashStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.writes
}

func (c *crashStore) CommitWorkflowPlan(ctx context.Context, plan state.WorkflowPlan) error {
	c.note()

	return c.Store.CommitWorkflowPlan(ctx, plan)
}

func (c *crashStore) StartWorkflowStep(ctx context.Context, runID, stepID string, at time.Time) error {
	c.note()

	return c.Store.StartWorkflowStep(ctx, runID, stepID, at)
}

func (c *crashStore) FinishWorkflowStep(ctx context.Context, runID, stepID string, out state.WorkflowStepOutcome) error {
	c.note()

	return c.Store.FinishWorkflowStep(ctx, runID, stepID, out)
}

func (c *crashStore) AdvanceWorkflowRun(ctx context.Context, runID string, adv state.WorkflowRunAdvance) error {
	c.note()

	return c.Store.AdvanceWorkflowRun(ctx, runID, adv)
}

func (c *crashStore) AdvanceWorkflowCleanupObligation(ctx context.Context, adv state.WorkflowObligationAdvance) error {
	c.note()

	return c.Store.AdvanceWorkflowCleanupObligation(ctx, adv)
}

// AppendWorkflowStepLog is deliberately NOT counted. A log record is
// evidence rather than a lifecycle transition: losing one loses output,
// which is bad, and does not change what a recovery pass has to do.
// Counting it would make the crash points depend on how chatty the
// fixture's hooks are.

// runUntilCrash runs one workflow with the journal stopping after
// `allowed` durable writes, and reports how many writes happened.
// The returns are NAMED and set from the deferred recover, because the
// panic unwinds past the return statement: a version of this that
// returned normally would report zero writes and no crash for every
// crash, and the whole suite would pass by never crashing at all.
func runUntilCrash(t *testing.T, h *harness, plan workflow.Plan, allowed int) (writes int, crashed bool) {
	t.Helper()

	crash := &crashStore{Store: h.store, allowed: allowed}
	h.engine.Store = crash

	defer func() {
		recovered := recover()
		if recovered != nil && recovered != errCrash {
			panic(recovered)
		}

		h.engine.Store = h.store
		writes, crashed = crash.count(), crash.crashed
	}()

	_, _ = h.engine.Run(context.Background(), RunRequest{
		Plan:        plan,
		BackupSetID: plan.BackupSetID(),
		Backup:      func(context.Context) error { return nil },
	})

	return crash.count(), crash.crashed
}

func TestACrashAtEveryDurableTransitionReconcilesToRecoveryRequired(t *testing.T) {
	t.Parallel()

	// First, how many durable writes a complete run makes, so the loop
	// covers all of them rather than a number somebody typed.
	total := func() int {
		h := newHarness(t)
		tr := newTree(t, fullTree())
		count, crashed := runUntilCrash(t, h, tr.snapshot(t, "run-1"), 1_000)
		if crashed {
			t.Fatalf("the control run crashed at %d writes, so the bound is wrong", count)
		}

		return count
	}()

	if total < 8 {
		t.Fatalf("a five-stage run made only %d durable writes; this suite is not exercising the sequence it claims to", total)
	}

	for allowed := range total {
		t.Run(fmt.Sprintf("after %d durable writes", allowed), func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			tr := newTree(t, fullTree())
			plan := tr.snapshot(t, "run-1")

			_, crashed := runUntilCrash(t, h, plan, allowed)
			if !crashed {
				t.Fatalf("the run finished without reaching write %d", allowed+1)
			}

			// The restart. A new engine, the same journal file: nothing
			// in memory survives, which is the whole point.
			restarted := &Engine{Store: h.store, Local: h.local, Remote: h.remote, Now: h.clock.Now}

			report, err := restarted.Reconcile(context.Background(), h.clock.Now())
			if err != nil {
				t.Fatalf("Reconcile after a crash at write %d: %v", allowed+1, err)
			}

			run, err := h.store.WorkflowRun(context.Background(), "run-1")
			if errors.Is(err, state.ErrWorkflowRunNotFound) {
				// The crash landed on the plan commit itself. That
				// transaction is atomic, so there is no run, no step and
				// no obligation -- and nothing for a recovery to do.
				// This is the one crash point with nothing outstanding,
				// and it is deterministic for the same reason: a plan is
				// committed in one transaction.
				if len(report.Holds) != 0 {
					t.Fatalf("a crash before any run existed left %d holds: %+v", len(report.Holds), report.Holds)
				}

				return
			}
			if err != nil {
				t.Fatalf("WorkflowRun: %v", err)
			}

			assertReconciled(t, h.store, restarted, run, allowed)

			// Reconciling again changes nothing. A startup pass that was
			// not idempotent would move a run further every time a
			// daemon restarted, which for a deployment that crash-loops
			// is a record that rewrites itself.
			before := snapshotRun(t, h.store, "run-1")
			if _, err := restarted.Reconcile(context.Background(), h.clock.Now()); err != nil {
				t.Fatalf("the second Reconcile failed: %v", err)
			}
			after := snapshotRun(t, h.store, "run-1")

			if !reflect.DeepEqual(before, after) {
				t.Errorf("a second reconciliation moved the run.\nbefore: %+v\n after: %+v", before, after)
			}
		})
	}
}

// assertReconciled is the invariant set every crash point has to satisfy.
func assertReconciled(t *testing.T, store *state.Journal, engine *Engine, run state.WorkflowRun, allowed int) {
	t.Helper()

	ctx := context.Background()

	steps, err := store.WorkflowSteps(ctx, run.RunID)
	if err != nil {
		t.Fatalf("WorkflowSteps: %v", err)
	}
	obligations, err := store.WorkflowCleanupObligations(ctx, run.RunID)
	if err != nil {
		t.Fatalf("WorkflowCleanupObligations: %v", err)
	}

	owed := map[workflow.Scope]bool{}
	for _, o := range obligations {
		if !o.State.Settled() {
			owed[o.Scope] = true
		}
	}

	// 1. No step is left RUNNING: an outcome nobody observed is recorded
	//    as interrupted, never left looking like work still in flight.
	//
	//    A step may still be pending, and exactly one kind may: an
	//    "after" step of a scope that is owed its cleanup. That step has
	//    not been abandoned, it is the thing a resume-cleanup exists to
	//    run, and marking it skipped would make it terminal -- after
	//    which the recovery would have nothing left to do and would
	//    report a machine as put back that nothing had touched.
	for _, s := range steps {
		st := workflow.State(s.State)

		if st == workflow.StateRunning {
			t.Errorf("after reconciling a crash at write %d, step %s is still running", allowed+1, s.ScriptName)
		}
		if st == workflow.StatePending {
			stillOwed := workflow.Phase(s.Phase) == workflow.PhaseAfter && owed[workflow.Scope(s.Scope)]
			if !stillOwed {
				t.Errorf("after reconciling a crash at write %d, step %s is pending and nothing is going to run it", allowed+1, s.ScriptName)
			}
		}
		if st == workflow.StateInterrupted && s.ExitCode != nil {
			t.Errorf("interrupted step %s carries exit code %d; nobody observed one", s.ScriptName, *s.ExitCode)
		}
		if st == workflow.StateSkipped && s.FinishedAt != nil && s.StartedAt == nil {
			t.Errorf("skipped step %s records a finish time and no start; internal/workflow refuses that pair, so the run cannot be recovered", s.ScriptName)
		}
	}

	// 2. No obligation is left mid-flight either: every one is settled
	//    or is explicitly requiring recovery. "Eligible" or "running"
	//    surviving a restart would be a promise nobody is keeping.
	outstanding := false

	for _, o := range obligations {
		switch {
		case o.State.Settled():
		case o.State.RequiresRecovery():
			outstanding = true
		default:
			t.Errorf("after reconciling a crash at write %d, the %s obligation is %q", allowed+1, o.Scope, o.State)
		}
	}

	// 3. The run is in exactly one of two positions, and which one is
	//    decided by whether a scope is outstanding -- never by where the
	//    crash happened to land.
	//
	//    Outstanding means recovery_required: the global scope's
	//    obligation is eligible from the moment the plan is committed,
	//    so a run interrupted anywhere in its work errs that way.
	//    Nothing outstanding means the work was demonstrably finished
	//    and only the summary row was missing, which is not a recovery
	//    and must not block the set.
	if outstanding {
		if run.State != string(workflow.StateRecoveryRequired) {
			t.Errorf("after a crash at write %d a scope is outstanding and the run is %q, want recovery_required", allowed+1, run.State)
		}
		if run.RecoveryState != string(workflow.RecoveryRequired) {
			t.Errorf("after a crash at write %d the run's recovery state is %q, want required", allowed+1, run.RecoveryState)
		}
	} else {
		if !workflow.State(run.State).Terminal() {
			t.Errorf("after a crash at write %d every scope is settled and the run is still %q", allowed+1, run.State)
		}
		if run.RecoveryState != string(workflow.RecoveryNone) {
			t.Errorf("after a crash at write %d every scope is settled and the run's recovery state is %q", allowed+1, run.RecoveryState)
		}
		if run.FinishedAt == nil {
			t.Errorf("after a crash at write %d the reconciled run has no finish time", allowed+1)
		}
	}

	// 4. The set is blocked and the spool is retained, which are the two
	//    things a resume-cleanup needs to exist later.
	if outstanding {
		if len(engine.RecoveryHolds()) == 0 {
			t.Errorf("after a crash at write %d nothing is holding the backup set", allowed+1)
		}

		blocked := false
		for _, set := range engine.SuspendedBackupSets() {
			if set.String() == run.BackupSetID {
				blocked = true
			}
		}
		if !blocked {
			t.Errorf("after a crash at write %d, backup set %s is not suspended", allowed+1, run.BackupSetID)
		}

		if _, err := os.Stat(run.ScriptSpoolRef); err != nil {
			t.Errorf("the spool of an unrecovered run is gone: %v", err)
		}

		domain := workflow.Run{State: workflow.State(run.State), RecoveryState: workflow.RecoveryState(run.RecoveryState)}
		if !domain.SpoolRetainable() {
			t.Errorf("a run in %q/%q reports its spool as reclaimable while its cleanup is still owed", run.State, run.RecoveryState)
		}
	}
}

// runSnapshot is the whole durable state of one run, for the idempotence
// comparison.
type runSnapshot struct {
	Run         state.WorkflowRun
	Steps       []state.WorkflowStep
	Obligations []workflow.CleanupObligation
}

func snapshotRun(t *testing.T, store *state.Journal, runID string) runSnapshot {
	t.Helper()

	ctx := context.Background()

	run, err := store.WorkflowRun(ctx, runID)
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	steps, err := store.WorkflowSteps(ctx, runID)
	if err != nil {
		t.Fatalf("WorkflowSteps: %v", err)
	}
	obligations, err := store.WorkflowCleanupObligations(ctx, runID)
	if err != nil {
		t.Fatalf("WorkflowCleanupObligations: %v", err)
	}

	return runSnapshot{Run: run, Steps: steps, Obligations: obligations}
}

// A run interrupted between two crash points must produce the SAME
// reconciliation whatever order the rows are read in, which is what
// "deterministic" means for a pure function of them.
//
// reconcileRun is that function, so it is called directly with the rows a
// real interrupted run left behind and its answer compared against the
// answer for the same rows shuffled.
func TestTheReconciliationIsAFunctionOfTheRowsAndNothingElse(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, fullTree())
	plan := tr.snapshot(t, "run-1")

	// Stop in the middle of the backup-set "before" stage, which is the
	// interesting position: one scope discharged nothing, one step is
	// running, one is pending.
	if _, crashed := runUntilCrash(t, h, plan, 6); !crashed {
		t.Fatal("the fixture run did not reach the crash point")
	}

	ctx := context.Background()

	run, err := h.store.WorkflowRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	steps, err := h.store.WorkflowSteps(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowSteps: %v", err)
	}
	obligations, err := h.store.WorkflowCleanupObligations(ctx, "run-1")
	if err != nil {
		t.Fatalf("WorkflowCleanupObligations: %v", err)
	}

	at := h.clock.Now()
	first := reconcileRun(run, steps, obligations, at)

	reversedSteps := make([]state.WorkflowStep, len(steps))
	for i, s := range steps {
		reversedSteps[len(steps)-1-i] = s
	}
	reversedObligations := make([]workflow.CleanupObligation, len(obligations))
	for i, o := range obligations {
		reversedObligations[len(obligations)-1-i] = o
	}

	second := reconcileRun(run, reversedSteps, reversedObligations, at)

	if first.Run != second.Run {
		t.Errorf("the run's reconciliation depends on row order:\n\t%+v\n\t%+v", first.Run, second.Run)
	}
	if len(first.Steps) != len(second.Steps) || len(first.Obligations) != len(second.Obligations) {
		t.Errorf("the reconciliation's size depends on row order: %d/%d steps, %d/%d obligations",
			len(first.Steps), len(second.Steps), len(first.Obligations), len(second.Obligations))
	}

	// Every conclusion is the same set, whatever order it was reached
	// in.
	got := map[string]workflow.State{}
	for _, s := range second.Steps {
		got[s.StepID] = s.State
	}
	for _, s := range first.Steps {
		if got[s.StepID] != s.State {
			t.Errorf("step %s reconciled to %q one way round and %q the other", s.StepID, s.State, got[s.StepID])
		}
	}

	if first.Run.To != workflow.StateRecoveryRequired {
		t.Errorf("an interrupted run reconciled to %q", first.Run.To)
	}
}

// The crash matrix, over the failure matrix.
//
// The suite above walks every crash point of a run in which nothing goes
// wrong. That is one row of the failure matrix, and it is the row least
// likely to be interesting: the positions where a reconciliation has to
// decide something are the ones where a step had already FAILED, or where
// a scope's cleanup had already failed, before the process stopped.
//
// What is asserted at each point is the pair of answers this design is
// made of. Either a scope is outstanding, in which case the run
// reconciles to recovery_required whatever else was going on -- or every
// scope is settled, in which case the run's work was demonstrably over
// and the reconciliation must reach THE SAME VERDICT the engine itself
// would have written: the same state, the same three statuses. A
// reconciliation that finalized a run to a different verdict than the
// uncrashed run would be a history that depends on whether the daemon
// survived its own last write.
func TestTheCrashMatrixAgreesWithTheUncrashedVerdict(t *testing.T) {
	t.Parallel()

	for _, scenario := range []struct {
		name string
		fail string
	}{
		{"all succeed", ""},
		{"global before fails", "10-mount.local.sh"},
		{"set after fails", "10-resume.remote.sh"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()

			// The verdict of the same run with nothing interrupting it,
			// and the number of durable writes it makes.
			verdict, total := uncrashedVerdict(t, scenario.fail)

			for allowed := range total {
				t.Run(fmt.Sprintf("after %d durable writes", allowed), func(t *testing.T) {
					t.Parallel()

					h := newHarness(t)
					tr := newTree(t, fullTree())
					if scenario.fail != "" {
						h.local.outcomes[scenario.fail] = failing(1)
						h.remote.outcomes[scenario.fail] = failing(1)
					}

					if _, crashed := runUntilCrash(t, h, tr.snapshot(t, "run-1"), allowed); !crashed {
						t.Fatalf("the run finished without reaching write %d", allowed+1)
					}

					restarted := &Engine{Store: h.store, Local: h.local, Remote: h.remote, Now: h.clock.Now}
					if _, err := restarted.Reconcile(context.Background(), h.clock.Now()); err != nil {
						t.Fatalf("Reconcile: %v", err)
					}

					run, err := h.store.WorkflowRun(context.Background(), "run-1")
					if errors.Is(err, state.ErrWorkflowRunNotFound) {
						return // the crash landed on the plan commit itself
					}
					if err != nil {
						t.Fatalf("WorkflowRun: %v", err)
					}

					obligations, err := h.store.WorkflowCleanupObligations(context.Background(), "run-1")
					if err != nil {
						t.Fatalf("WorkflowCleanupObligations: %v", err)
					}

					outstanding := false
					for _, o := range obligations {
						if !o.State.Settled() {
							outstanding = true
						}
					}

					if outstanding {
						if run.State != string(workflow.StateRecoveryRequired) {
							t.Fatalf("a scope is outstanding and the run is %q, want recovery_required", run.State)
						}

						return
					}

					got := runVerdict{
						state:    workflow.State(run.State),
						backup:   workflow.Status(run.BackupStatus),
						cleanup:  workflow.Status(run.CleanupStatus),
						workflow: workflow.Status(run.WorkflowStatus),
					}
					if got != verdict {
						t.Errorf("a run finalized by the reconciliation reads\n\t%+v\nand the same run uncrashed reads\n\t%+v", got, verdict)
					}
				})
			}
		})
	}
}

// runVerdict is the four fields every history surface reads: what the run
// ended as, and the three statuses that stay separate end to end.
type runVerdict struct {
	state    workflow.State
	backup   workflow.Status
	cleanup  workflow.Status
	workflow workflow.Status
}

// uncrashedVerdict runs one scenario through to completion and reports
// its verdict and how many durable writes it took, so the crash loop
// covers every position rather than a number somebody typed.
func uncrashedVerdict(t *testing.T, fail string) (runVerdict, int) {
	t.Helper()

	h := newHarness(t)
	tr := newTree(t, fullTree())
	if fail != "" {
		h.local.outcomes[fail] = failing(1)
		h.remote.outcomes[fail] = failing(1)
	}

	crash := &crashStore{Store: h.store, allowed: 1_000}
	h.engine.Store = crash

	res, err := h.engine.Run(context.Background(), RunRequest{
		Plan:        tr.snapshot(t, "run-1"),
		BackupSetID: tr.setID,
		Backup:      func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("the control run failed: %v", err)
	}
	if crash.crashed {
		t.Fatalf("the control run crashed at %d writes, so the bound is wrong", crash.count())
	}

	return runVerdict{
		state:    res.State,
		backup:   res.BackupStatus,
		cleanup:  res.CleanupStatus,
		workflow: res.WorkflowStatus,
	}, crash.count()
}
