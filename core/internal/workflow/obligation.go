package workflow

import (
	"fmt"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
)

// The cleanup obligation: the durable promise that once this product has
// entered a scope, the "after" stage of that scope is going to be
// accounted for -- run, refused, or handed to a person (#811).
//
// # Why an obligation exists at all, separate from the steps
//
// Because the steps are not the promise. A run that dies between its
// "before" stage and its "after" stage leaves a set of step rows that all
// read "pending", which is indistinguishable from a run that died before
// the "before" stage ever started -- and those two situations are a
// machine that has been quiesced and one that has not. The obligation is
// the one row that says "something in this scope has already had a side
// effect, and the undo has not happened yet", and it is written BEFORE
// that side effect rather than after it.
//
// # Why the vocabulary is seven values and not a boolean
//
// Because every one of them is a different thing to do next, and the
// thing an operator has to do next is the whole reason the record is
// durable. "Never eligible" is nothing to do; "eligible not started" is
// run it; "running" is wait; "success" is nothing to do; "failed" is a
// person, with the run over; "recovery required" is a person, with the
// run NOT over and the set blocked; "manually acknowledged" is a person
// who has already been, with their reason on the record. Collapsing any
// pair of those loses an answer somebody needs at three in the morning.
//
// # The rule that makes a crash tractable
//
// Every unsettled state can move to ObligationRecoveryRequired, and
// nothing moves out of a settled one. So a process that dies between any
// two durable transitions leaves an obligation that is either settled
// (nothing to do, and no later pass can re-open it) or unsettled (and the
// reconciliation moves it to recovery_required, which blocks the set
// until somebody resolves it). There is no third case, and that is what
// "reconciles deterministically and errs toward recovery_required" means
// in practice.
//
// The vocabulary is Go-enforced and stored as plain TEXT, for the reason
// states.go gives about every other vocabulary in this package: 0002 and
// 0006 are what widening a CHECK constraint costs in this schema.

// ObligationState is one position in a cleanup obligation's lifecycle.
type ObligationState string

// The seven states an obligation can be in.
const (
	// ObligationNeverEligible is a scope this run never entered, so
	// nothing in it had a side effect and its "after" stage is not owed.
	// It is the state a plan is committed in, and a crash leaves it
	// exactly here: a scope that was never entered cannot have become
	// dirty while nobody was looking.
	ObligationNeverEligible ObligationState = "never_eligible"

	// ObligationEligible is a scope that has been entered and whose
	// cleanup has not started. It is written durably BEFORE the first
	// side-effecting command in the scope, which is the whole point of
	// the record: a crash one instruction later still finds it here.
	ObligationEligible ObligationState = "eligible_not_started"

	// ObligationRunning is a scope whose "after" stage is executing.
	ObligationRunning ObligationState = "running"

	// ObligationSuccess is a scope whose every applicable "after" step
	// reached a terminal recorded state and none of them failed.
	ObligationSuccess ObligationState = "success"

	// ObligationFailed is a scope whose cleanup ran and did not succeed.
	// The obligation is discharged -- it was attempted, and what
	// happened is recorded -- and the machine is not back the way the
	// workflow found it, which is why the run that carries it fails even
	// when its backup succeeded.
	ObligationFailed ObligationState = "failed"

	// ObligationRecoveryRequired is a scope this process cannot account
	// for: it was entered, its cleanup did not reach a terminal recorded
	// state, and nobody observed what happened in between. It is the
	// state every interruption lands in, it keeps the run's spool alive,
	// and it blocks the backup set until a resume-cleanup or an
	// acknowledgement resolves it.
	ObligationRecoveryRequired ObligationState = "recovery_required"

	// ObligationAcknowledged is a scope a person took responsibility
	// for, with a reason on the record. It is the only exit from
	// recovery_required that is not a cleanup run, and it is why
	// AcknowledgedBy and AcknowledgeReason are required rather than
	// decorative: an obligation that can be cleared without saying who
	// and why is an obligation that gets cleared by a script.
	ObligationAcknowledged ObligationState = "manually_acknowledged"
)

// obligationStates is the vocabulary in a FIXED order, for stepStates'
// reason: it is rendered into refusals, and an operator comparing two
// runs' error text must not see the list reorder.
var obligationStates = []ObligationState{
	ObligationNeverEligible,
	ObligationEligible,
	ObligationRunning,
	ObligationSuccess,
	ObligationFailed,
	ObligationRecoveryRequired,
	ObligationAcknowledged,
}

// ObligationStates returns the obligation vocabulary in the documented
// order. A function rather than an exported slice, for StepStates'
// reason.
func ObligationStates() []ObligationState {
	return append([]ObligationState(nil), obligationStates...)
}

// Valid reports whether s is an obligation state this domain knows.
func (s ObligationState) Valid() bool {
	for _, known := range obligationStates {
		if s == known {
			return true
		}
	}

	return false
}

