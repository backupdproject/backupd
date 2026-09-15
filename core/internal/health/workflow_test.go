package health

import (
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/model"
)

// Two properties of the workflow half are load-bearing beyond this file,
// and neither is a calculation.
//
// The first is that the zero value means "this deployment runs no
// workflows". Every NewReport call site in the repository gets that value
// without asking for it, so if the zero value ever read as an unreachable
// runner or an outstanding hold, every deployment that configures no
// hooks would start reporting a broken workflow subsystem -- on the CLI,
// on the HTTP surface and in a scrape at once.
//
// The second is which facts turn the section red. OK() is what a surface
// decides to shout with, and the boundary that matters is the one nobody
// would think to check: a runner that is not CONFIGURED is not an
// unreachable runner, and reporting it as one is how an operator learns
// that this section is noise.

func TestWorkflowHealthZeroValueMeansNoWorkflows(t *testing.T) {
	t.Parallel()

	var w WorkflowHealth

	if w.Configured {
		t.Error("the zero value claims workflows are configured")
	}
	if !w.OK() {
		t.Error("the zero value is not OK, so every deployment that runs no hooks reports a problem it does not have")
	}

	// And through Report, which is how it actually reaches a surface:
	// a caller that never sets the section must not be reporting a
	// broken one.
	report := NewReport(ProcessHealth{}, nil, time.Now())
	if !report.Workflow.OK() || report.Workflow.Configured {
		t.Errorf("NewReport produced workflow health %+v; a caller that says nothing about workflows must report the absence of them", report.Workflow)
	}
}

func TestWorkflowHealthOK(t *testing.T) {
	t.Parallel()

	set, err := model.NewBackupSetID("production", "uploads-tree")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	reachable := WorkflowRunnerHealth{Configured: true, Reachable: true, Version: "1.4.0"}

	for _, tc := range []struct {
		what   string
		health WorkflowHealth
		ok     bool
	}{{
		what:   "a configured deployment with a reachable runner and a capable connection",
		health: WorkflowHealth{Configured: true, Runner: reachable, ExecConnections: []WorkflowExecConnectionHealth{{Ref: "db-hooks", Capable: true}}},
		ok:     true,
	}, {
		what:   "a runner that is configured and does not answer",
		health: WorkflowHealth{Configured: true, Runner: WorkflowRunnerHealth{Configured: true}},
		ok:     false,
	}, {
		// The boundary. A deployment whose hooks are all remote
		// declares no runner, and an unconfigured runner is not a
		// broken one: this is the case that decides whether the
		// section is worth reading.
		what:   "no runner declared at all",
		health: WorkflowHealth{Configured: true},
		ok:     true,
	}, {
		what:   "a declared connection that could not be proven capable",
		health: WorkflowHealth{Configured: true, Runner: reachable, ExecConnections: []WorkflowExecConnectionHealth{{Ref: "db-hooks", Capable: true}, {Ref: "nas-hooks"}}},
		ok:     false,
	}, {
		what: "an outstanding recovery hold",
		health: WorkflowHealth{
			Configured:    true,
			Runner:        reachable,
			RecoveryHolds: []WorkflowRecoveryHold{{RunID: "run-1", BackupSet: set, Scope: "global", EnteredAt: time.Now()}},
		},
		ok: false,
	}} {
		if got := tc.health.OK(); got != tc.ok {
			t.Errorf("%s: OK() = %v, want %v", tc.what, got, tc.ok)
		}
	}
}
