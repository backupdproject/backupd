// The durable half of a hold: a row that outlives the process which placed
// it, and the refusals that keep it honest.
//
// A hold is the one thing in this product that can stop a retention pass
// deleting a snapshot the policy has finished with, so the interesting
// assertions here are all about what it refuses. A hold on a run with no
// manifest protects nothing; a hold on a snapshot this product already
// deleted cannot bring it back; a hold whose reason nobody wrote is a hold
// nobody can act on later. Each of those is refused in a sentence rather
// than stored, because a hold that exists but protects nothing is worse
// than no hold at all: somebody read the list and stopped worrying.

package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/model"
)

// heldRun is a run at SUCCESS with a manifest: the only shape a hold can be
// placed on, which is what most of this file needs before it can start.
func heldRun(t *testing.T, j *Journal, runID, key string, set model.BackupSetID, snapshotID string) SnapshotRun {
	t.Helper()

	ctx := context.Background()
	req := testSnapshotRunRequest(t, runID, key, set, snapshotRunAt)
	run := beginRun(t, j, req)

	advanceThrough(t, j, run.RunID, snapshotRunAt, PhaseSourceScan, PhaseSnapshotWrite)
	id := snapshotID
	complete := true
	if err := j.AdvanceSnapshotRun(ctx, run.RunID, PhaseManifestCommitted, SnapshotRunUpdate{
		SnapshotID:     &id,
		SourceComplete: &complete,
		At:             snapshotRunAt.Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("AdvanceSnapshotRun(MANIFEST_COMMITTED): %v", err)
	}
	advanceThrough(t, j, run.RunID, snapshotRunAt.Add(10*time.Minute),
		PhaseVerification, PhaseCatalogCommit, PhaseSuccess)

	out, err := j.GetSnapshotRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetSnapshotRun(%s): %v", run.RunID, err)
	}
	return out
}

