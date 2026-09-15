package source_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/backupengine/kopia"
	"github.com/retnd/retnd/core/internal/backupengine/source"
	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/secretref"
	"github.com/retnd/retnd/core/internal/transport"
	"github.com/retnd/retnd/core/internal/transport/rclone"
)

const integrationPassphrase = "a-passphrase-long-enough-to-be-a-passphrase"

// realRepository opens a filesystem repository under a fresh temp root and
// returns it with that root, so a test can assert that nothing staged the
// source anywhere near it.
func realRepository(t *testing.T) (backupengine.StreamingRepository, string, string) {
	t.Helper()

	root := t.TempDir()
	domain, err := model.NewRepositoryDomainID("production")
	if err != nil {
		t.Fatalf("NewRepositoryDomainID: %v", err)
	}
	passFile := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(passFile, []byte(integrationPassphrase), 0o600); err != nil {
		t.Fatalf("writing the passphrase file: %v", err)
	}
	loc := backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     domain,
		Root:       root,
		StateDir:   filepath.Join(t.TempDir(), "state"),
		Passphrase: secretref.Ref{File: passFile},
	}

	eng := kopia.New()

	ctx := context.Background()
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

	streaming, ok := rep.(backupengine.StreamingRepository)
	if !ok {
		t.Fatalf("%T is not a streaming repository", rep)
	}

	repoDir, err := backupengine.ReservedLocalDir(root, domain)
	if err != nil {
		t.Fatalf("ReservedLocalDir: %v", err)
	}

	return streaming, root, repoDir
}

