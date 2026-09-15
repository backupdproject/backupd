package webhost

import (
	"context"
	"sync"

	"github.com/retnd/retnd/core/service"
)

// The BackupServiceClient doubles for EPIC L's workflow surface (#813),
// in their own file for the reason fixtures_snapshot_test.go is in its
// own: the two big fixtures next door stay about what they were already
// about, and a feature's state is looked up by the fake's own pointer so
// two fakes in one test never see each other's rows.

// workflowFixture is everything the workflow surface answers with, for
// one fake backend.
//
// The writes RECORD rather than only answer. That is the difference
// between a double and a stub, and it is what lets a test prove a handler
// reached the backend with the scope, the id and the secret REFERENCE it
// claimed rather than merely that a 200 came back -- which is the only
// way to tell a handler that dropped a field from one that passed it
// through.
type workflowFixture struct {
	mu sync.Mutex

	settings service.WorkflowSettings
	set      service.BackupSetWorkflow

	// env is keyed by scope, where "" is the deployment-wide layer, so
	// one fixture models both and a test can prove a per-set write did
	// not land on the deployment's list.
	env map[string][]service.WorkflowEnvVar

	validation service.WorkflowValidation

	runs  []service.WorkflowRunDetail
	run   service.WorkflowRunDetail
	steps []service.WorkflowStepDetail
	page  service.WorkflowStepLogPage
	holds []service.WorkflowRecoveryHold

	// resumed is the run a resume answers with, separate from run above
	// so a test can prove the handler serves what the resume returned
	// rather than whatever the last read did.
	resumed service.WorkflowRunDetail

	// What crossed the boundary, per call.
	lastSettingsUpdate service.UpdateWorkflowSettingsRequest
	lastSetUpdate      service.UpdateBackupSetWorkflowRequest
	lastSetUpdateID    string
	lastEnvScope       string
	lastEnvSet         service.WorkflowEnvVar
	lastEnvUnset       string
	lastValidatedID    string
	lastRunsFilter     string
	lastRunsLimit      int
	lastLogRequest     service.WorkflowStepLogRequest
	lastResumedRun     string
	lastAckRun         string
	lastAck            service.WorkflowAcknowledgement

	// err is what every READ in this fixture returns, and errOnWrite
	// what every write does. Two fields rather than one per method: the
	// tests that use them are about the mapping from a service refusal
	// to a status code, and which read produced it is not part of that
	// question.
	err        error
	errOnWrite error
}

var (
	workflowFixturesMu sync.Mutex
	workflowFixtures   = map[any]*workflowFixture{}
)

// workflowOf is one fake's workflow fixture, created on first use so no
// fixture constructor has to know this file exists.
func workflowOf(fake any) *workflowFixture {
	workflowFixturesMu.Lock()
	defer workflowFixturesMu.Unlock()

	f, ok := workflowFixtures[fake]
	if !ok {
		f = &workflowFixture{env: map[string][]service.WorkflowEnvVar{}}
		workflowFixtures[fake] = f
	}

	return f
}

// The env store, modelled rather than stubbed: a set replaces by name and
// an unset removes, because what the handler tests assert is that a write
// reached the scope it named, and a fixture that appended blindly would
// let a handler that ignored the path's scope pass.
func (f *workflowFixture) upsert(scope string, v service.WorkflowEnvVar) []service.WorkflowEnvVar {
	list := f.env[scope]
	for i := range list {
		if list[i].Name == v.Name {
			list[i] = v
			f.env[scope] = list

			return append([]service.WorkflowEnvVar(nil), list...)
		}
	}
	list = append(list, v)
	f.env[scope] = list

	return append([]service.WorkflowEnvVar(nil), list...)
}

func (f *workflowFixture) remove(scope, name string) ([]service.WorkflowEnvVar, error) {
	list := f.env[scope]
	for i := range list {
		if list[i].Name == name {
			list = append(list[:i:i], list[i+1:]...)
			f.env[scope] = list

			return append([]service.WorkflowEnvVar(nil), list...), nil
		}
	}

	// The real service refuses rather than reporting a silent success,
	// and so does this: an unset that quietly did nothing is
	// indistinguishable from one that worked, which is the case an
	// operator clearing a credential from the wrong scope lands in.
	return nil, service.ErrWorkflowEnvNotFound
}

func (f *syncFakeBackend) WorkflowSettings(context.Context) (service.WorkflowSettings, error) {
	fx := workflowOf(f)
	fx.mu.Lock()
	defer fx.mu.Unlock()

	settings := fx.settings
	settings.Environment = append([]service.WorkflowEnvVar(nil), fx.env[""]...)

	return settings, fx.err
}

