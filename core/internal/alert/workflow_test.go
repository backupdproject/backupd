package alert_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/alert"
)

// workflow.go answers two questions about EPIC L's hooks (#813), and this
// file is one test per question plus the gate that matters more than
// either.
//
// The first question is classification: three status words in, one
// outcome and a set of Kinds out. It is a table because the interesting
// content is the MAPPING -- which combinations produce two conditions,
// which produce one, and which produce silence -- and a table is the only
// shape in which a reader can see that a bypassed successful run and a
// run still in flight are silent for different reasons.
//
// The second is grouping: one condition per blocked backup set, however
// many holds the engine recorded against it.
//
// The gate is TestNotificationsNeverCarryScriptOutput. Everything this
// package renders about a workflow run came out of a script somebody else
// wrote, running on a machine this manager does not own, and a
// notification leaves the process for a phone, a mail relay or a chat
// webhook. So the test drives the same functions twice: once with the
// values a real run produces, to prove the useful ones do reach the
// operator, and once with values shaped like captured script output, to
// prove those do not. It also asserts the field set of WorkflowRun
// itself, because the real defence is that there is nowhere to put a
// hook's output, not that something strips it afterwards.

func mustEqualKinds(t *testing.T, what string, got, want []alert.Kind) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: kinds = %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: kinds = %v, want %v", what, got, want)
		}
	}
}

