// Package repomaintenance runs the embedded engine's own repository
// maintenance safely: one owner per Repository Domain, an exclusive
// operation fenced against destructive ones, and the vendor's safety
// margins left exactly as the vendor set them (EPIC K, issue #786).
//
// # It reclaims space; it never decides what to stop keeping
//
// This is the other half of the line internal/snapshotretention draws.
// Retention decides which SNAPSHOT stops being protected and removes its
// manifest, one at a time, from a two-method port that cannot reach
// storage. This package removes nothing by name: it asks the repository
// to reclaim whatever no manifest references any more, which is a
// question only the repository can answer, and its own port is
// correspondingly narrow -- Maintain and Stats, pinned by
// boundary_test.go. A maintenance pass that could delete a snapshot would
// be a second retention policy with no catalog, no holds and no
// last-known-good protection behind it.
//
// So the two are sequential and not alternatives: nothing is reclaimed
// until retention has deleted a manifest AND maintenance has found the
// content behind it unreferenced. Content a retained or held snapshot
// still points at survives every maintenance window there is, which is
// the property TestFullMaintenanceReclaimsOnlyWhatNothingReferences
// proves against a real repository.
//
// # One owner, and why this product has to enforce it itself
//
// The engine has its own answer to concurrent maintenance -- an owner
// string written into the repository, checked before it will run -- and
// this product's adapter deliberately overrides it (see the adapter's
// Maintain: deferring to whichever machine created the repository means a
// maintenance window that silently never runs). Having overridden it,
// this package owes the same guarantee from its own side, and pays it in
// two places:
//
//   - a durable ownership record per domain
//     (backupengine.MaintenanceOwnership), claimed by the instance that
//     maintains the repository and never taken from another instance
//     except by an explicit administrative Transfer;
//   - a Fence, which is what stops this instance's own passes from
//     interleaving a full maintenance with a snapshot delete.
//
// Neither is a distributed lock and neither pretends to be one. What
// makes even an unfenced concurrent pass non-destructive is the vendor's
// safety parameters, which is why the third thing this package does is
// refuse to weaken them: nothing here can ask for the "ignore safety" or
// "delete content immediately" behaviour the vendor also offers, because
// the mode is chosen from backupengine's two-value MaintenanceMode and
// the safety level is not a parameter of this port at all. The adapter
// passes maintenance.SafetyFull and
// internal/backupengine/kopia/maintenancesafety_test.go fails if that
// ever changes.
//
// # What it records, and why the record is not the journal
//
// Every window appends to the ownership record: what mode ran, whether
// it did anything, what it measured, and how it ended. That record is a
// file beside the repository's other local state rather than a row in
// the state journal, for the reason backupengine's own store gives -- the
// journal is the artifact catalog, and a repository's maintenance state
// is not artifact state.
//
// A failed window is recorded and alerted (AlertConditions, through
// internal/alert's existing model) and changes nothing else. It cannot
// have damaged a restore point, because a failure means the reclamation
// did not happen, and the only thing a repository loses by not being
// maintained is space.
//
// # What is deliberately not here
//
// No CLI, API or UI surface, and no Prometheus rendering: those are #788,
// and what this package offers them is Measure, a value built from the
// record. No configuration knobs either -- Intervals is a value the caller
// supplies and DefaultIntervals is what it means when they do not.
package repomaintenance
