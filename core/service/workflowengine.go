package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/backupdproject/backupd/core/internal/app"
	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/hostrunner"
	"github.com/backupdproject/backupd/core/internal/metrics"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/remoteexec"
	"github.com/backupdproject/backupd/core/internal/workflow"
	"github.com/backupdproject/backupd/core/internal/workflowrun"
	"github.com/google/uuid"
)

// Where EPIC L's engine is actually assembled and switched on (#813).
//
// internal/workflowrun shipped with L4 as a complete lifecycle and no
// wiring at all: nothing constructed an Engine, nothing called Reconcile,
// and internal/app ran every backup exactly as it did before EPIC L
// existed. This file is that wiring, and it is here rather than in
// internal/app for the reason app/workflow.go states -- the engine needs
// a journal, a host-runner client, a remote-exec dialer, a redactor and a
// deployment-wide lock table, and this is the layer that already holds
// or can build every one of them.
//
// # Reconcile is a startup step, not a lazy one
//
// workflowrun.Run refuses with ErrNotReconciled until Reconcile has run,
// and that refusal is load-bearing: the reconciliation is what decides
// which backup sets have an unresolved interruption, and a run started
// before that answer exists is a backup taken over a machine that may
// still be quiesced. So ReconcileWorkflows is called by the process that
// is about to serve, BEFORE it serves, and a failure there is a startup
// failure rather than something discovered at the first backup.
//
// It is deliberately NOT called from New. New is what every core/ test
// builds, in memory, against a journal it opened itself; making it do a
// reconciliation pass would put a journal write in the constructor and
// make the refusal untestable, because nothing could ever observe an
// engine that had not reconciled.
//
// # Why the executors are adapters and not values
//
// A hot reload replaces this service's configuration in place
// (configreload.go) and must move the workflow with it: an operator who
// declares a new execution connection, or points the runner socket
// somewhere else, expects the next run to use it. An Engine holding a
// hostrunner.Client value built at construction would be pinned to
// whatever config.yaml said when the process started. So the two
// executors resolve their connection PER STEP, from this service's own
// atomic view of the configuration, which is the same discipline
// remoteexec.RemoteExecutor already applies for its own reason (a
// capability is bound to a connection, so a cached client would be a
// proof that outlived the thing it was a proof about).
//
// The Engine itself is built once and never replaced, because two of its
// fields are per-DEPLOYMENT rather than per-configuration: the set lock
// table and the in-memory record of which sets are blocked. Rebuilding it
// on a reload would drop a lock a running backup holds and forget every
// recovery hold the startup pass found.

// ErrWorkflowsNotWired is what the workflow surfaces report in a process
// that never built an engine.
//
// Every core/ test builds a BackupService with New and never calls
// ReconcileWorkflows, so this is the answer those get, and it is a
// refusal rather than an empty result on purpose: "this deployment has no
// workflow runs" and "this process cannot answer that question" are
// different facts, and a surface that reported the first for the second
// would tell an operator their interrupted run does not exist.
var ErrWorkflowsNotWired = errors.New("service: this process has no workflow engine, so it cannot answer for workflow runs")

// ErrWorkflowBypassNotAuthorized is the refusal a --skip-workflow-scripts
// run gets from a caller that has not established administrator
// authority.
//
// See workflowRunOptions for what each surface has to do to establish it,
// and why the answer is different on a terminal and on an HTTP route.
var ErrWorkflowBypassNotAuthorized = errors.New("service: skipping a backup set's workflow scripts is an administrator action")

// ErrWorkflowBypassOnScheduledRun is #813's flat prohibition: a bypass is
// something a person does once, knowing why, and a schedule is nobody
// doing anything.
//
// A scheduled run that could carry it would be a deployment whose hooks
// are configured, whose operator believes they run, and which quietly
// stopped running them at some point nobody can date.
var ErrWorkflowBypassOnScheduledRun = errors.New("service: a scheduled run cannot skip its workflow scripts")

