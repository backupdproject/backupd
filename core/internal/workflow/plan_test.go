package workflow

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/model"
)

// Snapshot's suite, which is where #808's load-bearing claims are either
// true or not:
//
//   - the same workflow tree produces the same plan hash twice;
//   - a changed script produces a different one;
//   - replacing a script AFTER the snapshot changes nothing about the run;
//   - the spool is 0700/0600;
//   - every refusal in the validation table stops the whole plan.
//
// The fixtures build real trees and the assertions read the real
// filesystem, for discover_test.go's reason: these are properties of the
// operating system's behaviour, not of this package's opinion of it.

func testSetID(t *testing.T) model.BackupSetID {
	t.Helper()

	id, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	return id
}

// fixture is one workflow tree plus the request that snapshots it.
type fixture struct {
	root     Root
	rootDir  string
	spool    string
	setID    model.BackupSetID
	beforeSh string
	afterSh  string
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	root, rootDir := newRoot(t)
	before := mkStage(t, rootDir, "set-before")
	after := mkStage(t, rootDir, "set-after")

	f := fixture{
		root:    root,
		rootDir: rootDir,
		spool:   filepath.Join(t.TempDir(), "workflow-runs"),
		setID:   testSetID(t),
	}
	f.beforeSh = writeScript(t, before, "10-quiesce.local.sh", "#!/bin/sh\necho quiesce\n")
	f.afterSh = writeScript(t, after, "90-resume.local.sh", "#!/bin/sh\necho resume\n")

	return f
}

func (f fixture) request(runID string) SnapshotRequest {
	return SnapshotRequest{
		RunID:       runID,
		BackupSetID: f.setID,
		Root:        f.root,
		Stages:      PlanStages(StageDirs{}, StageDirs{Before: "set-before", After: "set-after"}),
		SpoolRoot:   f.spool,
	}
}

// The determinism claim. Two runs, two run ids, two spools, one hash.
//
// The negative half is in the same test on purpose: a hash function that
// returned a constant would pass the first assertion and is exactly the
// bug that makes the value worthless.
func TestSnapshotIsDeterministicOverAnUnchangedTree(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	first, err := Snapshot(f.request("run-1"))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	second, err := Snapshot(f.request("run-2"))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if first.ResolvedPlanHash == "" {
		t.Fatal("Snapshot produced no plan hash")
	}
	if first.ResolvedPlanHash != second.ResolvedPlanHash {
		t.Errorf("two runs over an unchanged workflow tree produced different plan hashes:\n\t%s\n\t%s\nThe hash is what answers \"has anything about what we execute changed since last night\"",
			first.ResolvedPlanHash, second.ResolvedPlanHash)
	}
	if first.ScriptSpoolRef == second.ScriptSpoolRef {
		t.Errorf("both runs spooled to %s; a run's captured scripts must be its own", first.ScriptSpoolRef)
	}

	// The negative control: one byte of one script, and the hash moves.
	if err := os.WriteFile(f.beforeSh, []byte("#!/bin/sh\necho quiesce harder\n"), 0o700); err != nil {
		t.Fatalf("rewriting the fixture: %v", err)
	}

	third, err := Snapshot(f.request("run-3"))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if third.ResolvedPlanHash == first.ResolvedPlanHash {
		t.Error("editing a script's content did not change the plan hash, so the hash does not cover the bytes it claims to")
	}
}

