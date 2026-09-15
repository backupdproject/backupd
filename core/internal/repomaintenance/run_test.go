package repomaintenance_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/alert"
	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/repomaintenance"
)

// What one maintenance window records, what it measures, and what a
// failed one does. The reclamation ARITHMETIC against a real repository
// lives in internal/backupengine/kopia; what is asked here is that this
// package reports what the repository told it and never invents a number.

var errMaintenanceFailed = errors.New("storage refused the index write")

// TestADueRepositoryGetsFullMaintenanceBeforeQuick: both are due on a
// repository that has never been maintained, and full is the one that
// reclaims space, so full is the one that runs.
func TestADueRepositoryGetsFullMaintenanceBeforeQuick(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	domain := testDomain(t, "never-maintained")
	runner := testRunner("instance-a", testStore(t), repomaintenance.NewFence())
	repo := &fakeRepository{ran: true}
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	result, err := runner.RunDue(ctx, domain, repo, now)
	if err != nil {
		t.Fatalf("RunDue: %v", err)
	}

	if !result.Attempted || result.Mode != backupengine.MaintenanceFull {
		t.Fatalf("RunDue performed %+v, want an attempted full maintenance", result)
	}

	// Immediately afterwards nothing is due: the record says so, and a
	// scheduler that ran maintenance on every cycle would be a repository
	// that spends its life being maintained.
	next, err := runner.RunDue(ctx, domain, repo, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("the second RunDue: %v", err)
	}

	if next.Attempted {
		t.Errorf("maintenance ran again a minute later: %s", next.Reason)
	}

	// A quick cycle comes due before the next full one.
	quick, err := runner.RunDue(ctx, domain, repo, now.Add(repomaintenance.DefaultIntervals.Quick+time.Minute))
	if err != nil {
		t.Fatalf("the quick RunDue: %v", err)
	}

	if !quick.Attempted || quick.Mode != backupengine.MaintenanceQuick {
		t.Fatalf("an hour later RunDue performed %+v, want an attempted quick maintenance", quick)
	}

	if got := repo.maintained(); len(got) != 2 {
		t.Errorf("the repository was maintained %v, want one full and one quick", got)
	}
}

