package repomaintenance

import (
	"context"
	"sync"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
)

// This file is the exclusive-operation fencing issue #786 requires, and
// the shape of it is decided by two sentences that pull in opposite
// directions: full maintenance is an exclusive repository operation, and
// operations on independent repositories may run concurrently.
//
// So the unit of exclusion is the Repository Domain and never the
// process. A single mutex around "maintenance" would satisfy the first
// sentence and break the second, and the way that failure presents is not
// a corrupted repository but a deployment with four NAS repositories
// whose nightly windows have quietly become serial.
//
// # What this fences, and what it deliberately does not
//
// The exclusive side is full maintenance. The shared side is a
// DESTRUCTIVE snapshot operation -- a manifest delete, which is what
// internal/snapshotretention's pass performs and the only thing in this
// product that removes anything from a repository. Everything else
// (taking a snapshot, reading one, restoring one, verifying one) is
// unfenced, on purpose: the embedded engine's own safety margins are what
// make maintenance safe to run beside a backup in progress
// (maintenance.SafetyFull, see the adapter's Maintain), and fencing
// backups behind a maintenance window would trade a risk the vendor has
// already handled for a certainty that a long full maintenance stops
// tonight's backup.
//
// # Why the fence is in-process, and what that does and does not claim
//
// It is a coordination primitive between this daemon's own passes. It is
// not a distributed lock and does not pretend to be one: two backupd
// instances pointed at one repository are prevented from maintaining it
// concurrently by the ownership record (run.go), which is a durable claim
// they can both read, and by the engine's own safety parameters, which
// are what make even an unfenced concurrent GC non-destructive.
//
// # Why acquisition is FIFO
//
// A queue that granted every compatible request immediately would starve
// full maintenance on a repository whose retention pass runs often
// enough: each delete would slip in while the previous one was leaving,
// and the exclusive waiter would sit behind a section that is never
// empty. So a waiter that arrives after a queued exclusive request waits
// behind it, which bounds the wait of the operation that reclaims space
// at the cost of a bounded wait on a delete that is not urgent.

// Fence serialises the operations on one repository that must not
// interleave, and only those on ONE repository: two domains never contend
// with each other.
//
// The zero value is not usable; call NewFence. A Fence is safe for
// concurrent use and holds no repository state, only the state of who is
// currently inside which section.
type Fence struct {
	mu    sync.Mutex
	gates map[model.RepositoryDomainID]*gate
}

// NewFence returns a fence with nothing held.
func NewFence() *Fence {
	return &Fence{gates: map[model.RepositoryDomainID]*gate{}}
}

// gate is one repository's exclusion state. Every field is guarded by the
// Fence's own mutex rather than by a per-gate one: acquisitions happen a
// handful of times per backup window, so there is nothing to gain from
// finer locking and a second lock is a second ordering to get wrong.
type gate struct {
	// shared is how many destructive-operation holders are inside.
	shared int

	// exclusive is whether the one exclusive holder is inside.
	exclusive bool

	// waiters is the FIFO queue. See the file header for why it is a
	// queue and not a free-for-all.
	waiters []*waiter
}

// idle reports whether nothing holds or wants this gate, which is when it
// can be forgotten.
func (g *gate) idle() bool { return !g.exclusive && g.shared == 0 && len(g.waiters) == 0 }

type waiter struct {
	exclusive bool

	// ready is closed when this waiter has been granted its section.
	ready chan struct{}

	// granted is the same fact readable under the fence's lock, which is
	// what makes the cancellation path able to tell "I was granted while
	// I was giving up" from "I never got it". Handing back a section
	// nobody is inside is the only correct answer to the first, and
	// leaving it held is how a cancelled retention pass wedges every
	// later maintenance window.
	granted bool
}

// Exclusive acquires the whole repository: nothing else this fence knows
// about runs against domain until the returned release is called.
//
// It is what full maintenance takes. The returned release is idempotent,
// so a caller may defer it and also call it early.
func (f *Fence) Exclusive(ctx context.Context, domain model.RepositoryDomainID) (func(), error) {
	return f.acquire(ctx, domain, true)
}

// Shared acquires the destructive-operation side: any number of these run
// together, and none of them runs while an exclusive holder is inside.
//
// It is what a snapshot delete takes. See GuardSnapshots, which is how a
// retention pass takes it without knowing this package exists.
func (f *Fence) Shared(ctx context.Context, domain model.RepositoryDomainID) (func(), error) {
	return f.acquire(ctx, domain, false)
}

