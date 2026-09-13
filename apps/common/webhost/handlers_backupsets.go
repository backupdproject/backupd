package webhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/backupdproject/backupd/core/service"
)

// Backup-set CRUD: create, list, read, update, remove, and the two
// per-set toggles.
//
// Creation is the route with the conditional gate, and it is the only
// place in this package where the destructive gate is consulted from
// inside a handler rather than from the route table. Persisting a backup
// set touches no backup data, so the route is not gated; but the same call
// with run_immediately set also starts a run cycle, which is precisely
// what the gate exists to block. Gating the route would make it impossible
// to configure anything before #92; not checking at all would make
// run_immediately an ungated way to start work. So the check is on the
// branch.
//
// Removal is the other one worth reading before changing. It writes
// configuration and deletes nothing: every artifact the set produced stays
// on storage and stays listed. The button says Remove and the dialog is
// styled destructive, which is why the tier is easy to get wrong, and §50
// decides by what is touched rather than by how it reads.
//
// backupSetSpec is shared with the first-run route rather than duplicated,
// because those two write the same shape into two different situations and
// the published contract shares it too.

// maxCreateBackupSetBodyBytes bounds POST /api/v1/backup-sets' request
// body, the same rationale as maxSubmitOperationBodyBytes
// (handlers_operations.go): generous headroom over any legitimate
// request (a handful of short strings and a small include-pattern list),
// while still bounding how much of a malformed or hostile request this
// handler reads into memory before giving up.
const maxCreateBackupSetBodyBytes = 1 << 20 // 1 MiB

// backupSetSpec is everything it takes to DESCRIBE one backup set: the
// add-backup-set wizard's (#98) Review step, translated into
// service.CreateBackupSetRequest. See that type's own doc for what
// SSHKeyID and KnownHostsLine reference (a prior POST /ssh-keys and
// POST /ssh/host-key-probe call respectively) and why neither carries
// key material or an unverified fingerprint directly.
//
// Nothing in it asks for anything to be RUN, which is what makes it the
// body of two operations rather than one: POST /api/v1/backup-sets folds
// a set into a configuration that already exists, and POST
// /api/v1/system/first-run writes the first configuration there has ever
// been (firstrun.go). They share this type rather than restating it,
// exactly as api/v1/openapi.json's BackupSetSpec is shared by both.
type backupSetSpec struct {
	SourceName         string   `json:"source_name"`
	Name               string   `json:"name"`
	Host               string   `json:"host"`
	Port               int      `json:"port"`
	User               string   `json:"user"`
	SSHKeyID           string   `json:"ssh_key_id"`
	KnownHostsLine     string   `json:"known_hosts_line"`
	RemotePath         string   `json:"remote_path"`
	LocalPath          string   `json:"local_path"`
	Include            []string `json:"include"`
	CompletionStrategy string   `json:"completion_strategy"`
	StableForSeconds   int      `json:"stable_for_seconds"`
	StaleAfterSeconds  int      `json:"stale_after_seconds"`
	// ValidatorID names one entry in the registered application-validator
	// catalog GET /api/v1/validators serves (handlers_validators.go), or
	// is omitted for none. It is an id and never a path: this package
	// could not accept a path here even if a handler wanted to, since
	// core/service is a separate module whose CreateBackupSetRequest has
	// no field for one, and refuses any id outside its own catalog
	// (docs/EPIC-B-multi-nas.md §26 Step 5).
	ValidatorID string `json:"validator_id"`
	Disabled    bool   `json:"disabled"`
	// ReadOnly declares this backup set's remote source read-only from
	// creation (issue #282's "pull from here, never delete here"), set
	// through the wizard/API rather than by hand-editing config.yaml
	// (issue #316). Omitted or false means exactly what every request
	// before this issue already meant: FR-15's delete step runs
	// unchanged.
	ReadOnly bool `json:"read_only"`
	// SkipConnectionCheck writes this set without proving its connection
	// first (issue #624). The service runs the same six-step check POST
	// /api/v1/backup-sets/test-connection answers, in front of the write,
	// and refuses with BACKUP_SET_CONNECTION_NOT_PROVEN when it fails;
	// this is the deliberate opt-out, and a set written under it is
	// marked connection_unverified until a test passes. The mark is the
	// service's own record of the skip, never a field a caller sets: an
	// earlier shape of this body carried connection_unverified as a claim
	// about what the caller had done, which any client other than this
	// repository's own could simply omit (PR #628 review). Omitted or
	// false checks, which is what every create should do.
	SkipConnectionCheck bool `json:"skip_connection_check"`

	// EPIC K's engine seam on the way in (#788).
	//
	// engine is "artifact", "kopia", or absent, and absent means
	// artifact -- which is what every request written before this field
	// existed means. Everything below it is read only for the
	// incremental engine and refused for an artifact set, by the same
	// config.Validate a hand-edited config.yaml goes through: nothing in
	// this package decides what a valid engine configuration is, for the
	// reason setBackupSetRetention gives about the same question.
	//
	// uuid is optional on the way in and the service mints one: its only
	// job is to be stable across every rename this set will ever have,
	// and a value an operator types is a value an operator can retype
	// differently.
	Engine                               string `json:"engine,omitempty"`
	UUID                                 string `json:"uuid,omitempty"`
	RepositoryDomain                     string `json:"repository_domain,omitempty"`
	SourceConsistency                    string `json:"source_consistency,omitempty"`
	VerificationLevel                    string `json:"verification_level,omitempty"`
	VerificationSamplePercent            int    `json:"verification_sample_percent,omitempty"`
	VerificationFullEverySeconds         int64  `json:"verification_full_every_seconds,omitempty"`
	VerificationRestoreDrillEverySeconds int64  `json:"verification_restore_drill_every_seconds,omitempty"`
	SourceMountPrefix                    string `json:"source_mount_prefix,omitempty"`
}

