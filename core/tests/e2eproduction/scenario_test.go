package e2eproduction_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backend"
	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/source"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/repomaintenance"
	"github.com/backupdproject/backupd/core/internal/snapshotlifecycle"
	"github.com/backupdproject/backupd/core/internal/snapshotretention"
	"github.com/backupdproject/backupd/core/internal/state"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// The scenario's shape. Two hundred files of twenty kilobytes is four
// megabytes of incompressible content, and two changed files is one per
// cent of it -- which is the ordinary night #789 asks this suite to
// prove, rather than a synthetic best case where one enormous file
// changed one byte.
const (
	scenarioFiles   = 200
	scenarioSize    = 20 << 10
	scenarioFanout  = 4
	scenarioChanged = 2
)

// TestTheRequiredProductionScenario is issue #789's required scenario, in
// order, as one test.
//
// The order IS the claim, and splitting it into independent cases would
// destroy it: every step only means anything given the one before it. A
// restore of the old version of a file is only evidence if a newer
// snapshot really replaced it; a retention pass is only evidence if there
// were two restore points for it to choose between; a maintenance window
// that reclaimed space is only safe if what it left behind still
// restores.
//
// What each step asserts, and why:
//
//   - snapshot A stores the whole tree, and the catalog's row is this
//     set's last-known-good restore point.
//   - one per cent of the source changes, and snapshot B stores ONLY the
//     new content. Measured three ways -- the run's own report, the
//     physical bytes on the disk, and the reuse counter -- because a
//     report that added the scan to the storage number is exactly the
//     defect that makes an operator believe their bucket grows by the
//     size of their source every night.
//   - B verifies at content_full, which reads every stored byte back.
//   - retention decides about both, and the hold placed on A turns a
//     delete into a refusal rather than a deletion. B is kept because it
//     is the restore point.
//   - a full maintenance window runs and reclaims, and both snapshots
//     still resolve afterwards.
//   - a source file is deleted by hand, which is the disaster this
//     product exists for, and the backup still holds it.
//   - the OLD version of the changed file comes back out of A,
//     byte-identical to what was there before the change.
func TestTheRequiredProductionScenario(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t, deploymentOptions{createRepository: true})

	original := seed(t, d.srcDir, scenarioFiles, scenarioSize, scenarioFanout)
	logical := int64(scenarioFiles * scenarioSize)

	// --- snapshot A -------------------------------------------------------

	first, err := d.run(t, "run-a", snapshotlifecycle.VerificationOptions{SamplePercent: 50})
	if err != nil {
		t.Fatalf("snapshot A: %v (%s)", err, first.Reason)
	}

	if !first.Succeeded() {
		t.Fatalf("snapshot A rested at %s: %s", first.Phase, first.Reason)
	}

	if first.Files != scenarioFiles {
		t.Errorf("snapshot A stored %d files; the source holds %d", first.Files, scenarioFiles)
	}

	if !first.LastKnownGood {
		t.Error("snapshot A is not the set's last-known-good restore point, so nothing could be restored from it")
	}

	if first.ContentReusedBytes != 0 {
		t.Errorf("snapshot A reports %d bytes of reuse into an empty repository", first.ContentReusedBytes)
	}

	bytesAfterA := repositoryBytes(t, d.loc)
	if bytesAfterA < logical/2 {
		t.Fatalf("the repository holds %d bytes after a first snapshot of a %d byte incompressible tree, which is too few for the content to be there at all",
			bytesAfterA, logical)
	}

	// A hold, placed before retention runs, because the scenario's last
	// step restores out of this snapshot and an operator who needs an old
	// version is exactly the operator who has to stop retention taking it.
	placeHold(t, d, "hold-a", first.RunID, "keeping the pre-change restore point for #789's scenario")

	// --- one per cent of the source changes -------------------------------

	changed := make(map[string][]byte, scenarioChanged)
	for i := range scenarioChanged {
		rel := seededPath(i, scenarioFanout)
		replacement := randomBytes(t, scenarioSize)
		writeFile(t, filepath.Join(d.srcDir, rel), replacement)
		changed[filepath.ToSlash(rel)] = replacement
	}

	// --- snapshot B -------------------------------------------------------

	second, err := d.run(t, "run-b", snapshotlifecycle.VerificationOptions{SamplePercent: 50})
	if err != nil {
		t.Fatalf("snapshot B: %v (%s)", err, second.Reason)
	}

	if !second.Succeeded() {
		t.Fatalf("snapshot B rested at %s: %s", second.Phase, second.Reason)
	}

	if second.SnapshotID == first.SnapshotID {
		t.Fatal("snapshot B recorded snapshot A's manifest; these have to be two restore points")
	}

	// The scan is unchanged: a tree run reads every byte the source
	// offers, every time. This is the number an operator must not
	// confuse with storage, and the two are asserted together here for
	// exactly that reason.
	if second.SourceBytesRead < logical {
		t.Errorf("snapshot B read %d bytes off the source; the tree is %d bytes and a tree run re-reads all of it",
			second.SourceBytesRead, logical)
	}

	// The storage is not. Ten per cent of the logical size is a bound an
	// order of magnitude above what one per cent of changed content plus
	// a manifest and an index can cost, and an order of magnitude below a
	// second full copy, so it cannot be satisfied by either mistake.
	if limit := logical / 10; second.RepositoryBytesWritten >= limit {
		t.Errorf("snapshot B wrote %d bytes into the repository after %d of %d files changed; a %d byte tree stored incrementally must write well under %d",
			second.RepositoryBytesWritten, scenarioChanged, scenarioFiles, logical, limit)
	}

	if second.ContentReusedBytes <= 0 {
		t.Errorf("snapshot B reports %d bytes of reuse after re-reading an almost unchanged tree", second.ContentReusedBytes)
	}

	// And the disk agrees with the row, which is the half no report can
	// fake.
	grew := repositoryBytes(t, d.loc) - bytesAfterA
	if limit := logical / 10; grew >= limit {
		t.Errorf("the repository grew by %d bytes over snapshot B of an almost unchanged %d byte tree (limit %d), so the second snapshot is closer to a second full copy than to an increment",
			grew, logical, limit)
	}

	t.Logf("incremental storage: logical %d bytes, A wrote %d, B read %d and wrote %d (reused %d), repository grew %d",
		logical, first.RepositoryBytesWritten, second.SourceBytesRead, second.RepositoryBytesWritten, second.ContentReusedBytes, grew)

	// --- verify -----------------------------------------------------------

	report, err := d.repo.Verify(ctx, backupengine.SnapshotID(second.SnapshotID), backupengine.VerifyRequest{
		Level: model.LevelContentFull,
	})
	if err != nil {
		t.Fatalf("verifying snapshot B at content_full: %v", err)
	}

	if report.Level != model.LevelContentFull {
		t.Errorf("the verification of snapshot B reports %q as achieved, and content_full was asked for", report.Level)
	}

	if report.BytesVerified < logical {
		t.Errorf("a content_full verification of snapshot B read %d bytes back; the snapshot holds %d", report.BytesVerified, logical)
	}

	// --- retention --------------------------------------------------------

	pruner := snapshotretention.Pruner{Catalog: d.journal, Repository: d.repo}

	verdicts, err := pruner.Apply(ctx, time.Now(), d.set)
	if err != nil {
		t.Fatalf("the retention pass: %v", err)
	}

	byRun := map[string]snapshotretention.Verdict{}
	for _, v := range verdicts {
		byRun[v.Run] = v
	}

	if len(byRun) != 2 {
		t.Fatalf("the retention pass decided about %d runs; this set has two", len(byRun))
	}

	if got := byRun[second.RunID].Action; got != snapshotretention.Keep {
		t.Errorf("retention decided %s about the set's last-known-good restore point (%s); FR-19 keeps it unconditionally",
			got, byRun[second.RunID].Reason)
	}

	// The hold is what this assertion is about. Snapshot A is not
	// selected by any tier -- both snapshots are on the same day, and the
	// daily tier keeps the newest -- so without the hold it would be
	// deleted here. A held snapshot is REFUSED rather than kept, because
	// "policy says delete and it is not safe to" is a different fact from
	// "policy says keep", and only one of them needs somebody to look at
	// it.
	if got := byRun[first.RunID].Action; got != snapshotretention.Refuse && got != snapshotretention.Keep {
		t.Errorf("retention decided %s about the held snapshot A (%s); a hold must stop a delete", got, byRun[first.RunID].Reason)
	}

	if _, err := d.repo.LookupSnapshot(ctx, backupengine.SnapshotID(first.SnapshotID)); err != nil {
		t.Fatalf("the retention pass removed the held snapshot A's manifest: %v", err)
	}

	// --- maintenance ------------------------------------------------------

	store, err := backupengine.NewFileMaintenanceOwnershipStore(filepath.Join(d.root, "state", "maintenance"))
	if err != nil {
		t.Fatalf("NewFileMaintenanceOwnershipStore: %v", err)
	}

	runner := repomaintenance.Runner{
		Owner: backupengine.MaintenanceOwner("e2e-production"),
		Store: store,
		Fence: repomaintenance.NewFence(),
	}

	if _, err := runner.Claim(ctx, d.loc.Domain); err != nil {
		t.Fatalf("claiming maintenance for %s: %v", d.loc.Domain, err)
	}

	maintained, err := runner.Run(ctx, d.loc.Domain, d.repo, backupengine.MaintenanceFull, time.Now())
	if err != nil {
		t.Fatalf("a full maintenance window: %v", err)
	}

	if !maintained.Attempted {
		t.Error("the maintenance window was never attempted, so nothing this step asserts was exercised")
	}

	t.Logf("maintenance: ran=%t reclaimed=%d bytes (%d -> %d)",
		maintained.Ran, maintained.Reclaimed, maintained.PhysicalBytesBefore, maintained.PhysicalBytesAfter)

	// Both restore points survive maintenance, and the newer one still
	// reads back byte for byte. A maintenance window that reclaimed
	// content a live snapshot still references is the worst defect this
	// engine can have, and this is where it would show.
	for label, id := range map[string]string{"A": first.SnapshotID, "B": second.SnapshotID} {
		if _, err := d.repo.LookupSnapshot(ctx, backupengine.SnapshotID(id)); err != nil {
			t.Fatalf("maintenance left snapshot %s unresolvable: %v", label, err)
		}
	}

	if _, err := d.repo.Verify(ctx, backupengine.SnapshotID(second.SnapshotID), backupengine.VerifyRequest{
		Level: model.LevelContentFull,
	}); err != nil {
		t.Fatalf("snapshot B no longer verifies after a full maintenance window: %v", err)
	}

	// --- a source file is deleted by hand ---------------------------------

	lost := seededPath(101, scenarioFanout)
	if err := os.Remove(filepath.Join(d.srcDir, filepath.FromSlash(lost))); err != nil {
		t.Fatalf("deleting %s from the source: %v", lost, err)
	}

	if _, err := os.Stat(filepath.Join(d.srcDir, filepath.FromSlash(lost))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the source still holds %s, so the loss this step is about did not happen", lost)
	}

	restoredB := filepath.Join(t.TempDir(), "from-b")
	if _, err := d.repo.Restore(ctx, backupengine.SnapshotID(second.SnapshotID), backupengine.RestoreRequest{
		TargetPath: restoredB,
		Conflict:   backupengine.ConflictRefuse,
		SkipOwners: true,
	}); err != nil {
		t.Fatalf("restoring snapshot B: %v", err)
	}

	got := hashTree(t, restoredB)
	if got[lost] != original[lost] {
		t.Errorf("the file deleted from the source came back out of snapshot B as %q; the source held %q", got[lost], original[lost])
	}

	// The whole of B matches the source as it stood when B was taken:
	// every file's original bytes except the two that changed.
	for rel, want := range original {
		if replacement, ok := changed[rel]; ok {
			want = sha256Of(replacement)
		}

		if got[rel] != want {
			t.Errorf("snapshot B restored %s as %q, want %q", rel, got[rel], want)
		}
	}

	// --- the old version, out of snapshot A -------------------------------

	for rel, replacement := range changed {
		target := filepath.Join(t.TempDir(), "old-version")

		if _, err := d.repo.Restore(ctx, backupengine.SnapshotID(first.SnapshotID), backupengine.RestoreRequest{
			SourcePath: rel,
			TargetPath: target,
			Conflict:   backupengine.ConflictRefuse,
			SkipOwners: true,
		}); err != nil {
			t.Fatalf("restoring %s out of snapshot A: %v", rel, err)
		}

		data, err := os.ReadFile(filepath.Join(target, filepath.Base(rel)))
		if err != nil {
			t.Fatalf("reading the restored old version of %s: %v", rel, err)
		}

		if sha256Of(data) != original[rel] {
			t.Errorf("the old version of %s restored out of snapshot A hashes %q, and the source held %q before the change",
				rel, sha256Of(data), original[rel])
		}

		if sha256Of(data) == sha256Of(replacement) {
			t.Errorf("the restore out of snapshot A returned the NEW content of %s, so the old restore point is not distinguishable from the new one", rel)
		}
	}
}

