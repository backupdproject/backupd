package repomaintenance_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/repomaintenance"
)

// One repository has one maintenance owner, and the tests in
// ownership_test.go ask that of operations that happen one after the
// other. These ask it of operations that happen at the same time, which
// is the only way the question is worth asking: every single-owner
// mechanism in this package is a read followed by a write, and a read
// followed by a write is two owners unless something makes it one
// operation.
//
// They are barrier-driven rather than timed. Every goroutine reports that
// it is ready and then blocks on one channel, so the contention is a fact
// about the test rather than a hope about the scheduler, and the
// assertions are counts of what really happened -- how many claims
// succeeded, how many times the repository was maintained -- rather than
// observations that nothing was seen to overlap.

// racers runs fn in n goroutines released simultaneously, and returns
// once they have all finished.
func racers(n int, fn func(i int)) {
	var (
		start = make(chan struct{})
		ready sync.WaitGroup
		done  sync.WaitGroup
	)

	ready.Add(n)
	done.Add(n)

	for i := range n {
		go func() {
			defer done.Done()

			ready.Done()
			<-start

			fn(i)
		}()
	}

	ready.Wait()
	close(start)
	done.Wait()
}

// TestOnlyOneOfManyInstancesClaimingAFreshRepositoryMaintainsIt is the
// single-owner criterion under contention, and the assertion that matters
// is the last one: the repository was maintained ONCE.
//
// Eight instances, none of which has ever seen this repository, all
// deciding at the same moment that it is unowned. Before the claim was
// atomic every one of them read "no record", wrote itself in as owner and
// went on to rewrite the same repository's indexes.
func TestOnlyOneOfManyInstancesClaimingAFreshRepositoryMaintainsIt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := testStore(t)
	domain := testDomain(t, "contended-domain")
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	// One repository, shared: the count of Maintain calls on it is the
	// count of instances that got through.
	repo := &fakeRepository{ran: true}

	const instances = 8

	var (
		mu       sync.Mutex
		ran      []backupengine.MaintenanceOwner
		refused  int
		puzzling []error
	)

	racers(instances, func(i int) {
		owner := backupengine.MaintenanceOwner("instance-" + strconv.Itoa(i))

		// A fence each, because a fence is in-process and these are
		// standing in for separate processes: two daemons started against
		// one state directory, or a daemon and an operator's command.
		// Sharing one would have the in-process maintenance lease
		// serialise the claims, and the claim is what is under test.
		runner := testRunner(string(owner), store, repomaintenance.NewFence())

		_, err := runner.Run(ctx, domain, repo, backupengine.MaintenanceFull, now)

		mu.Lock()
		defer mu.Unlock()

		switch {
		case err == nil:
			ran = append(ran, owner)
		case errors.Is(err, repomaintenance.ErrNotOwner):
			refused++
		default:
			puzzling = append(puzzling, err)
		}
	})

	if len(puzzling) > 0 {
		t.Fatalf("concurrent maintenance failed for unexpected reasons: %v", puzzling)
	}

	if len(ran) != 1 {
		t.Fatalf("%d of %d instances maintained the repository, want exactly 1: %v", len(ran), instances, ran)
	}

	if refused != instances-1 {
		t.Errorf("%d instances were refused as not the owner, want %d", refused, instances-1)
	}

	if got := repo.maintained(); len(got) != 1 {
		t.Fatalf("the repository was maintained %d times (%v); a repository has exactly one maintenance owner", len(got), got)
	}

	record, err := store.Load(ctx, domain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if record.Owner != ran[0] {
		t.Errorf("the record names %q as owner; the instance that maintained the repository was %q", record.Owner, ran[0])
	}

	if record.Runs != 1 {
		t.Errorf("the record counts %d runs after one maintenance window: a losing instance wrote over the winner's record", record.Runs)
	}
}

