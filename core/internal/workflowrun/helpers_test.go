package workflowrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/state"
	"github.com/backupdproject/backupd/core/internal/workflow"
	"github.com/backupdproject/backupd/core/internal/workflowexec"
)

// The fixtures every suite in this package shares: a real journal, a real
// workflow tree snapshotted into a real spool, and executors that record
// what they were asked to run instead of running it.
//
// Nothing here fakes the JOURNAL or the PLAN. Those two are what the
// engine's guarantees are made of -- the durable transitions and the
// verified captured bytes -- so a suite that substituted either would be
// testing an engine that does not exist. What is substituted is the
// execution of a hook, because whether bash runs is internal/hostrunner's
// and internal/remoteexec's question and they answer it against real
// processes and a real sshd.

// custodyTempDir is internal/workflow's own fixture helper, for the same
// reason it has one: the walk from a hook directory (or a spool) up to /
// refuses any ancestor another account can write, and t.TempDir lands
// under a mode-1777 /tmp on Linux. A fixture built there would be refused
// for a reason about the harness rather than about the fixture.
func custodyTempDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp(".", ".wfrun-")
	if err != nil {
		t.Fatalf("creating a fixture directory: %v", err)
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("resolving the fixture directory %s: %v", dir, err)
	}

	t.Cleanup(func() { os.RemoveAll(abs) }) //nolint:errcheck // a leftover fixture is inert

	return abs
}

func openJournal(t *testing.T) (*state.Journal, string) {
	t.Helper()

	dir := custodyTempDir(t)
	path := filepath.Join(dir, "state.db")

	j, err := state.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("opening the journal: %v", err)
	}
	t.Cleanup(func() { j.Close() }) //nolint:errcheck // the fixture directory goes too

	return j, path
}

func testSetID(t *testing.T) model.BackupSetID {
	t.Helper()

	id, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	return id
}

// stage names one of the four hook directories, for a fixture that
// declares what is in each.
type stage struct {
	scope workflow.Scope
	phase workflow.Phase
}

var (
	globalBefore = stage{workflow.ScopeGlobal, workflow.PhaseBefore}
	globalAfter  = stage{workflow.ScopeGlobal, workflow.PhaseAfter}
	setBefore    = stage{workflow.ScopeSet, workflow.PhaseBefore}
	setAfter     = stage{workflow.ScopeSet, workflow.PhaseAfter}
)

// tree is a workflow root on disk plus the plan snapshotted out of it.
type tree struct {
	root      string
	spoolRoot string
	dirs      map[stage]string
	setID     model.BackupSetID
}

// newTree lays out the four hook directories and the scripts in them.
//
// A stage present in scripts with an empty slice is a directory that
// EXISTS AND IS EMPTY, which is a different fact from a stage that is
// absent (not configured at all) and is exactly the case #811's
// nested-eligibility rule turns on. A stage absent from the map gets no
// directory and is not configured.
func newTree(t *testing.T, scripts map[stage][]string) *tree {
	t.Helper()

	base := custodyTempDir(t)
	root := filepath.Join(base, "workflows")

	tr := &tree{
		root:      root,
		spoolRoot: filepath.Join(base, "workflow-runs"),
		dirs:      map[stage]string{},
		setID:     testSetID(t),
	}

	names := map[stage]string{
		globalBefore: "global-before",
		globalAfter:  "global-after",
		setBefore:    "set-before",
		setAfter:     "set-after",
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("creating the workflow root: %v", err)
	}

	for st, files := range scripts {
		dir := filepath.Join(root, names[st])
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
		tr.dirs[st] = dir

		for _, name := range files {
			body := fmt.Sprintf("#!/usr/bin/env bash\necho %s\n", name)
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatalf("writing %s: %v", name, err)
			}
		}
	}

	return tr
}

// snapshot builds the plan for one run out of the tree as it is NOW.
func (tr *tree) snapshot(t *testing.T, runID string) workflow.Plan {
	t.Helper()

	return tr.snapshotWithEnv(t, runID, nil)
}

// snapshotWithEnv is snapshot with a configured environment, for the
// suites that need a real secret reference in play.
func (tr *tree) snapshotWithEnv(t *testing.T, runID string, vars []workflow.EnvVar) workflow.Plan {
	t.Helper()

	env, err := workflow.NewEnvironment(vars)
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}

	root, err := workflow.NewRoot(tr.root)
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}

	plan, err := workflow.Snapshot(workflow.SnapshotRequest{
		RunID:       runID,
		BackupSetID: tr.setID,
		Root:        root,
		Stages: workflow.PlanStages(
			workflow.StageDirs{Before: tr.dirs[globalBefore], After: tr.dirs[globalAfter]},
			workflow.StageDirs{Before: tr.dirs[setBefore], After: tr.dirs[setAfter]},
		),
		Env:                     env,
		Timeout:                 5 * time.Second,
		RemoteExecConnectionRef: "nas",
		SpoolRoot:               tr.spoolRoot,
	})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	return plan
}

// recorder is the shared dispatch log both fake executors append to, so a
// suite can assert the ORDER across the two of them -- which is the whole
// point of the ordering matrix, since a mixed plan's correctness is not
// visible from either executor alone.
type recorder struct {
	mu     sync.Mutex
	calls  []string
	bodies []string

	// env is keyed by script name rather than parallel to calls,
	// because calls also carries "backup" -- which has no environment
	// and would offset every index after it.
	env map[string]map[string]string
}

