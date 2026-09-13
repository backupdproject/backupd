package main

import (
	"strings"
	"testing"
)

// Where a recovery action goes when something is serving this deployment
// (#813).
//
// Recovery state lives in two places: the journal, and the refusal set in
// the memory of the process that reconciled it
// (workflowrun/recovery.go's "why the refusal is in memory and the truth
// is on disk"). So an acknowledgement or a resume performed in a SECOND
// process wrote the journal and left the serving engine still refusing
// the backup set it had just unblocked -- until somebody restarted that
// process -- and two processes could resume one run at once, because the
// lock that would stop them is a table in the other one's memory.
//
// The engine here is a real one over HTTP with a real core/service behind
// it (fakeengine_test.go), so a request this test observes is a request
// the engine really answered.

// TestAcknowledgeTravelsToTheServingEngine is the routing itself.
//
// The assertion is the REQUEST, because that is the whole property: the
// work has to be performed by the process that holds the refusal set,
// and a command that wrote the journal here would leave that process's
// answer unchanged whatever the journal then said.
func TestAcknowledgeTravelsToTheServingEngine(t *testing.T) {
	cliConfig := writeTestConfig(t)
	engine := startFakeEngineFor(t, cliConfig)
	engine.attach(t)

	code := run([]string{"workflow", "recovery", "acknowledge", "wfr_does_not_exist",
		"--reason", "checked the host by hand", "--config", cliConfig})

	// The run does not exist, so the answer is a refusal -- and the
	// refusal came from the ENGINE, which is what this test is about.
	if code == 0 {
		t.Error("acknowledging a run no journal holds succeeded")
	}
	if !sawRequest(engine, "/workflow-recovery/wfr_does_not_exist/acknowledge") {
		t.Errorf("the acknowledgement never reached the serving engine; it was performed locally, so that engine's holds are now stale. Requests: %v", engine.requests())
	}
}

// TestResumeCleanupTravelsToTheServingEngine is the same for the other
// mutation, which is the more consequential one: a resume EXECUTES hook
// scripts, and executing them from a second process means the engine that
// is refusing the set never learns they ran.
func TestResumeCleanupTravelsToTheServingEngine(t *testing.T) {
	cliConfig := writeTestConfig(t)
	engine := startFakeEngineFor(t, cliConfig)
	engine.attach(t)

	if code := run([]string{"workflow", "recovery", "resume-cleanup", "wfr_does_not_exist", "--config", cliConfig}); code == 0 {
		t.Error("resuming a run no journal holds succeeded")
	}
	if !sawRequest(engine, "/workflow-recovery/wfr_does_not_exist/resume-cleanup") {
		t.Errorf("the resume never reached the serving engine. Requests: %v", engine.requests())
	}
}

// TestRecoveryShowAsksTheServingEngineForItsHolds is the read half: the
// holds an operator is shown have to be the ones the next run will be
// refused against, and those are the serving process's.
func TestRecoveryShowAsksTheServingEngineForItsHolds(t *testing.T) {
	cliConfig := writeTestConfig(t)
	engine := startFakeEngineFor(t, cliConfig)
	engine.attach(t)

	var code int
	out := captureStdout(t, func() {
		code = run([]string{"workflow", "recovery", "show", "--config", cliConfig})
	})
	if code != 0 {
		t.Fatalf("workflow recovery show = %d; stdout:\n%s", code, out)
	}
	if !sawRequest(engine, "/workflow-recovery") {
		t.Errorf("the report was assembled from this process's own journal rather than from the engine holding the refusals. Requests: %v", engine.requests())
	}
	if !strings.Contains(out, "no backup set is being held") {
		t.Errorf("the routed report does not say whose answer it is:\n%s", out)
	}
}

// TestRunLogAsksTheServingEngine is the fourth verb. A follow's wake-up
// comes from the broker in the process executing the run, so a local read
// polls a journal nobody is writing.
func TestRunLogAsksTheServingEngine(t *testing.T) {
	cliConfig := writeTestConfig(t)
	engine := startFakeEngineFor(t, cliConfig)
	engine.attach(t)

	if code := run([]string{"workflow", "run", "log", "wfr_does_not_exist", "--step", "0000~global~before~10.local.sh", "--config", cliConfig}); code == 0 {
		t.Error("reading the log of a run no journal holds succeeded")
	}
	if !sawRequest(engine, "/workflow-runs/wfr_does_not_exist/steps/") {
		t.Errorf("the log read never reached the serving engine. Requests: %v", engine.requests())
	}
}

// TestAcknowledgeIsRefusedBesideAnUnreachableEngine is the third outcome,
// and the one that makes the other two safe: with something serving and
// no address, the mutation is refused rather than performed in the wrong
// process.
func TestAcknowledgeIsRefusedBesideAnUnreachableEngine(t *testing.T) {
	configPath := writeTestConfig(t)
	stop := startEngineHolding(t, configPath)
	defer stop()

	var code int
	out := captureStderr(t, func() {
		code = run([]string{"workflow", "recovery", "acknowledge", "wfr_1",
			"--reason", "checked it", "--config", configPath})
	})

	if code != 3 {
		t.Errorf("acknowledging beside an unreachable serving engine = %d, want 3; stderr:\n%s", code, out)
	}
	if !strings.Contains(out, "already serving this deployment") {
		t.Errorf("the refusal does not say what was found:\n%s", out)
	}
}

// sawRequest reports whether the engine was asked anything whose path
// contains want.
func sawRequest(engine *fakeEngine, want string) bool {
	for _, req := range engine.requests() {
		if strings.Contains(req, want) {
			return true
		}
	}

	return false
}
