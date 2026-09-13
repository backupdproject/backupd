package app

import (
	"context"
	"time"

	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/model"
)

// Issue #845: how a single sequential loop serves backup sets that are
// polled at different cadences.
//
// The shape everybody reaches for first is a timer per backup set. It is
// the one shape this engine may not take: "no two passes over a backup
// set overlap" is true here by construction, because there is exactly one
// loop and it never starts the next pass until the last one returned (see
// daemon.go and cycle.go). A second thing that can start a pass ends that
// proof, and replaces it with a lock somebody has to remember to take.
//
// So the loop stays single and sequential, and the cadence moves inward:
// the loop wakes at the tightest interval anything is entitled to
// (config.PollWakeInterval), and each wake asks, per set, whether ENOUGH
// TIME HAS PASSED since that set was last attempted
// (config.EffectivePollInterval). A set that is not due is skipped for
// this wake; nothing else about the pass changes. A wake where no set is
// due does nothing at all, which is what makes waking often cheap.
//
// # Attempted, not succeeded
//
// The timestamp this schedule turns on is when a pass over the set last
// STARTED, deliberately not Service.lastPoll ("when discovery last
// SUCCEEDED", health.go). A source that is down fails every pass, and a
// schedule keyed on success would treat that as "never polled" and retry
// it on every single wake -- fastest retries against the host least able
// to answer, which is the behaviour a poll interval exists to prevent.
//
// # Only a scheduled cycle filters
//
// A cycle an operator asked for is not filtered at all (see
// WithScheduledCycle). The schedule is this product's guess about when to
// look; a person pressing Run is not a guess.

// scheduledCycleKey marks a cycle as a scheduled tick on its context,
// following the same idiom WithOperation and WithBackupSetHolds use: the
// fact belongs to one cycle rather than to the Service, and the Service
// outlives cycles and serves manual ones between scheduled ones.
type scheduledCycleKey struct{}

// WithScheduledCycle marks ctx as belonging to a SCHEDULED cycle: one the
// poll loop started on its own, rather than one an operator submitted.
//
// RunCycle processes only the backup sets that are due on their own
// effective poll interval when this is set, and every enabled set when it
// is not. The default is therefore the operator's: a caller that forgets
// this gets a full pass, which is exactly today's behaviour and never a
// silently skipped backup.
//
// Both scheduling loops set it -- Service.Daemon here, and
// core/service's RunOnSchedule for the Web host -- and nothing else does.
func WithScheduledCycle(ctx context.Context) context.Context {
	return context.WithValue(ctx, scheduledCycleKey{}, true)
}

// IsScheduledCycle reports whether this cycle is a scheduled tick rather
// than one an operator submitted.
//
// Exported because the two scheduling loops live in different packages
// (this one's Daemon and core/service's RunOnSchedule) and the fact has
// to be assertable from outside: a loop that forgot WithScheduledCycle
// would poll every set on every wake, which is a bug no test inside this
// package can see.
func IsScheduledCycle(ctx context.Context) bool {
	v, _ := ctx.Value(scheduledCycleKey{}).(bool)
	return v
}

// pollDue reports whether a scheduled cycle should process bs now: never
// polled by this schedule, or at least this set's effective poll interval
// has passed since the last attempt.
//
// A set with no recorded attempt is due, which is what makes a freshly
// started process poll everything once immediately -- FR-1's "runs
// RunCycle once immediately, then again every interval" applied per set
// rather than to the loop as a whole.
func (s *Service) pollDue(bs config.BackupSet, now time.Time) bool {
	s.mu.Lock()
	last, ok := s.lastPollAttempt[bs.ID]
	s.mu.Unlock()
	if !ok {
		return true
	}
	return now.Sub(last) >= s.Config.EffectivePollInterval(bs)
}

// recordPollAttempt notes that a pass over this set is starting now.
//
// It is recorded for MANUAL passes too, and that is deliberate: the
// question this timestamp answers is "when was this source last looked
// at", and a manual run looked at it. The alternative -- recording only
// scheduled attempts -- would send the loop back at a source an operator
// polled by hand seconds ago, which is the one situation where a poll is
// certainly redundant.
func (s *Service) recordPollAttempt(set model.BackupSetID, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastPollAttempt == nil {
		s.lastPollAttempt = make(map[model.BackupSetID]time.Time)
	}
	s.lastPollAttempt[set] = now
}

// AdoptPollSchedule carries the poll schedule from the Service a
// configuration reload is replacing onto this one, the way AdoptAlerts
// carries alerting state (core/service's adoptConfig calls both).
//
// Without it, every settings save would restart the schedule: a new
// Service has attempted nothing, every set reads as never polled, and the
// next wake polls every source in the deployment. Saving an unrelated
// setting is not a reason to go and knock on every backup source, and a
// deployment whose operator is editing configuration is exactly when that
// would happen most.
//
// The schedule stays in memory and nothing writes it down, so a process
// restart does poll everything once. That is the same promise Daemon has
// always made (one cycle immediately at start) and the safe direction:
// the failure mode is one extra poll, not a source left unvisited.
func (s *Service) AdoptPollSchedule(prev *Service) {
	if prev == nil {
		return
	}
	prev.mu.Lock()
	carried := make(map[model.BackupSetID]time.Time, len(prev.lastPollAttempt))
	for id, at := range prev.lastPollAttempt {
		carried[id] = at
	}
	prev.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPollAttempt = carried
}
