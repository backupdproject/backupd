package workflow

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/model"
)

// The vocabularies' suite. These tests pin SPELLINGS and MEMBERSHIP, which
// sounds like testing constants and is not: every value here is written
// into a journal an older or a newer build may read, so a rename is a
// compatibility break, and the difference between the step set and the run
// set is the thing that stops a step being recorded as "cleanup_running".

// The durable spellings. A rename here is a journal an older build cannot
// interpret, so the strings are pinned rather than derived.
func TestVocabularySpellingsAreTheDurableOnes(t *testing.T) {
	t.Parallel()

	for value, want := range map[string]string{
		string(StatePending):          "pending",
		string(StateRunning):          "running",
		string(StateSuccess):          "success",
		string(StateFailed):           "failed",
		string(StateTimedOut):         "timed_out",
		string(StateCanceled):         "canceled",
		string(StateSkipped):          "skipped",
		string(StateInterrupted):      "interrupted",
		string(StateRecoveryRequired): "recovery_required",
		string(StateCleanupRunning):   "cleanup_running",
		string(StateCleanupFailed):    "cleanup_failed",
		string(StateRecovered):        "recovered",
		string(ScopeGlobal):           "global",
		string(ScopeSet):              "set",
		string(PhaseBefore):           "before",
		string(PhaseAfter):            "after",
		string(TargetLocal):           "local",
		string(TargetRemote):          "remote",
		string(RecoveryNone):          "none",
		string(RecoveryRequired):      "required",
		string(RecoveryInProgress):    "in_progress",
		string(RecoveryResolved):      "resolved",
	} {
		if value != want {
			t.Errorf("a durable vocabulary value is spelled %q, and the journal records %q", value, want)
		}
	}
}

// The four run-only states are not step states, and the eight step states
// are all run states. Both directions matter: the first is what stops a
// step being recorded as "cleanup_running", the second is what lets a
// run's state be derived from its steps' without a translation table.
func TestRunVocabularyExtendsTheStepVocabulary(t *testing.T) {
	t.Parallel()

	steps := StepStates()
	runs := RunStates()

	if len(steps) != 8 {
		t.Errorf("StepStates() has %d values, want the documented 8: %v", len(steps), steps)
	}
	if len(runs) != len(steps)+4 {
		t.Errorf("RunStates() has %d values, want %d", len(runs), len(steps)+4)
	}

	for _, s := range steps {
		if !s.ValidForStep() {
			t.Errorf("%s is in StepStates() and ValidForStep says otherwise", s)
		}
		if !s.ValidForRun() {
			t.Errorf("%s is a step state and not a run state, so a run's state cannot be derived from its steps'", s)
		}
	}

	for _, s := range []State{StateRecoveryRequired, StateCleanupRunning, StateCleanupFailed, StateRecovered} {
		if s.ValidForStep() {
			t.Errorf("%s is a run-level state and ValidForStep accepts it on a step", s)
		}
		if !s.ValidForRun() {
			t.Errorf("%s is not accepted on a run", s)
		}
	}

	if State("nonsense").ValidForRun() || State("nonsense").ValidForStep() {
		t.Error("an unknown state was accepted, so neither membership test does anything")
	}

	// Fixed order, not map order: these lists are rendered into refusal
	// messages and an operator comparing two runs' error text must not
	// see the list reorder.
	if !slices.Equal(RunStates()[:len(steps)], steps) {
		t.Error("RunStates() does not begin with StepStates() in order; the two lists are rendered into messages and their order is part of them")
	}
}

// The retention rule the spool depends on. Both axes have to be settled,
// and the table is here because the pair is easy to get the wrong way
// round and the wrong way round deletes the scripts a recovery was about
// to run.
func TestSpoolRetentionNeedsBothAxesSettled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		state    State
		recovery RecoveryState
		retain   bool
	}{
		{StatePending, RecoveryNone, true},
		{StateRunning, RecoveryNone, true},
		{StateSuccess, RecoveryNone, false},
		{StateFailed, RecoveryNone, false},
		{StateCleanupFailed, RecoveryNone, false},
		{StateRecovered, RecoveryResolved, false},

		// Terminal, and the recovery is still outstanding. This is the
		// row the whole method exists for.
		{StateCleanupFailed, RecoveryRequired, true},
		{StateFailed, RecoveryRequired, true},
		{StateSuccess, RecoveryInProgress, true},

		// Not terminal, whatever the recovery says.
		{StateRecoveryRequired, RecoveryResolved, true},
		{StateCleanupRunning, RecoveryNone, true},
	}

	for _, tc := range cases {
		run := Run{State: tc.state, RecoveryState: tc.recovery}
		if got := run.SpoolRetainable(); got != tc.retain {
			t.Errorf("a run at %s with recovery %s reports SpoolRetainable()=%v, want %v",
				tc.state, tc.recovery, got, tc.retain)
		}
	}
}

