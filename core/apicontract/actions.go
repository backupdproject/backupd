package apicontract

// The values SubmitOperationRequest.Action takes, which is the one field
// on this contract whose VALUE selects a code path rather than carrying
// data.
//
// # Why they are here and hand-written
//
// The contract document types that field as a string with no enum, so the
// generator has nothing to emit and this file is not generated (only
// contract.gen.go is compared against the document by
// scripts/api/check-contract-drift.sh). What the document does say, in
// SubmitOperationRequest's own description, is which action reads which
// parameter object, and the first three below are those names. The
// fourth, ActionRestoreSnapshot, is the durable action #787 writes rows
// with; the document grows its request parameters in #788, when a route
// starts accepting one.
//
// They live in this package because it is the one both sides of the wire
// may import. core/service and core/internal/archive define the action a
// durable operation row is written with, and core/cliecho has to decide
// which `backupd` command an action is equivalent to; core/service
// imports core/cliecho, so cliecho cannot read the constants from there.
// Before this file it read string literals instead, and one of them was
// wrong: it switched on "restore", which no client has ever sent, so
// every restore and every per-set run fell through to a default arm whose
// sentence was about a different verb (issue #599 review). A constant
// spelled in one place cannot be wrong in one of them.
//
// core/service.ActionRunCycle, core/service.ActionRunBackupSet,
// core/service.ActionRestoreSnapshot and core/internal/archive.ActionRestore
// are defined FROM these, so the wire value, the durable row's value and
// the echoed command all read one spelling.
const (
	// ActionRunCycle runs one cycle across every enabled backup set in
	// the deployment.
	ActionRunCycle = "run_cycle"

	// ActionRunBackupSet runs one cycle over exactly the backup set named
	// by SubmitOperationRequest.BackupSetID.
	ActionRunBackupSet = "run_backup_set"

	// ActionRestorePlacement asks a storage provider to make one archived
	// copy readable again, with the parameters in
	// SubmitOperationRequest.Restore.
	ActionRestorePlacement = "restore_placement"

	// ActionRestoreSnapshot restores a stored snapshot, or one directory
	// or file inside it, to a local directory on the machine running
	// this deployment (EPIC K, #787).
	//
	// It is a different act from ActionRestorePlacement and the two are
	// never folded together: that one asks a storage provider to make an
	// archived object readable again, over hours, at a cost, and writes
	// nothing anywhere; this one reads a restore point this deployment
	// holds and writes a tree onto a disk. An operator who confused them
	// would either pay a retrieval bill for a file that was never
	// archived or wait for a restore that is already finished.
	//
	// Its request parameters are not on this contract yet: the engine and
	// the durable operation land in #787 and the route that submits one
	// lands in #788.
	ActionRestoreSnapshot = "restore_snapshot"

	// ActionVerifySnapshot proves, now, that a stored snapshot is
	// actually restorable, at a stated depth (EPIC K, #788).
	//
	// It is separate from the verification a RUN performs because the
	// two are different claims about different moments: a run's
	// verification says what was proven on the night it ran, and this
	// one says what is provable today. Recording this one onto that
	// run's row would make a snapshot nobody verified at the time
	// indistinguishable from one that was, so it is reported on the
	// operation that performed it instead.
	ActionVerifySnapshot = "verify_snapshot"

	// ActionHoldSnapshot stops retention deleting one named snapshot
	// until somebody releases the hold (EPIC K, #788).
	ActionHoldSnapshot = "hold_snapshot"

	// ActionReleaseSnapshotHold ends one hold, by its id.
	//
	// It is a separate action from placing one rather than a boolean on
	// the same action, because the two take different parameters and
	// mean opposite things: a request whose meaning is decided by a flag
	// is a request somebody eventually sends with the flag wrong.
	ActionReleaseSnapshotHold = "release_snapshot_hold"
)
