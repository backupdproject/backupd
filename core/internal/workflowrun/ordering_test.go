package workflowrun

import (
	"context"
	"reflect"
	"testing"

	"github.com/backupdproject/backupd/core/internal/workflow"
)

// The five-stage ordering matrix (#811's first acceptance criterion).
//
// The order is global-before, backup-set-before, the backup, then
// backup-set-after and global-after: the "after" stages unwind in the
// reverse of the order the "before" stages were entered, which is the
// same nesting a shell trap, a defer stack and a transaction all use, and
// the only order in which a global hook that mounted something can rely
// on the per-set hooks having finished with it before it unmounts.
//
// The plan is mixed local and remote deliberately. A run whose steps all
// went to one executor would prove the sequencing of one adapter; what
// has to hold is that the sequence is the PLAN's and is unaffected by
// which side of the socket each step runs on.

func TestFiveStagesRunInPlanOrderAcrossBothTargets(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh", "20-notify.remote.sh"},
		setBefore:    {"10-quiesce.remote.sh", "20-checkpoint.local.sh"},
		setAfter:     {"10-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	res, err := h.run(t, tr.snapshot(t, "run-1"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{
		"local:10-mount.local.sh",
		"remote:20-notify.remote.sh",
		"remote:10-quiesce.remote.sh",
		"local:20-checkpoint.local.sh",
		"backup",
		"remote:10-resume.remote.sh",
		"local:90-unmount.local.sh",
	}
	if got := h.rec.dispatched(); !reflect.DeepEqual(got, want) {
		t.Errorf("the run dispatched\n\t%v\nwant\n\t%v", got, want)
	}

	if res.State != workflow.StateSuccess {
		t.Errorf("the run ended %q, want success", res.State)
	}
	if res.BackupStatus != workflow.StatusSuccess {
		t.Errorf("backup status is %q, want success", res.BackupStatus)
	}
	if res.CleanupStatus != workflow.StatusSuccess {
		t.Errorf("cleanup status is %q, want success", res.CleanupStatus)
	}
	if res.WorkflowStatus != workflow.StatusSuccess {
		t.Errorf("workflow status is %q, want success", res.WorkflowStatus)
	}
	if res.ScriptCount != 6 {
		t.Errorf("the run reports %d scripts, want 6", res.ScriptCount)
	}
	if res.FailedStep != "" {
		t.Errorf("a successful run names %q as its failed step", res.FailedStep)
	}
	if res.Duration <= 0 {
		t.Errorf("the run reports a duration of %s", res.Duration)
	}
	if res.Bypassed {
		t.Error("a run that executed every hook reports itself bypassed")
	}
}

// Every step reaches a terminal recorded state and the journal agrees
// with what came back, because the journal is what every surface above
// this reads and a result that only existed in memory would be a history
// with a hole in it wherever the process restarted.
func TestEveryStepIsDurablyRecorded(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setBefore:    {"10-quiesce.remote.sh"},
		setAfter:     {"10-resume.remote.sh"},
		globalAfter:  {"90-unmount.local.sh"},
	})

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	steps, err := h.store.WorkflowSteps(t.Context(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowSteps: %v", err)
	}
	if len(steps) != 4 {
		t.Fatalf("the journal has %d steps, want 4", len(steps))
	}

	for _, s := range steps {
		if s.State != string(workflow.StateSuccess) {
			t.Errorf("step %s is %q, want success", s.ScriptName, s.State)
		}
		if s.StartedAt == nil || s.FinishedAt == nil {
			t.Errorf("step %s has no start or finish time", s.ScriptName)
		}
		if s.ExitCode == nil || *s.ExitCode != 0 {
			t.Errorf("step %s recorded exit code %v", s.ScriptName, s.ExitCode)
		}
	}

	run, err := h.store.WorkflowRun(t.Context(), "run-1")
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.State != string(workflow.StateSuccess) {
		t.Errorf("the recorded run is %q, want success", run.State)
	}
	if run.FinishedAt == nil {
		t.Error("the recorded run has no finish time")
	}

	// Both scopes were entered and both discharged their obligation.
	for _, scope := range workflow.Scopes() {
		if got := obligationOf(t, h.store, "run-1", scope).State; got != workflow.ObligationSuccess {
			t.Errorf("the %s obligation is %q, want success", scope, got)
		}
	}
}