func (r *recorder) record(req StepRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls = append(r.calls, string(req.Step.Target)+":"+req.Step.ScriptName)
	r.bodies = append(r.bodies, string(req.Script.Body))

	env := map[string]string{}
	for _, kv := range req.Environ {
		name, value, _ := cut(kv)
		env[name] = value
	}

	if r.env == nil {
		r.env = map[string]map[string]string{}
	}
	r.env[req.Step.ScriptName] = env
}

func cut(kv string) (name, value string, found bool) {
	for i := range len(kv) {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}

	return kv, "", false
}

func (r *recorder) dispatched() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.calls...)
}

func (r *recorder) envOf(t *testing.T, scriptName string) map[string]string {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()

	env, ok := r.env[scriptName]
	if !ok {
		t.Fatalf("%s was never dispatched; these were: %v", scriptName, r.calls)
	}

	return env
}

// fakeExecutor answers for one target. outcomes maps a script's basename
// to what running it produced; anything not named exits 0.
type fakeExecutor struct {
	target   workflow.Target
	rec      *recorder
	outcomes map[string]func(context.Context, StepRequest) (StepOutcome, error)

	// output is written to the step's sink before the outcome is
	// returned, so a suite can assert what was captured.
	output map[string]string
}

func (f *fakeExecutor) ExecuteStep(ctx context.Context, req StepRequest) (StepOutcome, error) {
	if req.Step.Target != f.target {
		return StepOutcome{}, fmt.Errorf("the %s executor was handed %s, which is a %s step",
			f.target, req.Step.ScriptName, req.Step.Target)
	}

	f.rec.record(req)

	if text, ok := f.output[req.Step.ScriptName]; ok && req.Sink != nil {
		if err := req.Sink.Chunk(workflowexec.Chunk{
			Stream: workflowexec.StreamStdout,
			Seq:    1,
			Data:   []byte(text),
		}); err != nil {
			return StepOutcome{}, err
		}
	}

	if fn, ok := f.outcomes[req.Step.ScriptName]; ok {
		return fn(ctx, req)
	}

	return exited(0), nil
}

func newExecutors(rec *recorder) (*fakeExecutor, *fakeExecutor) {
	return &fakeExecutor{
			target:   workflow.TargetLocal,
			rec:      rec,
			outcomes: map[string]func(context.Context, StepRequest) (StepOutcome, error){},
			output:   map[string]string{},
		},
		&fakeExecutor{
			target:   workflow.TargetRemote,
			rec:      rec,
			outcomes: map[string]func(context.Context, StepRequest) (StepOutcome, error){},
			output:   map[string]string{},
		}
}

func exited(code int) StepOutcome {
	return StepOutcome{Disposition: DispositionExited, ExitCode: &code}
}

func failing(code int) func(context.Context, StepRequest) (StepOutcome, error) {
	return func(context.Context, StepRequest) (StepOutcome, error) { return exited(code), nil }
}

// clock is a test clock that moves one second per reading, so every
// durable timestamp is distinct and ordered without any test sleeping.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 9, 13, 2, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(time.Second)

	return c.now
}

// harness is an engine wired to a real journal and the two fakes.
type harness struct {
	engine *Engine
	store  *state.Journal
	dbPath string
	rec    *recorder
	local  *fakeExecutor
	remote *fakeExecutor
	clock  *clock
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	rec := &recorder{}
	local, remote := newExecutors(rec)
	j, dbPath := openJournal(t)
	clk := newClock()

	h := &harness{
		store:  j,
		dbPath: dbPath,
		rec:    rec,
		local:  local,
		remote: remote,
		clock:  clk,
	}

	h.engine = &Engine{
		Store:  j,
		Local:  local,
		Remote: remote,
		Now:    clk.Now,
	}

	// The engine refuses to run until a reconciliation has established
	// what is outstanding, which is the guard that keeps a restart from
	// backing up a set whose last run left a machine quiesced. Every
	// harness therefore starts the way a daemon does.
	if _, err := h.engine.Reconcile(context.Background(), clk.Now()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	return h
}

// run is Engine.Run with the fixture's backup: one that succeeds and
// records that it was called.
func (h *harness) run(t *testing.T, plan workflow.Plan) (RunResult, error) {
	t.Helper()

	return h.engine.Run(context.Background(), RunRequest{
		Plan: plan,
		Backup: func(context.Context) error {
			h.rec.mu.Lock()
			defer h.rec.mu.Unlock()
			h.rec.calls = append(h.rec.calls, "backup")

			return nil
		},
		BackupSetID: h.rec.setID(plan),
	})
}

// setID is on the recorder only so the call above reads as one line; the
// plan is the authority on which set a run belongs to.
func (r *recorder) setID(plan workflow.Plan) model.BackupSetID { return plan.BackupSetID() }

func stateOf(t *testing.T, j *state.Journal, runID, stepName string) state.WorkflowStep {
	t.Helper()

	steps, err := j.WorkflowSteps(context.Background(), runID)
	if err != nil {
		t.Fatalf("WorkflowSteps: %v", err)
	}
	for _, s := range steps {
		if s.ScriptName == stepName {
			return s
		}
	}

	t.Fatalf("run %q has no step for %s", runID, stepName)

	return state.WorkflowStep{}
}

func obligationOf(t *testing.T, j *state.Journal, runID string, scope workflow.Scope) workflow.CleanupObligation {
	t.Helper()

	obligations, err := j.WorkflowCleanupObligations(context.Background(), runID)
	if err != nil {
		t.Fatalf("WorkflowCleanupObligations: %v", err)
	}
	for _, o := range obligations {
		if o.Scope == scope {
			return o
		}
	}

	t.Fatalf("run %q has no %s obligation", runID, scope)

	return workflow.CleanupObligation{}
}