// backupSetRequest is POST /api/v1/backup-sets' request body: the spec
// above plus the two things only a create can ask for.
//
// The embedded struct marshals inline, so the wire shape is unchanged
// from when this was one flat type; what changed is that the fields below
// are declared on the operation that honours them and on no other.
type backupSetRequest struct {
	backupSetSpec
	// RunImmediately is the wizard's "Save, enable & run" tier (as
	// opposed to "Save & enable", RunImmediately: false, Disabled:
	// false, or "Save disabled", Disabled: true). See
	// service.CreateBackupSetRequest.RunImmediately's own doc for why
	// this is folded into create rather than a separate endpoint: this
	// issue's scope is exactly the four endpoints named in its own
	// title, and a per-backup-set-scoped run is not one of them.
	RunImmediately bool `json:"run_immediately"`

	// AcknowledgeRepoint answers the refusal a create over an id that
	// already has artifacts on record gets when it points somewhere other
	// than where that history came from (issue #411,
	// core/service/backupsetrepoint.go). It is not a field of the backup
	// set and nothing persists it, which is why it sits here and not on
	// backupSetSpec: first-run shares that spec, and a first configuration
	// is not a create over anything.
	AcknowledgeRepoint bool `json:"acknowledge_repoint"`
}

// backupSetResponse is the wire shape of one backup set, shared by
// GET /api/v1/backup-sets, GET /api/v1/backup-sets/{id} and the 201 POST
// /api/v1/backup-sets returns. It carries nothing service.BackupSet does
// not already have.
type backupSetResponse struct {
	ID                 string   `json:"id"`
	SourceName         string   `json:"source_name"`
	Name               string   `json:"name"`
	Host               string   `json:"host"`
	Port               int      `json:"port"`
	User               string   `json:"user"`
	RemotePath         string   `json:"remote_path"`
	LocalPath          string   `json:"local_path"`
	Include            []string `json:"include"`
	CompletionStrategy string   `json:"completion_strategy"`
	// StableForSeconds is the window the "stable" completion strategy
	// waits for, and 0 for every other strategy. Served (issue #350)
	// because an edit surface offering the strategy has to be able to
	// offer the window with it: without this a client could select
	// "stable" and had nothing to send alongside it, which core refuses,
	// so the only possible outcome was a save that failed.
	StableForSeconds int `json:"stable_for_seconds"`
	// StaleAfterSeconds is FR-24's freshness budget: how long this set may
	// go without a fresh backup before it is reported as stale.
	//
	// It is here because it was the one field this API could WRITE and
	// could not read back. BackupSetSpec has always accepted it and
	// UpdateBackupSetRequest has always been able to change it, so a
	// client that set it had no way to show what it had set, and a CLI
	// routing a create through this engine printed "not reported" for a
	// value it had just sent (issue #555, folded in with the routed-write
	// guard because it is the same command's own report).
	//
	// Never omitted, the same discipline the two booleans below follow: a
	// caller has to be able to tell a value from "this build does not
	// report it". It is always positive on a configured set, because a
	// configuration that leaves it unset is one core refuses to load.
	StaleAfterSeconds int `json:"stale_after_seconds"`
	// ValidatorID is the registered validator this backup set selected,
	// or "" for none. The id only: what it resolves to is a server-side
	// path, and this package never puts one on the wire (see
	// service.SSHKeyRef.KeyFile for the same rule applied to imported
	// keys).
	ValidatorID string `json:"validator_id"`
	// SSHKeyID is the key-store id this set's key resolves to (#592), the
	// same value ImportSSHKey returned and GET /api/v1/ssh-keys lists.
	// The id only: what it resolves to is a server-side path, and this
	// package never puts one on the wire (service.SSHKeyRef.KeyFile).
	//
	// Never omitted, and EMPTY IS A REAL ANSWER: it means this set uses a
	// key this deployment does not manage, which is every set pointing at
	// a mounted or hand-provisioned key file. It is here because
	// UpdateBackupSetRequest has always been able to WRITE ssh_key_id and
	// nothing could read it back, so a surface offering to replace a
	// set's key could not name the key being replaced.
	SSHKeyID string `json:"ssh_key_id"`
	Disabled bool   `json:"disabled"`
	// ReadOnly is the fully-resolved answer (service.BackupSet.ReadOnly):
	// see backupSetSpec.ReadOnly's own doc for what setting it means.
	// Never omitted, the same discipline Disabled above already follows:
	// a caller must be able to tell "not read-only" from "this build does
	// not report it".
	ReadOnly bool `json:"read_only"`
	// RetentionIsOverride is whether this set declares its own retention
	// policy (issue #333). Never omitted, for the reason ReadOnly above
	// is not: a list has to be able to tell "retained under the
	// deployment's policy" from "this build does not report it".
	//
	// The chain is not here. A list of sets would carry one copy of a
	// whole chain per set, and the surface that shows a chain is
	// /backup-sets/{source}/{set}/retention, which serves it on demand
	// alongside the deployment's own.
	RetentionIsOverride bool `json:"retention_is_override"`

	// PollIntervalSeconds is this set's own poll-interval override
	// (issue #845), and NULL when the set inherits the deployment's.
	//
	// Null is a real answer and a form has to render it as one: filling
	// the box with the deployment's number would make the next save an
	// explicit override, permanently detaching this set from a default
	// it was tracking. That is capacity's backup_root_configured
	// problem in a different field.
	PollIntervalSeconds *int `json:"poll_interval_seconds"`

	// EffectivePollIntervalSeconds is how often this set is actually
	// polled: its override, or the deployment's default. Served beside
	// the override so a form can label the inherit option with the real
	// number without a second request, and resolved by the engine so no
	// client combines the two scopes itself.
	EffectivePollIntervalSeconds int `json:"effective_poll_interval_seconds"`

	// TrustedHostKeys is what this set's known_hosts actually pins for its
	// own address (service.BackupSet.TrustedHostKeys). Omitted, not sent
	// as an empty list, and the difference is the whole point: absent
	// reads as "this deployment could not report what this set trusts",
	// which is what a client has to say out loud instead of rendering a
	// blank where a fingerprint goes.
	//
	// This is the field that ends a literal. The Web UI's connection panel
	// printed a hardcoded "ssh-ed25519" beside an empty fingerprint on
	// every deployment, because nothing on this response carried a host
	// key at all, and the halt banner for a CHANGED host key sent an
	// operator to that panel to compare a fingerprint that was not there.
	TrustedHostKeys []trustedHostKeyResponse `json:"trusted_host_keys,omitempty"`
	// TrustedHostKeyRecordedAt is when this deployment last wrote that
	// anchor, omitted when the set points at a known_hosts file this
	// deployment did not write. See service.trustedHostKeysFor for why a
	// hand-maintained file's timestamp answers a different question.
	TrustedHostKeyRecordedAt string `json:"trusted_host_key_recorded_at,omitempty"`
	// ConnectionUnverified is issue #624's mark: this set was written
	// without its connection ever having been proven. Omitted when false,
	// which is the ordinary case and is also how an engine built before
	// this field answers, so a client reads absence as "nothing here says
	// this was skipped" rather than as a claim either way.
	//
	// It is here so a surface can DRAW the difference. A set nobody proved
	// and a set checked against a real server were the same set on every
	// screen, which is what made --no-verify a hole rather than an escape
	// hatch.
	ConnectionUnverified bool `json:"connection_unverified,omitempty"`

	// EPIC K's engine seam, read back (#788).
	//
	// engine is never omitted and is always the RESOLVED value: a
	// configuration that says nothing reads "artifact" here rather than
	// empty, because a client that had to know the default would be a
	// second place the default is decided. This is the field a surface
	// draws "Artifact" or "Incremental" from, which is EPIC K's own
	// acceptance criterion.
	//
	// Everything below it is omitted for an artifact set, which has no
	// repository, no lineage and no verification budget. Absent is the
	// honest answer there, and a zero would read as a configured budget
	// of none.
	Engine                               string `json:"engine"`
	UUID                                 string `json:"uuid,omitempty"`
	RepositoryDomain                     string `json:"repository_domain,omitempty"`
	SourceConsistency                    string `json:"source_consistency,omitempty"`
	VerificationLevel                    string `json:"verification_level,omitempty"`
	VerificationSamplePercent            int    `json:"verification_sample_percent,omitempty"`
	VerificationFullEverySeconds         int64  `json:"verification_full_every_seconds,omitempty"`
	VerificationRestoreDrillEverySeconds int64  `json:"verification_restore_drill_every_seconds,omitempty"`
	SourceMountPrefix                    string `json:"source_mount_prefix,omitempty"`
}

