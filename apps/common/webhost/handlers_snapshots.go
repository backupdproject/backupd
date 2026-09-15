package webhost

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/retnd/retnd/core/service"
)

// EPIC K's snapshot reads (#788):
//
//	GET /api/v1/backup-sets/{source}/{set}/snapshots
//	GET /api/v1/backup-sets/{source}/{set}/snapshots/{run}
//	GET /api/v1/backup-sets/{source}/{set}/holds
//	GET /api/v1/backup-sets/{source}/{set}/snapshot-retention
//
// # Why they are sub-resources of the backup set
//
// Because that is what they are, and because the alternative is the thing
// EPIC K forbids: a /kopia namespace. A snapshot belongs to exactly one
// backup set, its identity is only meaningful inside that set's lineage,
// and every one of these reads is refused for a set that stores artifacts
// instead. Hanging them off the set makes all three true by construction
// rather than by a check somebody has to remember.
//
// # Why every counter can be null
//
// A run that died before its manifest was recorded, and a snapshot crash
// reconciliation adopted from the repository, both have counters nobody
// ever took. A run over an empty directory has counters that are
// genuinely zero. Sending 0 for both is how a surface reports "not
// measured" as "measured nothing", which for a backup is the difference
// between "we do not know" and "it did nothing".
//
// # Why there are no mutating routes here
//
// Holding a snapshot, releasing a hold, verifying one and restoring one
// are all durable, idempotency-keyed operations, so they are actions on
// POST /api/v1/operations rather than routes of their own. See
// handlers_operations.go: this deployment has one answer to how work that
// outlives a request begins, and a second one would drift.

// snapshotResponse is one snapshot run on the wire.
//
// Every timestamp is omitted rather than zero-valued until the event it
// names has happened, exactly as an operation's are, and every counter is
// a pointer so null survives.
type snapshotResponse struct {
	RunID                     string                 `json:"run_id"`
	BackupSetID               string                 `json:"backup_set_id"`
	OperationID               string                 `json:"operation_id,omitempty"`
	Engine                    string                 `json:"engine"`
	RepositoryDomain          string                 `json:"repository_domain"`
	SnapshotID                string                 `json:"snapshot_id,omitempty"`
	Phase                     string                 `json:"phase"`
	ConsistencyMode           string                 `json:"consistency_mode"`
	VerificationLevel         string                 `json:"verification_level"`
	VerificationLevelAchieved string                 `json:"verification_level_achieved,omitempty"`
	VerificationStatus        string                 `json:"verification_status,omitempty"`
	EntriesScanned            *int64                 `json:"entries_scanned"`
	Files                     *int64                 `json:"files"`
	Directories               *int64                 `json:"directories"`
	LogicalBytes              *int64                 `json:"logical_bytes"`
	SourceBytesRead           *int64                 `json:"source_bytes_read"`
	RepositoryBytesWritten    *int64                 `json:"repository_bytes_written"`
	ContentReusedBytes        *int64                 `json:"content_reused_bytes"`
	SourceComplete            *bool                  `json:"source_complete"`
	LastKnownGood             bool                   `json:"last_known_good"`
	Reason                    string                 `json:"reason,omitempty"`
	StartedAt                 string                 `json:"started_at"`
	CompletedAt               string                 `json:"completed_at,omitempty"`
	DurationSeconds           *int64                 `json:"duration_seconds"`
	DeleteRequestedAt         string                 `json:"delete_requested_at,omitempty"`
	Holds                     []snapshotHoldResponse `json:"holds,omitempty"`
}

// snapshotHoldResponse is one hold on the wire.
type snapshotHoldResponse struct {
	HoldID      string `json:"hold_id"`
	RunID       string `json:"run_id"`
	BackupSetID string `json:"backup_set_id"`
	Reason      string `json:"reason"`
	PlacedAt    string `json:"placed_at"`
	PlacedBy    string `json:"placed_by"`
	ReleasedAt  string `json:"released_at,omitempty"`
	ReleasedBy  string `json:"released_by,omitempty"`
	Active      bool   `json:"active"`
}

// snapshotTransitionResponse is one edge of the snapshot state machine.
type snapshotTransitionResponse struct {
	From   string `json:"from,omitempty"`
	To     string `json:"to"`
	At     string `json:"at"`
	Detail string `json:"detail,omitempty"`
}

type listSnapshotsResponse struct {
	Snapshots []snapshotResponse `json:"snapshots"`
}

type snapshotDetailResponse struct {
	Snapshot    snapshotResponse             `json:"snapshot"`
	Transitions []snapshotTransitionResponse `json:"transitions"`
}

type listSnapshotHoldsResponse struct {
	Holds []snapshotHoldResponse `json:"holds"`
}

// snapshotRetentionTierResponse is one selection that kept a snapshot.
type snapshotRetentionTierResponse struct {
	Tier       string `json:"tier"`
	SelectedBy string `json:"selected_by,omitempty"`
}

