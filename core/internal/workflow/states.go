package workflow

import "fmt"

// The vocabularies. Every string this domain will ever persist about a
// workflow is declared here, once, and the three properties that matter
// about that are worth stating before anything below is changed.
//
// The step vocabulary and the run vocabulary are ONE type with two
// membership tests, not two types. They overlap in eight of their twelve
// values, and two types would mean two spellings of "failed" that compare
// unequal at the one seam where a run's state is derived from its steps'
// -- which is exactly the mistake a compiler could not catch, because both
// sides would type-check. The membership tests are what keep the four
// run-only values off a step, and they are what the journal's write path
// calls.
//
// Nothing here is a CHECK constraint in SQL. 0002 and 0006 both had to
// rebuild a table to widen artifacts.state's CHECK, and migrate.go's
// suspendForeignKeys exists because of the damage that did to a populated
// journal; a vocabulary that is going to grow as #809-#814 land belongs
// where widening it costs a function, not a table rebuild. So the refusal
// lives in Go, on the write path, and 0012_workflow_runs.sql stores a
// plain TEXT column.
//
// And these are durable strings. They are written into a journal an older
// or newer build may read, so a value's SPELLING is a compatibility
// surface: "timed_out" may not become "timedOut" to match a JSON
// convention somewhere, and a value may not be removed once anything has
// written it.

// State is one position in a workflow run's or a workflow step's
// lifecycle.
type State string

// The eight states a STEP can be in, which are also the first eight a run
// can be in.
const (
	// StatePending is planned and not started. Every step in a fresh Plan
	// is here, and it is the state that makes a plan auditable before
	// anything has happened.
	StatePending State = "pending"

	// StateRunning is started and not finished. On a step it means a
	// process exists (or existed, if this process has since died: see
	// StateInterrupted for what a recovery turns it into).
	StateRunning State = "running"

	// StateSuccess is finished with exit status 0.
	StateSuccess State = "success"

	// StateFailed is finished with a non-zero exit status, or could not be
	// started at all.
	StateFailed State = "failed"

	// StateTimedOut is a step this product killed for exceeding its
	// timeout. It is distinct from StateFailed because the two are
	// different operator problems -- a script that returns 1 is a script
	// that decided something, and one that ran out of time is usually a
	// script waiting on something that never came -- and because a
	// timeout is a decision THIS product took, which an audit has to be
	// able to attribute.
	StateTimedOut State = "timed_out"

	// StateCanceled is a step a person or a shutdown stopped before it
	// finished.
	StateCanceled State = "canceled"

	// StateSkipped is a step that was planned and deliberately not run:
	// an earlier step in the same stage failed, or the stage was
	// abandoned. It is a real state rather than an absence so that a
	// plan read back after the fact accounts for every step it declared.
	StateSkipped State = "skipped"

	// StateInterrupted is a step that was StateRunning when this process
	// died. Nobody observed its exit status and nobody ever will, which
	// is precisely why it is not StateFailed: a recovery pass has to be
	// able to tell "this script reported failure" from "this script's
	// outcome is unknown and its side effects may be half-applied".
	StateInterrupted State = "interrupted"
)

// The four additional states only a RUN can be in. They are about the run
// as a whole -- what has to happen next, and by whom -- which is a
// question no individual step has.
const (
	// StateRecoveryRequired is a run this process cannot finish on its
	// own: a step is StateInterrupted, so a side effect may be
	// half-applied and the next backup must not simply proceed as if
	// nothing happened. It is the state that keeps a spool alive (see
	// Terminal).
	StateRecoveryRequired State = "recovery_required"

	// StateCleanupRunning is a run whose cleanup stage is executing.
	StateCleanupRunning State = "cleanup_running"

	// StateCleanupFailed is a run whose cleanup stage itself failed. A
	// terminal state, and the worst one: the backup may be fine and the
	// machine is not back the way the workflow found it.
	StateCleanupFailed State = "cleanup_failed"

	// StateRecovered is a run that needed recovery and got it. It is not
	// StateSuccess: what happened here is that somebody, or something,
	// cleaned up after an interruption, and flattening that into success
	// would erase the one fact worth keeping about the run.
	StateRecovered State = "recovered"
)

