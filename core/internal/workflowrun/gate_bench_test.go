// The scale rows #812 names, measured, plus the two assertions that make
// the claims those rows are about falsifiable.
//
// They are benchmarks in the repository for the reason
// internal/sourceconsistency's are: a published cost nobody can re-run is
// a claim rather than a measurement. The numbers in the gate's write-up
// were taken with
//
//	cd core && go test ./internal/workflowrun/ \
//	    -run '^$' -bench . -benchtime 1x -count 1
//
// and the ratios are stable enough to compare across changes when read
// from
//
//	cd core && go test ./internal/workflowrun/ \
//	    -run '^$' -bench . -benchtime 1x -count 5
//
// -benchtime 1x for every row in this file, which is unusual and is
// deliberate. Each iteration stages its own workflow tree (up to a
// hundred scripts read, hashed and copied into a fresh spool), commits
// its own plan and writes its own journal rows into the same SQLite
// file, so iteration N is measured against a bigger database than
// iteration one: a larger benchtime would mostly price the journal
// growing rather than the engine's work on one run. -count 5 rather than
// a longer benchtime is how a stable figure is obtained, because each
// count starts from an empty journal.
//
// What the numbers mean. ns/op is the WHOLE run: plan snapshot, the
// durable transitions, the five stages with their fake hooks, the log
// pipeline and the terminal advance. The fakes are the substitution the
// rest of this package makes -- a hook's bash is internal/hostrunner's
// and internal/remoteexec's question -- so these rows price THIS layer,
// which is what a regression here would show up in. planning_ms breaks
// out the part of that which is internal/workflow's snapshot.
// persisted_kib and dropped are counts from the run the last iteration
// produced, so they say what the row actually did rather than what it
// intended.
//
// The two no-hook baseline rows live in lock_test.go
// (BenchmarkZeroHookRun and BenchmarkBackupWithoutTheEngine): the engine
// wrapping a backup that has no workflow, against the same backup called
// directly. Everything here is to be read against that pair.
//
// The RUNNER's resident size is deliberately absent. Every local hook
// runs in a separate process -- an ephemeral container since #865 -- and
// a remote hook runs on another host entirely, so an RSS figure read out
// of THIS process would be a number about the wrong program. What is
// reported instead is heap_mb, the engine's own live heap, which is the
// footprint this process can honestly account for; the runner's own
// footprint is observable where the real runner runs, in
// core/tests/containerhooks.

package workflowrun

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/state"
	"github.com/backupdproject/backupd/core/internal/workflow"
	"github.com/backupdproject/backupd/core/internal/workflowexec"
)

// benchHooksPerStage is how many scripts each of the four stage
// directories holds in the hundred-hook rows.
//
// A hundred hooks cannot sit in ONE directory: workflow.MaxScriptsPerStage
// bounds a stage at 64 entries and refuses the whole directory past that,
// so a hundred-hook deployment is necessarily spread across the stages,
// and that is the shape priced here. Twenty-five per stage is a
// hundred-step run.
const (
	benchHooksPerStage = 25
	benchHooks         = 4 * benchHooksPerStage
)

// benchStages is the four stage directories in the order a run walks
// them, so a fixture that fills all four reads as one loop.
var benchStages = []stage{globalBefore, setBefore, setAfter, globalAfter}

// benchHarness is newHarness for a benchmark.
//
// The shared harness helpers take *testing.T because every other suite in
// this package is a test, and a benchmark hands them a zero value exactly
// as BenchmarkZeroHookRun does. That zero T can neither report a failure
// nor run a cleanup, which has two consequences this helper deals with:
// every error a benchmark can check, it checks itself against b, and the
// journal directory the zero T would have removed is removed here
// instead. Without the second part each row would leave a .wfrun-*
// directory behind in the package directory.
func benchHarness(b *testing.B) (*harness, *testing.T) {
	b.Helper()

	t := &testing.T{}
	h := newHarness(t)

	b.Cleanup(func() {
		_ = h.store.Close()
		_ = os.RemoveAll(filepath.Dir(h.dbPath))
	})

	return h, t
}

