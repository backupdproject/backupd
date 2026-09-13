package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/kopia"
	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/health"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/secretref"
	"github.com/backupdproject/backupd/core/internal/state"
	"github.com/backupdproject/backupd/core/internal/transport/rclone"
)

// The whole incremental engine on one wire, from the cycle an operator's
// schedule runs down to a real Kopia repository on a real filesystem:
// config, journal, the rclone source adapter over a real local tree, the
// source tree's pull-shaped walk, the snapshot lifecycle's phases, and
// the catalog rows that survive the process.
//
// Nothing here is a double. That is the point: the parts each have their
// own unit tests, and what those cannot show is that a set configured
// `engine: kopia` in a file produces a restore point at the end of one
// RunCycle, twice, with the second run storing almost nothing.

const testRepositoryPassphrase = "a-passphrase-long-enough-to-be-a-passphrase"

// incrementalDeployment is a validated-shaped deployment with one
// incremental set: a local source tree, a declared repository domain with
// a passphrase file, and the backup root the repository will live under.
//
// The resolved fields are filled in the way config.Validate fills them,
// by hand, for the reason every fixture in this package does it: these
// configs are built rather than loaded, and a set whose Engine is the
// zero value is a different set.
func incrementalDeployment(t *testing.T) (*config.Config, config.BackupSet, string) {
	t.Helper()

	sourceDir := t.TempDir()
	backupRoot := t.TempDir()

	passFile := filepath.Join(t.TempDir(), "repo.passphrase")
	if err := os.WriteFile(passFile, []byte(testRepositoryPassphrase), 0o600); err != nil {
		t.Fatalf("writing the passphrase file: %v", err)
	}

	domain, err := model.NewRepositoryDomainID("production")
	if err != nil {
		t.Fatalf("NewRepositoryDomainID: %v", err)
	}

	bs := config.BackupSet{
		Name:       "uploads-tree",
		ID:         mustSetID(t, "production", "uploads-tree"),
		UUID:       "6f1d2b7a-1c4e-4f8b-9a2d-3e5c7b9d1f00",
		Engine:     model.EngineKopia,
		Remote:     config.Remote{Type: "local"},
		RemotePath: sourceDir,
		StaleAfter: 30 * 60 * 60 * 1000 * 1000 * 1000,
	}
	bs.Repository = model.RepositoryRef{Domain: domain, Set: bs.ID}
	bs.Consistency = model.ModeLiveBestEffort
	bs.VerificationLevel = model.LevelStructural

	identity, err := model.NewSourceIdentity(model.SourceIdentityInput{
		SetUUID:  bs.UUID,
		Endpoint: model.SourceEndpoint{Kind: "local"},
		Root:     model.SourceRoot{Path: sourceDir, MountPrefix: sourceDir},
	})
	if err != nil {
		t.Fatalf("NewSourceIdentity: %v", err)
	}

	bs.SourceIdentity = identity

	cfg := testConfig(t, testSource("production", bs))
	cfg.Capacity.BackupRoot = backupRoot
	cfg.RepositoryDomains = []config.RepositoryDomainConfig{{
		ID:            domain.String(),
		Isolation:     string(model.RepositoryShared),
		Domain:        model.RepositoryDomain{ID: domain, Isolation: model.RepositoryShared},
		PassphraseRef: secretref.Ref{File: passFile},
	}}

	return cfg, bs, sourceDir
}

// incrementalService is the Service an operator's schedule would run:
// real journal, real transport, real engine.
func incrementalService(t *testing.T, cfg *config.Config) (*Service, *state.Journal) {
	t.Helper()

	journal := openJournal(t)
	svc := New(cfg, journal, rclone.New(), nil)
	svc.Repositories = kopia.New()

	return svc, journal
}

// seedTree writes files into a source tree, creating directories as
// needed.
func seedTree(t *testing.T, root string, files map[string][]byte) {
	t.Helper()

	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}

		if err := os.WriteFile(full, body, 0o600); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}
}

// incompressibleBytes is content deduplication cannot cheat on: a
// repository that stored it again would show it, and one that reused it
// cannot hide behind compression.
func incompressibleBytes(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b9))
	out := make([]byte, n)

	for i := range out {
		out[i] = byte(r.UintN(256))
	}

	return out
}

