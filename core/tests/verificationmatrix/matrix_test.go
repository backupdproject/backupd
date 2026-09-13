// Package verificationmatrix_test is EPIC K's verification doctrine held
// end to end: the real run driver, on the real catalog, against a real
// repository.
//
// Every other test of this material fakes one side of it, for good
// reasons. internal/snapshotlifecycle fakes the repository, because what
// a run needs from one is four calls and a real one would turn a state
// machine test into a repository format test.
// internal/backupengine/kopia fakes the catalog, because the ladder is a
// property of the engine and a journal would only slow it down. Both are
// right, and between them they leave exactly one claim unproven: that a
// row in the catalog saying "verified at this level, and this is the
// last-known-good restore point" was produced by a repository that
// really was checked to that depth.
//
// This package proves that one claim and nothing else. Its tests are
// slow by the standards of a unit test and there are deliberately few of
// them: the four levels through the driver, physical content reuse
// measured across two real runs, and damage to a real repository
// failing a real run without taking the previous restore point away.
package verificationmatrix_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/kopia"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/secretref"
	"github.com/backupdproject/backupd/core/internal/snapshotlifecycle"
	"github.com/backupdproject/backupd/core/internal/state"
)

const testPassphrase = "verification-matrix-passphrase-not-a-secret"

// payloadSize is per file, and it is incompressible random data so that
// the reuse measurement below cannot be satisfied by compression. Four
// files of a megabyte is enough for "the second run wrote almost
// nothing" to be unambiguous and small enough to stay a fast test.
const (
	payloadSize = 1 << 20
	payloadFile = 4
)

// --- the fixture --------------------------------------------------------

// fixture is one deployment: a source tree on disk, a repository, and a
// catalog.
type fixture struct {
	srcDir  string
	loc     backupengine.RepositoryLocation
	repo    backupengine.TreeRepository
	journal *state.Journal
	set     model.BackupSetID
	source  backupengine.Source
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	ctx := context.Background()
	root := t.TempDir()
	srcDir := filepath.Join(root, "source")

	for i := range payloadFile {
		payload := make([]byte, payloadSize)
		if _, err := rand.Read(payload); err != nil {
			t.Fatalf("generating payload %d: %v", i, err)
		}

		writeFile(t, filepath.Join(srcDir, fmt.Sprintf("dir-%d", i%2), fmt.Sprintf("file-%d.bin", i)), payload)
	}

	domain, err := model.NewRepositoryDomainID("production")
	if err != nil {
		t.Fatalf("NewRepositoryDomainID: %v", err)
	}

	passphrase := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(passphrase, []byte(testPassphrase+"\n"), 0o600); err != nil {
		t.Fatalf("writing the passphrase file: %v", err)
	}

	loc := backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     domain,
		Root:       root,
		StateDir:   filepath.Join(t.TempDir(), "state"),
		Passphrase: secretref.Ref{File: passphrase},
	}

	eng := kopia.New()
	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	opened, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	// The tree capability is asserted rather than assumed, exactly as
	// the production cycle asserts it: a repository that cannot store a
	// whole source tree as one snapshot cannot run an incremental set,
	// and this suite would otherwise be testing the per-object interim.
	repo, ok := opened.(backupengine.TreeRepository)
	if !ok {
		t.Fatal("this repository cannot store a source tree as one snapshot")
	}

	t.Cleanup(func() {
		if err := repo.Close(context.Background()); err != nil {
			t.Errorf("closing the repository: %v", err)
		}
	})

	journal, err := state.Open(ctx, filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("opening the journal: %v", err)
	}

	t.Cleanup(func() { journal.Close() }) //nolint:errcheck // a closed test database has nothing to report.

	set, err := model.NewBackupSetID("production", "postgres")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	return &fixture{
		srcDir:  srcDir,
		loc:     loc,
		repo:    repo,
		journal: journal,
		set:     set,
		source:  backupengine.Source{Host: "nas-01", User: "backupd", Path: srcDir},
	}
}

