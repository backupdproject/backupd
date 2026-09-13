package e2eproduction_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/snapshotlifecycle"
)

// The three source shapes #789 asks for as automated suites, measured
// rather than asserted about.
//
// # Why one body and two size tables
//
// A benchmark nobody runs is not evidence, and a million-entry directory
// is not something a per-commit gate can afford. So each shape has a
// SMOKE size that runs on every gate and a FULL size behind the
// `kopiabench` build tag, and both drive the same function. That is the
// only arrangement in which the thing the gate exercises is the thing the
// full run measures: two separate implementations would drift, and the
// one that drifted would be the one nobody runs.
//
//	go test ./tests/e2eproduction                       smoke sizes
//	go test -tags kopiabench -timeout 3h ./tests/...    full sizes
//
// # What each shape is for
//
//   - flat: entry count in ONE directory. This is the shape that decides
//     whether enumeration is bounded, because an unbounded listing
//     materialises the whole directory in the layer underneath before any
//     of this product's code could count it.
//   - deep: nesting depth. The tree bridge holds one open frame per
//     level, so depth is the dimension its memory actually scales with,
//     and a recursive walk that ran out of stack would do it here.
//   - large: one object far bigger than a pack blob, which is the shape
//     that decides whether content is streamed or buffered.
//
// # What every shape asserts, beyond running
//
// The incremental claim, which is EPIC K's acceptance criterion: a
// second snapshot after a ~1% change stores only the new content. It is
// asserted per shape rather than once, because each shape can break it
// differently -- a million tiny files can defeat deduplication through
// per-file metadata, a deep tree through re-written directory manifests,
// and a large file through a chunker that re-splits everything after an
// edit.

// scaleShape is one source shape at one size.
type scaleShape struct {
	name string

	// files, fanout and size describe the tree. depth, when non-zero,
	// nests instead: one file per level, depth levels deep.
	files  int
	fanout int
	size   int
	depth  int

	// heapBudgetBytes is the peak Go heap this shape may use while the
	// snapshot runs. It is a budget rather than a measurement of the last
	// run: the claim being defended is "memory does not scale with the
	// source", and a budget is the only shape of assertion that can fail
	// when it starts to.
	heapBudgetBytes uint64

	// changedFiles is how much of the source is rewritten before the
	// second snapshot, and writeLimitRatio is the reciprocal of the
	// fraction of the logical size the second snapshot's repository
	// writes must stay under.
	changedFiles    int
	writeLimitRatio int

	// changeWindowBytes is how much of a single-file shape is rewritten
	// in place. It is explicit rather than derived because the number
	// that matters is its relationship to the engine's splitter, not to
	// the file: see the large-file entry in scale_smoke_test.go.
	changeWindowBytes int
}

func TestSourceScaleShapes(t *testing.T) {
	for _, shape := range scaleShapes() {
		t.Run(shape.name, func(t *testing.T) {
			runScaleShape(t, shape)
		})
	}
}