// workflowRuntime is everything this service holds for EPIC L.
//
// One value, built once, so that "is the workflow wired" is one nil check
// rather than four, and so a hot reload cannot half-replace it.
type workflowRuntime struct {
	engine  *workflowrun.Engine
	metrics *metrics.Workflow

	// probes is the short-lived cache of the two reachability answers
	// the health report carries (workflowhealth.go). It is here rather
	// than on BackupService because it is part of the same per-process
	// workflow state the engine is, and because a hot reload must not
	// drop it: a dashboard polling across a settings save would
	// otherwise re-open an SSH connection to the source host at the
	// moment somebody is editing configuration.
	probes workflowProbeCache

	// version is this binary's product version, which the host runner
	// compares against its own and refuses on a mismatch. It is set
	// through SetBuildVersion because it arrives from an -ldflags
	// variable in whichever main built this process, and core/ has no
	// way to read one.
	version string

	// runsBlocked is the fail-closed gate a FAILED reconciliation
	// leaves behind (WorkflowReconcileGate).
	//
	// Atomic rather than guarded by a mutex because it is read on the
	// path of every run submission and every scheduler tick, and written
	// only by a reconciliation pass. It lives here, on the per-process
	// workflow state, for the reason the engine does: a hot reload
	// replaces the configuration and must not clear a refusal about this
	// process's knowledge of the journal.
	runsBlocked atomic.Bool
}

// SetBuildVersion tells this service which release it is.
//
// A post-construction setter for EnableAlerts' reason: the value comes
// from the provider binary (an -ldflags variable in cmd/backupd or
// backupd-web), and threading it through Open would make every core/ test
// that opens a service state a version it has no opinion about.
//
// What it is FOR is the host runner's version check. The engine and the
// runner are one program cut in half by a socket
// (cmd/backupd/workflowrunner.go), so a runner built from a different
// tree is a mismatch nothing else could detect, and the runner refuses a
// hello whose version is not its own. A process that never calls this
// presents an empty version and is refused by that check, which is the
// correct outcome: it cannot prove it matches.
func (b *BackupService) SetBuildVersion(version string) {
	if b == nil || b.workflow == nil {
		return
	}

	b.workflow.version = version
}

// newWorkflowRuntime assembles the engine. Called from New, once.
func newWorkflowRuntime(b *BackupService) *workflowRuntime {
	rt := &workflowRuntime{metrics: &metrics.Workflow{}}

	rt.engine = &workflowrun.Engine{
		Store:    b.journal,
		Local:    localHookExecutor{svc: b},
		Remote:   remoteHookExecutor{svc: b},
		Logs:     &workflowrun.Broker{},
		Logger:   b.logger,
		Observer: workflowObserver{metrics: rt.metrics},
		Now:      now,
	}

	return rt
}

// runtime returns this service's workflow runtime, or nil.
func (b *BackupService) runtime() *workflowRuntime {
	if b == nil {
		return nil
	}

	return b.workflow
}

// WorkflowEngineReady reports whether this process has an engine that has
// reconciled, which is what every workflow surface needs before it can
// answer.
func (b *BackupService) WorkflowEngineReady() bool {
	rt := b.runtime()

	return rt != nil && rt.engine != nil
}

// WorkflowReconcileReport is what one startup pass found.
type WorkflowReconcileReport struct {
	// Interrupted names the runs this pass moved to recovery_required:
	// runs a previous process was in the middle of when it stopped.
	Interrupted []string

	// Holds is every outstanding hold after the pass, including ones
	// recorded before this process started. A non-empty list means at
	// least one backup set is refusing to run.
	Holds []WorkflowRecoveryHold
}

