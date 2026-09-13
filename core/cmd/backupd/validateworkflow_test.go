package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/service"
)

// `validate workflow <source/backup-set>`: the report, the exit code, and
// the one sentence #813 asks for by name (the set is sound and its hooks
// are not).

// TestValidateWorkflowReportsEveryCheckAndExitsOnTheVerdict is the happy
// path: a deployment with a root, two empty stage directories and no
// hooks yet, which is where every deployment is on the day somebody turns
// this feature on.
//
// Two things are asserted and the second is the one that earns its place.
// Every check reports, skipped ones included, because a check that did
// not run is not a check that passed and an operator debugging a remote
// hook needs "skipped: this set has no remote scripts" rather than
// silence. And the exit code is the verdict, because that is what makes
// this usable in the monitoring shape `check` and `repository health`
// already are.
func TestValidateWorkflowReportsEveryCheckAndExitsOnTheVerdict(t *testing.T) {
	configPath, _ := workflowFixture(t)

	var code int
	out := captureStdout(t, func() {
		code = run([]string{"validate", "workflow", "production/postgres-primary", "--config", configPath})
	})

	for _, check := range service.WorkflowChecks() {
		if !strings.Contains(out, check) {
			t.Errorf("the report does not mention the %q check at all, so an operator cannot tell whether it passed, failed or was never run:\n%s", check, out)
		}
	}
	if !strings.Contains(out, "valid for backup:") || !strings.Contains(out, "workflow valid:") {
		t.Fatalf("the report does not carry both verdicts:\n%s", out)
	}

	// The exit code has to agree with the verdict it printed, whichever
	// way the verdict went on this machine: a validation that reports
	// unsound hooks and exits 0 is a monitoring surface that never fires.
	sound := strings.Contains(out, "workflow valid:   true")
	switch {
	case sound && code != 0:
		t.Errorf("the report says the hooks are sound and the command exited %d", code)
	case !sound && code != 1:
		t.Errorf("the report says the hooks are not sound and the command exited %d, want 1:\n%s", code, out)
	}
}

// TestValidateWorkflowSaysWhenTheSetIsSoundAndItsHooksAreNot is the
// combination #813 requires to be visibly sayable, driven by the way it
// actually happens: a stage directory somebody has configured and not
// created.
//
// The set itself is untouched -- same source, same destination, same
// retention -- so a report that folded the two verdicts together would
// tell an operator their backups are broken when what is broken is a
// directory. That is the failure this test exists to keep out, and the
// line it looks for is the one that makes the distinction readable rather
// than leaving it to two booleans an operator has to interpret.
func TestValidateWorkflowSaysWhenTheSetIsSoundAndItsHooksAreNot(t *testing.T) {
	configPath, root := workflowFixture(t)

	// A configured stage that is not there. Snapshot refuses it, which is
	// what a real run would do, so this is the same refusal a backup of
	// this set would meet tonight.
	if code := run([]string{"settings", "workflow", "patch", "--before-dir", "not-created-yet", "--config", configPath}); code != 0 {
		t.Fatalf("configuring a missing stage directory = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(root, "not-created-yet")); !os.IsNotExist(err) {
		t.Fatalf("the fixture's missing stage directory exists after all (Stat err = %v)", err)
	}

	var code int
	out := captureStdout(t, func() {
		code = run([]string{"validate", "workflow", "production/postgres-primary", "--config", configPath})
	})
	if code != 1 {
		t.Fatalf("a set whose hooks cannot be snapshotted exited %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "valid for backup: true") {
		t.Fatalf("this test needs a set that is sound on its own, and the fixture reported otherwise:\n%s", out)
	}
	if !strings.Contains(out, "this backup set is sound and its hooks are not") {
		t.Errorf("the report leaves the two verdicts to be interpreted from two booleans; #813 requires the combination to be said:\n%s", out)
	}
	if !strings.Contains(out, "resolved_directories") {
		t.Errorf("the failing check is not named in the report:\n%s", out)
	}
}

// TestValidateWorkflowJSONCarriesTheWholeReport pins what a script binds
// to.
//
// The whole report and not a summary: both verdicts, every finding
// including the skipped ones, and the resolved script list. A --json that
// filtered would be a second, quieter opinion about which checks matter,
// and the UI reading this surface has to be able to group by check id.
func TestValidateWorkflowJSONCarriesTheWholeReport(t *testing.T) {
	configPath, _ := workflowFixture(t)

	out := captureStdout(t, func() {
		// The exit code is the verdict and is not what this test is
		// about; it is asserted next door.
		run([]string{"validate", "workflow", "production/postgres-primary", "--config", configPath, "--json"})
	})

	var report service.WorkflowValidation
	if err := json.Unmarshal([]byte(jsonObjectIn(t, out)), &report); err != nil {
		t.Fatalf("--json did not emit a decodable report: %v\n%s", err, out)
	}
	if report.BackupSetID != "production/postgres-primary" {
		t.Errorf("the report names backup set %q", report.BackupSetID)
	}

	// One finding per check at least, and possibly several: a check that
	// is per script reports once per script (a syntax check over three
	// hooks is three answers), so this asserts COVERAGE rather than a
	// count. What must not happen is a check that goes unreported, which
	// is a surface claiming nothing about something it looked at.
	reported := map[string]bool{}
	for _, f := range report.Findings {
		reported[f.Check] = true
	}
	for _, check := range service.WorkflowChecks() {
		if !reported[check] {
			t.Errorf("--json carries no finding for the %q check, so a client grouping by check id has a silent hole in it", check)
		}
	}

	// The findings have to be identifiable by their closed vocabulary
	// rather than by their sentence, which is the whole reason the check
	// ids are values and the details are prose.
	known := map[string]bool{}
	for _, check := range service.WorkflowChecks() {
		known[check] = true
	}
	for _, f := range report.Findings {
		if !known[f.Check] {
			t.Errorf("the report carries a finding for check %q, which core/service does not declare", f.Check)
		}
		if f.Severity == "" {
			t.Errorf("the finding for %q carries no severity, so nothing can tell a pass from a skip", f.Check)
		}
	}
}

// TestValidateStillValidatesAnArtifact is the regression guard for the
// operand this form shares a command with.
//
// cmdValidate now reads its first operand as a possible verb, and the
// artifact form is the one that was there first. An id that is not the
// word "workflow" has to reach app.ParseArtifactID exactly as it did
// before, so the refusal below is the artifact form's own (a bad artifact
// id, exit 1) rather than the new form's usage complaint.
func TestValidateStillValidatesAnArtifact(t *testing.T) {
	configPath := writeTestConfig(t)

	var code int
	out := captureStderr(t, func() {
		code = run([]string{"validate", "production/postgres-primary/never-collected.dump", "--config", configPath})
	})
	if code != 1 {
		t.Fatalf("validating an artifact this deployment does not hold = %d, want 1 (an ordinary failure about the deployment); stderr:\n%s", code, out)
	}
}
