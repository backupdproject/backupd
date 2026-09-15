package repomaintenance

import (
	"fmt"
	"time"

	"github.com/retnd/retnd/core/internal/alert"
	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/model"
)

// What a maintenance record looks like to somebody who has to render it
// or be woken by it. Both of these are pure functions of the durable
// record, which is what lets a surface (#788) show the same facts the
// alerting pass acted on, rather than a second reading of them.

// Metrics is one repository's maintenance state as numbers.
//
// It is deliberately not a Prometheus registry, a gauge set or anything
// else that has to be wired: internal/metrics renders this product's
// scrape from a health report, and what that renderer needs from here is
// values. Building the metric names in this package would put half of
// the scrape's vocabulary somewhere internal/metrics cannot see it.
type Metrics struct {
	Domain model.RepositoryDomainID
	Owner  backupengine.MaintenanceOwner

	// LastQuick, LastFull and NextEligible are the record's own
	// timestamps. Zero means never, which a renderer must show as never
	// and not as the epoch.
	LastQuick    time.Time
	LastFull     time.Time
	NextEligible time.Time

	// Runs and Failures are the record's cumulative counters, so they
	// count windows the bounded history has already forgotten.
	Runs     int
	Failures int

	// ReclaimedBytes is the measured net change over every window this
	// record has described. See backupengine.MaintenanceOutcome.Reclaimed
	// for why it can be negative.
	ReclaimedBytes int64

	// Failing is whether the most recent window failed, which is the same
	// fact AlertConditions raises a condition for. It is a separate field
	// from Failures because "has ever failed" and "is failing now" send
	// an operator to different places.
	Failing bool
}

// Measure reads a record.
func Measure(record backupengine.MaintenanceOwnership) Metrics {
	return Metrics{
		Domain:         record.Domain,
		Owner:          record.Owner,
		LastQuick:      record.LastQuick,
		LastFull:       record.LastFull,
		NextEligible:   record.NextEligible,
		Runs:           record.Runs,
		Failures:       record.Failures,
		ReclaimedBytes: record.ReclaimedBytes,
		Failing:        record.LastResult.Err != "",
	}
}

// AlertConditions is what this repository's maintenance state means to
// the operator-facing notification path, in internal/alert's own
// vocabulary.
//
// # Why the condition is "the last window failed" and not "it failed N
// times"
//
// Because that is the condition alert.Dispatcher's model actually
// handles: it is true while maintenance keeps failing, it stops being
// observed the moment a window succeeds, and a later relapse is a fresh
// alert. A threshold on the failure count would fire once and then never
// resolve, because the count only goes up.
//
// # What the sentence has to say
//
// That nothing was lost. The obvious reading of "maintenance failed" is
// that the repository is damaged, and the reflexes that follow from that
// reading are worse than the fault: a maintenance failure means space was
// not reclaimed, no manifest was touched, and every restore point is
// exactly where it was. The message says so in those words, and
// TestAFailedMaintenanceIsRecordedAlertsAndBacksOff fails if it stops
// saying it.
func AlertConditions(record backupengine.MaintenanceOwnership) []alert.Condition {
	if record.LastResult.Err == "" {
		return nil
	}

	return []alert.Condition{{
		Kind:  alert.MaintenanceFailed,
		Scope: record.Domain.String(),
		Detail: fmt.Sprintf(
			"%s maintenance of repository %s failed: %s. No snapshot was deleted and no restore point was affected -- a failed maintenance reclaims nothing, it does not damage anything -- but storage freed by deleted snapshots stays occupied until a maintenance window succeeds.",
			record.LastResult.Mode, record.Domain, record.LastResult.Err),
	}}
}
