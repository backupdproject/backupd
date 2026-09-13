package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/secretref"
)

// The spool's own custody, which is the half of #808 that actually runs.
//
// Everything discover.go refuses is about the workflow TREE, and the tree
// stops being the authority the moment Snapshot returns. What executes is
// the spool -- so a spool another account can relocate, replace or write
// into is the same vulnerability with the protections pointing the wrong
// way. These are the cases MkdirAll made possible: it follows symbolic
// links in every component, and it succeeds on a directory that is already
// there.

// spoolFixture is a workflow tree plus a spool root laid out under a
// directory this test controls the whole ancestry of.
func spoolFixture(t *testing.T) (fixture, string) {
	t.Helper()

	f := newFixture(t)
	stateDir := custodyTempDir(t)
	f.spool = filepath.Join(stateDir, "workflow-runs")

	return f, stateDir
}

func TestSnapshotRefusesASpoolRootReachedThroughASymlink(t *testing.T) {
	t.Parallel()

	f, stateDir := spoolFixture(t)

	// Where the link points is somewhere this process can write, so the
	// snapshot would SUCCEED if the link were followed -- which is the
	// whole problem: the 0700 it then applies protects whoever created
	// the link, and the scripts the run executes live in their directory.
	elsewhere := custodyTempDir(t)
	if err := os.Symlink(elsewhere, f.spool); err != nil {
		t.Skipf("this filesystem will not create symlinks: %v", err)
	}

	_, err := Snapshot(f.request("run-1"))
	if err == nil {
		t.Fatal("a spool root that is a symbolic link was accepted; the spool is the only authority over what a run executes, so a spool somebody else chose the location of is a run somebody else chose the contents of")
	}
	if !errors.Is(err, ErrCustody) {
		t.Errorf("Snapshot returned %v, want an ErrCustody", err)
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("the refusal must say what was found, got:\n\t%v", err)
	}

	if _, statErr := os.Stat(filepath.Join(elsewhere, "run-1")); statErr == nil {
		t.Errorf("Snapshot created %s: it followed the link before refusing it", filepath.Join(elsewhere, "run-1"))
	}
	_ = stateDir
}

func TestSnapshotRefusesAGroupWritableSpoolAncestor(t *testing.T) {
	t.Parallel()

	f, stateDir := spoolFixture(t)

	// The state directory: the spool's parent, and a directory whose mode
	// this product does not set. A group-writable one means any account in
	// that group can replace the run directory between the snapshot and
	// the execution of its last step.
	if err := os.Chmod(stateDir, 0o775); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := Snapshot(f.request("run-1"))
	if err == nil {
		t.Fatal("a spool under a group-writable directory was accepted; an account in that group can swap the run directory after the plan is committed and substitute the bytes every step executes")
	}
	if !errors.Is(err, ErrCustody) {
		t.Errorf("Snapshot returned %v, want an ErrCustody", err)
	}
	if !strings.Contains(err.Error(), stateDir) {
		t.Errorf("the refusal must name the directory to fix (%s), got:\n\t%v", stateDir, err)
	}

	// The positive control: the same tree with the same spool root, once
	// the mode is what it should be.
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := Snapshot(f.request("run-1")); err != nil {
		t.Fatalf("the same snapshot was refused with an owner-only state directory: %v", err)
	}
}

