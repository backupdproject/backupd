package webhost

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/retnd/retnd/core/service"
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
		out.Repositories = append(out.Repositories, toRepositoryHealthBody(repo))
	}

	writeJSON(w, http.StatusOK, out)
}

// toRepositoryHealthBody is one domain's verdict on the wire, shared by
// the fleet read and by the create that answers with the domain it just
// declared. One projection rather than two, so a create and a list
// cannot come to disagree about what a repository's health looks like.
func toRepositoryHealthBody(repo service.RepositoryHealth) repositoryHealthResponse {
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

	return body
}

// maxRepositoryDomainBodyBytes bounds a declaration. It is small on
// purpose: the largest thing in this body is a passphrase COMMAND's argv,
// and a body bigger than this is not a repository domain.
const maxRepositoryDomainBodyBytes = 1 << 13 // 8 KiB

// createRepositoryDomainRequest is one domain as a caller declares it.
//
// There is no field here for passphrase MATERIAL and there will not be:
// the passphrase block names a file, an environment variable or a
// command, exactly as the storage medium's credentials block does, and
// for the same reason (FR-33).
type createRepositoryDomainRequest struct {
	ID               string                        `json:"id"`
	Description      string                        `json:"description"`
	Isolation        string                        `json:"isolation"`
	Passphrase       repositoryPassphraseReference `json:"passphrase"`
	Location         string                        `json:"location"`
	MaintenanceOwner string                        `json:"maintenance_owner"`
}

type repositoryPassphraseReference struct {
	File    string   `json:"file"`
	Env     string   `json:"env"`
	Command []string `json:"command"`
}

func (b createRepositoryDomainRequest) request() service.CreateRepositoryDomainRequest {
	return service.CreateRepositoryDomainRequest{
		ID:          b.ID,
		Description: b.Description,
		Isolation:   b.Isolation,
		Passphrase: service.RepositoryPassphraseRef{
			File:    b.Passphrase.File,
			Env:     b.Passphrase.Env,
			Command: b.Passphrase.Command,
		},
		Location:         b.Location,
		MaintenanceOwner: b.MaintenanceOwner,
	}
}

// createRepositoryDomain is POST /api/v1/repositories (#862).
//
// CSRF and no destructive gate, which is the tier POST /backup-sets and
// POST /storage-mediums sit in (§50's "state-changing but
// non-destructive"): declaring a boundary writes one entry into
// config.yaml, opens no storage, creates no repository and cannot reach
// a backup datum at all. See destructiveGateExemptRoutes for the list
// this claim is recorded on.
func (h *handlers) createRepositoryDomain(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRepositoryDomainBodyBytes)

	var body createRepositoryDomainRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxRepositoryDomainBodyBytes)

		return
	}

	created, err := h.backend.CreateRepositoryDomain(r.Context(), body.request())
	if err != nil {
		h.writeRepositoryDomainError(w, r, err)

		return
	}

	writeJSON(w, http.StatusCreated, toRepositoryHealthBody(created))
}

// writeRepositoryDomainError maps the three refusals this write has, and
// keeps them apart.
//
// All three of the first arms are 409, and that is deliberate rather than
// lazy: each is a well-formed request whose one problem is the state of
// this deployment, and none of them is fixed by editing the body the way
// a 400 says to. What makes them usable is the CODE, because the remedies
// are different -- turn the engine on, choose another id, or go and ask
// the instance that holds maintenance.
func (h *handlers) writeRepositoryDomainError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrIncrementalEngineDisabled):
		// The sentence is config's own: a config key and an environment
		// variable, no path, endpoint or credential, and it is the one
		// place an operator is told which flag to set.
		h.logRefusal(r, http.StatusConflict, "INCREMENTAL_ENGINE_DISABLED",
			writeError(w, http.StatusConflict, "INCREMENTAL_ENGINE_DISABLED", err.Error()), err)
	case errors.Is(err, service.ErrRepositoryDomainExists):
		h.logRefusal(r, http.StatusConflict, "REPOSITORY_DOMAIN_EXISTS",
			writeError(w, http.StatusConflict, "REPOSITORY_DOMAIN_EXISTS", err.Error()), err)
	case errors.Is(err, service.ErrRepositoryDomainMaintainedElsewhere):
		// Safe to echo: core/service composes this one out of the domain
		// id and the owner name recorded beside this deployment's own
		// state, and the owner name is a thing operators choose and
		// publish to each other.
		h.logRefusal(r, http.StatusConflict, "REPOSITORY_DOMAIN_MAINTAINED_ELSEWHERE",
			writeError(w, http.StatusConflict, "REPOSITORY_DOMAIN_MAINTAINED_ELSEWHERE", err.Error()), err)
	case errors.Is(err, service.ErrInvalidRequest):
		// Safe to echo, on core/service's own guarantee: a
		// config.ValidationError's text is built from that package's
		// field descriptions and the caller's own submitted values, and
		// the only secret-shaped thing on this boundary is a REFERENCE.
		h.logRefusal(r, http.StatusBadRequest, "INVALID_REQUEST",
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error()), err)
	case errors.Is(err, service.ErrConfigNotFileBacked):
		// The same sentence the other configuration writes answer with
		// (backup sets, settings, retention). It is a 500 because
		// nothing about the request is wrong, and it says which
		// deployment-shaped thing is missing rather than leaving an
		// operator to read "failed to declare the repository domain"
		// and go looking at the domain.
		h.internalError(w, r, "INTERNAL", "this deployment has no configuration file to persist to", err)
	default:
		h.internalError(w, r, "INTERNAL", "failed to declare the repository domain", err)
	}
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
