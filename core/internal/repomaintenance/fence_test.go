package repomaintenance_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/repomaintenance"
)

// The fence is the whole of issue #786's "unsafe concurrent destructive
// operations are prevented", and these tests ask it the three questions
// that claim decomposes into: does an exclusive operation exclude a
// destructive one, do two INDEPENDENT repositories still run at the same
// time, and does a caller that gave up waiting leave the fence usable.
//
// They are written against an observed timeline rather than against the
// fence's own bookkeeping. A test that asked the fence whether it thought
// it was held would pass against an implementation that recorded state
// and granted anyway.

// timeline records who was inside a fenced section and when, so overlap
// is a fact about the recording rather than about a sleep.
type timeline struct {
	mu     sync.Mutex
	inside map[string]int
	// overlaps records every pair that was inside at the same moment.
	overlaps map[string]bool
}

func newTimeline() *timeline {
	return &timeline{inside: map[string]int{}, overlaps: map[string]bool{}}
}

// enter marks who as running and records every other holder it now
// overlaps with.
func (t *timeline) enter(who string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for other, n := range t.inside {
		if n > 0 && other != who {
			t.overlaps[other+"|"+who] = true
			t.overlaps[who+"|"+other] = true
		}
	}

	t.inside[who]++
}

func (t *timeline) leave(who string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.inside[who]--
}

func (t *timeline) overlapped(a, b string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.overlaps[a+"|"+b]
}

// insideNow is how many holders named who are inside a section right
// now, which is what lets a test assert that something CANNOT get in
// while a barrier holds the other side.
func (t *timeline) insideNow(who string) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.inside[who]
}

func testDomain(t *testing.T, id string) model.RepositoryDomainID {
	t.Helper()

	domain, err := model.NewRepositoryDomainID(id)
	if err != nil {
		t.Fatalf("NewRepositoryDomainID(%q): %v", id, err)
	}

	return domain
}

// TestExclusiveMaintenanceAndDestructiveDeletesNeverOverlap is the
// fencing proof: with one repository, a full maintenance section and a
// snapshot-delete section cannot be inside at the same time, in either
// order, under contention.
func TestExclusiveMaintenanceAndDestructiveDeletesNeverOverlap(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fence := repomaintenance.NewFence()
	domain := testDomain(t, "one-repository")
	tl := newTimeline()

	const rounds = 40

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		for range rounds {
			release, err := fence.Exclusive(ctx, domain)
			if err != nil {
				t.Errorf("Exclusive: %v", err)

				return
			}

			tl.enter("maintenance")
			// Yield inside the section: an implementation that granted
			// both sides would almost certainly be caught by the
			// scheduler switching here.
			time.Sleep(time.Microsecond)
			tl.leave("maintenance")
			release()
		}
	}()

	go func() {
		defer wg.Done()

		for range rounds {
			release, err := fence.Shared(ctx, domain)
			if err != nil {
				t.Errorf("Shared: %v", err)

				return
			}

			tl.enter("delete")
			time.Sleep(time.Microsecond)
			tl.leave("delete")
			release()
		}
	}()

	wg.Wait()

	if tl.overlapped("maintenance", "delete") {
		t.Error("a full maintenance section and a snapshot-delete section were inside the fence at the same time for one repository")
	}
}

// TestIndependentRepositoriesAreNotSerialisedByTheFence is the other half
// of the requirement, and it is the half a global lock would fail: two
// repositories have nothing to do with each other and a maintenance
// window on one must not stop a delete on the other.
func TestIndependentRepositoriesAreNotSerialisedByTheFence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fence := repomaintenance.NewFence()
	first := testDomain(t, "first-repository")
	second := testDomain(t, "second-repository")

	releaseFirst, err := fence.Exclusive(ctx, first)
	if err != nil {
		t.Fatalf("Exclusive on the first repository: %v", err)
	}
	defer releaseFirst()

	// No timeout dance: if the second repository were serialised behind
	// the first this would block forever, and a deadline would only
	// convert that into a slow failure. A context that is already past
	// its deadline is granted only by a fence that did not have to wait.
	expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()

	releaseSecond, err := fence.Exclusive(expired, second)
	if err != nil {
		t.Fatalf("a second, independent repository could not be fenced while the first was held: %v", err)
	}

	releaseSecond()
}

// TestAWaiterThatGivesUpLeavesTheFenceUsable covers the failure that
// turns a safety mechanism into an outage: a caller whose context is
// cancelled while queued must not hold, and must not lose, the section it
// never got.
func TestAWaiterThatGivesUpLeavesTheFenceUsable(t *testing.T) {
	t.Parallel()

	fence := repomaintenance.NewFence()
	domain := testDomain(t, "cancelled-waiter")

	release, err := fence.Exclusive(context.Background(), domain)
	if err != nil {
		t.Fatalf("Exclusive: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	waited := make(chan error, 1)

	go func() {
		_, err := fence.Shared(ctx, domain)
		waited <- err
	}()

	// Give the waiter a moment to queue behind the exclusive holder, then
	// take it away.
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-waited:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled waiter returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled waiter never returned")
	}

	release()

	// The fence is still usable, and the abandoned waiter is not holding
	// the shared side of it.
	second, err := fence.Exclusive(context.Background(), domain)
	if err != nil {
		t.Fatalf("the fence was unusable after a waiter gave up: %v", err)
	}

	second()
}

// TestReleasingTwiceIsNotASecondRelease pins the one arithmetic mistake
// that would silently unfence a repository: a release called twice must
// not cancel out somebody else's hold.
func TestReleasingTwiceIsNotASecondRelease(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fence := repomaintenance.NewFence()
	domain := testDomain(t, "double-release")

	first, err := fence.Shared(ctx, domain)
	if err != nil {
		t.Fatalf("Shared: %v", err)
	}

	second, err := fence.Shared(ctx, domain)
	if err != nil {
		t.Fatalf("second Shared: %v", err)
	}

	first()
	first()

	// The second shared holder is still inside, so an exclusive request
	// must not be granted.
	expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()

	if release, err := fence.Exclusive(expired, domain); err == nil {
		release()
		t.Fatal("an exclusive section was granted while a shared holder was still inside; a double release had cancelled it out")
	}

	second()
}
