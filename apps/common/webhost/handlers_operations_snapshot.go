package webhost

import (
	"errors"
	"net/http"

	"github.com/backupdproject/backupd/core/service"
)

// EPIC K's four mutating acts, as actions on POST /api/v1/operations
// (#788): restore a snapshot, verify one, hold one, release a hold.
//
// # Why they are here and not four routes
//
// /operations is where this product already answers "how does a caller
// start something that outlives the request, exactly once, against a
// configuration it agrees is current". Every one of these four needs all
// three properties, and a second route family would have to restate them
// -- which is how one of them ends up without the idempotency key, which
// is the property nobody notices is missing until a retry has done
// something twice.
//
// A hold is not long-running and is still an operation, for that exact
// reason: the retry, not the duration. See
// core/service.SubmitSnapshotHold.
//
// # Why each carries its own parameter object
//
// Because the four take different parameters, and a single flat body
// whose meaning depended on the action would be a body somebody
// eventually fills in for the wrong one. Each object is refused outright
// when it arrives with the wrong action, exactly as the restore object
// beside it is.

// snapshotRestoreOperationRequest is the restore_snapshot action's
// parameters.
type snapshotRestoreOperationRequest struct {
	BackupSetID string `json:"backup_set_id"`
	SnapshotID  string `json:"snapshot_id,omitempty"`
	SourcePath  string `json:"source_path,omitempty"`
	TargetPath  string `json:"target_path"`
	Conflict    string `json:"conflict,omitempty"`
}

// snapshotVerifyOperationRequest is the verify_snapshot action's.
type snapshotVerifyOperationRequest struct {
	BackupSetID   string `json:"backup_set_id"`
	RunID         string `json:"run_id,omitempty"`
	Level         string `json:"level,omitempty"`
	SamplePercent int    `json:"sample_percent,omitempty"`
}

// snapshotHoldOperationRequest is the hold_snapshot action's.
type snapshotHoldOperationRequest struct {
	BackupSetID string `json:"backup_set_id"`

	// RunID is optional, and absent means this backup set's last known
	// good snapshot: the one an operator protecting "the current restore
	// point" means and the one they would otherwise have to look up
	// first. core/service and `backupd snapshot hold` have always read an
	// unnamed run that way; the contract now says so too.
	RunID  string `json:"run_id,omitempty"`
	Reason string `json:"reason"`
}

// snapshotHoldReleaseOperationRequest is the release_snapshot_hold
// action's.
type snapshotHoldReleaseOperationRequest struct {
	BackupSetID string `json:"backup_set_id"`
	HoldID      string `json:"hold_id"`
}

// submitSnapshotRestore is POST /operations with a restore_snapshot
// action: read a restore point this deployment holds and write a tree
// onto a disk it can reach.
//
// It carries requireDestructiveGate, like every other action on this
// route, and the reason is not that it deletes anything -- it does not.
// The gate stands in front of operations an operator cannot undo, and
// writing a restored tree into a directory somebody named is one: the
// files that were there under those names are gone once conflict is
// "overwrite", and there is no call that puts them back.
func (h *handlers) submitSnapshotRestore(w http.ResponseWriter, r *http.Request, idempotencyKey string, body submitOperationRequest) {
	if body.SnapshotRestore == nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			"a restore_snapshot submission requires snapshot_restore parameters")
		return
	}

	p := body.SnapshotRestore
	op, err := h.backend.SubmitSnapshotRestore(r.Context(), service.SnapshotRestoreRequest{
		Actor:          actorFromContext(r.Context()),
		IdempotencyKey: idempotencyKey,
		ConfigRevision: body.ConfigRevision,
		BackupSetID:    p.BackupSetID,
		SnapshotID:     p.SnapshotID,
		SourcePath:     p.SourcePath,
		TargetPath:     p.TargetPath,
		Conflict:       p.Conflict,
	})
	if err != nil {
		h.writeSnapshotActionError(w, r, err, body.ConfigRevision)
		return
	}

	writeJSON(w, http.StatusAccepted, toOperationResponse(op))
}

