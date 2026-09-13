package snapshotlifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/state"
)

// This file is one run of one incremental backup set, from the row that
// says it was asked for to the row that says it succeeded.
//
// Every phase is entered with a durable write BEFORE the work it names,
// and that ordering is the whole crash story. A row at SNAPSHOT_WRITE
// says an upload was in flight when this process stopped existing; a row
// at SNAPSHOT_WRITE carrying a manifest id says the upload finished and
// the process died before it could say anything else. Writing the phase
// afterwards would collapse those two into one indistinguishable state,
// and the reconciler would have to guess.

// ErrRunInFlight is returned when an idempotency key resolves to a run
// that is neither finished nor abandoned.
//
// It is not an error about the caller's request, which is why it is a
// sentinel a caller can match rather than a refusal sentence: a client
// retrying a submission it never heard back about is doing the right
// thing, and the right answer is "that run is still going, poll it",
// never a second pass over the same source.
var ErrRunInFlight = errors.New("snapshotlifecycle: this run is already in flight")

// ErrNoRepository and friends are the wiring refusals. They fire before
// the run's first durable write, because a row that exists for a run that
// could never have started is a row somebody has to reconcile later.
var (
	ErrNoRepository = errors.New("snapshotlifecycle: no repository was supplied for this run")
	ErrNoSource     = errors.New("snapshotlifecycle: no source tree was supplied for this run")
	ErrNoCatalog    = errors.New("snapshotlifecycle: no catalog was supplied; a snapshot run that cannot be recorded must not be started")
)

// Catalog is the durable half of a snapshot run: the rows and the
// transitions internal/state persists.
//
// It is an interface here, satisfied by *state.Journal, for the reason
// ADR 0001 gives about boundaries and for one more that is specific to
// this package: every crash boundary the reconciler decides has to be
// reachable in a test by writing a row and then pretending the process
// died, and a package that reached for a concrete journal would make that
// a database fixture instead of a value.
type Catalog interface {
	BeginSnapshotRun(ctx context.Context, req state.SnapshotRunRequest) (state.SnapshotRunOutcome, error)
	AdvanceSnapshotRun(ctx context.Context, runID string, to state.SnapshotPhase, upd state.SnapshotRunUpdate) error
	GetSnapshotRun(ctx context.Context, runID string) (state.SnapshotRun, error)
	UnfinishedSnapshotRuns(ctx context.Context) ([]state.SnapshotRun, error)

	// The per-set reads are keyed on the backup set's DURABLE uuid and
	// not on model.BackupSetID, whose two halves are names an operator
	// edits. A rename must not hide a set's history, its unfinished work
	// or its restore point, and it must not let a second last-known-good
	// row exist beside the first. See 0010_snapshot_run_lineage.sql.
	ListSnapshotRuns(ctx context.Context, setUUID string, limit int) ([]state.SnapshotRun, error)
	LastKnownGoodSnapshot(ctx context.Context, setUUID string) (state.SnapshotRun, error)

	// DomainSnapshotIDs is every manifest in one repository domain this
	// catalog accounts for, mapped to the run that claims it, read once
	// per reconciliation pass.
	//
	// It is one read for the whole domain rather than one per repository
	// snapshot because reconciliation asks the question about every
	// snapshot the repository holds, twice (attribution, then orphan
	// adoption), and a domain with a thousand manifests would otherwise
	// spend two thousand round trips per cycle on it.
	DomainSnapshotIDs(ctx context.Context, domain string) (map[string]string, error)

	// DomainHasSnapshot reports whether ANY backup set has ever committed
	// a manifest in this repository domain. It is the guard in front of
	// creating a repository, and it is domain-wide because a repository
	// serves a domain: evidence from a co-tenant set is evidence that a
	// repository already exists at that location.
	DomainHasSnapshot(ctx context.Context, domain string) (bool, error)

	// RepointLastKnownGood moves the last-known-good flag to an older
	// successful run of the same lineage. It is the only way the flag
	// moves other than a run reaching SUCCESS, and it exists for exactly
	// one situation: the newest restore point's snapshot has gone from
	// the repository, so the flag it held has been cleared and an older
	// snapshot that is still there should carry it. See
	// Reconciler.repointLastKnownGood.
	RepointLastKnownGood(ctx context.Context, setUUID, runID string) error
}

