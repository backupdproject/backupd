package service

import (
	"context"
	"fmt"
	"time"

	"github.com/retnd/retnd/core/internal/app"
	"github.com/retnd/retnd/core/internal/obs"
)

// This file is the unattended driver: the loop that keeps running cycles
// when nobody is asking it to, for a process that cannot reach the one
// internal/app already has.
//
// cmd/retnd's `daemon` gets that loop from
// internal/app.Service.Daemon directly. The generic Web host cannot, §7.2
// sees to that, and reimplementing it above this boundary would put the
// cadence, the single-flight decision and the panic policy in a package
// that can see none of the state they are about. So the loop lives here,
// on the inside of the seam, and apps/ drives it with a context and an
// interval.
//
// Nothing in this file queues. A tick that lands while an API-submitted
// operation holds the single-flight lock is dropped and logged, not
// buffered, because the work it would have done is the same work the next
// tick will do: backing up whatever is on disk now is not made more
// correct by having been asked for twice. Queueing would turn a slow
// afternoon into a backlog of identical passes that all still have to
// run.
//
// There are two timers here rather than one loop doing two jobs, and the
// alerting one deliberately does not take the single-flight lock. Its
// whole reason for existing is the case where a cycle has wedged, so a
// design where a wedged cycle silences the alerts about wedged cycles
// would be the one shape guaranteed not to work.
//
// Every goroutine started here recovers panics, and that is not defensive
// habit. This code shares a process with a persistent HTTP server: an
// unrecovered panic in a scheduled tick takes the API down with it, and a
// panic that escaped while holding the single-flight lock would leave
// every future tick and every future operator-submitted run waiting on a
// lock nothing will ever release.

// PollInterval reports the deployment-wide poll_interval this
// BackupService is currently running (config.Config.PollInterval), read
// off the live configuration so a settings save is reflected
// immediately rather than at the next restart.
//
// It is the DEPLOYMENT's default, not the loop's cadence: since issue
// #845 a backup set may poll on its own interval, so the loop sleeps to
// the earliest deadline across the sets (nextPollWake below) and decides
// per set what is due. Nothing drives a schedule off this value any
// more; it is reported for surfaces that want to show the operator what
// the default is.
func (b *BackupService) PollInterval() time.Duration {
	return b.state.Load().inner.Config.PollInterval.Duration()
}

// nextPollWake is how long the loop below sleeps before its next pass:
// the earliest moment any enabled backup set is due again
// (internal/app's NextPollWake), read from the live configuration and
// the live schedule on every pass.
//
// Deadlines rather than a fixed sleep of the tightest configured
// interval, for the reason NextPollWake's own doc gives: a fixed sleep
// rounds every other cadence up to a multiple of itself, so a set asking
// for 7 minutes under a 5 minute wake is polled every 10.
//
// Re-reading is the whole reason this is a method rather than a value
// captured when the loop started. "Editable in Settings" has to mean the
// cadence changes, and a loop holding the number it was born with would
// have answered 200 to the save and gone on ticking at the old rate.
func (b *BackupService) nextPollWake() time.Duration {
	return b.state.Load().inner.NextPollWake(now())
}

// notifyConfigChanged tells a sleeping loop that the configuration it
// computed its current sleep from is no longer the running one. It never
// blocks: see BackupService.configChanged.
func (b *BackupService) notifyConfigChanged() {
	select {
	case b.configChanged <- struct{}{}:
	default:
	}
}

// scheduleTimer is the loop's sleep, as a seam.
//
// A test asserting that a save reaches a SLEEPING loop has to be able to
// see a sleep abandoned, and the sleeps this loop takes are measured in
// minutes and hours: waiting one out is not an option, and shortening
// every fixture until it is would be testing a cadence no deployment
// runs.
var scheduleTimer = func(d time.Duration) (<-chan time.Time, func() bool) {
	timer := time.NewTimer(d)
	return timer.C, timer.Stop
}

