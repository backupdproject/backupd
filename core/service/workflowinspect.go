package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/retnd/retnd/core/internal/state"
	"github.com/retnd/retnd/core/internal/workflow"
	"github.com/retnd/retnd/core/internal/workflowrun"
)

// Reading a workflow run back: the run, its steps, and one step's
// captured output (#813).
//
// # The three statuses stay three
//
// Every shape in this file carries backup_status, cleanup_status and
// workflow_status as separate fields, and none of them derives one from
// the others. That is #810 and #811's rule and it is the whole reason the
// journal has three columns: "the backup succeeded and the cleanup did
// not" is the single most operationally important thing this feature can
// report -- it means a machine may be sitting quiesced with a good backup
// beside it -- and any surface that collapses the three into one verdict
// makes exactly that case unsayable.
//
// # Why the log read is a cursor and not a stream
//
// Because it has to work through whatever a NAS operator has in front of
// their deployment. service/liveactivity.go makes this argument at length
// for the activity feed and every word of it applies here: a held-open
// response is at the mercy of every buffering proxy and idle timeout
// between here and the client, and a design that only works when nothing
// in the path buffers is not a design.
//
// So a follower sends the last sequence it PROCESSED and gets what is
// newer, which is exactly the protocol workflowrun.Broker's own doc
// prescribes (the subscriber owns its cursor, because only the subscriber
// knows what it handled). Resume after a dropped connection is therefore
// not a special case at all: it is the ordinary read, with the cursor the
// follower already had.
//
// It has one thing the activity feed does not: an optional WAIT. A tail
// of a hook that prints a line every thirty seconds would otherwise be a
// poll loop choosing between latency and load. The wait is served from
// the engine's own broker, which is a non-blocking fan-out that can never
// slow a hook down, and it is BOUNDED so the response always arrives --
// an unbounded wait is a held-open response wearing a different name.
//
// # Authorization
//
// There is none in this file, deliberately, and that is not a gap. This
// is a method on a service; the surfaces above it are what authenticate.
// What matters for #813's "checked at establishment AND on replay" is
// that a log read is one ordinary request per page rather than a session
// that outlives its authorization: every page, including every resume
// after a drop, goes through the same authentication middleware the first
// one did. A long-lived stream is the shape that makes replay-time
// authorization a separate thing somebody has to remember; this one
// cannot have the bug.

// workflowLogWaitCeiling bounds how long one log read may wait for new
// output before answering with what it has.
//
// Five seconds: long enough that a quiet hook does not cost a request per
// second, short enough to be well inside every default proxy read timeout
// this product has met. A caller asking for longer gets this.
const workflowLogWaitCeiling = 5 * time.Second

// The bounds on one log page. Clamped rather than refused, the way every
// other paged read in this package is (ListActivity, LiveActivity): a
// client asking for a page should get a page rather than an argument
// about a number.
const (
	workflowLogDefaultLimit = 200
	workflowLogMaxLimit     = 2000
)

// WorkflowRunDetail is one run as every inspection surface reports it.
type WorkflowRunDetail struct {
	RunID       string
	BackupSetID string

	// State is the run's own lifecycle state (running, success, failed,
	// timed_out, canceled, recovery_required, cleanup_running,
	// cleanup_failed, recovered).
	State string

	// The three statuses, separate end to end. See this file's doc.
	BackupStatus   string
	CleanupStatus  string
	WorkflowStatus string

	// RecoveryState says whether this run is blocking its backup set.
	RecoveryState string

	// Bypassed says this run's hooks were deliberately skipped.
	Bypassed bool

	// FailedStep is the step id that broke the run -- the FIRST one,
	// because everything after it is a consequence -- and FailedScript
	// is that step's script name. Both empty for a run that did not
	// fail.
	FailedStep   string
	FailedScript string

	StartedAt  time.Time
	FinishedAt *time.Time

	// DurationMillis is how long the run took, or how long it has been
	// running so far when FinishedAt is nil.
	DurationMillis int64

	// ScriptCount is how many hook scripts the plan declared.
	ScriptCount int

	// Steps is every step in plan order. It is empty on a list read and
	// populated on a detail read; see WorkflowRuns.
	Steps []WorkflowStepDetail
}

