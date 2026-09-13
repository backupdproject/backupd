package source_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backend"
	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/source"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// These tests drive source.Tree from the side the backup engine is on:
// something that opens a directory, asks it for children one at a time,
// reads a child's stream to its end and moves on. Nothing here calls a
// sink, because a tree run has none - which is the whole point of the
// type - so the assertions are about the SHAPE handed to the engine, the
// counters the run reports about itself, and what happens to the run when
// the source moves underneath it.

// --- a consumer that walks the way a pull-based uploader walks ----------

// walkedDir is one directory as a consumer saw it: the order its children
// arrived in, its files' bytes, and its subdirectories.
type walkedDir struct {
	order []string
	files map[string]walkedFile
	dirs  map[string]*walkedDir
}

// walkedFile is one file entry as the consumer saw it, which is the
// entry's metadata plus every byte the stream produced.
type walkedFile struct {
	body    []byte
	size    int64
	modTime time.Time
}

func newWalkedDir() *walkedDir {
	return &walkedDir{files: map[string]walkedFile{}, dirs: map[string]*walkedDir{}}
}

// walkDir is the test's stand-in for the uploader: depth first, one entry
// at a time, every stream read to EOF and closed, the iterator closed on
// the way out.
//
// It returns what it managed to collect ALONGSIDE the error, because half
// the assertions in this file are about what the consumer had seen at the
// moment the run failed.
func walkDir(ctx context.Context, dir backupengine.SourceDir) (*walkedDir, error) {
	it, err := dir.Open(ctx)
	if err != nil {
		return nil, err
	}

	defer it.Close() //nolint:errcheck // the iterator's close has nothing of its own to report.

	out := newWalkedDir()

	for {
		entry, ok, err := it.Next(ctx)
		if err != nil {
			return out, err
		}

		if !ok {
			return out, nil
		}

		out.order = append(out.order, entry.Name)

		if entry.IsDir() {
			child, cerr := walkDir(ctx, entry.Dir)
			if child != nil {
				out.dirs[entry.Name] = child
			}

			if cerr != nil {
				return out, cerr
			}

			continue
		}

		body, rerr := readAll(ctx, entry.Stream)
		if rerr != nil {
			return out, rerr
		}

		out.files[entry.Name] = walkedFile{body: body, size: entry.Size, modTime: entry.ModTime}
	}
}

// readAll reads one entry's stream the way the engine does: one open, one
// forward pass, one close.
func readAll(ctx context.Context, s backupengine.StreamSource) ([]byte, error) {
	rc, err := s.Open(ctx)
	if err != nil {
		return nil, err
	}

	defer rc.Close() //nolint:errcheck // the run closes it too; this is the ordinary owner.

	var (
		out []byte
		buf = make([]byte, 4096)
	)

	for {
		n, err := rc.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}

		if errors.Is(err, io.EOF) {
			return out, nil
		}

		if err != nil {
			return out, err
		}
	}
}

func (d *walkedDir) fileNames() []string {
	out := make([]string, 0, len(d.files))
	for n := range d.files {
		out = append(out, n)
	}

	sort.Strings(out)

	return out
}

func (d *walkedDir) dirNames() []string {
	out := make([]string, 0, len(d.dirs))
	for n := range d.dirs {
		out = append(out, n)
	}

	sort.Strings(out)

	return out
}

// --- stand-ins this file needs and the shared fakes do not have ---------

// recordingStreamer is a fake source that remembers which paths it was
// asked to open, so "an unsafe path is never opened" can be asserted
// against the transport surface rather than inferred from a counter.
type recordingStreamer struct {
	*fakeSource

	mu     sync.Mutex
	opened []string
}

func (r *recordingStreamer) OpenSourceStream(ctx context.Context, src transport.Source, remotePath string) (io.ReadCloser, error) {
	r.mu.Lock()
	r.opened = append(r.opened, remotePath)
	r.mu.Unlock()

	return r.fakeSource.OpenSourceStream(ctx, src, remotePath)
}

func (r *recordingStreamer) openedPaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.opened...)
}

// listEnumerator yields a fixed sequence of paths in the order it is
// given, which is how an enumeration that is NOT depth-first grouped is
// produced on demand: the shared fakeSource sorts, and sorting makes
// every prefix contiguous, so it cannot express the shape being refused.
type listEnumerator struct {
	src   *fakeSource
	order []string
}

func (l listEnumerator) Enumerate(
	ctx context.Context,
	_ transport.Source,
	_ transport.EnumerateOptions,
	yield func(transport.RemoteArtifact) error,
) error {
	for _, p := range l.order {
		if err := ctx.Err(); err != nil {
			return err
		}

		o, ok := l.src.get(p)
		if !ok {
			continue
		}

		l.src.mu.Lock()
		art := l.src.artifact(p, o)
		l.src.mu.Unlock()

		if err := yield(art); err != nil {
			return err
		}
	}

	return nil
}

