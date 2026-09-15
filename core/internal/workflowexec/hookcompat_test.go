package workflowexec

import (
	"context"
	"strings"
	"testing"

	"github.com/retnd/retnd/core/internal/workflow"
)

// EPIC R's hook-environment compat window (#889, FR-37), proved the only
// way it can be: by running an operator's script under a real bash and
// reading what the script saw.
//
// This is the epic's silent class. A hook is somebody else's Bash file,
// `$BACKUPD_BACKUP_STATUS` against a build that stopped exporting that
// name is the empty string and not an error, and a hook that notifies on
// failure would go quiet without anything reporting it. So both names
// are exported for one release with identical values, and the evidence
// that has to exist is a hook printing each one.
//
// Everything below goes through the real assembly path --
// workflow.Environment.Resolve, then ProcessEnv and StdinPayload, then
// `bash --noprofile --norc -s` -- because the mistake worth catching is
// not "the map has two keys": it is a name that survives the map and is
// lost by the envelope, the quoting or the baseline sanitiser.

// hookFacts is one step's built-ins, as internal/workflowrun writes them:
// every documented name, with a value distinct enough that a mix-up
// between two of them is visible.
func hookFacts() map[string]string {
	return map[string]string{
		"RETND":                   "1",
		"RETND_RUN_ID":            "run-7",
		"RETND_BACKUP_SET_ID":     "prod/postgres-primary",
		"RETND_BACKUP_SET_NAME":   "postgres-primary",
		"RETND_PHASE":             "after",
		"RETND_STEP_ID":           "step-3",
		"RETND_STEP_NAME":         "20-unmount.local.sh",
		"RETND_STEP_TARGET":       "local",
		workflow.EnvSourceHost:    "db01",
		workflow.EnvSourcePath:    "/srv/pg",
		workflow.EnvDestination:   "/mnt/backups/prod",
		"RETND_WORK_DIR":          "/var/lib/retnd/workflow/run-7",
		"RETND_BACKUP_STATUS":     "failed",
		"RETND_WORKFLOW_STATUS":   "failed",
		"RETND_CLEANUP_STATUS":    "unknown",
		"RETND_BACKUP_ERROR_CODE": "TRANSPORT_LOST",
		"RETND_STARTED_AT":        "2026-01-02T03:04:05Z",
		"RETND_RECOVERY":          "1",
		"RETND_CLEANUP_REASON":    "interrupted_run",
	}
}

// runHook assembles a real hook environment and runs script under a real
// bash, returning what the script printed.
func runHook(t *testing.T, script string) string {
	t.Helper()

	env, err := workflow.NewEnvironment(workflow.SanitizedBaseline())
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	resolved, err := env.Resolve(context.Background(), hookFacts())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	environ, err := ProcessEnv(nil, resolved.Environ())
	if err != nil {
		t.Fatalf("ProcessEnv: %v", err)
	}
	payload, err := StdinPayload(environ, []byte(script))
	if err != nil {
		t.Fatalf("StdinPayload: %v", err)
	}

	stdout, stderr, exit := runUnderRealBash(t, hostileParent(), payload)
	if exit != 0 {
		t.Fatalf("hook exited %d\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	}

	return stdout
}

// The acceptance case in its own words: one hook prints the deprecated
// name, another prints the current one, both run against this build, and
// the two have to print the same thing.
//
// Two separate executions rather than one script printing both, because
// the two scripts are what an operator actually has: one written before
// the rename and one written after.
func TestAHookReadingTheDeprecatedNameSeesWhatTheCurrentNameSays(t *testing.T) {
	t.Parallel()

	deprecated := runHook(t, `printf 'status=%s\n' "$BACKUPD_BACKUP_STATUS"`)
	current := runHook(t, `printf 'status=%s\n' "$RETND_BACKUP_STATUS"`)

	if deprecated != current {
		t.Fatalf("a hook reading $BACKUPD_BACKUP_STATUS printed %q and one reading $RETND_BACKUP_STATUS printed %q; the compat window is identical values",
			deprecated, current)
	}
	if strings.TrimSpace(deprecated) != "status=failed" {
		t.Fatalf("hook printed %q, want status=failed: an empty value is exactly the silent failure this window exists to prevent", deprecated)
	}
}

// The same property for the whole documented set, including the bare
// name, so a variable cannot be missed by not being the one the case
// above happened to pick.
func TestEveryBuiltinReachesAHookUnderBothNames(t *testing.T) {
	t.Parallel()

	var script strings.Builder
	for _, name := range workflow.BuiltinEnvNames() {
		legacy := workflow.LegacyEnvName(name)
		if legacy == "" {
			t.Fatalf("%s has no deprecated spelling, so this test cannot check it", name)
		}
		script.WriteString(`printf '` + name + `=%s\n' "$` + name + "\"\n")
		script.WriteString(`printf '` + legacy + `=%s\n' "$` + legacy + "\"\n")
	}

	printed := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(runHook(t, script.String())), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("unparseable hook output line %q", line)
		}
		printed[name] = value
	}

	facts := hookFacts()
	for _, name := range workflow.BuiltinEnvNames() {
		legacy := workflow.LegacyEnvName(name)
		switch {
		case printed[name] != facts[name]:
			t.Errorf("$%s = %q, want %q", name, printed[name], facts[name])
		case printed[legacy] != facts[name]:
			t.Errorf("$%s = %q, want %q (the deprecated name has to carry the same value)", legacy, printed[legacy], facts[name])
		}
	}
}
