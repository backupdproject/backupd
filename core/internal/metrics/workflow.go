package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// EPIC L's seven metric families (#813), and the two rules that decide
// every line of this file.
//
// # Rule one: a label value comes from a closed vocabulary or from a
// backup set id, and from nowhere else
//
// Everything else in a workflow is operator-chosen text. A script is
// named by whoever wrote it, a hook directory is a path, an execution
// connection is an id somebody typed, and a step id is minted per run. A
// Prometheus label built from any of those is unbounded cardinality in a
// process that never forgets a series it has emitted: a deployment that
// generates one hook per database ends up with one time series per
// database, forever, and a deployment whose run ids reach a label is one
// whose exporter grows without limit until it is restarted.
//
// So the label sets below are drawn from enumerations this product
// defines and cannot exceed -- a scope (2), a phase (2), a target (2), a
// status (5), a step disposition (6) -- plus the backup set id, which the
// configuration bounds and which every other backupd_backup_set_ family
// already carries. The step id, the script name, the connection ref and
// every path are deliberately absent. labels_test.go holds this, against
// a snapshot deliberately loaded with secrets and paths in every field it
// has one.
//
// # Rule two: an observation is taken where the fact is, not derived
// afterwards
//
// Two of these families cannot be recomputed from the run's result by
// anything above internal/workflowrun. A log truncation happens inside a
// step's byte bound and leaves a marker in the journal, so counting it
// from outside means scanning every run's log on every scrape. A
// remote-exec failure is a DISPOSITION on a remote step, and #810's rule
// that transport loss is never a known exit code is exactly what makes it
// invisible to a reader looking at step STATES: a connection that never
// opened and a hook that exited 1 are both "failed".
//
// So internal/workflowrun states the facts through its Observer seam, and
// this type is the thing that counts them. It is a live counter set with
// a process lifetime, which is what a _total is: it only ever goes up,
// and it resets when the process does, which is the semantics a scraper
// already handles.
//
// # Why the durations are histograms and not gauges
//
// Because the question is "are hooks getting slower", and a gauge holding
// the last run's duration answers it only if you scrape at exactly the
// right moment. A histogram of a handful of buckets costs a bounded
// number of series and survives a scrape interval longer than a backup
// window, which on a nightly deployment is every scrape interval there
// is.

// workflowDurationBuckets is the ladder both duration families use, in
// seconds.
//
// Nine buckets, chosen against what a hook actually is rather than
// against a default ladder: a quiesce or an unmount is sub-second, a
// database checkpoint is seconds, a dump is minutes, and the top of the
// range is workflow.DefaultStepTimeout (five minutes) with one bucket
// above it so a deployment that raised its own bound still has somewhere
// to land. A ladder is shared by both families deliberately -- a run's
// duration and its steps' are compared by operators, and two ladders make
// that arithmetic wrong.
var workflowDurationBuckets = []float64{0.1, 0.5, 1, 5, 15, 60, 300, 900}

// Workflow counts what workflow runs did, for a scrape.
//
// The zero value is usable and is what a deployment with no workflows
// configured costs: a mutex and four nil maps, which allocate nothing
// until something is observed.
//
// Every method is safe to call from the goroutine running a hook, holds
// the lock for a map write and returns. That is the whole contract the
// engine's Observer seam asks for: a metrics backend that blocked would
// be a scrape that can stall a backup.
type Workflow struct {
	mu sync.Mutex

	runs         map[workflowRunKey]uint64
	runDuration  map[string]*histogram
	stepDuration map[workflowStepDurationKey]*histogram
	stepFailures map[workflowStepFailureKey]uint64
	stepTimeouts map[workflowStepKey]uint64
	remoteExec   map[workflowRemoteExecKey]uint64
	truncations  map[string]uint64
}