// benchTree is newTree with the same cleanup the zero T cannot do.
func benchTree(b *testing.B, t *testing.T, scripts map[stage][]string) *tree {
	b.Helper()

	tr := newTree(t, scripts)
	b.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(tr.root)) })

	return tr
}

// benchAllStages fills all four stage directories with benchHooksPerStage
// scripts of one target and returns the fixture plus the names, which are
// the same in every stage (a step's identity carries its scope and phase,
// so a repeated basename is a different step).
func benchAllStages(target workflow.Target) (map[stage][]string, []string) {
	names := make([]string, 0, benchHooksPerStage)
	for i := range benchHooksPerStage {
		names = append(names, fmt.Sprintf("%02d-hook.%s.sh", i, target))
	}

	scripts := make(map[stage][]string, len(benchStages))
	for _, st := range benchStages {
		scripts[st] = names
	}

	return scripts, names
}

// heapMB is this process's live heap in mebibytes. See this file's
// preamble for why the runner's resident size is not reported beside it.
func heapMB() float64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return float64(m.HeapAlloc) / (1 << 20)
}

// benchLogBytes is the total payload persisted for one run, which is what
// makes persisted_kib a count rather than an intention.
func benchLogBytes(b *testing.B, h *harness, runID string) int {
	b.Helper()

	recs, err := h.store.WorkflowStepLogsAfter(context.Background(), runID, 0, 100_000)
	if err != nil {
		b.Fatalf("WorkflowStepLogsAfter: %v", err)
	}

	total := 0
	for _, rec := range recs {
		total += len(rec.Payload)
	}

	return total
}

// BenchmarkAPlanWithNoWorkflowConfiguredAtAll is the disabled path: a
// deployment whose workflow tree exists on disk and whose runs pass no
// plan, which is what "workflows disabled" is at this layer.
//
// The tree is staged and never snapshotted on purpose. The row exists so
// that "disabling costs nothing" is in the record for a deployment that
// HAS hooks, rather than only for one that has none (BenchmarkZeroHookRun,
// which this should match).
func BenchmarkAPlanWithNoWorkflowConfiguredAtAll(b *testing.B) {
	h, t := benchHarness(b)
	benchTree(b, t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})

	req := RunRequest{
		BackupSetID: testSetID(t),
		Backup:      func(context.Context) error { return nil },
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, err := h.engine.Run(ctx, req); err != nil {
			b.Fatalf("Run: %v", err)
		}
	}

	b.StopTimer()
	b.ReportMetric(heapMB(), "heap_mb")
}

// BenchmarkAPlanWhoseStageDirectoriesAreEmpty is the shape a deployment
// that has turned workflows on but written no hooks yet has: four
// directories that exist and are empty.
//
// It is a different cost from the row above and not obviously so, which
// is why it is measured: this plan is NOT zero, so it commits a plan,
// raises and settles both obligations and advances a run through the five
// states -- all of it for no steps at all.
func BenchmarkAPlanWhoseStageDirectoriesAreEmpty(b *testing.B) {
	h, t := benchHarness(b)

	empty := make(map[stage][]string, len(benchStages))
	for _, st := range benchStages {
		empty[st] = nil
	}
	tr := benchTree(b, t, empty)

	b.ReportAllocs()
	b.ResetTimer()

	for i := range b.N {
		plan := tr.snapshot(t, fmt.Sprintf("run-%d", i))
		if _, err := h.run(t, plan); err != nil {
			b.Fatalf("Run: %v", err)
		}
	}

	b.StopTimer()
	b.ReportMetric(heapMB(), "heap_mb")
}

