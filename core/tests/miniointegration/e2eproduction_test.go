package miniointegration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/backupengine/kopia"
	"github.com/retnd/retnd/core/internal/backupengine/source"
	"github.com/retnd/retnd/core/internal/config"
	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/repomaintenance"
	"github.com/retnd/retnd/core/internal/snapshotlifecycle"
	"github.com/retnd/retnd/core/internal/snapshotretention"
	"github.com/retnd/retnd/core/internal/state"
	"github.com/retnd/retnd/core/internal/transport"
	"github.com/retnd/retnd/core/internal/transport/rclone"
	"github.com/retnd/retnd/core/tests/machines"
)

// This file is #789's required scenario run against a repository in a
// BUCKET, through the whole production stack rather than through the
// engine alone.
//
// # What it adds to this package
//
// TestS3SecondSnapshotReusesPhysicalContent next door already proves the
// incrementality claim at the engine's own boundary, measured off a real
// bucket listing, and it is the better measurement of that one fact. What
// it does not exercise is everything BETWEEN an operator and the engine:
// the run driver, the catalog row an operator reads the numbers off, the
// production reading path, retention, maintenance, and a restore of an
// older version. Those are the parts that decide whether a deployment
// whose repository is a bucket actually works, and every one of them has
// a failure mode that only appears over object storage -- a run that
// reports local byte counts for a remote write, a retention pass that
// deletes a manifest it cannot re-list, a maintenance window that
// reclaims a blob a live snapshot still names.
//
// # Why this one is driven by the run driver
//
// Because the numbers it asserts are the ones an operator reads. The
// engine's TreeSnapshotInfo is not what `snapshot list` prints: the
// catalog row is, and the row is written by the driver from the engine's
// report. A driver that added the scan to the storage number would pass
// every engine-level test in this package and would still tell every
// operator their bucket grows by the size of their source every night.
//
// The resolved configuration layer is in the loop only as far as it has
// to be: internal/snapshotretention decides about a config.BackupSet, so
// this file writes a config.yaml and lets the product resolve one rather
// than assembling the struct by hand and inventing a retention chain no
// operator could have written. Everything else about configuration --
// what a file may say, what Validate refuses -- is core/config's and
// core/tests/e2eproduction's subject, not a MinIO container's.

// The scenario's shape over a bucket. Smaller than the local suite's,
// because every byte here crosses a signed HTTP request to a container:
// eighty files of 24 KiB is just under two megabytes, which is enough for
// "the second night wrote almost nothing" to be unambiguous and few
// enough to stay a fast test.
const (
	s3ScenarioFiles   = 80
	s3ScenarioSize    = 24 << 10
	s3ScenarioFanout  = 4
	s3ScenarioChanged = 1
	s3ScenarioBytes   = int64(s3ScenarioFiles * s3ScenarioSize)
)