// Repository is everything this package asks of an open repository.
//
// It is narrower than backupengine.Repository on purpose: this package
// never creates, deletes, restores from or maintains a repository, and a
// port that offered it those would be a port through which a
// reconciliation bug could delete a restore point. The one write it can
// perform is storing a snapshot.
//
// A restore drill is the apparent exception and is not one. The drill
// happens INSIDE Verify, against a directory the caller names, so this
// package still cannot ask a repository to write anywhere of its own
// choosing: it can ask for a snapshot to be proven, and the proof for
// the top rung happens to involve a restore.
type Repository interface {
	SnapshotTree(ctx context.Context, req backupengine.TreeSnapshotRequest) (backupengine.TreeSnapshotInfo, error)
	Verify(ctx context.Context, id backupengine.SnapshotID, req backupengine.VerifyRequest) (backupengine.VerifyReport, error)
	LookupSnapshot(ctx context.Context, id backupengine.SnapshotID) (backupengine.SnapshotInfo, error)
	ListSnapshots(ctx context.Context, src backupengine.Source) ([]backupengine.SnapshotInfo, error)
}

// SourceTree is the pull-shaped source one run reads: the tree the engine
// walks, plus the reading side's own account of what it found.
//
// Report and Err are separate questions and a run needs both. Err is a
// run-level failure -- a listing that could not be completed, a read the
// source moved under -- and Report is the census: how many entries were
// considered, how many were stored and proven, and whether anything was
// left out. A tree can finish without an error and still be incomplete,
// which is exactly the case a success would otherwise be claimed for.
type SourceTree interface {
	// Root is the tree the engine walks.
	Root() backupengine.SourceDir

	// Report is the reading side's census. It is read after the walk.
	Report() ScanReport

	// Err is the run-level reading failure, or nil.
	Err() error

	// Close releases the source. It is called exactly once, on every
	// path out of a run.
	Close() error
}

// ScanReport is what the source side says about one pass.
//
// It is this package's own type rather than backupengine/source's Report
// because a snapshot run needs four facts out of it and none of the
// per-backend capability detail, and because a run driver that imported
// the source adapter's report would be a run driver that could not be
// tested without one.
type ScanReport struct {
	// Entries is how many source entries the pass considered, of every
	// kind, including the ones it refused.
	Entries int64

	// Stored is how many were stored and proven coherent.
	Stored int64

	// Skipped is how many were deliberately left out by policy or
	// configuration: a symlink under an ignore policy, a socket, an
	// excluded path. These do not make a backup incomplete.
	Skipped int64

	// Complete is the source side's own verdict: every entry it touched
	// was either stored and proven, or deliberately skipped. A false
	// here with a nil Err is the case this field exists for -- a
	// perfectly well-formed snapshot of a tree with a hole in it.
	Complete bool

	// Reason is one sentence about why the pass was not complete, or
	// empty. It is prose written for an operator by the layer that knows
	// what happened, and nothing branches on it.
	Reason string
}

// Observer hears what a run did, so metrics and the event stream can
// report it without this package deciding how either is shaped.
//
// Both methods are optional in the sense that a nil Observer is a no-op
// (see Runner.Observer), which is internal/obs's own arrangement: a
// package whose observability is mandatory is a package that cannot be
// tested without it.
type Observer interface {
	// ObservePhase is called after each durable phase write.
	ObservePhase(set model.BackupSetID, runID string, from, to state.SnapshotPhase, at time.Time)

	// ObserveRun is called once, with the run's final account of itself.
	ObserveRun(res RunResult)
}

// Runner drives snapshot runs. It holds no repository and no source: both
// arrive per run, because a repository handle is expensive to open and
// whoever owns the backup window owns that decision.
type Runner struct {
	// Catalog is where every phase is written down. A Runner without one
	// refuses to start a run rather than performing an unrecorded backup.
	Catalog Catalog

	// Now is injectable so a test can pin every timestamp a run writes.
	// Nil means time.Now in UTC.
	Now func() time.Time

	// Observer hears phase changes and the final result. Nil is silent.
	Observer Observer
}

