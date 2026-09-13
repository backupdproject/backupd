// Package metrics renders an already-computed internal/health.Report
// (FR-24) as Prometheus text exposition format.
//
// # Why this package exists, and why it stops here
//
// docs/adr/0002-phase-5-scope.md is the full reasoning; the short version:
// internal/health already computes every fact FR-24 asks for (process
// version info, and, per backup set, its health state, freshness, pending
// deletes, failures, quarantine counts, free space and every timestamp it
// tracks), and nothing renders any of it yet. This package is exactly one
// rendering, and only the mechanical, policy-free half of "metrics and
// alerts": turning already-computed values into a text format a scraper
// can read. It makes no decision about what counts as alert-worthy (that
// is health.State's job, already done, see State.OK()), invents no
// threshold, and has no opinion about delivery, a webhook, a pager, a
// dashboard query. Rendering exposition text needed none of those
// decisions to be made first, so this package makes none of them.
//
// # No wiring included on purpose
//
// Render takes a health.Report as a plain value and returns a string.
// Nothing in this package calls internal/health itself, holds a journal,
// or knows how a Report gets built. Nothing outside this package calls
// Render yet either: cmd/backupd has no subcommand to serve it from
// (issues #25, #26), the same position internal/health, internal/obs and
// internal/capacity are already in. Wiring this in later, a
// "backupd status --prometheus" flag, an HTTP handler, or both, is
// meant to be a few lines calling Render, not a redesign.
//
// # Format
//
// Output follows the Prometheus text exposition format, version 0.0.4
// (see ContentType): a "# HELP" and "# TYPE" line per metric name,
// followed by that metric's samples grouped together, one line each. Every
// metric name is prefixed backupd_ so it cannot collide with
// another exporter's metric on the same scrape target. A health.Report
// field the caller never populated (any of BackupSetInputs' three
// pointers) or one internal/health never had evidence for
// (NewestGoodBackupAge) omits that metric's sample for that backup set
// entirely, matching Prometheus's own convention that an unknown value has
// no series, rather than rendering a fabricated zero that would read as a
// real reading of zero.
package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/backupdproject/backupd/core/internal/health"
	"github.com/backupdproject/backupd/core/internal/lifecycle"
)

// ContentType is the MIME type a caller should set on an HTTP response
// (or otherwise associate with Render's output), per the Prometheus text
// exposition format's own versioning scheme.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// namePrefix roots every metric name this package emits. It is the binary
// name, backupd, with the hyphen replaced by an underscore, since a
// Prometheus metric name may not contain a hyphen.
const namePrefix = "backupd_"

// newestGoodBackupAgeHelp names, in the HELP line a scraping operator
// reads, exactly the states internal/health counts as known-good.
//
// It is built from lifecycle's own durable-restore-point set rather than
// typed out here, which is issue #505: the hand-typed version named three
// states and had done since REMOTE_RETAINED joined that set with #282. An
// operator running a read-only backup set reads this line to find out what
// the gauge beneath it is measuring, and every artifact they have is
// REMOTE_RETAINED, so the line named none of the states it was measuring
// for them and read as "this gauge does not cover you".
var newestGoodBackupAgeHelp = "Age of the newest known-good (" +
	lifecycle.DurableRestorePointNames() + ") backup, in seconds."

// healthStates lists FR-24's four backup-set states in the fixed order
// backup_set_state always renders them in, so sample order never depends
// on map iteration or on the order decideState happens to check them in.
var healthStates = []health.State{health.Healthy, health.Degraded, health.Stale, health.Failing}