// signallingEnumerator reports when the enumeration goroutine has
// actually RETURNED, which is the goroutine-leak assertion: a tree that
// closed its session while its producer was still walking would pass
// every other test in this file.
type signallingEnumerator struct {
	inner    transport.Enumerator
	returned chan struct{}
}

func newSignallingEnumerator(inner transport.Enumerator) *signallingEnumerator {
	return &signallingEnumerator{inner: inner, returned: make(chan struct{})}
}

func (s *signallingEnumerator) Enumerate(
	ctx context.Context,
	src transport.Source,
	opts transport.EnumerateOptions,
	yield func(transport.RemoteArtifact) error,
) error {
	defer close(s.returned)

	return s.inner.Enumerate(ctx, src, opts, yield)
}

func (s *signallingEnumerator) waitReturned(t *testing.T) {
	t.Helper()

	select {
	case <-s.returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the enumeration goroutine was still running after the tree was closed")
	}
}

// blockingSource is a source whose reads never finish on their own. It is
// how "a cancelled run unblocks a read that is waiting on a socket" is
// tested without a socket: the reader is adapter_test.go's blockingReader,
// which is the same stand-in the per-object path's cancellation test
// drives, so the two paths are proven against one fixture.
type blockingSource struct {
	*fakeSource

	reader *blockingReader
}

func newBlockingSource(inner *fakeSource) *blockingSource {
	return &blockingSource{
		fakeSource: inner,
		reader:     &blockingReader{blocked: make(chan struct{}), released: make(chan struct{})},
	}
}

func (b *blockingSource) OpenSourceStream(ctx context.Context, _ transport.Source, remotePath string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if _, ok := b.get(remotePath); !ok {
		return nil, transport.NewError(transport.NotFound, "open_source_stream", errors.New("object not found"))
	}

	return b.reader, nil
}

// waitReading blocks until a read has actually begun, so a cancellation
// is ordered against the read rather than raced with it.
func (b *blockingSource) waitReading(t *testing.T) {
	t.Helper()

	select {
	case <-b.reader.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("no read ever started")
	}
}

// --- the shape of the tree ---------------------------------------------

