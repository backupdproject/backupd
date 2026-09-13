package config

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The three things about a workflow's configuration that only the YAML
// layer can decide, tested through the YAML rather than through struct
// literals: whether a key was WRITTEN.

// A set that declares only a connection is configuring the deployment's
// GLOBAL stages as they apply to it, and that is a correct configuration
// rather than an empty block.
//
// The shape is ordinary and the first version of this validator refused
// it: /workflows/global-before holds 00-quiesce.remote.sh, which runs on
// the machine each set pulls FROM -- so the connection it runs over is
// necessarily per-set, and the set's own block is the only place to
// declare it. Refusing the block left an operator with a global remote
// hook and nowhere to say where it runs.
func TestASetMayConfigureOnlyAConnectionWhenGlobalStagesRun(t *testing.T) {
	t.Parallel()

	const source = `
poll_interval: 15m
state:
  database: /var/lib/backupd/state.db
workflows:
  root: /workflows
  global:
    before_dir: global-before
sources:
  - id: production
    backup_sets:
      - id: db
        remote:
          type: local
        remote_path: /srv/incoming
        local_path: /backups/production/db
        completion:
          strategy: rename
        stale_after: 30h
        workflow:
          remote_exec_connection_ref: production/db
`

	cfg := loadYAML(t, source)

	if err := cfg.Validate(); err != nil {
		t.Fatalf("a set declaring only remote_exec_connection_ref was refused, and a global .remote.sh hook has nowhere else to say where it runs: %v", err)
	}

	bs := &cfg.Sources[0].BackupSets[0]

	stages := cfg.WorkflowStagesFor(bs)
	if len(stages) != 1 {
		t.Fatalf("the set resolved %d stages, want the one global stage", len(stages))
	}
	if bs.Workflow.RemoteExecConnectionRef != "production/db" {
		t.Errorf("the connection came back as %q", bs.Workflow.RemoteExecConnectionRef)
	}

	// A timeout-only block is the same case: a per-set bound on hooks
	// that come from the global directory.
	timeoutOnly := loadYAML(t, strings.Replace(source,
		"          remote_exec_connection_ref: production/db",
		"          script_timeout: 45s", 1))

	if err := timeoutOnly.Validate(); err != nil {
		t.Errorf("a set declaring only script_timeout was refused: %v", err)
	}
}

// Presence, at the layer that has it. Each of these decodes to something
// a value-typed schema could not tell apart from something else, and the
// difference is what an operator gets told.
func TestWorkflowEnvironmentPresenceIsRepresentable(t *testing.T) {
	t.Parallel()

	withEnv := func(entry string) string {
		return `
poll_interval: 15m
state:
  database: /var/lib/backupd/state.db
workflows:
  root: /workflows
  global:
    before_dir: global-before
  environment:
` + entry + `
sources:
  - id: production
    backup_sets:
      - id: db
        remote:
          type: local
        remote_path: /srv/incoming
        local_path: /backups/production/db
        completion:
          strategy: rename
        stale_after: 30h
`
	}

	t.Run("an empty from_secret is refused rather than read as an empty literal", func(t *testing.T) {
		t.Parallel()

		cfg := loadYAML(t, withEnv("    - name: PGPASSWORD\n      from_secret: {}"))

		if cfg.Workflows.Environment[0].FromSecret == nil {
			t.Fatal("`from_secret: {}` decoded to no from_secret at all, so the schema cannot tell a written key from an absent one -- which is how a secret-backed variable becomes a silently empty literal")
		}

		err := cfg.Validate()
		if err == nil {
			t.Fatal("`from_secret: {}` was accepted; the variable would be an empty literal and the hook would get an empty credential")
		}
		if !strings.Contains(err.Error(), "names no location") {
			t.Errorf("Validate said:\n\t%v\nwant it to say the block names no location", err)
		}
	})

	t.Run("an absent value key is not an empty literal", func(t *testing.T) {
		t.Parallel()

		cfg := loadYAML(t, withEnv("    - name: PGPASSWORD\n      from_secret:\n        file: /run/secrets/db.pw"))

		if cfg.Workflows.Environment[0].Value != nil {
			t.Errorf("a variable that wrote no value key decoded with one (%q)", *cfg.Workflows.Environment[0].Value)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("a secret-backed variable with no value key was refused: %v", err)
		}
	})

	t.Run("an explicitly empty value is a real configuration", func(t *testing.T) {
		t.Parallel()

		cfg := loadYAML(t, withEnv(`    - name: PGOPTIONS
      value: ""`))

		entry := cfg.Workflows.Environment[0]
		if entry.Value == nil {
			t.Fatal("`value: \"\"` decoded to no value at all; an operator who writes it means an empty variable")
		}
		if *entry.Value != "" {
			t.Errorf("the literal decoded as %q", *entry.Value)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("an explicitly empty literal was refused: %v", err)
		}
	})

	t.Run("an empty value beside a secret is refused", func(t *testing.T) {
		t.Parallel()

		cfg := loadYAML(t, withEnv(`    - name: PGPASSWORD
      value: ""
      from_secret:
        file: /run/secrets/db.pw`))

		err := cfg.Validate()
		if err == nil {
			t.Fatal("a variable declaring both an empty literal and a secret was accepted; one of the two is being ignored and it keeps working, so nobody finds out which")
		}
		if !strings.Contains(err.Error(), "declares both value and from_secret") {
			t.Errorf("Validate said:\n\t%v\nwant it to name both keys", err)
		}
	})
}

