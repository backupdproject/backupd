package snapshotretention

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/retention"
	"github.com/backupdproject/backupd/core/internal/state"
)

// This file is where a snapshot is actually removed, and it is arranged
// the way internal/retention's prune.go is arranged, for the same reasons
// that file gives at length: one decision function, computed identically
// for the preview and the pass, with every safety check re-derived
// immediately before the irreversible call rather than trusted from
// earlier in the same pass.
//
// What differs from the artifact version is what a snapshot IS. There is
// no path to canonicalize, no root to prove containment beneath and no
// symlink to refuse, because a manifest id is not a path: it has no parent
// directory, nothing to traverse out of, and nothing on a disk that
// another process can swap. Inventing checks shaped like FR-20's would
// read as a safety proof while proving nothing.
//
// What replaces them is the catalog. A snapshot is deletable only if the
// journal says, at the moment of the delete, that this run succeeded, that
// this is the manifest it committed, that it belongs to the lineage being
// pruned, that it is not the restore point this set advertises, and that
// nobody has placed a hold on it. Every one of those is a row this product
// wrote, and every one of them is read again immediately before the
// engine is asked to remove anything.

// TierHold is the tier name a verdict carries when a hold is what kept the
// snapshot.
//
// It is a retention.GFSTier so that it renders and reads exactly like the
// tiers the shared classifier produces -- a preview showing DAILY,
// LAST_KNOWN_GOOD and HOLD in one list is showing three reasons of the
// same kind -- and it is defined here rather than in internal/retention
// because a hold is not part of FR-18's chain and artifacts do not have
// one.
//
// It is not RESERVED the way internal/retention reserves LAST_KNOWN_GOOD,
// and that is a deliberate non-decision. A deployment that has configured
// a GFS tier called `hold` renders as HOLD too, so a held snapshot inside
// that tier's window can show the name twice. Making the word reserved
// would refuse an existing config file at startup, for a collision whose
// entire consequence is a duplicated word beside a snapshot that is kept
// either way.
const TierHold retention.GFSTier = "HOLD"

// holdSelection is the single entry a hold ever contributes to a verdict's
// tier list. It is attributed to PROTECTION, the one member of that type
// that is not a placement: a hold is somebody's decision, not a calendar
// bucket, and dressing it as one would tell an operator this product made
// a scheduling decision it never made. FR-19's own term is attributed the
// same way, for the same reason.
var holdSelection = retention.GFSTierSelection{Tier: TierHold, By: retention.GFSSelectedByProtection}

// DefaultHistoryLimit is how many of a lineage's newest runs a pass reads
// when the caller names no bound.
//
// There is a bound at all because the catalog is append-only and an hourly
// schedule fills it faster than anything else in this journal, so an
// unbounded read grows with the deployment's whole history (see
// state.ListSnapshotRuns' own refusal of a zero limit). The consequence is
// stated rather than hidden: a snapshot older than this window is invisible
// to a pass and is therefore never deleted by one. That is the fail-safe
// direction -- more retention, not less -- and a deployment that wants
// those reached raises the limit rather than removing it.
const DefaultHistoryLimit = 1024

// Catalog is the durable half of a retention pass: the rows internal/state
// persists about runs and holds.
//
// It is an interface, satisfied by *state.Journal, for snapshotlifecycle's
// reason: every refusal this file makes has to be reachable in a test by
// writing a row, and a package that reached for a concrete journal would
// make each of them a database fixture instead of a value.
type Catalog interface {
	// ListSnapshotRuns is the lineage's history, newest first.
	ListSnapshotRuns(ctx context.Context, setUUID string, limit int) ([]state.SnapshotRun, error)

	// ActiveSnapshotHolds is every unreleased hold in the lineage. It is
	// read once for the plan and again before every single delete: a hold
	// placed while a long pass is running has to stop the deletes that
	// have not happened yet, which is the only thing still available to
	// decide.
	ActiveSnapshotHolds(ctx context.Context, setUUID string) ([]state.SnapshotHold, error)

	// LastKnownGoodSnapshot is the run this set advertises as its restore
	// point, or state.ErrSnapshotRunNotFound.
	LastKnownGoodSnapshot(ctx context.Context, setUUID string) (state.SnapshotRun, error)

	// GetSnapshotRun re-reads one row at the moment of the delete, rather
	// than trusting the copy the plan was drawn from.
	GetSnapshotRun(ctx context.Context, runID string) (state.SnapshotRun, error)

	// MarkSnapshotDeleteRequested records the durable INTENT, before the
	// repository is asked for anything.
	MarkSnapshotDeleteRequested(ctx context.Context, runID string, at time.Time) error

	// AdvanceSnapshotRun records the delete once it has happened.
	AdvanceSnapshotRun(ctx context.Context, runID string, to state.SnapshotPhase, upd state.SnapshotRunUpdate) error

	// ClearSnapshotDeleteRequested withdraws an intent a later pass has
	// decided against, so that a delete this product no longer means to
	// perform stops being reported as one it owes. See
	// withdrawStaleIntent.
	ClearSnapshotDeleteRequested(ctx context.Context, runID string, at time.Time) error
}