// Render renders report as Prometheus text exposition format.
//
// Backup sets are sorted by their model.BackupSetID string form before
// rendering, so two calls against Reports holding the same data in a
// different slice order produce byte-identical output; nothing here
// depends on the order report.BackupSets happened to be built in.
func Render(report health.Report) string {
	sets := append([]health.BackupSetHealth(nil), report.BackupSets...)
	sort.Slice(sets, func(i, j int) bool {
		return sets[i].Set.String() < sets[j].Set.String()
	})

	var b strings.Builder

	writeProcessInfo(&b, report)
	writeGeneratedAt(&b, report)
	writeState(&b, sets)

	writeGauge(&b, sets, "newest_good_backup_age_seconds",
		newestGoodBackupAgeHelp,
		func(s health.BackupSetHealth) (float64, bool) {
			if s.NewestGoodBackupAge == nil {
				return 0, false
			}
			return s.NewestGoodBackupAge.Seconds(), true
		})

	writeGauge(&b, sets, "stale_threshold_seconds",
		"Configured stale_after threshold for this backup set, in seconds.",
		func(s health.BackupSetHealth) (float64, bool) {
			return s.StaleThreshold.Seconds(), true
		})

	writeGauge(&b, sets, "pending_deletes",
		"Artifacts currently at REMOTE_DELETE_PENDING for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.PendingDeletes), true
		})

	writeGauge(&b, sets, "failures",
		"Artifacts currently FAILED for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.Failures), true
		})

	writeGauge(&b, sets, "quarantined",
		"Artifacts currently quarantined, recoverable or not, for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.QuarantinedCount), true
		})

	writeGauge(&b, sets, "quarantined_lost",
		"Artifacts currently QUARANTINED_LOST (irrecoverable) for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.QuarantinedLostCount), true
		})

	// Issue #227. A reinstated artifact never authorises deleting its
	// remote source again, so this number only ever grows, and a scrape is
	// the surface that answers "is it growing" over the months an operator
	// would actually notice it in. There is deliberately no companion
	// bytes metric: see health.BackupSetHealth's own field doc for why the
	// size of those preserved remote objects is not a fact this manager
	// has. Unlike free_bytes below, a zero here is a real reading rather
	// than a missing one, so the sample always renders.
	writeGauge(&b, sets, "reinstated_remote_retained",
		"Artifacts reinstated out of quarantine that still hold a remote source this manager will never delete. The bytes those remote objects occupy are deliberately not reported: this manager cannot see them.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.ReinstatedRemoteRetainedCount), true
		})

	writeGauge(&b, sets, "current_transfers",
		"Artifacts currently TRANSFERRING for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(len(s.CurrentTransfers)), true
		})

	writeGauge(&b, sets, "free_bytes",
		"Free space on this backup set's local destination filesystem, in bytes.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.FreeBytes == nil {
				return 0, false
			}
			return float64(*s.FreeBytes), true
		})

	writeGauge(&b, sets, "last_successful_poll_timestamp_seconds",
		"Unix time discovery last completed successfully for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.LastSuccessfulPollAt == nil {
				return 0, false
			}
			return float64(s.LastSuccessfulPollAt.Unix()), true
		})

	writeGauge(&b, sets, "last_completed_backup_timestamp_seconds",
		"Unix time of the newest artifact currently COMPLETE for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.LastCompletedBackupAt == nil {
				return 0, false
			}
			return float64(s.LastCompletedBackupAt.Unix()), true
		})

	// Issue #444, FR-24's placement half. These are the metrics that make
	// "the moves have been failing for a week" alertable, which is the
	// whole shape of the defect: the fact was visible for one pass, on a
	// terminal nobody was watching, and then gone.
	//
	// away_from_home is reported unconditionally because zero is the real
	// and common reading (every deployment whose artifacts are where they
	// belong, and every deployment that declares no medium at all). The
	// two ages beside it are not: an age only exists when there is
	// something to be the age of, and a zero would read as "this happened
	// just now", which is the opposite of missing.
	writeGauge(&b, sets, "away_from_home",
		"Artifacts whose durable copy is not on the storage medium this backup set's retention chain says it belongs on.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.Placement.AwayFromHome), true
		})

	writeGauge(&b, sets, "away_from_home_oldest_age_seconds",
		"How long the oldest away-from-home copy has existed on the medium it is sitting on, in seconds. An upper bound on how long it has been in the wrong place: nothing durable records when an artifact's home last changed.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.Placement.OldestAwayFromHomeAge == nil {
				return 0, false
			}
			return s.Placement.OldestAwayFromHomeAge.Seconds(), true
		})

	writeGauge(&b, sets, "open_moves",
		"Relocations this backup set has open in the move journal, in any non-terminal phase.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.Placement.OpenMoves), true
		})

	writeGauge(&b, sets, "open_move_oldest_age_seconds",
		"How long the oldest open relocation has been open, in seconds, whether or not anything has been recorded against it. Not every way a move gets stuck leaves a reason on the row, so this keeps growing where failed_moves cannot see the problem.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.Placement.OldestOpenMoveAge == nil {
				return 0, false
			}
			return s.Placement.OldestOpenMoveAge.Seconds(), true
		})

	writeGauge(&b, sets, "failed_moves",
		"Open relocations whose last attempt failed. This is the number that turns an otherwise-healthy backup set DEGRADED.",
		func(s health.BackupSetHealth) (float64, bool) {
			return float64(s.Placement.FailedMoves), true
		})

	writeGauge(&b, sets, "failed_move_oldest_age_seconds",
		"How long the oldest failing relocation has been open, in seconds, measured from when this manager wrote the move down. This is the difference between a blip and a wedge, and it is the one to alert on.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.Placement.OldestFailedMoveAge == nil {
				return 0, false
			}
			return s.Placement.OldestFailedMoveAge.Seconds(), true
		})

	writeGauge(&b, sets, "last_retention_run_timestamp_seconds",
		"Unix time GFS retention last ran for this backup set.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.LastRetentionRunAt == nil {
				return 0, false
			}
			return float64(s.LastRetentionRunAt.Unix()), true
		})

	// EPIC K's four numbers, as four series (#783).
	//
	// They are separate metrics rather than one with a label because
	// that is the difference the requirement is about: a scrape has to
	// be able to graph what was READ against what was WRITTEN without
	// summing them by accident, and a single
	// backupd_snapshot_bytes{kind="..."} family is exactly the shape a
	// dashboard sums. Presenting the logical size where a reader expects
	// "uploaded" is the claim EPIC K forbids, and a metric that can be
	// aggregated into that claim is the same mistake one query away.
	//
	// Every one of them is absent for a set that runs no snapshots, and
	// absent for an incremental set whose newest run never got far
	// enough to measure anything. A zero here would read as a real
	// reading of zero bytes, which for a deduplicating engine is a
	// plausible-looking number and therefore the worst possible way to
	// be wrong.
	writeGauge(&b, sets, "snapshot_entries_scanned",
		"Source entries the newest snapshot run of this backup set considered, of every kind, including the ones it deliberately skipped. It is the source side's own census, not files plus directories. Absent for a set that takes no snapshots.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.Snapshot == nil || !s.Snapshot.Measured {
				return 0, false
			}
			return float64(s.Snapshot.EntriesScanned), true
		})

	writeGauge(&b, sets, "snapshot_logical_bytes",
		"Size of the source tree as the source described it, for the newest snapshot run. This is what was SCANNED and is never what was uploaded.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.Snapshot == nil || !s.Snapshot.Measured {
				return 0, false
			}
			return float64(s.Snapshot.LogicalBytes), true
		})

	writeGauge(&b, sets, "snapshot_source_bytes_read",
		"Bytes the newest snapshot run actually pulled off the source. An incremental engine still reads the source; this is what that cost.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.Snapshot == nil || !s.Snapshot.Measured {
				return 0, false
			}
			return float64(s.Snapshot.SourceBytesRead), true
		})

	writeGauge(&b, sets, "snapshot_repository_bytes_written",
		"Bytes the newest snapshot run actually wrote into the repository's storage, after deduplication and compression. This is the only one of these numbers that is storage growth.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.Snapshot == nil || !s.Snapshot.Measured {
				return 0, false
			}
			return float64(s.Snapshot.RepositoryBytesWritten), true
		})

	writeGauge(&b, sets, "snapshot_content_reused_bytes",
		"Bytes the newest snapshot run did not have to store again, because the repository already held that content. The gap between read and written.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.Snapshot == nil || !s.Snapshot.Measured {
				return 0, false
			}
			return float64(s.Snapshot.ContentReusedBytes), true
		})

	writeGauge(&b, sets, "snapshot_files",
		"Files the newest snapshot run of this backup set stored. Absent for a set that takes no snapshots and for a run nobody measured.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.Snapshot == nil || !s.Snapshot.Measured {
				return 0, false
			}
			return float64(s.Snapshot.Files), true
		})

	writeGauge(&b, sets, "snapshot_duration_seconds",
		"How long the newest snapshot run of this backup set took, in seconds. Absent while a run is still going: a duration for something unfinished is a measurement of now rather than of the run.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.Snapshot == nil || s.Snapshot.Duration <= 0 {
				return 0, false
			}
			return s.Snapshot.Duration.Seconds(), true
		})

	writeGauge(&b, sets, "snapshot_verification_failed",
		"1 when the newest snapshot run of this backup set could not be proven readable, 0 when it could or when no verification has concluded. Absent for a set that takes no snapshots.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.Snapshot == nil {
				return 0, false
			}
			if s.Snapshot.VerificationFailed {
				return 1, true
			}
			return 0, true
		})

	writeGauge(&b, sets, "snapshot_unfinished_runs",
		"Snapshot runs of this backup set left in a non-terminal phase: what a crash left for the next cycle's reconciliation to decide. Zero after a clean cycle.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.Snapshot == nil {
				return 0, false
			}
			return float64(s.Snapshot.UnfinishedRuns), true
		})

	writeGauge(&b, sets, "snapshot_last_known_good_timestamp_seconds",
		"Unix time the newest snapshot this backup set can still restore from completed. Absent when it has none, which is not the same as old.",
		func(s health.BackupSetHealth) (float64, bool) {
			if s.Snapshot == nil || s.Snapshot.LastKnownGoodAt == nil {
				return 0, false
			}
			return float64(s.Snapshot.LastKnownGoodAt.Unix()), true
		})

	// EPIC L's health half (#813). The seven workflow COUNTER families
	// live in workflow.go, on a live counter set with a process
	// lifetime; these three are readings of an already-computed
	// health.Report like everything above them, which is why they
	// render here and not there.
	writeWorkflowHealth(&b, sets, report.Workflow)

	return b.String()
}

