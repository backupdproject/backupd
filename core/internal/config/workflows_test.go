package config

import (
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/secretref"
	"github.com/backupdproject/backupd/core/internal/workflow"
	"gopkg.in/yaml.v3"
)

// The workflow schema's suite (EPIC L, #808).
//
// Its first and most important test is the one that asserts NOTHING
// happened: a configuration with no workflow block has to come back out of
// this package byte-identical to what it was before the schema grew these
// keys. Everything else here is the ordinary business of a new config
// block -- required keys, refused shapes, inheritance -- and the golden
// test is the one that would catch this whole change having been a
// mistake.

// workflowKeyLine matches every YAML mapping key this change introduces,
// at any indentation. FR-35 forbids any of them appearing in a file that
// configured no workflow: core/service re-marshals the whole Config on
// every settings save, and a key injected into a file that never asked for
// it is refused outright by an older binary under Load's KnownFields(true).
var workflowKeyLine = regexp.MustCompile(`(?m)^\s*(workflows|workflow|environment|root|before_dir|after_dir|script_timeout|max_script_size_bytes|remote_exec_connection_ref|from_secret):`)

// TestMarshal_ANoWorkflowConfigGainsNoWorkflowKeys is #808's first
// acceptance criterion, held at the one place it can be held byte for
// byte: the marshaler core/service's writeConfigAtomically feeds the file
// from.
//
// The second half is the one that can actually fail in practice. A
// settings form that renders a workflow section, submitted unchanged on a
// config that configured none, hands this struct empty strings and empty
// slices; without omitempty those marshal to "workflow: {}" and
// "environment: []" on a file that never opted in.
func TestMarshal_ANoWorkflowConfigGainsNoWorkflowKeys(t *testing.T) {
	for _, fixture := range []string{"testdata/full.yaml", "testdata/minimal.yaml"} {
		t.Run(fixture, func(t *testing.T) {
			cfg, err := Load(fixture)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}

			base, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if found := workflowKeyLine.FindAllString(string(base), -1); len(found) != 0 {
				t.Errorf("a config that configures no workflow came back from a re-marshal carrying %v:\n%s", found, base)
			}

			cfg.Workflows = Workflows{}
			for i := range cfg.Sources {
				for j := range cfg.Sources[i].BackupSets {
					bs := &cfg.Sources[i].BackupSets[j]
					bs.Workflow = nil
					bs.Environment = []EnvironmentVariable{}
				}
			}

			afterEmptySubmission, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(base) != string(afterEmptySubmission) {
				t.Errorf("an explicitly-empty workflow submission changed the marshaled file.\nbefore:\n%s\nafter:\n%s", base, afterEmptySubmission)
			}
		})
	}

	// Positive control: the scan does find the keys when they are there,
	// so the absences above are evidence rather than a dead regexp.
	cfg := workflowConfig()
	encoded, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("Marshal a config that does configure a workflow: %v", err)
	}
	if found := workflowKeyLine.FindAllString(string(encoded), -1); len(found) < 8 {
		t.Errorf("positive control: a config with a workflow must marshal every key it wrote, got %v:\n%s", found, encoded)
	}
}

// A set with no workflow and no environment resolves to the same thing it
// always did: no stages, no environment beyond the baseline, and nothing
// that would make an artifact run behave differently.
func TestASetWithNoWorkflowResolvesToNoWorkflowAtAll(t *testing.T) {
	t.Parallel()

	cfg, err := Load("testdata/full.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if cfg.WorkflowsConfigured() {
		t.Error("a config with no workflows block reports workflows configured")
	}

	sets := 0
	for i := range cfg.Sources {
		for j := range cfg.Sources[i].BackupSets {
			sets++
			bs := &cfg.Sources[i].BackupSets[j]

			if stages := cfg.WorkflowStagesFor(bs); len(stages) != 0 {
				t.Errorf("%s resolved %d workflow stages", bs.Name, len(stages))
			}
			if names := bs.WorkflowEnvironment.Names(); len(names) != 0 {
				t.Errorf("%s resolved a workflow environment of %v, want nothing at all: a set that configured none must not acquire one", bs.Name, names)
			}
		}
	}

	if sets == 0 {
		t.Fatal("the fixture has no backup sets, so this test checked nothing")
	}
}

