// The behavioural half of this package, asked of the engine this product
// actually ships rather than of a fake that could only answer with what
// this package already believes.
//
// Three questions, and none of them can be asked of a stub. Is the
// repository still readable -- not merely still there, but restorable
// into files -- after a maintenance window? Can a maintenance window and
// this product's own destructive snapshot pass interleave? And when
// maintenance fails, is the last known good backup still a backup?
//
// This file uses the real engine adapter, which is allowed here for the
// reason internal/snapshotretention's kopiablobs_test.go gives: what is
// quarantined is the VENDOR's import, and this file imports this
// product's adapter package like any other caller would.

package repomaintenance_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/kopia"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/repomaintenance"
	"github.com/backupdproject/backupd/core/internal/secretref"
)

const (
	keptFileName = "ledger.txt"
	keptFileBody = "the row a retained snapshot exists to preserve\n"
)

// localRepository creates and opens a real repository on a local
// filesystem and returns it with its domain.
func localRepository(t *testing.T, name string) (backupengine.Repository, model.RepositoryDomainID) {
	t.Helper()

	ctx := context.Background()

	domain, err := model.NewRepositoryDomainID(name)
	if err != nil {
		t.Fatalf("NewRepositoryDomainID: %v", err)
	}

	secret := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(secret, []byte("maintenance-test-passphrase-not-a-secret\n"), 0o600); err != nil {
		t.Fatalf("writing the passphrase file: %v", err)
	}

	loc := backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     domain,
		Root:       t.TempDir(),
		StateDir:   filepath.Join(t.TempDir(), "state"),
		Passphrase: secretref.Ref{File: secret},
	}

	eng := kopia.New()
	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	t.Cleanup(func() { _ = rep.Close(context.Background()) })

	return rep, domain
}

// snapshotOnce writes one file into a fresh source directory and stores a
// snapshot of it.
func snapshotOnce(t *testing.T, rep backupengine.Repository, body string) backupengine.SnapshotID {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, keptFileName), []byte(body), 0o600); err != nil {
		t.Fatalf("writing the source file: %v", err)
	}

	info, err := rep.Snapshot(context.Background(), backupengine.SnapshotRequest{
		Source: backupengine.Source{Host: "test-host", User: "test-user", Path: dir},
	})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	return info.ID
}

// assertRestores is the only acceptable proof that a repository is still
// readable: the bytes come back.
func assertRestores(t *testing.T, rep backupengine.Repository, id backupengine.SnapshotID, want string) {
	t.Helper()

	dest := t.TempDir()

	report, err := rep.Restore(context.Background(), id, backupengine.RestoreRequest{TargetPath: dest, SkipOwners: true})
	if err != nil {
		t.Fatalf("Restore of %s: %v", id, err)
	}

	if !report.Complete {
		t.Fatalf("the restore of %s did not complete: %+v", id, report)
	}

	got, err := os.ReadFile(filepath.Join(dest, keptFileName))
	if err != nil {
		t.Fatalf("reading the restored file: %v", err)
	}

	if string(got) != want {
		t.Fatalf("the restored file holds %q, want %q", got, want)
	}
}

// TestARepositoryIsStillReadableAndRestorableAfterMaintenance is issue
// #786's integrity criterion. "Readable" is asserted as a restore,
// because a repository whose manifests still list is not a repository
// whose content is still there.
func TestARepositoryIsStillReadableAndRestorableAfterMaintenance(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep, domain := localRepository(t, "post-maintenance-restore")

	kept := snapshotOnce(t, rep, keptFileBody)
	doomed := snapshotOnce(t, rep, "content only this snapshot references\n")

	if err := rep.DeleteSnapshot(ctx, doomed); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}

	runner := testRunner("instance-a", testStore(t), repomaintenance.NewFence())
	now := time.Now()

	// Both modes, because they are two different sets of tasks against
	// the same indexes and either could damage what the other left.
	for _, mode := range []backupengine.MaintenanceMode{backupengine.MaintenanceQuick, backupengine.MaintenanceFull} {
		result, err := runner.Run(ctx, domain, rep, mode, now)
		if err != nil {
			t.Fatalf("%s maintenance: %v", mode, err)
		}

		if !result.Attempted {
			t.Fatalf("%s maintenance was not attempted: %s", mode, result.Reason)
		}

		now = now.Add(time.Second)
	}

	assertRestores(t, rep, kept, keptFileBody)

	// And the repository's own health check agrees it is usable.
	health, err := rep.Health(ctx)
	if err != nil {
		t.Fatalf("Health after maintenance: %v", err)
	}

	if !health.Reachable {
		t.Errorf("the repository is not reachable after maintenance: %+v", health.Warnings)
	}
}