// The whole point of the type: a nested source arrives as a nested tree,
// with the same names, the same contents and the same nesting, pulled by
// a consumer that only ever asks for the next child.
func TestOpenTreeDeliversTheSourceAsANestedTree(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("root1.txt", []byte("first at the root"), 1_700_000_001)
	f.put("root2.txt", []byte("second at the root"), 1_700_000_002)
	f.put("sub/a.txt", patternBytes(9000, 7), 1_700_000_003)
	f.put("sub/b.txt", []byte("b in sub"), 1_700_000_004)
	f.put("sub/deep/c.txt", []byte("c in deep"), 1_700_000_005)

	var results []source.Result

	opts := liveOpts()
	opts.OnResult = func(r source.Result) { results = append(results, r) }

	a := newAdapter(t, depsFor(f), opts)

	tree, err := a.OpenTree(context.Background(), fakeTransportSource())
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}

	defer tree.Close() //nolint:errcheck // the assertions below read Err() directly.

	// Report is documented as safe DURING the walk, which is what a
	// progress surface needs and what the race detector is here to
	// check: the producer is filling those counters while this reads
	// them.
	polling := make(chan struct{})
	polled := make(chan struct{})

	go func() {
		defer close(polled)

		for {
			select {
			case <-polling:
				return
			default:
				_ = tree.Report()
			}
		}
	}()

	root, err := walkDir(context.Background(), tree.Root())
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}

	close(polling)
	<-polled

	if err := tree.Err(); err != nil {
		t.Fatalf("Err() after a clean walk: %v", err)
	}

	if got, want := root.fileNames(), []string{"root1.txt", "root2.txt"}; !reflect.DeepEqual(got, want) {
		t.Errorf("root files = %v, want %v", got, want)
	}

	if got, want := root.dirNames(), []string{"sub"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("root directories = %v, want %v", got, want)
	}

	sub := root.dirs["sub"]
	if got, want := sub.fileNames(), []string{"a.txt", "b.txt"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sub files = %v, want %v", got, want)
	}

	if got, want := sub.dirNames(), []string{"deep"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sub directories = %v, want %v", got, want)
	}

	deep := sub.dirs["deep"]
	if got, want := deep.fileNames(), []string{"c.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("deep files = %v, want %v", got, want)
	}

	for path, want := range map[string][]byte{
		"root1.txt":      []byte("first at the root"),
		"root2.txt":      []byte("second at the root"),
		"sub/a.txt":      patternBytes(9000, 7),
		"sub/b.txt":      []byte("b in sub"),
		"sub/deep/c.txt": []byte("c in deep"),
	} {
		got := treeContent(t, root, path)
		if string(got) != string(want) {
			t.Errorf("%s: content is %d bytes, want %d", path, len(got), len(want))
		}
	}

	// A name carrying a separator would let the source's own listing
	// decide where in the snapshot its content lands, which is the
	// reason SourceEntry.Name is one element.
	for _, name := range append(root.order, append(sub.order, deep.order...)...) {
		if name == "" || name == "." || name == ".." {
			t.Errorf("entry name %q is not a path element", name)
		}

		for _, c := range name {
			if c == '/' {
				t.Errorf("entry name %q carries a path separator", name)
			}
		}
	}

	rep := tree.Report()

	var wantBytes int64 = int64(len("first at the root") + len("second at the root") + 9000 + len("b in sub") + len("c in deep"))

	if rep.Entries != 5 || rep.Stored != 5 || rep.Bytes != wantBytes {
		t.Errorf("report = Entries %d, Stored %d, Bytes %d; want 5, 5, %d", rep.Entries, rep.Stored, rep.Bytes, wantBytes)
	}

	if !rep.Complete() {
		t.Errorf("report says the run was incomplete: %+v", rep)
	}

	// Per-entry bytes are what the READ produced, which is the half of
	// the mutation check a listing's size cannot supply.
	byPath := map[string]int64{}
	for _, r := range results {
		byPath[r.Path] = r.Bytes

		if !r.Verified() {
			t.Errorf("%s: outcome %q, want a verified one", r.Path, r.Outcome)
		}
	}

	for path, want := range map[string]int64{
		"root1.txt":      int64(len("first at the root")),
		"root2.txt":      int64(len("second at the root")),
		"sub/a.txt":      9000,
		"sub/b.txt":      int64(len("b in sub")),
		"sub/deep/c.txt": int64(len("c in deep")),
	} {
		if byPath[path] != want {
			t.Errorf("%s: reported %d bytes read, want %d", path, byPath[path], want)
		}
	}

	// The size on the entry is the listing's number, and the source
	// declares a stable size, so it must agree with the read.
	if got := sub.files["a.txt"].size; got != 9000 {
		t.Errorf("sub/a.txt entry size = %d, want 9000", got)
	}

	if got := sub.files["a.txt"].modTime; got.Unix() != 1_700_000_003 {
		t.Errorf("sub/a.txt entry modtime = %v, want unix 1700000003", got)
	}
}

// treeContent walks a collected tree by slash path, so a table of
// expectations can be written the way the source was.
func treeContent(t *testing.T, root *walkedDir, p string) []byte {
	t.Helper()

	dir := root

	parts := splitSlash(p)
	for _, part := range parts[:len(parts)-1] {
		child, ok := dir.dirs[part]
		if !ok {
			t.Fatalf("%s: no directory %q in the tree", p, part)
		}

		dir = child
	}

	f, ok := dir.files[parts[len(parts)-1]]
	if !ok {
		t.Fatalf("%s: not in the tree", p)
	}

	return f.body
}

func splitSlash(p string) []string {
	var (
		out  []string
		cur  string
		emit = func() {
			if cur != "" {
				out = append(out, cur)
			}

			cur = ""
		}
	)

	for _, c := range p {
		if c == '/' {
			emit()

			continue
		}

		cur += string(c)
	}

	emit()

	return out
}

// --- policy, applied before anything is opened -------------------------

// A symbolic link under the ignore policy is counted and absent from the
// tree. It is never followed and never opened under either policy; the
// difference is only whether the link itself is stored.
func TestOpenTreeSkipsSymlinksUnderTheIgnorePolicy(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("keep.txt", []byte("kept"), 1_700_000_000)
	f.putKind("link", transport.EntryKindSymlink, "keep.txt")

	a := newAdapter(t, depsFor(f), liveOpts())

	tree, err := a.OpenTree(context.Background(), fakeTransportSource())
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}

	defer tree.Close() //nolint:errcheck // Err() is asserted directly.

	root, err := walkDir(context.Background(), tree.Root())
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}

	if got, want := root.fileNames(), []string{"keep.txt"}; !reflect.DeepEqual(got, want) {
		t.Errorf("root files = %v, want %v", got, want)
	}

	if rep := tree.Report(); rep.SkippedSymlink != 1 || rep.Stored != 1 {
		t.Errorf("report = SkippedSymlink %d, Stored %d; want 1, 1", rep.SkippedSymlink, rep.Stored)
	}
}

