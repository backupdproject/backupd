// Package e2eproduction_test is EPIC K's release gate held as one
// automated workflow (#789): the required end-to-end production scenario,
// the incremental-storage claim measured at production scale, a process
// crash, a restore from a clean environment, and the upgrade path from a
// deployment that predates the engine entirely.
//
// # What is real here and what is not
//
// Everything except the operator. The configuration is a config.yaml this
// package writes and the product's own Load and Validate resolve, so the
// backup sets, the repository domain, the verification budget, the
// retention chain and the source identity are the ones an operator's file
// produces rather than structs assembled in Go. The source is read by the
// production reading path (internal/backupengine/source.Adapter over
// rclone's local backend), the repository is a real Kopia repository on a
// real filesystem, the run is driven by the real
// internal/snapshotlifecycle.Runner, retention is decided by the real
// internal/snapshotretention.Pruner, maintenance by the real
// internal/repomaintenance.Runner, and the restores are the engine's own.
//
// # Why the source is a local volume rather than an SFTP server
//
// Because the product refuses the other thing, by design, and a suite
// that arranged to get past the refusal would be testing something
// nobody can run. A tree walk needs bounded listing;
// core/internal/backend/bundled/sftp.json declares bounded_listing false
// because rclone's sftp backend reads a directory through pkg/sftp's
// ReadDir, which has no resumable cursor; so source.Adapter.OpenTree
// refuses an SFTP source with backend.ErrUnboundedListing before it
// dials anything, and tells the operator to back that source up from an
// explicit path list instead -- which is the artifact engine's job, not
// this one's. TestAnSFTPSourceIsRefusedAWalkedTreeRunBeforeAnythingIsDialed
// pins that refusal, and scenario_test.go's comment says what it means
// for the scenario.
//
// # Why the scale tests are in this package and bounded by default
//
// A million-entry directory, a deep tree and a multi-gigabyte file are
// the three shapes that decide whether this engine is safe on a real
// source, and evidence nobody runs is not evidence. So each has a
// bounded SMOKE size that runs on every gate and a full size behind the
// `kopiabench` build tag (bench_full_test.go), sharing one body so the
// thing the gate exercises is the thing the full run measures.
package e2eproduction_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/backupengine/kopia"
	"github.com/retnd/retnd/core/internal/backupengine/source"
	"github.com/retnd/retnd/core/internal/config"
	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/secretref"
	"github.com/retnd/retnd/core/internal/snapshotlifecycle"
	"github.com/retnd/retnd/core/internal/state"
	"github.com/retnd/retnd/core/internal/transport"
	"github.com/retnd/retnd/core/internal/transport/rclone"
)

// repositoryPassphrase is this suite's own. It reaches the engine through
// a file, because a secretref is the only way to give the adapter one:
// there is no field on a repository location a literal secret fits into,
// for tests any more than for production wiring.
const repositoryPassphrase = "e2e-production-repository-passphrase-not-a-secret"

// setUUID is the lineage key every catalog read in this package is keyed
// on. It is fixed rather than minted per run so that a subprocess (the
// crash and clean-environment tests) and its parent agree on which set
// they are talking about without passing it over a pipe.
const setUUID = "3d5f7a91-2c4e-4b8d-9f1a-6e0c2b4d8a70"

// domainID is the one repository domain this suite declares.
const domainID = "production"

// deployment is one whole backupd installation: a configuration the
// product parsed, a source tree on disk, a Kopia repository, and a
// journal.
type deployment struct {
	root string

	// cfg is the resolved configuration, and set is the incremental
	// backup set inside it. Both come out of config.Load + Validate, so
	// every derived field (Engine, Repository, Consistency,
	// VerificationLevel, SourceIdentity, Retention) is the product's own
	// resolution rather than this file's guess.
	cfg *config.Config
	set config.BackupSet

	srcDir     string
	configPath string

	loc     backupengine.RepositoryLocation
	repo    backupengine.TreeRepository
	journal *state.Journal

	// closed is set by closeDeployment when a test hands the repository
	// and the journal to another process. The cleanups below read it, so
	// a deliberate early close does not become a spurious failure at the
	// end of the test.
	closed bool
}

// deploymentOptions are the few things a test varies.
type deploymentOptions struct {
	// root, when set, is an existing deployment root to reopen rather
	// than create. It is how the clean-environment and crash tests come
	// back to a repository a previous process wrote.
	root string

	// createRepository is false when the repository is expected to be
	// there already.
	createRepository bool

	// verificationLevel overrides the set's configured level.
	verificationLevel string
}