// WorkflowStepDetail is one step of a run.
type WorkflowStepDetail struct {
	StepID     string
	Order      int
	ScriptName string

	// Scope, Phase and Target are the closed vocabularies that say WHERE
	// in the five stages this step sits and where it runs.
	Scope  string
	Phase  string
	Target string

	// ExecutionConnectionRef names the connection a remote step runs
	// over, and is empty for a local one.
	ExecutionConnectionRef string

	State string

	// ExitCode is nil unless a process exited and this product observed
	// the status. Nil and 0 are not the same answer, which is #810's
	// rule: transport loss is never reported as a known exit code.
	ExitCode *int

	// TerminationConfirmed says this product PROVED the process was gone
	// after asking it to stop. False on a step that ended by itself.
	TerminationConfirmed bool

	TimeoutMillis  int64
	StartedAt      *time.Time
	FinishedAt     *time.Time
	DurationMillis int64
}

// WorkflowStepLogRecord is one captured record of a step's output.
type WorkflowStepLogRecord struct {
	// Seq is the run-monotonic cursor. It never repeats within a run and
	// never starts at zero, so a follower's "everything after N" is
	// exact.
	Seq uint64

	StepID string

	// Stream is "stdout" or "stderr", kept apart end to end. A hook that
	// writes progress to one and errors to the other is telling an
	// operator something, and a merged log throws it away.
	Stream string

	// Kind distinguishes captured OUTPUT from this product's own
	// truncation marker. A client that rendered the marker as though the
	// hook had printed it would be attributing this product's sentence to
	// somebody's script.
	Kind string

	At time.Time

	// Text is the already-redacted payload. Every value resolved from a
	// secret reference for the step that produced it was layered onto the
	// deployment's redactor before a byte of this was written down
	// (workflowrun/logs.go), and the redaction is STREAMING, so a
	// credential straddling two reads off the pipe is still caught.
	Text string
}

// WorkflowStepLogRequest is one page of one step's output.
type WorkflowStepLogRequest struct {
	RunID  string
	StepID string

	// After is the last sequence the caller PROCESSED. Zero reads from
	// the beginning, which is the historical read.
	After uint64

	Limit int

	// Wait is how long to wait for output newer than After before
	// answering empty. Zero is the historical read and never blocks;
	// anything above workflowLogWaitCeiling is clamped to it.
	Wait time.Duration
}

// WorkflowStepLogPage is what one read returns.
type WorkflowStepLogPage struct {
	RunID  string
	StepID string

	Records []WorkflowStepLogRecord

	// Cursor is the sequence to send as After next time: the last record
	// in this page, or the caller's own After when the page is empty.
	// Echoed rather than left to the client to compute, so an empty page
	// does not make a follower forget where it was.
	Cursor uint64

	// Truncated says this step's recording stopped at the persisted-output
	// bound at some point up to Cursor. It is a property of the LOG and
	// not of this page, so it stays true on every page after the marker:
	// a follower that joined late must still be told the record is
	// incomplete.
	Truncated bool

	// Complete says the step has finished and this page reached the end
	// of its output, so a follower may stop. It is the only thing that
	// distinguishes "nothing new yet" from "there will never be anything
	// new", and without it a `--follow` of a finished step never exits.
	Complete bool

	// StepState is the step's state at the moment the page was read,
	// which is what a client renders beside the tail.
	StepState string
}

// WorkflowRuns lists a backup set's workflow runs, newest first.
//
// Steps are deliberately NOT populated: a list of thirty runs each
// carrying its steps is thirty journal queries for a page nobody reads
// the steps from. WorkflowRun is the detail read.
func (b *BackupService) WorkflowRuns(ctx context.Context, backupSetID string, limit int) ([]WorkflowRunDetail, error) {
	if b == nil || b.journal == nil {
		return nil, ErrWorkflowsNotWired
	}

	rows, err := b.journal.WorkflowRunHistory(ctx, strings.TrimSpace(backupSetID), limit)
	if err != nil {
		return nil, fmt.Errorf("service: reading workflow runs: %w", err)
	}

	out := make([]WorkflowRunDetail, 0, len(rows))
	for _, r := range rows {
		out = append(out, toWorkflowRunDetail(r, nil))
	}

	return out, nil
}

