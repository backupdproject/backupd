package e2eproduction_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/snapshotlifecycle"
)

// harnessBinary builds core/tests/e2eproduction/harness once per test
// binary and hands every caller the same path.
//
// Built rather than `go run`, for the reason crashmatrix builds its own:
// `go run` puts a parent process between the test and the thing being
// killed, so a SIGKILL of the child is observed by the test as `go run`
// exiting 1, and the exit status the test wants to assert on -- "this
// process died of signal 9" -- is gone.
var (
	harnessOnce sync.Once
	harnessPath string
	harnessErr  error
)

// harnessDir is created once for the whole test binary and removed by
// TestMain, rather than being a t.TempDir: the harness is built once and
// used by several tests, and a binary living in the first caller's temp
// directory disappears the moment that test finishes.
var harnessDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "e2eproduction-harness")
	if err != nil {
		panic("creating the harness directory: " + err.Error())
	}

	harnessDir = dir

	code := m.Run()

	_ = os.RemoveAll(dir)

	os.Exit(code)
}

func harnessBinary(t *testing.T) string {
	t.Helper()

	harnessOnce.Do(func() {
		bin := filepath.Join(harnessDir, "e2e-harness")

		cmd := exec.Command("go", "build", "-o", bin, "./harness")
		cmd.Dir = "."

		if out, err := cmd.CombinedOutput(); err != nil {
			harnessErr = errors.New(string(out))

			return
		}

		harnessPath = bin
	})

	if harnessErr != nil {
		t.Fatalf("building the crash/restore harness: %v", harnessErr)
	}

	return harnessPath
}

