package snapshotlifecycle_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/snapshotlifecycle"
	"github.com/backupdproject/backupd/core/internal/state"
)

// One test per crash boundary #783 names, plus the two rules that hold
// across all of them: nothing is ever deleted, and a failed newer
// snapshot never takes the older one's place.
//
// Every test builds the boundary the way a crash builds it -- a row
// advanced to the phase a process died in, and a repository that either
// holds the manifest or does not -- because that is the only way to
// assert what recovery does with a state no code path ever creates
// deliberately.

const testDomain = "vault"

var testSource = backupengine.Source{Host: "nas", User: "backupd", Path: "/srv/data"}

func reconciler(t *testing.T, j *state.Journal) *snapshotlifecycle.Reconciler {
	t.Helper()

	return &snapshotlifecycle.Reconciler{Catalog: j, Now: tick()}
}

func reconcileRequest(set model.BackupSetID, repo snapshotlifecycle.Repository) snapshotlifecycle.ReconcileRequest {
	return snapshotlifecycle.ReconcileRequest{
		Set:            set,
		Domain:         model.RepositoryDomainID(testDomain),
		Source:         testSource,
		SourceIdentity: model.SourceIdentity("ab12cd34"),
		Repository:     repo,
	}
}

// seed puts a row at the phase a crash left it in, walking the same
// legal edges a live run would have walked to get there.
func seed(t *testing.T, j *state.Journal, set model.BackupSetID, runID string, phases []state.SnapshotPhase, upd map[state.SnapshotPhase]state.SnapshotRunUpdate) state.SnapshotRun {
	t.Helper()

	at := time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)

	outcome, err := j.BeginSnapshotRun(context.Background(), state.SnapshotRunRequest{
		RunID:             runID,
		IdempotencyKey:    runID,
		Set:               set,
		Engine:            model.EngineKopia.String(),
		Domain:            testDomain,
		SourceIdentity:    "ab12cd34",
		ConsistencyMode:   string(model.ModeLiveBestEffort),
		VerificationLevel: string(model.LevelStructural),
		StartedAt:         at,
	})
	if err != nil {
		t.Fatalf("seeding run %s: %v", runID, err)
	}

	for i, phase := range phases {
		u := upd[phase]
		u.At = at.Add(time.Duration(i+1) * time.Second)

		if err := j.AdvanceSnapshotRun(context.Background(), runID, phase, u); err != nil {
			t.Fatalf("seeding %s at %s: %v", runID, phase, err)
		}
	}

	run, err := j.GetSnapshotRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("reading seeded run %s: %v", runID, err)
	}

	_ = outcome

	return run
}

func phaseOf(t *testing.T, j *state.Journal, runID string) state.SnapshotPhase {
	t.Helper()

	run, err := j.GetSnapshotRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("reading run %s: %v", runID, err)
	}

	return run.Phase
}

func verdictOf(t *testing.T, rep snapshotlifecycle.ReconcileReport, kind snapshotlifecycle.VerdictKind) snapshotlifecycle.Verdict {
	t.Helper()

	for _, v := range rep.Verdicts {
		if v.Kind == kind {
			return v
		}
	}

	t.Fatalf("no %s verdict in %+v", kind, rep.Verdicts)

	return snapshotlifecycle.Verdict{}
}

func TestReconcile_ARunInterruptedBeforeAnyUploadIsFailed(t *testing.T) {
	t.Parallel()

	for _, phase := range []state.SnapshotPhase{state.PhasePending, state.PhaseSourceScan} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()

			j := journal(t)
			set := setID(t, "postgres")

			var phases []state.SnapshotPhase
			if phase != state.PhasePending {
				phases = append(phases, phase)
			}

			seed(t, j, set, "run-1", phases, nil)

			rep, err := reconciler(t, j).Reconcile(context.Background(), reconcileRequest(set, newFakeRepository()))
			if err != nil {
				t.Fatalf("reconciling: %v", err)
			}

			verdictOf(t, rep, snapshotlifecycle.VerdictAbandonedBeforeUpload)

			if got := phaseOf(t, j, "run-1"); got != state.PhaseFailed {
				t.Errorf("run left at %s, want %s", got, state.PhaseFailed)
			}
		})
	}
}

