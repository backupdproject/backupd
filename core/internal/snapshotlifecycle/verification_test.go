package snapshotlifecycle_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/snapshotlifecycle"
	"github.com/backupdproject/backupd/core/internal/state"
)

// The success policy, tested where it is decided: what depth one run
// asks for, what it must prove before its snapshot is advertised, and
// what a cadence does with the evidence of previous runs.
//
// These run against the real journal for the same reason the rest of this
// package's tests do -- the cadence reads catalog rows, and a fake
// catalog would be a second implementation of the thing under test.

// drillOptions is a verification policy with somewhere to restore to.
func drillOptions(t *testing.T) snapshotlifecycle.VerificationOptions {
	t.Helper()

	return snapshotlifecycle.VerificationOptions{DrillDir: filepath.Join(t.TempDir(), "drills")}
}

// lastRequest is the verification request the repository was handed
// most recently, which is how these tests assert what depth was ASKED
// for rather than only what ended up on the row.
func lastRequest(t *testing.T, repo *fakeRepository) backupengine.VerifyRequest {
	t.Helper()

	if len(repo.verifyRequests) == 0 {
		t.Fatal("the repository was never asked to verify anything")
	}

	return repo.verifyRequests[len(repo.verifyRequests)-1]
}

// storing makes a fake repository hand back a named manifest.
//
// Two runs of the same set in one test have to produce two different
// manifests: the catalog holds a unique index on (domain, snapshot id),
// because one repository snapshot attributed to two runs means one of
// those attributions is wrong. A test that reused an id would be testing
// that constraint instead of what it came for.
func storing(repo *fakeRepository, id string) *fakeRepository {
	repo.tree = func(req backupengine.TreeSnapshotRequest) (backupengine.TreeSnapshotInfo, error) {
		return backupengine.TreeSnapshotInfo{
			SnapshotInfo: backupengine.SnapshotInfo{
				ID:          backupengine.SnapshotID(id),
				Source:      req.Source,
				Files:       3,
				Directories: 1,
				Bytes:       3000,
			},
			SourceBytesRead:        3000,
			RepositoryBytesWritten: 900,
		}, nil
	}

	return repo
}

// TestRun_AsksForTheLevelItsSetIsConfiguredFor is the floor of the whole
// policy: a run verifies what its set was configured for, not what the
// engine finds convenient.
func TestRun_AsksForTheLevelItsSetIsConfiguredFor(t *testing.T) {
	t.Parallel()

	for _, level := range []model.VerificationLevel{
		model.LevelStructural,
		model.LevelContentSample,
		model.LevelContentFull,
		model.LevelRestoreDrill,
	} {
		t.Run(string(level), func(t *testing.T) {
			t.Parallel()

			j := journal(t)
			set := setID(t, "postgres")
			repo := newFakeRepository()

			req := request(set, repo, &fakeTree{report: completeScan()})
			req.VerificationLevel = level
			req.Verification = drillOptions(t)
			req.Verification.SamplePercent = 17

			res, err := runner(t, j).Run(context.Background(), req)
			if err != nil {
				t.Fatalf("running a set configured for %s: %v", level, err)
			}

			if got := lastRequest(t, repo).Level; got != level {
				t.Errorf("the run asked the repository for %q; the set is configured for %q", got, level)
			}

			if got := lastRequest(t, repo).SamplePercent; got != 17 {
				t.Errorf("the run asked for a %d%% sample; the policy says 17%%", got)
			}

			if res.VerificationAchieved != level {
				t.Errorf("the row records %q as proved, and the repository proved %q", res.VerificationAchieved, level)
			}

			if !res.LastKnownGood {
				t.Errorf("a run that proved its configured level %q is not the set's last-known-good restore point", level)
			}
		})
	}
}

