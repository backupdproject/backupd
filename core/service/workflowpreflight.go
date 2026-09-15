package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/retnd/retnd/core/internal/remoteexec"
	"github.com/retnd/retnd/core/internal/workflow"
)

// The executor half of a plan's checks, asked of ONE captured plan, by
// both of the two things that need the answer (#813).
//
// # Why it is factored out of validation
//
// #809's requirement is that an invalid REQUIRED hook fails before the
// first "before" script runs. Six of the fifteen validation items are
// answered by workflow.Snapshot itself, which a run performs too
// (snapshotWorkflow), so those six really do refuse before anything
// executes. The other five -- is there a runner, does it answer, does
// bash parse each local script, can the execution connection run a
// command at all, does the far side's bash parse each remote script --
// used to live only inside ValidateWorkflow, a separate verb an operator
// types. So a run whose SECOND hook was a syntax error, or whose runner
// socket was gone, or whose remote credential is an internal-sftp
// account that cannot exec, ran the FIRST hook and quiesced a database
// before finding out. That is exactly the failure #809 names.
//
// So the probes live here and both callers reach them through
// probePlanExecutors: the validation renders them as findings an operator
// reads, and a run treats the first failure as a refusal
// (refuseUnrunnablePlan). One implementation, so a validation that passes
// and a run that refuses cannot disagree about the same plan.
//
// # What may reach an interpreter
//
// The same rule validation always had, and it is why this is safe to do
// on the way into a run as well: nothing here executes a script body.
// The only two things that reach an interpreter are `bash -n` -- parse,
// never execute, on the runner's side through hostrunner.SyntaxCheck and
// on the far side through remoteexec.Preflight -- and remoteexec's own
// fixed capability probe, which is this product's script and never the
// operator's.
//
// # Why it probes the plan and not the configuration
//
// Because the plan is what will execute. A plan is captured bytes with a
// sha256 each, and a run re-verifies those hashes before it executes
// anything, so a syntax answer about the captured bytes is an answer
// about the bytes that will run. Re-reading the directory here would be
// asking about a file somebody may edit in between.

// workflowPlanFinding is one probe's answer about one plan.
//
// Three shapes, and the zero value is none of them: Err set is a
// refusal, Skipped set is "there was nothing of this kind to examine",
// and a plain Detail is a pass. That is the same three-way answer the
// validation report carries, which is what lets the translation in
// workflowvalidate.go be a mapping rather than a second classification.
type workflowPlanFinding struct {
	// Check is the WorkflowCheck* id this answer belongs to.
	Check string

	// Detail is the sentence an operator reads: the pass message, the
	// skip reason, or nothing when Err carries the words.
	Detail string

	// Err is the refusal, if this check failed.
	Err error

	// Skipped is set when nothing of this kind was examined. It is not
	// derived from Err == nil, because "passed" and "not examined" are
	// different facts and a green tick on a check that never ran is the
	// one output that would actively mislead.
	Skipped bool

	// Step is the step this answer is about, or nil when it is about the
	// runner or the connection rather than about one script.
	Step *workflow.Step
}

// probePlanExecutors asks this deployment's two executors whether they
// can run exactly this plan, and reports every answer to emit.
//
// emit returns false to stop probing, which is what makes this usable on
// the way into a run: a validation wants all five answers and a run wants
// to refuse at the first one that failed, without dialling the second
// executor to find a second reason for a run that is not going to happen.
func (b *BackupService) probePlanExecutors(ctx context.Context, plan workflow.Plan, emit func(workflowPlanFinding) bool) {
	if b.probeLocalPlan(ctx, plan, emit) {
		b.probeRemotePlan(ctx, plan, emit)
	}
}

// probeLocalPlan covers the host runner: is there one, does it answer,
// and does its bash parse each captured local script.
//
// It returns false when emit asked it to stop.
func (b *BackupService) probeLocalPlan(ctx context.Context, plan workflow.Plan, emit func(workflowPlanFinding) bool) bool {
	local := stepsWithTarget(plan, workflow.TargetLocal)

	if len(local) == 0 {
		return emit(workflowPlanFinding{
			Check: WorkflowCheckRunnerHealth, Skipped: true,
			Detail: "this backup set runs no local (NAME.local.sh) hooks, so it needs no host workflow runner",
		}) && emit(workflowPlanFinding{
			Check: WorkflowCheckLocalBashSyntax, Skipped: true,
			Detail: "this backup set runs no local (NAME.local.sh) hooks",
		})
	}

	client, err := b.hostRunnerClient()
	if err != nil {
		return emit(workflowPlanFinding{Check: WorkflowCheckRunnerHealth, Err: err}) &&
			emit(workflowPlanFinding{Check: WorkflowCheckLocalBashSyntax, Skipped: true, Detail: "not examined: there is no runner to ask"})
	}

	probeCtx, cancel := context.WithTimeout(ctx, workflowProbeTimeout)
	status, err := client.Status(probeCtx)
	cancel()

	if err != nil {
		return emit(workflowPlanFinding{Check: WorkflowCheckRunnerHealth, Err: err}) &&
			emit(workflowPlanFinding{Check: WorkflowCheckLocalBashSyntax, Skipped: true, Detail: "not examined: the runner did not answer"})
	}

	if !emit(workflowPlanFinding{Check: WorkflowCheckRunnerHealth, Detail: fmt.Sprintf(
		"the host workflow runner answered: version %s, bash %s, running as %s", status.Version, status.BashVersion, status.User)}) {
		return false
	}

	return probeLocalSyntax(ctx, plan, client, local, emit)
}