// WorkflowRun reads one run and every step of it.
func (b *BackupService) WorkflowRun(ctx context.Context, runID string) (WorkflowRunDetail, error) {
	if b == nil || b.journal == nil {
		return WorkflowRunDetail{}, ErrWorkflowsNotWired
	}
	if strings.TrimSpace(runID) == "" {
		return WorkflowRunDetail{}, fmt.Errorf("%w: reading a workflow run needs its id", ErrInvalidRequest)
	}

	run, err := b.journal.WorkflowRun(ctx, runID)
	if err != nil {
		return WorkflowRunDetail{}, translateWorkflowRunError(runID, err)
	}

	steps, err := b.journal.WorkflowSteps(ctx, runID)
	if err != nil {
		return WorkflowRunDetail{}, fmt.Errorf("service: reading the steps of workflow run %q: %w", runID, err)
	}

	return toWorkflowRunDetail(run, steps), nil
}

// WorkflowSteps reads one run's steps in plan order.
func (b *BackupService) WorkflowSteps(ctx context.Context, runID string) ([]WorkflowStepDetail, error) {
	detail, err := b.WorkflowRun(ctx, runID)
	if err != nil {
		return nil, err
	}

	return detail.Steps, nil
}

// WorkflowStepLogs reads one step's captured output from a cursor,
// optionally waiting for more.
//
// It is scoped to run_id AND step_id, which is #813's requirement and is
// also what makes the wait honest: the sequence counter is per RUN, so a
// page filtered to one step can be empty while the run's counter has
// moved, and a follower must not read that as the end of the output.
func (b *BackupService) WorkflowStepLogs(ctx context.Context, req WorkflowStepLogRequest) (WorkflowStepLogPage, error) {
	rt := b.runtime()
	if rt == nil || rt.engine == nil {
		return WorkflowStepLogPage{}, ErrWorkflowsNotWired
	}
	if strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.StepID) == "" {
		return WorkflowStepLogPage{}, fmt.Errorf("%w: a step log read names one run and one step", ErrInvalidRequest)
	}

	// The step is looked up FIRST, and its absence is a refusal rather
	// than an empty page. A run id and a step id that name nothing would
	// otherwise return "no output yet" forever, which is the answer a
	// typo deserves least: an operator watching an empty tail concludes
	// their hook produced nothing, not that they misspelled the step.
	step, err := b.findStep(ctx, req.RunID, req.StepID)
	if err != nil {
		return WorkflowStepLogPage{}, err
	}

	page, err := b.readStepLogPage(ctx, req, step)
	if err != nil {
		return WorkflowStepLogPage{}, err
	}

	if len(page.Records) > 0 || req.Wait <= 0 || page.Complete {
		return page, nil
	}

	return b.waitForStepLogs(ctx, req, step, page)
}

// readStepLogPage is one journal read, filtered to the step.
//
// The filtering is here rather than in the journal because the cursor is
// the RUN's: LogsAfter reads every record above the cursor for the whole
// run, and the page's own Cursor therefore has to advance past records
// belonging to other steps. A follower that only advanced past its own
// step's records would re-read the same window forever on a run whose
// later steps are noisy.
func (b *BackupService) readStepLogPage(ctx context.Context, req WorkflowStepLogRequest, step state.WorkflowStep) (WorkflowStepLogPage, error) {
	limit := req.Limit
	if limit <= 0 || limit > workflowLogMaxLimit {
		limit = workflowLogDefaultLimit
	}

	rt := b.runtime()

	records, err := rt.engine.LogsAfter(ctx, req.RunID, req.After, limit)
	if err != nil {
		return WorkflowStepLogPage{}, fmt.Errorf("service: reading the output of step %q: %w", req.StepID, err)
	}

	page := WorkflowStepLogPage{
		RunID:     req.RunID,
		StepID:    req.StepID,
		Cursor:    req.After,
		StepState: step.State,
	}

	for _, rec := range records {
		if rec.Seq > page.Cursor {
			page.Cursor = rec.Seq
		}
		if rec.StepID != req.StepID {
			continue
		}
		if rec.Kind == workflow.LogTruncated {
			page.Truncated = true
		}
		page.Records = append(page.Records, WorkflowStepLogRecord{
			Seq:    rec.Seq,
			StepID: rec.StepID,
			Stream: rec.Stream,
			Kind:   string(rec.Kind),
			At:     rec.CapturedAt,
			Text:   string(rec.Payload),
		})
	}

	// Truncation is a property of the whole log, so a follower that
	// joined after the marker still has to be told. Asked separately,
	// and only when this page did not already answer it, so the ordinary
	// page costs one query.
	if !page.Truncated {
		page.Truncated, err = b.stepLogWasTruncated(ctx, req.RunID, req.StepID, req.After)
		if err != nil {
			return WorkflowStepLogPage{}, err
		}
	}

	// Complete needs BOTH halves. A terminal step whose last records are
	// still above the cursor is not complete for this follower, and a
	// step with nothing new that is still running is not complete for
	// anybody.
	page.Complete = workflow.State(step.State).Terminal() && len(records) < limit

	return page, nil
}