// Run.Validate and Step.Validate are the journal's gate, so every field
// that has an invariant gets a row. The positive control at the end is
// what stops the whole table passing against a Validate that refuses
// everything.
func TestValidateRefusesRecordsTheJournalMustNotStore(t *testing.T) {
	t.Parallel()

	setID, err := model.NewBackupSetID("production", "db")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	started := time.Date(2026, 9, 13, 1, 0, 0, 0, time.UTC)
	earlier := started.Add(-time.Hour)

	goodRun := Run{
		ID:               "run-1",
		BackupSetID:      setID,
		State:            StateRunning,
		StartedAt:        started,
		BackupStatus:     StatusUnknown,
		CleanupStatus:    StatusUnknown,
		WorkflowStatus:   StatusRunning,
		RecoveryState:    RecoveryNone,
		ResolvedPlanHash: strings.Repeat("a", 64),
		ScriptSpoolRef:   "/var/lib/backupd/workflow-runs/run-1",
	}

	if err := goodRun.Validate(); err != nil {
		t.Fatalf("the control run was refused, so every row below proves nothing: %v", err)
	}

	runCases := []struct {
		what    string
		mutate  func(r *Run)
		mustSay string
	}{
		{"no id", func(r *Run) { r.ID = "" }, "must not be empty"},
		{"an id with a separator", func(r *Run) { r.ID = "a/b" }, "path separator"},
		{"an id that is the parent directory", func(r *Run) { r.ID = ".." }, "names a directory"},
		{"an id with a control character", func(r *Run) { r.ID = "a\nb" }, "control characters"},
		{"no backup set", func(r *Run) { r.BackupSetID = model.BackupSetID{} }, "names no backup set"},
		{"a state no run has", func(r *Run) { r.State = "nonsense" }, "is not one of"},
		{"a step-only state is fine", nil, ""},
		{"no start time", func(r *Run) { r.StartedAt = time.Time{} }, "no start time"},
		{"finished before it started", func(r *Run) { r.FinishedAt = &earlier }, "before it started"},
		{"a status this product never exports", func(r *Run) { r.BackupStatus = "maybe" }, "backup status"},
		{"a cleanup status this product never exports", func(r *Run) { r.CleanupStatus = "maybe" }, "cleanup status"},
		{"a workflow status this product never exports", func(r *Run) { r.WorkflowStatus = "maybe" }, "workflow status"},
		{"a recovery state this domain does not know", func(r *Run) { r.RecoveryState = "maybe" }, "recovery state"},
		{"no plan hash", func(r *Run) { r.ResolvedPlanHash = "" }, "no resolved plan hash"},
		{"no spool", func(r *Run) { r.ScriptSpoolRef = "" }, "no script spool"},
	}

	for _, tc := range runCases {
		t.Run("run: "+tc.what, func(t *testing.T) {
			t.Parallel()

			r := goodRun
			if tc.mutate != nil {
				tc.mutate(&r)
			}

			err := r.Validate()
			if tc.mustSay == "" {
				if err != nil {
					t.Fatalf("Validate refused a legal run: %v", err)
				}

				return
			}
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.what)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("Validate said:\n\t%v\nwant it to contain %q", err, tc.mustSay)
			}
		})
	}

	goodStep := Step{
		ID:           StepID(0, ScopeSet, PhaseBefore, "a.local.sh"),
		RunID:        "run-1",
		Scope:        ScopeSet,
		Phase:        PhaseBefore,
		Order:        0,
		ScriptName:   "a.local.sh",
		ScriptSHA256: strings.Repeat("b", 64),
		ScriptSize:   12,
		Target:       TargetLocal,
		Timeout:      time.Minute,
		SpoolRef:     "/var/lib/backupd/workflow-runs/run-1/scripts/0000~set~before~a.local.sh",
		State:        StatePending,
	}

	if err := goodStep.Validate(); err != nil {
		t.Fatalf("the control step was refused, so every row below proves nothing: %v", err)
	}

	stepCases := []struct {
		what    string
		mutate  func(s *Step)
		mustSay string
	}{
		{"a run-only state on a step", func(s *Step) { s.State = StateCleanupRunning }, "step state"},
		{"a scope this domain does not have", func(s *Step) { s.Scope = "host" }, "step scope"},
		{"a phase this domain does not have", func(s *Step) { s.Phase = "during" }, "step phase"},
		{"a negative order", func(s *Step) { s.Order = -1 }, "counted from zero"},
		{"a script name the rule refuses", func(s *Step) { s.ScriptName = "backup.sh" }, "does not say where it runs"},
		{"a target this domain does not have", func(s *Step) { s.Target = "somewhere" }, "step target"},
		{"a remote step with no connection", func(s *Step) {
			s.Target = TargetRemote
			s.ScriptName = "a.remote.sh"
			s.ID = StepID(s.Order, s.Scope, s.Phase, s.ScriptName)
		}, "names no execution connection"},
		{"a local step carrying a connection", func(s *Step) { s.ExecutionConnectionRef = "prod/db" }, "run somewhere it will not"},
		{"a hash that is not a sha256", func(s *Step) { s.ScriptSHA256 = "abc" }, "not a hex sha256"},
		{"no timeout", func(s *Step) { s.Timeout = 0 }, "no spelling of"},
		{"a negative timeout", func(s *Step) { s.Timeout = -time.Second }, "no spelling of"},
		{"no spooled script", func(s *Step) { s.SpoolRef = "" }, "no spooled script"},
		{"finished without starting", func(s *Step) { s.FinishedAt = &started }, "finished without ever starting"},
	}

	for _, tc := range stepCases {
		t.Run("step: "+tc.what, func(t *testing.T) {
			t.Parallel()

			s := goodStep
			tc.mutate(&s)

			err := s.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.what)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("Validate said:\n\t%v\nwant it to contain %q", err, tc.mustSay)
			}
		})
	}

	// A remote step WITH a connection is legal, which is the other half
	// of the pair above.
	remote := goodStep
	remote.Target = TargetRemote
	remote.ScriptName = "a.remote.sh"
	remote.ID = StepID(remote.Order, remote.Scope, remote.Phase, remote.ScriptName)
	remote.ExecutionConnectionRef = "production/db"

	if err := remote.Validate(); err != nil {
		t.Errorf("a remote step with an execution connection was refused: %v", err)
	}
}

