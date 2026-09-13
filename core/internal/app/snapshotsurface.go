package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/snapshotretention"
	"github.com/backupdproject/backupd/core/internal/state"
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
// Every refusal is the journal's: a run with no committed manifest, one
// already deleted, one whose delete intent is durable and one at LOST are
// all refused there, in the words that say why (state.PlaceSnapshotHold).
// Nothing is re-decided here, because a second opinion about what a hold
// can protect is a second answer.
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

	return catalog.PlaceSnapshotHold(ctx, state.SnapshotHoldRequest{
		HoldID:   req.HoldID,
		RunID:    run.RunID,
		Reason:   req.Reason,
		PlacedBy: req.PlacedBy,
		At:       at,
	})
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
		target = drillDirectoryFor(opts.DrillDir, run.RunID)
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

// drillDirectoryFor is where one on-demand restore drill writes.
//
// Inside the reserved namespace, for the reason that namespace exists: a
// drill restores a whole tree of somebody's data, and an unreserved
// scratch directory would present it to artifact discovery, retention and
// prune as a few thousand unrecognised artifacts.
func drillDirectoryFor(dir, runID string) string {
	return dir + "/verify-" + runID
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
