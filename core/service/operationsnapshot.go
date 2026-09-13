package service

import (
	"context"
)

// The snapshot half of a durable operation, on the surface a client
// already polls.
//
// # Why this is not a Kopia API
//
// EPIC K is explicit that the embedded engine gets no API of its own: a
// caller asks what an OPERATION did, and an operation that took a
// snapshot answers with the snapshot it took. So this is a nested object
// on the existing /operations read, populated from the catalog, in this
// product's own vocabulary -- a run id, a phase, four byte counts and a
// verification claim -- with nothing of the vendor's in it.
//
// # Why it is derived per read rather than stored on the row
//
// An operation's result text is written once, when the operation
// finishes. A snapshot run outlives that: reconciliation can later find
// its manifest gone and move the row to LOST, and a client polling the
// operation that produced it should see that rather than a frozen claim
// from the night it succeeded. Reading the catalog is one indexed query
// by operation id (state.SnapshotRunsByOperation), and it happens on
// GetOperation only -- never on ListOperations, for the reason
// deriveRestore gives about the same choice: a round trip per row on a
// page nobody is watching.

// maxOperationSnapshots bounds how many runs one operation reports.
//
// A run_cycle over a deployment of incremental sets takes one snapshot
// per set, so this is a count of backup sets rather than of history, and
// the bound exists so that a deployment with hundreds of sets cannot make
// one polling read unbounded.
const maxOperationSnapshots = 64

// deriveSnapshots attaches the snapshot runs an operation performed.
//
// A read failure leaves the field nil rather than failing the poll. The
// operation row is the receipt a client is waiting for, and an answer of
// "your request is still running" plus an unavailable extra is strictly
// better than no answer at all; nothing about a byte count is a safety
// claim that has to be refused when it cannot be read.
//
// The holds are NOT attached here, and that is the one asymmetry between
// this projection and the snapshot resource next door. An operation is a
// receipt for work that just happened, holds are a protection decision
// somebody makes afterwards, and reading them would put one query per
// run onto the polling path that a client hits every second while a
// backup runs.
func (b *BackupService) deriveSnapshots(ctx context.Context, op Operation) Operation {
	if op.ID == "" {
		return op
	}

	runs, err := b.journal.SnapshotRunsByOperation(ctx, op.ID, maxOperationSnapshots)
	if err != nil || len(runs) == 0 {
		return op
	}

	out := make([]Snapshot, 0, len(runs))
	for _, run := range runs {
		out = append(out, toSnapshot(run, nil))
	}

	op.Snapshots = out

	return op
}