// newDeployment writes a configuration, creates a repository and opens a
// journal, and registers the teardown for all three.
func newDeployment(t *testing.T, opts deploymentOptions) *deployment {
	t.Helper()

	ctx := context.Background()

	root := opts.root
	if root == "" {
		root = t.TempDir()
	}

	d := &deployment{
		root:   root,
		srcDir: filepath.Join(root, "source"),
	}

	for _, dir := range []string{d.srcDir, filepath.Join(root, "state"), filepath.Join(root, "backups")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("preparing %s: %v", dir, err)
		}
	}

	passphrase := filepath.Join(root, "repo.passphrase")
	if err := os.WriteFile(passphrase, []byte(repositoryPassphrase+"\n"), 0o600); err != nil {
		t.Fatalf("writing the repository passphrase: %v", err)
	}

	level := opts.verificationLevel
	if level == "" {
		level = string(model.LevelContentSample)
	}

	d.configPath = filepath.Join(root, "config.yaml")
	if err := os.WriteFile(d.configPath, []byte(configYAML(root, passphrase, level)), 0o600); err != nil {
		t.Fatalf("writing config.yaml: %v", err)
	}

	cfg, err := config.Load(d.configPath)
	if err != nil {
		t.Fatalf("the product could not load the configuration this suite wrote: %v", err)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("the product refused the configuration this suite wrote: %v", err)
	}

	d.cfg = cfg
	d.set = incrementalSet(t, cfg)

	domain, err := model.NewRepositoryDomainID(domainID)
	if err != nil {
		t.Fatalf("NewRepositoryDomainID: %v", err)
	}

	d.loc = backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     domain,
		Root:       filepath.Join(root, "backups"),
		StateDir:   filepath.Join(root, "state", "repository"),
		Passphrase: secretref.Ref{File: passphrase},
	}

	eng := kopia.New()

	if opts.createRepository {
		if err := eng.CreateRepository(ctx, d.loc); err != nil {
			t.Fatalf("CreateRepository: %v", err)
		}
	}

	opened, err := eng.OpenRepository(ctx, d.loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	repo, ok := opened.(backupengine.TreeRepository)
	if !ok {
		t.Fatal("this repository cannot store a source tree as one snapshot, so no incremental set can run against it")
	}

	d.repo = repo

	t.Cleanup(func() {
		if d.closed {
			return
		}

		if err := repo.Close(context.Background()); err != nil {
			t.Errorf("closing the repository: %v", err)
		}
	})

	journal, err := state.Open(ctx, filepath.Join(root, "state", "backupd.db"))
	if err != nil {
		t.Fatalf("opening the journal: %v", err)
	}

	d.journal = journal

	t.Cleanup(func() {
		if d.closed {
			return
		}

		journal.Close() //nolint:errcheck // a closed test database has nothing left to report.
	})

	return d
}

// configYAML is the operator's file. It is written as text rather than
// marshalled from a struct for the reason backupd-tests renders its own
// fixtures as text: the file is part of the contract, and a struct
// marshalled back out would prove only that this package can round-trip
// its own types.
func configYAML(root, passphrase, level string) string {
	return fmt.Sprintf(`poll_interval: 15m
state:
  database: %[1]s/state/backupd.db
retention:
  timezone: UTC
  week_starts_on: monday
  daily_days: 7
  weekly_months: 3
  monthly_months: 12
  protect_last_known_good: true
repository_domains:
  - id: %[4]s
    description: Snapshots for this deployment
    isolation: shared
    passphrase:
      file: %[2]s
sources:
  - id: production
    backup_sets:
      - id: uploads
        local_path: %[1]s/local
        remote:
          type: local
        remote_path: %[1]s/remote
        include:
          - "*.dump"
        completion:
          strategy: rename
        stale_after: 30h
      - id: uploads-tree
        engine: kopia
        uuid: %[5]s
        repository_domain: %[4]s
        source_consistency: live_best_effort
        verification_level: %[3]s
        verification_sample_percent: 50
        remote:
          type: local
        remote_path: %[1]s/source
        stale_after: 30h
`, root, passphrase, level, domainID, setUUID)
}

// incrementalSet is the kopia-engine set in a resolved configuration.
//
// It is found by its engine rather than by its index, so a change to the
// fixture's shape fails with a sentence rather than by silently running
// the artifact set.
func incrementalSet(t *testing.T, cfg *config.Config) config.BackupSet {
	t.Helper()

	for _, src := range cfg.Sources {
		for _, bs := range src.BackupSets {
			if bs.Engine == model.EngineKopia {
				return bs
			}
		}
	}

	t.Fatal("the configuration this suite wrote carries no incremental backup set")

	return config.BackupSet{}
}

// transportSource is the set's source as the transport sees it, built the
// way production builds it: the configured remote plus the set's own root.
func (d *deployment) transportSource() transport.Source {
	return transport.Source{
		ID:   d.set.ID.String(),
		Type: d.set.Remote.Type,
		Root: d.set.RemotePath,
	}
}