// stepStates is the step vocabulary in a FIXED order, because it is
// rendered into refusal messages and an operator comparing two runs' error
// text must not see the list reorder. Ranging a map to build a "must be
// one of" sentence is the bug this slice exists to avoid; internal/config's
// validate.go states the same rule for its own closed vocabularies.
var stepStates = []State{
	StatePending,
	StateRunning,
	StateSuccess,
	StateFailed,
	StateTimedOut,
	StateCanceled,
	StateSkipped,
	StateInterrupted,
}

// runOnlyStates are the four a run adds, in the order a run reaches them.
var runOnlyStates = []State{
	StateRecoveryRequired,
	StateCleanupRunning,
	StateCleanupFailed,
	StateRecovered,
}

// StepStates returns the states a step may be in, in the documented order.
//
// A function rather than an exported slice, for internal/backend's
// CapabilityKeys reason: a package-level slice is writable by every
// importer, so one line in a caller -- or in a test of a caller -- could
// reorder or truncate the vocabulary for the validation code that is
// supposed to be the authority on it.
func StepStates() []State { return append([]State(nil), stepStates...) }

// RunStates returns every state a run may be in: the step vocabulary
// followed by the four a run adds.
func RunStates() []State {
	out := make([]State, 0, len(stepStates)+len(runOnlyStates))
	out = append(out, stepStates...)

	return append(out, runOnlyStates...)
}

// ValidForStep reports whether s is a state a step may be in.
func (s State) ValidForStep() bool {
	for _, known := range stepStates {
		if s == known {
			return true
		}
	}

	return false
}

// ValidForRun reports whether s is a state a run may be in.
func (s State) ValidForRun() bool {
	if s.ValidForStep() {
		return true
	}

	for _, known := range runOnlyStates {
		if s == known {
			return true
		}
	}

	return false
}

// Terminal reports whether nothing more will happen to this run or step
// without somebody starting something new.
//
// This is load-bearing rather than convenient, and it is the reason the
// method is here rather than spelled out at each call site: the spool
// retention rule is "keep the captured scripts until the run is terminal
// AND its recovery is resolved", and a comparison written the wrong way
// round at one of two call sites deletes the scripts a recovery was going
// to run.
//
// StateRecoveryRequired and StateCleanupRunning are deliberately NOT
// terminal: both are "something still has to happen here". Everything
// else, StateCleanupFailed and StateInterrupted included, is: a failed
// cleanup is over and needs a person, and an interrupted step's outcome is
// never going to become known.
func (s State) Terminal() bool {
	switch s {
	case StatePending, StateRunning, StateRecoveryRequired, StateCleanupRunning:
		return false
	default:
		return true
	}
}

// The two transition graphs: what a STEP may do next, and what a RUN may
// do next (#811).
//
// # Why they are tables here rather than checks at the call site
//
// internal/lifecycle's machine.go makes the argument for the artifact
// pipeline and it is the same one: the graph is the safety property, so
// it has to be one object a test can walk exhaustively. The specific
// thing being protected is what a RESTART reads. A journal row can only
// be trusted to describe something that happened if there was no write
// that could have put it where it is by mistake, so the journal's write
// path asks these two functions and refuses anything else -- which makes
// "a crash between any two durable transitions reconciles
// deterministically" a statement about a finite graph rather than about
// every code path that ever writes a state.
//
// # The two properties worth reading before changing either table
//
// Nothing leaves a terminal state. A step that was recorded as skipped,
// failed or interrupted is over, and an edge out of it would let a later
// pass re-run a hook whose outcome is already in the audit.
//
// Every non-terminal state can reach the state that means "this needs a
// person": StateInterrupted for a step, StateRecoveryRequired for a run.
// That is what makes erring toward recovery possible from wherever a
// process happened to die.

// stepTransitions is the complete legal edge set for a step.
//
// StatePending -> StateSkipped is the abandoned-stage edge: an earlier
// step failed, so this one is deliberately not run, and recording that
// is what makes a plan read back after the fact account for every step it
// declared. StatePending -> StateFailed is the could-not-be-started edge
// -- a runner that refused the bytes, a connection that would not open --
// which is a failure of the step rather than an absence of one.
var stepTransitions = map[State][]State{
	StatePending: {StateRunning, StateSkipped, StateFailed, StateCanceled},
	StateRunning: {
		StateSuccess,
		StateFailed,
		StateTimedOut,
		StateCanceled,
		StateInterrupted,
	},
}