// TestMaintenanceReportsWhatItReclaimedAndNeverGuesses: the numbers come
// from the repository's own before-and-after accounting, and a run that
// freed nothing says nothing was freed.
func TestMaintenanceReportsWhatItReclaimedAndNeverGuesses(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	domain := testDomain(t, "reclaiming-domain")
	runner := testRunner("instance-a", testStore(t), repomaintenance.NewFence())
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	repo := &fakeRepository{ran: true, stats: []backupengine.RepositoryStats{
		{Blobs: 20, PhysicalBytes: 100 << 20},
		{Blobs: 12, PhysicalBytes: 60 << 20},
	}}

	result, err := runner.Run(ctx, domain, repo, backupengine.MaintenanceFull, now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.PhysicalBytesBefore != 100<<20 || result.PhysicalBytesAfter != 60<<20 {
		t.Fatalf("Run measured %d before and %d after, want 100MiB and 60MiB", result.PhysicalBytesBefore, result.PhysicalBytesAfter)
	}

	if result.Reclaimed != 40<<20 {
		t.Errorf("Run reports %d bytes reclaimed, want %d", result.Reclaimed, 40<<20)
	}

	record, err := runner.Store.Load(ctx, domain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(record.History) != 1 || record.History[0].Reclaimed != 40<<20 {
		t.Fatalf("the history records %+v, want one entry reclaiming %d bytes", record.History, 40<<20)
	}

	if !record.LastFull.Equal(now) {
		t.Errorf("the record says the last full maintenance was %v, want %v", record.LastFull, now)
	}
}

// TestQuickMaintenanceDoesNotPayForABlobListing: Stats walks the
// storage's own blob listing, which on a bucket is a real number of
// requests, and quick maintenance does not reclaim blobs. Measuring it
// anyway would put a per-hour listing on every repository this product
// manages to report a number that is always zero.
func TestQuickMaintenanceDoesNotPayForABlobListing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	domain := testDomain(t, "quick-domain")
	runner := testRunner("instance-a", testStore(t), repomaintenance.NewFence())
	repo := &fakeRepository{ran: true, stats: []backupengine.RepositoryStats{{PhysicalBytes: 1 << 20}}}

	if _, err := runner.Run(ctx, domain, repo, backupengine.MaintenanceQuick, time.Now()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if repo.statsCalls != 0 {
		t.Errorf("quick maintenance called Stats %d times, want 0", repo.statsCalls)
	}
}

// TestAFailedMaintenanceIsRecordedAlertsAndBacksOff is the failure
// contract: the operator is told through the same alerting model every
// other condition uses, the record says what happened, and the next
// attempt is not immediate.
func TestAFailedMaintenanceIsRecordedAlertsAndBacksOff(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	domain := testDomain(t, "failing-domain")
	runner := testRunner("instance-a", testStore(t), repomaintenance.NewFence())
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	repo := &fakeRepository{err: errMaintenanceFailed}

	result, err := runner.Run(ctx, domain, repo, backupengine.MaintenanceFull, now)
	if !errors.Is(err, errMaintenanceFailed) {
		t.Fatalf("Run returned %v, want the repository's own failure", err)
	}

	if result.Ran {
		t.Error("a failed maintenance reported that it ran")
	}

	record, err := runner.Store.Load(ctx, domain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if record.LastResult.Err == "" {
		t.Fatal("the record does not say the last maintenance failed")
	}

	if !record.LastFull.IsZero() {
		t.Errorf("a failed full maintenance recorded LastFull as %v; the repository has still never been fully maintained", record.LastFull)
	}

	// Alerting: the same Condition vocabulary every other proactive
	// notification in this product uses.
	conditions := repomaintenance.AlertConditions(record)
	if len(conditions) != 1 {
		t.Fatalf("a failed maintenance produced %d alert conditions, want 1", len(conditions))
	}

	if conditions[0].Kind != alert.MaintenanceFailed {
		t.Errorf("the condition is %q, want %q", conditions[0].Kind, alert.MaintenanceFailed)
	}

	if conditions[0].Scope != domain.String() {
		t.Errorf("the condition is scoped to %q, want the repository domain %q", conditions[0].Scope, domain)
	}

	// What the detail SAYS is deliberately not asserted. The operator has
	// to be told that nothing was lost -- the obvious reading of
	// "maintenance failed" is that the repository is damaged -- but
	// pinning a phrase makes equivalent wording a test failure and
	// misleading wording a pass. What is asserted is the record state
	// this alert is derived from, above and below: the window failed,
	// nothing advanced LastFull, and the condition resolves on success.

	// Backoff: a repository whose storage is refusing writes must not be
	// asked again on the next cycle.
	soon, err := runner.RunDue(ctx, domain, repo, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("RunDue after a failure: %v", err)
	}

	if soon.Attempted {
		t.Error("maintenance was retried one minute after it failed")
	}

	// And a later success resolves the condition, which is what makes it
	// safe to hand these to a Dispatcher every cycle.
	repo.err = nil
	repo.ran = true

	if _, err := runner.Run(ctx, domain, repo, backupengine.MaintenanceFull, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("the recovery run: %v", err)
	}

	recovered, err := runner.Store.Load(ctx, domain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := repomaintenance.AlertConditions(recovered); len(got) != 0 {
		t.Errorf("a successful maintenance still reports %v; the condition has to resolve or it alerts once and never again", got)
	}
}

// TestTheHistoryIsBoundedAndKeepsTheNewest: the record is written to
// every window forever, so the one thing it must not do is grow without
// limit in a file this daemon rewrites on every pass.
func TestTheHistoryIsBoundedAndKeepsTheNewest(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	domain := testDomain(t, "long-lived-domain")
	runner := testRunner("instance-a", testStore(t), repomaintenance.NewFence())
	runner.HistoryLimit = 3
	repo := &fakeRepository{ran: true}
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	for i := range 6 {
		if _, err := runner.Run(ctx, domain, repo, backupengine.MaintenanceQuick, now.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}

	record, err := runner.Store.Load(ctx, domain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(record.History) != 3 {
		t.Fatalf("the history holds %d entries, want the 3 it was limited to", len(record.History))
	}

	// The newest entry is the sixth window. Its At is when that window
	// finished, which is the instant the caller named plus however long
	// the run really took, so it is asserted as an interval rather than
	// as an equality.
	newest := record.History[len(record.History)-1].At
	if last := now.Add(5 * time.Hour); newest.Before(last) || newest.After(last.Add(time.Minute)) {
		t.Errorf("the newest history entry is %v, want the last run, which started at %v", newest, last)
	}

	metrics := repomaintenance.Measure(record)
	if metrics.Runs != 6 {
		t.Errorf("Measure counted %d runs, want the 6 that really happened; a bounded history must not make the counter forget", metrics.Runs)
	}
}

// TestAnUnknownModeIsRefusedBeforeTheRepositoryIsTouched: "maintenance"
// spelled wrong must never resolve to the mode that deletes things.
func TestAnUnknownModeIsRefusedBeforeTheRepositoryIsTouched(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	domain := testDomain(t, "bad-mode-domain")
	runner := testRunner("instance-a", testStore(t), repomaintenance.NewFence())
	repo := &fakeRepository{ran: true}

	if _, err := runner.Run(ctx, domain, repo, backupengine.MaintenanceMode("deep"), time.Now()); err == nil {
		t.Fatal("an unknown maintenance mode was accepted")
	}

	if got := repo.maintained(); len(got) != 0 {
		t.Errorf("the repository was asked for %v", got)
	}
}
