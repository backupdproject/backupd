package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/backupdproject/backupd/core/apicontract"
	"github.com/backupdproject/backupd/core/internal/app"
	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/state"
)

// Restoring a snapshot as a durable operation (EPIC K, #787).
//
// # Why it is an operation rather than a call that returns the tree
//
// Because a restore takes as long as the data is big. A request that
// waited for it would be a request that dies with the connection, on the
// day somebody is restoring, and the receipt for what they asked for
// would die with it: the row is what survives a closed tab, a proxy
// timeout and this process being restarted, and it is the same receipt
// run_cycle and restore_placement already hand out.
//
// # It is not restore_placement, and the two never merge
//
// restore_placement asks a storage provider to make an archived object
// readable again: nothing here executes it, nothing here can cancel it,
// it costs money, and its true state is re-derived from the provider on
// every read. This one is executed HERE, by this process, against a
// repository this deployment holds, and a process that dies really did
// abandon it -- which is why it is deliberately NOT in
// externallyExecutedActions and IS swept at startup.
//
// # No single-flight lock
//
// A restore does not take b.runOnce. A backup cycle and a restore do not
// contend for anything: the restore reads a repository and writes to a
// directory outside the backup pipeline, and the moment an operator is
// most likely to want one is during an incident, which is exactly when a
// scheduled cycle is also running. Refusing then would be a safety rule
// with no safety in it.

// ActionRestoreSnapshot is POST /api/v1/operations' local snapshot
// restore: one restore point, or one directory or file inside it, written
// to a directory on this machine.
const ActionRestoreSnapshot = apicontract.ActionRestoreSnapshot

// ErrSnapshotRestoreUnsupported is returned when the set named does not
// store snapshots at all.
//
// An artifact set's restore is restore_placement against a storage
// medium, which is a different act with a different cost, so answering
// this request with that one is refused rather than redirected.
var ErrSnapshotRestoreUnsupported = errors.New("service: this backup set does not store snapshots")

// SnapshotRestoreRequest is one operator's ask for one restore point to
// be written to a local directory.
//
// Every field is a plain string, including Conflict: this type crosses
// the boundary out of core/, so it carries no type from core/internal,
// and the policy is resolved through backupengine.ParseRestoreConflict
// before a row is written rather than after.
type SnapshotRestoreRequest struct {
	Actor          string
	IdempotencyKey string
	ConfigRevision string

	// BackupSetID is the source/set pair every surface in this product
	// prints.
	BackupSetID string

	// SnapshotID names the restore point. Empty means the set's
	// last-known-good one, resolved at the moment the restore runs.
	SnapshotID string

	// SourcePath is what inside the snapshot to restore, slash
	// separated. Empty means the whole snapshot.
	SourcePath string

	// TargetPath is the local directory to restore into.
	TargetPath string

	// Conflict is "refuse" (the default), "skip" or "overwrite".
	Conflict string
}

// snapshotRestoreParameters is what the durable row records about the
// request, so that a row read back after a restart still says what was
// asked for.
//
// The destination is recorded deliberately. It is a path the operator
// themselves named, it carries no credential, and "where did that restore
// put my files" is the first question anybody asks about a restore that
// finished while they were not looking.
type snapshotRestoreParameters struct {
	BackupSetID string `json:"backup_set_id"`
	SnapshotID  string `json:"snapshot_id,omitempty"`
	SourcePath  string `json:"source_path,omitempty"`
	TargetPath  string `json:"target_path"`
	Conflict    string `json:"conflict"`
}

