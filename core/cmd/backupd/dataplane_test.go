package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/hostrunner"
)

// What a backup this BINARY executes does about EPIC L (#813).
//
// `run`, `daemon` and `fetch` used to build their service through
// openService, which installs no workflow lifecycle at all. So a backup
// of a workflow-configured set taken by this binary created no workflow
// run, ran no hook script, and never asked whether the set was blocked by
// an interrupted run -- which is the one that makes it a safety defect
// rather than a missing feature: an ordinary `backupd fetch` proceeded
// over a source that an interrupted hook had left quiesced, took a
// backup of a stopped database, and reported it as a good one.
//
// Nothing here fakes the runner. A real host workflow runner on a real
// socket running real bash is what makes "the hook ran" and "no hook ran"
// evidence about a process boundary rather than about a stand-in.

// serveRunnerForCLI starts a host workflow runner this binary's own
// version can talk to, and returns its socket and credential file.
//
// The version is this package's `version` variable, because that is what
// openServingDataPlane states through SetBuildVersion and the runner
// refuses a hello whose version is not its own.
func serveRunnerForCLI(t *testing.T) (socket, tokenFile string) {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("the runner refuses to run as root, so this test cannot drive one here")
	}

	// Short, and therefore not t.TempDir(): a Unix socket address is a
	// fixed 104-byte field on Darwin, and Go names its per-test directory
	// after the test.
	root, err := os.MkdirTemp("/tmp", "bdcli")
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

	server, err := hostrunner.NewServer(hostrunner.Config{
		Layout:   layout,
		Version:  version,
		Token:    []byte(token),
		Bash:     bash,
		Grace:    200 * time.Millisecond,
		EUID:     os.Geteuid(),
		Username: hostrunner.CurrentUsername(os.Geteuid()),
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

// workflowFixtureWithRunner is a deployment whose backup set runs local
// hooks: the ordinary test configuration, a workflow root with the four
// stage directories, and the runner block an engine needs in order to
// execute a NAME.local.sh at all.
//
// It builds its own directory rather than calling workflowFixture,
// because the whole path has to be free of symbolic links: a run spools
// its captured scripts under the state directory, and workflow.Snapshot
// refuses to create a spool through a link (on a Mac, Go's per-test
// directory is under one).
func workflowFixtureWithRunner(t *testing.T) (configPath, root string) {
	t.Helper()

	socket, tokenFile := serveRunnerForCLI(t)

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
		"  script_timeout: 2m\n" +
		"  runner:\n" +
		"    socket: " + socket + "\n" +
		"    token_file: " + tokenFile + "\n"
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

// TestFetchRunsTheSetsHooksAndRecordsAWorkflowRun is the positive half of
// the blocker: a backup this binary takes is wrapped in the set's
// workflow, so the hooks fire and the run is on record.
func TestFetchRunsTheSetsHooksAndRecordsAWorkflowRun(t *testing.T) {
	configPath, root := workflowFixtureWithRunner(t)

	marker := filepath.Join(t.TempDir(), "the-hook-ran")
	writeHook(t, filepath.Join(root, "global-before"), "10-quiesce.local.sh",
		"#!/bin/bash\ntouch "+marker+"\n")

	argv := []string{"fetch", "--backup-set", "production/postgres-primary", "--config", configPath}
	var code int
	out := captureStdout(t, func() { code = run(argv) })
	if code != 0 {
		t.Fatalf("run(%v) = %d, want 0; stdout:\n%s", argv, code, out)
	}

	// The pass really ran: this is a local-backend remote with one file
	// waiting, so the artifact has to have landed.
	landed := filepath.Join(filepath.Dir(configPath), "local", "backup.dump")
	if _, err := os.Stat(landed); err != nil {
		t.Fatalf("the fetch transferred nothing, so this test is not observing a real pass: %v", err)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the set's \"before\" hook did not run (%s is absent), so this binary backed a configured set up outside its workflow: %v", marker, err)
	}

	// And the run is durable, which is what an operator reads afterwards
	// and what a recovery would be resumed from.
	listed := captureStdout(t, func() {
		if got := run([]string{"workflow", "run", "list", "--config", configPath}); got != 0 {
			t.Fatalf("workflow run list = %d", got)
		}
	})
	if !strings.Contains(listed, "production/postgres-primary") {
		t.Errorf("the fetch recorded no workflow run for the set it backed up:\n%s", listed)
	}
}

// fetchChildEnv and fetchChildConfig drive the child process below.
const (
	fetchChildEnv    = "BACKUP_MANAGER_TEST_FETCH_CHILD"
	fetchChildConfig = "BACKUP_MANAGER_TEST_FETCH_CONFIG"
)

// TestFetchChildProcess is not a test. It is the entry point of the child
// process TestFetchIsRefusedWhileACleanupIsOutstanding kills mid-hook,
// and it skips itself in an ordinary run.
func TestFetchChildProcess(t *testing.T) {
	if os.Getenv(fetchChildEnv) != "1" {
		t.Skip("child-process entry point: only runs when a parent test re-executes this binary")
	}
	os.Exit(run([]string{"fetch", "--backup-set", "production/postgres-primary", "--config", os.Getenv(fetchChildConfig)}))
}

// TestFetchIsRefusedWhileACleanupIsOutstanding is the safety property
// itself: a backup set whose previous run was interrupted with a cleanup
// nobody has accounted for refuses to back up until a person deals with
// it, and that refusal has to reach a backup THIS BINARY takes.
//
// The interruption is real. A child process runs `fetch`, its "before"
// hook is caught mid-execution and the process is killed outright, which
// leaves the run row open with the global scope entered and nothing
// discharged -- exactly what a host reboot or an OOM kill leaves. The
// next invocation reconciles, finds it, and must refuse.
//
// Before this, that invocation built its service through openService, so
// it had no lifecycle at all: it re-ran the "before" hook, quiesced the
// source again, took the backup and exited 0.
func TestFetchIsRefusedWhileACleanupIsOutstanding(t *testing.T) {
	configPath, root := workflowFixtureWithRunner(t)

	marker := filepath.Join(t.TempDir(), "before-ran")
	writeHook(t, filepath.Join(root, "global-before"), "10-quiesce.local.sh",
		"#!/bin/bash\ntouch "+marker+"\nsleep 120\n")

	child := exec.Command(os.Args[0], "-test.run=^TestFetchChildProcess$")
	child.Env = append(os.Environ(), fetchChildEnv+"=1", fetchChildConfig+"="+configPath)
	if err := child.Start(); err != nil {
		t.Fatalf("starting the fetch child: %v", err)
	}
	t.Cleanup(func() { _ = child.Process.Kill() })

	// Waited for rather than slept past: the marker appearing is the
	// hook really executing on the runner, which is what makes the kill
	// an interruption of a run that had entered its first scope.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child's \"before\" hook never ran, so nothing was interrupted and this test would pass vacuously")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := child.Process.Kill(); err != nil {
		t.Fatalf("killing the fetch child: %v", err)
	}
	// Reaped, because the kernel releases the journal lock when the
	// process is reaped and the next invocation has to be able to take
	// it.
	_ = child.Wait()
	// The killed run is on record as unsettled. It is not a HOLD yet:
	// nothing has reconciled since the kill, and `workflow recovery
	// show` deliberately reconciles nothing (it would reconcile a live
	// engine's in-flight run), so it reports the row as in flight or
	// abandoned. The next engine start is what decides, and the next
	// engine start is the fetch below.
	outstanding := captureStdout(t, func() {
		run([]string{"workflow", "recovery", "show", "--config", configPath})
	})
	if !strings.Contains(outstanding, "production/postgres-primary") {
		t.Fatalf("the killed run left no unsettled row, so there is nothing for the next run to be refused against:\n%s", outstanding)
	}

	// Whatever the next fetch does, it must not quiesce the source
	// again: removing the marker means a hook that ran would put it
	// back.
	if err := os.Remove(marker); err != nil {
		t.Fatalf("Remove(%s): %v", marker, err)
	}

	var code int
	second := captureStderr(t, func() {
		code = run([]string{"fetch", "--backup-set", "production/postgres-primary", "--config", configPath})
	})
	if code == 0 {
		t.Errorf("the fetch exited 0 while a cleanup nobody has accounted for is outstanding:\n%s", second)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the refused fetch ran the set's \"before\" hook, so it quiesced a source it was not allowed to back up")
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
