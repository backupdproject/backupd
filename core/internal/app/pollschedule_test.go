package app

import (
	"context"
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

	inFlight    int32
	maxInFlight int32

	mu     sync.Mutex
	counts map[model.BackupSetID]int
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
	j.mu.Unlock()

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

func (j *perSetJournal) reset() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.counts = nil
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
