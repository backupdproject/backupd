//go:build kopiabench

package e2eproduction_test

// The FULL sizes: the three benchmarks #789 names, at the scale the
// acceptance criterion is about.
//
//	cd core && go test -tags kopiabench -timeout 3h -run TestSourceScaleShapes -v ./tests/e2eproduction
//
// These are behind a build tag rather than a flag or an environment
// variable so that the ordinary `go test ./...` cannot reach them at all:
// a million-entry directory is about twenty minutes of seeding and a
// four-gigabyte file needs the disk for it, and a gate that sometimes
// paid that and sometimes did not would be a gate nobody could read the
// timings of.
//
// # The sizes, and why these
//
//   - flat at one million entries in ONE directory is the number the
//     issue names. At 256 bytes each that is a quarter of a gigabyte of
//     content and, far more importantly, a million-entry listing: the
//     shape that decides whether enumeration is bounded.
//   - deep at 180 levels is as deep as a source tree can be addressed
//     at all on Darwin: see levelName in scale_test.go. PATH_MAX, not
//     this engine, is what bounds this shape, and a number chosen to
//     look impressive would simply fail to create its own fixture.
//   - large at four gigabytes is past 32-bit offsets, past any plausible
//     in-memory buffer, and the size at which the measured cost of an
//     edit -- one or two content-splitter chunks, about 8 MB; see
//     scale_smoke_test.go for the measurements -- becomes two tenths of
//     one per cent. That is why this ratio is 32 where the smoke size's
//     is 4: the same absolute cost against a much larger file.
//
// # The heap budgets
//
// These are the numbers the acceptance criterion "supported source
// scales meet documented memory limits" is measured against, and they
// are deliberately far below the source size in every case: 768 MiB
// against a million entries, 256 MiB against four thousand levels, and
// 512 MiB against a four-gigabyte file. The last two are the interesting
// ones -- if memory scaled with the source at all, a 4 GiB file under a
// 512 MiB ceiling would fail immediately.
func scaleShapes() []scaleShape {
	return []scaleShape{
		{
			name:            "flat-directory-1m",
			files:           1_000_000,
			fanout:          1,
			size:            256,
			heapBudgetBytes: 768 << 20,
			changedFiles:    10_000,
			writeLimitRatio: 2,
		},
		{
			name:            "deep-tree-180",
			depth:           180,
			size:            4 << 10,
			heapBudgetBytes: 256 << 20,
			changedFiles:    4,
			writeLimitRatio: 4,
		},
		{
			name:              "large-file-4g",
			files:             1,
			fanout:            1,
			size:              4 << 30,
			heapBudgetBytes:   512 << 20,
			changedFiles:      1,
			changeWindowBytes: 4 << 10,
			writeLimitRatio:   32,
		},
	}
}