// Repository is everything retention may ask of an open repository, and
// its narrowness is a load-bearing part of issue #785 rather than
// minimalism for its own sake.
//
// Two methods: confirm that a manifest is there, and remove one manifest
// by identity. There is no Maintain, no Stats, no blob, pack or index
// surface, and no bulk or policy-shaped delete. Retention must never prune
// repository-internal storage -- only the engine's own maintenance (#786)
// reclaims that -- and the way to guarantee it is that nothing here can
// express it, not a comment asking future callers not to.
// TestRetentionsRepositoryPortCannotReachRepositoryStorage pins the method
// set so that widening this interface fails a test rather than a review.
type Repository interface {
	// LookupSnapshot answers whether the manifest this catalog recorded is
	// actually in the repository, which is the one thing a plan has to
	// know that the journal cannot tell it.
	LookupSnapshot(ctx context.Context, id backupengine.SnapshotID) (backupengine.SnapshotInfo, error)

	// DeleteSnapshot removes ONE manifest, named by identity. It does not
	// reclaim space and cannot be asked to.
	DeleteSnapshot(ctx context.Context, id backupengine.SnapshotID) error
}

// Action is a pass's verdict for one snapshot: what should happen, or did
// happen, to its manifest. The three values mean exactly what
// retention.PruneAction's three mean, deliberately, because they are read
// by people who have read that one.
type Action string

const (
	// Keep means a tier, a hold, or last-known-good protection selects
	// this snapshot: its manifest is never touched.
	Keep Action = "KEEP"

	// Delete means nothing selects it and every check passed. From
	// Decide, the manifest WOULD be removed; from Apply, it WAS.
	Delete Action = "DELETE"

	// Refuse means this snapshot was a delete candidate and something
	// stopped it: a check that did not pass, a set-wide hold, or a failure
	// during the pass itself. It is a third outcome and never folded into
	// Keep, because "policy says delete but it is not safe to" and "policy
	// says keep" are different facts and only one of them needs somebody
	// to look at it.
	Refuse Action = "REFUSE"
)

// Verdict is one snapshot's fully explained outcome.
type Verdict struct {
	// Run and Snapshot are the two identities this decision is about: the
	// catalog row, and the manifest in the repository. Both are carried
	// because a person investigating afterwards has one or the other, and
	// because the delete path re-reads the run to re-derive the manifest.
	Run      string
	Snapshot backupengine.SnapshotID

	Set    model.BackupSetID
	Action Action

	// StartedAt is when the run that produced this snapshot began, which
	// is the instant the classifier bucketed it by. It is here so a
	// preview can be read in time order without a second lookup.
	StartedAt time.Time

	// Tiers is every reason this snapshot survived, each paired with what
	// selected it: FR-18's tiers with their placement, FR-19's protected
	// term, and TierHold. Populated only on Keep, and copied from the
	// composed classifier verdict rather than recomputed, so it can never
	// disagree with the decision it explains.
	Tiers []retention.GFSTierSelection

	// Holds are the unreleased holds over this snapshot, whether or not
	// they are what kept it (a snapshot inside a tier's window can be held
	// too). Naming them is the point: "kept by a hold" is unactionable
	// without knowing which hold and who placed it.
	Holds []state.SnapshotHold

	// Reason is the sentence an operator reads: which tier kept it, or
	// that nothing did and every check passed, or exactly what refused it.
	Reason string

	// HoldReason is set on a Refuse, and only when the refusal is about
	// the whole backup SET rather than this snapshot: the restore point
	// this set advertises could not be confirmed in the repository, or the
	// catalog stopped being able to record what the pass was doing.
	//
	// It is a field rather than something a caller matches out of Reason
	// for internal/retention's reason: the layer above has to be able to
	// raise a standing condition without raising every ordinary refusal,
	// and matching on a sentence turns a wording change into an alerting
	// change.
	HoldReason string
}