// stepLogWasTruncated answers whether this step's recording stopped at
// the bound at any point up to cursor.
//
// It reads from zero, which is the only honest way to answer it: the
// marker sits at the position the recording stopped, so a follower that
// joined after it would never see it in its own pages. The read is
// bounded and only happens for a page that has not already found one.
func (b *BackupService) stepLogWasTruncated(ctx context.Context, runID, stepID string, upTo uint64) (bool, error) {
	if upTo == 0 {
		// The caller read from the beginning, so this page already saw
		// every record there is up to its own bound. Asking again would
		// re-read the same rows to learn the same thing.
		return false, nil
	}

	rt := b.runtime()

	records, err := rt.engine.LogsAfter(ctx, runID, 0, workflowLogMaxLimit)
	if err != nil {
		return false, fmt.Errorf("service: reading the output of step %q: %w", stepID, err)
	}

	for _, rec := range records {
		if rec.Seq > upTo {
			break
		}
		if rec.StepID == stepID && rec.Kind == workflow.LogTruncated {
			return true, nil
		}
	}

	return false, nil
}

// waitForStepLogs blocks, briefly, for output newer than the caller's
// cursor.
//
// It subscribes BEFORE deciding there is nothing new -- the caller's page
// read already happened, and a record written between that read and this
// subscription would otherwise be missed until the next poll. The
// subscription is only ever used as a WAKEUP: what it returns is
// discarded and the journal is read again, because the journal is the
// authority and a subscriber that fell behind has a hole in its live feed
// by construction (workflowrun/broker.go).
func (b *BackupService) waitForStepLogs(ctx context.Context, req WorkflowStepLogRequest, step state.WorkflowStep, empty WorkflowStepLogPage) (WorkflowStepLogPage, error) {
	wait := req.Wait
	if wait > workflowLogWaitCeiling {
		wait = workflowLogWaitCeiling
	}

	rt := b.runtime()

	sub := rt.engine.Logs.Subscribe(req.RunID, 0)
	defer sub.Close()

	// Re-read once under the subscription, closing the window between
	// the first read and Subscribe. Without it a hook that printed its
	// only line in that gap would keep a follower waiting the whole
	// ceiling for output that was already on disk.
	page, err := b.readStepLogPage(ctx, req, step)
	if err != nil {
		return WorkflowStepLogPage{}, err
	}
	if len(page.Records) > 0 || page.Complete {
		return page, nil
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			// The caller hung up or its deadline expired. The page it
			// would have got is the empty one, with its cursor
			// unchanged, and returning that rather than an error is what
			// makes a cancelled follow exit cleanly rather than printing
			// a failure at an operator who pressed Ctrl-C.
			return empty, nil
		case <-timer.C:
			return b.finalStepLogRead(ctx, req)
		case rec, ok := <-sub.Records():
			if !ok {
				return b.finalStepLogRead(ctx, req)
			}
			if rec.StepID != req.StepID || rec.Seq <= req.After {
				continue
			}

			return b.finalStepLogRead(ctx, req)
		}
	}
}

// finalStepLogRead re-reads the step and its page after a wait, so the
// answer reflects a step that finished while the follower was waiting.
func (b *BackupService) finalStepLogRead(ctx context.Context, req WorkflowStepLogRequest) (WorkflowStepLogPage, error) {
	step, err := b.findStep(ctx, req.RunID, req.StepID)
	if err != nil {
		return WorkflowStepLogPage{}, err
	}

	return b.readStepLogPage(ctx, req, step)
}

// ErrWorkflowStepNotFound is what a log read reports for a step id this
// run does not have.
var ErrWorkflowStepNotFound = errors.New("service: this workflow run has no step with that id")

