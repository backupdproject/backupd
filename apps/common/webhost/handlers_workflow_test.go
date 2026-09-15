package webhost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/retnd/retnd/apps/common/platform/capabilities"
	"github.com/retnd/retnd/core/service"
)

// EPIC L's API surface (#813), driven through the real router.
//
// Three of the tests here are the ones the issue asks for by name and the
// rest are the ordinary status-and-shape checks every route in this
// package carries:
//
//   - TestWorkflowStepLogs_AuthorizationIsRecheckedOnEveryCursorResume,
//     which is the whole argument for a cursor poll rather than a stream;
//   - TestNoWorkflowResponseCarriesAResolvedSecret, which asserts a
//     property of the SHAPE rather than the absence of a bug today;
//   - TestEveryWorkflowWriteIsReachableWithTheDestructiveGateClosed,
//     which turns the eight entries on destructiveGateExemptRoutes into
//     something that fails if somebody gates one.

// workflowRouter is a router over a sync fake, which is the double the
// workflow surface's own fixture hangs off (fixtures_workflow_test.go).
func workflowRouter(t *testing.T) (http.Handler, *syncFakeBackend) {
	t.Helper()
	backend := newSyncFakeBackend()
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       backend,
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})

	return router, backend
}

// workflowRequest drives one request, attaching the double-submit pair
// for anything that is not a GET so a mutating route's real answer is
// reached rather than requireCSRF's 403.
func workflowRequest(t *testing.T, router http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet {
		attachValidCSRF(req)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decoding the response body %q: %v", rec.Body.String(), err)
	}
}

// ------------------------------------------------- configuration reads ---

