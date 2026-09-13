package kopia

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kopia/kopia/fs"

	"github.com/backupdproject/backupd/core/internal/backupengine"
)

// These are the restore path's per-FILE contracts, driven at
// (*extractor).file rather than through Restore, and internal for the
// reason localrestoreescape_test.go is: what they need in a snapshot
// cannot be put there through this product's own write path.
//
// A file whose stored mode denies its owner a read is the clearest case.
// Such files exist on real sources -- 0200 drop-boxes, 0000 placeholders
// -- and a repository holds the mode it found. But this process cannot
// READ one to back it up unless it is root, so no fixture that snapshots
// a real tree can produce one, and the restore of one is exactly where
// the order of "chmod" and "verify" stops being a matter of taste.
//
// The cancellation contract is the other. Through Restore, a cancellation
// after the first noted entry lands on a DIRECTORY, so no file is ever
// written and the assertions inspect an empty tree: a copy loop that
// ignored the context entirely would pass. Cancelling in the middle of
// one file's bytes is the thing worth asserting, and the deterministic
// way to do it is a reader that cancels as it is read.

// stubFile is one file in a snapshot with exactly the mode, times and
// content a test wants, and nothing that had to survive an upload.
type stubFile struct {
	name    string
	mode    os.FileMode
	modTime time.Time
	body    []byte

	// open, when set, supplies the reader instead of body. It is how the
	// cancellation test gets a reader that cancels the context it is
	// being read under.
	open func(ctx context.Context) (fs.Reader, error)
}

func (f *stubFile) Name() string       { return f.name }
func (f *stubFile) Size() int64        { return int64(len(f.body)) }
func (f *stubFile) Mode() os.FileMode  { return f.mode }
func (f *stubFile) ModTime() time.Time { return f.modTime }
func (f *stubFile) IsDir() bool        { return false }
func (f *stubFile) Sys() any           { return nil }

func (f *stubFile) Owner() fs.OwnerInfo         { return fs.OwnerInfo{} }
func (f *stubFile) Device() fs.DeviceInfo       { return fs.DeviceInfo{} }
func (f *stubFile) LocalFilesystemPath() string { return "" }
func (f *stubFile) Close()                      {}

func (f *stubFile) Open(ctx context.Context) (fs.Reader, error) {
	if f.open != nil {
		return f.open(ctx)
	}

	return &stubReader{entry: f, ReadSeeker: bytes.NewReader(f.body)}, nil
}

// stubReader is an fs.Reader over whatever a test wants read.
type stubReader struct {
	io.ReadSeeker

	entry fs.Entry
}

func (r *stubReader) Close() error             { return nil }
func (r *stubReader) Entry() (fs.Entry, error) { return r.entry, nil }

// cancellingReader hands over one full copy chunk and cancels the
// restore's context while doing it, so the copy loop's next context check
// is the one under test.
type cancellingReader struct {
	cancel context.CancelFunc
	reads  int
	entry  fs.Entry
}

func (r *cancellingReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads > 1 {
		// A copy loop that checked the context only at the start would
		// get here, and a test that let it read on would be asserting
		// about a file that finished.
		return 0, errors.New("the copy read on after the context was cancelled")
	}

	for i := range p {
		p[i] = 'x'
	}

	r.cancel()

	return len(p), nil
}

func (r *cancellingReader) Seek(int64, int) (int64, error) { return 0, errors.New("not seekable") }
func (r *cancellingReader) Close() error                   { return nil }
func (r *cancellingReader) Entry() (fs.Entry, error)       { return r.entry, nil }

// testExtractor is an extractor writing into root, built the way Restore
// builds one so that the copy buffer is really there.
func testExtractor(t *testing.T, root string, req backupengine.RestoreRequest) *extractor {
	t.Helper()

	policy, err := backupengine.ParseRestoreConflict(string(req.Conflict))
	if err != nil {
		t.Fatalf("ParseRestoreConflict: %v", err)
	}

	return newExtractor(root, policy, req, true)
}