// runScaleShape seeds one shape, takes two snapshots over it and reports
// what the engine did.
func runScaleShape(t *testing.T, shape scaleShape) {
	t.Helper()

	ctx := context.Background()
	d := newDeployment(t, deploymentOptions{createRepository: true})

	logical := seedShape(t, d.srcDir, shape)

	peak := newHeapSampler()

	started := time.Now()

	first, err := d.run(t, "run-"+shape.name+"-a", snapshotlifecycle.VerificationOptions{})
	if err != nil || !first.Succeeded() {
		t.Fatalf("the first snapshot of the %s shape: %v (%s)", shape.name, err, first.Reason)
	}

	firstDuration := time.Since(started)
	firstPeak := peak.stop()

	if first.Files == 0 {
		t.Fatalf("the first snapshot of the %s shape stored no files at all: %+v", shape.name, first)
	}

	if firstPeak > shape.heapBudgetBytes {
		t.Errorf("the %s shape peaked at %d bytes of Go heap over a source of %d entries; the documented budget for this shape is %d",
			shape.name, firstPeak, first.Entries, shape.heapBudgetBytes)
	}

	// --- a small change, then the second snapshot -------------------------

	changeShape(t, d.srcDir, shape)

	peak = newHeapSampler()
	started = time.Now()

	second, err := d.run(t, "run-"+shape.name+"-b", snapshotlifecycle.VerificationOptions{})
	if err != nil || !second.Succeeded() {
		t.Fatalf("the second snapshot of the %s shape: %v (%s)", shape.name, err, second.Reason)
	}

	secondDuration := time.Since(started)
	secondPeak := peak.stop()

	if secondPeak > shape.heapBudgetBytes {
		t.Errorf("the %s shape's second snapshot peaked at %d bytes of Go heap; the documented budget for this shape is %d",
			shape.name, secondPeak, shape.heapBudgetBytes)
	}

	// The incremental claim. The second run re-READS the whole source --
	// every shape, every time, because scan-time metadata is not evidence
	// about content -- and must WRITE only what is new.
	if second.SourceBytesRead < logical {
		t.Errorf("the %s shape's second snapshot read %d bytes off the source; the tree is %d and a tree run re-reads all of it",
			shape.name, second.SourceBytesRead, logical)
	}

	if limit := logical / int64(shape.writeLimitRatio); second.RepositoryBytesWritten >= limit {
		t.Errorf("the %s shape's second snapshot wrote %d bytes into the repository after %d of %d files changed; a %d byte source stored incrementally must write under %d",
			shape.name, second.RepositoryBytesWritten, shape.changedFiles, shape.files, logical, limit)
	}

	if second.ContentReusedBytes <= 0 {
		t.Errorf("the %s shape's second snapshot reports %d bytes of reuse, so nothing was deduplicated",
			shape.name, second.ContentReusedBytes)
	}

	// A structural verification of the second snapshot, which is what
	// makes the numbers above facts about a readable restore point rather
	// than about a write that happened to return.
	report, err := d.repo.Verify(ctx, backupengine.SnapshotID(second.SnapshotID), backupengine.VerifyRequest{
		Level: model.LevelStructural,
	})
	if err != nil {
		t.Fatalf("verifying the %s shape's second snapshot: %v", shape.name, err)
	}

	if report.ObjectsVerified < first.Files {
		t.Errorf("a structural verification of the %s shape resolved %d objects; the snapshot holds at least %d files",
			shape.name, report.ObjectsVerified, first.Files)
	}

	t.Logf("%s: %d entries, %d logical bytes | A %s, wrote %d, heap peak %d | B %s, read %d, wrote %d, reused %d, heap peak %d | process peak RSS %d",
		shape.name, first.Entries, logical,
		firstDuration.Round(time.Millisecond), first.RepositoryBytesWritten, firstPeak,
		secondDuration.Round(time.Millisecond), second.SourceBytesRead, second.RepositoryBytesWritten, second.ContentReusedBytes, secondPeak,
		peakRSSBytes())
}

// seedShape writes one shape's tree and returns its logical size.
func seedShape(t *testing.T, dir string, shape scaleShape) int64 {
	t.Helper()

	if shape.depth > 0 {
		return seedDeep(t, dir, shape)
	}

	// One shared payload per shape, with the index written into the
	// first bytes so no two files are identical. Generating a fresh
	// random buffer per file costs more than the snapshot does at a
	// million entries, and the property the measurement needs is only
	// that content is incompressible and distinct.
	base := randomBytes(t, shape.size)

	for i := range shape.files {
		payload := make([]byte, shape.size)
		copy(payload, base)
		writeIndex(payload, i)
		writeFile(t, filepath.Join(dir, filepath.FromSlash(shapePath(shape, i))), payload)
	}

	return int64(shape.files) * int64(shape.size)
}

