package kopia_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/kopia"
)

// This file is the acceptance suite for the set-scoped tree snapshot that
// #783 introduces: one backup-set run, one snapshot, one source, with the
// set's objects as a tree inside it.
//
// It drives backupengine.TreeRepository rather than this package's
// internals, for the same reason stream_test.go drives
// backupengine.StreamingRepository: the capability is what has to hold, and
// a future engine behind the same port has to pass this file unchanged. The
// two claims that cannot be made from out here -- that the run produced ONE
// source in the vendor's own namespace, and that the caller's tags reached
// the manifest -- are in tree_internal_test.go, which is allowed to look.

// treeHost and treeUser are the identity half of a backup set's source. A
// test is one machine, so they are constants; what they are NOT is a
// per-object identity, which is the whole difference this port exists for.
const (
	treeHost = "tree-host"
	treeUser = "tree-user"
)

// treeRunID is the manager-owned snapshot-run id the requests below
// carry. Every request needs one -- SnapshotTree refuses an empty one
// before it touches storage -- because it is the evidence crash
// reconciliation matches an orphaned manifest against, and the tests
// that are about its value say so themselves rather than relying on
// this.
const treeRunID = "run-0f3c1d6a"

// treeChunkSize is how big each file in the reuse measurement is.
//
// The number is chosen so the measurement cannot pass by luck. Three files
// of this size are 1.5 MiB of INCOMPRESSIBLE data, so a run that stored
// everything again writes about 1.5 MiB and a run that reused content
// writes about a third of that plus a few kilobytes of index and directory
// manifests. The fixed overhead is therefore two orders of magnitude below
// the gap being asserted, which is what makes the assertion about
// deduplication rather than about noise.
const treeChunkSize = 512 << 10

// newTreeRepository opens a repository under a fresh temp root and returns
// it as the tree capability.
func newTreeRepository(t *testing.T) backupengine.TreeRepository {
	t.Helper()

	ctx := context.Background()

	loc := localLocation(t, t.TempDir(), "production")

	eng := kopia.New()

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	t.Cleanup(func() {
		if err := rep.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	tree, ok := rep.(backupengine.TreeRepository)
	if !ok {
		t.Fatalf("%T does not implement backupengine.TreeRepository; the set-scoped snapshot is not wired up", rep)
	}

	return tree
}

// treeSource is the backup set's identity in the repository: one path for
// the whole set, not one per object.
func treeSource(setPath string) backupengine.Source {
	return backupengine.Source{Host: treeHost, User: treeUser, Path: setPath}
}

// --- the source fakes -------------------------------------------------

// memDir is a directory of a source tree held in memory.
//
// It is deliberately a FORWARD CURSOR and not a slice with an index: Open
// hands out a fresh pass and records how many passes were asked for, so a
// test can assert that the engine walked each directory exactly once.
// Nothing here rewinds, because nothing a real source offers rewinds.
type memDir struct {
	entries []backupengine.SourceEntry

	// openErr, when set, fails the attempt to start a pass.
	openErr error

	// failNextAt is the 1-based call number of Next that reports a failed
	// listing instead of an entry. Zero never fails.
	failNextAt int

	opens atomic.Int64
}

func (d *memDir) Open(context.Context) (backupengine.SourceDirIterator, error) {
	d.opens.Add(1)

	if d.openErr != nil {
		return nil, d.openErr
	}

	return &memIter{dir: d}, nil
}

type memIter struct {
	dir    *memDir
	next   int
	closed int
}

func (it *memIter) Next(context.Context) (backupengine.SourceEntry, bool, error) {
	it.next++

	if it.dir.failNextAt == it.next {
		return backupengine.SourceEntry{}, false, errListingFailed
	}

	if it.next > len(it.dir.entries) {
		return backupengine.SourceEntry{}, false, nil
	}

	return it.dir.entries[it.next-1], true, nil
}

func (it *memIter) Close() error {
	it.closed++

	return nil
}

var errListingFailed = errors.New("the source's directory listing failed")

var errStreamBroke = errors.New("the source's stream broke part way through")

// memStream is one object's content, readable as many times as the engine
// asks, counting opens so a test can see whether a run read it at all.
type memStream struct {
	data    []byte
	modTime time.Time

	// breakAfter, when non-negative, is how many bytes the reader hands
	// over before reporting a failure instead of the rest.
	breakAfter int

	opens atomic.Int64
}

func newMemStream(data []byte) *memStream {
	return &memStream{data: data, modTime: time.Unix(1700000000, 0).UTC(), breakAfter: -1}
}

func (s *memStream) ModTime() time.Time { return s.modTime }

func (s *memStream) Open(context.Context) (io.ReadCloser, error) {
	s.opens.Add(1)

	if s.breakAfter >= 0 {
		return io.NopCloser(io.MultiReader(
			bytes.NewReader(s.data[:s.breakAfter]),
			brokenReader{},
		)), nil
	}

	return io.NopCloser(bytes.NewReader(s.data)), nil
}

// brokenReader is the rest of a stream that is never coming.
type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errStreamBroke }

// fileEntry and dirEntry build the two kinds of SourceEntry, so the tests
// below read as trees rather than as struct literals.
func fileEntry(name string, s *memStream) backupengine.SourceEntry {
	return backupengine.SourceEntry{
		Name:    name,
		ModTime: s.modTime,
		Size:    int64(len(s.data)),
		Stream:  s,
	}
}

// unsizedFileEntry is the same file as reported by a source that cannot
// promise a stable size -- a live remote directory, which is the normal
// case rather than the exotic one. Size is negative and ModTime is
// whatever the listing said; neither may change what gets stored.
func unsizedFileEntry(name string, s *memStream) backupengine.SourceEntry {
	e := fileEntry(name, s)
	e.Size = -1

	return e
}

// dirEntry is a directory as the source side actually reports one: no
// size, no modification time. A directory in a remote listing carries no
// metadata this product can trust, and the adapter has to store and
// restore it anyway.
func dirEntry(name string, d *memDir) backupengine.SourceEntry {
	return backupengine.SourceEntry{
		Name: name,
		Size: -1,
		Dir:  d,
	}
}

// randomBytes is incompressible test data, which is what makes a byte
// measurement mean what it says.
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()

	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generating incompressible test data: %v", err)
	}

	return b
}

