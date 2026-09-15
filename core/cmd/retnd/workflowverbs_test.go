package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The workflow surface's command lines: what is discoverable, what is
// dispatchable, and every refusal that has to happen before a journal is
// opened (EPIC L, #813).
//
// The refusals are the bulk of it, and each one is here because the
// alternative to refusing is worse than an error message. A sub-verb
// nobody dispatches, an operand nobody supplied and an acknowledgement
// with no reason all reach an operator as either a 2 they can read or as
// something that looked like it worked, and this binary's exit-code table
// promises the first.

// workflowFixture is a deployment with hooks configured: an approved
// root, two global stage directories, and a backup set that has a
// workflow block of its own.
//
// It builds on writeTestConfig rather than writing a config from scratch,
// so every one of these tests runs against the same validated fixture the
// rest of the package uses and a schema change lands in one place.
//
// The root is EvalSymlinks'd because internal/workflow resolves it and
// compares stage containment against the resolved path, and a temporary
// directory on macOS lives under /var, which is a link to /private/var. A
// fixture that skipped that would be testing the symlink handling rather
// than the verbs.
func workflowFixture(t *testing.T) (configPath, root string) {
	t.Helper()
	configPath = writeTestConfig(t)

	dir := filepath.Dir(configPath)
	root = filepath.Join(dir, "workflows")
	for _, sub := range []string{"", "global-before", "global-after", "set-before"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", root, err)
	}
	root = resolved

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	block := "workflows:\n" +
		"  root: " + root + "\n" +
		"  global:\n" +
		"    before_dir: global-before\n" +
		"    after_dir: global-after\n" +
		"  script_timeout: 2m\n"
	if err := os.WriteFile(configPath, append(raw, []byte(block)...), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	return configPath, root
}

// TestUsage_NamesEveryWorkflowVerb closes the level
// TestUsage_EveryRegisteredCommandIsPinned cannot reach, for `workflow`.
//
// That test reads main.go's dispatch map, which has one entry for
// `workflow` and cannot see either level below it, so a
// `workflow run purge` added tomorrow would be dispatchable, absent from
// usage(), invisible to the black-box verb guard in the tests repository
// and pinned by nothing -- the exact shape of failure #549 was filed
// about. TestUsage_NamesEveryMediumVerb and
// TestUsage_NamesEveryWorkflowRunnerVerb are the same guard for the other
// two commands that have verbs.
//
// The verbs come off the real dispatch tables rather than a list typed
// here, so adding one is checked without anybody remembering this test
// exists.
func TestUsage_NamesEveryWorkflowVerb(t *testing.T) {
	nouns := workflowNounNames()
	if len(nouns) == 0 {
		t.Fatal("workflowNounNames() is empty, so `workflow` dispatches nothing at all and this test would pass vacuously")
	}

	out := captureStderr(t, usage)
	for _, noun := range nouns {
		verbs := workflowVerbNames(workflowNouns[noun])
		if len(verbs) == 0 {
			t.Errorf("workflow %s has no verbs, so the noun is dispatchable and does nothing", noun)
		}
		for _, verb := range verbs {
			if !strings.Contains(out, "workflow "+noun+" "+verb+" ") {
				t.Errorf("usage() does not list \"workflow %s %s\"; an operator cannot discover it, the black-box verb guard cannot see it, and nothing pins a word of what it prints",
					noun, verb)
			}
		}
	}
}

// TestUsage_NamesTheWorkflowConfigurationForms is the same guard for the
// three workflow forms that live under other nouns.
//
// They are not in any verb table: `settings workflow`, `backup-set
// workflow` and `validate workflow` are dispatched by their own commands
// from a literal, so nothing structural relates them to the usage block.
// This is that relation, written out, because the whole configuration
// half of #813 is reachable only through those three words and an
// operator who cannot find them has a feature that did not ship.
func TestUsage_NamesTheWorkflowConfigurationForms(t *testing.T) {
	out := captureStderr(t, usage)
	for _, form := range []string{
		"settings workflow ",
		"settings workflow patch ",
		"settings workflow env list ",
		"settings workflow env set ",
		"settings workflow env unset ",
		"backup-set workflow <source/backup-set>",
		"backup-set workflow patch ",
		"backup-set workflow env ",
		"validate workflow <source/backup-set>",
		"fetch --skip-workflow-scripts",
	} {
		if !strings.Contains(out, "  "+form) {
			t.Errorf("usage() does not list %q, which is the only reference an operator on a terminal has for it", form)
		}
	}
}

// TestWorkflowRefusesABadCommandLineBeforeOpeningAnything is the refusal
// table.
//
// Every row exits 2, and the config path is deliberately one that does
// NOT exist: a row that reached the service would fail to load it and
// exit 1, so the code itself proves the refusal happened on the command
// line rather than on the deployment. That is
// cliecho_test.go's own technique inverted -- it proves the good command
// lines get as far as the configuration, and this proves the bad ones do
// not.
func TestWorkflowRefusesABadCommandLineBeforeOpeningAnything(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "there-is-no-config-here.yaml")

	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"no noun", []string{"workflow"}},
		{"unknown noun", []string{"workflow", "runs"}},
		{"noun with no verb", []string{"workflow", "run"}},
		{"unknown run verb", []string{"workflow", "run", "purge", "--config", missing}},
		{"unknown recovery verb", []string{"workflow", "recovery", "clear", "--config", missing}},
		{"show with no run id", []string{"workflow", "run", "show", "--config", missing}},
		{"show with two run ids", []string{"workflow", "run", "show", "a", "b", "--config", missing}},
		{"steps with no run id", []string{"workflow", "run", "steps", "--config", missing}},
		{"log with no step", []string{"workflow", "run", "log", "wfr_1", "--config", missing}},
		{"log with a negative cursor", []string{"workflow", "run", "log", "wfr_1", "--step", "s1", "--cursor", "-1", "--config", missing}},
		{"list with a surplus operand", []string{"workflow", "run", "list", "wfr_1", "--config", missing}},
		{"list with a set name that is not an id", []string{"workflow", "run", "list", "--backup-set", "postgres-primary", "--config", missing}},
		{"resume-cleanup with no run id", []string{"workflow", "recovery", "resume-cleanup", "--config", missing}},
		{"acknowledge with no reason", []string{"workflow", "recovery", "acknowledge", "wfr_1", "--config", missing}},
		{"acknowledge with a blank reason", []string{"workflow", "recovery", "acknowledge", "wfr_1", "--reason", "   ", "--config", missing}},
		{"acknowledge with no run id", []string{"workflow", "recovery", "acknowledge", "--reason", "dealt with it", "--config", missing}},
		{"recovery show with a surplus operand", []string{"workflow", "recovery", "show", "wfr_1", "--config", missing}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			out := captureStderr(t, func() { code = run(tc.argv) })
			if code != exitUsage {
				t.Fatalf("run(%v) = %d, want %d (nothing ran, the command line was wrong); stderr:\n%s", tc.argv, code, exitUsage, out)
			}
		})
	}
}

