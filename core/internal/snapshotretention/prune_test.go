// What this file is for: proving that the snapshot half of retention is
// the SAME retention, and that the two things which must survive it do.
//
// Every claim here is one of issue #785's acceptance criteria, and each
// one is written against the real journal rather than a fake catalog. The
// reason is the §39 case below: "a failed newer backup must not evict the
// last-known-good" is half a policy decision and half a durable rule about
// a column, and a test that stubbed the catalog would only ever prove the
// half this package owns.
//
// The repository, by contrast, IS a fake here, and deliberately so: what a
// unit test needs from it is a record of exactly which manifests were
// asked for and which were removed. The real engine is exercised in
// kopiablobs_test.go, which asks the question this fake cannot -- what
// happened to the repository's own storage.

package snapshotretention_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/lifecycle"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/retention"
	"github.com/backupdproject/backupd/core/internal/snapshotretention"
	"github.com/backupdproject/backupd/core/internal/state"
)

// pruneNow is the instant every decision in this file is taken as of, and
// every run below is dated relative to it. Retention is a calculation over
// a calendar, so a test that used time.Now would assert different things
// in different weeks.
var pruneNow = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

// dailyOnly is the smallest honest policy: one tier, seven days. Anything
// older than a week is outside every window and is a delete candidate,
// which is what makes the fixtures below short enough to read.
func dailyOnly() config.Retention {
	return config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 7}
}

func mustSet(t *testing.T, source, name string) model.BackupSetID {
	t.Helper()
	set, err := model.NewBackupSetID(source, name)
	if err != nil {
		t.Fatalf("NewBackupSetID(%q, %q): %v", source, name, err)
	}
	return set
}

// backupSet is the resolved configuration a pass decides with: the set's
// identity, its durable uuid, and the policy config.Validate resolved for
// it.
func backupSet(t *testing.T, set model.BackupSetID, uuid string, cfg config.Retention) config.BackupSet {
	t.Helper()
	return config.BackupSet{ID: set, UUID: uuid, Retention: cfg}
}

// runSpec is one snapshot run as this file wants it recorded: when it
// started, what manifest it committed, and how it ended.
type runSpec struct {
	runID      string
	snapshotID string
	startedAt  time.Time

	// verifyFails drives the §39 case: the run commits a real manifest and
	// then fails verification, which is the exact shape of "a newer backup
	// that must not become the restore point".
	verifyFails bool
}

