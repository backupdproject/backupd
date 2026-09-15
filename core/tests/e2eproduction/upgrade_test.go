package e2eproduction_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/config"
	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/repomaintenance"
	"github.com/retnd/retnd/core/internal/snapshotlifecycle"
	"github.com/retnd/retnd/core/internal/state"
)

// #789's two upgrade gates.
//
// They are different questions with the same word in them, and a suite
// that ran only one of them would leave the other's failure mode
// completely uncovered:
//
//   - upgrading retnd onto a deployment that predates the engine.
//     Nothing about such a deployment mentions an engine, a repository
//     domain or a verification level, and the promise is that it goes on
//     running exactly as it did, with no automatic conversion of any
//     artifact set to the incremental engine. That promise is made to
//     every existing installation, so it is tested through the shipped
//     CLI against a real pre-Kopia configuration, not through a struct.
//   - upgrading KOPIA underneath backupd. The vendored engine is a
//     dependency this product embeds, and a bump has to be provable in
//     one run: the whole §10 matrix against a real repository, plus the
//     four costs an embedded engine imposes on everything downstream --
//     binary size, dependency count, build, and resident memory.

// --- upgrading backupd onto a pre-Kopia deployment -----------------------

// preKopiaConfig is exactly what an installation from before EPIC K has:
// two artifact backup sets on local remotes, and not one word about an
// engine, a repository domain, a consistency mode or a verification
// level. Every one of those keys is omitted rather than set to its
// default, because the promise under test is about SILENCE resolving the
// way it always did.
func preKopiaConfig(root string) string {
	return fmt.Sprintf(`poll_interval: 15m
state:
  database: %[1]s/state/backupd.db
sources:
  - id: production
    backup_sets:
      - id: postgres-primary
        remote:
          type: local
        remote_path: %[1]s/remote
        local_path: %[1]s/local
        include:
          - "*.dump"
        completion:
          strategy: rename
        stale_after: 30h
      - id: app-uploads
        remote:
          type: local
        remote_path: %[1]s/remote
        local_path: %[1]s/local
        include:
          - "*.tar"
        completion:
          strategy: rename
        stale_after: 30h
retention:
  timezone: UTC
  week_starts_on: monday
  daily_days: 7
`, root)
}

