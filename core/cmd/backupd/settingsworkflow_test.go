package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/service"
)

// The workflow configuration writes, end to end against a real
// deployment: what a secret reference prints, what it must never print,
// and the two places an explicitly-passed zero value is a request rather
// than an omission (EPIC L, #813).

// TestWorkflowEnvPrintsTheLocationOfASecretAndNeverItsValue is the one
// test in this file that would matter even if everything else here were
// deleted.
//
// A workflow environment variable can be a LOCATION -- a file, a
// variable name, a program -- and the whole premise of that design is
// that no surface of this product ever reports what is at that location.
// An `env list` that resolved a reference would be a command any operator
// (and anything that can run this binary) reads every credential in the
// deployment out of.
//
// So this configures all three kinds, with the real material actually
// present and reachable from this process, and asserts twice: that the
// location IS printed, because a reference nobody can see is a
// configuration nobody can debug, and that the material is NOT, on the
// write's own output and on the read after it.
//
// The command reference is `cat <file>` rather than something like
// `printf hunter2` on purpose. An argv is printed in full, because the
// argv IS the location, so a secret typed INTO a command line would
// appear -- and that is the operator's own doing and not something this
// surface can undo. What must never happen is this product RUNNING the
// program and printing what came back, which is what the file's contents
// below are there to catch.
func TestWorkflowEnvPrintsTheLocationOfASecretAndNeverItsValue(t *testing.T) {
	configPath, _ := workflowFixture(t)

	const (
		fileMaterial    = "material-in-a-file-nothing-may-print"
		envMaterial     = "material-in-a-variable-nothing-may-print"
		commandMaterial = "material-a-command-would-print"
	)

	dir := filepath.Dir(configPath)
	secretFile := filepath.Join(dir, "pgpassword")
	if err := os.WriteFile(secretFile, []byte(fileMaterial), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	commandFile := filepath.Join(dir, "token")
	if err := os.WriteFile(commandFile, []byte(commandMaterial), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Present in this process's own environment, so a surface that
	// resolved by name would succeed rather than fail and look innocent.
	t.Setenv("PG_PW_FOR_TEST", envMaterial)

	writes := [][]string{
		{"settings", "workflow", "env", "set", "PGPASSWORD", "--secret-file", secretFile, "--config", configPath},
		{"settings", "workflow", "env", "set", "PGPASSWORD_FROM_ENV", "--secret-env", "PG_PW_FOR_TEST", "--config", configPath},
		{"settings", "workflow", "env", "set", "VAULT_TOKEN", "--secret-command", "cat", "--secret-command", commandFile, "--config", configPath},
	}

	var everything strings.Builder
	for _, argv := range writes {
		var code int
		out := captureStdout(t, func() { code = run(argv) })
		if code != 0 {
			t.Fatalf("run(%v) = %d, want 0; stdout:\n%s", argv, code, out)
		}
		everything.WriteString(out)
	}

	listed := captureStdout(t, func() {
		if code := run([]string{"settings", "workflow", "env", "list", "--config", configPath}); code != 0 {
			t.Fatalf("settings workflow env list = %d, want 0", code)
		}
	})
	everything.WriteString(listed)

	// The locations, on the read: an operator debugging a hook that
	// cannot see its password needs to know which file, which variable
	// and which program this deployment will look in.
	for _, location := range []string{secretFile, "PG_PW_FOR_TEST", "cat " + commandFile} {
		if !strings.Contains(listed, location) {
			t.Errorf("env list does not name the location %q, so a reference is configured that nobody can debug:\n%s", location, listed)
		}
	}
	for _, name := range []string{"PGPASSWORD", "PGPASSWORD_FROM_ENV", "VAULT_TOKEN"} {
		if !strings.Contains(listed, name) {
			t.Errorf("env list does not name the variable %q:\n%s", name, listed)
		}
	}

	// And never the material, on any of the four outputs.
	all := everything.String()
	for _, material := range []string{fileMaterial, envMaterial, commandMaterial} {
		if strings.Contains(all, material) {
			t.Fatalf("a workflow env surface printed secret material (%q). Every one of these surfaces reports a LOCATION and nothing else:\n%s", material, all)
		}
	}

	// The durable half: what was written is a reference too, so a later
	// read of config.yaml by anything at all finds no material either.
	saved := readFile(t, configPath)
	for _, material := range []string{fileMaterial, envMaterial, commandMaterial} {
		if strings.Contains(saved, material) {
			t.Fatalf("config.yaml now holds secret material (%q) rather than a reference to it:\n%s", material, saved)
		}
	}
}

// TestWorkflowEnvCarriesNoFieldAResolvedSecretCouldLiveIn is the
// structural half of the test above, and it is the reason that one can be
// as short as it is.
//
// The behavioural test proves today's surfaces print no material. This
// proves they CANNOT, which is a different and stronger claim, and it is
// true for exactly one reason: the type the CLI prints carries a name, a
// literal and a location, and has nowhere to put a resolved value. A
// field added to either of these types -- `Resolved`, `Current`,
// `Effective` -- would make every printer in this package one line away
// from being the surface an attacker reads the deployment's credentials
// out of, and nothing else in this repository would notice.
//
// So this is not a style assertion about a struct. It is the place where
// that addition has to be argued for, and the field lists are spelled out
// so the diff says what was added.
func TestWorkflowEnvCarriesNoFieldAResolvedSecretCouldLiveIn(t *testing.T) {
	for _, tc := range []struct {
		typ  reflect.Type
		want []string
	}{
		{reflect.TypeOf(service.WorkflowEnvVar{}), []string{"HasValue", "Name", "Secret", "Value"}},
		{reflect.TypeOf(service.WorkflowSecretRef{}), []string{"Command", "Env", "File"}},
	} {
		var got []string
		for i := range tc.typ.NumField() {
			got = append(got, tc.typ.Field(i).Name)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s has fields %v, and this package's printers were written against %v.\nIf a field was ADDED, say in the commit message why a value resolved from a secret reference cannot reach it, and check every printer in settingsworkflow.go before changing this list.",
				tc.typ, got, tc.want)
		}
	}
}

// TestWorkflowEnvSetConfiguresAnExplicitlyEmptyValue is the fs.Visit rule
// where it is load bearing.
//
// `--value ""` is a variable configured as an empty string, which is a
// different configuration from a variable that has no literal at all:
// core/service carries the difference as HasValue, config.yaml writes it
// as `value: ""`, and operators write it deliberately (a hook that checks
// whether a variable is SET behaves differently from one reading an empty
// one). Read off the flag's value rather than through fs.Visit, this
// command line would be "no source named" and would be refused as a
// mistake the operator did not make.
func TestWorkflowEnvSetConfiguresAnExplicitlyEmptyValue(t *testing.T) {
	configPath, _ := workflowFixture(t)

	argv := []string{"settings", "workflow", "env", "set", "PGOPTIONS", "--value", "", "--config", configPath}
	var code int
	out := captureStdout(t, func() { code = run(argv) })
	if code != 0 {
		t.Fatalf("run(%v) = %d, want 0; stdout:\n%s", argv, code, out)
	}
	if !strings.Contains(out, "PGOPTIONS") || !strings.Contains(out, "empty string") {
		t.Errorf("an explicitly empty value was not reported as one, so the output cannot be told from an unconfigured variable:\n%s", out)
	}

	// The durable half. This is what distinguishes the two states for
	// every later reader, including the engine that hands the variable to
	// a hook.
	if saved := readFile(t, configPath); !strings.Contains(saved, `value: ""`) {
		t.Errorf("config.yaml does not record the empty literal, so the variable was written as having no value at all:\n%s", saved)
	}
}

// TestWorkflowSettingsPatchClearsWithAnExplicitEmptyValue is the same
// rule on the deployment-wide block, where clearing is an operation with
// consequences: a stage directory that is cleared stops running.
func TestWorkflowSettingsPatchClearsWithAnExplicitEmptyValue(t *testing.T) {
	configPath, _ := workflowFixture(t)

	before := captureStdout(t, func() {
		if code := run([]string{"settings", "workflow", "--config", configPath}); code != 0 {
			t.Fatalf("settings workflow = %d, want 0", code)
		}
	})
	if !strings.Contains(before, "global-before") {
		t.Fatalf("the fixture does not configure a before stage, so this test cannot clear one:\n%s", before)
	}

	argv := []string{"settings", "workflow", "patch", "--before-dir", "", "--config", configPath}
	var code int
	out := captureStdout(t, func() { code = run(argv) })
	if code != 0 {
		t.Fatalf("run(%v) = %d, want 0; stdout:\n%s", argv, code, out)
	}
	if strings.Contains(out, "global-before") {
		t.Errorf("--before-dir \"\" did not clear the stage; the patch was read as an omission:\n%s", out)
	}
	if !strings.Contains(out, "this stage runs nothing") {
		t.Errorf("a cleared stage was printed as a blank rather than as a stage that runs nothing:\n%s", out)
	}
}

// TestBackupSetWorkflowPatchPinsAndUnpinsATimeout walks the per-set half
// of the same rule, in the order an operator actually does it: pin a
// short timeout to debug one slow hook, then give the pin back.
//
// The second patch is `--script-timeout 0`, and what it must do is return
// the set to the DEPLOYMENT's bound rather than to no bound at all. Both
// halves are asserted, because an implementation that dropped the pin and
// reported nothing about inheritance would look identical on the first
// line and leave an operator unable to tell whether their hooks now have
// two minutes or five.
func TestBackupSetWorkflowPatchPinsAndUnpinsATimeout(t *testing.T) {
	configPath, _ := workflowFixture(t)
	const set = "production/postgres-primary"

	pinned := captureStdout(t, func() {
		argv := []string{"backup-set", "workflow", "patch", set, "--before-dir", "set-before", "--script-timeout", "30s", "--config", configPath}
		if code := run(argv); code != 0 {
			t.Fatalf("run(%v) = %d, want 0", argv, code)
		}
	})
	if !strings.Contains(pinned, "30s (pinned by this set)") {
		t.Errorf("the set's own timeout was not reported as pinned:\n%s", pinned)
	}
	if !strings.Contains(pinned, "set-before") {
		t.Errorf("the set's own before stage was not reported:\n%s", pinned)
	}

	unpinned := captureStdout(t, func() {
		argv := []string{"backup-set", "workflow", "patch", set, "--script-timeout", "0", "--config", configPath}
		if code := run(argv); code != 0 {
			t.Fatalf("run(%v) = %d, want 0", argv, code)
		}
	})
	if !strings.Contains(unpinned, "2m0s (inherited; this set pins none)") {
		t.Errorf("--script-timeout 0 did not return this set to the deployment's 2m bound:\n%s", unpinned)
	}
	// The rest of the block has to survive a patch that named one field,
	// which is what makes this a patch rather than a replacement.
	if !strings.Contains(unpinned, "set-before") {
		t.Errorf("clearing the timeout also dropped the before stage this set configures:\n%s", unpinned)
	}
}

// TestWorkflowEnvUnsetRefusesAVariableThatIsNotThere pins the refusal
// rather than the success, because the success is unremarkable and the
// refusal is the whole point: an `unset PGPASSWORD` that quietly does
// nothing is indistinguishable, from a script, from one that worked, and
// the case it hides is an operator clearing a credential from the wrong
// scope and believing they have cleared it.
//
// Exit 1 and not 2: the command line is fine and the deployment is what
// disagrees, which is the line usage()'s exit-code table draws.
func TestWorkflowEnvUnsetRefusesAVariableThatIsNotThere(t *testing.T) {
	configPath, _ := workflowFixture(t)

	var code int
	out := captureStderr(t, func() {
		code = run([]string{"settings", "workflow", "env", "unset", "NEVER_CONFIGURED", "--config", configPath})
	})
	if code != 1 {
		t.Fatalf("unsetting a variable that is not configured = %d, want 1; stderr:\n%s", code, out)
	}
}

// TestSettingsWorkflowReadsThroughTheSettingsNoun proves the dispatch,
// which is the one part of this surface that is not a function call.
//
// cmdSettings scans its arguments for the word before parsing anything,
// so a flag written before it has to reach the same parse. If that scan
// were replaced by "the first operand is the verb", `settings --config X
// workflow` would run against the DEFAULT configuration path, which on a
// developer machine is a file that is not there and on a real deployment
// is the live one -- the identical failure
// TestRun_BackupSetRetentionAcceptsFlagsOnEitherSideOfTheVerb exists for.
func TestSettingsWorkflowReadsThroughTheSettingsNoun(t *testing.T) {
	configPath, root := workflowFixture(t)

	out := captureStdout(t, func() {
		argv := []string{"settings", "--config", configPath, "workflow"}
		if code := run(argv); code != 0 {
			t.Fatalf("run(%v) = %d, want 0", argv, code)
		}
	})
	if !strings.Contains(out, root) {
		t.Errorf("a --config written before the word \"workflow\" did not reach the command:\n%s", out)
	}
}