func openCatalog(t *testing.T) *state.Journal {
	t.Helper()
	j, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// record writes one run into the catalog exactly the way a real run driver
// does: phase by phase, with the manifest recorded on the way into
// MANIFEST_COMMITTED and the verification verdict on the way out of
// VERIFICATION.
func record(t *testing.T, j *state.Journal, set model.BackupSetID, uuid string, spec runSpec) {
	t.Helper()
	ctx := context.Background()

	out, err := j.BeginSnapshotRun(ctx, state.SnapshotRunRequest{
		RunID:             spec.runID,
		IdempotencyKey:    "key-" + spec.runID,
		Set:               set,
		SetUUID:           uuid,
		Engine:            "kopia",
		Domain:            "nas-primary",
		SourceIdentity:    "sha256:" + uuid,
		VerificationLevel: "structural",
		StartedAt:         spec.startedAt,
	})
	if err != nil {
		t.Fatalf("BeginSnapshotRun(%s): %v", spec.runID, err)
	}
	if !out.Created {
		t.Fatalf("BeginSnapshotRun(%s): the fixture reused a key", spec.runID)
	}

	at := spec.startedAt
	step := func(phase state.SnapshotPhase, upd state.SnapshotRunUpdate) {
		t.Helper()
		at = at.Add(time.Minute)
		upd.At = at
		if err := j.AdvanceSnapshotRun(ctx, spec.runID, phase, upd); err != nil {
			t.Fatalf("AdvanceSnapshotRun(%s -> %s): %v", spec.runID, phase, err)
		}
	}

	complete := true
	manifest := spec.snapshotID
	step(state.PhaseSourceScan, state.SnapshotRunUpdate{})
	step(state.PhaseSnapshotWrite, state.SnapshotRunUpdate{})
	step(state.PhaseManifestCommitted, state.SnapshotRunUpdate{SnapshotID: &manifest, SourceComplete: &complete})
	step(state.PhaseVerification, state.SnapshotRunUpdate{})

	if spec.verifyFails {
		failed := string(state.SnapshotVerificationFailed)
		why := "verification found unreadable content"
		step(state.PhaseFailed, state.SnapshotRunUpdate{VerificationStatus: &failed, Reason: &why})
		return
	}

	passed := string(state.SnapshotVerificationPassed)
	level := "structural"
	step(state.PhaseCatalogCommit, state.SnapshotRunUpdate{VerificationStatus: &passed, VerificationLevelAchieved: &level})
	step(state.PhaseSuccess, state.SnapshotRunUpdate{})
}

// fakeRepository is the repository half of a pass, recording every request
// it is given. What it records is the assertion: the ids retention asked
// about, and the ids it removed.
type fakeRepository struct {
	mu      sync.Mutex
	missing map[backupengine.SnapshotID]bool
	lookups []backupengine.SnapshotID
	deleted []backupengine.SnapshotID
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{missing: map[backupengine.SnapshotID]bool{}}
}

func (r *fakeRepository) LookupSnapshot(_ context.Context, id backupengine.SnapshotID) (backupengine.SnapshotInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookups = append(r.lookups, id)
	if r.missing[id] {
		return backupengine.SnapshotInfo{}, backupengine.ErrSnapshotNotFound
	}
	return backupengine.SnapshotInfo{ID: id}, nil
}

func (r *fakeRepository) DeleteSnapshot(_ context.Context, id backupengine.SnapshotID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.missing[id] {
		return backupengine.ErrSnapshotNotFound
	}
	r.deleted = append(r.deleted, id)
	r.missing[id] = true
	return nil
}

func (r *fakeRepository) deletedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.deleted))
	for _, id := range r.deleted {
		out = append(out, string(id))
	}
	sort.Strings(out)
	return out
}

// deletes projects a plan down to the manifests it proposes to remove.
func deletes(verdicts []snapshotretention.Verdict) []string {
	var out []string
	for _, v := range verdicts {
		if v.Action == snapshotretention.Delete {
			out = append(out, string(v.Snapshot))
		}
	}
	sort.Strings(out)
	return out
}

func verdictFor(t *testing.T, verdicts []snapshotretention.Verdict, snapshotID string) snapshotretention.Verdict {
	t.Helper()
	for _, v := range verdicts {
		if string(v.Snapshot) == snapshotID {
			return v
		}
	}
	t.Fatalf("no verdict for snapshot %q in %+v", snapshotID, verdicts)
	return snapshotretention.Verdict{}
}