// A run id this journal has seen before must not write into the earlier
// run's spool, and must not delete it either.
//
// Both halves matter and they pull in opposite directions, which is why
// the original code got it wrong in a way that looked tidy: it created the
// directory with MkdirAll (so a reused id was accepted) and cleaned up
// with RemoveAll on any refusal (so a refusal DELETED the earlier run's
// captured scripts). A recovery pass reading that spool is the reason
// those scripts are kept at all.
func TestSnapshotRefusesAReusedRunIDAndKeepsTheEarlierSpool(t *testing.T) {
	t.Parallel()

	f, _ := spoolFixture(t)

	first, err := Snapshot(f.request("run-1"))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	steps := first.Steps()
	if len(steps) == 0 {
		t.Fatal("the fixture produced no steps")
	}

	// A second plan for the same id, over a tree that has since changed,
	// so that "it silently reused the first plan" cannot pass as success.
	if err := os.WriteFile(f.beforeSh, []byte("#!/bin/sh\necho different\n"), 0o700); err != nil {
		t.Fatalf("rewriting the fixture: %v", err)
	}

	_, err = Snapshot(f.request("run-1"))
	if err == nil {
		t.Fatal("a second plan for one run id was accepted; the spool is created once and a run that executed a mixture of two plans is a run nothing can account for")
	}
	if !errors.Is(err, ErrPlan) {
		t.Errorf("Snapshot returned %v, want an ErrPlan", err)
	}
	if !strings.Contains(err.Error(), "already has a spool") {
		t.Errorf("the refusal must say why, got:\n\t%v", err)
	}

	// The earlier run's scripts are still there, byte for byte.
	script, err := first.OpenScript(steps[0].ID)
	if err != nil {
		t.Fatalf("the refused second snapshot damaged the first run's spool: %v", err)
	}
	if !strings.Contains(string(script.Body), "echo quiesce") {
		t.Errorf("the first run's spooled script now reads %q", script.Body)
	}
}

// The spool's directory entries are flushed before Snapshot returns,
// because a plan is journaled immediately afterwards and a crash between
// the two must not leave a run whose scripts have no names.
//
// This asserts the CALLS, which is as far as a unit test can go: a real
// fsync cannot be distinguished from a no-op without pulling the power,
// which is what internal/lifecycle's TestFsyncFileAndFsyncDir says about
// its own copy of the primitive. The crash matrix is where durability is
// actually exercised.
//
// Deliberately NOT parallel: it swaps a package-level seam. Go's testing
// package runs the non-parallel tests to completion before resuming the
// parallel ones, so no other test in this suite can be reading the seam
// while this one holds it.
func TestSnapshotFlushesTheSpoolBeforeReturning(t *testing.T) {
	f, _ := spoolFixture(t)

	var synced []string

	original := syncDir
	syncDir = func(d *os.File) error {
		synced = append(synced, d.Name())

		return original(d)
	}
	t.Cleanup(func() { syncDir = original })

	plan, err := Snapshot(f.request("run-1"))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	want := []string{
		filepath.Join(plan.ScriptSpoolRef(), scriptsDirName),
		plan.ScriptSpoolRef(),
		f.spool,
	}

	for _, dir := range want {
		found := false
		for _, got := range synced {
			if got == dir {
				found = true

				break
			}
		}
		if !found {
			t.Errorf("Snapshot returned without flushing %s.\nIt flushed %v.\nA crash after the plan is journaled and before the directory entry reaches the disk is a run whose scripts exist and cannot be found by name, which is the one state a recovery pass cannot tell apart from tampering", dir, synced)
		}
	}
}

