package kopia_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
)

// This file is #792's question asked of the other half of the product.
//
// #792 set the bound for enumerating one directory with a million entries
// in it: peak memory has to be a function of the enumerator's buffer
// rather than of the entry count, cancellation has to be observed during
// the walk, and nothing may accumulate one record per directory. The
// evidence for that claim is in transport/enumerate_test.go, and it is
// evidence about the TRANSPORT: the thing that reads somebody else's
// directory listing.
//
// What the transport hands its entries to is this package. A bounded
// enumerator in front of a snapshot that materialises every entry is an
// OOM with an extra hop in it, so the same bound has to hold for the two
// passes #784 adds to the picture: the run that STORES a large flat
// namespace, and the verification that later proves it. Both are asserted
// here as the same two properties #792 asserted -- the work completes with
// the counts the source produced, and ten times the entries does not cost
// ten times the memory.
//
// Why a generated source rather than files on a disk: the transport's own
// huge-directory test (rclone/enumerate_test.go) creates real entries
// because what it is testing IS a real directory read. Here the question
// is what the SNAPSHOT and the VERIFICATION retain per entry, and a
// generated backupengine.SourceDir asks it with the same forward cursor a
// real source offers while spending the time on the repository instead of
// on a million inodes.

const (
	// largeNSEntryCount is the flat namespace these tests store, and it
	// is an honest stand-in for #792's million rather than the million
	// itself.
	//
	// The difference between the two numbers is what each side pays per
	// entry. The transport's million-entry fixture is a directory read: a
	// name and an lstat. A snapshot of the same namespace writes one
	// stored object per entry, hashes it, packs it, indexes it and then
	// walks every one of those objects again to verify it, and -- because
	// a flat directory's cost is linear in its width, which is what the
	// budget below is about -- a million entries is not only ten times
	// the seconds but about 1.4 GiB of peak heap, which is a test that
	// dies on a CI box rather than reporting anything.
	//
	// 200,000 entries costs ten seconds and about 250 MiB for the pair of
	// tests here, and it is far past the point where what is being
	// guarded against shows up: a run that held one more record per entry
	// lands outside the budget by a factor of five at this count (that is
	// measured, by putting one there), and the comparison against a
	// namespace a tenth the size is what makes the number a statement
	// about SCALING rather than about one machine's idea of a lot of
	// memory.
	//
	// BACKUPD_HUGE_DIR_ENTRIES raises it, and it is deliberately the same
	// variable the transport's huge-directory test reads: one setting
	// runs both halves of #792's question at whatever bound is being
	// investigated, and docs/adr/0008's million-entry run is reproducible
	// here too.
	largeNSEntryCount = 200_000

	// largeNSPayloadSize is how big each stored object is.
	//
	// Small on purpose: these tests are about what is retained PER ENTRY,
	// so every byte spent on content is a byte of measurement noise and a
	// millisecond not spent on the next entry. It is not zero, because an
	// entry with no content is refused (checkTreeEntry) and because a
	// namespace of empty files would deduplicate to a single stored
	// object and prove nothing about a namespace at all.
	largeNSPayloadSize = 64
)

// largeNSModTime is the one modification time every generated entry
// carries. A per-entry time would be another per-entry allocation in the
// fixture, which is the opposite of what this file is measuring.
var largeNSModTime = time.Unix(1700000000, 0).UTC()

// largeNSEntries is how many entries this run uses.
//
// Skipped under -short for the same reason the transport's equivalent is:
// the fixture is the expensive part, and `go test -short` is what runs on
// every commit.
func largeNSEntries(t *testing.T) int {
	t.Helper()

	if testing.Short() {
		t.Skip("stores a large flat namespace; run without -short")
	}

	raw := os.Getenv("BACKUPD_HUGE_DIR_ENTRIES")
	if raw == "" {
		return largeNSEntryCount
	}

	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("BACKUPD_HUGE_DIR_ENTRIES=%q: %v", raw, err)
	}

	if n < 10 {
		t.Fatalf("BACKUPD_HUGE_DIR_ENTRIES=%d is too small to compare against a namespace a tenth the size", n)
	}

	return n
}

// --- the generated namespace -------------------------------------------