// TestTheSnapshotTimelineClassifiesLikeTheArtifactTimeline is issue #785's
// equivalence criterion, and it is the reason this package projects onto
// internal/retention's own input rather than reimplementing GFS.
//
// The two timelines are the same instants described two ways: a backup set
// whose artifacts were discovered daily for a month, and a backup set
// whose snapshot runs started daily for a month. If the classifier's
// answer differs between them, then "backupd's retention semantics" has
// come to mean two different things depending on which engine wrote the
// backup, which is precisely what an operator cannot be asked to reason
// about.
//
// The comparison is over the whole verdict, not just the keep flag: which
// TIER kept a snapshot, and which placement it was attributed to, are what
// a preview renders, and a projection that kept the right set of snapshots
// for the wrong stated reason would pass a weaker assertion.
func TestTheSnapshotTimelineClassifiesLikeTheArtifactTimeline(t *testing.T) {
	set := mustSet(t, "production", "postgres")
	cfg := config.Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 7, WeeklyMonths: 3, MonthlyMonths: 12}

	var (
		runs     []state.SnapshotRun
		artifact []state.Record
	)
	for day := range 40 {
		at := pruneNow.AddDate(0, 0, -day).Add(-3 * time.Hour)
		name := fmt.Sprintf("manifest-%02d", day)

		runs = append(runs, state.SnapshotRun{
			RunID:      "run-" + name,
			Set:        set,
			SetUUID:    "uuid-1",
			SnapshotID: name,
			Phase:      state.PhaseSuccess,
			StartedAt:  at,
			UpdatedAt:  at.Add(time.Minute),
		})

		id, err := model.NewArtifactID(set, name)
		if err != nil {
			t.Fatalf("NewArtifactID: %v", err)
		}
		artifact = append(artifact, state.Record{
			Artifact:     id,
			State:        string(lifecycle.Complete),
			DiscoveredAt: at,
			UpdatedAt:    at.Add(time.Minute),
		})
	}

	projected, unclassifiable := snapshotretention.Timeline(set, runs)
	if len(unclassifiable) != 0 {
		t.Fatalf("Timeline could not classify %+v; every run here is a plain success", unclassifiable)
	}

	fromSnapshots, snapLKG, err := retention.DecideKeep(pruneNow, cfg, set, projected)
	if err != nil {
		t.Fatalf("DecideKeep over the snapshot timeline: %v", err)
	}
	fromArtifacts, artLKG, err := retention.DecideKeep(pruneNow, cfg, set, artifact)
	if err != nil {
		t.Fatalf("DecideKeep over the artifact timeline: %v", err)
	}

	if !reflect.DeepEqual(fromSnapshots, fromArtifacts) {
		t.Errorf("the two timelines classify differently.\nsnapshots: %+v\nartifacts: %+v", fromSnapshots, fromArtifacts)
	}
	if snapLKG.Artifact != artLKG.Artifact || snapLKG.Protected != artLKG.Protected {
		t.Errorf("last-known-good differs: snapshots %+v, artifacts %+v", snapLKG, artLKG)
	}

	// The control. An equivalence test over two timelines that both keep
	// everything, or both keep nothing, would pass without comparing any
	// real decision.
	var kept, dropped int
	for _, v := range fromSnapshots {
		if v.Keep {
			kept++
			continue
		}
		dropped++
	}
	if kept == 0 || dropped == 0 {
		t.Fatalf("the fixture decided nothing: %d kept, %d dropped over %d snapshots", kept, dropped, len(fromSnapshots))
	}
}

