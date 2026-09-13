package health

import "time"

// The third half of FR-24, added by EPIC K (#788): a repository's own
// health, which is not a backup set's and cannot be derived from one.
//
// # Why it is a separate verdict
//
// Every set storing snapshots in a repository can be perfectly fresh
// while the repository itself is unwritable, out of maintenance, or
// reached by a process whose clock has drifted far enough to mis-order
// manifests. None of those change a freshness verdict, and none of them
// is visible in one: the last run succeeded, the last backup is an hour
// old, and the next one will fail. So this is its own record with its own
// state, and decideState is left alone -- what an incremental set's
// FRESHNESS verdict is made of is a separate question with its own
// answer, and folding a repository probe into it would make a backup set
// read as failing because a NAS was asleep when somebody loaded a page.
//
// # Why every probe is reported separately
//
// Because the remedies are different and a single boolean loses which one
// applies: unreachable is a mount, unwritable is a permission, invalid
// credentials is a passphrase, and overdue maintenance is a schedule. An
// operator handed "repository unhealthy" has to go and find out which,
// and the four answers are already known by the time the verdict is
// reached.

// RepositoryHealth is one repository domain's verdict.
//
// The three access probes are ordered by what they imply: a repository
// that is not Reachable is not Readable, and one that is not Readable is
// not Writable. They are still three fields rather than one ladder value
// because a client renders three ticks and because the middle one is the
// interesting failure: reachable-and-unreadable is exactly what an empty
// or wrong mount presents, which is the case that must never be mistaken
// for "no repository here yet".
type RepositoryHealth struct {
	// Domain is the repository domain's declared id.
	Domain string

	// MayShare is whether the configuration lets more than one backup set
	// store snapshots here. It is on the health record because a shared
	// domain's problem is every co-tenant's problem, and a surface that
	// showed the fault against one set would understate it.
	MayShare bool

	// BackupSets are the source/set ids storing snapshots here.
	BackupSets []string

	// State is this repository's verdict, in the same vocabulary the
	// backup-set half uses so one dashboard has one severity scale.
	// Stale is never used here: a repository does not go stale, the sets
	// in it do.
	State State

	// Reachable is whether the storage answered at all.
	Reachable bool

	// Readable is whether this deployment could open the repository,
	// which means its format metadata was read back.
	Readable bool

	// Writable is whether this deployment proved it could write. Proved
	// by the engine's own health check, which performs a write and
	// removes it, never inferred from a successful read: a read-only
	// mount, a WORM policy and an expired write credential all read
	// perfectly.
	Writable bool

	// CredentialsValid is whether the declared passphrase reference
	// resolved and opened the repository. False beside Reachable is the
	// signature of a rotated or mis-referenced secret, which is a
	// different job from a broken mount.
	CredentialsValid bool

	// ClockSane is whether this process's clock can be trusted for the
	// timestamps a repository reasons about. False means either the
	// engine reported skew against the storage, or this process's clock
	// is behind durable history it has already written -- a clock that
	// has gone backwards past its own record.
	ClockSane bool

	// ClockSkew is how far this process's clock is ahead of (positive) or
	// behind (negative) the newest durable timestamp on record, and
	// ClockSkewKnown is whether there was any such timestamp to compare
	// against. A brand-new deployment has none, which is neither a sane
	// nor an insane clock.
	ClockSkew      time.Duration
	ClockSkewKnown bool

	// MaintenanceOverdue is whether this repository has gone long enough
	// without maintenance to be worth an operator's attention. It is not
	// a backup failure and is never reported as one: the snapshots are
	// fine and the storage they sit in is growing without being
	// reclaimed.
	MaintenanceOverdue bool

	// LastMaintenanceAt and LastMaintenanceResult are the most recent
	// window. Zero and empty mean maintenance has never run, which is a
	// fact rather than a missing value.
	LastMaintenanceAt     time.Time
	LastMaintenanceResult string

	// LastSnapshotAt and LastSnapshotStatus are the newest snapshot run
	// in this domain, from any set sharing it, and its durable phase.
	LastSnapshotAt     time.Time
	LastSnapshotStatus string

	// LastVerificationAt and LastVerificationStatus are the newest run in
	// this domain that reached a verification verdict, and what it
	// concluded. Empty is a real and separate answer from "passed":
	// nothing here has ever been verified.
	LastVerificationAt     time.Time
	LastVerificationStatus string

	// Detail is one sentence naming what is wrong, empty when nothing is.
	// It never carries a path, an endpoint or any part of a credential:
	// this string is rendered on a dashboard and pasted into support
	// conversations.
	Detail string
}

// OK reports whether this repository needs no attention at all.
func (r RepositoryHealth) OK() bool { return r.State == Healthy }
