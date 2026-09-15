// Issue #845 across this boundary: the deployment-wide poll interval as a
// setting an operator can write, the per-set override as a field on a
// backup set, and the scheduler loop actually running at what those two
// say.
//
// The loop cases are the ones worth reading. A cadence that is only
// correct at process start is the failure this feature invites: the
// interval used to be copied out at construction and handed to the loop
// once, so a saved change would have waited for a restart to mean
// anything, which is precisely what "editable in Settings" must not
// deliver.

package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/app"
	"github.com/retnd/retnd/core/internal/config"
)

func newTestServiceWithPollInterval(t *testing.T, d time.Duration, sources ...config.Source) *BackupService {
	t.Helper()
	cfg := testConfig(sources...)
	cfg.PollInterval = config.Duration(d)
	return New(cfg, openTestJournal(t), nil, nil)
}

// TestRunOnSchedule_SleepsToTheTightestSetsOwnDeadline is the scheduler
// half of #845: the loop's cadence is derived from the whole
// configuration, not from the deployment default alone, or a set that
// polls more often than the deployment could never be reached.
//
// It asserts on the SLEEP the loop arms rather than on how many cycles
// ran in a wall-clock window, because the intervals a deployment
// actually runs are minutes and hours (config.MinPollInterval is a
// minute) and a count over a window can only be taken at cadences no
// deployment uses.
func TestRunOnSchedule_SleepsToTheTightestSetsOwnDeadline(t *testing.T) {
	svc, _ := openTestService(t)

	timers := make(chan *stubTimer, 8)
	withStubbedSchedulerTimer(t, timers)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- svc.RunOnSchedule(ctx) }()

	// Nothing overrides anything yet: the one set is due again at the
	// deployment's own fifteen minutes.
	if first := awaitTimer(t, timers); first.d < 14*time.Minute {
		t.Fatalf("the loop's first sleep is %s, want about 15m", first.d)
	}

	if _, err := svc.UpdateBackupSet(context.Background(), "production/postgres-primary", UpdateBackupSetRequest{
		PollInterval: durationPtr(5 * time.Minute),
	}); err != nil {
		t.Fatalf("UpdateBackupSet: %v", err)
	}

	next := awaitTimer(t, timers)
	if next.d > 5*time.Minute {
		t.Errorf("a set asking to be polled every 5m left the loop sleeping %s; it is still waking on the deployment default, so that set is never reached on its own cadence", next.d)
	}

	cancel()
	next.fire()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("RunOnSchedule: %v", err)
		}
	case <-time.After(schedulerTestBudget):
		t.Fatal("RunOnSchedule did not return after its context was cancelled")
	}
}

// TestRunOnSchedule_MarksItsCyclesAsScheduled is what connects this loop
// to the per-set due filter: a tick that did not say it was a scheduled
// one would poll every set on every wake, and the whole per-set cadence
// would silently do nothing.
func TestRunOnSchedule_MarksItsCyclesAsScheduled(t *testing.T) {
	svc := newTestServiceWithPollInterval(t, 20*time.Millisecond)

	scheduled := make(chan bool, 4)
	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		select {
		case scheduled <- app.IsScheduledCycle(ctx):
		default:
		}
		return app.CycleReport{}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := svc.RunOnSchedule(ctx); err != nil {
		t.Fatalf("RunOnSchedule: %v", err)
	}

	select {
	case got := <-scheduled:
		if !got {
			t.Error("a scheduled tick ran an unmarked cycle; every enabled set would be polled on every wake")
		}
	default:
		t.Fatal("the loop ran no cycle at all")
	}
}

// TestRunOnSchedule_RefusesANonPositiveConfiguredInterval keeps the guard
// that used to live on the argument: a zero interval would spin a tight
// loop, and a Service built in memory never went through config.Validate.
func TestRunOnSchedule_RefusesANonPositiveConfiguredInterval(t *testing.T) {
	svc := newTestServiceWithPollInterval(t, 0)
	if err := svc.RunOnSchedule(context.Background()); err == nil {
		t.Fatal("RunOnSchedule with a zero poll_interval = nil error, want a non-nil error")
	}
}

// TestSettings_ReportsAndWritesTheGlobalPollInterval is the Settings-page
// contract: the value is readable, writable, persisted in the operator's
// own file, and in effect on this process without a restart.
func TestSettings_ReportsAndWritesTheGlobalPollInterval(t *testing.T) {
	svc, configPath := openTestService(t)

	got, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if want := 15 * time.Minute; got.Service.PollInterval != want {
		t.Fatalf("Settings().Service.PollInterval = %s, want %s", got.Service.PollInterval, want)
	}

	updated, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Service: &ServiceUpdate{PollInterval: durationPtr(45 * time.Minute)},
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if want := 45 * time.Minute; updated.Service.PollInterval != want {
		t.Errorf("UpdateSettings().Service.PollInterval = %s, want %s", updated.Service.PollInterval, want)
	}

	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(written), "poll_interval: 45m") {
		t.Errorf("config file does not carry the new interval:\n%s", written)
	}

	// In effect on this process, not only on disk: the scheduler reads
	// its cadence from the running configuration.
	if got := svc.PollInterval(); got != 45*time.Minute {
		t.Errorf("PollInterval() = %s after the save, want 45m0s: the running process kept its start-up copy", got)
	}
}

