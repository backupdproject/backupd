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
//     interleaving a full maintenance with a snapshot delete, and what
//     serialises one domain's maintenance windows against each other.
//
// Every rule the record expresses is a check followed by a write, so the
// store it lives in offers no unconditional write: Create is atomic and
// fails if the repository already has a record, and CompareAndSwap
// writes only if the stored record is still at the revision the caller
// read. Without that, two instances both read "unclaimed" and both claim
// it, two administrators both transfer from the same expected owner, and
// a window that started before a handover writes itself back in as owner
// when it finishes.
//
// # What single-owner does not cover
//
// The record is LOCAL: a file under this instance's own state directory.
// Two instances that share a repository but not a state directory each
// claim it, and nothing here can see that. The atomic store fixes the
// races that are coordinatable -- concurrent claims and transfers through
// one state directory, a stale window reverting a handover, two due
// passes clobbering one history -- and does not make ownership visible
// across instances.
//
// It cannot: the embedded engine has no atomic create-if-absent on the
// storage both instances can see (kopia refuses
// blob.PutOptions.DoNotRecreate on every backend this product supports,
// and its manifests are last-write-wins), and its own maintenance
// exclusivity is a local lock file plus an advisory owner string. What
// keeps that case non-destructive is the third thing this package does:
// refuse to weaken the vendor's safety parameters. Nothing here can ask
// for the "ignore safety" or "delete content immediately" behaviour the
// vendor also offers, because the mode is chosen from backupengine's
// two-value MaintenanceMode and the safety level is not a parameter of
// this port at all. The adapter passes maintenance.SafetyFull and
// internal/backupengine/kopia/maintenancesafety_test.go fails if that
// ever changes. A shared coordinator, if this product ever needs one,
// belongs where maintenance is wired up and hardened (#788, #789); see
// ADR 0017.
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
