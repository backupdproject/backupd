package kopia

import (
	"bytes"
	"context"
	"crypto/rand"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"

	"github.com/retnd/retnd/core/internal/backupengine"
)

// Issue #786's "physical space reclamation is measurable", proved against
// a real repository at the vendor's FULL safety parameters, which is the
// only version of the claim worth having.
//
// # Why this test owns a clock
//
// SafetyFull is made of time: content is not collected until it is 24
// hours old, deleted contents are not dropped from the index until two
// garbage collections four hours apart agree, and an unreferenced pack
// blob is not deleted until it has been unreferenced for a day. Those
// margins are the whole point of the safety level -- they are what makes
// maintenance safe beside a snapshot that is still being written -- so a
// test that wanted to see reclamation without waiting a day has exactly
// two options: weaken the safety parameters, or move time.
//
// Weakening them would be a test of a configuration this product must
// never ship (see maintenancesafety_test.go), so this file moves time
// instead, and it has to move it in TWO places at once. The repository's
// own clock comes from repo.Options.TimeNowFunc, but the vendor also
// compares its local clock against the timestamp the STORAGE reports for
// the maintenance schedule blob and refuses to run if they disagree by
// more than five minutes -- correctly, because that comparison is how a
// repository detects the clock skew that would let one process collect
// another's content. So the storage is wrapped in one that stamps every
// blob it writes with the same clock, which is what a filesystem with a
// consistent clock would have done anyway.
//
// Nothing about the code under test is faked: this is the adapter's own
// Maintain, at maintenance.SafetyFull, against a real repository on a
// real filesystem.

// clockedStorageType is the storage type name this file registers so that
// a repository re-opened from its config file gets the wrapped storage
// back rather than a plain filesystem one.
const clockedStorageType = "backupd-test-clocked-filesystem"

// testClock is one repository's idea of the time, moved by the test.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// clocks maps a storage path to the clock its blobs are stamped with, so
// that the factory below can find the right one when the vendor rebuilds
// storage from a config file, and so that two tests can hold two
// repositories at two different fake times.
var (
	clocksMu sync.Mutex
	clocks   = map[string]*testClock{}
)

func registerClock(path string, c *testClock) {
	clocksMu.Lock()
	defer clocksMu.Unlock()

	clocks[path] = c
}

func clockFor(path string) *testClock {
	clocksMu.Lock()
	defer clocksMu.Unlock()

	if c, ok := clocks[path]; ok {
		return c
	}

	// A path nobody registered gets the real clock, which is the
	// behaviour of an unwrapped filesystem storage.
	return &testClock{now: time.Now()}
}

//nolint:gochecknoinits // a blob provider has to be registered before any config file naming it is opened.
func init() {
	blob.AddSupportedStorage(clockedStorageType, filesystem.Options{},
		func(ctx context.Context, opts *filesystem.Options, create bool) (blob.Storage, error) {
			st, err := filesystem.New(ctx, opts, create)
			if err != nil {
				return nil, err
			}

			return &clockedStorage{Storage: st, opts: *opts, clock: clockFor(opts.Path)}, nil
		})
}

// clockedStorage is a filesystem storage whose blobs carry the test's
// clock rather than the machine's.
type clockedStorage struct {
	blob.Storage

	opts  filesystem.Options
	clock *testClock
}

func (c *clockedStorage) ConnectionInfo() blob.ConnectionInfo {
	return blob.ConnectionInfo{Type: clockedStorageType, Config: &c.opts}
}

func (c *clockedStorage) PutBlob(ctx context.Context, id blob.ID, data blob.Bytes, opts blob.PutOptions) error {
	if opts.SetModTime.IsZero() {
		opts.SetModTime = c.clock.Now()
	}

	return c.Storage.PutBlob(ctx, id, data, opts) //nolint:wrapcheck // a transparent decorator
}

// clockedRepository builds a real repository on a real filesystem whose
// clock the test controls, and hands back this adapter's own repository
// handle over it.
const clockedPassphrase = "reclamation-test-passphrase-not-a-secret"