// The immutability claim, and the reason the spool exists. After a
// snapshot, the workflow tree is no longer the authority: the script can
// be rewritten, made hostile, or deleted outright, and the run still
// executes what was captured.
func TestSnapshotIsImmuneToAPostSnapshotReplacement(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	const original = "#!/bin/sh\necho quiesce\n"

	plan, err := Snapshot(f.request("run-1"))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	step := planStep(t, plan, "10-quiesce.local.sh")

	// The attack: replace the file with something else entirely, then
	// delete the other one. Both are things an operator, a deploy script
	// or an intruder can do while a backup is running.
	if err := os.WriteFile(f.beforeSh, []byte("#!/bin/sh\nrm -rf /\n"), 0o700); err != nil {
		t.Fatalf("replacing the script: %v", err)
	}
	if err := os.Remove(f.afterSh); err != nil {
		t.Fatalf("removing the script: %v", err)
	}

	spooled, err := os.ReadFile(step.SpoolRef)
	if err != nil {
		t.Fatalf("the spooled copy is not readable after the source was replaced: %v", err)
	}
	if string(spooled) != original {
		t.Errorf("the spooled script now reads %q; replacing the file in the workflow tree must not change what this run executes", spooled)
	}

	// The "after" step's script was DELETED from the tree, and its
	// spooled copy still has to be there: a recovery pass tomorrow runs
	// from the spool, and this is the case where the source is gone
	// entirely.
	afterStep := planStep(t, plan, "90-resume.local.sh")
	if _, err := os.Stat(afterStep.SpoolRef); err != nil {
		t.Errorf("the spooled copy of a script that has since been deleted from the workflow tree is gone too (%v); a recovery of this run would have nothing to execute", err)
	}

	// And re-snapshotting now gives a different plan, which is what
	// proves the first plan was a SNAPSHOT rather than a lazy view.
	after, err := Snapshot(f.request("run-2"))
	if err != nil {
		t.Fatalf("Snapshot after the replacement: %v", err)
	}
	if after.ResolvedPlanHash == plan.ResolvedPlanHash {
		t.Error("a plan taken after the tree changed has the same hash as one taken before, so the hash is not reading the tree at all")
	}
}

func planStep(t *testing.T, plan Plan, scriptName string) Step {
	t.Helper()

	for _, s := range plan.Steps {
		if s.ScriptName == scriptName {
			return s
		}
	}

	t.Fatalf("the plan has no step for %s; it has %d steps", scriptName, len(plan.Steps))

	return Step{}
}