// TestWorkflowConfigurationFormsRefuseABadCommandLine is the same table
// for the three forms under other nouns.
//
// `env set` with two sources and with none are the two rows that matter
// most: core/service refuses both as well, and reaching that refusal
// means opening a journal, taking this deployment's configuration-write
// claim and announcing a mode, so an operator who mistyped a flag beside
// a serving engine would meet a sentence about a daemon instead of about
// their command line.
func TestWorkflowConfigurationFormsRefuseABadCommandLine(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "there-is-no-config-here.yaml")

	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"settings workflow patch with no flag", []string{"settings", "workflow", "patch", "--config", missing}},
		{"settings workflow patch with an operand", []string{"settings", "workflow", "patch", "extra", "--config", missing}},
		{"settings workflow with a patch flag", []string{"settings", "workflow", "--root", "/workflows", "--config", missing}},
		{"settings workflow env with no verb", []string{"settings", "workflow", "env", "--config", missing}},
		{"settings workflow env unknown verb", []string{"settings", "workflow", "env", "show", "--config", missing}},
		{"settings workflow env set with no name", []string{"settings", "workflow", "env", "set", "--value", "x", "--config", missing}},
		{"settings workflow env set with no source", []string{"settings", "workflow", "env", "set", "PGHOST", "--config", missing}},
		{"settings workflow env set with two sources", []string{"settings", "workflow", "env", "set", "PGPASSWORD", "--value", "hunter2", "--secret-env", "PG_PW", "--config", missing}},
		{"settings workflow env set with a literal and a file", []string{"settings", "workflow", "env", "set", "PGPASSWORD", "--value", "", "--secret-file", "/run/secrets/pw", "--config", missing}},
		{"settings workflow env list with a set flag", []string{"settings", "workflow", "env", "list", "--value", "x", "--config", missing}},
		{"settings workflow env unset with no name", []string{"settings", "workflow", "env", "unset", "--config", missing}},
		{"backup-set workflow with no id", []string{"backup-set", "workflow", "--config", missing}},
		{"backup-set workflow with a bad id", []string{"backup-set", "workflow", "postgres-primary", "--config", missing}},
		{"backup-set workflow with an artifact id", []string{"backup-set", "workflow", "production/postgres-primary/backup.dump", "--config", missing}},
		{"backup-set workflow patch with no flag", []string{"backup-set", "workflow", "patch", "production/postgres-primary", "--config", missing}},
		{"backup-set workflow patch with no id", []string{"backup-set", "workflow", "patch", "--config", missing}},
		{"backup-set workflow env with no verb", []string{"backup-set", "workflow", "env", "production/postgres-primary", "--config", missing}},
		{"backup-set workflow env set with two sources", []string{"backup-set", "workflow", "env", "production/postgres-primary", "set", "PGPASSWORD", "--secret-env", "A", "--secret-file", "/b", "--config", missing}},
		{"validate workflow with no id", []string{"validate", "workflow", "--config", missing}},
		{"validate workflow with two ids", []string{"validate", "workflow", "a/b", "c/d", "--config", missing}},
		{"validate workflow with --content", []string{"validate", "workflow", "production/postgres-primary", "--content", "--config", missing}},
		{"validate artifact with --json", []string{"validate", "production/postgres-primary/backup.dump", "--json", "--config", missing}},
		{"fetch with --skip-workflow-scripts", []string{"fetch", "--backup-set", "production/postgres-primary", "--skip-workflow-scripts", "--config", missing}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			out := captureStderr(t, func() { code = run(tc.argv) })
			if code != exitUsage {
				t.Fatalf("run(%v) = %d, want %d (nothing ran, the command line was wrong); stderr:\n%s", tc.argv, code, exitUsage, out)
			}
		})
	}
}