// Settled reports whether this obligation no longer holds anything open.
//
// ObligationFailed is settled and ObligationRecoveryRequired is not, and
// the asymmetry is the point: a cleanup that ran and failed is a finished
// story with a bad ending, while a cleanup nobody can account for is an
// unfinished one. The first is reported; the second blocks the set.
func (s ObligationState) Settled() bool {
	switch s {
	case ObligationNeverEligible, ObligationSuccess, ObligationFailed, ObligationAcknowledged:
		return true
	default:
		return false
	}
}

// Terminal reports whether this obligation has no exits at all.
//
// It is NOT the same question as Settled, and the two were conflated
// here once. Settled means "no work is owed", and ObligationNeverEligible
// is settled -- a scope that was never entered owes nothing -- while
// still having an edge out of it: that scope can be ENTERED, which is
// exactly what the backup-set obligation does once global-before has
// succeeded. Terminal is the other property, the one the safety argument
// actually uses: nothing leaves Success, Failed or Acknowledged, so no
// later pass can re-open a scope somebody already closed.
//
// Callers deciding whether a scope holds something open ask Settled;
// callers reasoning about the shape of the graph ask Terminal.
func (s ObligationState) Terminal() bool {
	switch s {
	case ObligationSuccess, ObligationFailed, ObligationAcknowledged:
		return true
	default:
		return false
	}
}

// RequiresRecovery reports whether this obligation is the one that blocks
// its backup set. It is one method rather than an equality test at each
// call site because the refusal, the scheduler suspension, the health
// warning and the spool retention all ask it, and four spellings of one
// comparison is three chances to write it the wrong way round.
func (s ObligationState) RequiresRecovery() bool { return s == ObligationRecoveryRequired }

// obligationTransitions is the complete legal edge set, from each state
// to the states it may move to.
//
// Written out rather than derived from a rule, because every edge here is
// a product decision and the ABSENCE of an edge is the safety property:
// nothing leaves Success, Failed or Acknowledged, so no later pass can
// re-open a scope somebody already closed, and nothing reaches
// NeverEligible, so a scope that was entered can never be reported as
// having been clean all along.
var obligationTransitions = map[ObligationState][]ObligationState{
	ObligationNeverEligible: {ObligationEligible},
	ObligationEligible: {
		ObligationRunning,
		ObligationRecoveryRequired,
		ObligationAcknowledged,
	},
	ObligationRunning: {
		ObligationSuccess,
		ObligationFailed,
		ObligationRecoveryRequired,
	},
	ObligationRecoveryRequired: {
		ObligationRunning,
		ObligationAcknowledged,
	},
	ObligationSuccess:      nil,
	ObligationFailed:       nil,
	ObligationAcknowledged: nil,
}

// CanFollow reports whether an obligation in state s may move to next,
// returning the refusal rather than a bool so the journal's write path
// can hand an operator the sentence.
//
// A no-op transition (s to s) is refused as well. An idempotent-looking
// write here would hide a double advance, and the two places that matter
// -- a resumed cleanup and a reconciliation pass -- both need to know
// whether they are the ones that moved it.
func (s ObligationState) CanFollow(next ObligationState) error {
	if !s.Valid() {
		return vocabularyError("obligation state", s, ObligationStates())
	}
	if !next.Valid() {
		return vocabularyError("obligation state", next, ObligationStates())
	}

	for _, allowed := range obligationTransitions[s] {
		if allowed == next {
			return nil
		}
	}

	if s.Terminal() {
		return fmt.Errorf(
			"workflow: a cleanup obligation in %q has no exits and cannot move to %q; re-opening a scope somebody already closed would re-run an undo against a machine that is already back the way it was",
			s, next)
	}

	return fmt.Errorf("workflow: a cleanup obligation cannot move from %q to %q", s, next)
}

// CleanupObligation is one scope's durable promise, as the journal stores
// it and as the engine and the recovery pass read it back.
//
// It carries the backup set as well as the run because the refusal it
// drives is per SET: a run in recovery_required blocks the set it belongs
// to, and the pass that answers "may this set run" must not have to join
// two tables to find out which set an outstanding obligation is about.
type CleanupObligation struct {
	// RunID is the run this obligation belongs to, and Scope is which of
	// the two nested scopes it is about. The pair is the identity.
	RunID string
	Scope Scope

	// BackupSetID is the set whose runs this obligation blocks while it
	// requires recovery.
	BackupSetID model.BackupSetID

	// State is the obligation's position. Any of ObligationStates.
	State ObligationState

	// EnteredAt is when the scope was entered: nil for a scope that
	// never was, required for every other state. It is the timestamp an
	// operator reads to answer "how long has this machine been left
	// quiesced".
	EnteredAt *time.Time

	// StartedAt and FinishedAt bracket the cleanup itself. Nil is "has
	// not happened", never the zero time.
	StartedAt  *time.Time
	FinishedAt *time.Time

	// AcknowledgedAt, AcknowledgedBy and AcknowledgeReason are the audit
	// record behind ObligationAcknowledged. All three are required for
	// that state and must be absent for every other one: an
	// acknowledgement with no reason is a cleared alarm with no account
	// of why, which is the thing an audit exists to make impossible.
	AcknowledgedAt    *time.Time
	AcknowledgedBy    string
	AcknowledgeReason string
}