// ErrWorkflowReconcileIncomplete is the refusal every run action gets
// from a process whose workflow reconciliation did not succeed.
//
// It is what "fail closed" means here, and the reasoning is the same
// sentence read the other way round. The reconciliation is what decides
// which backup sets have an interrupted run whose cleanup nobody has
// accounted for; a run started before that answer exists is a backup
// taken over a machine that may still be quiesced. So the moment the
// answer is UNKNOWN is exactly the moment a run must not start, and a
// process that carried on serving with the lifecycle uninstalled would
// have disabled the guard precisely when it was needed.
//
// What it deliberately does NOT gate is reading. A deployment whose
// journal cannot be reconciled is a deployment an operator has to look
// at, and refusing to serve at all would take away the surface they
// would look at it from: the runs, the recovery holds, the health report
// and the resume/acknowledge actions all stay available, because they
// are how the gate gets lifted.
var ErrWorkflowReconcileIncomplete = errors.New("service: this process could not reconcile its workflow runs, so it refuses to start a backup: which backup sets are blocked by an interrupted run is unknown, and a run started now could be taken over a machine a previous run left quiesced")

// ReconcileWorkflows brings the journal into line with the fact that this
// process has just started, and must be called before this service serves
// anything.
//
// It is idempotent (workflowrun.Reconcile's own property), so a caller
// that is not sure whether it has run may call it again; what it must not
// do is skip it, because the engine refuses every run until it has.
//
// A FAILURE closes the run gate (ErrWorkflowReconcileIncomplete) for this
// process, and a later success opens it. That is why a caller may retry:
// the refusal is a state of this service, not of the call, so a process
// that reconciles successfully on a second attempt starts backing up
// again without a restart.
func (b *BackupService) ReconcileWorkflows(ctx context.Context) (WorkflowReconcileReport, error) {
	rt := b.runtime()
	if rt == nil || rt.engine == nil {
		return WorkflowReconcileReport{}, ErrWorkflowsNotWired
	}

	report, err := rt.engine.Reconcile(ctx, now())
	if err != nil {
		// Closed before the error is returned, so there is no window in
		// which a caller has been told the reconciliation failed and a
		// scheduler tick could still start a backup.
		rt.runsBlocked.Store(true)

		return WorkflowReconcileReport{}, fmt.Errorf("service: reconciling workflow runs: %w", err)
	}

	// The lifecycle is installed on the inner service only once the
	// reconciliation has succeeded, and that ordering is the whole point
	// of doing it here. An engine that refuses every Run with
	// ErrNotReconciled, wired into the cycle, would turn a reconciliation
	// failure into every backup in the deployment failing for a reason no
	// surface could explain.
	//
	// What used to follow from that was the opposite of safe: the
	// lifecycle stayed uninstalled and the cycle ran exactly as it did
	// before EPIC L -- no hooks AND no recovery check -- which is a
	// workflow-configured deployment quietly downgraded to unwrapped
	// backups at the one moment its recovery state was unknown. So the
	// gate above is what a failure produces now, and this line is what a
	// success produces.
	b.installWorkflowLifecycle()
	rt.runsBlocked.Store(false)

	return WorkflowReconcileReport{
		Interrupted: append([]string(nil), report.Interrupted...),
		Holds:       toRecoveryHolds(report.Holds),
	}, nil
}

// WorkflowReconcileGate is the refusal a run action must make, or nil.
//
// Every surface that can START a backup asks it: the two durable
// submissions (SubmitRunCycle, SubmitRunBackupSet), the scheduler's own
// tick, and the CLI execution verbs through their data-plane door. One
// question in one place, because a gate half of the callers consult is
// not a gate.
//
// A process that never reconciled at all is NOT gated, and that is
// deliberate rather than an oversight: every core/ test builds a service
// with New and never reconciles, as does every CLI verb that only reads,
// and gating those would turn "this process has no workflow engine
// running" into "this deployment cannot back up". The gate is about a
// reconciliation that was ATTEMPTED AND FAILED, which is the one case
// where this process knows it does not know.
func (b *BackupService) WorkflowReconcileGate() error {
	rt := b.runtime()
	if rt == nil {
		return nil
	}

	if rt.runsBlocked.Load() {
		return ErrWorkflowReconcileIncomplete
	}

	return nil
}