// Preserve stores the link itself, as a file whose bytes are the target
// string, on a backend that declares it stores links. A target that
// leaves the backup root is refused and counted, and nothing resolves
// either of them.
func TestOpenTreePreservesALinkAsItsTargetAndRefusesAnEscape(t *testing.T) {
	t.Parallel()

	caps := localLikeCapabilities()
	caps.SymlinkSemantics = backend.SymlinksStored

	f := newFakeSource()
	f.put("data/real.txt", []byte("content"), 1_700_000_000)
	f.putKind("links/inside", transport.EntryKindSymlink, "../data/real.txt")
	f.putKind("links/outside", transport.EntryKindSymlink, "../../etc/shadow")

	deps := depsFor(f)
	deps.Profiles = capabilityProfile("link_store", caps)

	a := newAdapter(t, deps, source.Options{
		Mode:     model.ModeLiveBestEffort,
		Preset:   model.PresetConservative,
		Symlinks: source.SymlinkPreserve,
	})

	tree, err := a.OpenTree(context.Background(), fakeTransportSource())
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}

	defer tree.Close() //nolint:errcheck // Err() is asserted directly.

	root, err := walkDir(context.Background(), tree.Root())
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}

	links, ok := root.dirs["links"]
	if !ok {
		t.Fatalf("no links directory in the tree: %v", root.dirNames())
	}

	if got, want := links.fileNames(), []string{"inside"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("links files = %v, want %v", got, want)
	}

	if got := string(links.files["inside"].body); got != "../data/real.txt" {
		t.Errorf("the preserved link holds %q, want its target", got)
	}

	rep := tree.Report()
	if rep.Refused != 1 {
		t.Errorf("Refused = %d, want 1 for the escaping link", rep.Refused)
	}

	if !reasonsContain(rep.Reasons, "outside the backup root") {
		t.Errorf("no reason mentions the escape: %v", rep.Reasons)
	}
}

// A socket, fifo or device node is skipped and counted, and is never
// opened: a fifo with no writer blocks in Read until something writes to
// it, which would hang a backup window on a file nobody meant to copy.
func TestOpenTreeSkipsASpecialFile(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("keep.txt", []byte("kept"), 1_700_000_000)
	f.putKind("queue.fifo", transport.EntryKindOther, "")

	rec := &recordingStreamer{fakeSource: f}
	deps := depsFor(f)
	deps.Streamer = rec

	a := newAdapter(t, deps, liveOpts())

	tree, err := a.OpenTree(context.Background(), fakeTransportSource())
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}

	defer tree.Close() //nolint:errcheck // Err() is asserted directly.

	root, err := walkDir(context.Background(), tree.Root())
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}

	if got, want := root.fileNames(), []string{"keep.txt"}; !reflect.DeepEqual(got, want) {
		t.Errorf("root files = %v, want %v", got, want)
	}

	if rep := tree.Report(); rep.SkippedSpecial != 1 {
		t.Errorf("SkippedSpecial = %d, want 1", rep.SkippedSpecial)
	}

	for _, p := range rec.openedPaths() {
		if p == "queue.fifo" {
			t.Fatal("the fifo was opened")
		}
	}
}

// A path this adapter will not use is refused, counted, and NEVER handed
// to the transport: a name designed to escape the root must not reach the
// layer that would resolve it.
func TestOpenTreeRefusesAnUnsafePathWithoutOpeningIt(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("../escape.txt", []byte("nope"), 1_700_000_000)
	f.put("fine.txt", []byte("fine"), 1_700_000_001)

	rec := &recordingStreamer{fakeSource: f}
	deps := depsFor(f)
	deps.Streamer = rec

	a := newAdapter(t, deps, liveOpts())

	tree, err := a.OpenTree(context.Background(), fakeTransportSource())
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}

	defer tree.Close() //nolint:errcheck // Err() is asserted directly.

	root, err := walkDir(context.Background(), tree.Root())
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}

	if got, want := root.fileNames(), []string{"fine.txt"}; !reflect.DeepEqual(got, want) {
		t.Errorf("root files = %v, want %v", got, want)
	}

	rep := tree.Report()
	if rep.Refused != 1 {
		t.Errorf("Refused = %d, want 1", rep.Refused)
	}

	if rep.Complete() {
		t.Error("a run that refused an entry the operator selected called itself complete")
	}

	for _, p := range rec.openedPaths() {
		if p == "../escape.txt" {
			t.Fatal("the unsafe path was opened")
		}
	}
}