// --- the tests --------------------------------------------------------

// TestSnapshotTreeStoresOneSnapshotPerRun is the claim the whole port
// exists for.
//
// The per-object port this replaces writes one snapshot, one manifest and
// one Kopia source PER OBJECT, so a three-file set produced three of each
// and an operator asking "how many restore points does this set have" got
// the answer "three per night". One run is one snapshot here, and the
// counts below are the difference stated as arithmetic rather than as
// prose.
func TestSnapshotTreeStoresOneSnapshotPerRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep := newTreeRepository(t)

	nested := &memDir{entries: []backupengine.SourceEntry{
		fileEntry("c.txt", newMemStream([]byte("third object\n"))),
	}}
	root := &memDir{entries: []backupengine.SourceEntry{
		fileEntry("a.txt", newMemStream([]byte("first object\n"))),
		fileEntry("b.bin", newMemStream([]byte("second object\n"))),
		dirEntry("nested", nested),
	}}

	src := treeSource("/sets/daily")

	info, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{
		Source:      src,
		RunID:       treeRunID,
		Root:        root,
		Description: "nightly run",
		Tags: map[string]string{
			backupengine.TagKeyBackupSet: "set-daily",
			backupengine.TagKeyDomain:    "production",
		},
	})
	if err != nil {
		t.Fatalf("SnapshotTree: %v", err)
	}

	if info.ID == "" {
		t.Error("SnapshotTree returned no snapshot id")
	}

	if info.Files != 3 {
		t.Errorf("snapshot holds %d files, want 3", info.Files)
	}

	// Root plus nested: a set-scoped snapshot has a directory structure,
	// which is precisely what the per-object port could not express.
	if info.Directories != 2 {
		t.Errorf("snapshot holds %d directories, want 2 (the root and nested)", info.Directories)
	}

	if info.Incomplete != "" {
		t.Errorf("snapshot is incomplete: %s", info.Incomplete)
	}

	// Every directory was walked exactly once. A second Open would mean
	// something in the engine rewound a forward cursor, which a real
	// source cannot serve.
	if got := root.opens.Load(); got != 1 {
		t.Errorf("the root directory was opened %d times, want exactly 1", got)
	}

	if got := nested.opens.Load(); got != 1 {
		t.Errorf("the nested directory was opened %d times, want exactly 1", got)
	}

	snaps, err := rep.ListSnapshots(ctx, src)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}

	if len(snaps) != 1 {
		t.Fatalf("the set has %d snapshots after one run, want exactly 1", len(snaps))
	}

	if snaps[0].ID != info.ID {
		t.Errorf("ListSnapshots reports id %q, SnapshotTree reported %q", snaps[0].ID, info.ID)
	}

	stats, err := rep.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if stats.Snapshots != 1 {
		t.Errorf("the repository holds %d snapshots after one run, want exactly 1", stats.Snapshots)
	}

	// Stats.Sources counts distinct backup-set tags, which is the
	// co-tenancy number an isolated domain is checked against. One run of
	// one set must move it by exactly one.
	if stats.Sources != 1 {
		t.Errorf("the repository reports %d backup sets sharing it, want exactly 1", stats.Sources)
	}
}

