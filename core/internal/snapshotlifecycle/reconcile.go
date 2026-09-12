package snapshotlifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/state"
)

// This file is what a crash leaves behind, and the rule for every place
// it can be left.
//
// Two durable stores hold a backup set's snapshot state: this manager's
// catalog and the repository itself. A process that stops existing
// between any two writes leaves them disagreeing, and there are only so
// many ways they can disagree. Each one has a rule below, each rule has a
// test, and the rules are deliberately boring: adopt what is provably
// ours, finish what is provably finished, fail what never landed, and
// quarantine -- never delete -- what cannot be attributed at all.
//
// # Why a reconciliation pass verifies
//
// A run that died after its manifest was committed holds a complete,
// well-formed snapshot that nothing has proven yet. Failing it would
// throw away a restore point over a power cut; marking it successful
// would write down a verification that never happened. So the pass
// verifies it, which is the only honest third option, and the cost is
// bounded by how many runs actually crashed rather than by how many ran.
//
// # Why nothing here deletes anything
//
// The repository may hold a snapshot this manager cannot account for. It
// may be from a run whose catalog write never landed, from a co-tenant
// set in a shared domain, or from an operator's own use of the vendor's
// tooling against their own bucket. The evidence that would tell those
// apart is exactly what the crash destroyed. Deleting it would be this
// product destroying a backup to tidy its own bookkeeping, so it is
// written down, named and surfaced instead.

// DefaultHistoryLimit bounds how many of a set's finished rows one
// reconciliation pass reads.
//
// The unfinished rows are read in full -- there are as many of those as
// there were crashes, which is a small number by construction -- and this
// bounds the FINISHED side, which grows by one row per run for ever. What
// the finished side is needed for is narrow: finding the newest
// successful run whose snapshot is still there, so last-known-good can be
// re-pointed when the newest one turns out to be gone. A window of a few
// hundred runs answers that for any deployment whose newest few hundred
// snapshots are not all simultaneously missing, and a pass that read the
// whole history would grow with the deployment's age.
const DefaultHistoryLimit = 256

// VerdictKind names what reconciliation decided about one run or one
// repository snapshot.
//
// The set is closed and each member is one crash boundary, because that
// is what makes "is every boundary covered" a question with an answer: a
// test walks this list, and a boundary with no verdict has nowhere to be
// reported from.
type VerdictKind string

const (
	// VerdictAbandonedBeforeUpload is a run that died before the engine
	// was asked for anything: nothing exists anywhere, and the row is
	// failed.
	VerdictAbandonedBeforeUpload VerdictKind = "abandoned_before_upload"

	// VerdictAbandonedUpload is a run that died with an upload in flight
	// and no manifest to show for it. The repository may hold unreferenced
	// content, which is maintenance's business and not a restore point.
	VerdictAbandonedUpload VerdictKind = "abandoned_upload"

	// VerdictManifestAdopted is a manifest the repository holds, produced
	// by a run whose catalog row never got to say so. The row adopts it
	// and the snapshot stops being unattributable.
	VerdictManifestAdopted VerdictKind = "manifest_adopted"

	// VerdictVerified is a run whose snapshot this pass proved and
	// committed: the "died after the manifest" and "died during
	// verification" boundaries both end here when the data is good.
	VerdictVerified VerdictKind = "verified"

	// VerdictCommitCompleted is the boundary this whole file is shaped
	// around: a run that was verified and died before the catalog said
	// so. Everything the success claim needs is already durable, so the
	// pass completes it rather than re-doing it.
	VerdictCommitCompleted VerdictKind = "commit_completed"

	// VerdictVerificationFailed is a run whose snapshot this pass could
	// not prove. The manifest is kept and attributed; the run is failed.
	VerdictVerificationFailed VerdictKind = "verification_failed"

	// VerdictManifestMissing is a row that names a manifest the
	// repository does not have, before it ever reached success.
	VerdictManifestMissing VerdictKind = "manifest_missing"

	// VerdictRestorePointLost is a successful run whose snapshot is no
	// longer in the repository. The promise cannot be kept any more, and
	// last-known-good moves to the newest one that can.
	VerdictRestorePointLost VerdictKind = "restore_point_lost"

	// VerdictDeleteCompleted is a delete this product intended, found
	// already carried out. The intent is what makes it decidable.
	VerdictDeleteCompleted VerdictKind = "delete_completed"

	// VerdictDeletePending is a delete this product intended and whose
	// snapshot is still there. It is reported and NOT re-issued: issuing
	// deletes is retention's decision (#785), and a reconciliation pass
	// that deleted on its own would be a second authority for the one
	// act that cannot be undone.
	VerdictDeletePending VerdictKind = "delete_pending"

	// VerdictQuarantined is a repository snapshot nothing can attribute.
	// It is recorded and left in place. See this file's header.
	VerdictQuarantined VerdictKind = "quarantined"

	// VerdictLastKnownGoodRepointed is last-known-good moving to an older
	// run because the newer one's snapshot is gone.
	VerdictLastKnownGoodRepointed VerdictKind = "last_known_good_repointed"

	// VerdictMaintenanceInterrupted is a recorded maintenance attempt
	// that did not finish cleanly. Nothing about a snapshot changes.
	VerdictMaintenanceInterrupted VerdictKind = "maintenance_interrupted"
)