// trustedHostKeyResponse is one pinned host key on the wire: the algorithm
// and the SHA256 fingerprint, which are the two strings an operator
// compares against the server in front of them. Never key material.
type trustedHostKeyResponse struct {
	Algorithm   string `json:"algorithm"`
	Fingerprint string `json:"fingerprint"`
}

func toBackupSetResponse(bs service.BackupSet) backupSetResponse {
	var trusted []trustedHostKeyResponse
	for _, k := range bs.TrustedHostKeys {
		trusted = append(trusted, trustedHostKeyResponse{Algorithm: k.Algorithm, Fingerprint: k.Fingerprint})
	}
	recordedAt := ""
	if !bs.TrustedHostKeyRecordedAt.IsZero() {
		recordedAt = bs.TrustedHostKeyRecordedAt.UTC().Format(time.RFC3339)
	}
	return backupSetResponse{
		ID:                  bs.ID,
		SourceName:          bs.SourceName,
		Name:                bs.Name,
		Host:                bs.Host,
		Port:                bs.Port,
		User:                bs.User,
		RemotePath:          bs.RemotePath,
		LocalPath:           bs.LocalPath,
		Include:             bs.Include,
		CompletionStrategy:  bs.CompletionStrategy,
		StableForSeconds:    int(bs.StableFor / time.Second),
		StaleAfterSeconds:   int(bs.StaleAfter / time.Second),
		ValidatorID:         string(bs.ValidatorID),
		SSHKeyID:            bs.SSHKeyID,
		Disabled:            bs.Disabled,
		ReadOnly:            bs.ReadOnly,
		RetentionIsOverride: bs.RetentionIsOverride,

		PollIntervalSeconds:          secondsPointerFromDuration(bs.PollInterval),
		EffectivePollIntervalSeconds: int(bs.EffectivePollInterval / time.Second),

		TrustedHostKeys:          trusted,
		TrustedHostKeyRecordedAt: recordedAt,
		ConnectionUnverified:     bs.ConnectionUnverified,

		Engine:                               bs.Engine,
		UUID:                                 bs.UUID,
		RepositoryDomain:                     bs.RepositoryDomain,
		SourceConsistency:                    bs.SourceConsistency,
		VerificationLevel:                    bs.VerificationLevel,
		VerificationSamplePercent:            bs.VerificationSamplePercent,
		VerificationFullEverySeconds:         int64(bs.VerificationFullEvery / time.Second),
		VerificationRestoreDrillEverySeconds: int64(bs.VerificationRestoreDrillEvery / time.Second),
		SourceMountPrefix:                    bs.SourceMountPrefix,
	}
}

