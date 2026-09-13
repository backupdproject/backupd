package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/state"
)

// Issue #845's due-filter, proven on a clock the test moves itself.
//
// The property is not "a cycle happened" but "this set's pass happened on
// its own cadence and that one's did not", which is only observable per
// set. perSetJournal counts the ListByBackupSet call every pass makes, so
// a set the cycle skipped is a count that did not move.
//
// Everything here runs through the real RunCycle rather than a
// due-calculation helper, because the whole risk of this feature is a
// filter that is correct in isolation and wired into the wrong loop.

// perSetJournal counts ListByBackupSet calls per backup set, which is how
// these tests tell "this set was processed this cycle" from "this set was
// skipped": every pass over a set makes at least one such call, and a
// skipped set makes none.
//
// It also carries slowJournal's in-flight high-water mark (daemon_test.go)
// so a test driving the real loop can assert the no-overlap invariant at
// the same time as the cadence, which is the pair this feature could
// plausibly break together.
type perSetJournal struct {
	*state.Journal
	delay time.Duration

	// clock, when non-nil, is the same clock the Service under test
	// reads, so a pass can be recorded at the instant the schedule
	// thinks it happened rather than at wall-clock now. A test asserting
	// WHEN a set was polled needs those to be the same instant.
	clock func() time.Time

	// onPass, when non-nil, runs each time a pass over a set is recorded
	// at a new instant. It is how a test makes time pass INSIDE a cycle,
	// which is the only way to reach the case where one set's pass
	// carries a later set over its own due boundary.
	onPass func(model.BackupSetID)

	inFlight    int32
	maxInFlight int32

	mu     sync.Mutex
	counts map[model.BackupSetID]int
	at     map[model.BackupSetID][]time.Time
}

func (j *perSetJournal) ListByBackupSet(ctx context.Context, set model.BackupSetID) ([]state.Record, error) {
	cur := atomic.AddInt32(&j.inFlight, 1)
	for {
		max := atomic.LoadInt32(&j.maxInFlight)
		if cur <= max || atomic.CompareAndSwapInt32(&j.maxInFlight, max, cur) {
			break
		}
	}
	defer atomic.AddInt32(&j.inFlight, -1)

	j.mu.Lock()
	if j.counts == nil {
		j.counts = make(map[model.BackupSetID]int)
	}
	j.counts[set]++
	fresh := false
	if j.clock != nil {
		// One pass makes several journal calls, and on a clock the test
		// moves they all read the same instant, so an instant already
		// recorded for this set is the same pass rather than a new one.
		now := j.clock()
		seen := j.at[set]
		if len(seen) == 0 || !seen[len(seen)-1].Equal(now) {
			if j.at == nil {
				j.at = make(map[model.BackupSetID][]time.Time)
			}
			j.at[set] = append(j.at[set], now)
			fresh = true
		}
	}
	onPass := j.onPass
	j.mu.Unlock()
	if fresh && onPass != nil {
		onPass(set)
	}
	if j.delay > 0 {
		time.Sleep(j.delay)
	}
	return j.Journal.ListByBackupSet(ctx, set)
}

// polled reports whether this set was processed since the last reset.
func (j *perSetJournal) polled(set model.BackupSetID) bool {
	return j.passes(set) > 0
}

// passes is how many ListByBackupSet calls this set has seen since the
// last reset, which is a lower bound on how many passes it has had.
func (j *perSetJournal) passes(set model.BackupSetID) int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.counts[set]
}

// polledAt is every instant this set was passed over, on the clock the
// test moves, which is what an assertion about CADENCE (as opposed to
// "was it polled at all") has to read.
func (j *perSetJournal) polledAt(set model.BackupSetID) []time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]time.Time(nil), j.at[set]...)
}

func (j *perSetJournal) reset() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.counts = nil
	j.at = nil
}

var _ Journal = (*perSetJournal)(nil)