// TestWorkflowJSONVerbsEmitDecodableObjects drives every --json read
// against a real deployment whose journal holds no workflow run, which is
// the state every deployment is in until a hook runs.
//
// The keys are asserted rather than the values, because what a script
// binds to is the shape: a list read that answered `null` instead of an
// empty array, or renamed its wrapper, breaks a caller that was working.
// An empty deployment is also the honest fixture for that -- it is where
// the "never null" rule is the only thing standing between a caller and a
// nil dereference.
func TestWorkflowJSONVerbsEmitDecodableObjects(t *testing.T) {
	configPath, _ := workflowFixture(t)

	for _, tc := range []struct {
		name string
		argv []string
		want []string
	}{
		{"run list", []string{"workflow", "run", "list", "--config", configPath, "--json"}, []string{"runs"}},
		{"recovery show", []string{"workflow", "recovery", "show", "--config", configPath, "--json"}, []string{"outstanding", "in_flight"}},
		{"settings workflow", []string{"settings", "workflow", "--config", configPath, "--json"}, []string{"Configured", "Root", "ScriptTimeout", "MaxScriptSizeBytes"}},
		{"settings workflow env list", []string{"settings", "workflow", "env", "list", "--config", configPath, "--json"}, []string{"backup_set_id", "environment"}},
		{"backup-set workflow", []string{"backup-set", "workflow", "production/postgres-primary", "--config", configPath, "--json"}, []string{"BackupSetID", "Stages", "EffectiveScriptTimeout"}},
		{"backup-set workflow env list", []string{"backup-set", "workflow", "env", "production/postgres-primary", "list", "--config", configPath, "--json"}, []string{"backup_set_id", "environment"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			out := captureStdout(t, func() { code = run(tc.argv) })
			if code != 0 {
				t.Fatalf("run(%v) = %d, want 0; stdout:\n%s", tc.argv, code, out)
			}

			var decoded map[string]json.RawMessage
			if err := json.Unmarshal([]byte(jsonObjectIn(t, out)), &decoded); err != nil {
				t.Fatalf("--json did not emit a decodable object: %v\n%s", err, out)
			}
			for _, key := range tc.want {
				if _, ok := decoded[key]; !ok {
					t.Errorf("--json object has no %q key; it has %v", key, sortedJSONKeys(decoded))
				}
			}
		})
	}
}

