// The durable operation half of a local snapshot restore (#787).
//
// What is asserted here is the RECEIPT, not the restore: that a submitted
// restore becomes a row a client can poll, that the row reaches a
// terminal status even when the work below fails or panics, that its
// completed summary says which restore point was used and what landed,
// and -- the one that matters most -- that a restore which did not finish
// is never written down as one that did.
//
// The engine underneath is substituted through the same seam runCycle and
// runBackupSetFetch use. That is deliberate: a restore's own correctness
// is proven against a real Kopia repository in
// core/internal/backupengine/kopia and core/internal/app, and what those
// cannot force deterministically is a restore that returns an incomplete
// report, or one that panics.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/app"
	"github.com/backupdproject/backupd/core/internal/backupengine"
)

// withRestoreStub substitutes the restore seam for one test and puts the
// real one back afterwards.
func withRestoreStub(t *testing.T, stub func(*app.Service, context.Context, app.SnapshotRestoreRequest) (app.SnapshotRestoreResult, error)) {
	t.Helper()

	original := restoreSnapshot
	restoreSnapshot = stub

	t.Cleanup(func() { restoreSnapshot = original })
}

func submitFixtureRestore(t *testing.T, svc *BackupService, req SnapshotRestoreRequest) (Operation, error) {
	t.Helper()

	if req.ConfigRevision == "" {
		req.ConfigRevision = svc.ConfigRevision()
	}

	if req.BackupSetID == "" {
		req.BackupSetID = fixtureSetID
	}

	if req.TargetPath == "" {
		req.TargetPath = t.TempDir()
	}

	return svc.SubmitSnapshotRestore(context.Background(), req)
}

// TestSubmitSnapshotRestore_RecordsWhatLandedAndWhichRestorePoint is the
// acceptance criterion that a restore appears in the operation surfaces
// the same way every other durable action does.
func TestSubmitSnapshotRestore_RecordsWhatLandedAndWhichRestorePoint(t *testing.T) {
	svc, _ := openTestService(t)

	withRestoreStub(t, func(_ *app.Service, _ context.Context, req app.SnapshotRestoreRequest) (app.SnapshotRestoreResult, error) {
		return app.SnapshotRestoreResult{
			// The caller named no snapshot, so the engine resolved the
			// set's last known good one. The row is the only place that
			// answer survives.
			SnapshotID: "resolved-restore-point",
			Report: backupengine.RestoreReport{
				Files:       3,
				Directories: 2,
				Bytes:       4096,
				Verified:    3,
				Complete:    true,
			},
		}, nil
	})

	op, err := submitFixtureRestore(t, svc, SnapshotRestoreRequest{
		IdempotencyKey: "restore-1",
		Actor:          "alice",
		SourcePath:     "runs/2026",
	})
	if err != nil {
		t.Fatalf("SubmitSnapshotRestore: %v", err)
	}

	if op.Action != ActionRestoreSnapshot {
		t.Errorf("Action = %q, want %q", op.Action, ActionRestoreSnapshot)
	}

	if op.BackupSetID != fixtureSetID {
		t.Errorf("BackupSetID = %q, want %q", op.BackupSetID, fixtureSetID)
	}

	done := waitForTerminalStatus(t, svc, op.ID)
	if done.Status != "completed" {
		t.Fatalf("status = %q, want completed (error = %q)", done.Status, done.Error)
	}

	var summary snapshotRestoreSummary
	if err := json.Unmarshal([]byte(done.Result), &summary); err != nil {
		t.Fatalf("the completed restore recorded an unreadable summary %q: %v", done.Result, err)
	}

	if summary.SnapshotID != "resolved-restore-point" {
		t.Errorf("the summary records restore point %q; a caller who named none cannot otherwise learn which one they got",
			summary.SnapshotID)
	}

	if summary.Files != 3 || summary.VerifiedFiles != 3 || summary.Bytes != 4096 {
		t.Errorf("the summary records %d files (%d verified) and %d bytes; want 3, 3, 4096",
			summary.Files, summary.VerifiedFiles, summary.Bytes)
	}

	if summary.SourcePath != "runs/2026" || summary.TargetPath == "" {
		t.Errorf("the summary records source %q into target %q; a restore nobody can locate afterwards is a restore nobody can use",
			summary.SourcePath, summary.TargetPath)
	}

	// And the row is in the deployment-wide list, which is what a client
	// that never submitted it reads.
	ops, err := svc.ListOperations(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListOperations: %v", err)
	}

	found := false

	for _, listed := range ops {
		if listed.ID == op.ID && listed.Action == ActionRestoreSnapshot {
			found = true
		}
	}

	if !found {
		t.Error("a submitted restore does not appear in the operations list, so it is invisible to anything but its own poll")
	}
}