// TestClassifyWorkflowRunAndItsConditions is #813's notification
// classification table: every combination of the two facts an operator
// acts on (did the backup happen, was the source machine put back),
// against the outcome and the exact conditions it must produce.
func TestClassifyWorkflowRunAndItsConditions(t *testing.T) {
	cases := []struct {
		name  string
		run   alert.WorkflowRun
		want  alert.WorkflowOutcome
		kinds []alert.Kind
	}{
		{
			// A "before" hook refused, so the backup never started and
			// the "after" hooks then ran cleanly. One job for an
			// operator: find out why the hook said no.
			name: "before hook refused and cleanup was clean",
			run: alert.WorkflowRun{
				BackupSet:      "production/postgres-primary",
				RunID:          "01HQ8Z5T4WYB7N0QG2R3XKD9VA",
				BackupStatus:   "skipped",
				CleanupStatus:  "success",
				WorkflowStatus: "failed",
				FailedStep:     "quiesce-pg",
				FailedScript:   "10-pg-quiesce.before.remote.sh",
			},
			want:  alert.WorkflowBeforeFailedBackupSkipped,
			kinds: []alert.Kind{alert.WorkflowFailed},
		},
		{
			// The hooks were fine and the backup itself failed. Same
			// remedy, different cause; the Detail carries the
			// difference, the Kind does not.
			name: "backup failed and cleanup was clean",
			run: alert.WorkflowRun{
				BackupSet:      "production/postgres-primary",
				RunID:          "01HQ8Z5T4WYB7N0QG2R3XKD9VB",
				BackupStatus:   "failed",
				CleanupStatus:  "success",
				WorkflowStatus: "failed",
			},
			want:  alert.WorkflowBackupFailedCleanupOK,
			kinds: []alert.Kind{alert.WorkflowFailed},
		},
		{
			// The one that must never be folded into "workflow failed".
			// There is a perfectly good backup from this run and a
			// database that may still be in backup mode.
			name: "backup succeeded and cleanup failed",
			run: alert.WorkflowRun{
				BackupSet:      "production/postgres-primary",
				RunID:          "01HQ8Z5T4WYB7N0QG2R3XKD9VC",
				BackupStatus:   "success",
				CleanupStatus:  "failed",
				WorkflowStatus: "failed",
				FailedStep:     "unquiesce-pg",
				FailedScript:   "10-pg-quiesce.after.remote.sh",
			},
			want:  alert.WorkflowBackupOKCleanupFailed,
			kinds: []alert.Kind{alert.WorkflowCleanupFailed},
		},
		{
			// Two unrelated jobs, so two notifications.
			name: "backup failed and cleanup failed",
			run: alert.WorkflowRun{
				BackupSet:      "production/postgres-primary",
				RunID:          "01HQ8Z5T4WYB7N0QG2R3XKD9VD",
				BackupStatus:   "failed",
				CleanupStatus:  "failed",
				WorkflowStatus: "failed",
			},
			want:  alert.WorkflowBothFailed,
			kinds: []alert.Kind{alert.WorkflowFailed, alert.WorkflowCleanupFailed},
		},
		{
			// The other way into WorkflowBothFailed: the backup was
			// skipped rather than failed. It still raises the backup
			// condition, because "the hook refused" and "the backup
			// broke" leave an operator equally without a restore point.
			name: "before hook refused and cleanup failed too",
			run: alert.WorkflowRun{
				BackupSet:      "production/postgres-primary",
				RunID:          "01HQ8Z5T4WYB7N0QG2R3XKD9VE",
				BackupStatus:   "skipped",
				CleanupStatus:  "failed",
				WorkflowStatus: "failed",
			},
			want:  alert.WorkflowBothFailed,
			kinds: []alert.Kind{alert.WorkflowFailed, alert.WorkflowCleanupFailed},
		},
		{
			// A resumed cleanup that failed: the process that would
			// have recorded the backup status is the one that died, so
			// nobody knows it. The cleanup alert must still fire, and
			// the backup alert must not, because claiming "you have no
			// backup" on a status nobody wrote sends an operator
			// looking for a restore point that may well exist.
			name: "cleanup failed with the backup status never established",
			run: alert.WorkflowRun{
				BackupSet:      "production/postgres-primary",
				RunID:          "01HQ8Z5T4WYB7N0QG2R3XKD9VF",
				BackupStatus:   "unknown",
				CleanupStatus:  "failed",
				WorkflowStatus: "failed",
			},
			want:  alert.WorkflowBackupOKCleanupFailed,
			kinds: []alert.Kind{alert.WorkflowCleanupFailed},
		},
		{
			// The ordinary night, which is almost every night.
			name: "everything succeeded",
			run: alert.WorkflowRun{
				BackupSet:      "production/postgres-primary",
				RunID:          "01HQ8Z5T4WYB7N0QG2R3XKD9VG",
				BackupStatus:   "success",
				CleanupStatus:  "success",
				WorkflowStatus: "success",
			},
			want:  alert.WorkflowOutcomeNone,
			kinds: nil,
		},
		{
			// A bypassed run that backed up. Nothing failed, so there
			// is nothing to notify anybody about: a deliberate bypass
			// is visible in the run row and in the audit record, which
			// is where an operator's own action belongs.
			//
			// The workflow status here is deliberately NOT success, so
			// this case cannot pass merely because of the
			// success shortcut and has to reach the same answer on the
			// facts.
			name: "bypassed run whose backup succeeded",
			run: alert.WorkflowRun{
				BackupSet:      "production/postgres-primary",
				RunID:          "01HQ8Z5T4WYB7N0QG2R3XKD9VH",
				BackupStatus:   "success",
				CleanupStatus:  "skipped",
				WorkflowStatus: "unknown",
				Bypassed:       true,
			},
			want:  alert.WorkflowOutcomeNone,
			kinds: nil,
		},
		{
			// A run still executing. Reading "not yet known" as failed
			// is how every in-flight backup becomes an alert.
			name: "run still in flight",
			run: alert.WorkflowRun{
				BackupSet:      "production/postgres-primary",
				RunID:          "01HQ8Z5T4WYB7N0QG2R3XKD9VJ",
				BackupStatus:   "running",
				CleanupStatus:  "unknown",
				WorkflowStatus: "running",
			},
			want:  alert.WorkflowOutcomeNone,
			kinds: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := alert.ClassifyWorkflowRun(tc.run); got != tc.want {
				t.Errorf("ClassifyWorkflowRun = %q, want %q", got, tc.want)
			}

			conds := alert.WorkflowConditions(tc.run)
			mustEqualKinds(t, "WorkflowConditions", kindsOf(conds), tc.kinds)

			// Every condition is scoped to the backup set, never to the
			// run, so the dispatcher de-duplicates a set whose workflow
			// keeps failing instead of alerting once per night forever.
			for _, c := range conds {
				if c.Scope != tc.run.BackupSet {
					t.Errorf("condition %s has scope %q, want the backup set %q", c.Kind, c.Scope, tc.run.BackupSet)
				}
				if strings.TrimSpace(c.Detail) == "" {
					t.Errorf("condition %s has an empty Detail: a notification with no message is worse than none", c.Kind)
				}
			}
		})
	}
}

// The canaries are shaped like the things a hook really emits: a line of
// stderr with a credential in it, an environment assignment, and the
// absolute path the script was read from. None of them is a backup set
// id, a run id, a step id or a script basename, which is exactly the
// point -- they are what a caller would be passing if they had reached
// for the wrong field.
const (
	outputCanary = `psql: FATAL: password authentication failed for user "backup"`
	secretCanary = `PGPASSWORD=hunter2 TOKEN=sk-live-DEADBEEF`
	pathCanary   = "/etc/backupd/hooks/production/10-pg-quiesce.before.remote.sh"
)

// canaryFragments are checked alongside the whole canaries so the test
// still fails if a future rendering emits only the interesting half.
var canaryFragments = []string{
	outputCanary, secretCanary, pathCanary,
	"hunter2", "sk-live-DEADBEEF", "/etc/backupd", "password authentication failed",
}