// An excluded path is not in the tree, and is counted as configuration
// rather than as policy or failure.
func TestOpenTreeHonoursExclusions(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("keep/a.txt", []byte("keep"), 1_700_000_000)
	f.put("secret/key.pem", []byte("secret"), 1_700_000_001)

	a := newAdapter(t, depsFor(f), liveOpts())

	src := fakeTransportSource()
	src.ExcludePaths = []string{"secret"}

	tree, err := a.OpenTree(context.Background(), src)
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}

	defer tree.Close() //nolint:errcheck // Err() is asserted directly.

	root, err := walkDir(context.Background(), tree.Root())
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}

	if got, want := root.dirNames(), []string{"keep"}; !reflect.DeepEqual(got, want) {
		t.Errorf("root directories = %v, want %v", got, want)
	}

	if rep := tree.Report(); rep.SkippedExcluded != 1 {
		t.Errorf("SkippedExcluded = %d, want 1", rep.SkippedExcluded)
	}
}

// --- the mutation check, whose unit is now the RUN ---------------------

// A file that moves under its own read fails the RUN. There is nothing
// per-object to discard out of a set-wide snapshot, so the only honest
// answer is that the whole snapshot does not happen: the consumer sees
// the error while it is still pulling, and no manifest is written.
func TestOpenTreeFailsTheRunWhenAFileMovesUnderTheRead(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("a/moving.bin", patternBytes(8000, 3), 1_700_000_000)
	f.put("z/after.txt", []byte("never reached"), 1_700_000_001)
	f.duringReadAfter = 1
	f.duringRead = func(s *fakeSource) {
		s.put("a/moving.bin", patternBytes(8000, 9), 1_700_000_500)
	}

	a := newAdapter(t, depsFor(f), liveOpts())

	tree, err := a.OpenTree(context.Background(), fakeTransportSource())
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}

	defer tree.Close() //nolint:errcheck // Err() is asserted directly.

	if _, err := walkDir(context.Background(), tree.Root()); err == nil {
		t.Fatal("the walk succeeded over a source that moved under the read")
	}

	if err := tree.Err(); err == nil {
		t.Fatal("Err() is nil after a torn read")
	} else if !errors.Is(err, source.ErrSourceMutated) {
		t.Fatalf("Err() = %v, want a source.ErrSourceMutated", err)
	}

	rep := tree.Report()
	if rep.Incomplete != 1 {
		t.Errorf("Incomplete = %d, want 1", rep.Incomplete)
	}

	if rep.Complete() {
		t.Error("a run with a torn read called itself complete")
	}

	if !reasonsContain(rep.Reasons, "a/moving.bin") {
		t.Errorf("no reason names the torn object: %v", rep.Reasons)
	}
}

// Under a consistency mode that guarantees a point in time the same
// source does not fail, because no post-read stat is performed at all: a
// frozen image cannot move while it is read, and asking anyway would be a
// round trip per object to confirm a promise the mode already made.
func TestOpenTreeDoesNotCheckTheReadWindowUnderAPointInTimeMode(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("a/moving.bin", patternBytes(8000, 3), 1_700_000_000)
	f.duringReadAfter = 1
	f.duringRead = func(s *fakeSource) {
		s.put("a/moving.bin", patternBytes(8000, 9), 1_700_000_500)
	}

	opts := liveOpts()
	opts.Mode = model.ModeExternalSnapshot

	a := newAdapter(t, depsFor(f), opts)

	tree, err := a.OpenTree(context.Background(), fakeTransportSource())
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}

	defer tree.Close() //nolint:errcheck // Err() is asserted directly.

	if _, err := walkDir(context.Background(), tree.Root()); err != nil {
		t.Fatalf("walking a frozen source: %v", err)
	}

	if err := tree.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil under a point-in-time mode", err)
	}

	if got := f.stats.Load(); got != 0 {
		t.Errorf("the run performed %d stats under a point-in-time mode, want none", got)
	}

	if rep := tree.Report(); rep.Stored != 1 || !rep.Complete() {
		t.Errorf("report = Stored %d, Complete %v; want 1, true", rep.Stored, rep.Complete())
	}
}

// --- the ordering assumption, enforced rather than trusted -------------

// An enumeration that is not depth-first grouped is REFUSED. The bridge
// reconstructs the tree from a single forward cursor, so a path that
// would require re-entering a directory the walk has already left cannot
// be placed; producing a tree with two "a" directories in it, or
// silently dropping the late entry, are both worse than a loud refusal.
func TestOpenTreeRefusesEnumerationThatIsNotDepthFirstGrouped(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("a/1", []byte("a one"), 1_700_000_000)
	f.put("b/1", []byte("b one"), 1_700_000_001)
	f.put("a/2", []byte("a two"), 1_700_000_002)

	deps := depsFor(f)
	deps.Enumerator = listEnumerator{src: f, order: []string{"a/1", "b/1", "a/2"}}

	a := newAdapter(t, deps, liveOpts())

	tree, err := a.OpenTree(context.Background(), fakeTransportSource())
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}

	defer tree.Close() //nolint:errcheck // Err() is asserted directly.

	root, walkErr := walkDir(context.Background(), tree.Root())
	if walkErr == nil {
		t.Fatal("the walk succeeded over an enumeration that was not grouped")
	}

	if !errors.Is(walkErr, source.ErrEnumerationNotGrouped) {
		t.Fatalf("walk error = %v, want source.ErrEnumerationNotGrouped", walkErr)
	}

	if !errors.Is(tree.Err(), source.ErrEnumerationNotGrouped) {
		t.Fatalf("Err() = %v, want source.ErrEnumerationNotGrouped", tree.Err())
	}

	if got, want := root.order, []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("root saw %v, want %v: a second \"a\" directory was materialised", got, want)
	}
}