// TestRestoringAFileWhoseModeDeniesReadingItStillVerifies is the order
// bug: the snapshot's mode may not be applied to the working file before
// the working file is read back.
//
// A restore that chmods first cannot re-open a 0200 file as anything but
// root, and app.RestoreSnapshot asks for verification on every durable
// restore -- so getting this order wrong makes a legitimate snapshot
// unrestorable, with an EACCES on a path the operator can see is there.
func TestRestoringAFileWhoseModeDeniesReadingItStillVerifies(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read any mode; this test is about what an ordinary process can do")
	}

	dest := t.TempDir()
	body := []byte("the contents of a file nobody may read")

	for _, mode := range []os.FileMode{0o200, 0o000} {
		t.Run(mode.String(), func(t *testing.T) {
			x := testExtractor(t, dest, backupengine.RestoreRequest{SkipOwners: true, VerifyContent: true})
			dst := filepath.Join(dest, "write-only-"+mode.String())

			f := &stubFile{name: filepath.Base(dst), mode: mode, body: body}

			if err := x.file(context.Background(), f, dst, f.name); err != nil {
				t.Fatalf("restoring a file stored with mode %s: %v", mode, err)
			}

			if x.verified != 1 {
				t.Errorf("the restore verified %d files; it was asked to verify the one it wrote", x.verified)
			}

			info, err := os.Lstat(dst)
			if err != nil {
				t.Fatalf("the restored file is not there: %v", err)
			}

			if info.Mode().Perm() != mode.Perm() {
				t.Errorf("the restored file has mode %s; the snapshot holds %s", info.Mode().Perm(), mode.Perm())
			}

			// And the bytes are the snapshot's, which is the claim
			// verification makes. Readable again only because this test
			// owns the file.
			if err := os.Chmod(dst, 0o600); err != nil {
				t.Fatalf("making the restored file readable to check it: %v", err)
			}

			got, err := os.ReadFile(dst) //nolint:gosec // a path this test built.
			if err != nil {
				t.Fatalf("reading the restored file: %v", err)
			}

			if !bytes.Equal(got, body) {
				t.Errorf("the restored file holds %q; the snapshot holds %q", got, body)
			}
		})
	}
}

// TestVerifyWrittenComparesTheDigestItWasGiven is the negative half of
// the integrity check.
//
// The suite otherwise only ever asserts that verification PASSES over
// bytes that are correct, which a verifyWritten that returned nil
// unconditionally would satisfy. What has to be true is that a wrong
// digest is reported, and that the comparison is of the bytes rather than
// of the spelling of a hash.
func TestVerifyWrittenComparesTheDigestItWasGiven(t *testing.T) {
	t.Parallel()

	x := testExtractor(t, t.TempDir(), backupengine.RestoreRequest{SkipOwners: true, VerifyContent: true})

	path := filepath.Join(t.TempDir(), "written")
	body := []byte("bytes that really are on the disk")

	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing the file to verify: %v", err)
	}

	f, err := os.Open(path) //nolint:gosec // a path this test built.
	if err != nil {
		t.Fatalf("opening the file to verify: %v", err)
	}

	defer f.Close() //nolint:errcheck // a read handle in a test.

	right := sha256.Sum256(body)

	if err := x.verifyWritten(f, right[:], "written"); err != nil {
		t.Errorf("verifying a file against its own digest failed: %v", err)
	}

	wrong := sha256.Sum256([]byte("bytes the repository actually handed over"))

	err = x.verifyWritten(f, wrong[:], "written")
	if err == nil {
		t.Fatal("verifying a file against a digest that is not its own succeeded, so the comparison is not one")
	}

	// The sentence has to say what was found and what was expected: it
	// is the only evidence an operator gets about a restore that wrote
	// the wrong bytes.
	if !strings.Contains(err.Error(), "written") {
		t.Errorf("the mismatch was reported as %q, which does not name the file", err)
	}
}