// run performs one whole run of the set at a level, through the real
// driver.
func (f *fixture) run(t *testing.T, runID string, level model.VerificationLevel, opts snapshotlifecycle.VerificationOptions) (snapshotlifecycle.RunResult, error) {
	t.Helper()

	runner := &snapshotlifecycle.Runner{Catalog: f.journal}

	return runner.Run(context.Background(), snapshotlifecycle.RunRequest{
		RunID:             runID,
		IdempotencyKey:    runID,
		Set:               f.set,
		Engine:            model.EngineKopia,
		Domain:            f.loc.Domain,
		SourceIdentity:    model.SourceIdentity("ab12cd34"),
		Consistency:       model.ModeLiveBestEffort,
		VerificationLevel: level,
		Verification:      opts,
		Source:            f.source,
		Description:       "the verification matrix",
		Repository:        f.repo,
		OpenTree:          func(context.Context) (snapshotlifecycle.SourceTree, error) { return localTree(f.srcDir), nil },
	})
}

// --- the tests ----------------------------------------------------------

// TestEveryLevelIsProvedByTheEngineAndRecordedByTheCatalog is the join
// this package exists for: a row saying content_full was produced by a
// repository that really read every byte, and a row saying restore_drill
// was produced by a restore that really happened onto a disk.
func TestEveryLevelIsProvedByTheEngineAndRecordedByTheCatalog(t *testing.T) {
	t.Parallel()

	for i, level := range model.VerificationLevels() {
		t.Run(string(level), func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			opts := snapshotlifecycle.VerificationOptions{
				SamplePercent: 50,
				DrillDir:      filepath.Join(t.TempDir(), "drills"),
			}

			res, err := f.run(t, fmt.Sprintf("run-%d", i), level, opts)
			if err != nil {
				t.Fatalf("a run configured for %s: %v (%s)", level, err, res.Reason)
			}

			if !res.Succeeded() {
				t.Fatalf("the run rested at %s: %s", res.Phase, res.Reason)
			}

			if res.VerificationLevel != level {
				t.Errorf("the row records %q as configured, and the set asked for %q", res.VerificationLevel, level)
			}

			if res.VerificationAchieved != level {
				t.Errorf("the row records %q as proved, and the run asked the engine for %q", res.VerificationAchieved, level)
			}

			if !res.LastKnownGood {
				t.Error("a run that proved its configured level is not the set's last-known-good restore point")
			}

			// The snapshot the row names is really in the repository,
			// which is what makes the row a restore point rather than a
			// claim about one.
			if _, err := f.repo.LookupSnapshot(context.Background(), backupengine.SnapshotID(res.SnapshotID)); err != nil {
				t.Errorf("the snapshot this run recorded is not in the repository: %v", err)
			}
		})
	}
}

// TestASecondRunReusesPhysicalContentRatherThanStoringItAgain is EPIC
// K's incrementality claim, measured rather than asserted, at the level
// an operator reads it: the catalog row.
//
// The numbers that matter are three different numbers, and the test
// checks the relationship between them rather than any one of them: the
// second run SCANS and READS the whole tree again, and WRITES almost
// nothing, because the repository already holds the content. A row that
// reported the scan as storage would say this deployment's bucket grows
// by the size of the source every night.
func TestASecondRunReusesPhysicalContentRatherThanStoringItAgain(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	first, err := f.run(t, "run-1", model.LevelContentFull, snapshotlifecycle.VerificationOptions{})
	if err != nil {
		t.Fatalf("the first run: %v (%s)", err, first.Reason)
	}

	if first.ContentReusedBytes != 0 {
		t.Errorf("the first run reports %d bytes of reuse into an empty repository", first.ContentReusedBytes)
	}

	before := repositoryBytes(t, f.loc)

	// One file changes. Everything else is byte-identical, which is the
	// ordinary night this measurement is about.
	writeFile(t, filepath.Join(f.srcDir, "dir-0", "file-0.bin"), []byte("a small replacement\n"))

	second, err := f.run(t, "run-2", model.LevelContentFull, snapshotlifecycle.VerificationOptions{})
	if err != nil {
		t.Fatalf("the second run: %v (%s)", err, second.Reason)
	}

	if second.SnapshotID == first.SnapshotID {
		t.Fatal("the second run recorded the first run's snapshot; these are two restore points")
	}

	if second.SourceBytesRead < payloadSize {
		t.Errorf("the second run reports reading %d bytes off the source; it re-read a tree of about %d",
			second.SourceBytesRead, payloadFile*payloadSize)
	}

	// The claim, as a ratio rather than an absolute: whatever the pack
	// and index overhead of a second manifest turns out to be, it cannot
	// be anywhere near a second copy of the tree.
	if limit := int64(payloadFile*payloadSize) / 4; second.RepositoryBytesWritten >= limit {
		t.Errorf("the second run wrote %d bytes into the repository, which is not far off a second copy of the %d byte tree (limit %d)",
			second.RepositoryBytesWritten, payloadFile*payloadSize, limit)
	}

	if second.ContentReusedBytes <= 0 {
		t.Errorf("the second run reports %d bytes of reuse after re-reading an almost unchanged tree", second.ContentReusedBytes)
	}

	// And the storage agrees with the row, which is the half no report
	// can fake: the bytes on the disk grew by roughly what was written,
	// not by the size of the source.
	grew := repositoryBytes(t, f.loc) - before
	if limit := int64(payloadFile*payloadSize) / 2; grew >= limit {
		t.Errorf("the repository grew by %d bytes over the second run of an almost unchanged %d byte tree (limit %d)",
			grew, payloadFile*payloadSize, limit)
	}
}

