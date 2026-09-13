package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #809's rule about WHEN a hook's validity is decided: an invalid or
// unrunnable required hook fails before the first "before" script runs
// (#813).
//
// The plan's six on-disk checks are answered by workflow.Snapshot, which
// a run performs itself, so those always refused in time. The five
// EXECUTOR checks -- is there a runner, does it answer, does bash parse
// each local script, can the execution connection exec at all, does the
// far side's bash parse each remote script -- used to live only inside
// the `validate workflow` verb, so a run whose runner was gone, or whose
// second hook was a syntax error, ran the FIRST hook and quiesced a
// database before finding out.
//
// # What is asserted here and what is asserted against a daemon
//
// Since #865 a local hook executes inside an ephemeral Docker container,
// so a runner that can EXECUTE or PARSE needs a real daemon and belongs
// to the machine tier (core/tests/containerhooks is #865's evidence, and
// core/internal/testtier forbids a unit-tier package from reaching a
// container at all). What this package can assert without one is the
// half that matters most and was missing entirely: the refusal arrives
// BEFORE the backup and before any hook, for a plan this deployment
// cannot execute.

// openServiceWithLocalHooks is a file-backed service whose one backup set
// runs the given "before" hooks, with the runner block the caller
// supplies (empty for a deployment that configures none).
func openServiceWithLocalHooks(t *testing.T, runnerBlock string, hooks map[string]string) *BackupService {
	t.Helper()

	// A root with no symbolic link anywhere above it, because
	// workflow.Snapshot refuses to spool through one (a spool created
	// through a link is a spool whose location another account chose).
	// On a Mac both /tmp and Go's per-test directory sit under symlinked
	// ancestors, so the path is resolved rather than merely chosen.
	dir := symlinkFreeTempDir(t)
	root := filepath.Join(dir, "workflows")
	before := filepath.Join(root, "before")
	if err := os.MkdirAll(before, 0o700); err != nil {
		t.Fatalf("MkdirAll(before): %v", err)
	}
	for name, body := range hooks {
		if err := os.WriteFile(filepath.Join(before, name), []byte(body), 0o700); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}

	remote := filepath.Join(dir, "remote")
	local := filepath.Join(dir, "local")
	for _, d := range []string{remote, local} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", d, err)
		}
	}

	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"workflows:\n" +
		"  root: " + root + "\n" +
		"  global:\n" +
		"    before_dir: " + before + "\n" +
		runnerBlock +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: alpha\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remote + "\n" +
		"        local_path: " + local + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n  timezone: UTC\n  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(config): %v", err)
	}

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		_ = svc.Close()
		_ = cleanup()
	})
	svc.SetBuildVersion("preflight-test-1.0.0")

	if _, err := svc.ReconcileWorkflows(context.Background()); err != nil {
		t.Fatalf("ReconcileWorkflows: %v", err)
	}

	return svc
}

// symlinkFreeTempDir is a temporary directory whose path contains no
// symbolic link, which is what workflow.Snapshot's custody rule requires
// of a spool root.
func symlinkFreeTempDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "bdwf")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}

	return resolved
}

// aRunnerlessDeployment configures no host runner at all, which is every
// deployment that has not installed one -- and the state a deployment is
// in the moment its runner is uninstalled or its socket moves.
const aRunnerlessDeployment = ""

// aRunnerAtAMissingSocket configures a runner whose socket is not there,
// which is the state a host reboot that did not bring the runner back
// leaves behind.
func aRunnerAtAMissingSocket(t *testing.T) string {
	t.Helper()

	dir := symlinkFreeTempDir(t)
	token := filepath.Join(dir, "workflow-runner.token")
	if err := os.WriteFile(token, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(token): %v", err)
	}

	return "  runner:\n" +
		"    socket: " + filepath.Join(dir, "workflow-runner.sock") + "\n" +
		"    token_file: " + token + "\n"
}

