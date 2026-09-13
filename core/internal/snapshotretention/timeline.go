package snapshotretention

import (
	"fmt"
	"sort"

	"github.com/backupdproject/backupd/core/internal/lifecycle"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/state"
)

// This file is the projection, and it is the reason this package can claim
// that snapshot retention and artifact retention are the same retention.
// Everything here answers one question: what does a snapshot run look like
// to a classifier that was written for artifacts?
//
// # Which runs are in scope, and why the answer is so narrow
//
// FR-18 classifies "managed, completed backups" and nothing else: an
// artifact still in flight, or one that failed, or one that was
// quarantined, never receives a GFS verdict at all (see gfs.go's
// gfsManagedCompleteStates). The snapshot equivalent of that set is one
// phase wide. A run at SUCCESS has been through the whole machine --
// source read completely, manifest committed, verification concluded,
// catalog written -- and its snapshot is a restore point this product
// advertises. Every other phase is one of the states FR-18 excludes:
// PENDING through CATALOG_COMMIT is in flight, FAILED and QUARANTINED
// never produced a restore point, LOST and DELETED no longer have a
// manifest to decide about.
//
// The consequence worth stating out loud, because it is the §39 case: a
// run whose backup completed and whose VERIFICATION then failed is at
// FAILED, so it is not in the timeline, so retention forms no opinion
// about it and never deletes its manifest. That is the same treatment a
// FAILED artifact gets, and it is deliberate in both places. What becomes
// of a failed run's manifest is a reconciliation question (is it an
// orphan? is it evidence?) and answering it by deleting is the one answer
// that cannot be taken back.
//
// # Which timestamp a snapshot is bucketed by
//
// The run's own start, and only that. FR-18's calculation admits two
// placements for an artifact -- when this manager discovered it, and the
// producer's own reported modification time -- because an artifact is a
// file somebody else wrote and whose age this manager did not witness
// (see bucketkey.go). A snapshot has no such second party: this manager
// started the run, off its own clock, and there is no remote-supplied
// timestamp to admit or refuse. So the projection carries the discovery
// placement and leaves the producer one absent, which is exactly what a
// state.Record with no Remote.ModTime means. Nothing is lost by that: the
// producer term can only ever ADD a keep (bucketkey.go's own invariant),
// and there is no producer here to add one.

// Unclassifiable is one snapshot run the projection could not turn into
// something the classifier can decide about, together with why.
//
// It exists because the alternative is silence. A run at SUCCESS carrying
// a manifest is IN retention's scope by every rule this package has, so
// dropping it from the timeline because its manifest id cannot be
// addressed would produce a preview that simply does not mention a
// snapshot in the repository -- and a plan whose omissions are invisible
// is the kind of plan an operator confirms. Decide turns each of these
// into a refusal instead, which is a decision nobody made, said out loud.
//
// Runs that are out of scope by PHASE are not here and never will be: a
// run still in flight, or one that failed, is not something retention
// declined to decide, it is something retention has no business deciding.
type Unclassifiable struct {
	RunID      string
	SnapshotID string

	// Why is one sentence naming what could not be done, in the words a
	// refusal will carry.
	Why string
}

// Timeline projects one backup set's snapshot runs onto the record shape
// internal/retention's classifier consumes, newest last, together with
// every run that is in scope and could not be projected.
//
// set is the identity every projected record is attributed to, and it is
// the caller's resolved set rather than the one on each row, because
// retention refuses records from another set outright (FR-7 isolation) and
// a row whose names were written before a rename would otherwise fail the
// whole pass. The lineage a run belongs to is its set_uuid, which the
// caller has already filtered on; the names are display metadata (see
// 0010_snapshot_run_lineage.sql).
//
// The returned records are sorted by manifest id, which is the order
// retention's own output is sorted in, so a caller comparing the two never
// has to sort either.
func Timeline(set model.BackupSetID, runs []state.SnapshotRun) ([]state.Record, []Unclassifiable) {
	var (
		records        []state.Record
		unclassifiable []Unclassifiable
	)

	for _, run := range runs {
		if !InScope(run) {
			continue
		}

		artifact, err := model.NewArtifactID(set, run.SnapshotID)
		if err != nil {
			// The manifest id is the classifier's identity for this
			// snapshot, and an id it cannot address is one no verdict can
			// be attributed to. REFUSE, via the caller, rather than a
			// silent omission or a fabricated name: see Unclassifiable.
			unclassifiable = append(unclassifiable, Unclassifiable{
				RunID:      run.RunID,
				SnapshotID: run.SnapshotID,
				Why: fmt.Sprintf(
					"run %s committed manifest %q, which this product cannot address as an identity (%v), so no retention decision can be attributed to it",
					run.RunID, run.SnapshotID, err),
			})
			continue
		}

		records = append(records, state.Record{
			Artifact: artifact,

			// COMPLETE is the artifact state whose meaning matches
			// SUCCESS: the backup succeeded and the pipeline is finished
			// with it. It has to be one of the four states
			// gfsManagedCompleteStates admits, or the classifier would
			// treat every snapshot as out of scope and decide nothing at
			// all; COMPLETE is the one of those four that does not also
			// claim something about a remote copy this engine does not
			// have.
			State: string(lifecycle.Complete),

			// The discovery placement, and deliberately the only one. See
			// this file's preamble.
			DiscoveredAt: run.StartedAt,
			UpdatedAt:    run.UpdatedAt,
		})
	}

	sort.Slice(records, func(i, j int) bool { return records[i].Artifact.Name < records[j].Artifact.Name })
	sort.Slice(unclassifiable, func(i, j int) bool { return unclassifiable[i].RunID < unclassifiable[j].RunID })
	return records, unclassifiable
}

// InScope reports whether retention has any business deciding about this
// run's snapshot.
//
// It is exported because the delete path re-derives it immediately before
// the irreversible call, against a freshly read row rather than the one
// the plan was drawn from, in the same spirit as internal/retention's
// pruneVerifySafeToDelete: a safety check worth having is worth re-running
// at the point of the dangerous action.
//
// The three conditions are one rule each, and none of them is implied by
// another. SUCCESS is FR-18's "managed, completed backup". A manifest id
// is what makes the snapshot addressable at all, and a SUCCESS row without
// one is a contradiction this function refuses rather than interprets. And
// a verification recorded as FAILED disqualifies the row whatever its
// phase says: a run should never reach SUCCESS carrying one, so a row that
// does is two facts in disagreement, and the answer to that on a delete
// path is to decide nothing.
func InScope(run state.SnapshotRun) bool {
	return run.Phase == state.PhaseSuccess &&
		run.SnapshotID != "" &&
		run.VerificationStatus != state.SnapshotVerificationFailed
}