// verdictKinds is every verdict, for the test that checks each boundary
// has one and for a surface that has to render them.
var verdictKinds = []VerdictKind{
	VerdictAbandonedBeforeUpload,
	VerdictAbandonedUpload,
	VerdictManifestAdopted,
	VerdictVerified,
	VerdictCommitCompleted,
	VerdictVerificationFailed,
	VerdictManifestMissing,
	VerdictRestorePointLost,
	VerdictDeleteCompleted,
	VerdictDeletePending,
	VerdictQuarantined,
	VerdictLastKnownGoodRepointed,
	VerdictMaintenanceInterrupted,
}

// VerdictKinds returns every verdict as a copy the caller owns.
func VerdictKinds() []VerdictKind {
	out := make([]VerdictKind, len(verdictKinds))
	copy(out, verdictKinds)

	return out
}

// Verdict is one decision about one run or one repository snapshot.
type Verdict struct {
	Kind VerdictKind

	// RunID is the catalog row this is about, including the row a
	// quarantine verdict opened for a snapshot that never had one.
	RunID string

	// SnapshotID is the engine's opaque id, empty when the verdict is
	// about a run that never produced one.
	SnapshotID string

	// From and To are the phase move this verdict made, and are equal
	// when it made none (a pending delete, an interrupted maintenance).
	From state.SnapshotPhase
	To   state.SnapshotPhase

	// Reason is the operator sentence. It never carries a secret or a
	// full source path: everything it names is a run id, a snapshot id, a
	// phase or a rendered error from a layer that is already written to
	// that contract.
	Reason string
}

// Changed reports whether this verdict moved a row, which is what a log
// or an activity feed filters on: a pass over a healthy deployment
// produces no changes and should be silent rather than noisy.
func (v Verdict) Changed() bool { return v.From != v.To }

// ReconcileReport is one pass over one backup set.
type ReconcileReport struct {
	Set      model.BackupSetID
	Verdicts []Verdict

	// LastKnownGoodRunID is the set's last-known-good run after this
	// pass, or empty if it has none. It is reported because the pass is
	// the one thing that can take it away (a snapshot that is gone) and
	// move it (to an older run that is still there), and a caller acting
	// on a restore point needs the answer from after reconciliation
	// rather than from before it.
	LastKnownGoodRunID string
}

// Changed is every verdict that moved a row.
func (r ReconcileReport) Changed() []Verdict {
	var out []Verdict
	for _, v := range r.Verdicts {
		if v.Changed() {
			out = append(out, v)
		}
	}

	return out
}

// ReconcileRequest is one backup set's reconciliation input.
type ReconcileRequest struct {
	Set    model.BackupSetID
	Domain model.RepositoryDomainID

	// Source is the set's identity in the repository: what its snapshots
	// are listed by.
	Source backupengine.Source

	// SourceIdentity is the set's stable source identity
	// (model.NewSourceIdentity), which is what a row records rather than
	// a path. See sourceIdentityOf.
	SourceIdentity model.SourceIdentity

	// Repository is the open repository for Domain.
	Repository Repository

	// Maintenance is the last recorded maintenance state for this
	// repository, or nil when there is none (a repository that has never
	// been maintained) or when the caller cannot read it.
	//
	// It is an input rather than something this package loads because the
	// record lives beside the repository's local state and the caller
	// already holds the path to it; and because a reconciliation pass must
	// not fail a backup set over an unreadable maintenance note.
	Maintenance *backupengine.MaintenanceOwnership
}