// workflowConfig is a minimal, valid config that DOES configure a
// workflow, used as the positive control above and as the base for the
// inheritance tests below.
func workflowConfig() Config {
	return Config{
		PollInterval: Duration(time.Minute),
		State:        State{Database: "/var/lib/backupd/state.db"},
		Workflows: Workflows{
			Root:          "/workflows",
			Global:        WorkflowStageDirs{BeforeDir: "global-before", AfterDir: "global-after"},
			ScriptTimeout: Duration(2 * time.Minute),
			Environment: []EnvironmentVariable{
				{Name: "DEPLOYMENT", Value: "production"},
				{Name: "API_TOKEN", FromSecret: SecretSource{File: "/run/secrets/token"}},
			},
			MaxScriptSizeBytes: 2 << 20,
		},
		Sources: []Source{{
			Name: "production",
			BackupSets: []BackupSet{{
				Name:       "db",
				Remote:     Remote{Type: "local"},
				RemotePath: "/srv/incoming",
				LocalPath:  "/backups/production/db",
				Completion: Completion{Strategy: "rename"},
				StaleAfter: Duration(30 * time.Hour),
				Workflow: &SetWorkflow{
					BeforeDir:               "db-before",
					AfterDir:                "db-after",
					ScriptTimeout:           Duration(30 * time.Second),
					RemoteExecConnectionRef: "production/db",
				},
				Environment: []EnvironmentVariable{{Name: "PGDATABASE", Value: "orders"}},
			}},
		}},
		Retention: Retention{},
	}
}

// The four stages resolve in execution order, and the per-set timeout wins
// over the deployment's.
func TestWorkflowResolvesStagesAndTimeoutPrecedence(t *testing.T) {
	t.Parallel()

	cfg := workflowConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	bs := &cfg.Sources[0].BackupSets[0]

	stages := cfg.WorkflowStagesFor(bs)
	want := []workflow.StageSpec{
		{Scope: workflow.ScopeGlobal, Phase: workflow.PhaseBefore, Dir: "global-before"},
		{Scope: workflow.ScopeSet, Phase: workflow.PhaseBefore, Dir: "db-before"},
		{Scope: workflow.ScopeSet, Phase: workflow.PhaseAfter, Dir: "db-after"},
		{Scope: workflow.ScopeGlobal, Phase: workflow.PhaseAfter, Dir: "global-after"},
	}

	if len(stages) != len(want) {
		t.Fatalf("resolved %d stages, want %d: %+v", len(stages), len(want), stages)
	}
	for i := range want {
		if stages[i] != want[i] {
			t.Errorf("stage %d is %+v, want %+v", i, stages[i], want[i])
		}
	}

	if got := cfg.EffectiveScriptTimeout(bs); got != 30*time.Second {
		t.Errorf("EffectiveScriptTimeout = %s, want the per-set 30s", got)
	}

	// With the per-set override removed, the deployment's value applies.
	bs.Workflow.ScriptTimeout = 0
	if got := cfg.EffectiveScriptTimeout(bs); got != 2*time.Minute {
		t.Errorf("EffectiveScriptTimeout = %s, want the deployment's 2m", got)
	}

	// And with neither, the domain's documented default -- read from the
	// one place that declares it, so a change there is not silently
	// contradicted here.
	cfg.Workflows.ScriptTimeout = 0
	if got := cfg.EffectiveScriptTimeout(bs); got != workflow.DefaultStepTimeout {
		t.Errorf("EffectiveScriptTimeout = %s, want workflow.DefaultStepTimeout (%s)", got, workflow.DefaultStepTimeout)
	}
}