func sumOf(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

func TestRunCycle_AnIncrementalSetSnapshotsItsSourceIntoARealRepository(t *testing.T) {
	t.Parallel()

	cfg, bs, sourceDir := incrementalDeployment(t)
	files := map[string][]byte{
		"index.txt":         incompressibleBytes(256<<10, 1),
		"runs/2026/db.dump": incompressibleBytes(256<<10, 2),
		"runs/2026/log.txt": incompressibleBytes(256<<10, 3),
	}
	seedTree(t, sourceDir, files)

	svc, journal := incrementalService(t, cfg)
	ctx := context.Background()

	report := svc.RunCycle(ctx)

	if len(report.Sets) != 1 {
		t.Fatalf("len(report.Sets) = %d, want 1", len(report.Sets))
	}

	set := report.Sets[0]
	if set.Err != nil {
		t.Fatalf("the incremental set's pass failed: %v", set.Err)
	}

	if set.Snapshot == nil || !set.Snapshot.Succeeded() {
		t.Fatalf("the pass produced no restore point: %+v", set.Snapshot)
	}

	run := set.Snapshot.Run

	if run.Phase != state.PhaseSuccess {
		t.Errorf("run ended at %s, want %s", run.Phase, state.PhaseSuccess)
	}

	if !run.LastKnownGood {
		t.Error("a successful run did not become the set's last-known-good restore point")
	}

	if run.Files != int64(len(files)) {
		t.Errorf("the snapshot holds %d files, want %d", run.Files, len(files))
	}

	if run.VerificationStatus != state.SnapshotVerificationPassed {
		t.Errorf("verification status %q, want passed", run.VerificationStatus)
	}

	// The four numbers a surface must be able to tell apart. On a first
	// run over incompressible content, written is close to read; what
	// must never be true is that the report cannot distinguish them at
	// all.
	if run.SourceBytesRead <= 0 || run.RepositoryBytesWritten <= 0 {
		t.Errorf("the run measured nothing: read %d, written %d", run.SourceBytesRead, run.RepositoryBytesWritten)
	}

	if run.LogicalBytes <= 0 {
		t.Errorf("the run scanned nothing: logical %d", run.LogicalBytes)
	}

	// The lineage is durable: the row is in the journal, under this set,
	// with the engine's manifest id on it.
	rows, err := journal.ListSnapshotRuns(ctx, snapshotLineage(bs), 10)
	if err != nil {
		t.Fatalf("listing snapshot runs: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("the journal holds %d runs for this set, want 1", len(rows))
	}

	if rows[0].SnapshotID == "" {
		t.Error("the catalog row records no snapshot id, so nothing can find the restore point again")
	}

	if rows[0].Engine != model.EngineKopia.String() || rows[0].Domain != "production" {
		t.Errorf("the row records engine %q in domain %q", rows[0].Engine, rows[0].Domain)
	}

	if rows[0].SourceIdentity != bs.SourceIdentity.String() {
		t.Errorf("the row records source identity %q, want the set's own", rows[0].SourceIdentity)
	}

	// ONE snapshot, under ONE source: the shape #783 exists to produce,
	// as the repository itself reports it.
	assertOneSnapshotPerRun(t, cfg, bs, 1)
}

func TestRunCycle_ASecondIncrementalRunReusesContentAndKeepsTheLineage(t *testing.T) {
	t.Parallel()

	cfg, bs, sourceDir := incrementalDeployment(t)
	files := map[string][]byte{
		"index.txt":         incompressibleBytes(256<<10, 11),
		"runs/2026/db.dump": incompressibleBytes(256<<10, 12),
		"runs/2026/log.txt": incompressibleBytes(256<<10, 13),
	}
	seedTree(t, sourceDir, files)

	svc, journal := incrementalService(t, cfg)
	ctx := context.Background()

	first := svc.RunCycle(ctx).Sets[0]
	if first.Err != nil {
		t.Fatalf("first pass: %v", first.Err)
	}

	// One file changes; the other two are byte-for-byte what the
	// repository already holds.
	changed := incompressibleBytes(256<<10, 99)
	seedTree(t, sourceDir, map[string][]byte{"runs/2026/log.txt": changed})

	second := svc.RunCycle(ctx).Sets[0]
	if second.Err != nil {
		t.Fatalf("second pass: %v", second.Err)
	}

	before, after := first.Snapshot.Run, second.Snapshot.Run

	if !after.Succeeded() {
		t.Fatalf("the second pass produced no restore point: %+v", after)
	}

	if after.RunID == before.RunID {
		t.Fatal("the second pass resolved to the first run's row; a new pass over a changed source is a new logical request")
	}

	// The measurement that says content was reused: the second run read
	// the whole tree again (every run reads every byte, deliberately) and
	// stored only what was new.
	if after.RepositoryBytesWritten >= before.RepositoryBytesWritten/2 {
		t.Errorf("the second run wrote %d bytes against the first run's %d; a tree with one changed file out of three should store far less",
			after.RepositoryBytesWritten, before.RepositoryBytesWritten)
	}

	if after.RepositoryBytesWritten >= after.SourceBytesRead/2 {
		t.Errorf("the second run wrote %d bytes of the %d it read; presenting what was read as what was stored is the claim EPIC K forbids",
			after.RepositoryBytesWritten, after.SourceBytesRead)
	}

	if after.ContentReusedBytes <= 0 {
		t.Errorf("the second run reports %d reused bytes", after.ContentReusedBytes)
	}

	// The lineage: two runs, both recorded, and only the newer one is the
	// restore point.
	rows, err := journal.ListSnapshotRuns(ctx, snapshotLineage(bs), 10)
	if err != nil {
		t.Fatalf("listing snapshot runs: %v", err)
	}

	if len(rows) != 2 {
		t.Fatalf("the journal holds %d runs, want 2", len(rows))
	}

	lkg, err := journal.LastKnownGoodSnapshot(ctx, snapshotLineage(bs))
	if err != nil {
		t.Fatalf("reading last-known-good: %v", err)
	}

	if lkg.RunID != after.RunID {
		t.Errorf("last-known-good is %q, want the newer successful run %q", lkg.RunID, after.RunID)
	}

	// Two runs, two snapshots, still one source: the incremental shape
	// rather than one Kopia source per object.
	assertOneSnapshotPerRun(t, cfg, bs, 2)

	// And the bytes come back: a restore of the newest snapshot
	// reproduces every file's hash, including the one that changed.
	want := map[string]string{
		"index.txt":         sumOf(files["index.txt"]),
		"runs/2026/db.dump": sumOf(files["runs/2026/db.dump"]),
		"runs/2026/log.txt": sumOf(changed),
	}
	assertRestoreMatches(t, cfg, bs, backupengine.SnapshotID(after.SnapshotID), want)
}

// A repository serves a DOMAIN, and a domain may be shared. So the guard
// in front of creating one has to ask "has anything ever committed a
// snapshot to this domain", not "has THIS set".
//
// The set-scoped version of the question is the bug: a set added to a
// populated domain has no history of its own, so the guard opened, and
// the cycle created a second, empty repository at the same location as a
// live one. Every later snapshot would then succeed, deduplicate against
// nothing, and be unrestorable from the repository the operator thinks
// they have.
func TestRunCycle_ANewSetInAPopulatedDomainRefusesToCreateASecondRepository(t *testing.T) {
	t.Parallel()

	cfg, bs, sourceDir := incrementalDeployment(t)
	seedTree(t, sourceDir, map[string][]byte{"index.txt": incompressibleBytes(64<<10, 21)})

	svc, journal := incrementalService(t, cfg)
	ctx := context.Background()

	// A co-tenant set that committed a manifest to this domain: its own
	// lineage, its own source identity, and no rows at all under the set
	// this cycle is about.
	cotenant, err := journal.BeginSnapshotRun(ctx, state.SnapshotRunRequest{
		RunID:             "cotenant-run",
		IdempotencyKey:    "cotenant-run",
		Set:               mustSetID(t, "production", "postgres-primary"),
		SetUUID:           "0c9a44e2-77b1-4d3f-8a61-5f0ab2c3d4e5",
		Engine:            model.EngineKopia.String(),
		Domain:            bs.Repository.Domain.String(),
		SourceIdentity:    "sha256:cotenant",
		VerificationLevel: string(model.LevelStructural),
		StartedAt:         svc.now(),
	})
	if err != nil {
		t.Fatalf("seeding the co-tenant run: %v", err)
	}

	for _, phase := range []state.SnapshotPhase{state.PhaseSourceScan, state.PhaseSnapshotWrite} {
		if err := journal.AdvanceSnapshotRun(ctx, cotenant.Run.RunID, phase, state.SnapshotRunUpdate{At: svc.now()}); err != nil {
			t.Fatalf("seeding the co-tenant phase %s: %v", phase, err)
		}
	}

	if err := journal.AdvanceSnapshotRun(ctx, cotenant.Run.RunID, state.PhaseManifestCommitted, state.SnapshotRunUpdate{
		SnapshotID: new("cotenant-manifest"),
		At:         svc.now(),
	}); err != nil {
		t.Fatalf("seeding the co-tenant manifest: %v", err)
	}

	set := svc.RunCycle(ctx).Sets[0]
	if set.Err == nil {
		t.Fatal("the cycle created a repository in a domain that already holds snapshots; a second repository over a live one is unrecoverable")
	}

	if !strings.Contains(set.Err.Error(), "refusing to create a second") {
		t.Errorf("the refusal reads %q, which does not tell an operator what was refused or why", set.Err)
	}

	// Nothing was written: no repository, and no row for this set.
	if _, err := kopia.New().OpenRepository(ctx, backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     bs.Repository.Domain,
		Root:       cfg.EffectiveBackupRoot(),
		Passphrase: cfg.RepositoryDomains[0].PassphraseRef,
	}); !errors.Is(err, backupengine.ErrRepositoryNotFound) {
		t.Errorf("opening the domain's location after the refusal returned %v, want ErrRepositoryNotFound: the refusal must not have created one", err)
	}

	rows, err := journal.ListSnapshotRuns(ctx, snapshotLineage(bs), 10)
	if err != nil {
		t.Fatalf("listing this set's runs: %v", err)
	}

	if len(rows) != 0 {
		t.Errorf("the refused pass left %d rows for this set, want none: a refusal before the first write leaves nothing to reconcile", len(rows))
	}
}

// The scanned-entry count is the source side's own census, and it has to
// survive the whole way to the surfaces that render it.
//
// It used to be dropped at the manifest commit and reconstructed as files
// plus directories, which is a different number: it leaves out every
// entry a pass deliberately skipped -- the symlink below -- and the run's
// own report never set it at all, so an operator polling a submission was
// told the run had scanned zero entries.
func TestRunCycle_TheScannedEntryCountSurvivesToEveryReport(t *testing.T) {
	t.Parallel()

	cfg, bs, sourceDir := incrementalDeployment(t)
	files := map[string][]byte{
		"index.txt":         incompressibleBytes(64<<10, 31),
		"runs/2026/db.dump": incompressibleBytes(64<<10, 32),
	}
	seedTree(t, sourceDir, files)

	// One entry the pass considers and deliberately does not store. A
	// count derived from what the snapshot HOLDS cannot see it.
	if err := os.Symlink(filepath.Join(sourceDir, "index.txt"), filepath.Join(sourceDir, "latest.txt")); err != nil {
		t.Fatalf("seeding a symlink: %v", err)
	}

	svc, journal := incrementalService(t, cfg)
	ctx := context.Background()

	set := svc.RunCycle(ctx).Sets[0]
	if set.Err != nil {
		t.Fatalf("the pass failed: %v", set.Err)
	}

	run := set.Snapshot.Run
	if !run.Succeeded() {
		t.Fatalf("the pass produced no restore point: %+v", run)
	}

	// The source side counted the two files it stored and the symlink it
	// refused to store: three entries CONSIDERED against two objects in
	// the snapshot. A count derived from what the snapshot holds cannot
	// produce that number, which is the whole reason this is its own
	// column.
	if want := int64(len(files)) + 1; run.Entries != want {
		t.Errorf("the run reports %d entries scanned, want %d (two files and the symlink it skipped)", run.Entries, want)
	}

	if run.Entries <= run.Files {
		t.Errorf("the run reports %d entries scanned against %d files stored; the entry the pass skipped is missing from the count",
			run.Entries, run.Files)
	}

	row, err := journal.GetSnapshotRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("reading the run's row: %v", err)
	}

	if row.EntriesScanned == nil || *row.EntriesScanned != run.Entries {
		t.Errorf("the catalog row records %v entries scanned, want the run's %d", row.EntriesScanned, run.Entries)
	}

	if row.SourceComplete == nil || !*row.SourceComplete {
		t.Errorf("source_complete = %v on a successful run; a skipped symlink is policy, not a hole in the backup", row.SourceComplete)
	}

	// And the health report, which is what `backupd status` and the
	// dashboard render.
	report, err := svc.BuildHealthReport(ctx, VersionInfo{})
	if err != nil {
		t.Fatalf("BuildHealthReport: %v", err)
	}

	var snap *health.SnapshotHealth
	for _, s := range report.BackupSets {
		if s.Set == bs.ID {
			snap = s.Snapshot
		}
	}

	if snap == nil {
		t.Fatal("the health report carries no snapshot block for an incremental set that has run")
	}

	if snap.EntriesScanned != run.Entries {
		t.Errorf("the health report says %d entries scanned, want %d", snap.EntriesScanned, run.Entries)
	}

	// What the set asked for and what the run proved, kept apart: this
	// set is configured for a structural verification and the pass
	// actually read every byte the snapshot references.
	if snap.VerificationLevel != string(model.LevelStructural) {
		t.Errorf("the health report says the set is configured for %q, want %q", snap.VerificationLevel, model.LevelStructural)
	}

	if snap.VerificationAchieved != string(model.LevelContentFull) {
		t.Errorf("the health report says the run proved %q, want %q: a report carrying only the configured level asserts a verification nobody performed",
			snap.VerificationAchieved, model.LevelContentFull)
	}

	if snap.UnfinishedRuns != 0 {
		t.Errorf("the health report counts %d unfinished runs after a clean cycle, want 0", snap.UnfinishedRuns)
	}
}