// BackupDataPlane is the internal/app.Service this BackupService wraps,
// for the commands that execute a cycle IN THEIR OWN PROCESS: cmd/backupd's
// `run`, `daemon` and `fetch`.
//
// # Why this seam exists at all
//
// Because those three used to build an app.Service directly, from a
// loaded configuration and a journal, and an app.Service built that way
// has no workflow lifecycle: the five-stage wrapper is installed by this
// package, on reconciliation, and nothing else can install it. So a
// CLI-triggered backup of a workflow-configured set created no run row,
// ran no hook, and -- the part that makes it a safety defect rather than
// a missing feature -- never consulted the recovery holds, so an
// ordinary `backupd run` proceeded over a source an interrupted hook had
// left quiesced.
//
// The alternative was for the CLI to submit its work to a serving engine,
// and that is not available: `run` and `fetch` are what an operator uses
// on a host with nothing serving at all, their output and exit status are
// pinned by FR-35, and a `backupd daemon` serves no HTTP to submit to.
// What they need is the identical service the engine builds, with the
// lifecycle really installed, which is what this returns.
//
// It returns internal/app's type, which apps/ cannot import
// (docs/EPIC-B-multi-nas.md §7.2) and cmd/backupd already does. That
// asymmetry is deliberate: a provider binary composes this package's own
// surfaces, and only the CLI drives a cycle in-process.
//
// It is emphatically NOT a way around this service's own guards. A caller
// that executes a cycle through the returned value must have reconciled
// first (ReconcileWorkflows) and must consult WorkflowReconcileGate, for
// the same reason the two durable submissions and the scheduler do; the
// CLI does both in one place, openBackupDataPlane.
func (b *BackupService) BackupDataPlane() *app.Service {
	st := b.state.Load()
	if st == nil {
		return nil
	}

	return st.inner
}

// installWorkflowLifecycle puts the lifecycle on the inner app.Service,
// for this configuration and for every one a hot reload adopts later.
func (b *BackupService) installWorkflowLifecycle() {
	st := b.state.Load()
	if st == nil || st.inner == nil {
		return
	}

	st.inner.Workflow = b.workflowLifecycle()
}

// workflowLifecycle is the app.WorkflowLifecycle this service installs.
func (b *BackupService) workflowLifecycle() app.WorkflowLifecycle {
	return &workflowLifecycle{svc: b}
}

// workflowRunOptions are the per-run decisions a surface makes that the
// lifecycle cannot work out for itself.
//
// They travel on the CONTEXT rather than in a parameter, because the
// lifecycle is invoked from inside internal/app's cycle loop and
// internal/app must not learn what a bypass is. That is the same
// mechanism the progress observer already rides on (app/progress.go), and
// it has the property that matters here: a caller that says nothing gets
// the safe answer, because the zero value of every field below is the
// conservative one.
type workflowRunOptions struct {
	// SkipScripts is #813's --skip-workflow-scripts.
	SkipScripts bool

	// Administrator says the surface has established that the caller may
	// take an administrator action.
	//
	// The two surfaces establish it differently and both are real. On a
	// terminal, running this binary against a deployment's configuration
	// and state directory IS the administrator boundary -- it is the same
	// authority `settings patch` and `backup-set create` already act on,
	// and there is no second one to check. Over HTTP, it is the
	// destructive gate (§13.3, #789): a bypass rides on POST
	// /api/v1/operations, which is already behind
	// requireDestructiveGate, so a deployment that has not proven its
	// trusted-proxy identity check cannot submit one at all.
	//
	// It is a field the caller sets rather than something inferred here,
	// because inferring it would mean this package deciding what
	// authority an HTTP session has, which is exactly the decision
	// capabilities.Authenticator and the gate exist to own.
	Administrator bool

	// Scheduled says this run came from the scheduler's tick rather than
	// from a person. A scheduled run may never bypass.
	Scheduled bool

	// Actor is who asked, for the audit record. Empty means the
	// scheduler.
	Actor string
}

type workflowOptionsKey struct{}

// withWorkflowRunOptions attaches per-run workflow decisions to ctx.
func withWorkflowRunOptions(ctx context.Context, o workflowRunOptions) context.Context {
	return context.WithValue(ctx, workflowOptionsKey{}, o)
}