// snapshotRetentionVerdictResponse is what retention would do about one
// snapshot, and why.
type snapshotRetentionVerdictResponse struct {
	RunID      string                          `json:"run_id"`
	SnapshotID string                          `json:"snapshot_id,omitempty"`
	Action     string                          `json:"action"`
	StartedAt  string                          `json:"started_at"`
	Tiers      []snapshotRetentionTierResponse `json:"tiers,omitempty"`
	Holds      []snapshotHoldResponse          `json:"holds,omitempty"`
	Reason     string                          `json:"reason"`
	HoldReason string                          `json:"hold_reason,omitempty"`
}

type snapshotRetentionResponse struct {
	GeneratedAt string                             `json:"generated_at"`
	Verdicts    []snapshotRetentionVerdictResponse `json:"verdicts"`
}

// listBackupSetSnapshots is GET .../snapshots. Read-only (§50), so no
// CSRF and no destructive gate, like every other GET in this package.
func (h *handlers) listBackupSetSnapshots(w http.ResponseWriter, r *http.Request) {
	snapshots, err := h.backend.ListSnapshots(r.Context(), backupSetIDFrom(r))
	if err != nil {
		h.writeSnapshotError(w, r, err)
		return
	}

	// A non-nil empty slice, so a set with no snapshots serves
	// `{"snapshots":[]}` rather than `{"snapshots":null}`. A client
	// should never have to write the null case for a list that is simply
	// empty.
	out := listSnapshotsResponse{Snapshots: make([]snapshotResponse, 0, len(snapshots))}
	for _, s := range snapshots {
		out.Snapshots = append(out.Snapshots, toSnapshotResponse(s))
	}

	writeJSON(w, http.StatusOK, out)
}

// getBackupSetSnapshot is GET .../snapshots/{run}.
func (h *handlers) getBackupSetSnapshot(w http.ResponseWriter, r *http.Request) {
	detail, err := h.backend.GetSnapshot(r.Context(), backupSetIDFrom(r), chi.URLParam(r, "run"))
	if err != nil {
		h.writeSnapshotError(w, r, err)
		return
	}

	out := snapshotDetailResponse{
		Snapshot:    toSnapshotResponse(detail.Snapshot),
		Transitions: make([]snapshotTransitionResponse, 0, len(detail.Transitions)),
	}
	for _, t := range detail.Transitions {
		out.Transitions = append(out.Transitions, snapshotTransitionResponse{
			From:   t.From,
			To:     t.To,
			At:     formatTime(t.At),
			Detail: t.Detail,
		})
	}

	writeJSON(w, http.StatusOK, out)
}

// listBackupSetSnapshotHolds is GET .../holds.
func (h *handlers) listBackupSetSnapshotHolds(w http.ResponseWriter, r *http.Request) {
	holds, err := h.backend.ListSnapshotHolds(r.Context(), backupSetIDFrom(r))
	if err != nil {
		h.writeSnapshotError(w, r, err)
		return
	}

	out := listSnapshotHoldsResponse{Holds: make([]snapshotHoldResponse, 0, len(holds))}
	for _, hold := range holds {
		out.Holds = append(out.Holds, toSnapshotHoldResponse(hold))
	}

	writeJSON(w, http.StatusOK, out)
}

// getBackupSetSnapshotRetention is GET .../snapshot-retention: the
// preview, which deletes nothing.
//
// Read-only and therefore ungated, which is worth stating because the
// artifact side's own preview sits beside an APPLY that is gated. There
// is no apply here: snapshot retention runs on the engine's own schedule,
// and this route exists so an operator can see what it will decide before
// it does.
func (h *handlers) getBackupSetSnapshotRetention(w http.ResponseWriter, r *http.Request) {
	preview, err := h.backend.SnapshotRetention(r.Context(), backupSetIDFrom(r))
	if err != nil {
		h.writeSnapshotError(w, r, err)
		return
	}

	out := snapshotRetentionResponse{
		GeneratedAt: formatTime(preview.GeneratedAt),
		Verdicts:    make([]snapshotRetentionVerdictResponse, 0, len(preview.Verdicts)),
	}
	for _, v := range preview.Verdicts {
		verdict := snapshotRetentionVerdictResponse{
			RunID:      v.RunID,
			SnapshotID: v.SnapshotID,
			Action:     v.Action,
			StartedAt:  formatTime(v.StartedAt),
			Reason:     v.Reason,
			HoldReason: v.HoldReason,
		}
		for _, t := range v.Tiers {
			verdict.Tiers = append(verdict.Tiers, snapshotRetentionTierResponse{Tier: t.Tier, SelectedBy: t.SelectedBy})
		}
		for _, hold := range v.Holds {
			verdict.Holds = append(verdict.Holds, toSnapshotHoldResponse(hold))
		}
		out.Verdicts = append(out.Verdicts, verdict)
	}

	writeJSON(w, http.StatusOK, out)
}