// runTransitions is the complete legal edge set for a run.
//
// StateCleanupRunning is reachable from StateRunning (the ordinary
// unwinding) and from StateRecoveryRequired (a resume-cleanup), and those
// two are the only ways cleanup ever starts. StateRecovered is reachable
// only from StateRecoveryRequired, because "somebody cleaned up after an
// interruption" is not something a run that was never interrupted can
// claim.
var runTransitions = map[State][]State{
	StatePending: {StateRunning, StateCanceled, StateFailed, StateRecoveryRequired},
	StateRunning: {
		StateCleanupRunning,
		StateSuccess,
		StateFailed,
		StateTimedOut,
		StateCanceled,
		StateRecoveryRequired,
	},
	StateCleanupRunning: {
		StateSuccess,
		StateFailed,
		StateTimedOut,
		StateCanceled,
		StateCleanupFailed,
		StateRecovered,
		StateRecoveryRequired,
	},
	StateRecoveryRequired: {StateCleanupRunning, StateRecovered, StateCleanupFailed},
}

// CanFollowForStep reports whether a step in state s may move to next,
// returning the refusal rather than a bool so the journal's write path
// can hand an operator the sentence.
//
// A no-op (s to s) is refused: an idempotent-looking write would hide a
// double advance, and both places that matter -- a resumed cleanup and a
// reconciliation pass -- need to know whether they were the one that
// moved it.
func (s State) CanFollowForStep(next State) error {
	if !s.ValidForStep() {
		return vocabularyError("step state", s, StepStates())
	}
	if !next.ValidForStep() {
		return vocabularyError("step state", next, StepStates())
	}

	return follows("step", s, next, stepTransitions[s])
}

// CanFollowForRun reports whether a run in state s may move to next. See
// CanFollowForStep.
func (s State) CanFollowForRun(next State) error {
	if !s.ValidForRun() {
		return vocabularyError("run state", s, RunStates())
	}
	if !next.ValidForRun() {
		return vocabularyError("run state", next, RunStates())
	}

	return follows("run", s, next, runTransitions[s])
}

// follows is the shared answer, including the two refusals that are worth
// telling apart: a terminal state has no exits at all, which is a
// different mistake from an edge that was simply never declared.
func follows(what string, from, to State, allowed []State) error {
	for _, a := range allowed {
		if a == to {
			return nil
		}
	}

	if from.Terminal() {
		return fmt.Errorf("workflow: this %s is %q, which is terminal, and cannot move to %q; re-opening a %s whose outcome is already recorded would re-run work an audit says is over",
			what, from, to, what)
	}

	return fmt.Errorf("workflow: a %s cannot move from %q to %q", what, from, to)
}

// Status is what a hook is told about a phase of the run it is part of:
// the value behind RETND_BACKUP_STATUS, RETND_WORKFLOW_STATUS and
// RETND_CLEANUP_STATUS.
//
// It is a smaller vocabulary than State on purpose. A hook is not being
// handed this product's internal lifecycle; it is being told the one thing
// a shell script can branch on. StatusUnknown is the honest answer a
// "before" hook gets about the backup that has not run yet, and it is a
// real value rather than an empty string because an unset variable and a
// variable saying "nobody knows yet" read identically in `test -z` and
// only one of them is true.
type Status string

// The status vocabulary.
const (
	StatusUnknown Status = "unknown"
	StatusRunning Status = "running"
	StatusSuccess Status = "success"
	StatusFailed  Status = "failed"
	StatusSkipped Status = "skipped"
)

var statuses = []Status{StatusUnknown, StatusRunning, StatusSuccess, StatusFailed, StatusSkipped}

// Statuses returns the status vocabulary in the documented order.
func Statuses() []Status { return append([]Status(nil), statuses...) }

// Valid reports whether s is a status this product will ever export.
func (s Status) Valid() bool {
	for _, known := range statuses {
		if s == known {
			return true
		}
	}

	return false
}

// RecoveryState is the second axis of a run: whether an interruption has
// been dealt with.
//
// It is separate from State rather than folded into it because the two
// answer different questions and a run needs both at once. A run can be
// StateRecovered (nothing more will happen to it) with recovery
// RecoveryResolved, and it can be StateCleanupFailed (terminal, bad) with
// recovery RecoveryRequired still outstanding -- and that pair is exactly
// the case the spool must not be reclaimed under.
type RecoveryState string

