package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/model"
)

// The on-demand restore drill, which is the only verification level that
// writes anything: it restores the whole snapshot to prove it can be
// restored.
//
// That makes it the one level with a housekeeping obligation, and two
// obligations rather than one. A drill that left its output behind leaves
// a full second copy of somebody's backup on a disk they are paying for,
// and a drill that restored into the SAME directory every time refused
// its own second attempt -- the restore runs under ConflictRefuse, on
// purpose, because a verification that can overwrite is a verification
// that can destroy data. The scheduled drill has settled both since #785
// (snapshotlifecycle's drillTarget and discardDrillOutput); the
// operator-triggered one reached neither.

// TestVerifySnapshot_TwoRestoreDrillsOfOneRunBothPassAndLeaveNothingBehind
// is the shape an operator produces by clicking "verify" twice, or by
// asking a second question after a first answer they did not trust.
//
// Nothing about the snapshot changed between the two, so a second pass
// that reports a failure is reporting on the FIRST pass's leftovers: a
// healthy restore point called damaged, which is the worst possible
// direction for this product to be wrong in.
func TestVerifySnapshot_TwoRestoreDrillsOfOneRunBothPassAndLeaveNothingBehind(t *testing.T) {
	t.Parallel()

	cfg, _, sourceDir := incrementalDeployment(t)
	seedTree(t, sourceDir, map[string][]byte{
		"notes.txt":      []byte("a line of somebody's data"),
		"nested/two.bin": incompressibleBytes(4096, 7),
	})

	svc, _ := incrementalService(t, cfg)
	ctx := context.Background()

	if set := svc.RunCycle(ctx).Sets[0]; set.Err != nil || set.Snapshot == nil || !set.Snapshot.Succeeded() {
		t.Fatalf("the cycle produced no restore point to drill: %v", set.Err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		result, err := svc.VerifySnapshot(ctx, VerifySnapshotRequest{
			SourceName: "production",
			SetName:    "uploads-tree",
			Level:      model.LevelRestoreDrill,
		})
		if err != nil {
			t.Fatalf("drill %d: VerifySnapshot: %v", attempt, err)
		}

		if !result.Passed {
			t.Fatalf("drill %d of an unchanged, healthy snapshot did not pass: %s", attempt, result.Err)
		}

		if result.Achieved != model.LevelRestoreDrill {
			t.Errorf("drill %d achieved %q, want %q: a drill that reports a weaker level proved something else", attempt, result.Achieved, model.LevelRestoreDrill)
		}
	}

	if left := drillLeftovers(t, cfg.EffectiveBackupRoot()); len(left) != 0 {
		t.Errorf("the drills left %v behind; each one is a full second copy of this backup on storage somebody pays for", left)
	}
}

// drillLeftovers is every directory still sitting in the reserved
// verification-drill area.
//
// The area itself is allowed to exist: it is created by the restore and
// removing the parent would be this check asserting something nobody
// promised. What must not survive a passing drill is anything INSIDE it.
func drillLeftovers(t *testing.T, backupRoot string) []string {
	t.Helper()

	reserved, err := backupengine.ReservedLocalStateDir(backupRoot)
	if err != nil {
		t.Fatalf("ReservedLocalStateDir: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(reserved, "verification-drills"))
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		t.Fatalf("reading the drill area: %v", err)
	}

	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}

	return out
}