// TestScheduledCycle_PollsEachSetOnItsOwnInterval is the heart of #845:
// one deployment, two sets, two cadences, one sequential loop. The set
// with the 5m override must be reached on every 5m tick, and the set
// inheriting the 15m default must not be reached on the ticks in
// between.
func TestScheduledCycle_PollsEachSetOnItsOwnInterval(t *testing.T) {
	fast := testBackupSet(t, t.TempDir())
	fast.Name = "fast"
	fast.ID = mustSetID(t, "production", "fast")
	five := config.Duration(5 * time.Minute)
	fast.PollInterval = &five

	slow := testBackupSet(t, t.TempDir())
	slow.Name = "slow"
	slow.ID = mustSetID(t, "production", "slow")

	cfg := testConfig(t, testSource("production", fast, slow))
	cfg.PollInterval = config.Duration(15 * time.Minute)

	journal := &perSetJournal{Journal: openJournal(t)}
	svc := New(cfg, journal, newFakeTransport(), nil)

	now := epoch
	svc.Now = func() time.Time { return now }

	ctx := WithScheduledCycle(context.Background())

	// The first scheduled pass polls everything: a set this process has
	// never polled is due, whatever its interval.
	svc.RunCycle(ctx)
	if !journal.polled(fast.ID) || !journal.polled(slow.ID) {
		t.Fatalf("first cycle: fast polled=%v slow polled=%v, want both", journal.polled(fast.ID), journal.polled(slow.ID))
	}

	// Wake at the 5m base granularity. The overriding set is due; the
	// inheriting one is 5 minutes into a 15 minute interval.
	for _, elapsed := range []time.Duration{5 * time.Minute, 10 * time.Minute} {
		journal.reset()
		now = epoch.Add(elapsed)
		svc.RunCycle(ctx)
		if !journal.polled(fast.ID) {
			t.Errorf("at +%s the 5m set was not polled", elapsed)
		}
		if journal.polled(slow.ID) {
			t.Errorf("at +%s the 15m set was polled early", elapsed)
		}
	}

	// At 15 minutes both are due again.
	journal.reset()
	now = epoch.Add(15 * time.Minute)
	svc.RunCycle(ctx)
	if !journal.polled(fast.ID) || !journal.polled(slow.ID) {
		t.Errorf("at +15m: fast polled=%v slow polled=%v, want both", journal.polled(fast.ID), journal.polled(slow.ID))
	}

	// A wake with nothing due processes nothing at all, rather than
	// falling back to "poll everything".
	journal.reset()
	now = epoch.Add(16 * time.Minute)
	svc.RunCycle(ctx)
	if journal.polled(fast.ID) || journal.polled(slow.ID) {
		t.Errorf("at +16m nothing was due but fast polled=%v slow polled=%v", journal.polled(fast.ID), journal.polled(slow.ID))
	}
}

// TestManualCycle_IgnoresTheDueFilter is the operator's half of the same
// rule: a run somebody asked for runs now. A cycle that is not a
// scheduled tick carries no due-filter at all, so every enabled set is
// processed however recently the schedule last reached it.
func TestManualCycle_IgnoresTheDueFilter(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())
	day := config.Duration(24 * time.Hour)
	bs.PollInterval = &day

	cfg := testConfig(t, testSource("production", bs))
	cfg.PollInterval = config.Duration(15 * time.Minute)

	journal := &perSetJournal{Journal: openJournal(t)}
	svc := New(cfg, journal, newFakeTransport(), nil)
	now := epoch
	svc.Now = func() time.Time { return now }

	svc.RunCycle(WithScheduledCycle(context.Background()))
	if !journal.polled(bs.ID) {
		t.Fatal("the first scheduled cycle did not poll the set")
	}

	// One second later the set is nowhere near due on its own 24h
	// cadence, and an operator presses Run.
	journal.reset()
	now = epoch.Add(time.Second)
	svc.RunCycle(context.Background())
	if !journal.polled(bs.ID) {
		t.Error("a manual RunCycle honoured the poll schedule; an operator-submitted run must run now")
	}

	// And a manual fetch, the other operator-driven entry point, is
	// equally unaffected: it never consults the schedule and still
	// reaches the source.
	journal.reset()
	if _, err := svc.Fetch(context.Background(), "production", bs.Name, false); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !journal.polled(bs.ID) {
		t.Error("a manual fetch did not reach the set")
	}
}

