package kopia

import (
	"context"
	"errors"
	"fmt"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/policy"
)

// Turning the engine's OWN snapshot retention off, once, for every
// repository this adapter opens (EPIC K, issue #785).
//
// # The behaviour this exists to stop
//
// backupd is the retention engine for the snapshots it writes: which
// snapshot may be deleted is decided in internal/snapshotretention, from
// the catalog, after GFS classification, last-known-good protection and
// operator holds have been applied, and the decision is carried out one
// manifest at a time through DeleteSnapshot. That arrangement is only true
// if nothing else deletes snapshots.
//
// Something else does, by default. The vendor's uploader checkpoints a
// long upload every 45 minutes, and every checkpoint ends by applying the
// EFFECTIVE RETENTION POLICY for the source it is uploading -- listing that
// source's snapshots, computing which of them the policy no longer keeps,
// and deleting those manifests (snapshot/upload/upload.go's checkpointRoot
// calls policy.ApplyRetentionPolicy with reallyDelete set). The default
// global policy keeps the latest 10 snapshots, 7 daily, 4 weekly, 24
// monthly and 3 annual, so on a repository holding more than that, any
// backup run lasting longer than the checkpoint interval quietly expires
// older snapshots.
//
// Every one of those deletions would be invisible to this product: not in
// the catalog, not in a plan an operator confirmed, and -- the part that
// matters most -- not subject to a hold. A snapshot a person placed a legal
// hold on would be removed by the engine in the middle of an unrelated
// backup, and the first evidence of it would be a restore that could not
// be performed.
//
// # Why the answer is a stored policy rather than a knob
//
// There is no uploader option that disables checkpoint-time retention, and
// the checkpoint itself is worth having (it is what keeps a long upload's
// progress usable after an interruption). What the retention calculation
// reads is the policy STORED IN THE REPOSITORY, so that is what this
// changes: the global policy is set to keep everything, and the engine's
// own arithmetic then concludes, correctly, that nothing has expired.
//
// "Keep everything" is spelled as six explicit zeros, which is the
// vendor's own way of saying it rather than a trick: RetentionPolicy.
// EffectiveKeepLatest returns MaxInt when every count is zero, so every
// snapshot is retained as "latest". Writing six large numbers instead
// would leave a policy that expires something eventually, at a boundary
// nobody chose.
//
// # Why it happens at open, and what it refuses
//
// At open, because that is the one place every path that could write or
// checkpoint a snapshot passes through, including repositories created by
// a build that predates this file. It is a manifest read on the ordinary
// path and writes nothing: a repository this product has already
// neutralized answers the question and is left alone.
//
// A repository whose policy is NOT neutral and whose storage will not
// accept the correction is refused rather than opened, and that is the
// deliberate part. Opening it anyway means accepting that the engine may
// delete this product's snapshots during the next long backup, which is
// exactly the outcome this file exists to prevent; a refusal at open is
// visible, and a snapshot deleted behind the catalog is not.

// neutralRetention is the policy this product stores globally: keep
// everything, decide nothing.
//
// The six pointers are to zero values and the zeros are load-bearing. A
// nil field means "inherit", which is how the vendor's defaults get back
// in; a zero field is an explicit "no count applies here", and six of them
// are what RetentionPolicy.EffectiveKeepLatest reads as MaxInt.
func neutralRetention() policy.RetentionPolicy {
	var (
		latest  policy.OptionalInt
		hourly  policy.OptionalInt
		daily   policy.OptionalInt
		weekly  policy.OptionalInt
		monthly policy.OptionalInt
		annual  policy.OptionalInt
	)

	return policy.RetentionPolicy{
		KeepLatest:  &latest,
		KeepHourly:  &hourly,
		KeepDaily:   &daily,
		KeepWeekly:  &weekly,
		KeepMonthly: &monthly,
		KeepAnnual:  &annual,
	}
}

// engineRetentionIsOff reports whether a stored policy already expires
// nothing.
//
// Every count has to be present AND zero. A nil count is not "zero", it is
// "inherit", and what it inherits is the vendor's default -- which is the
// policy that deletes things.
func engineRetentionIsOff(pol *policy.Policy) bool {
	if pol == nil {
		return false
	}

	for _, count := range []*policy.OptionalInt{
		pol.RetentionPolicy.KeepLatest,
		pol.RetentionPolicy.KeepHourly,
		pol.RetentionPolicy.KeepDaily,
		pol.RetentionPolicy.KeepWeekly,
		pol.RetentionPolicy.KeepMonthly,
		pol.RetentionPolicy.KeepAnnual,
	} {
		if count == nil || *count != 0 {
			return false
		}
	}

	return true
}

// disableEngineRetention makes sure this repository's global policy
// expires nothing, writing it only when it does not already say so.
//
// It preserves every other part of a stored global policy: an operator or
// a future feature may legitimately have set compression, error handling
// or scheduling globally, and this is a statement about retention alone.
func disableEngineRetention(ctx context.Context, rep repo.Repository) error {
	defined, err := policy.GetDefinedPolicy(ctx, rep, policy.GlobalPolicySourceInfo)
	switch {
	case err == nil && engineRetentionIsOff(defined):
		return nil
	case err != nil && !errors.Is(err, policy.ErrPolicyNotFound):
		return fmt.Errorf("kopia: reading the repository's global policy: %w", err)
	}

	updated := policy.Policy{}
	if defined != nil {
		updated = *defined
	}
	updated.RetentionPolicy = neutralRetention()

	if err := repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "backupd:disable-engine-retention"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			return policy.SetPolicy(ctx, w, policy.GlobalPolicySourceInfo, &updated) //nolint:wrapcheck // wrapped by the caller with the sentence that matters
		}); err != nil {
		return fmt.Errorf(
			"kopia: this repository's own snapshot retention is enabled and could not be turned off (%w); "+
				"backupd decides which snapshots may be deleted, and opening a repository that would expire them during a long backup is refused",
			err)
	}

	return nil
}