// RunRequest is one pass over one backup set's source.
//
// Everything identifying is required, and that is not defensive
// validation: these values are what makes a row attributable after a
// crash, and a row that cannot be attributed is the input to the one
// verdict this package refuses to automate (see the package doc on
// quarantine).
type RunRequest struct {
	// RunID is this run's own identity, caller-generated.
	RunID string

	// IdempotencyKey identifies the logical request. A replay resolves to
	// the run that already exists rather than starting a second pass over
	// the source.
	IdempotencyKey string

	// OperationID is the durable operation row this run belongs to, or
	// empty for a scheduled cycle that nobody submitted.
	OperationID string

	// Set is the backup set's names, for a surface to render. SetUUID is
	// its durable identifier, and it is what the catalog keys this run's
	// lineage on: a set an operator renames must keep its history, its
	// unfinished work and its restore point.
	Set     model.BackupSetID
	SetUUID string

	Engine         model.BackupEngine
	Domain         model.RepositoryDomainID
	SourceIdentity model.SourceIdentity
	Consistency    model.ConsistencyMode

	// VerificationLevel is the level this set is CONFIGURED for, and the
	// floor this run must actually prove before its snapshot becomes a
	// restore point. What the run proved is recorded separately; see
	// verification.
	VerificationLevel model.VerificationLevel

	// Verification is the cadence and the cost of proving it: the sample
	// size, how often the deeper rungs come round, and where a restore
	// drill may write. The zero value asks for the engine's default
	// sample and no periodic escalation.
	Verification VerificationOptions

	// Source is the set's identity in the repository's own namespace: one
	// SourceInfo per backup set, never one per object.
	Source backupengine.Source

	// Description is operator-facing text stored with the snapshot.
	Description string

	// Repository is the open repository this run's snapshot goes into.
	Repository Repository

	// OpenTree opens the source for this run. It is a function rather
	// than an already-open tree because opening it dials the source, and
	// that belongs inside the SOURCE_SCAN phase rather than before the
	// run has a row.
	OpenTree func(ctx context.Context) (SourceTree, error)
}

// RunResult is one run's final account of itself.
//
// The four byte numbers are four different facts and a surface must never
// present one as another. LogicalBytes is what the source said the tree
// weighs; SourceBytesRead is what this pass actually pulled off the
// source; RepositoryBytesWritten is what actually landed in storage;
// ContentReusedBytes is the engine's own account of content the
// repository already had, and it is NOT the difference between the last
// two -- that difference is deduplication plus compression plus pack and
// index overhead, and reporting it as reuse would credit a first-ever
// snapshot of compressible data with reusing most of the tree. What
// compression saved is deliberately not reported here at all rather than
// folded into a number that means something else. Reporting a 100 GB tree
// as 100 GB uploaded is the specific claim EPIC K forbids.
type RunResult struct {
	RunID      string
	Set        model.BackupSetID
	Phase      state.SnapshotPhase
	SnapshotID string

	// Entries is every source entry the pass considered, of every kind,
	// including the ones it deliberately skipped. It is the source side's
	// own census and NOT files + directories: a pass that refused a
	// hundred sockets considered them, and a snapshot nobody measured
	// reports zero here beside Measured == false rather than a
	// reconstruction that looks like a real count.
	Entries     int64
	Files       int64
	Directories int64

	LogicalBytes           int64
	SourceBytesRead        int64
	RepositoryBytesWritten int64
	ContentReusedBytes     int64

	// VerificationLevel is what the set asked for and
	// VerificationAchieved is what this run proved. They are two fields
	// because they are two claims, and a row that carried only the first
	// would assert a verification nobody performed.
	VerificationLevel    model.VerificationLevel
	VerificationAchieved model.VerificationLevel
	VerificationStatus   string

	// LastKnownGood is whether this run's snapshot is now the set's
	// last-known-good restore point.
	LastKnownGood bool

	// Measured is false when this run's byte and entry counters were
	// never taken: a snapshot adopted by crash reconciliation has a
	// manifest but no record of what the dead process read or wrote.
	//
	// It exists so a surface can render "not measured" instead of four
	// zeroes. A deduplicated run that genuinely wrote almost nothing and
	// a run nobody measured are both rows of small numbers, and only one
	// of them is a fact about storage.
	Measured bool

	// Replayed is true when the request resolved to a run that already
	// existed and finished, in which case nothing was read from the
	// source by this call.
	Replayed bool

	// Reason is the failure or quarantine sentence, empty on success.
	Reason string

	StartedAt   time.Time
	CompletedAt time.Time
}

// Succeeded reports whether this run advertises a restore point. It is
// the one question every caller asks, and it has exactly one answer
// (state.SnapshotPhase.Advertised) rather than a comparison each caller
// writes for itself.
func (r RunResult) Succeeded() bool { return r.Phase.Advertised() }

