package repomaintenance

import (
	"fmt"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
)

// Scheduling is deliberately the smallest thing that can be called a
// scheduler: a pure function from a record and an instant to a decision.
// It holds no clock, for internal/retention's reason -- the same inputs
// must produce the same answer whenever they are asked -- and it stores
// nothing, because everything it reads is already in the durable record
// the last window wrote.

// Intervals is how often each mode of maintenance is wanted.
type Intervals struct {
	// Quick is how long after a quick maintenance the next one is due.
	Quick time.Duration

	// Full is the same for full maintenance, the mode that reclaims
	// storage.
	Full time.Duration

	// Retry is how long a repository is left alone after a FAILED
	// window.
	//
	// It is separate from the two above because a failure is usually the
	// storage being unreachable or refusing writes, and retrying that on
	// every cycle turns one broken NAS into a log full of the same error
	// and a repository that is never left idle long enough for anything
	// else to use it. It is shorter than Full because the thing that
	// failed is usually transient.
	Retry time.Duration
}

// DefaultIntervals is the cadence a caller that names none gets.
//
// The two maintenance numbers are the embedded engine's own defaults
// (hourly quick, daily full), and matching them is deliberate: they are
// the cadence the vendor's safety parameters were chosen against -- a
// full cycle every 24 hours is what makes a 24-hour minimum content age
// and a two-cycle GC requirement converge on actually reclaiming
// something -- and a product that invented its own numbers would be
// tuning one half of a calculation it does not own.
var DefaultIntervals = Intervals{
	Quick: time.Hour,
	Full:  24 * time.Hour,
	Retry: 30 * time.Minute,
}

// withDefaults fills in whatever the caller left at zero. A zero interval
// is read as "not configured" rather than as "continuously", because the
// second reading turns an unset field into a repository being maintained
// in a loop.
func (in Intervals) withDefaults() Intervals {
	if in.Quick <= 0 {
		in.Quick = DefaultIntervals.Quick
	}

	if in.Full <= 0 {
		in.Full = DefaultIntervals.Full
	}

	if in.Retry <= 0 {
		in.Retry = DefaultIntervals.Retry
	}

	return in
}

// Decision is what a scheduler concluded about one repository at one
// instant: whether maintenance is due, which mode, and the sentence an
// operator or a log reads.
//
// Reason is populated either way. "Nothing was due" is the answer a
// surface has to be able to render for a healthy repository, and a
// decision that explained only its refusals would leave that surface
// inventing one.
type Decision struct {
	Due    bool
	Mode   backupengine.MaintenanceMode
	Reason string
}

// Due decides whether maintenance should run against this record now.
//
// # Full before quick
//
// When both are due, full runs. Quick maintenance compacts indexes and
// rewrites short metadata packs; full is the mode that actually reclaims
// the storage a deleted snapshot left behind, and a scheduler that
// preferred the cheap one would let a repository fill up while dutifully
// maintaining it every hour. Full also does quick's work on the way past.
//
// # NextEligible outranks both
//
// It is how a failed window's backoff is expressed, and how a future
// administrative surface (#788) will be able to say "leave this
// repository alone until Monday" without inventing a second mechanism.
func Due(record backupengine.MaintenanceOwnership, in Intervals, now time.Time) Decision {
	in = in.withDefaults()

	if !record.NextEligible.IsZero() && now.Before(record.NextEligible) {
		return Decision{Reason: fmt.Sprintf("maintenance is not eligible again until %s", record.NextEligible.UTC().Format(time.RFC3339))}
	}

	if due(record.LastFull, in.Full, now) {
		return Decision{Due: true, Mode: backupengine.MaintenanceFull, Reason: fullReason(record.LastFull, in.Full)}
	}

	if due(record.LastQuick, in.Quick, now) {
		return Decision{Due: true, Mode: backupengine.MaintenanceQuick, Reason: quickReason(record.LastQuick, in.Quick)}
	}

	return Decision{Reason: fmt.Sprintf("the last full maintenance was %s and the last quick one %s; neither is due",
		when(record.LastFull), when(record.LastQuick))}
}

// due is the one comparison, written once so that "never ran" cannot mean
// something different for the two modes.
func due(last time.Time, every time.Duration, now time.Time) bool {
	return last.IsZero() || !now.Before(last.Add(every))
}

func fullReason(last time.Time, every time.Duration) string {
	if last.IsZero() {
		return "this repository has never had a full maintenance"
	}

	return fmt.Sprintf("the last full maintenance was %s, more than %s ago", last.UTC().Format(time.RFC3339), every)
}

func quickReason(last time.Time, every time.Duration) string {
	if last.IsZero() {
		return "this repository has never had a quick maintenance"
	}

	return fmt.Sprintf("the last quick maintenance was %s, more than %s ago", last.UTC().Format(time.RFC3339), every)
}

func when(t time.Time) string {
	if t.IsZero() {
		return "never"
	}

	return t.UTC().Format(time.RFC3339)
}
