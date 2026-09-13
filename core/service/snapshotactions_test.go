package service

import (
	"context"
	"errors"
	"testing"
)

// The idempotency key on EPIC K's four snapshot actions, at the layer
// that mints the durable row.
//
// The key exists so a resubmitted request resolves to the operation it
// already created. What it must not do is resolve a DIFFERENT request to
// that operation, and the snapshot actions are where the two are hardest
// to tell apart: hold this snapshot and hold that one are the same
// actor, the same action and the same configuration revision, differing
// only in the parameter object. A client reusing one key across two
// targets was answered with the first operation and a "not created"
// flag, which every surface reads as "your request is already in
// flight".

// TestCreateSnapshotOperation_SameKeyDifferentParametersIsAConflict is
// that, asked through the seam all four submissions share.
//
// Both directions again, and the second is what stops the first from
// being satisfied by a service that refused every retry: a genuine
// resubmission of the identical request still resolves to the row that
// exists, which is the property the key is there for.
func TestCreateSnapshotOperation_SameKeyDifferentParametersIsAConflict(t *testing.T) {
	svc, _ := openServiceWithTransport(t, nil)
	ctx := context.Background()

	first, created, err := svc.createSnapshotOperation(ctx, "rev-1", ActionHoldSnapshot, "alice", "one-key", "production/postgres",
		snapshotHoldParameters{BackupSetID: "production/postgres", RunID: "run-1", Reason: "the incident review"})
	if err != nil || !created {
		t.Fatalf("the first hold: created=%v err=%v, want a created row", created, err)
	}

	_, _, err = svc.createSnapshotOperation(ctx, "rev-1", ActionHoldSnapshot, "alice", "one-key", "production/postgres",
		snapshotHoldParameters{BackupSetID: "production/postgres", RunID: "run-2", Reason: "the incident review"})
	if !errors.Is(err, ErrIdempotencyKeyConflict) {
		t.Fatalf("holding a DIFFERENT run under the same key = %v, want ErrIdempotencyKeyConflict: replaying the first row tells the caller run-2 is held when run-1 is", err)
	}

	replay, created, err := svc.createSnapshotOperation(ctx, "rev-1", ActionHoldSnapshot, "alice", "one-key", "production/postgres",
		snapshotHoldParameters{BackupSetID: "production/postgres", RunID: "run-1", Reason: "the incident review"})
	if err != nil {
		t.Fatalf("resubmitting the identical hold: %v", err)
	}
	if created {
		t.Error("an identical resubmission created a second row, so one retried click places two holds")
	}
	if replay.ID != first.ID {
		t.Errorf("an identical resubmission resolved to operation %q, want the original %q", replay.ID, first.ID)
	}
}