// --- lifecycle: one session, no goroutine, no open reader --------------

// A consumer that stops pulling and closes the tree is not a failure of
// the source, and it is not allowed to leave anything behind: the
// producer returns, the one transport session closes exactly once, and
// nothing is used through it afterwards.
func TestOpenTreeCloseStopsTheProducerAndClosesTheSessionOnce(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	for _, p := range []string{"a/1", "a/2", "b/1", "b/2", "c/1"} {
		f.put(p, patternBytes(4096, 1), 1_700_000_000)
	}

	s := &sessionSource{fakeSource: f}
	enum := newSignallingEnumerator(f)

	deps := depsFor(f)
	deps.Streamer = s
	deps.Stater = s
	deps.Enumerator = enum

	a := newAdapter(t, deps, liveOpts())

	tree, err := a.OpenTree(context.Background(), fakeTransportSource())
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}

	if got := s.opened.Load(); got != 1 {
		t.Fatalf("OpenTree opened %d sessions, want exactly 1", got)
	}

	// Descend one level, take one file, read PART of it, and then walk
	// away without closing the reader. This is the shape a leak hides
	// in: the producer is parked beside a read the engine has abandoned,
	// there is an open reader nobody owns, and the one connection the
	// whole run shares is the thing nobody is looking at.
	it, err := tree.Root().Open(context.Background())
	if err != nil {
		t.Fatalf("opening the root: %v", err)
	}

	entry, ok, err := it.Next(context.Background())
	if err != nil || !ok || !entry.IsDir() {
		t.Fatalf("first entry = %+v, ok %v, err %v; want a directory", entry, ok, err)
	}

	inner, err := entry.Dir.Open(context.Background())
	if err != nil {
		t.Fatalf("opening %q: %v", entry.Name, err)
	}

	file, ok, err := inner.Next(context.Background())
	if err != nil || !ok || file.IsDir() {
		t.Fatalf("first child = %+v, ok %v, err %v; want a file", file, ok, err)
	}

	rc, err := file.Stream.Open(context.Background())
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}

	if _, err := rc.Read(make([]byte, 16)); err != nil {
		t.Fatalf("reading the first bytes: %v", err)
	}

	if cerr := inner.Close(); cerr != nil {
		t.Fatalf("closing the inner iterator: %v", cerr)
	}

	if cerr := it.Close(); cerr != nil {
		t.Fatalf("closing the root iterator: %v", cerr)
	}

	closeErr := tree.Close()
	if !errors.Is(closeErr, source.ErrTreeAbandoned) {
		t.Errorf("Close() = %v, want source.ErrTreeAbandoned for a walk that stopped early", closeErr)
	}

	if !errors.Is(tree.Err(), source.ErrTreeAbandoned) {
		t.Errorf("Err() = %v, want source.ErrTreeAbandoned", tree.Err())
	}

	enum.waitReturned(t)

	if got := s.closed.Load(); got != 1 {
		t.Errorf("the session was closed %d times, want exactly 1", got)
	}

	if got := s.usesAfterClose(); got != 0 {
		t.Errorf("%d operations were attempted through a closed session", got)
	}

	// The reader the engine abandoned mid-read is closed by the run, not
	// left for the garbage collector: on a real transport that is a
	// connection, and a daemon that had run a few thousand backups would
	// stop being able to open files.
	if got := f.inturn.Load(); got != 0 {
		t.Errorf("%d readers were left open after Close", got)
	}

	// Idempotent: a second Close does not close the session again and
	// does not change the answer.
	if err := tree.Close(); !errors.Is(err, source.ErrTreeAbandoned) {
		t.Errorf("second Close() = %v, want the same answer", err)
	}

	if got := s.closed.Load(); got != 1 {
		t.Errorf("a second Close closed the session again: %d", got)
	}
}

