package kopia

import (
	"context"
	"errors"
	"fmt"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
)

// Stopping the engine from expiring snapshots this product wrote: a pin on
// every manifest, and a neutral global policy behind it (EPIC K, #785).
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
// # Why a stored policy is not enough, and what actually guarantees this
//
// There is no uploader option that disables checkpoint-time retention, and
// the checkpoint itself is worth having (it is what keeps a long upload's
// progress usable after an interruption). So there are two mechanisms
// here, and only one of them is load-bearing.
//
// The LOAD-BEARING one is a PIN on every manifest this adapter saves. The
// vendor's expiry keeps any manifest carrying a pin whatever the policy
// says (snapshot/policy/expire.go keeps a snapshot when it has a retention
// reason OR a pin), and the vendor's DeleteManifest -- which is what this
// adapter's DeleteSnapshot calls -- ignores pins entirely. That asymmetry
// is exactly the arrangement this product needs: the engine may never
// expire a snapshot backupd wrote, and backupd may still delete one when
// its own retention pass decides to.
//
// The other mechanism is the stored GLOBAL policy, set to keep everything.
// It is defence in depth and nothing more, because what a checkpoint's
// retention calculation reads is not the global policy but the EFFECTIVE
// one: a host, user@host or path policy overrides the global, and a
// NoParent policy discards it. One `kopia policy set --host nas-1
// --keep-latest 1` by somebody's own kopia install against a shared bucket
// leaves this file's global correction reading "already neutral" while the
// next long upload expires everything older than the newest snapshot. A
// product whose only protection was the global policy would lose held
// snapshots to that, silently, and
// TestAPolicySomebodyElseSetCannotExpireThisProductsSnapshots is the
// reproduction.
//
// "Keep everything" is spelled as six explicit zeros, which is the
// vendor's own way of saying it rather than a trick: RetentionPolicy.
// EffectiveKeepLatest returns MaxInt when every count is zero, so every
// snapshot is retained as "latest". Writing six large numbers instead
// would leave a policy that expires something eventually, at a boundary
// nobody chose.
//
// # Why it happens at open, and why it never refuses
//
// At open, because that is the one place every path that could write or
// checkpoint a snapshot passes through, including repositories created by
// a build that predates this file. It is a manifest read on the ordinary
// path and writes nothing: a repository this product has already
// neutralized answers the question and is left alone.
//
// It is BEST-EFFORT, and a failure to write the correction never refuses
// the open. Opening is also how this product reaches a repository to READ
// it -- a restore, a verification drill, a lifecycle reconciliation -- and
// storage that will not accept a write is an ordinary disaster-recovery
// posture: a WORM bucket, an object lock, a read-only mount, a legal hold.
// Refusing there would turn "this product cannot correct a policy" into
// "this product cannot restore", which is worse than the outcome the
// correction exists to prevent. What makes that safe to give up on is the
// pin: a repository whose policy could not be corrected still cannot
// expire a snapshot this adapter wrote. The failure is not silent -- the
// open repository reports it as a health warning, see
// backupengine.HealthWarningEngineRetention.

// enginePin is the pin this adapter puts on every manifest it saves, and
// it is this product's name because that is what a person reading `kopia
// snapshot list --show-pins` against a shared repository needs to see:
// which software is asserting that this snapshot may not be expired.
//
// Changing it would un-protect every manifest already written, so it is a
// stored identifier and not a cosmetic string.
const enginePin = "backupd"

// pinManifest marks one manifest as this product's before it is saved, so
// that no retention policy in this repository -- global, host, path, or
// one set by software this product never heard of -- can expire it.
//
// Called on every path that saves a manifest (adapter.go's Snapshot,
// stream.go, tree.go). A path that forgot it would produce snapshots the
// engine may delete behind the catalog, which is the whole failure this
// file exists to prevent, and
// TestEveryManifestThisAdapterSavesIsPinned is what notices.
func pinManifest(m *snapshot.Manifest) {
	m.UpdatePins([]string{enginePin}, nil)
}

// neutralizeRetention returns existing with its six COUNTS set to explicit
// zeros and everything else about it left exactly as it was.
//
// The six pointers are to zero values and the zeros are load-bearing. A
// nil field means "inherit", which is how the vendor's defaults get back
// in; a zero field is an explicit "no count applies here", and six of them
// are what RetentionPolicy.EffectiveKeepLatest reads as MaxInt.
//
// It edits a copy rather than returning a fresh RetentionPolicy because
// that struct is not only the counts: IgnoreIdenticalSnapshots lives there
// too and decides whether a run that produced byte-identical content is
// recorded at all. Replacing the whole struct would switch an operator's
// setting off inside a write whose entire claim is that it changes
// retention counts and nothing else.
func neutralizeRetention(existing policy.RetentionPolicy) policy.RetentionPolicy {
	var (
		latest  policy.OptionalInt
		hourly  policy.OptionalInt
		daily   policy.OptionalInt
		weekly  policy.OptionalInt
		monthly policy.OptionalInt
		annual  policy.OptionalInt
	)

	existing.KeepLatest = &latest
	existing.KeepHourly = &hourly
	existing.KeepDaily = &daily
	existing.KeepWeekly = &weekly
	existing.KeepMonthly = &monthly
	existing.KeepAnnual = &annual

	return existing
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
// or scheduling globally, and this is a statement about retention COUNTS
// alone -- including the rest of the retention section itself, see
// neutralizeRetention.
//
// Its error is a report, not a veto. OpenRepository logs it as a health
// warning and opens the repository anyway; the pin on every manifest this
// adapter saves is what keeps that safe. See this file's header.
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
	updated.RetentionPolicy = neutralizeRetention(updated.RetentionPolicy)

	if err := repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "backupd:disable-engine-retention"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			return policy.SetPolicy(ctx, w, policy.GlobalPolicySourceInfo, &updated) //nolint:wrapcheck // wrapped by the caller with the sentence that matters
		}); err != nil {
		return fmt.Errorf(
			"kopia: this repository's own snapshot retention is enabled and could not be turned off (%w); "+
				"every manifest backupd writes here is pinned, so the engine cannot expire one, but a snapshot written by anything else in this repository can still be deleted by it",
			err)
	}

	return nil
}
