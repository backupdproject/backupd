package webhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/backupdproject/backupd/core/service"
)

// EPIC L's configuration and validation surface (#813):
//
//	GET    /api/v1/settings/workflow
//	PATCH  /api/v1/settings/workflow
//	GET    /api/v1/settings/workflow/environment
//	PUT    /api/v1/settings/workflow/environment/{name}
//	DELETE /api/v1/settings/workflow/environment/{name}
//	GET    /api/v1/backup-sets/{source}/{set}/workflow
//	PATCH  /api/v1/backup-sets/{source}/{set}/workflow
//	GET    /api/v1/backup-sets/{source}/{set}/workflow/environment
//	PUT    /api/v1/backup-sets/{source}/{set}/workflow/environment/{name}
//	DELETE /api/v1/backup-sets/{source}/{set}/workflow/environment/{name}
//	GET    /api/v1/backup-sets/{source}/{set}/workflow/validation
//
// # The one rule that governs every shape in this file
//
// A secret is a LOCATION and never a value, in both directions. The env
// shapes carry a file path, a variable NAME or an argv, and there is no
// field anywhere in this file a resolved secret could be written into or
// read out of. That is the type rather than a convention: core/service
// resolves a reference at the moment a hook is about to be executed and
// nothing carries the result back, so no response this package can
// compose has one to leak. TestNoWorkflowResponseCarriesAResolvedSecret
// drives the env routes with a reference and asserts the answer carries
// only the location, which is the property rather than the absence of a
// bug today.
//
// The consequence is worth stating because it reads as a gap: no route
// here can show an operator the value of a secret variable, ever. It
// shows the name and where the value comes from. A surface that could
// print it would be a surface an attacker holding one session could read
// every credential in the deployment from.
//
// # Why the workflow configuration is a sub-resource rather than a block
// # on /settings
//
// Because the environment is a COLLECTION with its own identities, and a
// PATCH body cannot express "remove this one entry": an absent field
// already means "leave this alone" there, which is the same argument
// issue #333 made for putting one backup set's retention policy on its
// own path with three methods instead of onto the set's own shape. Once
// the environment is a sub-resource the rest of the block belongs beside
// it, so a client reads one path to find out how this deployment runs
// hooks rather than filtering a settings response.
//
// # Why the two scopes are two paths and one pair of shapes
//
// The deployment-wide layer and one backup set's are the same list at two
// levels, and core/service serves both through one method (an empty
// backup set id is the deployment scope). So the wire shapes are shared
// and the SCOPE is in the path. Two response types would be two places
// the secret-reference rule has to be kept, which is one more than there
// should be.

// maxWorkflowConfigBodyBytes bounds every write in this file.
//
// It is small on purpose. The largest thing any of these bodies can
// legitimately carry is a stage directory path or a secret command's
// argv, and a body bigger than this is not a workflow configuration. The
// backup-set routes' 1 MiB exists for an include list and key material,
// neither of which has a spelling here.
const maxWorkflowConfigBodyBytes = 1 << 13 // 8 KiB

// workflowSecretRefBody is where one environment value comes from. See
// this file's doc: a location, never a value, in both directions.
type workflowSecretRefBody struct {
	Command []string `json:"command"`
	Env     string   `json:"env"`
	File    string   `json:"file"`
}

// workflowEnvVarBody is one configured environment entry on the wire.
//
// Value and HasValue are two fields rather than one nullable string
// because the difference is real and a client renders it: `value: ""` is
// a deliberately empty variable, which operators write, and no literal at
// all is a variable whose value comes from a secret reference.
type workflowEnvVarBody struct {
	HasValue bool                  `json:"has_value"`
	Name     string                `json:"name"`
	Secret   workflowSecretRefBody `json:"secret"`
	Value    string                `json:"value"`
}

// listWorkflowEnvironmentResponse is one scope's entries, after a read or
// a write.
//
// The writes answer with the whole list rather than with the one entry
// they touched, because a set or an unset is only meaningful against what
// else is there: an operator clearing a credential needs to see what is
// left, and the entry they just wrote is in the answer either way.
type listWorkflowEnvironmentResponse struct {
	BackupSetID string               `json:"backup_set_id"`
	Variables   []workflowEnvVarBody `json:"variables"`
}

// workflowEnvironmentVariableRequest is the PUT body for one entry.
//
// Value is a pointer and Secret is not, and both of those are deliberate.
// A present-but-empty literal is a different request from no literal at
// all, so that field has to carry three states; a secret reference is
// absent when every one of its three fields is empty, so it needs only
// two and a pointer would add a nil case that means the same thing as the
// zero value.
//
// The variable's NAME is not here. It is the last path segment, because a
// body that could name a second variable would be a request whose path
// and body can disagree and this handler would then have to decide which
// one the operator meant.
type workflowEnvironmentVariableRequest struct {
	Secret workflowSecretRefBody `json:"secret"`
	Value  *string               `json:"value"`
}