func TestReconcile_ARunThatDiedDuringItsUploadWithNoManifestIsFailed(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	seed(t, j, set, "run-1", []state.SnapshotPhase{state.PhaseSourceScan, state.PhaseSnapshotWrite}, nil)

	rep, err := reconciler(t, j).Reconcile(context.Background(), reconcileRequest(set, newFakeRepository()))
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	verdictOf(t, rep, snapshotlifecycle.VerdictAbandonedUpload)

	if got := phaseOf(t, j, "run-1"); got != state.PhaseFailed {
		t.Errorf("run left at %s, want %s", got, state.PhaseFailed)
	}

	if rep.LastKnownGoodRunID != "" {
		t.Errorf("an abandoned upload produced last-known-good %q", rep.LastKnownGoodRunID)
	}
}

func TestReconcile_AManifestTheCatalogNeverRecordedIsAdoptedAndProvenRatherThanDeleted(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	run := seed(t, j, set, "run-1", []state.SnapshotPhase{state.PhaseSourceScan, state.PhaseSnapshotWrite}, nil)

	// The manifest the dead process saved: after the run started, under
	// this set's source, claimed by nothing.
	repo := newFakeRepository()
	repo.snapshots["snap-orphan"] = backupengine.SnapshotInfo{
		ID: "snap-orphan", Source: testSource, Files: 3, Directories: 1, Bytes: 4096,
		Start: run.StartedAt.Add(time.Second), End: run.StartedAt.Add(2 * time.Second),
	}

	rep, err := reconciler(t, j).Reconcile(context.Background(), reconcileRequest(set, repo))
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	adopted := verdictOf(t, rep, snapshotlifecycle.VerdictManifestAdopted)
	if adopted.SnapshotID != "snap-orphan" {
		t.Errorf("adopted snapshot %q, want snap-orphan", adopted.SnapshotID)
	}

	verdictOf(t, rep, snapshotlifecycle.VerdictVerified)

	after, err := j.GetSnapshotRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}

	if after.Phase != state.PhaseSuccess {
		t.Errorf("adopted run is at %s, want %s", after.Phase, state.PhaseSuccess)
	}

	if after.SnapshotID != "snap-orphan" {
		t.Errorf("adopted run records snapshot %q", after.SnapshotID)
	}

	if after.SourceBytesRead != nil || after.RepositoryBytesWritten != nil {
		t.Error("adoption invented measurements the dead process never recorded; an unmeasured counter must stay unmeasured rather than becoming a zero")
	}

	if rep.LastKnownGoodRunID != "run-1" {
		t.Errorf("last-known-good is %q, want run-1", rep.LastKnownGoodRunID)
	}

	if _, ok := repo.snapshots["snap-orphan"]; !ok {
		t.Error("the adopted snapshot is gone from the repository; reconciliation must never delete one")
	}
}

func TestReconcile_ARowWhoseManifestIsGoneIsFailedAndNeverSucceeds(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	seed(t, j, set,
		"run-1",
		[]state.SnapshotPhase{state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted},
		map[state.SnapshotPhase]state.SnapshotRunUpdate{
			state.PhaseManifestCommitted: {SnapshotID: new("snap-vanished")},
		})

	rep, err := reconciler(t, j).Reconcile(context.Background(), reconcileRequest(set, newFakeRepository()))
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	verdictOf(t, rep, snapshotlifecycle.VerdictManifestMissing)

	if got := phaseOf(t, j, "run-1"); got != state.PhaseFailed {
		t.Errorf("run left at %s, want %s", got, state.PhaseFailed)
	}
}

