// Command harness is the separate PROCESS half of #789's crash-recovery
// and clean-environment-restore evidence.
//
// Both of those claims are about what happens across a process boundary,
// and neither can be made from inside one. A crash is a process that
// stopped existing in the middle of a durable write: simulated by
// returning an error, it proves only that the error path works, and the
// defects it is meant to catch -- a manifest written but never recorded,
// a catalog row left mid-phase, a lock nobody released -- all live
// precisely in the gap a simulated crash steps over. A clean-environment
// restore is the claim that the repository plus its passphrase is
// sufficient: asserted in the test process, it is asserted by a process
// that has the source tree, the journal, the configuration and a warm
// repository cache in its own memory, so it cannot fail.
//
// So this is a real binary, built by the test, run as a child, and in the
// crash mode killed with a real SIGKILL from inside itself. The
// discipline is core/tests/crashmatrix/harness's, for the reasons its own
// doc gives; what is different here is the subject: an incremental
// snapshot run rather than an artifact transfer.
//
// # Two modes, and why the second one is deliberately ignorant
//
//	-mode=crash-mid-snapshot   load the deployment, start a snapshot run,
//	                           and SIGKILL this process the moment the
//	                           engine has been handed -kill-after-files
//	                           files. No manifest exists at that point and
//	                           the catalog row is mid-phase, which is
//	                           exactly the state reconciliation has to
//	                           resolve.
//
//	-mode=restore              restore one snapshot with NOTHING but the
//	                           repository location, the domain and the
//	                           passphrase file. It does not read
//	                           config.yaml, it does not open the journal
//	                           and it does not look at the source tree,
//	                           because the claim under test is that an
//	                           operator whose deployment burned down can
//	                           restore from the repository and the secret
//	                           alone.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

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

func main() {
	var (
		mode       = flag.String("mode", "", "crash-mid-snapshot or restore")
		root       = flag.String("root", "", "the deployment root (crash-mid-snapshot)")
		runID      = flag.String("run-id", "", "the snapshot run id (crash-mid-snapshot)")
		killAfter  = flag.Int("kill-after-files", 0, "SIGKILL this process once this many files have been handed to the engine")
		repoRoot   = flag.String("repository-root", "", "the backup root holding the repository (restore)")
		domain     = flag.String("domain", "", "the repository domain (restore)")
		passphrase = flag.String("passphrase-file", "", "the file holding the repository passphrase (restore)")
		stateDir   = flag.String("state-dir", "", "a writable directory for the repository's own connection state (restore)")
		snapshotID = flag.String("snapshot", "", "the snapshot to restore (restore)")
		target     = flag.String("to", "", "the directory to restore into (restore)")
	)

	flag.Parse()

	var err error

	switch *mode {
	case "crash-mid-snapshot":
		err = crashMidSnapshot(*root, *runID, *killAfter)
	case "restore":
		err = restore(*repoRoot, *domain, *passphrase, *stateDir, *snapshotID, *target)
	default:
		err = fmt.Errorf("unknown -mode %q", *mode)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		os.Exit(1)
	}
}

// selfDestruct ends this process the way a power cut does: no deferred
// function runs, no buffered write is flushed, no lock is released, and
// nothing this program's own code could do to soften the landing gets a
// chance to. The infinite loop after the signal guarantees this goroutine
// makes no further forward progress -- in particular no further journal
// or repository write -- while the kernel finishes tearing the process
// down.
func selfDestruct(reason string) {
	fmt.Fprintf(os.Stderr, "E2E_SELF_KILL: %s\n", reason)
	_ = os.Stderr.Sync()
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)

	for {
	}
}

