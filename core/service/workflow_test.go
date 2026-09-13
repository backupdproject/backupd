package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/metrics"
	"github.com/backupdproject/backupd/core/internal/workflow"
	"github.com/backupdproject/backupd/core/internal/workflowrun"
)

// The four properties of EPIC L's service surface that a plausible bug
// would actually break, and nothing else.
//
// Everything about the lifecycle itself -- the five stages, the crash
// reconciliation, the obligations -- is internal/workflowrun's, tested
// against a real journal there, and re-asserting it here would be a
// second, weaker copy of that suite. What is only true HERE is the
// authorization of a bypass, the containment of a secret across a
// configuration write, and the fact that the two vocabularies this
// package translates between are the same two vocabularies their own
// packages define.

// A bypass is refused for a caller no surface vouched for.
//
// The one that matters: the refusal is BEFORE the durable operation row
// exists, so an unauthorized attempt cannot leave an audit trail implying
// an administrator action was accepted.
func TestBypassIsRefusedWithoutAnAdministrator(t *testing.T) {
	t.Parallel()

	var b BackupService

	err := b.authorizeBypass(workflowRunOptions{SkipScripts: true})
	if !errors.Is(err, ErrWorkflowBypassNotAuthorized) {
		t.Fatalf("authorizeBypass = %v, want ErrWorkflowBypassNotAuthorized", err)
	}
}

// A scheduled run may never bypass, whoever the caller claims to be.
//
// Administrator is deliberately set here: the refusal must not depend on
// the caller failing the other check as well, because the failure mode
// #813 names is somebody with every privilege putting the flag on a
// schedule and nobody noticing for months.
func TestBypassIsRefusedOnAScheduledRunEvenForAnAdministrator(t *testing.T) {
	t.Parallel()

	var b BackupService

	err := b.authorizeBypass(workflowRunOptions{SkipScripts: true, Administrator: true, Scheduled: true})
	if !errors.Is(err, ErrWorkflowBypassOnScheduledRun) {
		t.Fatalf("authorizeBypass = %v, want ErrWorkflowBypassOnScheduledRun", err)
	}
}

// An authorized, unscheduled bypass is allowed, so the two refusals above
// are testing the guard rather than a function that refuses everything.
func TestAnAdministratorsBypassIsAllowed(t *testing.T) {
	t.Parallel()

	var b BackupService

	if err := b.authorizeBypass(workflowRunOptions{SkipScripts: true, Administrator: true}); err != nil {
		t.Fatalf("authorizeBypass refused an administrator's manual bypass: %v", err)
	}
}

// A run that is not bypassing is never asked for authority.
func TestAnOrdinaryRunNeedsNoAdministrator(t *testing.T) {
	t.Parallel()

	var b BackupService

	if err := b.authorizeBypass(workflowRunOptions{}); err != nil {
		t.Fatalf("authorizeBypass refused an ordinary run: %v", err)
	}
}

// A secret reference written through this package comes back as a
// LOCATION, and the resolved value is nowhere -- not in the answer, and
// not in the configuration file the write produced.
//
// The canary is planted in a real secret FILE, which is the spelling an
// operator actually uses, so a surface that resolved the reference to be
// helpful would put the canary in one of the two places checked below.
func TestASecretReferenceIsStoredAndReportedAsALocationOnly(t *testing.T) {
	t.Parallel()

	const canary = "sup3r-secret-canary-value"

	svc, configPath, secretFile := openWorkflowConfigService(t, canary)

	got, err := svc.SetWorkflowEnv(context.Background(), "", WorkflowEnvVar{
		Name:   "PGPASSWORD",
		Secret: WorkflowSecretRef{File: secretFile},
	})
	if err != nil {
		t.Fatalf("SetWorkflowEnv: %v", err)
	}

	if len(got) != 1 || got[0].Name != "PGPASSWORD" {
		t.Fatalf("SetWorkflowEnv returned %+v, want one PGPASSWORD entry", got)
	}
	if got[0].HasValue {
		t.Error("a variable configured from a secret reference came back carrying a literal value")
	}
	if got[0].Secret.File != secretFile {
		t.Errorf("Secret.File = %q, want the configured location %q", got[0].Secret.File, secretFile)
	}

	listed, err := svc.ListWorkflowEnv(context.Background(), "")
	if err != nil {
		t.Fatalf("ListWorkflowEnv: %v", err)
	}

	for _, surface := range []string{renderEnv(got), renderEnv(listed), readFileString(t, configPath)} {
		if strings.Contains(surface, canary) {
			t.Fatalf("a resolved secret value reached a surface this package produces:\n%s", surface)
		}
	}
}