// The verification-status vocabulary is internal/state's, because that is
// where the column and its CHECK constraint live. These are the three a
// run can actually produce, named here so this file reads in one
// vocabulary rather than two.
const (
	verificationPending = state.SnapshotVerificationPending
	verificationPassed  = state.SnapshotVerificationPassed
	verificationFailed  = state.SnapshotVerificationFailed
)

// maxReasonRunes bounds the failure sentence a run persists.
//
// A verification of a badly damaged repository produces up to a thousand
// per-object findings (the adapter's own error budget), and a catalog row
// is read by a person and by an API response. So the row carries a
// sentence and a count, and the findings themselves stay in the report the
// caller already has.
const maxReasonRunes = 512

// Run performs one pass: PENDING, SOURCE_SCAN, SNAPSHOT_WRITE,
// MANIFEST_COMMITTED, VERIFICATION, CATALOG_COMMIT, SUCCESS.
//
// It returns a result for every outcome it managed to record, including
// the failures, because the row is the answer: a caller that got an error
// and an empty result would have nothing to tell an operator about a run
// that definitely happened. The error is non-nil whenever the run did not
// reach SUCCESS.
//
// # What a failure guarantees
//
// No restore point is advertised (the row is not SUCCESS, so
// Advertised is false), the previous last-known-good row keeps its flag,
// and nothing about the set's success timestamps moves. That is enforced
// one layer down -- only a transition to SUCCESS touches the
// last-known-good flag, inside the transaction that makes it -- and it is
// what makes "a failed newer snapshot cannot replace last-known-good" a
// property of the schema rather than a rule this file remembers.
func (r *Runner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := r.validate(req); err != nil {
		return RunResult{}, err
	}

	started := r.now()

	outcome, err := r.Catalog.BeginSnapshotRun(ctx, state.SnapshotRunRequest{
		RunID:             req.RunID,
		IdempotencyKey:    req.IdempotencyKey,
		Set:               req.Set,
		SetUUID:           req.SetUUID,
		OperationID:       req.OperationID,
		Engine:            req.Engine.String(),
		Domain:            req.Domain.String(),
		SourceIdentity:    req.SourceIdentity.String(),
		ConsistencyMode:   string(req.Consistency),
		VerificationLevel: string(req.VerificationLevel),
		StartedAt:         started,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("snapshotlifecycle: opening a run for %s: %w", req.Set, err)
	}

	run := outcome.Run

	if !outcome.Created {
		// A replay. A finished run is its own answer; an unfinished one
		// belongs to a pass that is either still going in this process or
		// was abandoned by a dead one, and in both cases the thing that
		// must not happen is a second pass over the source under the same
		// identity. The reconciler is what resolves the abandoned case,
		// deliberately: it is the only code here that is allowed to
		// decide what a half-finished run became.
		if !run.Phase.Terminal() {
			return r.result(ctx, run.RunID, true), ErrRunInFlight
		}

		return r.result(ctx, run.RunID, true), nil
	}

	return r.drive(ctx, req, run)
}

// drive walks the phases. It is separate from Run so that Run owns
// admission (validation, idempotency) and this owns the pass itself.
func (r *Runner) drive(ctx context.Context, req RunRequest, run state.SnapshotRun) (RunResult, error) {
	if err := r.advance(ctx, &run, state.PhaseSourceScan, state.SnapshotRunUpdate{}); err != nil {
		return r.result(ctx, run.RunID, false), err
	}

	tree, err := req.OpenTree(ctx)
	if err != nil {
		return r.fail(ctx, &run, fmt.Sprintf("the source could not be opened: %v", err), err)
	}

	// The tree is closed on every path out of here, including the ones
	// that fail a phase in between: it holds the run's one transport
	// session, and a run that returned while holding it would leak a
	// connection per backup window.
	defer tree.Close() //nolint:errcheck // the source side reports its own close failures through Err.

	if err := r.advance(ctx, &run, state.PhaseSnapshotWrite, state.SnapshotRunUpdate{}); err != nil {
		return r.result(ctx, run.RunID, false), err
	}

	info, err := req.Repository.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{
		Source: req.Source,
		Root:   tree.Root(),

		// The manifest carries this run's own id, written by the adapter
		// as backupengine.TagKeyRun. It is what makes crash
		// reconciliation's orphan adoption an identity match rather than
		// a guess about time: without it, the only evidence that an
		// unclaimed manifest belongs to a dead run is that it appeared
		// after the run started, which is equally true of a co-tenant
		// set's snapshot and of one an operator took by hand.
		RunID:       req.RunID,
		Description: req.Description,
		Tags: map[string]string{
			backupengine.TagKeyBackupSet: req.Set.String(),
			backupengine.TagKeyDomain:    req.Domain.String(),
		},
	})
	if err != nil {
		// Nothing was committed: the engine's contract is one snapshot or
		// none, so there is no manifest to attribute and no restore point
		// to withdraw. Whatever the source side also noticed is worth
		// saying in the same sentence, because "the upload failed" and
		// "the source moved under the reader" are the same event seen
		// from two sides and an operator needs the second one.
		return r.fail(ctx, &run, snapshotFailureReason(err, tree.Err()), err)
	}

	scan := tree.Report()

	// The source side's verdict, decided here and written durably in the
	// same statement as the manifest. Two different failures make a pass
	// incomplete -- a run-level reading error and a census that did not
	// cover everything -- and neither of them is visible in the manifest,
	// which is a perfectly well-formed snapshot of a tree with a hole in
	// it.
	//
	// Recording it HERE, before the run has done anything about it, is
	// the whole point. A process that dies between this write and the
	// failure below leaves a row at MANIFEST_COMMITTED that reconciliation
	// would otherwise verify and promote to SUCCESS, advertising a
	// restore point already known to omit source data. With the verdict
	// on the row, reconciliation refuses to promote it.
	complete := tree.Err() == nil && scan.Complete

	// The manifest exists, so it is recorded before anything is decided
	// about it. A run that died here with the id already on its row is
	// reconcilable; one that died with the id still only in this
	// process's memory is the manifest-with-no-catalog-row case, which
	// costs a quarantine decision later. So this write happens first,
	// even when the scan below is about to fail the run.
	upd := state.SnapshotRunUpdate{
		SnapshotID:             new(string(info.ID)),
		EntriesScanned:         &scan.Entries,
		Files:                  &info.Files,
		Directories:            &info.Directories,
		LogicalBytes:           &info.Bytes,
		SourceBytesRead:        &info.SourceBytesRead,
		RepositoryBytesWritten: &info.RepositoryBytesWritten,
		SourceComplete:         &complete,
	}

	// Reuse is the ENGINE's own deduplication accounting or it is nothing.
	// The arithmetic that suggests itself here -- bytes read minus bytes
	// written -- is not reuse: it also contains compression and the
	// repository's pack and index overhead, so a first-ever snapshot of
	// compressible data would report most of the tree as content the
	// repository already had. An engine that cannot account for reuse
	// leaves the column NULL, which every surface already renders as "not
	// measured".
	if info.ContentReuseMeasured {
		upd.ContentReusedBytes = &info.ContentReusedBytes
	}

	if err := r.advance(ctx, &run, state.PhaseManifestCommitted, upd); err != nil {
		return r.result(ctx, run.RunID, false), err
	}

	// An incomplete scan fails the run and KEEPS the manifest. The
	// snapshot is a real, well-formed snapshot of a tree with a hole in
	// it: deleting it here would destroy the only copy of whatever it
	// does hold, on the authority of the code that noticed the hole, and
	// advertising it would be the silent partial backup this product
	// exists to prevent. So it stays, attributed to this set and this
	// run, for retention (#785) and for a person to decide about.
	if err := tree.Err(); err != nil {
		return r.fail(ctx, &run, snapshotFailureReason(nil, err), err)
	}

	if !scan.Complete {
		reason := scan.Reason
		if reason == "" {
			reason = "the source pass did not cover every entry it was asked to"
		}

		return r.fail(ctx, &run, reason, errors.New(reason))
	}

	return r.verifyAndCommit(ctx, req, &run)
}