// submitSnapshotVerify is POST /operations with a verify_snapshot action.
func (h *handlers) submitSnapshotVerify(w http.ResponseWriter, r *http.Request, idempotencyKey string, body submitOperationRequest) {
	if body.SnapshotVerify == nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			"a verify_snapshot submission requires snapshot_verify parameters")
		return
	}

	p := body.SnapshotVerify
	op, err := h.backend.SubmitSnapshotVerify(r.Context(), service.SnapshotVerifyRequest{
		Actor:          actorFromContext(r.Context()),
		IdempotencyKey: idempotencyKey,
		ConfigRevision: body.ConfigRevision,
		BackupSetID:    p.BackupSetID,
		RunID:          p.RunID,
		Level:          p.Level,
		SamplePercent:  p.SamplePercent,
	})
	if err != nil {
		h.writeSnapshotActionError(w, r, err, body.ConfigRevision)
		return
	}

	writeJSON(w, http.StatusAccepted, toOperationResponse(op))
}

// submitSnapshotHold is POST /operations with a hold_snapshot action.
func (h *handlers) submitSnapshotHold(w http.ResponseWriter, r *http.Request, idempotencyKey string, body submitOperationRequest) {
	if body.SnapshotHold == nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			"a hold_snapshot submission requires snapshot_hold parameters")
		return
	}

	p := body.SnapshotHold
	op, err := h.backend.SubmitSnapshotHold(r.Context(), service.SnapshotHoldRequest{
		Actor:          actorFromContext(r.Context()),
		IdempotencyKey: idempotencyKey,
		ConfigRevision: body.ConfigRevision,
		BackupSetID:    p.BackupSetID,
		RunID:          p.RunID,
		Reason:         p.Reason,
	})
	if err != nil {
		h.writeSnapshotActionError(w, r, err, body.ConfigRevision)
		return
	}

	writeJSON(w, http.StatusAccepted, toOperationResponse(op))
}

// submitSnapshotHoldRelease is POST /operations with a
// release_snapshot_hold action.
func (h *handlers) submitSnapshotHoldRelease(w http.ResponseWriter, r *http.Request, idempotencyKey string, body submitOperationRequest) {
	if body.SnapshotHoldRelease == nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			"a release_snapshot_hold submission requires snapshot_hold_release parameters")
		return
	}

	p := body.SnapshotHoldRelease
	op, err := h.backend.SubmitSnapshotHoldRelease(r.Context(), service.SnapshotHoldReleaseRequest{
		Actor:          actorFromContext(r.Context()),
		IdempotencyKey: idempotencyKey,
		ConfigRevision: body.ConfigRevision,
		BackupSetID:    p.BackupSetID,
		HoldID:         p.HoldID,
	})
	if err != nil {
		h.writeSnapshotActionError(w, r, err, body.ConfigRevision)
		return
	}

	writeJSON(w, http.StatusAccepted, toOperationResponse(op))
}