// TestDamageToTheRepositoryFailsTheRunAndKeepsThePreviousRestorePoint is
// the negative half of the doctrine, with real damage.
//
// The failure mode being ruled out is the one that makes a backup
// product worthless: a newer run that could not be proven replacing an
// older one that was. Nothing here is faked -- a pack blob is deleted
// from the storage, the verification really fails, and the older row
// keeps the flag.
func TestDamageToTheRepositoryFailsTheRunAndKeepsThePreviousRestorePoint(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newFixture(t)

	good, err := f.run(t, "run-1", model.LevelContentFull, snapshotlifecycle.VerificationOptions{})
	if err != nil {
		t.Fatalf("the first run: %v (%s)", err, good.Reason)
	}

	// The repository is closed and reopened around the damage so that
	// the failing run reads the storage rather than this process's
	// memory of it.
	if err := f.repo.Close(ctx); err != nil {
		t.Fatalf("closing before damage: %v", err)
	}

	removeLargestBlob(t, repositoryDir(t, f.loc))

	reopened, err := kopia.New().OpenRepository(ctx, f.loc)
	if err != nil {
		t.Fatalf("reopening the damaged repository: %v", err)
	}

	damaged, ok := reopened.(backupengine.TreeRepository)
	if !ok {
		t.Fatal("the reopened repository cannot store a source tree")
	}

	f.repo = damaged

	writeFile(t, filepath.Join(f.srcDir, "dir-1", "file-3.bin"), []byte("a change, so the second run has something to store\n"))

	second, err := f.run(t, "run-2", model.LevelContentFull, snapshotlifecycle.VerificationOptions{})
	if err == nil {
		t.Fatal("a run verified a repository that is missing a pack blob")
	}

	if second.Succeeded() {
		t.Errorf("the run rested at %s", second.Phase)
	}

	if second.VerificationAchieved != "" {
		t.Errorf("the failed run recorded %q as proved", second.VerificationAchieved)
	}

	if second.Reason == "" || !strings.Contains(second.Reason, "verification") {
		t.Errorf("the row's reason is %q; an operator reading it has to learn that the verification is what failed", second.Reason)
	}

	lkg, err := f.journal.LastKnownGoodSnapshot(ctx, f.set)
	if err != nil {
		t.Fatalf("reading last-known-good: %v", err)
	}

	if lkg.RunID != good.RunID {
		t.Errorf("last-known-good is run %q; the only run that was ever proved is %q", lkg.RunID, good.RunID)
	}

	// And the damaged repository still holds both manifests: nothing in
	// this path deletes a snapshot, including the one that could not be
	// verified.
	snaps, err := damaged.ListSnapshots(ctx, f.source)
	if err != nil {
		t.Fatalf("listing snapshots: %v", err)
	}

	if len(snaps) != 2 {
		t.Errorf("the repository holds %d snapshot(s) after a failed run; a failure withdraws a claim, never a snapshot", len(snaps))
	}
}