// The recovery vocabulary.
const (
	// RecoveryNone is a run nothing was ever interrupted in. Every run
	// starts here.
	RecoveryNone RecoveryState = "none"

	// RecoveryRequired is a run with an interrupted step, waiting for a
	// recovery pass or a person.
	RecoveryRequired RecoveryState = "required"

	// RecoveryInProgress is a recovery that has started. It exists so a
	// second pass, or a second daemon, does not start one on top of it.
	RecoveryInProgress RecoveryState = "in_progress"

	// RecoveryResolved is a recovery that finished. It says nothing about
	// whether the run succeeded.
	RecoveryResolved RecoveryState = "resolved"
)

var recoveryStates = []RecoveryState{RecoveryNone, RecoveryRequired, RecoveryInProgress, RecoveryResolved}

// RecoveryStates returns the recovery vocabulary in the documented order.
func RecoveryStates() []RecoveryState { return append([]RecoveryState(nil), recoveryStates...) }

// Valid reports whether r is a recovery state this domain knows.
func (r RecoveryState) Valid() bool {
	for _, known := range recoveryStates {
		if r == known {
			return true
		}
	}

	return false
}

// Settled reports whether this recovery state no longer holds anything
// open. See State.Terminal for why the pair is asked together.
func (r RecoveryState) Settled() bool {
	return r == RecoveryNone || r == RecoveryResolved
}

// Scope is which configuration declared a step: the deployment's own hook
// directories, or one backup set's.
//
// It is durable and it is not cosmetic. A global "after" hook and a
// per-set "after" hook are two different operator decisions that can both
// name a script called the same thing, and the scope is what keeps them
// distinguishable in a plan, in a spool path and in an audit.
type Scope string

// The two scopes.
const (
	ScopeGlobal Scope = "global"
	ScopeSet    Scope = "set"
)

var scopes = []Scope{ScopeGlobal, ScopeSet}

// Scopes returns the scope vocabulary in the documented order.
func Scopes() []Scope { return append([]Scope(nil), scopes...) }

// Valid reports whether s is a known scope.
func (s Scope) Valid() bool {
	for _, known := range scopes {
		if s == known {
			return true
		}
	}

	return false
}

// Phase is which side of the backup a step runs on.
type Phase string

// The two phases.
const (
	PhaseBefore Phase = "before"
	PhaseAfter  Phase = "after"
)

var phases = []Phase{PhaseBefore, PhaseAfter}

// Phases returns the phase vocabulary in the documented order.
func Phases() []Phase { return append([]Phase(nil), phases...) }

// Valid reports whether p is a known phase.
func (p Phase) Valid() bool {
	for _, known := range phases {
		if p == known {
			return true
		}
	}

	return false
}

// Target is WHERE a step's script runs, and it comes from the script's own
// basename rather than from any configuration.
//
// That is the single most important decision in this package. A target
// that were configurable per directory, or defaulted when unstated, would
// mean a script can be moved or a key can be edited and the same bytes
// then run somewhere else -- on a production NAS instead of on this
// daemon's host. So it is a property of the FILENAME, it is mandatory, and
// ParseScriptName refuses a name that does not state it.
type Target string

// The two targets.
const (
	// TargetLocal runs on the machine running this daemon.
	TargetLocal Target = "local"

	// TargetRemote runs on the host the backup set pulls from, over the
	// set's own connection. A remote step needs an execution connection
	// (Step.ExecutionConnectionRef); a plan that declares one without it
	// is refused by Step.Validate.
	TargetRemote Target = "remote"
)

var targets = []Target{TargetLocal, TargetRemote}

// Targets returns the target vocabulary in the documented order.
func Targets() []Target { return append([]Target(nil), targets...) }

// Valid reports whether t is a known target.
func (t Target) Valid() bool {
	for _, known := range targets {
		if t == known {
			return true
		}
	}

	return false
}

// vocabularyError renders the "must be one of" refusal for any of the
// vocabularies above, in the fixed declared order.
func vocabularyError[T ~string](what string, got T, allowed []T) error {
	names := make([]string, 0, len(allowed))
	for _, a := range allowed {
		names = append(names, string(a))
	}

	return fmt.Errorf("workflow: %s %q is not one of %v", what, string(got), names)
}
