package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/hostrunner"
	"github.com/backupdproject/backupd/core/internal/workflow"
)

// #809's rule about WHEN a hook's validity is decided: an invalid
// required hook fails before the first "before" script runs (#813).
//
// The plan's six on-disk checks are answered by workflow.Snapshot, which
// a run performs itself, so those always refused in time. The five
// executor checks -- is there a runner, does it answer, does bash parse
// each local script, can the execution connection exec at all, does the
// far side's bash parse each remote script -- used to live only inside
// the `validate workflow` verb, so a run whose SECOND hook was a syntax
// error ran the FIRST one and quiesced a database before finding out.
//
// Nothing here fakes the runner. A real server on a real socket with a
// real bash is what makes the assertion "the first hook left no marker"
// evidence: a stand-in inside the test binary would prove something
// about the stand-in, and the whole claim is about the process boundary.

// runnerVersion is the version both halves present. The runner refuses a
// hello whose version is not its own, which is the mismatch nothing else
// could detect, so the test states it once for both ends.
const runnerVersion = "preflight-test-1.0.0"

// serveRunner starts a host workflow runner and returns its socket and
// credential file, as the ENGINE has to be configured to see them.
func serveRunner(t *testing.T) (socket, tokenFile string) {
	t.Helper()

	// Short, and therefore not t.TempDir(): a Unix socket address is a
	// fixed 104-byte field on Darwin and Go names its per-test directory
	// after the test, so a test whose name is a sentence binds nothing at
	// all. internal/hostrunner's own suite makes the same choice for the
	// same reason.
	root, err := os.MkdirTemp("/tmp", "bdpf")
	if err != nil {
		t.Fatalf("preparing a short temporary root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	layout := hostrunner.Layout{
		RuntimeDir:   filepath.Join(root, "run"),
		WorkspaceDir: filepath.Join(root, "workspace"),
		SecretsDir:   filepath.Join(root, "secrets"),
	}
	for _, dir := range []string{layout.RuntimeDir, layout.WorkspaceDir, layout.SecretsDir} {
		if err := hostrunner.EnsureDir(dir); err != nil {
			t.Fatalf("preparing %s: %v", dir, err)
		}
	}

	bash, err := hostrunner.FindBash(context.Background(), "")
	if err != nil {
		t.Fatalf("this host has no bash, so nothing about a local hook can be checked: %v", err)
	}

	const token = "0123456789abcdef0123456789abcdef"
	tokenFile = filepath.Join(layout.SecretsDir, hostrunner.TokenName)
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(token): %v", err)
	}

	euid := os.Geteuid()
	if euid == 0 {
		t.Skip("the runner refuses to run as root, so this test cannot drive one here")
	}

	server, err := hostrunner.NewServer(hostrunner.Config{
		Layout:   layout,
		Version:  runnerVersion,
		Token:    []byte(token),
		Bash:     bash,
		Grace:    200 * time.Millisecond,
		EUID:     euid,
		Username: hostrunner.CurrentUsername(euid),
	})
	if err != nil {
		t.Fatalf("preparing the runner: %v", err)
	}
	if err := server.Listen(); err != nil {
		t.Fatalf("the runner could not listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = server.Close()
	})

	return layout.SocketPath(), tokenFile
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

// openServiceWithLocalHooks is a file-backed service whose one backup set
// runs the given "before" hooks, against the runner this test serves.
func openServiceWithLocalHooks(t *testing.T, socket, tokenFile string, hooks map[string]string) (*BackupService, workflow.Plan) {
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
		"  runner:\n" +
		"    socket: " + socket + "\n" +
		"    token_file: " + tokenFile + "\n" +
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
	svc.SetBuildVersion(runnerVersion)

	if _, err := svc.ReconcileWorkflows(context.Background()); err != nil {
		t.Fatalf("ReconcileWorkflows: %v", err)
	}

	set, err := lookupConfiguredBackupSet(svc.state.Load().inner.Config, "production/alpha")
	if err != nil {
		t.Fatalf("looking the set up: %v", err)
	}
	plan, err := svc.snapshotWorkflow(set, workflowRunOptions{})
	if err != nil {
		t.Fatalf("snapshotWorkflow: %v", err)
	}

	return svc, plan
}

// TestAnInvalidLaterHookRefusesBeforeTheFirstHookRuns is the finding
// itself, planted the way it happens: two "before" hooks, ordered by
// name, the first of which would quiesce something and the second of
// which does not parse.
func TestAnInvalidLaterHookRefusesBeforeTheFirstHookRuns(t *testing.T) {
	socket, tokenFile := serveRunner(t)

	marker := filepath.Join(t.TempDir(), "the-first-hook-ran")
	svc, _ := openServiceWithLocalHooks(t, socket, tokenFile, map[string]string{
		"10-quiesce.local.sh": "#!/bin/bash\ntouch " + marker + "\n",
		// `if` with no `fi`: bash -n refuses it, and bash running it
		// refuses it too -- but only after the hook before it has
		// already run.
		"20-broken.local.sh": "#!/bin/bash\nif true; then\n  echo halfway\n",
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
		t.Fatal("a workflow whose second hook does not parse was accepted; #809 requires an invalid required hook to fail before the first before script")
	}
	if backupRan {
		t.Error("the backup ran under a workflow this deployment cannot execute")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Errorf("the first hook executed (%s exists) before the invalid second hook was found; the source has been touched by a run that then refused", marker)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stat of the marker: %v", statErr)
	}
	if !strings.Contains(err.Error(), "20-broken.local.sh") {
		t.Errorf("the refusal does not name the script that caused it: %v", err)
	}
}

// TestAnUnreachableRunnerRefusesTheRunRatherThanTheFirstHook is the same
// rule for the other reason a local hook cannot run: the runner is gone.
//
// It is the case an operator meets after a host reboot that did not bring
// the runner back, and it is worth its own case because the refusal comes
// from a different probe: the plan is perfect and there is nothing to ask.
func TestAnUnreachableRunnerRefusesTheRunRatherThanTheFirstHook(t *testing.T) {
	socket, tokenFile := serveRunner(t)

	marker := filepath.Join(t.TempDir(), "the-first-hook-ran")
	svc, _ := openServiceWithLocalHooks(t, socket, tokenFile, map[string]string{
		"10-quiesce.local.sh": "#!/bin/bash\ntouch " + marker + "\n",
	})

	// The socket is removed AFTER the plan was captured, which is the
	// sequence a restart produces: the configuration is fine and the
	// door is not there.
	if err := os.Remove(socket); err != nil {
		t.Fatalf("removing the socket: %v", err)
	}

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
		t.Fatal("a run whose local hooks have no runner to execute them was accepted")
	}
	if backupRan {
		t.Error("the backup ran with the set's before hooks unexecuted and unreported")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("a hook ran against a runner this test had already stopped")
	}
}

// TestAPlanWhoseHooksAllParseIsNotRefused is the other half, and without
// it the two cases above would pass against a preflight that refused
// everything.
func TestAPlanWhoseHooksAllParseIsNotRefused(t *testing.T) {
	socket, tokenFile := serveRunner(t)

	marker := filepath.Join(t.TempDir(), "the-hook-ran")
	svc, _ := openServiceWithLocalHooks(t, socket, tokenFile, map[string]string{
		"10-quiesce.local.sh": "#!/bin/bash\ntouch " + marker + "\n",
	})

	set, err := lookupConfiguredBackupSet(svc.state.Load().inner.Config, "production/alpha")
	if err != nil {
		t.Fatalf("looking the set up: %v", err)
	}

	backupRan := false
	if err := svc.workflowLifecycle().AroundBackupSet(context.Background(), set, func(context.Context) error {
		backupRan = true

		return nil
	}); err != nil {
		t.Fatalf("AroundBackupSet refused a workflow whose hooks are all fine: %v", err)
	}

	if !backupRan {
		t.Error("the backup did not run")
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("the hook did not run: %v", statErr)
	}
}

// TestAFailedBeforeHookFailsThePassRatherThanPassingItSilently is the
// outcome this seam must never produce: a backup that did not happen,
// reported as one that did.
//
// The engine records a backup a "before" hook prevented as SKIPPED, with
// no BackupErr, because nothing was attempted. A lifecycle that only read
// BackupErr therefore handed internal/app a nil error for a pass that
// took no backup: the set was recorded with no error and no artifacts,
// `backupd run` exited 0, and the deployment reported a healthy night on
// which nothing was backed up.
func TestAFailedBeforeHookFailsThePassRatherThanPassingItSilently(t *testing.T) {
	socket, tokenFile := serveRunner(t)

	svc, _ := openServiceWithLocalHooks(t, socket, tokenFile, map[string]string{
		// Parses, and exits non-zero: the hook a database quiesce that
		// could not take its lock produces.
		"10-quiesce.local.sh": "#!/bin/bash\nexit 3\n",
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

	if backupRan {
		t.Fatal("the backup ran after its before hook failed")
	}
	if err == nil {
		t.Fatal("a pass whose before hook failed, and which therefore took no backup, was reported as a success")
	}
	if !strings.Contains(err.Error(), "did not take a backup") {
		t.Errorf("the failure does not say that no backup was taken: %v", err)
	}
}
