package app

import (
	"context"
	"fmt"

	"github.com/retnd/retnd/core/internal/config"
	"github.com/retnd/retnd/core/internal/state"
	"github.com/retnd/retnd/core/legacypath"
)

// The one thing in this package that runs before a Service can exist.
//
// Every other file here is a method on a Service, so it is handed an
// already-loaded configuration and an already-open journal and never asks
// where either came from. This file is where both are produced, and it is
// the only production code in internal/app that calls
// config.LoadAndValidate or state.Open. It should stay the only one: a
// second caller of state.Open here would be a second opinion about whether
// the database is usable, formed after something had already started using
// it.

// Check is the `retnd check` use case: a pre-flight answer to "can
// this deployment actually start", checked before anything is asked to
// process a single artifact.
//
// It is a package-level function, not a Service method, on purpose: a
// Service cannot exist yet at this point, since building one needs an
// already-validated Config and an already-open Journal, exactly the two
// things Check exists to produce (or fail loudly on producing).
//
// Check does two things, in order:
//
//  1. config.LoadAndValidate(configPath): parses and validates FR-5's
//     configuration file. Every problem config.Validate can find (a
//     missing field, a duplicate backup set id, an invalid timezone, ...)
//     is reported here, in one pass, rather than surfacing one at a time
//     the first time something downstream happens to touch the field.
//  2. state.Open against the configured database path: opens (creating if
//     necessary) the FR-9 lifecycle journal and runs its migrations. This
//     is a real, meaningful check, not a formality: it proves the
//     configured state.database path is writable, that SQLite's WAL mode
//     and synchronous=FULL pragmas apply cleanly, and that this binary's
//     embedded migrations either already match what is on disk or apply
//     to it without conflict (see internal/state.Open's own doc for
//     ErrUnknownSchemaVersion and ErrSchemaDrift, both of which Check
//     surfaces exactly as state.Open reports them).
//
// Check does not contact any configured remote: proving connectivity to a
// source is a materially different, slower and credential-dependent check
// than "is this config and this database usable", and conflating the two
// would make Check's failure mode ambiguous (a bad password and a typo in
// local_path would look identical). `retnd reconcile` and
// `retnd fetch` are what exercise real connectivity.
//
// The returned *config.Config is the same up-to-date result LoadAndValidate
// produced, so a caller (cmd/retnd's `check` command) can print a
// summary of what was validated without loading the file a second time.
//
// The returned Preflight is FR-38's answer for this deployment: which of
// the configuration directory and the state database, if either, is being
// served from a pre-rename path. `check` is one of the three surfaces
// FR-38 requires that answer on (the others are the startup log and the
// deployment-check route), and it is the one an operator reaches for
// months later, when the warning has long since scrolled away. It is
// returned rather than printed here for the reason everything else in
// this package is: this file produces facts and cmd/retnd prints them.
func Check(ctx context.Context, configPath string) (*config.Config, legacypath.Preflight, error) {
	// Before the load, not after: the whole failure FR-38 exists to stop
	// is a resolved path that holds nothing being read as "not set up
	// yet". `check` answers the same question `serve` does and has to
	// answer it about the same directory, so it goes through the same
	// pure decision rather than a second opinion.
	cfgAdoption := legacypath.ForConfig(configPath)
	if cfgAdoption.Outcome == legacypath.Ambiguous {
		return nil, legacypath.Preflight{Config: cfgAdoption}, &legacypath.AmbiguousError{Adoption: cfgAdoption}
	}

	cfg, err := config.LoadAndValidate(cfgAdoption.Path)
	if err != nil {
		return nil, legacypath.Preflight{Config: cfgAdoption}, fmt.Errorf("config: %w", err)
	}

	stateAdoption := legacypath.ForStateDatabase(cfg.State.Database)
	pre := legacypath.Preflight{Config: cfgAdoption, State: stateAdoption}
	if stateAdoption.Outcome == legacypath.Ambiguous {
		return cfg, pre, &legacypath.AmbiguousError{Adoption: stateAdoption}
	}
	// Same reasoning as OpenConfigAndJournal's: everything that derives a
	// location from this field has to name the directory the journal is
	// really in, and `check` proving the wrong directory is writable is a
	// pre-flight that passes for a deployment that cannot start.
	cfg.State.Database = stateAdoption.Path

	j, err := state.Open(ctx, cfg.State.Database)
	if err != nil {
		return cfg, pre, fmt.Errorf("state: %w", err)
	}
	if err := j.Close(); err != nil {
		return cfg, pre, fmt.Errorf("state: closing %s: %w", cfg.State.Database, err)
	}

	return cfg, pre, nil
}
