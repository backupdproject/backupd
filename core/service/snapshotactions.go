package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/backupdproject/backupd/core/apicontract"
	"github.com/backupdproject/backupd/core/internal/app"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/state"
)

// EPIC K's three remaining mutating acts, on the one route that already
// starts durable work (#788): verify a snapshot, hold one, release a
// hold.
//
// # Why all three are operations rather than routes of their own
//
// Because /operations is where this product already answers "how does a
// caller start something that outlives the request, exactly once, against
// a configuration it agrees is current". A second answer would drift, and
// the drift would be in idempotency, which is the one property nobody
// notices is missing until a retry has done something twice.
//
// A hold is not long-running, and it is still an operation. The reason is
// the retry, not the duration: a browser that resubmits a hold after a
// dropped connection must place one hold, and the idempotency key on this
// route is the mechanism that already exists for saying so. The work is a
// single journal write, so it happens INSIDE the submission and the row
// comes back already completed -- a caller polling gets its answer on the
// first read rather than being told to come back for a write that has
// finished.
//
// A verification is genuinely long, so it rides a goroutine exactly as a
// restore does, and for the same reasons that file argues.

const (
	// ActionVerifySnapshot, ActionHoldSnapshot and
	// ActionReleaseSnapshotHold are defined from core/apicontract's
	// constants so the wire value, the durable row's value and the
	// echoed command all read one spelling.
	ActionVerifySnapshot      = apicontract.ActionVerifySnapshot
	ActionHoldSnapshot        = apicontract.ActionHoldSnapshot
	ActionReleaseSnapshotHold = apicontract.ActionReleaseSnapshotHold
)

// SnapshotVerifyRequest is one operator's ask that a restore point be
// proven readable now.
type SnapshotVerifyRequest struct {
	Actor          string
	IdempotencyKey string
	ConfigRevision string

	BackupSetID string

	// RunID selects the run to verify. Empty means the set's
	// last-known-good snapshot.
	RunID string

	// Level is "structural", "content_sample", "content_full" or
	// "restore_drill". Empty means the level the set is configured for.
	Level string

	// SamplePercent overrides the configured sample size, 1 to 100. Zero
	// means the configured one.
	SamplePercent int
}

// SnapshotHoldRequest is one operator's ask that a snapshot not be
// deleted.
type SnapshotHoldRequest struct {
	Actor          string
	IdempotencyKey string
	ConfigRevision string

	BackupSetID string

	// RunID is the run whose snapshot is held. Empty means the set's
	// last-known-good snapshot, which is what an operator protecting
	// "the current restore point" means.
	RunID string

	// Reason is required. A hold nobody explained is one nobody dares
	// release, which makes it permanent by accident.
	Reason string
}

// SnapshotHoldReleaseRequest ends one hold.
type SnapshotHoldReleaseRequest struct {
	Actor          string
	IdempotencyKey string
	ConfigRevision string

	BackupSetID string
	HoldID      string
}

// snapshotVerifyParameters, snapshotHoldParameters and
// snapshotHoldReleaseParameters are what the durable rows record about
// each request, so a row read back after a restart still says what was
// asked for.
type snapshotVerifyParameters struct {
	BackupSetID   string `json:"backup_set_id"`
	RunID         string `json:"run_id,omitempty"`
	Level         string `json:"level,omitempty"`
	SamplePercent int    `json:"sample_percent,omitempty"`
}

type snapshotHoldParameters struct {
	BackupSetID string `json:"backup_set_id"`
	RunID       string `json:"run_id,omitempty"`
	Reason      string `json:"reason"`
}

type snapshotHoldReleaseParameters struct {
	BackupSetID string `json:"backup_set_id"`
	HoldID      string `json:"hold_id"`
}

