package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/config"
	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/snapshotretention"
	"github.com/retnd/retnd/core/internal/state"
)

// The READ side of EPIC K, plus the two writes that are not a backup:
// what this deployment's snapshot catalog holds, what retention would do
// about it, and the holds that stop retention doing it (#788).
//
// # Why it is here and not in core/service
//
// Everything in this file needs the loaded configuration to turn a
// source/set pair into a backup set, and half of it needs a repository
// opened at the location that configuration derives. Both already live
// here (snapshotcycle.go's repositoryLocation, snapshotrestore.go's
// incrementalBackupSet), and a second copy in the layer above would be a
// second answer to where a repository is.
//
// # What is NOT here
//
// No new engine behaviour. Every decision below is reached by code that
// already existed and already has its own tests: snapshotretention's
// Pruner decides retention, state's journal decides what a hold may
// protect, and the engine's own Health decides whether a repository
// answers. This file finds the inputs and hands them over.

// maxSnapshotListing bounds how much of a lineage one read returns.
//
// A lineage grows one row per run for the life of the deployment, so an
// unbounded read is an unbounded response on a page somebody refreshes.
// Two hundred is several months of nightly runs, which is more history
// than any surface renders at once and enough that a retention preview
// covering the same window is drawn from the same rows.
const maxSnapshotListing = 200

// ErrSnapshotNotFound is what a read or a hold reports for a run id this
// backup set's lineage does not contain.
//
// It is distinct from state.ErrSnapshotRunNotFound, and the difference is
// the one that matters: that one means no run anywhere has this id, and
// this one also covers a run that exists and belongs to a DIFFERENT
// backup set. Answering the second with the row would let a caller read,
// and hold, another set's snapshot through a path that named their own.
var ErrSnapshotNotFound = errors.New("app: no snapshot of that id belongs to this backup set")

// ErrSnapshotNotHoldable is what a hold reports for a run that has no
// snapshot to protect: one that committed no manifest, one whose
// snapshot this product already deleted, one already being deleted, and
// one at LOST.
//
// It exists because the journal's four refusals used to arrive here
// unclassified, and an unclassified error is what core/service turns
// into "an internal error occurred" and the API into a 500. Holding a
// FAILED run is not a malfunction; it is a button on a screen that has
// moved on, and 500 is the one answer that tells an operator to report a
// bug instead of refreshing the page.
//
// The sentence this carries is composed HERE rather than passed through,
// because the journal's own wording names the run id and the durable
// phase and is written for a log rather than for a caller. The rule is
// the one every refusal crossing this boundary follows: a message the
// layers above may echo carries no path and no storage-layer text.
var ErrSnapshotNotHoldable = errors.New("app: this snapshot cannot be held")

// SnapshotNotHoldable is ErrSnapshotNotHoldable with the reason attached,
// so the layer above reads WHICH state this is out of a field rather
// than out of a sentence.
//
// A type rather than four sentinels, and a field rather than prose
// parsing, because core/service has to put this reason in front of an
// operator and must not be re-deriving it: a reason recovered by
// splitting a message on a colon is a reason that changes the first time
// somebody rewords the message.
type SnapshotNotHoldable struct {
	// Reason is the operator-facing sentence, composed by
	// notHoldableReason. It carries no path and no storage-layer text.
	Reason string
}

func (e *SnapshotNotHoldable) Error() string {
	return ErrSnapshotNotHoldable.Error() + ": " + e.Reason
}

// Unwrap is what makes errors.Is(err, ErrSnapshotNotHoldable) true, so a
// caller that only needs the class never has to know this type exists.
func (e *SnapshotNotHoldable) Unwrap() error { return ErrSnapshotNotHoldable }