// testRef is a valid repository reference, which the production sink
// requires: it refuses to write a snapshot it cannot attribute to a
// backup set and a domain.
func testRef(t *testing.T, set string) model.RepositoryRef {
	t.Helper()

	domain, err := model.NewRepositoryDomainID("production")
	if err != nil {
		t.Fatalf("NewRepositoryDomainID: %v", err)
	}

	id, err := model.NewBackupSetID("nas", set)
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	return model.RepositoryRef{Domain: domain, Set: id}
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// The whole wire with no stand-in on it: a real local tree, a real rclone
// Fs and Object, the reader its Open returns, this adapter, and a real
// Kopia repository. The bytes exist twice at the end - on the source and
// in the repository - and nowhere in between.
func TestALocalTreeStreamsThroughTheAdapterIntoARealRepository(t *testing.T) {
	t.Parallel()

	sourceDir := t.TempDir()

	files := map[string][]byte{
		"runs/2026/db.dump":       patternBytes(6<<20, 0xC0FFEE),
		"runs/2026/manifest.json": []byte(`{"run":"2026-01-01"}`),
		"index.txt":               []byte("one\ntwo\nthree\n"),
	}

	for name, body := range files {
		full := filepath.Join(sourceDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
		if err := os.WriteFile(full, body, 0o600); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	adapter := rclone.New()
	src := transport.Source{ID: "local-source", Type: "local", Root: sourceDir}

	repo, repoRoot, repoDir := realRepository(t)

	sink := source.RepositorySink{
		Repo:        repo,
		Source:      backupengine.Source{Host: "nas", User: "backupd", Path: "/sets/local-source"},
		Ref:         testRef(t, "local-source"),
		Description: "integration",
	}

	// OnResult is called from every worker at once, which the option
	// documents; a test that forgot that would be racing its own map.
	var storedMu sync.Mutex

	stored := map[string]backupengine.SnapshotID{}

	a, err := source.New(source.Deps{
		Streamer:   adapter,
		Stater:     adapter,
		Enumerator: adapter,
	}, source.Options{
		Mode:        model.ModeLiveBestEffort,
		Preset:      model.PresetConservative,
		Concurrency: 4,
		OnResult: func(r source.Result) {
			if !r.Verified() {
				return
			}
			storedMu.Lock()
			defer storedMu.Unlock()
			stored[r.Path] = backupengine.SnapshotID(r.StoredID)
		},
	})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	rep, err := a.Backup(context.Background(), source.Request{Source: src, Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if !rep.Complete() {
		t.Fatalf("the run is incomplete: %+v", rep)
	}

	if rep.Stored != int64(len(files)) {
		t.Fatalf("stored %d objects, want %d: %+v", rep.Stored, len(files), rep)
	}

	if rep.Backend != "local_volume" {
		t.Errorf("the report was judged against %q, want local_volume", rep.Backend)
	}

	// Read every object back out of the repository and compare it with
	// what is on the source. This is the only assertion that proves the
	// bytes survived the trip rather than that a call returned nil.
	for name, body := range files {
		id, ok := stored[name]
		if !ok {
			t.Errorf("%s was not stored", name)

			continue
		}

		rc, err := repo.OpenSnapshotStream(context.Background(), id)
		if err != nil {
			t.Errorf("OpenSnapshotStream(%s): %v", name, err)

			continue
		}

		got, err := io.ReadAll(rc)
		_ = rc.Close()

		if err != nil {
			t.Errorf("reading %s back: %v", name, err)

			continue
		}

		if sha256Of(got) != sha256Of(body) {
			t.Errorf("%s came back as %d bytes (%s); the source holds %d (%s)",
				name, len(got), sha256Of(got), len(body), sha256Of(body))
		}
	}

	// Nothing staged the source. Everything under the repository
	// directory is content the repository wrote and is supposed to be
	// there; everywhere else under the run's root - a cache, a temp
	// directory, a partial file - must hold nothing the size of an
	// object, because a staging copy is exactly what that would be.
	assertNothingStaged(t, repoRoot, repoDir)

	// And the source is untouched.
	for name, body := range files {
		got, err := os.ReadFile(filepath.Join(sourceDir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("the backup removed %s from the source: %v", name, err)
		}
		if sha256Of(got) != sha256Of(body) {
			t.Fatalf("the backup modified %s on the source", name)
		}
	}
}

// assertNothingStaged fails if any file outside the repository directory
// is large enough to be a copy of a source object.
//
// The repository itself is excluded and that exclusion is the honest
// part: its pack blobs ARE the bytes, chunked and compressed, and one of
// them can legitimately be the size of the object that filled it. What is
// being tested is that no OTHER file exists - no cache of the whole
// object, no ".partial", no staging directory - which is the anti-pattern
// this adapter was built to make unavailable.
func assertNothingStaged(t *testing.T, root, repoDir string) {
	t.Helper()

	const suspicious = 1 << 20

	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == repoDir {
				return filepath.SkipDir
			}

			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if info.Size() >= suspicious {
			t.Errorf("%s is %d bytes and is not inside the repository: something staged a copy of an object", p, info.Size())
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking the run root: %v", err)
	}
}

// A fifo on the source is described as what it is and never opened. It is
// worth a real fifo rather than a fake: a read of one with no writer does
// not return, so an adapter that opened it would hang this test instead of
// failing it, which is the failure mode the special-file policy exists to
// prevent.
func TestAFifoOnARealSourceIsNeverOpened(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("no fifos here")
	}

	sourceDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(sourceDir, "real.txt"), []byte("content"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	fifo := filepath.Join(sourceDir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo is unavailable here: %v", err)
	}

	if err := os.Symlink("real.txt", filepath.Join(sourceDir, "link")); err != nil {
		t.Fatalf("seeding a symlink: %v", err)
	}

	adapter := rclone.New()
	src := transport.Source{ID: "special", Type: "local", Root: sourceDir}
	sink := newRecordingSink()

	a, err := source.New(source.Deps{Streamer: adapter, Stater: adapter, Enumerator: adapter}, source.Options{
		Mode:   model.ModeLiveBestEffort,
		Preset: model.PresetConservative,
	})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	done := make(chan source.Report, 1)

	go func() {
		rep, err := a.Backup(context.Background(), source.Request{Source: src, Sink: sink})
		if err != nil {
			t.Errorf("Backup: %v", err)
		}
		done <- rep
	}()

	var rep source.Report

	select {
	case rep = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the backup did not finish: something opened the fifo and is waiting for a writer that will never come")
	}

	if rep.SkippedSpecial != 1 {
		t.Errorf("the fifo was accounted for as %d special files, want 1: %+v", rep.SkippedSpecial, rep)
	}

	if rep.SkippedSymlink != 1 {
		t.Errorf("the symlink was accounted for as %d links, want 1: %+v", rep.SkippedSymlink, rep)
	}

	if got := sink.paths(); !equalStrings(got, []string{"real.txt"}) {
		t.Errorf("the sink holds %v, want only the regular file", got)
	}

	// The fifo and the link are still on the source afterwards.
	for _, name := range []string{"pipe", "link", "real.txt"} {
		if _, err := os.Lstat(filepath.Join(sourceDir, name)); err != nil {
			t.Errorf("the backup removed %s from the source: %v", name, err)
		}
	}
}

// The enumeration-scale gate from the bounded-listing work, driven
// through the adapter rather than through the enumerator alone: a
// directory of many small files is walked and stored with a peak that is
// the chunk and the worker count, not the directory.
func TestADirectoryAtScaleIsWalkedWithinABound(t *testing.T) {
	// Not parallel, for the reason TestALargeObjectStreamsWithoutBeingHeld
	// gives: a heap measurement taken while a sibling test is allocating
	// measures the sibling.
	if testing.Short() {
		t.Skip("scale test; -short skips it")
	}

	const files = 20000

	sourceDir := t.TempDir()
	body := []byte("x")

	for i := range files {
		name := filepath.Join(sourceDir, fmt.Sprintf("f%05d.bin", i))
		if err := os.WriteFile(name, body, 0o600); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	adapter := rclone.New()
	src := transport.Source{ID: "scale", Type: "local", Root: sourceDir}
	sink := &countingSink{}

	a, err := source.New(source.Deps{Streamer: adapter, Stater: adapter, Enumerator: adapter}, source.Options{
		Mode:         model.ModeLiveBestEffort,
		Preset:       model.PresetConservative,
		Concurrency:  4,
		ChunkEntries: 512,
	})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	runtime.GC()

	var start runtime.MemStats

	runtime.ReadMemStats(&start)

	rep, err := a.Backup(context.Background(), source.Request{Source: src, Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	runtime.GC()

	var end runtime.MemStats

	runtime.ReadMemStats(&end)

	if rep.Stored != files {
		t.Fatalf("stored %d of %d: %+v", rep.Stored, files, rep)
	}

	// The report itself is the thing that must not scale: counters and a
	// capped sample, never a record per file.
	if len(rep.Reasons) > 64 {
		t.Fatalf("the report grew to %d sentences over %d files", len(rep.Reasons), files)
	}

	// 20,000 entries at ~200 bytes each would be 4 MB if the walk held
	// them; the chunk is 512, so the walk's own share is under 100 KB.
	// The budget is generous because a rclone Fs and a transport
	// connection live inside it too, and it is still far below "the
	// directory is in memory".
	const heldBudget = 32 << 20

	if live := int64(end.HeapAlloc); live > heldBudget {
		t.Fatalf("the heap holds %d bytes after walking %d entries", live, files)
	}
}

// Cancellation against a real source and a real repository, under -race:
// the run stops, the call returns, and nothing is still running.
func TestCancellationAgainstARealSourceLeavesNothingRunning(t *testing.T) {
	t.Parallel()

	sourceDir := t.TempDir()
	for i := range 300 {
		name := filepath.Join(sourceDir, fmt.Sprintf("f%03d.bin", i))
		if err := os.WriteFile(name, patternBytes(64<<10, uint32(i+1)), 0o600); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	adapter := rclone.New()
	src := transport.Source{ID: "cancel", Type: "local", Root: sourceDir}
	repo, _, _ := realRepository(t)

	ctx, cancel := context.WithCancel(context.Background())

	var seen atomic.Int64

	a, err := source.New(source.Deps{Streamer: adapter, Stater: adapter, Enumerator: adapter}, source.Options{
		Mode:        model.ModeLiveBestEffort,
		Preset:      model.PresetConservative,
		Concurrency: 4,
		OnResult: func(source.Result) {
			if seen.Add(1) == 10 {
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	before := runtime.NumGoroutine()

	sink := source.RepositorySink{
		Repo:   repo,
		Source: backupengine.Source{Host: "nas", User: "backupd", Path: "/sets/cancel"},
		Ref:    testRef(t, "cancel"),
	}

	if _, err := a.Backup(ctx, source.Request{Source: src, Sink: sink}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled backup returned %v, want context.Canceled", err)
	}

	if n := seen.Load(); n >= 300 {
		t.Fatal("the run read the whole source after being cancelled")
	}

	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("%d goroutines before the cancelled run and %d after it", before, after)
	}
}

// A file rewritten in place while a real rclone read is in flight, into a
// real repository: the torn snapshot is removed and the settled content
// is what remains.
func TestARealFileRewrittenMidReadIsNotLeftInTheRepository(t *testing.T) {
	t.Parallel()

	sourceDir := t.TempDir()
	target := filepath.Join(sourceDir, "busy.bin")

	first := patternBytes(8<<20, 0xAAAA)
	second := patternBytes(8<<20, 0xBBBB)

	if err := os.WriteFile(target, first, 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	adapter := rclone.New()
	src := transport.Source{ID: "busy", Type: "local", Root: sourceDir}
	repo, _, _ := realRepository(t)

	var (
		rewritten atomic.Bool
		lastID    atomic.Value
	)

	sink := &rewriteDuringStore{
		inner: source.RepositorySink{
			Repo:   repo,
			Source: backupengine.Source{Host: "nas", User: "backupd", Path: "/sets/busy"},
			Ref:    testRef(t, "busy"),
		},
		duringRead: func(attempt int) {
			if attempt != 1 || !rewritten.CompareAndSwap(false, true) {
				return
			}
			// Same length, different bytes: the rewrite a size
			// comparison alone cannot see.
			if err := os.WriteFile(target, second, 0o600); err != nil {
				t.Errorf("rewriting the source: %v", err)
				return
			}
			// And the timestamp is moved a minute forward, explicitly.
			//
			// Not for convenience: a rewrite of the same length inside
			// the SAME SECOND is invisible to this check, because
			// transport.RemoteArtifact.ModTime is unix seconds and a
			// local filesystem's sizes match. That blind window is a
			// real property of a streaming capture - the second read
			// that would close it is the one thing a single-pass stream
			// cannot do - and it is documented rather than papered
			// over. This test proves the detection that DOES exist, so
			// it puts the mutation outside the window on purpose.
			when := time.Now().Add(time.Minute)
			if err := os.Chtimes(target, when, when); err != nil {
				t.Errorf("moving the timestamp: %v", err)
			}
		},
		after: func(id string) { lastID.Store(id) },
	}

	a, err := source.New(source.Deps{Streamer: adapter, Stater: adapter, Enumerator: adapter}, source.Options{
		Mode:   model.ModeLiveBestEffort,
		Preset: model.PresetConservative,
	})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	rep, err := a.Backup(context.Background(), source.Request{Source: src, Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if rep.Retried != 1 || !rep.Complete() {
		t.Fatalf("want one retried object in a complete run, got %+v", rep)
	}

	id, _ := lastID.Load().(string)
	if id == "" {
		t.Fatal("nothing was stored")
	}

	rc, err := repo.OpenSnapshotStream(context.Background(), backupengine.SnapshotID(id))
	if err != nil {
		t.Fatalf("OpenSnapshotStream: %v", err)
	}

	got, err := io.ReadAll(rc)
	_ = rc.Close()

	if err != nil {
		t.Fatalf("reading the settled snapshot: %v", err)
	}

	if sha256Of(got) != sha256Of(second) {
		t.Fatalf("the repository holds the torn read, not the settled content")
	}

	// The torn snapshot is gone from the repository, not merely unused.
	snaps, err := repo.ListSnapshots(context.Background(), backupengine.Source{
		Host: "nas", User: "backupd", Path: "/sets/busy/busy.bin",
	})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}

	if len(snaps) != 1 {
		t.Fatalf("the repository holds %d snapshots of this object; the torn one was supposed to be discarded", len(snaps))
	}
}

// rewriteDuringStore rewrites the source while the adapter's read of it
// is in flight, which is the only way to make a torn read happen on
// purpose rather than by luck.
type rewriteDuringStore struct {
	inner      source.RepositorySink
	duringRead func(attempt int)
	after      func(id string)
}

func (r *rewriteDuringStore) Store(ctx context.Context, obj source.Object) (source.Stored, error) {
	attempt := obj.Attempt
	obj.Stream = &hookedStream{
		inner: obj.Stream,
		hook:  func() { r.duringRead(attempt) },
	}

	out, err := r.inner.Store(ctx, obj)
	if err == nil {
		r.after(out.ID)
	}

	return out, err
}

func (r *rewriteDuringStore) Discard(ctx context.Context, id string) error {
	return r.inner.Discard(ctx, id)
}

// hookedStream fires once, after the first bytes of a read have been
// delivered, so a mutation is ordered against the read rather than raced
// with it.
type hookedStream struct {
	inner backupengine.StreamSource
	hook  func()
}

func (h *hookedStream) ModTime() time.Time { return h.inner.ModTime() }

func (h *hookedStream) Open(ctx context.Context) (io.ReadCloser, error) {
	rc, err := h.inner.Open(ctx)
	if err != nil {
		return nil, err
	}

	return &hookedReader{inner: rc, hook: h.hook}, nil
}

type hookedReader struct {
	inner io.ReadCloser
	hook  func()
	once  sync.Once
}

func (h *hookedReader) Read(p []byte) (int, error) {
	n, err := h.inner.Read(p)
	if n > 0 {
		h.once.Do(h.hook)
	}

	return n, err
}

func (h *hookedReader) Close() error { return h.inner.Close() }

// A cancel that lands AFTER a Store succeeded must not leave the
// snapshot it produced behind.
//
// This is the narrow window the cancellation contract missed. The bytes
// are in the repository before anything can be compared - that is the
// premise of a single-pass stream - so the run checks the read window
// after the store returns. A cancellation that arrives in between ends
// the run right there, and what it used to leave behind was a snapshot
// nobody ever checked, sitting in the repository as a restore point
// indistinguishable from a verified one. An operator restoring it would
// get bytes this product never proved were the file's.
//
// It is run against a real repository rather than a fake sink because
// the removal has to survive the thing that makes it hard: the run's
// context is already cancelled, so a Discard performed on that context
// would be refused by the repository and the snapshot would stay. Only
// a real DeleteSnapshot can fail that way.
func TestACancelAfterAStoreLeavesNoUnverifiedSnapshotBehind(t *testing.T) {
	t.Parallel()

	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "late.bin"), patternBytes(1<<20, 0xC0DE), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	adapter := rclone.New()
	src := transport.Source{ID: "late", Type: "local", Root: sourceDir}
	repo, _, _ := realRepository(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &cancelAfterStore{
		inner: source.RepositorySink{
			Repo:   repo,
			Source: backupengine.Source{Host: "nas", User: "backupd", Path: "/sets/late"},
			Ref:    testRef(t, "late"),
		},
		cancel: cancel,
	}

	a, err := source.New(source.Deps{Streamer: adapter, Stater: adapter, Enumerator: adapter}, source.Options{
		Mode:   model.ModeLiveBestEffort,
		Preset: model.PresetConservative,
	})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	rep, _ := a.Backup(ctx, source.Request{Source: src, Sink: sink})

	if rep.Stored != 0 {
		t.Fatalf("the cancelled run reported %d objects stored and proven; nothing was ever checked: %+v", rep.Stored, rep)
	}

	if rep.Complete() {
		t.Fatalf("a run cancelled after a store called itself complete: %+v", rep)
	}

	if id := sink.storedID(); id == "" {
		t.Fatal("the store never happened, so this test proved nothing about what a cancel leaves behind")
	}

	snaps, err := repo.ListSnapshots(context.Background(), backupengine.Source{
		Host: "nas", User: "backupd", Path: "/sets/late/late.bin",
	})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}

	if len(snaps) != 0 {
		t.Fatalf("the repository holds %d snapshots of an object the run never verified; a cancelled store must be discarded, not kept", len(snaps))
	}
}

// cancelAfterStore cancels the run in the window between a successful
// Store and the mutation check that would have verified it, which is the
// race this is about, made deterministic.
type cancelAfterStore struct {
	inner  source.RepositorySink
	cancel context.CancelFunc

	mu sync.Mutex
	id string
}

func (c *cancelAfterStore) Store(ctx context.Context, obj source.Object) (source.Stored, error) {
	out, err := c.inner.Store(ctx, obj)
	if err != nil {
		return out, err
	}

	c.mu.Lock()
	c.id = out.ID
	c.mu.Unlock()

	c.cancel()

	return out, nil
}

func (c *cancelAfterStore) Discard(ctx context.Context, id string) error {
	//nolint:wrapcheck // this is the production sink's own error.
	return c.inner.Discard(ctx, id)
}

func (c *cancelAfterStore) storedID() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.id
}
