//go:build !kopiabench

package e2eproduction_test

// The SMOKE sizes: the same three shapes, small enough that the whole of
// TestSourceScaleShapes finishes in a few seconds, so a per-commit gate
// keeps exercising all three code paths.
//
// Sizing them is a judgement and it is worth stating. Each is the
// smallest that still exercises the mechanism the shape exists to test:
//
//   - flat at twenty thousand entries in ONE directory is well past
//     rclone's own listing chunk, so an unbounded listing is already a
//     visible allocation here; a thousand would fit in one chunk and
//     prove nothing.
//   - deep at 128 levels is past the point a naive recursive walk starts
//     to matter. It is also close to what the filesystem allows at all:
//     see levelName in scale_test.go for why PATH_MAX, not memory, is
//     what bounds this shape.
//   - large at 48 MiB is more than two of the vendor's ~20 MiB pack
//     blobs and several of its content-splitter chunks, so content
//     really is split and the splitter is really exercised by the edit.
//
// # The large-file edit is four kilobytes, and that is the point
//
// Measured on this repository at 48 MiB (the numbers are from the
// suite's own log lines): a 4 KiB in-place edit makes the second
// snapshot write 8.25 MB, a 64 KiB edit writes 8.40 MB, and a 480 KiB
// edit writes 20.6 MB. So the cost of an edit is not proportional to the
// edit -- it is one or two of the vendor's content-splitter chunks,
// whose default average is 4 MiB. That is the honest claim for a large
// file, and it is the one that scales the right way: the same 8 MB on a
// four-gigabyte file is two tenths of one per cent, which is why the
// full size below can hold a far tighter ratio than this one.
//
// A 4 KiB window is therefore the interesting case -- "one byte of a big
// file moved" -- and a ratio of 4 is the bound it has to meet.
//
// # The heap budgets
//
// Generous on purpose: they are a ceiling that catches "memory scales
// with the source", not a regression detector for a few megabytes. The
// full sizes in scale_full_test.go are where the numbers get tight.
func scaleShapes() []scaleShape {
	return []scaleShape{
		{
			name:            "flat-directory",
			files:           20_000,
			fanout:          1,
			size:            256,
			heapBudgetBytes: 512 << 20,
			changedFiles:    200,
			writeLimitRatio: 2,
		},
		{
			name:            "deep-tree",
			depth:           128,
			size:            4 << 10,
			heapBudgetBytes: 256 << 20,
			changedFiles:    2,
			writeLimitRatio: 4,
		},
		{
			name:              "large-file",
			files:             1,
			fanout:            1,
			size:              48 << 20,
			heapBudgetBytes:   512 << 20,
			changedFiles:      1,
			changeWindowBytes: 4 << 10,
			writeLimitRatio:   4,
		},
	}
}