// verifyAndCommit is the half of a run that happens after the engine is
// done: prove the snapshot to the depth this set requires, then say so
// durably.
func (r *Runner) verifyAndCommit(ctx context.Context, req RunRequest, run *state.SnapshotRun) (RunResult, error) {
	if err := r.advance(ctx, run, state.PhaseVerification, state.SnapshotRunUpdate{
		VerificationStatus: new(verificationPending),
	}); err != nil {
		return r.result(ctx, run.RunID, false), err
	}

	plan := planVerification(req.VerificationLevel, req.Verification, run.RunID, r.verificationHistory(ctx, req.SetUUID), r.now())

	achieved, report, verifyErr := plan.run(ctx, req.Repository, backupengine.SnapshotID(run.SnapshotID))
	if verifyErr != nil {
		reason := verificationFailureReason(verifyErr, report)

		return r.failWith(ctx, run, reason, verifyErr, state.SnapshotRunUpdate{
			VerificationStatus: new(verificationFailed),

			// The achieved level is written back to empty, explicitly.
			// A run that entered verification carrying a level from an
			// earlier attempt and then failed must not keep claiming it.
			// A verification that proved something real but shallower
			// than the set requires is one of these failures, and it
			// must not leave the shallower claim on the row either.
			VerificationLevelAchieved: new(""),
			Reason:                    &reason,
		})
	}

	// CATALOG_COMMIT is entered with the verification result already on
	// it, and that is what makes the boundary after it recoverable: a
	// process that dies between here and SUCCESS leaves a row that
	// carries everything the success claim needs, so the reconciler can
	// complete it rather than having to re-verify or fail it.
	if err := r.advance(ctx, run, state.PhaseCatalogCommit, state.SnapshotRunUpdate{
		VerificationStatus:        new(verificationPassed),
		VerificationLevelAchieved: new(string(achieved)),
	}); err != nil {
		return r.result(ctx, run.RunID, false), err
	}

	if err := r.advance(ctx, run, state.PhaseSuccess, state.SnapshotRunUpdate{}); err != nil {
		return r.result(ctx, run.RunID, false), err
	}

	// The drill's restored tree is only discarded once the run it proved
	// has succeeded; every failure above keeps it (see
	// discardDrillOutput). A removal that fails is not a reason to fail a
	// backup that is proven and recorded: the directory is under the
	// caller's own scratch root, named for this run, and the next drill
	// writes beside it rather than into it.
	_ = plan.discardDrillOutput() //nolint:errcheck // see above.

	res := r.result(ctx, run.RunID, false)
	r.observeRun(res)

	return res, nil
}