// SubmitSnapshotRestore persists a restore operation and starts executing
// it, with the same durability contract SubmitRunCycle has: the row
// exists before anything runs, the answer outlives the request, and
// execution rides on this service's own lifetime rather than the
// caller's.
//
// Everything refusable is refused BEFORE the row is written, so a request
// this deployment cannot serve leaves no operation for somebody to poll
// forever: a stale configuration revision, an unknown backup set, a set
// that stores artifacts rather than snapshots, a missing destination, and
// a conflict policy that is not one of the three.
func (b *BackupService) SubmitSnapshotRestore(ctx context.Context, req SnapshotRestoreRequest) (Operation, error) {
	if req.IdempotencyKey == "" {
		return Operation{}, fmt.Errorf("%w: %s request requires an idempotency key", ErrInvalidRequest, ActionRestoreSnapshot)
	}

	if req.ConfigRevision == "" {
		return Operation{}, fmt.Errorf("%w: %s request requires a configuration revision", ErrInvalidRequest, ActionRestoreSnapshot)
	}

	if req.TargetPath == "" {
		return Operation{}, fmt.Errorf("%w: %s request requires a directory to restore into", ErrInvalidRequest, ActionRestoreSnapshot)
	}

	conflict, err := backupengine.ParseRestoreConflict(req.Conflict)
	if err != nil {
		// The sentence is this boundary's own and names only the value
		// the caller sent, which is what makes it safe to echo back.
		return Operation{}, fmt.Errorf("%w: %q is not a restore conflict policy; use refuse, skip or overwrite",
			ErrInvalidRequest, req.Conflict)
	}

	sourceName, setName, ok := splitBackupSetID(req.BackupSetID)
	if !ok {
		return Operation{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, req.BackupSetID)
	}

	// One atomic read, for the reason SubmitRunBackupSet takes one: the
	// revision this call checks against, records, and resolves the set
	// against all have to be the same picture of the configuration.
	st := b.state.Load()
	if req.ConfigRevision != st.revision {
		return Operation{}, fmt.Errorf("%w: request carries %q, current is %q", ErrConfigRevisionStale, req.ConfigRevision, st.revision)
	}

	set, ok := configuredBackupSet(st.inner, sourceName, setName)
	if !ok {
		return Operation{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, req.BackupSetID)
	}

	// Refused HERE rather than by the engine, because this is the last
	// point at which the answer costs nothing. An artifact set has no
	// snapshot to restore -- its restore is restore_placement against a
	// storage medium -- and letting the request through would write a
	// durable row for work that is going to fail as soon as it starts,
	// which an operator then has to poll to be told what this call
	// already knew.
	if set.Engine != model.EngineKopia {
		return Operation{}, fmt.Errorf("%w: %s", ErrSnapshotRestoreUnsupported, req.BackupSetID)
	}

	// EPIC K's production gate (#789), refused at the same point and for
	// the same reason: internal/app will refuse this restore the instant
	// the operation starts, and a durable row written first is an
	// operator polling an operation to be told what this call already
	// knew. See incrementalgate.go.
	if err := refuseGatedIncrementalEngine(st.inner.Config); err != nil {
		return Operation{}, err
	}

	parameters, err := json.Marshal(snapshotRestoreParameters{
		BackupSetID: req.BackupSetID,
		SnapshotID:  req.SnapshotID,
		SourcePath:  req.SourcePath,
		TargetPath:  req.TargetPath,
		Conflict:    string(conflict),
	})
	if err != nil {
		// A struct of five strings; Marshal cannot fail against it.
		parameters = []byte("{}")
	}

	outcome, err := b.journal.CreateOperation(ctx, state.OperationRequest{
		OperationID:    "op_" + uuid.New().String(),
		IdempotencyKey: req.IdempotencyKey,
		Actor:          req.Actor,
		BackupSet:      req.BackupSetID,
		ConfigRevision: st.revision,
		Action:         ActionRestoreSnapshot,
		Parameters:     string(parameters),
		CreatedAt:      now(),
	})
	if err != nil {
		if errors.Is(err, state.ErrOperationIdempotencyKeyReused) {
			return Operation{}, fmt.Errorf("%w: idempotency key already used for a different request", ErrIdempotencyKeyConflict)
		}

		// Deliberately not %w-wrapped: err may carry a state-layer
		// sentence naming SQLite internals, and nothing from that
		// vocabulary crosses this boundary (SubmitRunCycle's rule).
		return Operation{}, fmt.Errorf("service: submit %s: an internal error occurred", ActionRestoreSnapshot)
	}

	if !outcome.Created {
		return toOperation(outcome.Operation), nil
	}

	b.wg.Add(1)

	go b.executeSnapshotRestore(outcome.Operation.OperationID, app.SnapshotRestoreRequest{
		SourceName: sourceName,
		SetName:    setName,
		SnapshotID: req.SnapshotID,
		SourcePath: req.SourcePath,
		TargetPath: req.TargetPath,
		Conflict:   conflict,
	})

	return toOperation(outcome.Operation), nil
}