// RunOnSchedule repeats one internal/app.Service.RunCycle pass until ctx
// is done, at the cadence the running configuration asks for — the same
// repeated-cycle shape
// internal/app.Service.Daemon already gives cmd/retnd's own
// `daemon` command — but reachable from apps/ (core/internal is not,
// docs/EPIC-B-multi-nas.md §7.2), which is what a process composing this
// BackupService with an HTTP API (the generic Web host's `serve` command,
// §9.2/§9.3) needs instead of reimplementing the loop against
// internal/app directly.
//
// # Shares BackupService's single-flight guard with SubmitRunCycle
//
// A process that runs this method AND serves POST /api/v1/operations
// (SubmitRunCycle, operations.go) against the same BackupService has two
// independent callers that can each want to run
// internal/app.Service.RunCycle at once: a scheduled tick here, and an
// operator-submitted run there. internal/app/cycle.go's own doc says two
// concurrent passes over the same backup set must never happen, and
// SubmitRunCycle already enforces that for its own callers via
// BackupService's runOnce mutex (see that method's doc). This method
// reuses the exact same mutex, via runScheduledCycle below, rather than
// running RunCycle unguarded: when a tick lands while runOnce is already
// held by an in-flight SubmitRunCycle, it skips that tick instead of
// running concurrently or blocking the loop, and simply tries again at
// the next interval. Skipping (not queueing or blocking) mirrors
// SubmitRunCycle's own ErrOperationAlreadyRunning choice: a scheduled
// tick that cannot run right now is not lost information worth blocking
// the loop over, it runs on the next tick instead.
//
// ctx governs the loop directly (unlike SubmitRunCycle's async execution,
// which deliberately uses BackupService's own longer-lived b.ctx instead
// of a caller's context — see that method's doc for why): RunOnSchedule
// has no shorter-lived caller to decouple from in the first place, it IS
// the top-level driver for as long as the process runs, exactly like
// internal/app.Service.Daemon is for the CLI's own `daemon` command. A
// caller (the `serve` command) is expected to pass the same
// signal-derived, process-shutdown context it also uses for the HTTP
// server's own graceful shutdown, which is what §9.3's "share ... a
// process shutdown context" actually means in practice: both halves stop
// because the same ctx was canceled, not because one tells the other to.
//
// # Where the cadence comes from (issue #845)
//
// It used to be an argument, and cannot be any more: a backup set may
// override the deployment-wide poll_interval, so the cadence is a
// property of the whole configuration rather than one number a caller
// could hold. Each pass is marked as a scheduled cycle so internal/app
// decides per set which ones are due, and the sleep between passes is
// the earliest of those sets' deadlines (nextPollWake above), recomputed
// every pass. A wake on which no set is due does nothing at all.
//
// # A save reaches a sleeping loop
//
// The sleep is abandoned when the configuration changes, rather than run
// out first. This is what the Settings page's "in effect now, with no
// restart" means for the cadence specifically: a deployment moved from
// daily to hourly has a loop sitting in a timer that is up to a day
// long, and waiting that out before honouring the save would make the
// page's sentence false for exactly as long as the old interval. So
// adoptConfig signals (configreload.go) and this loop recomputes.
func (b *BackupService) RunOnSchedule(ctx context.Context) error {
	if interval := b.PollInterval(); interval <= 0 {
		return fmt.Errorf("service: RunOnSchedule needs a positive poll_interval, got %s", interval)
	}

	// Work Package 3.5's alerting pass runs on its own timer beside this
	// loop, not only at the end of a cycle (see internal/app's AlertTick).
	// A cycle that wedges on one slow transfer never reaches the pass at
	// its end, and neither does a tick this loop skipped because an
	// API-submitted operation is stuck holding runOnce - both of which are
	// exactly "the manager is up and not producing backups", the situation
	// the stale alert exists to report. It runs at the DEPLOYMENT's
	// poll_interval rather than at this loop's wake interval: it reports
	// on the manager rather than on any one source, so one set asking to
	// be polled every minute is not a reason to rebuild a health report
	// every minute. It stops when ctx does; the deferred receive keeps
	// this method from returning while it is still running.
	alertsStopped := make(chan struct{})
	go func() {
		defer close(alertsStopped)
		b.runAlertTicks(ctx, b.PollInterval())
	}()
	defer func() { <-alertsStopped }()

	for {
		b.runScheduledCycle(ctx)

		if ctx.Err() != nil {
			return nil
		}

		if !b.sleepUntilDue(ctx) {
			return nil
		}
	}
}