// Pruner decides and, when asked, carries out one backup set's snapshot
// retention.
//
// It holds no clock. Both entry points take the instant explicitly, for
// internal/retention's determinism reason: the same inputs must produce
// the same plan whenever it is computed, and a package that reached for
// time.Now could not be asked twice.
type Pruner struct {
	// Catalog is where the decision is read from and written to. A pass
	// without one refuses rather than deciding from the repository, which
	// would be exactly the engine-side retention this product must not
	// have.
	Catalog Catalog

	// Repository is the engine. Decide needs it to confirm the restore
	// point exists; Apply needs it to remove a manifest.
	Repository Repository

	// HistoryLimit bounds how much of the lineage a pass reads. Zero means
	// DefaultHistoryLimit.
	HistoryLimit int
}

var (
	errNoCatalog    = errors.New("snapshotretention: no catalog was supplied; a retention pass that cannot read what this product recorded must not decide anything")
	errNoRepository = errors.New("snapshotretention: no repository was supplied; a pass cannot confirm a restore point it cannot look up")
)

func (p Pruner) preflight(bs config.BackupSet) error {
	switch {
	case p.Catalog == nil:
		return errNoCatalog
	case p.Repository == nil:
		return errNoRepository
	case bs.UUID == "":
		return fmt.Errorf("snapshotretention: backup set %s has no durable uuid, and a snapshot lineage is keyed by uuid rather than by names an operator can edit", bs.ID)
	case bs.ID.IsZero():
		return errors.New("snapshotretention: a retention pass needs the backup set it is deciding about")
	}
	return nil
}

func (p Pruner) historyLimit() int {
	if p.HistoryLimit > 0 {
		return p.HistoryLimit
	}
	return DefaultHistoryLimit
}

// Decide computes the KEEP/DELETE/REFUSE verdict for every snapshot this
// product has an opinion about in one backup set, and changes nothing.
//
// It is exactly, and only, what a preview renders, and it is also Apply's
// own first step, so a pass can never act on a decision a preview could
// not have shown. The policy it decides with is bs.Retention, which
// config.Validate resolved for this set (a per-set override where one is
// configured, the deployment's chain otherwise); it is read off the set
// rather than passed separately so that a caller cannot hand this function
// one set's history and another set's policy.
//
// The returned slice is ordered oldest snapshot first, which is both how a
// history reads and the order Apply deletes in: a pass interrupted halfway
// has removed the oldest snapshots rather than an arbitrary half.
func (p Pruner) Decide(ctx context.Context, now time.Time, bs config.BackupSet) ([]Verdict, error) {
	verdicts, _, err := p.plan(ctx, now, bs)
	return verdicts, err
}

