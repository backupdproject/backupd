package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What a backup this BINARY executes does about EPIC L (#813).
//
// `run`, `daemon` and `fetch` used to build their service through
// openService, which installs no workflow lifecycle at all. So a backup
// of a workflow-configured set taken by this binary created no workflow
// run, ran no hook script, and never asked whether the set was blocked by
// an interrupted run -- the one that makes it a safety defect rather than
// a missing feature: an ordinary `backupd fetch` proceeded over a source
// that an interrupted hook had left quiesced, took a backup of a stopped
// database, and reported it as a good one.
//
// # How that is asserted here, and where the executing evidence lives
//
// Since #865 a local hook runs inside an ephemeral Docker container, so a
// runner that can really EXECUTE one needs a daemon and belongs to the
// machine tier (core/tests/containerhooks is #865's evidence, and
// core/internal/testtier forbids a unit-tier package from reaching a
// container at all). What is asserted here instead is sharper than "the
// hook ran" for this purpose: a fetch of a set whose hooks this
// deployment CANNOT execute must be refused and must transfer nothing.
// Only a process with the lifecycle really installed refuses it -- a
// process with a nil lifecycle transfers the artifact and exits 0, which
// is exactly the defect.

// workflowFixtureWithoutARunner is a deployment whose backup set runs a
// NAME.local.sh hook and which has no host workflow runner configured.
//
// It builds its own directory rather than calling workflowFixture,
// because the whole path has to be free of symbolic links: a run spools
// its captured scripts under the state directory, and workflow.Snapshot
// refuses to create a spool through a link (on a Mac, Go's per-test
// directory is under one).
func workflowFixtureWithoutARunner(t *testing.T) (configPath, root string) {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	configPath = writeTestConfigIn(t, dir, filepath.Join(dir, "state.db"))

	root = filepath.Join(dir, "workflows")
	for _, sub := range []string{"", "global-before", "global-after"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	block := "workflows:\n" +
		"  root: " + root + "\n" +
		"  global:\n" +
		"    before_dir: global-before\n" +
		"    after_dir: global-after\n" +
		"  script_timeout: 2m\n"
	if err := os.WriteFile(configPath, append(raw, []byte(block)...), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	return configPath, root
}

// writeHook writes one executable hook into a stage directory.
func writeHook(t *testing.T, stageDir, name, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(stageDir, name), []byte(body), 0o700); err != nil {
		t.Fatalf("WriteFile(%s): %v", name, err)
	}
}

// TestFetchGoesThroughTheWorkflowLifecycle is the blocker, asserted
// through the one outcome only an installed lifecycle can produce.
func TestFetchGoesThroughTheWorkflowLifecycle(t *testing.T) {
	configPath, root := workflowFixtureWithoutARunner(t)
	writeHook(t, filepath.Join(root, "global-before"), "10-quiesce.local.sh", "#!/bin/bash\ntrue\n")

	var code int
	printed := captureStderr(t, func() {
		captureStdout(t, func() {
			code = run([]string{"fetch", "--backup-set", "production/postgres-primary", "--config", configPath})
		})
	})

	if code == 0 {
		t.Fatalf("the fetch exited 0 for a set whose hook scripts this deployment cannot execute; it took the backup outside its workflow. stderr:\n%s", printed)
	}
	if !strings.Contains(printed, "host workflow runner") {
		t.Errorf("the failure does not name the runner, so it is not the lifecycle's refusal:\n%s", printed)
	}

	// And nothing was transferred: one object was waiting on the remote,
	// and a pass that ran outside its workflow would have landed it.
	landed := filepath.Join(filepath.Dir(configPath), "local", "backup.dump")
	if _, err := os.Stat(landed); err == nil {
		t.Errorf("%s exists: the backup was taken even though the set's hooks could not run", landed)
	}
}

// TestRunGoesThroughTheWorkflowLifecycle is the same for the cycle verb,
// because a nightly `run` from cron is the path a deployment actually
// relies on.
func TestRunGoesThroughTheWorkflowLifecycle(t *testing.T) {
	configPath, root := workflowFixtureWithoutARunner(t)
	writeHook(t, filepath.Join(root, "global-before"), "10-quiesce.local.sh", "#!/bin/bash\ntrue\n")

	var code int
	captureStderr(t, func() {
		captureStdout(t, func() {
			code = run([]string{"run", "--config", configPath})
		})
	})

	if code == 0 {
		t.Error("`run` exited 0 for a cycle whose only backup set could not run its hooks")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(configPath), "local", "backup.dump")); err == nil {
		t.Error("the cycle transferred an artifact outside the set's workflow")
	}
}

// TestFetchIsRefusedBesideAServingEngine keeps the two executors apart.
//
// A second process that reconciled would mark the serving engine's
// in-flight run interrupted and block the set underneath a backup that is
// still running, and one that did not reconcile would be the
// nil-lifecycle backup this whole file is about. So the answer is a
// refusal, at exit 3, like every other beside-a-live-engine case.
func TestFetchIsRefusedBesideAServingEngine(t *testing.T) {
	configPath := writeTestConfig(t)
	stop := startEngineHolding(t, configPath)
	defer stop()

	var code int
	out := captureStderr(t, func() {
		code = run([]string{"fetch", "--backup-set", "production/postgres-primary", "--config", configPath})
	})

	if code != 3 {
		t.Errorf("fetch beside a serving engine = %d, want 3; stderr:\n%s", code, out)
	}
	if !strings.Contains(out, "already serving this deployment") {
		t.Errorf("the refusal does not say what was found:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(configPath), "local", "backup.dump")); err == nil {
		t.Error("the refused fetch transferred an artifact anyway")
	}
}

// TestFetchDryRunStillAnswersBesideAServingEngine is the other half of
// that refusal: a dry run transfers nothing, records nothing and runs no
// hook, so it is a read and must keep working where an operator usually
// is when they preview a set.
func TestFetchDryRunStillAnswersBesideAServingEngine(t *testing.T) {
	configPath := writeTestConfig(t)
	stop := startEngineHolding(t, configPath)
	defer stop()

	var code int
	out := captureStdout(t, func() {
		code = run([]string{"fetch", "--backup-set", "production/postgres-primary", "--dry-run", "--config", configPath})
	})

	if code != 0 {
		t.Errorf("a dry run beside a serving engine = %d, want 0; stdout:\n%s", code, out)
	}
	if !strings.Contains(out, "object(s) on the remote") {
		t.Errorf("the dry run printed no preview:\n%s", out)
	}
}