// snapshotCatalog is the durable surface this file reads and writes.
//
// It is an interface asserted off the Journal for ErrNoSnapshotCatalog's
// reason (snapshotcycle.go): Journal is implemented by a dozen test
// doubles across this repository, and widening it would make every one of
// them grow methods for a feature they have no business in.
type snapshotCatalog interface {
	ListSnapshotRuns(ctx context.Context, setUUID string, limit int) ([]state.SnapshotRun, error)
	GetSnapshotRun(ctx context.Context, runID string) (state.SnapshotRun, error)
	SnapshotRunTransitions(ctx context.Context, runID string) ([]state.SnapshotRunTransition, error)
	LastKnownGoodSnapshot(ctx context.Context, setUUID string) (state.SnapshotRun, error)
	ActiveSnapshotHolds(ctx context.Context, setUUID string) ([]state.SnapshotHold, error)
	SnapshotHolds(ctx context.Context, runID string) ([]state.SnapshotHold, error)
	PlaceSnapshotHold(ctx context.Context, req state.SnapshotHoldRequest) (state.SnapshotHold, error)
	ReleaseSnapshotHold(ctx context.Context, holdID string, at time.Time, by string) error
}

// SnapshotRecord is one run plus the holds over it, which is the unit
// every surface renders.
//
// The holds are attached here rather than fetched separately because the
// question "may this be deleted" is never asked without them, and a
// client that had to make a second call per row would either make N of
// them or render a delete control that is wrong.
type SnapshotRecord struct {
	Run   state.SnapshotRun
	Holds []state.SnapshotHold
}

// SnapshotDetail is one run in full: the record, and how it got to the
// phase it is in.
type SnapshotDetail struct {
	SnapshotRecord
	Transitions []state.SnapshotRunTransition
}

// ListSnapshots is one incremental backup set's snapshot history, newest
// first, with each run's unreleased holds attached.
func (s *Service) ListSnapshots(ctx context.Context, sourceName, setName string) ([]SnapshotRecord, error) {
	bs, catalog, err := s.snapshotSurface(sourceName, setName)
	if err != nil {
		return nil, err
	}

	runs, err := catalog.ListSnapshotRuns(ctx, snapshotLineage(bs), maxSnapshotListing)
	if err != nil {
		return nil, fmt.Errorf("reading %s's snapshot history: %w", bs.ID, err)
	}

	holds, err := catalog.ActiveSnapshotHolds(ctx, snapshotLineage(bs))
	if err != nil {
		return nil, fmt.Errorf("reading %s's snapshot holds: %w", bs.ID, err)
	}

	byRun := make(map[string][]state.SnapshotHold, len(holds))
	for _, h := range holds {
		byRun[h.RunID] = append(byRun[h.RunID], h)
	}

	out := make([]SnapshotRecord, 0, len(runs))
	for _, run := range runs {
		out = append(out, SnapshotRecord{Run: run, Holds: byRun[run.RunID]})
	}

	return out, nil
}

// GetSnapshot is one run of one backup set, with its holds and its
// transition log.
//
// The lineage check is not a formality. A run id is opaque and globally
// unique, so a caller holding one from anywhere could otherwise read it
// through any set's path; refusing a run whose lineage is not this set's
// keeps the resource's identity honest.
func (s *Service) GetSnapshot(ctx context.Context, sourceName, setName, runID string) (SnapshotDetail, error) {
	bs, catalog, err := s.snapshotSurface(sourceName, setName)
	if err != nil {
		return SnapshotDetail{}, err
	}

	run, err := s.snapshotRunOf(ctx, catalog, bs, runID)
	if err != nil {
		return SnapshotDetail{}, err
	}

	holds, err := catalog.SnapshotHolds(ctx, run.RunID)
	if err != nil {
		return SnapshotDetail{}, fmt.Errorf("reading holds over snapshot %s: %w", run.RunID, err)
	}

	transitions, err := catalog.SnapshotRunTransitions(ctx, run.RunID)
	if err != nil {
		return SnapshotDetail{}, fmt.Errorf("reading the history of snapshot %s: %w", run.RunID, err)
	}

	active := make([]state.SnapshotHold, 0, len(holds))
	for _, h := range holds {
		if h.Active() {
			active = append(active, h)
		}
	}

	return SnapshotDetail{
		SnapshotRecord: SnapshotRecord{Run: run, Holds: active},
		Transitions:    transitions,
	}, nil
}