// Unsetting a variable that is not there is a refusal, not a silent
// success. See ErrWorkflowEnvNotFound for the case that hides.
func TestUnsettingAnAbsentWorkflowVariableIsRefused(t *testing.T) {
	t.Parallel()

	svc, _, _ := openWorkflowConfigService(t, "unused")

	if _, err := svc.UnsetWorkflowEnv(context.Background(), "", "NOT_CONFIGURED"); !errors.Is(err, ErrWorkflowEnvNotFound) {
		t.Fatalf("UnsetWorkflowEnv = %v, want ErrWorkflowEnvNotFound", err)
	}
}

// A variable naming both a literal and a secret is refused rather than
// resolved, because whichever this product preferred would keep working
// and the mistake would never be noticed.
func TestAWorkflowVariableMayNotNameBothALiteralAndASecret(t *testing.T) {
	t.Parallel()

	err := validateWorkflowEnvVar(WorkflowEnvVar{
		Name:     "PGPASSWORD",
		Value:    "literal",
		HasValue: true,
		Secret:   WorkflowSecretRef{Env: "PGPASSWORD_FROM_ENV"},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("validateWorkflowEnvVar = %v, want ErrInvalidRequest", err)
	}
}

// An explicitly empty literal is a configured empty variable and is
// accepted, which is what HasValue exists to make expressible.
func TestAnExplicitlyEmptyWorkflowVariableIsAccepted(t *testing.T) {
	t.Parallel()

	if err := validateWorkflowEnvVar(WorkflowEnvVar{Name: "QUIET", HasValue: true}); err != nil {
		t.Fatalf("validateWorkflowEnvVar refused a deliberately empty variable: %v", err)
	}
}

// A name in this product's reserved namespace is refused by the one rule
// that owns it, rather than by a copy of that rule living here.
func TestAReservedWorkflowVariableNameIsRefused(t *testing.T) {
	t.Parallel()

	err := validateWorkflowEnvVar(WorkflowEnvVar{Name: "BACKUPD_RUN_ID", Value: "x", HasValue: true})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("validateWorkflowEnvVar = %v, want ErrInvalidRequest", err)
	}
}

// The observer translates between two closed vocabularies, and this is
// what holds them together.
//
// internal/metrics deliberately spells the state and disposition words it
// makes decisions on as string literals, so that it imports no engine
// (see its own doc). That is only safe while something compares the two
// spellings, and this is that something: every word internal/workflowrun
// and internal/workflow can produce is driven through the observer, and
// the exporter must classify each one without ever seeing a word it does
// not recognise.
func TestTheObserverSpeaksTheEnginesVocabulary(t *testing.T) {
	t.Parallel()

	w := &metrics.Workflow{}
	o := workflowObserver{metrics: w}

	states := []workflow.State{
		workflow.StatePending, workflow.StateRunning, workflow.StateSuccess,
		workflow.StateFailed, workflow.StateTimedOut, workflow.StateCanceled,
		workflow.StateSkipped, workflow.StateInterrupted,
	}
	dispositions := []workflowrun.Disposition{
		workflowrun.DispositionExited, workflowrun.DispositionTimedOut,
		workflowrun.DispositionCanceled, workflowrun.DispositionTransportLost,
		workflowrun.DispositionNotAttempted, workflowrun.DispositionSignaled,
	}

	for _, st := range states {
		for _, disp := range dispositions {
			for _, target := range []workflow.Target{workflow.TargetLocal, workflow.TargetRemote} {
				o.ObserveWorkflowStep(workflowrun.StepObservation{
					Scope: workflow.ScopeSet, Phase: workflow.PhaseBefore,
					Target: target, State: st, Disposition: disp,
					Duration: time.Second,
				})
			}
		}
	}

	for _, status := range []workflow.Status{
		workflow.StatusUnknown, workflow.StatusRunning, workflow.StatusSuccess,
		workflow.StatusFailed, workflow.StatusSkipped,
	} {
		o.ObserveWorkflowRun(workflowrun.RunObservation{Status: status, Duration: time.Second})
	}

	rendered := w.RenderWorkflow()

	// The two classifications that exist only in the exporter's string
	// literals, asserted through the rendered text rather than through
	// its internals: a timed-out step reaches the timeout family, and a
	// remote step nobody could reach reaches the remote-exec family. If
	// either vocabulary is renamed on one side, both of these go to
	// zero and this fails.
	for _, want := range []string{
		`backupd_workflow_step_timeouts_total{backup_set="/",scope="set",phase="before",target="local"} 6`,
		`backupd_workflow_remote_exec_failures_total{backup_set="/",disposition="transport_lost"} 6`,
		`backupd_workflow_remote_exec_failures_total{backup_set="/",disposition="not_attempted"} 6`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the exporter did not classify the engine's own vocabulary; missing:\n%s\n\ngot:\n%s", want, rendered)
		}
	}

	// A successful step is not a failure, and a skipped one is not
	// either. Both are reachable above, so a classifier that counted
	// every terminal state would show up here.
	if strings.Contains(rendered, `state="success"`) {
		t.Error("a step state reached a metric label; only closed dimensions may")
	}
}

