// Package snapshotretention decides which of a backup set's snapshots may
// be deleted, and is the only thing in this product allowed to ask a
// repository to remove one (EPIC K, issue #785).
//
// It is a bridge, not a second retention engine, and that distinction is
// the whole design. The policy -- FR-18's GFS tiers, FR-19's
// last-known-good protection, and the calendar arithmetic underneath both
// -- lives in internal/retention and is not reimplemented here for
// snapshots. What lives here is the projection that lets a snapshot
// history be classified by that same code (timeline.go), the one
// protection a snapshot has and an artifact does not (a hold, see below),
// and the delete path that carries a committed decision to the engine
// (prune.go).
//
// # Why a projection instead of a second classifier
//
// "Existing backupd retention semantics map to snapshots" is issue #785's
// first acceptance criterion, and there are two ways to satisfy it. One is
// to write a GFS implementation for snapshots and a test asserting it
// agrees with the artifact one. The other is to have exactly one
// implementation and hand it both kinds of history. The first is the
// version that drifts: the two implementations agree on the day the test
// is written, and then a tier granularity, a DST edge or a tie-break rule
// is fixed in one of them.
//
// So Timeline projects the catalog's snapshot runs onto the shape
// internal/retention already consumes (state.Record), and this package
// calls retention.DecideKeep. A snapshot timeline and an artifact timeline
// describing the same instants produce the same verdicts because they are
// the same calculation, not because two calculations were compared.
//
// # What this package adds on top of the shared classifier
//
// A hold. It is the one input a snapshot has that an artifact does not: a
// person's decision that one specific snapshot must not be deleted, for a
// reason no policy can compute, recorded durably in the catalog
// (state.PlaceSnapshotHold, 0011_snapshot_holds.sql). It is composed into
// the classifier's verdicts exactly the way FR-19's protected term is --
// as an extra KEEP with its own tier name and no placement, because a hold
// is not a bucket selection -- rather than as a filter applied afterwards,
// so a preview can say a snapshot is kept BY the hold and name it.
//
// # The three things this package must never do
//
// It never enables a competing retention schedule inside the engine. The
// engine has snapshot-removal primitives and this product uses exactly
// one of them, one snapshot at a time, after deciding. An engine-side
// retention policy would delete on terms this catalog never recorded and
// could not see a hold at all (that is why backupengine.Repository has no
// Prune, Forget or ApplyRetention: see its package doc).
//
// It never touches repository-internal storage. Deleting a snapshot
// removes a MANIFEST; the content behind it stays until the engine's own
// maintenance decides nothing references it any more, which is issue #786
// and not this package. The Repository port below is two methods wide for
// that reason: there is no blob, pack, index or maintenance surface in
// scope here for a future change to reach for, and
// TestRetentionsRepositoryPortCannotReachRepositoryStorage fails the build
// if one appears.
//
// And it never deletes a snapshot the catalog has not first recorded an
// intent for. The intent is written before the repository is asked, so a
// crash in between is decidable rather than a guess: a run with an intent
// whose manifest is gone is a delete that completed, and a run with no
// intent whose manifest is gone is a loss to investigate. Those two demand
// opposite responses.
//
// # What is deliberately out of scope
//
// Physical reclamation (#786), restore (#787) and every CLI, API and UI
// surface (#788). A pass here reports what it decided and what it removed;
// nothing in this package renders anything.
package snapshotretention