func writeProcessInfo(b *strings.Builder, report health.Report) {
	name := namePrefix + "process_info"
	writeHelp(b, name, "Build information for the running backupd process. Constant 1; the version data is in the labels.")
	writeType(b, name, "gauge")
	fmt.Fprintf(b, "%s{binary_version=%s,rclone_version=%s} 1\n",
		name, quoteLabel(report.Process.BinaryVersion), quoteLabel(report.Process.RcloneVersion))
}

func writeGeneratedAt(b *strings.Builder, report health.Report) {
	name := namePrefix + "report_generated_timestamp_seconds"
	writeHelp(b, name, "Unix time this health report was generated.")
	writeType(b, name, "gauge")
	fmt.Fprintf(b, "%s %d\n", name, report.GeneratedAt.Unix())
}

// writeState renders backup_set_state as the standard Prometheus "one-hot
// enum" pattern: one sample per (backup_set, state) pair, 1 for the state
// BackupSetHealth.State actually holds and 0 for the other three, rather
// than a single sample whose value is some arbitrary state-to-number
// mapping a reader would have to memorize.
func writeState(b *strings.Builder, sets []health.BackupSetHealth) {
	name := namePrefix + "backup_set_state"
	writeHelp(b, name, "Backup set health state (FR-24), as a one-hot indicator per state label: exactly one state label reads 1 for a given backup_set, the rest read 0.")
	writeType(b, name, "gauge")
	for _, s := range sets {
		for _, st := range healthStates {
			v := 0
			if s.State == st {
				v = 1
			}
			fmt.Fprintf(b, "%s{backup_set=%s,state=%s} %d\n",
				name, quoteLabel(s.Set.String()), quoteLabel(strings.ToLower(st.String())), v)
		}
	}
}