// backupSetIDFrom rebuilds the source/set id these routes are addressed
// by, from the two path segments the router matched.
func backupSetIDFrom(r *http.Request) string {
	return chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
}

func toSnapshotResponse(s service.Snapshot) snapshotResponse {
	out := snapshotResponse{
		RunID:                     s.RunID,
		BackupSetID:               s.BackupSetID,
		OperationID:               s.OperationID,
		Engine:                    s.Engine,
		RepositoryDomain:          s.RepositoryDomain,
		SnapshotID:                s.SnapshotID,
		Phase:                     s.Phase,
		ConsistencyMode:           s.SourceConsistency,
		VerificationLevel:         s.VerificationLevel,
		VerificationLevelAchieved: s.VerificationAchieved,
		VerificationStatus:        s.VerificationStatus,
		EntriesScanned:            s.EntriesScanned,
		Files:                     s.Files,
		Directories:               s.Directories,
		LogicalBytes:              s.LogicalBytes,
		SourceBytesRead:           s.SourceBytesRead,
		RepositoryBytesWritten:    s.RepositoryBytesWritten,
		ContentReusedBytes:        s.ContentReusedBytes,
		SourceComplete:            s.SourceComplete,
		LastKnownGood:             s.LastKnownGood,
		Reason:                    s.Reason,
		StartedAt:                 formatTime(s.StartedAt),
		CompletedAt:               formatTimePtr(s.CompletedAt),
		DeleteRequestedAt:         formatTimePtr(s.DeleteRequestedAt),
	}

	if s.Duration != nil {
		// Whole seconds, truncated. A snapshot run is measured in
		// minutes and hours, and a fractional second on a duration a
		// surface renders as "4m 12s" is precision nobody reads and one
		// more thing for two clients to round differently.
		seconds := int64(s.Duration.Seconds())
		out.DurationSeconds = &seconds
	}

	for _, hold := range s.Holds {
		out.Holds = append(out.Holds, toSnapshotHoldResponse(hold))
	}

	return out
}

func toSnapshotHoldResponse(h service.SnapshotHold) snapshotHoldResponse {
	return snapshotHoldResponse{
		HoldID:      h.HoldID,
		RunID:       h.RunID,
		BackupSetID: h.BackupSetID,
		Reason:      h.Reason,
		PlacedAt:    formatTime(h.PlacedAt),
		PlacedBy:    h.PlacedBy,
		ReleasedAt:  formatTimePtr(h.ReleasedAt),
		ReleasedBy:  h.ReleasedBy,
		Active:      h.Active,
	}
}

// writeSnapshotError maps the snapshot surface's refusals onto their
// declared statuses.
//
// Every message echoed here is core/service's own prose, which is the
// rule service.ErrInvalidRequest's doc sets out: never an unclassified
// error, which could carry state-layer or repository text.
func (h *handlers) writeSnapshotError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrBackupSetNotFound):
		h.logRefusal(r, http.StatusNotFound, "BACKUP_SET_NOT_FOUND",
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", err.Error()), err)
	case errors.Is(err, service.ErrSnapshotNotFound):
		h.logRefusal(r, http.StatusNotFound, "SNAPSHOT_NOT_FOUND",
			writeError(w, http.StatusNotFound, "SNAPSHOT_NOT_FOUND", err.Error()), err)
	case errors.Is(err, service.ErrSnapshotHoldNotFound):
		h.logRefusal(r, http.StatusNotFound, "SNAPSHOT_HOLD_NOT_FOUND",
			writeError(w, http.StatusNotFound, "SNAPSHOT_HOLD_NOT_FOUND", err.Error()), err)
	case errors.Is(err, service.ErrSnapshotRestoreUnsupported):
		// Its own code rather than INVALID_REQUEST: the request is well
		// formed and the SET is the wrong kind, so a client reading a
		// validation failure would offer the operator a fix to the body.
		h.logRefusal(r, http.StatusBadRequest, "BACKUP_SET_NOT_INCREMENTAL",
			writeError(w, http.StatusBadRequest, "BACKUP_SET_NOT_INCREMENTAL", err.Error()), err)
	case errors.Is(err, service.ErrIncrementalEngineDisabled):
		// EPIC K's production feature gate (#789). A conflict rather
		// than the 400 above, and the difference is the fix: that one
		// says this SET is the wrong kind, which a client answers by
		// asking about a different set; this one says the DEPLOYMENT does
		// not run the engine, which only a configuration change answers.
		// Safe to echo -- core/internal/config's own sentence, naming
		// the config key and the environment variable.
		h.logRefusal(r, http.StatusConflict, "INCREMENTAL_ENGINE_DISABLED",
			writeError(w, http.StatusConflict, "INCREMENTAL_ENGINE_DISABLED", err.Error()), err)
	default:
		h.internalError(w, r, "INTERNAL", "an internal error occurred", err)
	}
}