// TestSubmitSnapshotRestore_AnIncompleteRestoreIsNeverRecordedAsFinished
// is the acceptance criterion about cancellation, asserted where it
// actually matters.
//
// A restore torn down partway leaves real files in the destination and a
// report that does not claim completeness. Recording that as "completed"
// would advertise a partial tree as a finished restore to every surface
// that reads the row, which is the one failure mode a restore must not
// have -- and it is exactly what a caller that only checks the error
// would do.
func TestSubmitSnapshotRestore_AnIncompleteRestoreIsNeverRecordedAsFinished(t *testing.T) {
	svc, _ := openTestService(t)

	withRestoreStub(t, func(*app.Service, context.Context, app.SnapshotRestoreRequest) (app.SnapshotRestoreResult, error) {
		return app.SnapshotRestoreResult{
			SnapshotID: "partial",
			Report: backupengine.RestoreReport{
				Files:    2,
				Bytes:    99,
				Complete: false,
			},
		}, nil
	})

	op, err := submitFixtureRestore(t, svc, SnapshotRestoreRequest{IdempotencyKey: "restore-partial"})
	if err != nil {
		t.Fatalf("SubmitSnapshotRestore: %v", err)
	}

	done := waitForTerminalStatus(t, svc, op.ID)
	if done.Status != "failed" {
		t.Fatalf("a restore that did not claim completeness was recorded as %q with result %q", done.Status, done.Result)
	}

	if !strings.Contains(done.Error, "did not finish") {
		t.Errorf("the row's reason is %q, which does not tell an operator the destination holds only part of what they asked for", done.Error)
	}
}

// TestSubmitSnapshotRestore_ACancelledRestoreSaysSo is the other half of
// the cancellation story: the row says what happened in words an operator
// can act on, rather than in whatever sentence came up from below.
func TestSubmitSnapshotRestore_ACancelledRestoreSaysSo(t *testing.T) {
	svc, _ := openTestService(t)

	withRestoreStub(t, func(*app.Service, context.Context, app.SnapshotRestoreRequest) (app.SnapshotRestoreResult, error) {
		return app.SnapshotRestoreResult{}, context.Canceled
	})

	op, err := submitFixtureRestore(t, svc, SnapshotRestoreRequest{IdempotencyKey: "restore-cancelled"})
	if err != nil {
		t.Fatalf("SubmitSnapshotRestore: %v", err)
	}

	done := waitForTerminalStatus(t, svc, op.ID)
	if done.Status != "failed" {
		t.Fatalf("a cancelled restore ended at %q", done.Status)
	}

	if !strings.Contains(done.Error, "stopped before it finished") {
		t.Errorf("a cancelled restore recorded %q", done.Error)
	}
}

// TestSubmitSnapshotRestore_RecoversFromAPanicAndRecordsFailure: this
// runs on a goroutine inside an always-on process, so an unrecovered
// panic takes the API server with it.
func TestSubmitSnapshotRestore_RecoversFromAPanicAndRecordsFailure(t *testing.T) {
	svc, _ := openTestService(t)

	withRestoreStub(t, func(*app.Service, context.Context, app.SnapshotRestoreRequest) (app.SnapshotRestoreResult, error) {
		panic("the restore path blew up")
	})

	op, err := submitFixtureRestore(t, svc, SnapshotRestoreRequest{IdempotencyKey: "restore-panic"})
	if err != nil {
		t.Fatalf("SubmitSnapshotRestore: %v", err)
	}

	if done := waitForTerminalStatus(t, svc, op.ID); done.Status != "failed" {
		t.Fatalf("a panicking restore ended at %q, want failed", done.Status)
	}
}