// TestAPreKopiaDeploymentRunsUnchangedAndIsNeverMigrated is the
// acceptance criterion "existing artifact backup sets remain unchanged",
// proven by running one.
//
// It drives the shipped binary rather than the packages, because the
// claim is about what happens when an operator installs a new version
// over an old deployment: they run `backupd run`, and either their
// backups happen or they do not. A test at the package level would be
// asserting that this suite can construct a Config.
//
// The four things it pins, in the order they would break:
//
//  1. the configuration LOADS. A new binary that refused an old file
//     would be a broken upgrade before anything ran, and it is the most
//     likely failure: Load uses KnownFields(true), so a key the new
//     build invented and wrote into the file would make an older binary
//     refuse it and a missing key could make the new one refuse this.
//  2. a cycle RUNS and moves the artifacts, with the same exit code and
//     the same journal rows it always produced.
//  3. nothing MIGRATED. The file on disk is byte-identical afterwards,
//     both sets still resolve to the artifact engine, and the journal
//     holds no snapshot run for either of them. This is the one the
//     EPIC's non-goal is about: an automatic artifact-to-kopia
//     conversion would silently reinterpret a job.
//  4. the snapshot surfaces REFUSE for those sets rather than answering
//     about them, which is how an operator finds out the sets are not
//     incremental instead of reading an empty list as "no backups".
func TestAPreKopiaDeploymentRunsUnchangedAndIsNeverMigrated(t *testing.T) {
	ctx := context.Background()

	root := t.TempDir()
	for _, dir := range []string{"state", "remote", "local"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatalf("preparing %s: %v", dir, err)
		}
	}

	// Two artifacts on the remote, one per set, old enough that the
	// rename strategy counts them complete.
	writeFile(t, filepath.Join(root, "remote", "pg-2026-06-01.dump"), randomBytes(t, 64<<10))
	writeFile(t, filepath.Join(root, "remote", "uploads-2026-06-01.tar"), randomBytes(t, 32<<10))

	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte(preKopiaConfig(root)), 0o600); err != nil {
		t.Fatalf("writing the pre-Kopia configuration: %v", err)
	}

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading the configuration back: %v", err)
	}

	// 1. it loads, and silence resolves to the artifact engine.
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("this build cannot load a configuration written before the engine existed: %v", err)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("this build refuses a configuration written before the engine existed: %v", err)
	}

	for _, src := range cfg.Sources {
		for _, bs := range src.BackupSets {
			if bs.Engine != model.EngineArtifact {
				t.Errorf("%s resolved to the %q engine from a file that never mentions one; silence must resolve to %q",
					bs.ID, bs.Engine, model.EngineArtifact)
			}

			if bs.Repository.Domain != "" {
				t.Errorf("%s acquired the repository domain %q from a file that declares none", bs.ID, bs.Repository.Domain)
			}
		}
	}

	// 2. a cycle runs, through the shipped command.
	out, err := runCLI(t, root, "run", "--config", configPath)
	if err != nil {
		t.Fatalf("`backupd run` against a pre-Kopia deployment: %v\n%s", err, out)
	}

	for _, name := range []string{"pg-2026-06-01.dump", "uploads-2026-06-01.tar"} {
		if _, err := os.Stat(filepath.Join(root, "local", name)); err != nil {
			t.Errorf("the cycle did not bring %s down to the local backup root: %v\n%s", name, err, out)
		}
	}

	// 3. nothing migrated.
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("re-reading the configuration: %v", err)
	}

	if string(after) != string(before) {
		t.Errorf("a cycle rewrote the configuration file. An upgrade must not edit an operator's file, and it must never convert an artifact set to the incremental engine:\n--- before\n%s\n--- after\n%s",
			before, after)
	}

	journal, err := state.Open(ctx, filepath.Join(root, "state", "backupd.db"))
	if err != nil {
		t.Fatalf("opening the journal the cycle wrote: %v", err)
	}

	defer journal.Close() //nolint:errcheck // a closed test database has nothing left to report.

	sets, err := journal.ListBackupSetIDs(ctx)
	if err != nil {
		t.Fatalf("listing the journal's backup sets: %v", err)
	}

	if len(sets) != 2 {
		t.Errorf("the journal holds rows for %d backup sets (%v) after a cycle over two", len(sets), sets)
	}

	for _, id := range sets {
		records, err := journal.ListByBackupSet(ctx, id)
		if err != nil {
			t.Fatalf("listing %s's artifacts: %v", id, err)
		}

		if len(records) != 1 {
			t.Errorf("%s holds %d artifact rows after a cycle over one artifact", id, len(records))
		}
	}

	// No snapshot lineage exists, and the strongest form of that is that
	// there is no KEY one could hang off: a lineage is looked up by a
	// set's durable uuid, an artifact set has none, and the journal
	// refuses the lookup rather than answering about an empty string.
	// A migration would have had to mint one.
	for _, src := range cfg.Sources {
		for _, bs := range src.BackupSets {
			if bs.UUID != "" {
				t.Errorf("the artifact set %s carries the durable uuid %q; nothing should have minted a snapshot lineage key for it",
					bs.ID, bs.UUID)
			}

			if _, err := journal.ListSnapshotRuns(ctx, strings.ToLower(bs.UUID), 100); err == nil {
				t.Errorf("the journal answered a snapshot-lineage lookup for the artifact set %s", bs.ID)
			}
		}
	}

	// And no repository domain acquired a snapshot either, which is the
	// same claim read from the other end: the domain a migration would
	// have invented holds nothing.
	if has, err := journal.DomainHasSnapshot(ctx, domainID); err != nil {
		t.Fatalf("asking whether the %s domain holds a snapshot: %v", domainID, err)
	} else if has {
		t.Errorf("the %s repository domain holds a snapshot after a cycle over two artifact sets", domainID)
	}

	if unfinished, err := journal.UnfinishedSnapshotRuns(ctx); err != nil {
		t.Fatalf("listing unfinished snapshot runs: %v", err)
	} else if len(unfinished) != 0 {
		t.Errorf("the journal holds %d unfinished snapshot runs after a cycle over two artifact sets", len(unfinished))
	}

	// 4. the snapshot surfaces refuse rather than answering.
	out, err = runCLI(t, root, "snapshot", "list", "production/postgres-primary", "--config", configPath)
	if err == nil {
		t.Errorf("`snapshot list` answered for an artifact backup set:\n%s", out)
	}

	if !strings.Contains(out, "does not store snapshots") {
		t.Errorf("`snapshot list` refused an artifact set without saying why:\n%s", out)
	}
}

