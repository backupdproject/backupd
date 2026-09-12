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

	first, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, Root: build()})
	if err != nil {
		t.Fatalf("SnapshotTree (first run): %v", err)
	}

	// One file is rewritten in place, at the same size, with entirely new
	// content. Nothing about the listing changes.
	changing.data = randomBytes(t, treeChunkSize)

	second, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, Root: build()})
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

	info, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, Root: root})
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

	if _, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, Root: root}); err == nil {
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

	_, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, Root: root})
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

		if _, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, Root: root}); err == nil {
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

		if _, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, Root: root}); err == nil {
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

			_, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, Root: tc.root})
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