// writeGauge writes one metric family, name namePrefix+"backup_set_"+suffix,
// with one sample per backup set in sets for which value reports ok. A
// backup set for which value reports !ok contributes no sample at all: see
// the package doc for why an unknown value must never render as a
// fabricated zero.
func writeGauge(b *strings.Builder, sets []health.BackupSetHealth, suffix, help string, value func(health.BackupSetHealth) (float64, bool)) {
	name := namePrefix + "backup_set_" + suffix
	writeHelp(b, name, help)
	writeType(b, name, "gauge")
	for _, s := range sets {
		v, ok := value(s)
		if !ok {
			continue
		}
		fmt.Fprintf(b, "%s{backup_set=%s} %s\n", name, quoteLabel(s.Set.String()), formatFloat(v))
	}
}

// writeWorkflowHealth renders EPIC L's three workflow-health gauges
// (#813): whether hooks can be run at all, and whether a run is waiting
// for a person.
//
// They are gauges rather than counters because every one of them is a
// STATE that can go back: a runner comes back, a far side regains its
// capabilities, a hold gets acknowledged. The counters in workflow.go
// answer the other half ("has this been failing"), and neither shape
// substitutes for the other -- a counter cannot say "it is broken right
// now" and a gauge cannot say "it broke fourteen times last night".
//
// Each family writes its HELP and TYPE lines unconditionally and its
// samples only where a reading exists, which is exactly what writeGauge
// above does for the backup-set families. A scraper reading a family
// with no samples learns "nobody measured this", which is the truth on a
// deployment that runs no workflows; a fabricated zero would tell it the
// runner is unreachable, and this package's doc argues at length that
// those two must never render the same.
func writeWorkflowHealth(b *strings.Builder, sets []health.BackupSetHealth, w health.WorkflowHealth) {
	writeWorkflowRunner(b, w.Runner)
	writeWorkflowExecConnections(b, w.ExecConnections)
	writeWorkflowRecoveryRequired(b, sets, w)
}