func TestGetWorkflowSettings_ReportsTheResolvedConfiguration(t *testing.T) {
	router, backend := workflowRouter(t)
	fx := workflowOf(backend)
	fx.settings = service.WorkflowSettings{
		Configured:              true,
		Root:                    "/workflows",
		BeforeDir:               "before.d",
		AfterDir:                "after.d",
		ScriptTimeout:           5 * time.Minute,
		ScriptTimeoutConfigured: true,
		MaxScriptSizeBytes:      65536,
		ExecConnections:         []string{"pg-primary"},
		Runner:                  service.WorkflowRunnerSettings{Configured: true, Socket: "/run/backupd/runner.sock", TokenFile: "/run/backupd/runner.token"},
	}
	fx.env[""] = []service.WorkflowEnvVar{{Name: "PGHOST", Value: "10.0.0.14", HasValue: true}}

	rec := workflowRequest(t, router, http.MethodGet, "/api/v1/settings/workflow", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var body workflowSettingsResponse
	decodeBody(t, rec, &body)

	// The timeout in SECONDS is the assertion that earns this test: the
	// service reports a time.Duration, and a handler that marshalled it
	// straight through would put 300000000000 on the wire and every
	// client would render five thousand minutes.
	if body.ScriptTimeoutSeconds != 300 {
		t.Errorf("script_timeout_seconds = %d, want 300", body.ScriptTimeoutSeconds)
	}
	if !body.ScriptTimeoutConfigured || !body.Configured {
		t.Errorf("configured = %v and script_timeout_configured = %v, want both true", body.Configured, body.ScriptTimeoutConfigured)
	}
	if body.Root != "/workflows" || body.BeforeDir != "before.d" || body.AfterDir != "after.d" {
		t.Errorf("the resolved directories came back as %q/%q/%q", body.Root, body.BeforeDir, body.AfterDir)
	}
	if len(body.ExecConnections) != 1 || body.ExecConnections[0] != "pg-primary" {
		t.Errorf("exec_connections = %v, want the one declared connection by NAME", body.ExecConnections)
	}
	if body.Runner.Socket != "/run/backupd/runner.sock" || body.Runner.TokenFile != "/run/backupd/runner.token" {
		t.Errorf("the runner came back as %+v", body.Runner)
	}
	if len(body.Environment) != 1 || body.Environment[0].Name != "PGHOST" {
		t.Errorf("environment = %+v, want the deployment-wide layer", body.Environment)
	}
}

func TestGetBackupSetWorkflow_ReportsBothTheSetsOwnBoundAndTheEffectiveOne(t *testing.T) {
	router, backend := workflowRouter(t)
	fx := workflowOf(backend)
	fx.set = service.BackupSetWorkflow{
		Configured:               true,
		BeforeDir:                "pg.before.d",
		ScriptTimeout:            30 * time.Second,
		EffectiveScriptTimeout:   30 * time.Second,
		RemoteExecConnectionRef:  "pg-primary",
		ResolvedEnvironmentNames: []string{"PGHOST", "PGPASSWORD"},
		Stages: []service.WorkflowStage{
			{Scope: "global", Phase: "before", Dir: "before.d"},
			{Scope: "set", Phase: "before", Dir: "pg.before.d"},
		},
	}

	rec := workflowRequest(t, router, http.MethodGet, "/api/v1/backup-sets/api-server/var-backups/workflow", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var body backupSetWorkflowResponse
	decodeBody(t, rec, &body)

	// The id is rebuilt from the two path segments, which is the one
	// thing a handler taking source/set separately can get wrong.
	if body.BackupSetID != "api-server/var-backups" {
		t.Errorf("backup_set_id = %q, want the two path segments joined", body.BackupSetID)
	}
	if body.ScriptTimeoutSeconds != 30 || body.EffectiveScriptTimeoutSeconds != 30 {
		t.Errorf("the two timeouts came back as %d and %d seconds, want 30 and 30",
			body.ScriptTimeoutSeconds, body.EffectiveScriptTimeoutSeconds)
	}
	if len(body.Stages) != 2 || body.Stages[0].Scope != "global" || body.Stages[1].Dir != "pg.before.d" {
		t.Errorf("stages = %+v, want the two resolved stages in execution order", body.Stages)
	}
	if len(body.ResolvedEnvironmentNames) != 2 {
		t.Errorf("resolved_environment_names = %v, want the merged NAMES", body.ResolvedEnvironmentNames)
	}
}

func TestGetBackupSetWorkflowValidation_CarriesTwoVerdictsAndEveryFinding(t *testing.T) {
	router, backend := workflowRouter(t)
	workflowOf(backend).validation = service.WorkflowValidation{
		// The case the two verdicts exist for: the backup set is fine
		// and its hooks are not.
		ValidForBackup: true,
		WorkflowValid:  false,
		Configured:     true,
		Root:           "/workflows",
		Scripts: []service.WorkflowValidatedScript{{
			StepID: "s1", Order: 1, ScriptName: "10-quiesce.remote.sh",
			Scope: "set", Phase: "before", Target: "remote",
			SHA256: "abc123", Size: 412, TimeoutMillis: 30000,
			ExecutionConnectionRef: "pg-primary",
		}},
		Findings: []service.WorkflowFinding{
			{Check: service.WorkflowCheckDirectories, Severity: service.WorkflowSeverityOK, Detail: "every stage directory resolves inside the root"},
			{Check: service.WorkflowCheckExecCapability, Severity: service.WorkflowSeverityError, Detail: "the account cannot run a command", Script: "10-quiesce.remote.sh", Target: "remote"},
		},
	}

	rec := workflowRequest(t, router, http.MethodGet, "/api/v1/backup-sets/api-server/var-backups/workflow/validation", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var body workflowValidationResponse
	decodeBody(t, rec, &body)

	// A 200 with workflow_valid false is the contract: the validation
	// SUCCEEDED and the configuration is broken, and a route that
	// answered 4xx for the second would make a broken hook tree
	// indistinguishable from a broken request.
	if !body.ValidForBackup || body.WorkflowValid {
		t.Errorf("valid_for_backup = %v and workflow_valid = %v, want true and false",
			body.ValidForBackup, body.WorkflowValid)
	}
	if len(body.Findings) != 2 || body.Findings[1].Severity != service.WorkflowSeverityError {
		t.Fatalf("findings = %+v, want both, with the second an error", body.Findings)
	}
	if len(body.Scripts) != 1 || body.Scripts[0].Sha256 != "abc123" || body.Scripts[0].TimeoutMs != 30000 {
		t.Errorf("scripts = %+v, want the discovered hook with its hash and bound", body.Scripts)
	}
}

// ------------------------------------------------ configuration writes ---

func TestUpdateWorkflowSettings_PassesOnlyTheFieldsTheRequestNamed(t *testing.T) {
	router, backend := workflowRouter(t)

	rec := workflowRequest(t, router, http.MethodPatch, "/api/v1/settings/workflow",
		`{"root":"/workflows","script_timeout_seconds":120}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	got := workflowOf(backend).lastSettingsUpdate
	if got.Root == nil || *got.Root != "/workflows" {
		t.Errorf("root reached the backend as %v, want a pointer to /workflows", got.Root)
	}
	if got.ScriptTimeout == nil || *got.ScriptTimeout != 2*time.Minute {
		t.Errorf("script_timeout reached the backend as %v, want 2m", got.ScriptTimeout)
	}
	// The fields the body did not mention have to arrive nil. A PATCH
	// that filled them in would clear an operator's stage directories on
	// every unrelated edit, which is the failure the pointers exist for.
	if got.BeforeDir != nil || got.AfterDir != nil || got.MaxScriptSizeBytes != nil {
		t.Errorf("a field nobody named reached the backend non-nil: %+v", got)
	}
}

func TestUpdateWorkflowSettings_AnEmptyDirectoryIsAClearAndNotAnOmission(t *testing.T) {
	router, backend := workflowRouter(t)

	rec := workflowRequest(t, router, http.MethodPatch, "/api/v1/settings/workflow", `{"before_dir":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	got := workflowOf(backend).lastSettingsUpdate
	if got.BeforeDir == nil || *got.BeforeDir != "" {
		t.Fatalf("before_dir reached the backend as %v, want a pointer to the empty string: clearing a stage directory disables that stage, and it is an operation an operator performs deliberately", got.BeforeDir)
	}
}

func TestUpdateWorkflowSettings_RefusesABodyThatNamesNothing(t *testing.T) {
	router, backend := workflowRouter(t)

	rec := workflowRequest(t, router, http.MethodPatch, "/api/v1/settings/workflow", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "INVALID_REQUEST" {
		t.Errorf("code = %q, want INVALID_REQUEST", code)
	}
	// And nothing reached the backend: a write that changed nothing must
	// not move the configuration revision.
	if got := workflowOf(backend).lastSettingsUpdate; got.Root != nil || got.BeforeDir != nil {
		t.Errorf("the refused request still reached the backend as %+v", got)
	}
}

func TestUpdateWorkflowSettings_RefusesATimeoutTooLargeToBeADuration(t *testing.T) {
	router, _ := workflowRouter(t)

	// Seconds x 1e9 overflows an int64 nanosecond duration, and the
	// values just below the wrap land on short, plausible bounds. Refused
	// with the number named, rather than accepted and silently turned
	// into a cadence nobody chose.
	rec := workflowRequest(t, router, http.MethodPatch, "/api/v1/settings/workflow",
		`{"script_timeout_seconds":9223372036854775}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "script_timeout_seconds") {
		t.Errorf("the refusal does not name the field: %s", rec.Body.String())
	}
}

func TestUpdateBackupSetWorkflow_RefusesADeploymentWithNoWorkflowRoot(t *testing.T) {
	router, backend := workflowRouter(t)
	workflowOf(backend).errOnWrite = service.ErrWorkflowsNotConfigured

	rec := workflowRequest(t, router, http.MethodPatch,
		"/api/v1/backup-sets/api-server/var-backups/workflow", `{"before_dir":"pg.before.d"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body: %s", rec.Code, rec.Body.String())
	}
	// Its own code rather than INVALID_REQUEST: the request was fine and
	// the deployment is not ready, and the two remedies are different.
	if code := errorCodeOf(t, rec); code != "WORKFLOWS_NOT_CONFIGURED" {
		t.Errorf("code = %q, want WORKFLOWS_NOT_CONFIGURED", code)
	}
}

func TestUpdateBackupSetWorkflow_ReportsASetThisDeploymentDoesNotConfigure(t *testing.T) {
	router, backend := workflowRouter(t)
	workflowOf(backend).errOnWrite = fmt.Errorf("%w: api-server/gone", service.ErrBackupSetNotFound)

	rec := workflowRequest(t, router, http.MethodPatch,
		"/api/v1/backup-sets/api-server/gone/workflow", `{"before_dir":"pg.before.d"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "BACKUP_SET_NOT_FOUND" {
		t.Errorf("code = %q, want BACKUP_SET_NOT_FOUND", code)
	}
}

func TestUpdateBackupSetWorkflow_PassesTheExecConnectionAndTheSetsOwnBound(t *testing.T) {
	router, backend := workflowRouter(t)

	rec := workflowRequest(t, router, http.MethodPatch,
		"/api/v1/backup-sets/api-server/var-backups/workflow",
		`{"script_timeout_seconds":90,"remote_exec_connection_ref":"pg-primary"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	fx := workflowOf(backend)
	if fx.lastSetUpdateID != "api-server/var-backups" {
		t.Errorf("the write reached the backend for %q, want the two path segments joined", fx.lastSetUpdateID)
	}
	if fx.lastSetUpdate.ScriptTimeout == nil || *fx.lastSetUpdate.ScriptTimeout != 90*time.Second {
		t.Errorf("script_timeout reached the backend as %v, want 90s", fx.lastSetUpdate.ScriptTimeout)
	}
	if fx.lastSetUpdate.RemoteExecConnectionRef == nil || *fx.lastSetUpdate.RemoteExecConnectionRef != "pg-primary" {
		t.Errorf("the connection reference reached the backend as %v", fx.lastSetUpdate.RemoteExecConnectionRef)
	}
}

// ------------------------------------------------------- environment ---

// The two scopes are one implementation with the scope handed in, so the
// case that matters is that a write lands on the scope the PATH named and
// not on the other one.
func TestWorkflowEnvironment_AWriteLandsOnTheScopeThePathNamed(t *testing.T) {
	router, backend := workflowRouter(t)
	fx := workflowOf(backend)

	if rec := workflowRequest(t, router, http.MethodPut,
		"/api/v1/backup-sets/api-server/var-backups/workflow/environment/PGPASSWORD",
		`{"secret":{"env":"RETND_PG_PASSWORD"}}`); rec.Code != http.StatusOK {
		t.Fatalf("the per-set write got %d, body: %s", rec.Code, rec.Body.String())
	}

	if got := len(fx.env[""]); got != 0 {
		t.Errorf("the deployment-wide layer holds %d entries after a per-set write, want 0", got)
	}
	if got := fx.env["api-server/var-backups"]; len(got) != 1 || got[0].Name != "PGPASSWORD" {
		t.Fatalf("the set's layer holds %+v, want the one variable the path named", got)
	}

	// And the read of the other scope is still empty, which is what an
	// operator asking "did I put this in the right place" is looking at.
	rec := workflowRequest(t, router, http.MethodGet, "/api/v1/settings/workflow/environment", "")
	var body listWorkflowEnvironmentResponse
	decodeBody(t, rec, &body)
	if body.BackupSetID != "" || len(body.Variables) != 0 {
		t.Errorf("the deployment-wide read came back as %+v, want the empty deployment scope", body)
	}
}

func TestWorkflowEnvironment_UnsettingAVariableThatIsNotThereIsRefused(t *testing.T) {
	router, _ := workflowRouter(t)

	rec := workflowRequest(t, router, http.MethodDelete,
		"/api/v1/settings/workflow/environment/PGPASSWORD", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rec.Code, rec.Body.String())
	}
	// A refusal rather than a silent success, because "unset PGPASSWORD"
	// that quietly did nothing is indistinguishable from one that worked,
	// and the case it hides is an operator clearing a credential from the
	// wrong scope.
	if code := errorCodeOf(t, rec); code != "WORKFLOW_ENV_NOT_FOUND" {
		t.Errorf("code = %q, want WORKFLOW_ENV_NOT_FOUND", code)
	}
}

func TestWorkflowEnvironment_APresentEmptyValueIsNotTheSameAsNoValue(t *testing.T) {
	router, backend := workflowRouter(t)
	fx := workflowOf(backend)

	if rec := workflowRequest(t, router, http.MethodPut,
		"/api/v1/settings/workflow/environment/PGOPTIONS", `{"value":""}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	// `value: ""` is a deliberately empty variable and operators write
	// it. A handler that only forwarded non-empty literals would turn it
	// into a variable with no value at all, which config.Validate then
	// reads as a contradiction beside a secret reference.
	if got := fx.lastEnvSet; !got.HasValue || got.Value != "" {
		t.Fatalf("the write reached the backend as %+v, want HasValue true with an empty literal", got)
	}

	rec := workflowRequest(t, router, http.MethodGet, "/api/v1/settings/workflow/environment", "")
	var body listWorkflowEnvironmentResponse
	decodeBody(t, rec, &body)
	if len(body.Variables) != 1 || !body.Variables[0].HasValue {
		t.Errorf("the read came back as %+v, want has_value true", body.Variables)
	}
}

// workflowSecretSpellings are the three locations a value can come from,
// each with a marker a test can search a whole response body for.
var workflowSecretSpellings = []struct {
	name   string
	body   string
	marker string
}{
	{"a file", `{"secret":{"file":"/etc/backupd/pg.passphrase"}}`, "/etc/backupd/pg.passphrase"},
	{"a variable name", `{"secret":{"env":"RETND_PG_PASSWORD"}}`, "RETND_PG_PASSWORD"},
	{"a command", `{"secret":{"command":["vault","read","-field=password","secret/pg"]}}`, "secret/pg"},
}

// TestNoWorkflowResponseCarriesAResolvedSecret is #813's requirement that
// no workflow response body can contain a resolved secret, asserted as a
// property of the SHAPE rather than as the absence of a bug today.
//
// It drives both env scopes with each of the three secret spellings and
// then checks two things about every response. The location the request
// named is present, which is the positive control: without it, a handler
// that dropped the whole secret block would pass the interesting half
// trivially. And the set of JSON keys the body carries, at every depth, is
// exactly the set this contract declares -- so a field named
// `resolved_value`, or a `secret` block that grew a `value`, fails here on
// the commit that adds it rather than after somebody reads a response.
func TestNoWorkflowResponseCarriesAResolvedSecret(t *testing.T) {
	scopes := []struct {
		name string
		path string
	}{
		{"the deployment scope", "/api/v1/settings/workflow/environment"},
		{"one backup set", "/api/v1/backup-sets/api-server/var-backups/workflow/environment"},
	}

	for _, scope := range scopes {
		for _, spelling := range workflowSecretSpellings {
			t.Run(scope.name+" carrying "+spelling.name, func(t *testing.T) {
				router, _ := workflowRouter(t)

				write := workflowRequest(t, router, http.MethodPut, scope.path+"/PGPASSWORD", spelling.body)
				if write.Code != http.StatusOK {
					t.Fatalf("the write got %d, body: %s", write.Code, write.Body.String())
				}
				read := workflowRequest(t, router, http.MethodGet, scope.path, "")
				if read.Code != http.StatusOK {
					t.Fatalf("the read got %d, body: %s", read.Code, read.Body.String())
				}

				for _, rec := range []*httptest.ResponseRecorder{write, read} {
					if !strings.Contains(rec.Body.String(), spelling.marker) {
						t.Fatalf("the response does not carry the location the request named (%s), so an assertion about what it does NOT carry would pass for the wrong reason: %s",
							spelling.marker, rec.Body.String())
					}
					if keys := unexpectedJSONKeys(t, rec.Body.Bytes(), workflowEnvironmentResponseKeys); len(keys) > 0 {
						t.Errorf("the response carries %v, which this contract does not declare. A workflow response may carry a secret's LOCATION and never its value, so a new field here has to be argued for rather than added.", keys)
					}

					var body listWorkflowEnvironmentResponse
					decodeBody(t, rec, &body)
					if len(body.Variables) != 1 {
						t.Fatalf("variables = %+v, want the one entry", body.Variables)
					}
					// A variable whose value comes from a reference has no
					// literal, and both fields say so: a handler that
					// filled `value` in from anywhere would be reporting a
					// resolved secret in the one field shaped to hold one.
					if v := body.Variables[0]; v.Value != "" || v.HasValue {
						t.Errorf("the entry came back with value %q and has_value %v, want an empty literal and false", v.Value, v.HasValue)
					}
				}
			})
		}
	}
}

// workflowEnvironmentResponseKeys is every JSON key an environment
// response may carry, at any depth. Written out rather than derived from
// the Go type on purpose: deriving it from the shape under test would
// make the assertion agree with whatever that shape became, which is the
// one thing this check must not do. The contract's own schemas
// (ListWorkflowEnvironmentResponse, WorkflowEnvironmentVariable,
// WorkflowSecretReference) are what this list transcribes.
var workflowEnvironmentResponseKeys = map[string]bool{
	"backup_set_id": true,
	"variables":     true,
	"name":          true,
	"value":         true,
	"has_value":     true,
	"secret":        true,
	"file":          true,
	"env":           true,
	"command":       true,
}

// unexpectedJSONKeys reports every key in a JSON document that is not in
// allowed, at any depth, sorted.
func unexpectedJSONKeys(t *testing.T, raw []byte, allowed map[string]bool) []string {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decoding %q: %v", raw, err)
	}
	found := map[string]bool{}
	walkJSONKeys(decoded, func(key string) {
		if !allowed[key] {
			found[key] = true
		}
	})
	out := make([]string, 0, len(found))
	for key := range found {
		out = append(out, key)
	}
	sort.Strings(out)

	return out
}

func walkJSONKeys(node any, visit func(string)) {
	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			visit(key)
			walkJSONKeys(value, visit)
		}
	case []any:
		for _, value := range typed {
			walkJSONKeys(value, visit)
		}
	}
}

// TestNoWorkflowResponseCarriesAResolvedSecret_WouldCatchOne is the
// control the case above needs: a negative assertion over a key set is
// exactly the shape that degrades into checking nothing, so this proves
// the walk fires on a body that carries one extra field.
func TestNoWorkflowResponseCarriesAResolvedSecret_WouldCatchOne(t *testing.T) {
	leaked := []byte(`{"variables":[{"name":"PGPASSWORD","secret":{"env":"X","resolved_value":"hunter2"}}]}`)
	if keys := unexpectedJSONKeys(t, leaked, workflowEnvironmentResponseKeys); len(keys) != 1 || keys[0] != "resolved_value" {
		t.Fatalf("the key walk reported %v for a body carrying a resolved value, so the assertion above proves nothing", keys)
	}
}

// ----------------------------------------------------------- run reads ---

func TestListWorkflowRuns_PassesTheFilterAndTheLimitAsSent(t *testing.T) {
	router, backend := workflowRouter(t)
	fx := workflowOf(backend)
	fx.runs = []service.WorkflowRunDetail{{
		RunID: "wfr_1", BackupSetID: "api-server/var-backups",
		State: "success", BackupStatus: "success", CleanupStatus: "success", WorkflowStatus: "success",
		RecoveryState: "none", StartedAt: time.Unix(1700000000, 0).UTC(), DurationMillis: 4200, ScriptCount: 3,
	}}

	rec := workflowRequest(t, router, http.MethodGet,
		"/api/v1/workflow-runs?backup_set=api-server%2Fvar-backups&limit=25", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if fx.lastRunsFilter != "api-server/var-backups" || fx.lastRunsLimit != 25 {
		t.Errorf("the read reached the backend with filter %q and limit %d", fx.lastRunsFilter, fx.lastRunsLimit)
	}

	var body listWorkflowRunsResponse
	decodeBody(t, rec, &body)
	if len(body.Runs) != 1 {
		t.Fatalf("runs = %+v, want one", body.Runs)
	}
	run := body.Runs[0]
	// The three statuses stay three on the wire. A response that carried
	// one verdict could not say "the backup succeeded and the cleanup did
	// not", which is the case this whole feature exists to report.
	if run.BackupStatus == "" || run.CleanupStatus == "" || run.WorkflowStatus == "" {
		t.Errorf("a status axis came back empty: %+v", run)
	}
	if run.DurationMs != 4200 {
		t.Errorf("duration_ms = %d, want the milliseconds core/service measured", run.DurationMs)
	}
	// Empty rather than null, so a client has one shape to iterate.
	if run.Steps == nil {
		t.Errorf("steps came back as null on a list read; an empty array is one shape to render instead of two")
	}
}

func TestListWorkflowRuns_IgnoresALimitTheRouteTreatsAsAbsent(t *testing.T) {
	router, backend := workflowRouter(t)

	// Absent, unparseable and non-positive all mean the engine's own
	// default. Refusing would blank a panel over a query string, which is
	// the one thing these reads exist not to do.
	for _, query := range []string{"", "?limit=banana", "?limit=0", "?limit=-4"} {
		rec := workflowRequest(t, router, http.MethodGet, "/api/v1/workflow-runs"+query, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%q got %d, want 200, body: %s", query, rec.Code, rec.Body.String())
		}
		if got := workflowOf(backend).lastRunsLimit; got != 0 {
			t.Errorf("%q reached the backend as limit %d, want 0 (the backend's own default)", query, got)
		}
	}
}

func TestGetWorkflowRun_ReportsARunThisDeploymentDoesNotHold(t *testing.T) {
	router, backend := workflowRouter(t)
	workflowOf(backend).err = fmt.Errorf("%w: wfr_gone", service.ErrWorkflowRunNotFound)

	rec := workflowRequest(t, router, http.MethodGet, "/api/v1/workflow-runs/wfr_gone", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "WORKFLOW_RUN_NOT_FOUND" {
		t.Errorf("code = %q, want WORKFLOW_RUN_NOT_FOUND", code)
	}
}

func TestListWorkflowRunSteps_KeepsAnUnobservedExitStatusDistinctFromZero(t *testing.T) {
	router, backend := workflowRouter(t)
	zero := 0
	workflowOf(backend).steps = []service.WorkflowStepDetail{
		{StepID: "s1", Order: 1, ScriptName: "10-quiesce.local.sh", Scope: "set", Phase: "before", Target: "local",
			State: "success", ExitCode: &zero, TimeoutMillis: 30000, DurationMillis: 120},
		{StepID: "s2", Order: 2, ScriptName: "20-release.local.sh", Scope: "set", Phase: "after", Target: "local",
			State: "interrupted", TerminationConfirmed: false, TimeoutMillis: 30000},
	}

	rec := workflowRequest(t, router, http.MethodGet, "/api/v1/workflow-runs/wfr_1/steps", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var body listWorkflowStepsResponse
	decodeBody(t, rec, &body)
	if body.RunID != "wfr_1" {
		t.Errorf("run_id = %q, want the run the path named", body.RunID)
	}
	if len(body.Steps) != 2 {
		t.Fatalf("steps = %+v, want two", body.Steps)
	}
	// Nil and 0 are different answers: one is "the hook exited cleanly"
	// and the other is "we never found out", and a client that read a
	// missing status as success would report an interrupted hook as fine.
	if body.Steps[0].ExitCode == nil || *body.Steps[0].ExitCode != 0 {
		t.Errorf("the observed exit status came back as %v, want 0", body.Steps[0].ExitCode)
	}
	if body.Steps[1].ExitCode != nil {
		t.Errorf("the interrupted step came back with exit_code %v, want null", body.Steps[1].ExitCode)
	}
}

func TestGetWorkflowStepLogs_CarriesTheCursorTheFollowerSent(t *testing.T) {
	router, backend := workflowRouter(t)
	fx := workflowOf(backend)
	fx.page = service.WorkflowStepLogPage{
		Records: []service.WorkflowStepLogRecord{
			{Seq: 43, StepID: "s1", Stream: "stdout", Kind: "output", At: time.Unix(1700000000, 0).UTC(), Text: "pausing"},
		},
		Cursor: 43, Truncated: true, Complete: false, StepState: "running",
	}

	rec := workflowRequest(t, router, http.MethodGet,
		"/api/v1/workflow-runs/wfr_1/steps/s1/logs?after=42&limit=500&wait=3", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	got := fx.lastLogRequest
	if got.RunID != "wfr_1" || got.StepID != "s1" {
		t.Errorf("the read reached the backend scoped to %q/%q, want wfr_1/s1", got.RunID, got.StepID)
	}
	if got.After != 42 || got.Limit != 500 || got.Wait != 3*time.Second {
		t.Errorf("the read reached the backend as after=%d limit=%d wait=%v", got.After, got.Limit, got.Wait)
	}

	var body workflowStepLogPageResponse
	decodeBody(t, rec, &body)
	if body.Cursor != 43 {
		t.Errorf("cursor = %d, want the sequence to send next", body.Cursor)
	}
	// Truncated is a property of the LOG and not of this page, and
	// complete is the only thing that tells a follower it may stop.
	if !body.Truncated || body.Complete {
		t.Errorf("truncated = %v and complete = %v, want true and false", body.Truncated, body.Complete)
	}
	if len(body.Records) != 1 || body.Records[0].Stream != "stdout" || body.Records[0].Kind != "output" {
		t.Errorf("records = %+v, want the one record with its stream and kind kept apart", body.Records)
	}
}

func TestGetWorkflowStepLogs_ReportsAStepTheRunDoesNotHave(t *testing.T) {
	router, backend := workflowRouter(t)
	workflowOf(backend).err = service.ErrWorkflowStepNotFound

	rec := workflowRequest(t, router, http.MethodGet, "/api/v1/workflow-runs/wfr_1/steps/nope/logs", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rec.Code, rec.Body.String())
	}
	// Its own code rather than the run's: a follower polling logs meets
	// this the moment a run is re-planned, and being told the RUN does
	// not exist would send it to the wrong screen.
	if code := errorCodeOf(t, rec); code != "WORKFLOW_STEP_NOT_FOUND" {
		t.Errorf("code = %q, want WORKFLOW_STEP_NOT_FOUND", code)
	}
}

func TestWorkflowReads_ReportAProcessWithNoWorkflowEngineRatherThanAnEmptyAnswer(t *testing.T) {
	// "This deployment has no workflow runs" and "this process cannot
	// tell you" are different answers, and a surface that reported the
	// first for the second would tell an operator their interrupted run
	// does not exist.
	for _, target := range []string{
		"/api/v1/workflow-runs",
		"/api/v1/workflow-runs/wfr_1",
		"/api/v1/workflow-runs/wfr_1/steps/s1/logs",
		"/api/v1/workflow-recovery",
	} {
		router, backend := workflowRouter(t)
		workflowOf(backend).err = service.ErrWorkflowsNotWired

		rec := workflowRequest(t, router, http.MethodGet, target, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s got %d, want 503, body: %s", target, rec.Code, rec.Body.String())
			continue
		}
		if code := errorCodeOf(t, rec); code != "WORKFLOW_ENGINE_UNAVAILABLE" {
			t.Errorf("%s answered %q, want WORKFLOW_ENGINE_UNAVAILABLE", target, code)
		}
	}
}

// ------------------------------------------------------------ recovery ---

func TestListWorkflowRecovery_ReportsEveryHoldWithWhereItsHooksAre(t *testing.T) {
	router, backend := workflowRouter(t)
	workflowOf(backend).holds = []service.WorkflowRecoveryHold{{
		RunID: "wfr_1", BackupSetID: "api-server/var-backups", Scope: "set",
		EnteredAt: time.Unix(1700000000, 0).UTC(), SpoolRef: "/data/state/workflow-spool/wfr_1",
	}}

	rec := workflowRequest(t, router, http.MethodGet, "/api/v1/workflow-recovery", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var body workflowRecoveryResponse
	decodeBody(t, rec, &body)
	if len(body.Holds) != 1 {
		t.Fatalf("holds = %+v, want one", body.Holds)
	}
	hold := body.Holds[0]
	// entered_at answers the first question an operator asks -- how long
	// has this machine been left like this -- and spool_ref is where the
	// hooks a resume would run can be read before deciding.
	if hold.EnteredAt == "" || hold.SpoolRef == "" {
		t.Errorf("the hold came back as %+v, want both the time it was entered and the spool", hold)
	}
}

func TestResumeWorkflowCleanup_AnswersWithTheRowTheNextBackupWillBeRefusedAgainst(t *testing.T) {
	router, backend := workflowRouter(t)
	fx := workflowOf(backend)
	// A resume that ran and left the obligation outstanding, which is the
	// case a bespoke "ok" body would have made unsayable.
	fx.resumed = service.WorkflowRunDetail{
		State: "cleanup_failed", BackupStatus: "success", CleanupStatus: "failed",
		WorkflowStatus: "failed", RecoveryState: "required",
	}

	rec := workflowRequest(t, router, http.MethodPost, "/api/v1/workflow-recovery/wfr_1/resume-cleanup", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if fx.lastResumedRun != "wfr_1" {
		t.Errorf("the resume reached the backend for %q, want wfr_1", fx.lastResumedRun)
	}

	var body workflowRunResponse
	decodeBody(t, rec, &body)
	if body.RecoveryState != "required" || body.CleanupStatus != "failed" || body.BackupStatus != "success" {
		t.Errorf("the resumed run came back as %+v, want the row that still says the cleanup is owed", body)
	}
}

func TestAcknowledgeWorkflowRecovery_TakesTheActorFromTheSessionAndTheReasonFromTheBody(t *testing.T) {
	router, backend := workflowRouter(t)

	rec := workflowRequest(t, router, http.MethodPost, "/api/v1/workflow-recovery/wfr_1/acknowledge",
		`{"reason":"unmounted the snapshot by hand"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("a 204 carried a body: %q", body)
	}

	got := workflowOf(backend).lastAck
	if got.Reason != "unmounted the snapshot by hand" {
		t.Errorf("reason reached the backend as %q", got.Reason)
	}
	// The actor is the session's, never the caller's claim: the whole
	// value of this record is answering who unblocked a backup set and
	// why, and a name the caller chose would answer neither.
	if got.Actor != "alice" {
		t.Errorf("actor reached the backend as %q, want the authenticated session's own name", got.Actor)
	}
}

func TestAcknowledgeWorkflowRecovery_RefusesAnAcknowledgementWithNoReason(t *testing.T) {
	router, backend := workflowRouter(t)
	workflowOf(backend).errOnWrite = service.ErrWorkflowAcknowledgeReasonRequired

	rec := workflowRequest(t, router, http.MethodPost, "/api/v1/workflow-recovery/wfr_1/acknowledge", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
	// Its own code and not INVALID_REQUEST: the request was not
	// malformed, it was complete and refused, and the remedy is a
	// sentence rather than a different field.
	if code := errorCodeOf(t, rec); code != "WORKFLOW_ACKNOWLEDGEMENT_REASON_REQUIRED" {
		t.Errorf("code = %q, want WORKFLOW_ACKNOWLEDGEMENT_REASON_REQUIRED", code)
	}
}

// -------------------------------------------------------------- tiering ---

// workflowWrites is every mutating workflow route, with a body each one
// accepts. It is the same list destructiveGateExemptRoutes carries, in
// concrete form.
var workflowWrites = []struct {
	method string
	target string
	body   string
}{
	{http.MethodPatch, "/api/v1/settings/workflow", `{"root":"/workflows"}`},
	{http.MethodPut, "/api/v1/settings/workflow/environment/PGHOST", `{"value":"10.0.0.14"}`},
	{http.MethodDelete, "/api/v1/settings/workflow/environment/PGHOST", ""},
	{http.MethodPatch, "/api/v1/backup-sets/api-server/var-backups/workflow", `{"before_dir":"pg.before.d"}`},
	{http.MethodPut, "/api/v1/backup-sets/api-server/var-backups/workflow/environment/PGHOST", `{"value":"10.0.0.14"}`},
	{http.MethodDelete, "/api/v1/backup-sets/api-server/var-backups/workflow/environment/PGHOST", ""},
	{http.MethodPost, "/api/v1/workflow-recovery/wfr_1/resume-cleanup", ""},
	{http.MethodPost, "/api/v1/workflow-recovery/wfr_1/acknowledge", `{"reason":"dealt with by hand"}`},
}

// TestEveryWorkflowWriteIsReachableWithTheDestructiveGateClosed turns the
// eight entries this surface adds to destructiveGateExemptRoutes into
// something that fails if somebody gates one.
//
// The gate is a deployment-wide attestation an operator may never have
// made, and every one of these writes is the wrong thing to put behind
// it: five edit configuration, and the two recovery writes are how an
// operator UNWINDS a hook that stopped their database. A gated
// resume-cleanup is a source machine left quiesced because a deployment
// had not turned destructive operations on.
//
// It asserts on the gate's own typed code rather than on a status,
// because the gate and requireCSRF both answer 403 -- which is how a
// weaker version of this test in this package once stayed green with the
// gate deleted from both routes it claimed to cover.
func TestEveryWorkflowWriteIsReachableWithTheDestructiveGateClosed(t *testing.T) {
	for _, write := range workflowWrites {
		t.Run(write.method+" "+write.target, func(t *testing.T) {
			backend := newSyncFakeBackend()
			// The unset needs something to remove, or it answers 404
			// before the tiering question is reached -- which would be a
			// pass for the wrong reason.
			fx := workflowOf(backend)
			fx.env[""] = []service.WorkflowEnvVar{{Name: "PGHOST", Value: "10.0.0.14", HasValue: true}}
			fx.env["api-server/var-backups"] = []service.WorkflowEnvVar{{Name: "PGHOST", Value: "10.0.0.14", HasValue: true}}

			router := NewRouter(RouterConfig{
				Platform:      allowingPlatform("alice"),
				Backend:       backend,
				Gate:          NotYetImplementedGate{}, // the production default: NOT passed
				BinaryVersion: "test",
				Commit:        "test",
			})

			rec := workflowRequest(t, router, write.method, write.target, write.body)
			if code := errorCodeOf(t, rec); code == "DESTRUCTIVE_OPERATIONS_DISABLED" {
				t.Fatalf("a closed destructive gate refused this write (%d %s). Every workflow write edits configuration or unwinds a hook that is already owed; gating one puts it out of reach of an operator who has not turned destructive operations on, which is the opposite of what the gate protects.",
					rec.Code, rec.Body.String())
			}
			if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 200 or 204, body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// ------------------------------------------------------ authorization ---

// revocableAuthenticator is an Authenticator a test can switch off
// between two requests, which is the only way to express "this session
// has since become unauthenticated" against a router that authenticates
// per request.
type revocableAuthenticator struct{ allowed *atomic.Bool }

func (a revocableAuthenticator) Authenticate(_ context.Context, _ capabilities.AuthRequest) (capabilities.AuthContext, error) {
	if !a.allowed.Load() {
		return capabilities.AuthContext{}, capabilities.ErrCapabilityUnsupported
	}

	return capabilities.AuthContext{Authenticated: true, Username: "alice"}, nil
}

type revocablePlatform struct {
	capabilities.BasePlatformAdapter
	allowed *atomic.Bool
}

func (revocablePlatform) ID() capabilities.PlatformID { return capabilities.PlatformGeneric }

func (revocablePlatform) Capabilities() capabilities.PlatformCapabilities {
	return capabilities.PlatformCapabilities{}
}

func (p revocablePlatform) Authenticator() capabilities.Authenticator {
	return revocableAuthenticator{allowed: p.allowed}
}

func (revocablePlatform) PlatformInfo(_ context.Context) (capabilities.PlatformInfo, error) {
	return capabilities.PlatformInfo{ID: capabilities.PlatformGeneric, Name: "revocable"}, nil
}

// TestWorkflowStepLogs_AuthorizationIsRecheckedOnEveryCursorResume is
// #813's requirement about the step-log read, and the reason the read is
// a cursor poll rather than a stream.
//
// A stream holds one authorization decision for as long as the connection
// lasts, so "is this caller still allowed" becomes a thing somebody has to
// implement and then remember to keep. This route cannot have that bug,
// because a resume is not a continuation of anything: it is an ordinary
// request carrying a cursor, so it passes the same authMiddleware the
// first page did.
//
// The three cases are the whole claim. An unauthenticated establishment is
// refused; an authenticated establishment and an authenticated resume both
// work, which is what makes the third case about authorization rather than
// about the route being broken; and a resume from a session that has SINCE
// become unauthenticated is refused with the same 401 -- at replay time,
// not only at establishment.
func TestWorkflowStepLogs_AuthorizationIsRecheckedOnEveryCursorResume(t *testing.T) {
	const (
		establish = "/api/v1/workflow-runs/wfr_1/steps/s1/logs"
		resume    = "/api/v1/workflow-runs/wfr_1/steps/s1/logs?after=42"
	)

	allowed := &atomic.Bool{}
	backend := newSyncFakeBackend()
	workflowOf(backend).page = service.WorkflowStepLogPage{Cursor: 43, StepState: "running"}
	router := NewRouter(RouterConfig{
		Platform:      revocablePlatform{allowed: allowed},
		Backend:       backend,
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})

	get := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))

		return rec
	}

	// (a) Establishment, unauthenticated.
	if rec := get(establish); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated establishment got %d, want 401, body: %s", rec.Code, rec.Body.String())
	}
	// A resume is refused in the same state, so the cursor is not a way
	// in either.
	if rec := get(resume); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated resume got %d, want 401, body: %s", rec.Code, rec.Body.String())
	}

	// The positive control: with the session live, both the first page
	// and the resume are served. Without this, the assertions above and
	// below would be equally consistent with a route that never answers.
	allowed.Store(true)
	if rec := get(establish); rec.Code != http.StatusOK {
		t.Fatalf("an authenticated establishment got %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if rec := get(resume); rec.Code != http.StatusOK {
		t.Fatalf("an authenticated resume got %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	// (b) The session goes away between two pages of the same follow.
	// This is the case a stream gets wrong: the follower holds a cursor
	// and an established intent, and neither is authorization.
	allowed.Store(false)
	rec := get(resume)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a resume from a session that has since become unauthenticated got %d, want 401. Authorization on this route is re-checked on every page, including every replay, which is the property that makes a cursor poll safe where a long-lived stream would not be.",
			rec.Code)
	}
	if code := errorCodeOf(t, rec); code != "UNAUTHENTICATED" {
		t.Errorf("code = %q, want UNAUTHENTICATED", code)
	}
}

// ------------------------------------------------------------- bypass ---

func TestSubmitRunBackupSet_CarriesTheBypassAndItsAuthorization(t *testing.T) {
	backend := newSyncFakeBackend()
	router := NewRouter(RouterConfig{
		Platform: allowingPlatform("alice"), Backend: backend, Gate: alwaysPassGate{},
		BinaryVersion: "test", Commit: "test",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/operations", strings.NewReader(
		`{"action":"run_backup_set","config_revision":"rev-1","backup_set_id":"api-server/var-backups","skip_workflow_scripts":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "bypass-1")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body: %s", rec.Code, rec.Body.String())
	}

	got := backend.lastRunBackupSet
	if !got.SkipWorkflowScripts {
		t.Errorf("skip_workflow_scripts did not reach the service; the flag is the whole request and a handler that dropped it would answer 202 for a run that executes the hooks anyway")
	}
	// Administrator is this surface's statement that the caller may take
	// an administrator action, and over HTTP the gate in front of this
	// route IS that boundary. Asserted here because core/service refuses
	// the bypass without it, so a handler that left it false would turn
	// every bypass into a refusal nobody could act on.
	if !got.Administrator {
		t.Errorf("the submission did not claim administrator authority, so core/service would refuse the bypass. This route is behind requireDestructiveGate, which is what makes the claim true.")
	}
}

func TestSubmitRunCycle_RefusesABypassThatBelongsToAPerSetRun(t *testing.T) {
	backend := newSyncFakeBackend()
	router := NewRouter(RouterConfig{
		Platform: allowingPlatform("alice"), Backend: backend, Gate: alwaysPassGate{},
		BinaryVersion: "test", Commit: "test",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/operations", strings.NewReader(
		`{"action":"run_cycle","config_revision":"rev-1","skip_workflow_scripts":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "bypass-2")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// A deployment-wide bypass is not an operation this product has.
	// Serving it as an ordinary cycle would teach a client the field is
	// optional here, which ends with somebody believing every set's hooks
	// were skipped.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "INVALID_REQUEST" {
		t.Errorf("code = %q, want INVALID_REQUEST", code)
	}
	if !strings.Contains(rec.Body.String(), "skip_workflow_scripts") {
		t.Errorf("the refusal does not name the field that belongs to another action: %s", rec.Body.String())
	}
}

// The save gate on the wire (#906): a write refused by a hook script
// answers 409 with its own code AND with the blocking findings as
// structured fields.
//
// The fields are the assertion rather than the message, because they are
// the reason this body exists: a client that had to parse "at 4:1" out of
// prose would be parsing a string this package's own documentation
// describes as free to change without notice.
func TestUpdateWorkflowSettings_RefusedByAHookScriptCarriesTheBlockingFindings(t *testing.T) {
	router, backend := workflowRouter(t)
	workflowOf(backend).errOnWrite = &service.WorkflowScriptRefusal{
		Scripts: []service.WorkflowRefusedScript{
			{
				ScriptName: "10-quiesce.local.sh", Dir: "global.before.d",
				Scope: "global", Phase: "before",
				ParseError: "`if` statement must end with `fi`", ParseErrorLine: 4, ParseErrorCol: 1,
			},
			{
				ScriptName: "90-clean.local.sh", Dir: "global.before.d",
				Scope: "global", Phase: "before",
				Findings: []service.WorkflowLintFinding{{
					Code: "BSH003", Severity: "error", Line: 2, Col: 8,
					Message: "this recursive, forced delete targets /tmp whenever the expansion in it is empty",
				}},
			},
		},
	}

	rec := workflowRequest(t, router, http.MethodPatch, "/api/v1/settings/workflow", `{"before_dir":"global.before.d"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body: %s", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "WORKFLOW_SCRIPT_REJECTED" {
		t.Errorf("code = %q, want WORKFLOW_SCRIPT_REJECTED", code)
	}

	var body workflowScriptRejectedResponse
	decodeBody(t, rec, &body)

	if len(body.BlockingScripts) != 2 {
		t.Fatalf("blocking_scripts = %+v, want both refused scripts", body.BlockingScripts)
	}

	unparseable := body.BlockingScripts[0]
	if unparseable.ScriptName != "10-quiesce.local.sh" || unparseable.Dir != "global.before.d" {
		t.Errorf("the first blocking script is %+v, want 10-quiesce.local.sh in global.before.d", unparseable)
	}
	if unparseable.ParseErrorLine != 4 || unparseable.ParseErrorCol != 1 || unparseable.ParseError == "" {
		t.Errorf("the parse fault lost its position: %+v", unparseable)
	}

	refused := body.BlockingScripts[1]
	if len(refused.Findings) != 1 {
		t.Fatalf("findings = %+v, want the one blocking finding", refused.Findings)
	}
	if f := refused.Findings[0]; f.Code != "BSH003" || f.Severity != "error" || f.Line != 2 || f.Col != 8 {
		t.Errorf("finding = %+v, want BSH003 error at 2:8", f)
	}
	if refused.Findings[0].Message == "" {
		t.Error("the blocking finding carries no message, so nothing tells an operator what to write instead")
	}
}

// The per-script verification reaches the wire on the validation read,
// including the "not examined" state, which must never arrive as a pass.
func TestGetBackupSetWorkflowValidation_CarriesEachScriptsLintFindings(t *testing.T) {
	router, backend := workflowRouter(t)
	workflowOf(backend).validation = service.WorkflowValidation{
		ValidForBackup: true, WorkflowValid: false, Configured: true, Root: "/workflows",
		Scripts: []service.WorkflowValidatedScript{
			{
				StepID: "s1", Order: 1, ScriptName: "10-quiesce.local.sh", Scope: "global", Phase: "before", Target: "local",
				Lint: service.WorkflowScriptLint{
					Examined: true, Parsed: true,
					Findings: []service.WorkflowLintFinding{
						{Code: "BSH003", Severity: "error", Line: 2, Col: 8, Message: "a recursive, forced delete of a root-level path"},
						{Code: "BSH002", Severity: "warning", Line: 5, Col: 1, Message: "this cd does not check whether it worked"},
					},
				},
			},
			{
				StepID: "s2", Order: 2, ScriptName: "20-dump.remote.sh", Scope: "set", Phase: "before", Target: "remote",
				Lint: service.WorkflowScriptLint{
					Examined:          false,
					NotExaminedReason: "not examined: 20-dump.remote.sh is 2097153 bytes and this check reads at most 1048576",
				},
			},
		},
	}

	rec := workflowRequest(t, router, http.MethodGet, "/api/v1/backup-sets/api-server/var-backups/workflow/validation", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var body workflowValidationResponse
	decodeBody(t, rec, &body)

	if len(body.Scripts) != 2 {
		t.Fatalf("scripts = %+v, want both", body.Scripts)
	}

	lint := body.Scripts[0].Lint
	if !lint.Examined || !lint.Parsed {
		t.Errorf("the first script's lint = %+v, want examined and parsed", lint)
	}
	if len(lint.Findings) != 2 {
		t.Fatalf("findings = %+v, want both severities, not only the blocking one", lint.Findings)
	}
	if lint.Findings[0].Code != "BSH003" || lint.Findings[0].Line != 2 || lint.Findings[0].Col != 8 {
		t.Errorf("the error finding lost its identity or position: %+v", lint.Findings[0])
	}
	if lint.Findings[1].Severity != "warning" {
		t.Errorf("the non-blocking finding was dropped or re-graded: %+v", lint.Findings[1])
	}

	unexamined := body.Scripts[1].Lint
	if unexamined.Examined || unexamined.Parsed {
		t.Errorf("a script nobody examined came back as examined or parsed: %+v", unexamined)
	}
	if unexamined.NotExaminedReason == "" {
		t.Error("a not-examined script carries no reason, which is the one output that would mislead")
	}
	if unexamined.Findings == nil {
		t.Error("findings serialised as null rather than [], which is two shapes for a client to render")
	}
}
