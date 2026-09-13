package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// EPIC K's production feature gate (#789).
//
// # Why the engine ships behind a flag at all
//
// Everything #780-#788 built is reachable from a two-line edit to a
// config file: declare a repository domain, give a backup set
// "engine: kopia". That is the right shape for the feature and the wrong
// shape for its first release. A deployment that upgrades to pick up a
// retention fix must not find a second backup engine one typo away from
// live, and an operator evaluating the incremental engine must be able to
// say "not on this host" once, in one place, rather than trusting that
// nobody adds a set.
//
// So the engine is off until a deployment says otherwise, and the switch
// is one boolean at the top level of the config file with one environment
// override. Not per set, not per repository domain: a gate with a
// granularity is a gate somebody has to reason about, and the question
// this answers is "is this deployment running the incremental engine at
// all".
//
// # What the gate deliberately does NOT do
//
// It does not refuse the CONFIGURATION. A config file that declares
// incremental sets while the gate is shut loads, validates and runs --
// and its ARTIFACT sets back up exactly as before. That asymmetry is the
// whole design:
//
//   - A gate enforced at load would mean turning the flag off takes the
//     entire deployment down, artifact sets included, because Validate
//     failing is a daemon that will not start. A safety switch whose use
//     causes a bigger outage than the thing it switches off is not a
//     safety switch.
//   - A gate enforced where the engine is USED refuses exactly the work
//     that would have run the gated engine, names itself in the refusal,
//     and leaves every other backup in the deployment alone.
//
// The enforcement points are therefore the four places that hold a
// *Config and are about to drive the engine: the cycle's engine dispatch
// (internal/app), snapshot restore (internal/app, core/service), the
// repository probe and its alert pass (internal/app), and backup-set
// create/update (core/service). Nothing below those -- not
// snapshotlifecycle, not the kopia adapter, not internal/source -- knows
// about the gate, because none of them holds a configuration and a
// second opinion in a lower layer is how two gates drift apart.
//
// It also does not gate the SOURCE side. A backup set's connection test
// still reaches its source, still runs #852's source write probe and
// still reports what it found, on both engines: whether this host may
// delete from a source is a fact about the source, it is what an operator
// checks before they decide anything, and answering "the incremental
// engine is off" to a question about an SFTP server would be a non sequitur.

// IncrementalEngineEnvVar is the environment override for the gate.
//
// It is for the deployments that run this engine's process without
// editing its config file: a direct `backupd` invocation, a systemd
// unit, `docker compose exec`, a CLI-only install, and the tests and
// clean-environment restores that have to answer the question without a
// file to hand. The container images this product ships pass a declared
// list of variables through (container/compose.yaml has no catch-all),
// so a compose or appliance deployment turns the gate on in config.yaml
// -- the file it already bind-mounts and the one this product's own
// settings writes go into. Both mechanisms are documented; neither is a
// fallback for the other.
//
// It wins over the file in BOTH directions, which is the property that
// makes it worth having at all. Reading it as "may only turn the engine
// on" would leave an operator with no way to stop a running engine
// except editing a file inside a container; reading it as "may only turn
// it off" would leave a CLI-only host unable to evaluate the engine
// without a config edit it may not own.
const IncrementalEngineEnvVar = "BACKUPD_INCREMENTAL_ENGINE"

// ErrIncrementalEngineDisabled is the refusal every surface raises when
// something would have driven the incremental engine while the gate is
// shut. Surfaces wrap it with the subject they were asked about; nothing
// re-words it.
//
// The sentence carries no package prefix, unlike most sentinels in this
// tree, because this one is rendered to an operator verbatim -- in an API
// error body, on a CLI's stderr and in a cycle's per-set failure line --
// and "config:" in front of it would be this program talking to itself.
//
// Three things have to be in it, and they are what its test pins: the
// config key to write, the environment variable that does the same job,
// and the promise that artifact backup sets are unaffected. The third is
// there because the operator most likely to read this sentence is one who
// has just upgraded and does not yet know whether their existing backups
// stopped.
var ErrIncrementalEngineDisabled = errors.New(
	"the incremental (kopia) backup engine is disabled in this deployment: " +
		"set incremental_engine.enabled: true in config.yaml, or " + IncrementalEngineEnvVar + "=1 in the environment, to enable it; " +
		"existing artifact backup sets are unaffected")

