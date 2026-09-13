// The behavioural half of "retention removes manifests and never storage",
// asked of a real repository rather than of a fake that could only answer
// with what this package already believes.
//
// Three questions are asked of one pass, and none of them can be asked of
// a stub. Did the approved manifest actually stop resolving? Is the held
// snapshot still RESTORABLE -- not merely still listed, but readable back
// into files -- after a pass that deleted its neighbour? And did anything
// disappear from the repository's own storage?
//
// The last one is the criterion the issue states as "repository pack files
// are never directly pruned by backupd", and it is asserted as a
// directory-level fact: every blob file present before the pass is present
// after it. Nothing weaker would do. A pass that deleted a manifest AND
// the packs its content lived in would satisfy every assertion in
// prune_test.go, and would also have deleted the content of every other
// snapshot that shared it.
//
// This test uses the real engine adapter, which is allowed here for the
// same reason internal/backupengine's own boundary tests allow it: what is
// quarantined is the VENDOR's import, and this file imports this product's
// adapter package like any other caller would.

package snapshotretention_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/kopia"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/secretref"
	"github.com/backupdproject/backupd/core/internal/snapshotretention"
	"github.com/backupdproject/backupd/core/internal/state"
)

// heldFileName and heldFileBody are what the restore has to hand back. A
// restore that produced an empty tree, or a tree of the right shape with
// nothing in the files, would pass a "did it error" assertion.
const (
	heldFileName = "ledger.txt"
	heldFileBody = "the row a held snapshot exists to preserve\n"
)

// localRepository creates and opens a real repository on a local
// filesystem, and returns it with the directory its blobs actually live
// in.
//
// The blob directory comes from the product's own ReservedLocalDir rather
// than being composed here: a test that guessed where the repository is
// would keep passing while looking at an empty directory.
func localRepository(t *testing.T) (backupengine.Repository, string) {
	t.Helper()

	ctx := context.Background()
	root := t.TempDir()

	domain, err := model.NewRepositoryDomainID("retention-blobs")
	if err != nil {
		t.Fatalf("NewRepositoryDomainID: %v", err)
	}

	secret := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(secret, []byte("retention-test-passphrase-not-a-secret\n"), 0o600); err != nil {
		t.Fatalf("writing the passphrase file: %v", err)
	}

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

	blobs, err := backupengine.ReservedLocalDir(loc.Root, loc.Domain)
	if err != nil {
		t.Fatalf("ReservedLocalDir: %v", err)
	}
	return rep, blobs
}

// blobFiles is every file under the repository's storage, relative to it
// and sorted: the inventory a pass must not shrink.
func blobFiles(t *testing.T, dir string) []string {
	t.Helper()

	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository storage at %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

// TestRetentionRemovesAManifestAndNotOneByteOfRepositoryStorage is the
// end-to-end claim, against the engine this product actually ships.
func TestRetentionRemovesAManifestAndNotOneByteOfRepositoryStorage(t *testing.T) {
	ctx := context.Background()
	rep, blobs := localRepository(t)

	// One source, snapshotted three times. One source rather than three
	// because sharing content is the case that matters: if a pass reclaimed
	// storage, the packs it freed would be packs the surviving snapshots
	// are still made of.
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, heldFileName), []byte(heldFileBody), 0o600); err != nil {
		t.Fatalf("writing the source file: %v", err)
	}

	source := backupengine.Source{Host: "test-host", User: "test-user", Path: srcDir}
	var manifests []backupengine.SnapshotID
	for i := range 3 {
		if err := os.WriteFile(filepath.Join(srcDir, "changing.txt"), []byte{byte('a' + i)}, 0o600); err != nil {
			t.Fatalf("changing the source: %v", err)
		}
		info, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{Source: source, Description: "retention fixture"})
		if err != nil {
			t.Fatalf("Snapshot %d: %v", i, err)
		}
		manifests = append(manifests, info.ID)
	}

	held, doomed, fresh := manifests[0], manifests[1], manifests[2]

	before := blobFiles(t, blobs)
	if len(before) < 3 {
		t.Fatalf("the repository holds %d blob files after three snapshots, so this test is not looking at a real repository", len(before))
	}

	// The catalog's account of those three runs: two of them old enough
	// that no tier keeps them, one fresh so the set has a restore point.
	j := openCatalog(t)
	set := mustSet(t, "production", "ledger")
	bs := backupSet(t, set, "uuid-blobs", dailyOnly())

	record(t, j, set, bs.UUID, runSpec{runID: "run-held", snapshotID: string(held), startedAt: pruneNow.AddDate(0, 0, -40)})
	record(t, j, set, bs.UUID, runSpec{runID: "run-doomed", snapshotID: string(doomed), startedAt: pruneNow.AddDate(0, 0, -39)})
	record(t, j, set, bs.UUID, runSpec{runID: "run-fresh", snapshotID: string(fresh), startedAt: pruneNow.Add(-2 * time.Hour)})

	if _, err := j.PlaceSnapshotHold(ctx, state.SnapshotHoldRequest{
		HoldID: "hold-blobs", RunID: "run-held", Reason: "kept for the audit", PlacedBy: "ops", At: pruneNow.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("PlaceSnapshotHold: %v", err)
	}

	applied, err := snapshotretention.Pruner{Catalog: j, Repository: rep}.Apply(ctx, pruneNow, bs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := deletes(applied); len(got) != 1 || got[0] != string(doomed) {
		t.Fatalf("the pass deleted %v, want only the unheld expired snapshot %s", got, doomed)
	}

	// The approved manifest is really gone from the repository, not merely
	// reported as deleted by the code that decided to delete it.
	if _, err := rep.LookupSnapshot(ctx, doomed); !errors.Is(err, backupengine.ErrSnapshotNotFound) {
		t.Errorf("LookupSnapshot(%s) = %v, want ErrSnapshotNotFound after the pass removed it", doomed, err)
	}

	// The held one is still referenced AND still restorable, which is the
	// claim a hold actually makes. Listing it would not be enough: a
	// manifest whose content had been reclaimed still lists.
	if _, err := rep.LookupSnapshot(ctx, held); err != nil {
		t.Fatalf("LookupSnapshot(%s) after the pass: %v; a held snapshot must stay referenced", held, err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := rep.Restore(ctx, held, backupengine.RestoreRequest{TargetPath: target, SkipOwners: true}); err != nil {
		t.Fatalf("Restore(%s): %v; a held snapshot must stay restorable across a retention pass", held, err)
	}
	body, err := os.ReadFile(filepath.Join(target, heldFileName))
	if err != nil {
		t.Fatalf("reading the restored file: %v", err)
	}
	if string(body) != heldFileBody {
		t.Errorf("the restored held snapshot contains %q, want %q", body, heldFileBody)
	}

	// And nothing left the repository's storage. Additions are expected --
	// removing a manifest is itself a write -- so this compares in one
	// direction only.
	after := map[string]bool{}
	for _, name := range blobFiles(t, blobs) {
		after[name] = true
	}
	for _, name := range before {
		if !after[name] {
			t.Errorf("the repository storage lost %s during a retention pass; retention removes manifests, and only repository maintenance (#786) may reclaim a pack, an index or any other blob", name)
		}
	}
}