// TestAddingAnIncrementalSetLeavesTheArtifactSetsExactlyAsTheyWere is the
// other half of the non-migration promise: the explicit, operator-driven
// path in.
//
// A deployment adopts the engine by declaring a domain and a new set.
// What must not change is everything else, and "everything else" is
// asserted field by field on the RESOLVED sets rather than on the file:
// a resolution that started reading a new default differently would
// leave the file identical and change what runs.
func TestAddingAnIncrementalSetLeavesTheArtifactSetsExactlyAsTheyWere(t *testing.T) {
	root := t.TempDir()

	if err := os.MkdirAll(filepath.Join(root, "state"), 0o750); err != nil {
		t.Fatalf("preparing the state directory: %v", err)
	}

	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte(preKopiaConfig(root)), 0o600); err != nil {
		t.Fatalf("writing the pre-Kopia configuration: %v", err)
	}

	was, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := was.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// The operator's own edit: a declared domain and one new set that
	// names it. Nothing about the existing sets is touched.
	passphrase := filepath.Join(root, "repo.passphrase")
	if err := os.WriteFile(passphrase, []byte(repositoryPassphrase+"\n"), 0o600); err != nil {
		t.Fatalf("writing the repository passphrase: %v", err)
	}

	domains := fmt.Sprintf(`repository_domains:
  - id: %s
    description: Snapshots for this deployment
    isolation: shared
    passphrase:
      file: %s
sources:
`, domainID, passphrase)

	added := fmt.Sprintf(`      - id: uploads-tree
        engine: kopia
        uuid: %s
        repository_domain: %s
        source_consistency: live_best_effort
        verification_level: content_sample
        remote:
          type: local
        remote_path: %s/source
        stale_after: 30h
retention:
`, setUUID, domainID, root)

	// The new set is spliced in where a backup set goes -- inside the
	// sources block, ahead of the retention block that follows it --
	// because an operator's edit lands in the file's own structure and a
	// test that appended it to the end would be testing a file nobody
	// could have written.
	edited := strings.Replace(preKopiaConfig(root), "sources:\n", domains, 1)
	edited = strings.Replace(edited, "retention:\n", added, 1)

	if err := os.WriteFile(configPath, []byte(edited), 0o600); err != nil {
		t.Fatalf("writing the edited configuration: %v", err)
	}

	now, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("loading the configuration after the operator added an incremental set: %v", err)
	}

	if err := now.Validate(); err != nil {
		t.Fatalf("the configuration with one incremental set added is refused: %v", err)
	}

	if len(now.Sources[0].BackupSets) != len(was.Sources[0].BackupSets)+1 {
		t.Fatalf("the edited configuration holds %d sets; it should hold the original %d plus one",
			len(now.Sources[0].BackupSets), len(was.Sources[0].BackupSets))
	}

	for i, before := range was.Sources[0].BackupSets {
		after := now.Sources[0].BackupSets[i]

		if after.Engine != model.EngineArtifact {
			t.Errorf("%s changed engine to %q when an incremental set was added beside it", before.ID, after.Engine)
		}

		for _, field := range []struct {
			name        string
			was, become any
		}{
			{"id", before.ID, after.ID},
			{"remote type", before.Remote.Type, after.Remote.Type},
			{"remote path", before.RemotePath, after.RemotePath},
			{"local path", before.LocalPath, after.LocalPath},
			{"completion strategy", before.Completion.Strategy, after.Completion.Strategy},
			{"stale after", before.StaleAfter, after.StaleAfter},
			{"read only", before.ReadOnly, after.ReadOnly},
			{"disabled", before.Disabled, after.Disabled},
			{"source identity", before.SourceIdentity, after.SourceIdentity},
		} {
			if field.was != field.become {
				t.Errorf("%s's %s became %v; it was %v before the incremental set was added", before.ID, field.name, field.become, field.was)
			}
		}
	}

	// And the new set is the only one that is incremental.
	incremental := 0

	for _, bs := range now.Sources[0].BackupSets {
		if bs.Engine == model.EngineKopia {
			incremental++
		}
	}

	if incremental != 1 {
		t.Errorf("%d sets resolved to the incremental engine; the operator declared one", incremental)
	}
}

// --- upgrading kopia underneath backupd ----------------------------------