// Reconciler resolves what a crash left behind, for one backup set at a
// time.
type Reconciler struct {
	// Catalog is the durable side of the disagreement.
	Catalog Catalog

	// HistoryLimit bounds the finished rows one pass reads. Zero means
	// DefaultHistoryLimit.
	HistoryLimit int

	// Now is injectable so a test can pin the timestamps a pass writes.
	Now func() time.Time

	// Observer hears each phase move, so the same feed that carries a
	// run's progress carries its recovery. Nil is silent.
	Observer Observer
}

// Reconcile decides every open question about one backup set's snapshots
// and returns what it decided.
//
// It reads the repository FIRST and refuses to decide anything if that
// read fails. Every verdict below is of the form "the catalog says X and
// the repository says Y", so a pass that could not ask the repository
// would be a pass deciding from half the evidence -- and the failure mode
// is specific and terrible: an unreachable repository would look exactly
// like a repository that has lost every snapshot in it, and the pass would
// mark a whole deployment's restore points lost.
func (r *Reconciler) Reconcile(ctx context.Context, req ReconcileRequest) (ReconcileReport, error) {
	if r.Catalog == nil {
		return ReconcileReport{}, ErrNoCatalog
	}

	if req.Repository == nil {
		return ReconcileReport{}, ErrNoRepository
	}

	if req.Set.IsZero() || req.Domain.IsZero() {
		return ReconcileReport{}, errors.New("snapshotlifecycle: reconciliation needs the backup set and the repository domain it is about")
	}

	present, err := r.repositorySnapshots(ctx, req)
	if err != nil {
		return ReconcileReport{}, err
	}

	report := ReconcileReport{Set: req.Set}

	unfinished, err := r.unfinishedFor(ctx, req.Set)
	if err != nil {
		return ReconcileReport{}, err
	}

	for i := range unfinished {
		verdicts, err := r.resolveUnfinished(ctx, req, unfinished[i], present)
		if err != nil {
			return report, err
		}

		report.Verdicts = append(report.Verdicts, verdicts...)
	}

	finished, err := r.Catalog.ListSnapshotRuns(ctx, req.Set, r.historyLimit())
	if err != nil {
		return report, fmt.Errorf("snapshotlifecycle: reading %s's snapshot history: %w", req.Set, err)
	}

	for i := range finished {
		verdicts, err := r.resolveFinished(ctx, finished[i], present)
		if err != nil {
			return report, err
		}

		report.Verdicts = append(report.Verdicts, verdicts...)
	}

	quarantines, err := r.quarantineUnattributed(ctx, req, present)
	if err != nil {
		return report, err
	}

	report.Verdicts = append(report.Verdicts, quarantines...)

	repoint, err := r.repointLastKnownGood(ctx, req, present)
	if err != nil {
		return report, err
	}

	report.Verdicts = append(report.Verdicts, repoint...)

	if v, ok := maintenanceVerdict(req); ok {
		report.Verdicts = append(report.Verdicts, v)
	}

	lkg, err := r.Catalog.LastKnownGoodSnapshot(ctx, req.Set)
	switch {
	case err == nil:
		report.LastKnownGoodRunID = lkg.RunID
	case errors.Is(err, state.ErrSnapshotRunNotFound):
		// A set with no successful run yet, or one whose only successful
		// runs have lost their snapshots. Both are real and neither is a
		// failure of this pass.
	default:
		return report, fmt.Errorf("snapshotlifecycle: reading %s's last-known-good snapshot: %w", req.Set, err)
	}

	return report, nil
}

// repositorySnapshots is what the repository holds for this set's source,
// keyed by id.
func (r *Reconciler) repositorySnapshots(ctx context.Context, req ReconcileRequest) (map[string]backupengine.SnapshotInfo, error) {
	infos, err := req.Repository.ListSnapshots(ctx, req.Source)
	if err != nil {
		return nil, fmt.Errorf("snapshotlifecycle: listing %s's snapshots in repository %s: %w", req.Set, req.Domain, err)
	}

	present := make(map[string]backupengine.SnapshotInfo, len(infos))
	for _, info := range infos {
		present[string(info.ID)] = info
	}

	return present, nil
}

// unfinishedFor is every non-terminal row for this set, oldest first.
//
// The journal's worklist is deployment-wide, because a reconciler reads it
// to find what a dead process abandoned and a dead process does not tidy
// up per set. Filtering here rather than asking for a narrower query
// keeps the journal's side one read and one index, and the population is
// bounded by how many runs were interrupted rather than by how many ran.
func (r *Reconciler) unfinishedFor(ctx context.Context, set model.BackupSetID) ([]state.SnapshotRun, error) {
	all, err := r.Catalog.UnfinishedSnapshotRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("snapshotlifecycle: reading the unfinished snapshot runs: %w", err)
	}

	var out []state.SnapshotRun
	for _, run := range all {
		if run.Set == set {
			out = append(out, run)
		}
	}

	return out, nil
}

