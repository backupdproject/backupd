package remoteexec

import (
	"strings"
	"testing"

	"github.com/retnd/retnd/core/internal/workflow"
)

// TestStepTokenIsUsableForEveryIdentifierThisProductCanProduce is #919
// asserted at its source. The token is built to satisfy tokenRule rather
// than offered to it, so the interesting inputs are the ones that would
// have been refused: a real workflow step id, whose tilde separators
// tokenRule has never admitted, and every other shape a script name, a
// backup set name or a future identifier could carry into it.
//
// tokenRule itself is the oracle. Asserting against a hand-written pattern
// here would let the two drift apart, which is exactly the drift #919 was:
// two rules, each correct on its own, that had been mutually unsatisfiable
// since the day both existed.
func TestStepTokenIsUsableForEveryIdentifierThisProductCanProduce(t *testing.T) {
	t.Parallel()

	longName := strings.Repeat("a", 300) + ".remote.sh"

	for _, tc := range []struct {
		name   string
		runID  string
		stepID string
	}{
		{
			name:   "a real step id",
			runID:  "wfr_2f1c9a54-8a1e-4c77-9f1b-7c3d2e5a6b90",
			stepID: workflow.StepID(10, workflow.ScopeSet, workflow.PhaseBefore, "10-quiesce.remote.sh"),
		},
		{
			name:   "a script name longer than the whole token",
			runID:  "wfr_2f1c9a54-8a1e-4c77-9f1b-7c3d2e5a6b90",
			stepID: workflow.StepID(9999, workflow.ScopeGlobal, workflow.PhaseAfter, longName),
		},
		{name: "spaces", runID: "run one", stepID: "step two"},
		{name: "a command substitution", runID: "$(id)", stepID: "`whoami`"},
		{name: "a leading tilde", runID: "~root", stepID: "~/step"},
		{name: "a shell metacharacter throughout", runID: "a;b|c&d>e", stepID: "f<g*h?i"},
		{name: "not ascii", runID: "läuft", stepID: "步骤"},
		{name: "nothing at all", runID: "", stepID: ""},
		{name: "no run", runID: "", stepID: "0001~set~pre~a.remote.sh"},
		{name: "a newline", runID: "run\nid", stepID: "step\tid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			token := StepToken(tc.runID, tc.stepID)
			if !tokenRule.MatchString(token) {
				t.Fatalf("StepToken(%q, %q) = %q, which tokenRule refuses -- so Run would refuse the step before opening a session (#919)",
					tc.runID, tc.stepID, token)
			}

			// The refusal tokenRule exists for is a shell reading the
			// token as something other than one ordinary word, and the
			// leading byte is the case the character class alone does
			// not cover.
			if strings.HasPrefix(token, "-") {
				t.Errorf("token %q begins with a dash, which the receiving bash reads as options of its own", token)
			}

			if got := (Request{Token: token, Sink: &collectingSink{}}).validate(); got != nil {
				t.Errorf("a request carrying this token was refused: %v", got)
			}
		})
	}
}

// TestStepTokenNamesOneStepOfOneRun is the reaper's requirement. The token
// is matched as a whole "-s" operand in the remote process list
// (groupScript), so two steps that share one are two steps whose
// termination cannot be told apart: stopping one would find, and kill, a
// hook running normally under the other.
func TestStepTokenNamesOneStepOfOneRun(t *testing.T) {
	t.Parallel()

	const (
		runA = "wfr_2f1c9a54-8a1e-4c77-9f1b-7c3d2e5a6b90"
		runB = "wfr_9b8d7c6e-5a4f-4321-8765-0fedcba98765"
	)
	stepA := workflow.StepID(10, workflow.ScopeSet, workflow.PhaseBefore, "10-quiesce.remote.sh")
	stepB := workflow.StepID(20, workflow.ScopeSet, workflow.PhaseBefore, "10-quiesce.remote.sh")

	// The long pair is the truncated case, where the pair's digest is the
	// only thing left carrying the distinction: the two step ids agree
	// for far longer than the token's ceiling.
	longA := workflow.StepID(10, workflow.ScopeSet, workflow.PhaseBefore, strings.Repeat("q", 200)+"1.remote.sh")
	longB := workflow.StepID(10, workflow.ScopeSet, workflow.PhaseBefore, strings.Repeat("q", 200)+"2.remote.sh")

	seen := map[string]string{}
	for _, pair := range [][2]string{
		{runA, stepA}, {runA, stepB}, {runB, stepA}, {runB, stepB},
		{runA, longA}, {runA, longB}, {runB, longA},
	} {
		token := StepToken(pair[0], pair[1])
		if owner, clash := seen[token]; clash {
			t.Fatalf("token %q is shared by %s and (%q, %q): terminating one step would reap the other",
				token, owner, pair[0], pair[1])
		}
		seen[token] = "(" + pair[0] + ", " + pair[1] + ")"
	}

	// Stability for the same pair: the token is handed to the far side
	// once and read back out of the process list later, so a token that
	// differed between those two moments would be a step nothing could
	// terminate.
	if first, second := StepToken(runA, stepA), StepToken(runA, stepA); first != second {
		t.Errorf("the same step produced %q and then %q", first, second)
	}
}