// BenchmarkAHundredTinyLocalHooks is a hundred local hooks in one run,
// each of which exits 0 immediately.
//
// planning_ms is broken out because the two halves scale differently and
// an operator reading one number could not tell them apart: the snapshot
// is a hundred file reads, hashes and spool copies inside the backup
// window, while the run is a hundred dispatches and their durable
// transitions. A regression in either is a different thing to go and fix.
func BenchmarkAHundredTinyLocalHooks(b *testing.B) {
	h, t := benchHarness(b)

	scripts, _ := benchAllStages(workflow.TargetLocal)
	tr := benchTree(b, t, scripts)

	var planning time.Duration

	b.ReportAllocs()
	b.ResetTimer()

	for i := range b.N {
		start := time.Now()
		plan := tr.snapshot(t, fmt.Sprintf("run-%d", i))
		planning += time.Since(start)

		res, err := h.run(t, plan)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		if res.ScriptCount != benchHooks {
			b.Fatalf("the plan declared %d scripts, want %d", res.ScriptCount, benchHooks)
		}
	}

	b.StopTimer()
	b.ReportMetric(float64(planning)/float64(b.N)/float64(time.Millisecond), "planning_ms")
	b.ReportMetric(heapMB(), "heap_mb")
}

// BenchmarkAHundredRemoteHooks is the same hundred hooks with every one
// of them remote, so the two rows can be read against each other.
//
// The plan's difference is the connection reference every remote step
// carries and the executor it is dispatched to; the snapshot still reads,
// hashes and spools a hundred files, because a remote hook's bytes are
// captured on this server before they are ever sent anywhere.
func BenchmarkAHundredRemoteHooks(b *testing.B) {
	h, t := benchHarness(b)

	scripts, _ := benchAllStages(workflow.TargetRemote)
	tr := benchTree(b, t, scripts)

	var planning time.Duration

	b.ReportAllocs()
	b.ResetTimer()

	for i := range b.N {
		start := time.Now()
		plan := tr.snapshot(t, fmt.Sprintf("run-%d", i))
		planning += time.Since(start)

		res, err := h.run(t, plan)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		if res.ScriptCount != benchHooks {
			b.Fatalf("the plan declared %d scripts, want %d", res.ScriptCount, benchHooks)
		}
	}

	b.StopTimer()
	b.ReportMetric(float64(planning)/float64(b.N)/float64(time.Millisecond), "planning_ms")
	b.ReportMetric(heapMB(), "heap_mb")
}

// benchFlood is the size of the noisy-hook row: 128 chunks of 4 KiB is
// 512 KiB offered, which is twice DefaultStepOutputBytes.
//
// Deliberately past the bound. The interesting half of this row is what
// happens once the recording stops: the hook keeps writing and every
// chunk still has to be drained and redacted, because a hook blocked on a
// full pipe is a backup that never finishes. A size under the bound would
// price only the cheap half.
const (
	benchFloodChunks     = 128
	benchFloodChunkBytes = 4 << 10
)

// BenchmarkAHighOutputScript prices the log pipeline -- redact, persist,
// publish -- rather than the engine's bookkeeping, by giving a run one
// hook and half a mebibyte of its output.
func BenchmarkAHighOutputScript(b *testing.B) {
	h, t := benchHarness(b)
	tr := benchTree(b, t, map[stage][]string{globalBefore: {"10-chatty.local.sh"}})

	chunk := bytes.Repeat([]byte("x"), benchFloodChunkBytes)

	var writeErrs int
	h.local.outcomes["10-chatty.local.sh"] = func(_ context.Context, req StepRequest) (StepOutcome, error) {
		for i := range benchFloodChunks {
			if err := req.Sink.Chunk(workflowexec.Chunk{
				Stream: workflowexec.StreamStdout,
				Seq:    uint64(i + 1),
				Data:   chunk,
			}); err != nil {
				writeErrs++
			}
		}

		return exited(0), nil
	}

	lastRun := ""

	b.ReportAllocs()
	b.ResetTimer()

	for i := range b.N {
		lastRun = fmt.Sprintf("run-%d", i)
		if _, err := h.run(t, tr.snapshot(t, lastRun)); err != nil {
			b.Fatalf("Run: %v", err)
		}
	}

	b.StopTimer()

	// A write error here would mean the hook was refused mid-output,
	// which would make the row price a stopped pipeline rather than a
	// working one.
	if writeErrs != 0 {
		b.Fatalf("%d chunks came back as write errors; a hook past the bound must keep draining", writeErrs)
	}

	persisted := float64(benchLogBytes(b, h, lastRun)) / 1024
	b.ReportMetric(persisted, "persisted_kib")

	// The derived figure: what one persisted kibibyte cost, which is the
	// number to compare when the redaction filter or the journal's insert
	// path changes. Offered bytes are 512 KiB regardless; persisted bytes
	// stop at the bound, and this prices the work per byte that was kept.
	if persisted > 0 {
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/persisted, "ns/KiB")
	}
	b.ReportMetric(heapMB(), "heap_mb")
}