// kopiaUpgradeRecord is the four costs an embedded engine imposes,
// measured, plus the version they belong to.
//
// It is checked in (testdata/kopia-upgrade.json) so that a kopia bump is
// a diff rather than a discussion: run this suite, read the before and
// after off one log line, and update the record in the same change.
type kopiaUpgradeRecord struct {
	KopiaVersion string `json:"kopia_version"`

	// BinarySizeBytes and DependencyModules are DETERMINISTIC for a
	// commit -- two builds of one tree produce the same numbers -- so
	// they are the two this suite gates on.
	BinarySizeBytes   int64 `json:"binary_size_bytes"`
	DependencyModules int   `json:"dependency_modules"`

	// BuildSeconds and PeakRSSBytes are measurements of THIS machine
	// under THIS load. They are recorded and never gated, for
	// docs/perf/README.md's reason: a threshold on a number a busy host
	// can move is a threshold that gets deleted the third time it fires
	// spuriously.
	BuildSeconds float64 `json:"build_seconds"`
	PeakRSSBytes int64   `json:"peak_rss_bytes"`
}

// The two gated numbers get two tolerances, because they are
// reproducible to different degrees and pretending otherwise would make
// one of them useless.
//
// The MODULE count is a property of the module graph: the same tree
// answers the same number on any machine, and the only legitimate way it
// moves is somebody adding or removing a dependency. Ten per cent of 126
// is twelve modules, which is wide enough for a build-tag difference
// between platforms and narrow enough that an engine upgrade dragging a
// new transitive world in cannot hide in it.
//
// The BINARY size is not: it moves with the Go toolchain version, the
// target platform and the linker's mood, none of which is a change to
// this product. Thirty-five per cent is sized from that -- it is a
// ceiling against "the embedded engine doubled the artifact", which is
// the failure this number exists to catch, and it is deliberately not a
// regression detector for a megabyte.
const (
	upgradeModuleDrift = 0.10
	upgradeBinaryDrift = 0.35
)