func (b *BackupService) findStep(ctx context.Context, runID, stepID string) (state.WorkflowStep, error) {
	steps, err := b.journal.WorkflowSteps(ctx, runID)
	if err != nil {
		return state.WorkflowStep{}, fmt.Errorf("service: reading the steps of workflow run %q: %w", runID, err)
	}

	if len(steps) == 0 {
		// No steps at all means either a run this journal does not have
		// or one with no hooks. WorkflowRun is the call that tells them
		// apart, and it is only made on this path, so the ordinary read
		// still costs one query.
		if _, err := b.journal.WorkflowRun(ctx, runID); err != nil {
			return state.WorkflowStep{}, translateWorkflowRunError(runID, err)
		}
	}

	for _, s := range steps {
		if s.StepID == stepID {
			return s, nil
		}
	}

	return state.WorkflowStep{}, fmt.Errorf("%w: %s has no step %s", ErrWorkflowStepNotFound, runID, stepID)
}

// toWorkflowRunDetail translates a journal row, and its steps when the
// caller read them.
func toWorkflowRunDetail(run state.WorkflowRun, steps []state.WorkflowStep) WorkflowRunDetail {
	d := WorkflowRunDetail{
		RunID:          run.RunID,
		BackupSetID:    run.BackupSetID,
		State:          run.State,
		BackupStatus:   run.BackupStatus,
		CleanupStatus:  run.CleanupStatus,
		WorkflowStatus: run.WorkflowStatus,
		RecoveryState:  run.RecoveryState,
		Bypassed:       run.Bypassed,
		StartedAt:      run.StartedAt,
		FinishedAt:     run.FinishedAt,
		ScriptCount:    len(steps),
	}

	end := now()
	if run.FinishedAt != nil {
		end = *run.FinishedAt
	}
	d.DurationMillis = end.Sub(run.StartedAt).Milliseconds()

	for _, s := range steps {
		d.Steps = append(d.Steps, toWorkflowStepDetail(s))
	}

	// The failed step is the FIRST one in plan order that did not
	// succeed, which is the one an operator has to understand: every
	// step after it either did not run or ran in a state nobody
	// designed. A skipped step is not a failure -- the run moved past it
	// because something else already went wrong.
	for _, s := range d.Steps {
		if s.State == string(workflow.StateSuccess) || s.State == string(workflow.StateSkipped) || s.State == string(workflow.StatePending) {
			continue
		}
		d.FailedStep = s.StepID
		d.FailedScript = s.ScriptName

		break
	}

	return d
}

func toWorkflowStepDetail(s state.WorkflowStep) WorkflowStepDetail {
	d := WorkflowStepDetail{
		StepID:                 s.StepID,
		Order:                  s.Order,
		ScriptName:             s.ScriptName,
		Scope:                  s.Scope,
		Phase:                  s.Phase,
		Target:                 s.Target,
		ExecutionConnectionRef: s.ExecutionConnectionRef,
		State:                  s.State,
		ExitCode:               s.ExitCode,
		TerminationConfirmed:   s.TerminationConfirmed,
		TimeoutMillis:          s.Timeout.Milliseconds(),
		StartedAt:              s.StartedAt,
		FinishedAt:             s.FinishedAt,
	}

	// A duration only for a step that has both ends. A step that started
	// and has not finished reports none rather than "so far", because a
	// step list is read after the fact and a growing number in a settled
	// row reads as a measurement rather than as a step still running.
	if s.StartedAt != nil && s.FinishedAt != nil {
		d.DurationMillis = s.FinishedAt.Sub(*s.StartedAt).Milliseconds()
	}

	return d
}

// workflowStatusWords is internal/workflow's own status vocabulary, in
// the order the contract declares it.
//
// It exists so the wire enum and the engine's vocabulary are compared
// rather than both being written down and trusted, which is the same
// reason service.LiveActivityOutcomes exists for the other enum on the
// activity feed.
var workflowStatusWords = func() []string {
	statuses := []workflow.Status{
		workflow.StatusUnknown,
		workflow.StatusRunning,
		workflow.StatusSuccess,
		workflow.StatusFailed,
		workflow.StatusSkipped,
	}

	out := make([]string, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, string(s))
	}

	return out
}()

// WorkflowStatuses lists every status a run's three axes can hold.
func WorkflowStatuses() []string { return append([]string(nil), workflowStatusWords...) }

// WorkflowDispositions lists every disposition a step outcome can carry,
// which is the label vocabulary the failure metrics use.
func WorkflowDispositions() []string {
	return []string{
		string(workflowrun.DispositionExited),
		string(workflowrun.DispositionTimedOut),
		string(workflowrun.DispositionCanceled),
		string(workflowrun.DispositionTransportLost),
		string(workflowrun.DispositionNotAttempted),
		string(workflowrun.DispositionSignaled),
	}
}