func TestReconcile_ARunThatDiedAfterItsManifestIsVerifiedAndCommitted(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	seed(t, j, set,
		"run-1",
		[]state.SnapshotPhase{state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted},
		map[state.SnapshotPhase]state.SnapshotRunUpdate{
			state.PhaseManifestCommitted: {SnapshotID: new("snap-1")},
		})

	repo := newFakeRepository()
	repo.snapshots["snap-1"] = backupengine.SnapshotInfo{ID: "snap-1", Source: testSource, Files: 3, Bytes: 4096}

	rep, err := reconciler(t, j).Reconcile(context.Background(), reconcileRequest(set, repo))
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	verdictOf(t, rep, snapshotlifecycle.VerdictVerified)

	if repo.verifyCalls != 1 {
		t.Errorf("the snapshot was verified %d times, want once", repo.verifyCalls)
	}

	after, err := j.GetSnapshotRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}

	if after.Phase != state.PhaseSuccess {
		t.Errorf("phase %s, want %s", after.Phase, state.PhaseSuccess)
	}

	if after.VerificationStatus != "passed" {
		t.Errorf("verification status %q, want passed", after.VerificationStatus)
	}

	if after.VerificationLevelAchieved != string(model.LevelContentFull) {
		t.Errorf("achieved level %q, want %q", after.VerificationLevelAchieved, model.LevelContentFull)
	}
}

func TestReconcile_ARunThatDiedDuringVerificationIsVerifiedAgainFromTheManifest(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	seed(t, j, set,
		"run-1",
		[]state.SnapshotPhase{state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted, state.PhaseVerification},
		map[state.SnapshotPhase]state.SnapshotRunUpdate{
			state.PhaseManifestCommitted: {SnapshotID: new("snap-1")},
			state.PhaseVerification:      {VerificationStatus: new("pending")},
		})

	repo := newFakeRepository()
	repo.snapshots["snap-1"] = backupengine.SnapshotInfo{ID: "snap-1", Source: testSource, Files: 3, Bytes: 4096}

	rep, err := reconciler(t, j).Reconcile(context.Background(), reconcileRequest(set, repo))
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	verdictOf(t, rep, snapshotlifecycle.VerdictVerified)

	if repo.verifyCalls != 1 {
		t.Errorf("verified %d times, want once", repo.verifyCalls)
	}

	// The re-entry is recorded, so the log says the first verification
	// never finished rather than quietly looking like one attempt.
	transitions, err := j.SnapshotRunTransitions(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("reading transitions: %v", err)
	}

	var sawReentry bool
	for _, tr := range transitions {
		if tr.From == state.PhaseVerification && tr.To == state.PhaseManifestCommitted {
			sawReentry = true
		}
	}

	if !sawReentry {
		t.Error("the interrupted verification was retried without recording the re-entry")
	}

	if got := phaseOf(t, j, "run-1"); got != state.PhaseSuccess {
		t.Errorf("phase %s, want %s", got, state.PhaseSuccess)
	}
}

func TestReconcile_ARunVerifiedBeforeTheCatalogCommitIsCompletedWithoutVerifyingAgain(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	seed(t, j, set,
		"run-1",
		[]state.SnapshotPhase{
			state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted,
			state.PhaseVerification, state.PhaseCatalogCommit,
		},
		map[state.SnapshotPhase]state.SnapshotRunUpdate{
			state.PhaseManifestCommitted: {SnapshotID: new("snap-1")},
			state.PhaseCatalogCommit: {
				VerificationStatus:        new("passed"),
				VerificationLevelAchieved: new(string(model.LevelContentFull)),
			},
		})

	repo := newFakeRepository()
	repo.snapshots["snap-1"] = backupengine.SnapshotInfo{ID: "snap-1", Source: testSource, Files: 3, Bytes: 4096}

	rep, err := reconciler(t, j).Reconcile(context.Background(), reconcileRequest(set, repo))
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	verdictOf(t, rep, snapshotlifecycle.VerdictCommitCompleted)

	if repo.verifyCalls != 0 {
		t.Errorf("a run whose verification was already durable was verified %d more times", repo.verifyCalls)
	}

	if got := phaseOf(t, j, "run-1"); got != state.PhaseSuccess {
		t.Errorf("phase %s, want %s", got, state.PhaseSuccess)
	}

	if rep.LastKnownGoodRunID != "run-1" {
		t.Errorf("last-known-good is %q, want run-1", rep.LastKnownGoodRunID)
	}
}