func assertNoCanary(t *testing.T, what string, conds []alert.Condition) {
	t.Helper()
	if len(conds) == 0 {
		t.Fatalf("%s produced no conditions: this check would pass vacuously", what)
	}
	for _, c := range conds {
		for _, fragment := range canaryFragments {
			if strings.Contains(c.Detail, fragment) {
				t.Errorf("%s: condition %s carries %q in its Detail %q", what, c.Kind, fragment, c.Detail)
			}
		}
	}
}

// TestNotificationsNeverCarryScriptOutput is #813's hard rule. A hook is
// code somebody else wrote, its output is bytes that script chose, and a
// notification is delivered OUT of this process to somewhere nobody
// audited. So a notification may name the backup set, the run, the failed
// step, the failed script's basename and the three status words, and
// nothing else at all.
func TestNotificationsNeverCarryScriptOutput(t *testing.T) {
	// First, the useful half. A rule that redacted everything would pass
	// the canary check and be worthless, so this pins that the four
	// values an operator needs really do reach them.
	t.Run("names the values an operator needs", func(t *testing.T) {
		conds := alert.WorkflowConditions(alert.WorkflowRun{
			BackupSet:      "production/postgres-primary",
			RunID:          "01HQ8Z5T4WYB7N0QG2R3XKD9VA",
			BackupStatus:   "success",
			CleanupStatus:  "failed",
			WorkflowStatus: "failed",
			FailedStep:     "unquiesce-pg",
			FailedScript:   "10-pg-quiesce.after.remote.sh",
		})
		if len(conds) != 1 {
			t.Fatalf("WorkflowConditions returned %d conditions, want 1", len(conds))
		}

		for _, want := range []string{
			"production/postgres-primary",
			"01HQ8Z5T4WYB7N0QG2R3XKD9VA",
			"unquiesce-pg",
			"10-pg-quiesce.after.remote.sh",
			"cleanup failed",
		} {
			if !strings.Contains(conds[0].Detail, want) {
				t.Errorf("Detail %q does not name %q", conds[0].Detail, want)
			}
		}
	})

	// Then the canaries, once per status field, because a status word
	// that is not a status word is the one case where the value cannot
	// simply be dropped: it has to become "unknown", which is this
	// vocabulary's own honest answer for "nobody established this".
	//
	// Each variant leaves the other two statuses real so the run still
	// classifies as something and the check has conditions to inspect.
	t.Run("withholds anything that is not one of those values", func(t *testing.T) {
		variants := []struct {
			name                            string
			backup, cleanup, workflowStatus string
		}{
			{"backup status carries output", secretCanary, "failed", "failed"},
			{"cleanup status carries output", "failed", secretCanary, "failed"},
			{"workflow status carries output", "failed", "failed", secretCanary},
		}

		for _, v := range variants {
			t.Run(v.name, func(t *testing.T) {
				conds := alert.WorkflowConditions(alert.WorkflowRun{
					BackupSet:      pathCanary,
					RunID:          secretCanary,
					BackupStatus:   v.backup,
					CleanupStatus:  v.cleanup,
					WorkflowStatus: v.workflowStatus,
					FailedStep:     outputCanary,
					FailedScript:   pathCanary,
				})
				assertNoCanary(t, "WorkflowConditions", conds)
			})
		}
	})

	t.Run("recovery conditions withhold it too", func(t *testing.T) {
		conds := alert.WorkflowRecoveryConditions([]alert.WorkflowRecoveryHoldSubject{{
			BackupSet: pathCanary,
			RunID:     secretCanary,
			Scope:     outputCanary,
		}})
		assertNoCanary(t, "WorkflowRecoveryConditions", conds)
	})

	// The structural half, and the one that actually holds the line.
	// Redaction is a second chance; the first is that a caller holding a
	// step's captured output, an exit code's stderr tail, a resolved
	// secret or a spool path has nowhere to put it. This fails the
	// moment somebody widens the type, which is when the decision is
	// being made rather than after it has shipped.
	t.Run("WorkflowRun has nowhere to put script output", func(t *testing.T) {
		want := map[string]reflect.Kind{
			"BackupSet":      reflect.String,
			"RunID":          reflect.String,
			"BackupStatus":   reflect.String,
			"CleanupStatus":  reflect.String,
			"WorkflowStatus": reflect.String,
			"FailedStep":     reflect.String,
			"FailedScript":   reflect.String,
			"Bypassed":       reflect.Bool,
		}

		typ := reflect.TypeOf(alert.WorkflowRun{})
		if typ.NumField() != len(want) {
			t.Fatalf("WorkflowRun has %d fields, want exactly the %d a notification may name", typ.NumField(), len(want))
		}
		for i := range typ.NumField() {
			f := typ.Field(i)
			kind, allowed := want[f.Name]
			if !allowed {
				t.Errorf("WorkflowRun.%s is not one of the values a notification may carry; see workflow.go on why this type is closed", f.Name)
				continue
			}
			if f.Type.Kind() != kind {
				t.Errorf("WorkflowRun.%s is %s, want %s", f.Name, f.Type.Kind(), kind)
			}
		}
	})
}