// The environment's precedence, resolved by Validate into the field every
// consumer reads, so nothing downstream merges three layers itself.
func TestWorkflowEnvironmentInheritsAndOverrides(t *testing.T) {
	t.Parallel()

	cfg := workflowConfig()
	cfg.Sources[0].BackupSets[0].Environment = []EnvironmentVariable{
		{Name: "PGDATABASE", Value: "orders"},
		{Name: "DEPLOYMENT", Value: "staging"}, // overrides the deployment-wide value
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	got := map[string]EnvironmentVariable{}
	for _, v := range cfg.Sources[0].BackupSets[0].Environment {
		got[v.Name] = v
	}

	resolved := map[string]workflow.EnvVar{}
	for _, v := range cfg.Sources[0].BackupSets[0].WorkflowEnvironment.Vars() {
		resolved[v.Name] = v
	}

	if resolved["DEPLOYMENT"].Value != "staging" {
		t.Errorf("DEPLOYMENT resolved to %q, want the set's own %q", resolved["DEPLOYMENT"].Value, "staging")
	}
	if resolved["PGDATABASE"].Value != "orders" {
		t.Errorf("PGDATABASE resolved to %q", resolved["PGDATABASE"].Value)
	}
	if resolved["PATH"].Value == "" {
		t.Error("the sanitized baseline is not part of the resolved environment, so a hook could not invoke anything without an absolute path")
	}

	// The secret travels as a REFERENCE, field for field, and no
	// resolution has happened: Validate never opens a file.
	token, ok := resolved["API_TOKEN"]
	if !ok {
		t.Fatal("API_TOKEN did not reach the resolved environment")
	}
	if !token.IsSecret() {
		t.Error("API_TOKEN resolved as a literal, so its secret reference was lost on the way through the schema")
	}
	if token.Secret.File != "/run/secrets/token" || token.Secret.Env != "" || len(token.Secret.Command) != 0 {
		t.Errorf("API_TOKEN's reference is %+v, want the declared file source unchanged", token.Secret)
	}
	if token.Value != "" {
		t.Errorf("API_TOKEN carries a value %q; a secret-backed variable must carry a location and nothing else", token.Value)
	}
}

// The refusal table. Each row is a config an operator can write, and the
// message has to name the key they have to go and fix, because this text
// is what the daemon prints when it will not start.
func TestValidateRefusesUnusableWorkflowConfigurations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		what    string
		mutate  func(c *Config)
		mustSay string
	}{
		{
			what:    "a hook directory with no approved root",
			mutate:  func(c *Config) { c.Workflows.Root = "" },
			mustSay: "workflows.root",
		},
		{
			what:    "a relative root",
			mutate:  func(c *Config) { c.Workflows.Root = "workflows" },
			mustSay: "absolute path",
		},
		{
			what:    "a root containing ..",
			mutate:  func(c *Config) { c.Workflows.Root = "/srv/../workflows" },
			mustSay: `".."`,
		},
		{
			what:    "a global hook directory that escapes the root",
			mutate:  func(c *Config) { c.Workflows.Global.BeforeDir = "../elsewhere" },
			mustSay: "workflows.global.before_dir",
		},
		{
			what:    "a per-set hook directory that escapes the root",
			mutate:  func(c *Config) { c.Sources[0].BackupSets[0].Workflow.BeforeDir = "../elsewhere" },
			mustSay: "workflow.before_dir",
		},
		{
			what:    "a negative script timeout",
			mutate:  func(c *Config) { c.Workflows.ScriptTimeout = Duration(-time.Second) },
			mustSay: "workflows.script_timeout",
		},
		{
			what:    "a per-set script timeout of zero written explicitly",
			mutate:  func(c *Config) { c.Sources[0].BackupSets[0].Workflow.ScriptTimeout = Duration(-1) },
			mustSay: "workflow.script_timeout",
		},
		{
			what:    "a maximum script size above the ceiling",
			mutate:  func(c *Config) { c.Workflows.MaxScriptSizeBytes = workflow.MaxConfigurableScriptSize + 1 },
			mustSay: "workflows.max_script_size_bytes",
		},
		{
			what:    "a negative maximum script size",
			mutate:  func(c *Config) { c.Workflows.MaxScriptSizeBytes = -1 },
			mustSay: "workflows.max_script_size_bytes",
		},
		{
			what:    "a reserved environment name",
			mutate:  func(c *Config) { c.Workflows.Environment[0].Name = "BACKUPD_RUN_ID" },
			mustSay: "reserved",
		},
		{
			what:    "a reserved environment name on a backup set",
			mutate:  func(c *Config) { c.Sources[0].BackupSets[0].Environment[0].Name = "BACKUPD" },
			mustSay: "reserved",
		},
		{
			what:    "an environment name no shell can read",
			mutate:  func(c *Config) { c.Workflows.Environment[0].Name = "MY-VAR" },
			mustSay: workflow.EnvNamePattern,
		},
		{
			what:    "a NUL in an environment value",
			mutate:  func(c *Config) { c.Workflows.Environment[0].Value = "a\x00b" },
			mustSay: "NUL",
		},
		{
			what: "a literal and a secret on one variable",
			mutate: func(c *Config) {
				c.Workflows.Environment[0].FromSecret = SecretSource{Env: "SOMEWHERE"}
			},
			mustSay: "both a literal value and a secret reference",
		},
		{
			what: "two secret sources on one variable",
			mutate: func(c *Config) {
				c.Workflows.Environment[1].FromSecret = SecretSource{File: "/a", Env: "B"}
			},
			mustSay: "more than one secret source",
		},
		{
			what: "the same variable twice in one block",
			mutate: func(c *Config) {
				c.Workflows.Environment = append(c.Workflows.Environment, EnvironmentVariable{Name: "DEPLOYMENT", Value: "again"})
			},
			mustSay: "declared twice",
		},
		{
			what: "a workflow block with neither stage configured",
			mutate: func(c *Config) {
				c.Sources[0].BackupSets[0].Workflow = &SetWorkflow{}
			},
			mustSay: "configures no hook directory",
		},
		{
			what: "an environment no hook directory would ever read",
			mutate: func(c *Config) {
				c.Workflows.Global = WorkflowStageDirs{}
				c.Sources[0].BackupSets[0].Workflow = nil
			},
			mustSay: "nothing would ever read it",
		},
		{
			what: "an environment on a set whose deployment has no workflow root",
			mutate: func(c *Config) {
				c.Workflows = Workflows{}
				c.Sources[0].BackupSets[0].Workflow = nil
			},
			mustSay: "workflows.root",
		},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()

			cfg := workflowConfig()
			tc.mutate(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.what)
			}

			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Validate returned %T, want a *ValidationError", err)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("Validate said:\n\t%v\nwant it to contain %q, which is what the operator has to go and fix", err, tc.mustSay)
			}
		})
	}

	// The positive control. Without it every row above would pass against
	// a validator that refused every workflow configuration.
	cfg := workflowConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the control configuration was refused, so every row above proves nothing: %v", err)
	}
}

