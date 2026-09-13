package app

import (
	"errors"
	"testing"

	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/model"
)

// What the workflow seam may and may not say about a pass it wrapped
// (#813).
//
// The distinction under test is the one core/service/cyclesummary_test.go
// already pins from the other end: a quarantined or failed ARTIFACT is a
// business outcome that a completed cycle reports as counts, and a
// systemic failure is a cycle that could not be performed. The lifecycle
// has to be told which of those happened -- an "after" hook reads it as
// BACKUPD_BACKUP_STATUS -- and the sentinel it is told it with must not
// then come back out as the pass's own Err, because that is what turns a
// counted artifact failure into a cycle nobody ran.

func aSet() config.BackupSet {
	return config.BackupSet{ID: model.BackupSetID{Source: "prod", Set: "db"}}
}

// TestAFailedArtifactInsideAWorkflowStaysABusinessOutcome is the fold's
// central rule: the backup RAN, so its own result stands.
func TestAFailedArtifactInsideAWorkflowStaysABusinessOutcome(t *testing.T) {
	bs := aSet()
	pass := BackupSetCycleResult{
		Set:             bs.ID,
		FailedArtifacts: 1,
		Progress:        CycleProgress{Walked: 2, Durable: 1},
	}

	// What the lifecycle is handed: one bit saying the pass did not come
	// out clean, with no detail, because the detail is in pass itself.
	told := passOutcome(pass)
	if !errors.Is(told, errBackupSetPassFailed) {
		t.Fatalf("passOutcome = %v, want the pass-failed sentinel; the rest of this test is about that value", told)
	}

	// And what the engine hands back: a run whose BackupErr is exactly
	// that sentinel (workflowLifecycle.AroundBackupSet returns
	// res.BackupErr unchanged).
	folded := foldWorkflowRefusal(bs, pass, true, told)

	if folded.Err != nil {
		t.Errorf("the folded result carries Err = %v; a counted artifact failure is not a systemic failure, and storing the sentinel here makes executeRunCycle classify the whole cycle as one", folded.Err)
	}
	if v := folded.Verdict(); v.Systemic {
		t.Errorf("the folded result's verdict is systemic; the cycle completed and reported %d failed artifact(s)", folded.FailedArtifacts)
	}
	if folded.FailedArtifacts != 1 || folded.Progress != pass.Progress {
		t.Errorf("the folded result lost the pass's own counts: %+v", folded)
	}
}

// TestAWorkflowRefusalIsTheWholeStoryWhenTheBackupNeverRan is the other
// half, and it is what the fold is FOR: a set blocked by an unresolved
// interruption, or a "before" hook that said no, is a pass that did not
// happen.
func TestAWorkflowRefusalIsTheWholeStoryWhenTheBackupNeverRan(t *testing.T) {
	bs := aSet()
	blocked := errors.New("this backup set has an unresolved interruption")

	folded := foldWorkflowRefusal(bs, BackupSetCycleResult{}, false, blocked)

	if !errors.Is(folded.Err, blocked) {
		t.Errorf("Err = %v, want the refusal; a run that never happened has nothing else to report", folded.Err)
	}
	if !folded.Verdict().Systemic {
		t.Error("a refused pass has to read as a systemic failure: no backup was taken")
	}
}

// TestAFailedAfterHookStillFailsAPassThatRan keeps the fix above from
// swallowing the failure it is not about. A cleanup that could not run is
// #811's "a failed cleanup fails the run", and it arrives here as an
// error that is NOT the pass sentinel.
func TestAFailedAfterHookStillFailsAPassThatRan(t *testing.T) {
	bs := aSet()
	cleanup := errors.New("the cleanup hooks could not complete")

	folded := foldWorkflowRefusal(bs, BackupSetCycleResult{Set: bs.ID}, true, cleanup)

	if !errors.Is(folded.Err, cleanup) {
		t.Errorf("Err = %v, want the lifecycle's own failure", folded.Err)
	}
}