// workflowRunnerSettingsBody is the host runner as this process sees it.
type workflowRunnerSettingsBody struct {
	Configured bool   `json:"configured"`
	Socket     string `json:"socket"`
	TokenFile  string `json:"token_file"`
}

// workflowSettingsResponse is the deployment-wide block, resolved.
type workflowSettingsResponse struct {
	AfterDir                string                     `json:"after_dir"`
	BeforeDir               string                     `json:"before_dir"`
	Configured              bool                       `json:"configured"`
	Environment             []workflowEnvVarBody       `json:"environment"`
	ExecConnections         []string                   `json:"exec_connections"`
	MaxScriptSizeBytes      int64                      `json:"max_script_size_bytes"`
	Root                    string                     `json:"root"`
	Runner                  workflowRunnerSettingsBody `json:"runner"`
	ScriptTimeoutConfigured bool                       `json:"script_timeout_configured"`
	ScriptTimeoutSeconds    int64                      `json:"script_timeout_seconds"`
}

// updateWorkflowSettingsRequest is PATCH /settings/workflow's body.
//
// Every field is a pointer for the reason settingsRequest's are: a PATCH
// means "change what I named", and a plain string cannot tell a key the
// caller omitted from one they want cleared. Clearing a stage directory
// DISABLES that stage, which is a real operation an operator performs, so
// it has to be expressible.
type updateWorkflowSettingsRequest struct {
	AfterDir             *string `json:"after_dir"`
	BeforeDir            *string `json:"before_dir"`
	MaxScriptSizeBytes   *int64  `json:"max_script_size_bytes"`
	Root                 *string `json:"root"`
	ScriptTimeoutSeconds *int64  `json:"script_timeout_seconds"`
}

func (r updateWorkflowSettingsRequest) namesNothing() bool {
	return r.AfterDir == nil && r.BeforeDir == nil && r.MaxScriptSizeBytes == nil &&
		r.Root == nil && r.ScriptTimeoutSeconds == nil
}

// workflowStageBody is one scope-and-phase pair that has a directory.
type workflowStageBody struct {
	Dir   string `json:"dir"`
	Phase string `json:"phase"`
	Scope string `json:"scope"`
}

// backupSetWorkflowResponse is one set's block, resolved against the
// deployment's.
type backupSetWorkflowResponse struct {
	AfterDir                      string               `json:"after_dir"`
	BackupSetID                   string               `json:"backup_set_id"`
	BeforeDir                     string               `json:"before_dir"`
	Configured                    bool                 `json:"configured"`
	EffectiveScriptTimeoutSeconds int64                `json:"effective_script_timeout_seconds"`
	Environment                   []workflowEnvVarBody `json:"environment"`
	RemoteExecConnectionRef       string               `json:"remote_exec_connection_ref"`
	ResolvedEnvironmentNames      []string             `json:"resolved_environment_names"`
	ScriptTimeoutSeconds          int64                `json:"script_timeout_seconds"`
	Stages                        []workflowStageBody  `json:"stages"`
}

// updateBackupSetWorkflowRequest is PATCH .../workflow's body, with the
// same pointer rule the deployment-wide patch above keeps.
type updateBackupSetWorkflowRequest struct {
	AfterDir                *string `json:"after_dir"`
	BeforeDir               *string `json:"before_dir"`
	RemoteExecConnectionRef *string `json:"remote_exec_connection_ref"`
	ScriptTimeoutSeconds    *int64  `json:"script_timeout_seconds"`
}

func (r updateBackupSetWorkflowRequest) namesNothing() bool {
	return r.AfterDir == nil && r.BeforeDir == nil &&
		r.RemoteExecConnectionRef == nil && r.ScriptTimeoutSeconds == nil
}

// workflowFindingBody is one validation check's answer.
type workflowFindingBody struct {
	Check    string `json:"check"`
	Detail   string `json:"detail"`
	Phase    string `json:"phase"`
	Scope    string `json:"scope"`
	Script   string `json:"script"`
	Severity string `json:"severity"`
	Target   string `json:"target"`
}

// workflowLintFindingBody is one thing backupd's own shell rules
// reported about one hook script.
//
// This product's own checks and its own BSH codes, and NOT ShellCheck:
// ShellCheck is GPL-3.0 and this product is Apache-2.0, so the analysis
// is implemented against a Go shell parser's syntax tree rather than
// shipped as somebody else's tool. Nothing here is a translation of a
// core/service value: the code and the severity travel as the rule wrote
// them, so an operator reading a code in a browser and grepping the same
// code in `validate workflow` meets one vocabulary.
type workflowLintFindingBody struct {
	Code     string                    `json:"code"`
	Col      int                       `json:"col"`
	Excerpt  workflowSourceExcerptBody `json:"excerpt"`
	Line     int                       `json:"line"`
	Message  string                    `json:"message"`
	Severity string                    `json:"severity"`
}

