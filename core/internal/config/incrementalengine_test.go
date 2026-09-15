// The production feature gate for the incremental engine (#789, EPIC K's
// release gate), tested from the two directions that can hurt a running
// deployment: a gate that cannot be turned on, and a gate whose being
// turned off stops a backup that has nothing to do with it.
//
// The load-bearing cases here are the last three in the file. A gate is
// a safety mechanism, so the interesting questions are not "does true
// mean on" but:
//
//   - Can turning it OFF brick a daemon whose config still names an
//     incremental set? (No: Validate must accept that config. The artifact
//     sets in the same file have to keep running.)
//   - Can a settings save inject the key into a file that never heard of
//     it? (No: FR-35's forward-compatibility rule, the same one every
//     other EPIC-era block is held to.)
//   - Can the environment override leak into the file? (No: the env var is
//     resolved on read, never written back into the Config that
//     core/service re-marshals.)

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestIncrementalEngineIsOffByDefault(t *testing.T) {
	t.Setenv(IncrementalEngineEnvVar, "")

	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if c.IncrementalEngineEnabled() {
		t.Error("a configuration that never mentions incremental_engine reports the incremental engine enabled; " +
			"the production gate's whole point is that it is off until somebody turns it on")
	}
}

func TestIncrementalEngineTurnsOnFromTheFile(t *testing.T) {
	t.Setenv(IncrementalEngineEnvVar, "")

	c := incrementalConfig()
	c.IncrementalEngine.Enabled = true
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if !c.IncrementalEngineEnabled() {
		t.Error("incremental_engine.enabled: true does not enable the incremental engine, so the gate cannot be opened at all")
	}
}

// A file that says nothing and an environment that says nothing is the
// only "unset" there is, and Load must accept the key itself: a config
// carrying the gate has to survive Load's KnownFields(true).
func TestIncrementalEngineLoadsFromAConfigFile(t *testing.T) {
	t.Setenv(IncrementalEngineEnvVar, "")

	base, err := os.ReadFile("testdata/engine-kopia.yaml")
	if err != nil {
		t.Fatalf("reading the incremental fixture: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, append(base, []byte("\nincremental_engine:\n  enabled: true\n")...), 0o600); err != nil {
		t.Fatalf("writing the gated fixture: %v", err)
	}

	cfg, err := LoadAndValidate(path)
	if err != nil {
		t.Fatalf("LoadAndValidate: %v", err)
	}

	if !cfg.IncrementalEngineEnabled() {
		t.Error("a config file carrying incremental_engine.enabled: true loads with the incremental engine still gated off")
	}
}

// The environment wins in BOTH directions, which is the whole reason it
// exists: a deployment must be able to turn the engine on without an
// operator hand-editing a mounted config file, and an operator mid-incident
// must be able to turn it off without editing one either.
func TestIncrementalEngineEnvironmentOverridesTheFile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inFile bool
		env    string
		want   bool
	}{
		{"env enables what the file left off", false, "1", true},
		{"env disables what the file turned on", true, "0", false},
		{"true", false, "true", true},
		{"mixed-case TRUE", false, "TRUE", true},
		{"yes", false, "yes", true},
		{"on", false, "on", true},
		{"false", true, "false", false},
		{"no", true, "no", false},
		{"off", true, "off", false},
		{"unset defers to the file's on", true, "", true},
		{"unset defers to the file's off", false, "", false},
		{"whitespace is not a value", true, "   ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(IncrementalEngineEnvVar, tc.env)

			c := incrementalConfig()
			c.IncrementalEngine.Enabled = tc.inFile
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}

			if got := c.IncrementalEngineEnabled(); got != tc.want {
				t.Errorf("file=%v %s=%q resolved to enabled=%v, want %v",
					tc.inFile, IncrementalEngineEnvVar, tc.env, got, tc.want)
			}
		})
	}
}

// A typo in the gate is refused rather than read as "off". The usual rule
// for a diagnostic knob is the opposite (an unparseable LOG_LEVEL must
// never take a backup host down), and this is not a diagnostic knob:
// silently reading RETND_INCREMENTAL_ENGINE=ture as off would turn every
// incremental backup in the deployment into a refusal, which is the exact
// failure the operator was trying to avoid by setting it.
func TestIncrementalEngineRefusesAnUnparseableEnvironmentValue(t *testing.T) {
	t.Setenv(IncrementalEngineEnvVar, "ture")

	c := incrementalConfig()

	err := c.Validate()
	if err == nil {
		t.Fatalf("Validate accepted %s=ture", IncrementalEngineEnvVar)
	}

	for _, want := range []string{IncrementalEngineEnvVar, "ture", "true", "false"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it does not say what was refused or what is legal: %v", want, err)
		}
	}
}

// Turning the gate off must never stop a daemon from starting.
//
// This is the one case where a feature flag becomes a bigger outage than
// the feature it gates. A deployment that enabled the engine, created
// incremental sets and then turned the flag back off still has ARTIFACT
// sets in the same file, and those have to keep running: refusing the
// whole config at load would take every backup in the deployment down to
// gate one engine. So the gate is enforced where the engine is USED, and
// Validate stays silent about it.
func TestIncrementalEngineOffStillValidatesAConfigWithIncrementalSets(t *testing.T) {
	t.Setenv(IncrementalEngineEnvVar, "")

	c := incrementalConfig()
	c.IncrementalEngine.Enabled = false

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate refused a configuration whose incremental sets are merely gated off, which would stop the artifact sets in the same file from running: %v", err)
	}
	if c.IncrementalEngineEnabled() {
		t.Fatal("the gate reports enabled after Validate, which is not what the file says")
	}
}

// FR-35's forward-compatibility rule, applied to this block. core/service
// re-marshals the whole Config on every settings save, so a key that
// appears in an operator's file because they changed an unrelated setting
// is a file an older binary refuses outright under Load's KnownFields(true).
func TestIncrementalEngineIsNotInjectedIntoAFileThatNeverHadIt(t *testing.T) {
	t.Setenv(IncrementalEngineEnvVar, "1")

	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	out, err := yaml.Marshal(&c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if strings.Contains(string(out), "incremental_engine") {
		t.Errorf("re-marshaling a config that never mentioned the gate injected it:\n%s", out)
	}

	// And the environment override must not have been baked into the
	// struct on the way past: resolution reads the variable, it never
	// writes the answer into the file core/service is about to save.
	if c.IncrementalEngine.Enabled {
		t.Error("the environment override was written into Config.IncrementalEngine.Enabled, " +
			"so the next settings save would persist a gate the operator never wrote into their file")
	}
	if !c.IncrementalEngineEnabled() {
		t.Error("the environment override did not take effect, so it was neither persisted nor honoured")
	}
}

// One sentence, one source. Every surface that refuses because the gate is
// shut says this, so an operator who reads it anywhere knows the config
// key, the environment variable and that their artifact sets are fine.
func TestIncrementalEngineDisabledErrorNamesTheFlagAndSparesArtifactSets(t *testing.T) {
	msg := ErrIncrementalEngineDisabled.Error()
	for _, want := range []string{
		"incremental_engine.enabled: true",
		IncrementalEngineEnvVar,
		"artifact",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal %q does not mention %q, so an operator reading it cannot act on it", msg, want)
		}
	}
}