// TestSnapshotTreeReusesContentOnASecondRun is the measurement behind
// TreeSnapshotInfo's three-byte-counts doc.
//
// It is not enough for the second run to succeed: the point of storing a
// set as one snapshot is that unchanged content costs nothing to keep, and
// the only evidence of that is what the run actually pushed into storage.
// So this asserts the GAP -- run two writes a fraction of run one and a
// fraction of the tree's logical size -- and then asserts that the report
// keeps the two numbers apart, because presenting Bytes as "uploaded" is
// the specific lie that would make a deduplicated repository look like it
// grows by the size of the source every night.
func TestSnapshotTreeReusesContentOnASecondRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep := newTreeRepository(t)

	src := treeSource("/sets/reuse")

	stable := newMemStream(randomBytes(t, treeChunkSize))
	alsoStable := newMemStream(randomBytes(t, treeChunkSize))
	changing := newMemStream(randomBytes(t, treeChunkSize))

	build := func() *memDir {
		nested := &memDir{entries: []backupengine.SourceEntry{
			fileEntry("also-stable.bin", alsoStable),
		}}

		return &memDir{entries: []backupengine.SourceEntry{
			fileEntry("stable.bin", stable),
			fileEntry("changing.bin", changing),
			dirEntry("nested", nested),
		}}
	}

	first, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, RunID: treeRunID, Root: build()})
	if err != nil {
		t.Fatalf("SnapshotTree (first run): %v", err)
	}

	// One file is rewritten in place, at the same size, with entirely new
	// content. Nothing about the listing changes.
	changing.data = randomBytes(t, treeChunkSize)

	second, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, RunID: treeRunID, Root: build()})
	if err != nil {
		t.Fatalf("SnapshotTree (second run): %v", err)
	}

	const logical = 3 * treeChunkSize

	if first.Bytes != logical {
		t.Errorf("first run scanned %d bytes, want %d", first.Bytes, logical)
	}

	if second.Bytes != first.Bytes {
		t.Errorf("the tree's logical size changed between runs: %d then %d", first.Bytes, second.Bytes)
	}

	// Every run reads every byte the source offers. Reuse happens below
	// this boundary, on content already read, never by trusting a
	// scan-time size or mtime.
	if first.SourceBytesRead != logical {
		t.Errorf("first run read %d bytes from the source, want the whole tree (%d)", first.SourceBytesRead, logical)
	}

	if second.SourceBytesRead != logical {
		t.Errorf("second run read %d bytes from the source, want the whole tree (%d)", second.SourceBytesRead, logical)
	}

	if first.RepositoryBytesWritten < logical/2 {
		t.Fatalf("first run wrote only %d bytes for a %d byte tree; the measurement below would prove nothing",
			first.RepositoryBytesWritten, logical)
	}

	// The two thresholds answer two different questions: the first says
	// this run cost far less than the run that stored everything, the
	// second says it cost far less than the tree it snapshotted. A number
	// that passed one and failed the other would be reuse that did not
	// actually happen.
	if wantBelow := first.RepositoryBytesWritten * 4 / 10; second.RepositoryBytesWritten >= wantBelow {
		t.Errorf("second run wrote %d bytes, want below 40%% of the first run's %d (%d); content was not reused",
			second.RepositoryBytesWritten, first.RepositoryBytesWritten, wantBelow)
	}

	if wantBelow := second.Bytes * 6 / 10; second.RepositoryBytesWritten >= wantBelow {
		t.Errorf("second run wrote %d bytes for a %d byte tree, want below 60%% (%d); content was not reused",
			second.RepositoryBytesWritten, second.Bytes, wantBelow)
	}

	// The report keeps scanned and written apart. If these were ever the
	// same number, every assertion above would be vacuous and an operator
	// reading "uploaded" would be reading the size of their source.
	if second.RepositoryBytesWritten == second.Bytes {
		t.Error("the second run reports the same number for scanned bytes and written bytes; one of them is not measured")
	}

	t.Logf("run 1: scanned=%d read=%d written=%d", first.Bytes, first.SourceBytesRead, first.RepositoryBytesWritten)
	t.Logf("run 2: scanned=%d read=%d written=%d", second.Bytes, second.SourceBytesRead, second.RepositoryBytesWritten)

	snaps, err := rep.ListSnapshots(ctx, src)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}

	if len(snaps) != 2 {
		t.Errorf("the set has %d snapshots after two runs, want 2", len(snaps))
	}
}