// createBackupSetResponse embeds backupSetResponse (its fields marshal
// inline, at the top level) plus, only when the request's
// RunImmediately was set and honoured, the run_cycle Operation it kicked
// off — the same shape POST /api/v1/operations already returns for one,
// so a client parses it identically either way. RunError is the mandatory
// review's M6 fix (PR #155): set only when the backup set itself was
// created successfully but its requested immediate run failed to start —
// still a 201 (the resource this route creates DOES exist now), never
// alongside Operation (at most one of the two is ever non-empty, mirroring
// service.Operation's own Result/Error convention).
type createBackupSetResponse struct {
	backupSetResponse
	Operation *operationResponse `json:"operation,omitempty"`
	RunError  string             `json:"run_error,omitempty"`
}

// listBackupSetsResponse is GET /api/v1/backup-sets' body: an object
// with one array field, not a bare JSON array at the top level, so a
// future field (pagination, a total count) can be added without
// breaking every existing client the way changing a top-level array's
// shape would.
type listBackupSetsResponse struct {
	BackupSets []backupSetResponse `json:"backup_sets"`
}

// createBackupSet is POST /api/v1/backup-sets: issue #146's
// create-backup-set endpoint, the write path the wizard's three Save
// buttons call. State-changing but non-destructive
// (docs/EPIC-B-multi-nas.md §50: "create/edit backup set"), so it is
// CSRF-protected (router.go) but not unconditionally gated behind the
// destructive-ops gate (gate.go) — creating a backup set never touches,
// let alone deletes, remote or local backup data by itself.
//
// # run_immediately IS gated (mandatory review finding M3, PR #155)
//
// body.RunImmediately turns this same call into "also start a
// run_cycle", the exact action requireDestructiveGate exists to block
// (handlers_operations.go's submitOperation, the ONLY route
// router.go wraps in that middleware, exists to run that action too) —
// so this branch is checked against h.gate directly, below, before the
// backend is ever called. This is deliberately NOT route-level
// middleware: a caller that only wants to persist (RunImmediately false,
// the common case, and "Save disabled") must stay unaffected by whether
// #92's gate has been verified yet, matching
// destructiveGateExemptRoutes' own justification
// (router_test.go) for why this route is structurally exempt from
// requireDestructiveGate in the first place.
//
// # The gate refuses the RUN, not the CREATE (issue #597)
//
// It used to refuse the whole call with a 403 and persist nothing, and
// that cost an operator their entire wizard submission for a reason that
// has nothing to do with creating a backup set. Creating one touches no
// backup data, which is why this route is exempt from the middleware at
// all; refusing the create because the RUN could not happen refused a
// non-destructive action for a destructive one's reason, and the wizard
// reported it as "Could not save this backup set", which was not what
// happened. Retrying then hit config.Validate's duplicate-id rejection
// with no way to tell "already exists because your last attempt worked"
// from "your request was wrong from the start" — M6's own argument,
// below, applied to the case M3 created.
//
// So the set is persisted, RunImmediately is cleared before the backend
// is called, and the 201 carries RunError with the gate's own sentence.
// That is the shape M6 built and nothing in production could reach.
func (h *handlers) createBackupSet(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateBackupSetBodyBytes)

	var body backupSetRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxCreateBackupSetBodyBytes)
		return
	}

	// The gate decides whether the RUN happens, not whether the CREATE
	// does. With it shut the set is still persisted and the response says
	// the run did not start; see this handler's own doc, and the test that
	// pins both halves.
	runRefusal := ""
	runImmediately := body.RunImmediately
	if runImmediately && !destructiveGatePassed(h.gate) {
		runImmediately = false
		runRefusal = destructiveGateRefusal
	}

	req := service.CreateBackupSetRequest{
		SourceName:          body.SourceName,
		Name:                body.Name,
		Host:                body.Host,
		Port:                body.Port,
		User:                body.User,
		SSHKeyID:            body.SSHKeyID,
		KnownHostsLine:      body.KnownHostsLine,
		RemotePath:          body.RemotePath,
		LocalPath:           body.LocalPath,
		Include:             body.Include,
		CompletionStrategy:  body.CompletionStrategy,
		ValidatorID:         service.ValidatorID(body.ValidatorID),
		StableFor:           secondsToDuration(body.StableForSeconds),
		StaleAfter:          secondsToDuration(body.StaleAfterSeconds),
		Disabled:            body.Disabled,
		ReadOnly:            body.ReadOnly,
		SkipConnectionCheck: body.SkipConnectionCheck,

		Engine:                        body.Engine,
		UUID:                          body.UUID,
		RepositoryDomain:              body.RepositoryDomain,
		SourceConsistency:             body.SourceConsistency,
		VerificationLevel:             body.VerificationLevel,
		VerificationSamplePercent:     body.VerificationSamplePercent,
		VerificationFullEvery:         time.Duration(body.VerificationFullEverySeconds) * time.Second,
		VerificationRestoreDrillEvery: time.Duration(body.VerificationRestoreDrillEverySeconds) * time.Second,
		SourceMountPrefix:             body.SourceMountPrefix,

		RunImmediately:     runImmediately,
		AcknowledgeRepoint: body.AcknowledgeRepoint,
		Actor:              actorFromContext(r.Context()),
	}

	result, err := h.backend.CreateBackupSet(r.Context(), req)
	if err != nil {
		if result.Set.ID == "" {
			// Creation itself never happened — nothing was persisted, so
			// the ordinary error mapping (400, 409 or 500, per the
			// failure kind) is the whole story.
			h.writeBackupSetError(w, r, err)
			return
		}
		// Mandatory review finding M6 (PR #155): the backup set IS
		// already durably persisted and hot-reloaded at this point (see
		// service.CreateBackupSet's own doc) — only the immediate
		// run_cycle it also requested failed to start. Collapsing this
		// to a bare 500, as if creation itself had failed, is actively
		// misleading: a caller that retries the whole create next hits
		// config.Validate's duplicate-id rejection instead, with no way
		// to tell "already exists because your last attempt actually
		// worked" apart from "your request was wrong from the start".
		// 201, not 500: the resource this route creates was, in fact,
		// created.
		resp := createBackupSetResponse{
			backupSetResponse: toBackupSetResponse(result.Set),
			RunError:          runStartErrorMessage(err),
		}
		writeJSON(w, http.StatusCreated, resp)
		return
	}

	resp := createBackupSetResponse{
		backupSetResponse: toBackupSetResponse(result.Set),
		// Empty unless the gate refused the run above, in which case it
		// carries the gate's own sentence. At most one of RunError and
		// Operation is ever set: nothing was asked to run, so there is no
		// operation for the client to poll.
		RunError: runRefusal,
	}
	if result.Operation != nil {
		op := toOperationResponse(*result.Operation)
		resp.Operation = &op
	}
	writeJSON(w, http.StatusCreated, resp)
}