// A clean walk closes its session once as well, and Close reports
// nothing: a completed tree is the one case where the consumer owes the
// run no explanation.
func TestOpenTreeCloseAfterACleanWalkReportsNothing(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("a/1", []byte("one"), 1_700_000_000)

	s := &sessionSource{fakeSource: f}
	enum := newSignallingEnumerator(f)

	deps := depsFor(f)
	deps.Streamer = s
	deps.Stater = s
	deps.Enumerator = enum

	a := newAdapter(t, deps, liveOpts())

	tree, err := a.OpenTree(context.Background(), fakeTransportSource())
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}

	if _, err := walkDir(context.Background(), tree.Root()); err != nil {
		t.Fatalf("walking the tree: %v", err)
	}

	if err := tree.Close(); err != nil {
		t.Fatalf("Close() after a clean walk: %v", err)
	}

	enum.waitReturned(t)

	if got := s.closed.Load(); got != 1 {
		t.Errorf("the session was closed %d times, want exactly 1", got)
	}

	if got := f.inturn.Load(); got != 0 {
		t.Errorf("%d readers were left open after a clean walk", got)
	}
}

// A cancelled context releases a read that is blocked on a source that
// will never answer, which is the only thing that unblocks one: a Read
// waiting on a socket does not consult a context, so the run closes the
// reader from the outside.
func TestOpenTreeCancellationReleasesABlockedRead(t *testing.T) {
	t.Parallel()

	inner := newFakeSource()
	inner.put("slow.bin", patternBytes(1024, 5), 1_700_000_000)

	blocked := newBlockingSource(inner)
	enum := newSignallingEnumerator(inner)

	deps := depsFor(inner)
	deps.Streamer = blocked
	deps.Enumerator = enum

	a := newAdapter(t, deps, liveOpts())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tree, err := a.OpenTree(ctx, fakeTransportSource())
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}

	walked := make(chan error, 1)

	go func() {
		_, werr := walkDir(ctx, tree.Root())
		walked <- werr
	}()

	blocked.waitReading(t)
	cancel()

	select {
	case werr := <-walked:
		if werr == nil {
			t.Fatal("the walk succeeded while its read was blocked and cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled walk did not return")
	}

	if err := tree.Close(); err == nil {
		t.Error("Close() reported nothing for a cancelled run")
	}

	enum.waitReturned(t)

	if err := tree.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("Err() = %v, want a cancellation", err)
	}
}

// --- refusals, before anything is dialed -------------------------------

// Every refusal a per-object run makes before it dials, a tree run makes
// too, and it makes them BEFORE the session is opened: a capability a
// backend does not have is not a fact that improves after a connection.
func TestOpenTreeRefusesBeforeDialing(t *testing.T) {
	t.Parallel()

	noStream := localLikeCapabilities()
	noStream.StreamingOpen = false

	noListing := localLikeCapabilities()
	noListing.BoundedListing = false

	noEvidence := localLikeCapabilities()
	noEvidence.StableSize = false
	noEvidence.MTimePrecision = backend.MTimeUnknown
	noEvidence.GenerationIdentity = backend.GenerationNone

	cases := []struct {
		name string
		deps func(*sessionSource) source.Deps
		opts source.Options
		want error
	}{
		{
			name: "a backend that cannot stream",
			deps: func(s *sessionSource) source.Deps {
				d := treeDeps(s)
				d.Profiles = capabilityProfile("no_stream", noStream)

				return d
			},
			opts: liveOpts(),
			want: source.ErrUnstreamable,
		},
		{
			name: "a backend whose directories cannot be walked in bounded memory",
			deps: func(s *sessionSource) source.Deps {
				d := treeDeps(s)
				d.Profiles = capabilityProfile("no_listing", noListing)

				return d
			},
			opts: liveOpts(),
			want: backend.ErrUnboundedListing,
		},
		{
			name: "a backend that reports nothing a read window could be checked against",
			deps: func(s *sessionSource) source.Deps {
				d := treeDeps(s)
				d.Profiles = capabilityProfile("no_evidence", noEvidence)

				return d
			},
			opts: liveOpts(),
			want: source.ErrUndetectableMutation,
		},
		{
			name: "no way to re-stat an object after reading it",
			deps: func(s *sessionSource) source.Deps {
				d := treeDeps(s)
				d.Stater = nil

				return d
			},
			opts: liveOpts(),
			want: source.ErrNoStater,
		},
		{
			name: "no enumerator at all",
			deps: func(s *sessionSource) source.Deps {
				d := treeDeps(s)
				d.Enumerator = nil

				return d
			},
			opts: liveOpts(),
			want: source.ErrNoEnumerator,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeSource()
			f.put("a/1", []byte("one"), 1_700_000_000)

			s := &sessionSource{fakeSource: f}

			a := newAdapter(t, tc.deps(s), tc.opts)

			tree, err := a.OpenTree(context.Background(), fakeTransportSource())
			if err == nil {
				_ = tree.Close()

				t.Fatal("OpenTree accepted a source it cannot honour")
			}

			if !errors.Is(err, tc.want) {
				t.Fatalf("OpenTree = %v, want %v", err, tc.want)
			}

			if got := s.opened.Load(); got != 0 {
				t.Errorf("%d sessions were opened before the refusal", got)
			}
		})
	}
}

