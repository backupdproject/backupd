package main

import (
	"context"
	"fmt"
	"os"

	"github.com/backupdproject/backupd/core/cliecho"
	"github.com/backupdproject/backupd/core/internal/app"
	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/service"
)

// The door every subcommand that EXECUTES a backup comes through: `run`,
// `daemon` and `fetch` (#813).
//
// # Why they cannot come through openService
//
// openService builds an internal/app.Service directly, out of a loaded
// configuration and a journal, and an app.Service built that way has no
// workflow lifecycle at all: the five-stage wrapper is installed by
// core/service, on reconciliation, and nothing else can install it. So
// every backup this binary took of a workflow-configured backup set
// created no workflow run, ran no hook script, and never asked whether
// the set was blocked by an interrupted run -- which means an ordinary
// `backupd run` proceeded over a source that a hook interrupted at three
// in the morning had left quiesced, taking a backup of a stopped database
// and reporting it as a good one.
//
// That is the failure this file exists to make impossible. A backup
// executed by this binary now goes through the same BackupService the
// engine builds, reconciled, with the lifecycle really installed, so the
// hooks fire and the recovery holds are consulted by the same code the
// serving engine uses. There is no second implementation to disagree.
//
// # And why a serving engine is a refusal rather than a second executor
//
// Two processes cannot both hold this deployment's workflow state. The
// "is this set locked" guard that protects a live run is an in-memory
// lock table belonging to the process executing it
// (workflowrun.Engine.setIsLocked), and the recovery holds a run is
// refused against are in that same memory. So a second process that
// reconciled would mark the serving engine's in-flight run interrupted
// and block the set underneath a backup that is still running, and a
// second process that did NOT reconcile is the nil-lifecycle backup this
// file removes.
//
// Routing the work to that engine instead is not available: `run` and
// `fetch` are what an operator uses on a host with nothing serving, their
// output, their exit status and their --dry-run are pinned by FR-35, and
// a `backupd daemon` serves no HTTP to submit anything to. So the answer
// is the refusal every other beside-a-live-engine case in this binary
// gives, with the reason this one has, and exit 3 so a script can wait on
// it.
//
// `daemon` is the one command that does not probe, because it IS the
// serving process: it announces first (service.AnnounceServing, which
// refuses if another process got there) and then opens its data plane, so
// the announcement it is holding is its own.

// openBackupDataPlane opens the reconciled data plane for a one-shot
// execution verb, refusing when another process serves this deployment.
//
// The probe comes first, before the journal is opened, so a refused
// invocation has touched nothing at all.
func openBackupDataPlane(ctx context.Context, configPath string) (*app.Service, *config.Config, func(), error) {
	engine, err := detectRunningEngine(configPath)
	if err != nil {
		return nil, nil, func() {}, cannotTellRunError(err)
	}
	if engine != nil {
		return nil, nil, func() {}, dataPlaneRefusal(engine)
	}

	return openServingDataPlane(ctx, configPath)
}

// openServingDataPlane is openBackupDataPlane for `daemon`, which has
// already announced that it serves this deployment.
//
// The returned cleanup func closes the service (which closes the journal
// and releases its lock); callers should always `defer cleanup()`
// immediately.
func openServingDataPlane(ctx context.Context, configPath string) (*app.Service, *config.Config, func(), error) {
	svc, closeFn, err := service.Open(ctx, configPath)
	if err != nil {
		return nil, nil, func() {}, err
	}
	cleanup := func() {
		if err := closeFn(); err != nil {
			fmt.Fprintf(os.Stderr, cliecho.Binary+": closing the backup service: %v\n", err)
		}
	}

	// Before the reconciliation, because the reconciliation is the first
	// thing that can reach the host workflow runner, and the runner
	// refuses a hello whose version is not its own: a process that never
	// states a version is refused by that check for a reason no operator
	// could act on.
	svc.SetBuildVersion(version)

	// EPIC L's startup reconciliation, and a FAILURE here ends the
	// invocation. That is the fail-closed half of #813: the
	// reconciliation is what decides which sets are blocked by an
	// interrupted run, so a process that could not work that out must not
	// take a backup -- the alternative, which is what this binary used to
	// do implicitly, is a backup taken over a machine that may still be
	// quiesced, with the hooks silently not running either.
	report, err := svc.ReconcileWorkflows(ctx)
	if err != nil {
		cleanup()

		return nil, nil, func() {}, err
	}
	if len(report.Holds) > 0 {
		// Said once, here, rather than per set: the per-set refusal is
		// the lifecycle's and arrives with the set's own name, and this
		// is the line that explains why a cycle is about to refuse sets
		// an operator believes are healthy.
		fmt.Fprintf(os.Stderr, cliecho.Binary+": %d workflow cleanup(s) from an interrupted run are outstanding; the affected backup sets refuse to run until each is resumed or acknowledged (`%s workflow recovery show`)\n",
			len(report.Holds), cliecho.Binary)
	}

	inner := svc.BackupDataPlane()
	if inner == nil {
		cleanup()

		return nil, nil, func() {}, fmt.Errorf("%s: this deployment's backup service has no data plane, so nothing can be executed", cliecho.Binary)
	}

	return inner, inner.Config, cleanup, nil
}

// dataPlaneRefusal is what a one-shot execution verb says when something
// else is serving this deployment.
//
// It is engineHeld, so it exits 3 like every other refusal of that shape,
// and it names what a backup taken here would have skipped rather than
// only that it was refused: an operator who reads "refused" and nothing
// else has no way to tell this from a broken deployment.
func dataPlaneRefusal(engine *service.RunningEngine) error {
	return engineHeld{fmt.Errorf(
		"another process is already serving this deployment (state database %s), so nothing was backed up: a backup taken here would run outside that process's workflow lifecycle, so this backup set's hook scripts would not fire and the recovery holds that process holds in memory could not be consulted, and two processes backing up one deployment at once was never safe. Stop that process and run this command again; if it serves this deployment's Web UI or HTTP API, run the backup there instead",
		engine.StateDatabase)}
}

// cannotTellRunError is cannotTellError for an execution verb: the check
// could not be performed, so the command refuses rather than backing up
// as if nothing were running.
//
// A refusal rather than a shrug, for cannotTellError's own reason. The
// check does fail in production -- EACCES on a lock file owned by another
// uid, ENOTSUP where flock is unavailable, EIO on a sick volume -- and
// "I could not tell" and "nothing is running" are the same behaviour only
// if you are willing to take the backup anyway, which is the defect.
func cannotTellRunError(err error) error {
	return fmt.Errorf("cannot tell whether another process is already serving this deployment, and a backup taken here while one is would run outside that process's workflow lifecycle, so nothing was backed up: %w", err)
}
