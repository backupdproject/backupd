package service

import (
	"context"

	"github.com/backupdproject/backupd/core/internal/state"
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

// OperationSnapshot is one snapshot run an operation performed.
//
// The four byte counts are four different facts and a surface must never
// present one as another: LogicalBytes is what the source tree weighs,
// SourceBytesRead is what the run pulled off the source,
// RepositoryBytesWritten is what actually landed in storage, and
// ContentReusedBytes is what deduplication saved. Rendering the first as
// "uploaded" is the specific misrepresentation EPIC K forbids.
type OperationSnapshot struct {
	// BackupSetID is "source/set".
	BackupSetID string

	// RunID is this manager's identifier for the pass; SnapshotID is the
	// engine's opaque manifest id, empty for a run that never committed
	// one. Neither is a path and neither is a secret.
	RunID      string
	SnapshotID string

	// Engine and RepositoryDomain are which machinery ran and which
	// declared boundary its output went into.
	Engine           string
	RepositoryDomain string

	// Phase is the run's durable phase: PENDING, SOURCE_SCAN,
	// SNAPSHOT_WRITE, MANIFEST_COMMITTED, VERIFICATION, CATALOG_COMMIT,
	// SUCCESS, FAILED, LOST, DELETED or QUARANTINED.
	//
	// A phase and not a boolean, because "did this work" has more than
	// two answers here and the interesting ones are in the middle: a run
	// whose manifest is committed and unverified is neither a success
	// nor a failure, and a client that had to guess would guess wrong in
	// whichever direction its author was feeling.
	Phase string

	// Succeeded is whether this run advertises a restore point, which is
	// the one question every client asks and the one place the phase
	// vocabulary should not have to be interpreted client-side.
	Succeeded bool

	// LastKnownGood is whether this run's snapshot is currently the set's
	// last-known-good restore point.
	LastKnownGood bool

	// SourceConsistency is the mode the operator arranged around the
	// source while it was read (live_best_effort, externally_quiesced,
	// external_snapshot). It is on the receipt because it is what a
	// restore point's trustworthiness rests on.
	SourceConsistency string

	// VerificationStatus is "", "pending", "passed" or "failed".
	// VerificationLevel is what the set asked for and
	// VerificationAchieved is what this run actually proved. The last two
	// are separate because a set configured for a restore drill whose run
	// only verified content must not read as having drilled.
	VerificationStatus   string
	VerificationLevel    string
	VerificationAchieved string

	// Files and Directories are what the snapshot holds.
	Files       int64
	Directories int64

	// The four byte counts. See this type's own doc.
	LogicalBytes           int64
	SourceBytesRead        int64
	RepositoryBytesWritten int64
	ContentReusedBytes     int64

	// Measured is false when the counters were never taken, which is the
	// state of a snapshot adopted by crash reconciliation: the manifest
	// exists and the process that would have counted the bytes died. A
	// client renders that as "not measured", never as zero.
	Measured bool

	// Reason is the run's own sentence about why it failed, was
	// quarantined or was lost. Empty for a successful run.
	Reason string
}

// deriveSnapshots attaches the snapshot runs an operation performed.
//
// A read failure leaves the field nil rather than failing the poll. The
// operation row is the receipt a client is waiting for, and an answer of
// "your request is still running" plus an unavailable extra is strictly
// better than no answer at all; nothing about a byte count is a safety
// claim that has to be refused when it cannot be read.
func (b *BackupService) deriveSnapshots(ctx context.Context, op Operation) Operation {
	if op.ID == "" {
		return op
	}

	runs, err := b.journal.SnapshotRunsByOperation(ctx, op.ID, maxOperationSnapshots)
	if err != nil || len(runs) == 0 {
		return op
	}

	out := make([]OperationSnapshot, 0, len(runs))
	for _, run := range runs {
		out = append(out, toOperationSnapshot(run))
	}

	op.Snapshots = out

	return op
}

func toOperationSnapshot(run state.SnapshotRun) OperationSnapshot {
	return OperationSnapshot{
		BackupSetID:            run.Set.String(),
		RunID:                  run.RunID,
		SnapshotID:             run.SnapshotID,
		Engine:                 run.Engine,
		RepositoryDomain:       run.Domain,
		Phase:                  string(run.Phase),
		Succeeded:              run.Phase.Advertised(),
		LastKnownGood:          run.LastKnownGood,
		SourceConsistency:      run.ConsistencyMode,
		VerificationStatus:     run.VerificationStatus,
		VerificationLevel:      run.VerificationLevel,
		VerificationAchieved:   run.VerificationLevelAchieved,
		Files:                  counterOf(run.Files),
		Directories:            counterOf(run.Directories),
		LogicalBytes:           counterOf(run.LogicalBytes),
		SourceBytesRead:        counterOf(run.SourceBytesRead),
		RepositoryBytesWritten: counterOf(run.RepositoryBytesWritten),
		ContentReusedBytes:     counterOf(run.ContentReusedBytes),
		Measured:               run.SourceBytesRead != nil && run.RepositoryBytesWritten != nil,
		Reason:                 run.Reason,
	}
}

// counterOf flattens a counter the catalog keeps nullable, with Measured
// above carrying the distinction the nil was there to make.
func counterOf(v *int64) int64 {
	if v == nil {
		return 0
	}

	return *v
}