func workflowRunOptionsFrom(ctx context.Context) workflowRunOptions {
	o, _ := ctx.Value(workflowOptionsKey{}).(workflowRunOptions)

	return o
}

// workflowLifecycle is the app.WorkflowLifecycle implementation: it turns
// one backup set's pass into one workflow run.
type workflowLifecycle struct {
	svc *BackupService
}

// AroundBackupSet runs backup inside set's five-stage workflow.
//
// The zero-plan case is not a special path here, and that is deliberate:
// workflowrun.Run already has one, and it is exactly two lines long (take
// the set lock, call the backup). Adding a second one here would mean a
// deployment with no hooks skipped the RECOVERY check as well, which is
// the one thing that must be asked of every run -- a set can be blocked
// by an interrupted run whose hooks were later removed from the
// configuration, and that set must still refuse to back up until somebody
// has looked at the machine.
func (l *workflowLifecycle) AroundBackupSet(ctx context.Context, set config.BackupSet, backup func(context.Context) error) error {
	rt := l.svc.runtime()
	if rt == nil || rt.engine == nil {
		return backup(ctx)
	}

	opts := workflowRunOptionsFrom(ctx)
	if err := l.svc.authorizeBypass(opts); err != nil {
		return err
	}

	plan, err := l.svc.snapshotWorkflow(set, opts)
	if err != nil {
		// A plan this deployment cannot build is a run that must not
		// start. #809's requirement is that a hook's validation fails
		// BEFORE the backup does anything, and this is that requirement
		// at the only moment it can be honoured: the backup has not been
		// called, so nothing has been quiesced and nothing has been
		// transferred.
		return err
	}
	// #809's "an invalid required hook fails before the first before
	// script", on the EXACT plan this run captured (workflowpreflight.go
	// holds the whole argument). Without it the six checks Snapshot
	// answers refused in time and the five executor checks did not: a
	// run whose second hook was a syntax error, or whose runner socket
	// was gone, or whose remote credential is an internal-sftp account,
	// ran the first hook and quiesced a database before finding out.
	//
	// A bypass skips it, and only a bypass. authorizeBypass above has
	// already established the authority for that, and a bypassed run
	// executes no hook at all, so there is nothing to be invalid: it
	// would be probing a runner for scripts nobody is going to send it.
	if !opts.SkipScripts {
		if err := l.svc.refuseUnrunnablePlan(ctx, plan); err != nil {
			return err
		}
	}

	res, runErr := rt.engine.Run(ctx, workflowrun.RunRequest{
		Plan:        plan,
		BackupSetID: set.ID,
		Backup:      backup,
		Facts:       workflowFacts(set),
		Bypassed:    opts.SkipScripts,
	})

	l.svc.recordWorkflowRun(ctx, set, res, opts)

	if runErr != nil {
		return runErr
	}

	// The backup's own error is returned unchanged, so the pass's caller
	// sees exactly what it would have seen with no workflow configured.
	// A failed CLEANUP is reported separately below, because #811's rule
	// is that it fails the run even when the backup succeeded, and a
	// caller that only looked at BackupErr would report that run as a
	// success.
	if res.BackupErr != nil {
		return res.BackupErr
	}

	// A backup the workflow PREVENTED. res.BackupErr is nil here because
	// the backup was never called -- a "before" hook failed, timed out or
	// was cancelled, and the engine recorded the backup as skipped -- so
	// without this the pass returns success to a cycle that took no
	// backup at all. That is the one outcome this seam must never
	// produce: internal/app would record the set with no error and no
	// artifacts, `backupd run` would exit 0, and the deployment would
	// report a healthy night on which nothing was backed up.
	//
	// The run's own state says which of the three it was, and the failed
	// step says where to look, so both are named rather than summarised.
	if res.BackupStatus == workflow.StatusSkipped {
		if res.FailedStep != "" {
			return fmt.Errorf("service: workflow run %s did not take a backup of %s: it stopped at %s and the backup was never started (run state %s)",
				res.RunID, set.ID, res.FailedStep, res.State)
		}

		return fmt.Errorf("service: workflow run %s did not take a backup of %s: the workflow ended before the backup could start (run state %s)",
			res.RunID, set.ID, res.State)
	}

	if res.RecoveryOutstanding {
		return fmt.Errorf("%w: workflow run %s left a cleanup scope that cannot be accounted for, and %s will refuse to run until it is resumed or acknowledged",
			workflowrun.ErrRecoveryRequired, res.RunID, set.ID)
	}

	if res.CleanupStatus == workflow.StatusFailed {
		return fmt.Errorf("service: the backup succeeded and workflow run %s could not complete its cleanup hooks; the source may be left as a \"before\" hook put it", res.RunID)
	}

	return nil
}