// The spool's modes. 0700 on the directories and 0600 on the files is what
// makes "the plan is the only execution authority" a claim the operating
// system enforces rather than one this package asserts: a spool another
// account could write would be a second place to change what runs.
func TestSnapshotProtectsTheSpool(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	plan, err := Snapshot(f.request("run-1"))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if len(plan.Steps) == 0 {
		t.Fatal("the fixture produced no steps, so this test checked no permissions at all")
	}

	var checkedDirs, checkedFiles int

	err = filepath.WalkDir(plan.ScriptSpoolRef, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		switch {
		case d.IsDir():
			checkedDirs++

			if info.Mode().Perm() != 0o700 {
				t.Errorf("spool directory %s has mode %04o, want 0700", path, info.Mode().Perm())
			}
		default:
			checkedFiles++

			if info.Mode().Perm() != 0o600 {
				t.Errorf("spooled script %s has mode %04o, want 0600", path, info.Mode().Perm())
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking the spool: %v", err)
	}

	// Anti-vacuity: a walk that found nothing would have passed every
	// assertion above.
	if checkedDirs < 2 || checkedFiles != len(plan.Steps) {
		t.Fatalf("the walk saw %d directories and %d files; the spool must have the run directory, its scripts directory and one file per step (%d)",
			checkedDirs, checkedFiles, len(plan.Steps))
	}
}

// The captured hash is the hash of the captured bytes. Without this, the
// spool and the plan could disagree and nothing would notice.
func TestSnapshotHashesWhatItSpooled(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	plan, err := Snapshot(f.request("run-1"))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	for _, step := range plan.Steps {
		body, err := os.ReadFile(step.SpoolRef)
		if err != nil {
			t.Fatalf("reading the spooled %s: %v", step.ScriptName, err)
		}

		if got := sha256Hex(body); got != step.ScriptSHA256 {
			t.Errorf("step %s records hash %s and its spooled bytes hash to %s", step.ID, step.ScriptSHA256, got)
		}
		if int64(len(body)) != step.ScriptSize {
			t.Errorf("step %s records size %d and its spooled file is %d bytes", step.ID, step.ScriptSize, len(body))
		}
	}
}

// Ordering across stages: within a stage the order is bytewise, and
// between stages it is the execution order PlanStages defines. Both halves
// are here because a plan that sorted globally by name would run an
// "after" hook before a "before" hook whose name sorts later.
func TestSnapshotOrdersStagesByExecutionAndScriptsByBytes(t *testing.T) {
	t.Parallel()

	root, rootDir := newRoot(t)

	globalBefore := mkStage(t, rootDir, "global-before")
	setBefore := mkStage(t, rootDir, "set-before")
	setAfter := mkStage(t, rootDir, "set-after")
	globalAfter := mkStage(t, rootDir, "global-after")

	// "zzz" in the earliest stage and "aaa" in the latest: a global sort
	// by name would invert them.
	writeScript(t, globalBefore, "zzz.local.sh", "#!/bin/sh\n")
	writeScript(t, setBefore, "b.remote.sh", "#!/bin/sh\n")
	writeScript(t, setBefore, "a.local.sh", "#!/bin/sh\n")
	writeScript(t, setAfter, "m.local.sh", "#!/bin/sh\n")
	writeScript(t, globalAfter, "aaa.local.sh", "#!/bin/sh\n")

	plan, err := Snapshot(SnapshotRequest{
		RunID:       "run-1",
		BackupSetID: testSetID(t),
		Root:        root,
		Stages: PlanStages(
			StageDirs{Before: "global-before", After: "global-after"},
			StageDirs{Before: "set-before", After: "set-after"},
		),
		RemoteExecConnectionRef: "production/postgres-primary",
		SpoolRoot:               filepath.Join(t.TempDir(), "workflow-runs"),
	})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	type row struct {
		scope Scope
		phase Phase
		name  string
	}

	want := []row{
		{ScopeGlobal, PhaseBefore, "zzz.local.sh"},
		{ScopeSet, PhaseBefore, "a.local.sh"},
		{ScopeSet, PhaseBefore, "b.remote.sh"},
		{ScopeSet, PhaseAfter, "m.local.sh"},
		{ScopeGlobal, PhaseAfter, "aaa.local.sh"},
	}

	if len(plan.Steps) != len(want) {
		t.Fatalf("the plan has %d steps, want %d", len(plan.Steps), len(want))
	}

	for i, w := range want {
		got := plan.Steps[i]
		if got.Scope != w.scope || got.Phase != w.phase || got.ScriptName != w.name {
			t.Errorf("step %d is %s/%s/%s, want %s/%s/%s", i, got.Scope, got.Phase, got.ScriptName, w.scope, w.phase, w.name)
		}
		if got.Order != i {
			t.Errorf("step %d records order %d", i, got.Order)
		}
	}

	// The remote step carries the connection and the local ones do not.
	for _, step := range plan.Steps {
		switch step.Target {
		case TargetRemote:
			if step.ExecutionConnectionRef == "" {
				t.Errorf("remote step %s has no execution connection", step.ID)
			}
		case TargetLocal:
			if step.ExecutionConnectionRef != "" {
				t.Errorf("local step %s carries execution connection %q", step.ID, step.ExecutionConnectionRef)
			}
		}
	}
}

// The validation-refusal table, driven end to end through Snapshot,
// because that is the entry point a run actually uses and every row here
// has to stop the WHOLE plan rather than skip one script.
//
// Every row also asserts the spool was cleaned up. A refused snapshot that
// left a half-built spool behind would leave a directory a later recovery
// pass could find and misread as a plan.
func TestSnapshotRefusesTheWholePlanAndCleansUp(t *testing.T) {
	t.Parallel()

	cases := []struct {
		what    string
		build   func(t *testing.T, rootDir, stage string)
		wantIs  error
		mustSay string
	}{
		{
			what: "a group-writable script",
			build: func(t *testing.T, _, stage string) {
				p := writeScript(t, stage, "a.local.sh", "#!/bin/sh\n")
				if err := os.Chmod(p, 0o770); err != nil {
					t.Fatalf("chmod: %v", err)
				}
			},
			wantIs:  ErrCustody,
			mustSay: "rewrite it",
		},
		{
			what: "a world-writable script",
			build: func(t *testing.T, _, stage string) {
				p := writeScript(t, stage, "a.local.sh", "#!/bin/sh\n")
				if err := os.Chmod(p, 0o707); err != nil {
					t.Fatalf("chmod: %v", err)
				}
			},
			wantIs:  ErrCustody,
			mustSay: "chmod go-w",
		},
		{
			what: "an oversized script",
			build: func(t *testing.T, _, stage string) {
				writeScript(t, stage, "a.local.sh", strings.Repeat("x", int(DefaultMaxScriptSize)+1))
			},
			wantIs:  ErrScriptTooLarge,
			mustSay: "refuses the whole file rather than executing a prefix",
		},
		{
			what: "a script with whitespace in its name",
			build: func(t *testing.T, _, stage string) {
				writeScript(t, stage, "a b.local.sh", "#!/bin/sh\n")
			},
			wantIs:  ErrScriptName,
			mustSay: "whitespace",
		},
		{
			what: "a plain .sh with no target",
			build: func(t *testing.T, _, stage string) {
				writeScript(t, stage, "backup.sh", "#!/bin/sh\n")
			},
			wantIs:  ErrScriptName,
			mustSay: "does not say where it runs",
		},
		{
			what: "a hidden file",
			build: func(t *testing.T, _, stage string) {
				writeScript(t, stage, ".a.local.sh", "#!/bin/sh\n")
			},
			wantIs:  ErrScriptName,
			mustSay: "begins with a dot",
		},
		{
			what: "a fifo where a script belongs",
			build: func(t *testing.T, _, stage string) {
				mkfifo(t, filepath.Join(stage, "a.local.sh"))
			},
			wantIs:  ErrCustody,
			mustSay: "not a regular file",
		},
		{
			what: "a remote script with no execution connection",
			build: func(t *testing.T, _, stage string) {
				writeScript(t, stage, "a.remote.sh", "#!/bin/sh\n")
			},
			wantIs:  ErrPlan,
			mustSay: "no execution connection is configured",
		},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()

			root, rootDir := newRoot(t)
			stage := mkStage(t, rootDir, "before")
			tc.build(t, rootDir, stage)

			spoolRoot := filepath.Join(t.TempDir(), "workflow-runs")

			plan, err := Snapshot(SnapshotRequest{
				RunID:       "run-1",
				BackupSetID: testSetID(t),
				Root:        root,
				Stages:      PlanStages(StageDirs{}, StageDirs{Before: "before"}),
				SpoolRoot:   spoolRoot,
			})
			if err == nil {
				t.Fatalf("Snapshot produced a plan with %d steps; this tree must be refused", len(plan.Steps))
			}
			if !errors.Is(err, tc.wantIs) {
				t.Errorf("Snapshot returned %v, want it to wrap %v", err, tc.wantIs)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("Snapshot said:\n\t%v\nwant it to contain %q", err, tc.mustSay)
			}
			if _, statErr := os.Stat(filepath.Join(spoolRoot, "run-1")); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("a refused snapshot left its spool behind at %s (stat: %v); a later recovery pass could find it and misread it as a plan",
					filepath.Join(spoolRoot, "run-1"), statErr)
			}
		})
	}
}