// IncrementalEngine is the gate as the config file spells it.
//
// A struct rather than a bare top-level boolean so that a later
// deployment-wide setting about this engine has an obvious home and does
// not arrive as a second top-level key nobody can group. It carries
// exactly one field today, deliberately: see this file's doc for why the
// gate has no per-set or per-domain granularity.
type IncrementalEngine struct {
	// Enabled is the file's word on whether this deployment runs the
	// incremental engine.
	//
	// Absent and false are the same fact, which is why this is a plain
	// bool rather than the *bool that MaxMovesPerCycle uses: there is no
	// third state to distinguish. "I have not thought about this" and "I
	// do not want it" both mean the engine stays shut, and that is the
	// safe direction for a value nobody set.
	Enabled bool `yaml:"enabled"`
}

// IncrementalEngineEnabled reports whether this deployment runs the
// incremental engine: the file's word, unless the environment overrides
// it.
//
// This is the ONLY place that question is answered. A surface that
// re-derived it from os.Getenv would be a second gate, and two gates
// disagree the first time one of them learns a new spelling.
//
// It reads the environment on every call rather than resolving once into
// the Config, and that is not laziness. core/service re-marshals the
// whole Config on every settings save (FR-35), so a resolved override
// written into the struct would be PERSISTED into an operator's file: the
// deployment's environment variable would silently become a line in a
// config they never edited, and would then survive the variable being
// removed. The cost is one getenv per question, on paths that are about
// to open a repository.
//
// An unparseable value falls back to the file. Validate refuses such a
// value outright (validateIncrementalEngine), so a daemon never reaches
// this state; a Config built by hand in a test can, and the file's own
// word is the only other answer available.
func (c *Config) IncrementalEngineEnabled() bool {
	if override, ok, err := parseIncrementalEngineOverride(os.Getenv(IncrementalEngineEnvVar)); err == nil && ok {
		return override
	}

	return c.IncrementalEngine.Enabled
}

// incrementalEngineOverrideSpellings are the values the environment
// override accepts, on-values first.
//
// A closed set rather than strconv.ParseBool: ParseBool accepts "t" and
// "F" and rejects "yes", "on" and "off", which are exactly the spellings
// an operator writes into a compose file. Case is folded, surrounding
// whitespace is trimmed, and anything else is a refusal rather than a
// guess.
var incrementalEngineOverrideSpellings = map[string]bool{
	"1": true, "true": true, "yes": true, "on": true,
	"0": false, "false": false, "no": false, "off": false,
}

// parseIncrementalEngineOverride reads the environment override. The
// second result is whether the variable said anything at all: unset, and
// whitespace, are both "the file decides".
func parseIncrementalEngineOverride(raw string) (enabled, present bool, err error) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return false, false, nil
	}

	value, ok := incrementalEngineOverrideSpellings[trimmed]
	if !ok {
		return false, false, fmt.Errorf(
			"%s=%q is not a value this gate understands: write one of 1, true, yes, on to enable the incremental engine, "+
				"or 0, false, no, off to disable it; unset the variable to let incremental_engine.enabled in config.yaml decide",
			IncrementalEngineEnvVar, raw)
	}

	return value, true, nil
}

// validateIncrementalEngine refuses an environment override this gate
// cannot read.
//
// The opposite rule to the diagnostics knobs (an unparseable LOG_LEVEL
// falls back to INFO rather than refusing to start, because a typo in a
// diagnostic must never take a backup host down), and the asymmetry is
// the judgement. Reading BACKUPD_INCREMENTAL_ENGINE=ture as "off" would
// turn every incremental backup in the deployment into a refusal, quietly
// and for as long as nobody re-reads the compose file -- so the typo is
// reported at the one moment somebody is watching, which is the restart
// that set it.
//
// Nothing else about the gate is validated, because there is nothing
// else: a boolean in a file that Load already type-checks cannot be
// wrong, and a config naming incremental sets while the gate is shut is
// deliberately legal (see this file's doc).
func (v *validator) validateIncrementalEngine() {
	if _, _, err := parseIncrementalEngineOverride(os.Getenv(IncrementalEngineEnvVar)); err != nil {
		v.addf("%s", err.Error())
	}
}