// TestUpdateSettings_RefusesAPollIntervalUnderTheFloor is the "invalid or
// too small is refused with a clear message" half of #845's acceptance.
func TestUpdateSettings_RefusesAPollIntervalUnderTheFloor(t *testing.T) {
	svc, configPath := openTestService(t)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	for _, d := range []time.Duration{0, -time.Minute, 30 * time.Second} {
		_, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
			Service: &ServiceUpdate{PollInterval: durationPtr(d)},
		})
		if err == nil {
			t.Fatalf("a poll_interval of %s was accepted", d)
		}
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("poll_interval %s: error = %v, want ErrInvalidRequest", d, err)
		}
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(after) != string(before) {
		t.Error("a refused poll_interval still rewrote the operator's config file")
	}
}

// TestUpdateBackupSet_WritesAndClearsThePerSetOverride covers the second
// scope end to end, including the way "inherit again" is spelled: an
// explicit zero, which cannot collide with a real interval because
// anything under config.MinPollInterval is refused.
func TestUpdateBackupSet_WritesAndClearsThePerSetOverride(t *testing.T) {
	svc, _ := openTestService(t)

	before, err := svc.GetBackupSet(context.Background(), "production/postgres-primary")
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	if before.PollInterval != nil {
		t.Fatalf("a set that configured no override reports %s, want nil (inherit)", before.PollInterval)
	}
	if want := 15 * time.Minute; before.EffectivePollInterval != want {
		t.Errorf("EffectivePollInterval = %s, want the deployment's %s", before.EffectivePollInterval, want)
	}

	set, err := svc.UpdateBackupSet(context.Background(), "production/postgres-primary", UpdateBackupSetRequest{
		PollInterval: durationPtr(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("UpdateBackupSet: %v", err)
	}
	if set.PollInterval == nil || *set.PollInterval != 5*time.Minute {
		t.Fatalf("PollInterval = %v, want 5m0s", set.PollInterval)
	}
	if set.EffectivePollInterval != 5*time.Minute {
		t.Errorf("EffectivePollInterval = %s, want the override's 5m0s", set.EffectivePollInterval)
	}

	// An unrelated sparse edit must not disturb it.
	set, err = svc.UpdateBackupSet(context.Background(), "production/postgres-primary", UpdateBackupSetRequest{
		StaleAfter: durationPtr(48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("UpdateBackupSet (stale_after): %v", err)
	}
	if set.PollInterval == nil || *set.PollInterval != 5*time.Minute {
		t.Fatalf("an edit that named stale_after moved poll_interval to %v", set.PollInterval)
	}

	// Zero is "inherit again".
	set, err = svc.UpdateBackupSet(context.Background(), "production/postgres-primary", UpdateBackupSetRequest{
		PollInterval: durationPtr(0),
	})
	if err != nil {
		t.Fatalf("UpdateBackupSet (clear): %v", err)
	}
	if set.PollInterval != nil {
		t.Fatalf("PollInterval = %v after a clear, want nil (inherit)", set.PollInterval)
	}
	if want := 15 * time.Minute; set.EffectivePollInterval != want {
		t.Errorf("EffectivePollInterval = %s after a clear, want the deployment's %s", set.EffectivePollInterval, want)
	}
}

// TestUpdateBackupSet_RefusesAnOverrideUnderTheFloor holds the per-set
// scope to the same floor as the global one, since a typo here hammers
// one operator's source just as hard.
func TestUpdateBackupSet_RefusesAnOverrideUnderTheFloor(t *testing.T) {
	svc, _ := openTestService(t)

	_, err := svc.UpdateBackupSet(context.Background(), "production/postgres-primary", UpdateBackupSetRequest{
		PollInterval: durationPtr(15 * time.Second),
	})
	if err == nil {
		t.Fatal("a 15s per-set poll_interval was accepted")
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("error = %v, want ErrInvalidRequest", err)
	}
}

// TestASettingsSaveDoesNotRestartThePollSchedule is the regression for
// the one thing this feature could break for every deployment at once.
//
// Every configuration write rebuilds the whole app.Service
// (configreload.go). A rebuild that did not carry the poll schedule
// across would leave every backup set reading as never polled, so the
// next scheduled wake would go and knock on every source in the
// deployment -- and it would do that after ANY save, which is exactly
// when an operator is making several in a row.
//
// It goes through UpdateSettings and UpdateBackupSet rather than calling
// the adoption directly, because "the adoption works" was never in
// doubt: whether the write path calls it is the whole question.
func TestASettingsSaveDoesNotRestartThePollSchedule(t *testing.T) {
	svc, _ := openTestService(t)

	// One scheduled cycle through the production path, so the set has an
	// attempt behind it and is not due again for its 15 minute interval.
	svc.runScheduledCycle(context.Background())
	if wake := svc.state.Load().inner.NextPollWake(time.Now()); wake < 14*time.Minute {
		t.Fatalf("after one scheduled cycle the next wake is %s, want about 15m: the fixture never recorded an attempt, so this test would prove nothing", wake)
	}

	// A settings save, of the interval itself: the schedule has to
	// survive it AND be measured against the new interval.
	if _, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Service: &ServiceUpdate{PollInterval: durationPtr(20 * time.Minute)},
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if wake := svc.state.Load().inner.NextPollWake(time.Now()); wake < 19*time.Minute {
		t.Fatalf("after a settings save the next wake is %s, want about 20m: the save reset the poll schedule, so the next wake polls every source in the deployment", wake)
	}

	// And a backup-set save, which is the same reload through a
	// different door.
	if _, err := svc.UpdateBackupSet(context.Background(), "production/postgres-primary", UpdateBackupSetRequest{
		StaleAfter: durationPtr(48 * time.Hour),
	}); err != nil {
		t.Fatalf("UpdateBackupSet: %v", err)
	}
	if wake := svc.state.Load().inner.NextPollWake(time.Now()); wake < 19*time.Minute {
		t.Fatalf("after a backup-set save the next wake is %s, want about 20m: the save reset the poll schedule", wake)
	}
}

// TestRunOnSchedule_RecomputesItsSleepWhenTheConfigurationChanges is the
// claim the Settings page makes in so many words -- "in effect now, with
// no restart" -- held to its literal meaning.
//
// A loop that only re-reads the configuration after its current sleep
// ends honours a save at the END of the old interval, so an operator who
// moves a deployment from daily to every minute waits up to a day for the
// first minute-long tick, while the page says the change is live. The
// save therefore has to reach a SLEEPING loop: adoptConfig signals, and
// the loop drops the timer it is holding and computes a new one.
func TestRunOnSchedule_RecomputesItsSleepWhenTheConfigurationChanges(t *testing.T) {
	svc, _ := openTestService(t)

	timers := make(chan *stubTimer, 8)
	withStubbedSchedulerTimer(t, timers)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- svc.RunOnSchedule(ctx) }()

	// The first sleep follows the first cycle: the set was just
	// attempted, so it is due again in the deployment's 15 minutes.
	first := awaitTimer(t, timers)
	if first.d < 14*time.Minute {
		t.Fatalf("the loop's first sleep is %s, want about 15m", first.d)
	}

	// The operator saves a one-minute interval while the loop is asleep
	// inside that fifteen-minute timer.
	if _, err := svc.UpdateSettings(context.Background(), UpdateSettingsRequest{
		Service: &ServiceUpdate{PollInterval: durationPtr(time.Minute)},
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	next := awaitTimer(t, timers)
	if next.d > time.Minute {
		t.Errorf("after a save to a one-minute interval the sleeping loop re-armed for %s; the new cadence only takes effect when the OLD timer finally fires", next.d)
	}

	cancel()
	next.fire()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("RunOnSchedule: %v", err)
		}
	case <-time.After(schedulerTestBudget):
		t.Fatal("RunOnSchedule did not return after its context was cancelled")
	}
}

// schedulerTestBudget is how long these tests wait for the loop to do
// something. It is not a latency assertion: the only failure it can catch
// is a loop that will never do the thing at all.
const schedulerTestBudget = 10 * time.Second

// stubTimer is one sleep the scheduler loop asked for, held open so the
// test decides when (and whether) it ever fires.
type stubTimer struct {
	d    time.Duration
	ch   chan time.Time
	stop chan struct{}
}

func (s *stubTimer) fire() {
	select {
	case s.ch <- time.Now():
	default:
	}
}

// withStubbedSchedulerTimer replaces the loop's sleep with one this test
// controls, publishing every sleep the loop asks for on timers. A real
// timer cannot express the case under test: the assertion is about a
// fifteen-minute sleep being abandoned, and no test may wait fifteen
// minutes to see it.
func withStubbedSchedulerTimer(t *testing.T, timers chan *stubTimer) {
	t.Helper()
	orig := scheduleTimer
	t.Cleanup(func() { scheduleTimer = orig })
	scheduleTimer = func(d time.Duration) (<-chan time.Time, func() bool) {
		s := &stubTimer{d: d, ch: make(chan time.Time, 1), stop: make(chan struct{})}
		select {
		case timers <- s:
		default:
		}
		return s.ch, func() bool { close(s.stop); return true }
	}
}

func awaitTimer(t *testing.T, timers chan *stubTimer) *stubTimer {
	t.Helper()
	select {
	case s := <-timers:
		return s
	case <-time.After(schedulerTestBudget):
		t.Fatal("the scheduler loop never armed another timer: a configuration change did not reach the sleeping loop at all")
		return nil
	}
}