// authorizeBypass is the whole of --skip-workflow-scripts' authorization,
// in one place so no surface can reach the bypass past it.
//
// It deliberately does NOT check recovery_required. That refusal is
// structural and belongs to the engine: workflowrun.Run asks
// checkNotBlocked before it looks at anything else, including Bypassed,
// so a bypassed run of a blocked set is refused by the same code path an
// ordinary run is. Re-implementing it here would be a second answer to
// the one question #813 requires to be uncircumventable.
func (b *BackupService) authorizeBypass(o workflowRunOptions) error {
	if !o.SkipScripts {
		return nil
	}

	if o.Scheduled {
		return ErrWorkflowBypassOnScheduledRun
	}

	if !o.Administrator {
		return fmt.Errorf("%w: it is refused for a caller this surface has not established as one", ErrWorkflowBypassNotAuthorized)
	}

	return nil
}

// snapshotWorkflow captures the plan one run will execute.
//
// It returns the ZERO plan, and no error, for a set that configures no
// stages at all: that is workflowrun.Run's cheap path, and #811 requires
// a deployment with no hooks to pay nothing for this feature.
func (b *BackupService) snapshotWorkflow(set config.BackupSet, o workflowRunOptions) (workflow.Plan, error) {
	cfg := b.state.Load().inner.Config

	stages := cfg.WorkflowStagesFor(&set)
	if len(stages) == 0 {
		return workflow.Plan{}, nil
	}

	root, err := workflow.NewRoot(cfg.Workflows.Root)
	if err != nil {
		return workflow.Plan{}, err
	}

	spool := cfg.WorkflowSpoolDir()
	if spool == "" {
		return workflow.Plan{}, errors.New("service: this deployment configures workflow hooks and no state database, so there is nowhere to keep the captured copy of a run's scripts")
	}

	var execRef string
	if set.Workflow != nil {
		execRef = set.Workflow.RemoteExecConnectionRef
	}

	return workflow.Snapshot(workflow.SnapshotRequest{
		RunID:                   "wfr_" + uuid.New().String(),
		BackupSetID:             set.ID,
		Root:                    root,
		Stages:                  stages,
		Env:                     set.WorkflowEnvironment,
		Timeout:                 cfg.EffectiveScriptTimeout(&set),
		RemoteExecConnectionRef: execRef,
		MaxScriptSize:           cfg.EffectiveMaxScriptSize(),
		SpoolRoot:               spool,
	})
}

// workflowFacts are the three built-ins this product cannot derive inside
// internal/workflow: where the source is, and where its bytes land.
//
// A hook that unmounts a snapshot needs the path it was told to back up,
// and it needs it on a RECOVERY too, which is why these are persisted
// with the plan rather than injected per call (RunRequest.Facts' own
// doc). Nothing here is a credential: a host name, a remote path and a
// local path are the same three facts every log line about this set
// already carries.
func workflowFacts(set config.BackupSet) map[string]string {
	return map[string]string{
		workflow.EnvSourceHost:  set.Remote.Host,
		workflow.EnvSourcePath:  set.RemotePath,
		workflow.EnvDestination: set.LocalPath,
	}
}

// localHookExecutor dispatches a NAME.local.sh to the host runner,
// resolving the runner's address from this service's CURRENT
// configuration.
type localHookExecutor struct {
	svc *BackupService
}