// sourceAdapter is the production reading side, with every capability
// taken off the real rclone transport by type assertion exactly as
// app.Service.sourceAdapter does.
func (d *deployment) sourceAdapter(t *testing.T) *source.Adapter {
	t.Helper()

	adapter := rclone.New()

	a, err := source.New(source.Deps{
		Streamer:   adapter,
		Stater:     adapter,
		Enumerator: adapter,
	}, source.Options{
		Mode:   d.set.Consistency,
		Preset: model.PresetConservative,
	})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	return a
}

// run performs one whole snapshot run of the set through the real driver.
func (d *deployment) run(t *testing.T, runID string, opts snapshotlifecycle.VerificationOptions) (snapshotlifecycle.RunResult, error) {
	t.Helper()

	adapter := d.sourceAdapter(t)
	src := d.transportSource()

	runner := &snapshotlifecycle.Runner{Catalog: d.journal}

	return runner.Run(context.Background(), snapshotlifecycle.RunRequest{
		RunID:             runID,
		IdempotencyKey:    runID,
		Set:               d.set.ID,
		SetUUID:           setUUID,
		Engine:            d.set.Engine,
		Domain:            d.set.Repository.Domain,
		SourceIdentity:    d.set.SourceIdentity,
		Consistency:       d.set.Consistency,
		VerificationLevel: d.set.VerificationLevel,
		Verification:      opts,
		Source:            d.snapshotSource(),
		Description:       "backupd " + d.set.ID.String(),
		Repository:        d.repo,
		OpenTree: func(ctx context.Context) (snapshotlifecycle.SourceTree, error) {
			tree, err := adapter.OpenTree(ctx, src)
			if err != nil {
				return nil, fmt.Errorf("opening the source tree: %w", err)
			}

			return sourceTree{tree: tree}, nil
		},
	})
}

// snapshotSource is the set's identity in the repository's namespace,
// derived the way app.snapshotSource derives it: this product's own name,
// the domain as the user, and the set's stable source identity as the
// path, so a renamed host does not fork the lineage.
func (d *deployment) snapshotSource() backupengine.Source {
	return backupengine.Source{
		Host: "backupd",
		User: d.set.Repository.Domain.String(),
		Path: "/" + string(d.set.SourceIdentity),
	}
}

// sourceTree adapts the reading side's Tree onto the lifecycle's port,
// the same one-struct translation app.sourceTree performs.
type sourceTree struct {
	tree *source.Tree
}

func (t sourceTree) Root() backupengine.SourceDir { return t.tree.Root() }
func (t sourceTree) Err() error                   { return t.tree.Err() }
func (t sourceTree) Close() error                 { return t.tree.Close() }

// Report projects the reading side's census onto the four facts a run
// records. Complete is the source side's own verdict and is not
// re-derived here.
func (t sourceTree) Report() snapshotlifecycle.ScanReport {
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

// --- the source tree on disk ---------------------------------------------

// seed writes n files of size bytes each, spread over fanout
// subdirectories, with incompressible content derived from the index.
//
// Incompressible matters for every measurement in this package: a
// repository that compressed the payload away would satisfy "the second
// run wrote almost nothing" without deduplicating anything.
func seed(t *testing.T, dir string, n, size, fanout int) map[string]string {
	t.Helper()

	want := make(map[string]string, n)

	for i := range n {
		rel := filepath.FromSlash(seededPath(i, fanout))
		payload := randomBytes(t, size)
		writeFile(t, filepath.Join(dir, rel), payload)
		want[filepath.ToSlash(rel)] = sha256Of(payload)
	}

	return want
}

// seededPath is where seed puts the i'th file, as a slash path. It is one
// function because a test that recomputed it would eventually disagree
// with the seeding, and the disagreement would look like a restore
// returning the wrong file.
func seededPath(i, fanout int) string {
	return fmt.Sprintf("dir-%02d/file-%06d.bin", i%fanout, i)
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()

	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generating %d bytes of payload: %v", n, err)
	}

	return b
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// hashTree maps every regular file's slash-separated path under dir to
// the hex SHA-256 of its contents.
func hashTree(t *testing.T, dir string) map[string]string {
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

		out[filepath.ToSlash(rel)] = sha256Of(data)

		return nil
	})
	if err != nil {
		t.Fatalf("hashing the tree under %s: %v", dir, err)
	}

	return out
}

// repositoryBytes is the physical size of everything the repository
// holds, which is the measurement no report can fake.
func repositoryBytes(t *testing.T, loc backupengine.RepositoryLocation) int64 {
	t.Helper()

	dir, err := backupengine.ReservedLocalDir(loc.Root, loc.Domain)
	if err != nil {
		t.Fatalf("ReservedLocalDir: %v", err)
	}

	var total int64

	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
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

		total += info.Size()

		return nil
	})
	if err != nil {
		t.Fatalf("measuring the repository under %s: %v", dir, err)
	}

	return total
}