// BenchmarkLogsAfterOverAHundredStepHistory prices the catch-up read a
// follower that fell behind performs, over a run with a hundred steps'
// worth of output in the journal.
//
// The history is staged before the timer starts because it is the
// fixture, not the subject: what is measured is one LogsAfter over it,
// which is what a reconnecting CLI tail or a browser tab does.
func BenchmarkLogsAfterOverAHundredStepHistory(b *testing.B) {
	h, t := benchHarness(b)

	scripts, names := benchAllStages(workflow.TargetLocal)
	tr := benchTree(b, t, scripts)

	for _, name := range names {
		h.local.outcomes[name] = emitting("a line of output from " + name + "\n")
	}

	const runID = "run-history"
	if _, err := h.run(t, tr.snapshot(t, runID)); err != nil {
		b.Fatalf("Run: %v", err)
	}

	records, err := h.engine.LogsAfter(context.Background(), runID, 0, 100_000)
	if err != nil {
		b.Fatalf("LogsAfter: %v", err)
	}
	if len(records) != benchHooks {
		b.Fatalf("the staged history has %d records, want %d", len(records), benchHooks)
	}

	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		recs, err := h.engine.LogsAfter(ctx, runID, 0, 100_000)
		if err != nil {
			b.Fatalf("LogsAfter: %v", err)
		}
		if len(recs) != benchHooks {
			b.Fatalf("the replay returned %d records", len(recs))
		}
	}

	b.StopTimer()
	b.ReportMetric(float64(len(records)), "records")
	b.ReportMetric(heapMB(), "heap_mb")
}

// benchFollowerChunks is how many chunks the stalled-follower row emits.
// The same 400 broker_test.go's slow-follower test uses, so the two
// numbers are about the same amount of output.
const benchFollowerChunks = 400

// BenchmarkASlowFollowerWhileAScriptEmits prices the fan-out with a
// follower that has a queue of two and never reads it: a browser tab
// somebody closed.
//
// hook_ms is the metric to watch. It is the hook's OWN wall clock,
// measured inside the fake, and it is the acceptance criterion in a
// number: a follower that stopped reading must never appear in the
// hook's cost. dropped is how many records the follower missed, reported
// so the row is visibly the stalled case rather than a fast follower
// that happened to keep up.
func BenchmarkASlowFollowerWhileAScriptEmits(b *testing.B) {
	h, t := benchHarness(b)
	h.engine.Logs = &Broker{}

	tr := benchTree(b, t, map[stage][]string{globalBefore: {"10-chatty.local.sh"}})

	var hookTook, hookTotal time.Duration
	h.local.outcomes["10-chatty.local.sh"] = chattyHook(benchFollowerChunks, &hookTook)

	var lastSub *Subscription
	lagged := 0

	b.ReportAllocs()
	b.ResetTimer()

	for i := range b.N {
		runID := fmt.Sprintf("run-%d", i)

		if lastSub != nil {
			lastSub.Close()
		}
		sub := h.engine.Logs.Subscribe(runID, 2)
		lastSub = sub

		if _, err := h.run(t, tr.snapshot(t, runID)); err != nil {
			b.Fatalf("Run: %v", err)
		}

		hookTotal += hookTook
		if sub.Lagged() {
			lagged++
		}
	}

	b.StopTimer()

	// Drained after the timer, because the queue's contents are the
	// follower's business and not the producer's cost.
	delivered := 0
	if lastSub != nil {
		for {
			select {
			case _, ok := <-lastSub.Records():
				if ok {
					delivered++

					continue
				}
			default:
			}

			break
		}
		lastSub.Close()
	}

	if lagged != b.N {
		b.Fatalf("%d of %d iterations reported a lagging follower; a queue of two that is never read must always lag, and a row where it does not is measuring something else", lagged, b.N)
	}

	b.ReportMetric(float64(benchFollowerChunks-delivered), "dropped")
	b.ReportMetric(float64(hookTotal)/float64(b.N)/float64(time.Millisecond), "hook_ms")
	b.ReportMetric(heapMB(), "heap_mb")
}

