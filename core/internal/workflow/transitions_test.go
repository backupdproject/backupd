package workflow

import "testing"

// The two transition graphs, walked exhaustively (#811).
//
// internal/lifecycle's machine_test.go walks its own graph the same way
// and for the same reason: a graph checked example by example is a graph
// whose missing edge nobody notices until a run is stuck at three in the
// morning.

func TestStepTransitions(t *testing.T) {
	t.Parallel()

	legal := map[State][]State{
		StatePending:     {StateRunning, StateSkipped, StateFailed, StateCanceled},
		StateRunning:     {StateSuccess, StateFailed, StateTimedOut, StateCanceled, StateInterrupted},
		StateSuccess:     nil,
		StateFailed:      nil,
		StateTimedOut:    nil,
		StateCanceled:    nil,
		StateSkipped:     nil,
		StateInterrupted: nil,
	}

	walk(t, "step", StepStates(), legal, State.CanFollowForStep)
}

func TestRunTransitions(t *testing.T) {
	t.Parallel()

	legal := map[State][]State{
		StatePending: {StateRunning, StateCanceled, StateFailed, StateRecoveryRequired},
		StateRunning: {
			StateCleanupRunning, StateSuccess, StateFailed, StateTimedOut,
			StateCanceled, StateRecoveryRequired,
		},
		StateCleanupRunning: {
			StateSuccess, StateFailed, StateTimedOut, StateCanceled,
			StateCleanupFailed, StateRecovered, StateRecoveryRequired,
		},
		StateRecoveryRequired: {StateCleanupRunning, StateRecovered, StateCleanupFailed},
	}

	walk(t, "run", RunStates(), legal, State.CanFollowForRun)
}

func walk(t *testing.T, what string, states []State, legal map[State][]State, canFollow func(State, State) error) {
	t.Helper()

	for _, from := range states {
		allowed := map[State]bool{}
		for _, to := range legal[from] {
			allowed[to] = true
		}

		for _, to := range states {
			err := canFollow(from, to)
			if allowed[to] && err != nil {
				t.Errorf("%s %q -> %q is legal and was refused: %v", what, from, to, err)
			}
			if !allowed[to] && err == nil {
				t.Errorf("%s %q -> %q is not legal and was accepted", what, from, to)
			}
		}
	}
}

// A terminal state has no exits, which is what stops a later pass
// re-running a hook whose outcome is already in the audit.
func TestNoTerminalStateHasAnExit(t *testing.T) {
	t.Parallel()

	for _, from := range StepStates() {
		if !from.Terminal() {
			continue
		}
		for _, to := range StepStates() {
			if err := from.CanFollowForStep(to); err == nil {
				t.Errorf("a step may move from the terminal %q to %q", from, to)
			}
		}
	}

	for _, from := range RunStates() {
		if !from.Terminal() {
			continue
		}
		for _, to := range RunStates() {
			if err := from.CanFollowForRun(to); err == nil {
				t.Errorf("a run may move from the terminal %q to %q", from, to)
			}
		}
	}
}

// Wherever a process dies, the reconciliation has somewhere to put what
// it found, and the two answers are different on purpose. A step that was
// RUNNING has an outcome nobody will ever know, so it can be recorded as
// interrupted; a step that was still PENDING ran nothing, so the honest
// record is skipped and interrupted is refused -- a pending step recorded
// as interrupted would put a half-applied side effect on the books that
// never happened. The run is what carries "this needs a person".
func TestEveryUnfinishedPositionCanErrTowardRecovery(t *testing.T) {
	t.Parallel()

	if err := StateRunning.CanFollowForStep(StateInterrupted); err != nil {
		t.Errorf("a running step cannot be recorded as interrupted: %v", err)
	}
	if err := StatePending.CanFollowForStep(StateSkipped); err != nil {
		t.Errorf("a pending step cannot be recorded as skipped: %v", err)
	}
	if err := StatePending.CanFollowForStep(StateInterrupted); err == nil {
		t.Error("a step that never started may be recorded as interrupted, which puts a side effect nobody had on the books")
	}

	for _, from := range RunStates() {
		if from.Terminal() || from == StateRecoveryRequired {
			continue
		}
		if err := from.CanFollowForRun(StateRecoveryRequired); err != nil {
			t.Errorf("a run in %q cannot be recorded as requiring recovery: %v", from, err)
		}
	}
}

// StateRecovered says a run that needed recovery got it, so it is
// reachable from nowhere else. A run that was never interrupted must not
// be able to claim it, because the whole value of the state is that it
// does not flatten an interruption into success.
func TestOnlyAnInterruptedRunCanBecomeRecovered(t *testing.T) {
	t.Parallel()

	for _, from := range RunStates() {
		err := from.CanFollowForRun(StateRecovered)

		allowed := from == StateRecoveryRequired || from == StateCleanupRunning
		if allowed && err != nil {
			t.Errorf("a run in %q cannot become recovered: %v", from, err)
		}
		if !allowed && err == nil {
			t.Errorf("a run in %q, which was never interrupted, may claim to have been recovered", from)
		}
	}
}
