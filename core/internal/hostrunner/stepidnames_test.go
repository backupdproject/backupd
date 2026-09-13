package hostrunner

import (
	"regexp"
	"strings"
	"testing"
)

// The two rules a step id has to satisfy at once, and the reason neither
// may narrow the other (#813).
//
// internal/workflow.StepID joins an order, a scope, a phase and a script
// name with "~", deliberately: it is the one character the script-name
// rule reserves, so the id cannot be ambiguous. That id then has to
// become a path component under the runtime directory AND part of a
// docker container name, and the two accept different alphabets. Getting
// either wrong is the same operator-visible failure -- every `.local.sh`
// hook refused, against a perfectly healthy runner, for a name nobody
// chose.

// aMintedStepID is the shape internal/workflow really produces. It is
// written out rather than imported so this package keeps depending on
// nothing: what matters is the shape, and a change to it that this
// literal missed would show up as a step id this package cannot handle,
// which is the failure under test.
const (
	aMintedRunID  = "wfr_2b7bd8c6-d695-459a-9fac-b9c7373b2c4e"
	aMintedStepID = "0000~global~before~10-quiesce.local.sh"
)

// dockerNamePattern is docker's own rule for a container name.
var dockerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func TestAMintedStepIDIsAcceptedAsAPathComponent(t *testing.T) {
	if err := ValidID("run id", aMintedRunID); err != nil {
		t.Errorf("ValidID refused a run id this product mints: %v", err)
	}
	if err := ValidID("step id", aMintedStepID); err != nil {
		t.Errorf("ValidID refused a step id this product mints, so no local hook could execute at all: %v", err)
	}
}

func TestAMintedStepIDBecomesALegalContainerName(t *testing.T) {
	name := containerName(aMintedRunID, aMintedStepID, "0123456789abcdef")

	if !dockerNamePattern.MatchString(name) {
		t.Fatalf("the container name %q is not one docker accepts, so `docker create` refuses every local hook", name)
	}
	if strings.Contains(name, "~") {
		t.Errorf("the tilde every step id carries reached the container name: %q", name)
	}
	// Still legible: the point of transliterating rather than hashing is
	// that an operator reading `docker ps` can see which step it is.
	if !strings.Contains(name, "10-quiesce.local.sh") {
		t.Errorf("the name no longer says which script it is running: %q", name)
	}
	if !strings.HasPrefix(name, containerNamePrefix) {
		t.Errorf("the name is not one a sweep of this runner's leftovers would select: %q", name)
	}
}

func TestTransliterationKeepsTheLengthTheBudgetWasComputedFrom(t *testing.T) {
	// The id portion is bounded by arithmetic over BYTES, so a
	// substitution that changed the length would make that bound wrong.
	if got, want := len(dockerNameSafe(aMintedStepID)), len(aMintedStepID); got != want {
		t.Errorf("transliteration changed the length from %d to %d", want, got)
	}
}