// ListSnapshotHolds is every unreleased hold in one backup set's lineage.
func (s *Service) ListSnapshotHolds(ctx context.Context, sourceName, setName string) ([]state.SnapshotHold, error) {
	bs, catalog, err := s.snapshotSurface(sourceName, setName)
	if err != nil {
		return nil, err
	}

	holds, err := catalog.ActiveSnapshotHolds(ctx, snapshotLineage(bs))
	if err != nil {
		return nil, fmt.Errorf("reading %s's snapshot holds: %w", bs.ID, err)
	}

	return holds, nil
}

// PlaceSnapshotHoldRequest is one hold, as an operator states it.
type PlaceSnapshotHoldRequest struct {
	SourceName string
	SetName    string

	// RunID is the run whose snapshot is held. Empty means this set's
	// last-known-good snapshot, which is the one an operator protecting
	// "the current restore point" means and the one they would otherwise
	// have to look up first.
	RunID string

	// HoldID is the caller's identifier, which is what makes a
	// resubmitted request resolve to the hold that already exists rather
	// than stacking a second one over the same decision.
	HoldID string

	Reason   string
	PlacedBy string
	At       time.Time
}

// PlaceSnapshotHold records that one snapshot must not be deleted.
//
// Every refusal is DECIDED by the journal: a run with no committed
// manifest, one already deleted, one whose delete intent is durable and
// one at LOST are all refused there, inside the same transaction that
// would otherwise insert the row (state.PlaceSnapshotHold). Nothing is
// re-decided here, because a second opinion about what a hold can
// protect is a second answer, and a check made outside that transaction
// would be a check a concurrent delete can walk past.
//
// What IS done here is classifying that decision. The journal's four
// refusals are one class -- there is no snapshot here to protect -- and
// they used to leave this layer as unclassified errors, which
// core/service reads as "an internal error occurred" and the API answers
// 500 for. The sentence is composed from the run this call already
// holds, so what crosses the boundary names the state rather than a run
// id and a durable phase.
func (s *Service) PlaceSnapshotHold(ctx context.Context, req PlaceSnapshotHoldRequest) (state.SnapshotHold, error) {
	bs, catalog, err := s.snapshotSurface(req.SourceName, req.SetName)
	if err != nil {
		return state.SnapshotHold{}, err
	}

	run, err := s.snapshotRunOf(ctx, catalog, bs, req.RunID)
	if err != nil {
		return state.SnapshotHold{}, err
	}

	at := req.At
	if at.IsZero() {
		at = s.now()
	}

	hold, err := catalog.PlaceSnapshotHold(ctx, state.SnapshotHoldRequest{
		HoldID:   req.HoldID,
		RunID:    run.RunID,
		Reason:   req.Reason,
		PlacedBy: req.PlacedBy,
		At:       at,
	})
	if errors.Is(err, state.ErrSnapshotNotHoldable) {
		return state.SnapshotHold{}, &SnapshotNotHoldable{Reason: notHoldableReason(run)}
	}

	return hold, err
}