// TestTwoInstancesWithSeparateStateDirectoriesAreNotCoordinated pins the
// boundary of what the ownership record can promise, because it is a
// promise that is easy to over-read and expensive to be wrong about.
//
// The record is LOCAL: one file per repository under the instance's own
// state directory. Two instances that share a repository but not a state
// directory therefore each claim it, and nothing in this package can see
// that. What keeps that non-destructive is the engine's own safety
// margins (maintenance.SafetyFull, see the adapter's Maintain), not this
// record.
//
// Closing it needs an ownership lease in storage BOTH instances can read,
// acquired atomically, and the embedded engine cannot express one: kopia
// refuses blob.PutOptions.DoNotRecreate on every backend this product
// supports and its own maintenance exclusivity is a local lock file. See
// ADR 0017; it is tracked against #788, where maintenance is wired up.
//
// This test exists so that the day somebody does move the record into
// shared storage, the claim this package makes about itself has to be
// updated with it.
func TestTwoInstancesWithSeparateStateDirectoriesAreNotCoordinated(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	domain := testDomain(t, "two-state-dirs")
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	first := testRunner("instance-a", testStore(t), repomaintenance.NewFence())
	second := testRunner("instance-b", testStore(t), repomaintenance.NewFence())

	if _, err := first.Claim(ctx, domain); err != nil {
		t.Fatalf("the first instance's claim: %v", err)
	}

	if _, err := second.Claim(ctx, domain); err != nil {
		t.Fatalf("the second instance's claim: %v", err)
	}

	// Both own it, as far as either can tell. If this ever starts
	// failing, the record has become shared and the package doc, ADR 0017
	// and this test are all describing something that is no longer true.
	for _, runner := range []repomaintenance.Runner{first, second} {
		if _, err := runner.Run(ctx, domain, &fakeRepository{ran: true}, backupengine.MaintenanceFull, now); err != nil {
			t.Fatalf("%s could not maintain the repository it believes it owns: %v", runner.Owner, err)
		}
	}
}