// sleepUntilDue waits out the gap to the next due backup set, and reports
// false when ctx ended instead. A configuration change does not end the
// wait: it restarts it, against the configuration that just landed,
// because the sleep in hand was computed from one that is no longer
// running.
//
// Re-arming rather than returning is what keeps a settings page from
// driving cycles. The Web UI saves a box at a time, so a single visit is
// several configuration writes in a row, and a loop that ran a pass for
// each of them would turn editing a form into a burst of scheduled
// cycles. Nothing is lost by not running one here: if the new
// configuration has made a set due, the sleep this computes is the floor
// (NextPollWake's minimum) and the pass follows in seconds.
func (b *BackupService) sleepUntilDue(ctx context.Context) bool {
	for {
		fire, stop := scheduleTimer(b.nextPollWake())
		select {
		case <-ctx.Done():
			stop()
			return false
		case <-b.configChanged:
			stop()
		case <-fire:
			return true
		}
	}
}

// runScheduledCycle runs exactly one RunCycle pass, guarded by the same
// runOnce mutex executeRunCycle (operations.go) uses, so a tick that
// lands while an API-submitted operation is already running skips rather
// than overlapping it. Unlike executeRunCycle, there is no operation row
// to fail here: a skipped scheduled tick was never persisted as an
// operation in the first place (it is not caller-submitted work with an
// idempotency key to account for), it simply runs again at the next
// tick.
//
// # Panic recovery (issue #119's review, finding 5)
//
// executeRunCycle (operations.go) already recovers a panic inside RunCycle
// specifically because an unrecovered one there "would crash the entire
// persistent API server hosting this BackupService, not just one CLI
// invocation" (that method's own doc) - true here for exactly the same
// reason, and RunOnSchedule (this file) is what first makes THIS method
// share a process with that same persistent API server. The deferred
// recover below is declared AFTER b.runOnce.Unlock() specifically so it
// runs BEFORE it (defer is LIFO): a panic must release the single-flight
// lock the same way a normal return does, or a single panicking cycle
// would permanently wedge every future scheduled tick AND every future
// API-submitted operation behind a lock nothing will ever release again.
// This is deliberately NOT a bare catch-and-continue: the panic is logged
// at error level, loudly, exactly like executeRunCycle's own recovery
// does, rather than silently swallowed - running the next tick against
// state a panic just proved was unexpected is its own risk, and an
// operator watching logs needs to see that this happened, not just that
// the scheduler loop kept ticking as if nothing had.
func (b *BackupService) runScheduledCycle(ctx context.Context) {
	// Fail closed on EPIC L's reconciliation (#813), and before the
	// single-flight lock is taken rather than after it: a tick is the
	// path that runs unattended, so it is the one that would quietly
	// back up over a machine a previous run left quiesced if this
	// process never worked out which sets are blocked. There is no
	// operation row to fail here, so the log line is the record, and the
	// next tick tries again -- a later successful reconciliation opens
	// the gate without a restart.
	if err := b.WorkflowReconcileGate(); err != nil {
		b.logger.Error(ctx, "scheduled-cycle-workflow-gate", err)

		return
	}

	if !b.runOnce.TryLock() {
		b.logger.Event(ctx, obs.LevelInfo, "scheduled_cycle_skipped",
			"skipped scheduled run_cycle: an API-submitted operation is already in progress")
		return
	}
	defer b.runOnce.Unlock()
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error(context.Background(), "scheduled-cycle-panic", fmt.Errorf("recovered panic: %v", r))
		}
	}()

	// Rewrite the registered validators before anything execs one. A
	// scheduled tick is the path that runs unattended for weeks, so it is
	// the one most likely to meet a script that has been replaced or
	// reaped since the process started (validator.go's
	// refreshValidatorScripts). A failure skips this tick rather than
	// running it: a cycle that cannot guarantee its validators are the
	// scripts this build ships is a cycle that must not be allowed to pass
	// an artifact and authorize deleting the remote source. There is no
	// operation row to fail here, so the log line is the record, and the
	// next tick tries again.
	if err := b.refreshValidatorScripts(); err != nil {
		b.logger.Error(ctx, "scheduled-cycle-validator-scripts", err)
		return
	}

	// A scheduled tick reports its progress into cycleWatch (edithold.go)
	// and honours edit holds, exactly like an API-submitted one. Before
	// this, a scheduled tick installed no observer at all, so a transfer
	// the scheduler was running was invisible to every reader in this
	// process, and the scheduler is precisely what runs unattended,
	// which makes it the cycle an operator is most likely to be about to
	// interrupt.
	b.cycleWatch.begin()
	defer b.cycleWatch.end()
	// A scheduled tick feeds the live activity feed exactly as an
	// API-submitted one does (issue #573). It has to: the scheduler is
	// the path that runs unattended for weeks, so it is the cycle an
	// operator looking at a dashboard is most likely to be watching.
	b.activity.beginCycle()
	defer b.activity.endCycle()
	// WithScheduledCycle is what makes the per-set poll cadence real
	// (issue #845): internal/app processes only the sets whose own
	// interval has elapsed on a cycle marked this way, and every enabled
	// set on one that is not. An operator-submitted run (executeRunCycle,
	// operations.go) deliberately carries no such mark.
	// withWorkflowRunOptions marks the pass as SCHEDULED, which is what
	// makes #813's "never applied to scheduled runs" structural rather
	// than a rule somebody has to remember: the lifecycle refuses a
	// bypass on a run carrying this mark, so there is no combination of
	// configuration and request that could produce one here.
	runCycle(b.state.Load().inner,
		withWorkflowRunOptions(
			app.WithScheduledCycle(
				app.WithBackupSetHolds(
					app.WithProgressObserver(ctx, progressFanout{b.cycleWatch, b.activity}),
					b.holds)),
			workflowRunOptions{Scheduled: true}))
}

// runAlertTicks repeats one out-of-cycle alerting pass at interval until
// ctx is done, against whatever Service the latest configuration
// hot-reload left in place (b.state is re-read every tick, so a pass
// after a CreateBackupSet speaks about the new config, not the one this
// loop started with).
//
// It deliberately does NOT take runOnce. That lock exists so two passes
// never process the same backup set at once, and this pass processes
// nothing: it reads a health report and hands verdicts to the dispatcher,
// which is safe for concurrent use. Taking it would make this tick skip
// in exactly the case it was added for, a cycle that is stuck holding it.
func (b *BackupService) runAlertTicks(ctx context.Context, interval time.Duration) {
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		b.tickAlerts(ctx)
	}
}

// tickAlerts runs one pass, with the same panic recovery
// runScheduledCycle documents: this goroutine shares a process with a
// persistent API server, so an unrecovered panic here would take that
// server down, and an alerting problem must never be able to do that.
func (b *BackupService) tickAlerts(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error(context.Background(), "alert-tick-panic", fmt.Errorf("recovered panic: %v", r))
		}
	}()

	b.state.Load().inner.AlertTick(ctx)
}