// TestCancellingOneFilesCopyLeavesNothingBehind is the cancellation
// contract where it actually applies: in the middle of a file's bytes.
//
// Three separate things have to be true, and each of them is a bug
// somebody has shipped: the error says it was cancelled (a copy that
// ignored the context would return nil), the destination has no file
// under its real name, and the WORKING file is gone too -- a restore that
// forgot os.Remove leaves a plausible partial file under a dotted name
// for whoever cleans up to wonder about.
func TestCancellingOneFilesCopyLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	dest := t.TempDir()
	dst := filepath.Join(dest, "large.bin")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	x := testExtractor(t, dest, backupengine.RestoreRequest{SkipOwners: true, VerifyContent: true})

	f := &stubFile{
		name: "large.bin",
		mode: 0o600,
		// Bigger than one copy step, so the copy has to come back round
		// to the context check rather than finishing the file first.
		body: make([]byte, 4*restoreCopyChunk),
	}
	f.open = func(context.Context) (fs.Reader, error) {
		return &cancellingReader{cancel: cancel, entry: f}, nil
	}

	err := x.file(ctx, f, dst, f.name)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled copy returned %v; want an error wrapping context.Canceled", err)
	}

	if _, statErr := os.Lstat(dst); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a cancelled restore left %s under its real name (%v)", dst, statErr)
	}

	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("reading the destination: %v", err)
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), restoreWorkPrefix) {
			t.Errorf("a cancelled restore left the working file %s behind", e.Name())
		}
	}

	if x.files != 0 {
		t.Errorf("a cancelled restore counted %d files it did not publish", x.files)
	}

	// The byte count is the one an operator reads to decide whether to
	// trust the tree, so it may not include a file that was removed.
	if x.bytes != 0 {
		t.Errorf("a cancelled restore counted %d bytes for a file it removed", x.bytes)
	}

	if x.verified != 0 {
		t.Errorf("a cancelled restore claims to have verified %d files", x.verified)
	}
}

// stubSymlink is one symbolic link in a snapshot, with the time a test
// wants on the LINK rather than on whatever it points at.
type stubSymlink struct {
	stubFile

	target string
}

func (l *stubSymlink) Mode() os.FileMode { return l.mode | os.ModeSymlink }

func (l *stubSymlink) Readlink(context.Context) (string, error) { return l.target, nil }

// Resolve is never called: following a restored link is the one thing
// this restore path will not do, and a stub that resolved would let a
// regression that started following them pass.
func (l *stubSymlink) Resolve(context.Context) (fs.Entry, error) {
	return nil, errors.New("a restore resolved a symbolic link it was only supposed to write")
}

// TestRestoringASymlinkSetsTheLinksOwnTime is the case os.Chtimes cannot
// serve.
//
// Chtimes follows a symbolic link, so restoring a link's timestamp with
// it would stamp the link's TARGET -- a write outside the restore
// destination whenever the target is outside it, which is the escape this
// package refuses everywhere else. The link's own time therefore goes
// through lchtimes, and this asserts the result on the link (Lstat) while
// proving the target was left alone.
func TestRestoringASymlinkSetsTheLinksOwnTime(t *testing.T) {
	t.Parallel()

	dest := t.TempDir()
	stored := time.Date(2018, 7, 6, 5, 4, 3, 0, time.UTC)
	targetStamp := time.Date(2020, 11, 12, 13, 14, 15, 0, time.UTC)

	// A real file for the link to point at, with a time of its own, so
	// that stamping through the link is visible rather than harmless.
	target := filepath.Join(dest, "target.txt")
	if err := os.WriteFile(target, []byte("the file the link points at"), 0o600); err != nil {
		t.Fatalf("writing the link's target: %v", err)
	}

	if err := os.Chtimes(target, targetStamp, targetStamp); err != nil {
		t.Fatalf("stamping the link's target: %v", err)
	}

	x := testExtractor(t, dest, backupengine.RestoreRequest{SkipOwners: true})
	dst := filepath.Join(dest, "link")

	l := &stubSymlink{
		stubFile: stubFile{name: "link", mode: 0o777, modTime: stored},
		target:   "target.txt",
	}

	if err := x.symlink(context.Background(), l, dst, l.name); err != nil {
		t.Fatalf("restoring a symbolic link: %v", err)
	}

	info, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("the restored link is not there: %v", err)
	}

	if !info.ModTime().Equal(stored) {
		t.Errorf("the restored link carries modification time %s; the snapshot holds %s", info.ModTime().UTC(), stored)
	}

	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat-ing the link's target: %v", err)
	}

	if !targetInfo.ModTime().Equal(targetStamp) {
		t.Errorf("restoring the link rewrote its TARGET's modification time to %s; it was %s",
			targetInfo.ModTime().UTC(), targetStamp)
	}
}