// plan is Decide, plus the run rows the delete path needs, so that Apply
// does not read the lineage twice.
func (p Pruner) plan(ctx context.Context, now time.Time, bs config.BackupSet) ([]Verdict, map[string]state.SnapshotRun, error) {
	if err := p.preflight(bs); err != nil {
		return nil, nil, err
	}

	runs, err := p.Catalog.ListSnapshotRuns(ctx, bs.UUID, p.historyLimit())
	if err != nil {
		return nil, nil, fmt.Errorf("snapshotretention: reading %s's snapshot history: %w", bs.ID, err)
	}

	records, unclassifiable := Timeline(bs.ID, runs)

	// One classification, by the code that already owns retention policy.
	// This is the whole of this package's GFS, last-known-good, tier
	// window and calendar behaviour: there is no second implementation
	// here to disagree with it.
	keep, lkg, err := retention.DecideKeep(now, bs.Retention, bs.ID, records)
	if err != nil {
		return nil, nil, fmt.Errorf("snapshotretention: classifying %s's snapshots: %w", bs.ID, err)
	}

	holds, err := p.Catalog.ActiveSnapshotHolds(ctx, bs.UUID)
	if err != nil {
		return nil, nil, fmt.Errorf("snapshotretention: reading %s's holds: %w", bs.ID, err)
	}
	holdsByRun := map[string][]state.SnapshotHold{}
	for _, hold := range holds {
		holdsByRun[hold.RunID] = append(holdsByRun[hold.RunID], hold)
	}

	byManifest := map[string]state.SnapshotRun{}
	for _, run := range runs {
		if InScope(run) {
			byManifest[run.SnapshotID] = run
		}
	}

	keep = applyHolds(keep, byManifest, holdsByRun)

	var (
		out   []Verdict
		byRun = map[string]state.SnapshotRun{}
	)

	for _, v := range keep {
		run, ok := byManifest[v.Artifact.Name]
		if !ok {
			// Unreachable given that every record came from byManifest's
			// own source, and refused rather than assumed: a verdict about
			// a manifest this pass cannot tie back to a run is a verdict
			// with nothing to delete and nothing to record.
			out = append(out, Verdict{
				Snapshot: backupengine.SnapshotID(v.Artifact.Name), Set: bs.ID, Action: Refuse,
				Reason: fmt.Sprintf("refusing to decide about manifest %q: this pass has no catalog row for it", v.Artifact.Name),
			})
			continue
		}
		byRun[run.RunID] = run
		out = append(out, p.evaluate(bs, run, v, lkg, holdsByRun[run.RunID]))
	}

	for _, u := range unclassifiable {
		out = append(out, Verdict{
			Run: u.RunID, Snapshot: backupengine.SnapshotID(u.SnapshotID), Set: bs.ID,
			Action: Refuse, Reason: "refusing to decide: " + u.Why,
		})
	}

	// The set-wide guard, asked once for the plan and again before every
	// delete: see restorePointUnconfirmed.
	why, err := p.restorePointUnconfirmed(ctx, bs)
	if err != nil {
		return nil, nil, err
	}
	holdEveryDelete(out, why)

	sortVerdicts(out)
	return out, byRun, nil
}

// applyHolds composes the hold term into the classifier's verdicts, the
// way retention.ApplyLastKnownGood composes FR-19's protected term: a held
// snapshot's verdict gains TierHold beside whatever already kept it, and a
// held snapshot no tier kept flips to Keep with TierHold as the only
// reason.
//
// Composed into the verdict rather than filtered out of the delete list
// afterwards, and the difference is what a preview can say. A filter
// produces "this snapshot was not deleted"; this produces "this snapshot
// is kept by hold-1, placed by ops@example.com because incident 8812 is
// still open", which is the sentence somebody deciding whether to release
// it actually needs.
func applyHolds(verdicts []retention.GFSVerdict, byManifest map[string]state.SnapshotRun, holdsByRun map[string][]state.SnapshotHold) []retention.GFSVerdict {
	if len(holdsByRun) == 0 {
		return verdicts
	}

	out := make([]retention.GFSVerdict, len(verdicts))
	copy(out, verdicts)
	for i, v := range out {
		run, ok := byManifest[v.Artifact.Name]
		if !ok || len(holdsByRun[run.RunID]) == 0 {
			continue
		}

		out[i].Keep = true
		out[i].Tiers = append(append([]retention.GFSTierSelection(nil), v.Tiers...), holdSelection)

		// A collision is a note about which of two candidates lost a
		// tie-break for a bucket, and it is only ever populated on a
		// verdict that is not kept (see GFSVerdict.SiblingCollisions).
		// This one is kept now, so carrying it would report a delete
		// candidate's disambiguation on a snapshot nothing is deleting.
		out[i].SiblingCollisions = nil
	}
	return out
}