// TestWorkflowListJSONIsNeverNull pins the one thing an empty list read
// must not do.
//
// `null` and `[]` decode differently in every client this product has: a
// caller that ranges over the array works against one and dereferences
// nil against the other, and the deployment that produces `null` is the
// brand-new one nobody tests against by hand.
func TestWorkflowListJSONIsNeverNull(t *testing.T) {
	configPath, _ := workflowFixture(t)

	out := captureStdout(t, func() {
		if code := run([]string{"workflow", "run", "list", "--config", configPath, "--json"}); code != 0 {
			t.Fatalf("workflow run list --json = %d, want 0", code)
		}
	})
	var decoded struct {
		Runs []json.RawMessage `json:"runs"`
	}
	if err := json.Unmarshal([]byte(jsonObjectIn(t, out)), &decoded); err != nil {
		t.Fatalf("decoding: %v\n%s", err, out)
	}
	if decoded.Runs == nil {
		t.Errorf("an empty run list emitted null rather than an empty array:\n%s", out)
	}
}

// TestWorkflowRunReadsTreatAnUnknownRunAsAnOrdinaryFailure draws the
// line between this binary's 2 and its 1, on the one operand that cannot
// be checked before the journal is open.
//
// A run id is an opaque string this product minted, so unlike a backup
// set id there is no shape to refuse: anything non-empty could be one,
// and whether it names a run is a fact about the deployment. That makes
// it a 1 -- "a set or an artifact that is not there", as usage()'s
// exit-code table has it -- and a 2 would be this binary claiming the
// command line was wrong when it was not, which is the distinction a
// wrapper script branches on.
//
// It doubles as the proof that each of these verbs actually reaches the
// journal: a verb that refused its own arguments, or never opened
// anything, would come back 2 here.
func TestWorkflowRunReadsTreatAnUnknownRunAsAnOrdinaryFailure(t *testing.T) {
	configPath, _ := workflowFixture(t)

	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"show", []string{"workflow", "run", "show", "wfr_nothing-like-this", "--config", configPath}},
		{"steps", []string{"workflow", "run", "steps", "wfr_nothing-like-this", "--config", configPath}},
		{"log", []string{"workflow", "run", "log", "wfr_nothing-like-this", "--step", "01-quiesce", "--config", configPath}},
		{"resume-cleanup", []string{"workflow", "recovery", "resume-cleanup", "wfr_nothing-like-this", "--config", configPath}},
		{"acknowledge", []string{"workflow", "recovery", "acknowledge", "wfr_nothing-like-this", "--reason", "checked the machine by hand", "--config", configPath}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			out := captureStderr(t, func() {
				captureStdout(t, func() { code = run(tc.argv) })
			})
			if code != 1 {
				t.Fatalf("run(%v) = %d, want 1 (the command line was fine and the deployment has no such run); stderr:\n%s", tc.argv, code, out)
			}
		})
	}
}

// jsonObjectIn returns the JSON object inside a command's stdout.
//
// A direct write announces its mode on stdout beside its own report
// (mode.go argues why: a report is qualified by which world it was made
// in), so a write verb's output is a line of prose and then an object.
// This finds the object rather than making every test below care, and it
// fails loudly rather than silently returning "{}" -- a helper that
// tolerated no JSON at all would make every assertion in this file pass
// against a command that printed nothing.
func jsonObjectIn(t *testing.T, out string) string {
	t.Helper()
	start := strings.Index(out, "{")
	end := strings.LastIndex(out, "}")
	if start < 0 || end < start {
		t.Fatalf("no JSON object in this output:\n%s", out)
	}

	return out[start : end+1]
}

func sortedJSONKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	return keys
}