// executeSnapshotRestore is the asynchronous half: mark the operation
// running, restore, and record what happened.
//
// It is shaped like executeRunBackupSet, minus the two things a restore
// genuinely does not have: no single-flight lock (see this file's own
// doc) and no cycle watch, because a restore is not a pass over a backup
// set and advertising it as one would make the edit-hold prompt warn
// about interrupting a backup that is not running.
//
// The panic recovery is not defensive decoration. This runs on a
// goroutine inside an always-on process, so an unrecovered panic here
// takes down the API server rather than one command.
func (b *BackupService) executeSnapshotRestore(operationID string, req app.SnapshotRestoreRequest) {
	defer b.wg.Done()

	defer func() {
		if r := recover(); r != nil {
			b.logger.Error(context.Background(), "execute-snapshot-restore-panic", fmt.Errorf("recovered panic: %v", r))

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

	startedAt := now()

	// b.ctx, not context.Background(): a restore belongs to this
	// service's lifetime, so Close can ask it to stop. A restore that is
	// stopped that way is recorded as failed rather than completed, and
	// the restore path itself leaves no partially-written file under a
	// real name, so "failed" describes a destination holding fewer files
	// rather than a tree that lies about being whole.
	result, err := restoreSnapshot(b.state.Load().inner, b.ctx, req)
	if err != nil {
		// Not err.Error(): the error comes up from internal/app and can
		// name a repository path or a storage failure verbatim. The row's
		// error field is read back by clients, so it says what happened
		// in this package's own vocabulary and the detail goes to the log.
		b.logger.Error(context.Background(), "snapshot-restore", err)

		if failErr := b.journal.FailOperation(context.Background(), operationID, now(),
			snapshotRestoreFailure(err)); failErr != nil {
			b.logger.Error(context.Background(), "fail-operation", failErr)
		}

		return
	}

	// A restore that came back without an error and without claiming
	// completeness is recorded as a failure, not as a success with small
	// numbers on it. The engine sets Complete only when it reached the
	// end of what was asked for, and this is the one place where that
	// claim turns into a durable row somebody later reads as "your files
	// are back": a partial tree recorded as completed is exactly the lie
	// RestoreReport.Complete exists to prevent, and the guard belongs
	// where the claim is written rather than only where it is made.
	if !result.Report.Complete {
		if failErr := b.journal.FailOperation(context.Background(), operationID, now(),
			"this restore did not finish, so the destination holds only part of what was asked for"); failErr != nil {
			b.logger.Error(context.Background(), "fail-operation", failErr)
		}

		return
	}

	if err := b.journal.CompleteOperation(context.Background(), operationID, now(),
		summarizeSnapshotRestore(req, result, startedAt)); err != nil {
		b.logger.Error(context.Background(), "complete-operation", err)
	}
}

// restoreSnapshot is a seam over (*app.Service).RestoreSnapshot, exactly
// like runCycle is one over RunCycle and for the same reason: a test can
// substitute a stand-in that panics or blocks without this package
// needing an interface around *app.Service for one call site. Nothing
// overrides it in production.
var restoreSnapshot = func(inner *app.Service, ctx context.Context, req app.SnapshotRestoreRequest) (app.SnapshotRestoreResult, error) {
	return inner.RestoreSnapshot(ctx, req)
}

// snapshotRestoreFailure turns a restore's failure into the sentence the
// durable row carries.
//
// The classified cases are the ones an operator can act on, and each says
// something different about what to do next. Everything else is one
// generic sentence, because an unclassified error from below can name a
// filesystem path, an endpoint or a storage-layer type, and the row is
// read back by clients.
func snapshotRestoreFailure(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "the restore was stopped before it finished; nothing it had not finished writing was left behind"
	case errors.Is(err, app.ErrSetHasNoSnapshots):
		return "this backup set has no restore point to restore from"
	case errors.Is(err, app.ErrNotAnIncrementalSet):
		return "this backup set stores artifacts rather than snapshots"
	case errors.Is(err, backupengine.ErrSnapshotNotFound):
		return "that restore point is not in this deployment's repository"
	case errors.Is(err, backupengine.ErrRestorePathNotFound):
		return "that path is not in the restore point"
	case errors.Is(err, backupengine.ErrRestoreConflict):
		return "the destination already holds something the restore would have replaced"
	case errors.Is(err, backupengine.ErrUnsafeSnapshotPath):
		return "the restore was refused: the restore point names an entry that would be written outside the destination"
	case errors.Is(err, backupengine.ErrRepositoryNotFound):
		return "this deployment's repository could not be opened; check that its storage is mounted"
	default:
		return "this restore did not finish"
	}
}

// snapshotRestoreSummary is the opaque JSON blob a completed restore
// records.
//
// SnapshotID is here even though the request may have named it, because
// the request may NOT have: "the last known good one" is a question
// answered at the moment of the restore, and the row is the only place
// that answer survives.
type snapshotRestoreSummary struct {
	BackupSetID     string  `json:"backup_set_id"`
	SnapshotID      string  `json:"snapshot_id"`
	SourcePath      string  `json:"source_path,omitempty"`
	TargetPath      string  `json:"target_path"`
	Files           int64   `json:"files"`
	Directories     int64   `json:"directories"`
	Symlinks        int64   `json:"symlinks"`
	Skipped         int64   `json:"skipped"`
	Bytes           int64   `json:"bytes"`
	VerifiedFiles   int64   `json:"verified_files"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// summarizeSnapshotRestore renders what a finished restore got done.
func summarizeSnapshotRestore(req app.SnapshotRestoreRequest, result app.SnapshotRestoreResult, startedAt time.Time) string {
	summary, err := json.Marshal(snapshotRestoreSummary{
		BackupSetID:     req.SourceName + "/" + req.SetName,
		SnapshotID:      result.SnapshotID,
		SourcePath:      req.SourcePath,
		TargetPath:      req.TargetPath,
		Files:           result.Report.Files,
		Directories:     result.Report.Directories,
		Symlinks:        result.Report.Symlinks,
		Skipped:         result.Report.Skipped,
		Bytes:           result.Report.Bytes,
		VerifiedFiles:   result.Report.Verified,
		DurationSeconds: now().Sub(startedAt).Seconds(),
	})
	if err != nil {
		return "{}"
	}

	return string(summary)
}
