package snapshotlifecycle

import (
	"fmt"
	"slices"

	"github.com/backupdproject/backupd/core/internal/state"
)

// This file is the graph: which phase of a snapshot run may follow which.
//
// It is a TABLE rather than code, for internal/lifecycle/machine.go's
// reason: "these are all the legal moves" has to be a statement a reader
// can check by looking at one declaration, instead of by finding every
// place a phase name is mentioned. Nothing in this package branches on a
// phase name to decide legality; Validate reads the table, and the tests
// walk it.
//
// The vocabulary itself belongs to internal/state, which owns the column
// it is stored in (see that package's doc on the one piece of vocabulary
// it does own). What lives here is the part a schema cannot express: the
// ORDER, the refusals, and the two edges that exist only because a
// process can die.

// Edge is one legal (From, To) move in the snapshot-run graph.
//
// A comparable struct, exactly like lifecycle.Transition, so the table can
// be turned into a set and legality is one map lookup with no string
// concatenation to build a key.
type Edge struct {
	From state.SnapshotPhase
	To   state.SnapshotPhase
}

// Edges is the single source of truth for every legal phase change. A
// move that is not in here is refused, and current == target is handled by
// Validate as an idempotent no-op rather than being listed as a row (a run
// re-entering the phase it is already in is a retry of a durable write,
// which is the shape the journal's own idempotency is built for).
//
// # The nominal path
//
// PENDING -> SOURCE_SCAN -> SNAPSHOT_WRITE -> MANIFEST_COMMITTED ->
// VERIFICATION -> CATALOG_COMMIT -> SUCCESS. Each edge is one durable
// write, entered BEFORE the work it names rather than after it, which is
// what makes a crash decidable: a row sitting at SNAPSHOT_WRITE says an
// upload was in flight, and a row sitting at SNAPSHOT_WRITE with a
// manifest id on it says the upload finished and the process died before
// it could say so.
//
// # FAILED: reachable from every pre-success phase, and a dead end
//
// Every phase before SUCCESS may fail, and FAILED has no exit. That is
// deliberate and it differs from the artifact graph, where FAILED goes
// back to DISCOVERED: an artifact is one file that can be copied again,
// while a snapshot run is a receipt for one pass over a source at one
// moment. Retrying it means READING THE SOURCE AGAIN, which is a new pass
// at a new moment and therefore a new run with its own row. Re-entering a
// failed row would overwrite the evidence of what went wrong with the
// evidence of what went wrong the second time.
//
// A FAILED row may carry a snapshot id, and that is not a contradiction. A
// run whose source scan came back incomplete has a perfectly well-formed
// manifest of a tree with a hole in it; the manifest is kept, attributed
// and visible precisely so that retention (#785) and an operator can
// decide about it, rather than being deleted by the code that noticed the
// hole.
//
// # SUCCESS -> LOST: the manifest went away
//
// A run that reached SUCCESS made a promise: a restore point exists.
// Reconciliation may later find that the manifest it names is no longer in
// the repository, which is a fact about the world rather than about the
// run, and it must be sayable without rewriting history. LOST says the
// promise can no longer be kept and is what moves last-known-good along to
// the newest run whose snapshot is still there.
//
// # VERIFICATION -> MANIFEST_COMMITTED: the one backward edge
//
// A verification interrupted by a crash is retried from the manifest,
// because the manifest is durable and the verification is not: nothing
// about a half-finished read of a snapshot is worth keeping, and the only
// alternatives are to call an unverified snapshot verified or to fail a
// run whose data is intact. Both are worse. This is the only edge that
// moves a row backwards along the nominal path, and internal/state
// permits exactly this one for the same reason.
//
// # PENDING -> QUARANTINED: a row that was never a run
//
// Reconciliation opens a row for a repository snapshot nothing can
// attribute, so that the thing has a name, a timestamp and a place on a
// screen. Such a row is not a run and never had a source scan, so its only
// legal move is to the verdict it was created to record. See the package
// doc for why the verdict is never "delete it".
var Edges = []Edge{
	// The nominal path.
	{state.PhasePending, state.PhaseSourceScan},
	{state.PhaseSourceScan, state.PhaseSnapshotWrite},
	{state.PhaseSnapshotWrite, state.PhaseManifestCommitted},
	{state.PhaseManifestCommitted, state.PhaseVerification},
	{state.PhaseVerification, state.PhaseCatalogCommit},
	{state.PhaseCatalogCommit, state.PhaseSuccess},

	// Failure, from every phase that can still fail.
	{state.PhasePending, state.PhaseFailed},
	{state.PhaseSourceScan, state.PhaseFailed},
	{state.PhaseSnapshotWrite, state.PhaseFailed},
	{state.PhaseManifestCommitted, state.PhaseFailed},
	{state.PhaseVerification, state.PhaseFailed},
	{state.PhaseCatalogCommit, state.PhaseFailed},

	// Crash recovery's re-entry: verify again from the durable manifest.
	{state.PhaseVerification, state.PhaseManifestCommitted},

	// The world changing under a finished run.
	{state.PhaseSuccess, state.PhaseLost},
	{state.PhaseSuccess, state.PhaseDeleted},
	{state.PhaseLost, state.PhaseDeleted},

	// A row opened for something that was never a run of ours.
	{state.PhasePending, state.PhaseQuarantined},
}