// seedDeep writes one file per level, depth levels down.
func seedDeep(t *testing.T, dir string, shape scaleShape) int64 {
	t.Helper()

	base := randomBytes(t, shape.size)
	path := dir

	for i := range shape.depth {
		path = filepath.Join(path, levelName(i))

		payload := make([]byte, shape.size)
		copy(payload, base)
		writeIndex(payload, i)
		writeFile(t, filepath.Join(path, "leaf.bin"), payload)
	}

	return int64(shape.depth) * int64(shape.size)
}

// changeShape rewrites this shape's changed files with new content.
//
// For the large-file shape it rewrites a WINDOW inside the file rather
// than the whole thing, because "one byte of a big file moved" is the
// case a content-defined chunker has to get right and rewriting the file
// whole would not exercise it at all.
func changeShape(t *testing.T, dir string, shape scaleShape) {
	t.Helper()

	if shape.files == 1 && shape.depth == 0 {
		path := filepath.Join(dir, filepath.FromSlash(shapePath(shape, 0)))

		f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatalf("opening %s to change it: %v", path, err)
		}

		window := shape.changeWindowBytes
		if window <= 0 {
			window = shape.size / 100
		}

		if _, err := f.WriteAt(randomBytes(t, window), int64(shape.size/2)); err != nil {
			t.Fatalf("rewriting a %d byte window inside %s: %v", window, path, err)
		}

		if err := f.Close(); err != nil {
			t.Fatalf("closing %s: %v", path, err)
		}

		return
	}

	if shape.depth > 0 {
		path := dir
		for i := range shape.changedFiles {
			path = filepath.Join(path, levelName(i))
			writeFile(t, filepath.Join(path, "leaf.bin"), randomBytes(t, shape.size))
		}

		return
	}

	for i := range shape.changedFiles {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(shapePath(shape, i))), randomBytes(t, shape.size))
	}
}

// levelName is one level of the deep shape's path, and it is as short as
// it can be for a reason that binds this benchmark harder than memory
// does: the depth a source tree can reach at all is bounded by the
// operating system's PATH_MAX, which is 1024 bytes on Darwin and 4096 on
// Linux, and every byte spent on a readable directory name is a level
// this shape cannot reach. rclone's local backend addresses every entry
// by absolute path, so a tree too deep to name is a tree this product
// cannot read regardless of what the engine could do with it.
func levelName(i int) string { return strconv.Itoa(i) }

// shapePath is where a shape's i'th file lives.
func shapePath(shape scaleShape, i int) string {
	if shape.fanout <= 1 {
		return fmt.Sprintf("file-%07d.bin", i)
	}

	return seededPath(i, shape.fanout)
}

// writeIndex stamps i into the first bytes of a payload, so files built
// from one base buffer are still distinct content.
func writeIndex(payload []byte, i int) {
	for b := range min(8, len(payload)) {
		payload[b] = byte(i >> (8 * b))
	}
}

// --- measurement ----------------------------------------------------------

// heapSampler tracks the highest Go heap this process held while a run
// was in flight.
//
// It samples rather than reading once at the end, because the number that
// matters is the PEAK: a walk that buffered a million entries and then
// released them would read back as almost nothing from a single
// after-the-fact measurement, which is exactly the defect a bounded
// enumeration exists to prevent.
type heapSampler struct {
	peak atomic.Uint64
	done chan struct{}
	sunk chan struct{}
}

func newHeapSampler() *heapSampler {
	s := &heapSampler{done: make(chan struct{}), sunk: make(chan struct{})}

	go func() {
		defer close(s.sunk)

		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		for {
			s.sample()

			select {
			case <-s.done:
				s.sample()

				return
			case <-ticker.C:
			}
		}
	}()

	return s
}

func (s *heapSampler) sample() {
	var m runtime.MemStats

	runtime.ReadMemStats(&m)

	inUse := m.HeapInuse + m.StackInuse

	for {
		was := s.peak.Load()
		if inUse <= was || s.peak.CompareAndSwap(was, inUse) {
			return
		}
	}
}

func (s *heapSampler) stop() uint64 {
	close(s.done)
	<-s.sunk

	return s.peak.Load()
}