// evaluate is one snapshot's decision, given the composed verdict the
// classifier produced for it.
//
// Every refusal below is a contradiction between two things this product
// wrote, and each is checked here even though the composition above should
// have made it unreachable. That redundancy is the same one
// internal/retention's pruneEvaluate keeps and for the same reason: a
// check worth having is worth re-running at the point of the dangerous
// action, and this function's output is what authorizes a delete.
func (p Pruner) evaluate(
	bs config.BackupSet,
	run state.SnapshotRun,
	keep retention.GFSVerdict,
	lkg retention.LastKnownGoodResult,
	holds []state.SnapshotHold,
) Verdict {
	v := Verdict{
		Run:       run.RunID,
		Snapshot:  backupengine.SnapshotID(run.SnapshotID),
		Set:       bs.ID,
		StartedAt: run.StartedAt,
		Holds:     holds,
	}

	if keep.Keep {
		v.Action = Keep
		v.Tiers = append([]retention.GFSTierSelection(nil), keep.Tiers...)
		v.Reason = keepReason(keep.Tiers, holds)
		return v
	}

	v.Action = Refuse
	switch {
	case len(holds) > 0:
		v.Reason = fmt.Sprintf(
			"refusing to delete snapshot %s: %d hold(s) are recorded against run %s, but the classification passed in claims it is not kept; that contradiction means mismatched inputs, not a delete candidate",
			run.SnapshotID, len(holds), run.RunID)
		return v

	case lkg.Protected && lkg.Artifact.Name == run.SnapshotID:
		v.Reason = fmt.Sprintf(
			"refusing to delete snapshot %s: it holds last-known-good protection, but the classification passed in claims it is not kept; that contradiction means mismatched inputs, not a delete candidate",
			run.SnapshotID)
		return v

	case run.LastKnownGood && lkg.Enabled:
		// The catalog's own durable flag, which is a different fact from
		// the classifier's computed protection: the flag says which
		// snapshot this product ADVERTISES as its restore point right now,
		// and it moves only when a run reaches SUCCESS or reconciliation
		// re-points it. A pass that deleted the advertised restore point
		// because a calculation disagreed with the row would leave every
		// caller asking "what may I restore" holding a manifest id that
		// resolves to nothing.
		//
		// Gated on the resolved protect_last_known_good reading for
		// restorePointUnconfirmed's reason: the column is set on EVERY
		// successful run, so an operator who explicitly turned protection
		// off would otherwise find the newest snapshot in the set refused
		// for ever, by a flag they had said out loud they did not want
		// honoured.
		v.Reason = fmt.Sprintf(
			"refusing to delete snapshot %s: the catalog marks run %s as this set's last-known-good restore point, whatever the retention calculation concluded",
			run.SnapshotID, run.RunID)
		return v

	case !InScope(run):
		v.Reason = fmt.Sprintf(
			"refusing to delete snapshot %s: run %s is at %s with verification %q, which is not a completed, verified backup this pass may decide about",
			run.SnapshotID, run.RunID, run.Phase, run.VerificationStatus)
		return v

	case run.SetUUID != bs.UUID:
		v.Reason = fmt.Sprintf(
			"refusing to delete snapshot %s: run %s belongs to lineage %s, and this pass is pruning lineage %s",
			run.SnapshotID, run.RunID, run.SetUUID, bs.UUID)
		return v
	}

	v.Action = Delete
	v.Reason = fmt.Sprintf(
		"no configured retention tier selects snapshot %s, it holds no last-known-good protection and no hold is recorded against it; "+
			"run %s is a completed, verified backup of this lineage, so its manifest may be removed. "+
			"Removing a manifest does not reclaim its storage: content nothing references any more is reclaimed by repository maintenance, separately",
		run.SnapshotID, run.RunID)
	return v
}

// keepReason names every reason a snapshot survived, in the order they
// were composed, because "kept by DAILY" and "kept by DAILY and a hold"
// call for different actions from whoever is reading.
func keepReason(tiers []retention.GFSTierSelection, holds []state.SnapshotHold) string {
	if len(tiers) == 0 {
		// Unreachable from the composition above, and written out rather
		// than left as an empty sentence: a KEEP that names no reason is
		// indistinguishable from a bug that kept everything.
		return "kept, but this pass recorded no reason, which is a bug in the decision that produced it"
	}

	reason := "kept by "
	for i, t := range tiers {
		if i > 0 {
			reason += ", "
		}
		reason += t.String()
	}
	for _, hold := range holds {
		reason += fmt.Sprintf("; hold %s placed by %s at %s: %s",
			hold.HoldID, hold.PlacedBy, hold.PlacedAt.UTC().Format(time.RFC3339), hold.Reason)
	}
	return reason
}