// TestARestoreDrillThroughTheDriverProducesTheSourceTree is the top rung
// with the evidence checked by the test rather than by the engine.
//
// The engine's own drill compares what it restored against what the
// repository holds, which is the check that can be made from inside. The
// check that cannot is this one: that what came out equals the tree that
// went IN, hashed from the original files on disk.
func TestARestoreDrillThroughTheDriverProducesTheSourceTree(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	drills := filepath.Join(t.TempDir(), "drills")

	// A passing drill discards its own scratch tree, by design, so this
	// test cannot inspect it. What it can do is the stronger thing: run
	// the drill, then restore the snapshot the drill proved and compare
	// that against the source tree on disk.
	res, err := f.run(t, "run-1", model.LevelRestoreDrill, snapshotlifecycle.VerificationOptions{DrillDir: drills})
	if err != nil {
		t.Fatalf("the drill run: %v (%s)", err, res.Reason)
	}

	if res.VerificationAchieved != model.LevelRestoreDrill {
		t.Fatalf("the row records %q as proved", res.VerificationAchieved)
	}

	// A passing drill leaves nothing behind: the scratch tree is a
	// second copy of somebody's data and the run that needed it has
	// succeeded.
	if entries, err := os.ReadDir(drills); err == nil && len(entries) != 0 {
		t.Errorf("the drill directory still holds %d entry/entries after a passing drill", len(entries))
	}

	// The independent check: restore the snapshot the drill proved, and
	// compare it with the source tree this test wrote.
	target := filepath.Join(t.TempDir(), "restored")

	if _, err := f.repo.Restore(context.Background(), backupengine.SnapshotID(res.SnapshotID), backupengine.RestoreRequest{
		TargetPath: target,
		SkipOwners: true,
	}); err != nil {
		t.Fatalf("restoring the drilled snapshot: %v", err)
	}

	want, got := hashTree(t, f.srcDir), hashTree(t, target)
	if len(want) != len(got) {
		t.Fatalf("the restore produced %d file(s), the source had %d", len(got), len(want))
	}

	for name, hash := range want {
		if got[name] != hash {
			t.Errorf("%s restored with hash %s, want %s", name, got[name], hash)
		}
	}
}

// TestASetConfiguredForADrillWithNowhereToRunItNeverStartsARun is the
// refusal at the level an operator meets it: a misconfiguration costs a
// refused run, not a whole source read followed by a verification
// failure.
func TestASetConfiguredForADrillWithNowhereToRunItNeverStartsARun(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	if _, err := f.run(t, "run-1", model.LevelRestoreDrill, snapshotlifecycle.VerificationOptions{}); err == nil {
		t.Fatal("a set configured for restore drills ran with nowhere to restore to")
	}

	runs, err := f.journal.ListSnapshotRuns(context.Background(), f.set, 10)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}

	if len(runs) != 0 {
		t.Errorf("the refusal left %d row(s) behind", len(runs))
	}

	if snaps, err := f.repo.ListSnapshots(context.Background(), f.source); err != nil || len(snaps) != 0 {
		t.Errorf("the refused run stored %d snapshot(s) (err %v)", len(snaps), err)
	}
}