// largeNSDir is one directory holding count entries that do not exist
// anywhere, built as they are asked for.
//
// It is a backupengine.SourceDir and nothing more, which is the point:
// the engine sees exactly what a real source gives it -- a forward cursor
// it may pass over once -- and the fixture itself holds no per-entry
// state, so anything this file measures is the adapter's and the vendor's
// rather than the test's own.
type largeNSDir struct {
	count int

	// opens and served are what the source can say about the walk that
	// the snapshot's own report cannot: how many passes were asked for,
	// and how many entries were actually handed over. A report claiming
	// 200,000 files is checked against the second of these rather than
	// against itself.
	opens  atomic.Int64
	served atomic.Int64
}

func (d *largeNSDir) Open(context.Context) (backupengine.SourceDirIterator, error) {
	d.opens.Add(1)

	return &largeNSIter{dir: d}, nil
}

type largeNSIter struct {
	dir  *largeNSDir
	next int
}

func (it *largeNSIter) Next(context.Context) (backupengine.SourceEntry, bool, error) {
	if it.next >= it.dir.count {
		return backupengine.SourceEntry{}, false, nil
	}

	i := it.next
	it.next++

	it.dir.served.Add(1)

	return backupengine.SourceEntry{
		Name:    fmt.Sprintf("object-%08d.bin", i),
		ModTime: largeNSModTime,
		Size:    largeNSPayloadSize,
		Stream:  largeNSStream(i),
	}, true, nil
}

func (it *largeNSIter) Close() error { return nil }

// largeNSStream is one entry's content, derived from its index.
//
// It is an int rather than a struct holding bytes so that the namespace
// costs the fixture nothing: the payload exists only while it is being
// read, and a test whose own fixture retained 200,000 buffers could not
// tell whether the peak it measured was the adapter's or its own.
type largeNSStream int

func (s largeNSStream) ModTime() time.Time { return largeNSModTime }

func (s largeNSStream) Open(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(newLargeNSPayload(int(s))), nil
}

// newLargeNSPayload is one entry's bytes: distinct per index, because a
// namespace of identical files is stored once, verified once, and says
// nothing about a namespace.
func newLargeNSPayload(i int) io.Reader {
	b := make([]byte, largeNSPayloadSize)

	for off := 0; off+8 <= len(b); off += 8 {
		binary.LittleEndian.PutUint64(b[off:], uint64(i)*0x9E3779B97F4A7C15+uint64(off))
	}

	return bytes.NewReader(b)
}

// --- measurement -------------------------------------------------------

// largeNSCost is what one pass over the namespace cost.
//
// peakHeap is the number the assertions are made against, because the
// failure this file exists to catch -- one live record per entry -- is a
// heap allocation. rssDelta is reported beside it and deliberately not
// asserted on: the vendor's verification walk keeps its already-seen set
// in memory-mapped segments (internal/bigmap) rather than on the Go heap,
// so RSS is the only place that shows up, and RSS is a process-wide
// high-water mark that any concurrently running test can raise. It is
// logged so that a reader checking whether the bound is real can see both
// halves, and asserted on nowhere, so that a sibling test's payload
// cannot fail this one.
type largeNSCost struct {
	entries  int
	peakHeap uint64
	rssDelta int64
	elapsed  time.Duration
}

// perEntry is the number every assertion here is made against: peak heap
// divided by the namespace, so that a budget means the same thing at
// 200,000 entries as at #792's million.
func (c largeNSCost) perEntry() uint64 { return c.peakHeap / uint64(c.entries) }

func (c largeNSCost) String() string {
	return fmt.Sprintf("entries=%d peak_heap=%s max_rss_delta=%s bytes_per_entry=%d elapsed=%s",
		c.entries, largeNSMiB(c.peakHeap), largeNSMiB(uint64(max(c.rssDelta, 0))),
		c.perEntry(), c.elapsed.Round(time.Millisecond))
}

func largeNSMiB(b uint64) string { return fmt.Sprintf("%.1fMiB", float64(b)/(1<<20)) }

// largeNSMeasure runs one pass with the package's own heap sampler around
// it and returns what it cost above a freshly collected baseline.
func largeNSMeasure(t *testing.T, entries int, pass func()) largeNSCost {
	t.Helper()

	runtime.GC()

	var base runtime.MemStats

	runtime.ReadMemStats(&base)

	rssBefore := largeNSMaxRSS(t)
	start := time.Now()
	peak := peakHeap(pass)
	cost := largeNSCost{
		entries:  entries,
		rssDelta: largeNSMaxRSS(t) - rssBefore,
		elapsed:  time.Since(start),
	}

	if peak > base.HeapAlloc {
		cost.peakHeap = peak - base.HeapAlloc
	}

	return cost
}