// TestScheduledCycle_DueFilterSurvivesAConfigReload pins the one thing
// that makes this schedule worth keeping in memory at all: core/service
// builds a whole new app.Service on every settings save, and a schedule
// that reset there would poll every source again on every configuration
// edit.
func TestScheduledCycle_DueFilterSurvivesAConfigReload(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())
	cfg := testConfig(t, testSource("production", bs))
	cfg.PollInterval = config.Duration(15 * time.Minute)

	journal := &perSetJournal{Journal: openJournal(t)}
	svc := New(cfg, journal, newFakeTransport(), nil)
	now := epoch
	svc.Now = func() time.Time { return now }

	ctx := WithScheduledCycle(context.Background())
	svc.RunCycle(ctx)

	reloaded := New(cfg, journal, newFakeTransport(), nil)
	reloaded.Now = func() time.Time { return now }
	reloaded.AdoptPollSchedule(svc)

	journal.reset()
	now = epoch.Add(time.Minute)
	reloaded.RunCycle(ctx)
	if journal.polled(bs.ID) {
		t.Error("a set polled one minute ago was polled again by the service a config reload built; the schedule did not carry across")
	}
}

// TestAdoptPollSchedule_SharesTheScheduleRatherThanCopyingIt is the
// window a copy leaves open.
//
// A configuration reload does not stop the cycle loop: core/service
// builds the replacement Service while the one it replaces may be
// halfway through a pass, recording attempts. A reload that COPIED the
// schedule would drop every attempt recorded after the copy -- and those
// are the sets a long cycle is still working through, so the losses land
// on the slowest sources, which are the ones least worth polling again
// immediately. Sharing the schedule closes the window instead of
// narrowing it.
func TestAdoptPollSchedule_SharesTheScheduleRatherThanCopyingIt(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())
	cfg := testConfig(t, testSource("production", bs))
	cfg.PollInterval = config.Duration(15 * time.Minute)

	journal := &perSetJournal{Journal: openJournal(t)}
	svc := New(cfg, journal, newFakeTransport(), nil)
	now := epoch
	svc.Now = func() time.Time { return now }

	ctx := WithScheduledCycle(context.Background())
	svc.RunCycle(ctx)

	reloaded := New(cfg, journal, newFakeTransport(), nil)
	reloaded.Now = func() time.Time { return now }
	reloaded.AdoptPollSchedule(svc)

	// The cycle that was already running when the reload happened
	// finishes its pass over the set, fifteen minutes in, and records it
	// against the Service it started on.
	now = epoch.Add(15 * time.Minute)
	svc.RunCycle(ctx)

	// A minute later the replacement Service is the one the loop is
	// driving, and that set was polled sixty seconds ago.
	journal.reset()
	now = epoch.Add(16 * time.Minute)
	reloaded.RunCycle(ctx)
	if journal.polled(bs.ID) {
		t.Error("a set the OLD service polled a minute ago was polled again by the one that replaced it; the reload took a copy of the schedule and lost every attempt recorded after it")
	}
}