// TestRun_AVerificationShallowerThanTheSetRequiresIsNotARestorePoint is
// the rule that makes last-known-good mean something.
//
// A run that proved SOMETHING is the dangerous case, not a run that
// failed: the snapshot exists, the verification returned no error, and
// the only thing wrong is that the check was shallower than the set
// asked for. Advertising it would tell an operator who configured
// nightly restore drills that their restore point has been restore
// tested, and the first time they would find out otherwise is during a
// restore.
func TestRun_AVerificationShallowerThanTheSetRequiresIsNotARestorePoint(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")

	// An older run that really did prove the configured level, so the
	// test can also show the restore point it holds is not taken away by
	// the newer run that did not.
	good := request(set, newFakeRepository(), &fakeTree{report: completeScan()})
	good.VerificationLevel = model.LevelContentFull

	first, err := runner(t, j).Run(context.Background(), good)
	if err != nil {
		t.Fatalf("the first run: %v", err)
	}

	repo := storing(newFakeRepository(), "snap-2")
	repo.achieved = model.LevelStructural

	shallow := request(set, repo, &fakeTree{report: completeScan()})
	shallow.RunID, shallow.IdempotencyKey = "run-2", "key-2"
	shallow.VerificationLevel = model.LevelContentFull

	res, err := runner(t, j).Run(context.Background(), shallow)
	if err == nil {
		t.Fatal("a run whose verification proved less than its set requires succeeded")
	}

	if res.Succeeded() {
		t.Errorf("the run reached %s", res.Phase)
	}

	if res.VerificationAchieved != "" {
		t.Errorf("the failed run recorded %q as proved; a run that did not pass proves nothing", res.VerificationAchieved)
	}

	if !strings.Contains(res.Reason, string(model.LevelContentFull)) || !strings.Contains(res.Reason, string(model.LevelStructural)) {
		t.Errorf("the recorded reason is %q; it must name what was proved and what was required", res.Reason)
	}

	lkg, err := j.LastKnownGoodSnapshot(context.Background(), set)
	if err != nil {
		t.Fatalf("reading last-known-good: %v", err)
	}

	if lkg.RunID != first.RunID {
		t.Errorf("last-known-good moved to %q; the older run %q is the only one that proved the configured level",
			lkg.RunID, first.RunID)
	}
}

// TestRun_APeriodicFullReadIsDueUntilOneActuallySucceeds is the cadence,
// and the reason it is measured from achieved levels rather than from a
// counter.
//
// A deployment whose weekly full read keeps failing must stay overdue. A
// run-counting cadence marks it done on the attempt, so the deep check
// silently stops happening on precisely the repository that is failing
// it.
func TestRun_APeriodicFullReadIsDueUntilOneActuallySucceeds(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	j := journal(t)
	set := setID(t, "postgres")

	// A previous run that reached SUCCESS proving only structural: the
	// cadence must not read it as a full check.
	seed(t, j, set, "run-0",
		[]state.SnapshotPhase{
			state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted,
			state.PhaseVerification, state.PhaseCatalogCommit, state.PhaseSuccess,
		},
		map[state.SnapshotPhase]state.SnapshotRunUpdate{
			state.PhaseManifestCommitted: {SnapshotID: new("snap-0")},
			state.PhaseCatalogCommit: {
				VerificationStatus:        new("passed"),
				VerificationLevelAchieved: new(string(model.LevelStructural)),
			},
		})

	repo := storing(newFakeRepository(), "snap-1")
	req := request(set, repo, &fakeTree{report: completeScan()})
	req.RunID, req.IdempotencyKey = "run-1", "key-1"
	req.VerificationLevel = model.LevelStructural
	req.Verification = snapshotlifecycle.VerificationOptions{FullEvery: time.Hour}

	if _, err := runner(t, j).Run(ctx, req); err != nil {
		t.Fatalf("the run whose full read was due: %v", err)
	}

	if got := lastRequest(t, repo).Level; got != model.LevelContentFull {
		t.Fatalf("a set with no successful full read asked for %q", got)
	}

	// And now that one has succeeded, the next run inside the window is
	// back to the configured level: the cadence is a period, not a
	// ratchet that makes every later run expensive.
	next := storing(newFakeRepository(), "snap-2")
	again := request(set, next, &fakeTree{report: completeScan()})
	again.RunID, again.IdempotencyKey = "run-2", "key-2"
	again.VerificationLevel = model.LevelStructural
	again.Verification = snapshotlifecycle.VerificationOptions{FullEvery: time.Hour}

	if _, err := runner(t, j).Run(ctx, again); err != nil {
		t.Fatalf("the run after the full read: %v", err)
	}

	if got := lastRequest(t, next).Level; got != model.LevelStructural {
		t.Errorf("the run after a successful full read asked for %q, want the configured %q",
			got, model.LevelStructural)
	}
}