// The symlink policy against the backend's own declaration is the one
// refusal that needs both halves configured, and it is refused at
// OpenTree rather than degraded to "ignore" at run time: a policy that
// silently degrades is a policy that lies to whoever set it.
func TestOpenTreeRefusesPreserveOnABackendThatSkipsLinks(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("a/1", []byte("one"), 1_700_000_000)

	s := &sessionSource{fakeSource: f}

	deps := treeDeps(s)
	deps.Links = f

	a := newAdapter(t, deps, source.Options{
		Mode:     model.ModeLiveBestEffort,
		Preset:   model.PresetConservative,
		Symlinks: source.SymlinkPreserve,
	})

	_, err := a.OpenTree(context.Background(), fakeTransportSource())
	if err == nil {
		t.Fatal("OpenTree accepted preserve on a backend that skips links")
	}

	if got := s.opened.Load(); got != 0 {
		t.Errorf("%d sessions were opened before the refusal", got)
	}
}

// treeDeps wires a session-capable fake as every capability a tree run
// needs, so a case can knock exactly one of them out.
func treeDeps(s *sessionSource) source.Deps {
	return source.Deps{Streamer: s, Stater: s, Enumerator: s.fakeSource}
}

// A directory's iteration is one-shot and says so: a second pass over the
// same directory cannot be served from a forward cursor over a remote
// listing, and replaying one would mean holding a buffer this package
// refuses to hold.
func TestOpenTreeRefusesASecondPassOverADirectory(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("a/1", []byte("one"), 1_700_000_000)

	a := newAdapter(t, depsFor(f), liveOpts())

	tree, err := a.OpenTree(context.Background(), fakeTransportSource())
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}

	defer tree.Close() //nolint:errcheck // the assertion is on Open.

	root := tree.Root()

	first, err := root.Open(context.Background())
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}

	defer first.Close() //nolint:errcheck // nothing to report from an abandoned pass.

	if _, err := root.Open(context.Background()); err == nil {
		t.Fatal("a second pass over the root was accepted")
	}
}

// A consumer that pulls from two frames at once is refused BY NAME.
//
// The alternative is the reason this test exists: the entry still being
// read would be declared finished by the other frame's Next, its
// post-read check would compare a partial byte count against the
// source's full size, and the run would fail with ErrSourceMutated
// naming a file nothing had touched - sending an operator to look for a
// mutation on their source that never happened.
func TestOpenTreeRefusesAConcurrentPull(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("a/1", patternBytes(4096, 2), 1_700_000_000)
	f.put("a/2", patternBytes(4096, 3), 1_700_000_001)

	a := newAdapter(t, depsFor(f), liveOpts())

	tree, err := a.OpenTree(context.Background(), fakeTransportSource())
	if err != nil {
		t.Fatalf("OpenTree: %v", err)
	}

	defer tree.Close() //nolint:errcheck // Err() is asserted directly.

	rootIt, err := tree.Root().Open(context.Background())
	if err != nil {
		t.Fatalf("opening the root: %v", err)
	}

	defer rootIt.Close() //nolint:errcheck // nothing to report from an abandoned pass.

	dir, ok, err := rootIt.Next(context.Background())
	if err != nil || !ok || !dir.IsDir() {
		t.Fatalf("first entry = %+v, ok %v, err %v; want a directory", dir, ok, err)
	}

	inner, err := dir.Dir.Open(context.Background())
	if err != nil {
		t.Fatalf("opening %q: %v", dir.Name, err)
	}

	defer inner.Close() //nolint:errcheck // as above.

	if _, ok, err := inner.Next(context.Background()); err != nil || !ok {
		t.Fatalf("first child: ok %v, err %v", ok, err)
	}

	// The file handed out by the inner frame has not been read yet, and
	// the root is asked for its next child anyway.
	_, _, err = rootIt.Next(context.Background())
	if !errors.Is(err, source.ErrConcurrentWalk) {
		t.Fatalf("Next from a second frame = %v, want source.ErrConcurrentWalk", err)
	}

	if !errors.Is(tree.Err(), source.ErrConcurrentWalk) {
		t.Errorf("Err() = %v, want source.ErrConcurrentWalk", tree.Err())
	}

	if errors.Is(tree.Err(), source.ErrSourceMutated) {
		t.Error("a concurrent pull was reported as a mutation of the operator's source")
	}
}
