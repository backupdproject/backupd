// #789's resource-limit gate: the DOCUMENTED per-source memory bound,
// asserted as an absolute figure at the scale this product says it
// supports.
//
// # Why this is not what largenamespace_test.go already asserts
//
// #784's two tests assert a SLOPE: at most largeNSSnapshotBudget bytes of
// peak heap per entry, and a cost per entry that falls rather than rises
// as the namespace grows tenfold. That is the right shape for catching a
// regression -- a run that starts retaining one more record per entry
// lands outside it -- and it is not an answer to the question an operator
// sizing a NAS asks, which is "how much memory does one source of my size
// need". A slope with no scale attached permits any absolute number at
// all.
//
// So this file states the scale and the ceiling together, in bytes, and
// measures the two passes that actually run against it. The budget is
// #784's per-entry figure multiplied by the supported scale, which is the
// point: the per-entry budget IS the documented bound, and this is what
// it means in megabytes at a size somebody deploys.
//
// # The bound, and why it is linear at all
//
// Kopia stores one directory as one manifest object listing every child,
// so storing it builds every child's snapshot.DirEntry in memory before
// it can be written and reading it decodes the whole manifest back into a
// slice (snapshotfs.DirManifestBuilder, snapshotfs.readDirEntries). Both
// are linear in the width of ONE directory, and neither is this adapter's
// to change. largeNSSnapshotBudget's own doc has the full attribution.
//
// The consequence is the documented limit: memory scales with the widest
// single directory in the source, NOT with the source's total entry
// count. A million files spread over a deep tree costs what one of its
// directories costs; a million files in one directory costs a million
// entries' worth at once. That distinction is the whole of what an
// operator has to know, and it is why this test measures a FLAT namespace
// -- the worst case -- rather than a tree.

package kopia_test

import (
	"context"
	"testing"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/model"
)

// supportedFlatDirectoryEntries is the widest single source directory
// this product documents support for, and millionEntryHeapCeiling is what
// that costs at the per-entry budget #784 pinned.
//
// One million, because that is the number #792 and docs/adr/0008 measured
// the enumeration side against, and a bounded enumerator in front of a
// snapshot that cannot hold the same namespace would be an OOM with an
// extra hop in it. The ceiling is stated for the SNAPSHOT pass because it
// is the larger of the two (a verification decodes the manifest but
// builds no pack), so a deployment sized for it is sized for both.
const (
	supportedFlatDirectoryEntries = 1_000_000
	millionEntryHeapCeiling       = int64(supportedFlatDirectoryEntries) * largeNSSnapshotBudget
)

// TestOneSourceStaysWithinTheDocumentedMemoryBound measures both passes
// at the scale this package's harness runs and holds each to the absolute
// figure the documented bound implies at that scale.
//
// It reuses #784's harness deliberately rather than re-running a
// million-entry walk: the fixture for a million entries costs minutes and
// about 1.4 GiB, which is a test that dies on CI rather than reporting
// anything (largeNSEntryCount's own doc). What makes the smaller run an
// honest statement about the supported scale is the pair of claims
// underneath it -- this file's absolute ceiling, and #784's non-growth
// assertion that the cost per entry falls rather than rises with the
// namespace -- so an extrapolation from 200,000 to 1,000,000 is an upper
// bound rather than a guess. BACKUPD_HUGE_DIR_ENTRIES=1000000 runs the
// real thing on a machine that can hold it, and the same assertions
// apply unchanged.
func TestOneSourceStaysWithinTheDocumentedMemoryBound(t *testing.T) {
	entries := largeNSEntries(t)

	rep, _, info, snapshotCost := largeNSSnapshot(t, entries)

	verifyCost := largeNSMeasure(t, entries, func() {
		if _, err := rep.Verify(context.Background(), info.ID, backupengine.VerifyRequest{Level: model.LevelStructural}); err != nil {
			t.Fatalf("Verify over %d entries: %v", entries, err)
		}
	})

	for _, tc := range []struct {
		what   string
		budget uint64
		cost   largeNSCost
	}{
		{"snapshotting one source", largeNSSnapshotBudget, snapshotCost},
		{"verifying one source", largeNSVerifyBudget, verifyCost},
	} {
		ceiling := tc.budget * uint64(entries)

		t.Logf("%s of %d entries: %s (ceiling %s, documented %d-entry ceiling %s)",
			tc.what, entries, tc.cost, largeNSMiB(ceiling),
			supportedFlatDirectoryEntries, largeNSMiB(tc.budget*supportedFlatDirectoryEntries))

		if tc.cost.peakHeap > ceiling {
			t.Errorf("%s cost %s of peak heap against a documented ceiling of %s at this scale: "+
				"a deployment sized from this product's own documented figure would be sized too small",
				tc.what, largeNSMiB(tc.cost.peakHeap), largeNSMiB(ceiling))
		}
	}

	// The extrapolation is only sound while the measured scale is a
	// tenth or more of the supported one, because that is what #784's
	// non-growth assertion is measured over. A future edit that lowers
	// largeNSEntryCount would silently turn this test into a much weaker
	// claim than its name.
	if int64(entries)*10 < supportedFlatDirectoryEntries {
		t.Errorf("this test measured %d entries and claims a bound at %d: the gap is too wide for the non-growth "+
			"assertion in largenamespace_test.go to carry, so either the measured scale or the documented one is wrong",
			entries, supportedFlatDirectoryEntries)
	}

	if millionEntryHeapCeiling <= 0 {
		t.Fatal("the documented ceiling overflowed, so nothing here is asserting a bound")
	}
}