// restorePointUnconfirmed asks the one question the journal cannot answer
// on its own: is the snapshot this backup set advertises as its restore
// point actually in the repository?
//
// It returns the empty string when it is, and one sentence naming what is
// wrong when it is not. That sentence stops the whole pass, not the
// snapshot it is about, and the difference is the point: the fact is about
// the SET, and a pass that carried on would delete exactly the snapshots
// that were still restorable. It is internal/retention's own FR-30 guard
// (pruneLastKnownGoodUnconfirmed), ported to the thing a snapshot's
// readable copy actually is -- a manifest the engine can resolve.
//
// It reads and never writes, so calling it once for the plan and again
// before every delete costs one lookup per deletion and no correctness.
// Apply's own doc says why it is asked that often.
//
// An operator who has explicitly set protect_last_known_good to false has
// said out loud that retention may empty this backup set, and this guard
// has nothing to say about it -- overriding that would silently stop a
// documented configuration working the first time a manifest went missing.
func (p Pruner) restorePointUnconfirmed(ctx context.Context, bs config.BackupSet) (string, error) {
	if !protectsLastKnownGood(bs) {
		return "", nil
	}

	lkg, err := p.Catalog.LastKnownGoodSnapshot(ctx, bs.UUID)
	switch {
	case errors.Is(err, state.ErrSnapshotRunNotFound):
		return fmt.Sprintf(
			"backup set %s advertises no last-known-good snapshot at all: either nothing has ever succeeded, or the run that held the flag lost its snapshot and reconciliation has not re-pointed it yet",
			bs.ID), nil
	case err != nil:
		return "", fmt.Errorf("snapshotretention: reading %s's restore point: %w", bs.ID, err)
	case lkg.SnapshotID == "":
		return fmt.Sprintf(
			"backup set %s's last-known-good run %s carries no manifest id, so the restore point it names cannot be resolved",
			bs.ID, lkg.RunID), nil
	}

	switch _, err := p.Repository.LookupSnapshot(ctx, backupengine.SnapshotID(lkg.SnapshotID)); {
	case errors.Is(err, backupengine.ErrSnapshotNotFound):
		return fmt.Sprintf(
			"backup set %s's last-known-good snapshot %s (run %s) is not in the repository",
			bs.ID, lkg.SnapshotID, lkg.RunID), nil
	case err != nil:
		// Not confirmed is not the same as confirmed absent, and neither
		// is a reason to delete anything. A repository this pass cannot
		// question is one it must not act against.
		return fmt.Sprintf(
			"backup set %s's last-known-good snapshot %s could not be confirmed in the repository: %v",
			bs.ID, lkg.SnapshotID, err), nil
	}
	return "", nil
}

// protectsLastKnownGood is the resolved protect_last_known_good reading
// for one backup set: absent means protect, and only an explicit false
// turns it off.
//
// It is spelled once because three places need the same answer -- the
// set-wide restore-point guard, the per-snapshot refusal in evaluate and
// the re-derivation in refusesNow -- and three copies of "nil means true"
// is three chances for one of them to read an explicit false as
// protection. It matches retention.LastKnownGoodDecide's own reading of
// the same field, which is what LastKnownGoodResult.Enabled reports.
func protectsLastKnownGood(bs config.BackupSet) bool {
	return bs.Retention.ProtectLastKnownGood == nil || *bs.Retention.ProtectLastKnownGood
}

// holdEveryDelete turns every Delete in verdicts into a Refuse naming why,
// or does nothing at all when why is empty.
//
// Every delete, not the ones that look related, for the reason
// restorePointUnconfirmed gives. Apply hands it the TAIL of its own slice
// rather than the whole of it: a pass that has already removed something
// and then finds the restore point gone stops there, and rewriting the
// verdicts of manifests it has already deleted would report a deletion
// that happened as one that was refused.
func holdEveryDelete(verdicts []Verdict, why string) {
	if why == "" {
		return
	}
	for i := range verdicts {
		if verdicts[i].Action != Delete {
			continue
		}
		verdicts[i].Action = Refuse
		verdicts[i].HoldReason = why
		verdicts[i].Reason = fmt.Sprintf(
			"refusing to delete snapshot %s: %s. Deleting anything in this backup set now would remove a restore point while the one it advertises is not one, "+
				"so nothing here is deleted until reconciliation settles what this set actually holds",
			verdicts[i].Snapshot, why)
	}
}