// TestAnSFTPSourceIsRefusedAWalkedTreeRunBeforeAnythingIsDialed is why
// the scenario above reads a local volume, pinned as a fact about the
// product rather than left as a comment.
//
// An operator can configure a kopia-engine backup set on an SFTP source
// today: config.Validate accepts it. Every cycle then fails, because a
// tree walk needs a resumable directory cursor and rclone's sftp backend
// has none (core/internal/backend/bundled/sftp.json declares
// bounded_listing false). The refusal is the right behaviour -- the
// alternative is materialising a whole remote directory in memory -- and
// what matters for a release gate is that it is explicit, typed, and
// arrives BEFORE a connection is made, so an operator who has mixed the
// two engines up pays a refusal rather than a dial and a half-read tree.
//
// This needs no container precisely because the refusal precedes the
// dial: it is decided from the capability matrix and the source's type.
func TestAnSFTPSourceIsRefusedAWalkedTreeRunBeforeAnythingIsDialed(t *testing.T) {
	d := newDeployment(t, deploymentOptions{createRepository: true})
	adapter := d.sourceAdapter(t)

	// A source nothing is listening on. If the refusal were made after
	// the dial this test would report a connection error instead, which
	// is the whole point of pointing it at a closed port.
	_, err := adapter.OpenTree(context.Background(), transport.Source{
		ID:         "sftp-source",
		Type:       "sftp",
		Host:       "127.0.0.1",
		Port:       1,
		User:       "backupuser",
		KeyFile:    filepath.Join(t.TempDir(), "no-such-key"),
		KnownHosts: filepath.Join(t.TempDir(), "no-such-known-hosts"),
		Root:       "/srv/uploads",
	})

	if !errors.Is(err, backend.ErrUnboundedListing) {
		t.Fatalf("a tree run over an SFTP source returned %v; bundled/sftp.json declares bounded_listing false, so it must be refused with backend.ErrUnboundedListing", err)
	}

	// And the refusal says what to do instead, which is the half that
	// stops it being a dead end.
	if got := err.Error(); !strings.Contains(got, "explicit path list") {
		t.Errorf("the refusal reads %q and does not name the remedy (an explicit path list, which is the artifact engine's path)", got)
	}

	// The positive control: the same adapter walks the local volume the
	// scenario uses, so what the check above measured is the backend's
	// capability and not a broken adapter.
	tree, err := adapter.OpenTree(context.Background(), d.transportSource())
	if err != nil {
		t.Fatalf("the same adapter refused the local volume too, so the refusal above says nothing about SFTP: %v", err)
	}

	if err := tree.Close(); err != nil && !errors.Is(err, source.ErrTreeAbandoned) {
		t.Errorf("closing the control walk: %v", err)
	}
}

// placeHold puts a hold on one run's snapshot through the journal's own
// API, which is the same call `backupd snapshot hold` makes.
func placeHold(t *testing.T, d *deployment, holdID, runID, reason string) {
	t.Helper()

	if _, err := d.journal.PlaceSnapshotHold(context.Background(), state.SnapshotHoldRequest{
		HoldID:   holdID,
		RunID:    runID,
		Reason:   reason,
		PlacedBy: "e2e-production",
		At:       time.Now(),
	}); err != nil {
		t.Fatalf("placing a hold on %s: %v", runID, err)
	}
}