func clockedRepository(t *testing.T, clock *testClock) (*repository, string) {
	t.Helper()

	ctx := context.Background()
	blobs := filepath.Join(t.TempDir(), "blobs")

	if err := os.MkdirAll(blobs, 0o700); err != nil {
		t.Fatalf("creating the blob directory: %v", err)
	}

	registerClock(blobs, clock)

	opts := filesystem.Options{Path: blobs}

	plain, err := filesystem.New(ctx, &opts, true)
	if err != nil {
		t.Fatalf("filesystem.New: %v", err)
	}

	st := &clockedStorage{Storage: plain, opts: opts, clock: clock}

	const passphrase = clockedPassphrase

	if err := repo.Initialize(ctx, st, &repo.NewRepositoryOptions{}, passphrase); err != nil {
		t.Fatalf("repo.Initialize: %v", err)
	}

	configPath := filepath.Join(t.TempDir(), "repository.config")
	if err := repo.Connect(ctx, configPath, st, passphrase, &repo.ConnectOptions{}); err != nil {
		t.Fatalf("repo.Connect: %v", err)
	}

	if err := st.Close(ctx); err != nil {
		t.Fatalf("closing the connect-time storage: %v", err)
	}

	rep, err := repo.Open(ctx, configPath, passphrase, &repo.Options{TimeNowFunc: clock.Now})
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}

	t.Cleanup(func() { _ = rep.Close(context.Background()) })

	direct, ok := rep.(repo.DirectRepository)
	if !ok {
		t.Fatal("the repository opened without direct storage access")
	}

	return &repository{rep: rep, direct: direct, adapter: New(WithClock(clock.Now))}, blobs
}

// dirBytes is what the repository really occupies, counted from the
// filesystem rather than from the repository's own accounting: the
// question "did maintenance free any disk" is a question about the disk.
func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()

	var total int64

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		total += info.Size()

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}

	return total
}

// incompressibleSource writes one directory holding n bytes of random
// data, which is the only kind of fixture whose size survives
// deduplication and compression well enough to be measured.
func incompressibleSource(t *testing.T, n int) (string, []byte) {
	t.Helper()

	body := make([]byte, n)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("generating source data: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "payload.bin"), body, 0o600); err != nil {
		t.Fatalf("writing the source: %v", err)
	}

	return dir, body
}