// TestSnapshotTreeRestoresEveryByte is the only assertion that makes the
// rest of this file mean anything.
//
// A snapshot is a claim that the bytes can be got back, and the counters
// above are all reports about a claim nobody checked. So this one restores
// the whole tree and compares every file's SHA-256 and the layout it landed
// in, because a restore that puts the right bytes in the wrong place is
// exactly the failure the per-object port had.
func TestSnapshotTreeRestoresEveryByte(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep := newTreeRepository(t)

	want := map[string][]byte{
		"a.txt":                 []byte("first object\n"),
		"b.bin":                 randomBytes(t, 128<<10),
		"nested/c.txt":          []byte("third object\n"),
		"nested/deeper/d.bin":   randomBytes(t, 64<<10),
		"nested/deeper/e.empty": {},
	}

	deeper := &memDir{entries: []backupengine.SourceEntry{
		fileEntry("d.bin", newMemStream(want["nested/deeper/d.bin"])),
		fileEntry("e.empty", newMemStream(want["nested/deeper/e.empty"])),
	}}
	nested := &memDir{entries: []backupengine.SourceEntry{
		fileEntry("c.txt", newMemStream(want["nested/c.txt"])),
		dirEntry("deeper", deeper),
	}}
	root := &memDir{entries: []backupengine.SourceEntry{
		// One of these arrives with no size from the listing, because
		// a source that cannot promise one is the normal case and the
		// bytes that come back must not depend on it.
		unsizedFileEntry("a.txt", newMemStream(want["a.txt"])),
		fileEntry("b.bin", newMemStream(want["b.bin"])),
		dirEntry("nested", nested),
	}}

	src := treeSource("/sets/restore")

	info, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, RunID: treeRunID, Root: root})
	if err != nil {
		t.Fatalf("SnapshotTree: %v", err)
	}

	target := filepath.Join(t.TempDir(), "restored")

	// SkipOwners is what an unprivileged restore has to do, and it is
	// what a source tree needs regardless: a remote object has no local
	// uid/gid, so the entries carry none and a restore that tried to
	// apply them would be chowning every file to root.
	report, err := rep.Restore(ctx, info.ID, backupengine.RestoreRequest{TargetPath: target, SkipOwners: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if report.Files != int64(len(want)) {
		t.Errorf("restore wrote %d files, want %d", report.Files, len(want))
	}

	got := map[string][]byte{}

	if err := filepath.WalkDir(target, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(target, p)
		if err != nil {
			return err
		}

		body, err := os.ReadFile(p) //nolint:gosec // the path is this test's own temp directory.
		if err != nil {
			return err
		}

		got[filepath.ToSlash(rel)] = body

		return nil
	}); err != nil {
		t.Fatalf("walking the restored tree: %v", err)
	}

	if len(got) != len(want) {
		t.Errorf("restored tree holds %d files, want %d (%s)", len(got), len(want), strings.Join(sortedKeys(got), ", "))
	}

	for name, body := range want {
		restored, ok := got[name]
		if !ok {
			t.Errorf("%s is missing from the restored tree; its layout was not preserved", name)

			continue
		}

		if sum(restored) != sum(body) {
			t.Errorf("%s restored to a different SHA-256: got %s, want %s", name, sum(restored), sum(body))
		}
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)

	return hex.EncodeToString(h[:])
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// TestSnapshotTreeLeavesNoSnapshotWhenAStreamBreaks is the "or stores
// nothing at all" half of SnapshotTree's contract.
//
// Kopia treats a failure to read one entry as a property of that entry: it
// records the error against the directory and hands back a well-formed
// manifest with an error count on it. Saving that manifest would advertise
// a restore point with a hole in it, so the assertion is not that the call
// failed -- it is that the repository holds NOTHING for this set
// afterwards.
func TestSnapshotTreeLeavesNoSnapshotWhenAStreamBreaks(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep := newTreeRepository(t)

	broken := newMemStream(randomBytes(t, 128<<10))
	broken.breakAfter = 4096

	root := &memDir{entries: []backupengine.SourceEntry{
		fileEntry("fine.txt", newMemStream([]byte("this one reads cleanly\n"))),
		fileEntry("broken.bin", broken),
	}}

	src := treeSource("/sets/broken-stream")

	if _, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, RunID: treeRunID, Root: root}); err == nil {
		t.Fatal("SnapshotTree returned no error for a stream that broke part way through")
	} else if !errors.Is(err, errStreamBroke) {
		t.Errorf("SnapshotTree reported %v, want the source's own failure (%v)", err, errStreamBroke)
	}

	assertNoSnapshots(t, ctx, rep, src)
}