func (e localHookExecutor) ExecuteStep(ctx context.Context, req workflowrun.StepRequest) (workflowrun.StepOutcome, error) {
	client, err := e.svc.hostRunnerClient()
	if err != nil {
		// An error return rather than a manufactured outcome: the engine
		// records it as DispositionNotAttempted with this sentence as
		// the detail, which is exactly what happened, and doing it there
		// keeps one place deciding what an unattemptable step looks like.
		return workflowrun.StepOutcome{}, err
	}

	return workflowrun.LocalExecutor{Client: client}.ExecuteStep(ctx, req)
}

// hostRunnerClient builds a client for the runner this deployment
// configured, reading the credential fresh.
//
// Fresh on every step, and not cached, because the credential is a FILE
// an installer rewrites: a reinstall that rotates the token would
// otherwise be a deployment whose hooks fail authentication until the
// daemon is restarted. Reading a small file per hook costs nothing beside
// running one.
func (b *BackupService) hostRunnerClient() (hostrunner.Client, error) {
	cfg := b.state.Load().inner.Config
	runner := cfg.Workflows.Runner

	if !runner.Configured() {
		return hostrunner.Client{}, errors.New("service: this deployment has local (NAME.local.sh) hook scripts and no host workflow runner configured; set workflows.runner.socket and workflows.runner.token_file to the socket and credential the installer created, as this process sees them")
	}

	token, err := hostrunner.LoadToken(runner.TokenFile)
	if err != nil {
		return hostrunner.Client{}, err
	}

	rt := b.runtime()
	version := ""
	if rt != nil {
		version = rt.version
	}

	return hostrunner.Client{
		SocketPath: runner.Socket,
		Version:    version,
		Token:      string(token),
	}, nil
}

// remoteHookExecutor dispatches a NAME.remote.sh over an exec-capable SSH
// connection, resolved per step against this service's current
// configuration.
type remoteHookExecutor struct {
	svc *BackupService
}

func (e remoteHookExecutor) ExecuteStep(ctx context.Context, req workflowrun.StepRequest) (workflowrun.StepOutcome, error) {
	return workflowrun.RemoteExecutor{Connect: e.svc.dialExecConnection}.ExecuteStep(ctx, req)
}

// dialExecConnection resolves a reference and opens the connection.
//
// remoteexec.Resolve is what decides what a reference MEANS -- a declared
// execution connection, or a backup set's own source connection (#810) --
// and it is called rather than re-implemented so a surface that validates
// a reference and a run that uses one cannot disagree about which one it
// found.
func (b *BackupService) dialExecConnection(ctx context.Context, ref string) (*remoteexec.Client, error) {
	conn, err := remoteexec.Resolve(b.state.Load().inner.Config, ref)
	if err != nil {
		return nil, err
	}

	return remoteexec.Dial(ctx, conn)
}

// workflowObserver translates the engine's own vocabulary into the
// exporter's label strings.
//
// The translation lives here, at the layer that imports both packages, so
// internal/metrics keeps importing no engine and internal/workflowrun
// keeps knowing nothing about Prometheus. The mapping is held against
// both vocabularies by a test in this package, which is what stops the
// two spellings drifting.
type workflowObserver struct {
	metrics *metrics.Workflow
}

func (o workflowObserver) ObserveWorkflowRun(r workflowrun.RunObservation) {
	o.metrics.ObserveRun(metrics.WorkflowRun{
		BackupSet: r.BackupSetID.String(),
		Status:    string(r.Status),
		Bypassed:  r.Bypassed,
		Duration:  r.Duration,
	})
}

func (o workflowObserver) ObserveWorkflowStep(s workflowrun.StepObservation) {
	o.metrics.ObserveStep(metrics.WorkflowStep{
		BackupSet:   s.BackupSetID.String(),
		Scope:       string(s.Scope),
		Phase:       string(s.Phase),
		Target:      string(s.Target),
		State:       string(s.State),
		Disposition: string(s.Disposition),
		Duration:    s.Duration,
	})
}