func TestReconcile_ARunThatReachedTheCatalogCommitWithoutPassingVerificationIsFailed(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	seed(t, j, set,
		"run-1",
		[]state.SnapshotPhase{
			state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted,
			state.PhaseVerification, state.PhaseCatalogCommit,
		},
		map[state.SnapshotPhase]state.SnapshotRunUpdate{
			state.PhaseManifestCommitted: {SnapshotID: new("snap-1")},
			state.PhaseCatalogCommit:     {VerificationStatus: new("pending")},
		})

	repo := newFakeRepository()
	repo.snapshots["snap-1"] = backupengine.SnapshotInfo{ID: "snap-1", Source: testSource}

	rep, err := reconciler(t, j).Reconcile(context.Background(), reconcileRequest(set, repo))
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	verdictOf(t, rep, snapshotlifecycle.VerdictVerificationFailed)

	if got := phaseOf(t, j, "run-1"); got != state.PhaseFailed {
		t.Errorf("phase %s, want %s", got, state.PhaseFailed)
	}
}

func TestReconcile_AnUnattributableSnapshotIsQuarantinedOnceAndNeverDeleted(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")

	repo := newFakeRepository()
	repo.snapshots["snap-unknown"] = backupengine.SnapshotInfo{ID: "snap-unknown", Source: testSource, Files: 1, Bytes: 10}

	r := reconciler(t, j)

	rep, err := r.Reconcile(context.Background(), reconcileRequest(set, repo))
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	q := verdictOf(t, rep, snapshotlifecycle.VerdictQuarantined)
	if q.SnapshotID != "snap-unknown" {
		t.Errorf("quarantined %q", q.SnapshotID)
	}

	if _, ok := repo.snapshots["snap-unknown"]; !ok {
		t.Fatal("the unattributable snapshot was deleted; it must be quarantined and left in place")
	}

	row, err := j.GetSnapshotRun(context.Background(), q.RunID)
	if err != nil {
		t.Fatalf("reading the quarantine row: %v", err)
	}

	if row.Phase != state.PhaseQuarantined {
		t.Errorf("quarantine row is at %s", row.Phase)
	}

	if row.Reason == "" {
		t.Error("the quarantine row says nothing about why it exists")
	}

	if row.SourceIdentity != "ab12cd34" {
		t.Errorf("quarantine row records source identity %q, want the set's digest", row.SourceIdentity)
	}

	// A standing, unresolved condition must not be re-reported (and
	// re-recorded) on every cycle.
	second, err := r.Reconcile(context.Background(), reconcileRequest(set, repo))
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}

	for _, v := range second.Verdicts {
		if v.Kind == snapshotlifecycle.VerdictQuarantined {
			t.Error("the second pass quarantined the same snapshot again")
		}
	}

	rows, err := j.ListSnapshotRuns(context.Background(), set, 50)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}

	var quarantines int
	for _, row := range rows {
		if row.Phase == state.PhaseQuarantined {
			quarantines++
		}
	}

	if quarantines != 1 {
		t.Errorf("%d quarantine rows for one snapshot, want 1", quarantines)
	}
}