// countingStore counts every call the engine makes to the journal and
// passes each one through to it.
//
// Modelled on helpers_test.go's failingStore and asking the opposite
// question. That decorator asks what the engine does when a durable write
// does not land; this one asks whether a write HAPPENS AT ALL.
type countingStore struct {
	Store

	mu    sync.Mutex
	calls map[string]int
}

func counted(store Store) *countingStore {
	return &countingStore{Store: store, calls: map[string]int{}}
}

func (c *countingStore) count(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls[method]++
}

func (c *countingStore) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	n := 0
	for _, calls := range c.calls {
		n += calls
	}

	return n
}

// seen names what was called and how often, so a failure says which
// journal call appeared rather than only that one did.
func (c *countingStore) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]string, 0, len(c.calls))
	for method, calls := range c.calls {
		out = append(out, fmt.Sprintf("%s x%d", method, calls))
	}
	sort.Strings(out)

	return out
}

func (c *countingStore) CommitWorkflowPlan(ctx context.Context, plan state.WorkflowPlan) error {
	c.count("CommitWorkflowPlan")

	return c.Store.CommitWorkflowPlan(ctx, plan)
}

func (c *countingStore) StartWorkflowStep(ctx context.Context, runID, stepID string, at time.Time) error {
	c.count("StartWorkflowStep")

	return c.Store.StartWorkflowStep(ctx, runID, stepID, at)
}

func (c *countingStore) FinishWorkflowStep(ctx context.Context, runID, stepID string, out state.WorkflowStepOutcome) error {
	c.count("FinishWorkflowStep")

	return c.Store.FinishWorkflowStep(ctx, runID, stepID, out)
}

func (c *countingStore) AdvanceWorkflowRun(ctx context.Context, runID string, adv state.WorkflowRunAdvance) error {
	c.count("AdvanceWorkflowRun")

	return c.Store.AdvanceWorkflowRun(ctx, runID, adv)
}

func (c *countingStore) AdvanceWorkflowCleanupObligation(ctx context.Context, adv state.WorkflowObligationAdvance) error {
	c.count("AdvanceWorkflowCleanupObligation")

	return c.Store.AdvanceWorkflowCleanupObligation(ctx, adv)
}

func (c *countingStore) ApplyWorkflowReconciliation(ctx context.Context, rec state.WorkflowReconciliation) error {
	c.count("ApplyWorkflowReconciliation")

	return c.Store.ApplyWorkflowReconciliation(ctx, rec)
}

func (c *countingStore) ResolveWorkflowRecovery(ctx context.Context, runID string, to workflow.State, at time.Time) error {
	c.count("ResolveWorkflowRecovery")

	return c.Store.ResolveWorkflowRecovery(ctx, runID, to, at)
}

func (c *countingStore) WorkflowRun(ctx context.Context, runID string) (state.WorkflowRun, error) {
	c.count("WorkflowRun")

	return c.Store.WorkflowRun(ctx, runID)
}

func (c *countingStore) WorkflowSteps(ctx context.Context, runID string) ([]state.WorkflowStep, error) {
	c.count("WorkflowSteps")

	return c.Store.WorkflowSteps(ctx, runID)
}

func (c *countingStore) WorkflowCleanupObligations(ctx context.Context, runID string) ([]workflow.CleanupObligation, error) {
	c.count("WorkflowCleanupObligations")

	return c.Store.WorkflowCleanupObligations(ctx, runID)
}