// notHoldableReason says which of the four states this run is in, in
// words a caller may put in front of an operator.
//
// The order is state.PlaceSnapshotHold's own, so the sentence names the
// same fact that transaction refused on. It is read off the run this
// call already fetched rather than parsed out of the journal's message:
// a reason derived from prose is a reason that changes when somebody
// rewords a log line.
//
// The default arm is not dead code and is not a guess either. The
// journal decides inside its own transaction, so a delete that lands
// between this call's read and that decision is refused on a fact this
// run copy does not show -- and "somebody is deleting it right now" is
// exactly what an operator should be told in that case.
func notHoldableReason(run state.SnapshotRun) string {
	switch {
	case run.SnapshotID == "":
		return "this run committed no manifest, so there is no snapshot for a hold to protect. Hold a run that reached SUCCESS instead"
	case run.Phase == state.PhaseDeleted:
		return "this snapshot has already been removed from the repository, and a hold cannot bring one back"
	case run.Phase == state.PhaseLost:
		return "this snapshot is not in the repository, so a hold on it would protect nothing; reconciliation records this when a manifest has gone without a delete ever being recorded"
	case run.DeleteRequestedAt != nil:
		return "this snapshot is already being deleted, so a hold accepted now would protect nothing"
	default:
		return "this snapshot stopped being holdable while the request was in flight, which is what a delete landing at the same moment looks like; re-read this set's snapshots and hold one that is still a restore point"
	}
}

// ReleaseSnapshotHold ends one hold.
//
// The hold is checked against this backup set's lineage before it is
// released, for GetSnapshot's reason: a hold id is opaque, and releasing
// another set's protection through a path that named this one is exactly
// the mistake an audit asks about afterwards.
func (s *Service) ReleaseSnapshotHold(ctx context.Context, sourceName, setName, holdID, releasedBy string, at time.Time) error {
	bs, catalog, err := s.snapshotSurface(sourceName, setName)
	if err != nil {
		return err
	}

	holds, err := catalog.ActiveSnapshotHolds(ctx, snapshotLineage(bs))
	if err != nil {
		return fmt.Errorf("reading %s's snapshot holds: %w", bs.ID, err)
	}

	found := false
	for _, h := range holds {
		if h.HoldID == holdID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: %s holds no active hold %q", state.ErrSnapshotHoldNotFound, bs.ID, holdID)
	}

	if at.IsZero() {
		at = s.now()
	}

	return catalog.ReleaseSnapshotHold(ctx, holdID, at, releasedBy)
}

// SnapshotRetentionPreview is what snapshot retention would decide about
// every one of this set's snapshots right now, oldest first.
//
// It calls Decide and never Apply, and the difference is the whole
// resource: this is a preview an operator reads, and the deletion pass is
// the engine's own, run on its own schedule. A read that could delete
// would be a GET that deletes.
func (s *Service) SnapshotRetentionPreview(ctx context.Context, sourceName, setName string) ([]snapshotretention.Verdict, error) {
	bs, _, err := s.snapshotSurface(sourceName, setName)
	if err != nil {
		return nil, err
	}

	catalog, ok := s.Journal.(snapshotretention.Catalog)
	if !ok {
		return nil, ErrNoSnapshotCatalog
	}

	repo, closeRepo, err := s.openSetRepository(ctx, bs)
	if err != nil {
		return nil, err
	}
	defer closeRepo()

	pruner := snapshotretention.Pruner{Catalog: catalog, Repository: repo}

	return pruner.Decide(ctx, s.now(), bs)
}

// VerifySnapshotRequest is one on-demand verification.
type VerifySnapshotRequest struct {
	SourceName string
	SetName    string

	// RunID selects the run to verify. Empty means this set's
	// last-known-good snapshot, which is the restore point an operator
	// asking "can I actually restore" means.
	RunID string

	// Level is how deep to look. Empty means the level the backup set is
	// configured for, which is the claim the set already makes about
	// itself; an on-demand check that quietly did less would report a
	// weaker verification as though it were the configured one.
	Level model.VerificationLevel

	// SamplePercent overrides the configured sample size for this one
	// check. Zero means the configured one.
	SamplePercent int
}