// Validate is idempotent over the workflow block, which the whole file's
// resolve-in-place discipline depends on: ValidateRetention hands the
// CLI's override path through the same code, and a second call must change
// nothing.
func TestValidateIsIdempotentOverTheWorkflowBlock(t *testing.T) {
	t.Parallel()

	cfg := workflowConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	first, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	firstEnv := cfg.Sources[0].BackupSets[0].WorkflowEnvironment.Names()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("second Validate: %v", err)
	}

	second, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	secondEnv := cfg.Sources[0].BackupSets[0].WorkflowEnvironment.Names()

	if string(first) != string(second) {
		t.Errorf("a second Validate changed the config.\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if strings.Join(firstEnv, ",") != strings.Join(secondEnv, ",") {
		t.Errorf("a second Validate changed the resolved environment: %v then %v", firstEnv, secondEnv)
	}
}

// The spool lives beside the state database, exactly as the application
// validator's script directory does. It is derived rather than configured
// so there is no second place an operator can point it at a share.
func TestWorkflowSpoolDirSitsBesideTheStateDatabase(t *testing.T) {
	t.Parallel()

	cfg := workflowConfig()
	if got, want := cfg.WorkflowSpoolDir(), "/var/lib/backupd/workflow-runs"; got != want {
		t.Errorf("WorkflowSpoolDir() = %q, want %q", got, want)
	}

	// No state database means no spool location, which the caller has to
	// be able to tell apart from a relative path it might then create in
	// its working directory.
	cfg.State.Database = ""
	if got := cfg.WorkflowSpoolDir(); got != "" {
		t.Errorf("WorkflowSpoolDir() = %q with no state database configured, want \"\"", got)
	}
}

