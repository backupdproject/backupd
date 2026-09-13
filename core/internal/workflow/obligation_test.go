package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
)

// The cleanup obligation's vocabulary and its transition rule (#811).
//
// What these tests are about is the ONE property the whole crash-safety
// argument rests on: from any durable position, the set of positions the
// obligation may legally move to is fixed, and every way of leaving an
// unfinished obligation leads to recovery_required rather than to
// something that reads as finished.

func TestObligationStatesAreTheSevenTheIssueNames(t *testing.T) {
	t.Parallel()

	want := []ObligationState{
		ObligationNeverEligible,
		ObligationEligible,
		ObligationRunning,
		ObligationSuccess,
		ObligationFailed,
		ObligationRecoveryRequired,
		ObligationAcknowledged,
	}

	got := ObligationStates()
	if len(got) != len(want) {
		t.Fatalf("there are %d obligation states, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("obligation state %d is %q, want %q", i, got[i], want[i])
		}
	}

	for _, s := range got {
		if !s.Valid() {
			t.Errorf("%q is in the vocabulary and reports itself invalid", s)
		}
	}
	if ObligationState("whatever").Valid() {
		t.Error("an undeclared obligation state reports itself valid")
	}
}

// ObligationStates is a copy, for StepStates' reason: a package-level
// slice is writable by every importer, and the vocabulary an engine
// validates against must not be editable by one.
func TestObligationStatesCannotBeEditedByACaller(t *testing.T) {
	t.Parallel()

	ObligationStates()[0] = "tampered"

	if ObligationStates()[0] != ObligationNeverEligible {
		t.Fatal("a caller edited the obligation vocabulary through the slice it was handed")
	}
}

// The three questions the engine and the recovery pass ask, and the
// answers that make the refusals correct. recovery_required is the one
// unsettled state that is not "running": something still has to happen,
// by somebody, and a run carrying it must keep blocking its set.
func TestObligationSettlementAndRecovery(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		state    ObligationState
		settled  bool
		recovery bool
	}{
		{ObligationNeverEligible, true, false},
		{ObligationEligible, false, false},
		{ObligationRunning, false, false},
		{ObligationSuccess, true, false},
		{ObligationFailed, true, false},
		{ObligationRecoveryRequired, false, true},
		{ObligationAcknowledged, true, false},
	} {
		if got := tc.state.Settled(); got != tc.settled {
			t.Errorf("%q Settled() = %v, want %v", tc.state, got, tc.settled)
		}
		if got := tc.state.RequiresRecovery(); got != tc.recovery {
			t.Errorf("%q RequiresRecovery() = %v, want %v", tc.state, got, tc.recovery)
		}
	}
}

// The transition rule. Every legal edge is here, and so is the property
// that matters more: nothing leaves a settled obligation, so a resolved
// or acknowledged scope cannot be quietly re-opened and an acknowledged
// one cannot be walked back to something a later pass would re-run.
func TestObligationTransitions(t *testing.T) {
	t.Parallel()

	legal := map[ObligationState][]ObligationState{
		ObligationNeverEligible:    {ObligationEligible},
		ObligationEligible:         {ObligationRunning, ObligationRecoveryRequired, ObligationAcknowledged},
		ObligationRunning:          {ObligationSuccess, ObligationFailed, ObligationRecoveryRequired},
		ObligationRecoveryRequired: {ObligationRunning, ObligationAcknowledged},
		ObligationSuccess:          nil,
		ObligationFailed:           nil,
		ObligationAcknowledged:     nil,
	}

	for _, from := range ObligationStates() {
		allowed := map[ObligationState]bool{}
		for _, to := range legal[from] {
			allowed[to] = true
		}

		for _, to := range ObligationStates() {
			err := from.CanFollow(to)
			if allowed[to] && err != nil {
				t.Errorf("%q -> %q is a legal obligation transition and was refused: %v", from, to, err)
			}
			if !allowed[to] && err == nil {
				t.Errorf("%q -> %q is not a legal obligation transition and was accepted", from, to)
			}
		}
	}
}