// The size bound is configurable and the configuration is bounded. Both
// halves are the point: a bound nobody can raise refuses a legitimate
// hook, and a bound with no ceiling is a protection a deployment can
// configure away while debugging something else.
func TestSnapshotBoundsTheConfigurableSizeLimit(t *testing.T) {
	t.Parallel()

	build := func(t *testing.T, size int) SnapshotRequest {
		t.Helper()

		root, rootDir := newRoot(t)
		stage := mkStage(t, rootDir, "before")
		writeScript(t, stage, "a.local.sh", strings.Repeat("x", size))

		return SnapshotRequest{
			RunID:       "run-1",
			BackupSetID: testSetID(t),
			Root:        root,
			Stages:      PlanStages(StageDirs{}, StageDirs{Before: "before"}),
			SpoolRoot:   filepath.Join(t.TempDir(), "workflow-runs"),
		}
	}

	t.Run("a raised bound accepts a script the default refuses", func(t *testing.T) {
		t.Parallel()

		req := build(t, int(DefaultMaxScriptSize)+1)
		req.MaxScriptSize = DefaultMaxScriptSize * 2

		if _, err := Snapshot(req); err != nil {
			t.Errorf("a script within the configured bound was refused: %v", err)
		}
	})

	t.Run("a bound above the ceiling is refused", func(t *testing.T) {
		t.Parallel()

		req := build(t, 16)
		req.MaxScriptSize = MaxConfigurableScriptSize + 1

		_, err := Snapshot(req)
		if err == nil {
			t.Fatal("a maximum script size above the ceiling was accepted, so the ceiling does nothing")
		}
		if !errors.Is(err, ErrPlan) {
			t.Errorf("Snapshot returned %v, want an ErrPlan", err)
		}
		if !strings.Contains(err.Error(), "cannot configure the protection away") {
			t.Errorf("the refusal must say why there is a ceiling, got:\n\t%v", err)
		}
	})
}