// TestSnapshotTreeCancellationUnblocksAReadInFlight is the reason the run
// tracks the readers it handed out.
//
// Kopia's copy loop checks its own cancellation flag BETWEEN reads, never
// during one, so a reader parked on a socket that is never going to
// answer is not reachable from inside the uploader: a cancelled backup
// window would hang until the transport's own timeout, which for a stalled
// NAS is "never". Closing the reader from outside is what turns that into
// a run that ends.
//
// The second assertion is the one that is easy to lose. A read torn down
// that way fails with whatever the transport felt like reporting, and an
// operator who cancelled a backup must be told the backup was cancelled,
// not that something used a closed network connection.
func TestSnapshotTreeCancellationUnblocksAReadInFlight(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rep := newTreeRepository(t)

	stalled := &stalledStream{opened: make(chan struct{}), done: make(chan struct{})}

	root := &memDir{entries: []backupengine.SourceEntry{
		{Name: "stalled.bin", ModTime: time.Unix(1700000000, 0).UTC(), Size: 1 << 20, Stream: stalled},
	}}

	src := treeSource("/sets/cancelled")

	go func() {
		<-stalled.opened
		cancel()
	}()

	_, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, RunID: treeRunID, Root: root})
	if err == nil {
		t.Fatal("SnapshotTree stored a snapshot of a source it never finished reading")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("SnapshotTree reported %v, want the caller's own cancellation", err)
	}

	assertNoSnapshots(t, context.Background(), rep, src)
}

// stalledStream is a source whose reader never answers until it is closed
// from outside, which is what a remote read against a machine that has
// gone away looks like from in here.
type stalledStream struct {
	// opened is closed the first time the engine asks for the bytes,
	// which is the moment the cancellation has something to interrupt.
	opened chan struct{}

	// done is closed by Close, which is the only thing that ever ends a
	// Read on this stream.
	done chan struct{}

	openOnce  sync.Once
	closeOnce sync.Once
}

func (s *stalledStream) ModTime() time.Time { return time.Unix(1700000000, 0).UTC() }

func (s *stalledStream) Open(context.Context) (io.ReadCloser, error) {
	s.openOnce.Do(func() { close(s.opened) })

	return s, nil
}

func (s *stalledStream) Read([]byte) (int, error) {
	<-s.done

	return 0, errors.New("the stalled reader was closed")
}

func (s *stalledStream) Close() error {
	s.closeOnce.Do(func() { close(s.done) })

	return nil
}

// TestSnapshotTreeLeavesNoSnapshotWhenAListingFails covers the other way a
// tree goes wrong, in both the places it can happen.
//
// The two subtests are not the same path. A failure listing the ROOT ends
// the upload outright; a failure listing a SUBDIRECTORY is reported to the
// progress sink and turns into a manifest that claims the directory was
// empty, which is the one failure mode a backup must never have. Only the
// second one proves the guard.
func TestSnapshotTreeLeavesNoSnapshotWhenAListingFails(t *testing.T) {
	t.Parallel()

	t.Run("root", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		rep := newTreeRepository(t)

		root := &memDir{
			entries:    []backupengine.SourceEntry{fileEntry("a.txt", newMemStream([]byte("a\n")))},
			failNextAt: 1,
		}

		src := treeSource("/sets/broken-root-listing")

		if _, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, RunID: treeRunID, Root: root}); err == nil {
			t.Fatal("SnapshotTree returned no error for a root directory whose listing failed")
		} else if !errors.Is(err, errListingFailed) {
			t.Errorf("SnapshotTree reported %v, want the source's own failure (%v)", err, errListingFailed)
		}

		assertNoSnapshots(t, ctx, rep, src)
	})

	t.Run("subdirectory", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		rep := newTreeRepository(t)

		nested := &memDir{
			entries:    []backupengine.SourceEntry{fileEntry("c.txt", newMemStream([]byte("c\n")))},
			failNextAt: 2,
		}
		root := &memDir{entries: []backupengine.SourceEntry{
			fileEntry("a.txt", newMemStream([]byte("a\n"))),
			dirEntry("nested", nested),
		}}

		src := treeSource("/sets/broken-nested-listing")

		if _, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, RunID: treeRunID, Root: root}); err == nil {
			t.Fatal("SnapshotTree returned no error for a subdirectory whose listing failed")
		} else if !errors.Is(err, errListingFailed) {
			t.Errorf("SnapshotTree reported %v, want the source's own failure (%v)", err, errListingFailed)
		}

		assertNoSnapshots(t, ctx, rep, src)
	})
}

