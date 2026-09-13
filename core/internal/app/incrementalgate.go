package app

import (
	"fmt"

	"github.com/backupdproject/backupd/core/internal/config"
)

// EPIC K's production feature gate, enforced (#789). The flag itself,
// and the argument for why it is off by default and why it is NOT
// enforced at config load, are in internal/config/incrementalengine.go.
//
// This file is the enforcement side, and there is exactly one check in
// it. Every incremental surface in this package reaches the engine
// through one of two doors -- incrementalBackupSet, which every
// per-set surface (list, detail, holds, retention preview, verify,
// restore) goes through, and the cycle's own engine dispatch -- so the
// gate sits on those doors and nowhere else. A per-surface check would
// be a gate with five copies, and the fifth is the one somebody forgets
// when they add a sixth surface.
//
// # Why the read-only surfaces are gated too
//
// Listing a gated set's snapshots costs nothing and opens nothing: the
// catalog is this deployment's own journal. Gating it anyway is the
// deliberate choice, because a surface that lists restore points while
// every action on them is refused is a surface that misrepresents what
// this deployment can do. An operator looking at eleven snapshots and a
// Restore button that answers 409 has been told the engine is available;
// one clear refusal naming the flag has been told the truth and the fix.
//
// # What is NOT gated, and must not be
//
//   - The configuration. A file declaring incremental sets loads and
//     validates with the gate shut, and the ARTIFACT sets in it back up
//     exactly as before (see incrementalengine.go).
//   - Deleting or editing an existing incremental set's configuration
//     (core/service). Config maintenance has to keep working; refusing
//     to let an operator remove a set BECAUSE the engine it names is
//     disabled would be a gate that traps what it gates.
//   - Anything about a source. A connection test still dials the source
//     and still runs #852's source write probe, on either engine.

// incrementalEngineGate reports the gate's refusal, or nil when this
// deployment runs the incremental engine.
//
// A Service holding no configuration is refused rather than allowed. The
// gate's answer comes from a config file, so "there is no config file"
// cannot be read as consent -- and every other question this package
// answers without a configuration already fails for the same reason.
func (s *Service) incrementalEngineGate() error {
	if s.Config != nil && s.Config.IncrementalEngineEnabled() {
		return nil
	}

	return config.ErrIncrementalEngineDisabled
}

// gatedSetRefusal is the gate's refusal as one backup set's cycle
// failure, naming the set so a report row says which work did not
// happen.
//
// A failure rather than a skip, which is the same judgement
// reportBarrenSets and unrunnableEngine already made: a set that
// quietly does nothing every cycle is indistinguishable from one that is
// working, and this is a set an operator deliberately configured. The
// sentence adds what a cycle report has to say and the flag's own
// sentence cannot know -- that nothing was read from the source and
// nothing was written to any repository -- because the operator reading
// it is deciding whether this pass did anything they need to undo.
func gatedSetRefusal(bs config.BackupSet) error {
	return fmt.Errorf("%w. This set (%s) was not backed up in this pass: nothing was read from its source, "+
		"no repository was opened, and nothing on the source was deleted",
		config.ErrIncrementalEngineDisabled, bs.ID)
}