// resolveUnfinished is the crash-boundary table for a row that never
// reached a terminal phase.
func (r *Reconciler) resolveUnfinished(
	ctx context.Context,
	req ReconcileRequest,
	run state.SnapshotRun,
	present map[string]backupengine.SnapshotInfo,
) ([]Verdict, error) {
	switch run.Phase {
	case state.PhasePending, state.PhaseSourceScan:
		// Nothing was asked of the engine, so nothing can exist to
		// adopt. The row is a receipt for a pass that never happened.
		return r.finish(ctx, run, state.PhaseFailed, VerdictAbandonedBeforeUpload,
			fmt.Sprintf("this run was interrupted at %s, before any snapshot was written", run.Phase))

	case state.PhaseSnapshotWrite:
		return r.resolveInterruptedUpload(ctx, req, run, present)

	case state.PhaseManifestCommitted, state.PhaseVerification:
		return r.resolveUnverified(ctx, req, run, present)

	case state.PhaseCatalogCommit:
		return r.completeCommit(ctx, run, present)

	default:
		// Terminal phases do not appear in the unfinished worklist, and a
		// phase this function has not been taught must not be silently
		// left alone: an unreconciled row is an invisible one.
		return nil, fmt.Errorf("snapshotlifecycle: run %s is in phase %s, which reconciliation has no rule for", run.RunID, run.Phase)
	}
}

// resolveInterruptedUpload decides a row that died with an upload in
// flight.
//
// The repository is asked whether a manifest for this run exists after
// all, because the window between "the engine saved the manifest" and
// "the catalog recorded it" is exactly where a process dies. A manifest
// found there is ADOPTED rather than deleted: it is a complete snapshot
// of this set's source, and the only thing wrong with it is that nothing
// had written down that it exists.
func (r *Reconciler) resolveInterruptedUpload(
	ctx context.Context,
	req ReconcileRequest,
	run state.SnapshotRun,
	present map[string]backupengine.SnapshotInfo,
) ([]Verdict, error) {
	if adopted, ok := r.orphanFor(ctx, req, run, present); ok {
		verdicts, err := r.adopt(ctx, run, adopted)
		if err != nil {
			return verdicts, err
		}

		more, err := r.resolveUnverified(ctx, req, r.reload(ctx, run.RunID, run), present)

		return append(verdicts, more...), err
	}

	return r.finish(ctx, run, state.PhaseFailed, VerdictAbandonedUpload,
		"this run was interrupted while its snapshot was being written, and the repository holds no manifest for it")
}

// resolveUnverified verifies a row whose snapshot is durable but
// unproven, which is both the "died after the manifest" and the "died
// during verification" boundary.
func (r *Reconciler) resolveUnverified(
	ctx context.Context,
	req ReconcileRequest,
	run state.SnapshotRun,
	present map[string]backupengine.SnapshotInfo,
) ([]Verdict, error) {
	if run.SnapshotID == "" {
		return r.finish(ctx, run, state.PhaseFailed, VerdictManifestMissing,
			fmt.Sprintf("this run reached %s without recording a snapshot id, so there is nothing to verify", run.Phase))
	}

	if _, ok := present[run.SnapshotID]; !ok {
		return r.finish(ctx, run, state.PhaseFailed, VerdictManifestMissing,
			"the snapshot this run committed is no longer in the repository, so the run cannot be completed")
	}

	var verdicts []Verdict

	// A verification interrupted by a crash is retried from the durable
	// manifest, which is the one backward move the graph permits. Doing
	// it as an explicit, recorded move rather than by verifying from
	// where the row sits keeps the transition log honest about the fact
	// that the first verification never finished.
	if run.Phase == state.PhaseVerification {
		if err := r.move(ctx, &run, state.PhaseManifestCommitted, state.SnapshotRunUpdate{}); err != nil {
			return verdicts, err
		}
	}

	if err := r.move(ctx, &run, state.PhaseVerification, state.SnapshotRunUpdate{VerificationStatus: new(verificationPending)}); err != nil {
		return verdicts, err
	}

	achieved, report, verifyErr := verify(ctx, req.Repository, backupengine.SnapshotID(run.SnapshotID))
	if verifyErr != nil {
		// finish records the failed verification status on the same row
		// as the phase, in one write, because they are one fact.
		v, err := r.finish(ctx, run, state.PhaseFailed, VerdictVerificationFailed, verificationFailureReason(verifyErr, report))

		return append(verdicts, v...), err
	}

	if err := r.move(ctx, &run, state.PhaseCatalogCommit, state.SnapshotRunUpdate{
		VerificationStatus:        new(verificationPassed),
		VerificationLevelAchieved: new(string(achieved)),
	}); err != nil {
		return verdicts, err
	}

	if err := r.move(ctx, &run, state.PhaseSuccess, state.SnapshotRunUpdate{}); err != nil {
		return verdicts, err
	}

	return append(verdicts, Verdict{
		Kind:       VerdictVerified,
		RunID:      run.RunID,
		SnapshotID: run.SnapshotID,
		From:       state.PhaseManifestCommitted,
		To:         state.PhaseSuccess,
		Reason:     "this run's snapshot was verified after an interrupted pass and is now a restore point",
	}), nil
}