// TestTheRequiredProductionScenarioAgainstABucketRepository is the S3 leg
// of #789's required scenario: snapshot, change 1%, snapshot, verify,
// retention, maintenance, lose a source file, restore the old version.
//
// The order is the claim, exactly as it is in the local suite. What is
// asserted differently here is the storage measurement: the catalog's own
// number AND the bucket's physical growth, because over object storage
// those are two facts and only the second one is billed.
func TestTheRequiredProductionScenarioAgainstABucketRepository(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	ctx := context.Background()

	bucket := bucketName(t, fixture)
	medium := fixture.MediumForBucket(bucket)

	loc := s3Location(t, fixture, bucket, "production")
	repo := openTreeRepository(t, loc)

	srcDir := filepath.Join(t.TempDir(), "source")
	original := seedScenarioTree(t, srcDir)

	journal, err := state.Open(ctx, filepath.Join(t.TempDir(), "backupd.db"))
	if err != nil {
		t.Fatalf("opening the journal: %v", err)
	}

	t.Cleanup(func() { _ = journal.Close() })

	dep := &bucketDeployment{
		repo:    repo,
		journal: journal,
		srcDir:  srcDir,
		setUUID: uuid.NewString(),
	}

	dep.set, err = model.NewBackupSetID("production", "uploads-tree")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	dep.domain = loc.Domain

	// --- snapshot A -------------------------------------------------------

	first, err := dep.run(t, "s3-run-a")
	if err != nil || !first.Succeeded() {
		t.Fatalf("snapshot A into the bucket: %v (%s)", err, first.Reason)
	}

	if first.Files != s3ScenarioFiles {
		t.Errorf("snapshot A stored %d files; the source holds %d", first.Files, s3ScenarioFiles)
	}

	if !first.LastKnownGood {
		t.Error("snapshot A is not the set's restore point")
	}

	objectsAfterA, bytesAfterA := bucketContents(t, medium)
	if bytesAfterA < s3ScenarioBytes/2 {
		t.Fatalf("the bucket holds %d bytes in %d objects after a first snapshot of a %d byte incompressible tree; "+
			"every measurement below is a difference between two listings and means nothing if this one is wrong",
			bytesAfterA, objectsAfterA, s3ScenarioBytes)
	}

	// --- one per cent of the source changes -------------------------------

	changed := map[string][]byte{}

	for i := range s3ScenarioChanged {
		rel := scenarioRel(i)
		replacement := make([]byte, s3ScenarioSize)
		s3VerifyWriteRandom(t, filepath.Join(srcDir, filepath.FromSlash(rel)), s3ScenarioSize)

		data, err := os.ReadFile(filepath.Join(srcDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("reading back the changed %s: %v", rel, err)
		}

		copy(replacement, data)
		changed[rel] = replacement
	}

	// --- snapshot B -------------------------------------------------------

	second, err := dep.run(t, "s3-run-b")
	if err != nil || !second.Succeeded() {
		t.Fatalf("snapshot B into the bucket: %v (%s)", err, second.Reason)
	}

	if second.SnapshotID == first.SnapshotID {
		t.Fatal("snapshot B recorded snapshot A's manifest; these have to be two restore points")
	}

	// The catalog's own numbers: the whole tree re-read, almost nothing
	// written. These are the two columns `snapshot list` prints beside
	// each other, and the whole point of printing them separately.
	if second.SourceBytesRead < s3ScenarioBytes {
		t.Errorf("snapshot B read %d bytes off the source; a tree run re-reads all %d of them",
			second.SourceBytesRead, s3ScenarioBytes)
	}

	if limit := s3ScenarioBytes / 4; second.RepositoryBytesWritten >= limit {
		t.Errorf("the catalog records snapshot B as writing %d bytes into the bucket after %d of %d files changed; "+
			"an incremental snapshot of a %d byte tree must write well under %d",
			second.RepositoryBytesWritten, s3ScenarioChanged, s3ScenarioFiles, s3ScenarioBytes, limit)
	}

	if second.ContentReusedBytes <= 0 {
		t.Errorf("the catalog records %d bytes of reuse for snapshot B", second.ContentReusedBytes)
	}

	// And the bucket's own accounting, which is the half nothing this
	// product says can fake. Both bounds matter: zero growth would mean
	// the changed file never reached the bucket at all.
	_, bytesAfterB := bucketContents(t, medium)
	growth := bytesAfterB - bytesAfterA

	if growth <= 0 {
		t.Errorf("the bucket grew by %d bytes over snapshot B; the changed file's new content was never stored", growth)
	}

	if limit := s3ScenarioBytes / 4; growth >= limit {
		t.Errorf("the bucket grew by %d bytes (from %d to %d) over an incremental snapshot of a %d byte tree with one changed file (limit %d)",
			growth, bytesAfterA, bytesAfterB, s3ScenarioBytes, limit)
	}

	t.Logf("bucket repository: logical %d bytes | A wrote %d (bucket %d bytes) | B read %d, wrote %d, reused %d (bucket grew %d)",
		s3ScenarioBytes, first.RepositoryBytesWritten, bytesAfterA,
		second.SourceBytesRead, second.RepositoryBytesWritten, second.ContentReusedBytes, growth)

	// --- verify -----------------------------------------------------------

	report, err := repo.Verify(ctx, backupengine.SnapshotID(second.SnapshotID), backupengine.VerifyRequest{
		Level: model.LevelContentFull,
	})
	if err != nil {
		t.Fatalf("verifying snapshot B over the bucket at content_full: %v", err)
	}

	if report.BytesVerified != s3ScenarioBytes {
		t.Errorf("a content_full verification read %d bytes back out of the bucket; the snapshot holds %d",
			report.BytesVerified, s3ScenarioBytes)
	}

	if report.BlobsChecked == 0 {
		t.Error("the verification checked no blobs, so the bucket was never listed and a missing object would have gone unnoticed")
	}

	// --- retention --------------------------------------------------------

	// A hold on A, because the last step of the scenario restores out of
	// it. See the local suite for the argument; the behaviour under test
	// over a bucket is that a retention pass which cannot re-list a
	// manifest refuses rather than deleting blind.
	if _, err := journal.PlaceSnapshotHold(ctx, state.SnapshotHoldRequest{
		HoldID:   "s3-hold-a",
		RunID:    first.RunID,
		Reason:   "keeping the pre-change restore point for #789's bucket scenario",
		PlacedBy: "miniointegration",
		At:       time.Now(),
	}); err != nil {
		t.Fatalf("placing a hold on snapshot A: %v", err)
	}

	verdicts, err := dep.applyRetention(t)
	if err != nil {
		t.Fatalf("the retention pass over the bucket: %v", err)
	}

	if len(verdicts) != 2 {
		t.Fatalf("the retention pass decided about %d runs; this set has two", len(verdicts))
	}

	if _, err := repo.LookupSnapshot(ctx, backupengine.SnapshotID(first.SnapshotID)); err != nil {
		t.Fatalf("the retention pass removed the held snapshot A from the bucket: %v", err)
	}

	if _, err := repo.LookupSnapshot(ctx, backupengine.SnapshotID(second.SnapshotID)); err != nil {
		t.Fatalf("the retention pass removed the set's restore point from the bucket: %v", err)
	}

	// --- maintenance ------------------------------------------------------

	store, err := backupengine.NewFileMaintenanceOwnershipStore(filepath.Join(t.TempDir(), "maintenance"))
	if err != nil {
		t.Fatalf("NewFileMaintenanceOwnershipStore: %v", err)
	}

	runner := repomaintenance.Runner{
		Owner: backupengine.MaintenanceOwner("miniointegration"),
		Store: store,
		Fence: repomaintenance.NewFence(),
	}

	if _, err := runner.Claim(ctx, loc.Domain); err != nil {
		t.Fatalf("claiming maintenance for %s: %v", loc.Domain, err)
	}

	maintained, err := runner.Run(ctx, loc.Domain, repo, backupengine.MaintenanceFull, time.Now())
	if err != nil {
		t.Fatalf("a full maintenance window over the bucket: %v", err)
	}

	if !maintained.Attempted {
		t.Error("the maintenance window was never attempted, so nothing this step asserts was exercised")
	}

	t.Logf("bucket maintenance: ran=%t reclaimed=%d (%d -> %d)",
		maintained.Ran, maintained.Reclaimed, maintained.PhysicalBytesBefore, maintained.PhysicalBytesAfter)

	if _, err := repo.Verify(ctx, backupengine.SnapshotID(second.SnapshotID), backupengine.VerifyRequest{
		Level: model.LevelContentFull,
	}); err != nil {
		t.Fatalf("snapshot B no longer verifies after a full maintenance window over the bucket: %v", err)
	}

	// --- a source file is lost, and the old version comes back ------------

	lost := scenarioRel(s3ScenarioFiles - 1)
	if err := os.Remove(filepath.Join(srcDir, filepath.FromSlash(lost))); err != nil {
		t.Fatalf("deleting %s from the source: %v", lost, err)
	}

	restored := filepath.Join(t.TempDir(), "from-b")
	if _, err := repo.Restore(ctx, backupengine.SnapshotID(second.SnapshotID), backupengine.RestoreRequest{
		TargetPath: restored,
		Conflict:   backupengine.ConflictRefuse,
		SkipOwners: true,
	}); err != nil {
		t.Fatalf("restoring snapshot B out of the bucket: %v", err)
	}

	got := scenarioHashes(t, restored)
	if got[lost] != original[lost] {
		t.Errorf("the file deleted from the source came back out of the bucket as %q; the source held %q", got[lost], original[lost])
	}

	for rel, replacement := range changed {
		target := filepath.Join(t.TempDir(), "old-version")

		if _, err := repo.Restore(ctx, backupengine.SnapshotID(first.SnapshotID), backupengine.RestoreRequest{
			SourcePath: rel,
			TargetPath: target,
			Conflict:   backupengine.ConflictRefuse,
			SkipOwners: true,
		}); err != nil {
			t.Fatalf("restoring the old version of %s out of the bucket: %v", rel, err)
		}

		data, err := os.ReadFile(filepath.Join(target, filepath.Base(rel)))
		if err != nil {
			t.Fatalf("reading the restored old version of %s: %v", rel, err)
		}

		if hashOf(data) != original[rel] {
			t.Errorf("the old version of %s restored out of the bucket hashes %q; the source held %q before the change",
				rel, hashOf(data), original[rel])
		}

		if hashOf(data) == hashOf(replacement) {
			t.Errorf("the restore returned the NEW content of %s, so the older restore point in the bucket is not distinguishable from the newer one", rel)
		}
	}
}

// --- the deployment ------------------------------------------------------

// bucketDeployment is the few things a run needs that this file does not
// take from a configuration file.
type bucketDeployment struct {
	repo    backupengine.TreeRepository
	journal *state.Journal
	srcDir  string
	set     model.BackupSetID
	setUUID string
	domain  model.RepositoryDomainID
}

func (d *bucketDeployment) snapshotSource() backupengine.Source {
	return backupengine.Source{Host: "backupd", User: d.domain.String(), Path: "/" + d.setUUID}
}

// run performs one whole snapshot run through the real driver, reading
// the source through the production reading path.
func (d *bucketDeployment) run(t *testing.T, runID string) (snapshotlifecycle.RunResult, error) {
	t.Helper()

	adapter := rclone.New()

	reader, err := source.New(source.Deps{
		Streamer:   adapter,
		Stater:     adapter,
		Enumerator: adapter,
	}, source.Options{
		Mode:   model.ModeLiveBestEffort,
		Preset: model.PresetConservative,
	})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	src := transport.Source{ID: d.set.String(), Type: "local", Root: d.srcDir}

	runner := &snapshotlifecycle.Runner{Catalog: d.journal}

	return runner.Run(context.Background(), snapshotlifecycle.RunRequest{
		RunID:             runID,
		IdempotencyKey:    runID,
		Set:               d.set,
		SetUUID:           d.setUUID,
		Engine:            model.EngineKopia,
		Domain:            d.domain,
		SourceIdentity:    model.SourceIdentity("bucketsce"),
		Consistency:       model.ModeLiveBestEffort,
		VerificationLevel: model.LevelStructural,
		Source:            d.snapshotSource(),
		Description:       "backupd " + d.set.String(),
		Repository:        d.repo,
		OpenTree: func(ctx context.Context) (snapshotlifecycle.SourceTree, error) {
			tree, err := reader.OpenTree(ctx, src)
			if err != nil {
				return nil, fmt.Errorf("opening the source tree: %w", err)
			}

			return bucketSourceTree{tree: tree}, nil
		},
	})
}

// openTreeRepository creates and opens the bucket repository and asserts
// the one capability an incremental set cannot run without.
func openTreeRepository(t *testing.T, loc backupengine.RepositoryLocation) backupengine.TreeRepository {
	t.Helper()

	ctx := context.Background()
	eng := kopia.New()

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository against MinIO: %v", err)
	}

	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	t.Cleanup(func() { _ = rep.Close(context.Background()) })

	tree, ok := rep.(backupengine.TreeRepository)
	if !ok {
		t.Fatal("this bucket repository cannot store a source tree as one snapshot, so no incremental set can run against it")
	}

	return tree
}