// TestTheKopiaUpgradeCompatibilitySuite is EPIC K §10's compatibility
// gate: the whole matrix against a real repository, plus the recorded
// costs.
//
// Every rung is here because a kopia upgrade has broken exactly this
// kind of thing in other products: a format version that opens but
// cannot be written, a maintenance pass that stops reclaiming, a restore
// that loses metadata, a crash that leaves an unreadable index. Running
// them as one test against one repository is the point -- each rung is
// only meaningful given the state the previous one left.
func TestTheKopiaUpgradeCompatibilitySuite(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t, deploymentOptions{createRepository: true})

	want := seed(t, d.srcDir, 40, 16<<10, 4)
	logical := int64(40 * (16 << 10))

	peak := newHeapSampler()

	// create + open happened in newDeployment; asserting the capability
	// here is what makes "open" a rung rather than a precondition.
	if _, err := d.repo.Stats(ctx); err != nil {
		t.Fatalf("open: the repository this build created cannot be read: %v", err)
	}

	// snapshot
	first, err := d.run(t, "upgrade-a", snapshotlifecycle.VerificationOptions{SamplePercent: 50})
	if err != nil || !first.Succeeded() {
		t.Fatalf("snapshot: %v (%s)", err, first.Reason)
	}

	// incremental + reuse
	writeFile(t, filepath.Join(d.srcDir, filepath.FromSlash(seededPath(0, 4))), randomBytes(t, 16<<10))

	second, err := d.run(t, "upgrade-b", snapshotlifecycle.VerificationOptions{SamplePercent: 50})
	if err != nil || !second.Succeeded() {
		t.Fatalf("incremental: %v (%s)", err, second.Reason)
	}

	if second.ContentReusedBytes <= 0 || second.RepositoryBytesWritten >= logical/4 {
		t.Errorf("reuse: the incremental snapshot wrote %d bytes and reused %d over a %d byte tree with one changed file; this kopia version is not deduplicating across runs",
			second.RepositoryBytesWritten, second.ContentReusedBytes, logical)
	}

	// verify, at the deepest level that does not need a disk
	verified, err := d.repo.Verify(ctx, backupengine.SnapshotID(second.SnapshotID), backupengine.VerifyRequest{
		Level: model.LevelContentFull,
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	if verified.BytesVerified != logical {
		t.Errorf("verify: read %d bytes back for a %d byte snapshot", verified.BytesVerified, logical)
	}

	// restore, compared against the source rather than against the
	// repository, which is the comparison that catches a faithful store
	// of the wrong bytes.
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := d.repo.Restore(ctx, backupengine.SnapshotID(first.SnapshotID), backupengine.RestoreRequest{
		TargetPath: target,
		Conflict:   backupengine.ConflictRefuse,
		SkipOwners: true,
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	got := hashTree(t, target)
	for rel, sum := range want {
		if got[rel] != sum {
			t.Errorf("restore: %s came back as %q, want %q", rel, got[rel], sum)
		}
	}

	// delete + maintenance, which is the pair: a deleted manifest's
	// content is only reclaimed by a maintenance pass, and a version
	// that stopped reclaiming would leave a repository growing for ever.
	if err := d.repo.DeleteSnapshot(ctx, backupengine.SnapshotID(first.SnapshotID)); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := d.repo.LookupSnapshot(ctx, backupengine.SnapshotID(first.SnapshotID)); err == nil {
		t.Error("delete: the deleted snapshot still resolves")
	}

	store, err := backupengine.NewFileMaintenanceOwnershipStore(filepath.Join(d.root, "state", "maintenance"))
	if err != nil {
		t.Fatalf("NewFileMaintenanceOwnershipStore: %v", err)
	}

	runner := repomaintenance.Runner{
		Owner: backupengine.MaintenanceOwner("kopia-upgrade-gate"),
		Store: store,
		Fence: repomaintenance.NewFence(),
	}

	if _, err := runner.Claim(ctx, d.loc.Domain); err != nil {
		t.Fatalf("maintenance: claiming %s: %v", d.loc.Domain, err)
	}

	maintained, err := runner.Run(ctx, d.loc.Domain, d.repo, backupengine.MaintenanceFull, time.Now())
	if err != nil {
		t.Fatalf("maintenance: %v", err)
	}

	if !maintained.Attempted {
		t.Error("maintenance: the window was never attempted")
	}

	// The surviving snapshot still verifies afterwards, which is the
	// assertion that a reclaim took content it should not have.
	if _, err := d.repo.Verify(ctx, backupengine.SnapshotID(second.SnapshotID), backupengine.VerifyRequest{
		Level: model.LevelContentFull,
	}); err != nil {
		t.Fatalf("maintenance: the surviving snapshot no longer verifies: %v", err)
	}

	// crash-recovery: the repository this process is holding open is
	// reopened cold, which is what a restarted daemon does, and the
	// snapshot still resolves. The process-level crash is
	// crash_test.go's; this rung is about the FORMAT surviving a
	// reopen, which is what a kopia upgrade most plausibly breaks.
	closeDeployment(t, d)

	reopened := newDeployment(t, deploymentOptions{root: d.root})

	if _, err := reopened.repo.LookupSnapshot(ctx, backupengine.SnapshotID(second.SnapshotID)); err != nil {
		t.Fatalf("crash-recovery: the snapshot does not resolve after the repository was reopened cold: %v", err)
	}

	rss := peak.stop()

	// --- the recorded costs -----------------------------------------------

	measured := measureUpgradeCosts(t, int64(rss))
	baseline := loadUpgradeBaseline(t)

	t.Logf("kopia upgrade costs: version %s -> %s | binary %d -> %d bytes | modules %d -> %d | build %.1fs -> %.1fs | peak RSS %d -> %d bytes",
		baseline.KopiaVersion, measured.KopiaVersion,
		baseline.BinarySizeBytes, measured.BinarySizeBytes,
		baseline.DependencyModules, measured.DependencyModules,
		baseline.BuildSeconds, measured.BuildSeconds,
		baseline.PeakRSSBytes, measured.PeakRSSBytes)

	if measured.KopiaVersion != baseline.KopiaVersion {
		t.Errorf("this tree vendors kopia %s and testdata/kopia-upgrade.json records %s. That is exactly the change this suite exists for: read the before/after line above and update the record in the same commit as the bump.",
			measured.KopiaVersion, baseline.KopiaVersion)
	}

	assertWithinDrift(t, "binary size", float64(baseline.BinarySizeBytes), float64(measured.BinarySizeBytes), upgradeBinaryDrift)
	assertWithinDrift(t, "dependency modules", float64(baseline.DependencyModules), float64(measured.DependencyModules), upgradeModuleDrift)
}

// measureUpgradeCosts builds the shipped command and measures what it
// costs.
func measureUpgradeCosts(t *testing.T, peakRSS int64) kopiaUpgradeRecord {
	t.Helper()

	bin := filepath.Join(harnessDir, "backupd-measured")

	started := time.Now()

	build := exec.Command("go", "build", "-o", bin, "./cmd/retnd")
	build.Dir = coreDir(t)

	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the shipped command to measure it: %v\n%s", err, out)
	}

	elapsed := time.Since(started)

	info, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("measuring the built command: %v", err)
	}

	return kopiaUpgradeRecord{
		KopiaVersion:      kopiaVersion(t),
		BinarySizeBytes:   info.Size(),
		DependencyModules: dependencyModules(t),
		BuildSeconds:      elapsed.Seconds(),
		PeakRSSBytes:      peakRSS,
	}
}

// kopiaVersion is the version this tree vendors, read out of go.mod
// rather than out of a constant somebody has to remember to change.
func kopiaVersion(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(coreDir(t), "go.mod"))
	if err != nil {
		t.Fatalf("reading core/go.mod: %v", err)
	}

	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "github.com/kopia/kopia" {
			return fields[1]
		}
	}

	t.Fatal("core/go.mod does not require github.com/kopia/kopia, so this suite has nothing to measure")

	return ""
}

