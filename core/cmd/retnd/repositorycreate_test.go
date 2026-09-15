package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `backupd repository create` (issue #862): the CLI half of POST
// /repositories, through the same write door.
//
// The persistence assertion here is deliberately a SECOND INVOCATION
// rather than a look at what the first one printed. Both runs are their
// own process-level unit of work in this package's terms -- their own
// service, their own load of config.yaml -- so a create that only
// hot-reloaded its own copy passes nothing here, and the failure that
// would hide is the one an operator meets: a domain declared from a
// terminal that the next command, and the engine, never sees.
//
// The secret is a canary for the same reason the service test's is: what
// the declaration persists is a REFERENCE to the file, so the file's own
// content must be findable nowhere in config.yaml.

// testCLIRepositoryPassphrase is obviously fake and is written only into
// a file these tests' own temp directory owns.
const testCLIRepositoryPassphrase = "EXAMPLE-CLI-REPOSITORY-PASSPHRASE-NOT-A-REAL-ONE"

func TestRepositoryCreate_PersistsADeclarationTheNextInvocationSees(t *testing.T) {
	configPath := aDeploymentWithSnapshots(t)
	passphrase := filepath.Join(filepath.Dir(configPath), "offsite.passphrase")
	if err := os.WriteFile(passphrase, []byte(testCLIRepositoryPassphrase), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var code int
	stdout := captureStdout(t, func() {
		code = run([]string{
			"repository", "--config", configPath, "create", "offsite-b2",
			"--isolation", "isolated",
			"--description", "Second copy, off site",
			"--passphrase-file", passphrase,
		})
	})
	if code != exitOK {
		t.Fatalf("repository create = %d, want %d; it printed %q", code, exitOK, stdout)
	}
	if !strings.Contains(stdout, "offsite-b2") {
		t.Errorf("the create printed nothing naming the domain it declared:\n%s", stdout)
	}

	// The file, and the reference rather than the secret.
	raw, err := os.ReadFile(configPath) //nolint:gosec // a path this test created.
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "id: offsite-b2") {
		t.Fatalf("config.yaml does not declare the domain:\n%s", raw)
	}
	if !strings.Contains(string(raw), "file: "+passphrase) {
		t.Errorf("config.yaml carries no passphrase reference to %s:\n%s", passphrase, raw)
	}
	// The secret itself, which the declaration references and must never
	// copy, exactly as the service test asserts one layer down.
	if strings.Contains(string(raw), testCLIRepositoryPassphrase) {
		t.Fatalf("the passphrase itself is in config.yaml:\n%s", raw)
	}

	// The second, independent invocation: a fresh service, a fresh load
	// of the file, and the domain has to be there.
	second := captureStdout(t, func() {
		code = run([]string{"repository", "--config", configPath, "health"})
	})
	if !strings.Contains(second, "offsite-b2") {
		t.Errorf("a later `repository health` does not report the declared domain, so the create did not persist:\n%s", second)
	}
}

// The gate, at the terminal. An operator whose deployment does not run
// the incremental engine is told which flag to set, and nothing is
// written.
func TestRepositoryCreate_IsRefusedWhenTheIncrementalEngineIsGatedOff(t *testing.T) {
	configPath := aDeploymentWithSnapshots(t)
	passphrase := filepath.Join(filepath.Dir(configPath), "offsite.passphrase")
	if err := os.WriteFile(passphrase, []byte(testCLIRepositoryPassphrase), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before, err := os.ReadFile(configPath) //nolint:gosec // a path this test created.
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	t.Setenv("RETND_INCREMENTAL_ENGINE", "0")

	var code int
	stderr := captureStderr(t, func() {
		code = run([]string{
			"repository", "--config", configPath, "create", "offsite-b2",
			"--isolation", "shared",
			"--passphrase-file", passphrase,
		})
	})
	// exitFailure specifically, not merely "not exitOK": exitUsage is
	// what a mistyped invocation gets, and a gated deployment is not a
	// mistyped invocation -- the command was right and this deployment
	// cannot honour it, which is the difference between "fix your
	// command line" and "turn the engine on".
	if code != exitFailure {
		t.Fatalf("repository create on a gated deployment = %d, want exitFailure (%d); it printed %q", code, exitFailure, stderr)
	}
	// And the one place an operator is told which key to set. A refusal
	// that did not name it leaves them with a command that will not work
	// and nothing to change.
	if !strings.Contains(stderr, "incremental_engine.enabled") {
		t.Errorf("the refusal does not name the flag that answers it:\n%s", stderr)
	}

	after, err := os.ReadFile(configPath) //nolint:gosec // a path this test created.
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(after) != string(before) {
		t.Error("a refused create changed config.yaml")
	}
}

// The arity and vocabulary refusals are usage errors, not failures: an
// operator who typed the verb wrong gets exit 2 and a sentence, and
// nothing opens a configuration to find that out.
func TestRepositoryCreate_RefusesAMalformedInvocation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no domain id", []string{"repository", "create", "--isolation", "shared"}},
		{"two domain ids", []string{"repository", "create", "a", "b", "--isolation", "shared"}},
		{"no isolation, which has no default", []string{"repository", "create", "offsite-b2"}},
		{"no passphrase reference", []string{"repository", "create", "offsite-b2", "--isolation", "shared"}},
		{"two passphrase references", []string{
			"repository", "create", "offsite-b2", "--isolation", "shared",
			"--passphrase-file", "/etc/backupd/p", "--passphrase-env", "RETND_P",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A config path that does not exist, so a case that reached
			// the write door would fail for the wrong reason and this
			// test would still be checking what it says it checks.
			args := append(append([]string{}, tc.args...), "--config", "/nonexistent/no-such-config.yaml")
			if code := run(args); code != exitUsage {
				t.Fatalf("repository create (%s) = %d, want %d", tc.name, code, exitUsage)
			}
		})
	}
}
