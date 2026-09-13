package app

import (
	"context"
	"sync"
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
// the loop sleeps until the earliest moment any enabled set is due
// (NextPollWake below), and each wake asks, per set, whether ENOUGH TIME
// HAS PASSED since that set was last attempted
// (config.EffectivePollInterval). A set that is not due is skipped for
// this wake; nothing else about the pass changes. A wake where no set is
// due does nothing at all, which is what makes waking cheap.
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

// pollSchedule is when each backup set was last attempted, and the one
// piece of this package's state that OUTLIVES the Service holding it.
//
// It is a struct with its own lock, shared by pointer, rather than a map
// a reload copies. core/service rebuilds the whole app.Service on every
// configuration write (configreload.go) while the scheduling loop may be
// inside a cycle recording attempts, so a copy has two failure modes at
// once: attempts recorded after the copy are lost, and the loop and the
// copier disagree about which map is the live one. One object, pointed at
// by both Services, has neither -- the new Service adopts the same
// schedule the old one is still writing, and every write is under the
// same mutex.
type pollSchedule struct {
	mu       sync.Mutex
	attempts map[model.BackupSetID]time.Time
}

func newPollSchedule() *pollSchedule {
	return &pollSchedule{attempts: make(map[model.BackupSetID]time.Time)}
}

// lastAttempt is when a pass over set last started, and whether one ever
// has.
func (p *pollSchedule) lastAttempt(set model.BackupSetID) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	at, ok := p.attempts[set]
	return at, ok
}

func (p *pollSchedule) record(set model.BackupSetID, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attempts == nil {
		p.attempts = make(map[model.BackupSetID]time.Time)
	}
	p.attempts[set] = now
}

// pollScheduleState is this Service's schedule, created on first use.
//
// New fills the field in, so the lazy path is for a Service built as a
// struct literal, which every test double in this package is. It is
// guarded by s.mu because AdoptPollSchedule writes the same pointer from
// whichever goroutine is reloading the configuration.
func (s *Service) pollScheduleState() *pollSchedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schedule == nil {
		s.schedule = newPollSchedule()
	}
	return s.schedule
}

// pollDue reports whether a scheduled cycle should process bs at the
// instant now: never polled by this schedule, or at least this set's
// effective poll interval has passed since the last attempt.
//
// A set with no recorded attempt is due, which is what makes a freshly
// started process poll everything once immediately -- FR-1's "runs
// RunCycle once immediately, then again every interval" applied per set
// rather than to the loop as a whole.
//
// now is passed in rather than read here, and that is the whole reason
// this takes an argument: one cycle answers this question twice (for its
// progress denominator and for the loop itself) and both answers have to
// come from the same instant, or a long pass over an early set quietly
// draws a later one into a wake it was not due for. See RunCycle.
func (s *Service) pollDue(bs config.BackupSet, now time.Time) bool {
	last, ok := s.pollScheduleState().lastAttempt(bs.ID)
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
//
// It is written before the pass runs and whatever the pass finds, which
// is what keeps a source that is DOWN on its own cadence: a schedule
// keyed on success would read a failing set as never polled and retry it
// on every wake, aiming this product's fastest retries at the host least
// able to answer them.
func (s *Service) recordPollAttempt(set model.BackupSetID, now time.Time) {
	s.pollScheduleState().record(set, now)
}

// NextPollWake is how long a scheduling loop should sleep before its next
// scheduled cycle: until the EARLIEST moment any enabled backup set
// becomes due, measured from when each one was last attempted.
//
// # Why not simply sleep the tightest configured interval
//
// That was the first shape and it aliases. A loop that wakes every 5
// minutes reaches a 7 minute set on the 10 minute boundary, so an
// operator who asked for 7 got 10, and the further two cadences are from
// dividing each other the worse it reads. Deadlines do not alias: each
// set is visited the first time the loop is awake at or after its own
// deadline, and the loop is awake exactly then because that deadline is
// what it slept to.
//
// # The bounds
//
// A deployment with no enabled set sleeps the deployment-wide interval:
// there is no deadline to aim at, and waking at the configured cadence is
// what makes a set that appears in the meantime (a create, a re-enable)
// picked up by a loop that also listens for that change.
//
// A deadline already in the past returns minPollWake rather than zero.
// Every set the cycle actually visits records an attempt and moves its
// own deadline forward, so a past deadline means a set the cycle SKIPPED
// for another reason -- an edit hold, most likely -- and a zero sleep
// would turn that into a spin for as long as the hold is held.
func (s *Service) NextPollWake(now time.Time) time.Duration {
	schedule := s.pollScheduleState()

	var earliest time.Time
	for _, src := range s.Config.Sources {
		for _, bs := range src.BackupSets {
			if bs.Disabled {
				continue
			}
			deadline := now
			if last, ok := schedule.lastAttempt(bs.ID); ok {
				deadline = last.Add(s.Config.EffectivePollInterval(bs))
			}
			if earliest.IsZero() || deadline.Before(earliest) {
				earliest = deadline
			}
		}
	}

	if earliest.IsZero() {
		return s.Config.PollInterval.Duration()
	}
	if wake := earliest.Sub(now); wake > minPollWake {
		return wake
	}
	return minPollWake
}

// minPollWake is the shortest sleep NextPollWake will ask for.
//
// It is not a cadence -- config.MinPollInterval is, and it is a minute.
// This is the floor under a loop that has just been told something is
// already overdue, which only happens for a set the cycle could not
// visit, and it is what keeps "cannot visit it yet" from becoming a busy
// loop.
const minPollWake = 5 * time.Second

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
// The two Services SHARE the schedule rather than one copying the
// other's. A reload does not stop the cycle loop, so between a copy and
// the swap there is a window in which the old Service is still recording
// attempts nothing would carry forward -- and those are precisely the
// sets a long cycle is working through, so the losses would cluster on
// the slowest sources. Sharing removes the window instead of narrowing
// it.
//
// The schedule stays in memory and nothing writes it down, so a process
// restart does poll everything once. That is the same promise Daemon has
// always made (one cycle immediately at start) and the safe direction:
// the failure mode is one extra poll, not a source left unvisited.
func (s *Service) AdoptPollSchedule(prev *Service) {
	if prev == nil {
		return
	}
	shared := prev.pollScheduleState()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schedule = shared
}