// The comparable keys each family is bucketed by. Structs rather than a
// joined string, so a label value containing the separator cannot forge a
// different series -- which for a backup set id, the one free-text label
// here, is the difference between a bounded map and one a caller chooses
// the shape of.
type (
	workflowRunKey struct {
		set      string
		status   string
		bypassed bool
	}

	workflowStepDurationKey struct {
		set    string
		phase  string
		target string
	}

	workflowStepKey struct {
		set    string
		scope  string
		phase  string
		target string
	}

	workflowStepFailureKey struct {
		workflowStepKey
		disposition string
	}

	workflowRemoteExecKey struct {
		set         string
		disposition string
	}
)

// WorkflowRun is one finished run, as this exporter needs it.
//
// Strings rather than the domain types, so this package keeps the
// property its own doc claims: it renders values somebody else computed
// and imports no engine. The caller translating its enums into these
// strings is core/service's observer adapter, which is also where the
// enum-to-label mapping can be held against the vocabulary by a test.
type WorkflowRun struct {
	BackupSet string
	Status    string
	Bypassed  bool
	Duration  time.Duration
}

// WorkflowStep is one finished step.
type WorkflowStep struct {
	BackupSet string
	Scope     string
	Phase     string
	Target    string

	// State is the step's terminal state and Disposition is what the
	// executing adapter observed. Both are needed and neither is
	// derivable from the other: the timeout family keys off the state
	// (this product decided the step outlived its bound) and the
	// remote-exec family keys off the disposition (nobody saw how it
	// ended, or it never started).
	State       string
	Disposition string

	Duration time.Duration
}

// The state and disposition words this file makes decisions on.
//
// They are string literals rather than an import of internal/workflow and
// internal/workflowrun, for the reason WorkflowRun's doc gives, and they
// are held against those packages' own constants by a test in
// core/service, which is the layer that already imports both. A
// vocabulary spelled in two places without something comparing them is a
// vocabulary that drifts, and the comparison is what stops it.
const (
	workflowStateTimedOut = "timed_out"

	workflowDispositionExited        = "exited"
	workflowDispositionTransportLost = "transport_lost"
	workflowDispositionNotAttempted  = "not_attempted"

	workflowTargetRemote = "remote"
)

// ObserveRun counts one finished run and records how long it took.
func (w *Workflow) ObserveRun(r WorkflowRun) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.runs == nil {
		w.runs = make(map[workflowRunKey]uint64, 4)
		w.runDuration = make(map[string]*histogram, 4)
	}

	w.runs[workflowRunKey{set: r.BackupSet, status: r.Status, bypassed: r.Bypassed}]++

	h := w.runDuration[r.BackupSet]
	if h == nil {
		h = newHistogram(workflowDurationBuckets)
		w.runDuration[r.BackupSet] = h
	}
	h.observe(r.Duration.Seconds())
}

// ObserveStep counts one finished step: its duration always, and a
// failure, a timeout or a remote-exec failure when it was one.
//
// The three failure families deliberately overlap. A remote step that
// timed out increments the timeout counter AND the failure counter, and a
// remote step whose connection was lost increments the remote-exec
// counter AND the failure counter, because they answer three different
// operator questions -- "are hooks failing", "are they running out of
// time", "is the far side reachable" -- and a partition would make each
// one's answer depend on knowing the other two.
//
// What is NOT a remote-exec failure is a remote hook that ran and exited
// non-zero. The connection worked; somebody's script said no. Counting it
// here would make the family that answers "can this deployment reach the
// source host" go up every time a database refused to quiesce.
func (w *Workflow) ObserveStep(s WorkflowStep) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.stepDuration == nil {
		w.stepDuration = make(map[workflowStepDurationKey]*histogram, 8)
		w.stepFailures = make(map[workflowStepFailureKey]uint64, 8)
		w.stepTimeouts = make(map[workflowStepKey]uint64, 8)
		w.remoteExec = make(map[workflowRemoteExecKey]uint64, 4)
	}

	dk := workflowStepDurationKey{set: s.BackupSet, phase: s.Phase, target: s.Target}
	h := w.stepDuration[dk]
	if h == nil {
		h = newHistogram(workflowDurationBuckets)
		w.stepDuration[dk] = h
	}
	h.observe(s.Duration.Seconds())

	sk := workflowStepKey{set: s.BackupSet, scope: s.Scope, phase: s.Phase, target: s.Target}

	if s.State == workflowStateTimedOut {
		w.stepTimeouts[sk]++
	}

	if workflowStepSucceeded(s.State) {
		return
	}

	w.stepFailures[workflowStepFailureKey{workflowStepKey: sk, disposition: s.Disposition}]++

	if s.Target == workflowTargetRemote && workflowIsExecFailure(s.Disposition) {
		w.remoteExec[workflowRemoteExecKey{set: s.BackupSet, disposition: s.Disposition}]++
	}
}