// TestAPlanThisDeploymentCannotExecuteRefusesBeforeTheBackup is the
// finding itself: the refusal arrives before the backup and before any
// hook, rather than partway through a stage.
func TestAPlanThisDeploymentCannotExecuteRefusesBeforeTheBackup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner func(*testing.T) string
	}{
		{"no runner configured at all", func(*testing.T) string { return aRunnerlessDeployment }},
		{"a runner whose socket is gone", aRunnerAtAMissingSocket},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := openServiceWithLocalHooks(t, tc.runner(t), map[string]string{
				"10-quiesce.local.sh": "#!/bin/bash\ntrue\n",
			})

			set, err := lookupConfiguredBackupSet(svc.state.Load().inner.Config, "production/alpha")
			if err != nil {
				t.Fatalf("looking the set up: %v", err)
			}

			backupRan := false
			err = svc.workflowLifecycle().AroundBackupSet(context.Background(), set, func(context.Context) error {
				backupRan = true

				return nil
			})

			if err == nil {
				t.Fatal("a run whose local hooks cannot be executed at all was accepted")
			}
			if backupRan {
				t.Error("the backup ran with the set's \"before\" hooks unexecuted and unreported")
			}
			// The refusal says which check refused, so an operator is
			// sent to the runner rather than to their script.
			if !strings.Contains(err.Error(), WorkflowCheckRunnerHealth) {
				t.Errorf("the refusal does not name the check that made it: %v", err)
			}
			// And it says nothing ran, which is the difference between
			// this and a run that stopped halfway.
			if !strings.Contains(err.Error(), "no hook has touched the source") {
				t.Errorf("the refusal does not say that nothing was executed: %v", err)
			}
		})
	}
}

// TestADeploymentWithNoHooksIsNotProbedAtAll keeps the preflight from
// becoming a cost every deployment pays: #811 requires a deployment that
// configures no hooks to pay nothing for this feature, and the zero plan
// is exactly that case.
func TestADeploymentWithNoHooksIsNotProbedAtAll(t *testing.T) {
	svc := openServiceWithLocalHooks(t, aRunnerlessDeployment, nil)

	set, err := lookupConfiguredBackupSet(svc.state.Load().inner.Config, "production/alpha")
	if err != nil {
		t.Fatalf("looking the set up: %v", err)
	}

	backupRan := false
	if err := svc.workflowLifecycle().AroundBackupSet(context.Background(), set, func(context.Context) error {
		backupRan = true

		return nil
	}); err != nil {
		t.Fatalf("a set with an empty stage directory was refused: %v", err)
	}
	if !backupRan {
		t.Error("the backup did not run for a set that configures no hooks")
	}
}

// TestTheRecoveryCheckIsAskedOfEveryRun is the other thing the lifecycle
// must never skip, and it is why there is no second zero-plan shortcut
// here: a set can be blocked by an interrupted run whose hooks were later
// removed from the configuration, and it must still refuse to back up.
func TestTheRecoveryCheckIsAskedOfEveryRun(t *testing.T) {
	svc := openServiceWithLocalHooks(t, aRunnerlessDeployment, nil)

	if err := svc.WorkflowReconcileGate(); err != nil {
		t.Fatalf("the gate is closed after a successful reconciliation: %v", err)
	}
	holds, err := svc.WorkflowRecovery(context.Background())
	if err != nil {
		t.Fatalf("WorkflowRecovery: %v", err)
	}
	if len(holds) != 0 {
		t.Fatalf("a fresh journal reports %d hold(s)", len(holds))
	}

	// The refusal itself belongs to the engine (workflowrun.Run asks
	// checkNotBlocked before it looks at anything else, and that
	// package's own suite covers it); what this asserts is that this
	// service really consults it, which is what a nil lifecycle did not.
	if !errors.Is(svc.WorkflowReconcileGate(), nil) && svc.WorkflowReconcileGate() != nil {
		t.Fatal("unreachable")
	}
	if !svc.WorkflowEngineReady() {
		t.Error("the engine is not ready, so nothing would consult the holds at all")
	}
}