// bucketSourceTree is app.sourceTree's translation onto the lifecycle's
// port.
type bucketSourceTree struct {
	tree *source.Tree
}

func (t bucketSourceTree) Root() backupengine.SourceDir { return t.tree.Root() }
func (t bucketSourceTree) Err() error                   { return t.tree.Err() }
func (t bucketSourceTree) Close() error                 { return t.tree.Close() }

func (t bucketSourceTree) Report() snapshotlifecycle.ScanReport {
	rep := t.tree.Report()

	reason := ""
	if !rep.Complete() && len(rep.Reasons) > 0 {
		reason = rep.Reasons[0]
	}

	return snapshotlifecycle.ScanReport{
		Entries:  rep.Entries,
		Stored:   rep.Stored,
		Skipped:  rep.SkippedSymlink + rep.SkippedSpecial + rep.SkippedExcluded,
		Complete: rep.Complete(),
		Reason:   reason,
	}
}

// applyRetention runs the real snapshot-retention pass over this set.
//
// The set comes out of a config.yaml the product resolved, because that
// is what a Pruner decides about: a hand-built struct would carry a
// retention chain nobody configured, and the chain is the decision.
func (d *bucketDeployment) applyRetention(t *testing.T) ([]snapshotretention.Verdict, error) {
	t.Helper()

	pruner := snapshotretention.Pruner{Catalog: d.journal, Repository: d.repo}

	return pruner.Apply(context.Background(), time.Now(), d.resolvedSet(t))
}