// TestConcurrentTransfersFromOneOwnerHaveExactlyOneWinner is the
// compare-and-set that makes a handover a handover.
//
// Two administrators, each moving the same repository from the owner they
// both correctly believe owns it, to two different instances. Both
// expected-owner checks pass. Exactly one of them may become the truth,
// because the alternative is a repository whose record says instance-b
// while instance-c was told it now owns maintenance.
func TestConcurrentTransfersFromOneOwnerHaveExactlyOneWinner(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := testStore(t)
	domain := testDomain(t, "contended-handover")

	origin := testRunner("instance-a", store, repomaintenance.NewFence())
	if _, err := origin.Claim(ctx, domain); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	const administrators = 6

	var (
		mu       sync.Mutex
		accepted []backupengine.MaintenanceOwner
		refused  int
		puzzling []error
	)

	racers(administrators, func(i int) {
		to := backupengine.MaintenanceOwner("instance-" + strconv.Itoa(i))

		_, err := repomaintenance.Transfer(ctx, store, domain, "instance-a", to)

		mu.Lock()
		defer mu.Unlock()

		switch {
		case err == nil:
			accepted = append(accepted, to)
		case errors.Is(err, repomaintenance.ErrOwnershipMoved):
			refused++
		default:
			puzzling = append(puzzling, err)
		}
	})

	if len(puzzling) > 0 {
		t.Fatalf("concurrent transfers failed for unexpected reasons: %v", puzzling)
	}

	if len(accepted) != 1 {
		t.Fatalf("%d of %d concurrent transfers from one owner succeeded, want exactly 1: %v",
			len(accepted), administrators, accepted)
	}

	if refused != administrators-1 {
		t.Errorf("%d transfers were refused as having moved, want %d", refused, administrators-1)
	}

	record, err := store.Load(ctx, domain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if record.Owner != accepted[0] {
		t.Errorf("the record names %q as owner; the only transfer that was accepted moved it to %q",
			record.Owner, accepted[0])
	}
}

// TestAWindowThatStartedBeforeATransferCannotUndoIt is the stale write,
// and it is the one with teeth: a maintenance window is long, a handover
// takes an instant, and an owner that wrote back what it read at the
// start would silently make itself the owner again -- of a repository an
// administrator has just given to somebody else, which is then maintained
// by both.
//
// The window is held open at a barrier inside Maintain, so the transfer
// really does land in the middle of it.
func TestAWindowThatStartedBeforeATransferCannotUndoIt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := testStore(t)
	domain := testDomain(t, "transferred-mid-window")
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	inside := make(chan struct{})
	resume := make(chan struct{})

	repo := &blockingRepository{
		inside: inside,
		resume: resume,
	}

	outgoing := testRunner("instance-a", store, repomaintenance.NewFence())

	type outcome struct {
		result repomaintenance.Result
		err    error
	}

	finished := make(chan outcome, 1)

	go func() {
		result, err := outgoing.Run(ctx, domain, repo, backupengine.MaintenanceFull, now)
		finished <- outcome{result, err}
	}()

	<-inside

	// Mid-window: the record the running window read is now one revision
	// behind.
	moved, err := repomaintenance.Transfer(ctx, store, domain, "instance-a", "instance-b")
	if err != nil {
		t.Fatalf("the transfer: %v", err)
	}

	close(resume)

	got := <-finished

	if !errors.Is(got.err, backupengine.ErrMaintenanceOwnershipStale) {
		t.Fatalf("the window that outlived its record returned %v, want a stale-record refusal", got.err)
	}

	record, err := store.Load(ctx, domain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if record.Owner != "instance-b" {
		t.Fatalf("the record names %q as owner; the transfer to instance-b was reverted by a window that started before it", record.Owner)
	}

	if record.Runs != 0 || len(record.History) != 0 {
		t.Errorf("the refused write left %d runs and %d history entries on the new owner's record",
			record.Runs, len(record.History))
	}

	if !record.NextEligible.Equal(moved.NextEligible) {
		t.Errorf("the refused write changed NextEligible to %v, want the transfer's %v", record.NextEligible, moved.NextEligible)
	}

	// And the result reports the record that is real -- the one the
	// window was decided from -- rather than the one it failed to write.
	if got.result.Record.Runs != 0 {
		t.Errorf("the refused window reports a record with %d runs, as though it had been stored", got.result.Record.Runs)
	}
}

// TestTwoConcurrentDuePassesMaintainOnceAndKeepOneHistory is the
// double-run, which the exclusive fence cannot prevent and would in fact
// tidy into a pair of consecutive windows.
//
// Two scheduled passes wake at the same moment, both read a repository
// that has never been maintained, and both are right that a full
// maintenance is due. Serialising them is not enough: the second would
// then run a full maintenance against a repository that had one a
// millisecond ago, and write its own outcome over the first's. The
// decision has to be taken again once the first has finished.
func TestTwoConcurrentDuePassesMaintainOnceAndKeepOneHistory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := testStore(t)
	fence := repomaintenance.NewFence()
	domain := testDomain(t, "two-scheduled-passes")
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	runner := testRunner("instance-a", store, fence)
	repo := &fakeRepository{ran: true}

	var (
		mu        sync.Mutex
		attempted int
		puzzling  []error
	)

	racers(2, func(int) {
		result, err := runner.RunDue(ctx, domain, repo, now)

		mu.Lock()
		defer mu.Unlock()

		if err != nil {
			puzzling = append(puzzling, err)

			return
		}

		if result.Attempted {
			attempted++
		}
	})

	if len(puzzling) > 0 {
		t.Fatalf("concurrent due passes failed: %v", puzzling)
	}

	if attempted != 1 {
		t.Errorf("%d of 2 concurrent due passes attempted maintenance, want 1", attempted)
	}

	if got := repo.maintained(); len(got) != 1 {
		t.Fatalf("the repository was maintained %d times (%v) by two passes that were due at the same moment", len(got), got)
	}

	record, err := store.Load(ctx, domain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if record.Runs != 1 || len(record.History) != 1 {
		t.Errorf("the record counts %d runs with %d history entries after one window; the second pass clobbered the first's",
			record.Runs, len(record.History))
	}
}

// TestACancelledMaintenanceLeaseIsNotLeftHeld: the lease is taken before
// a window's own work and released after it, so a caller that gives up
// waiting for it must not leave the repository unable to be maintained
// ever again.
func TestACancelledMaintenanceLeaseIsNotLeftHeld(t *testing.T) {
	t.Parallel()

	fence := repomaintenance.NewFence()
	domain := testDomain(t, "abandoned-lease")

	held, err := fence.SerialiseMaintenance(context.Background(), domain)
	if err != nil {
		t.Fatalf("SerialiseMaintenance: %v", err)
	}

	giveUp, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := fence.SerialiseMaintenance(giveUp, domain); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled waiter got %v, want context.Canceled", err)
	}

	held()

	again, err := fence.SerialiseMaintenance(context.Background(), domain)
	if err != nil {
		t.Fatalf("the lease could not be taken after a waiter gave up: %v", err)
	}

	again()
}

// blockingRepository holds a maintenance window open at a barrier, so
// that something else can happen while it is genuinely in progress.
type blockingRepository struct {
	// inside is closed when Maintain has been entered.
	inside chan struct{}

	// resume is waited on before Maintain returns.
	resume chan struct{}

	mu    sync.Mutex
	calls int
}

func (b *blockingRepository) Maintain(_ context.Context, mode backupengine.MaintenanceMode) (backupengine.MaintenanceReport, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()

	close(b.inside)
	<-b.resume

	return backupengine.MaintenanceReport{Mode: mode, Ran: true}, nil
}

func (b *blockingRepository) Stats(context.Context) (backupengine.RepositoryStats, error) {
	return backupengine.RepositoryStats{}, nil
}