// writeSnapshotActionError maps the four actions' refusals onto their
// declared statuses.
//
// Every one of these is a state an operator reaches by clicking a button
// on a screen that has moved on, so every one is a typed refusal rather
// than a 500. Every message echoed is core/service's own prose, which is
// the rule service.ErrInvalidRequest's doc sets out: never an
// unclassified error, which could carry state-layer or repository text.
func (h *handlers) writeSnapshotActionError(w http.ResponseWriter, r *http.Request, err error, revision string) {
	switch {
	case errors.Is(err, service.ErrConfigRevisionStale):
		writeConfigRevisionStale(w, err.Error(), h.backend.ConfigRevision())
	case errors.Is(err, service.ErrIdempotencyKeyConflict):
		writeError(w, http.StatusConflict, "IDEMPOTENCY_KEY_CONFLICT", err.Error())
	case errors.Is(err, service.ErrBackupSetNotFound):
		h.logRefusal(r, http.StatusNotFound, "BACKUP_SET_NOT_FOUND",
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", err.Error()), err)
	case errors.Is(err, service.ErrSnapshotNotFound):
		h.logRefusal(r, http.StatusNotFound, "SNAPSHOT_NOT_FOUND",
			writeError(w, http.StatusNotFound, "SNAPSHOT_NOT_FOUND", err.Error()), err)
	case errors.Is(err, service.ErrSnapshotHoldNotFound):
		h.logRefusal(r, http.StatusNotFound, "SNAPSHOT_HOLD_NOT_FOUND",
			writeError(w, http.StatusNotFound, "SNAPSHOT_HOLD_NOT_FOUND", err.Error()), err)
	case errors.Is(err, service.ErrSnapshotNotHoldable):
		// A conflict rather than a validation failure: the body is well
		// formed and the snapshot is the thing that has moved on, so
		// nothing a client could change about the request would make it
		// work. Logged like the other typed refusals because it is
		// reached by a control that was on screen a moment ago, which
		// makes a run of them worth seeing.
		h.logRefusal(r, http.StatusConflict, "SNAPSHOT_NOT_HOLDABLE",
			writeError(w, http.StatusConflict, "SNAPSHOT_NOT_HOLDABLE", err.Error()), err)
	case errors.Is(err, service.ErrSnapshotRestoreUnsupported):
		h.logRefusal(r, http.StatusBadRequest, "BACKUP_SET_NOT_INCREMENTAL",
			writeError(w, http.StatusBadRequest, "BACKUP_SET_NOT_INCREMENTAL", err.Error()), err)
	case errors.Is(err, service.ErrIncrementalEngineDisabled):
		// EPIC K's production feature gate (#789), a conflict for
		// writeSnapshotError's reason: this deployment does not run the
		// engine, and no change to this request makes it work. It is
		// refused BEFORE any durable operation row is written
		// (core/service), so there is nothing in the activity feed for
		// an operator to go and look at.
		h.logRefusal(r, http.StatusConflict, "INCREMENTAL_ENGINE_DISABLED",
			writeError(w, http.StatusConflict, "INCREMENTAL_ENGINE_DISABLED", err.Error()), err)
	case errors.Is(err, service.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	default:
		h.internalError(w, r, "INTERNAL", "failed to submit operation", err)
	}
}

// refuseForeignParameters refuses a body carrying a parameter object that
// belongs to a different action.
//
// It is one function rather than a check inside each arm because the rule
// is one rule, and the two places it used to live (a run_cycle's own
// check for restore parameters, and nothing at all for the other four)
// are exactly how a submission with the wrong object in it got served.
// A server that ignores fields it did not expect teaches clients those
// fields are optional, and the next reader of that client cannot tell
// which of the operations was meant.
//
// The returned error is only a signal that a response has already been
// written; nothing reads its text.
func refuseForeignParameters(w http.ResponseWriter, body submitOperationRequest) error {
	carried := []struct {
		name  string
		owner string
		set   bool
	}{
		// The flat one, and it belongs in this list for exactly the
		// reason the nested objects do. backup_set_id is
		// run_backup_set's parameter; every other action either names
		// its set inside its own object (the four incremental ones, the
		// restore) or acts deployment-wide (run_cycle). It used to be
		// refused for run_cycle alone, so a hold_snapshot carrying a
		// top-level backup_set_id was served with the field ignored --
		// and the two sets in that body could be DIFFERENT, which is a
		// caller holding a snapshot in one set while believing it held
		// one in another.
		{"backup_set_id", service.ActionRunBackupSet, body.BackupSetID != ""},
		{"restore", service.ActionRestorePlacement, body.Restore != nil},
		{"snapshot_restore", service.ActionRestoreSnapshot, body.SnapshotRestore != nil},
		{"snapshot_verify", service.ActionVerifySnapshot, body.SnapshotVerify != nil},
		{"snapshot_hold", service.ActionHoldSnapshot, body.SnapshotHold != nil},
		{"snapshot_hold_release", service.ActionReleaseSnapshotHold, body.SnapshotHoldRelease != nil},
	}

	for _, c := range carried {
		if c.set && body.Action != c.owner {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				"a "+body.Action+" submission carried "+c.name+" parameters, which belong to "+c.owner)

			return errForeignParameters
		}
	}

	return nil
}

// errForeignParameters is the signal above, and never a message: the
// response is already written by the time it is returned.
var errForeignParameters = errors.New("webhost: submission carried another action's parameters")