// runStartErrorMessage turns the error CreateBackupSet returns when the
// backup set was persisted but its requested immediate run_cycle failed
// to start into a message safe to put on the wire, using the same
// sentinel-to-safe-string classification submitOperation
// (handlers_operations.go) already applies to the identical
// SubmitRunCycle error vocabulary — err here always wraps one of those
// same sentinels (see service.CreateBackupSetRequest.RunImmediately's
// doc), so nothing from a deeper, unclassified layer can reach this far.
func runStartErrorMessage(err error) string {
	switch {
	case errors.Is(err, service.ErrConfigRevisionStale),
		errors.Is(err, service.ErrIdempotencyKeyConflict),
		errors.Is(err, service.ErrOperationAlreadyRunning),
		errors.Is(err, service.ErrInvalidRequest):
		return err.Error()
	default:
		return "the backup set was created, but starting the requested run failed"
	}
}

// listBackupSets is GET /api/v1/backup-sets: read-only (§50), no CSRF, no
// destructive gate — see router.go.
func (h *handlers) listBackupSets(w http.ResponseWriter, r *http.Request) {
	sets, err := h.backend.ListBackupSets(r.Context())
	if err != nil {
		h.internalError(w, r, "INTERNAL", "failed to list backup sets", err)
		return
	}
	resp := listBackupSetsResponse{BackupSets: make([]backupSetResponse, 0, len(sets))}
	for _, s := range sets {
		resp.BackupSets = append(resp.BackupSets, toBackupSetResponse(s))
	}
	writeJSON(w, http.StatusOK, resp)
}

// getBackupSet is GET /api/v1/backup-sets/{id}: read-only (§50). id is
// read from chi's "*" wildcard param, not a named {id} segment: a backup
// set's id is "source/name" (model.BackupSetID.String()), and a plain
// chi path parameter never matches a literal "/" within one segment (see
// router.go's route-registration comment for why {id:.*} does not work
// either).
func (h *handlers) getBackupSet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "*")
	set, err := h.backend.GetBackupSet(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrBackupSetNotFound) {
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
			return
		}
		h.internalError(w, r, "INTERNAL", "failed to load backup set", err)
		return
	}
	writeJSON(w, http.StatusOK, toBackupSetResponse(set))
}

// writeBackupSetError maps a CreateBackupSet (or, via the same sentinels,
// any other backupsets.go method) error to the HTTP status/code this
// package's other handlers already establish the vocabulary for
// (handlers_operations.go's identical switch is the direct precedent).
func (h *handlers) writeBackupSetError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidRequest):
		// Safe to echo: every ErrInvalidRequest this package returns is
		// built from its own field-description strings and the caller's
		// own request values (config.ValidationError, or backupsets.go's
		// own validateCreateRequest), never from state/rclone internals.
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	case errors.Is(err, service.ErrSSHKeyNotFound):
		writeError(w, http.StatusBadRequest, "SSH_KEY_NOT_FOUND", "the referenced ssh_key_id does not exist; import a key first")
	case errors.Is(err, service.ErrHistoryRepointNotAcknowledged):
		// 409 for the same reason the edit's own refusal below is one,
		// and its own code because the two are not interchangeable to a
		// client: this one is about an id rather than a resource that
		// exists, and what it offers an operator is "create anyway"
		// rather than "save anyway". Safe to echo on the same terms.
		writeError(w, http.StatusConflict, "BACKUP_SET_HISTORY_REPOINT_NOT_ACKNOWLEDGED", err.Error())
	case errors.Is(err, service.ErrHostKeyChangeNotAcknowledged):
		// 409 and its own code, beside the two repoint refusals rather
		// than folded into either: this one is about the host's identity
		// rather than the data's, and what it offers an operator is
		// "trust the new key anyway" rather than "save anyway". A client
		// that could not tell them apart would offer the wrong
		// confirmation, and for a host key that is the confirmation that
		// matters. Safe to echo on the same terms: core/service builds
		// this message from its own text plus two fingerprints and the
		// caller's own address.
		writeError(w, http.StatusConflict, "BACKUP_SET_HOST_KEY_CHANGE_NOT_ACKNOWLEDGED", err.Error())
	case errors.Is(err, service.ErrConnectionNotProven):
		// 409 and its own code, beside the three refusals above rather
		// than folded into any of them: this one is not about a decision
		// the operator has to confirm, it is about the world not being
		// the way the edit assumes, and what it offers is "fix the source
		// and save again" or "save it unproven" rather than "do it
		// anyway". A client that read it as INVALID_REQUEST would tell an
		// operator their form was wrong when their form was right and
		// their server was down.
		//
		// Safe to echo: core/service builds this message from
		// internal/sourcecheck's own sentences and the caller's own
		// values, never from a transport error's text (issue #624).
		writeError(w, http.StatusConflict, "BACKUP_SET_CONNECTION_NOT_PROVEN", err.Error())
	case errors.Is(err, service.ErrSourceNotWritable):
		// 409 and its own code, next to ErrConnectionNotProven rather
		// than folded into it, because the two offer an operator
		// different things (issue #852). That one says "the source is
		// not answering, fix it or save unproven"; this one says "the
		// source answered, and these credentials cannot write there, so
		// this set cannot delete from it" — and what it offers is "save
		// it read-only" or "grant write permission on the source". A
		// client that could not tell them apart would offer the wrong
		// way out, and `skip_connection_check` is NOT a way out of this
		// one.
		//
		// Safe to echo, on the same terms: core/service builds this
		// message from its own text alone.
		writeError(w, http.StatusConflict, "BACKUP_SET_SOURCE_NOT_WRITABLE", err.Error())
	case errors.Is(err, service.ErrRepointNotAcknowledged):
		// 409 rather than 400, because this is not a malformed request:
		// it is a well-formed one whose consequences the caller has to
		// see first, and it conflicts with the state of the resource
		// (artifacts already on record for this set) rather than with
		// its own shape. A client that could only see "400" could offer
		// an operator nothing better than the same failure again.
		//
		// Safe to echo, on the same terms as ErrInvalidRequest above:
		// core/service builds this message from its own text plus the
		// caller's own path values and a count, never from a state or
		// rclone internal.
		writeError(w, http.StatusConflict, "BACKUP_SET_REPOINT_NOT_ACKNOWLEDGED", err.Error())
	case errors.Is(err, service.ErrConfigNotFileBacked):
		h.internalError(w, r, "INTERNAL", "this deployment has no configuration file to persist to", err)
	default:
		// Deliberately not err.Error(): an unclassified error could carry
		// filesystem or rclone-internal text (see handlers_operations.go's
		// identical default case for the same reasoning).
		//
		// "write" rather than "create": this function serves the update
		// path too (issue #350), and telling an operator whose edit
		// failed that a creation failed sends them looking for a set
		// that was never being created.
		h.internalError(w, r, "INTERNAL", "failed to write backup set", err)
	}
}