// writeWorkflowRunner renders the Host Workflow Runner's reachability,
// labelled with the version it answered with.
//
// The version is a label rather than a second metric for the reason
// process_info already carries its build strings that way: a version is
// a dimension of the reading and not a number to graph. It is on THIS
// family rather than a runner_info family beside it because the pair is
// the diagnosis -- 0 with a version label is a runner that refused this
// engine's release, 0 with an empty one is a socket nobody answered --
// and two families would make an operator join them by hand to find that
// out. An empty label value is the exposition format's own spelling of
// "no such dimension", which is what an unanswered handshake knows about
// the far side's version.
//
// The runner's bash version is deliberately NOT a label. This family
// answers one question, and a deployment that upgrades bash would
// otherwise mint a new series for a change that has nothing to do with
// whether the socket answers. It is on the status surface, where
// hostrunner reports it, and that is where somebody writing a hook reads
// it.
//
// A deployment that declares no runner gets no sample at all: nothing
// took a reading, and a 0 would read as "the runner is down" on every
// deployment that never had one.
func writeWorkflowRunner(b *strings.Builder, r health.WorkflowRunnerHealth) {
	name := namePrefix + "workflow_runner_reachable"
	writeHelp(b, name, "1 when this engine completed a handshake with the Host Workflow Runner that executes local hook scripts, 0 when it could not. Absent when this deployment declares no runner. Why a handshake failed is a sentence on the status surface, never a label here.")
	writeType(b, name, "gauge")

	if !r.Configured {
		return
	}

	writeSample(b, name, []label{{"version", r.Version}}, gaugeBool(r.Reachable))
}

// writeWorkflowExecConnections renders one capability reading per
// declared execution connection.
//
// `connection` is the one new free-text label this package gained, and it
// is bounded by the CONFIGURATION: the values are the ids in
// workflows.exec_connections, an operator edits that file and restarts
// to change them, and config's own validation already refuses a
// duplicate. That is the same bound backup_set has, which is the only
// free-text label this package had before, and it is the reason this one
// is allowed where a script name or a step id is not: those are minted
// per run, by whoever wrote the hook, and would grow a series per hook
// forever (workflow.go's rule one).
//
// Sorted by ref, stably, so two scrapes of the same report are
// byte-identical. Stably specifically because a duplicate ref would
// otherwise render in whichever order the sort happened to leave it:
// configuration validation forbids one, and a renderer whose output
// depends on validation having run is a renderer that is wrong on the
// day somebody constructs a Report in a test.
func writeWorkflowExecConnections(b *strings.Builder, conns []health.WorkflowExecConnectionHealth) {
	name := namePrefix + "workflow_exec_connection_capable"
	writeHelp(b, name, "1 when a declared workflow execution connection was proven able to run this product's remote hooks, 0 when it was not or could not be checked. One sample per connection declared in workflows.exec_connections. Why a connection is not capable is a sentence on the status surface, never a label here.")
	writeType(b, name, "gauge")

	sorted := append([]health.WorkflowExecConnectionHealth(nil), conns...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Ref < sorted[j].Ref })

	for _, c := range sorted {
		writeSample(b, name, []label{{"connection", c.Ref}}, gaugeBool(c.Capable))
	}
}