// StepID has to be a safe single path component, because it is the spool
// file's name. The separator it uses must therefore be one the script-name
// rule forbids, or a script could be named so as to collide with another
// step's spool file.
func TestStepIDIsASafePathComponent(t *testing.T) {
	t.Parallel()

	id := StepID(7, ScopeGlobal, PhaseAfter, "90-resume.remote.sh")

	if err := validPathComponent("step id", id); err != nil {
		t.Fatalf("StepID produced %q, which is not a safe path component: %v", id, err)
	}
	if !strings.HasPrefix(id, "0007~") {
		t.Errorf("StepID = %q; the zero-padded order goes first so a listing of a spool reads in execution order", id)
	}

	// The separator must be a byte no script name may contain, or two
	// steps could be made to derive the same id.
	for _, sep := range []string{"~"} {
		if _, err := ParseScriptName("a" + sep + "b.local.sh"); err == nil {
			t.Errorf("the script-name rule accepts %q, which StepID uses as a separator: two different steps could then derive one spool filename", sep)
		}
	}

	// Two steps that differ only in scope get different ids, which is the
	// collision a global and a per-set hook of the same name would
	// otherwise produce.
	if a, b := StepID(3, ScopeGlobal, PhaseBefore, "x.local.sh"), StepID(3, ScopeSet, PhaseBefore, "x.local.sh"); a == b {
		t.Errorf("a global and a per-set step derive the same id %q", a)
	}
}