// ObserveLogTruncation counts one step whose output reached its
// persisted-output bound.
//
// The only label is the target, because that is the only bounded fact
// about it that an operator can act on: a local hook flooding its output
// and a remote one flooding a socket are different problems. Which step
// it was is in the step's own log, where a truncation marker sits in
// sequence at the exact position the recording stopped.
func (w *Workflow) ObserveLogTruncation(target string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.truncations == nil {
		w.truncations = make(map[string]uint64, 2)
	}

	w.truncations[target]++
}

// workflowStepSucceeded reports whether a step's terminal state is one
// that counts against nothing.
//
// Skipped is not a failure. A step the run never reached -- because a
// stage above it failed, or because the run was bypassed -- did not fail;
// the step that DID fail is already counted, and counting its whole tail
// again would make one bad hook look like five.
func workflowStepSucceeded(state string) bool {
	return state == "success" || state == "skipped"
}

// workflowIsExecFailure reports whether a disposition means the far side
// could not be reached or driven, as opposed to a hook that ran.
func workflowIsExecFailure(disposition string) bool {
	switch disposition {
	case workflowDispositionTransportLost, workflowDispositionNotAttempted:
		return true
	default:
		return false
	}
}

// RenderWorkflow renders this counter set as Prometheus text exposition
// format, in the same dialect and with the same prefix Render uses.
//
// A family with no observations renders its HELP and TYPE lines and no
// samples. That is deliberate and is the opposite of the rule Render
// applies to an unknown gauge: a counter at zero is a real reading ("no
// hook has timed out"), and a family that vanished when nothing had
// happened yet would make every dashboard built on it break on exactly
// the deployments where it is working.
//
// Every map is sorted before rendering, for Render's reason: two scrapes
// of the same data have to produce byte-identical output, and ranging a
// map does not.
func (w *Workflow) RenderWorkflow() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	var b strings.Builder

	w.writeRuns(&b)
	writeHistogramFamily(&b, namePrefix+"workflow_run_duration_seconds",
		"How long a whole workflow run took, in seconds.",
		sortedHistograms(w.runDuration, func(set string) []label {
			return []label{{"backup_set", set}}
		}))
	writeHistogramFamily(&b, namePrefix+"workflow_step_duration_seconds",
		"How long one workflow hook script took, in seconds.",
		sortedHistograms(w.stepDuration, func(k workflowStepDurationKey) []label {
			return []label{{"backup_set", k.set}, {"phase", k.phase}, {"target", k.target}}
		}))
	w.writeStepFailures(&b)
	w.writeStepTimeouts(&b)
	w.writeRemoteExecFailures(&b)
	w.writeTruncations(&b)

	return b.String()
}

func (w *Workflow) writeRuns(b *strings.Builder) {
	name := namePrefix + "workflow_runs_total"
	writeHelp(b, name, "Workflow runs finished, by the run's workflow status and whether its hooks were deliberately skipped.")
	writeType(b, name, "counter")

	for _, s := range sortedCounters(w.runs, func(k workflowRunKey) []label {
		return []label{
			{"backup_set", k.set},
			{"status", k.status},
			{"bypassed", boolLabel(k.bypassed)},
		}
	}) {
		writeSample(b, name, s.labels, float64(s.value))
	}
}