func (f *syncFakeBackend) UpdateWorkflowSettings(_ context.Context, req service.UpdateWorkflowSettingsRequest) (service.WorkflowSettings, error) {
	fx := workflowOf(f)
	fx.mu.Lock()
	defer fx.mu.Unlock()

	fx.lastSettingsUpdate = req
	if fx.errOnWrite != nil {
		return service.WorkflowSettings{}, fx.errOnWrite
	}
	// Applied the way the real one applies a patch -- only the fields the
	// request named move -- so a handler that filled in everything it
	// could would look wrong here rather than plausible.
	if req.Root != nil {
		fx.settings.Root = *req.Root
		fx.settings.Configured = *req.Root != ""
	}
	if req.BeforeDir != nil {
		fx.settings.BeforeDir = *req.BeforeDir
	}
	if req.AfterDir != nil {
		fx.settings.AfterDir = *req.AfterDir
	}
	if req.ScriptTimeout != nil {
		fx.settings.ScriptTimeout = *req.ScriptTimeout
		fx.settings.ScriptTimeoutConfigured = *req.ScriptTimeout != 0
	}
	if req.MaxScriptSizeBytes != nil {
		fx.settings.MaxScriptSizeBytes = *req.MaxScriptSizeBytes
	}

	settings := fx.settings
	settings.Environment = append([]service.WorkflowEnvVar(nil), fx.env[""]...)

	return settings, nil
}

func (f *syncFakeBackend) BackupSetWorkflow(_ context.Context, id string) (service.BackupSetWorkflow, error) {
	fx := workflowOf(f)
	fx.mu.Lock()
	defer fx.mu.Unlock()

	set := fx.set
	set.BackupSetID = id
	set.Environment = append([]service.WorkflowEnvVar(nil), fx.env[id]...)

	return set, fx.err
}

func (f *syncFakeBackend) UpdateBackupSetWorkflow(_ context.Context, id string, req service.UpdateBackupSetWorkflowRequest) (service.BackupSetWorkflow, error) {
	fx := workflowOf(f)
	fx.mu.Lock()
	defer fx.mu.Unlock()

	fx.lastSetUpdateID, fx.lastSetUpdate = id, req
	if fx.errOnWrite != nil {
		return service.BackupSetWorkflow{}, fx.errOnWrite
	}
	if req.BeforeDir != nil {
		fx.set.BeforeDir = *req.BeforeDir
	}
	if req.AfterDir != nil {
		fx.set.AfterDir = *req.AfterDir
	}
	if req.ScriptTimeout != nil {
		fx.set.ScriptTimeout = *req.ScriptTimeout
	}
	if req.RemoteExecConnectionRef != nil {
		fx.set.RemoteExecConnectionRef = *req.RemoteExecConnectionRef
	}
	fx.set.Configured = true

	set := fx.set
	set.BackupSetID = id
	set.Environment = append([]service.WorkflowEnvVar(nil), fx.env[id]...)

	return set, nil
}

func (f *syncFakeBackend) ListWorkflowEnv(_ context.Context, backupSetID string) ([]service.WorkflowEnvVar, error) {
	fx := workflowOf(f)
	fx.mu.Lock()
	defer fx.mu.Unlock()

	fx.lastEnvScope = backupSetID

	return append([]service.WorkflowEnvVar(nil), fx.env[backupSetID]...), fx.err
}

func (f *syncFakeBackend) SetWorkflowEnv(_ context.Context, backupSetID string, v service.WorkflowEnvVar) ([]service.WorkflowEnvVar, error) {
	fx := workflowOf(f)
	fx.mu.Lock()
	defer fx.mu.Unlock()

	fx.lastEnvScope, fx.lastEnvSet = backupSetID, v
	if fx.errOnWrite != nil {
		return nil, fx.errOnWrite
	}

	return fx.upsert(backupSetID, v), nil
}

func (f *syncFakeBackend) UnsetWorkflowEnv(_ context.Context, backupSetID, name string) ([]service.WorkflowEnvVar, error) {
	fx := workflowOf(f)
	fx.mu.Lock()
	defer fx.mu.Unlock()

	fx.lastEnvScope, fx.lastEnvUnset = backupSetID, name
	if fx.errOnWrite != nil {
		return nil, fx.errOnWrite
	}

	return fx.remove(backupSetID, name)
}

func (f *syncFakeBackend) ValidateWorkflow(_ context.Context, id string) (service.WorkflowValidation, error) {
	fx := workflowOf(f)
	fx.mu.Lock()
	defer fx.mu.Unlock()

	fx.lastValidatedID = id
	report := fx.validation
	report.BackupSetID = id

	return report, fx.err
}

func (f *syncFakeBackend) WorkflowRuns(_ context.Context, backupSetID string, limit int) ([]service.WorkflowRunDetail, error) {
	fx := workflowOf(f)
	fx.mu.Lock()
	defer fx.mu.Unlock()

	fx.lastRunsFilter, fx.lastRunsLimit = backupSetID, limit

	return append([]service.WorkflowRunDetail(nil), fx.runs...), fx.err
}

func (f *syncFakeBackend) WorkflowRun(_ context.Context, runID string) (service.WorkflowRunDetail, error) {
	fx := workflowOf(f)
	fx.mu.Lock()
	defer fx.mu.Unlock()

	run := fx.run
	run.RunID = runID

	return run, fx.err
}