// largeNSMaxRSS reads this process's high-water resident set, which is
// monotonic per process: only a delta measured around one pass means
// anything, and only when that pass is the biggest so far. The tests
// below run the smaller namespace first for that reason.
func largeNSMaxRSS(t *testing.T) int64 {
	t.Helper()

	var ru syscall.Rusage

	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatalf("getrusage: %v", err)
	}

	if runtime.GOOS == "darwin" {
		return int64(ru.Maxrss) // bytes
	}

	return int64(ru.Maxrss) * 1024 // kilobytes everywhere else
}

// largeNSSnapshotBudget and largeNSVerifyBudget are what ONE ENTRY of a
// flat namespace is allowed to cost in peak heap, and they are #792's
// bound restated as the strongest claim this side of the product can
// honestly make.
//
// #792's bound is a CONSTANT: ten times the entries, at most twice the
// memory, because a chunked enumerator's peak is its chunk. Neither the
// snapshot nor the verification of a huge FLAT directory can make that
// claim, and the reason is Kopia's on-disk format rather than anything
// this adapter decides. A directory is one manifest object listing all
// of its children, sorted by name, so storing one builds every child's
// snapshot.DirEntry in memory before it can be written
// (snapshotfs.DirManifestBuilder) and reading one decodes the whole
// manifest back into a slice before the walk can iterate it
// (snapshotfs.readDirEntries). Both are linear in the width of ONE
// directory. Measured at 200,000 entries, with the peak attributed by a
// live-heap profile: the snapshot's ~1.4KiB per entry is the pending
// pack and index for tiny contents (content.WriteManager flushes a pack
// on BYTES, never on entry count) plus that manifest being built and
// JSON-encoded, and the verification's ~1.0KiB per entry is the same
// manifest being read back and decoded whole, over the walker's fixed
// already-seen set (internal/bigmap). The one term that was NOT the
// vendor's -- this adapter retaining every reader it had handed out for
// the life of a run, a quarter of the snapshot's peak -- is what writing
// this file found, and openStreams now drops a reader when it closes.
//
// So the two assertions below are the ones that are true and still bite.
// The PER-ENTRY budget is an absolute ceiling on the linear term -- a
// regression that starts holding one more record per entry, a read
// buffer or an fs.Entry, lands outside it -- and the budget is stated
// per entry rather than per pass so that it means the same thing at
// 200,000 entries and at #792's million. The NON-GROWTH assertion is
// what keeps linear from quietly becoming quadratic: fixed costs
// amortise, so the cost per entry over ten times the namespace must come
// DOWN rather than up.
//
// Both budgets are roughly twice what is measured, which is headroom for
// a machine with a different GC schedule and not room for another record
// per entry.
const (
	largeNSSnapshotBudget = 3 << 10
	largeNSVerifyBudget   = 2 << 10
)

// largeNSAssertBounded checks one pass's cost per entry against its
// budget, and against the same pass over a namespace a tenth the size.
func largeNSAssertBounded(t *testing.T, what string, budget uint64, small, large largeNSCost) {
	t.Helper()

	t.Logf("%s, a tenth the namespace: %s", what, small)
	t.Logf("%s, the whole namespace:   %s", what, large)

	if got := large.perEntry(); got > budget {
		t.Errorf("%s of %d entries cost %d bytes of peak heap per entry, budget %d: something is holding a record per entry that the manifest itself does not need",
			what, large.entries, got, budget)
	}

	// A quarter is noise; a second retained record per entry is not, and
	// anything superlinear is an order of magnitude out.
	if limit := small.perEntry() * 5 / 4; large.perEntry() > limit {
		t.Errorf("%s cost %d bytes per entry at %d entries and %d at %d: the cost per entry must fall as fixed costs amortise, not rise -- rising means the work per entry is growing with the namespace",
			what, small.perEntry(), small.entries, large.perEntry(), large.entries)
	}
}