// writeDecodeError maps a json.Decoder error the same way submitOperation
// (handlers_operations.go) already does for POST /api/v1/operations,
// factored out here so this file and that one do not each carry their own
// copy of the http.MaxBytesError-vs-anything-else switch.
func writeDecodeError(w http.ResponseWriter, err error, limit int64) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			fmt.Sprintf("request body exceeds the %d byte limit", limit))
		return
	}
	writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed JSON body")
}

// secondsToDuration turns a wire "N seconds" integer into a
// time.Duration; the wizard's JSON body carries seconds (a plain number)
// rather than a Go-duration-string, so the HTTP layer is the one place
// that conversion happens, before service.CreateBackupSetRequest sees a
// real time.Duration like the rest of this codebase already uses.
func secondsToDuration(s int) time.Duration {
	return time.Duration(s) * time.Second
}

// setEnabledRequest is POST /api/v1/backup-sets/{id}/enabled's body.
type setEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

// setBackupSetEnabled is POST /api/v1/backup-sets/{id}/enabled: turn one
// backup set on or off.
//
// It carries requireCSRF and NOT requireDestructiveGate, which puts it in
// the same tier as createBackupSet and updateSettings
// (destructiveGateExemptRoutes, router_test.go). Nothing reachable from
// here touches, moves or deletes a byte of backup data: a disabled set is
// excluded from every run cycle, and everything already backed up stays
// exactly where it is, which core/service pins directly
// (TestSetBackupSetEnabled_DisablingDeletesNothing).
//
// The direction that sounds dangerous is turning a set OFF, because new
// restore points stop being made and freshness decays. That is not hidden:
// FR-24's health computation reports the set going stale, GET
// /api/v1/system/health serves it, and the same call turns it back on.
//
// The id is read from two named segments rather than a catch-all: unlike
// an artifact id this one has a fixed arity of two, and this route needs a
// literal "/enabled" tail after it, which a catch-all would swallow.
func (h *handlers) setBackupSetEnabled(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateBackupSetBodyBytes)

	var body setEnabledRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxCreateBackupSetBodyBytes)
		return
	}

	id := chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
	updated, err := h.backend.SetBackupSetEnabled(r.Context(), id, body.Enabled)
	if err != nil {
		if errors.Is(err, service.ErrBackupSetNotFound) {
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
			return
		}
		h.writeBackupSetError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toBackupSetResponse(updated))
}

// setReadOnlyRequest is POST /api/v1/backup-sets/{id}/read-only's body.
type setReadOnlyRequest struct {
	ReadOnly bool `json:"read_only"`
}

// setBackupSetReadOnly is POST /api/v1/backup-sets/{source}/{set}/read-only
// (issue #316): declare, or withdraw, one already-persisted backup set's
// read-only status without hand-editing config.yaml — the CRUD-parity
// counterpart setBackupSetEnabled already has for `disabled`.
//
// Same tier as setBackupSetEnabled: requireCSRF but NOT
// requireDestructiveGate. Nothing reachable from here touches, moves or
// deletes a byte of backup data either direction. Turning read-only ON
// only PREVENTS a future deletion (service.SetBackupSetReadOnly's own
// doc); turning it back OFF does not reach back and delete anything this
// manager already retained under it, so neither direction is the
// "delete a byte of backup data" requireDestructiveGate exists to gate.
//
// Since issue #852 the OFF direction runs a real connection check first
// and answers 409 BACKUP_SET_SOURCE_NOT_WRITABLE when the source's own
// credentials cannot write there: "delete from the source after backup"
// is a promise this deployment has to be able to keep. So this route can
// now make an outbound SSH connection in one direction, which is the same
// side effect POST /backup-sets already has and the reason both carry
// requireCSRF.
//
// The id is read from two named segments, like setBackupSetEnabled
// beside it, for the identical reason: a backup set id is always exactly
// source/name, a fixed arity, and this route needs a literal
// "/read-only" tail a catch-all would swallow.
func (h *handlers) setBackupSetReadOnly(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateBackupSetBodyBytes)

	var body setReadOnlyRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxCreateBackupSetBodyBytes)
		return
	}

	id := chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
	updated, err := h.backend.SetBackupSetReadOnly(r.Context(), id, body.ReadOnly)
	if err != nil {
		if errors.Is(err, service.ErrBackupSetNotFound) {
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
			return
		}
		h.writeBackupSetError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toBackupSetResponse(updated))
}