// Apply computes Decide's own plan and removes the manifest behind every
// Delete verdict in it, oldest snapshot first.
//
// Its first act is calling the same decision path a preview calls, with
// the same arguments, so every Keep and Refuse it returns is one a preview
// would have shown. What it adds is a second, independent proof
// immediately before each irreversible call:
//
//   - the run row is read again, from the catalog, and every condition
//     that made it a candidate is re-derived from what it says NOW rather
//     than from what it said when the plan was drawn;
//   - the lineage's holds are read again, so a hold placed while a long
//     pass was running stops the deletes that have not happened yet;
//   - the set's restore point is confirmed again, and a pass that finds it
//     missing partway through stops there rather than removing the
//     snapshots that are still restorable.
//
// The two durable writes are ordered and the order is the crash story. The
// intent is recorded BEFORE the repository is asked, so a run with an
// intent whose manifest is gone reads as a delete that completed, and a
// run with no intent whose manifest is gone reads as a loss to
// investigate. Reversed, this product would raise an incident about a
// deletion it performed on purpose. A catalog that cannot record the
// intent therefore stops the delete: the repository is never asked.
//
// A pass also withdraws a delete intent it has decided against: a run this
// pass KEEPS whose row still carries the intent of an earlier one has that
// intent cleared, so that a delete this product no longer means to perform
// stops being reported as one it owes. See withdrawStaleIntent.
func (p Pruner) Apply(ctx context.Context, now time.Time, bs config.BackupSet) ([]Verdict, error) {
	verdicts, byRun, err := p.plan(ctx, now, bs)
	if err != nil {
		return nil, err
	}

	for i := range verdicts {
		if verdicts[i].Action != Delete {
			p.withdrawStaleIntent(ctx, now, byRun[verdicts[i].Run], &verdicts[i])
			continue
		}

		// The plan is the reviewed decision; the evidence is fresh. See
		// this function's doc for why each of these is asked per delete
		// rather than once for the pass.
		if why, err := p.restorePointUnconfirmed(ctx, bs); err != nil {
			return verdicts, err
		} else if why != "" {
			holdEveryDelete(verdicts[i:], why)
			continue
		}

		if why := p.refusesNow(ctx, bs, byRun[verdicts[i].Run], verdicts[i]); why != "" {
			verdicts[i].Action = Refuse
			verdicts[i].Reason = why
			continue
		}

		if err := p.Catalog.MarkSnapshotDeleteRequested(ctx, verdicts[i].Run, now); err != nil {
			verdicts[i].Action = Refuse
			verdicts[i].Reason = fmt.Sprintf(
				"refusing to delete snapshot %s: the catalog could not record the delete intent that has to be durable before the repository is asked (%v)",
				verdicts[i].Snapshot, err)
			continue
		}

		deleted := true
		switch err := p.Repository.DeleteSnapshot(ctx, verdicts[i].Snapshot); {
		case errors.Is(err, backupengine.ErrSnapshotNotFound):
			// The intent is recorded and the manifest is not there. That
			// is this delete, already done -- by a previous pass that died
			// between the two writes, most likely -- and recording it as
			// anything else would leave a row nothing can ever resolve.
			verdicts[i].Reason = fmt.Sprintf(
				"snapshot %s was already absent from the repository when this pass reached it; the delete this product intended is complete",
				verdicts[i].Snapshot)
		case err != nil:
			deleted = false
			verdicts[i].Action = Refuse
			verdicts[i].Reason = fmt.Sprintf("the repository refused to delete snapshot %s: %v", verdicts[i].Snapshot, err)
		}
		if !deleted {
			continue
		}

		reason := verdicts[i].Reason
		if err := p.Catalog.AdvanceSnapshotRun(ctx, verdicts[i].Run, state.PhaseDeleted, state.SnapshotRunUpdate{
			Reason: &reason,
			At:     now,
		}); err != nil {
			// The manifest is gone and the catalog does not say so. The
			// delete is reported as what it is -- it happened -- and the
			// rest of the pass is held: a catalog that cannot record what
			// this pass is doing is a catalog that cannot make the next
			// delete decidable after a crash either.
			verdicts[i].Reason += fmt.Sprintf(
				"; the catalog could not record the deletion (%v), so this run's row still says %s and reconciliation will have to settle it",
				err, state.PhaseSuccess)
			holdEveryDelete(verdicts[i+1:], fmt.Sprintf(
				"the catalog stopped recording this pass's deletions (%v), and a delete this product cannot write down is one it must not perform again", err))
		}
	}

	return verdicts, nil
}