// largeNSSnapshot stores one flat namespace of n entries into its own
// repository and returns the repository, the source and what the run
// cost.
//
// A fresh repository per size is not tidiness. Content is deduplicated
// below this boundary, so storing the tenth-sized namespace into the same
// repository first would make the large run a reuse measurement: 20,000
// of its entries would already be there, and the pass being measured
// would not be the one being described.
func largeNSSnapshot(t *testing.T, n int) (backupengine.TreeRepository, backupengine.Source, backupengine.TreeSnapshotInfo, largeNSCost) {
	t.Helper()

	rep := newTreeRepository(t)
	src := treeSource(fmt.Sprintf("/sets/large-namespace-%d", n))
	root := &largeNSDir{count: n}

	var (
		info backupengine.TreeSnapshotInfo
		err  error
	)

	cost := largeNSMeasure(t, n, func() {
		info, err = rep.SnapshotTree(context.Background(), backupengine.TreeSnapshotRequest{
			Source:      src,
			RunID:       treeRunID,
			Root:        root,
			Description: fmt.Sprintf("one directory holding %d objects", n),
		})
	})
	if err != nil {
		t.Fatalf("SnapshotTree over %d entries: %v", n, err)
	}

	// The source's own count, not the report's: a snapshot that stored
	// 199,999 of 200,000 entries and reported 199,999 of them is
	// self-consistent and is exactly the hole a backup must never have.
	if got := root.served.Load(); got != int64(n) {
		t.Fatalf("the source handed over %d entries, the namespace holds %d", got, n)
	}

	if got := root.opens.Load(); got != 1 {
		t.Errorf("the source directory was opened %d times; a source listing is a forward cursor and may be passed over once", got)
	}

	if info.Files != int64(n) {
		t.Errorf("the snapshot reports %d files, the source produced %d", info.Files, n)
	}

	if info.Directories != 1 {
		t.Errorf("the snapshot reports %d directories over one flat namespace, want exactly 1", info.Directories)
	}

	if info.Incomplete != "" {
		t.Errorf("the snapshot of the namespace is incomplete: %s", info.Incomplete)
	}

	if want := int64(n) * largeNSPayloadSize; info.SourceBytesRead != want {
		t.Errorf("the run read %d bytes off the source, the namespace holds %d", info.SourceBytesRead, want)
	}

	return rep, src, info, cost
}

// --- the tests ---------------------------------------------------------

// TestSnapshotTreeHoldsALargeFlatNamespaceWithinABound is #792's bound
// asked of the snapshot path.
//
// The transport delivers one huge directory in bounded memory; this is
// the claim that handing those entries to a snapshot does not undo it.
// The failure it guards is concrete and it is written down in tree.go's
// own doc on treeDir.Child: a directory that answered a lookup by
// remembering everything it had seen would turn a set of a million
// objects into a million entries in memory. So would a run that queued an
// upload per entry, or one that held every reader it had opened, and none
// of those show up as a wrong answer -- they show up as a daemon that
// dies on the first customer whose producer writes a million files a day.
//
// It deliberately does not call t.Parallel: peakHeap samples process-wide
// HeapAlloc, so a sibling test's payload would be measured as this one's
// (see TestLargeStreamIsBoundedInMemory, which learned that the hard
// way).
func TestSnapshotTreeHoldsALargeFlatNamespaceWithinABound(t *testing.T) {
	entries := largeNSEntries(t)

	// Smallest first: the RSS figure logged beside the heap one is a
	// process high-water mark, and a delta around the smaller pass only
	// means anything while the bigger one has not happened yet.
	_, _, _, small := largeNSSnapshot(t, entries/10)
	_, _, _, large := largeNSSnapshot(t, entries)

	largeNSAssertBounded(t, "snapshotting a flat namespace", largeNSSnapshotBudget, small, large)
}