// removeBackupSet is DELETE /api/v1/backup-sets/{source}/{set} (issue
// #391): take one backup set out of the configuration.
//
// It answers 204 with no body, matching releaseBackupSetEditHold, because
// there is no resource left to return. Every other write in this group
// returns the set it changed; this one changed it into nothing.
//
// A set this deployment does not configure, INCLUDING one an earlier call
// already removed, gets 404 BACKUP_SET_NOT_FOUND. So the effect is
// idempotent and the status deliberately is not: a client that retries
// after a lost response learns the set is gone either way, and a client
// that typed the name wrong is told so rather than being congratulated on
// a removal that did not happen. That is the whole shape of the defect
// this issue is about, and it is not worth reintroducing for the sake of
// a tidier verb table.
//
// The id comes from two named segments, exactly like /enabled,
// /read-only and the PATCH above, for the identical reason: a backup set
// id is always exactly source/name.
func (h *handlers) removeBackupSet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
	if err := h.backend.RemoveBackupSet(r.Context(), id); err != nil {
		if errors.Is(err, service.ErrBackupSetNotFound) {
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
			return
		}
		h.writeBackupSetError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// updateBackupSetRequest is PATCH /api/v1/backup-sets/{source}/{set}'s
// body: issue #350's edit surface, and a sparse one.
//
// Every field is a pointer, and that is load-bearing rather than
// stylistic. encoding/json leaves a pointer nil when its key is absent
// and sets it when the key is present, which is what lets this route
// carry "change only remote_path" as a request that is structurally
// incapable of also moving local_path. A value-typed body could not: a
// missing "port" and an explicit "port": 0 would arrive identically, and
// 0 is a real answer here (it selects the default port), so the Web UI's
// per-box Save would end up shipping every other box's current contents
// alongside the one an operator actually pressed Save on.
//
// It deliberately carries no name/source_name, no ssh_key_id and no
// known_hosts_line. See core/service/backupsetupdate.go's own package doc
// for why each of those is not an edit.
type updateBackupSetRequest struct {
	Host       *string   `json:"host"`
	Port       *int      `json:"port"`
	User       *string   `json:"user"`
	RemotePath *string   `json:"remote_path"`
	LocalPath  *string   `json:"local_path"`
	Include    *[]string `json:"include"`

	CompletionStrategy *string `json:"completion_strategy"`
	StableForSeconds   *int    `json:"stable_for_seconds"`
	StaleAfterSeconds  *int    `json:"stale_after_seconds"`

	// PollIntervalSeconds changes how often this set's source is checked
	// (issue #845). Absent leaves it alone, like every field here; an
	// explicit 0 is the spelling of "inherit the deployment's interval
	// again", which is unambiguous because the engine's floor
	// (schema.service.min_poll_interval_seconds) makes zero a value no
	// caller could be asking for.
	PollIntervalSeconds *int `json:"poll_interval_seconds"`

	ValidatorID *string `json:"validator_id"`

	// SSHKeyID and KnownHostsLine are issue #572's two: the key this set
	// authenticates with and the host key it trusts, both editable in
	// place since the only alternative was removing the set and creating
	// it again. Pointers like every other field of the set above, so a
	// body that never mentions them leaves both alone. Neither carries
	// material: an id an import produced, and the line a probe returned.
	SSHKeyID       *string `json:"ssh_key_id"`
	KnownHostsLine *string `json:"known_hosts_line"`

	// AcknowledgeRepoint is not a field of the backup set and is not a
	// pointer for that reason: it answers one refusal for one request
	// rather than carrying a stored value. Absent is false, which is the
	// honest reading of a client that did not mention it. See
	// core/service/backupsetrepoint.go for what it acknowledges.
	AcknowledgeRepoint bool `json:"acknowledge_repoint"`

	// AcknowledgeHostKeyChange is the same shape answering a different
	// refusal: that this edit means to trust a different host key for the
	// same host. Two flags rather than one, for the reason
	// core/service/backupsethostkey.go gives.
	AcknowledgeHostKeyChange bool `json:"acknowledge_host_key_change"`

	// SkipConnectionCheck writes this edit without proving the connection
	// first (issue #624). An edit that changes host, port, user,
	// ssh_key_id, known_hosts_line or remote_path is checked against the
	// source before it is written and refused with
	// BACKUP_SET_CONNECTION_NOT_PROVEN when the check fails; this is the
	// deliberate opt-out, and a set written under it is marked
	// connection_unverified until a test passes. Not a pointer, for the
	// reason the two acknowledgements above are not.
	SkipConnectionCheck bool `json:"skip_connection_check"`

	// EPIC K's editable verification budget (#788). Five fields and no
	// more: engine, uuid and repository_domain are create-only, because
	// changing any of them is a migration rather than an edit. See
	// core/service's own note above UpdateBackupSetRequest.isEmpty.
	SourceConsistency                    *string `json:"source_consistency,omitempty"`
	VerificationLevel                    *string `json:"verification_level,omitempty"`
	VerificationSamplePercent            *int    `json:"verification_sample_percent,omitempty"`
	VerificationFullEverySeconds         *int64  `json:"verification_full_every_seconds,omitempty"`
	VerificationRestoreDrillEverySeconds *int64  `json:"verification_restore_drill_every_seconds,omitempty"`
}

// updateBackupSet is PATCH /api/v1/backup-sets/{source}/{set} (issue
// #350): change one already-persisted backup set's definition.
//
// PATCH rather than PUT, and PATCH rather than a POST tail like /enabled
// and /read-only beside it. Those two are single-valued toggles with a
// name; this is a partial edit of a resource, which is what PATCH means,
// and this package already uses it for exactly that shape at PATCH
// /api/v1/settings. A PUT would promise whole-resource replacement, which
// this route deliberately does not offer: a client that sent a PUT
// missing a field would be asking for it to be cleared, and in a backup
// tool that is the kind of promise that quietly empties an include list.
//
// requireCSRF and NOT requireDestructiveGate, following createBackupSet's
// own tier (destructiveGateExemptRoutes, router_test.go). §50 puts
// "create/edit backup set" in one bucket, and nothing reachable from here
// touches, moves or deletes a byte of backup data: the config file
// changes, and the next cycle acts on the new definition. The gate exists
// for run_immediately and for retention apply, not for writing a set.
//
// The id comes from two named segments rather than the catch-all
// getBackupSet uses, exactly like /enabled and /read-only: a backup set
// id is always exactly source/name, a fixed arity, so a route that says
// so lets chi answer a malformed id with a 404 instead of a handler
// having to interpret one.
func (h *handlers) updateBackupSet(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateBackupSetBodyBytes)

	var body updateBackupSetRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxCreateBackupSetBodyBytes)
		return
	}

	pollInterval, err := checkedSecondsPointerToDuration(body.PollIntervalSeconds, "poll_interval_seconds")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}

	req := service.UpdateBackupSetRequest{
		Host:               body.Host,
		Port:               body.Port,
		User:               body.User,
		RemotePath:         body.RemotePath,
		LocalPath:          body.LocalPath,
		Include:            body.Include,
		CompletionStrategy: body.CompletionStrategy,
		StableFor:          secondsPointerToDuration(body.StableForSeconds),
		StaleAfter:         secondsPointerToDuration(body.StaleAfterSeconds),
		PollInterval:       pollInterval,
		SSHKeyID:           body.SSHKeyID,
		KnownHostsLine:     body.KnownHostsLine,

		AcknowledgeRepoint:       body.AcknowledgeRepoint,
		AcknowledgeHostKeyChange: body.AcknowledgeHostKeyChange,
		SkipConnectionCheck:      body.SkipConnectionCheck,

		SourceConsistency:             body.SourceConsistency,
		VerificationLevel:             body.VerificationLevel,
		VerificationSamplePercent:     body.VerificationSamplePercent,
		VerificationFullEvery:         secondsPointerToDurationFromInt64(body.VerificationFullEverySeconds),
		VerificationRestoreDrillEvery: secondsPointerToDurationFromInt64(body.VerificationRestoreDrillEverySeconds),
	}
	if body.ValidatorID != nil {
		id := service.ValidatorID(*body.ValidatorID)
		req.ValidatorID = &id
	}

	id := chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
	updated, err := h.backend.UpdateBackupSet(r.Context(), id, req)
	if err != nil {
		if errors.Is(err, service.ErrBackupSetNotFound) {
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
			return
		}
		h.writeBackupSetError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toBackupSetResponse(updated))
}

