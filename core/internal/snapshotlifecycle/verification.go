package snapshotlifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/state"
)

// This file is the success policy for verification: what one run is asked
// to prove, what it must prove before its snapshot may be advertised, and
// how a cadence of deeper checks is decided from what previous runs
// actually achieved.
//
// # The policy, in four sentences
//
// Structural verification happens on every run, because it is the floor
// of every rung above it and it costs one walk of the manifest. A set's
// CONFIGURED level is the floor that run must reach: a run that proves
// less than the set asked for does not reach SUCCESS, which is what makes
// "last-known-good means verified to the configured level" a property of
// the state machine rather than a sentence in a document. A deeper rung
// may be scheduled on a cadence -- a periodic full read, a periodic
// restore drill -- and that only ever raises the bar for a run, never
// lowers it. And the level written into the catalog is the one the engine
// reported performing, never the one this package asked for.
//
// # Why the cadence is measured from what was PROVEN
//
// The obvious implementation counts runs since the last deep check. It is
// wrong in the one case that matters: a set whose deep checks keep failing
// counts them anyway, and the cadence quietly stops happening while the
// row says it is configured. Reading the catalog for the newest run whose
// ACHIEVED level satisfies the rung means a failed drill leaves the drill
// due, permanently, until one succeeds.

// verificationHistory is how many recent runs the cadence reads.
//
// It bounds a query rather than expressing a policy: the cadence only
// needs the newest run that achieved each rung, and a set that has not
// achieved one within its last fifty runs is a set whose deep check is
// overdue by any definition. Reading further back to find a success from
// last year would let an ancient row suppress a drill.
const verificationHistory = 50

// VerificationOptions is everything about proving a snapshot that is not
// the level itself: how big a sample is, how often the deeper rungs come
// round, and where a restore drill is allowed to write.
//
// It is separate from RunRequest.VerificationLevel because the level is a
// claim recorded on every row and these are the mechanics of producing
// it. A zero VerificationOptions is legal and means exactly what it says:
// the default sample size, no periodic escalation, and nowhere to run a
// drill.
type VerificationOptions struct {
	// SamplePercent is how much of a snapshot's content a sampled
	// verification reads. Zero means the engine's own default
	// (backupengine.DefaultVerifySamplePercent).
	SamplePercent int

	// FullEvery is how often a full content read is due, regardless of
	// the set's configured level. Zero means never, which is the right
	// default for a deployment that has not decided: a full read of every
	// set is a real I/O budget and nobody should acquire one by omission.
	FullEvery time.Duration

	// DrillEvery is how often a restore drill is due. Zero means never.
	//
	// A drill also needs DrillDir, and a cadence configured without one
	// is dropped rather than failing runs: the escalation is this
	// package's idea, not the operator's instruction, so it must not be
	// able to fail a backup. A set whose CONFIGURED level is
	// restore_drill is the operator's instruction, and that one is
	// refused up front when there is nowhere to restore to.
	DrillEvery time.Duration

	// DrillDir is the directory a restore drill restores into, one
	// subdirectory per run. It must be somewhere with room for a whole
	// snapshot and somewhere artifact management will not walk into; the
	// reserved local state directory (backupengine.ReservedLocalStateDir)
	// is both.
	DrillDir string
}

// verification is one run's decision about what to prove, and the one
// place the request to the engine is built.
type verification struct {
	// requested is what this run asks the engine for: the configured
	// level, raised by whichever cadence is due.
	requested model.VerificationLevel

	// required is the set's CONFIGURED level. A run that proves less than
	// this fails, whatever it did prove.
	required model.VerificationLevel

	sample int

	// target is where THIS attempt's drill restores, empty when this run
	// is not drilling. See drillTarget: it is a fresh directory every
	// time, never one an earlier attempt may have left files in.
	target string

	// drillDir and runID are what discardDrillOutput needs in order to
	// find the directories earlier attempts of the same run abandoned.
	drillDir string
	runID    string
}

// planVerification decides what one run proves, from the set's configured
// level, the cadence, and what previous runs actually achieved.
func planVerification(
	configured model.VerificationLevel,
	opts VerificationOptions,
	runID string,
	history []state.SnapshotRun,
	now time.Time,
) verification {
	level := configured

	if opts.FullEvery > 0 && levelIsDue(history, model.LevelContentFull, opts.FullEvery, now) {
		level = deeper(level, model.LevelContentFull)
	}

	// The drill escalation is dropped without a directory, deliberately:
	// see VerificationOptions.DrillEvery.
	if opts.DrillEvery > 0 && opts.DrillDir != "" && levelIsDue(history, model.LevelRestoreDrill, opts.DrillEvery, now) {
		level = deeper(level, model.LevelRestoreDrill)
	}

	v := verification{
		requested: level,
		required:  configured,
		sample:    opts.SamplePercent,
		drillDir:  opts.DrillDir,
		runID:     runID,
	}

	if level == model.LevelRestoreDrill {
		v.target = drillTarget(opts.DrillDir, runID)
	}

	return v
}

// deeper is the higher of two rungs, which is the only direction an
// escalation may move.
func deeper(a, b model.VerificationLevel) model.VerificationLevel {
	if b.Rank() > a.Rank() {
		return b
	}

	return a
}

