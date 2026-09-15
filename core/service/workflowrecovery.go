package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/retnd/retnd/core/internal/alert"
	"github.com/retnd/retnd/core/internal/config"
	"github.com/retnd/retnd/core/internal/state"
	"github.com/retnd/retnd/core/internal/workflow"
	"github.com/retnd/retnd/core/internal/workflowrun"
)

// The three things an operator can do about a workflow run this product
// could not finish (#813): look at it, resume its cleanup, or take
// responsibility for it by hand.
//
// # Why there is no fourth
//
// There is deliberately no "clear this" and no "ignore this". A run in
// recovery_required is one whose "after" hooks may never have run, which
// means a source machine may be sitting quiesced, mounted or paused right
// now. The only two honest exits are to RUN the cleanup that is owed, or
// for a person to state, in words that are recorded, that they have dealt
// with it themselves. internal/workflowrun enforces that from below --
// ResolveWorkflowRecovery refuses while any obligation is unsettled, and
// the acknowledgement's reason is required by the domain type -- and
// nothing here weakens it.
//
// # Why acknowledgement takes a reason and a person
//
// Because the whole value of the record is answering, six months later,
// why a backup set was unblocked without its cleanup ever running. An
// acknowledgement with no reason is a button somebody presses to make a
// warning go away, which is precisely the outcome the recovery axis
// exists to prevent.

// ErrWorkflowRunNotFound is what every workflow run read reports for an
// id this deployment's journal does not hold.
var ErrWorkflowRunNotFound = errors.New("service: no workflow run with that id")

// ErrWorkflowAcknowledgeReasonRequired is the refusal for an
// acknowledgement with nothing written on it.
//
// Its own sentinel rather than ErrInvalidRequest, because a client
// reading INVALID_REQUEST would tell an operator their request was
// malformed. It was not: it was complete and it was refused, and the
// remedy is a sentence rather than a different field.
var ErrWorkflowAcknowledgeReasonRequired = errors.New("service: acknowledging an interrupted workflow run requires a reason")

// WorkflowRecoveryHold is one reason a backup set is refusing to run.
type WorkflowRecoveryHold struct {
	RunID       string
	BackupSetID string

	// Scope is which nesting level's cleanup is outstanding: the
	// deployment's own hooks, or this backup set's.
	Scope string

	// EnteredAt is when that scope was entered, which is the answer to
	// the first question an operator asks: how long has this machine
	// been left like this.
	EnteredAt time.Time

	// SpoolRef is the run's retained copy of the scripts a
	// resume-cleanup would execute.
	//
	// A path, and deliberately reported: an operator deciding between
	// resuming and acknowledging wants to read the hooks first, and this
	// is where they are. It is a path this product created under its own
	// state directory, not a credential, and it is the same class of fact
	// every other surface in this product prints about where it keeps
	// things. It is emphatically NOT allowed anywhere near a metric
	// label; see internal/metrics/workflow.go for that rule.
	SpoolRef string
}

// WorkflowAcknowledgement is a person taking responsibility for a scope
// this product cannot account for.
type WorkflowAcknowledgement struct {
	// Actor is who is taking it. Required, and it is the surface's
	// answer rather than the caller's claim: the API takes it from the
	// authenticated session, the CLI from the account running the
	// binary.
	Actor string

	// Reason is what they did, or why it does not matter. Required.
	Reason string
}

// WorkflowRecovery returns every outstanding hold, sorted, which is what
// `workflow recovery show` and the health report are built from.
func (b *BackupService) WorkflowRecovery(_ context.Context) ([]WorkflowRecoveryHold, error) {
	rt := b.runtime()
	if rt == nil || rt.engine == nil {
		return nil, ErrWorkflowsNotWired
	}

	return toRecoveryHolds(rt.engine.RecoveryHolds()), nil
}

// ResumeWorkflowCleanup runs the eligible "after" stages of an
// interrupted run, out of that run's own captured bytes.
//
// Everything it executes comes from the journal and the spool, never from
// today's configuration: see workflowrun.ResumeCleanup, which re-verifies
// every script against the sha256 recorded at snapshot time. An operator
// who edited /workflows while the daemon was down has changed nothing
// about what this runs, which is the property that makes resuming safe
// after an unattended restart.
//
// It is a synchronous call and not a durable operation, unlike a backup.
// The reason is what it is FOR: an operator is sitting at a terminal or a
// dialog having just been told a machine may be left quiesced, and the
// answer they need is whether the cleanup worked. A queued operation
// would turn that into a poll, and the thing being unwound is bounded by
// the cleanup timeout anyway (workflowrun.DefaultCleanupTimeout), so
// there is no long-running work to survive a disconnect.
func (b *BackupService) ResumeWorkflowCleanup(ctx context.Context, runID string) (WorkflowRunDetail, error) {
	rt := b.runtime()
	if rt == nil || rt.engine == nil {
		return WorkflowRunDetail{}, ErrWorkflowsNotWired
	}
	if strings.TrimSpace(runID) == "" {
		return WorkflowRunDetail{}, fmt.Errorf("%w: resuming a cleanup needs the run id it belongs to", ErrInvalidRequest)
	}

	res, err := rt.engine.ResumeCleanup(ctx, runID)
	if err != nil {
		return WorkflowRunDetail{}, translateWorkflowRunError(runID, err)
	}

	// Read back rather than translating the in-memory result, so what a
	// caller is shown after a resume is the same row every other surface
	// reads. A resume that left the run still blocked has to say so from
	// the journal, because that is where the refusal an operator will
	// meet on their next run comes from.
	return b.WorkflowRun(ctx, res.RunID)
}

