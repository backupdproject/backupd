package workflow

import (
	"strings"
	"testing"
	"time"
)

// Step.Validate's suite, and the one property that makes it a security
// check rather than a schema check: a step's TARGET and its IDENTITY are
// both DERIVED from fields the journal also stores, so a record whose
// stored target disagrees with its stored script name is refused instead
// of executed.
//
// Why this matters more than it reads. #810 executes a step by looking at
// Target: local means "run it here, as the daemon"; remote means "run it
// over this connection, on the machine the backup pulls from". A record
// saying quiesce.remote.sh runs LOCALLY is a script written to run on
// somebody else's database server being run as root on the backup server,
// and the recovery path reads these records back off disk after a restart
// -- so "the planner would never build that" is not the guarantee. The
// guarantee is that no such record validates.

// validStep is the positive control every case below mutates: a plan step
// that Validate accepts, with its id derived exactly as Snapshot derives
// it.
func validStep() Step {
	const name = "10-quiesce.local.sh"

	return Step{
		ID:           StepID(0, ScopeSet, PhaseBefore, name),
		RunID:        "run-1",
		Scope:        ScopeSet,
		Phase:        PhaseBefore,
		Order:        0,
		ScriptName:   name,
		ScriptSHA256: strings.Repeat("a", 64),
		ScriptSize:   12,
		Target:       TargetLocal,
		Timeout:      time.Minute,
		SpoolRef:     "/var/lib/backupd/workflow-runs/run-1/scripts/" + StepID(0, ScopeSet, PhaseBefore, name),
		State:        StatePending,
	}
}

func TestStepValidateRefusesATargetItsNameDoesNotName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		what    string
		mutate  func(*Step)
		mustSay string
	}{
		{
			what: "a remote script claiming to run locally",
			mutate: func(s *Step) {
				s.ScriptName = "10-quiesce.remote.sh"
				s.ID = StepID(s.Order, s.Scope, s.Phase, s.ScriptName)
				s.Target = TargetLocal
				s.ExecutionConnectionRef = ""
			},
			mustSay: "10-quiesce.remote.sh",
		},
		{
			what: "a local script claiming to run remotely",
			mutate: func(s *Step) {
				s.Target = TargetRemote
				s.ExecutionConnectionRef = "primary"
			},
			mustSay: "10-quiesce.local.sh",
		},
		{
			what: "an id that is not derived from the step's own fields",
			mutate: func(s *Step) {
				s.ID = "0000~set~before~something-else.local.sh"
			},
			mustSay: "derived",
		},
		{
			what: "an id derived from a different order",
			mutate: func(s *Step) {
				s.Order = 3
			},
			mustSay: "derived",
		},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()

			s := validStep()
			tc.mutate(&s)

			err := s.Validate()
			if err == nil {
				t.Fatalf("Step.Validate accepted %s", tc.what)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("Step.Validate said:\n\t%v\nwant it to contain %q", err, tc.mustSay)
			}
		})
	}

	// The positive control: without it every row above would pass against
	// a Validate that refused everything.
	if err := validStep().Validate(); err != nil {
		t.Fatalf("the control step was refused, so every row above proves nothing: %v", err)
	}

	// And the remote control, so that "a remote step is refused" cannot be
	// how the first row passes.
	remote := validStep()
	remote.ScriptName = "10-quiesce.remote.sh"
	remote.ID = StepID(remote.Order, remote.Scope, remote.Phase, remote.ScriptName)
	remote.Target = TargetRemote
	remote.ExecutionConnectionRef = "primary"
	remote.SpoolRef = "/var/lib/backupd/workflow-runs/run-1/scripts/" + remote.ID

	if err := remote.Validate(); err != nil {
		t.Fatalf("a well-formed remote step was refused: %v", err)
	}
}