// The capability, which is what replaces "the plan carries a path and the
// execution layer opens it".
//
// The refusals below are the ones #810 and #811 would otherwise have to
// remember to make, each in their own code, correctly, forever.
func TestOpenScriptVerifiesWhatItOpens(t *testing.T) {
	t.Parallel()

	f, _ := spoolFixture(t)

	plan, err := Snapshot(f.request("run-1"))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	steps := plan.Steps()
	if len(steps) == 0 {
		t.Fatal("the fixture produced no steps")
	}

	// The positive control first: the capability works, so every refusal
	// below is about the refusal rather than about OpenScript being
	// broken.
	script, err := plan.OpenScript(steps[0].ID)
	if err != nil {
		t.Fatalf("OpenScript on a fresh plan: %v", err)
	}
	if !strings.Contains(string(script.Body), "echo quiesce") {
		t.Errorf("OpenScript returned %q", script.Body)
	}
	if script.Path != filepath.Join(plan.ScriptSpoolRef(), scriptsDirName, steps[0].ID) {
		t.Errorf("OpenScript opened %s, which is not inside this run's own spool", script.Path)
	}

	t.Run("a step this plan does not have", func(t *testing.T) {
		if _, err := plan.OpenScript("0000~set~before~not-in-this-plan.local.sh"); err == nil {
			t.Fatal("OpenScript opened a step the plan never declared")
		} else if !errors.Is(err, ErrSpool) {
			t.Errorf("OpenScript returned %v, want an ErrSpool", err)
		}
	})

	t.Run("bytes that no longer hash to what the plan recorded", func(t *testing.T) {
		swapped, err := Snapshot(f.request("run-2"))
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}

		id := swapped.Steps()[0].ID
		path := filepath.Join(swapped.ScriptSpoolRef(), scriptsDirName, id)

		// The same LENGTH, different bytes: a substitution that a size
		// check cannot see, which is why the hash is what decides.
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the spooled script: %v", err)
		}

		hostile := []byte("#!/bin/sh\nrm -rf /\n")
		for len(hostile) < len(original) {
			hostile = append(hostile, '#')
		}
		hostile = hostile[:len(original)]

		if err := os.WriteFile(path, hostile, 0o600); err != nil {
			t.Fatalf("rewriting the spooled script: %v", err)
		}

		_, err = swapped.OpenScript(id)
		if err == nil {
			t.Fatal("OpenScript returned bytes that do not hash to what the plan recorded; the hash is the only thing that distinguishes the script that passed validation from whatever is in the spool now")
		}
		if !errors.Is(err, ErrSpool) {
			t.Errorf("OpenScript returned %v, want an ErrSpool", err)
		}
		if !strings.Contains(err.Error(), "hashes to") {
			t.Errorf("the refusal must say the hashes differ, got:\n\t%v", err)
		}
	})

	t.Run("a spool whose ancestry stopped being ours", func(t *testing.T) {
		loosened, err := Snapshot(f.request("run-3"))
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}

		if err := os.Chmod(loosened.ScriptSpoolRef(), 0o777); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() {
			os.Chmod(loosened.ScriptSpoolRef(), 0o700) //nolint:errcheck // best effort, for the cleanup walk
		})

		if _, err := loosened.OpenScript(loosened.Steps()[0].ID); err == nil {
			t.Fatal("OpenScript read out of a run directory anybody can write to; the custody check has to hold at execution time and not only at snapshot time")
		} else if !errors.Is(err, ErrCustody) {
			t.Errorf("OpenScript returned %v, want an ErrCustody", err)
		}
	})
}