// completeCommit finishes a run that was verified and died before the
// catalog said so.
//
// This is the boundary #783 names specifically, and the rule is the
// narrow one: complete it ONLY if the verification result it carries is a
// pass and its snapshot is still there. Everything the success claim
// needs is then already durable, and re-verifying would spend a full read
// of the snapshot to learn what the row already says.
func (r *Reconciler) completeCommit(
	ctx context.Context,
	run state.SnapshotRun,
	present map[string]backupengine.SnapshotInfo,
) ([]Verdict, error) {
	if _, ok := present[run.SnapshotID]; !ok {
		return r.finish(ctx, run, state.PhaseFailed, VerdictManifestMissing,
			"this run was verified, but the snapshot it verified is no longer in the repository")
	}

	if run.VerificationStatus != verificationPassed {
		return r.finish(ctx, run, state.PhaseFailed, VerdictVerificationFailed,
			fmt.Sprintf("this run reached the catalog commit with verification %q, which is not a pass", run.VerificationStatus))
	}

	if err := r.move(ctx, &run, state.PhaseSuccess, state.SnapshotRunUpdate{}); err != nil {
		return nil, err
	}

	return []Verdict{{
		Kind:       VerdictCommitCompleted,
		RunID:      run.RunID,
		SnapshotID: run.SnapshotID,
		From:       state.PhaseCatalogCommit,
		To:         state.PhaseSuccess,
		Reason:     "this run was verified before it was interrupted, so its restore point was committed rather than re-proven",
	}}, nil
}

// resolveFinished decides the two things that can happen to a run that
// already ended: the delete this product asked for, and the snapshot that
// went away on its own.
func (r *Reconciler) resolveFinished(
	ctx context.Context,
	run state.SnapshotRun,
	present map[string]backupengine.SnapshotInfo,
) ([]Verdict, error) {
	// A row with no snapshot id has nothing in the repository to compare
	// against: a failed pre-upload run, or a quarantine row this pass
	// has not finished writing. Neither is a question for this function.
	if run.SnapshotID == "" {
		return nil, nil
	}

	// A quarantine row is not one of this manager's runs. It records
	// something FOUND, so "the snapshot it names has gone" is not a lost
	// restore point -- nothing ever advertised it -- and re-deciding it
	// every cycle would turn a standing condition into a stream of
	// events.
	if run.Phase == state.PhaseQuarantined {
		return nil, nil
	}

	_, stillThere := present[run.SnapshotID]

	if run.DeleteRequestedAt != nil && run.Phase != state.PhaseDeleted {
		if stillThere {
			return []Verdict{{
				Kind:       VerdictDeletePending,
				RunID:      run.RunID,
				SnapshotID: run.SnapshotID,
				From:       run.Phase,
				To:         run.Phase,
				Reason:     "a delete was recorded for this snapshot and the repository still holds it; retention owns issuing it",
			}}, nil
		}

		return r.finish(ctx, run, state.PhaseDeleted, VerdictDeleteCompleted,
			"the delete recorded for this snapshot had already been carried out when this pass looked")
	}

	if run.Phase == state.PhaseSuccess && !stillThere {
		return r.finish(ctx, run, state.PhaseLost, VerdictRestorePointLost,
			"this run's snapshot is no longer in the repository, and no delete of it was ever recorded")
	}

	return nil, nil
}