// crashMidSnapshot runs one real snapshot run and dies inside it.
func crashMidSnapshot(root, runID string, killAfter int) error {
	if root == "" || runID == "" || killAfter <= 0 {
		return errors.New("crash-mid-snapshot needs -root, -run-id and a positive -kill-after-files")
	}

	ctx := context.Background()

	cfg, err := config.Load(filepath.Join(root, "config.yaml"))
	if err != nil {
		return fmt.Errorf("loading the deployment's configuration: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validating the deployment's configuration: %w", err)
	}

	set, ok := incrementalSet(cfg)
	if !ok {
		return errors.New("this deployment declares no incremental backup set")
	}

	loc, err := locationFor(root, set.Repository.Domain.String(), filepath.Join(root, "repo.passphrase"), filepath.Join(root, "state", "repository"))
	if err != nil {
		return err
	}

	opened, err := kopia.New().OpenRepository(ctx, loc)
	if err != nil {
		return fmt.Errorf("opening the repository: %w", err)
	}

	repo, ok := opened.(backupengine.TreeRepository)
	if !ok {
		return errors.New("this repository cannot store a source tree as one snapshot")
	}

	journal, err := state.Open(ctx, filepath.Join(root, "state", "backupd.db"))
	if err != nil {
		return fmt.Errorf("opening the journal: %w", err)
	}

	adapter := rclone.New()

	reader, err := source.New(source.Deps{Streamer: adapter, Stater: adapter, Enumerator: adapter}, source.Options{
		Mode:   set.Consistency,
		Preset: model.PresetConservative,
	})
	if err != nil {
		return fmt.Errorf("source.New: %w", err)
	}

	src := transport.Source{ID: set.ID.String(), Type: set.Remote.Type, Root: set.RemotePath}

	runner := &snapshotlifecycle.Runner{Catalog: journal}

	handed := 0

	_, err = runner.Run(ctx, snapshotlifecycle.RunRequest{
		RunID:             runID,
		IdempotencyKey:    runID,
		Set:               set.ID,
		SetUUID:           set.UUID,
		Engine:            set.Engine,
		Domain:            set.Repository.Domain,
		SourceIdentity:    set.SourceIdentity,
		Consistency:       set.Consistency,
		VerificationLevel: set.VerificationLevel,
		Source: backupengine.Source{
			Host: "backupd",
			User: set.Repository.Domain.String(),
			Path: "/" + string(set.SourceIdentity),
		},
		Description: "backupd " + set.ID.String(),
		Repository:  repo,
		OpenTree: func(ctx context.Context) (snapshotlifecycle.SourceTree, error) {
			tree, err := reader.OpenTree(ctx, src)
			if err != nil {
				return nil, fmt.Errorf("opening the source tree: %w", err)
			}

			return &killingTree{inner: lifecycleTree{tree: tree}, budget: killAfter, handed: &handed}, nil
		},
	})

	// Reaching here at all is the failure this mode has to report: the
	// run finished without the process dying, so whatever the parent
	// asserts about recovery would be asserting about a clean run.
	return fmt.Errorf("the run returned (err=%v) after handing %d files to the engine; it was supposed to be killed after %d", err, handed, killAfter)
}

// restore is the clean-environment restore: repository, domain,
// passphrase, snapshot, target. Nothing else.
func restore(repoRoot, domain, passphraseFile, stateDir, snapshotID, target string) error {
	if repoRoot == "" || domain == "" || passphraseFile == "" || stateDir == "" || snapshotID == "" || target == "" {
		return errors.New("restore needs -repository-root, -domain, -passphrase-file, -state-dir, -snapshot and -to")
	}

	loc, err := locationForRoot(repoRoot, domain, passphraseFile, stateDir)
	if err != nil {
		return err
	}

	ctx := context.Background()

	repo, err := kopia.New().OpenRepository(ctx, loc)
	if err != nil {
		return fmt.Errorf("opening the repository with nothing but its location and passphrase: %w", err)
	}

	defer repo.Close(context.Background()) //nolint:errcheck // the report is already on stdout by then.

	report, err := repo.Restore(ctx, backupengine.SnapshotID(snapshotID), backupengine.RestoreRequest{
		TargetPath: target,
		Conflict:   backupengine.ConflictRefuse,
		SkipOwners: true,
	})
	if err != nil {
		return fmt.Errorf("restoring %s: %w", snapshotID, err)
	}

	// The report goes to stdout as JSON so the parent asserts on the
	// child's own numbers rather than only on the files that appeared.
	out, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encoding the restore report: %w", err)
	}

	fmt.Println(string(out))

	return nil
}