// TestFullMaintenanceReclaimsOnlyWhatNothingReferences is the whole
// criterion in one pass: delete the manifest that is the only reference
// to several megabytes of content, run full maintenance until the
// vendor's safety margins allow the reclamation, and measure the disk.
//
// Both halves are asserted, and the second is the one that matters most:
// the content the RETAINED snapshot references is still there, and still
// restores byte for byte. A maintenance window that shrank the repository
// by deleting everything would satisfy the first half perfectly.
func TestFullMaintenanceReclaimsOnlyWhatNothingReferences(t *testing.T) {
	ctx := context.Background()

	clock := &testClock{now: time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)}
	rep, blobs := clockedRepository(t, clock)

	const (
		keptBytes   = 4 << 20
		doomedBytes = 16 << 20
	)

	keptDir, keptBody := incompressibleSource(t, keptBytes)
	doomedDir, _ := incompressibleSource(t, doomedBytes)

	kept, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{
		Source: backupengine.Source{Host: "test-host", User: "test-user", Path: keptDir},
	})
	if err != nil {
		t.Fatalf("snapshotting the retained source: %v", err)
	}

	// A minute apart, and that matters: two manifests written at the same
	// instant, and a manifest deleted in the same instant it was written,
	// are indistinguishable by the modification time the repository
	// merges its manifest entries by. A real deployment cannot produce
	// that; a test holding a frozen clock can, and what it produces is a
	// deleted snapshot that comes back.
	clock.Advance(time.Minute)

	doomed, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{
		Source: backupengine.Source{Host: "test-host", User: "test-user", Path: doomedDir},
	})
	if err != nil {
		t.Fatalf("snapshotting the disposable source: %v", err)
	}

	before := dirBytes(t, blobs)
	if before < keptBytes+doomedBytes {
		t.Fatalf("the repository holds %d bytes after storing %d of random data, so this test is not measuring a real repository",
			before, keptBytes+doomedBytes)
	}

	// Retention's half of the arrangement: the manifest goes, and nothing
	// on the disk moves. That is #785's own guarantee, and it is what
	// leaves this content unreferenced for maintenance to find.
	clock.Advance(time.Minute)

	if err := rep.DeleteSnapshot(ctx, doomed.ID); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}

	if afterDelete := dirBytes(t, blobs); afterDelete < before {
		t.Fatalf("deleting a manifest freed %d bytes of storage on its own; retention must never reclaim storage", before-afterDelete)
	}

	// Days, each with a backup and a maintenance window, until the
	// reclamation lands -- and proving the product reclaims WITHOUT
	// shortening the vendor's margins is the entire point of this test.
	// The vendor spreads the work deliberately: content is marked deleted
	// only once it is older than the minimum age, deleted contents leave
	// the index only once two garbage collections far enough apart have
	// agreed, the index epoch holding those entries is only compacted
	// once enough index blobs have accumulated behind it, and the packs
	// are only deleted once they have been unreferenced for longer than
	// the pack-delete margin.
	//
	// A backup runs on each of those days as well as a maintenance
	// window, and that is not decoration: a repository nothing writes to
	// accumulates no index blobs, so the epoch that still references the
	// deleted content is never superseded and the packs behind it are
	// never orphaned. Modelling a deployment that backs up daily and
	// maintains daily is both the realistic fixture and the one whose
	// reclamation is reachable.
	//
	// The number of days is a BOUND rather than an expectation, and
	// deliberately so: how the vendor distributes those steps across runs
	// is its business and moves between versions, and a test that pinned
	// "the fourth window frees it" would fail on an upstream bump that
	// was still perfectly safe. What is asserted is that a repository
	// maintained at full safety really does give the space back.
	const days = 14

	dailyDir := t.TempDir()
	after := before

	for day := range days {
		// The second window is close behind the first, because two
		// garbage collections have to be separated by more than the
		// safety margin before deleted contents may leave the index, and
		// a schedule of daily windows would satisfy that on its own.
		gap := 25 * time.Hour
		if day == 1 {
			gap = 5 * time.Hour
		}

		clock.Advance(gap)

		if err := os.WriteFile(filepath.Join(dailyDir, "daily.txt"), []byte(clock.Now().String()), 0o600); err != nil {
			t.Fatalf("writing the daily source: %v", err)
		}

		if _, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{
			Source: backupengine.Source{Host: "test-host", User: "test-user", Path: dailyDir},
		}); err != nil {
			t.Fatalf("the day %d backup: %v", day+1, err)
		}

		report, err := rep.Maintain(ctx, backupengine.MaintenanceFull)
		if err != nil {
			t.Fatalf("the day %d full maintenance: %v", day+1, err)
		}

		if !report.Ran {
			t.Fatalf("the day %d full maintenance declined to do anything", day+1)
		}

		after = dirBytes(t, blobs)
		t.Logf("after day %d the repository occupies %d bytes", day+1, after)

		if before-after >= doomedBytes/2 {
			break
		}
	}

	// The measurement. Most of the disposable snapshot's content has to
	// have gone; the threshold is deliberately below its full size
	// because pack blobs are shared and index blobs are rewritten, and a
	// test asserting an exact number would be asserting the vendor's
	// packing arithmetic.
	if freed := before - after; freed < doomedBytes/2 {
		t.Errorf("after %d days of backup and full maintenance the repository has given back %d bytes of the %d the deleted snapshot was the only reference to (before %d, after %d)",
			days, freed, doomedBytes, before, after)
	}

	// And the half that must not have happened.
	if after < keptBytes {
		t.Fatalf("the repository is down to %d bytes, less than the %d the retained snapshot's content occupies: maintenance reclaimed something a snapshot still references",
			after, keptBytes)
	}

	dest := t.TempDir()

	restored, err := rep.Restore(ctx, kept.ID, backupengine.RestoreRequest{TargetPath: dest, SkipOwners: true})
	if err != nil {
		t.Fatalf("restoring the retained snapshot after maintenance: %v", err)
	}

	if !restored.Complete {
		t.Fatalf("the restore did not complete: %+v", restored)
	}

	got, err := os.ReadFile(filepath.Join(dest, "payload.bin"))
	if err != nil {
		t.Fatalf("reading the restored payload: %v", err)
	}

	if !bytes.Equal(got, keptBody) {
		t.Fatalf("the restored payload is %d bytes and does not match the %d that were backed up", len(got), len(keptBody))
	}
}