// TestACrashDuringARestoreDrillIsRecoveredRatherThanFailed is the
// crash-during-restore boundary with a real repository, a real snapshot
// and a real restore, which is the only place the defect it pins is
// visible.
//
// The story is one power cut. A run configured for restore drills commits
// its manifest, starts restoring the snapshot into its drill directory
// and the machine goes away mid-file. On the way back up the
// reconciliation pass has to prove that snapshot to the level the row was
// admitted under -- so it drills again, with Overwrite=false, and the
// first implementation pointed it at the directory the crash had filled
// with partial files. The restore failed on the first one, and an INTACT
// committed snapshot was moved to FAILED: a restore point thrown away
// because of the wreckage of the attempt that crashed.
//
// Nothing here is faked. The snapshot is real, the partial restore on the
// disk is real, and the assertion is the operator's: after the restart
// the row is SUCCESS and the set has a restore point.
func TestACrashDuringARestoreDrillIsRecoveredRatherThanFailed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newFixture(t)

	// The manifest the crashed run committed, written the way the driver
	// writes it so the row below names a snapshot that really is there.
	info, err := f.repo.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{
		Source:      f.source,
		Root:        localTree(f.srcDir).Root(),
		Description: "the run that died during its drill",
		Tags: map[string]string{
			backupengine.TagKeyBackupSet: f.set.String(),
			backupengine.TagKeyDomain:    f.loc.Domain.String(),
		},
	})
	if err != nil {
		t.Fatalf("storing the snapshot the crashed run committed: %v", err)
	}

	const runID = "run-1"

	seedCrashedDrill(t, f, runID, string(info.ID))

	// What the crash left on the disk: the drill's directory, holding
	// the beginning of one of the snapshot's own files. The partial
	// content matters -- a restore with Overwrite=false refuses a file
	// that exists, whatever is in it.
	drills := filepath.Join(t.TempDir(), "drills")
	abandoned := filepath.Join(drills, "restore-drill-"+runID)
	writeFile(t, filepath.Join(abandoned, "dir-0", "file-0.bin"), []byte("the first few bytes of a restore that never finished"))

	rec := &snapshotlifecycle.Reconciler{
		Catalog:      f.journal,
		Verification: snapshotlifecycle.VerificationOptions{DrillDir: drills},
	}

	report, err := rec.Reconcile(ctx, snapshotlifecycle.ReconcileRequest{
		Set:            f.set,
		Domain:         f.loc.Domain,
		Source:         f.source,
		SourceIdentity: model.SourceIdentity("ab12cd34"),
		Repository:     f.repo,
	})
	if err != nil {
		t.Fatalf("reconciling after the crash: %v", err)
	}

	run, err := f.journal.GetSnapshotRun(ctx, runID)
	if err != nil {
		t.Fatalf("reading the recovered row: %v", err)
	}

	if run.Phase != state.PhaseSuccess {
		t.Fatalf("the interrupted drill run ended at %s with verdicts %+v; its snapshot is intact and the only thing in the way was a partial restore on the disk",
			run.Phase, report.Verdicts)
	}

	// The recovery proved the rung the row was admitted under, by really
	// restoring the snapshot somewhere else and hashing it.
	if got := model.VerificationLevel(run.VerificationLevelAchieved); got != model.LevelRestoreDrill {
		t.Errorf("the recovered row records %q as proved; the row was admitted at %q", got, model.LevelRestoreDrill)
	}

	lkg, err := f.journal.LastKnownGoodSnapshot(ctx, f.set)
	if err != nil {
		t.Fatalf("reading last-known-good after the recovery: %v", err)
	}

	if lkg.RunID != runID {
		t.Errorf("the set's restore point is run %q; the recovered run is %q", lkg.RunID, runID)
	}

	// The proven snapshot needs no scratch trees: the fresh attempt and
	// the wreckage of the crashed one both go.
	if entries, err := os.ReadDir(drills); err == nil && len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}

		t.Errorf("the drill directory still holds %v after a recovery that proved the snapshot; every one of those is a copy of somebody's data", names)
	}
}

// seedCrashedDrill puts a row on the catalog exactly where a crash during
// a restore drill leaves one: configured for a drill, manifest committed,
// nothing verified.
//
// It writes through the journal's own API rather than SQL, walking the
// same edges a live run walks, so the row it leaves is one the state
// machine could really have produced.
func seedCrashedDrill(t *testing.T, f *fixture, runID, snapshotID string) {
	t.Helper()

	ctx := context.Background()
	at := time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)

	if _, err := f.journal.BeginSnapshotRun(ctx, state.SnapshotRunRequest{
		RunID:             runID,
		IdempotencyKey:    runID,
		Set:               f.set,
		Engine:            model.EngineKopia.String(),
		Domain:            f.loc.Domain.String(),
		SourceIdentity:    "ab12cd34",
		ConsistencyMode:   string(model.ModeLiveBestEffort),
		VerificationLevel: string(model.LevelRestoreDrill),
		StartedAt:         at,
	}); err != nil {
		t.Fatalf("seeding the crashed run: %v", err)
	}

	phases := []state.SnapshotPhase{state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted}

	for i, phase := range phases {
		upd := state.SnapshotRunUpdate{At: at.Add(time.Duration(i+1) * time.Second)}
		if phase == state.PhaseManifestCommitted {
			upd.SnapshotID = &snapshotID
		}

		if err := f.journal.AdvanceSnapshotRun(ctx, runID, phase, upd); err != nil {
			t.Fatalf("seeding %s: %v", phase, err)
		}
	}
}

// --- a source tree on the local disk -------------------------------------

// localTree is the smallest honest SourceTree: a real directory, walked
// lazily, reporting a complete pass.
//
// The production reading side (internal/backupengine/source) is a
// transport session with consistency checks, a read window and a
// capability matrix, and every one of those is somebody else's test. What
// this package needs is a tree whose bytes are real and whose census is
// truthful, so that the numbers the run reports are numbers about actual
// content.
type localSourceTree struct {
	root    string
	entries int64
	stored  int64
}

