package service

import (
	"github.com/backupdproject/backupd/core/internal/config"
)

// EPIC K's production feature gate at this boundary (#789). The flag is
// internal/config's (incrementalengine.go); internal/app enforces it on
// the paths that drive the engine. This file is what the layer ABOVE
// core/ needs, and it is two things.
//
// # The sentinel, re-exported
//
// apps/ cannot import core/internal, so a handler that must answer "the
// incremental engine is disabled in this deployment" with its own status
// and error code needs the sentinel here. It is the SAME error value, not
// a copy: errors.Is holds across both layers, and a second error would
// be a second sentence to keep in step with the first.
//
// # The one check this layer adds
//
// internal/app refuses every path that would RUN the engine, which is
// enough to keep a gated deployment from touching a repository. What it
// cannot do is refuse a gated set being CREATED, or a durable operation
// row being written for work that is going to be refused the moment it
// starts -- both of those happen here, above the engine, and both are
// refusals that "cost nothing" only at this point. That is the same
// reasoning SubmitSnapshotRestore's own engine check already states.
//
// What this layer deliberately does NOT gate is editing or deleting an
// existing incremental set. An operator whose deployment has the engine
// switched off must still be able to disable, rename, re-point or remove
// the sets that name it: a gate that traps the configuration it gates
// leaves an operator with a set they can neither run nor get rid of.

// ErrIncrementalEngineDisabled is config.ErrIncrementalEngineDisabled,
// reachable from outside core/. Its sentence names the config key, the
// environment variable and the fact that artifact backup sets are
// unaffected.
var ErrIncrementalEngineDisabled = config.ErrIncrementalEngineDisabled

// refuseGatedIncrementalEngine returns ErrIncrementalEngineDisabled
// unless cfg's deployment runs the incremental engine.
//
// A nil configuration is refused rather than allowed, for the reason
// internal/app's own gate gives: the answer lives in a config file, so
// the absence of one cannot be read as consent.
func refuseGatedIncrementalEngine(cfg *config.Config) error {
	if cfg != nil && cfg.IncrementalEngineEnabled() {
		return nil
	}

	return ErrIncrementalEngineDisabled
}