// probeLocalSyntax asks the runner to parse each local script's captured
// bytes.
//
// hostrunner.SyntaxCheck runs `bash -n` over the bytes and executes
// nothing, which is the runner's own documented guarantee and the only
// reason this is safe to do at all, let alone on the way into a run.
func probeLocalSyntax(ctx context.Context, plan workflow.Plan, client hostRunnerStatus, steps []workflow.Step, emit func(workflowPlanFinding) bool) bool {
	failures := 0

	for i := range steps {
		step := steps[i]

		script, err := plan.OpenScript(step.ID)
		if err != nil {
			failures++
			if !emit(workflowPlanFinding{Check: WorkflowCheckLocalBashSyntax, Err: err, Step: &step}) {
				return false
			}

			continue
		}

		probeCtx, cancel := context.WithTimeout(ctx, workflowProbeTimeout)
		err = client.SyntaxCheck(probeCtx, plan.RunID(), step.ID, script.Body)
		cancel()

		if err != nil {
			failures++
			if !emit(workflowPlanFinding{Check: WorkflowCheckLocalBashSyntax, Err: err, Step: &step}) {
				return false
			}
		}
	}

	if failures > 0 {
		return true
	}

	return emit(workflowPlanFinding{Check: WorkflowCheckLocalBashSyntax, Detail: fmt.Sprintf(
		"%d local hook(s) parse. Nothing was executed: the runner was asked to parse the bytes and run nothing", len(steps))})
}

// probeRemotePlan covers the three remote items: which connection was
// selected, whether it can actually run a command, and whether the remote
// bash parses each script.
func (b *BackupService) probeRemotePlan(ctx context.Context, plan workflow.Plan, emit func(workflowPlanFinding) bool) bool {
	remote := stepsWithTarget(plan, workflow.TargetRemote)

	if len(remote) == 0 {
		for _, check := range []string{WorkflowCheckExecConnection, WorkflowCheckExecCapability, WorkflowCheckRemoteBashSyntax} {
			if !emit(workflowPlanFinding{Check: check, Skipped: true, Detail: "this backup set runs no remote (NAME.remote.sh) hooks"}) {
				return false
			}
		}

		return true
	}

	cfg := b.state.Load().inner.Config
	ref := remote[0].ExecutionConnectionRef

	conn, err := remoteexec.Resolve(cfg, ref)
	if err != nil {
		return emit(workflowPlanFinding{Check: WorkflowCheckExecConnection, Err: err}) &&
			emit(workflowPlanFinding{Check: WorkflowCheckExecCapability, Skipped: true, Detail: "not examined: no execution connection resolved"}) &&
			emit(workflowPlanFinding{Check: WorkflowCheckRemoteBashSyntax, Skipped: true, Detail: "not examined: no execution connection resolved"})
	}

	if !emit(workflowPlanFinding{Check: WorkflowCheckExecConnection, Detail: fmt.Sprintf(
		"remote hooks run over execution connection %q as %s@%s", conn.Ref, conn.Source.User, conn.Source.Host)}) {
		return false
	}

	probeCtx, cancel := context.WithTimeout(ctx, workflowProbeTimeout)
	defer cancel()

	client, err := remoteexec.Dial(probeCtx, conn)
	if err != nil {
		return emit(workflowPlanFinding{Check: WorkflowCheckExecCapability, Err: err}) &&
			emit(workflowPlanFinding{Check: WorkflowCheckRemoteBashSyntax, Skipped: true, Detail: "not examined: the execution connection did not open"})
	}
	defer client.Close() //nolint:errcheck // a probe connection

	return probeRemoteCapability(probeCtx, plan, client, conn, remote, emit)
}