// An unset stage is disabled and an existing empty one is zero steps. The
// two produce the same number of steps and are emphatically not the same
// configuration, so both are asserted here and the missing-directory case
// (the third of #808's three) is the refusal beside them.
func TestSnapshotDistinguishesDisabledEmptyAndMissingStages(t *testing.T) {
	t.Parallel()

	root, rootDir := newRoot(t)
	mkStage(t, rootDir, "empty-before")

	base := SnapshotRequest{
		RunID:       "run-1",
		BackupSetID: testSetID(t),
		Root:        root,
		SpoolRoot:   filepath.Join(t.TempDir(), "workflow-runs"),
	}

	t.Run("no stage configured at all", func(t *testing.T) {
		t.Parallel()

		req := base
		req.SpoolRoot = filepath.Join(t.TempDir(), "workflow-runs")
		req.Stages = PlanStages(StageDirs{}, StageDirs{})

		plan, err := Snapshot(req)
		if err != nil {
			t.Fatalf("a set with no hook directories configured must snapshot cleanly: %v", err)
		}
		if len(plan.Steps) != 0 {
			t.Errorf("plan has %d steps", len(plan.Steps))
		}
	})

	t.Run("a configured directory that is empty", func(t *testing.T) {
		t.Parallel()

		req := base
		req.SpoolRoot = filepath.Join(t.TempDir(), "workflow-runs")
		req.Stages = PlanStages(StageDirs{}, StageDirs{Before: "empty-before"})

		plan, err := Snapshot(req)
		if err != nil {
			t.Fatalf("a configured but empty hook directory must be zero steps, not a refusal: %v", err)
		}
		if len(plan.Steps) != 0 {
			t.Errorf("plan has %d steps", len(plan.Steps))
		}
	})

	t.Run("a configured directory that is missing", func(t *testing.T) {
		t.Parallel()

		req := base
		req.SpoolRoot = filepath.Join(t.TempDir(), "workflow-runs")
		req.Stages = PlanStages(StageDirs{}, StageDirs{Before: "not-there"})

		_, err := Snapshot(req)
		if err == nil {
			t.Fatal("a configured hook directory that does not exist was accepted as zero steps; an operator who mistyped a directory name would get a backup that silently runs no hooks")
		}
		if !errors.Is(err, ErrStageDir) {
			t.Errorf("Snapshot returned %v, want an ErrStageDir", err)
		}
	})
}

// PlanStages is the nesting rule, asserted directly because it is a
// product decision rather than a property of the data, and because the
// reverse-unwind order is the one thing a hook author's mental model
// depends on.
func TestPlanStagesUnwindsAfterHooksInReverse(t *testing.T) {
	t.Parallel()

	got := PlanStages(StageDirs{Before: "gb", After: "ga"}, StageDirs{Before: "sb", After: "sa"})

	want := []StageSpec{
		{Scope: ScopeGlobal, Phase: PhaseBefore, Dir: "gb"},
		{Scope: ScopeSet, Phase: PhaseBefore, Dir: "sb"},
		{Scope: ScopeSet, Phase: PhaseAfter, Dir: "sa"},
		{Scope: ScopeGlobal, Phase: PhaseAfter, Dir: "ga"},
	}

	if len(got) != len(want) {
		t.Fatalf("PlanStages returned %d stages, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stage %d is %+v, want %+v", i, got[i], want[i])
		}
	}

	// Disabled stages are omitted rather than returned empty, so a caller
	// cannot mistake "not configured" for "configured with the empty
	// path".
	if only := PlanStages(StageDirs{}, StageDirs{After: "sa"}); len(only) != 1 || only[0].Phase != PhaseAfter {
		t.Errorf("PlanStages with one configured stage returned %+v", only)
	}
}

func mkfifo(t *testing.T, path string) {
	t.Helper()

	if err := syscallMkfifo(path); err != nil {
		t.Skipf("this platform will not create a fifo at %s: %v", path, err)
	}
}

func sha256Hex(b []byte) string {
	return fmt.Sprintf("%x", sha256Sum(b))
}