// secondsPointerFromDuration is the read direction of
// secondsPointerToDuration: a duration a backup set may not have at all
// becomes a nullable number, so "this set inherits" stays distinguishable
// from "this set polls at the same interval the deployment does".
func secondsPointerFromDuration(d *time.Duration) *int {
	if d == nil {
		return nil
	}
	s := int(*d / time.Second)
	return &s
}

// secondsPointerToDuration is secondsToDuration for a field that has to
// keep telling "absent" apart from "zero". It returns nil for nil, so a
// body that never mentioned stable_for_seconds reaches core/service as a
// nil *time.Duration rather than as a pointer to zero, which
// core/service would read as an operator asking for zero.
func secondsPointerToDuration(s *int) *time.Duration {
	if s == nil {
		return nil
	}
	d := secondsToDuration(*s)
	return &d
}

// maxDurationSeconds is the largest whole number of seconds a
// time.Duration can carry: it is nanoseconds in an int64, so anything
// above this overflows.
const maxDurationSeconds = int64(math.MaxInt64 / int64(time.Second))

// checkedSecondsPointerToDuration is secondsPointerToDuration for a field
// where the multiplication itself can lie.
//
// A JSON body may carry any number the decoder accepts, and seconds ×
// 1e9 wraps silently: 2^55 seconds lands on exactly zero, and zero is
// not a rejected value on a poll interval -- it is the spelling of
// "inherit the deployment's interval again", so the largest number a
// client can send would quietly CLEAR an operator's override and be
// answered 200. Other values wrap to short, entirely plausible cadences,
// which is the same failure pointed at the operator's sources.
//
// So the bound is checked before the multiply, and the refusal names the
// number rather than the internal type: an operator who typed too many
// zeroes needs to see that, not "invalid request".
func checkedSecondsPointerToDuration(s *int, field string) (*time.Duration, error) {
	if s == nil {
		return nil, nil
	}
	if int64(*s) > maxDurationSeconds || int64(*s) < -maxDurationSeconds {
		return nil, fmt.Errorf("%s is %d seconds, which is longer than this engine can express as an interval (at most %d seconds)", field, *s, maxDurationSeconds)
	}
	d := secondsToDuration(*s)
	return &d, nil
}

// secondsPointerToDurationFromInt64 is secondsPointerToDuration for a
// cadence, which is spelled int64 on the wire because a verification
// cadence is measured in weeks and an int32 of seconds runs out at
// sixty-eight years. nil stays nil, which is "leave this alone"; zero is
// a real value here and means never.
func secondsPointerToDurationFromInt64(v *int64) *time.Duration {
	if v == nil {
		return nil
	}

	d := time.Duration(*v) * time.Second

	return &d
}
