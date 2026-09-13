package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `fetch` beside EPIC L: what --dry-run says about a set's hooks, what
// --skip-workflow-scripts does instead of pretending, and the claim both
// of those rest on -- that this command runs no hook at all (#813).

// hookThatWouldLeaveAMarker writes an executable hook into stageDir and
// returns the path it would create if anything ever ran it.
//
// A marker file rather than a mock executor, because the claim being
// tested is about a process boundary: the hook would be dispatched by a
// host runner or over SSH by the process serving this deployment, and a
// stand-in installed inside the test binary would prove something about
// the stand-in. A file that is not there is evidence no interpreter
// anywhere ran these bytes.
func hookThatWouldLeaveAMarker(t *testing.T, stageDir string) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "the-hook-ran")
	script := "#!/usr/bin/env bash\nset -eu\ntouch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(stageDir, "10-quiesce.local.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	return marker
}

// TestFetchDryRunReportsTheResolvedWorkflowAndExecutesNothing is what
// --dry-run promises, applied to the half of a real run that matters
// most.
//
// --dry-run's promise is "what would a real run do", and since EPIC L a
// real run of a configured set wraps the pass in a five-stage workflow. A
// preview that listed the objects on the remote and said nothing about
// the hooks was answering a narrower question than the flag asks, and it
// is the question an operator reaching for --skip-workflow-scripts is
// usually actually asking.
//
// The two assertions are the two halves of "report, and run nothing": the
// stage this set would execute is named, and the hook sitting in it left
// no trace.
func TestFetchDryRunReportsTheResolvedWorkflowAndExecutesNothing(t *testing.T) {
	configPath, root := workflowFixture(t)
	marker := hookThatWouldLeaveAMarker(t, filepath.Join(root, "global-before"))

	argv := []string{"fetch", "--backup-set", "production/postgres-primary", "--dry-run", "--config", configPath}
	var code int
	out := captureStdout(t, func() { code = run(argv) })
	if code != 0 {
		t.Fatalf("run(%v) = %d, want 0; stdout:\n%s", argv, code, out)
	}

	if !strings.Contains(out, "workflow:") {
		t.Errorf("a dry run against a set with hooks said nothing about them:\n%s", out)
	}
	if !strings.Contains(out, root) {
		t.Errorf("the dry run did not name the approved root its stage directories are relative to:\n%s", out)
	}
	if !strings.Contains(out, "global-before") {
		t.Errorf("the dry run did not name the stage directory a real run would execute:\n%s", out)
	}
	if !strings.Contains(out, "executed none of them") {
		t.Errorf("the dry run listed stages without saying it ran none of them, which reads as a list of things it just did:\n%s", out)
	}

	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("--dry-run executed a hook: %s exists. A dry run may read and report and must change nothing", marker)
	} else if !os.IsNotExist(err) {
		t.Fatalf("Stat(%s): %v", marker, err)
	}

	// And nothing was transferred either, which is the part --dry-run
	// already promised before hooks existed.
	if _, err := os.Stat(filepath.Join(filepath.Dir(configPath), "local", "backup.dump")); !os.IsNotExist(err) {
		t.Errorf("--dry-run transferred an artifact (Stat err = %v)", err)
	}
}

// The test that used to sit here asserted the opposite of the property
// this binary now has, and it was deleted rather than re-pinned.
//
// It was TestFetchRunsNoHookEvenWithoutDryRun, and it pinned `fetch`
// taking a backup of a workflow-configured set while running none of
// that set's hooks -- which was true, was the defect, and is what
// dataplane_test.go's TestFetchRunsTheSetsHooksAndRecordsAWorkflowRun
// now asserts the other way round: the pass goes through the same
// reconciled lifecycle the engine uses, so the hooks fire, the run is on
// record, and a set blocked by an interrupted run is refused
// (TestFetchIsRefusedWhileACleanupIsOutstanding). Its own doc said it
// should fail the day this command was wired to a real lifecycle, and
// this is that day.

// TestFetchRefusesSkipWorkflowScriptsAndSaysWhereABypassLives drives the
// refusal against a deployment that loads, so the exit code cannot be an
// artifact of a missing configuration file.
//
// A refusal is worth having here and a silent acceptance is not. Accepted
// and ignored, this flag would exit 0, run no hook, and teach an operator
// both that `fetch` normally runs hooks and that this is how to stop it
// -- which is the belief #813's audit trail exists to prevent, since a
// bypass is supposed to be recorded on the run row and in a warn-level
// event naming who asked.
//
// The refusal has to name the alternative, because a refusal an operator
// cannot act on is only a slightly better silence.
func TestFetchRefusesSkipWorkflowScriptsAndSaysWhereABypassLives(t *testing.T) {
	configPath, root := workflowFixture(t)
	marker := hookThatWouldLeaveAMarker(t, filepath.Join(root, "global-before"))

	argv := []string{"fetch", "--backup-set", "production/postgres-primary", "--skip-workflow-scripts", "--config", configPath}
	var code int
	printed := captureStderr(t, func() {
		captureStdout(t, func() { code = run(argv) })
	})
	if code != exitUsage {
		t.Fatalf("run(%v) = %d, want %d; stderr:\n%s", argv, code, exitUsage, printed)
	}
	for _, expected := range []string{"skip-workflow-scripts", "production/postgres-primary", "workflow run list", "validate workflow"} {
		if !strings.Contains(printed, expected) {
			t.Errorf("the refusal does not mention %q, so an operator cannot act on it:\n%s", expected, printed)
		}
	}

	// Nothing ran: not the pass, and not a hook.
	if _, err := os.Stat(filepath.Join(filepath.Dir(configPath), "local", "backup.dump")); !os.IsNotExist(err) {
		t.Errorf("the refused command transferred an artifact anyway (Stat err = %v)", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("the refused command executed a hook: %s exists", marker)
	}
}

// TestFetchDryRunSaysSoWhenNoWorkflowIsConfigured is the other half of
// the report, and it is the case every deployment that has not adopted
// hooks is in.
//
// Silence would be indistinguishable from a stage list nobody printed, so
// the report states that a run would execute no hook rather than omitting
// the section. That is the same rule the env and stage printers follow:
// an absent thing is said out loud once, not left as a blank.
func TestFetchDryRunSaysSoWhenNoWorkflowIsConfigured(t *testing.T) {
	configPath := writeTestConfig(t)

	argv := []string{"fetch", "--backup-set", "production/postgres-primary", "--dry-run", "--config", configPath}
	var code int
	out := captureStdout(t, func() { code = run(argv) })
	if code != 0 {
		t.Fatalf("run(%v) = %d, want 0; stdout:\n%s", argv, code, out)
	}
	if !strings.Contains(out, "no workflow root") {
		t.Errorf("a dry run on a deployment with no hooks said nothing about that:\n%s", out)
	}
}