// TestSnapshotTreeRefusesEntriesNothingCanBeReadFrom is the refusal set.
//
// Every case here is a source description that could be stored as
// SOMETHING -- an empty file, a name with a separator in it that lands
// somewhere else in the tree -- and storing it is how a backup acquires a
// hole nobody can see. They are refusals with sentences instead.
func TestSnapshotTreeRefusesEntriesNothingCanBeReadFrom(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		root backupengine.SourceDir
		want string
	}{
		"no root at all": {
			root: nil,
			want: "root",
		},
		"neither directory nor stream": {
			root: &memDir{entries: []backupengine.SourceEntry{{Name: "hollow"}}},
			want: "hollow",
		},
		"name carrying a separator": {
			root: &memDir{entries: []backupengine.SourceEntry{
				fileEntry("nested/escaped.txt", newMemStream([]byte("x\n"))),
			}},
			want: "nested/escaped.txt",
		},
		"empty name": {
			root: &memDir{entries: []backupengine.SourceEntry{
				fileEntry("", newMemStream([]byte("x\n"))),
			}},
			want: "name",
		},
		"the current directory": {
			root: &memDir{entries: []backupengine.SourceEntry{
				dirEntry(".", &memDir{}),
			}},
			want: ".",
		},
		"the parent directory": {
			root: &memDir{entries: []backupengine.SourceEntry{
				dirEntry("..", &memDir{}),
			}},
			want: "..",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			rep := newTreeRepository(t)

			src := treeSource("/sets/refused")

			_, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, RunID: treeRunID, Root: tc.root})
			if err == nil {
				t.Fatal("SnapshotTree stored the tree instead of refusing it")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not say what was wrong; it should name %q", err, tc.want)
			}

			assertNoSnapshots(t, ctx, rep, src)
		})
	}
}

// assertNoSnapshots is the shared half of every failure case: the run
// produced no restore point, so there is nothing for a later pass to have
// to decide whether to trust.
func assertNoSnapshots(t *testing.T, ctx context.Context, rep backupengine.TreeRepository, src backupengine.Source) {
	t.Helper()

	snaps, err := rep.ListSnapshots(ctx, src)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}

	if len(snaps) != 0 {
		t.Fatalf("the failed run left %d snapshot(s) behind; a torn tree must produce no manifest at all", len(snaps))
	}

	stats, err := rep.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if stats.Snapshots != 0 {
		t.Errorf("the repository holds %d snapshots after a failed run, want none", stats.Snapshots)
	}
}

// compressibleBytes is the counterpart to randomBytes: a short pattern
// repeated until it is n bytes long, which is data a repository can
// store in far less space than it occupies at the source.
//
// It exists to separate the two savings a backup has, because the
// arithmetic this file refuses -- read minus written -- adds them
// together and calls the total deduplication.
func compressibleBytes(pattern string, n int) []byte {
	return bytes.Repeat([]byte(pattern), n/len(pattern)+1)[:n]
}