// probeRemoteCapability proves the far side can run a command at all, and
// that it can parse each script.
//
// Both come out of one call per script: remoteexec.Preflight runs this
// product's own fixed probe -- not the operator's hook -- and then a
// `bash -n` of the script's bytes, and it refuses a forced-command
// account that merely ACCEPTS an exec request and runs its own program.
// That refusal is the reason the capability answer exists at all: the
// recommended posture for a backup source is an account confined to
// internal-sftp, which authenticates perfectly and cannot run a hook, and
// a deployment that only discovered this at 2am is exactly what #810
// exists to prevent.
func probeRemoteCapability(ctx context.Context, plan workflow.Plan, client *remoteexec.Client, conn remoteexec.Connection, steps []workflow.Step, emit func(workflowPlanFinding) bool) bool {
	capable := false
	syntaxFailures := 0

	for i := range steps {
		step := steps[i]

		script, err := plan.OpenScript(step.ID)
		if err != nil {
			syntaxFailures++
			if !emit(workflowPlanFinding{Check: WorkflowCheckRemoteBashSyntax, Err: err, Step: &step}) {
				return false
			}

			continue
		}

		if _, err := client.Preflight(ctx, script.Body); err != nil {
			// A capability refusal is about the CONNECTION and a syntax
			// refusal is about the SCRIPT, and #813 lists them as two
			// items because the remedies are unrelated: one is an
			// account or an sshd configuration, the other is a typo in
			// somebody's hook. remoteexec exports a sentinel for the
			// first, which is what tells them apart here rather than a
			// reading of the sentence.
			if errors.Is(err, remoteexec.ErrExecCapability) {
				return emit(workflowPlanFinding{Check: WorkflowCheckExecCapability, Err: err}) &&
					emit(workflowPlanFinding{Check: WorkflowCheckRemoteBashSyntax, Skipped: true,
						Detail: "not examined: this connection cannot run a command, so nothing on it could parse a script"})
			}

			capable = true
			syntaxFailures++
			if !emit(workflowPlanFinding{Check: WorkflowCheckRemoteBashSyntax, Err: err, Step: &step}) {
				return false
			}

			continue
		}

		capable = true
	}

	if capable {
		if !emit(workflowPlanFinding{Check: WorkflowCheckExecCapability, Detail: fmt.Sprintf(
			"%s@%s accepted an exec channel and proved it runs the bytes it is sent rather than a program of its own", conn.Source.User, conn.Source.Host)}) {
			return false
		}
	}

	if syntaxFailures > 0 {
		return true
	}

	return emit(workflowPlanFinding{Check: WorkflowCheckRemoteBashSyntax, Detail: fmt.Sprintf(
		"%d remote hook(s) parse with %s on the far side. Nothing was executed: each script was sent to bash -n", len(steps), conn.Bash())})
}

// refuseUnrunnablePlan is #809's "an invalid required hook fails before
// the first before script", applied to the plan a run has just captured.
//
// It returns the FIRST failure and stops there. A run that is not going
// to happen does not need a second reason, and the first one is the one
// an operator has to act on -- the same rule
// workflowValidator.attributeSnapshotFailure follows for the six checks
// Snapshot answers.
//
// The whole pass is bounded as well as each probe, for the reason
// workflowValidationDeadline exists: a set with several remote hooks
// against a host that black-holes packets would otherwise add up probe
// timeouts in front of a backup. A deadline reached is itself a refusal,
// which is the conservative answer -- this product could not establish
// that the hooks will run, so it does not quiesce anything.
func (b *BackupService) refuseUnrunnablePlan(ctx context.Context, plan workflow.Plan) error {
	if plan.IsZero() || len(plan.Steps()) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, workflowPreflightDeadline)
	defer cancel()

	var refusal error
	b.probePlanExecutors(ctx, plan, func(f workflowPlanFinding) bool {
		if f.Err == nil {
			return true
		}

		if f.Step != nil {
			refusal = fmt.Errorf("service: this backup set's workflow cannot run, so nothing was executed and no hook has touched the source: %s (%s): %w",
				f.Step.ScriptName, f.Check, f.Err)
		} else {
			refusal = fmt.Errorf("service: this backup set's workflow cannot run, so nothing was executed and no hook has touched the source: %s: %w",
				f.Check, f.Err)
		}

		return false
	})

	return refusal
}

// workflowPreflightDeadline bounds the preflight a run performs.
//
// Shorter than workflowValidationDeadline, because the caller is
// different: a validation is an operator waiting at a terminal for a
// complete report, and this is a backup about to start, where five
// minutes of probing in front of every pass would be five minutes added
// to a nightly cycle's critical path for a deployment whose runner is
// gone.
const workflowPreflightDeadline = 90 * time.Second