// withdrawStaleIntent clears a delete intent left on a run whose snapshot
// this pass has just decided to KEEP.
//
// An intent is durable because a crash between recording it and the
// manifest going has to be decidable afterwards, and it is read as a
// standing statement: snapshotlifecycle reports a run that carries one
// while its manifest is still present as VerdictDeletePending, every
// cycle. That is correct while the decision behind it stands. It stops
// being correct the moment a later pass keeps the same snapshot -- an
// operator widened a window, somebody placed a hold, a delete the
// repository refused is now one this product no longer wants -- and what
// is left is a permanent alarm about a deletion that is never coming,
// indistinguishable from one that is.
//
// Only a KEEP withdraws it. A REFUSE means the delete did not happen THIS
// time -- the repository denied it, the restore point could not be
// confirmed, the row moved under the pass -- and the intent still
// describes what this product means to do.
//
// A failure to withdraw is recorded on the verdict and never fails the
// pass: nothing was deleted, and a stale intent is a false alarm rather
// than a danger.
func (p Pruner) withdrawStaleIntent(ctx context.Context, now time.Time, run state.SnapshotRun, v *Verdict) {
	if v.Action != Keep || run.DeleteRequestedAt == nil {
		return
	}

	if err := p.Catalog.ClearSnapshotDeleteRequested(ctx, v.Run, now); err != nil {
		v.Reason += fmt.Sprintf(
			"; a delete intent recorded at %s is still on run %s and could not be withdrawn (%v), so reconciliation will keep reporting a delete this pass has decided against",
			run.DeleteRequestedAt.UTC().Format(time.RFC3339), v.Run, err)
	}
}

// refusesNow re-derives, against a freshly read row, every condition that
// made this snapshot a delete candidate, and returns the sentence that
// refuses it or the empty string.
//
// It re-reads rather than re-checking the copy in hand because deleting
// other snapshots in the same pass takes real time, and everything it asks
// about is a row another process can change: a run can be re-pointed as
// the restore point, a hold can be placed, a reconciliation can move a
// phase.
func (p Pruner) refusesNow(ctx context.Context, bs config.BackupSet, planned state.SnapshotRun, v Verdict) string {
	run, err := p.Catalog.GetSnapshotRun(ctx, v.Run)
	if err != nil {
		return fmt.Sprintf("refusing to delete snapshot %s: its catalog row could not be re-read immediately before the delete (%v)", v.Snapshot, err)
	}

	switch {
	case run.SnapshotID != string(v.Snapshot):
		return fmt.Sprintf(
			"refusing to delete snapshot %s: run %s now records manifest %q, so the plan and the catalog no longer agree about what would be deleted",
			v.Snapshot, run.RunID, run.SnapshotID)
	case run.SetUUID != bs.UUID:
		return fmt.Sprintf("refusing to delete snapshot %s: run %s belongs to lineage %s, not %s", v.Snapshot, run.RunID, run.SetUUID, bs.UUID)
	case !InScope(run):
		return fmt.Sprintf(
			"refusing to delete snapshot %s: run %s is now at %s with verification %q, which is not something this pass may delete",
			v.Snapshot, run.RunID, run.Phase, run.VerificationStatus)
	case run.LastKnownGood && protectsLastKnownGood(bs):
		return fmt.Sprintf("refusing to delete snapshot %s: the catalog now marks run %s as this set's last-known-good restore point", v.Snapshot, run.RunID)
	case !planned.StartedAt.IsZero() && !run.StartedAt.Equal(planned.StartedAt):
		return fmt.Sprintf(
			"refusing to delete snapshot %s: run %s started at %s, and the plan was drawn against a row that started at %s",
			v.Snapshot, run.RunID, run.StartedAt.UTC().Format(time.RFC3339), planned.StartedAt.UTC().Format(time.RFC3339))
	}

	holds, err := p.Catalog.ActiveSnapshotHolds(ctx, bs.UUID)
	if err != nil {
		return fmt.Sprintf("refusing to delete snapshot %s: this set's holds could not be re-read immediately before the delete (%v)", v.Snapshot, err)
	}
	for _, hold := range holds {
		if hold.RunID == run.RunID {
			return fmt.Sprintf(
				"refusing to delete snapshot %s: hold %s was placed against run %s by %s (%s)",
				v.Snapshot, hold.HoldID, run.RunID, hold.PlacedBy, hold.Reason)
		}
	}
	return ""
}

// sortVerdicts orders a plan oldest snapshot first, breaking ties on the
// manifest id so two passes over the same history render identically
// whatever order the catalog handed the rows back in.
func sortVerdicts(out []Verdict) {
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].Snapshot < out[j].Snapshot
	})
}