// workflowSourceExcerptBody is a few of a script's own lines, beside the
// position that names one of them.
//
// The lines arrive already inert -- control characters removed, length
// bounded -- from internal/workflowlint, over the bytes the validation
// hashed. Sanitising here instead would be a sanitiser every other
// surface that draws a script would need its own copy of, and a copy one
// of them would be missing.
type workflowSourceExcerptBody struct {
	Lines []workflowSourceLineBody `json:"lines"`
}

// workflowSourceLineBody is one line of a hook script, numbered as an
// editor numbers it.
type workflowSourceLineBody struct {
	Number    int    `json:"number"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

// workflowScriptLintBody is what the verification established about one
// script's bytes.
//
// The three states are kept apart on the wire for the reason
// core/service keeps them apart: examined and parsed, examined and
// refused, and NOT EXAMINED. A client that folded the third into either
// of the others would render a pass nobody proved or a fault nobody
// found.
type workflowScriptLintBody struct {
	Examined          bool                      `json:"examined"`
	Findings          []workflowLintFindingBody `json:"findings"`
	NotExaminedReason string                    `json:"not_examined_reason"`
	ParseError        string                    `json:"parse_error"`
	ParseErrorCol     int                       `json:"parse_error_col"`
	ParseErrorExcerpt workflowSourceExcerptBody `json:"parse_error_excerpt"`
	ParseErrorLine    int                       `json:"parse_error_line"`
	Parsed            bool                      `json:"parsed"`
}

// workflowValidatedScriptBody is one discovered hook.
type workflowValidatedScriptBody struct {
	ExecutionConnectionRef string                 `json:"execution_connection_ref"`
	Lint                   workflowScriptLintBody `json:"lint"`
	Order                  int                    `json:"order"`
	Phase                  string                 `json:"phase"`
	Scope                  string                 `json:"scope"`
	ScriptName             string                 `json:"script_name"`
	Sha256                 string                 `json:"sha256"`
	SizeBytes              int64                  `json:"size_bytes"`
	StepID                 string                 `json:"step_id"`
	Target                 string                 `json:"target"`
	TimeoutMs              int64                  `json:"timeout_ms"`
}

// workflowRefusedScriptBody is one script that refused a workflow
// configuration write.
//
// Only the BLOCKING half: a parse error, or the error-severity findings.
// A refusal that also listed the warnings would read as though they had
// refused it, and the whole point of the documented threshold is that
// they do not.
type workflowRefusedScriptBody struct {
	BackupSetID       string                    `json:"backup_set_id"`
	Dir               string                    `json:"dir"`
	Findings          []workflowLintFindingBody `json:"findings"`
	ParseError        string                    `json:"parse_error"`
	ParseErrorCol     int                       `json:"parse_error_col"`
	ParseErrorExcerpt workflowSourceExcerptBody `json:"parse_error_excerpt"`
	ParseErrorLine    int                       `json:"parse_error_line"`
	Phase             string                    `json:"phase"`
	Scope             string                    `json:"scope"`
	ScriptName        string                    `json:"script_name"`
}

// workflowScriptRejectedResponse is the WORKFLOW_SCRIPT_REJECTED 409
// body.
//
// It extends the error envelope with the blocking scripts as structured
// fields, and it is the second body in this package to do that
// (configRevisionStaleResponse is the first) on the same argument that
// one makes: the positions would otherwise exist only inside the
// human-readable message, which this package's own documentation
// describes as free to change without notice, so a client drawing "line
// 4, column 1" beside the save it refused would be parsing prose nobody
// promised to keep stable.
type workflowScriptRejectedResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	BlockingScripts []workflowRefusedScriptBody `json:"blocking_scripts"`
}

// workflowValidationResponse is one set's whole report.
//
// Two verdicts, and they are not folded into one. A backup set whose
// source connects and whose hook directory nobody has created yet is
// valid for backup and invalid for workflows, which is the ordinary state
// during setup; a single verdict would tell an operator their backups are
// failing when they are not.
type workflowValidationResponse struct {
	BackupSetID    string                        `json:"backup_set_id"`
	Configured     bool                          `json:"configured"`
	Findings       []workflowFindingBody         `json:"findings"`
	Root           string                        `json:"root"`
	Scripts        []workflowValidatedScriptBody `json:"scripts"`
	Stages         []workflowStageBody           `json:"stages"`
	ValidForBackup bool                          `json:"valid_for_backup"`
	WorkflowValid  bool                          `json:"workflow_valid"`
}

// getWorkflowSettings is GET /api/v1/settings/workflow. Read-only (§50).
func (h *handlers) getWorkflowSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := h.backend.WorkflowSettings(r.Context())
	if err != nil {
		h.writeWorkflowConfigError(w, r, err, "failed to read the workflow configuration")

		return
	}

	writeJSON(w, http.StatusOK, toWorkflowSettingsBody(settings))
}

// updateWorkflowSettings is PATCH /api/v1/settings/workflow.
//
// CSRF and no destructive gate, which is the tier PATCH /settings sits in
// (§50's "state-changing but non-destructive"): it rewrites one block of
// config.yaml and hot-reloads, opens no storage, moves no journal row and
// cannot reach a backup datum at all. The claim is recorded on the route
// (router.go) and on the list that pins it (destructiveGateExemptRoutes).
//
// There is one thing worth being explicit about, because it is the
// nearest this route comes to being dangerous: clearing a stage directory
// DISABLES that stage, so hooks an operator believes are running stop
// running. That is a configuration change with a visible answer -- the
// response is the resolved block, with the stage gone -- and it is the
// same class of change as disabling a backup set, which is also CSRF and
// not gated. What it cannot do is delete anything.
func (h *handlers) updateWorkflowSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkflowConfigBodyBytes)

	var body updateWorkflowSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxWorkflowConfigBodyBytes)

		return
	}
	if body.namesNothing() {
		// Refused rather than answered 200 for a write that changed
		// nothing, which is toUpdateSettingsRequest's rule: a settings
		// form reporting success for an edit that never happened is the
		// failure that rule exists for.
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			"a workflow settings write must name at least one setting to change")

		return
	}

	timeout, err := checkedSecondsPointerToDurationFromInt64(body.ScriptTimeoutSeconds, "script_timeout_seconds")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())

		return
	}

	settings, err := h.backend.UpdateWorkflowSettings(r.Context(), service.UpdateWorkflowSettingsRequest{
		Root:               body.Root,
		BeforeDir:          body.BeforeDir,
		AfterDir:           body.AfterDir,
		ScriptTimeout:      timeout,
		MaxScriptSizeBytes: body.MaxScriptSizeBytes,
	})
	if err != nil {
		h.writeWorkflowConfigError(w, r, err, "failed to write the workflow configuration")

		return
	}

	writeJSON(w, http.StatusOK, toWorkflowSettingsBody(settings))
}

// listWorkflowEnvironment is GET /api/v1/settings/workflow/environment.
// Read-only (§50).
func (h *handlers) listWorkflowEnvironment(w http.ResponseWriter, r *http.Request) {
	h.serveWorkflowEnvList(w, r, "")
}

// setWorkflowEnvironment is PUT
// /api/v1/settings/workflow/environment/{name}.
func (h *handlers) setWorkflowEnvironment(w http.ResponseWriter, r *http.Request) {
	h.serveWorkflowEnvSet(w, r, "")
}

// unsetWorkflowEnvironment is DELETE
// /api/v1/settings/workflow/environment/{name}.
func (h *handlers) unsetWorkflowEnvironment(w http.ResponseWriter, r *http.Request) {
	h.serveWorkflowEnvUnset(w, r, "")
}

// getBackupSetWorkflow is GET
// /api/v1/backup-sets/{source}/{set}/workflow. Read-only (§50).
func (h *handlers) getBackupSetWorkflow(w http.ResponseWriter, r *http.Request) {
	set, err := h.backend.BackupSetWorkflow(r.Context(), backupSetIDFromRoute(r))
	if err != nil {
		h.writeWorkflowConfigError(w, r, err, "failed to read the backup set's workflow configuration")

		return
	}

	writeJSON(w, http.StatusOK, toBackupSetWorkflowBody(set))
}

// updateBackupSetWorkflow is PATCH
// /api/v1/backup-sets/{source}/{set}/workflow, in the same tier as the
// deployment-wide patch above and as PATCH /backup-sets/{source}/{set}.
func (h *handlers) updateBackupSetWorkflow(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkflowConfigBodyBytes)

	var body updateBackupSetWorkflowRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxWorkflowConfigBodyBytes)

		return
	}
	if body.namesNothing() {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			"a backup set workflow write must name at least one setting to change")

		return
	}

	timeout, err := checkedSecondsPointerToDurationFromInt64(body.ScriptTimeoutSeconds, "script_timeout_seconds")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())

		return
	}

	set, err := h.backend.UpdateBackupSetWorkflow(r.Context(), backupSetIDFromRoute(r), service.UpdateBackupSetWorkflowRequest{
		BeforeDir:               body.BeforeDir,
		AfterDir:                body.AfterDir,
		ScriptTimeout:           timeout,
		RemoteExecConnectionRef: body.RemoteExecConnectionRef,
	})
	if err != nil {
		h.writeWorkflowConfigError(w, r, err, "failed to write the backup set's workflow configuration")

		return
	}

	writeJSON(w, http.StatusOK, toBackupSetWorkflowBody(set))
}

// listBackupSetWorkflowEnvironment is GET
// /api/v1/backup-sets/{source}/{set}/workflow/environment. Read-only
// (§50).
func (h *handlers) listBackupSetWorkflowEnvironment(w http.ResponseWriter, r *http.Request) {
	h.serveWorkflowEnvList(w, r, backupSetIDFromRoute(r))
}

// setBackupSetWorkflowEnvironment is PUT
// /api/v1/backup-sets/{source}/{set}/workflow/environment/{name}.
func (h *handlers) setBackupSetWorkflowEnvironment(w http.ResponseWriter, r *http.Request) {
	h.serveWorkflowEnvSet(w, r, backupSetIDFromRoute(r))
}

// unsetBackupSetWorkflowEnvironment is DELETE
// /api/v1/backup-sets/{source}/{set}/workflow/environment/{name}.
func (h *handlers) unsetBackupSetWorkflowEnvironment(w http.ResponseWriter, r *http.Request) {
	h.serveWorkflowEnvUnset(w, r, backupSetIDFromRoute(r))
}

// The three env operations, once each, with the scope handed in.
//
// One implementation for both scopes rather than two, for the reason
// core/service has one method for both: they are the same operation on
// two lists, and a second copy is a second place this file's
// secret-reference rule has to be kept.

func (h *handlers) serveWorkflowEnvList(w http.ResponseWriter, r *http.Request, backupSetID string) {
	vars, err := h.backend.ListWorkflowEnv(r.Context(), backupSetID)
	if err != nil {
		h.writeWorkflowConfigError(w, r, err, "failed to read the workflow environment")

		return
	}

	writeJSON(w, http.StatusOK, toWorkflowEnvListBody(backupSetID, vars))
}

func (h *handlers) serveWorkflowEnvSet(w http.ResponseWriter, r *http.Request, backupSetID string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkflowConfigBodyBytes)

	var body workflowEnvironmentVariableRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxWorkflowConfigBodyBytes)

		return
	}

	v := service.WorkflowEnvVar{
		Name: chi.URLParam(r, "name"),
		Secret: service.WorkflowSecretRef{
			File:    body.Secret.File,
			Env:     body.Secret.Env,
			Command: body.Secret.Command,
		},
	}
	if body.Value != nil {
		v.Value, v.HasValue = *body.Value, true
	}

	// Neither the name rule nor the exactly-one-source rule is checked
	// here. core/service holds both -- the name rule by calling
	// internal/workflow's own validator rather than copying it, the
	// source rule because config.Validate cannot report it well once a
	// contradictory request has already become two written keys -- and a
	// copy on this boundary would be a second answer that can pass while
	// the real one refuses.
	vars, err := h.backend.SetWorkflowEnv(r.Context(), backupSetID, v)
	if err != nil {
		h.writeWorkflowConfigError(w, r, err, "failed to write the workflow environment variable")

		return
	}

	writeJSON(w, http.StatusOK, toWorkflowEnvListBody(backupSetID, vars))
}

func (h *handlers) serveWorkflowEnvUnset(w http.ResponseWriter, r *http.Request, backupSetID string) {
	vars, err := h.backend.UnsetWorkflowEnv(r.Context(), backupSetID, chi.URLParam(r, "name"))
	if err != nil {
		h.writeWorkflowConfigError(w, r, err, "failed to remove the workflow environment variable")

		return
	}

	writeJSON(w, http.StatusOK, toWorkflowEnvListBody(backupSetID, vars))
}

// getBackupSetWorkflowValidation is GET
// /api/v1/backup-sets/{source}/{set}/workflow/validation. Read-only
// (§50), and read-only in the strongest sense this product has: the
// validation it serves never executes a hook body, so the only things
// this route can cause to run are `bash -n`, which parses, and
// core/service's own fixed remote capability probe.
//
// It is still a real cost -- it captures and hashes every script, and it
// opens a socket to the host runner and an SSH connection to the source
// -- which is why it is its own route rather than a block on the
// configuration read beside it. A dashboard that polled the workflow
// configuration would otherwise be probing an operator's source host on
// a timer.
func (h *handlers) getBackupSetWorkflowValidation(w http.ResponseWriter, r *http.Request) {
	report, err := h.backend.ValidateWorkflow(r.Context(), backupSetIDFromRoute(r))
	if err != nil {
		h.writeWorkflowConfigError(w, r, err, "failed to validate the backup set's workflow")

		return
	}

	writeJSON(w, http.StatusOK, toWorkflowValidationBody(report))
}

// writeWorkflowConfigError maps this file's refusals onto their declared
// statuses, and keeps them apart.
//
// Every message echoed here is core/service's own prose, which is
// service.ErrInvalidRequest's standing rule: an unclassified error can
// carry a filesystem path or a state-layer sentence, so the default arm
// says nothing about err and records it under the response's correlation
// id instead (refusal.go).
func (h *handlers) writeWorkflowConfigError(w http.ResponseWriter, r *http.Request, err error, internal string) {
	// #906's save gate, first, because it is the one refusal here that
	// carries STRUCTURE. It is matched by TYPE rather than by sentinel --
	// errors.As, not errors.Is -- since the fields are the answer: a
	// client drawing which script and which line refused the save cannot
	// get those out of a sentinel.
	var rejected *service.WorkflowScriptRefusal
	if errors.As(err, &rejected) {
		h.writeWorkflowScriptRejected(w, r, rejected)

		return
	}

	switch {
	case errors.Is(err, service.ErrBackupSetNotFound):
		h.logRefusal(r, http.StatusNotFound, "BACKUP_SET_NOT_FOUND",
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set"), err)
	case errors.Is(err, service.ErrWorkflowEnvNotFound):
		// A refusal rather than a silent 200, which is the sentinel's own
		// argument: an unset that quietly did nothing is
		// indistinguishable from one that worked, and the case it hides
		// is an operator clearing a credential from the wrong scope and
		// believing they have cleared it.
		h.logRefusal(r, http.StatusNotFound, "WORKFLOW_ENV_NOT_FOUND",
			writeError(w, http.StatusNotFound, "WORKFLOW_ENV_NOT_FOUND", err.Error()), err)
	case errors.Is(err, service.ErrWorkflowsNotConfigured):
		// 409 and its own code rather than 400 INVALID_REQUEST: the
		// request is fine and the deployment is not ready. The remedy is
		// a deployment-wide patch naming a workflow root, which is a
		// different action from correcting a field, and a client told
		// INVALID_REQUEST would send an operator back to the form they
		// filled in correctly.
		h.logRefusal(r, http.StatusConflict, "WORKFLOWS_NOT_CONFIGURED",
			writeError(w, http.StatusConflict, "WORKFLOWS_NOT_CONFIGURED", err.Error()), err)
	case errors.Is(err, service.ErrWorkflowValidationUnavailable), errors.Is(err, service.ErrWorkflowsNotWired):
		// One code for two sentinels, and the two sentences are kept.
		// They are different facts -- no state directory to spool into,
		// and no workflow engine in this process -- and they are the same
		// ANSWER to a client: this process cannot tell you, and no change
		// to the request will make it. Both are fixed by how the
		// deployment is installed rather than by anything a caller sends,
		// which is why they share a code and why err.Error() is echoed:
		// the sentence is what says which of the two it is.
		h.logRefusal(r, http.StatusServiceUnavailable, "WORKFLOW_ENGINE_UNAVAILABLE",
			writeError(w, http.StatusServiceUnavailable, "WORKFLOW_ENGINE_UNAVAILABLE", err.Error()), err)
	case errors.Is(err, service.ErrInvalidRequest):
		// Safe to echo on core/service's own guarantee: this sentinel's
		// text is built from that package's field descriptions and the
		// caller's own values, and the only secret-shaped thing that
		// crosses this boundary is a REFERENCE.
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	case errors.Is(err, service.ErrConfigNotFileBacked):
		// The sentence every other configuration write answers with. A
		// 500 because nothing about the request is wrong, and it names
		// the deployment-shaped thing that is missing rather than leaving
		// an operator to read "failed to write" and go looking at their
		// hooks.
		h.internalError(w, r, "INTERNAL", "this deployment has no configuration file to persist to", err)
	default:
		h.internalError(w, r, "INTERNAL", internal, err)
	}
}

// backupSetIDFromRoute rebuilds the "source/backup-set" id from the two
// path parameters every per-set route in this file declares.
//
// Two segments rather than one catch-all, for the reason the snapshot and
// retention routes take two: a backup set id is always exactly
// source/name, so a route that says so lets chi answer a malformed one
// with a 404 instead of a handler having to interpret one.
func backupSetIDFromRoute(r *http.Request) string {
	return chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
}

// checkedSecondsPointerToDurationFromInt64 is
// checkedSecondsPointerToDuration for a field spelled int64 on the wire,
// and it is checked for that function's exact reason: seconds × 1e9 wraps
// silently, and on these fields zero is not a rejected value -- it is the
// spelling of "clear this bound and inherit again". So the largest number
// a client can send would quietly CLEAR an operator's pinned timeout and
// be answered 200, and the values just below it wrap to short, entirely
// plausible bounds, which is a hook killed at a cadence nobody chose.
func checkedSecondsPointerToDurationFromInt64(v *int64, field string) (*time.Duration, error) {
	if v == nil {
		return nil, nil
	}
	if *v > maxDurationSeconds || *v < -maxDurationSeconds {
		return nil, fmt.Errorf("%s is %d seconds, which is longer than this engine can express as an interval (at most %d seconds)", field, *v, maxDurationSeconds)
	}

	d := time.Duration(*v) * time.Second

	return &d, nil
}

// durationSeconds renders a configured bound in the unit an operator sets
// it in.
//
// Whole seconds, truncated, because every duration an operator configures
// on this API is spelled in seconds (stale_after_seconds,
// stable_for_seconds, poll_interval_seconds) and a second spelling would
// be a second thing for a client to get wrong.
//
// RUN-time facts are milliseconds instead, and there is no helper for
// those because core/service already reports them as milliseconds: a
// bound of 300ms rendered as "0 seconds" would be a lie, and a configured
// policy of five minutes rendered as 300000 is noise, so the two units
// are kept apart at the boundary where each is the honest one.
func durationSeconds(d time.Duration) int64 {
	return int64(d / time.Second)
}

// toWorkflowEnvVarBody is one entry on the wire.
//
// The secret is copied field by field rather than by embedding
// core/service's type, which is what makes it structurally impossible for
// a field added there to appear here unreviewed: a resolved value would
// have to be given a json tag in this file by somebody, rather than
// arriving because a struct grew.
func toWorkflowEnvVarBody(v service.WorkflowEnvVar) workflowEnvVarBody {
	return workflowEnvVarBody{
		Name:     v.Name,
		Value:    v.Value,
		HasValue: v.HasValue,
		Secret: workflowSecretRefBody{
			File:    v.Secret.File,
			Env:     v.Secret.Env,
			Command: v.Secret.Command,
		},
	}
}

// toWorkflowEnvListBody renders one scope's entries.
//
// The slice is allocated at length zero rather than left nil, so a scope
// with nothing configured serialises "variables": [] instead of null: one
// shape for a client to render rather than two, which is
// writeArtifactsResponse's rule and is kept by every list in this
// package.
func toWorkflowEnvListBody(backupSetID string, vars []service.WorkflowEnvVar) listWorkflowEnvironmentResponse {
	out := listWorkflowEnvironmentResponse{
		BackupSetID: backupSetID,
		Variables:   make([]workflowEnvVarBody, 0, len(vars)),
	}
	for _, v := range vars {
		out.Variables = append(out.Variables, toWorkflowEnvVarBody(v))
	}

	return out
}

func toWorkflowSettingsBody(s service.WorkflowSettings) workflowSettingsResponse {
	out := workflowSettingsResponse{
		Configured:              s.Configured,
		Root:                    s.Root,
		BeforeDir:               s.BeforeDir,
		AfterDir:                s.AfterDir,
		ScriptTimeoutSeconds:    durationSeconds(s.ScriptTimeout),
		ScriptTimeoutConfigured: s.ScriptTimeoutConfigured,
		MaxScriptSizeBytes:      s.MaxScriptSizeBytes,
		Environment:             make([]workflowEnvVarBody, 0, len(s.Environment)),
		ExecConnections:         make([]string, 0, len(s.ExecConnections)),
		Runner: workflowRunnerSettingsBody{
			Configured: s.Runner.Configured,
			Socket:     s.Runner.Socket,
			TokenFile:  s.Runner.TokenFile,
		},
	}
	for _, v := range s.Environment {
		out.Environment = append(out.Environment, toWorkflowEnvVarBody(v))
	}
	out.ExecConnections = append(out.ExecConnections, s.ExecConnections...)

	return out
}

func toBackupSetWorkflowBody(s service.BackupSetWorkflow) backupSetWorkflowResponse {
	out := backupSetWorkflowResponse{
		BackupSetID:                   s.BackupSetID,
		Configured:                    s.Configured,
		BeforeDir:                     s.BeforeDir,
		AfterDir:                      s.AfterDir,
		ScriptTimeoutSeconds:          durationSeconds(s.ScriptTimeout),
		EffectiveScriptTimeoutSeconds: durationSeconds(s.EffectiveScriptTimeout),
		RemoteExecConnectionRef:       s.RemoteExecConnectionRef,
		Environment:                   make([]workflowEnvVarBody, 0, len(s.Environment)),
		ResolvedEnvironmentNames:      make([]string, 0, len(s.ResolvedEnvironmentNames)),
		Stages:                        toWorkflowStageBodies(s.Stages),
	}
	for _, v := range s.Environment {
		out.Environment = append(out.Environment, toWorkflowEnvVarBody(v))
	}
	out.ResolvedEnvironmentNames = append(out.ResolvedEnvironmentNames, s.ResolvedEnvironmentNames...)

	return out
}

func toWorkflowStageBodies(stages []service.WorkflowStage) []workflowStageBody {
	out := make([]workflowStageBody, 0, len(stages))
	for _, s := range stages {
		out = append(out, workflowStageBody{Scope: s.Scope, Phase: s.Phase, Dir: s.Dir})
	}

	return out
}

func toWorkflowValidationBody(v service.WorkflowValidation) workflowValidationResponse {
	out := workflowValidationResponse{
		BackupSetID:    v.BackupSetID,
		ValidForBackup: v.ValidForBackup,
		WorkflowValid:  v.WorkflowValid,
		Configured:     v.Configured,
		Root:           v.Root,
		Stages:         toWorkflowStageBodies(v.Stages),
		Scripts:        make([]workflowValidatedScriptBody, 0, len(v.Scripts)),
		Findings:       make([]workflowFindingBody, 0, len(v.Findings)),
	}
	for _, s := range v.Scripts {
		out.Scripts = append(out.Scripts, workflowValidatedScriptBody{
			StepID:                 s.StepID,
			Order:                  s.Order,
			ScriptName:             s.ScriptName,
			Scope:                  s.Scope,
			Phase:                  s.Phase,
			Target:                 s.Target,
			Sha256:                 s.SHA256,
			SizeBytes:              s.Size,
			TimeoutMs:              s.TimeoutMillis,
			ExecutionConnectionRef: s.ExecutionConnectionRef,
			Lint:                   toWorkflowScriptLintBody(s.Lint),
		})
	}
	for _, f := range v.Findings {
		out.Findings = append(out.Findings, workflowFindingBody{
			Check:    f.Check,
			Severity: f.Severity,
			Detail:   f.Detail,
			Scope:    f.Scope,
			Phase:    f.Phase,
			Target:   f.Target,
			Script:   f.Script,
		})
	}

	return out
}

// toWorkflowScriptLintBody renders one script's verification onto the
// wire.
//
// The slice is allocated at length zero rather than left nil, so a clean
// script serialises "findings": [] instead of null: one shape for a
// client to render rather than two, which is this package's rule for
// every list it returns.
func toWorkflowScriptLintBody(l service.WorkflowScriptLint) workflowScriptLintBody {
	out := workflowScriptLintBody{
		Examined:          l.Examined,
		NotExaminedReason: l.NotExaminedReason,
		Parsed:            l.Parsed,
		ParseError:        l.ParseError,
		ParseErrorLine:    l.ParseErrorLine,
		ParseErrorCol:     l.ParseErrorCol,
		Findings:          toWorkflowLintFindingBodies(l.Findings),
		ParseErrorExcerpt: toWorkflowSourceExcerptBody(l.ParseErrorExcerpt),
	}

	return out
}

// toWorkflowSourceExcerptBody renders one excerpt.
//
// The slice is allocated at length zero rather than left nil, so a
// finding with no excerpt serialises "lines": [] instead of null: one
// shape for a client to render rather than two.
func toWorkflowSourceExcerptBody(e service.WorkflowSourceExcerpt) workflowSourceExcerptBody {
	out := workflowSourceExcerptBody{Lines: make([]workflowSourceLineBody, 0, len(e.Lines))}
	for _, l := range e.Lines {
		out.Lines = append(out.Lines, workflowSourceLineBody{
			Number:    l.Number,
			Text:      l.Text,
			Truncated: l.Truncated,
		})
	}

	return out
}

func toWorkflowLintFindingBodies(findings []service.WorkflowLintFinding) []workflowLintFindingBody {
	out := make([]workflowLintFindingBody, 0, len(findings))
	for _, f := range findings {
		out = append(out, workflowLintFindingBody{
			Code:     f.Code,
			Severity: f.Severity,
			Line:     f.Line,
			Col:      f.Col,
			Message:  f.Message,
			Excerpt:  toWorkflowSourceExcerptBody(f.Excerpt),
		})
	}

	return out
}

// writeWorkflowScriptRejected answers a refused workflow write with the
// blocking scripts as fields.
//
// The MESSAGE is core/service's own multi-line sentence, echoed on that
// package's guarantee about what it may contain: script basenames, stage
// directories as configured, positions, and a rule's own prose. Nothing
// on the path that produces it resolves a secret, and the structured
// fields carry the same strings, so a client has no reason to parse the
// message and a terminal has no reason to parse the fields.
func (h *handlers) writeWorkflowScriptRejected(w http.ResponseWriter, r *http.Request, refusal *service.WorkflowScriptRefusal) {
	body := workflowScriptRejectedResponse{
		BlockingScripts: make([]workflowRefusedScriptBody, 0, len(refusal.Scripts)),
	}
	// The literal, and not apicontract's constant: the registry test
	// that proves every code a handler emits is declared by the contract
	// scans this package's source for exactly this spelling, and a code
	// reached through an identifier would be a code that check cannot
	// see (contract_test.go's assignedCode).
	body.Error.Code = "WORKFLOW_SCRIPT_REJECTED"
	body.Error.Message = refusal.Error()

	for _, s := range refusal.Scripts {
		body.BlockingScripts = append(body.BlockingScripts, workflowRefusedScriptBody{
			ScriptName:        s.ScriptName,
			Dir:               s.Dir,
			Scope:             s.Scope,
			Phase:             s.Phase,
			BackupSetID:       s.BackupSetID,
			ParseError:        s.ParseError,
			ParseErrorLine:    s.ParseErrorLine,
			ParseErrorCol:     s.ParseErrorCol,
			ParseErrorExcerpt: toWorkflowSourceExcerptBody(s.ParseErrorExcerpt),
			Findings:          toWorkflowLintFindingBodies(s.Findings),
		})
	}

	// Logged as a refusal like every other 409 this file answers, so the
	// correlation id an operator quotes names the same event in the
	// process's own log.
	h.logRefusal(r, http.StatusConflict, body.Error.Code, responseCorrelationID(w), refusal)

	writeJSON(w, http.StatusConflict, body)
}
