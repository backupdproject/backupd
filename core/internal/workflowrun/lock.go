package workflowrun

import (
	"errors"
	"fmt"
	"sync"

	"github.com/retnd/retnd/core/internal/model"
)

// The backup-set lock, and the two things #811 says about it that a
// mutex on its own would get wrong.
//
// # It spans all five stages, cleanup included
//
// The lock is taken before the first "before" hook and released after
// the last "after" hook, so "a new run cannot start while the previous
// run's cleanup is still active" needs no second mechanism: the previous
// run still holds it. A lock scoped to the backup would let a new run's
// global-before hook mount something while the old run's global-after
// hook was unmounting it, on the same set, which is the interleaving the
// nesting exists to prevent.
//
// # It is per SET, because global means inherited
//
// A "global" hook directory is configuration inherited by every backup
// set, not a deployment-wide critical section. Two sets backing up at
// once both run their own copy of the global hooks, and serialising them
// would turn one slow global hook into a deployment that backs up one
// set at a time. So there is one lock per set and none across sets.
//
// # Why it refuses rather than queues
//
// SubmitRunCycle takes the same position and states the reason: backing
// up whatever is on disk now is not made more correct by having been
// asked for twice. A queued second run would also mean a cleanup stage
// racing a "before" stage for the same set as soon as the queue drained
// -- which is the thing the lock exists to stop.

// ErrRunInFlight is returned when a backup set already has a workflow run
// in progress, including one whose cleanup is still running.
var ErrRunInFlight = errors.New("workflowrun: this backup set already has a workflow run in progress")

// SetLocks holds at most one run per backup set.
//
// It is in-process, and that is the right scope: one daemon runs one
// deployment's backups, and a second process writing the same journal is
// refused far below this by the journal itself. What survives a restart
// is the durable side -- an interrupted run leaves an obligation
// requiring recovery, and that blocks the set through Engine's refusal
// rather than through a lock file nobody can clean up.
type SetLocks struct {
	mu   sync.Mutex
	held map[model.BackupSetID]string
}

// NewSetLocks returns an empty lock table.
func NewSetLocks() *SetLocks { return &SetLocks{held: map[model.BackupSetID]string{}} }

// Acquire takes the lock for one set on behalf of one run, or refuses
// with ErrRunInFlight naming the run that has it.
//
// The returned function releases it and is safe to call once. A caller
// that loses the race gets a nil release function and must not call it,
// which is why the error is checked first at every call site.
func (l *SetLocks) Acquire(set model.BackupSetID, runID string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.held == nil {
		l.held = map[model.BackupSetID]string{}
	}

	if holder, busy := l.held[set]; busy {
		return nil, fmt.Errorf("%w: %s is running workflow %q, and the lock spans its cleanup as well as its backup, so a second run would interleave one run's unwinding with another's setup",
			ErrRunInFlight, set, holder)
	}

	l.held[set] = runID

	var once sync.Once

	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()

			if l.held[set] == runID {
				delete(l.held, set)
			}
		})
	}, nil
}

// Holder returns the run currently holding one set's lock, if any. It
// exists for the diagnostics that have to explain a refusal, and for the
// tests that assert the lock is still held across the cleanup stages.
func (l *SetLocks) Holder(set model.BackupSetID) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	holder, busy := l.held[set]

	return holder, busy
}
