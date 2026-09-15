package workflowrun

import (
	"context"
	"reflect"
	"testing"

	"github.com/retnd/retnd/core/internal/workflow"
)

// The nested cleanup scope (#811's Nested cleanup scope section), as a
// table.
//
// The rule in one sentence: the global scope is entered when the run
// begins and the backup-set scope only once global-before has succeeded.
// Everything in this table follows from that, including the row that is
// easiest to get wrong -- a configured but EMPTY "before" directory still
// enters its scope, so its "after" stage still runs. An operator who put
// a report script in the "after" directory and nothing in the "before"
// one meant exactly that, and post-run reporting is a legitimate use of a
// stage.

func TestNestedEligibility(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		scripts map[stage][]string

		// failScript, if set, is the script that exits non-zero.
		failScript string

		wantDispatched []string
		wantGlobal     workflow.ObligationState
		wantSet        workflow.ObligationState
		wantCleanup    workflow.Status
	}{
		{
			name: "both scopes entered and both discharged",
			scripts: map[stage][]string{
				globalBefore: {"10-a.local.sh"},
				setBefore:    {"10-b.local.sh"},
				setAfter:     {"20-c.local.sh"},
				globalAfter:  {"20-d.local.sh"},
			},
			wantDispatched: []string{
				"local:10-a.local.sh", "local:10-b.local.sh", "backup",
				"local:20-c.local.sh", "local:20-d.local.sh",
			},
			wantGlobal:  workflow.ObligationSuccess,
			wantSet:     workflow.ObligationSuccess,
			wantCleanup: workflow.StatusSuccess,
		},
		{
			name: "global before fails: only the global scope is owed",
			scripts: map[stage][]string{
				globalBefore: {"10-a.local.sh"},
				setBefore:    {"10-b.local.sh"},
				setAfter:     {"20-c.local.sh"},
				globalAfter:  {"20-d.local.sh"},
			},
			failScript:     "10-a.local.sh",
			wantDispatched: []string{"local:10-a.local.sh", "local:20-d.local.sh"},
			wantGlobal:     workflow.ObligationSuccess,
			wantSet:        workflow.ObligationNeverEligible,
			wantCleanup:    workflow.StatusSuccess,
		},
		{
			name: "set before fails: both scopes are owed",
			scripts: map[stage][]string{
				globalBefore: {"10-a.local.sh"},
				setBefore:    {"10-b.local.sh"},
				setAfter:     {"20-c.local.sh"},
				globalAfter:  {"20-d.local.sh"},
			},
			failScript: "10-b.local.sh",
			wantDispatched: []string{
				"local:10-a.local.sh", "local:10-b.local.sh",
				"local:20-c.local.sh", "local:20-d.local.sh",
			},
			wantGlobal:  workflow.ObligationSuccess,
			wantSet:     workflow.ObligationSuccess,
			wantCleanup: workflow.StatusSuccess,
		},
		{
			// The row this rule exists for. Both "before" directories
			// are configured and EMPTY, so nothing runs in front of the
			// backup -- and both scopes are still entered, so both
			// "after" stages run.
			name: "empty before directories still make the after stages eligible",
			scripts: map[stage][]string{
				globalBefore: {},
				setBefore:    {},
				setAfter:     {"20-c.local.sh"},
				globalAfter:  {"20-d.local.sh"},
			},
			wantDispatched: []string{"backup", "local:20-c.local.sh", "local:20-d.local.sh"},
			wantGlobal:     workflow.ObligationSuccess,
			wantSet:        workflow.ObligationSuccess,
			wantCleanup:    workflow.StatusSuccess,
		},
		{
			// No "before" stage configured at all is the same answer for
			// a different reason: the scope is entered by the RUN
			// beginning, not by a directory existing.
			name: "after stages with no before stages configured",
			scripts: map[stage][]string{
				setAfter:    {"20-c.local.sh"},
				globalAfter: {"20-d.local.sh"},
			},
			wantDispatched: []string{"backup", "local:20-c.local.sh", "local:20-d.local.sh"},
			wantGlobal:     workflow.ObligationSuccess,
			wantSet:        workflow.ObligationSuccess,
			wantCleanup:    workflow.StatusSuccess,
		},
		{
			// A scope with nothing to undo is still accounted for: the
			// obligation was to be able to say what became of the scope,
			// and "there was nothing in it" is an answer.
			name: "before stages with no after stages",
			scripts: map[stage][]string{
				globalBefore: {"10-a.local.sh"},
				setBefore:    {"10-b.local.sh"},
			},
			wantDispatched: []string{"local:10-a.local.sh", "local:10-b.local.sh", "backup"},
			wantGlobal:     workflow.ObligationSuccess,
			wantSet:        workflow.ObligationSuccess,
			wantCleanup:    workflow.StatusSuccess,
		},
		{
			// Cleanup was PLANNED and none of it was eligible, which is
			// the one case cleanup_status is "skipped": there was
			// something to undo for the set scope and that scope was
			// never entered.
			name: "global before fails with only a set after stage",
			scripts: map[stage][]string{
				globalBefore: {"10-a.local.sh"},
				setAfter:     {"20-c.local.sh"},
			},
			failScript:     "10-a.local.sh",
			wantDispatched: []string{"local:10-a.local.sh"},
			wantGlobal:     workflow.ObligationSuccess,
			wantSet:        workflow.ObligationNeverEligible,
			wantCleanup:    workflow.StatusSkipped,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			tr := newTree(t, tc.scripts)

			if tc.failScript != "" {
				h.local.outcomes[tc.failScript] = failing(1)
			}

			res, err := h.run(t, tr.snapshot(t, "run-1"))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if got := h.rec.dispatched(); !reflect.DeepEqual(got, tc.wantDispatched) {
				t.Errorf("dispatched\n\t%v\nwant\n\t%v", got, tc.wantDispatched)
			}
			if got := obligationOf(t, h.store, "run-1", workflow.ScopeGlobal).State; got != tc.wantGlobal {
				t.Errorf("the global obligation is %q, want %q", got, tc.wantGlobal)
			}
			if got := obligationOf(t, h.store, "run-1", workflow.ScopeSet).State; got != tc.wantSet {
				t.Errorf("the set obligation is %q, want %q", got, tc.wantSet)
			}
			if res.CleanupStatus != tc.wantCleanup {
				t.Errorf("cleanup status is %q, want %q", res.CleanupStatus, tc.wantCleanup)
			}

			// Every step the plan declared is accounted for: a step that
			// was deliberately not run is "skipped" rather than left
			// pending, because a plan read back afterwards has to say
			// what happened to all of it.
			steps, err := h.store.WorkflowSteps(context.Background(), "run-1")
			if err != nil {
				t.Fatalf("WorkflowSteps: %v", err)
			}
			for _, s := range steps {
				if !workflow.State(s.State).Terminal() {
					t.Errorf("step %s was left in %q, which is not a terminal state", s.ScriptName, s.State)
				}
			}
		})
	}
}

// The obligation is durably eligible BEFORE the first side-effecting
// command in its scope, and for the backup-set scope the first such
// command can be the BACKUP rather than a hook -- a set with an empty
// "before" directory and a real "after" one.
//
// So the write has to happen when the scope is entered, not when its
// first hook starts. This asserts it from inside the backup: at the
// moment today's backup-set operation is called, the journal already says
// the set scope is owed its cleanup.
func TestTheSetScopeIsOwedItsCleanupBeforeTheBackupRuns(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{
		setBefore: {},
		setAfter:  {"20-c.local.sh"},
	})

	var observed workflow.ObligationState
	_, err := h.engine.Run(context.Background(), RunRequest{
		Plan:        tr.snapshot(t, "run-1"),
		BackupSetID: tr.setID,
		Backup: func(context.Context) error {
			observed = obligationOf(t, h.store, "run-1", workflow.ScopeSet).State

			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if observed != workflow.ObligationEligible {
		t.Errorf("when the backup started the set obligation was %q, want %q; the backup is itself a side effect in that scope, so a crash during it must reconcile to a scope that is owed its cleanup",
			observed, workflow.ObligationEligible)
	}
}