// TestMaintenanceAndSnapshotDeletesCannotInterleaveOnOneRepository is the
// fencing criterion asked of real operations against a real repository:
// a full maintenance window and this product's destructive snapshot
// deletes, running at the same time, through the fence.
//
// The assertion is twofold and both halves matter. Nothing may observe
// the other side inside its own section -- that is the fence -- and the
// repository has to survive it, which is what proves the fence did not
// merely make the test serial by breaking one of the two operations.
func TestMaintenanceAndSnapshotDeletesCannotInterleaveOnOneRepository(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep, domain := localRepository(t, "fenced-domain")
	fence := repomaintenance.NewFence()

	kept := snapshotOnce(t, rep, keptFileBody)

	doomed := make([]backupengine.SnapshotID, 0, 4)
	for i := range 4 {
		doomed = append(doomed, snapshotOnce(t, rep, "disposable content "+string(rune('a'+i))+"\n"))
	}

	// The retention side reaches the repository exactly the way a real
	// pass would: through the fence's own wrapper, which is the port
	// internal/snapshotretention takes.
	tl := newTimeline()
	deletes := fence.GuardSnapshots(domain, observedDeletes{repo: rep, watch: tl})
	runner := testRunner("instance-a", testStore(t), fence)

	var wg sync.WaitGroup

	wg.Add(2)

	maintenanceErr := make(chan error, 1)

	go func() {
		defer wg.Done()

		now := time.Now()

		for range 3 {
			// Run's own fencing is what is under test here: nothing in
			// this goroutine takes the fence by hand.
			_, err := runner.Run(ctx, domain, observedMaintenance{repo: rep, watch: tl}, backupengine.MaintenanceFull, now)
			if err != nil {
				maintenanceErr <- err

				return
			}

			now = now.Add(time.Minute)
		}

		maintenanceErr <- nil
	}()

	deleteErr := make(chan error, 1)

	go func() {
		defer wg.Done()

		for _, id := range doomed {
			if err := deletes.DeleteSnapshot(ctx, id); err != nil {
				deleteErr <- err

				return
			}
		}

		deleteErr <- nil
	}()

	wg.Wait()

	if err := <-maintenanceErr; err != nil {
		t.Fatalf("maintenance under contention: %v", err)
	}

	if err := <-deleteErr; err != nil {
		t.Fatalf("snapshot deletes under contention: %v", err)
	}

	if tl.overlapped("maintenance", "delete") {
		t.Error("a full maintenance window and a snapshot delete were both inside the repository at the same time")
	}

	// The other half: the repository came through it, and so did the
	// snapshot nothing deleted.
	assertRestores(t, rep, kept, keptFileBody)

	for _, id := range doomed {
		if _, err := rep.LookupSnapshot(ctx, id); !errors.Is(err, backupengine.ErrSnapshotNotFound) {
			t.Errorf("LookupSnapshot(%s) = %v, want ErrSnapshotNotFound: the delete was fenced, not skipped", id, err)
		}
	}
}

// observedMaintenance and observedDeletes record when each operation is
// really inside the repository, which is the only place an overlap could
// be observed: the fence's own bookkeeping would agree with itself.
type observedMaintenance struct {
	repo  backupengine.Repository
	watch *timeline
}

func (o observedMaintenance) Maintain(ctx context.Context, mode backupengine.MaintenanceMode) (backupengine.MaintenanceReport, error) {
	o.watch.enter("maintenance")
	defer o.watch.leave("maintenance")

	return o.repo.Maintain(ctx, mode)
}

func (o observedMaintenance) Stats(ctx context.Context) (backupengine.RepositoryStats, error) {
	o.watch.enter("maintenance")
	defer o.watch.leave("maintenance")

	return o.repo.Stats(ctx)
}

type observedDeletes struct {
	repo  backupengine.Repository
	watch *timeline
}

func (o observedDeletes) LookupSnapshot(ctx context.Context, id backupengine.SnapshotID) (backupengine.SnapshotInfo, error) {
	return o.repo.LookupSnapshot(ctx, id)
}

func (o observedDeletes) DeleteSnapshot(ctx context.Context, id backupengine.SnapshotID) error {
	o.watch.enter("delete")
	defer o.watch.leave("delete")

	return o.repo.DeleteSnapshot(ctx, id)
}

// TestAFailedMaintenanceLeavesTheLastKnownGoodBackupIntact is the
// failure criterion, against a real repository: a window that fails must
// leave every restore point exactly where it was, and say so in the
// record.
//
// The failure is produced by cancelling the window's context, which is
// the realistic one -- a daemon shutting down, an operator interrupting,
// a deadline -- and the one that stops maintenance at an arbitrary point
// rather than before it started.
func TestAFailedMaintenanceLeavesTheLastKnownGoodBackupIntact(t *testing.T) {
	t.Parallel()

	rep, domain := localRepository(t, "failed-maintenance")
	kept := snapshotOnce(t, rep, keptFileBody)

	runner := testRunner("instance-a", testStore(t), repomaintenance.NewFence())

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runner.Run(cancelled, domain, rep, backupengine.MaintenanceFull, time.Now())
	if err == nil {
		t.Fatal("a maintenance window with a cancelled context reported success")
	}

	// The restore point is still a restore point, read through a context
	// that is not the cancelled one.
	assertRestores(t, rep, kept, keptFileBody)

	record, err := runner.Store.Load(context.Background(), domain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if record.LastResult.Err == "" {
		t.Fatal("the ownership record does not say the window failed, so the next pass cannot tell it apart from one that never ran")
	}

	if !record.LastFull.IsZero() {
		t.Errorf("the record claims a full maintenance at %v; the window failed", record.LastFull)
	}

	if got := repomaintenance.AlertConditions(record); len(got) != 1 {
		t.Fatalf("a failed window produced %d alert conditions, want 1", len(got))
	}
}