func (c *countingStore) ObligationsRequiringRecovery(ctx context.Context) ([]workflow.CleanupObligation, error) {
	c.count("ObligationsRequiringRecovery")

	return c.Store.ObligationsRequiringRecovery(ctx)
}

func (c *countingStore) WorkflowRunsInStates(ctx context.Context, states ...workflow.State) ([]state.WorkflowRun, error) {
	c.count("WorkflowRunsInStates")

	return c.Store.WorkflowRunsInStates(ctx, states...)
}

func (c *countingStore) WorkflowRunsRequiringRecovery(ctx context.Context) ([]state.WorkflowRun, error) {
	c.count("WorkflowRunsRequiringRecovery")

	return c.Store.WorkflowRunsRequiringRecovery(ctx)
}

func (c *countingStore) RecoverWorkflowPlan(ctx context.Context, runID string) (workflow.Plan, error) {
	c.count("RecoverWorkflowPlan")

	return c.Store.RecoverWorkflowPlan(ctx, runID)
}

func (c *countingStore) WorkflowRunFacts(ctx context.Context, runID string) (map[string]string, error) {
	c.count("WorkflowRunFacts")

	return c.Store.WorkflowRunFacts(ctx, runID)
}

func (c *countingStore) AppendWorkflowStepLog(ctx context.Context, rec workflow.StepLog) error {
	c.count("AppendWorkflowStepLog")

	return c.Store.AppendWorkflowStepLog(ctx, rec)
}

func (c *countingStore) WorkflowStepLogsAfter(ctx context.Context, runID string, afterSeq uint64, limit int) ([]workflow.StepLog, error) {
	c.count("WorkflowStepLogsAfter")

	return c.Store.WorkflowStepLogsAfter(ctx, runID, afterSeq, limit)
}

func (c *countingStore) WorkflowStepLogLastSeq(ctx context.Context, runID string) (uint64, error) {
	c.count("WorkflowStepLogLastSeq")

	return c.Store.WorkflowStepLogLastSeq(ctx, runID)
}