// TestRun_ACadenceNeverAsksForLessThanTheSetIsConfiguredFor is the
// direction rule. An escalation raises the bar; nothing lowers it.
func TestRun_ACadenceNeverAsksForLessThanTheSetIsConfiguredFor(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	repo := newFakeRepository()

	req := request(set, repo, &fakeTree{report: completeScan()})
	req.VerificationLevel = model.LevelContentFull

	// A drill cadence that is nowhere near due, over a set configured for
	// a full read: the run must still read everything.
	req.Verification = drillOptions(t)
	req.Verification.DrillEvery = 365 * 24 * time.Hour

	seedAchieved(t, j, set, "run-0", model.LevelRestoreDrill)

	if _, err := runner(t, j).Run(context.Background(), req); err != nil {
		t.Fatalf("running: %v", err)
	}

	if got := lastRequest(t, repo).Level; got != model.LevelContentFull {
		t.Errorf("the run asked for %q; its set is configured for %q and no deeper rung was due", got, model.LevelContentFull)
	}
}

// TestRun_ASetConfiguredForRestoreDrillsIsRefusedWithNowhereToRestoreTo
// is a refusal that happens BEFORE the run's first durable write.
//
// The alternative is the expensive shape of the same mistake: read the
// whole source, store a snapshot, and then fail the verification for a
// configuration reason that was knowable before anything was dialled.
func TestRun_ASetConfiguredForRestoreDrillsIsRefusedWithNowhereToRestoreTo(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	repo := newFakeRepository()
	tree := &fakeTree{report: completeScan()}

	req := request(set, repo, tree)
	req.VerificationLevel = model.LevelRestoreDrill

	if _, err := runner(t, j).Run(context.Background(), req); err == nil {
		t.Fatal("a set configured for restore drills ran with nowhere to restore to")
	}

	if repo.treeCalls != 0 {
		t.Errorf("the source was read %d time(s) before the refusal", repo.treeCalls)
	}

	if runs, err := j.ListSnapshotRuns(context.Background(), set, 10); err != nil || len(runs) != 0 {
		t.Errorf("the refusal left %d row(s) behind (err %v); nothing should have to reconcile a run that never started", len(runs), err)
	}
}

// TestRun_ADrillCadenceWithNowhereToRestoreToIsDroppedRatherThanFailing
// is the other half of that decision, and the asymmetry is deliberate.
//
// A configured level is an operator's instruction and is refused when it
// cannot be honoured. A CADENCE is this package's own idea about when to
// look harder, and an idea of ours must never be able to fail somebody's
// backup: the run proceeds at the level the set actually requires.
func TestRun_ADrillCadenceWithNowhereToRestoreToIsDroppedRatherThanFailing(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	repo := newFakeRepository()

	req := request(set, repo, &fakeTree{report: completeScan()})
	req.VerificationLevel = model.LevelContentSample
	req.Verification = snapshotlifecycle.VerificationOptions{DrillEvery: time.Hour}

	res, err := runner(t, j).Run(context.Background(), req)
	if err != nil {
		t.Fatalf("running: %v", err)
	}

	if got := lastRequest(t, repo).Level; got != model.LevelContentSample {
		t.Errorf("the run asked for %q; with nowhere to drill it should ask for its configured %q", got, model.LevelContentSample)
	}

	if !res.Succeeded() {
		t.Errorf("the run failed over a cadence nobody could honour: %s", res.Reason)
	}
}