// TestWorkflowRecoveryConditionsAreOnePerBackupSet pins the grouping.
// internal/workflowrun holds one record per interrupted SCOPE because
// that is what a resume pass discharges; an operator runs one command per
// SET, so two scopes of one run are one notification naming both.
func TestWorkflowRecoveryConditionsAreOnePerBackupSet(t *testing.T) {
	conds := alert.WorkflowRecoveryConditions([]alert.WorkflowRecoveryHoldSubject{
		{BackupSet: "production/postgres-primary", RunID: "01HQ8Z5T4WYB7N0QG2R3XKD9VA", Scope: "global"},
		{BackupSet: "production/postgres-primary", RunID: "01HQ8Z5T4WYB7N0QG2R3XKD9VA", Scope: "set"},
		{BackupSet: "staging/mysql", RunID: "01HQ8Z5T4WYB7N0QG2R3XKD9VB", Scope: "set"},
	})

	mustEqualKinds(t, "WorkflowRecoveryConditions", kindsOf(conds),
		[]alert.Kind{alert.WorkflowRecoveryRequired, alert.WorkflowRecoveryRequired})

	if conds[0].Scope != "production/postgres-primary" || conds[1].Scope != "staging/mysql" {
		t.Fatalf("scopes = %q and %q, want one condition per backup set in the order the holds named them", conds[0].Scope, conds[1].Scope)
	}

	// Both outstanding scopes of the one interrupted run are named, and
	// the run is named once rather than twice.
	for _, want := range []string{"01HQ8Z5T4WYB7N0QG2R3XKD9VA", "global", "set"} {
		if !strings.Contains(conds[0].Detail, want) {
			t.Errorf("Detail %q does not name %q", conds[0].Detail, want)
		}
	}
	if strings.Count(conds[0].Detail, "01HQ8Z5T4WYB7N0QG2R3XKD9VA") != 1 {
		t.Errorf("Detail %q names the blocking run more than once: two scopes of one run are one problem", conds[0].Detail)
	}

	if alert.WorkflowRecoveryConditions(nil) != nil {
		t.Error("WorkflowRecoveryConditions(nil) returned a condition: nothing is held, so nothing is blocked")
	}
}

// TestWorkflowKindsAreDeliverable proves the three new kinds are in the
// vocabulary and that each renders a headline of its own.
//
// The titles are checked by delivering, rather than by reading an
// unexported function, because the headline only matters as the thing a
// sink receives. A kind with no case in title() still produces an Alert,
// carrying the generic fallback -- which is why an empty check would not
// be enough and this asserts the fallback is NOT what came out.
func TestWorkflowKindsAreDeliverable(t *testing.T) {
	const genericFallback = "retnd alert"

	workflowKinds := []alert.Kind{alert.WorkflowFailed, alert.WorkflowCleanupFailed, alert.WorkflowRecoveryRequired}

	for _, k := range workflowKinds {
		found := false
		for _, known := range alert.Kinds {
			if known == k {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Kind %q is not in alert.Kinds, so no reader or test sees the whole vocabulary", k)
		}
	}

	sink := &recordingSink{}
	d := alert.NewDispatcher(sink, nil)
	now := time.Date(2025, 3, 4, 1, 30, 0, 0, time.UTC)

	conditions := make([]alert.Condition, 0, len(workflowKinds))
	for _, k := range workflowKinds {
		conditions = append(conditions, alert.Condition{Kind: k, Scope: "production/postgres-primary", Detail: "detail"})
	}

	delivered := d.Observe(context.Background(), conditions, nil, now)
	if len(delivered) != len(workflowKinds) {
		t.Fatalf("Observe delivered %d alerts, want %d", len(delivered), len(workflowKinds))
	}

	titles := map[string]alert.Kind{}
	for _, a := range delivered {
		switch {
		case strings.TrimSpace(a.Title) == "":
			t.Errorf("Kind %q renders an empty title", a.Kind)
		case a.Title == genericFallback:
			t.Errorf("Kind %q renders the generic fallback title, so it has no case in title()", a.Kind)
		}
		if other, clash := titles[a.Title]; clash {
			t.Errorf("Kinds %q and %q share the title %q, so two different remedies read identically", other, a.Kind, a.Title)
		}
		titles[a.Title] = a.Kind
	}
}