// Recovery: the same capability, over a plan rebuilt from what the journal
// stored rather than from what Snapshot just built.
//
// This is the case the whole opaque-plan design exists for. After a
// restart there is no Snapshot in the picture: there are rows, and a
// directory on disk, and something has to decide whether the two agree
// before a script runs as root.
func TestRecoverPlanChecksWhatItWasHandedAndOpensTheSameBytes(t *testing.T) {
	t.Parallel()

	f, _ := spoolFixture(t)

	plan, err := Snapshot(f.request("run-1"))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	recoverable := func() RecoveredPlan {
		return RecoveredPlan{
			RunID:            plan.RunID(),
			BackupSetID:      plan.BackupSetID(),
			Steps:            plan.Steps(),
			Env:              plan.Env(),
			ResolvedPlanHash: plan.ResolvedPlanHash(),
			ScriptSpoolRef:   plan.ScriptSpoolRef(),
		}
	}

	recovered, err := RecoverPlan(recoverable())
	if err != nil {
		t.Fatalf("RecoverPlan on what Snapshot produced: %v", err)
	}

	fresh, err := plan.OpenScript(plan.Steps()[0].ID)
	if err != nil {
		t.Fatalf("OpenScript: %v", err)
	}

	again, err := recovered.OpenScript(plan.Steps()[0].ID)
	if err != nil {
		t.Fatalf("OpenScript on a recovered plan: %v", err)
	}
	if string(again.Body) != string(fresh.Body) {
		t.Errorf("a recovered plan opened different bytes:\n\t%q\n\t%q", again.Body, fresh.Body)
	}

	cases := []struct {
		what    string
		mutate  func(*RecoveredPlan)
		mustSay string
	}{
		{
			what: "a spool ref that escapes the run's own spool",
			mutate: func(r *RecoveredPlan) {
				r.Steps[0].SpoolRef = filepath.Join(r.ScriptSpoolRef, scriptsDirName, "..", "..", "..", "etc", "cron.d", "x")
			},
			mustSay: "spool holds it at",
		},
		{
			what: "a spool ref somewhere else entirely",
			mutate: func(r *RecoveredPlan) {
				r.Steps[0].SpoolRef = "/tmp/anywhere/" + r.Steps[0].ID
			},
			mustSay: "spool holds it at",
		},
		{
			what: "a step whose target no longer matches its name",
			mutate: func(r *RecoveredPlan) {
				r.Steps[0].Target = TargetRemote
				r.Steps[0].ExecutionConnectionRef = "primary"
			},
			mustSay: "says it runs",
		},
		{
			what:    "no plan hash",
			mutate:  func(r *RecoveredPlan) { r.ResolvedPlanHash = "" },
			mustSay: "no resolved plan hash",
		},
		{
			what:    "a relative spool",
			mutate:  func(r *RecoveredPlan) { r.ScriptSpoolRef = "workflow-runs/run-1" },
			mustSay: "not an absolute path",
		},
		{
			what: "steps recovered out of plan order",
			mutate: func(r *RecoveredPlan) {
				r.Steps[0], r.Steps[1] = r.Steps[1], r.Steps[0]
			},
			mustSay: "out of order",
		},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()

			rec := recoverable()
			tc.mutate(&rec)

			got, err := RecoverPlan(rec)
			if err == nil {
				t.Fatalf("RecoverPlan accepted %s (%d steps)", tc.what, len(got.Steps()))
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("RecoverPlan said:\n\t%v\nwant it to contain %q", err, tc.mustSay)
			}
		})
	}
}

// The plan hash has to be INJECTIVE over the things it covers, and version
// 1 of the encoding was not: it joined fields with tabs and records with
// newlines, and an environment variable's value is arbitrary bytes.
//
// Each pair below renders to one byte string under the old encoding and to
// two under the new one. They are not exotic inputs -- a value with a
// newline in it is a PEM key or a multi-line SQL snippet, which is
// precisely what an operator puts in a hook's environment.
func TestPlanHashDistinguishesValuesThatContainItsDelimiters(t *testing.T) {
	t.Parallel()

	f, _ := spoolFixture(t)

	hashOf := func(t *testing.T, runID string, vars []EnvVar) string {
		t.Helper()

		env, err := NewEnvironment(vars)
		if err != nil {
			t.Fatalf("NewEnvironment: %v", err)
		}

		req := f.request(runID)
		req.Env = env

		plan, err := Snapshot(req)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}

		return plan.ResolvedPlanHash()
	}

	cases := []struct {
		what string
		a    []EnvVar
		b    []EnvVar
	}{
		{
			what: "a literal value carrying the record and field separators",
			a:    []EnvVar{{Name: "A", Value: "x\nenv\tB\tliteral\ty"}},
			b:    []EnvVar{{Name: "A", Value: "x"}, {Name: "B", Value: "y"}},
		},
		{
			what: "a secret argv whose argument carries the argv separator",
			a:    []EnvVar{{Name: "A", Secret: secretref.Ref{Command: []string{"vault\x1fread"}}}},
			b:    []EnvVar{{Name: "A", Secret: secretref.Ref{Command: []string{"vault", "read"}}}},
		},
		{
			what: "a secret file path carrying the separators",
			a:    []EnvVar{{Name: "A", Secret: secretref.Ref{File: "/k\nenv\tB\tliteral\ty"}}},
			b:    []EnvVar{{Name: "A", Secret: secretref.Ref{File: "/k"}}, {Name: "B", Value: "y"}},
		},
	}

	for i, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			first := hashOf(t, "collide-a-"+itoa(i), tc.a)
			second := hashOf(t, "collide-b-"+itoa(i), tc.b)

			if first == second {
				t.Errorf("two different environments hash alike (%s).\n\t%v\n\t%v\nThe hash answers \"has anything about what we execute changed\", and a collision is that question answered \"no\" about a change that happened",
					first, tc.a, tc.b)
			}
		})
	}

	// The control: the hash is still DETERMINISTIC over one environment,
	// which is the property the rows above must not have broken.
	one := hashOf(t, "same-1", []EnvVar{{Name: "A", Value: "x\ny"}})
	two := hashOf(t, "same-2", []EnvVar{{Name: "A", Value: "x\ny"}})

	if one != two {
		t.Errorf("the same environment hashed two ways:\n\t%s\n\t%s", one, two)
	}
}