// TestRun_ADrillRestoresIntoItsOwnDirectoryAndKeepsItOnlyWhenItFails is
// the evidence rule.
//
// A passing drill has left a second copy of a backup on a disk somebody
// pays for, and a failing one has left the only available description of
// what a restore of this snapshot actually produces. So the first is
// removed and the second is kept, and both directories are named for the
// run so two runs can never restore into each other's output.
func TestRun_ADrillRestoresIntoItsOwnDirectoryAndKeepsItOnlyWhenItFails(t *testing.T) {
	t.Parallel()

	t.Run("a passing drill is cleaned up", func(t *testing.T) {
		t.Parallel()

		j := journal(t)
		set := setID(t, "postgres")
		repo := newFakeRepository()

		req := request(set, repo, &fakeTree{report: completeScan()})
		req.VerificationLevel = model.LevelRestoreDrill
		req.Verification = drillOptions(t)

		if _, err := runner(t, j).Run(context.Background(), req); err != nil {
			t.Fatalf("running: %v", err)
		}

		target := lastRequest(t, repo).RestoreTarget
		if target == "" {
			t.Fatal("the drill was asked for with no restore target")
		}

		if !strings.Contains(target, req.RunID) {
			t.Errorf("the drill restored into %q, which is not named for run %q", target, req.RunID)
		}

		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Errorf("the passing drill's restored tree is still at %s (stat err %v)", target, err)
		}
	})

	t.Run("a failing drill keeps its evidence", func(t *testing.T) {
		t.Parallel()

		j := journal(t)
		set := setID(t, "postgres")
		repo := newFakeRepository()

		// The drill itself succeeds at restoring and then proves less
		// than the set requires, which is the case that leaves output
		// on the disk AND fails the run.
		repo.achieved = model.LevelContentFull

		req := request(set, repo, &fakeTree{report: completeScan()})
		req.VerificationLevel = model.LevelRestoreDrill
		req.Verification = drillOptions(t)

		if _, err := runner(t, j).Run(context.Background(), req); err == nil {
			t.Fatal("a drill that proved only a content read succeeded")
		}

		target := lastRequest(t, repo).RestoreTarget
		if _, err := os.Stat(target); err != nil {
			t.Errorf("the failed drill's output at %s is gone (%v); it is the only evidence of what the restore produced", target, err)
		}
	})
}

// TestReconcile_RecoveryProvesTheLevelTheRowWasAdmittedUnder pins what a
// crash-recovery pass verifies.
//
// Two wrong answers are available and both are worse. Re-verifying at
// today's configured level would fail a good snapshot whenever somebody
// raises a set's level between the crash and the recovery; escalating on
// the cadence would turn one interrupted run into a full restore during
// the recovery of a deployment that has just come back up.
func TestReconcile_RecoveryProvesTheLevelTheRowWasAdmittedUnder(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")

	seed(t, j, set, "run-1",
		[]state.SnapshotPhase{state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted},
		map[state.SnapshotPhase]state.SnapshotRunUpdate{
			state.PhaseManifestCommitted: {SnapshotID: new("snap-1")},
		})

	repo := newFakeRepository()
	repo.snapshots["snap-1"] = backupengine.SnapshotInfo{ID: "snap-1", Source: testSource, Files: 3, Bytes: 4096}

	rec := reconciler(t, j)
	rec.Verification = snapshotlifecycle.VerificationOptions{
		SamplePercent: 9,

		// A cadence a recovery pass must ignore.
		FullEvery:  time.Nanosecond,
		DrillEvery: time.Nanosecond,
		DrillDir:   filepath.Join(t.TempDir(), "drills"),
	}

	if _, err := rec.Reconcile(context.Background(), reconcileRequest(set, repo)); err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	got := lastRequest(t, repo)

	// seed admits every row at the structural level.
	if got.Level != model.LevelStructural {
		t.Errorf("the recovery pass verified at %q; the row was admitted at %q", got.Level, model.LevelStructural)
	}

	if got.SamplePercent != 9 {
		t.Errorf("the recovery pass asked for a %d%% sample; the reconciler is configured for 9%%", got.SamplePercent)
	}
}