// SubmitSnapshotVerify persists a verification operation and starts it.
func (b *BackupService) SubmitSnapshotVerify(ctx context.Context, req SnapshotVerifyRequest) (Operation, error) {
	sourceName, setName, st, err := b.incrementalSubmission(ActionVerifySnapshot, req.IdempotencyKey, req.ConfigRevision, req.BackupSetID)
	if err != nil {
		return Operation{}, err
	}

	level := model.VerificationLevel("")
	if req.Level != "" {
		parsed, perr := model.ParseVerificationLevel(req.Level)
		if perr != nil {
			// This package's own sentence, naming only the value the
			// caller sent, which is what makes it safe to echo back.
			return Operation{}, fmt.Errorf("%w: %q is not a verification level; use structural, content_sample, content_full or restore_drill",
				ErrInvalidRequest, req.Level)
		}

		level = parsed
	}

	if req.SamplePercent < 0 || req.SamplePercent > 100 {
		return Operation{}, fmt.Errorf("%w: a verification sample is a percentage of files, 1 to 100", ErrInvalidRequest)
	}

	op, created, err := b.createSnapshotOperation(ctx, st.revision, ActionVerifySnapshot, req.Actor, req.IdempotencyKey, req.BackupSetID,
		snapshotVerifyParameters{
			BackupSetID:   req.BackupSetID,
			RunID:         req.RunID,
			Level:         req.Level,
			SamplePercent: req.SamplePercent,
		})
	if err != nil || !created {
		return op, err
	}

	b.wg.Add(1)

	go b.executeSnapshotVerify(op.ID, app.VerifySnapshotRequest{
		SourceName:    sourceName,
		SetName:       setName,
		RunID:         req.RunID,
		Level:         level,
		SamplePercent: req.SamplePercent,
	})

	return op, nil
}

// SubmitSnapshotHold places one hold and records the operation that did
// it.
//
// The hold happens inside the submission rather than on a goroutine, for
// this file's own reason: it is one journal write, and an operation that
// came back "queued" for work that has already finished would make every
// client poll for an answer it already had.
func (b *BackupService) SubmitSnapshotHold(ctx context.Context, req SnapshotHoldRequest) (Operation, error) {
	sourceName, setName, st, err := b.incrementalSubmission(ActionHoldSnapshot, req.IdempotencyKey, req.ConfigRevision, req.BackupSetID)
	if err != nil {
		return Operation{}, err
	}

	if req.Reason == "" {
		return Operation{}, fmt.Errorf("%w: a hold requires a reason; a hold nobody explained is one nobody dares release", ErrInvalidRequest)
	}

	op, created, err := b.createSnapshotOperation(ctx, st.revision, ActionHoldSnapshot, req.Actor, req.IdempotencyKey, req.BackupSetID,
		snapshotHoldParameters{BackupSetID: req.BackupSetID, RunID: req.RunID, Reason: req.Reason})
	if err != nil || !created {
		return op, err
	}

	actor := req.Actor
	if actor == "" {
		actor = "api"
	}

	hold, err := st.inner.PlaceSnapshotHold(ctx, app.PlaceSnapshotHoldRequest{
		SourceName: sourceName,
		SetName:    setName,
		RunID:      req.RunID,
		// The operation's own id is the hold id, which is what makes a
		// retried submission place one hold: the operation route already
		// resolved the retry to one row, and deriving the hold's identity
		// from it means the two can never disagree about how many holds
		// one request placed.
		HoldID:   "hold_" + op.ID,
		Reason:   req.Reason,
		PlacedBy: actor,
		At:       now(),
	})
	if err != nil {
		return b.failSnapshotOperation(ctx, op, snapshotHoldFailure(err)), snapshotSurfaceError(req.BackupSetID, err)
	}

	return b.completeSnapshotOperation(ctx, op, mustJSON(snapshotHoldSummary{
		BackupSetID: req.BackupSetID,
		HoldID:      hold.HoldID,
		RunID:       hold.RunID,
		Reason:      hold.Reason,
		PlacedBy:    hold.PlacedBy,
	})), nil
}