// TestSnapshotTreeReportsNoReuseForAFirstSnapshotOfCompressibleData is the
// regression the arithmetic definition of reuse fails.
//
// SourceBytesRead minus RepositoryBytesWritten is not deduplication. That
// difference also holds compression, and the pack and index overhead of
// storing anything at all, and this run has no reuse to report and no way
// to have earned any: a first-ever snapshot, of content nothing has seen,
// into an empty repository. The honest answer is zero and the arithmetic
// cannot produce it.
//
// WHICH WAY it goes wrong is a policy knob, and both ways are wrong. The
// tree here is highly compressible, so under an upload policy with a
// compressor configured the run stores a fraction of what it read and the
// difference reports most of a brand new backup as reused -- an operator
// told their first night was mostly free, and a catalog recording a dedup
// ratio for a run that deduplicated nothing. This adapter uploads under
// policy.DefaultPolicy, whose file compressor is "none", so today the
// difference goes the other way: storing costs MORE than reading, because
// pack headers, index blobs and directory manifests count as written and
// are not source content at all, and the arithmetic yields a NEGATIVE
// reuse that only looks like the right answer after something clamps it.
//
// So what keeps the zero below from being vacuous is not a threshold on
// either byte count -- that would pin whichever way the knob happens to
// be set today. It is that the two definitions visibly disagree on this
// fixture, which is true under both settings and is the entire claim.
func TestSnapshotTreeReportsNoReuseForAFirstSnapshotOfCompressibleData(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep := newTreeRepository(t)

	src := treeSource("/sets/compressible")

	// Two different patterns, so nothing in this tree deduplicates
	// against anything else in it. Any saving here would be compression,
	// and compression is not reuse.
	root := &memDir{entries: []backupengine.SourceEntry{
		fileEntry("prose.txt", newMemStream(compressibleBytes(
			"the quick brown fox jumps over the lazy dog\n", treeChunkSize))),
		fileEntry("service.log", newMemStream(compressibleBytes(
			"2026-01-01T00:00:00Z level=info msg=\"nothing happened\"\n", treeChunkSize))),
	}}

	info, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, RunID: treeRunID, Root: root})
	if err != nil {
		t.Fatalf("SnapshotTree: %v", err)
	}

	arithmetic := info.SourceBytesRead - info.RepositoryBytesWritten

	t.Logf("first compressible run: scanned=%d read=%d written=%d reused=%d measured=%t (read-written=%d)",
		info.Bytes, info.SourceBytesRead, info.RepositoryBytesWritten,
		info.ContentReusedBytes, info.ContentReuseMeasured, arithmetic)

	if !info.ContentReuseMeasured {
		t.Fatal("the run reports reuse as not measured; the engine's dedup accounting was not readable, which is what a vendor upgrade that renamed the counter looks like from here")
	}

	if info.ContentReusedBytes != 0 {
		t.Errorf("a first snapshot reports %d bytes of reused content, want 0; nothing in this repository existed before this run",
			info.ContentReusedBytes)
	}

	if arithmetic == info.ContentReusedBytes {
		t.Fatalf("read minus written (%d) is the same number as the reported reuse (%d) on this fixture, so nothing here tells the engine's dedup accounting apart from the arithmetic that conflates it with compression and storage overhead",
			arithmetic, info.ContentReusedBytes)
	}
}

// TestSnapshotTreeMeasuresContentReuseOfAnUnchangedTree is the other half:
// the number is not just honestly zero, it is actually wired to something.
//
// A second run over a tree nothing touched hands the repository content it
// already holds, byte for byte, so the engine's own dedup accounting has to
// see nearly the whole tree. A counter that was never incremented, or one
// whose name the adapter looks up by a string that no longer exists, would
// report a perfectly honest zero here -- which is why the assertion is a
// large fraction of the tree rather than "greater than zero".
func TestSnapshotTreeMeasuresContentReuseOfAnUnchangedTree(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep := newTreeRepository(t)

	src := treeSource("/sets/unchanged")

	first := newMemStream(randomBytes(t, treeChunkSize))
	second := newMemStream(randomBytes(t, treeChunkSize))
	third := newMemStream(randomBytes(t, treeChunkSize))

	build := func() *memDir {
		nested := &memDir{entries: []backupengine.SourceEntry{fileEntry("third.bin", third)}}

		return &memDir{entries: []backupengine.SourceEntry{
			fileEntry("first.bin", first),
			fileEntry("second.bin", second),
			dirEntry("nested", nested),
		}}
	}

	one, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, RunID: treeRunID, Root: build()})
	if err != nil {
		t.Fatalf("SnapshotTree (first run): %v", err)
	}

	two, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, RunID: "run-2e90ab41", Root: build()})
	if err != nil {
		t.Fatalf("SnapshotTree (second run): %v", err)
	}

	const logical = 3 * treeChunkSize

	t.Logf("run 1: read=%d written=%d reused=%d measured=%t",
		one.SourceBytesRead, one.RepositoryBytesWritten, one.ContentReusedBytes, one.ContentReuseMeasured)
	t.Logf("run 2: read=%d written=%d reused=%d measured=%t",
		two.SourceBytesRead, two.RepositoryBytesWritten, two.ContentReusedBytes, two.ContentReuseMeasured)

	if !one.ContentReuseMeasured || !two.ContentReuseMeasured {
		t.Fatalf("reuse was not measured (run 1: %t, run 2: %t); the adapter could not read the engine's dedup accounting",
			one.ContentReuseMeasured, two.ContentReuseMeasured)
	}

	if one.ContentReusedBytes != 0 {
		t.Errorf("the first run reports %d bytes of reused content for a repository that held nothing, want 0", one.ContentReusedBytes)
	}

	// The delta is this run's, not the repository's lifetime total. A
	// total would count the first run's contents too and would only ever
	// grow, which is how "reuse" turns into a number that always looks
	// better than the run before it.
	if want := int64(logical) * 9 / 10; two.ContentReusedBytes < want {
		t.Errorf("the second run reports %d bytes of reused content for an unchanged %d byte tree, want at least %d",
			two.ContentReusedBytes, logical, want)
	}

	if two.ContentReusedBytes > int64(logical)*2 {
		t.Errorf("the second run reports %d bytes of reused content for a %d byte tree; that is more content than the tree has, so this is a lifetime total rather than this run's delta",
			two.ContentReusedBytes, logical)
	}
}