// Every unsettled obligation can reach recovery_required, because that is
// the transition a crash or a lost outcome takes, and there is no durable
// position from which "we cannot account for this scope" is unsayable.
func TestEveryUnsettledObligationCanReachRecoveryRequired(t *testing.T) {
	t.Parallel()

	for _, s := range ObligationStates() {
		if s.Settled() || s == ObligationRecoveryRequired {
			continue
		}
		if err := s.CanFollow(ObligationRecoveryRequired); err != nil {
			t.Errorf("an unsettled obligation in %q cannot be moved to recovery_required: %v", s, err)
		}
	}
}

func TestCleanupObligationValidate(t *testing.T) {
	t.Parallel()

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	at := time.Date(2026, 9, 13, 2, 0, 0, 0, time.UTC)

	base := CleanupObligation{
		RunID:       "run-1",
		BackupSetID: set,
		Scope:       ScopeGlobal,
		State:       ObligationEligible,
		EnteredAt:   &at,
	}

	if err := base.Validate(); err != nil {
		t.Fatalf("a well-formed obligation was refused: %v", err)
	}

	for _, tc := range []struct {
		name string
		want string
		mut  func(*CleanupObligation)
	}{
		{"no run", "run id", func(o *CleanupObligation) { o.RunID = "" }},
		{"no set", "backup set", func(o *CleanupObligation) { o.BackupSetID = model.BackupSetID{} }},
		{"bad scope", "scope", func(o *CleanupObligation) { o.Scope = "elsewhere" }},
		{"bad state", "obligation state", func(o *CleanupObligation) { o.State = "nearly" }},
		{
			"eligible with no entry time",
			"entered",
			func(o *CleanupObligation) { o.EnteredAt = nil },
		},
		{
			"never eligible with an entry time",
			"never became eligible",
			func(o *CleanupObligation) { o.State = ObligationNeverEligible },
		},
		{
			"acknowledged with no reason",
			"audit reason",
			func(o *CleanupObligation) {
				o.State = ObligationAcknowledged
				o.AcknowledgedAt = &at
				o.AcknowledgedBy = "operator"
			},
		},
		{
			"acknowledged with no actor",
			"who acknowledged",
			func(o *CleanupObligation) {
				o.State = ObligationAcknowledged
				o.AcknowledgedAt = &at
				o.AcknowledgeReason = "unmounted by hand"
			},
		},
		{
			"acknowledged with no time",
			"when it was acknowledged",
			func(o *CleanupObligation) {
				o.State = ObligationAcknowledged
				o.AcknowledgedBy = "operator"
				o.AcknowledgeReason = "unmounted by hand"
			},
		},
	} {
		o := base
		tc.mut(&o)

		err := o.Validate()
		if err == nil {
			t.Errorf("%s: accepted", tc.name)

			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: the refusal does not mention %q: %v", tc.name, tc.want, err)
		}
	}
}

// An acknowledgement is the only exit from recovery_required that is not
// a cleanup run, so it carries an actor and a reason -- and a reason that
// is only whitespace is not a reason. This is what makes the audit record
// worth having rather than a checkbox.
func TestAcknowledgementRefusesAWhitespaceReason(t *testing.T) {
	t.Parallel()

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	at := time.Date(2026, 9, 13, 2, 0, 0, 0, time.UTC)

	o := CleanupObligation{
		RunID:             "run-1",
		BackupSetID:       set,
		Scope:             ScopeSet,
		State:             ObligationAcknowledged,
		EnteredAt:         &at,
		AcknowledgedAt:    &at,
		AcknowledgedBy:    "operator",
		AcknowledgeReason: "   \t \n ",
	}

	if err := o.Validate(); err == nil {
		t.Fatal("an acknowledgement whose reason is only whitespace was accepted")
	}
}