// VerifySnapshotResult is what an on-demand verification actually did.
//
// Achieved is read off the engine's report and never from the request:
// the whole value of the ladder is that a claim of restore_drill means a
// restore happened.
type VerifySnapshotResult struct {
	RunID      string
	SnapshotID string

	// Requested is the depth asked for and Achieved is what the engine
	// says it performed. They are two fields because they are two claims.
	Requested model.VerificationLevel
	Achieved  model.VerificationLevel

	Report   backupengine.VerifyReport
	Duration time.Duration

	// Passed is whether the verification completed and found nothing
	// wrong, which is the one question the surface above asks. A
	// verification torn down by a cancelled context is not a pass and is
	// not damage; Err carries which.
	Passed bool
	Err    string
}

// VerifySnapshot proves, now, that a stored snapshot is what the catalog
// says it is.
//
// # Why nothing is written back onto the run
//
// A run's verification_level_achieved is what THAT RUN proved, on the
// night it ran. An on-demand check months later is a different claim
// about a different moment, and writing it onto the row would make a
// snapshot that was never verified at the time indistinguishable from one
// that was. The result is reported on the operation that performed it,
// which is the record of when somebody asked.
func (s *Service) VerifySnapshot(ctx context.Context, req VerifySnapshotRequest) (result VerifySnapshotResult, err error) {
	bs, catalog, err := s.snapshotSurface(req.SourceName, req.SetName)
	if err != nil {
		return VerifySnapshotResult{}, err
	}

	run, err := s.snapshotRunOf(ctx, catalog, bs, req.RunID)
	if err != nil {
		return VerifySnapshotResult{}, err
	}

	if run.SnapshotID == "" {
		return VerifySnapshotResult{}, fmt.Errorf("%w: snapshot run %s committed no manifest, so there is nothing to verify", ErrSetHasNoSnapshots, run.RunID)
	}

	level := req.Level
	if level == "" {
		level = bs.VerificationLevel
	}
	if level == "" {
		level = model.DefaultVerificationLevel
	}

	sample := req.SamplePercent
	if sample == 0 {
		sample = bs.VerificationSamplePercentConfig
	}

	opts := s.verificationOptions(bs)
	target := ""
	if level == model.LevelRestoreDrill {
		if opts.DrillDir == "" {
			return VerifySnapshotResult{}, ErrRepositoryStorageUnsupported
		}

		target, err = drillDirectoryFor(opts.DrillDir, run.RunID)
		if err != nil {
			return VerifySnapshotResult{}, fmt.Errorf("preparing a directory for this drill to restore into: %w", err)
		}
	}

	repo, closeRepo, err := s.openSetRepository(ctx, bs)
	if err != nil {
		return VerifySnapshotResult{}, err
	}
	defer closeRepo()

	started := s.now()
	report, verr := repo.Verify(ctx, backupengine.SnapshotID(run.SnapshotID), backupengine.VerifyRequest{
		Level:         level,
		SamplePercent: sample,
		RestoreTarget: target,
	})

	// The restored tree goes, and only on success. Both halves are
	// snapshotlifecycle.discardDrillOutput's reasoning applied to the
	// on-demand drill: a passing drill has proved what it was for and
	// what is left is a full second copy of somebody's backup on storage
	// they pay for, while a FAILING one has left the only available
	// evidence of what restoring this snapshot actually produces.
	//
	// The removal's own failure is not returned. The verification is
	// already decided by this point, and failing a proven backup over a
	// directory that would not unlink would send an operator to
	// investigate a restore point that is fine.
	if verr == nil && target != "" {
		_ = os.RemoveAll(target) //nolint:errcheck // see above.
	}

	result = VerifySnapshotResult{
		RunID:      run.RunID,
		SnapshotID: run.SnapshotID,
		Requested:  level,
		Achieved:   report.Level,
		Report:     report,
		Duration:   s.now().Sub(started),
		Passed:     verr == nil,
	}
	if verr != nil {
		result.Err = verr.Error()
	}

	return result, nil
}