// TestSubmitSnapshotRestore_RefusalsHappenBeforeAnyRowExists: a request
// this deployment cannot serve must not leave an operation somebody polls
// forever, and each refusal has to be its own sentinel because each sends
// the caller somewhere different.
func TestSubmitSnapshotRestore_RefusalsHappenBeforeAnyRowExists(t *testing.T) {
	svc, _ := openTestService(t)

	withRestoreStub(t, func(*app.Service, context.Context, app.SnapshotRestoreRequest) (app.SnapshotRestoreResult, error) {
		t.Error("a refused restore reached the engine")

		return app.SnapshotRestoreResult{}, nil
	})

	for _, tc := range []struct {
		name string
		req  SnapshotRestoreRequest
		want error
	}{
		{
			name: "no idempotency key",
			req:  SnapshotRestoreRequest{ConfigRevision: svc.ConfigRevision(), BackupSetID: fixtureSetID, TargetPath: t.TempDir()},
			want: ErrInvalidRequest,
		},
		{
			name: "no destination",
			req:  SnapshotRestoreRequest{IdempotencyKey: "k1", ConfigRevision: svc.ConfigRevision(), BackupSetID: fixtureSetID, TargetPath: "-"},
			want: ErrInvalidRequest,
		},
		{
			name: "a conflict policy that is not one of the three",
			req: SnapshotRestoreRequest{
				IdempotencyKey: "k2", ConfigRevision: svc.ConfigRevision(), BackupSetID: fixtureSetID,
				TargetPath: t.TempDir(), Conflict: "Overwrite",
			},
			want: ErrInvalidRequest,
		},
		{
			name: "a stale configuration revision",
			req: SnapshotRestoreRequest{
				IdempotencyKey: "k3", ConfigRevision: "stale", BackupSetID: fixtureSetID, TargetPath: t.TempDir(),
			},
			want: ErrConfigRevisionStale,
		},
		{
			name: "a backup set nobody configured",
			req: SnapshotRestoreRequest{
				IdempotencyKey: "k4", ConfigRevision: svc.ConfigRevision(),
				BackupSetID: "production/not-a-set", TargetPath: t.TempDir(),
			},
			want: ErrBackupSetNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			if req.TargetPath == "-" {
				req.TargetPath = ""
			}

			before, err := svc.ListOperations(context.Background(), 100)
			if err != nil {
				t.Fatalf("ListOperations: %v", err)
			}

			if _, err := svc.SubmitSnapshotRestore(context.Background(), req); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}

			after, err := svc.ListOperations(context.Background(), 100)
			if err != nil {
				t.Fatalf("ListOperations: %v", err)
			}

			if len(after) != len(before) {
				t.Errorf("a refused restore left %d new operation row(s) behind", len(after)-len(before))
			}
		})
	}
}

// TestSubmitSnapshotRestore_ReplayingAnIdempotencyKeyDoesNotRestoreTwice:
// a retry of a request whose response was lost must observe the original,
// not start a second restore into the same directory.
func TestSubmitSnapshotRestore_ReplayingAnIdempotencyKeyDoesNotRestoreTwice(t *testing.T) {
	svc, _ := openTestService(t)

	started := make(chan struct{}, 8)

	withRestoreStub(t, func(*app.Service, context.Context, app.SnapshotRestoreRequest) (app.SnapshotRestoreResult, error) {
		started <- struct{}{}

		return app.SnapshotRestoreResult{SnapshotID: "s1", Report: backupengine.RestoreReport{Complete: true}}, nil
	})

	target := t.TempDir()

	first, err := submitFixtureRestore(t, svc, SnapshotRestoreRequest{IdempotencyKey: "replayed", TargetPath: target})
	if err != nil {
		t.Fatalf("first submission: %v", err)
	}

	if done := waitForTerminalStatus(t, svc, first.ID); done.Status != "completed" {
		t.Fatalf("first submission ended at %q", done.Status)
	}

	second, err := submitFixtureRestore(t, svc, SnapshotRestoreRequest{IdempotencyKey: "replayed", TargetPath: target})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if second.ID != first.ID {
		t.Errorf("a replayed idempotency key produced a second operation %q beside %q", second.ID, first.ID)
	}

	if len(started) != 1 {
		t.Errorf("the engine was asked to restore %d times for two submissions of one request", len(started))
	}
}