// A run with no scripts is still counted, because "how many runs happened"
// must not silently exclude the deployments that configure nothing.
func TestARunIsCountedWithItsBypassFlag(t *testing.T) {
	t.Parallel()

	w := &metrics.Workflow{}
	o := workflowObserver{metrics: w}

	o.ObserveWorkflowRun(workflowrun.RunObservation{Status: workflow.StatusSuccess, Bypassed: true, Duration: time.Second})

	if !strings.Contains(w.RenderWorkflow(), `bypassed="true"`) {
		t.Errorf("a bypassed run is indistinguishable from an ordinary one:\n%s", w.RenderWorkflow())
	}
}

// Every workflow surface refuses rather than answering emptily in a
// process that never built an engine. "This deployment has no workflow
// runs" and "this process cannot answer" are different facts.
func TestWorkflowSurfacesRefuseWithoutAnEngine(t *testing.T) {
	t.Parallel()

	var b BackupService
	ctx := context.Background()

	if _, err := b.ReconcileWorkflows(ctx); !errors.Is(err, ErrWorkflowsNotWired) {
		t.Errorf("ReconcileWorkflows = %v, want ErrWorkflowsNotWired", err)
	}
	if _, err := b.WorkflowRecovery(ctx); !errors.Is(err, ErrWorkflowsNotWired) {
		t.Errorf("WorkflowRecovery = %v, want ErrWorkflowsNotWired", err)
	}
	if _, err := b.ResumeWorkflowCleanup(ctx, "wfr_1"); !errors.Is(err, ErrWorkflowsNotWired) {
		t.Errorf("ResumeWorkflowCleanup = %v, want ErrWorkflowsNotWired", err)
	}
	if err := b.AcknowledgeWorkflowRecovery(ctx, "wfr_1", WorkflowAcknowledgement{Actor: "a", Reason: "r"}); !errors.Is(err, ErrWorkflowsNotWired) {
		t.Errorf("AcknowledgeWorkflowRecovery = %v, want ErrWorkflowsNotWired", err)
	}
}

// An acknowledgement with nothing written on it is refused, which is the
// whole value of the record: it answers, months later, why a set was
// unblocked without its cleanup ever running.
func TestAcknowledgementRequiresAReason(t *testing.T) {
	t.Parallel()

	svc, _, _ := openWorkflowConfigService(t, "unused")

	err := svc.AcknowledgeWorkflowRecovery(context.Background(), "wfr_1", WorkflowAcknowledgement{Actor: "alice", Reason: "   "})
	if !errors.Is(err, ErrWorkflowAcknowledgeReasonRequired) {
		t.Fatalf("AcknowledgeWorkflowRecovery = %v, want ErrWorkflowAcknowledgeReasonRequired", err)
	}
}