// TestSnapshotTreeReportsTheRunTagOnTheReadSide is the write half and the
// read half of the attribution contract, asserted together because either
// one alone is a claim nothing can act on.
//
// Crash reconciliation adopts an orphaned manifest only when its run,
// domain and set all match a catalog row exactly, and it can only do that
// if the adapter wrote the run id the caller could not omit AND the read
// side gives all three back. A snapshot that stored the tag but reported
// SnapshotInfo.Tags as nil would send reconciliation back to matching on
// time, which is the ambiguity the tag exists to remove.
func TestSnapshotTreeReportsTheRunTagOnTheReadSide(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep := newTreeRepository(t)

	src := treeSource("/sets/attributed")

	root := &memDir{entries: []backupengine.SourceEntry{
		fileEntry("a.txt", newMemStream([]byte("first object\n"))),
	}}

	const runID = "run-9c41f0d2"

	info, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{
		Source: src,
		RunID:  runID,
		Root:   root,
		Tags: map[string]string{
			backupengine.TagKeyBackupSet: "set-attributed",
			backupengine.TagKeyDomain:    "production",
		},
	})
	if err != nil {
		t.Fatalf("SnapshotTree: %v", err)
	}

	want := map[string]string{
		backupengine.TagKeyRun:       runID,
		backupengine.TagKeyBackupSet: "set-attributed",
		backupengine.TagKeyDomain:    "production",
	}

	snaps, err := rep.ListSnapshots(ctx, src)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}

	if len(snaps) != 1 {
		t.Fatalf("the set has %d snapshots after one run, want exactly 1", len(snaps))
	}

	assertTags(t, "ListSnapshots", snaps[0].Tags, want)

	found, err := rep.LookupSnapshot(ctx, info.ID)
	if err != nil {
		t.Fatalf("LookupSnapshot(%s): %v", info.ID, err)
	}

	assertTags(t, "LookupSnapshot", found.Tags, want)
}

// assertTags compares one read path's tags against what the run stored.
// Both paths are asserted against the same map because reconciliation
// uses whichever it has to hand, and a snapshot that is attributable
// through one and not the other is attributable by luck.
func assertTags(t *testing.T, path string, got, want map[string]string) {
	t.Helper()

	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s reports %s=%q, want %q (tags: %v)", path, key, got[key], value, got)
		}
	}

	if len(got) != len(want) {
		t.Errorf("%s reports %d tags (%v), want exactly the %d written", path, len(got), got, len(want))
	}
}

// TestSnapshotTreeRefusesARunWithoutARunID is the refusal that keeps the
// tag from being optional in practice.
//
// An id the caller may leave out is an id that is missing on the path
// nobody exercises by hand, and the missing case cannot be repaired
// afterwards: nothing later can work out which run wrote a manifest that
// never said. So the run is refused before any storage is touched, and
// what makes that assertion worth writing is the second half -- the
// repository holds nothing for this source afterwards, rather than an
// unattributable snapshot plus an error.
func TestSnapshotTreeRefusesARunWithoutARunID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep := newTreeRepository(t)

	src := treeSource("/sets/unattributed")

	root := &memDir{entries: []backupengine.SourceEntry{
		fileEntry("a.txt", newMemStream([]byte("first object\n"))),
	}}

	_, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{
		Source: src,
		Root:   root,
		Tags: map[string]string{
			backupengine.TagKeyBackupSet: "set-unattributed",
			backupengine.TagKeyDomain:    "production",
		},
	})
	if err == nil {
		t.Fatal("SnapshotTree stored a snapshot for a run that did not say which run it was")
	}

	if !strings.Contains(err.Error(), "run id") {
		t.Errorf("SnapshotTree reported %v, want a refusal naming the missing run id", err)
	}

	// The source was never read either: the refusal happens before the
	// walk, not after a tree has been pulled off a remote for nothing.
	if got := root.opens.Load(); got != 0 {
		t.Errorf("the refused run opened the source %d times, want 0", got)
	}

	assertNoSnapshots(t, ctx, rep, src)
}