// quarantineUnattributed records every repository snapshot under this
// set's source identity that no catalog row accounts for.
//
// The keyed lookup, rather than a scan of the rows this pass happens to
// have read, is what makes the answer right for a snapshot older than the
// history window: the question "is this manifest one of ours" has a
// unique-index answer and does not depend on how far back a pass looked.
func (r *Reconciler) quarantineUnattributed(
	ctx context.Context,
	req ReconcileRequest,
	present map[string]backupengine.SnapshotInfo,
) ([]Verdict, error) {
	var verdicts []Verdict

	for _, id := range sortedIDs(present) {
		_, err := r.Catalog.SnapshotRunBySnapshotID(ctx, req.Domain.String(), id)
		switch {
		case err == nil:
			continue
		case errors.Is(err, state.ErrSnapshotRunNotFound):
		default:
			return verdicts, fmt.Errorf("snapshotlifecycle: asking whether snapshot %s is ours: %w", id, err)
		}

		v, err := r.quarantine(ctx, req, present[id])
		if err != nil {
			return verdicts, err
		}

		if v != nil {
			verdicts = append(verdicts, *v)
		}
	}

	return verdicts, nil
}

// quarantine opens a row for an unattributable snapshot and records the
// verdict on it.
//
// The row's identity is derived from the repository domain and the
// snapshot id, so a pass that runs every cycle over a repository holding
// a snapshot nobody will ever claim records it ONCE. Without that, a
// reconciliation loop would write a new quarantine row per cycle for
// ever, which is a disk-filling bug dressed up as diligence.
func (r *Reconciler) quarantine(ctx context.Context, req ReconcileRequest, info backupengine.SnapshotInfo) (*Verdict, error) {
	id := quarantineRunID(req.Domain, string(info.ID))

	outcome, err := r.Catalog.BeginSnapshotRun(ctx, state.SnapshotRunRequest{
		RunID:          id,
		IdempotencyKey: id,
		Set:            req.Set,
		Engine:         model.EngineKopia.String(),
		Domain:         req.Domain.String(),
		SourceIdentity: sourceIdentityOf(req),
		StartedAt:      r.now(),
		// A quarantine row makes no verification claim and asks for
		// none: it is a record of something found, not of something
		// this manager did.
		VerificationLevel: string(model.LevelStructural),
	})
	if err != nil {
		return nil, fmt.Errorf("snapshotlifecycle: opening a quarantine row for snapshot %s: %w", info.ID, err)
	}

	if !outcome.Created {
		// Already quarantined by an earlier pass. Reporting it again
		// every cycle would make a standing, unresolved condition look
		// like a new event each time.
		return nil, nil
	}

	run := outcome.Run
	reason := fmt.Sprintf(
		"this snapshot is in repository %s under backup set %s's source identity, and no catalog row accounts for it; "+
			"it has been quarantined rather than deleted, because a snapshot this manager cannot explain may still be somebody's only copy",
		req.Domain, req.Set)

	if err := r.move(ctx, &run, state.PhaseQuarantined, state.SnapshotRunUpdate{
		SnapshotID: new(string(info.ID)),
		Reason:     &reason,
	}); err != nil {
		return nil, err
	}

	return &Verdict{
		Kind:       VerdictQuarantined,
		RunID:      run.RunID,
		SnapshotID: string(info.ID),
		From:       state.PhasePending,
		To:         state.PhaseQuarantined,
		Reason:     reason,
	}, nil
}

// repointLastKnownGood moves the last-known-good flag to the newest
// successful run whose snapshot is still in the repository, when the set
// has no flagged run any more.
//
// It runs after the row-level verdicts, because the pass above is what
// takes the flag away: advancing a row to LOST clears it, which is
// correct and which would otherwise leave a set with a perfectly good
// older snapshot reporting no restore point at all.
func (r *Reconciler) repointLastKnownGood(
	ctx context.Context,
	req ReconcileRequest,
	present map[string]backupengine.SnapshotInfo,
) ([]Verdict, error) {
	_, err := r.Catalog.LastKnownGoodSnapshot(ctx, req.Set)
	switch {
	case err == nil:
		return nil, nil
	case errors.Is(err, state.ErrSnapshotRunNotFound):
	default:
		return nil, fmt.Errorf("snapshotlifecycle: reading %s's last-known-good snapshot: %w", req.Set, err)
	}

	runs, err := r.Catalog.ListSnapshotRuns(ctx, req.Set, r.historyLimit())
	if err != nil {
		return nil, fmt.Errorf("snapshotlifecycle: reading %s's snapshot history: %w", req.Set, err)
	}

	for _, run := range runs {
		if run.Phase != state.PhaseSuccess || run.SnapshotID == "" {
			continue
		}

		if _, ok := present[run.SnapshotID]; !ok {
			continue
		}

		if err := r.Catalog.RepointLastKnownGood(ctx, req.Set, run.RunID); err != nil {
			return nil, fmt.Errorf("snapshotlifecycle: re-pointing %s's last-known-good snapshot at run %s: %w", req.Set, run.RunID, err)
		}

		return []Verdict{{
			Kind:       VerdictLastKnownGoodRepointed,
			RunID:      run.RunID,
			SnapshotID: run.SnapshotID,
			From:       state.PhaseSuccess,
			To:         state.PhaseSuccess,
			Reason:     "the newest restore point is gone, so last-known-good was moved to the newest one the repository still holds",
		}}, nil
	}

	return nil, nil
}

