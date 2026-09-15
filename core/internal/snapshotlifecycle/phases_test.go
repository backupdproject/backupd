package snapshotlifecycle_test

import (
	"errors"
	"testing"

	"github.com/retnd/retnd/core/internal/snapshotlifecycle"
	"github.com/retnd/retnd/core/internal/state"
)

func TestValidate_TheNominalPathIsLegalInOrderAndIllegalOutOfIt(t *testing.T) {
	t.Parallel()

	nominal := []state.SnapshotPhase{
		state.PhasePending,
		state.PhaseSourceScan,
		state.PhaseSnapshotWrite,
		state.PhaseManifestCommitted,
		state.PhaseVerification,
		state.PhaseCatalogCommit,
		state.PhaseSuccess,
	}

	for i := range len(nominal) - 1 {
		if err := snapshotlifecycle.Validate(nominal[i], nominal[i+1]); err != nil {
			t.Errorf("%s -> %s is refused: %v", nominal[i], nominal[i+1], err)
		}
	}

	// Skipping a phase is the interesting refusal: it is exactly what a
	// caller that "knows" the snapshot is fine would do, and it is what
	// would let a run reach success without a verification ever having
	// been attempted.
	if err := snapshotlifecycle.Validate(state.PhaseManifestCommitted, state.PhaseSuccess); err == nil {
		t.Error("a run may jump from a committed manifest straight to success, skipping verification")
	}

	if err := snapshotlifecycle.Validate(state.PhaseSnapshotWrite, state.PhaseCatalogCommit); err == nil {
		t.Error("a run may jump from the upload straight to the catalog commit")
	}
}

func TestValidate_ReEnteringThePhaseARunIsAlreadyInIsANoOp(t *testing.T) {
	t.Parallel()

	for _, p := range snapshotlifecycle.Phases() {
		if err := snapshotlifecycle.Validate(p, p); err != nil {
			t.Errorf("%s -> %s is refused: %v", p, p, err)
		}
	}
}

func TestValidate_TheOnlyBackwardMoveIsAVerificationRetriedFromItsManifest(t *testing.T) {
	t.Parallel()

	if err := snapshotlifecycle.Validate(state.PhaseVerification, state.PhaseManifestCommitted); err != nil {
		t.Errorf("a verification interrupted by a crash cannot be retried: %v", err)
	}

	backwards := []snapshotlifecycle.Edge{
		{From: state.PhaseSnapshotWrite, To: state.PhaseSourceScan},
		{From: state.PhaseManifestCommitted, To: state.PhaseSnapshotWrite},
		{From: state.PhaseCatalogCommit, To: state.PhaseVerification},
		{From: state.PhaseSuccess, To: state.PhaseCatalogCommit},
	}

	for _, e := range backwards {
		if err := snapshotlifecycle.Validate(e.From, e.To); err == nil {
			t.Errorf("%s -> %s is permitted", e.From, e.To)
		}
	}
}

func TestValidate_AFailedRunIsADeadEndAndIsNeverReEntered(t *testing.T) {
	t.Parallel()

	if got := snapshotlifecycle.Successors(state.PhaseFailed); len(got) != 0 {
		t.Errorf("FAILED has successors %v; a retry is a new run with its own row", got)
	}

	for _, p := range snapshotlifecycle.Phases() {
		if p == state.PhaseFailed {
			continue
		}

		if err := snapshotlifecycle.Validate(state.PhaseFailed, p); err == nil {
			t.Errorf("FAILED -> %s is permitted", p)
		}
	}
}

func TestValidate_ATerminalPhaseCanOnlyMoveWhereTheWorldForcesIt(t *testing.T) {
	t.Parallel()

	// SUCCESS and LOST are terminal for a run and still have to be able
	// to record what happened to the SNAPSHOT afterwards: it went away,
	// or a delete this product intended was carried out. Nothing else
	// may follow them, and QUARANTINED and DELETED are final.
	cases := map[state.SnapshotPhase][]state.SnapshotPhase{
		state.PhaseSuccess:     {state.PhaseLost, state.PhaseDeleted},
		state.PhaseLost:        {state.PhaseDeleted},
		state.PhaseDeleted:     {},
		state.PhaseQuarantined: {},
	}

	for from, want := range cases {
		got := snapshotlifecycle.Successors(from)
		if len(got) != len(want) {
			t.Errorf("%s has successors %v, want %v", from, got, want)

			continue
		}

		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s has successors %v, want %v", from, got, want)
			}
		}
	}
}

func TestValidate_AnUnknownPhaseIsRefusedAsUnknownRatherThanAsAnIllegalMove(t *testing.T) {
	t.Parallel()

	var illegal snapshotlifecycle.ErrIllegalPhase

	err := snapshotlifecycle.Validate("PENDING", "ASCENDED")
	if err == nil {
		t.Fatal("a phase nothing has heard of was accepted")
	}

	if errors.As(err, &illegal) {
		t.Errorf("an unknown phase was reported as an illegal move: %v", err)
	}

	if err := snapshotlifecycle.Validate("", state.PhaseSourceScan); err == nil {
		t.Error("an empty current phase was accepted")
	}
}

// TestEdges_EveryPhaseTheJournalPersistsHasARuleHere is the pin between
// the two halves of this vocabulary: internal/state owns the column and
// its CHECK constraint, this package owns the graph, and the failure mode
// worth defending against is a phase added to one and forgotten in the
// other -- a row the schema accepts and reconciliation has no rule for,
// which is an invisible backup.
func TestEdges_EveryPhaseTheJournalPersistsHasARuleHere(t *testing.T) {
	t.Parallel()

	mentioned := map[state.SnapshotPhase]bool{}
	for _, e := range snapshotlifecycle.Edges {
		mentioned[e.From] = true
		mentioned[e.To] = true
	}

	for _, p := range state.SnapshotPhases() {
		if !mentioned[p] {
			t.Errorf("phase %s is persisted by internal/state and appears nowhere in the graph", p)
		}
	}

	for p := range mentioned {
		if _, err := state.ParseSnapshotPhase(string(p)); err != nil {
			t.Errorf("the graph names phase %s, which internal/state cannot persist: %v", p, err)
		}
	}
}

func TestEdges_EveryNonTerminalPhaseCanFailAndEveryTerminalPhaseReportsItself(t *testing.T) {
	t.Parallel()

	for _, p := range state.SnapshotPhases() {
		switch {
		case p.Terminal():
			if len(snapshotlifecycle.Successors(p)) > 0 && p != state.PhaseSuccess && p != state.PhaseLost {
				t.Errorf("terminal phase %s has successors", p)
			}
		default:
			if err := snapshotlifecycle.Validate(p, state.PhaseFailed); err != nil {
				t.Errorf("phase %s cannot fail: %v", p, err)
			}
		}
	}
}

func TestVerdictKinds_AreACopyTheCallerOwns(t *testing.T) {
	t.Parallel()

	first := snapshotlifecycle.VerdictKinds()
	if len(first) == 0 {
		t.Fatal("no verdicts")
	}

	first[0] = "mutated"

	if snapshotlifecycle.VerdictKinds()[0] == "mutated" {
		t.Error("a caller can rewrite the verdict vocabulary for the whole process")
	}
}