// SubmitSnapshotHoldRelease ends one hold, recording the operation that
// did it.
func (b *BackupService) SubmitSnapshotHoldRelease(ctx context.Context, req SnapshotHoldReleaseRequest) (Operation, error) {
	sourceName, setName, st, err := b.incrementalSubmission(ActionReleaseSnapshotHold, req.IdempotencyKey, req.ConfigRevision, req.BackupSetID)
	if err != nil {
		return Operation{}, err
	}

	if req.HoldID == "" {
		return Operation{}, fmt.Errorf("%w: releasing a hold requires the hold's id", ErrInvalidRequest)
	}

	op, created, err := b.createSnapshotOperation(ctx, st.revision, ActionReleaseSnapshotHold, req.Actor, req.IdempotencyKey, req.BackupSetID,
		snapshotHoldReleaseParameters{BackupSetID: req.BackupSetID, HoldID: req.HoldID})
	if err != nil || !created {
		return op, err
	}

	actor := req.Actor
	if actor == "" {
		actor = "api"
	}

	if err := st.inner.ReleaseSnapshotHold(ctx, sourceName, setName, req.HoldID, actor, now()); err != nil {
		return b.failSnapshotOperation(ctx, op, "that hold could not be released"), snapshotSurfaceError(req.BackupSetID, err)
	}

	return b.completeSnapshotOperation(ctx, op, mustJSON(snapshotHoldReleaseParameters{
		BackupSetID: req.BackupSetID,
		HoldID:      req.HoldID,
	})), nil
}

// snapshotHoldSummary is what a completed hold records.
type snapshotHoldSummary struct {
	BackupSetID string `json:"backup_set_id"`
	HoldID      string `json:"hold_id"`
	RunID       string `json:"run_id"`
	Reason      string `json:"reason"`
	PlacedBy    string `json:"placed_by"`
}