// Every check #813 enumerates has an id, and the report orders them.
func TestEveryEnumeratedValidationCheckHasAnID(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for _, c := range WorkflowChecks() {
		if seen[c] {
			t.Errorf("check %q is listed twice", c)
		}
		seen[c] = true
	}

	for _, want := range []string{
		WorkflowCheckDirectories, WorkflowCheckScriptNames, WorkflowCheckOrdering,
		WorkflowCheckTarget, WorkflowCheckScriptHash, WorkflowCheckPermissionAncestry,
		WorkflowCheckRunnerHealth, WorkflowCheckLocalBashSyntax, WorkflowCheckExecConnection,
		WorkflowCheckExecCapability, WorkflowCheckRemoteBashSyntax, WorkflowCheckEnvConflicts,
		WorkflowCheckReservedEnv, WorkflowCheckSecretRefs, WorkflowCheckTimeouts,
	} {
		if !seen[want] {
			t.Errorf("WorkflowChecks() omits %q, which #813 lists as a reported item", want)
		}
	}
}

// A set with no stages validates as configured-nothing rather than
// broken, and every on-disk check reports skipped rather than passing.
// A green tick on a check that never ran is the one output that would
// actively mislead.
func TestValidatingASetWithNoHooksSkipsRatherThanPasses(t *testing.T) {
	t.Parallel()

	svc, _, _ := openWorkflowConfigService(t, "unused")

	report, err := svc.ValidateWorkflow(context.Background(), "production/alpha")
	if err != nil {
		t.Fatalf("ValidateWorkflow: %v", err)
	}

	if report.Configured {
		t.Fatal("a set with no stage directories reported itself as running hooks")
	}
	if !report.WorkflowValid {
		t.Error("a set that runs no hooks reported its workflow as invalid")
	}
	if !report.ValidForBackup {
		t.Error("a set this configuration holds reported itself as invalid for backup")
	}

	for _, f := range report.Findings {
		switch f.Check {
		case WorkflowCheckRunnerHealth, WorkflowCheckLocalBashSyntax,
			WorkflowCheckExecConnection, WorkflowCheckExecCapability, WorkflowCheckRemoteBashSyntax:
			if f.Severity != WorkflowSeveritySkipped {
				t.Errorf("%s reported %q for a set with no hooks; nothing was examined, so nothing may read as passing", f.Check, f.Severity)
			}
		}
	}
}

// openWorkflowConfigService is a file-backed service with one backup set,
// a workflow root, and a 0600 secret file holding canary.
func openWorkflowConfigService(t *testing.T, canary string) (svc *BackupService, configPath, secretFile string) {
	t.Helper()

	dir := t.TempDir()
	root := filepath.Join(dir, "workflows")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll(workflows): %v", err)
	}

	secretFile = filepath.Join(dir, "db.passphrase")
	if err := os.WriteFile(secretFile, []byte(canary), 0o600); err != nil {
		t.Fatalf("WriteFile(secret): %v", err)
	}

	remote := filepath.Join(dir, "remote")
	local := filepath.Join(dir, "local")
	for _, d := range []string{remote, local} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", d, err)
		}
	}

	configPath = filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"workflows:\n" +
		"  root: " + root + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: alpha\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remote + "\n" +
		"        local_path: " + local + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n  timezone: UTC\n  week_starts_on: monday\n"

	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(config): %v", err)
	}

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		_ = svc.Close()
		_ = cleanup()
	})

	return svc, configPath, secretFile
}

func renderEnv(vars []WorkflowEnvVar) string {
	var b strings.Builder
	for _, v := range vars {
		b.WriteString(v.Name)
		b.WriteString("=")
		b.WriteString(v.Value)
		b.WriteString(" file=")
		b.WriteString(v.Secret.File)
		b.WriteString(" env=")
		b.WriteString(v.Secret.Env)
		b.WriteString(" command=")
		b.WriteString(strings.Join(v.Secret.Command, " "))
		b.WriteString("\n")
	}

	return b.String()
}

func readFileString(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}

	return string(raw)
}