// TestVerificationOfALargeNamespaceIsBoundedAndCancellable is the same
// bound asked of the pass that runs AFTER the backup, plus the property
// that makes it survivable in a maintenance window.
//
// A structural verification of a large namespace resolves one stored
// object per entry, and the two ways to get that wrong are the two
// assertions here. Accumulating a record per object turns nightly
// verification into the thing that kills the daemon instead of the backup
// doing it. Not answering a cancelled context turns a maintenance window
// that has closed into a walk nobody can stop -- and #784's verification
// levels are a scheduled, recurring pass, so an operator's shutdown is
// not an exotic event but the normal end of one.
//
// The cancelled run's report is the third claim and the easiest to lose:
// a cancelled verification returns an error AND what it had managed to
// check, because those are two different halves of the answer, and it
// never returns the achieved level -- a run that was interrupted proved
// nothing, and a catalog row that recorded "structural" off the back of
// it would be a claim nobody checked.
func TestVerificationOfALargeNamespaceIsBoundedAndCancellable(t *testing.T) {
	entries := largeNSEntries(t)

	structural := backupengine.VerifyRequest{Level: model.LevelStructural}

	// Every entry plus the root directory: the walk resolves the
	// structure of both kinds of object, and the directory is where the
	// namespace's 200,000 entries are named.
	verifyOne := func(n int) (backupengine.VerifyReport, largeNSCost) {
		t.Helper()

		rep, _, info, _ := largeNSSnapshot(t, n)

		var (
			report backupengine.VerifyReport
			err    error
		)

		cost := largeNSMeasure(t, n, func() {
			report, err = rep.Verify(context.Background(), info.ID, structural)
		})
		if err != nil {
			t.Fatalf("Verify at %s over %d entries: %v", model.LevelStructural, n, err)
		}

		if report.Level != model.LevelStructural {
			t.Errorf("the verification achieved %q, want %q", report.Level, model.LevelStructural)
		}

		if want := int64(n) + 1; report.ObjectsVerified != want {
			t.Errorf("the verification resolved %d objects over a namespace of %d entries, want %d (every entry plus the directory naming them)",
				report.ObjectsVerified, n, want)
		}

		// Structural is not a content test, and a large namespace is
		// exactly where an implementation might be tempted to read
		// "just a little" to feel better about it.
		if report.FilesVerified != 0 || report.BytesVerified != 0 {
			t.Errorf("a structural verification read %d files and %d bytes; reading nothing is what makes it affordable on every backup",
				report.FilesVerified, report.BytesVerified)
		}

		if len(report.Errors) != 0 {
			t.Errorf("verifying an undamaged namespace reported %d findings: %v", len(report.Errors), report.Errors)
		}

		return report, cost
	}

	_, small := verifyOne(entries / 10)
	whole, large := verifyOne(entries)

	largeNSAssertBounded(t, "verifying a flat namespace", largeNSVerifyBudget, small, large)

	// --- and now the same verification, interrupted --------------------

	rep, _, info, _ := largeNSSnapshot(t, entries)

	// Cancelled a tenth of the way in, measured rather than guessed: the
	// full walk above is the only honest source for how long this one
	// takes, and a fixed sleep would either finish the walk on a fast
	// machine or never start it on a loaded one.
	cancelAfter := max(large.elapsed/10, 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(cancelAfter)
		cancel()
	}()

	start := time.Now()
	report, err := rep.Verify(ctx, info.ID, backupengine.VerifyRequest{Level: model.LevelStructural})
	stopped := time.Since(start)

	cancel()

	if err == nil {
		t.Fatalf("a verification cancelled after %s returned no error and a report of %d objects; a cancelled walk proved nothing and must say so",
			cancelAfter, report.ObjectsVerified)
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("the cancelled verification failed with %v; an operator who cancelled has to see context.Canceled, not whatever noticed first", err)
	}

	// Promptly: within the cancellation point plus the same again. A
	// walk that ran to completion and then noticed would pass an
	// errors.Is check and still be the bug.
	if limit := cancelAfter + large.elapsed/2; stopped > limit {
		t.Errorf("the cancelled verification took %s to return, cancelled at %s; the whole walk takes %s, so this one did not stop when it was asked",
			stopped.Round(time.Millisecond), cancelAfter, large.elapsed.Round(time.Millisecond))
	}

	if report.Level != "" {
		t.Errorf("the cancelled verification claims to have achieved %q; an interrupted run proved no level at all", report.Level)
	}

	if report.ObjectsVerified >= whole.ObjectsVerified {
		t.Errorf("the cancelled verification reports %d objects and the complete one reported %d; a report that is not partial means the cancellation did nothing",
			report.ObjectsVerified, whole.ObjectsVerified)
	}

	t.Logf("cancelled after %s: returned in %s having resolved %d of %d objects",
		cancelAfter, stopped.Round(time.Millisecond), report.ObjectsVerified, whole.ObjectsVerified)
}