// TestAProcessKilledMidSnapshotIsRecoveredAndTheNextRunSucceeds is
// #789's process-crash recovery evidence, with a real process and a real
// SIGKILL.
//
// The story is one power cut during a nightly backup. A separate process
// opens the deployment, starts a snapshot run, gets a few files into the
// tree and stops existing. What it leaves behind is the state
// reconciliation exists for: a catalog row mid-phase, no manifest in the
// repository, and a repository whose state directory was written by a
// process that never closed it.
//
// The assertions are the operator's, in order:
//
//   - the child really died of signal 9, not of an error it handled. A
//     test that accepted a clean exit here would be asserting about the
//     error path and calling it a crash.
//   - the repository still OPENS afterwards. A repository a dead process
//     left locked is a deployment that cannot back up again until
//     somebody intervenes.
//   - reconciliation resolves the dead run rather than leaving it
//     mid-phase for ever, and it does NOT advertise it as a restore
//     point: nothing was committed, so there is nothing to restore.
//   - the next run succeeds, and its snapshot verifies. This is the one
//     that matters most: a deployment that recovers into a state where
//     backups no longer work has not recovered.
func TestAProcessKilledMidSnapshotIsRecoveredAndTheNextRunSucceeds(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t, deploymentOptions{createRepository: true})

	seed(t, d.srcDir, 40, 8<<10, 4)

	// The first snapshot, taken cleanly, so the crash lands on a
	// deployment with history rather than on an empty one. Recovery on
	// top of an existing restore point is the case that can go wrong in
	// both directions: the dead run must not be adopted, and the good
	// row must not be disturbed.
	good, err := d.run(t, "run-before-crash", snapshotlifecycle.VerificationOptions{SamplePercent: 50})
	if err != nil || !good.Succeeded() {
		t.Fatalf("the snapshot before the crash: %v (%s)", err, good.Reason)
	}

	// The journal and the repository have to be closed here: the child
	// is a second process opening the same SQLite database and the same
	// repository state directory, and holding them open would test this
	// suite's locking rather than the product's recovery.
	closeDeployment(t, d)

	const crashRunID = "run-killed"

	cmd := exec.Command(harnessBinary(t),
		"-mode", "crash-mid-snapshot",
		"-root", d.root,
		"-run-id", crashRunID,
		"-kill-after-files", "5",
	)

	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the harness exited with %v rather than being killed; it printed:\n%s", err, out)
	}

	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the harness ended as %v rather than by SIGKILL, so no crash was simulated; it printed:\n%s", exit, out)
	}

	if !strings.Contains(string(out), "E2E_SELF_KILL") {
		t.Fatalf("the harness died without announcing the kill, so it may have died of something other than the seam under test:\n%s", out)
	}

	// --- back up, in a new process's shoes --------------------------------

	reopened := newDeployment(t, deploymentOptions{root: d.root})

	// The dead run's row is really there and really unfinished, which is
	// the precondition every assertion below depends on.
	killed, err := reopened.journal.GetSnapshotRun(ctx, crashRunID)
	if err != nil {
		t.Fatalf("the killed run left no catalog row, so the crash happened before anything durable and this test proves nothing: %v", err)
	}

	if killed.Phase.Terminal() {
		t.Fatalf("the killed run's row is already terminal (%s); a process killed mid-snapshot cannot have finished its own row", killed.Phase)
	}

	report, err := reopened.reconcile(t)
	if err != nil {
		t.Fatalf("reconciling after the crash: %v", err)
	}

	resolved, err := reopened.journal.GetSnapshotRun(ctx, crashRunID)
	if err != nil {
		t.Fatalf("re-reading the killed run: %v", err)
	}

	if !resolved.Phase.Terminal() {
		t.Errorf("reconciliation left the killed run at %s; an unfinished run nothing resolves is one every later pass has to reason about for ever (verdicts: %d)",
			resolved.Phase, len(report.Changed()))
	}

	if resolved.Phase.Advertised() {
		t.Errorf("reconciliation advertised the killed run (%s) as a restore point; it committed no manifest, so there is nothing to restore from", resolved.Phase)
	}

	// The good row is untouched: still the set's restore point.
	lkg, err := reopened.journal.LastKnownGoodSnapshot(ctx, setUUID)
	if err != nil {
		t.Fatalf("the set has no last-known-good restore point after the crash: %v", err)
	}

	if lkg.RunID != good.RunID {
		t.Errorf("the set's restore point moved to %s over a crash; it was %s and nothing newer was ever committed", lkg.RunID, good.RunID)
	}

	// --- and the deployment still works -----------------------------------

	after, err := reopened.run(t, "run-after-crash", snapshotlifecycle.VerificationOptions{SamplePercent: 50})
	if err != nil || !after.Succeeded() {
		t.Fatalf("the snapshot after the crash: %v (%s)", err, after.Reason)
	}

	if !after.LastKnownGood {
		t.Error("the snapshot taken after the crash is not the set's restore point, so recovery left the deployment unable to make one")
	}

	if _, err := reopened.repo.Verify(ctx, backupengine.SnapshotID(after.SnapshotID), backupengine.VerifyRequest{
		Level: model.LevelContentFull,
	}); err != nil {
		t.Fatalf("the snapshot taken after the crash does not verify: %v", err)
	}

	// The crash left no orphan manifest behind that later passes would
	// have to keep re-deciding about. Two snapshots: the one before and
	// the one after.
	snapshots, err := reopened.repo.ListSnapshots(ctx, reopened.snapshotSource())
	if err != nil {
		t.Fatalf("listing the repository's snapshots: %v", err)
	}

	if len(snapshots) != 2 {
		ids := make([]string, 0, len(snapshots))
		for _, s := range snapshots {
			ids = append(ids, string(s.ID))
		}

		t.Errorf("the repository holds %d snapshots (%s); a crash before any commit plus two good runs is two", len(snapshots), strings.Join(ids, ", "))
	}
}