// itoa keeps the run ids in the table above distinct without pulling
// strconv into this file for one call.
func itoa(i int) string { return string(rune('a' + i)) }

// A plan's Env is immutable after the snapshot, INCLUDING the one slice
// hiding inside it: a command-sourced secret's argv.
//
// The shallow copy this replaces was a live handle on what the run
// resolves. The hash is taken at snapshot time, the argv is read at
// execution time, and a caller who reused its slice -- which is exactly
// what a config-reload path does -- would move the second without moving
// the first.
func TestSnapshotDeepCopiesASecretArgv(t *testing.T) {
	t.Parallel()

	f, _ := spoolFixture(t)

	argv := []string{"vault", "read", "-field=password", "secret/db"}

	env, err := NewEnvironment([]EnvVar{{Name: "DB_PASSWORD", Secret: secretref.Ref{Command: argv}}})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}

	req := f.request("run-1")
	req.Env = env

	plan, err := Snapshot(req)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// The mutation a reused slice makes possible.
	argv[3] = "secret/root"

	planned := plan.Env().Vars()
	if len(planned) != 1 {
		t.Fatalf("the plan carries %d variables, want 1", len(planned))
	}
	if got := planned[0].Secret.Command[3]; got != "secret/db" {
		t.Errorf("after the caller mutated its own argv, the plan resolves %q instead of %q: the plan's hash still describes the old secret and the run would read the new one", got, "secret/db")
	}

	// And the other direction: what Vars hands out cannot be used to
	// reach back into the plan.
	planned[0].Secret.Command[3] = "secret/escalated"

	if got := plan.Env().Vars()[0].Secret.Command[3]; got != "secret/db" {
		t.Errorf("mutating the slice Vars returned changed the plan to %q", got)
	}
}

// The aggregate bound. One script is bounded, one stage's script COUNT is
// bounded, and the product of the two is not: this is what makes the plan
// as a whole bounded work.
func TestSnapshotBoundsTheAggregatePlanSize(t *testing.T) {
	t.Parallel()

	root, rootDir := newRoot(t)
	stage := mkStage(t, rootDir, "before")

	// Five two-mebibyte scripts: every one of them legal under a raised
	// per-file bound, and together past MaxPlanBytes.
	body := strings.Repeat("#", 2<<20)
	for i := range 5 {
		writeScript(t, stage, scriptNumbered(i), body)
	}

	_, err := Snapshot(SnapshotRequest{
		RunID:         "run-1",
		BackupSetID:   testSetID(t),
		Root:          root,
		Stages:        PlanStages(StageDirs{}, StageDirs{Before: "before"}),
		SpoolRoot:     filepath.Join(custodyTempDir(t), "workflow-runs"),
		MaxScriptSize: 4 << 20,
	})
	if err == nil {
		t.Fatal("a plan of ten mebibytes of scripts was accepted; the per-file and per-stage bounds do not bound the product, and the backup window pays for the capture")
	}
	if !errors.Is(err, ErrScriptTooLarge) {
		t.Errorf("Snapshot returned %v, want an ErrScriptTooLarge", err)
	}
}