func (w *Workflow) writeStepFailures(b *strings.Builder) {
	name := namePrefix + "workflow_step_failures_total"
	writeHelp(b, name, "Workflow hook scripts that did not succeed, by stage and by what became of the step's process.")
	writeType(b, name, "counter")

	for _, s := range sortedCounters(w.stepFailures, func(k workflowStepFailureKey) []label {
		return append(stepLabels(k.workflowStepKey), label{"disposition", k.disposition})
	}) {
		writeSample(b, name, s.labels, float64(s.value))
	}
}

func (w *Workflow) writeStepTimeouts(b *strings.Builder) {
	name := namePrefix + "workflow_step_timeouts_total"
	writeHelp(b, name, "Workflow hook scripts stopped for outliving their configured script timeout.")
	writeType(b, name, "counter")

	for _, s := range sortedCounters(w.stepTimeouts, stepLabels) {
		writeSample(b, name, s.labels, float64(s.value))
	}
}

func (w *Workflow) writeRemoteExecFailures(b *strings.Builder) {
	name := namePrefix + "workflow_remote_exec_failures_total"
	writeHelp(b, name, "Remote workflow hooks this deployment could not run at all: a connection that would not open, a capability that could not be proven, or a channel that closed with no status. A remote hook that ran and exited non-zero is not one of these.")
	writeType(b, name, "counter")

	for _, s := range sortedCounters(w.remoteExec, func(k workflowRemoteExecKey) []label {
		return []label{{"backup_set", k.set}, {"disposition", k.disposition}}
	}) {
		writeSample(b, name, s.labels, float64(s.value))
	}
}

func (w *Workflow) writeTruncations(b *strings.Builder) {
	name := namePrefix + "workflow_log_truncations_total"
	writeHelp(b, name, "Workflow steps whose captured output reached the persisted-output bound and stopped being recorded. The hook kept running; only the recording stopped.")
	writeType(b, name, "counter")

	for _, s := range sortedCounters(w.truncations, func(target string) []label {
		return []label{{"target", target}}
	}) {
		writeSample(b, name, s.labels, float64(s.value))
	}
}

func stepLabels(k workflowStepKey) []label {
	return []label{
		{"backup_set", k.set},
		{"scope", k.scope},
		{"phase", k.phase},
		{"target", k.target},
	}
}

// boolLabel is how a boolean reaches a label value.
//
// "true"/"false" rather than 1/0, because a label is a string dimension
// and not a value: `bypassed="1"` reads as a count of something to
// everybody who has ever seen a Prometheus metric, and the two spellings
// would end up mixed the first time a second boolean label is added.
func boolLabel(v bool) string {
	if v {
		return "true"
	}

	return "false"
}

// label is one rendered dimension. A pair rather than a map, because the
// exposition format's label order is part of the bytes and a map has
// none.
type label struct {
	name  string
	value string
}

// sample is one rendered counter line.
type sample struct {
	labels []label
	value  uint64
	sortBy string
}

// sortedCounters turns one counter map into rendered samples in a stable
// order. The order is the rendered label list's own text, which is
// deterministic and reads in the order an operator scans: backup set
// first, then the dimensions inside it.
func sortedCounters[K comparable](m map[K]uint64, labelsOf func(K) []label) []sample {
	out := make([]sample, 0, len(m))
	for k, v := range m {
		lbls := labelsOf(k)
		out = append(out, sample{labels: lbls, value: v, sortBy: labelKey(lbls)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].sortBy < out[j].sortBy })

	return out
}

// histogramSample is one rendered histogram, with the labels every one of
// its lines carries.
type histogramSample struct {
	labels []label
	hist   *histogram
	sortBy string
}

