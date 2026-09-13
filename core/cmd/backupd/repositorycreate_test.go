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

func TestRepositoryCreate_PersistsADeclarationTheNextInvocationSees(t *testing.T) {
	configPath := aDeploymentWithSnapshots(t)
	passphrase := filepath.Join(filepath.Dir(configPath), "offsite.passphrase")
	if err := os.WriteFile(passphrase, []byte("another-passphrase-long-enough-to-be-one"), 0o600); err != nil {
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
	if err := os.WriteFile(passphrase, []byte("another-passphrase-long-enough-to-be-one"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before, err := os.ReadFile(configPath) //nolint:gosec // a path this test created.
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	t.Setenv("BACKUPD_INCREMENTAL_ENGINE", "0")

	code := run([]string{
		"repository", "--config", configPath, "create", "offsite-b2",
		"--isolation", "shared",
		"--passphrase-file", passphrase,
	})
	if code == exitOK {
		t.Fatal("repository create succeeded on a deployment with the incremental engine gated off")
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
			"--passphrase-file", "/etc/backupd/p", "--passphrase-env", "BACKUPD_P",
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