func (o workflowObserver) ObserveWorkflowLogTruncation(target workflow.Target) {
	o.metrics.ObserveLogTruncation(string(target))
}

// WorkflowMetrics renders this process's workflow counters as Prometheus
// text exposition format.
//
// It returns "" when no engine was ever built, rather than a set of
// families at zero, because zero would be a claim: "no workflow run has
// failed here" reads very differently from "this process does not run
// workflows", and a dashboard cannot tell them apart from the numbers.
func (b *BackupService) WorkflowMetrics() string {
	rt := b.runtime()
	if rt == nil || rt.metrics == nil {
		return ""
	}

	return rt.metrics.RenderWorkflow()
}

// SuspendedBackupSets names the backup sets refusing to run because a
// workflow run of theirs was interrupted and has not been dealt with.
//
// It is the read a scheduler makes before a tick and a surface makes
// before it offers a run button, and both get the same answer from the
// same place: two implementations of "is this set blocked" would
// eventually disagree, and the one that said no would start a backup over
// a quiesced machine.
func (b *BackupService) SuspendedBackupSets() []model.BackupSetID {
	rt := b.runtime()
	if rt == nil || rt.engine == nil {
		return nil
	}

	return rt.engine.SuspendedBackupSets()
}

// recordWorkflowRun is everything that happens to a finished run besides
// the journal: the audit line for a bypass, and the notification for an
// outcome somebody has to act on.
//
// The metrics are NOT here. They are taken inside the engine, through the
// Observer seam, because two of the seven families are facts only the
// engine can see (a truncated log, a remote step that never started) and
// splitting the seven across two layers would be two places to look for
// one answer.
func (b *BackupService) recordWorkflowRun(ctx context.Context, set config.BackupSet, res workflowrun.RunResult, o workflowRunOptions) {
	if res.RunID == "" {
		// A set with no hooks at all: no run row, nothing to report.
		return
	}

	if res.Bypassed {
		b.auditWorkflowBypass(ctx, set, res, o)
	}

	b.notifyWorkflowOutcome(ctx, set, res)
}

// auditWorkflowBypass records that somebody deliberately skipped a backup
// set's hooks.
//
// At LevelWarn with an explicit result, and named in the event stream, so
// it is impossible to miss: #813 requires a bypass to be "highly
// visible", and the failure mode it is guarding against is a deployment
// where somebody added the flag to a cron line months ago and everyone
// still believes the hooks run. A line at info would be exactly as
// invisible as that.
//
// It carries the actor and never a credential: the run row already
// records bypassed=1 durably, and this is the line an operator greps for.
func (b *BackupService) auditWorkflowBypass(ctx context.Context, set config.BackupSet, res workflowrun.RunResult, o workflowRunOptions) {
	if b.logger == nil {
		return
	}

	actor := o.Actor
	if actor == "" {
		actor = "unknown"
	}

	b.logger.WorkflowBypassed(ctx, res.RunID, set.ID.String(), actor, res.ScriptCount)
}

// toRecoveryHolds translates the engine's holds into this package's shape.
func toRecoveryHolds(holds []workflowrun.RecoveryHold) []WorkflowRecoveryHold {
	out := make([]WorkflowRecoveryHold, 0, len(holds))
	for _, h := range holds {
		out = append(out, WorkflowRecoveryHold{
			RunID:       h.RunID,
			BackupSetID: h.BackupSetID.String(),
			Scope:       string(h.Scope),
			EnteredAt:   h.EnteredAt,
			SpoolRef:    h.SpoolRef,
		})
	}

	return out
}

// workflowStepTimeout is the bound a preflight probe is given.
//
// Short, and much shorter than a hook's own timeout, because every use of
// it is a QUESTION rather than work: is the runner there, can this
// connection run a command, does bash parse these bytes. An operator
// running `backupd validate` is waiting at a terminal, and a validation
// that hangs for the length of a script timeout against an unreachable
// host is one nobody runs twice.
const workflowProbeTimeout = 20 * time.Second