func locationFor(root, domain, passphraseFile, stateDir string) (backupengine.RepositoryLocation, error) {
	return locationForRoot(filepath.Join(root, "backups"), domain, passphraseFile, stateDir)
}

func locationForRoot(backupRoot, domain, passphraseFile, stateDir string) (backupengine.RepositoryLocation, error) {
	id, err := model.NewRepositoryDomainID(domain)
	if err != nil {
		return backupengine.RepositoryLocation{}, fmt.Errorf("NewRepositoryDomainID(%q): %w", domain, err)
	}

	return backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     id,
		Root:       backupRoot,
		StateDir:   stateDir,
		Passphrase: secretref.Ref{File: passphraseFile},
	}, nil
}

func incrementalSet(cfg *config.Config) (config.BackupSet, bool) {
	for _, src := range cfg.Sources {
		for _, bs := range src.BackupSets {
			if bs.Engine == model.EngineKopia {
				return bs, true
			}
		}
	}

	return config.BackupSet{}, false
}

// lifecycleTree is app.sourceTree's one-struct translation of the reading
// side's Tree onto the lifecycle's port, repeated here because this is a
// separate program and importing core/internal/app to get it would drag a
// whole Service in.
type lifecycleTree struct {
	tree *source.Tree
}

func (t lifecycleTree) Root() backupengine.SourceDir { return t.tree.Root() }
func (t lifecycleTree) Err() error                   { return t.tree.Err() }
func (t lifecycleTree) Close() error                 { return t.tree.Close() }

func (t lifecycleTree) Report() snapshotlifecycle.ScanReport {
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

// --- the killing decorator ------------------------------------------------

// killingTree is the seam that makes the crash precise. It composes the
// already-exported SourceTree interface and nothing else: the run driver,
// the reading side and the engine are all untouched, which is what makes
// the crash a statement about them rather than about a test hook inside
// them.
//
// The kill happens when an entry is HANDED to the engine, before its
// bytes have been read, so the process dies with a catalog row mid-phase
// and no manifest in the repository at all -- the state a power cut
// during a nightly backup actually leaves.
type killingTree struct {
	inner  snapshotlifecycle.SourceTree
	budget int
	handed *int
}

func (t *killingTree) Root() backupengine.SourceDir {
	return &killingDir{tree: t, inner: t.inner.Root()}
}

func (t *killingTree) Report() snapshotlifecycle.ScanReport { return t.inner.Report() }
func (t *killingTree) Err() error                           { return t.inner.Err() }
func (t *killingTree) Close() error                         { return t.inner.Close() }

type killingDir struct {
	tree  *killingTree
	inner backupengine.SourceDir
}

func (d *killingDir) Open(ctx context.Context) (backupengine.SourceDirIterator, error) {
	it, err := d.inner.Open(ctx)
	if err != nil {
		return nil, err
	}

	return &killingIterator{tree: d.tree, inner: it}, nil
}

type killingIterator struct {
	tree  *killingTree
	inner backupengine.SourceDirIterator
}

func (it *killingIterator) Next(ctx context.Context) (backupengine.SourceEntry, bool, error) {
	entry, ok, err := it.inner.Next(ctx)
	if !ok || err != nil {
		return entry, ok, err
	}

	if entry.Dir != nil {
		entry.Dir = &killingDir{tree: it.tree, inner: entry.Dir}

		return entry, true, nil
	}

	*it.tree.handed++

	if *it.tree.handed >= it.tree.budget {
		selfDestruct(fmt.Sprintf("%d files handed to the engine, mid-snapshot", *it.tree.handed))
	}

	return entry, true, nil
}

func (it *killingIterator) Close() error { return it.inner.Close() }
