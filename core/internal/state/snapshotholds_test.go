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

	"github.com/backupdproject/backupd/core/internal/model"
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