// TestReconcile_ARecoveryDrillRestoresIntoAFreshDirectory is the
// crash-during-restore boundary, and the defect it pins failed VALID
// backups.
//
// A drill run that died mid-restore leaves partial files in its target.
// The recovery pass restores with Overwrite=false -- deliberately, since
// a verification that can overwrite is a verification that can destroy
// data -- so a retry into the SAME directory errors on the first file
// that is already there, and an intact committed snapshot is moved to
// FAILED over the leftovers of the attempt that crashed. Every recovery
// therefore gets a fresh attempt directory beside the old one.
func TestReconcile_ARecoveryDrillRestoresIntoAFreshDirectory(t *testing.T) {
	t.Parallel()

	// recovery seeds a run that crashed after committing its manifest,
	// with the wreckage of an interrupted restore already on the disk,
	// and reconciles it.
	recovery := func(t *testing.T, repo *fakeRepository) (drills, abandoned string, err error) {
		t.Helper()

		j := journal(t)
		set := setID(t, "postgres")

		seedConfigured(t, j, set, "run-1", model.LevelRestoreDrill,
			[]state.SnapshotPhase{state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted},
			map[state.SnapshotPhase]state.SnapshotRunUpdate{
				state.PhaseManifestCommitted: {SnapshotID: new("snap-1")},
			})

		repo.snapshots["snap-1"] = backupengine.SnapshotInfo{ID: "snap-1", Source: testSource, Files: 3, Bytes: 4096}

		drills = filepath.Join(t.TempDir(), "drills")
		abandoned = filepath.Join(drills, "restore-drill-run-1")

		// What a crash in the middle of a restore leaves: a real
		// directory holding some of the files, truncated.
		partial := filepath.Join(abandoned, "dir-0", "file-0.bin")
		if mkErr := os.MkdirAll(filepath.Dir(partial), 0o750); mkErr != nil {
			t.Fatalf("creating the crashed attempt's directory: %v", mkErr)
		}

		if wErr := os.WriteFile(partial, []byte("half a file"), 0o600); wErr != nil {
			t.Fatalf("writing the crashed attempt's partial file: %v", wErr)
		}

		rec := reconciler(t, j)
		rec.Verification = snapshotlifecycle.VerificationOptions{DrillDir: drills}

		_, err = rec.Reconcile(context.Background(), reconcileRequest(set, repo))

		return drills, abandoned, err
	}

	t.Run("the retry never restores into the crashed attempt's directory", func(t *testing.T) {
		t.Parallel()

		repo := newFakeRepository()

		drills, abandoned, err := recovery(t, repo)
		if err != nil {
			t.Fatalf("reconciling an interrupted drill run: %v", err)
		}

		target := lastRequest(t, repo).RestoreTarget
		if target == abandoned {
			t.Errorf("the recovery restored into %s, the directory the crashed attempt left partial files in; with Overwrite=false that fails an intact snapshot on the first file that is already there", target)
		}

		if !strings.HasPrefix(target, drills+string(os.PathSeparator)) || !strings.Contains(target, "run-1") {
			t.Errorf("the recovery restored into %q, which is not an attempt directory of run-1 under %s", target, drills)
		}

		// A proven snapshot needs no evidence, so the successful
		// recovery takes the abandoned wreckage with it rather than
		// leaving a partial copy of a backup on the disk for ever.
		if _, statErr := os.Stat(abandoned); !os.IsNotExist(statErr) {
			t.Errorf("the crashed attempt's directory %s survived a recovery that PROVED the snapshot (stat err %v)", abandoned, statErr)
		}
	})

	t.Run("a failed recovery keeps every attempt as evidence", func(t *testing.T) {
		t.Parallel()

		repo := newFakeRepository()

		// The drill restores and then proves less than the row was
		// admitted under, which is the outcome that fails the run with
		// output on the disk.
		repo.achieved = model.LevelContentFull

		_, abandoned, err := recovery(t, repo)
		if err != nil {
			t.Fatalf("reconciling: %v", err)
		}

		if _, statErr := os.Stat(abandoned); statErr != nil {
			t.Errorf("the crashed attempt's directory %s is gone (%v); after a recovery that could not prove the snapshot it is part of the evidence an operator has to look at", abandoned, statErr)
		}

		if target := lastRequest(t, repo).RestoreTarget; target != "" {
			if _, statErr := os.Stat(target); statErr != nil && !os.IsNotExist(statErr) {
				t.Errorf("stat of the failed attempt's directory %s: %v", target, statErr)
			}
		}
	})
}

// seedAchieved puts a finished, successful run on the catalog that
// proved the given level, which is what a cadence reads.
func seedAchieved(t *testing.T, j *state.Journal, set model.BackupSetID, runID string, achieved model.VerificationLevel) {
	t.Helper()

	seed(t, j, set, runID,
		[]state.SnapshotPhase{
			state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted,
			state.PhaseVerification, state.PhaseCatalogCommit, state.PhaseSuccess,
		},
		map[state.SnapshotPhase]state.SnapshotRunUpdate{
			state.PhaseManifestCommitted: {SnapshotID: new(runID + "-snap")},
			state.PhaseCatalogCommit: {
				VerificationStatus:        new("passed"),
				VerificationLevelAchieved: new(string(achieved)),
			},
		})
}
