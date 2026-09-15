package repomaintenance

import (
	"context"
	"sync"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/model"
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
// It is a coordination primitive between this daemon's own passes, and it
// is not a distributed lock. Two backupd instances sharing a state
// directory are kept to one owner per repository by the ownership
// record's atomic create and compare-and-set (run.go, and the store in
// internal/backupengine); two instances that do NOT share a state
// directory are not coordinated at all, and what makes that
// non-destructive rather than dangerous is the engine's own safety
// parameters. Both are spelled out in the package doc and ADR 0017; this
// file promises nothing about either.
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
// fence_internal_test.go is where that ordering is pinned.

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

	// runs is the maintenance lease, and it is a SEPARATE map from gates
	// on purpose. The gates answer "may a destructive operation and a
	// full maintenance be inside this repository together" and the answer
	// for two deletes is yes; runs answers "may two maintenance WINDOWS
	// be under way for this repository", where the answer is never yes
	// and where a quick window has to exclude another quick one -- which
	// the shared side of a gate, correctly, does not.
	runs map[model.RepositoryDomainID]*lease
}

// NewFence returns a fence with nothing held.
func NewFence() *Fence {
	return &Fence{
		gates: map[model.RepositoryDomainID]*gate{},
		runs:  map[model.RepositoryDomainID]*lease{},
	}
}

// lease is one repository's maintenance lease: a single token, held by
// whoever is running a maintenance window against that repository.
//
// waiting is guarded by the Fence's mutex and is what lets an idle lease
// be forgotten without losing a waiter that has not yet taken the token.
type lease struct {
	token   chan struct{}
	waiting int
}

// SerialiseMaintenance reserves the right to run a maintenance window
// against one repository, and returns the release.
//
// It is what makes "decide whether maintenance is due, then do it, then
// record it" one operation rather than three. Two scheduled passes that
// both decide a full maintenance is due would otherwise both be right,
// both queue for the exclusive side of the fence, and run full
// maintenance twice in a row -- the second time against a repository that
// had just had one -- with each writing its own outcome over the other's
// history and counters. The exclusive gate cannot prevent that: it
// serialises them, which is exactly what makes them both run.
//
// It is in-process, and unlike the ownership record it does not pretend
// otherwise: what stops a SECOND INSTANCE writing over this window's
// outcome is the record's revision (see run.go), not this lease.
func (f *Fence) SerialiseMaintenance(ctx context.Context, domain model.RepositoryDomainID) (func(), error) {
	f.mu.Lock()

	l := f.runs[domain]
	if l == nil {
		l = &lease{token: make(chan struct{}, 1)}
		f.runs[domain] = l
	}

	l.waiting++
	f.mu.Unlock()

	// An uncontended lease is granted without consulting ctx, for
	// acquire's reason: a caller that would not have waited is better
	// served by being let through to fail on its own terms than by an
	// error about a queue it was never in. It also keeps a cancelled
	// window's outcome recordable, which is what makes a failure
	// distinguishable from a window that never ran.
	select {
	case l.token <- struct{}{}:
		return f.releaseLease(domain, l), nil
	default:
	}

	select {
	case l.token <- struct{}{}:
		return f.releaseLease(domain, l), nil

	case <-ctx.Done():
		f.mu.Lock()
		l.waiting--
		f.forgetLeaseIfIdle(domain, l)
		f.mu.Unlock()

		return nil, ctx.Err()
	}
}

// releaseLease hands the token back, once however many times it is
// called: a window that both defers its release and returns it early is
// the ordinary shape, and a second release that freed somebody else's
// lease would let two windows run.
func (f *Fence) releaseLease(domain model.RepositoryDomainID, l *lease) func() {
	var once sync.Once

	return func() {
		once.Do(func() {
			<-l.token

			f.mu.Lock()
			l.waiting--
			f.forgetLeaseIfIdle(domain, l)
			f.mu.Unlock()
		})
	}
}

// forgetLeaseIfIdle drops a lease nobody holds or wants, for the reason
// forgetIfIdle gives. Must be called with the lock held.
func (f *Fence) forgetLeaseIfIdle(domain model.RepositoryDomainID, l *lease) {
	if l.waiting == 0 && len(l.token) == 0 {
		delete(f.runs, domain)
	}
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