// TestARestoreSucceedsFromACleanEnvironmentWithNothingButTheRepositoryAndItsPassphrase
// is #789's clean-environment restore criterion, made unfakeable by
// running it somewhere the deployment does not exist.
//
// The disaster this is about is total: the machine that took the backups
// is gone, and what survives is the storage and the secret. So the child
// process is given the repository root, the domain, the passphrase file,
// a scratch state directory and a snapshot id, and nothing else -- no
// config.yaml, no journal, no source tree, and an environment stripped to
// what a Go program cannot run without. Everything the restore needs it
// must therefore read out of the repository itself.
//
// The evidence is the restored tree, hashed against the source as it
// stood when the snapshot was taken, in the PARENT: the child's own
// report is asserted too, but a report is the thing under test and cannot
// also be the proof.
func TestARestoreSucceedsFromACleanEnvironmentWithNothingButTheRepositoryAndItsPassphrase(t *testing.T) {
	d := newDeployment(t, deploymentOptions{createRepository: true})

	want := seed(t, d.srcDir, 60, 12<<10, 3)

	res, err := d.run(t, "run-clean-env", snapshotlifecycle.VerificationOptions{SamplePercent: 50})
	if err != nil || !res.Succeeded() {
		t.Fatalf("the snapshot to restore: %v (%s)", err, res.Reason)
	}

	// Everything this process holds open on that repository is released,
	// so the child opens it cold: no warm index cache, no shared handle.
	closeDeployment(t, d)

	// The source tree is taken away as well. A restore that quietly read
	// the source would then fail rather than pass, which is the only way
	// to be sure it did not.
	if err := os.RemoveAll(d.srcDir); err != nil {
		t.Fatalf("removing the source tree: %v", err)
	}

	target := filepath.Join(t.TempDir(), "restored")
	scratch := filepath.Join(t.TempDir(), "state")

	cmd := exec.Command(harnessBinary(t),
		"-mode", "restore",
		"-repository-root", filepath.Join(d.root, "backups"),
		"-domain", domainID,
		"-passphrase-file", filepath.Join(d.root, "repo.passphrase"),
		"-state-dir", scratch,
		"-snapshot", res.SnapshotID,
		"-to", target,
	)

	// A deliberately bare environment: PATH, HOME and TMPDIR are what a
	// process needs to run at all on this platform, and nothing here
	// carries a route to the deployment, a credential, or a pointer at
	// its configuration.
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + filepath.Join(t.TempDir(), "home"),
		"TMPDIR=" + t.TempDir(),
	}

	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("the clean-environment restore failed: %v\n%s", err, exit.Stderr)
		}

		t.Fatalf("the clean-environment restore failed: %v", err)
	}

	var restored backupengine.RestoreReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &restored); err != nil {
		t.Fatalf("reading the child's restore report %q: %v", out, err)
	}

	if !restored.Complete {
		t.Errorf("the child reports an incomplete restore: %+v", restored)
	}

	if restored.Files != int64(len(want)) {
		t.Errorf("the child restored %d files; the snapshot holds %d", restored.Files, len(want))
	}

	// The parent's own check, on the bytes that landed.
	got := hashTree(t, target)

	if len(got) != len(want) {
		t.Errorf("the clean-environment restore produced %d files, want %d", len(got), len(want))
	}

	for rel, sum := range want {
		if got[rel] != sum {
			t.Errorf("%s restored as %q, want %q", rel, got[rel], sum)
		}
	}
}

// closeDeployment releases the journal and the repository this process
// holds, so another process can open them.
//
// It is idempotent against the cleanup newDeployment registered: both
// closes are tolerant of having already happened, and the cleanup's error
// reporting is what would otherwise turn a deliberate early close into a
// spurious failure at the end of the test.
func closeDeployment(t *testing.T, d *deployment) {
	t.Helper()

	if err := d.repo.Close(context.Background()); err != nil {
		t.Fatalf("closing the repository before handing it to another process: %v", err)
	}

	d.closed = true

	if err := d.journal.Close(); err != nil {
		t.Fatalf("closing the journal before handing it to another process: %v", err)
	}
}

// reconcile runs the real crash reconciler over this set, which is what
// app.Service does at the start of every cycle.
func (d *deployment) reconcile(t *testing.T) (snapshotlifecycle.ReconcileReport, error) {
	t.Helper()

	reconciler := &snapshotlifecycle.Reconciler{Catalog: d.journal}

	return reconciler.Reconcile(context.Background(), snapshotlifecycle.ReconcileRequest{
		Set:            d.set.ID,
		SetUUID:        setUUID,
		Domain:         d.set.Repository.Domain,
		Source:         d.snapshotSource(),
		SourceIdentity: d.set.SourceIdentity,
		Repository:     d.repo,
	})
}