// #808's "no resolved secret in any persisted artifact", held against the
// CONFIG artifact.
//
// internal/state holds the same line against the journal's bytes and
// internal/workflow holds it against the spool. This is the third place a
// resolved value could end up, and the one with the most innocent-looking
// route to it: core/service re-marshals the whole Config on every settings
// save (FR-35), so any field that came to hold resolved material would be
// written into config.yaml on the operator's next unrelated click.
func TestAResolvedSecretReachesNoMarshaledConfigByte(t *testing.T) {
	t.Parallel()

	const sentinel = "sentinel-41b7e2-never-in-a-config-file"

	secretFile := filepath.Join(t.TempDir(), "db.pw")
	if err := os.WriteFile(secretFile, []byte(sentinel+"\n"), 0o600); err != nil {
		t.Fatalf("writing the secret fixture: %v", err)
	}

	cfg := workflowConfig()
	cfg.Workflows.Environment = []EnvironmentVariable{
		{Name: "PGPASSWORD", FromSecret: &SecretSource{File: secretFile}},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	bs := &cfg.Sources[0].BackupSets[0]

	resolved, err := bs.WorkflowEnvironment.Resolve(context.Background(), map[string]string{})
	if err != nil {
		t.Fatalf("resolving the set's workflow environment: %v", err)
	}

	found := false
	for _, kv := range resolved.Environ() {
		if kv == "PGPASSWORD="+sentinel {
			found = true
		}
	}
	if !found {
		t.Fatal("the fixture's secret did not resolve to the sentinel, so searching the marshaled config for it proves nothing")
	}

	// The marshal core/service's writeConfigAtomically feeds the file
	// from, AFTER the resolution: a field that picked the value up would
	// carry it here.
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if bytes.Contains(encoded, []byte(sentinel)) {
		t.Errorf("the marshaled config contains the resolved secret:\n%s\n\nEvery settings save rewrites this file, so a resolved value reaching it is a credential written to disk in the clear by a click on an unrelated page", encoded)
	}

	// And the location is still there, because that is what the file is
	// supposed to hold -- and what keeps the search above from passing on
	// an empty document.
	if !bytes.Contains(encoded, []byte(secretFile)) {
		t.Errorf("the marshaled config lost the secret's location (%s):\n%s", secretFile, encoded)
	}
}

// loadYAML writes a config document to a temporary file and loads it
// through Load, so the test exercises the decoder the daemon uses --
// KnownFields(true) included, which is what makes a misspelled key a
// refusal rather than a silently ignored line.
func loadYAML(t *testing.T, document string) *Config {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	return cfg
}