// verificationHistory is what this set has actually proved recently, for
// the cadence to read.
//
// A history that cannot be read produces no history rather than an error,
// and the consequence is deliberately the safe direction: with nothing to
// show that a deep check has happened recently, every cadence reads as
// due, so an unreadable catalog makes this run verify MORE rather than
// less.
func (r *Runner) verificationHistory(ctx context.Context, setUUID string) []state.SnapshotRun {
	history, err := r.Catalog.ListSnapshotRuns(ctx, setUUID, verificationHistory)
	if err != nil {
		return nil
	}

	return history
}

// advance is the one place a phase is written, so the graph is consulted
// on every durable move and nothing can write a phase the table does not
// permit.
func (r *Runner) advance(ctx context.Context, run *state.SnapshotRun, to state.SnapshotPhase, upd state.SnapshotRunUpdate) error {
	from := run.Phase
	if err := Validate(from, to); err != nil {
		return err
	}

	at := r.now()
	upd.At = at

	if err := r.Catalog.AdvanceSnapshotRun(ctx, run.RunID, to, upd); err != nil {
		return fmt.Errorf("snapshotlifecycle: recording %s -> %s for run %s: %w", from, to, run.RunID, err)
	}

	run.Phase = to
	applyUpdate(run, upd)

	if r.Observer != nil {
		r.Observer.ObservePhase(run.Set, run.RunID, from, to, at)
	}

	return nil
}

// applyUpdate mirrors onto the in-memory row what the journal just wrote.
//
// It exists because the next phase reads fields the previous one set --
// verification reads the snapshot id the manifest commit recorded -- and
// the alternative is re-reading the row after every write. That would be
// a query per phase to learn something this process already knows, and,
// worse, it would make a phase silently depend on the read succeeding: an
// empty snapshot id looks exactly like a snapshot that was never stored,
// which is the failure this function was written in response to.
//
// Only the fields a later phase or a report actually reads are mirrored.
// Timestamps are deliberately not: updated_at and completed_at are the
// journal's, and Runner.result reads the row back for the final answer.
func applyUpdate(run *state.SnapshotRun, upd state.SnapshotRunUpdate) {
	if upd.SnapshotID != nil {
		run.SnapshotID = *upd.SnapshotID
	}

	if upd.EntriesScanned != nil {
		run.EntriesScanned = upd.EntriesScanned
	}

	if upd.Files != nil {
		run.Files = upd.Files
	}

	if upd.Directories != nil {
		run.Directories = upd.Directories
	}

	if upd.LogicalBytes != nil {
		run.LogicalBytes = upd.LogicalBytes
	}

	if upd.SourceBytesRead != nil {
		run.SourceBytesRead = upd.SourceBytesRead
	}

	if upd.RepositoryBytesWritten != nil {
		run.RepositoryBytesWritten = upd.RepositoryBytesWritten
	}

	if upd.ContentReusedBytes != nil {
		run.ContentReusedBytes = upd.ContentReusedBytes
	}

	if upd.SourceComplete != nil {
		run.SourceComplete = upd.SourceComplete
	}

	if upd.VerificationStatus != nil {
		run.VerificationStatus = *upd.VerificationStatus
	}

	if upd.VerificationLevelAchieved != nil {
		run.VerificationLevelAchieved = *upd.VerificationLevelAchieved
	}

	if upd.Reason != nil {
		run.Reason = *upd.Reason
	}
}