// levelIsDue reports whether the newest run that PROVED this rung is
// older than the cadence, or whether there has never been one.
func levelIsDue(history []state.SnapshotRun, level model.VerificationLevel, every time.Duration, now time.Time) bool {
	for i := range history {
		run := history[i]

		if run.CompletedAt == nil {
			continue
		}

		if !model.VerificationLevel(run.VerificationLevelAchieved).AtLeast(level) {
			continue
		}

		// The history is newest first, so the first row that satisfies
		// the rung is the newest one that did.
		return now.Sub(*run.CompletedAt) >= every
	}

	return true
}

// run performs the verification and reports the level actually proved.
//
// A shortfall -- the engine proved something real, but less than the set
// asked for -- is a FAILURE and not a quiet downgrade. It is the one
// outcome this function exists to turn into an error, because everything
// downstream reads a SUCCESS row as "verified to the configured level",
// and the alternative is a deployment that configured nightly restore
// drills, got structural checks, and found out during a restore.
func (v verification) run(ctx context.Context, repo Repository, id backupengine.SnapshotID) (model.VerificationLevel, backupengine.VerifyReport, error) {
	report, err := repo.Verify(ctx, id, backupengine.VerifyRequest{
		Level:         v.requested,
		SamplePercent: v.sample,
		RestoreTarget: v.target,
	})
	if err != nil {
		return "", report, err //nolint:wrapcheck // the caller renders this into the row's reason with the report beside it.
	}

	if !report.Level.AtLeast(v.required) {
		return report.Level, report, fmt.Errorf(
			"snapshotlifecycle: this run proved %q and the set requires %q; a restore point is not advertised on a check shallower than the one it was configured for",
			report.Level, v.required)
	}

	return report.Level, report, nil
}

// drillDirPrefix and drillAttemptInfix name the directories one run's
// drill attempts restore into: restore-drill-<runID> for the first, and
// restore-drill-<runID>.attempt-N for every one after it.
//
// The names are structured rather than random because they are read by
// people: an operator looking at a set that keeps failing its drill needs
// to be able to tell which directory belongs to which run, and in what
// order the attempts happened.
const (
	drillDirPrefix    = "restore-drill-"
	drillAttemptInfix = ".attempt-"
)

// drillTarget is the directory the NEXT attempt of one run's drill
// restores into: a directory that does not exist yet, beside whatever
// earlier attempts left behind.
//
// A fresh directory per attempt, rather than the run's one directory, is
// what makes crash recovery survivable. A drill restores under
// ConflictRefuse -- a verification that can overwrite is a verification
// that can destroy data -- so a run that died with a restore in flight
// left partial files, and a recovery pass restoring into the same place
// failed on the first file that was already there. The snapshot was
// intact; the run went to FAILED over the wreckage of the attempt that
// crashed, which is a valid restore point thrown away for bookkeeping
// reasons.
//
// The old attempt is left exactly where it is. It is the only description
// of what a restore of this snapshot actually produced, and this pass has
// not yet proven anything that would make it worthless. discardDrillOutput
// is what removes it, and only once the snapshot is proven.
func drillTarget(dir, runID string) string {
	base := filepath.Join(dir, drillDirPrefix+runID)

	highest := 0

	for _, existing := range drillAttempts(dir, runID) {
		if existing == base {
			highest = max(highest, 1)

			continue
		}

		n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(existing), drillDirPrefix+runID+drillAttemptInfix))
		if err == nil {
			highest = max(highest, n)
		}
	}

	if highest == 0 {
		return base
	}

	return fmt.Sprintf("%s%s%d", base, drillAttemptInfix, highest+1)
}

// drillAttempts is every directory under dir that belongs to this run's
// drill, sorted so the order a reader sees is the order they were made.
//
// A directory that cannot be read yields nothing, which is the right
// answer for the ordinary first attempt: the drill directory is created
// by the restore itself, so on the first drill of a deployment there is
// nothing there to enumerate.
func drillAttempts(dir, runID string) []string {
	if dir == "" || runID == "" {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	base := drillDirPrefix + runID

	var out []string

	for _, e := range entries {
		if e.Name() == base || strings.HasPrefix(e.Name(), base+drillAttemptInfix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}

	slices.Sort(out)

	return out
}

// discardDrillOutput removes a successful drill's restored tree, and with
// it every attempt directory an earlier crash of the same run abandoned.
//
// Only on success, and that asymmetry is the point: a drill that FAILED
// has left the only available evidence of what a restore of this snapshot
// actually produces, and deleting it would leave an operator with a
// sentence in a catalog row and nothing to look at. Once the snapshot IS
// proven, every one of those directories is a partial or complete second
// copy of a backup on a disk somebody is paying for, including the ones
// the crashes left, so they all go together.
//
// A removal that fails is reported by the caller as nothing at all: the
// verification is already decided, and a leftover directory is a
// housekeeping problem rather than a reason to fail a proven backup.
func (v verification) discardDrillOutput() error {
	if v.target == "" {
		return nil
	}

	dirs := drillAttempts(v.drillDir, v.runID)
	if !slices.Contains(dirs, v.target) {
		dirs = append(dirs, v.target)
	}

	var errs []error

	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("snapshotlifecycle: removing the restore drill's output at %s: %w", dir, err))
		}
	}

	return errors.Join(errs...)
}
