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
	"fmt"
	"maps"
	"os"
	"path/filepath"
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
// filesystem and returns it with its domain and the directory its blobs
// live in, which is what lets a test fail the storage under it.
func localRepository(t *testing.T, name string) (backupengine.Repository, model.RepositoryDomainID, string) {
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

	root := t.TempDir()

	loc := backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     domain,
		Root:       root,
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

	return rep, domain, root
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
	rep, domain, _ := localRepository(t, "post-maintenance-restore")

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
// deletes, contending for the same repository through the fence.
//
// The contention is a barrier and not a race. The maintenance window is
// held open INSIDE the exclusive section, and while it is held the test
// asserts that a delete which has already been asked for cannot get in.
// Launching both and checking that no overlap was observed proves much
// less than it looks like: a fence that excluded nothing would pass it
// whenever the two operations happened not to collide.
//
// Then the barrier is released and the assertions turn positive: the
// delete completes, the repository is still restorable, and the snapshot
// nothing deleted is still there. That half is what proves the fence did
// not merely make the test serial by breaking one of the two operations.
func TestMaintenanceAndSnapshotDeletesCannotInterleaveOnOneRepository(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep, domain, _ := localRepository(t, "fenced-domain")
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

	inside := make(chan struct{})
	resume := make(chan struct{})

	maintenanceErr := make(chan error, 1)

	go func() {
		// Run's own fencing is what is under test here: nothing in this
		// goroutine takes the fence by hand.
		_, err := runner.Run(ctx, domain,
			observedMaintenance{repo: rep, watch: tl, inside: inside, resume: resume},
			backupengine.MaintenanceFull, time.Now())
		maintenanceErr <- err
	}()

	// Maintenance is now inside the exclusive section and staying there.
	<-inside

	asked := make(chan struct{})
	deleteErr := make(chan error, 1)

	go func() {
		close(asked)

		for _, id := range doomed {
			if err := deletes.DeleteSnapshot(ctx, id); err != nil {
				deleteErr <- err

				return
			}
		}

		deleteErr <- nil
	}()

	<-asked

	// The delete has been asked for and cannot have started: the
	// repository is inside a full maintenance window. This is the
	// assertion an unfenced delete fails.
	if n := tl.insideNow("delete"); n != 0 {
		t.Fatalf("%d snapshot deletes were inside the repository during a full maintenance window", n)
	}

	select {
	case err := <-deleteErr:
		t.Fatalf("a snapshot delete completed (%v) while a full maintenance window held the repository", err)
	default:
	}

	close(resume)

	if err := <-maintenanceErr; err != nil {
		t.Fatalf("maintenance under contention: %v", err)
	}

	// And now that the exclusive section is over, the delete that was
	// waiting really does get through: a fence that never released would
	// be just as broken as one that never held.
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

	// inside is closed once maintenance is inside the repository, and
	// resume is waited on before it leaves. Together they hold the
	// exclusive section open for as long as a test needs to ask what
	// else can get in. A zero inside/resume pair is not a barrier.
	inside chan struct{}
	resume chan struct{}
}

func (o observedMaintenance) Maintain(ctx context.Context, mode backupengine.MaintenanceMode) (backupengine.MaintenanceReport, error) {
	o.watch.enter("maintenance")
	defer o.watch.leave("maintenance")

	if o.inside != nil {
		close(o.inside)
		<-o.resume
	}

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
// The failure has to land while maintenance is under way, and that is the
// whole difficulty. A window refused before it starts -- a context
// cancelled before Run, say -- proves only that a no-op is harmless,
// which nobody doubted. So this test makes maintenance really maintain
// the repository first, confirms from the storage that it wrote
// something, and only then takes the storage away underneath it.
//
// What that leaves behind is the state an operator actually meets: a
// repository whose indexes have been partly rewritten by a maintenance
// pass that then died. The assertion is that the snapshot taken before
// it still restores, byte for byte, and that the failure is recorded and
// alerted rather than silently skipped.
func TestAFailedMaintenanceLeavesTheLastKnownGoodBackupIntact(t *testing.T) {
	t.Parallel()

	rep, domain, root := localRepository(t, "failed-maintenance")
	kept := snapshotOnce(t, rep, keptFileBody)

	runner := testRunner("instance-a", testStore(t), repomaintenance.NewFence())

	window, abandon := context.WithCancel(context.Background())
	defer abandon()

	failing := &failsMidMaintenance{t: t, repo: rep, root: root, abandon: abandon}

	_, err := runner.Run(window, domain, failing, backupengine.MaintenanceFull, time.Now())
	if err == nil {
		t.Fatal("a maintenance window whose storage stopped accepting writes reported success")
	}

	if !failing.wrote {
		t.Fatal("the failure was injected before maintenance had written anything, so this test is about a no-op")
	}

	// The restore point is still a restore point, read through a context
	// that is not the abandoned one.
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

	if record.Failures != 1 {
		t.Errorf("the record counts %d failures after one failed window", record.Failures)
	}

	if got := repomaintenance.AlertConditions(record); len(got) != 1 {
		t.Fatalf("a failed window produced %d alert conditions, want 1", len(got))
	}
}

// failsMidMaintenance runs a real maintenance pass against a real
// repository and then fails the window, so that what the window under
// test meets is a repository maintenance has already modified.
//
// It is a wrapper rather than a fault-injecting storage layer because the
// engine's port does not expose one: the adapter opens its own storage
// from a location. What it can do is make that storage refuse writes,
// which is the failure an operator sees as a full disk, a revoked
// credential or an unmounted volume.
type failsMidMaintenance struct {
	t    *testing.T
	repo backupengine.Repository
	root string

	// abandon cancels the window, and is the fallback for a test process
	// that permissions cannot stop (a container running as root). Both
	// are failures arriving mid-maintenance; only one of them can be
	// relied on everywhere.
	abandon func()

	// wrote records that the first pass really did change the storage.
	// The test fails if it did not, because then there was no
	// maintenance in progress to interrupt.
	wrote bool
}

func (f *failsMidMaintenance) Maintain(ctx context.Context, mode backupengine.MaintenanceMode) (backupengine.MaintenanceReport, error) {
	before := storageContents(f.t, f.root)

	if _, err := f.repo.Maintain(ctx, backupengine.MaintenanceQuick); err != nil {
		return backupengine.MaintenanceReport{}, fmt.Errorf("the maintenance pass that was meant to succeed first: %w", err)
	}

	f.wrote = !maps.Equal(before, storageContents(f.t, f.root))

	restore, effective := refuseWrites(f.t, f.root)
	defer restore()

	if !effective {
		f.abandon()
	}

	report, err := f.repo.Maintain(ctx, mode)
	if err == nil {
		return report, errors.New("maintenance reported success against storage that was refusing writes")
	}

	return report, err
}

func (f *failsMidMaintenance) Stats(ctx context.Context) (backupengine.RepositoryStats, error) {
	return f.repo.Stats(ctx)
}

// storageContents is every blob in a repository with its size, which is
// how this file tells "maintenance wrote something" from "maintenance
// declined to do anything".
func storageContents(t *testing.T, root string) map[string]int64 {
	t.Helper()

	contents := map[string]int64{}

	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		contents[path] = info.Size()

		return nil
	}); err != nil {
		t.Fatalf("reading the repository's storage: %v", err)
	}

	return contents
}

// refuseWrites makes a repository's storage reject new blobs, and reports
// whether it really does: a process running as root is not stopped by a
// permission bit, and a failure this test only pretended to inject would
// prove nothing at all.
func refuseWrites(t *testing.T, root string) (restore func(), effective bool) {
	t.Helper()

	var dirs []string

	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			dirs = append(dirs, path)
		}

		return nil
	}); err != nil {
		t.Fatalf("walking the repository's storage: %v", err)
	}

	for _, dir := range dirs {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("making %s read-only: %v", dir, err)
		}
	}

	restore = func() {
		for _, dir := range dirs {
			_ = os.Chmod(dir, 0o700)
		}
	}

	probe := filepath.Join(root, ".write-probe")

	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		return restore, true
	}

	_ = os.Remove(probe)

	return restore, false
}