// acquire is both entry points. A request that can be granted without
// waiting is granted without consulting ctx: an already-cancelled caller
// that would have waited for nothing is better served by being let
// through to fail on its own terms than by a fence error that describes
// the queue rather than the cancellation.
func (f *Fence) acquire(ctx context.Context, domain model.RepositoryDomainID, exclusive bool) (func(), error) {
	f.mu.Lock()

	g := f.gates[domain]
	if g == nil {
		g = &gate{}
		f.gates[domain] = g
	}

	w := &waiter{exclusive: exclusive, ready: make(chan struct{})}
	g.waiters = append(g.waiters, w)
	f.dispatch(g)

	granted := w.granted
	f.mu.Unlock()

	if granted {
		return f.releaser(domain, exclusive), nil
	}

	select {
	case <-w.ready:
		return f.releaser(domain, exclusive), nil

	case <-ctx.Done():
		f.mu.Lock()

		if w.granted {
			// Granted in the instant we stopped waiting for it. The
			// section is really held, so it is really given back.
			f.mu.Unlock()
			f.releaser(domain, exclusive)()

			return nil, ctx.Err()
		}

		for i, queued := range g.waiters {
			if queued == w {
				g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)

				break
			}
		}

		// Leaving the queue can unblock whatever was behind this waiter:
		// a shared request queued behind an exclusive one that has given
		// up is now at the head.
		f.dispatch(g)
		f.forgetIfIdle(domain, g)
		f.mu.Unlock()

		return nil, ctx.Err()
	}
}

// dispatch grants as much of the head of the queue as the gate's current
// state allows. It must be called with the fence's lock held.
func (f *Fence) dispatch(g *gate) {
	for len(g.waiters) > 0 {
		w := g.waiters[0]

		if w.exclusive {
			if g.exclusive || g.shared > 0 {
				return
			}

			g.exclusive = true
		} else {
			if g.exclusive {
				return
			}

			g.shared++
		}

		w.granted = true
		close(w.ready)
		g.waiters = g.waiters[1:]
	}
}

// releaser returns the idempotent release for one granted section.
func (f *Fence) releaser(domain model.RepositoryDomainID, exclusive bool) func() {
	var once sync.Once

	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()

			g := f.gates[domain]
			if g == nil {
				return
			}

			if exclusive {
				g.exclusive = false
			} else if g.shared > 0 {
				g.shared--
			}

			f.dispatch(g)
			f.forgetIfIdle(domain, g)
		})
	}
}

// forgetIfIdle drops a gate nobody holds or wants, so that a long-lived
// daemon's fence does not grow one entry per repository it has ever
// touched. Must be called with the lock held.
func (f *Fence) forgetIfIdle(domain model.RepositoryDomainID, g *gate) {
	if g.idle() {
		delete(f.gates, domain)
	}
}

// SnapshotRepository is the destructive half of a repository as a
// retention pass sees it: look one manifest up, remove one manifest.
//
// It is deliberately the same method set as internal/snapshotretention's
// own Repository port, so that GuardSnapshots' result can be handed
// straight to a Pruner. It is restated here rather than imported because
// the dependency must point this way round: retention decides what to
// delete and knows nothing about maintenance windows, and a fence it had
// to be told about would be a fence somebody can forget.
type SnapshotRepository interface {
	LookupSnapshot(ctx context.Context, id backupengine.SnapshotID) (backupengine.SnapshotInfo, error)
	DeleteSnapshot(ctx context.Context, id backupengine.SnapshotID) error
}

// GuardSnapshots wraps a repository so that every manifest delete against
// domain takes the shared side of this fence, and therefore cannot run
// while a full maintenance is inside the exclusive side.
//
// LookupSnapshot is deliberately NOT fenced. It reads, so it can never be
// half of a destructive race, and fencing it would put a retention
// pass's whole planning phase behind a maintenance window that has no
// reason to block it. The delete that a stale plan might reach is fenced,
// and internal/snapshotretention re-reads its own evidence immediately
// before every delete, so a plan drawn during maintenance is re-decided
// after it.
func (f *Fence) GuardSnapshots(domain model.RepositoryDomainID, repo SnapshotRepository) SnapshotRepository {
	return guarded{fence: f, domain: domain, repo: repo}
}

type guarded struct {
	fence  *Fence
	domain model.RepositoryDomainID
	repo   SnapshotRepository
}

func (g guarded) LookupSnapshot(ctx context.Context, id backupengine.SnapshotID) (backupengine.SnapshotInfo, error) {
	return g.repo.LookupSnapshot(ctx, id)
}

func (g guarded) DeleteSnapshot(ctx context.Context, id backupengine.SnapshotID) error {
	release, err := g.fence.Shared(ctx, g.domain)
	if err != nil {
		return err
	}
	defer release()

	return g.repo.DeleteSnapshot(ctx, id)
}