// AcknowledgeWorkflowRecovery records that an operator has dealt with an
// interrupted run by hand, and unblocks the backup set.
func (b *BackupService) AcknowledgeWorkflowRecovery(ctx context.Context, runID string, ack WorkflowAcknowledgement) error {
	rt := b.runtime()
	if rt == nil || rt.engine == nil {
		return ErrWorkflowsNotWired
	}
	if strings.TrimSpace(runID) == "" {
		return fmt.Errorf("%w: acknowledging a recovery needs the run id it belongs to", ErrInvalidRequest)
	}
	if strings.TrimSpace(ack.Reason) == "" {
		return ErrWorkflowAcknowledgeReasonRequired
	}
	if strings.TrimSpace(ack.Actor) == "" {
		return fmt.Errorf("%w: an acknowledgement records who took responsibility, and this surface did not say", ErrInvalidRequest)
	}

	if err := rt.engine.AcknowledgeRecovery(ctx, runID, workflowrun.Acknowledgement{
		By:     ack.Actor,
		Reason: ack.Reason,
	}); err != nil {
		return translateWorkflowRunError(runID, err)
	}

	return nil
}

// translateWorkflowRunError puts an engine refusal into this package's
// vocabulary.
//
// Only the "no such run" case is translated, and everything else is
// passed through unchanged. That asymmetry is deliberate: a missing run
// is a fact about the REQUEST that every client has to be able to tell
// apart (a 404, a different exit code), while the rest are the engine
// explaining a refusal in sentences written for an operator, and
// rewriting those here would replace the explanation with a worse one.
func translateWorkflowRunError(runID string, err error) error {
	if errors.Is(err, workflowrun.ErrRecoveryRequired) {
		return err
	}

	// The journal exports its own sentinel for a run it does not hold,
	// and it is matched with errors.Is rather than by the shape of the
	// message: a substring test on an operator sentence is a test that
	// passes until somebody improves the wording.
	if errors.Is(err, state.ErrWorkflowRunNotFound) {
		return fmt.Errorf("%w: %s", ErrWorkflowRunNotFound, runID)
	}

	return err
}

// notifyWorkflowOutcome raises whichever of #813's four outcome
// combinations this run landed in.
//
// The classification is internal/alert's, not this function's, for the
// reason every other condition builder in that package exists: the
// decision "which of these is worth telling somebody about" is one place,
// with a table test over all four combinations, rather than an if-chain
// at the one call site that happens to have the run in hand.
//
// A run that succeeded raises nothing, and raising nothing is what lets
// the next failure alert: the dispatcher forgets a condition it no longer
// sees, so a set that fails, is fixed, and fails again is two
// notifications rather than one.
func (b *BackupService) notifyWorkflowOutcome(ctx context.Context, set config.BackupSet, res workflowrun.RunResult) {
	conditions := alert.WorkflowConditions(alert.WorkflowRun{
		BackupSet:      set.ID.String(),
		RunID:          res.RunID,
		BackupStatus:   string(res.BackupStatus),
		CleanupStatus:  string(res.CleanupStatus),
		WorkflowStatus: string(res.WorkflowStatus),
		FailedStep:     res.FailedStep,
		FailedScript:   failedScriptName(res),
		Bypassed:       res.Bypassed,
	})

	if res.RecoveryOutstanding {
		conditions = append(conditions, alert.WorkflowRecoveryConditions([]alert.WorkflowRecoveryHoldSubject{{
			BackupSet: set.ID.String(),
			RunID:     res.RunID,
			Scope:     string(workflow.ScopeSet),
		}})...)
	}

	b.dispatchWorkflowConditions(ctx, conditions)
}

// failedScriptName is the BASENAME of the script that broke the run, or
// "".
//
// A basename and never a path, because this value reaches a notification
// that leaves the deployment. It is read off the run's own step results
// rather than re-derived from a path, so there is no path here to
// accidentally pass through.
func failedScriptName(res workflowrun.RunResult) string {
	if res.FailedStep == "" {
		return ""
	}

	for _, s := range res.Steps {
		if s.StepID == res.FailedStep {
			return s.ScriptName
		}
	}

	return ""
}

// dispatchWorkflowConditions hands conditions to whatever the provider
// installed, or drops them.
//
// Dropping is the correct behaviour for a deployment with no notifier: a
// condition nobody can be told about is not an error, and the run row,
// the event stream and the health report all still carry the fact. Every
// other alert path in this package makes the same choice (alerts.go).
func (b *BackupService) dispatchWorkflowConditions(ctx context.Context, conditions []alert.Condition) {
	if len(conditions) == 0 {
		return
	}

	st := b.state.Load()
	if st == nil || st.inner == nil || st.inner.Alerts == nil {
		return
	}

	st.inner.Alerts.Observe(ctx, conditions, nil, now())
}