// drillDirectoryFor reserves the directory ONE on-demand restore drill
// writes into: a fresh one per attempt, inside the reserved namespace.
//
// Inside that namespace for the reason it exists: a drill restores a
// whole tree of somebody's data, and an unreserved scratch directory
// would present it to artifact discovery, retention and prune as a few
// thousand unrecognised artifacts.
//
// Fresh per attempt for snapshotlifecycle.drillTarget's reason, which
// this path used to ignore. A drill restores under ConflictRefuse -- a
// verification that can overwrite is a verification that can destroy
// data -- so a fixed directory per run means the FIRST drill's output is
// what the second drill collides with. A second check of an unchanged,
// healthy snapshot then came back failed, which is this product calling
// a good restore point damaged over its own leftovers.
//
// MkdirTemp rather than an attempt counter: the counter exists in the
// lifecycle so that a crashed run's abandoned attempts stay
// distinguishable and in order for somebody reading them, and this path
// removes its own output as soon as it has passed, so there is nothing
// to order. What it needs is a name nothing else holds, including
// another operator asking the same question at the same moment.
func drillDirectoryFor(dir, runID string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	return os.MkdirTemp(dir, "verify-"+runID+"-")
}

// snapshotSurface resolves one incremental backup set and the catalog its
// rows live in, or says which of the two is missing.
func (s *Service) snapshotSurface(sourceName, setName string) (config.BackupSet, snapshotCatalog, error) {
	bs, err := s.incrementalBackupSet(sourceName, setName)
	if err != nil {
		return config.BackupSet{}, nil, err
	}

	catalog, ok := s.Journal.(snapshotCatalog)
	if !ok {
		return config.BackupSet{}, nil, ErrNoSnapshotCatalog
	}

	return bs, catalog, nil
}

// snapshotRunOf resolves a run id against one backup set's lineage, or
// the set's last-known-good snapshot when none is named.
func (s *Service) snapshotRunOf(ctx context.Context, catalog snapshotCatalog, bs config.BackupSet, runID string) (state.SnapshotRun, error) {
	lineage := snapshotLineage(bs)

	if runID == "" {
		run, err := catalog.LastKnownGoodSnapshot(ctx, lineage)
		if err != nil {
			if errors.Is(err, state.ErrSnapshotRunNotFound) {
				return state.SnapshotRun{}, fmt.Errorf("%w: %s", ErrSetHasNoSnapshots, bs.ID)
			}

			return state.SnapshotRun{}, fmt.Errorf("reading the last known good snapshot of %s: %w", bs.ID, err)
		}

		return run, nil
	}

	run, err := catalog.GetSnapshotRun(ctx, runID)
	if err != nil {
		if errors.Is(err, state.ErrSnapshotRunNotFound) {
			return state.SnapshotRun{}, fmt.Errorf("%w: %s has no snapshot %q", ErrSnapshotNotFound, bs.ID, runID)
		}

		return state.SnapshotRun{}, fmt.Errorf("reading snapshot %s: %w", runID, err)
	}

	if !strings.EqualFold(run.SetUUID, lineage) {
		return state.SnapshotRun{}, fmt.Errorf("%w: %s has no snapshot %q", ErrSnapshotNotFound, bs.ID, runID)
	}

	return run, nil
}

// openSetRepository opens one set's repository for a read, and returns
// the closer beside it.
//
// It never CREATES one, which is the difference between this and the
// cycle's own openRepository: a read is not evidence that a repository
// should exist, and a NAS that is asleep presents exactly as a location
// with nothing in it.
func (s *Service) openSetRepository(ctx context.Context, bs config.BackupSet) (backupengine.Repository, func(), error) {
	if s.Repositories == nil {
		return nil, nil, ErrNoIncrementalEngine
	}

	loc, err := s.repositoryLocation(bs)
	if err != nil {
		return nil, nil, err
	}

	repo, err := s.Repositories.OpenRepository(ctx, loc)
	if err != nil {
		return nil, nil, fmt.Errorf("opening repository %s: %w", loc.Domain, err)
	}

	return repo, func() { _ = repo.Close(ctx) }, nil
}