func TestReconcile_ASuccessfulRunWhoseSnapshotIsGoneBecomesLostAndLastKnownGoodMovesBack(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")

	nominal := []state.SnapshotPhase{
		state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted,
		state.PhaseVerification, state.PhaseCatalogCommit, state.PhaseSuccess,
	}

	seed(t, j, set, "run-old", nominal, map[state.SnapshotPhase]state.SnapshotRunUpdate{
		state.PhaseManifestCommitted: {SnapshotID: new("snap-old")},
		state.PhaseCatalogCommit:     {VerificationStatus: new("passed")},
	})
	seed(t, j, set, "run-new", nominal, map[state.SnapshotPhase]state.SnapshotRunUpdate{
		state.PhaseManifestCommitted: {SnapshotID: new("snap-new")},
		state.PhaseCatalogCommit:     {VerificationStatus: new("passed")},
	})

	// Only the older snapshot survives in the repository.
	repo := newFakeRepository()
	repo.snapshots["snap-old"] = backupengine.SnapshotInfo{ID: "snap-old", Source: testSource}

	rep, err := reconciler(t, j).Reconcile(context.Background(), reconcileRequest(set, repo))
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	lost := verdictOf(t, rep, snapshotlifecycle.VerdictRestorePointLost)
	if lost.RunID != "run-new" {
		t.Errorf("lost verdict is about %q, want run-new", lost.RunID)
	}

	if got := phaseOf(t, j, "run-new"); got != state.PhaseLost {
		t.Errorf("run-new is at %s, want %s", got, state.PhaseLost)
	}

	verdictOf(t, rep, snapshotlifecycle.VerdictLastKnownGoodRepointed)

	if rep.LastKnownGoodRunID != "run-old" {
		t.Errorf("last-known-good is %q, want run-old", rep.LastKnownGoodRunID)
	}

	lkg, err := j.LastKnownGoodSnapshot(context.Background(), set)
	if err != nil {
		t.Fatalf("reading last-known-good: %v", err)
	}

	if lkg.RunID != "run-old" || lkg.SnapshotID != "snap-old" {
		t.Errorf("last-known-good row is %q/%q, want run-old/snap-old", lkg.RunID, lkg.SnapshotID)
	}
}

func TestReconcile_ADeleteIntentIsCompletedOnlyWhenTheSnapshotIsActuallyGone(t *testing.T) {
	t.Parallel()

	nominal := []state.SnapshotPhase{
		state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted,
		state.PhaseVerification, state.PhaseCatalogCommit, state.PhaseSuccess,
	}
	updates := map[state.SnapshotPhase]state.SnapshotRunUpdate{
		state.PhaseManifestCommitted: {SnapshotID: new("snap-1")},
		state.PhaseCatalogCommit:     {VerificationStatus: new("passed")},
	}

	t.Run("gone", func(t *testing.T) {
		t.Parallel()

		j := journal(t)
		set := setID(t, "postgres")
		seed(t, j, set, "run-1", nominal, updates)

		if err := j.MarkSnapshotDeleteRequested(context.Background(), "run-1", time.Now().UTC()); err != nil {
			t.Fatalf("recording the delete intent: %v", err)
		}

		rep, err := reconciler(t, j).Reconcile(context.Background(), reconcileRequest(set, newFakeRepository()))
		if err != nil {
			t.Fatalf("reconciling: %v", err)
		}

		verdictOf(t, rep, snapshotlifecycle.VerdictDeleteCompleted)

		if got := phaseOf(t, j, "run-1"); got != state.PhaseDeleted {
			t.Errorf("phase %s, want %s", got, state.PhaseDeleted)
		}
	})

	t.Run("still there", func(t *testing.T) {
		t.Parallel()

		j := journal(t)
		set := setID(t, "postgres")
		seed(t, j, set, "run-1", nominal, updates)

		if err := j.MarkSnapshotDeleteRequested(context.Background(), "run-1", time.Now().UTC()); err != nil {
			t.Fatalf("recording the delete intent: %v", err)
		}

		repo := newFakeRepository()
		repo.snapshots["snap-1"] = backupengine.SnapshotInfo{ID: "snap-1", Source: testSource}

		rep, err := reconciler(t, j).Reconcile(context.Background(), reconcileRequest(set, repo))
		if err != nil {
			t.Fatalf("reconciling: %v", err)
		}

		verdictOf(t, rep, snapshotlifecycle.VerdictDeletePending)

		if got := phaseOf(t, j, "run-1"); got != state.PhaseSuccess {
			t.Errorf("a pending delete moved the row to %s; reconciliation may not act on the intent", got)
		}

		if _, ok := repo.snapshots["snap-1"]; !ok {
			t.Error("reconciliation issued the delete; only retention may do that")
		}
	})
}

