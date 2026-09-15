package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/retnd/retnd/core/internal/obs"
)

// FR-1's long-running mode, and why the loop is here rather than in the CLI.
//
// The loop is nine lines and cmd/retnd could hold it. It does not,
// because two of this product's guarantees are properties of the loop's
// shape: no two passes over a backup set ever overlap, and a shutdown stops
// work at a boundary where nothing is half-written. Both are argued from the
// code being a plain for loop with a timer rather than a ticker, and an
// argument like that is only worth something if it is made somewhere a test
// can exercise it without sending the process a real signal.
//
// So the split is that the CLI decides what a signal MEANS and turns it into
// a cancelled context, and this file decides what happens next. Nothing here
// installs a signal handler, and nothing here should.
//
// The alerting pass beside the cycle loop is the one thing that runs on its
// own timer instead of at the end of a cycle. That is not tidiness: the
// condition most worth reporting is a daemon that is up and producing
// nothing, and a cycle wedged on one slow transfer never reaches its own
// tail. See AlertTick.

// Daemon is FR-1's `daemon` execution mode: it runs RunCycle once
// immediately, then repeatedly, until ctx is done.
//
// # The cadence comes from the configuration, not from the caller
//
// It used to be an argument, and since issue #845 it cannot be: a backup
// set may override the deployment-wide poll_interval, so "how often does
// this loop wake" is a question about the whole configuration rather
// than one number, and a caller passing one in would be passing half the
// answer. The loop sleeps until the EARLIEST moment any enabled set is
// due again (NextPollWake), recomputed after every pass, which for a
// deployment that overrides nothing is exactly poll_interval, the loop
// this has always been.
//
// Which sets a given wake actually processes is RunCycle's own decision,
// made per set against that set's effective interval, because the cycle
// is marked as a scheduled one here (WithScheduledCycle). A wake where
// nothing is due does nothing. See pollschedule.go for why the cadence
// lives inside one sequential loop rather than in a timer per set.
//
// The alerting pass below stays on the DEPLOYMENT's poll_interval rather
// than on whatever the next wake happens to be: it reports on the
// manager, not on any one source, so one set asking to be polled every
// minute is not a reason to rebuild a health report every minute.
//
// cmd/retnd owns turning SIGTERM/SIGINT into ctx's cancellation
// (via signal.NotifyContext), exactly as FR-1 asks for "handle
// SIGTERM/SIGINT" and "use Go context cancellation" to be read together:
// this package only ever reacts to ctx, and never installs a signal
// handler of its own, so the CLI stays the one place that decides what a
// signal means and this package stays testable without sending itself a
// real signal.
//
// # No overlapping cycles
//
// This is a single, unbuffered for loop: the next RunCycle call is never
// started until the previous one has returned, and the wait between them
// is a plain select against a timer and ctx.Done(), never a
// time.Ticker firing on its own schedule regardless of whether the last
// cycle finished. A cycle that happens to run longer than interval simply
// means the next one starts late, immediately after the slow one returns,
// rather than two cycles ever overlapping. Combined with RunCycle's own
// sequential, non-concurrent processing of every configured backup set
// (see its doc), this makes "no overlapping processing for the same
// backup set" true for every backup set, all the time, by construction.
//
// # Returning
//
// Daemon returns nil whenever ctx becomes done, whether that is observed
// right after a RunCycle call returns or while waiting out the interval
// between cycles: either way this is FR-1's ordinary, expected shutdown
// path, not an error condition cmd/retnd needs to distinguish
// from a clean exit. It returns a non-nil error only for a configuration
// problem (a non-positive poll_interval) caught before the loop ever
// starts.
func (s *Service) Daemon(ctx context.Context) error {
	interval := s.Config.PollInterval.Duration()
	if interval <= 0 {
		return fmt.Errorf("app: daemon needs a positive poll_interval, got %s", interval)
	}

	s.logger().Event(ctx, obs.LevelInfo, "daemon_start", "daemon starting",
		slog.Duration("poll_interval", interval))

	// Work Package 3.5's alerting pass also runs on its own timer, beside
	// the cycle loop rather than inside it (see AlertTick). A cycle that
	// wedges on one slow transfer never reaches the pass at its end, and
	// "the daemon is up but producing nothing" is the exact situation the
	// stale alert exists to report, so it cannot be the situation that
	// silences it. This goroutine stops when ctx does, and the deferred
	// receive below keeps Daemon from returning while it is still running.
	alertsStopped := make(chan struct{})
	go func() {
		defer close(alertsStopped)
		s.runAlertTicks(ctx, interval)
	}()
	defer func() { <-alertsStopped }()

	for {
		s.RunCycle(WithScheduledCycle(ctx))

		if ctx.Err() != nil {
			s.logger().Event(ctx, obs.LevelInfo, "daemon_stop", "daemon shutting down", slog.String("reason", ctx.Err().Error()))
			return nil
		}

		// The next wake is the earliest moment any enabled set is due
		// again, recomputed every pass (pollschedule.go's
		// NextPollWake). Recomputed rather than held because the answer
		// depends on what this cycle just attempted, and a fixed sleep
		// of the tightest configured interval would round every other
		// set's cadence up to a multiple of it -- a 7 minute set under
		// a 5 minute wake is a 10 minute set, which is not what its
		// operator asked for.
		fire, stop := s.pollSleep(s.NextPollWake(s.now()))
		select {
		case <-ctx.Done():
			stop()
			s.logger().Event(ctx, obs.LevelInfo, "daemon_stop", "daemon shutting down", slog.String("reason", ctx.Err().Error()))
			return nil
		case <-fire:
		}
	}
}

// pollSleep arms the loop's sleep: the injected timer when a test
// installed one (Service.newTimer), and a real one otherwise.
func (s *Service) pollSleep(d time.Duration) (<-chan time.Time, func() bool) {
	if s.newTimer != nil {
		return s.newTimer(d)
	}
	timer := time.NewTimer(d)
	return timer.C, timer.Stop
}