// maintenanceVerdict reports an interrupted maintenance attempt.
//
// # Why an interrupted maintenance needs no repair
//
// Maintenance rewrites indexes and reclaims content nothing references.
// It never removes content a snapshot points at, and the engine's own
// fencing is what guarantees that across an interruption, so a maintenance
// run that was killed leaves unreferenced blobs behind and nothing else:
// no catalog row changes meaning, no manifest becomes invalid, and no
// restore point is affected. The next maintenance window reclaims what the
// last one did not.
//
// So the rule here is deliberately to CHANGE NOTHING and say so. What
// would be dangerous is the opposite reflex -- reading "maintenance did
// not finish" as "the repository may be damaged" and starting to verify or
// delete things -- because that turns a harmless interruption into a pass
// that writes.
//
// The evidence is the record's own last result: a maintenance attempt
// whose outcome was recorded as a failure. A process killed before it
// could record anything leaves the record untouched, which is
// indistinguishable from no maintenance having been attempted, and reading
// it as exactly that is both the only available answer and the safe one.
func maintenanceVerdict(req ReconcileRequest) (Verdict, bool) {
	if req.Maintenance == nil || req.Maintenance.LastResult.Err == "" {
		return Verdict{}, false
	}

	return Verdict{
		Kind:   VerdictMaintenanceInterrupted,
		Reason: fmt.Sprintf("the last %s maintenance of repository %s did not finish cleanly (%s); no snapshot and no catalog row was changed by this pass, and the next maintenance window reclaims whatever it left behind", req.Maintenance.LastResult.Mode, req.Domain, req.Maintenance.LastResult.Err),
	}, true
}

// orphanFor looks for a manifest in the repository that this interrupted
// run must have produced.
//
// The claim has to be made carefully, because adopting the wrong manifest
// onto a row would attribute somebody else's snapshot to this run. Two
// conditions have to hold: the manifest is not already claimed by any
// catalog row, and it was started at or after this run was (a snapshot
// under this set's source identity that predates the run cannot be its
// output). If more than one candidate matches, none is adopted: two
// unclaimed manifests inside one run's window is not a situation to guess
// at, and the run fails while both manifests stay in place for the
// quarantine pass to name.
func (r *Reconciler) orphanFor(
	ctx context.Context,
	req ReconcileRequest,
	run state.SnapshotRun,
	present map[string]backupengine.SnapshotInfo,
) (backupengine.SnapshotInfo, bool) {
	var (
		found backupengine.SnapshotInfo
		count int
	)

	for _, id := range sortedIDs(present) {
		info := present[id]

		if info.Start.Before(run.StartedAt) {
			continue
		}

		if _, err := r.Catalog.SnapshotRunBySnapshotID(ctx, req.Domain.String(), id); err == nil {
			continue
		}

		found, count = info, count+1
	}

	if count != 1 {
		return backupengine.SnapshotInfo{}, false
	}

	return found, true
}

