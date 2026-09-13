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
	"sync/atomic"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/app"
	"github.com/backupdproject/backupd/core/internal/config"
)

func newTestServiceWithPollInterval(t *testing.T, d time.Duration, sources ...config.Source) *BackupService {
	t.Helper()
	cfg := testConfig(sources...)
	cfg.PollInterval = config.Duration(d)
	return New(cfg, openTestJournal(t), nil, nil)
}

// TestRunOnSchedule_WakesAtTheTightestConfiguredCadence is the scheduler
// half of #845: the loop's sleep is derived from the whole configuration,
// not from the deployment default alone, or a set that polls more often
// than the deployment could never be reached.
func TestRunOnSchedule_WakesAtTheTightestConfiguredCadence(t *testing.T) {
	tight := config.Duration(20 * time.Millisecond)
	set := config.BackupSet{Name: "fast", PollInterval: &tight}
	svc := newTestServiceWithPollInterval(t, time.Hour, config.Source{Name: "production", BackupSets: []config.BackupSet{set}})

	var cycles int32
	withStubbedRunCycle(t, func(inner *app.Service, ctx context.Context) app.CycleReport {
		atomic.AddInt32(&cycles, 1)
		return app.CycleReport{}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	if err := svc.RunOnSchedule(ctx); err != nil {
		t.Fatalf("RunOnSchedule: %v", err)
	}

	if got := atomic.LoadInt32(&cycles); got < 2 {
		t.Errorf("the loop ran %d cycle(s) in 150ms with a 20ms per-set override, want at least 2: it slept the deployment default instead", got)
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