// legal is Edges as a set, built once. A map lookup rather than a linear
// scan because Validate is called on every durable write a run makes.
var legal = func() map[Edge]struct{} {
	set := make(map[Edge]struct{}, len(Edges))
	for _, e := range Edges {
		set[e] = struct{}{}
	}

	return set
}()

// ErrIllegalPhase is returned by Validate for a move the graph does not
// contain.
//
// It is one sentinel rather than one per shape because the caller's
// response is the same for all of them -- refuse the write, report the
// attempt -- and the sentence carries which move was attempted.
type ErrIllegalPhase struct {
	From state.SnapshotPhase
	To   state.SnapshotPhase
}

func (e ErrIllegalPhase) Error() string {
	return fmt.Sprintf("snapshotlifecycle: a snapshot run may not move from %s to %s", e.From, e.To)
}

// Validate reports whether a run in phase from may move to phase to.
//
// from == to is legal and means nothing has to change: a durable write
// that was made, and then made again because the process that made it did
// not survive to hear that it had, must not be the thing that fails a
// backup. Every other move is a table lookup.
//
// Both phases are parsed first, so an unknown phase is refused as an
// unknown phase rather than reported as an illegal move between something
// and something else.
func Validate(from, to state.SnapshotPhase) error {
	if _, err := state.ParseSnapshotPhase(string(from)); err != nil {
		return fmt.Errorf("snapshotlifecycle: current phase: %w", err)
	}

	if _, err := state.ParseSnapshotPhase(string(to)); err != nil {
		return fmt.Errorf("snapshotlifecycle: target phase: %w", err)
	}

	if from == to {
		return nil
	}

	if _, ok := legal[Edge{From: from, To: to}]; !ok {
		return ErrIllegalPhase{From: from, To: to}
	}

	return nil
}

// Successors is every phase a run in this phase may move to, in the order
// the table declares them.
//
// It exists for the surfaces that have to offer or explain the next step
// without restating the graph, and for the tests that walk it. A terminal
// phase returns nothing, which is the same answer Terminal reports and is
// derived from the table rather than from a second list of terminal
// phases.
func Successors(p state.SnapshotPhase) []state.SnapshotPhase {
	var out []state.SnapshotPhase
	for _, e := range Edges {
		if e.From == p {
			out = append(out, e.To)
		}
	}

	return out
}

// Phases is every phase, in the order a run passes through them, with the
// outcomes last. It is a copy the caller owns, following
// model.BackupEngines(): a caller able to assign through it could add a
// phase Validate has never heard of.
func Phases() []state.SnapshotPhase { return slices.Clone(state.SnapshotPhases()) }