// A YAML round trip of the whole block, because the shape an operator
// writes is the product surface and a renamed key is a config file that
// stops loading.
func TestWorkflowBlockRoundTripsThroughYAML(t *testing.T) {
	t.Parallel()

	const doc = `
poll_interval: 1m
state:
  database: /var/lib/backupd/state.db
workflows:
  root: /workflows
  global:
    before_dir: global-before
    after_dir: global-after
  script_timeout: 90s
  max_script_size_bytes: 2097152
  environment:
    - name: DEPLOYMENT
      value: production
    - name: API_TOKEN
      from_secret:
        file: /run/secrets/token
sources:
  - id: production
    backup_sets:
      - id: db
        remote:
          type: local
        remote_path: /srv/incoming
        local_path: /backups/production/db
        stale_after: 30h
        workflow:
          before_dir: db-before
          after_dir: db-after
          script_timeout: 30s
          remote_exec_connection_ref: production/db
        environment:
          - name: PGDATABASE
            value: orders
`

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(doc))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("decoding the documented shape: %v", err)
	}

	if cfg.Workflows.Root != "/workflows" {
		t.Errorf("workflows.root = %q", cfg.Workflows.Root)
	}
	if cfg.Workflows.Global.BeforeDir != "global-before" || cfg.Workflows.Global.AfterDir != "global-after" {
		t.Errorf("workflows.global = %+v", cfg.Workflows.Global)
	}
	if cfg.Workflows.ScriptTimeout.Duration() != 90*time.Second {
		t.Errorf("workflows.script_timeout = %s", cfg.Workflows.ScriptTimeout)
	}
	if cfg.Workflows.MaxScriptSizeBytes != 2<<20 {
		t.Errorf("workflows.max_script_size_bytes = %d", cfg.Workflows.MaxScriptSizeBytes)
	}
	if len(cfg.Workflows.Environment) != 2 {
		t.Fatalf("workflows.environment has %d entries", len(cfg.Workflows.Environment))
	}
	if cfg.Workflows.Environment[1].FromSecret.File != "/run/secrets/token" {
		t.Errorf("the secret reference did not survive the decode: %+v", cfg.Workflows.Environment[1])
	}

	bs := cfg.Sources[0].BackupSets[0]
	if bs.Workflow == nil {
		t.Fatal("the per-set workflow block did not decode")
	}
	if bs.Workflow.BeforeDir != "db-before" || bs.Workflow.AfterDir != "db-after" {
		t.Errorf("per-set workflow = %+v", bs.Workflow)
	}
	if bs.Workflow.ScriptTimeout.Duration() != 30*time.Second {
		t.Errorf("per-set script_timeout = %s", bs.Workflow.ScriptTimeout)
	}
	if bs.Workflow.RemoteExecConnectionRef != "production/db" {
		t.Errorf("remote_exec_connection_ref = %q", bs.Workflow.RemoteExecConnectionRef)
	}
	if len(bs.Environment) != 1 || bs.Environment[0].Name != "PGDATABASE" {
		t.Errorf("per-set environment = %+v", bs.Environment)
	}
}

// The secret source is the same three fields as everywhere else in this
// product, not a fourth vocabulary. A field on one and not the others is a
// credential source an operator can write and internal/secretref cannot
// resolve.
func TestSecretSourceIsTheSameShapeAsEveryOtherSecretDeclaration(t *testing.T) {
	t.Parallel()

	source := SecretSource{File: "/a", Env: "B", Command: []string{"c"}}
	ref := source.secretRef()

	if want := (secretref.Ref{File: "/a", Env: "B", Command: []string{"c"}}); !reflect.DeepEqual(ref, want) {
		t.Errorf("secretRef() = %+v, want %+v: the same three fields, unchanged", ref, want)
	}

	// And a config file must not be able to paste material directly.
	// There is no such field, and this is the assertion that fails if one
	// is added: the type's whole field set is the three locations.
	want := []string{"file,omitempty", "env,omitempty", "command,omitempty"}
	typ := reflect.TypeFor[SecretSource]()

	if typ.NumField() != len(want) {
		t.Fatalf("SecretSource has %d fields, want exactly the three locations. A fourth field is a place to paste a secret into a config file, which is the one shape this product does not accept a secret in", typ.NumField())
	}
	for i, tag := range want {
		if got := typ.Field(i).Tag.Get("yaml"); got != tag {
			t.Errorf("field %d is tagged %q, want %q: the spelling is the product's contract with an operator's config file", i, got, tag)
		}
	}
}