// assertOneSnapshotPerRun opens the deployment's repository and checks
// that it holds exactly one snapshot per run, all under one source.
func assertOneSnapshotPerRun(t *testing.T, cfg *config.Config, bs config.BackupSet, runs int) {
	t.Helper()

	repo := openTestRepository(t, cfg, bs)

	infos, err := repo.ListSnapshots(context.Background(), snapshotSource(bs))
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}

	if len(infos) != runs {
		t.Fatalf("the repository holds %d snapshots for this set, want %d (one per run, not one per object)", len(infos), runs)
	}

	stats, err := repo.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if stats.Sources != 1 {
		t.Errorf("the repository reports %d sources for one backup set, want 1", stats.Sources)
	}

	if stats.Snapshots != runs {
		t.Errorf("the repository reports %d snapshots, want %d", stats.Snapshots, runs)
	}
}

// assertRestoreMatches restores one snapshot and compares every file's
// content hash with what the source held.
func assertRestoreMatches(t *testing.T, cfg *config.Config, bs config.BackupSet, id backupengine.SnapshotID, want map[string]string) {
	t.Helper()

	repo := openTestRepository(t, cfg, bs)
	dest := t.TempDir()

	if _, err := repo.Restore(context.Background(), id, backupengine.RestoreRequest{TargetPath: dest, SkipOwners: true}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	for name, sum := range want {
		body, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("reading the restored %s: %v", name, err)

			continue
		}

		if got := sumOf(body); got != sum {
			t.Errorf("the restored %s hashes to %s, want %s", name, got, sum)
		}
	}
}

func openTestRepository(t *testing.T, cfg *config.Config, bs config.BackupSet) backupengine.Repository {
	t.Helper()

	repo, err := kopia.New().OpenRepository(context.Background(), backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     bs.Repository.Domain,
		Root:       cfg.EffectiveBackupRoot(),
		Passphrase: cfg.RepositoryDomains[0].PassphraseRef,
	})
	if err != nil {
		t.Fatalf("opening the repository the cycle wrote to: %v", err)
	}

	t.Cleanup(func() {
		if err := repo.Close(context.Background()); err != nil {
			t.Errorf("closing the repository: %v", err)
		}
	})

	return repo
}
