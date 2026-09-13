package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/backupdproject/backupd/core/internal/config"
)

// Where EPIC L's five-stage lifecycle meets this package's two entry
// points into the artifact pipeline (#813).
//
// # Why a seam here rather than the engine calling in
//
// Because the lifecycle has to WRAP the backup, not follow it. #811's
// whole shape is "before" hooks, then the backup, then "after" hooks that
// are owed whatever happened in between, and the thing that has to be
// inside that sandwich is exactly one backup set's pass. There are two
// places in this package that run one -- the cycle's loop and Fetch --
// and both of them are here, so a hook cannot be configured and then
// silently not fire on the nightly cycle because only the manual path was
// wired.
//
// The seam is an interface and not a call into internal/workflowrun,
// deliberately, and the layering is the reason: workflowrun needs a
// journal, a host-runner client, a remote-exec dialer, a redactor and a
// deployment-wide lock table, all of which core/service already assembles
// and none of which this package should learn to. A nil Workflow is every
// deployment that configures no workflows and every test in this
// repository written before EPIC L, and it runs the identical code path
// those always ran.
//
// # What the wrapper may and may not decide
//
// It may decide that the backup does not happen: a "before" hook that
// failed, or a backup set with an unresolved interruption, both mean the
// pass must not start. In that case the result carries the refusal as its
// own Err, so it counts as a failed pass in exactly the way every other
// systemic failure does, and no surface has to learn a fourth outcome.
//
// It may NOT decide anything about the pass itself. Whatever
// processBackupSet or Fetch produced is what is reported, unchanged: the
// lifecycle's job is to run hooks around a backup and record what
// happened, and a wrapper that reinterpreted the backup's own verdict
// would be a second opinion about a cycle it did not run.

// WorkflowLifecycle runs one backup set's pass inside that set's
// workflow.
//
// backup is the pass. It is called at most once: not at all when the
// lifecycle refuses the run or a "before" hook fails, and exactly once
// otherwise. Its error is the BACKUP's own outcome, which the lifecycle
// records on the run's backup_status axis and hands to the "after" hooks;
// the lifecycle's own error return is a different fact -- this run could
// not happen, or its bookkeeping failed -- and is never a restatement of
// what backup returned.
type WorkflowLifecycle interface {
	AroundBackupSet(ctx context.Context, set config.BackupSet, backup func(context.Context) error) error
}

// errBackupSetPassFailed is what the wrapper reports to the lifecycle for
// a pass that ran and did not come out clean.
//
// A sentinel with no detail, because the detail is already recorded: the
// pass's own result carries every failed artifact, its reconcile findings
// and its discovery errors, and the surfaces that report a cycle read
// those. What the lifecycle needs from this is one bit -- did the backup
// succeed -- because that bit is what an "after" hook is told through
// BACKUPD_BACKUP_STATUS and what the run row records. Handing it a
// wrapped pipeline error instead would put an internal sentence in front
// of somebody's shell script.
var errBackupSetPassFailed = errors.New("app: this backup set's pass did not complete cleanly")

// runBackupSetInWorkflow runs one set's cycle share inside its workflow,
// or directly when no lifecycle is installed.
func (s *Service) runBackupSetInWorkflow(ctx context.Context, src config.Source, bs config.BackupSet) BackupSetCycleResult {
	if s.Workflow == nil {
		return s.processBackupSet(ctx, src, bs)
	}

	var result BackupSetCycleResult
	ran := false

	err := s.Workflow.AroundBackupSet(ctx, bs, func(ctx context.Context) error {
		ran = true
		result = s.processBackupSet(ctx, src, bs)

		return passOutcome(result)
	})

	return foldWorkflowRefusal(bs, result, ran, err)
}

// passOutcome is the one bit the lifecycle needs about a pass.
func passOutcome(r BackupSetCycleResult) error {
	if r.Outcome() == SetOutcomeOK {
		return nil
	}

	if r.Err != nil {
		return r.Err
	}

	return errBackupSetPassFailed
}

// foldWorkflowRefusal turns the lifecycle's own error into a pass result.
//
// The distinction it draws is the one an operator cares about. If the
// backup RAN, the pass's own result stands and the lifecycle's error is
// about the hooks -- an "after" hook that failed, a cleanup that could
// not be recorded -- so it is attached only when the pass had no error of
// its own to report, because the first error is the one somebody has to
// understand. If the backup did NOT run, there is no pass to report and
// the refusal is the whole story: a set blocked by an unresolved
// interruption, or a "before" hook that said no.
//
// # Why the pass sentinel is dropped rather than stored
//
// errBackupSetPassFailed is this package's own word to the lifecycle for
// "the pass ran and did not come out clean", and the engine hands it
// straight back as the run's BackupErr. A pass whose only fault is a
// counted artifact -- FailedArtifacts > 0, Err nil, which is a
// quarantine -- would otherwise come out of here carrying it as Err, and
// Err is what SystemicFailure reads: a cycle that completed and reported
// its counts would be classified as a cycle that could not be performed
// (core/service's cyclesummary_test.go pins the other half of that
// distinction). So a lifecycle error that is only this package's own
// restatement of the pass's verdict is discarded here, and Err is left
// for what it means -- the lifecycle could not run this pass, or could
// not finish what it wrapped it in. fetchInWorkflow excludes the same
// sentinel for the same reason, one entry point over.
func foldWorkflowRefusal(bs config.BackupSet, result BackupSetCycleResult, ran bool, err error) BackupSetCycleResult {
	if err == nil {
		return result
	}

	if !ran {
		return BackupSetCycleResult{Set: bs.ID, Err: err}
	}

	if errors.Is(err, errBackupSetPassFailed) {
		return result
	}

	if result.Err == nil {
		result.Err = err
	}

	return result
}

// fetchInWorkflow is runBackupSetInWorkflow for the other entry point.
//
// It is a separate function rather than the same one because Fetch's
// result type is its own and its refusal has nowhere to go but the error
// return: Fetch's caller is a command or an API request that is waiting
// for an answer, not a cycle report that collects one row per set.
func (s *Service) fetchInWorkflow(ctx context.Context, bs config.BackupSet, pass func(context.Context) (FetchResult, error)) (FetchResult, error) {
	if s.Workflow == nil {
		return pass(ctx)
	}

	var (
		result  FetchResult
		passErr error
		ran     bool
	)

	err := s.Workflow.AroundBackupSet(ctx, bs, func(ctx context.Context) error {
		ran = true
		result, passErr = pass(ctx)
		if passErr != nil {
			return passErr
		}

		if result.Outcome() != SetOutcomeOK {
			return errBackupSetPassFailed
		}

		return nil
	})

	switch {
	case passErr != nil:
		// The pass's own failure, reported unchanged. It happened first
		// and it is the one that says what went wrong.
		return result, passErr
	case err != nil && !ran:
		return FetchResult{Set: bs.ID}, err
	case err != nil && !errors.Is(err, errBackupSetPassFailed):
		// The backup ran and the workflow did not: an "after" hook
		// failed, or the lifecycle could not record what happened. The
		// result stands -- artifacts really were transferred -- and the
		// error says the run as a whole did not succeed, which is #811's
		// rule that a failed cleanup fails the run.
		return result, fmt.Errorf("app: fetch: %w", err)
	}

	return result, nil
}
