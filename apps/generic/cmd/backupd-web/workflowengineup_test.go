package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/service"
)

// The sequence a process has to perform before it serves, and the branch
// that used to skip it (#813).
//
// EPIC L's lifecycle is installed on reconciliation and by nothing else,
// so a process that serves without reconciling runs every backup exactly
// as it did before EPIC L: no hooks, and -- the part that makes it a
// safety defect -- no recovery check, so a configured set is backed up
// over a source an interrupted hook may have left quiesced.
//
// A start WITH a configuration did the sequence. First-run ACTIVATION did
// not: it opened the service and published it, so a fresh install served
// and scheduled with a nil lifecycle for the whole life of the process,
// which is the shape every app-store install begins in. Both branches now
// call openWorkflowEngineOn, and this file holds it to what that has to
// achieve.

// configWithALocalHookAndNoRunner is a deployment whose backup set runs a
// NAME.local.sh hook and which has no host workflow runner configured.
//
// That combination is what makes the lifecycle OBSERVABLE from outside
// core/service without a runner in this test binary: a run of this set
// has to be refused, in those words, and only a process that really
// installed the lifecycle can refuse it. A process with a nil lifecycle
// backs the set up and reports success.
func configWithALocalHookAndNoRunner(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	root := filepath.Join(dir, "workflows")
	before := filepath.Join(root, "before")
	if err := os.MkdirAll(before, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(before, "10-quiesce.local.sh"), []byte("#!/bin/bash\ntrue\n"), 0o700); err != nil {
		t.Fatalf("WriteFile(hook): %v", err)
	}

	remote := filepath.Join(dir, "remote")
	local := filepath.Join(dir, "local")
	for _, d := range []string{remote, local} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", d, err)
		}
	}
	// One object waiting on the remote, so "the pass never ran" is an
	// observable fact rather than an absence that would be true anyway.
	if err := os.WriteFile(filepath.Join(remote, "backup.dump"), []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile(remote object): %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"workflows:\n" +
		"  root: " + root + "\n" +
		"  global:\n" +
		"    before_dir: " + before + "\n" +
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

	return configPath
}

// awaitOperation polls one operation to a terminal status.
func awaitOperation(t *testing.T, backend *service.BackupService, id string) service.Operation {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		op, err := backend.GetOperation(context.Background(), id)
		if err != nil {
			t.Fatalf("GetOperation: %v", err)
		}
		if op.Status == "completed" || op.Status == "failed" {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s is still %s after 30s", id, op.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTheWorkflowLifecycleIsInstalledBeforeAnythingIsServed is the
// property both start branches now share, asserted through a run.
func TestTheWorkflowLifecycleIsInstalledBeforeAnythingIsServed(t *testing.T) {
	configPath := configWithALocalHookAndNoRunner(t)

	backend, closeFn, err := service.Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })

	if err := openWorkflowEngineOn(context.Background(), backend, nil); err != nil {
		t.Fatalf("openWorkflowEngineOn: %v", err)
	}

	if !backend.WorkflowEngineReady() {
		t.Fatal("the workflow engine is not ready after the startup sequence")
	}
	if gate := backend.WorkflowReconcileGate(); gate != nil {
		t.Fatalf("the run gate is closed after a successful reconciliation: %v", gate)
	}

	sets, err := backend.ListBackupSets(context.Background())
	if err != nil {
		t.Fatalf("ListBackupSets: %v", err)
	}
	if len(sets) != 1 {
		t.Fatalf("the fixture has %d backup sets, want 1", len(sets))
	}

	op, err := backend.SubmitRunBackupSet(context.Background(), service.RunBackupSetRequest{
		IdempotencyKey: "run-once",
		ConfigRevision: backend.ConfigRevision(),
		BackupSetID:    sets[0].ID,
		Actor:          "test",
	})
	if err != nil {
		t.Fatalf("SubmitRunBackupSet: %v", err)
	}

	finished := awaitOperation(t, backend, op.ID)
	if finished.Status != "failed" {
		t.Fatalf("a run of a set whose local hook has no runner came back %q; with the lifecycle installed the run has to be refused, and a process serving with a nil lifecycle is exactly what this asserts against. Result: %s",
			finished.Status, finished.Result)
	}

	// And the pass never happened: one object was waiting on the remote,
	// and a run that took the backup outside its workflow would have
	// landed it. The row's own error field is deliberately generic (the
	// detail goes to the log, runbackupset.go), so this is the fact
	// worth asserting rather than a sentence.
	landed := filepath.Join(filepath.Dir(configPath), "local", "backup.dump")
	if _, err := os.Stat(landed); err == nil {
		t.Errorf("%s exists: the backup was taken even though this deployment cannot run the set's hooks", landed)
	}
}

// TestAFailedStartupSequenceIsReportedRatherThanSwallowed is what the
// first-run activation branch depends on: the error reaches the caller,
// which is what turns it into restart_required instead of a published
// backend with no lifecycle.
//
// The reconciliation is broken the way an unusable journal breaks it --
// the service's own journal is closed underneath it -- because that is
// the failure this branch cannot be allowed to serve through.
func TestAFailedStartupSequenceIsReportedRatherThanSwallowed(t *testing.T) {
	configPath := configWithALocalHookAndNoRunner(t)

	backend, closeFn, err := service.Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := closeFn(); err != nil {
		t.Fatalf("closing the service: %v", err)
	}

	err = openWorkflowEngineOn(context.Background(), backend, nil)
	if err == nil {
		t.Fatal("the startup sequence reported success against a journal that cannot be read; the activation path would then publish a backend with no workflow lifecycle")
	}
	if errors.Is(err, service.ErrWorkflowsNotWired) {
		t.Fatalf("the error says this process has no engine, which is not the failure under test: %v", err)
	}

	if gate := backend.WorkflowReconcileGate(); !errors.Is(gate, service.ErrWorkflowReconcileIncomplete) {
		t.Errorf("WorkflowReconcileGate = %v after a failed reconciliation, want the fail-closed refusal", gate)
	}
}
