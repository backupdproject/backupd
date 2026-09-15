package repomaintenance

import (
	"context"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/model"
)

// The fence's FIFO promise -- "a waiter that arrives after a queued
// exclusive request waits behind it" -- is the one property that cannot
// be asked from outside the package, because proving it needs the test to
// know WHEN a request is queued rather than guessing with a sleep.
//
// It is worth a test of its own because the failure it prevents is
// invisible: a fence that granted every compatible request immediately
// would pass every other test in this package, be measurably more
// concurrent, and starve full maintenance on exactly the repositories
// that need it -- the ones whose retention pass deletes something often
// enough that the shared side is never empty. The symptom is a bucket
// that grows forever while the logs say maintenance is scheduled.
//
// What this file reads from the fence's own state is only ever "has this
// request been queued or granted yet", which is what makes the waiting
// deterministic and the negative assertion bite. What entered, and in
// what order, is observed from outside.

// held is the fence's view of one repository.
type held struct {
	shared    int
	exclusive bool
	waiters   int
}

func (f *Fence) state(domain model.RepositoryDomainID) held {
	f.mu.Lock()
	defer f.mu.Unlock()

	g := f.gates[domain]
	if g == nil {
		return held{}
	}

	return held{shared: g.shared, exclusive: g.exclusive, waiters: len(g.waiters)}
}

// awaitWaiters blocks until n requests are queued for domain, which is
// how this test knows a goroutine has really got as far as the queue.
func (f *Fence) awaitWaiters(t *testing.T, domain model.RepositoryDomainID, n int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if f.state(domain).waiters == n {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatalf("%d requests queued for %s, want %d", f.state(domain).waiters, domain, n)
}

// TestAQueuedFullMaintenanceIsNotOvertakenByALaterDelete is the
// non-barging proof, and every step of it is a barrier rather than a
// sleep.
//
// One delete is inside the shared side. A full maintenance queues behind
// it. Then a SECOND delete arrives, which is compatible with the one
// already inside and which a fence that granted whatever it could would
// admit. It has to wait for the maintenance, or the maintenance waits
// forever.
func TestAQueuedFullMaintenanceIsNotOvertakenByALaterDelete(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fence := NewFence()

	domain, err := model.NewRepositoryDomainID("starved-by-deletes")
	if err != nil {
		t.Fatalf("NewRepositoryDomainID: %v", err)
	}

	entered := make(chan string, 2)

	firstDelete, err := fence.Shared(ctx, domain)
	if err != nil {
		t.Fatalf("the first delete could not take the shared side: %v", err)
	}

	releaseMaintenance := make(chan struct{})
	maintenanceLeft := make(chan struct{})

	go func() {
		defer close(maintenanceLeft)

		release, err := fence.Exclusive(ctx, domain)
		if err != nil {
			entered <- "maintenance failed: " + err.Error()

			return
		}

		entered <- "maintenance"
		<-releaseMaintenance
		release()
	}()

	fence.awaitWaiters(t, domain, 1)

	lateDeleteLeft := make(chan struct{})

	go func() {
		defer close(lateDeleteLeft)

		release, err := fence.Shared(ctx, domain)
		if err != nil {
			entered <- "late delete failed: " + err.Error()

			return
		}

		entered <- "late delete"
		release()
	}()

	fence.awaitWaiters(t, domain, 2)

	// Neither has entered, because the first delete is still inside.
	if state := fence.state(domain); state.exclusive || state.shared != 1 || state.waiters != 2 {
		t.Fatalf("with one delete inside and two requests queued the fence holds %+v", state)
	}

	assertNothingEntered(t, entered)

	firstDelete()

	if got := <-entered; got != "maintenance" {
		t.Fatalf("%q entered first; the queued full maintenance was next in line", got)
	}

	// The later delete is still queued while the maintenance is inside.
	// This is the assertion a barging fence fails: it would have granted
	// the compatible request the moment the first delete left, and the
	// maintenance would have been behind it.
	if state := fence.state(domain); !state.exclusive || state.shared != 0 || state.waiters != 1 {
		t.Fatalf("with full maintenance inside the fence holds %+v; the later delete got in beside it", state)
	}

	assertNothingEntered(t, entered)

	close(releaseMaintenance)
	<-maintenanceLeft

	if got := <-entered; got != "late delete" {
		t.Fatalf("after the maintenance released, %q entered, want the delete that had been waiting", got)
	}

	<-lateDeleteLeft
}

// assertNothingEntered is the negative half. It is paired with a check of
// the fence's own state at every call site, because "nothing has arrived
// on a channel yet" is on its own a statement about scheduling.
func assertNothingEntered(t *testing.T, entered <-chan string) {
	t.Helper()

	select {
	case who := <-entered:
		t.Fatalf("%q entered a fenced section it should have been waiting for", who)
	default:
	}
}
