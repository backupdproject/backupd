package webhost

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/backupdproject/backupd/core/service"
)

// EPIC L's inspection and recovery surface (#813):
//
//	GET    /api/v1/workflow-runs
//	GET    /api/v1/workflow-runs/{run}
//	GET    /api/v1/workflow-runs/{run}/steps
//	GET    /api/v1/workflow-runs/{run}/steps/{step}/logs
//	GET    /api/v1/workflow-recovery
//	POST   /api/v1/workflow-recovery/{run}/resume-cleanup
//	POST   /api/v1/workflow-recovery/{run}/acknowledge
//
// # Why a workflow run is a top-level resource
//
// It is not a sub-resource of the backup set, even though every run has
// one, and the reason is the read an operator actually makes. A run that
// is holding a backup set is found by asking "what is stuck in this
// deployment", not by visiting each set in turn; and a run outlives the
// configuration that produced it, so a set whose configuration has been
// removed still has runs an operator needs to see. Hanging them off
// /backup-sets/{source}/{set} would make both of those reads impossible
// to spell. The backup-set filter is a query parameter on the list, which
// is the narrower question and the one a set's own page asks.
//
// # The three statuses stay three
//
// Every shape here carries backup_status, cleanup_status and
// workflow_status as separate fields and derives none of them from the
// others. That is the whole reason the journal has three columns: "the
// backup succeeded and the cleanup did not" is the single most
// operationally important thing this feature can report, because it means
// a machine may be sitting quiesced with a good backup beside it, and any
// surface that collapsed the three into one verdict would make exactly
// that case unsayable.
//
// # Why the log read is a cursor poll and not a stream, and why that is a
// # security property rather than only a transport one
//
// core/service/liveactivity.go makes the transport argument at length for
// the activity feed and every word of it applies here: this product is
// deployed on a NAS behind whatever reverse proxy the operator already
// had, and in the shipped two-container topology behind a second one of
// our own, so a held-open response has to survive every buffering and
// idle-timeout default in that path. A design that only works when
// nothing in the path buffers is not a design. So a follower sends the
// last sequence it PROCESSED and gets what is newer, and resume after a
// dropped connection is not a special case at all -- it is the ordinary
// read with the cursor the follower already had.
//
// The consequence that matters for #813 is that authorization on REPLAY
// is structural here rather than remembered. Every page, including every
// resume, is one ordinary request through the /api/v1 group, so it passes
// authMiddleware exactly as the first one did: a session that has since
// expired, been signed out, or had its cookie revoked is refused on its
// next page with the same 401 an unauthenticated establishment gets.
// There is no long-lived subscription holding an authorization decision
// taken minutes ago, which is the shape that makes replay-time
// authorization a separate thing somebody has to implement and then
// remember to keep. Both halves of that are driven by
// TestWorkflowStepLogs_AuthorizationIsRecheckedOnEveryCursorResume.
//
// The read is scoped to run_id AND step_id, which is #813's requirement
// and is also what makes the optional wait honest: the sequence counter
// is per RUN, so a page filtered to one step can legitimately be empty
// while the run's counter has moved, and `complete` rather than an empty
// page is what tells a follower it may stop.

// maxWorkflowRecoveryBodyBytes bounds the one body in this file.
//
// An acknowledgement carries a reason an operator typed, and a reason
// longer than this is not a reason -- it is a paste. Small on purpose,
// like the repository declaration's own bound.
const maxWorkflowRecoveryBodyBytes = 1 << 13 // 8 KiB

// workflowStepBody is one step of one run.
type workflowStepBody struct {
	DurationMs             int64  `json:"duration_ms"`
	ExecutionConnectionRef string `json:"execution_connection_ref"`
	// ExitCode is nil unless a process exited AND this product observed
	// the status. Nil and 0 are not the same answer, which is #810's
	// rule: transport loss is never reported as a known exit code,
	// because "the hook said it was fine" and "we never found out" are
	// the two answers an operator most needs to tell apart.
	ExitCode             *int   `json:"exit_code"`
	FinishedAt           string `json:"finished_at"`
	Order                int    `json:"order"`
	Phase                string `json:"phase"`
	Scope                string `json:"scope"`
	ScriptName           string `json:"script_name"`
	StartedAt            string `json:"started_at"`
	State                string `json:"state"`
	StepID               string `json:"step_id"`
	Target               string `json:"target"`
	TerminationConfirmed bool   `json:"termination_confirmed"`
	TimeoutMs            int64  `json:"timeout_ms"`
}

