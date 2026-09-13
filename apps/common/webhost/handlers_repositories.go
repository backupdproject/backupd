package webhost

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/backupdproject/backupd/core/service"
)

// EPIC K's repository reads (#788):
//
//	GET /api/v1/repositories
//	GET /api/v1/repositories/{domain}/maintenance
//
// # Why a repository is a resource and a Kopia namespace is not
//
// A repository domain is something an operator DECLARES in this product's
// own configuration: a security boundary with an id, an isolation
// posture, and a passphrase reference. It is not the vendor's repository
// and this route exposes none of the vendor's vocabulary -- no blobs, no
// packs, no indexes, no maintenance command to run. What it serves is the
// six questions an operator has about a store their backups depend on,
// and the answers are this product's.
//
// # Why maintenance is a sub-resource rather than fields on the health
//
// Because the two have different costs and different readers. The health
// read opens every declared repository, which is real storage traffic;
// the maintenance read opens nothing at all, because the ownership record
// is a file this deployment writes beside its own state. That is exactly
// the case an operator asks in -- the repository is usually the thing
// that is not answering -- so the answer must not require it.

// repositoryHealthResponse is one repository domain's verdict on the
// wire.
type repositoryHealthResponse struct {
	Domain                 string   `json:"domain"`
	MayShare               bool     `json:"may_share"`
	State                  string   `json:"state"`
	Reachable              bool     `json:"reachable"`
	Readable               bool     `json:"readable"`
	Writable               bool     `json:"writable"`
	CredentialsValid       bool     `json:"credentials_valid"`
	ClockSane              bool     `json:"clock_sane"`
	ClockSkewSeconds       *int64   `json:"clock_skew_seconds"`
	MaintenanceOverdue     bool     `json:"maintenance_overdue"`
	LastMaintenanceAt      string   `json:"last_maintenance_at,omitempty"`
	LastMaintenanceResult  string   `json:"last_maintenance_result,omitempty"`
	LastSnapshotAt         string   `json:"last_snapshot_at,omitempty"`
	LastSnapshotStatus     string   `json:"last_snapshot_status,omitempty"`
	LastVerificationAt     string   `json:"last_verification_at,omitempty"`
	LastVerificationStatus string   `json:"last_verification_status,omitempty"`
	BackupSets             []string `json:"backup_sets,omitempty"`
	Detail                 string   `json:"detail,omitempty"`
}

type listRepositoriesResponse struct {
	GeneratedAt  string                     `json:"generated_at"`
	Repositories []repositoryHealthResponse `json:"repositories"`
}

// repositoryMaintenanceResponse is one repository's maintenance state.
type repositoryMaintenanceResponse struct {
	Domain         string `json:"domain"`
	Owner          string `json:"owner"`
	LastQuickAt    string `json:"last_quick_at,omitempty"`
	LastFullAt     string `json:"last_full_at,omitempty"`
	NextEligibleAt string `json:"next_eligible_at,omitempty"`
	Due            bool   `json:"due"`
	DueMode        string `json:"due_mode,omitempty"`
	DueReason      string `json:"due_reason,omitempty"`
	Overdue        bool   `json:"overdue"`
	Runs           int64  `json:"runs"`
	Failures       int64  `json:"failures"`
	ReclaimedBytes int64  `json:"reclaimed_bytes"`
	Failing        bool   `json:"failing"`
}

// listRepositories is GET /api/v1/repositories. Read-only (§50).
//
// Nothing is cached, for the reason the backup-set health read is not: a
// cached repository verdict keeps reporting green after the storage under
// it has gone away, which is the one moment the answer matters.
func (h *handlers) listRepositories(w http.ResponseWriter, r *http.Request) {
	report, err := h.backend.ListRepositories(r.Context())
	if err != nil {
		// EPIC K's production feature gate (#789) is the one refusal
		// this read has that is not an internal error: with the engine
		// disabled nothing may open a repository, so there is no verdict
		// to serve and every field would be a guess. 409 with the flag
		// named, so the page can say "the incremental engine is disabled
		// in this deployment" instead of "something went wrong".
		if errors.Is(err, service.ErrIncrementalEngineDisabled) {
			h.logRefusal(r, http.StatusConflict, "INCREMENTAL_ENGINE_DISABLED",
				writeError(w, http.StatusConflict, "INCREMENTAL_ENGINE_DISABLED", err.Error()), err)

			return
		}

		h.internalError(w, r, "INTERNAL", "an internal error occurred", err)

		return
	}

	out := listRepositoriesResponse{
		GeneratedAt:  formatTime(report.GeneratedAt),
		Repositories: make([]repositoryHealthResponse, 0, len(report.Repositories)),
	}
	for _, repo := range report.Repositories {
		body := repositoryHealthResponse{
			Domain:                 repo.Domain,
			MayShare:               repo.MayShare,
			State:                  repo.State,
			Reachable:              repo.Reachable,
			Readable:               repo.Readable,
			Writable:               repo.Writable,
			CredentialsValid:       repo.CredentialsValid,
			ClockSane:              repo.ClockSane,
			MaintenanceOverdue:     repo.MaintenanceOverdue,
			LastMaintenanceAt:      formatTime(repo.LastMaintenanceAt),
			LastMaintenanceResult:  repo.LastMaintenanceResult,
			LastSnapshotAt:         formatTime(repo.LastSnapshotAt),
			LastSnapshotStatus:     repo.LastSnapshotStatus,
			LastVerificationAt:     formatTime(repo.LastVerificationAt),
			LastVerificationStatus: repo.LastVerificationStatus,
			BackupSets:             repo.BackupSets,
			Detail:                 repo.Detail,
		}
		if repo.ClockSkew != nil {
			// Whole seconds, signed: positive is a clock ahead of this
			// deployment's own newest durable timestamp, negative is one
			// behind it, and behind is the direction that reorders
			// snapshots.
			seconds := int64(repo.ClockSkew.Seconds())
			body.ClockSkewSeconds = &seconds
		}
		out.Repositories = append(out.Repositories, body)
	}

	writeJSON(w, http.StatusOK, out)
}

// getRepositoryMaintenance is GET
// /api/v1/repositories/{domain}/maintenance. Read-only (§50).
func (h *handlers) getRepositoryMaintenance(w http.ResponseWriter, r *http.Request) {
	state, err := h.backend.RepositoryMaintenanceState(r.Context(), chi.URLParam(r, "domain"))
	if err != nil {
		if errors.Is(err, service.ErrRepositoryDomainNotFound) {
			h.logRefusal(r, http.StatusNotFound, "REPOSITORY_DOMAIN_NOT_FOUND",
				writeError(w, http.StatusNotFound, "REPOSITORY_DOMAIN_NOT_FOUND", err.Error()), err)
			return
		}

		h.internalError(w, r, "INTERNAL", "an internal error occurred", err)

		return
	}

	writeJSON(w, http.StatusOK, repositoryMaintenanceResponse{
		Domain:         state.Domain,
		Owner:          state.Owner,
		LastQuickAt:    formatTime(state.LastQuickAt),
		LastFullAt:     formatTime(state.LastFullAt),
		NextEligibleAt: formatTime(state.NextEligibleAt),
		Due:            state.Due,
		DueMode:        state.DueMode,
		DueReason:      state.DueReason,
		Overdue:        state.Overdue,
		Runs:           state.Runs,
		Failures:       state.Failures,
		ReclaimedBytes: state.ReclaimedBytes,
		Failing:        state.Failing,
	})
}