// dependencyModules is how many distinct modules end up inside the
// shipped command.
//
// The number that matters is the one an audit has to cover, which is
// modules rather than packages: compliance.json, the NOTICE file and the
// SBOM are all per-module, so this is the count that turns into work
// when it moves.
func dependencyModules(t *testing.T) int {
	t.Helper()

	cmd := exec.Command("go", "list", "-deps", "-f", "{{with .Module}}{{.Path}}{{end}}", "./cmd/retnd")
	cmd.Dir = coreDir(t)

	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("listing the command's dependencies: %v\n%s", err, exit.Stderr)
		}

		t.Fatalf("listing the command's dependencies: %v", err)
	}

	seen := map[string]struct{}{}

	for line := range strings.Lines(string(out)) {
		if path := strings.TrimSpace(line); path != "" {
			seen[path] = struct{}{}
		}
	}

	return len(seen)
}

func loadUpgradeBaseline(t *testing.T) kopiaUpgradeRecord {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "kopia-upgrade.json"))
	if err != nil {
		t.Fatalf("this suite is the kopia-upgrade gate and cannot read its own record: %v. A missing record is a refusal, not a skip.", err)
	}

	var record kopiaUpgradeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("reading testdata/kopia-upgrade.json: %v", err)
	}

	return record
}

func assertWithinDrift(t *testing.T, what string, baseline, measured, drift float64) {
	t.Helper()

	if baseline <= 0 {
		t.Errorf("the recorded %s is %v, which no measurement can be compared against", what, baseline)

		return
	}

	ratio := measured / baseline
	if ratio < 1-drift || ratio > 1+drift {
		t.Errorf("%s moved from %v to %v (%.1f%% of the record), past the %.0f%% this gate allows. Either the change is wanted -- update testdata/kopia-upgrade.json in this commit -- or it is the cost of a dependency nobody meant to add.",
			what, baseline, measured, ratio*100, drift*100)
	}
}

// --- shared plumbing ------------------------------------------------------

// coreDir is the core module's root, which is two levels above this
// package.
func coreDir(t *testing.T) string {
	t.Helper()

	dir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the core module's directory: %v", err)
	}

	return dir
}

var (
	cliOnce sync.Once
	cliPath string
	cliErr  error
)

// cliBinary builds the shipped command once per test binary.
func cliBinary(t *testing.T) string {
	t.Helper()

	cliOnce.Do(func() {
		bin := filepath.Join(harnessDir, "backupd")

		cmd := exec.Command("go", "build", "-o", bin, "./cmd/retnd")
		cmd.Dir = coreDir(t)

		if out, err := cmd.CombinedOutput(); err != nil {
			cliErr = errors.New(string(out))

			return
		}

		cliPath = bin
	})

	if cliErr != nil {
		t.Fatalf("building the shipped command: %v", cliErr)
	}

	return cliPath
}

// runCLI runs the shipped command and returns everything it printed on
// both streams, which is what an operator sees.
func runCLI(t *testing.T, cwd string, args ...string) (string, error) {
	t.Helper()

	cmd := exec.Command(cliBinary(t), args...)
	cmd.Dir = cwd

	out, err := cmd.CombinedOutput()

	return string(out), err
}