func localTree(root string) *localSourceTree { return &localSourceTree{root: root} }

func (t *localSourceTree) Root() backupengine.SourceDir { return &localDir{tree: t, path: t.root} }

func (t *localSourceTree) Report() snapshotlifecycle.ScanReport {
	return snapshotlifecycle.ScanReport{Entries: t.entries, Stored: t.stored, Complete: true}
}

func (t *localSourceTree) Err() error   { return nil }
func (t *localSourceTree) Close() error { return nil }

type localDir struct {
	tree *localSourceTree
	path string
}

func (d *localDir) Open(context.Context) (backupengine.SourceDirIterator, error) {
	entries, err := os.ReadDir(d.path)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", d.path, err)
	}

	return &localIterator{dir: d, entries: entries}, nil
}

type localIterator struct {
	dir     *localDir
	entries []fs.DirEntry
	at      int
}

func (i *localIterator) Next(context.Context) (backupengine.SourceEntry, bool, error) {
	if i.at >= len(i.entries) {
		return backupengine.SourceEntry{}, false, nil
	}

	e := i.entries[i.at]
	i.at++

	info, err := e.Info()
	if err != nil {
		return backupengine.SourceEntry{}, false, fmt.Errorf("stat %s: %w", e.Name(), err)
	}

	i.dir.tree.entries++

	path := filepath.Join(i.dir.path, e.Name())

	if e.IsDir() {
		return backupengine.SourceEntry{
			Name:    e.Name(),
			ModTime: info.ModTime(),
			Dir:     &localDir{tree: i.dir.tree, path: path},
		}, true, nil
	}

	i.dir.tree.stored++

	return backupengine.SourceEntry{
		Name:    e.Name(),
		ModTime: info.ModTime(),
		Size:    info.Size(),
		Stream:  &localStream{path: path, modTime: info.ModTime()},
	}, true, nil
}

func (i *localIterator) Close() error { return nil }

type localStream struct {
	path    string
	modTime time.Time
}

func (s *localStream) ModTime() time.Time { return s.modTime }

func (s *localStream) Open(context.Context) (io.ReadCloser, error) {
	f, err := os.Open(s.path) //nolint:gosec // the path came from this test's own temporary directory.
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", s.path, err)
	}

	return f, nil
}

// --- measurement helpers -------------------------------------------------

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// repositoryDir is where a local repository's blobs actually live,
// resolved through the product's own function rather than composed a
// second time here.
func repositoryDir(t *testing.T, loc backupengine.RepositoryLocation) string {
	t.Helper()

	dir, err := backupengine.ReservedLocalDir(loc.Root, loc.Domain)
	if err != nil {
		t.Fatalf("ReservedLocalDir: %v", err)
	}

	return dir
}

// repositoryBytes is the physical size of the repository, which is the
// measurement no report can fake.
func repositoryBytes(t *testing.T, loc backupengine.RepositoryLocation) int64 {
	t.Helper()

	var total int64

	err := filepath.WalkDir(repositoryDir(t, loc), func(path string, d fs.DirEntry, err error) error {
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
		t.Fatalf("measuring the repository: %v", err)
	}

	return total
}

// removeLargestBlob deletes the biggest blob in the repository, which is
// a pack blob holding file content rather than an index or a format blob.
func removeLargestBlob(t *testing.T, dir string) {
	t.Helper()

	var (
		biggest string
		size    int64
	)

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

		if info.Mode().IsRegular() && info.Size() > size {
			biggest, size = path, info.Size()
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}

	if biggest == "" {
		t.Fatalf("no blobs found under %s", dir)
	}

	if err := os.Remove(biggest); err != nil {
		t.Fatalf("removing %s: %v", biggest, err)
	}
}

// hashTree maps every regular file's slash-separated path relative to
// dir to the hex SHA-256 of its contents.
func hashTree(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}

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

		sum, err := hashFile(path)
		if err != nil {
			return err
		}

		out[filepath.ToSlash(rel)] = sum

		return nil
	})
	if err != nil {
		t.Fatalf("hashing %s: %v", dir, err)
	}

	return out
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // test-owned temporary paths.
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}

	defer f.Close() //nolint:errcheck // a read-only file has nothing to report on close.

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