// adopt records a manifest onto the row that produced it.
func (r *Reconciler) adopt(ctx context.Context, run state.SnapshotRun, info backupengine.SnapshotInfo) ([]Verdict, error) {
	// Only what the manifest itself can answer is recorded. The bytes a
	// run READ off the source and the bytes it WROTE to storage are
	// measurements the dead process took and did not persist, and
	// inventing them from the manifest's logical size would report a
	// deduplicated snapshot as having uploaded its whole tree, which is
	// the one arithmetic EPIC K forbids. They stay zero, which reads as
	// "not measured".
	if err := r.move(ctx, &run, state.PhaseManifestCommitted, state.SnapshotRunUpdate{
		SnapshotID:   new(string(info.ID)),
		Files:        &info.Files,
		Directories:  &info.Directories,
		LogicalBytes: &info.Bytes,
	}); err != nil {
		return nil, err
	}

	return []Verdict{{
		Kind:       VerdictManifestAdopted,
		RunID:      run.RunID,
		SnapshotID: string(info.ID),
		From:       state.PhaseSnapshotWrite,
		To:         state.PhaseManifestCommitted,
		Reason:     "the repository held an unclaimed snapshot this run must have written, so the row adopted it instead of leaving it unattributable",
	}}, nil
}

// finish records a terminal phase and returns the verdict for it.
func (r *Reconciler) finish(
	ctx context.Context,
	run state.SnapshotRun,
	to state.SnapshotPhase,
	kind VerdictKind,
	reason string,
) ([]Verdict, error) {
	from := run.Phase
	bounded := truncateReason(reason)

	upd := state.SnapshotRunUpdate{Reason: &bounded}
	if kind == VerdictVerificationFailed {
		upd.VerificationStatus = new(verificationFailed)
	}

	if err := r.move(ctx, &run, to, upd); err != nil {
		return nil, err
	}

	return []Verdict{{
		Kind:       kind,
		RunID:      run.RunID,
		SnapshotID: run.SnapshotID,
		From:       from,
		To:         to,
		Reason:     bounded,
	}}, nil
}

// move is the reconciler's durable write, validated through the same
// graph a live run is validated through. One table, one validator, two
// callers: a recovery path that could take an edge a run cannot would be a
// second state machine.
func (r *Reconciler) move(ctx context.Context, run *state.SnapshotRun, to state.SnapshotPhase, upd state.SnapshotRunUpdate) error {
	from := run.Phase
	if err := Validate(from, to); err != nil {
		return err
	}

	at := r.now()
	upd.At = at

	if err := r.Catalog.AdvanceSnapshotRun(ctx, run.RunID, to, upd); err != nil {
		return fmt.Errorf("snapshotlifecycle: reconciling %s from %s to %s: %w", run.RunID, from, to, err)
	}

	run.Phase = to
	applyUpdate(run, upd)

	if r.Observer != nil {
		r.Observer.ObservePhase(run.Set, run.RunID, from, to, at)
	}

	return nil
}

// reload re-reads a row this pass has already changed, falling back to
// what it holds if the read fails: the pass has already made its durable
// write, and failing the whole set's reconciliation over a read is worse
// than continuing from a row that is one field stale.
func (r *Reconciler) reload(ctx context.Context, runID string, fallback state.SnapshotRun) state.SnapshotRun {
	run, err := r.Catalog.GetSnapshotRun(ctx, runID)
	if err != nil {
		return fallback
	}

	return run
}

func (r *Reconciler) historyLimit() int {
	if r.HistoryLimit > 0 {
		return r.HistoryLimit
	}

	return DefaultHistoryLimit
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}

	return time.Now().UTC()
}

// quarantineRunID derives the stable row id for an unattributable
// snapshot. Stable, because the whole point is that a second pass over
// the same repository recognises the row it already wrote.
func quarantineRunID(domain model.RepositoryDomainID, snapshotID string) string {
	return "quarantine:" + domain.String() + ":" + snapshotID
}

// sourceIdentityOf is the identity a quarantine row records.
//
// The snapshot was listed under this set's source, which is what makes
// the set the right place to file it, so the set's own identity is what
// goes on the row -- model.SourceIdentity, a digest, never the source's
// path. That is what keeps a source's directory structure out of the
// catalog and out of every surface that renders one, and it is why this
// is a function with a comment rather than a field read at the call
// site: req.Source.Path is right there, it is the wrong value, and it
// would look correct for ever.
func sourceIdentityOf(req ReconcileRequest) string {
	return req.SourceIdentity.String()
}

// sortedIDs is the repository's snapshot ids in a stable order.
//
// Determinism is the requirement, not aesthetics: a reconciliation pass
// that adopted or quarantined in map-iteration order would make two
// passes over the same disagreement produce two different sets of
// verdicts, and "deterministic per boundary" is the acceptance criterion
// this whole file is written against.
func sortedIDs(present map[string]backupengine.SnapshotInfo) []string {
	out := make([]string, 0, len(present))
	for id := range present {
		out = append(out, id)
	}

	sort.Strings(out)

	return out
}