// The executable form of "no meaningful overhead on the no-hooks path".
//
// A timing bound cannot state it: a nanosecond figure that holds on a
// developer's laptop says nothing about a loaded NAS, and a benchmark
// that regressed by a third would still be a benchmark that passed. The
// exact property underneath the timing claim is that the path does no
// journal work, and that is a count. The bug it catches is the ordinary
// one -- a "record that a set ran without hooks" row, a "has this set got
// an outstanding interruption" query moved out of the reconciled
// in-memory answer and into a SELECT, a log sequence primed before the
// plan is examined -- any of which turns every backup of every set in a
// deployment with no hooks into journal traffic, and none of which any
// existing test in this package would notice.
func TestTheNoHookPathDoesNoJournalWorkAtAll(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	// The decorator goes on AFTER the harness has reconciled, which is
	// what a daemon does once at startup and which legitimately reads the
	// journal. What is counted is one backup, not the startup.
	counting := counted(h.store)
	h.engine.Store = counting

	backupRan := false
	res, err := h.engine.Run(context.Background(), RunRequest{
		BackupSetID: testSetID(t),
		Backup: func(context.Context) error {
			backupRan = true

			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Asserted first and separately, because a Run that refused to do
	// anything at all would also make zero journal calls and would pass
	// the assertion below for entirely the wrong reason.
	if !backupRan {
		t.Fatal("the backup did not run, so the call below counts the journal work of nothing happening")
	}
	if res.BackupErr != nil {
		t.Fatalf("the backup reported %v", res.BackupErr)
	}

	if counting.total() != 0 {
		t.Errorf("a backup of a set with no workflow made %d journal calls (%s); this path must touch the journal zero times",
			counting.total(), strings.Join(counting.seen(), ", "))
	}
}

// The bound is per step, and logs_test.go asserts that -- one marker, the
// bytes kept under the bound, both streams sharing it. The claim this
// test adds is the RUN-level one, which does not follow from the per-step
// property on its own: a whole run's persisted output is bounded by its
// step count times the bound, so an incident cannot grow the journal
// without limit however many hooks are shouting.
//
// It is written to fail in both directions, which is the point of the
// lower bound below. A regression that let the recording run past the
// bound fails the aggregate assertion; a regression that made the bound
// per RUN rather than per step -- a shared counter, a truncated flag on
// the recorder instead of on the step -- would pass the aggregate while
// silencing every hook after the first, and that is what the per-step
// floor catches.
func TestALogFloodIsBoundedAcrossAWholeRun(t *testing.T) {
	t.Parallel()

	// A small bound rather than the 256 KiB default, so that the run is a
	// few dozen journal rows. The arithmetic being asserted is the same
	// at any bound.
	const (
		bound      = 512
		chunkBytes = 256
		chunks     = 16
	)

	h := newHarness(t)
	h.engine.StepOutputBytes = bound

	names := []string{"10-noisy.local.sh", "20-noisier.local.sh"}
	scripts := make(map[stage][]string, len(benchStages))
	for _, st := range benchStages {
		scripts[st] = names
	}
	tr := newTree(t, scripts)

	steps := len(benchStages) * len(names)
	chunk := bytes.Repeat([]byte("x"), chunkBytes)

	var writeErrs int
	flood := func(_ context.Context, req StepRequest) (StepOutcome, error) {
		for i := range chunks {
			if err := req.Sink.Chunk(workflowexec.Chunk{
				Stream: workflowexec.StreamStdout,
				Seq:    uint64(i + 1),
				Data:   chunk,
			}); err != nil {
				writeErrs++
			}
		}

		return exited(0), nil
	}
	for _, name := range names {
		h.local.outcomes[name] = flood
	}

	res, err := h.run(t, tr.snapshot(t, "run-1"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ScriptCount != steps {
		t.Fatalf("the plan declared %d scripts, want %d", res.ScriptCount, steps)
	}
	if writeErrs != 0 {
		t.Errorf("%d chunks came back as write errors; a hook past the bound must keep draining, not fail", writeErrs)
	}

	recs := logsOf(t, h, "run-1")

	output := map[string]int{}
	markers := map[string]int{}
	markerBytes := 0
	for _, rec := range recs {
		if rec.Kind == workflow.LogTruncated {
			markers[rec.StepID]++
			markerBytes += len(rec.Payload)

			continue
		}
		output[rec.StepID] += len(rec.Payload)
	}

	// The aggregate: every step offered 4 KiB and the whole run persisted
	// no more than its share of the bound.
	total := 0
	for _, kept := range output {
		total += kept
	}
	if total > steps*bound {
		t.Errorf("a run of %d flooding steps persisted %d bytes of output; the whole-run bound is %d",
			steps, total, steps*bound)
	}

	// One marker per step, and nothing else in the journal: the markers
	// are the only records beyond the output, so the run's entire
	// footprint is bounded by the two figures asserted here.
	if len(markers) != steps {
		t.Errorf("%d of %d steps recorded a truncation marker; every one of them flooded", len(markers), steps)
	}
	for stepID, n := range markers {
		if n != 1 {
			t.Errorf("step %s recorded %d truncation markers, want exactly 1", stepID, n)
		}
	}
	if markerBytes > steps*1024 {
		t.Errorf("the truncation markers of %d steps came to %d bytes", steps, markerBytes)
	}

	// The floor, which is what distinguishes a bound from a silence.
	// Each step must have persisted output of its own, so a bound that
	// leaked across steps is a failure here rather than an improvement
	// in the figure above.
	if len(output) != steps {
		t.Fatalf("%d of %d steps persisted any output at all; a per-run bound would silence the later hooks",
			len(output), steps)
	}
	for stepID, kept := range output {
		if kept < chunkBytes {
			t.Errorf("step %s persisted %d bytes before it was truncated; the bound is %d and it is per step",
				stepID, kept, bound)
		}
	}
}