func sortedHistograms[K comparable](m map[K]*histogram, labelsOf func(K) []label) []histogramSample {
	out := make([]histogramSample, 0, len(m))
	for k, h := range m {
		lbls := labelsOf(k)
		out = append(out, histogramSample{labels: lbls, hist: h, sortBy: labelKey(lbls)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].sortBy < out[j].sortBy })

	return out
}

// labelKey is a sort key built from a label list. It is never rendered;
// the "\x00" separator is there so that two different label lists cannot
// produce the same key by concatenation.
func labelKey(labels []label) string {
	var b strings.Builder
	for _, l := range labels {
		b.WriteString(l.name)
		b.WriteString("\x00")
		b.WriteString(l.value)
		b.WriteString("\x00")
	}

	return b.String()
}

// writeSample renders one line, with every label value escaped by the
// same quoteLabel the rest of this package uses.
func writeSample(b *strings.Builder, name string, labels []label, value float64) {
	b.WriteString(name)
	writeLabels(b, labels, label{})
	b.WriteString(" ")
	b.WriteString(formatFloat(value))
	b.WriteString("\n")
}

// writeLabels renders a label set, optionally with one extra appended
// (which is how a bucket's `le` is added without every caller rebuilding
// the list).
func writeLabels(b *strings.Builder, labels []label, extra label) {
	if len(labels) == 0 && extra.name == "" {
		return
	}
	b.WriteString("{")
	for i, l := range labels {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(b, "%s=%s", l.name, quoteLabel(l.value))
	}
	if extra.name != "" {
		if len(labels) > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(b, "%s=%s", extra.name, quoteLabel(extra.value))
	}
	b.WriteString("}")
}

// writeHistogramFamily renders one histogram family: the cumulative
// buckets, then the sum, then the count, which is the order and the
// naming the exposition format requires.
func writeHistogramFamily(b *strings.Builder, name, help string, samples []histogramSample) {
	writeHelp(b, name, help)
	writeType(b, name, "histogram")

	for _, s := range samples {
		cumulative := uint64(0)
		for i, upper := range s.hist.bounds {
			cumulative += s.hist.counts[i]
			b.WriteString(name + "_bucket")
			writeLabels(b, s.labels, label{"le", formatFloat(upper)})
			fmt.Fprintf(b, " %d\n", cumulative)
		}
		b.WriteString(name + "_bucket")
		writeLabels(b, s.labels, label{"le", "+Inf"})
		fmt.Fprintf(b, " %d\n", s.hist.count)

		b.WriteString(name + "_sum")
		writeLabels(b, s.labels, label{})
		b.WriteString(" " + formatFloat(s.hist.sum) + "\n")

		b.WriteString(name + "_count")
		writeLabels(b, s.labels, label{})
		fmt.Fprintf(b, " %d\n", s.hist.count)
	}
}

// histogram is a fixed-ladder observation counter.
//
// counts[i] holds the observations that fell in (bounds[i-1], bounds[i]],
// NOT the cumulative total: the exposition format wants cumulative
// buckets and they are accumulated at render time, because a bucket
// updated on every observation is len(bounds) writes per observation on a
// path a hook is waiting behind.
type histogram struct {
	bounds []float64
	counts []uint64
	sum    float64
	count  uint64
}

func newHistogram(bounds []float64) *histogram {
	return &histogram{bounds: bounds, counts: make([]uint64, len(bounds))}
}

// observe records one value.
//
// A negative duration is recorded as zero rather than dropped. It cannot
// be produced by a monotonic measurement, but a run's duration is a
// difference of two wall-clock reads through an injectable clock, and a
// histogram that silently discarded observations would make _count
// disagree with the run counter beside it -- which is the one arithmetic
// an operator does with these two families.
func (h *histogram) observe(v float64) {
	if v < 0 {
		v = 0
	}

	h.count++
	h.sum += v

	for i, upper := range h.bounds {
		if v <= upper {
			h.counts[i]++

			return
		}
	}
}
