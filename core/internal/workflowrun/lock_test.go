package workflowrun

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/workflow"
)

// The backup-set lock: it spans all five stages, it is per set, and it
// refuses rather than queues (#811's Locking/concurrency section).

// A second run of the same set is refused while the first one's CLEANUP
// is still going. That is the case the lock exists for: a lock scoped to
// the backup would let a new run's "before" hook mount something while
// the old run's "after" hook was unmounting it.
func TestASecondRunIsRefusedUntilTheFirstRunsCleanupHasFinished(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	var (
		duringCleanup error
		sawCleanup    bool
	)

	// Snapshotted once, outside: a spool is created once per run id, so
	// re-planning the same id inside a hook that runs more than once
	// would fail for a reason that has nothing to do with the lock.
	second := tr.snapshot(t, "run-2")

	h.local.outcomes["90-unmount.local.sh"] = func(context.Context, StepRequest) (StepOutcome, error) {
		if sawCleanup {
			return exited(0), nil
		}
		sawCleanup = true

		// A second run, attempted from inside the first run's cleanup.
		_, duringCleanup = h.engine.Run(context.Background(), RunRequest{
			Plan:        second,
			BackupSetID: tr.setID,
			Backup:      func(context.Context) error { return nil },
		})

		return exited(0), nil
	}

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !sawCleanup {
		t.Fatal("the cleanup stage never ran, so nothing was attempted during it")
	}
	if !errors.Is(duringCleanup, ErrRunInFlight) {
		t.Fatalf("a run started during the previous run's cleanup returned %v, want ErrRunInFlight", duringCleanup)
	}

	// And once the cleanup is over the set is free again.
	if _, err := h.engine.Run(context.Background(), RunRequest{
		Plan:        tr.snapshot(t, "run-3"),
		BackupSetID: tr.setID,
		Backup:      func(context.Context) error { return nil },
	}); err != nil {
		t.Fatalf("a run after the cleanup finished was refused: %v", err)
	}
}

// The lock is held for the whole of a run, from before the first hook
// until after the last cleanup step, and it is released afterwards.
func TestTheLockSpansTheWholeRun(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setAfter:     {"50-resume.local.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	held := map[string]bool{}
	for _, name := range []string{"10-mount.local.sh", "50-resume.local.sh", "90-unmount.local.sh"} {
		h.local.outcomes[name] = func(context.Context, StepRequest) (StepOutcome, error) {
			holder, busy := h.engine.locks().Holder(tr.setID)
			held[holder] = busy

			return exited(0), nil
		}
	}

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !held["run-1"] {
		t.Errorf("the set lock was not held by run-1 during its stages: %v", held)
	}
	if _, busy := h.engine.locks().Holder(tr.setID); busy {
		t.Error("the set lock is still held after the run finished")
	}
}

// Global hooks are inherited configuration, not a deployment-wide
// critical section: two different backup sets run at the same time, each
// with its own copy of the global stages.
func TestDifferentSetsRunConcurrently(t *testing.T) {
	t.Parallel()

	locks := NewSetLocks()

	first, err := model.NewBackupSetID("production", "postgres")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	second, err := model.NewBackupSetID("production", "uploads")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	releaseFirst, err := locks.Acquire(first, "run-1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if _, err := locks.Acquire(second, "run-2"); err != nil {
		t.Fatalf("a run of a different set was refused: %v", err)
	}
	if _, err := locks.Acquire(first, "run-3"); !errors.Is(err, ErrRunInFlight) {
		t.Fatalf("a second run of the SAME set returned %v, want ErrRunInFlight", err)
	}

	releaseFirst()

	if _, err := locks.Acquire(first, "run-4"); err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
}

// Releasing twice is a no-op rather than a way to hand one set's lock to
// two runs. The engine defers the release and also releases on its
// refusal paths, so the second call has to be safe.
func TestReleasingALockTwiceDoesNotFreeSomebodyElsesRun(t *testing.T) {
	t.Parallel()

	locks := NewSetLocks()

	set, err := model.NewBackupSetID("production", "postgres")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	release, err := locks.Acquire(set, "run-1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	if _, err := locks.Acquire(set, "run-2"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	release()

	if _, err := locks.Acquire(set, "run-3"); !errors.Is(err, ErrRunInFlight) {
		t.Fatal("a second release of an already-released lock freed the run that had taken it since")
	}
}

// Concurrent acquisition of one set's lock has exactly one winner.
func TestOnlyOneRunWinsTheLock(t *testing.T) {
	t.Parallel()

	locks := NewSetLocks()

	set, err := model.NewBackupSetID("production", "postgres")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	const racers = 32

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)

	for i := range racers {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			if _, err := locks.Acquire(set, "run"); err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if winners != 1 {
		t.Fatalf("%d runs took one backup set's lock at once", winners)
	}
}

// A backup set with no workflow configured behaves exactly as it did
// before this package existed: the backup runs, and nothing else
// happens. No run row, no spool, no obligation, no log.
//
// #811's "adds no meaningful overhead to today's run" is a claim about
// this path, and the way it goes wrong is not a slow function -- it is
// somebody adding a journal read or a plan commit "for consistency".
func TestTheZeroHookPathRecordsNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	set := testSetID(t)

	called := false
	res, err := h.engine.Run(context.Background(), RunRequest{
		BackupSetID: set,
		Backup: func(context.Context) error {
			called = true

			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !called {
		t.Fatal("the backup did not run")
	}
	if res.RunID != "" {
		t.Errorf("a set with no workflow produced run %q", res.RunID)
	}

	runs, err := h.store.WorkflowRunsInStates(context.Background(), workflow.RunStates()...)
	if err != nil {
		t.Fatalf("WorkflowRunsInStates: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("a set with no workflow wrote %d run rows", len(runs))
	}

	// The error the backup returned comes back unchanged, because the
	// caller's own error handling is what it was before.
	boom := errors.New("the repository is full")
	res, err = h.engine.Run(context.Background(), RunRequest{
		BackupSetID: set,
		Backup:      func(context.Context) error { return boom },
	})
	if err != nil {
		t.Fatalf("Run returned an error of its own: %v", err)
	}
	if !errors.Is(res.BackupErr, boom) {
		t.Errorf("the backup's error came back as %v", res.BackupErr)
	}
}

// The zero-hook path's cost, measured. What this guards against is a
// journal read or a durable write appearing on the path a deployment
// with no hooks takes on every backup of every set.
func BenchmarkZeroHookRun(b *testing.B) {
	t := &testing.T{}

	h := newHarness(t)
	set := testSetID(t)

	req := RunRequest{
		BackupSetID: set,
		Backup:      func(context.Context) error { return nil },
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, err := h.engine.Run(ctx, req); err != nil {
			b.Fatalf("Run: %v", err)
		}
	}
}

// The same backup, called directly, so the benchmark above has something
// to be compared against rather than a number on its own.
func BenchmarkBackupWithoutTheEngine(b *testing.B) {
	backup := func(context.Context) error { return nil }
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if err := backup(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