// TestDaemon_WakesAtTheTightestConfiguredCadence proves the base
// granularity is derived rather than taken from the deployment default:
// a set overriding the interval downwards has to be reached on its own
// cadence, and the only loop allowed to reach it is this one.
//
// It also re-asserts the no-overlap invariant under two cadences, since
// that invariant is the reason this feature is a due-filter inside one
// sequential loop instead of a timer per set.
func TestDaemon_WakesAtTheTightestConfiguredCadence(t *testing.T) {
	fast := testBackupSet(t, t.TempDir())
	fast.Name = "fast"
	fast.ID = mustSetID(t, "production", "fast")
	tight := config.Duration(10 * time.Millisecond)
	fast.PollInterval = &tight

	cfg := testConfig(t, testSource("production", fast))
	// A deployment default far longer than the test's own lifetime: if
	// the loop slept this, the override could never be honoured.
	cfg.PollInterval = config.Duration(time.Hour)

	journal := &perSetJournal{Journal: openJournal(t), delay: 5 * time.Millisecond}

	svc := New(cfg, journal, newFakeTransport(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	if err := svc.Daemon(ctx); err != nil {
		t.Fatalf("Daemon: %v", err)
	}

	if got := journal.passes(fast.ID); got < 3 {
		t.Errorf("the 10ms-override set saw %d journal pass(es) in 150ms; the loop slept the deployment default instead of the tightest configured cadence", got)
	}
	if max := atomic.LoadInt32(&journal.maxInFlight); max > 1 {
		t.Errorf("max concurrent ListByBackupSet calls = %d, want 1: cycles overlapped", max)
	}
}

// virtualClock is the daemon loop's own clock, moved by the loop itself:
// every sleep it asks for is granted at once, by jumping the clock
// forward exactly that far.
//
// It exists because the property TestDaemon_WakesAtEachSetsOwnDeadline
// is about is measured in minutes. A loop that slept real 5m and 7m
// intervals could not be tested at all, and one tested at milliseconds
// would be asserting on the scheduler of the machine running the suite
// rather than on this package's arithmetic.
type virtualClock struct {
	mu    sync.Mutex
	now   time.Time
	wakes int

	// stopAfter is how many wakes the loop is granted before ctx is
	// cancelled, since a loop whose sleeps cost nothing would otherwise
	// never stop.
	stopAfter int
	stop      context.CancelFunc
}

func (c *virtualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// advance moves the clock on without the loop asking, which is how a test
// makes time pass inside a cycle.
func (c *virtualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// timer is the Service.newTimer seam: the loop's sleep, granted instantly
// at the far end of the interval it asked for.
func (c *virtualClock) timer(d time.Duration) (<-chan time.Time, func() bool) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.wakes++
	done := c.wakes >= c.stopAfter
	fired := c.now
	c.mu.Unlock()

	if done && c.stop != nil {
		c.stop()
	}
	ch := make(chan time.Time, 1)
	ch <- fired
	return ch, func() bool { return true }
}

// TestDaemon_WakesAtEachSetsOwnDeadline is the cadence half of #845 that
// a fixed minimum sleep gets wrong.
//
// Waking at the tightest configured interval and asking "is anything
// due" is not the same schedule as waking when something IS due: under a
// 5m wake a 7m set is only ever reached on a 10m boundary, so an
// operator who asked for 7 minutes silently got 10. The loop therefore
// sleeps until the EARLIEST deadline across the enabled sets, recomputed
// every pass, and this pins the timeline that produces.
func TestDaemon_WakesAtEachSetsOwnDeadline(t *testing.T) {
	five := config.Duration(5 * time.Minute)
	seven := config.Duration(7 * time.Minute)

	fast := testBackupSet(t, t.TempDir())
	fast.Name = "five"
	fast.ID = mustSetID(t, "production", "five")
	fast.PollInterval = &five

	slow := testBackupSet(t, t.TempDir())
	slow.Name = "seven"
	slow.ID = mustSetID(t, "production", "seven")
	slow.PollInterval = &seven

	cfg := testConfig(t, testSource("production", fast, slow))
	cfg.PollInterval = config.Duration(time.Hour)

	clock := &virtualClock{now: epoch, stopAfter: 8}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock.stop = cancel

	journal := &perSetJournal{Journal: openJournal(t), clock: clock.Now}
	svc := New(cfg, journal, newFakeTransport(), nil)
	svc.Now = clock.Now
	svc.newTimer = clock.timer

	if err := svc.Daemon(ctx); err != nil {
		t.Fatalf("Daemon: %v", err)
	}

	// Both sets are polled on the first pass (neither has ever been
	// attempted), and from then on each one is reached on its own
	// deadline: 0,5,10,15,20 and 0,7,14,21.
	assertPolledAt(t, journal, fast.ID, "five-minute", epoch, 0, 5, 10, 15, 20)
	assertPolledAt(t, journal, slow.ID, "seven-minute", epoch, 0, 7, 14, 21)
}

// assertPolledAt checks that this set's first passes landed exactly on
// the given minute offsets from base. It compares a PREFIX, because the
// loop is stopped by cancelling its context and may get one more pass in
// before it notices; what the offsets pin is the cadence, not how many
// passes a cancelled loop managed.
func assertPolledAt(t *testing.T, j *perSetJournal, set model.BackupSetID, label string, base time.Time, minutes ...int) {
	t.Helper()
	got := j.polledAt(set)
	if len(got) < len(minutes) {
		t.Fatalf("the %s set was polled %d time(s) (%v), want at least %d: %v", label, len(got), offsetsFrom(base, got), len(minutes), minutes)
	}
	for i, m := range minutes {
		want := base.Add(time.Duration(m) * time.Minute)
		if !got[i].Equal(want) {
			t.Fatalf("the %s set was polled at minutes %v, want it to start %v: a set is being reached on another set's cadence rather than its own", label, offsetsFrom(base, got), minutes)
		}
	}
}

func offsetsFrom(base time.Time, times []time.Time) []float64 {
	out := make([]float64, 0, len(times))
	for _, at := range times {
		out = append(out, at.Sub(base).Minutes())
	}
	return out
}

// TestScheduledCycle_AFailingSourceIsRetriedOnItsIntervalNotEveryWake is
// the ATTEMPTED-not-SUCCEEDED half of this schedule, and the half a
// timing test over healthy sources cannot see.
//
// A schedule keyed on the last SUCCESSFUL poll reads a source that is
// down as "never polled", which is due on every single wake: the fastest
// retries this product can produce, aimed at the host least able to
// answer them. So the timestamp is written before the pass runs and
// whatever the pass then finds, and this holds a failing set to exactly
// the cadence a healthy one gets.
func TestScheduledCycle_AFailingSourceIsRetriedOnItsIntervalNotEveryWake(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())
	cfg := testConfig(t, testSource("production", bs))
	cfg.PollInterval = config.Duration(15 * time.Minute)

	tr := newFakeTransport()
	tr.failForSourceID = bs.ID.String()
	tr.failErr = errors.New("source is unreachable")

	svc := New(cfg, openJournal(t), tr, nil)
	now := epoch
	svc.Now = func() time.Time { return now }

	ctx := WithScheduledCycle(context.Background())

	report := svc.RunCycle(ctx)
	if len(report.Sets) != 1 {
		t.Fatalf("the first scheduled cycle processed %d set(s), want 1", len(report.Sets))
	}
	if report.Sets[0].Err == nil {
		t.Fatal("the fixture's source was supposed to fail discovery, and did not; this test would prove nothing")
	}

	// One minute later, with the source still down. A schedule keyed on
	// success would treat this set as never polled and try again now.
	now = epoch.Add(time.Minute)
	if report := svc.RunCycle(ctx); len(report.Sets) != 0 {
		t.Errorf("a set whose poll FAILED a minute ago was retried at the next wake; the schedule is keyed on the last success, so a source that is down is hammered every wake")
	}

	// And at its interval it is retried, so a failing source is not
	// dropped from the schedule either.
	now = epoch.Add(15 * time.Minute)
	if report := svc.RunCycle(ctx); len(report.Sets) != 1 {
		t.Errorf("at +15m the failing set was processed %d time(s), want 1: a failed attempt must not remove a set from the schedule", len(report.Sets))
	}
}

// TestScheduledCycle_FreezesDueAtTheCycleStartInstant is the agreement
// between the two places this cycle decides what it is going to do.
//
// The denominator live progress reports is counted before the loop
// starts, and the per-set filter used to be re-evaluated as the loop
// reached each set -- against a clock that had moved, by exactly as long
// as the earlier sets' passes took. A pass long enough to carry a later
// set over its own deadline therefore processed a set the denominator
// had excluded ("2 of 1 backup sets"), and which sets a wake visited
// depended on the order they happen to sit in the configuration file.
// Both questions are now answered against the cycle's start instant.
func TestScheduledCycle_FreezesDueAtTheCycleStartInstant(t *testing.T) {
	five := config.Duration(5 * time.Minute)
	first := testBackupSet(t, t.TempDir())
	first.Name = "first"
	first.ID = mustSetID(t, "production", "first")
	first.PollInterval = &five

	second := testBackupSet(t, t.TempDir())
	second.Name = "second"
	second.ID = mustSetID(t, "production", "second")

	cfg := testConfig(t, testSource("production", first, second))
	cfg.PollInterval = config.Duration(15 * time.Minute)

	clock := &virtualClock{now: epoch}
	journal := &perSetJournal{Journal: openJournal(t), clock: clock.Now}
	svc := New(cfg, journal, newFakeTransport(), nil)
	svc.Now = clock.Now

	ctx := WithScheduledCycle(context.Background())
	svc.RunCycle(ctx)

	// +5m: the overriding set is due and the inheriting one is five
	// minutes into a fifteen minute interval. The first set's pass then
	// takes ten minutes of this test's time, which carries the wake past
	// the second set's deadline while the cycle is still running.
	clock.advance(5 * time.Minute)
	var once sync.Once
	journal.onPass = func(set model.BackupSetID) {
		if set == first.ID {
			once.Do(func() { clock.advance(10 * time.Minute) })
		}
	}

	obs := &recordingProgressObserver{}
	report := svc.RunCycle(WithProgressObserver(ctx, obs))

	if got, want := len(report.Sets), 1; got != want {
		t.Errorf("the cycle processed %d set(s), want %d: a set that was not due when the cycle started was drawn in by how long an earlier set's pass took", got, want)
	}
	if got, want := obs.total(), len(report.Sets); got != want {
		t.Errorf("live progress reported %d backup set(s) in this cycle and %d were processed; the denominator and the filter answered the same question at two different instants", got, want)
	}
}

// recordingProgressObserver keeps the denominator the cycle published, so
// a test can compare what the cycle SAID it would do with what it did.
type recordingProgressObserver struct {
	mu        sync.Mutex
	setsTotal int
}

func (o *recordingProgressObserver) ObserveProgress(p Progress) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.setsTotal = p.BackupSetsTotal
}

func (o *recordingProgressObserver) total() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.setsTotal
}