// TestAHeldSnapshotSurvivesAPruneThatWouldOtherwiseDeleteIt is the hold
// criterion, driven end to end: the same snapshot is planned for deletion
// without the hold and refused with it, and the manifest is still in the
// repository after the pass that refused it.
//
// Both halves matter. A hold that changed the plan but not the pass would
// be a preview that lies; a hold that stopped the delete without saying so
// would be an operator asking why retention has stopped working.
func TestAHeldSnapshotSurvivesAPruneThatWouldOtherwiseDeleteIt(t *testing.T) {
	ctx := context.Background()
	j := openCatalog(t)
	set := mustSet(t, "production", "postgres")
	bs := backupSet(t, set, "uuid-hold", dailyOnly())

	// Two ancient snapshots, both outside every window, plus a fresh one
	// so the set has a restore point that is not up for deletion.
	record(t, j, set, bs.UUID, runSpec{runID: "run-old-a", snapshotID: "manifest-old-a", startedAt: pruneNow.AddDate(0, 0, -40)})
	record(t, j, set, bs.UUID, runSpec{runID: "run-old-b", snapshotID: "manifest-old-b", startedAt: pruneNow.AddDate(0, 0, -39)})
	record(t, j, set, bs.UUID, runSpec{runID: "run-fresh", snapshotID: "manifest-fresh", startedAt: pruneNow.Add(-2 * time.Hour)})

	repo := newFakeRepository()
	pruner := snapshotretention.Pruner{Catalog: j, Repository: repo}

	before, err := pruner.Decide(ctx, pruneNow, bs)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := deletes(before); !reflect.DeepEqual(got, []string{"manifest-old-a", "manifest-old-b"}) {
		t.Fatalf("without a hold the plan deletes %v, want both old manifests", got)
	}

	if _, err := j.PlaceSnapshotHold(ctx, state.SnapshotHoldRequest{
		HoldID:   "hold-1",
		RunID:    "run-old-a",
		Reason:   "incident 8812 is still open",
		PlacedBy: "ops@example.com",
		At:       pruneNow.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("PlaceSnapshotHold: %v", err)
	}

	planned, err := pruner.Decide(ctx, pruneNow, bs)
	if err != nil {
		t.Fatalf("Decide after the hold: %v", err)
	}
	held := verdictFor(t, planned, "manifest-old-a")
	if held.Action != snapshotretention.Keep {
		t.Errorf("the held snapshot's action is %s, want %s: %s", held.Action, snapshotretention.Keep, held.Reason)
	}
	if len(held.Holds) != 1 || held.Holds[0].HoldID != "hold-1" {
		t.Errorf("the verdict does not name the hold that kept it: %+v", held.Holds)
	}
	if !tiersContain(held.Tiers, snapshotretention.TierHold) {
		t.Errorf("Tiers = %v, want it to name %s", held.Tiers, snapshotretention.TierHold)
	}

	applied, err := pruner.Apply(ctx, pruneNow, bs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := deletes(applied); !reflect.DeepEqual(got, []string{"manifest-old-b"}) {
		t.Fatalf("the pass deleted %v, want only the unheld old manifest", got)
	}
	if got := repo.deletedIDs(); !reflect.DeepEqual(got, []string{"manifest-old-b"}) {
		t.Fatalf("the repository was asked to delete %v, want only manifest-old-b", got)
	}

	// The held run is untouched in the catalog too: still a restore point,
	// with no delete intent recorded against it.
	run, err := j.GetSnapshotRun(ctx, "run-old-a")
	if err != nil {
		t.Fatalf("GetSnapshotRun: %v", err)
	}
	if run.Phase != state.PhaseSuccess {
		t.Errorf("the held run is at %s, want it left at %s", run.Phase, state.PhaseSuccess)
	}
	if run.DeleteRequestedAt != nil {
		t.Errorf("a delete intent was recorded against a held snapshot: %v", run.DeleteRequestedAt)
	}

	// And releasing the hold is what makes it deletable, which is the
	// other half of "never deleted until released".
	if err := j.ReleaseSnapshotHold(ctx, "hold-1", pruneNow, "ops@example.com"); err != nil {
		t.Fatalf("ReleaseSnapshotHold: %v", err)
	}
	after, err := pruner.Apply(ctx, pruneNow, bs)
	if err != nil {
		t.Fatalf("Apply after release: %v", err)
	}
	if got := deletes(after); !reflect.DeepEqual(got, []string{"manifest-old-a"}) {
		t.Fatalf("after the release the pass deleted %v, want manifest-old-a", got)
	}
}

func tiersContain(tiers []retention.GFSTierSelection, want retention.GFSTier) bool {
	for _, s := range tiers {
		if s.Tier == want {
			return true
		}
	}
	return false
}

// TestAFailedVerificationNeverEvictsTheLastKnownGood is the §39
// regression, stated exactly as the EPIC states it: A is verified, B
// completes and its verification FAILS, retention runs, and A is still the
// restore point afterwards.
//
// The trap this guards is a retention engine that reads "the newest run
// with a manifest" as the restore point. B has a real manifest in the
// repository -- that is what makes the case worth writing -- so an engine
// that ranked by manifest rather than by proven outcome would protect B,
// leave A outside every window, and delete the only snapshot anybody could
// actually restore from.
func TestAFailedVerificationNeverEvictsTheLastKnownGood(t *testing.T) {
	ctx := context.Background()
	j := openCatalog(t)
	set := mustSet(t, "production", "postgres")
	bs := backupSet(t, set, "uuid-lkg", dailyOnly())

	// A is old enough that nothing but last-known-good protection can keep
	// it: outside the seven-day window by a month.
	record(t, j, set, bs.UUID, runSpec{runID: "run-a", snapshotID: "manifest-a", startedAt: pruneNow.AddDate(0, 0, -37)})
	record(t, j, set, bs.UUID, runSpec{runID: "run-b", snapshotID: "manifest-b", startedAt: pruneNow.Add(-2 * time.Hour), verifyFails: true})

	repo := newFakeRepository()
	pruner := snapshotretention.Pruner{Catalog: j, Repository: repo}

	planned, err := pruner.Decide(ctx, pruneNow, bs)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	a := verdictFor(t, planned, "manifest-a")
	if a.Action != snapshotretention.Keep {
		t.Fatalf("A's action is %s, want %s: %s", a.Action, snapshotretention.Keep, a.Reason)
	}
	if !tiersContain(a.Tiers, retention.TierLastKnownGood) {
		t.Errorf("A is kept but not as the last-known-good restore point: %v", a.Tiers)
	}

	// B is not classified at all. A run that failed is not a completed
	// backup, exactly as a FAILED artifact is outside GFS's remit, so
	// retention has no opinion to offer about it and certainly does not
	// delete it: whatever becomes of a failed run's manifest is the
	// reconciler's business, not this pass's.
	for _, v := range planned {
		if string(v.Snapshot) == "manifest-b" {
			t.Errorf("retention issued a verdict about a failed run's manifest: %+v", v)
		}
	}

	applied, err := pruner.Apply(ctx, pruneNow, bs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := deletes(applied); len(got) != 0 {
		t.Fatalf("the pass deleted %v, want nothing", got)
	}
	if got := repo.deletedIDs(); len(got) != 0 {
		t.Fatalf("the repository was asked to delete %v, want nothing", got)
	}

	lkg, err := j.LastKnownGoodSnapshot(ctx, bs.UUID)
	if err != nil {
		t.Fatalf("LastKnownGoodSnapshot: %v", err)
	}
	if lkg.RunID != "run-a" {
		t.Fatalf("the restore point after the pass is %s, want run-a: a failed newer snapshot evicted the last known good", lkg.RunID)
	}
}

// TestThePreviewIsExactlyWhatThePassDeletes is FR-20's mandatory dry-run
// applied to snapshots: an operator confirms a plan, and the pass that
// follows removes those manifests and no others.
//
// It is asserted as set equality against the REPOSITORY's own record of
// what it was asked to delete, not against the returned verdicts, because
// the verdicts are this package's own account of itself and would agree
// with the plan even if nothing were ever removed.
func TestThePreviewIsExactlyWhatThePassDeletes(t *testing.T) {
	ctx := context.Background()
	j := openCatalog(t)
	set := mustSet(t, "production", "postgres")
	bs := backupSet(t, set, "uuid-preview", dailyOnly())

	for day := range 30 {
		record(t, j, set, bs.UUID, runSpec{
			runID:      fmt.Sprintf("run-%02d", day),
			snapshotID: fmt.Sprintf("manifest-%02d", day),
			startedAt:  pruneNow.AddDate(0, 0, -day).Add(-3 * time.Hour),
		})
	}

	repo := newFakeRepository()
	pruner := snapshotretention.Pruner{Catalog: j, Repository: repo}

	preview, err := pruner.Decide(ctx, pruneNow, bs)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	planned := deletes(preview)
	if len(planned) == 0 {
		t.Fatal("the preview proposes no deletions, so this test compares nothing")
	}

	applied, err := pruner.Apply(ctx, pruneNow, bs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := deletes(applied); !reflect.DeepEqual(got, planned) {
		t.Errorf("the pass reports deleting %v, the preview proposed %v", got, planned)
	}
	if got := repo.deletedIDs(); !reflect.DeepEqual(got, planned) {
		t.Errorf("the repository was asked to delete %v, the preview proposed %v", got, planned)
	}

	// Every removed manifest is recorded as intended-then-deleted, which
	// is what makes a crash in the middle of a pass decidable rather than
	// a guess.
	for _, id := range planned {
		var found bool
		runs, err := j.ListSnapshotRuns(ctx, bs.UUID, 100)
		if err != nil {
			t.Fatalf("ListSnapshotRuns: %v", err)
		}
		for _, run := range runs {
			if run.SnapshotID != id {
				continue
			}
			found = true
			if run.Phase != state.PhaseDeleted {
				t.Errorf("run %s is at %s after its manifest was deleted, want %s", run.RunID, run.Phase, state.PhaseDeleted)
			}
			if run.DeleteRequestedAt == nil {
				t.Errorf("run %s records no delete intent, so a crash mid-delete would be indistinguishable from a loss", run.RunID)
			}
		}
		if !found {
			t.Errorf("no catalog row for the deleted manifest %q", id)
		}
	}
}

// TestNothingIsDeletedWhileTheRestorePointCannotBeFound ports FR-30's
// standing refusal to snapshots: if the restore point this set advertises
// is not in the repository, the pass stops rather than removing the
// copies that are still there.
//
// The whole pass, not the one snapshot that looks related. The fact that
// stops it is about the SET, and half a pass carried out removes exactly
// the snapshots that were still restorable.
func TestNothingIsDeletedWhileTheRestorePointCannotBeFound(t *testing.T) {
	ctx := context.Background()
	j := openCatalog(t)
	set := mustSet(t, "production", "postgres")
	bs := backupSet(t, set, "uuid-gone", dailyOnly())

	record(t, j, set, bs.UUID, runSpec{runID: "run-old-a", snapshotID: "manifest-old-a", startedAt: pruneNow.AddDate(0, 0, -40)})
	record(t, j, set, bs.UUID, runSpec{runID: "run-old-b", snapshotID: "manifest-old-b", startedAt: pruneNow.AddDate(0, 0, -39)})
	record(t, j, set, bs.UUID, runSpec{runID: "run-fresh", snapshotID: "manifest-fresh", startedAt: pruneNow.Add(-2 * time.Hour)})

	repo := newFakeRepository()
	repo.missing["manifest-fresh"] = true // the restore point is gone from the repository

	pruner := snapshotretention.Pruner{Catalog: j, Repository: repo}
	planned, err := pruner.Decide(ctx, pruneNow, bs)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := deletes(planned); len(got) != 0 {
		t.Fatalf("the plan proposes deleting %v while this set's restore point cannot be found", got)
	}
	for _, id := range []string{"manifest-old-a", "manifest-old-b"} {
		v := verdictFor(t, planned, id)
		if v.Action != snapshotretention.Refuse {
			t.Errorf("%s: action %s, want %s", id, v.Action, snapshotretention.Refuse)
		}
		if v.HoldReason == "" {
			t.Errorf("%s: the refusal does not carry the set-wide reason, so nothing above can tell it from an ordinary per-snapshot refusal", id)
		}
	}

	applied, err := pruner.Apply(ctx, pruneNow, bs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := deletes(applied); len(got) != 0 {
		t.Fatalf("the pass deleted %v, want nothing", got)
	}
	if got := repo.deletedIDs(); len(got) != 0 {
		t.Fatalf("the repository was asked to delete %v, want nothing", got)
	}
}

// TestOnlyABackupdApprovedManifestIsEverDeleted is the criterion in its
// strongest available form for a unit test: the repository is asked to
// delete exactly the ids the plan approved, and the run that is still
// in flight, the run that failed, and the run holding the restore point
// are all absent from that list.
//
// The in-flight run is the interesting one. Its manifest is committed and
// real, so an engine that walked the repository rather than the catalog
// would find it and, seeing no policy keeping it, remove a snapshot whose
// own run has not finished deciding whether it is any good.
func TestOnlyABackupdApprovedManifestIsEverDeleted(t *testing.T) {
	ctx := context.Background()
	j := openCatalog(t)
	set := mustSet(t, "production", "postgres")
	bs := backupSet(t, set, "uuid-approved", dailyOnly())

	record(t, j, set, bs.UUID, runSpec{runID: "run-old", snapshotID: "manifest-old", startedAt: pruneNow.AddDate(0, 0, -40)})
	record(t, j, set, bs.UUID, runSpec{runID: "run-failed", snapshotID: "manifest-failed", startedAt: pruneNow.AddDate(0, 0, -38), verifyFails: true})
	record(t, j, set, bs.UUID, runSpec{runID: "run-fresh", snapshotID: "manifest-fresh", startedAt: pruneNow.Add(-2 * time.Hour)})

	// A run that committed a manifest and is still verifying.
	inflight := state.SnapshotRunRequest{
		RunID: "run-inflight", IdempotencyKey: "key-inflight", Set: set, SetUUID: bs.UUID,
		Engine: "kopia", Domain: "nas-primary", SourceIdentity: "sha256:x",
		VerificationLevel: "structural", StartedAt: pruneNow.AddDate(0, 0, -35),
	}
	if _, err := j.BeginSnapshotRun(ctx, inflight); err != nil {
		t.Fatalf("BeginSnapshotRun: %v", err)
	}
	manifest := "manifest-inflight"
	for i, phase := range []state.SnapshotPhase{state.PhaseSourceScan, state.PhaseSnapshotWrite, state.PhaseManifestCommitted} {
		upd := state.SnapshotRunUpdate{At: inflight.StartedAt.Add(time.Duration(i+1) * time.Minute)}
		if phase == state.PhaseManifestCommitted {
			upd.SnapshotID = &manifest
		}
		if err := j.AdvanceSnapshotRun(ctx, inflight.RunID, phase, upd); err != nil {
			t.Fatalf("AdvanceSnapshotRun(%s): %v", phase, err)
		}
	}

	repo := newFakeRepository()
	pruner := snapshotretention.Pruner{Catalog: j, Repository: repo}

	applied, err := pruner.Apply(ctx, pruneNow, bs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := deletes(applied); !reflect.DeepEqual(got, []string{"manifest-old"}) {
		t.Fatalf("the pass deleted %v, want only manifest-old", got)
	}
	if got := repo.deletedIDs(); !reflect.DeepEqual(got, []string{"manifest-old"}) {
		t.Fatalf("the repository was asked to delete %v, want only manifest-old", got)
	}

	// Neither the failed run's manifest nor the in-flight one is even in
	// the plan. That is the claim that has to hold HERE rather than at the
	// journal: the catalog would refuse a delete intent for both of them
	// anyway, and a scope this package got wrong would be invisible behind
	// that refusal until the day a run in one of those states happened to
	// be at a phase the journal does admit.
	for _, v := range applied {
		switch string(v.Snapshot) {
		case "manifest-failed", "manifest-inflight":
			t.Errorf("retention formed an opinion about %s, which is not a completed verified backup: %+v", v.Snapshot, v)
		}
	}
}

// TestAPassStopsWhenTheCatalogRefusesTheDurableIntent proves the order of
// the two writes that make a delete decidable after a crash: the intent is
// recorded BEFORE the repository is asked, so a catalog that refuses the
// intent means the repository is never asked at all.
//
// Reversed, the failure is silent and permanent: a manifest gone from the
// repository with no intent recorded reads, to the reconciler, as a
// snapshot this product LOST, and it raises an incident about a deletion
// it performed on purpose.
func TestAPassStopsWhenTheCatalogRefusesTheDurableIntent(t *testing.T) {
	ctx := context.Background()
	j := openCatalog(t)
	set := mustSet(t, "production", "postgres")
	bs := backupSet(t, set, "uuid-intent", dailyOnly())

	record(t, j, set, bs.UUID, runSpec{runID: "run-old", snapshotID: "manifest-old", startedAt: pruneNow.AddDate(0, 0, -40)})
	record(t, j, set, bs.UUID, runSpec{runID: "run-fresh", snapshotID: "manifest-fresh", startedAt: pruneNow.Add(-2 * time.Hour)})

	repo := newFakeRepository()
	pruner := snapshotretention.Pruner{Catalog: refusingIntent{j}, Repository: repo}

	applied, err := pruner.Apply(ctx, pruneNow, bs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := repo.deletedIDs(); len(got) != 0 {
		t.Fatalf("the repository deleted %v even though no delete intent could be recorded", got)
	}
	v := verdictFor(t, applied, "manifest-old")
	if v.Action != snapshotretention.Refuse {
		t.Errorf("action %s, want %s: %s", v.Action, snapshotretention.Refuse, v.Reason)
	}
}

// refusingIntent is the catalog with its one durable pre-delete write
// broken, and nothing else changed.
type refusingIntent struct{ *state.Journal }

func (refusingIntent) MarkSnapshotDeleteRequested(context.Context, string, time.Time) error {
	return errors.New("catalog is read-only right now")
}