// workflowRunResponse is one run, with its steps on a detail read and
// without them on a list read.
type workflowRunResponse struct {
	BackupSetID    string             `json:"backup_set_id"`
	BackupStatus   string             `json:"backup_status"`
	Bypassed       bool               `json:"bypassed"`
	CleanupStatus  string             `json:"cleanup_status"`
	DurationMs     int64              `json:"duration_ms"`
	FailedScript   string             `json:"failed_script"`
	FailedStep     string             `json:"failed_step"`
	FinishedAt     string             `json:"finished_at"`
	RecoveryState  string             `json:"recovery_state"`
	RunID          string             `json:"run_id"`
	ScriptCount    int                `json:"script_count"`
	StartedAt      string             `json:"started_at"`
	State          string             `json:"state"`
	Steps          []workflowStepBody `json:"steps"`
	WorkflowStatus string             `json:"workflow_status"`
}

type listWorkflowRunsResponse struct {
	Runs []workflowRunResponse `json:"runs"`
}

type listWorkflowStepsResponse struct {
	RunID string             `json:"run_id"`
	Steps []workflowStepBody `json:"steps"`
}

// workflowStepLogRecordBody is one captured record.
//
// Text is already redacted where it matters: every value resolved from a
// secret reference for the step that produced it was layered onto the
// deployment's redactor before a byte of it was written down, and the
// redaction is streaming, so a credential straddling two reads off the
// pipe is still caught. Nothing in this package re-redacts, because a
// second redactor here would be a second answer to what a secret is.
type workflowStepLogRecordBody struct {
	At     string `json:"at"`
	Kind   string `json:"kind"`
	Seq    uint64 `json:"seq"`
	StepID string `json:"step_id"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

// workflowStepLogPageResponse is one page of one step's output.
type workflowStepLogPageResponse struct {
	Complete  bool                        `json:"complete"`
	Cursor    uint64                      `json:"cursor"`
	Records   []workflowStepLogRecordBody `json:"records"`
	RunID     string                      `json:"run_id"`
	StepID    string                      `json:"step_id"`
	StepState string                      `json:"step_state"`
	Truncated bool                        `json:"truncated"`
}

// workflowRecoveryHoldBody is one reason a backup set is refusing to run.
type workflowRecoveryHoldBody struct {
	BackupSetID string `json:"backup_set_id"`
	EnteredAt   string `json:"entered_at"`
	RunID       string `json:"run_id"`
	Scope       string `json:"scope"`
	SpoolRef    string `json:"spool_ref"`
}

type workflowRecoveryResponse struct {
	Holds []workflowRecoveryHoldBody `json:"holds"`
}

// workflowAcknowledgementRequest is the acknowledge body.
//
// There is no actor field and there will not be one: the actor is read
// from the authenticated session, because an acknowledgement records who
// took responsibility and a caller-supplied name would be a claim rather
// than an answer.
type workflowAcknowledgementRequest struct {
	Reason string `json:"reason"`
}

// listWorkflowRuns is GET /api/v1/workflow-runs. Read-only (§50).
//
// Both query parameters are advisory in exactly the way GET /activity's
// limit already is: absent, unparseable or non-positive means the
// engine's own answer. A client asking for a page should get a page
// rather than an argument about a number, and blanking a panel an
// operator went to look at over a query string is the one thing these
// reads exist not to do.
//
// backup_set is NOT refused for an id this deployment does not configure,
// which is the opposite of GET /activity/live's choice one file over, and
// the difference is the point: a run outlives the configuration that
// produced it, so "this set is gone and here is what it did" is a real
// and useful answer, while an empty live feed for a set that does not
// exist reads exactly like a quiet set.
func (h *handlers) listWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := h.backend.WorkflowRuns(r.Context(), r.URL.Query().Get("backup_set"), positiveQueryInt(r, "limit"))
	if err != nil {
		h.writeWorkflowRunError(w, r, err, "failed to read this deployment's workflow runs")

		return
	}

	resp := listWorkflowRunsResponse{Runs: make([]workflowRunResponse, 0, len(runs))}
	for _, run := range runs {
		resp.Runs = append(resp.Runs, toWorkflowRunBody(run))
	}

	writeJSON(w, http.StatusOK, resp)
}

// getWorkflowRun is GET /api/v1/workflow-runs/{run}. Read-only (§50).
func (h *handlers) getWorkflowRun(w http.ResponseWriter, r *http.Request) {
	run, err := h.backend.WorkflowRun(r.Context(), chi.URLParam(r, "run"))
	if err != nil {
		h.writeWorkflowRunError(w, r, err, "failed to read the workflow run")

		return
	}

	writeJSON(w, http.StatusOK, toWorkflowRunBody(run))
}

// listWorkflowRunSteps is GET /api/v1/workflow-runs/{run}/steps.
// Read-only (§50).
//
// Its own route rather than a client re-reading the whole run, because a
// client following a workflow polls the steps: the run's own row moves
// twice, at the start and at the end, and the steps are what change in
// between.
func (h *handlers) listWorkflowRunSteps(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run")

	steps, err := h.backend.WorkflowSteps(r.Context(), runID)
	if err != nil {
		h.writeWorkflowRunError(w, r, err, "failed to read the workflow run's steps")

		return
	}

	writeJSON(w, http.StatusOK, listWorkflowStepsResponse{
		RunID: runID,
		Steps: toWorkflowStepBodies(steps),
	})
}

// getWorkflowStepLogs is GET
// /api/v1/workflow-runs/{run}/steps/{step}/logs. Read-only (§50), so no
// CSRF and no destructive gate, and authenticated on every single page --
// see this file's doc for why that is the whole of this route's
// authorization design rather than a first line of it.
//
// The three query parameters are advisory in the same way the list's are,
// with one addition: `wait` is CLAMPED by core/service to its own ceiling
// rather than honoured as sent, because an unbounded wait is a held-open
// response wearing a different name.
func (h *handlers) getWorkflowStepLogs(w http.ResponseWriter, r *http.Request) {
	req := service.WorkflowStepLogRequest{
		RunID:  chi.URLParam(r, "run"),
		StepID: chi.URLParam(r, "step"),
		Limit:  positiveQueryInt(r, "limit"),
	}
	if raw := r.URL.Query().Get("after"); raw != "" {
		// Parsed as unsigned and ignored when it is not: the cursor is a
		// run-monotonic sequence that never starts at zero, so a
		// negative or malformed value cannot name a position and reading
		// from the beginning is the only honest fallback. Refusing
		// instead would turn a follower's own bad cursor into a blank
		// panel rather than into a re-read.
		if parsed, err := strconv.ParseUint(raw, 10, 64); err == nil {
			req.After = parsed
		}
	}
	if seconds := positiveQueryInt(r, "wait"); seconds > 0 {
		req.Wait = time.Duration(seconds) * time.Second
	}

	page, err := h.backend.WorkflowStepLogs(r.Context(), req)
	if err != nil {
		h.writeWorkflowRunError(w, r, err, "failed to read the workflow step's output")

		return
	}

	resp := workflowStepLogPageResponse{
		RunID:     page.RunID,
		StepID:    page.StepID,
		Cursor:    page.Cursor,
		Truncated: page.Truncated,
		Complete:  page.Complete,
		StepState: page.StepState,
		Records:   make([]workflowStepLogRecordBody, 0, len(page.Records)),
	}
	for _, rec := range page.Records {
		resp.Records = append(resp.Records, workflowStepLogRecordBody{
			Seq:    rec.Seq,
			StepID: rec.StepID,
			Stream: rec.Stream,
			Kind:   rec.Kind,
			At:     formatTime(rec.At),
			Text:   rec.Text,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// listWorkflowRecovery is GET /api/v1/workflow-recovery. Read-only (§50).
//
// This is the read an operator makes when a backup set has stopped
// backing up, so it answers from the engine's own hold set rather than by
// scanning the run list for rows that look blocking: the holds are what
// the next run will actually be refused against.
func (h *handlers) listWorkflowRecovery(w http.ResponseWriter, r *http.Request) {
	holds, err := h.backend.WorkflowRecovery(r.Context())
	if err != nil {
		h.writeWorkflowRunError(w, r, err, "failed to read this deployment's workflow recovery holds")

		return
	}

	resp := workflowRecoveryResponse{Holds: make([]workflowRecoveryHoldBody, 0, len(holds))}
	for _, hold := range holds {
		resp.Holds = append(resp.Holds, workflowRecoveryHoldBody{
			RunID:       hold.RunID,
			BackupSetID: hold.BackupSetID,
			Scope:       hold.Scope,
			EnteredAt:   formatTime(hold.EnteredAt),
			SpoolRef:    hold.SpoolRef,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// resumeWorkflowCleanup is POST
// /api/v1/workflow-recovery/{run}/resume-cleanup.
//
// CSRF and NOT the destructive gate, and this is the one route in EPIC L
// where that deserves an argument rather than a citation, because it
// EXECUTES operator-written code. It is exempt for three reasons that
// hold together:
//
// What it runs is not today's configuration. Every script comes out of
// the run's own retained spool and is re-verified against the sha256
// recorded when that run was planned, so nothing a caller sends and
// nothing an operator edited in the meantime decides what executes; the
// request names a run id and nothing else.
//
// What it runs is the cleanup that is already OWED. The alternative to
// this route is a source machine left quiesced, mounted or paused, which
// is a worse state than any this route can produce. Gating it would mean
// an operator who has not turned destructive operations on cannot unwind
// a hook that stopped their database -- the exact opposite of what the
// gate protects.
//
// It cannot reach backup data. It runs "after" hooks and moves a journal
// row; it deletes no artifact, no snapshot and no remote object, which is
// the class this gate stands in front of.
//
// The claim is recorded on the route in router.go and on the list that
// pins it (destructiveGateExemptRoutes, router_test.go).
func (h *handlers) resumeWorkflowCleanup(w http.ResponseWriter, r *http.Request) {
	run, err := h.backend.ResumeWorkflowCleanup(r.Context(), chi.URLParam(r, "run"))
	if err != nil {
		h.writeWorkflowRunError(w, r, err, "failed to resume the workflow run's cleanup")

		return
	}

	// The run as the journal now holds it, which core/service re-reads
	// rather than composing from the resume's in-memory result: a resume
	// that left the run still blocked has to say so from the row the
	// next backup will be refused against. So a 200 here means the
	// cleanup was attempted and the row was re-read, not that the
	// obligation is settled -- recovery_state is where that is said.
	writeJSON(w, http.StatusOK, toWorkflowRunBody(run))
}

// acknowledgeWorkflowRecovery is POST
// /api/v1/workflow-recovery/{run}/acknowledge: a person taking
// responsibility, in words that are recorded, for a scope this product
// cannot account for.
//
// Same tier as the resume beside it and for a narrower reason: this one
// executes nothing at all. It writes a recovery record and unblocks the
// backup set, and core/service refuses it while any cleanup obligation is
// still unsettled, so it is an acknowledgement of work somebody did
// rather than a way to dismiss work nobody did.
//
// The actor is taken from the authenticated session and never from the
// body. An acknowledgement whose actor the caller could choose would
// answer the question this record exists for -- who unblocked this, and
// why -- with whatever name the caller typed.
func (h *handlers) acknowledgeWorkflowRecovery(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkflowRecoveryBodyBytes)

	var body workflowAcknowledgementRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxWorkflowRecoveryBodyBytes)

		return
	}

	err := h.backend.AcknowledgeWorkflowRecovery(r.Context(), chi.URLParam(r, "run"), service.WorkflowAcknowledgement{
		Actor:  actorFromContext(r.Context()),
		Reason: body.Reason,
	})
	if err != nil {
		h.writeWorkflowRunError(w, r, err, "failed to acknowledge the workflow run")

		return
	}

	// 204, like every other write in this package that leaves no
	// resource to describe: what the operator wants to see afterwards is
	// the run, and GET /workflow-runs/{run} is where it is.
	w.WriteHeader(http.StatusNoContent)
}

// writeWorkflowRunError maps this file's refusals onto their declared
// statuses.
//
// The run and step 404s are separate codes rather than one, because the
// remedies differ: a missing run means the id is wrong or the row has
// been pruned, and a missing step means the run is there and a client is
// holding a stale step list. A follower polling logs meets the second one
// the moment a run is re-planned, and telling it the run does not exist
// would send it to the wrong screen.
//
// There is deliberately no arm for the engine's own refusals beyond
// these. core/service passes those through as the sentences it wrote for
// an operator, and this package cannot match on them: apps/ may not
// import core/internal, so an errors.Is against workflowrun's sentinels
// is not available here and a substring match on prose is a
// classification that silently stops working when somebody improves the
// wording. They land on the default arm, where the response keeps the
// correlation id and none of err, and the log gets err in full under that
// same id (#598).
func (h *handlers) writeWorkflowRunError(w http.ResponseWriter, r *http.Request, err error, internal string) {
	switch {
	case errors.Is(err, service.ErrWorkflowRunNotFound):
		h.logRefusal(r, http.StatusNotFound, "WORKFLOW_RUN_NOT_FOUND",
			writeError(w, http.StatusNotFound, "WORKFLOW_RUN_NOT_FOUND", err.Error()), err)
	case errors.Is(err, service.ErrWorkflowStepNotFound):
		h.logRefusal(r, http.StatusNotFound, "WORKFLOW_STEP_NOT_FOUND",
			writeError(w, http.StatusNotFound, "WORKFLOW_STEP_NOT_FOUND", err.Error()), err)
	case errors.Is(err, service.ErrWorkflowAcknowledgeReasonRequired):
		// Its own code rather than INVALID_REQUEST, on the sentinel's own
		// argument: a client reading INVALID_REQUEST would tell an
		// operator their request was malformed, and it was not. It was
		// complete and it was refused, and the remedy is a sentence
		// rather than a different field.
		h.logRefusal(r, http.StatusBadRequest, "WORKFLOW_ACKNOWLEDGEMENT_REASON_REQUIRED",
			writeError(w, http.StatusBadRequest, "WORKFLOW_ACKNOWLEDGEMENT_REASON_REQUIRED", err.Error()), err)
	case errors.Is(err, service.ErrWorkflowsNotWired):
		// The same code and the same 503 the configuration surface
		// answers with (writeWorkflowConfigError): this process cannot
		// tell you, and no change to the request will make it. It is
		// deliberately not an empty result -- "no backup set is being
		// held" and "this process cannot say whether one is" are
		// different answers, and reporting the first for the second would
		// tell an operator their interrupted run does not exist.
		h.logRefusal(r, http.StatusServiceUnavailable, "WORKFLOW_ENGINE_UNAVAILABLE",
			writeError(w, http.StatusServiceUnavailable, "WORKFLOW_ENGINE_UNAVAILABLE", err.Error()), err)
	case errors.Is(err, service.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	default:
		h.internalError(w, r, "INTERNAL", internal, err)
	}
}

// positiveQueryInt reads a count from the query string, reporting 0 for
// anything the route treats as absent: an unparseable value, a
// non-positive one, and no value at all are one request, and 0 is what
// core/service reads as "use your own default".
//
// One helper because this file needs it three times, and it NORMALISES
// rather than passing a number through, which is the one way it differs
// from the inline reading GET /activity performs (that route hands a
// negative limit to the backend and lets it clamp, which is the same
// request in effect and is pinned by a test of its own). Normalising here
// is what lets these three routes state their contract without reference
// to anybody's clamping.
//
// It is also the reading core/cliecho's positiveQuery performs on the
// other side of the wire, which is what makes the command echoed for one
// of these requests name the value the request actually made rather than
// a --limit 0 nobody sent.
func positiveQueryInt(r *http.Request, name string) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return 0
	}

	return parsed
}

// toWorkflowRunBody renders one run. Steps come through as an empty
// non-nil slice on a list read, so a client has one shape to iterate
// rather than two.
func toWorkflowRunBody(run service.WorkflowRunDetail) workflowRunResponse {
	return workflowRunResponse{
		RunID:          run.RunID,
		BackupSetID:    run.BackupSetID,
		State:          run.State,
		BackupStatus:   run.BackupStatus,
		CleanupStatus:  run.CleanupStatus,
		WorkflowStatus: run.WorkflowStatus,
		RecoveryState:  run.RecoveryState,
		Bypassed:       run.Bypassed,
		FailedStep:     run.FailedStep,
		FailedScript:   run.FailedScript,
		StartedAt:      formatTime(run.StartedAt),
		FinishedAt:     formatTimePtr(run.FinishedAt),
		DurationMs:     run.DurationMillis,
		ScriptCount:    run.ScriptCount,
		Steps:          toWorkflowStepBodies(run.Steps),
	}
}

func toWorkflowStepBodies(steps []service.WorkflowStepDetail) []workflowStepBody {
	out := make([]workflowStepBody, 0, len(steps))
	for _, s := range steps {
		out = append(out, workflowStepBody{
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
			TimeoutMs:              s.TimeoutMillis,
			StartedAt:              formatTimePtr(s.StartedAt),
			FinishedAt:             formatTimePtr(s.FinishedAt),
			DurationMs:             s.DurationMillis,
		})
	}

	return out
}
