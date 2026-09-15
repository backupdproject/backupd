package workflowrun

import (
	"time"

	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/workflow"
)

// The measurement seam: what this engine tells a metrics backend about a
// run, a step and a lost byte of output.
//
// # Why the engine reports rather than a caller deriving
//
// Two of the seven families #813 asks for cannot be derived from
// RunResult by anything above this package, and one of them is the one
// that matters most operationally. A log truncation happens inside the
// per-step bound (logs.go) and leaves a marker in the journal rather than
// a field on the result; counting it from outside would mean a scan of
// every run's log records on every scrape. A remote-exec failure is a
// DISPOSITION -- transport lost, not attempted -- on a step whose target
// is remote, and the disposition is deliberately not the step's state
// (#810: transport loss is never a known exit code), so a caller reading
// only states would count a connection that never opened as a hook that
// failed.
//
// So the engine states the facts and this interface carries them. It is
// nil-safe at every call site, because the overwhelmingly common
// deployment has no scrape target and must pay nothing for one.
//
// # Why these observations carry no script name and no path
//
// Because they become Prometheus LABELS, and a label whose value is a
// filename an operator chose is unbounded cardinality: a deployment that
// generates a hook per database ends up with one time series per
// database, forever, in a process that never forgets a series it has
// emitted. #813 names that prohibition twice. Everything here is drawn
// from a CLOSED vocabulary -- a scope, a phase, a target, a state, a
// disposition, a status -- except the backup set id, which is bounded by
// the configuration and is already a label on every other backupd_ metric
// family.
//
// The one thing an observation could carry and does not is a hook's own
// output. That belongs in the per-step log, redacted and bounded, and
// nowhere else.

// RunObservation is one finished workflow run, as a metrics backend needs
// it.
type RunObservation struct {
	BackupSetID model.BackupSetID

	// Status is the run's WORKFLOW verdict, which is the one axis a
	// single counter can carry. The backup's and the cleanup's are
	// separate facts and stay on the run row and the event stream; a
	// metric that multiplied the three would be three dimensions of a
	// closed vocabulary multiplied by every backup set.
	Status workflow.Status

	// Bypassed says the run's hooks were deliberately skipped, which is
	// a label rather than a separate family: "how many runs happened"
	// and "how many of them ran nobody's hooks" are the same question
	// asked twice, and an operator watching for a deployment where
	// somebody has started routinely passing --skip-workflow-scripts
	// needs them in one family to compare.
	Bypassed bool

	Duration time.Duration
}

// StepObservation is one finished step.
type StepObservation struct {
	BackupSetID model.BackupSetID

	Scope  workflow.Scope
	Phase  workflow.Phase
	Target workflow.Target

	// State is the step's terminal state and Disposition is what the
	// adapter observed. Both, because they answer different questions:
	// a remote step that came back transport_lost and one that exited 1
	// are both StateFailed, and only the second is a hook that ran.
	State       workflow.State
	Disposition Disposition

	Duration time.Duration
}

// Observer receives one call per finished run, per finished step and per
// truncated step log.
//
// Every method must be safe to call from the goroutine running a hook and
// must not block: a metrics backend that took a lock a scrape holds would
// be a scrape that can stall a backup.
type Observer interface {
	ObserveWorkflowRun(RunObservation)
	ObserveWorkflowStep(StepObservation)

	// ObserveWorkflowLogTruncation is one step's output reaching the
	// persisted-output bound. It carries the target and nothing else:
	// which step it was is in the step's own log, and a step id is a
	// per-run value that must never become a label.
	ObserveWorkflowLogTruncation(target workflow.Target)
}

// observeRun and the two beside it are the nil-safe call sites. They are
// methods on Engine rather than nil checks scattered through the stage
// loop, so the loop reads as the sequence it is.
func (e *Engine) observeRun(o RunObservation) {
	if e.Observer == nil {
		return
	}

	e.Observer.ObserveWorkflowRun(o)
}

func (e *Engine) observeStep(o StepObservation) {
	if e.Observer == nil {
		return
	}

	e.Observer.ObserveWorkflowStep(o)
}

func (e *Engine) observeTruncation(target workflow.Target) {
	if e.Observer == nil {
		return
	}

	e.Observer.ObserveWorkflowLogTruncation(target)
}