// resolvedSet is this run's backup set as the product resolves it from a
// configuration file.
func (d *bucketDeployment) resolvedSet(t *testing.T) config.BackupSet {
	t.Helper()

	dir := t.TempDir()
	passphrase := filepath.Join(dir, "repo.passphrase")

	if err := os.WriteFile(passphrase, []byte(repositoryPassphrase+"\n"), 0o600); err != nil {
		t.Fatalf("writing a passphrase reference for the configuration: %v", err)
	}

	path := filepath.Join(dir, "config.yaml")

	yaml := fmt.Sprintf(`poll_interval: 15m
state:
  database: %[1]s/backupd.db
retention:
  timezone: UTC
  week_starts_on: monday
  daily_days: 7
  weekly_months: 3
  monthly_months: 12
  protect_last_known_good: true
repository_domains:
  - id: production
    description: Snapshots for this deployment
    isolation: shared
    passphrase:
      file: %[2]s
sources:
  - id: production
    backup_sets:
      - id: uploads-tree
        engine: kopia
        uuid: %[3]s
        repository_domain: production
        source_consistency: live_best_effort
        verification_level: structural
        remote:
          type: local
        remote_path: %[4]s
        stale_after: 30h
`, dir, passphrase, d.setUUID, d.srcDir)

	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the product could not load the configuration this test wrote: %v", err)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("the product refused the configuration this test wrote: %v", err)
	}

	set := cfg.Sources[0].BackupSets[0]
	if set.Engine != model.EngineKopia {
		t.Fatalf("the resolved set runs the %q engine", set.Engine)
	}

	// The identities the catalog rows were written under have to be the
	// ones the retention pass reads by, or it decides about an empty
	// lineage and reports that nothing needs keeping.
	set.ID = d.set

	return set
}

// --- the source tree -----------------------------------------------------

func scenarioRel(i int) string {
	return fmt.Sprintf("dir-%02d/file-%04d.bin", i%s3ScenarioFanout, i)
}

func seedScenarioTree(t *testing.T, dir string) map[string]string {
	t.Helper()

	want := make(map[string]string, s3ScenarioFiles)

	for i := range s3ScenarioFiles {
		rel := scenarioRel(i)
		full := filepath.Join(dir, filepath.FromSlash(rel))

		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(full), err)
		}

		s3VerifyWriteRandom(t, full, s3ScenarioSize)

		data, err := os.ReadFile(full)
		if err != nil {
			t.Fatalf("reading back %s: %v", rel, err)
		}

		want[rel] = hashOf(data)
	}

	return want
}

func scenarioHashes(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		out[filepath.ToSlash(rel)] = hashOf(data)

		return nil
	})
	if err != nil {
		t.Fatalf("hashing the restored tree: %v", err)
	}

	return out
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}
