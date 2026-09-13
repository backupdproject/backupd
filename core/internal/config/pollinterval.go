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

// PollWakeInterval is the base granularity a scheduling loop has to wake
// at to honour every cadence this configuration asks for: the smallest
// effective interval across every set that will actually be polled, and
// the deployment default when nothing overrides it downwards.
//
// The alternative -- a timer per backup set -- is the one shape this
// engine may not take. "No two passes over a backup set overlap" is a
// property of there being exactly one sequential loop (see
// internal/app/daemon.go and cycle.go), and it stops being true the
// moment a second thing can start a pass. So the loop keeps its single
// timer and simply wakes often enough that the set with the tightest
// cadence is never late; which sets are DUE on a given wake is decided
// per set, against this same resolution (EffectivePollInterval above).
//
// A DISABLED set is not counted. It is never polled at all (RunCycle
// skips it outright), so letting its cadence quicken the loop would be
// waking the process up on behalf of work nobody does.
//
// A set whose override is LONGER than the default does not slow the loop
// down either: every other set is still entitled to the default, and a
// wake is cheap -- it is the pass that costs, and a pass that no set is
// due for does nothing.
func (c *Config) PollWakeInterval() time.Duration {
	wake := c.PollInterval.Duration()
	for _, src := range c.Sources {
		for _, bs := range src.BackupSets {
			if bs.Disabled || bs.PollInterval == nil {
				continue
			}
			if d := bs.PollInterval.Duration(); d > 0 && d < wake {
				wake = d
			}
		}
	}
	return wake
}
