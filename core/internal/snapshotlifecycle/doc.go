// Package snapshotlifecycle drives one incremental backup set's run
// through its durable phases, and reconciles what a crash left behind.
//
// It is to the snapshot engine what internal/lifecycle is to the artifact
// engine: the state machine and the crash-recovery rules, with the
// persistence in internal/state and the data plane in
// internal/backupengine. It holds no repository, opens no source and
// speaks no vendor's vocabulary; everything it touches arrives as an
// interface this package declares.
//
// # The one sentence this package exists to enforce
//
// A manifest returned by the engine is not a successful backup.
//
// The engine's job ends when a snapshot is stored. Everything a person
// means by "the backup worked" comes after that: the source was read
// completely, the snapshot was verified to the level this deployment asked
// for, and the catalog durably says so. Each of those is a phase here, and
// each of them is a place a process can die. So the phases are persisted
// as they are entered (PENDING, SOURCE_SCAN, SNAPSHOT_WRITE,
// MANIFEST_COMMITTED, VERIFICATION, CATALOG_COMMIT, SUCCESS), and every
// boundary between them has a reconciliation rule that a test pins.
//
// The consequence that matters most is negative: until a run reaches
// SUCCESS, no restore point is advertised, the previous last-known-good
// snapshot is not pruned and not replaced, and the set's success timestamp
// does not move. A newer snapshot that failed anywhere before the catalog
// commit loses to the older one that did not, every time.
//
// # Why reconciliation never deletes
//
// The repository is the other half of the durable state, and the two can
// disagree in both directions after a crash: a manifest with no catalog
// row, and a catalog row whose manifest is gone. The tempting symmetry --
// delete what the catalog does not know about -- is refused outright. A
// manifest this manager cannot attribute may be a snapshot written by a
// run whose catalog write never landed, a snapshot belonging to a co-tenant
// backup set in a shared repository domain, or a snapshot an operator took
// by hand with the vendor's own tooling against their own bucket. Deleting
// it costs a restore point that somebody may be relying on, and the
// information needed to tell those cases apart is exactly the information
// the crash destroyed. So an unattributable snapshot is QUARANTINED: it is
// written down, named, surfaced, and left in place for a person to decide
// about.
//
// # What is deliberately not here
//
// No retention, no pruning, no maintenance scheduling: those are #785 and
// #786, and this package's only relationship with them is the durable
// delete INTENT it can read (a snapshot delete that was interrupted is a
// decidable state, and deciding it is not the same as issuing it).
//
// Verification DEPTH is #784's. This package runs the verification the
// repository offers and records two different facts about it -- the level
// the set was configured for and the level a run actually proved -- because
// a row that recorded only the configured level would claim a content
// verification nobody performed.
package snapshotlifecycle