// snapshotVerifySummary is what a completed verification records.
//
// Achieved is separate from Requested for the reason the catalog keeps
// two columns: a check that asked for a restore drill and managed a
// content read has to read as exactly that.
type snapshotVerifySummary struct {
	BackupSetID     string  `json:"backup_set_id"`
	RunID           string  `json:"run_id"`
	SnapshotID      string  `json:"snapshot_id"`
	Requested       string  `json:"verification_level"`
	Achieved        string  `json:"verification_level_achieved"`
	ObjectsVerified int64   `json:"objects_verified"`
	FilesVerified   int64   `json:"files_verified"`
	BytesVerified   int64   `json:"bytes_verified"`
	BlobsChecked    int64   `json:"blobs_checked"`
	Findings        int     `json:"findings"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// incrementalSubmission performs every check the three submissions share,
// in the order that keeps a refusable request from ever writing a row.
//
// One atomic read of the configuration state, for SubmitRunBackupSet's
// reason: the revision this call checks against, records, and resolves
// the set against all have to be the same picture.
func (b *BackupService) incrementalSubmission(action, key, revision, backupSetID string) (string, string, *configState, error) {
	if key == "" {
		return "", "", nil, fmt.Errorf("%w: %s request requires an idempotency key", ErrInvalidRequest, action)
	}

	if revision == "" {
		return "", "", nil, fmt.Errorf("%w: %s request requires a configuration revision", ErrInvalidRequest, action)
	}

	sourceName, setName, ok := splitBackupSetID(backupSetID)
	if !ok {
		return "", "", nil, fmt.Errorf("%w: %s", ErrBackupSetNotFound, backupSetID)
	}

	st := b.state.Load()
	if revision != st.revision {
		return "", "", nil, fmt.Errorf("%w: request carries %q, current is %q", ErrConfigRevisionStale, revision, st.revision)
	}

	set, ok := configuredBackupSet(st.inner, sourceName, setName)
	if !ok {
		return "", "", nil, fmt.Errorf("%w: %s", ErrBackupSetNotFound, backupSetID)
	}

	// Refused here rather than by the engine, for SubmitSnapshotRestore's
	// reason: an artifact set has no snapshot to verify or hold, and
	// letting the request through would write a durable row for work that
	// fails as soon as it starts.
	if set.Engine != model.EngineKopia {
		return "", "", nil, fmt.Errorf("%w: %s", ErrSnapshotRestoreUnsupported, backupSetID)
	}

	return sourceName, setName, st, nil
}

// createSnapshotOperation writes the durable row, reporting whether this
// call is the one that created it.
func (b *BackupService) createSnapshotOperation(
	ctx context.Context,
	revision, action, actor, key, backupSetID string,
	parameters any,
) (Operation, bool, error) {
	outcome, err := b.journal.CreateOperation(ctx, state.OperationRequest{
		OperationID:    "op_" + uuid.New().String(),
		IdempotencyKey: key,
		Actor:          actor,
		BackupSet:      backupSetID,
		ConfigRevision: revision,
		Action:         action,
		Parameters:     mustJSON(parameters),
		CreatedAt:      now(),
	})
	if err != nil {
		if errors.Is(err, state.ErrOperationIdempotencyKeyReused) {
			return Operation{}, false, fmt.Errorf("%w: idempotency key already used for a different request", ErrIdempotencyKeyConflict)
		}

		// Deliberately not %w-wrapped: err may carry a state-layer
		// sentence naming SQLite internals, and nothing from that
		// vocabulary crosses this boundary.
		return Operation{}, false, fmt.Errorf("service: submit %s: an internal error occurred", action)
	}

	return toOperation(outcome.Operation), outcome.Created, nil
}

// completeSnapshotOperation records a finished synchronous act and
// returns the row as it now reads.
//
// A failure to record is logged and not returned: the act itself
// happened, and reporting it as failed because the receipt could not be
// written would send an operator to undo something that worked.
func (b *BackupService) completeSnapshotOperation(ctx context.Context, op Operation, summary string) Operation {
	if err := b.journal.CompleteOperation(ctx, op.ID, now(), summary); err != nil {
		b.logger.Error(ctx, "complete-operation", err)

		return op
	}

	op.Status = "completed"
	op.Result = summary
	op.FinishedAt = now()

	return op
}

// failSnapshotOperation records a synchronous act that did not happen.
func (b *BackupService) failSnapshotOperation(ctx context.Context, op Operation, reason string) Operation {
	if err := b.journal.FailOperation(ctx, op.ID, now(), reason); err != nil {
		b.logger.Error(ctx, "fail-operation", err)

		return op
	}

	op.Status = "failed"
	op.Error = reason
	op.FinishedAt = now()

	return op
}

// executeSnapshotVerify is the asynchronous half of a verification.
//
// The panic recovery is not decoration: this runs on a goroutine inside
// an always-on process, so an unrecovered panic here takes down the API
// server rather than one command.
func (b *BackupService) executeSnapshotVerify(operationID string, req app.VerifySnapshotRequest) {
	defer b.wg.Done()

	defer func() {
		if r := recover(); r != nil {
			b.logger.Error(context.Background(), "execute-snapshot-verify-panic", fmt.Errorf("recovered panic: %v", r))

			if err := b.journal.FailOperation(context.Background(), operationID, now(),
				"an internal error occurred while running this operation"); err != nil {
				b.logger.Error(context.Background(), "fail-operation-after-panic", err)
			}
		}
	}()

	if err := b.journal.MarkOperationRunning(context.Background(), operationID, now()); err != nil {
		b.logger.Error(context.Background(), "mark-operation-running", err)

		return
	}

	result, err := verifySnapshot(b.state.Load().inner, b.ctx, req)
	if err != nil {
		b.logger.Error(context.Background(), "snapshot-verify", err)

		if failErr := b.journal.FailOperation(context.Background(), operationID, now(),
			snapshotVerifyFailure(err)); failErr != nil {
			b.logger.Error(context.Background(), "fail-operation", failErr)
		}

		return
	}

	// A verification that ran and did not pass is a FAILED operation, not
	// a completed one carrying a quiet field. The whole point of asking
	// is to be told, and an operator scanning a list of operations reads
	// the status column.
	if !result.Passed {
		if failErr := b.journal.FailOperation(context.Background(), operationID, now(),
			verificationFailureSentence(result)); failErr != nil {
			b.logger.Error(context.Background(), "fail-operation", failErr)
		}

		return
	}

	if err := b.journal.CompleteOperation(context.Background(), operationID, now(),
		summarizeSnapshotVerify(req, result)); err != nil {
		b.logger.Error(context.Background(), "complete-operation", err)
	}
}

// verifySnapshot is a seam over (*app.Service).VerifySnapshot, exactly
// like restoreSnapshot is one over RestoreSnapshot and for the same
// reason. Nothing overrides it in production.
var verifySnapshot = func(inner *app.Service, ctx context.Context, req app.VerifySnapshotRequest) (app.VerifySnapshotResult, error) {
	return inner.VerifySnapshot(ctx, req)
}

// verificationFailureSentence says what a verification that ran found.
//
// It names the count and not the findings themselves: a finding can carry
// an entry's path out of somebody's source tree, and this string is read
// back by clients and pasted into support conversations.
func verificationFailureSentence(result app.VerifySnapshotResult) string {
	if len(result.Report.Errors) == 0 {
		return "this verification did not complete, so nothing about this restore point has been proven"
	}

	return fmt.Sprintf("this verification found %d problem(s) with the restore point; the details are in this deployment's log",
		len(result.Report.Errors))
}

// snapshotVerifyFailure turns a verification's own failure into the
// sentence the durable row carries. Everything unclassified is one
// generic sentence, because an error from below can name a filesystem
// path or a storage-layer type.
func snapshotVerifyFailure(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "the verification was stopped before it finished, so nothing about this restore point has been proven"
	case errors.Is(err, app.ErrSetHasNoSnapshots):
		return "this backup set has no restore point to verify"
	case errors.Is(err, app.ErrSnapshotNotFound):
		return "that snapshot is not in this backup set's history"
	case errors.Is(err, app.ErrNotAnIncrementalSet):
		return "this backup set stores artifacts rather than snapshots"
	default:
		return "this verification did not finish"
	}
}

// snapshotHoldFailure turns the journal's refusals into the row's
// sentence. Every one of these is a state an operator reaches by clicking
// a button on a screen that has moved on.
func snapshotHoldFailure(err error) string {
	switch {
	case errors.Is(err, app.ErrSnapshotNotFound):
		return "that snapshot is not in this backup set's history"
	case errors.Is(err, app.ErrSetHasNoSnapshots):
		return "this backup set has no restore point to hold"
	case errors.Is(err, app.ErrSnapshotNotHoldable):
		// The app layer's own sentence, for snapshotSurfaceError's
		// reason: it is composed there from the run's phase alone, so it
		// carries no path, and which of the four states this was is the
		// only part an operator reading the operation row later can do
		// anything with.
		return notHoldableSentence(err)
	default:
		return "that snapshot could not be held"
	}
}

func summarizeSnapshotVerify(req app.VerifySnapshotRequest, result app.VerifySnapshotResult) string {
	return mustJSON(snapshotVerifySummary{
		BackupSetID:     req.SourceName + "/" + req.SetName,
		RunID:           result.RunID,
		SnapshotID:      result.SnapshotID,
		Requested:       string(result.Requested),
		Achieved:        string(result.Achieved),
		ObjectsVerified: result.Report.ObjectsVerified,
		FilesVerified:   result.Report.FilesVerified,
		BytesVerified:   result.Report.BytesVerified,
		BlobsChecked:    result.Report.BlobsChecked,
		Findings:        len(result.Report.Errors),
		DurationSeconds: result.Duration.Seconds(),
	})
}

// mustJSON renders a parameter or summary struct.
//
// It cannot fail against the structs in this file, which are flat records
// of strings and numbers, so a failure is a bug here rather than a
// runtime condition and an empty object is a better outcome than a
// refused operation.
func mustJSON(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}

	return string(out)
}