func (f *syncFakeBackend) WorkflowSteps(context.Context, string) ([]service.WorkflowStepDetail, error) {
	fx := workflowOf(f)
	fx.mu.Lock()
	defer fx.mu.Unlock()

	return append([]service.WorkflowStepDetail(nil), fx.steps...), fx.err
}

func (f *syncFakeBackend) WorkflowStepLogs(_ context.Context, req service.WorkflowStepLogRequest) (service.WorkflowStepLogPage, error) {
	fx := workflowOf(f)
	fx.mu.Lock()
	defer fx.mu.Unlock()

	fx.lastLogRequest = req
	page := fx.page
	page.RunID, page.StepID = req.RunID, req.StepID

	return page, fx.err
}

func (f *syncFakeBackend) WorkflowRecovery(context.Context) ([]service.WorkflowRecoveryHold, error) {
	fx := workflowOf(f)
	fx.mu.Lock()
	defer fx.mu.Unlock()

	return append([]service.WorkflowRecoveryHold(nil), fx.holds...), fx.err
}

func (f *syncFakeBackend) ResumeWorkflowCleanup(_ context.Context, runID string) (service.WorkflowRunDetail, error) {
	fx := workflowOf(f)
	fx.mu.Lock()
	defer fx.mu.Unlock()

	fx.lastResumedRun = runID
	if fx.errOnWrite != nil {
		return service.WorkflowRunDetail{}, fx.errOnWrite
	}
	run := fx.resumed
	run.RunID = runID

	return run, nil
}

func (f *syncFakeBackend) AcknowledgeWorkflowRecovery(_ context.Context, runID string, ack service.WorkflowAcknowledgement) error {
	fx := workflowOf(f)
	fx.mu.Lock()
	defer fx.mu.Unlock()

	fx.lastAckRun, fx.lastAck = runID, ack

	return fx.errOnWrite
}

// The async double's half. It exists to prove operation lifetime survives
// a disconnected client (disconnect_test.go), so none of these has
// behaviour worth modelling: each answers the empty value, which is a
// real answer for a deployment that has run no workflow, rather than an
// error every test would then have to route around.

func (f *asyncFakeBackend) WorkflowSettings(context.Context) (service.WorkflowSettings, error) {
	return service.WorkflowSettings{}, nil
}

func (f *asyncFakeBackend) UpdateWorkflowSettings(context.Context, service.UpdateWorkflowSettingsRequest) (service.WorkflowSettings, error) {
	return service.WorkflowSettings{}, nil
}

func (f *asyncFakeBackend) BackupSetWorkflow(_ context.Context, id string) (service.BackupSetWorkflow, error) {
	return service.BackupSetWorkflow{BackupSetID: id}, nil
}

func (f *asyncFakeBackend) UpdateBackupSetWorkflow(_ context.Context, id string, _ service.UpdateBackupSetWorkflowRequest) (service.BackupSetWorkflow, error) {
	return service.BackupSetWorkflow{BackupSetID: id, Configured: true}, nil
}

func (f *asyncFakeBackend) ListWorkflowEnv(context.Context, string) ([]service.WorkflowEnvVar, error) {
	return nil, nil
}

func (f *asyncFakeBackend) SetWorkflowEnv(_ context.Context, _ string, v service.WorkflowEnvVar) ([]service.WorkflowEnvVar, error) {
	return []service.WorkflowEnvVar{v}, nil
}

func (f *asyncFakeBackend) UnsetWorkflowEnv(context.Context, string, string) ([]service.WorkflowEnvVar, error) {
	return nil, nil
}

func (f *asyncFakeBackend) ValidateWorkflow(_ context.Context, id string) (service.WorkflowValidation, error) {
	return service.WorkflowValidation{BackupSetID: id, ValidForBackup: true, WorkflowValid: true}, nil
}

func (f *asyncFakeBackend) WorkflowRuns(context.Context, string, int) ([]service.WorkflowRunDetail, error) {
	return nil, nil
}

func (f *asyncFakeBackend) WorkflowRun(_ context.Context, runID string) (service.WorkflowRunDetail, error) {
	return service.WorkflowRunDetail{RunID: runID}, nil
}

func (f *asyncFakeBackend) WorkflowSteps(context.Context, string) ([]service.WorkflowStepDetail, error) {
	return nil, nil
}

func (f *asyncFakeBackend) WorkflowStepLogs(_ context.Context, req service.WorkflowStepLogRequest) (service.WorkflowStepLogPage, error) {
	return service.WorkflowStepLogPage{RunID: req.RunID, StepID: req.StepID, Cursor: req.After}, nil
}

func (f *asyncFakeBackend) WorkflowRecovery(context.Context) ([]service.WorkflowRecoveryHold, error) {
	return nil, nil
}

func (f *asyncFakeBackend) ResumeWorkflowCleanup(_ context.Context, runID string) (service.WorkflowRunDetail, error) {
	return service.WorkflowRunDetail{RunID: runID}, nil
}

func (f *asyncFakeBackend) AcknowledgeWorkflowRecovery(context.Context, string, service.WorkflowAcknowledgement) error {
	return nil
}