// TestNextPollWake_BoundsTheSleep covers the two answers that are not a
// deadline at all.
//
// A DISABLED set is never polled (RunCycle skips it outright), so a
// deployment whose only fast set is disabled must not be woken on its
// behalf: that is a process waking up for work nobody does. And a
// deadline already in the past has to become a short sleep rather than
// no sleep: every set a cycle visits records an attempt and moves its
// own deadline on, so a deadline still in the past belongs to a set the
// cycle SKIPPED -- held for editing, most likely -- and a zero would
// turn that into a spin for as long as the hold lasts.
func TestNextPollWake_BoundsTheSleep(t *testing.T) {
	bs := testBackupSet(t, t.TempDir())
	fast := config.Duration(time.Minute)
	bs.PollInterval = &fast
	bs.Disabled = true

	cfg := testConfig(t, testSource("production", bs))
	cfg.PollInterval = config.Duration(15 * time.Minute)

	svc := New(cfg, openJournal(t), newFakeTransport(), nil)
	svc.Now = func() time.Time { return epoch }

	if got, want := svc.NextPollWake(epoch), 15*time.Minute; got != want {
		t.Errorf("NextPollWake with only a disabled set = %s, want the deployment's %s: the loop wakes on behalf of a set it will never poll", got, want)
	}

	// Enabled, never attempted: due now, and answered with the floor
	// rather than with zero.
	cfg.Sources[0].BackupSets[0].Disabled = false
	if got := svc.NextPollWake(epoch); got <= 0 {
		t.Errorf("NextPollWake for an overdue set = %s, want a positive floor: a zero sleep spins the loop", got)
	}
	if got, want := svc.NextPollWake(epoch), time.Minute; got > want {
		t.Errorf("NextPollWake for an overdue set = %s, want no more than %s", got, want)
	}
}