// A step is durably RUNNING before its bytes reach a runner. This is the
// transition every crash-recovery claim in #811 is built on, so it is
// asserted from inside the executor: at the moment the hook is handed
// over, a separate read of the journal already says the step is running.
func TestAStepIsDurablyRunningBeforeItsBytesReachARunner(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
	})

	var observed string
	h.local.outcomes["10-mount.local.sh"] = func(ctx context.Context, req StepRequest) (StepOutcome, error) {
		observed = stateOf(t, h.store, req.RunID, req.Step.ScriptName).State

		return exited(0), nil
	}

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if observed != string(workflow.StateRunning) {
		t.Errorf("while the hook was executing the journal said %q, want %q; a hook whose bytes reached a runner before the journal recorded it can leave a side effect no recovery pass will ever find",
			observed, workflow.StateRunning)
	}
}

// The environment a hook is handed: this product's own facts about the
// run, and the honest answer about the backup that has not happened yet.
func TestBuiltinsTellAHookWhereItIsInTheRun(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
		setAfter:     {"10-resume.remote.sh"},
	})

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	before := h.rec.envOf(t, "10-mount.local.sh")
	for name, want := range map[string]string{
		"BACKUPD":                 "1",
		"BACKUPD_RUN_ID":          "run-1",
		"BACKUPD_BACKUP_SET_ID":   "production/postgres-primary",
		"BACKUPD_BACKUP_SET_NAME": "postgres-primary",
		"BACKUPD_PHASE":           "before",
		"BACKUPD_STEP_NAME":       "10-mount.local.sh",
		"BACKUPD_STEP_TARGET":     "local",
		"BACKUPD_BACKUP_STATUS":   "unknown",
		"BACKUPD_CLEANUP_STATUS":  "unknown",
		"BACKUPD_WORKFLOW_STATUS": "running",
		"BACKUPD_RECOVERY":        "0",
		"BACKUPD_CLEANUP_REASON":  "run_completed",
	} {
		if got := before[name]; got != want {
			t.Errorf("a before hook saw %s=%q, want %q", name, got, want)
		}
	}
	if before["BACKUPD_STARTED_AT"] == "" {
		t.Error("a before hook was told no start time")
	}

	// The after hook is told what actually happened, which is the whole
	// reason the three statuses are separate fields.
	after := h.rec.envOf(t, "10-resume.remote.sh")
	for name, want := range map[string]string{
		"BACKUPD_PHASE":           "after",
		"BACKUPD_BACKUP_STATUS":   "success",
		"BACKUPD_CLEANUP_STATUS":  "running",
		"BACKUPD_WORKFLOW_STATUS": "running",
		"BACKUPD_STEP_TARGET":     "remote",
	} {
		if got := after[name]; got != want {
			t.Errorf("an after hook saw %s=%q, want %q", name, got, want)
		}
	}
}

// A step runs the SPOOLED bytes, verified against the plan's hash, and
// never a path in the workflow tree. The executors are handed the bytes
// and have nothing else they could open.
func TestAStepIsHandedTheSpooledBytes(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-mount.local.sh"},
	})

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()

	if len(h.rec.bodies) == 0 {
		t.Fatal("nothing was dispatched")
	}
	if want := "echo 10-mount.local.sh"; !contains(h.rec.bodies[0], want) {
		t.Errorf("the executor was handed %q, want it to contain %q", h.rec.bodies[0], want)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}

	return -1
}