// fail records a pre-commit failure and returns the row plus the cause.
func (r *Runner) fail(ctx context.Context, run *state.SnapshotRun, reason string, cause error) (RunResult, error) {
	return r.failWith(ctx, run, reason, cause, state.SnapshotRunUpdate{Reason: &reason})
}

// failWith is fail with extra columns to record on the way out, for the
// failures that know something more than a sentence (a verification
// result, for instance).
//
// A failure whose own durable write fails is reported as both errors
// joined, never as the original alone: the second one means the catalog
// does not know this run failed, which is a different and worse
// situation, and the reconciler is the thing that will have to sort it
// out.
func (r *Runner) failWith(
	ctx context.Context,
	run *state.SnapshotRun,
	reason string,
	cause error,
	upd state.SnapshotRunUpdate,
) (RunResult, error) {
	if upd.Reason == nil {
		upd.Reason = &reason
	}

	err := r.advance(ctx, run, state.PhaseFailed, upd)

	res := r.result(ctx, run.RunID, false)
	r.observeRun(res)

	if err != nil {
		return res, errors.Join(cause, err)
	}

	return res, fmt.Errorf("snapshotlifecycle: run %s failed at %s: %w", run.RunID, run.Phase, cause)
}

// result reads the row back and projects it.
//
// Reading rather than assembling from what this process remembers is
// deliberate: the row is what survived, a caller acting on anything else
// would be acting on a claim the journal does not make, and a read of one
// row by primary key is not a cost worth trading that for.
func (r *Runner) result(ctx context.Context, runID string, replayed bool) RunResult {
	run, err := r.Catalog.GetSnapshotRun(ctx, runID)
	if err != nil {
		// The row was written and cannot be read back, which is a
		// journal problem rather than a backup problem. The caller
		// already has (or is about to get) the error; what it must not
		// get is a result that looks like a successful run.
		return RunResult{RunID: runID, Replayed: replayed, Reason: fmt.Sprintf("the run's own catalog row could not be read back: %v", err)}
	}

	return projectRun(run, replayed)
}

// projectRun is the row-to-result projection, in one place so that Run,
// the reconciler and the tests all read a row the same way.
func projectRun(run state.SnapshotRun, replayed bool) RunResult {
	return RunResult{
		RunID:                  run.RunID,
		Set:                    run.Set,
		Phase:                  run.Phase,
		SnapshotID:             run.SnapshotID,
		Entries:                measured(run.EntriesScanned),
		Files:                  measured(run.Files),
		Directories:            measured(run.Directories),
		LogicalBytes:           measured(run.LogicalBytes),
		SourceBytesRead:        measured(run.SourceBytesRead),
		RepositoryBytesWritten: measured(run.RepositoryBytesWritten),
		ContentReusedBytes:     measured(run.ContentReusedBytes),
		VerificationLevel:      model.VerificationLevel(run.VerificationLevel),
		VerificationAchieved:   model.VerificationLevel(run.VerificationLevelAchieved),
		VerificationStatus:     run.VerificationStatus,
		LastKnownGood:          run.LastKnownGood,
		Measured:               run.SourceBytesRead != nil && run.RepositoryBytesWritten != nil,
		Replayed:               replayed,
		Reason:                 run.Reason,
		StartedAt:              run.StartedAt,
		CompletedAt:            at(run.CompletedAt),
	}
}

// measured flattens a counter the journal keeps nullable.
//
// The journal distinguishes "nobody measured this" from "this measured
// zero", which is a distinction a catalog row needs: an adopted snapshot
// has no read or written byte count because the process that would have
// counted them died. A RunResult is a report, and reporting an unmeasured
// counter as zero would be the same lie in the other direction -- so the
// two are kept apart by Measured below rather than by the number itself.
func measured(v *int64) int64 {
	if v == nil {
		return 0
	}

	return *v
}