// writeWorkflowRecoveryRequired renders how many unaccounted-for cleanup
// scopes each backup set is blocked by.
//
// A count per set rather than a series per hold, because a hold is
// identified by a run id and a run id must never become a label
// (workflow.go's rule one: it is minted per run, so a deployment with a
// nightly backup and an unlucky month would leave thirty dead series
// behind). The ids an operator needs in order to resume or acknowledge
// are on the status surface, which is the surface those commands are run
// from anyway.
//
// Every backup set in the report renders, including the ones at zero,
// and that is the opposite of the rule the optional gauges above follow.
// Zero here is a real reading -- this pass looked and found nothing
// outstanding -- and it is the reading an alert has to see in order to
// RESOLVE. A family that only appeared when something was wrong would
// make "recovery_required > 0" resolve on a scrape failure and on a
// restart, which are the two moments an operator most needs it not to.
//
// A hold naming a set that is not in the report still renders, which is
// why the counts start from the report's sets and are then incremented
// from the holds rather than looked up. A set removed from the
// configuration while one of its runs is still blocked is exactly the
// case where dropping the series would hide the problem forever.
//
// Nothing renders at all when no workflows are configured: there is no
// engine, so there is no pass that could have found a hold, and a screen
// of zeros for a subsystem this deployment does not run is how an
// operator learns to scroll past this section.
func writeWorkflowRecoveryRequired(b *strings.Builder, sets []health.BackupSetHealth, w health.WorkflowHealth) {
	name := namePrefix + "workflow_recovery_required"
	writeHelp(b, name, "Workflow cleanup scopes this deployment cannot account for, per backup set: a run was interrupted and nobody has resumed or acknowledged it, so this set's runs are being refused. Zero is the ordinary reading. The run ids needed to resolve one are on the status surface, never a label here.")
	writeType(b, name, "gauge")

	if !w.Configured {
		return
	}

	counts := make(map[string]uint64, len(sets)+len(w.RecoveryHolds))
	for _, s := range sets {
		counts[s.Set.String()] = 0
	}
	for _, hold := range w.RecoveryHolds {
		counts[hold.BackupSet.String()]++
	}

	for _, s := range sortedCounters(counts, func(set string) []label {
		return []label{{"backup_set", set}}
	}) {
		writeSample(b, name, s.labels, float64(s.value))
	}
}

// gaugeBool is how a boolean reaches a gauge VALUE, as opposed to
// boolLabel, which is how one reaches a label. Both spellings exist
// because the two positions have different conventions and mixing them
// is how a dashboard ends up summing the string "true".
func gaugeBool(v bool) float64 {
	if v {
		return 1
	}

	return 0
}

func writeHelp(b *strings.Builder, name, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
}

func writeType(b *strings.Builder, name, typ string) {
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
}

// formatFloat renders v the way Prometheus's own exporters do: the
// shortest decimal representation that round-trips, no exponent, no
// trailing zeros. strconv.FormatFloat with precision -1 already guarantees
// round-tripping; 'f' rather than 'g' keeps it away from scientific
// notation, which the exposition format allows but no widely-used
// Prometheus client library actually emits for a plain gauge value.
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// quoteLabel renders v as a double-quoted Prometheus label value, escaping
// backslash, double quote and newline exactly as the text exposition
// format's own escaping rules require.
//
// Every label value this package actually emits comes from either a
// model.BackupSetID (whose own validation already forbids control
// characters, and "/" needs no escaping in this format) or a build-time
// version string, so none of this is expected to ever fire in practice.
// It is here anyway because Render's contract is "always valid exposition
// text for whatever health.Report it is handed", not "valid for every
// health.Report this repository's own validation happens to allow through
// today".
func quoteLabel(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(v) + `"`
}