// Validate reports the first way this obligation is not one the journal
// may store. See Run.Validate for why it does not collect.
func (o CleanupObligation) Validate() error {
	if err := validPathComponent("run id", o.RunID); err != nil {
		return err
	}

	if !o.Scope.Valid() {
		return vocabularyError("scope", o.Scope, Scopes())
	}

	if o.BackupSetID.IsZero() {
		return fmt.Errorf("workflow: the cleanup obligation for %s of run %q names no backup set; the refusal it drives is per set, so an obligation nobody can attribute to one cannot block anything",
			o.Scope, o.RunID)
	}

	if !o.State.Valid() {
		return vocabularyError("obligation state", o.State, ObligationStates())
	}

	if o.State == ObligationNeverEligible {
		if o.EnteredAt != nil {
			return fmt.Errorf("workflow: the %s cleanup obligation of run %q never became eligible and carries an entry time; a scope that was entered is owed its cleanup and must not be recorded as one that was not",
				o.Scope, o.RunID)
		}
	} else if o.EnteredAt == nil {
		return fmt.Errorf("workflow: the %s cleanup obligation of run %q is %q and records no time at which the scope was entered; how long a machine has been left quiesced is exactly what a recovery pass reports",
			o.Scope, o.RunID, o.State)
	}

	if err := o.validateOrdering(); err != nil {
		return err
	}

	return o.validateAcknowledgement()
}

// validateOrdering holds the three timestamps to the order they can only
// have happened in. A finish before a start is a record that cannot be
// read as a duration, and a duration is what the history surfaces show.
func (o CleanupObligation) validateOrdering() error {
	if o.FinishedAt != nil && o.StartedAt == nil {
		return fmt.Errorf("workflow: the %s cleanup obligation of run %q finished without having started",
			o.Scope, o.RunID)
	}
	if o.StartedAt != nil && o.EnteredAt != nil && o.StartedAt.Before(*o.EnteredAt) {
		return fmt.Errorf("workflow: the %s cleanup obligation of run %q started at %s, before the scope was entered at %s",
			o.Scope, o.RunID, o.StartedAt.UTC().Format(time.RFC3339), o.EnteredAt.UTC().Format(time.RFC3339))
	}
	if o.FinishedAt != nil && o.StartedAt != nil && o.FinishedAt.Before(*o.StartedAt) {
		return fmt.Errorf("workflow: the %s cleanup obligation of run %q finished at %s, before it started at %s",
			o.Scope, o.RunID, o.FinishedAt.UTC().Format(time.RFC3339), o.StartedAt.UTC().Format(time.RFC3339))
	}

	return nil
}

// validateAcknowledgement requires the whole audit record for an
// acknowledged obligation and refuses a partial one anywhere else.
func (o CleanupObligation) validateAcknowledgement() error {
	acknowledged := o.State == ObligationAcknowledged

	if !acknowledged {
		if o.AcknowledgedAt != nil || o.AcknowledgedBy != "" || o.AcknowledgeReason != "" {
			return fmt.Errorf("workflow: the %s cleanup obligation of run %q is %q and carries an acknowledgement; an acknowledgement is the record of a person taking responsibility and must not be attached to a scope nobody was asked about",
				o.Scope, o.RunID, o.State)
		}

		return nil
	}

	if strings.TrimSpace(o.AcknowledgeReason) == "" {
		return fmt.Errorf("workflow: the %s cleanup obligation of run %q is acknowledged with no audit reason; the reason is the whole difference between an operator who decided the machine is fine and a cleared alarm nobody can explain",
			o.Scope, o.RunID)
	}
	if strings.TrimSpace(o.AcknowledgedBy) == "" {
		return fmt.Errorf("workflow: the %s cleanup obligation of run %q does not record who acknowledged it",
			o.Scope, o.RunID)
	}
	if o.AcknowledgedAt == nil {
		return fmt.Errorf("workflow: the %s cleanup obligation of run %q does not record when it was acknowledged",
			o.Scope, o.RunID)
	}

	return nil
}

// CleanupReasonInterruptedRun is the value of BACKUPD_CLEANUP_REASON for
// an "after" stage a recovery is running rather than the run itself.
//
// It is a durable string and a hook's contract: a script that branches on
// it is deciding whether the thing it is unwinding was left by a run that
// finished its own work or by one that died mid-flight, which are
// different amounts of trust to place in whatever state it finds.
const CleanupReasonInterruptedRun = "interrupted_run"

// CleanupReasonRunCompleted is the value for the ordinary case: the run
// reached its own "after" stage. It is a real value rather than an unset
// variable for StatusUnknown's reason -- an unset variable and one saying
// "this is the normal path" read identically in `test -z`.
const CleanupReasonRunCompleted = "run_completed"
