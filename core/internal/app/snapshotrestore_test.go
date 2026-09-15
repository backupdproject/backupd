package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/model"
)

// A restore that goes all the way through: a real cycle writes a real
// snapshot into a real repository, and then the restore path an operator
// reaches finds the restore point for itself and puts the tree back.
//
// The point of doing it end to end here rather than with a repository
// double is what the double would have to invent: which snapshot "the
// last known good one" means is a catalog fact written by the run, and a
// test that handed the id straight to the restore would never exercise
// the lookup that an operator's request actually depends on.

func TestRestoreSnapshot_RestoresTheLastKnownGoodRestorePoint(t *testing.T) {
	t.Parallel()

	cfg, _, sourceDir := incrementalDeployment(t)
	files := map[string][]byte{
		"index.txt":         incompressibleBytes(64<<10, 11),
		"runs/2026/db.dump": incompressibleBytes(64<<10, 12),
		"runs/2026/log.txt": incompressibleBytes(64<<10, 13),
	}
	seedTree(t, sourceDir, files)

	svc, _ := incrementalService(t, cfg)
	ctx := context.Background()

	if set := svc.RunCycle(ctx).Sets[0]; set.Err != nil || set.Snapshot == nil || !set.Snapshot.Succeeded() {
		t.Fatalf("the cycle produced no restore point to restore from: %v", set.Err)
	}

	dest := filepath.Join(t.TempDir(), "restored")

	result, err := svc.RestoreSnapshot(ctx, SnapshotRestoreRequest{
		SourceName: "production",
		SetName:    "uploads-tree",
		TargetPath: dest,
	})
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	if result.SnapshotID == "" {
		t.Error("the restore reports no restore point, so nobody can write down which one they got back")
	}

	if !result.Report.Complete {
		t.Error("a restore that returned no error did not report itself complete")
	}

	if result.Report.Files != int64(len(files)) {
		t.Errorf("the restore wrote %d files; the snapshot holds %d", result.Report.Files, len(files))
	}

	// Verification is not optional on this path: the operation that
	// records a restore records it as evidence.
	if result.Report.Verified != result.Report.Files {
		t.Errorf("the restore verified %d of the %d files it wrote", result.Report.Verified, result.Report.Files)
	}

	for name, body := range files {
		restored, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("reading restored %s: %v", name, err)

			continue
		}

		if sumOf(restored) != sumOf(body) {
			t.Errorf("restored %s does not match what was backed up", name)
		}
	}
}

// TestRestoreSnapshot_RestoresOneFileOutOfTheRestorePoint is the request
// an operator actually makes most often: one file back, without unpacking
// everything beside it.
func TestRestoreSnapshot_RestoresOneFileOutOfTheRestorePoint(t *testing.T) {
	t.Parallel()

	cfg, _, sourceDir := incrementalDeployment(t)
	wanted := incompressibleBytes(64<<10, 21)
	seedTree(t, sourceDir, map[string][]byte{
		"index.txt":         incompressibleBytes(64<<10, 22),
		"runs/2026/db.dump": wanted,
	})

	svc, _ := incrementalService(t, cfg)
	ctx := context.Background()

	if set := svc.RunCycle(ctx).Sets[0]; set.Err != nil {
		t.Fatalf("the cycle failed: %v", set.Err)
	}

	dest := filepath.Join(t.TempDir(), "restored")

	result, err := svc.RestoreSnapshot(ctx, SnapshotRestoreRequest{
		SourceName: "production",
		SetName:    "uploads-tree",
		SourcePath: "runs/2026/db.dump",
		TargetPath: dest,
	})
	if err != nil {
		t.Fatalf("RestoreSnapshot of one file: %v", err)
	}

	if result.Report.Files != 1 {
		t.Errorf("a single-file restore wrote %d files", result.Report.Files)
	}

	restored, err := os.ReadFile(filepath.Join(dest, "db.dump"))
	if err != nil {
		t.Fatalf("reading the restored file: %v", err)
	}

	if sumOf(restored) != sumOf(wanted) {
		t.Error("the restored file does not match what was backed up")
	}

	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("reading the destination: %v", err)
	}

	if len(entries) != 1 {
		t.Errorf("a single-file restore left %d entries in the destination", len(entries))
	}
}

// TestRestoreSnapshot_RefusalsAreTheirOwnAnswers: every refusal below
// sends an operator somewhere different, so none of them may arrive as
// the same error.
func TestRestoreSnapshot_RefusalsAreTheirOwnAnswers(t *testing.T) {
	t.Parallel()

	cfg, _, _ := incrementalDeployment(t)
	svc, _ := incrementalService(t, cfg)
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "restored")

	t.Run("a set nobody configured", func(t *testing.T) {
		_, err := svc.RestoreSnapshot(ctx, SnapshotRestoreRequest{
			SourceName: "production",
			SetName:    "not-a-set",
			TargetPath: dest,
		})

		if !errors.Is(err, ErrBackupSetNotConfigured) {
			t.Errorf("restoring an unconfigured set returned %v; want ErrBackupSetNotConfigured", err)
		}
	})

	t.Run("a set that never ran", func(t *testing.T) {
		_, err := svc.RestoreSnapshot(ctx, SnapshotRestoreRequest{
			SourceName: "production",
			SetName:    "uploads-tree",
			TargetPath: dest,
		})

		if !errors.Is(err, ErrSetHasNoSnapshots) {
			t.Errorf("restoring a set with no restore point returned %v; want ErrSetHasNoSnapshots", err)
		}
	})

	t.Run("an artifact set", func(t *testing.T) {
		artifactCfg, _, _ := incrementalDeployment(t)
		artifactCfg.Sources[0].BackupSets[0].Engine = model.EngineArtifact

		artifactSvc, _ := incrementalService(t, artifactCfg)

		_, err := artifactSvc.RestoreSnapshot(ctx, SnapshotRestoreRequest{
			SourceName: "production",
			SetName:    "uploads-tree",
			TargetPath: dest,
		})

		if !errors.Is(err, ErrNotAnIncrementalSet) {
			t.Errorf("restoring an artifact set as a snapshot returned %v; want ErrNotAnIncrementalSet", err)
		}
	})

	t.Run("a snapshot id that names nothing", func(t *testing.T) {
		_, err := svc.RestoreSnapshot(ctx, SnapshotRestoreRequest{
			SourceName: "production",
			SetName:    "uploads-tree",
			SnapshotID: "0123456789abcdef0123456789abcdef",
			TargetPath: dest,
		})

		if !errors.Is(err, backupengine.ErrSnapshotNotFound) && !errors.Is(err, backupengine.ErrRepositoryNotFound) {
			t.Errorf("restoring an unknown snapshot returned %v; want a missing snapshot or repository", err)
		}
	})
}