func TestReconcile_AnUnreadableRepositoryDecidesNothing(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	seed(t, j, set,
		"run-1",
		[]state.SnapshotPhase{
			state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted,
			state.PhaseVerification, state.PhaseCatalogCommit, state.PhaseSuccess,
		},
		map[state.SnapshotPhase]state.SnapshotRunUpdate{
			state.PhaseManifestCommitted: {SnapshotID: new("snap-1")},
			state.PhaseCatalogCommit:     {VerificationStatus: new("passed")},
		})

	repo := newFakeRepository()
	repo.listErr = errors.New("the NAS is asleep")

	if _, err := reconciler(t, j).Reconcile(context.Background(), reconcileRequest(set, repo)); err == nil {
		t.Fatal("a pass that could not read the repository reported a result")
	}

	if got := phaseOf(t, j, "run-1"); got != state.PhaseSuccess {
		t.Errorf("an unreachable repository moved a restore point to %s", got)
	}

	lkg, err := j.LastKnownGoodSnapshot(context.Background(), set)
	if err != nil {
		t.Fatalf("reading last-known-good: %v", err)
	}

	if lkg.RunID != "run-1" {
		t.Errorf("last-known-good is %q after an unreachable repository", lkg.RunID)
	}
}

func TestReconcile_AnInterruptedMaintenanceIsReportedAndChangesNothing(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	seed(t, j, set,
		"run-1",
		[]state.SnapshotPhase{
			state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted,
			state.PhaseVerification, state.PhaseCatalogCommit, state.PhaseSuccess,
		},
		map[state.SnapshotPhase]state.SnapshotRunUpdate{
			state.PhaseManifestCommitted: {SnapshotID: new("snap-1")},
			state.PhaseCatalogCommit:     {VerificationStatus: new("passed")},
		})

	repo := newFakeRepository()
	repo.snapshots["snap-1"] = backupengine.SnapshotInfo{ID: "snap-1", Source: testSource}

	req := reconcileRequest(set, repo)
	req.Maintenance = &backupengine.MaintenanceOwnership{
		Domain: model.RepositoryDomainID(testDomain),
		Owner:  backupengine.MaintenanceOwner("nas-1"),
		LastResult: backupengine.MaintenanceOutcome{
			At:   time.Now().UTC().Add(-time.Hour),
			Mode: backupengine.MaintenanceQuick,
			Ran:  true,
			Err:  "interrupted",
		},
	}

	rep, err := reconciler(t, j).Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	v := verdictOf(t, rep, snapshotlifecycle.VerdictMaintenanceInterrupted)
	if v.Changed() {
		t.Error("an interrupted maintenance moved a row")
	}

	if got := phaseOf(t, j, "run-1"); got != state.PhaseSuccess {
		t.Errorf("phase %s, want %s: maintenance never invalidates a restore point", got, state.PhaseSuccess)
	}

	if rep.LastKnownGoodRunID != "run-1" {
		t.Errorf("last-known-good is %q", rep.LastKnownGoodRunID)
	}
}

func TestReconcile_ASecondPassOverAReconciledDeploymentChangesNothing(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")

	seed(t, j, set, "run-crashed", []state.SnapshotPhase{state.PhaseSourceScan, state.PhaseSnapshotWrite}, nil)
	seed(t, j, set,
		"run-committed",
		[]state.SnapshotPhase{state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted},
		map[state.SnapshotPhase]state.SnapshotRunUpdate{
			state.PhaseManifestCommitted: {SnapshotID: new("snap-1")},
		})

	repo := newFakeRepository()
	repo.snapshots["snap-1"] = backupengine.SnapshotInfo{ID: "snap-1", Source: testSource}
	repo.snapshots["snap-unknown"] = backupengine.SnapshotInfo{ID: "snap-unknown", Source: testSource}

	r := reconciler(t, j)

	if _, err := r.Reconcile(context.Background(), reconcileRequest(set, repo)); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	second, err := r.Reconcile(context.Background(), reconcileRequest(set, repo))
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}

	if changed := second.Changed(); len(changed) != 0 {
		t.Errorf("the second pass changed %d rows: %+v", len(changed), changed)
	}
}
