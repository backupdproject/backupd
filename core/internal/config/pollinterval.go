// The two scopes a source poll cadence is decided at, and the one place
// they are combined (issue #845).
//
// Before this file there was one deployment-wide poll_interval and the
// daemon loop slept it between passes over every set. A per-set override
// makes "how often is this source checked" a question with two possible
// answers, and a question with two answers resolved at more than one call
// site is a question the engine and the UI eventually answer differently.
// So the resolution is here, as a method on the Config that owns both
// halves, and nothing outside reaches for BackupSet.PollInterval to
// decide a cadence.

package config

import "time"

// MinPollInterval is the floor under both scopes of poll_interval: the
// deployment-wide default and any per-set override.
//
// It exists because this value is now editable from a web form, and a
// form is a place where a missing unit suffix or a stray keystroke is
// one click from production. Below a minute the loop stops being a
// schedule and becomes pressure on somebody's source host: every wake
// opens SSH connections to every enabled set's remote and lists it, and
// a source that is a NAS with spinning disks feels that. A minute is
// already far finer than any backup cadence this product is for, so the
// floor costs nothing real and removes the whole class of mistake.
//
// It is enforced in Validate rather than in the form, so a hand-edited
// config.yaml is held to the same rule as a PATCH, and neither surface
// gets to be the lenient one.
const MinPollInterval = time.Minute

// EffectivePollInterval is how often bs is actually polled: its own
// override when it has one, and the deployment's default otherwise.
//
// This is the single home of that decision. It is a method on Config
// rather than on BackupSet because inheritance needs both halves, and a
// BackupSet method would have to be handed the global anyway -- at which
// point the call site could hand it the wrong one.
func (c *Config) EffectivePollInterval(bs BackupSet) time.Duration {
	if bs.PollInterval != nil {
		return bs.PollInterval.Duration()
	}
	return c.PollInterval.Duration()
}