// at flattens a timestamp the journal keeps nullable. The zero time is
// what every surface in this repository already reads as "has not
// happened yet" (see service.Operation's StartedAt/FinishedAt).
func at(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}

	return *t
}

func (r *Runner) observeRun(res RunResult) {
	if r.Observer != nil {
		r.Observer.ObserveRun(res)
	}
}

// validate refuses a request that could not be recorded or could not be
// attributed later. It runs before the first durable write, so a refusal
// leaves nothing behind to reconcile.
func (r *Runner) validate(req RunRequest) error {
	if r.Catalog == nil {
		return ErrNoCatalog
	}

	if req.Repository == nil {
		return ErrNoRepository
	}

	if req.OpenTree == nil {
		return ErrNoSource
	}

	if req.RunID == "" {
		return errors.New("snapshotlifecycle: a snapshot run needs its own id")
	}

	if req.IdempotencyKey == "" {
		return errors.New("snapshotlifecycle: a snapshot run needs an idempotency key, or a retried request would read the source twice")
	}

	if req.Set.IsZero() {
		return errors.New("snapshotlifecycle: a snapshot run needs the backup set it is for")
	}

	if req.SetUUID == "" {
		return errors.New("snapshotlifecycle: a snapshot run needs the backup set's durable uuid, or its snapshot lineage would be keyed on a name an operator can edit")
	}

	if !req.Engine.UsesRepository() {
		return fmt.Errorf("snapshotlifecycle: engine %q has no repository, so it has no snapshot lifecycle; this package drives the incremental engine only", req.Engine)
	}

	if req.Domain.IsZero() {
		return errors.New("snapshotlifecycle: a snapshot run needs the repository domain it is writing to")
	}

	if req.SourceIdentity.IsZero() {
		return errors.New("snapshotlifecycle: a snapshot run needs the set's source identity, or its snapshot lineage cannot be found again")
	}

	if req.VerificationLevel == "" {
		return errors.New("snapshotlifecycle: a snapshot run needs the verification level its set is configured for")
	}

	// A set configured for restore drills is an operator instruction, and
	// a drill needs somewhere to restore to. Refusing here, before the
	// run's first durable write, is the difference between a
	// configuration mistake an operator can fix and a nightly backup that
	// reads its whole source and then fails its verification for a reason
	// that has nothing to do with the data.
	if req.VerificationLevel == model.LevelRestoreDrill && req.Verification.DrillDir == "" {
		return errors.New("snapshotlifecycle: this set is configured for restore-drill verification and no directory was given for the drill to restore into")
	}

	return nil
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}

	return time.Now().UTC()
}

// snapshotFailureReason renders the operator sentence for a failed pass
// out of what the two sides said.
//
// Both halves are worth having and neither is sufficient. The engine's
// error says the snapshot was not stored; the source side's says why the
// bytes stopped arriving, which is the half an operator can act on. When
// only one is present, the sentence is that one.
func snapshotFailureReason(engineErr, sourceErr error) string {
	switch {
	case engineErr != nil && sourceErr != nil:
		return truncateReason(fmt.Sprintf("the snapshot was not stored (%v); the source pass reported: %v", engineErr, sourceErr))
	case engineErr != nil:
		return truncateReason(fmt.Sprintf("the snapshot was not stored: %v", engineErr))
	case sourceErr != nil:
		return truncateReason(fmt.Sprintf("the source pass failed: %v", sourceErr))
	default:
		return "the snapshot was not stored, and neither side said why"
	}
}

// verificationFailureReason renders a failed verification: the error, plus
// how many findings there were and the first of them.
//
// A count and one example rather than the list, for maxReasonRunes'
// reason. The full list is in the report the caller already holds.
func verificationFailureReason(err error, report backupengine.VerifyReport) string {
	if n := len(report.Errors); n > 0 {
		return truncateReason(fmt.Sprintf("verification failed with %d finding(s), the first being %q: %v", n, report.Errors[0], err))
	}

	return truncateReason(fmt.Sprintf("verification did not complete: %v", err))
}

// truncateReason bounds a persisted sentence without cutting a rune in
// half, and says that it did so rather than leaving a reader wondering
// whether the sentence simply stopped.
func truncateReason(s string) string {
	s = strings.TrimSpace(s)

	runes := []rune(s)
	if len(runes) <= maxReasonRunes {
		return s
	}

	return string(runes[:maxReasonRunes]) + "... (truncated)"
}