// TestSnapshotHoldSurvivesAndNamesWhoAndWhy is the whole point of the row:
// a hold placed by one process is readable, in full, by the next one.
//
// Every field is asserted rather than just the existence of a row, because
// a hold with no reason and no placer is a hold an operator finding it in
// six months cannot release with any confidence. The read is by lineage
// (set uuid) because that is how the retention pass asks the question: it
// is deciding about a whole backup set and needs every active hold in it,
// not one it already knows to look for.
func TestSnapshotHoldSurvivesAndNamesWhoAndWhy(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres")
	run := heldRun(t, j, "run-hold-1", "key-hold-1", set, "manifest-1")

	placed := snapshotRunAt.Add(time.Hour)
	hold, err := j.PlaceSnapshotHold(ctx, SnapshotHoldRequest{
		HoldID:   "hold-1",
		RunID:    run.RunID,
		Reason:   "litigation hold, matter 4471",
		PlacedBy: "ops@example.com",
		At:       placed,
	})
	if err != nil {
		t.Fatalf("PlaceSnapshotHold: %v", err)
	}
	if hold.SetUUID != run.SetUUID {
		t.Errorf("SetUUID = %q, want %q: the lineage comes off the run row, never from the caller", hold.SetUUID, run.SetUUID)
	}
	if hold.ReleasedAt != nil {
		t.Errorf("ReleasedAt = %v, want nil: a hold is active the moment it is placed", hold.ReleasedAt)
	}

	active, err := j.ActiveSnapshotHolds(ctx, run.SetUUID)
	if err != nil {
		t.Fatalf("ActiveSnapshotHolds: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("ActiveSnapshotHolds returned %d holds, want 1: %+v", len(active), active)
	}
	got := active[0]
	switch {
	case got.HoldID != "hold-1":
		t.Errorf("HoldID = %q, want hold-1", got.HoldID)
	case got.RunID != run.RunID:
		t.Errorf("RunID = %q, want %q", got.RunID, run.RunID)
	case got.Reason != "litigation hold, matter 4471":
		t.Errorf("Reason = %q: a hold nobody explained is one nobody can release", got.Reason)
	case got.PlacedBy != "ops@example.com":
		t.Errorf("PlacedBy = %q, want ops@example.com", got.PlacedBy)
	case !got.PlacedAt.Equal(placed):
		t.Errorf("PlacedAt = %s, want %s", got.PlacedAt, placed)
	}
}

// TestReleasedSnapshotHoldStopsProtectingAndStaysOnTheRecord is the other
// half of the lifecycle, and the two claims in the name are deliberately
// both asserted.
//
// A released hold must leave the active list, or releasing it did nothing.
// It must also still be readable, because "who released the hold on the
// snapshot that was then deleted, and when" is the first question asked
// after a deletion somebody disputes, and a DELETE from this table would
// answer it with silence.
func TestReleasedSnapshotHoldStopsProtectingAndStaysOnTheRecord(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres")
	run := heldRun(t, j, "run-hold-2", "key-hold-2", set, "manifest-2")

	if _, err := j.PlaceSnapshotHold(ctx, SnapshotHoldRequest{
		HoldID: "hold-2", RunID: run.RunID, Reason: "pending audit", PlacedBy: "ops", At: snapshotRunAt.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PlaceSnapshotHold: %v", err)
	}

	released := snapshotRunAt.Add(48 * time.Hour)
	if err := j.ReleaseSnapshotHold(ctx, "hold-2", released, "auditor"); err != nil {
		t.Fatalf("ReleaseSnapshotHold: %v", err)
	}

	active, err := j.ActiveSnapshotHolds(ctx, run.SetUUID)
	if err != nil {
		t.Fatalf("ActiveSnapshotHolds: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("ActiveSnapshotHolds returned %+v after the hold was released, want none", active)
	}

	all, err := j.SnapshotHolds(ctx, run.RunID)
	if err != nil {
		t.Fatalf("SnapshotHolds: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("SnapshotHolds returned %d rows, want the released hold to still be on the record", len(all))
	}
	switch {
	case all[0].ReleasedAt == nil:
		t.Errorf("ReleasedAt is nil on a released hold: %+v", all[0])
	case !all[0].ReleasedAt.Equal(released):
		t.Errorf("ReleasedAt = %s, want %s", all[0].ReleasedAt, released)
	case all[0].ReleasedBy != "auditor":
		t.Errorf("ReleasedBy = %q, want auditor: who released a hold is the first question after a disputed deletion", all[0].ReleasedBy)
	}

	// Releasing twice keeps the first instant. It is when the protection
	// actually ended, and a retry that rewrote it would move the one
	// timestamp an investigation reads.
	if err := j.ReleaseSnapshotHold(ctx, "hold-2", released.Add(time.Hour), "somebody-else"); err != nil {
		t.Fatalf("ReleaseSnapshotHold (repeat): %v", err)
	}
	all, err = j.SnapshotHolds(ctx, run.RunID)
	if err != nil {
		t.Fatalf("SnapshotHolds: %v", err)
	}
	if !all[0].ReleasedAt.Equal(released) {
		t.Errorf("ReleasedAt = %s after a repeat release, want the original %s", all[0].ReleasedAt, released)
	}
}

// TestPlaceSnapshotHoldRefusesWhatAHoldCannotProtect drives every refusal
// this write makes, because a hold that was accepted and protects nothing
// is the failure mode worth paying for: somebody reads the list, sees a
// hold, and stops worrying about a snapshot retention is free to remove.
func TestPlaceSnapshotHoldRefusesWhatAHoldCannotProtect(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres")

	// A run that never committed a manifest has no snapshot to hold.
	inflight := beginRun(t, j, testSnapshotRunRequest(t, "run-inflight", "key-inflight", set, snapshotRunAt))
	if _, err := j.PlaceSnapshotHold(ctx, SnapshotHoldRequest{
		HoldID: "hold-inflight", RunID: inflight.RunID, Reason: "why not", PlacedBy: "ops", At: snapshotRunAt,
	}); err == nil {
		t.Error("PlaceSnapshotHold accepted a hold on a run with no manifest, which protects nothing")
	}

	// A snapshot this product already deleted cannot be held back.
	deleted := heldRun(t, j, "run-deleted", "key-deleted", set, "manifest-deleted")
	if err := j.MarkSnapshotDeleteRequested(ctx, deleted.RunID, snapshotRunAt.Add(time.Hour)); err != nil {
		t.Fatalf("MarkSnapshotDeleteRequested: %v", err)
	}
	if err := j.AdvanceSnapshotRun(ctx, deleted.RunID, PhaseDeleted, SnapshotRunUpdate{At: snapshotRunAt.Add(2 * time.Hour)}); err != nil {
		t.Fatalf("AdvanceSnapshotRun(DELETED): %v", err)
	}
	if _, err := j.PlaceSnapshotHold(ctx, SnapshotHoldRequest{
		HoldID: "hold-deleted", RunID: deleted.RunID, Reason: "too late", PlacedBy: "ops", At: snapshotRunAt.Add(3 * time.Hour),
	}); err == nil {
		t.Error("PlaceSnapshotHold accepted a hold on a snapshot that has already been deleted")
	}

	// A run nothing recorded.
	if _, err := j.PlaceSnapshotHold(ctx, SnapshotHoldRequest{
		HoldID: "hold-ghost", RunID: "run-that-never-was", Reason: "r", PlacedBy: "ops", At: snapshotRunAt,
	}); !errors.Is(err, ErrSnapshotRunNotFound) {
		t.Errorf("PlaceSnapshotHold on an unknown run: %v, want ErrSnapshotRunNotFound", err)
	}

	// The fields that make a hold actionable later.
	live := heldRun(t, j, "run-live", "key-live", set, "manifest-live")
	for _, bad := range []struct {
		name string
		req  SnapshotHoldRequest
	}{
		{"no hold id", SnapshotHoldRequest{RunID: live.RunID, Reason: "r", PlacedBy: "ops", At: snapshotRunAt}},
		{"no reason", SnapshotHoldRequest{HoldID: "h", RunID: live.RunID, PlacedBy: "ops", At: snapshotRunAt}},
		{"no placer", SnapshotHoldRequest{HoldID: "h", RunID: live.RunID, Reason: "r", At: snapshotRunAt}},
		{"no time", SnapshotHoldRequest{HoldID: "h", RunID: live.RunID, Reason: "r", PlacedBy: "ops"}},
	} {
		if _, err := j.PlaceSnapshotHold(ctx, bad.req); err == nil {
			t.Errorf("PlaceSnapshotHold accepted a request with %s", bad.name)
		}
	}

	if err := j.ReleaseSnapshotHold(ctx, "hold-that-never-was", snapshotRunAt, "ops"); !errors.Is(err, ErrSnapshotHoldNotFound) {
		t.Errorf("ReleaseSnapshotHold on an unknown hold: %v, want ErrSnapshotHoldNotFound", err)
	}
}

// TestPlaceSnapshotHoldReplaysItsOwnIdAndRefusesSomebodyElsesRun is the
// idempotency contract every durable write in this package holds: a caller
// that crashed between placing a hold and observing it must resolve to the
// hold it already placed, and must never have its key answer for a
// different snapshot.
func TestPlaceSnapshotHoldReplaysItsOwnIdAndRefusesSomebodyElsesRun(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres")
	first := heldRun(t, j, "run-a", "key-a", set, "manifest-a")
	second := heldRun(t, j, "run-b", "key-b", set, "manifest-b")

	placed := snapshotRunAt.Add(time.Hour)
	if _, err := j.PlaceSnapshotHold(ctx, SnapshotHoldRequest{
		HoldID: "hold-x", RunID: first.RunID, Reason: "audit", PlacedBy: "ops", At: placed,
	}); err != nil {
		t.Fatalf("PlaceSnapshotHold: %v", err)
	}

	replay, err := j.PlaceSnapshotHold(ctx, SnapshotHoldRequest{
		HoldID: "hold-x", RunID: first.RunID, Reason: "audit", PlacedBy: "ops", At: placed.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("PlaceSnapshotHold (replay): %v", err)
	}
	if !replay.PlacedAt.Equal(placed) {
		t.Errorf("PlacedAt = %s on replay, want the original %s: how long a hold has stood is the fact this column carries",
			replay.PlacedAt, placed)
	}

	if _, err := j.PlaceSnapshotHold(ctx, SnapshotHoldRequest{
		HoldID: "hold-x", RunID: second.RunID, Reason: "audit", PlacedBy: "ops", At: placed,
	}); err == nil {
		t.Error("PlaceSnapshotHold let one hold id answer for two different runs")
	}
}

// TestPlaceSnapshotHoldRefusesAHoldThatArrivesTooLate is the race the
// phase check alone does not close.
//
// A retention pass records its delete INTENT durably before it asks the
// repository for anything, precisely so that a crash in the middle is
// decidable. Between that write and the manifest actually going, the run
// is still at SUCCESS -- so a hold placed in that window used to be
// accepted, against a snapshot whose deletion was already under way and
// would complete moments later. The same goes for a run at LOST, whose
// manifest is not in the repository at all.
//
// Both are the failure this write exists to prevent, in its most
// expensive shape: a person places a legal hold, gets a row back, and
// stops worrying about a snapshot that is already gone.
func TestPlaceSnapshotHoldRefusesAHoldThatArrivesTooLate(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres")

	// A delete this product has already committed to.
	going := heldRun(t, j, "run-going", "key-going", set, "manifest-going")
	if err := j.MarkSnapshotDeleteRequested(ctx, going.RunID, snapshotRunAt.Add(time.Hour)); err != nil {
		t.Fatalf("MarkSnapshotDeleteRequested: %v", err)
	}
	if _, err := j.PlaceSnapshotHold(ctx, SnapshotHoldRequest{
		HoldID: "hold-going", RunID: going.RunID, Reason: "litigation", PlacedBy: "ops", At: snapshotRunAt.Add(2 * time.Hour),
	}); err == nil {
		t.Error("PlaceSnapshotHold accepted a hold on a snapshot whose delete this product had already recorded; " +
			"the run is still at SUCCESS for a few more seconds and the hold protects nothing")
	}

	// A run whose manifest has gone without a delete ever being recorded.
	lost := heldRun(t, j, "run-lost", "key-lost", set, "manifest-lost")
	if err := j.AdvanceSnapshotRun(ctx, lost.RunID, PhaseLost, SnapshotRunUpdate{At: snapshotRunAt.Add(time.Hour)}); err != nil {
		t.Fatalf("AdvanceSnapshotRun(LOST): %v", err)
	}
	if _, err := j.PlaceSnapshotHold(ctx, SnapshotHoldRequest{
		HoldID: "hold-lost", RunID: lost.RunID, Reason: "litigation", PlacedBy: "ops", At: snapshotRunAt.Add(2 * time.Hour),
	}); err == nil {
		t.Error("PlaceSnapshotHold accepted a hold on a run at LOST, whose manifest is not in the repository to protect")
	}
}

// TestPlaceSnapshotHoldRefusesToReplayAReleasedHold is the other
// accepted-and-protects-nothing shape, and the one a careful caller still
// walks into.
//
// Replaying a hold id is how a caller that crashed mid-write resolves what
// it did, and the answer it gets back is a hold row. If that hold has
// since been RELEASED, returning it with a nil error tells a caller which
// checks the error -- and not Active() -- that its snapshot is protected,
// when the protection was deliberately ended by somebody else. The refusal
// is its own error so that a caller can tell "this id is spent" from "this
// id belongs to another run".
func TestPlaceSnapshotHoldRefusesToReplayAReleasedHold(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres")
	run := heldRun(t, j, "run-replay", "key-replay", set, "manifest-replay")

	placed := snapshotRunAt.Add(time.Hour)
	if _, err := j.PlaceSnapshotHold(ctx, SnapshotHoldRequest{
		HoldID: "hold-spent", RunID: run.RunID, Reason: "audit", PlacedBy: "ops", At: placed,
	}); err != nil {
		t.Fatalf("PlaceSnapshotHold: %v", err)
	}
	if err := j.ReleaseSnapshotHold(ctx, "hold-spent", placed.Add(time.Hour), "ops"); err != nil {
		t.Fatalf("ReleaseSnapshotHold: %v", err)
	}

	replay, err := j.PlaceSnapshotHold(ctx, SnapshotHoldRequest{
		HoldID: "hold-spent", RunID: run.RunID, Reason: "audit", PlacedBy: "ops", At: placed.Add(2 * time.Hour),
	})
	if !errors.Is(err, ErrSnapshotHoldReleased) {
		t.Errorf("PlaceSnapshotHold replaying a released hold returned (%+v, %v), want ErrSnapshotHoldReleased: "+
			"a caller that reads the error and not Active() would believe this snapshot is protected", replay, err)
	}

	// The released row is untouched: releasing is a fact, and a refused
	// replay must not resurrect or rewrite it.
	all, err := j.SnapshotHolds(ctx, run.RunID)
	if err != nil {
		t.Fatalf("SnapshotHolds: %v", err)
	}
	if len(all) != 1 || all[0].Active() {
		t.Errorf("the hold history is %+v, want the one released hold left as it was", all)
	}
}
