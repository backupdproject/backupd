package workflowrun

import (
	"context"
	"sync"

	"github.com/backupdproject/backupd/core/internal/workflow"
)

// The log fan-out, and the one property it exists to guarantee: a slow or
// disconnected follower can never slow a hook down.
//
// # Why a queue and a journal rather than either alone
//
// A follower reading straight from the journal would poll, which for a
// live tail means either a busy loop or a delay an operator notices. A
// follower fed only by a queue would either block the producer when it
// fell behind (which is a hook whose stdout pipe fills up, which is a
// backup that stops) or lose output with no way to get it back.
//
// So there are two paths and one authority. Every record is written to
// the journal first and then OFFERED to each subscriber's bounded queue.
// An offer that would block is dropped and the subscriber is marked as
// having fallen behind. Catching up is a read of the journal from the
// last sequence the subscriber actually processed, which is gapless
// because the journal has every record and the sequence numbers are
// contiguous per run.
//
// # Why the subscriber owns its cursor
//
// Because only the subscriber knows what it has PROCESSED, as opposed to
// what was handed to its channel. A broker that tracked the cursor would
// have to guess at the boundary, and the guess is exactly where a gap or
// a duplicate comes from. So the protocol is: read records, remember the
// last sequence you handled, and if Lagged() is true, ask the engine for
// everything after that number.
//
// # Why dropping is per subscriber
//
// One stalled browser tab must not cost a CLI tail its live feed, and
// neither must cost the hook anything at all. Each subscription has its
// own queue and its own "fell behind" flag.

// DefaultSubscriberQueue is how many records a follower may be behind
// before it is dropped from the live path and told to catch up from the
// journal.
//
// A few hundred is enough that an ordinary consumer never lags on a
// burst, and small enough that a thousand followers of a noisy run cost
// megabytes rather than gigabytes. The number is not load-bearing for
// correctness: a subscriber that lags is not a subscriber that loses
// output, only one that has to read it from the journal.
const DefaultSubscriberQueue = 256

// Broker fans one run's log records out to its followers.
//
// The zero value is usable: a broker with no subscribers is a mutex and a
// nil map, which is what the overwhelmingly common case (nobody watching)
// costs.
type Broker struct {
	mu     sync.Mutex
	next   uint64
	subs   map[uint64]*Subscription
	closed bool
}

// Subscription is one follower's live feed of one run.
type Subscription struct {
	runID string
	id    uint64

	records chan workflow.StepLog

	broker *Broker

	mu     sync.Mutex
	lagged bool
	done   bool
}

// Subscribe returns a live feed of one run's log records.
//
// capacity of zero takes DefaultSubscriberQueue. The subscription must be
// closed when the follower goes away; a subscription that is never closed
// keeps a channel and a map entry alive for the life of the process,
// which is why every caller defers it.
func (b *Broker) Subscribe(runID string, capacity int) *Subscription {
	if capacity <= 0 {
		capacity = DefaultSubscriberQueue
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.subs == nil {
		b.subs = map[uint64]*Subscription{}
	}
	b.next++

	s := &Subscription{
		runID:   runID,
		id:      b.next,
		records: make(chan workflow.StepLog, capacity),
		broker:  b,
	}
	b.subs[s.id] = s

	return s
}

// publish offers one record to every subscriber of its run.
//
// It NEVER blocks, and that is the whole function. The send is a
// non-blocking select, so a full queue means the record is dropped for
// that subscriber and the subscriber is marked as having fallen behind;
// the hook that produced the bytes is not waiting on anybody.
func (b *Broker) publish(rec workflow.StepLog) {
	b.mu.Lock()
	subs := make([]*Subscription, 0, len(b.subs))
	for _, s := range b.subs {
		if s.runID == rec.RunID {
			subs = append(subs, s)
		}
	}
	b.mu.Unlock()

	for _, s := range subs {
		s.offer(rec)
	}
}

// offer hands one record to this subscription's queue, or drops it.
//
// The lock is held ACROSS the send, which is the opposite of what a
// "don't hold a lock over a channel operation" instinct says and is
// required here for two separate reasons.
//
// The first is that this send cannot block: it is a non-blocking select,
// so the lock is held for a queue slot's worth of work and never for as
// long as a reader takes. The second is what the alternative costs. A
// version that checked s.done, released the lock and then sent had a
// window a few instructions wide in which Close could close the channel
// -- and a send on a closed channel is a panic, in the goroutine reading
// a hook's stdout, which takes the daemon with it. A follower hanging up
// mid-output is an ordinary event (a closed browser tab), so that window
// is not theoretical.
//
// A subscription that has already been DROPPED is not offered to again
// either. Once a record has been lost, this queue's contents are
// contiguous only up to the hole, and putting later records into it
// would hand the follower 1, 2, then 301 with nothing to mark the gap.
// The live path stops at the drop and the journal is the authority from
// there (Lagged, Engine.LogsAfter).
func (s *Subscription) offer(rec workflow.StepLog) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.done || s.lagged {
		return
	}

	select {
	case s.records <- rec:
	default:
		s.lagged = true
	}
}

// Records is the channel live records arrive on. It is closed when the
// subscription is closed.
func (s *Subscription) Records() <-chan workflow.StepLog { return s.records }

// Lagged reports whether this subscription has missed a record because it
// was not reading fast enough.
//
// A follower that sees true asks the engine for everything after the last
// sequence it processed (Engine.LogsAfter) and carries on. It is not
// cleared by that: a subscription that lagged once has a hole in its
// LIVE stream, and the honest answer to "is your live stream complete" is
// no from then on, which is what makes a follower's catch-up read
// unconditional rather than a race.
func (s *Subscription) Lagged() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lagged
}

// Close releases the subscription. It is safe to call twice, which is
// one operator closing a tab while a shutdown closes everything.
//
// The channel is closed INSIDE the same critical section that sets done,
// and the pair is what makes offer's send safe: there is no moment at
// which done is set and the channel is still open, and none at which an
// offer holds the lock and the channel closes underneath it. The
// broker's own map is touched afterwards, outside s.mu, because the
// ordering there is the other way round -- publish takes the broker's
// lock and releases it before offering -- and taking the two in one
// order everywhere is what keeps that from being a deadlock.
func (s *Subscription) Close() {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()

		return
	}
	s.done = true
	close(s.records)
	s.mu.Unlock()

	s.broker.mu.Lock()
	delete(s.broker.subs, s.id)
	s.broker.mu.Unlock()
}

// LogsAfter is the catch-up read: one run's records with a sequence above
// cursor, oldest first, up to limit of them.
//
// It goes to the journal rather than to any buffer the broker kept,
// because the journal is the authority and a buffer would have the same
// bound (and therefore the same hole) the live queue has.
func (e *Engine) LogsAfter(ctx context.Context, runID string, cursor uint64, limit int) ([]workflow.StepLog, error) {
	if limit <= 0 {
		limit = DefaultSubscriberQueue
	}

	return e.Store.WorkflowStepLogsAfter(ctx, runID, cursor, limit)
}
