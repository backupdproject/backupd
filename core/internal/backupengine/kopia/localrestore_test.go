package kopia_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/kopia"
)

// These are the local restore path's own tests (#787): the three
// granularities an operator asks for, the conflict policy, the integrity
// check, and what a cancelled restore leaves behind.
//
// Every content assertion is a SHA-256 comparison against the tree the
// snapshot was taken of, never against the restore's own statistics. A
// restore path that truncates, skips or mis-places a file reports
// perfectly plausible counts while doing it; the bytes on the disk are
// the only evidence that does not come from the code under test.

// restoreFixtureModTime is the modification time every entry in the
// fixture tree carries. It is in the past on purpose: a restore that
// never sets a time at all produces "now", and only a stamp nothing in
// the test could have produced by accident tells the two apart.
var restoreFixtureModTime = time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)

// restoreFixtureRootMode is the mode the fixture's source root carries.
// Nothing else in this suite creates a directory with it, so a restore
// root wearing it came from the snapshot rather than from a default.
const restoreFixtureRootMode os.FileMode = 0o705

// stampTree puts restoreFixtureModTime on every file and directory under
// root.
//
// Deepest entry first, because writing into a directory updates that
// directory's own time: a parent stamped before its children were touched
// would not keep the stamp. Symbolic links are left alone -- os.Chtimes
// follows one, so stamping a link here would stamp its target instead.
func stampTree(t *testing.T, root string) {
	t.Helper()

	var paths []string

	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.Type()&os.ModeSymlink == 0 {
			paths = append(paths, p)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s to stamp it: %v", root, err)
	}

	for i := len(paths) - 1; i >= 0; i-- {
		if err := os.Chtimes(paths[i], restoreFixtureModTime, restoreFixtureModTime); err != nil {
			t.Fatalf("stamping %s: %v", paths[i], err)
		}
	}
}

// restoreFixture is one repository holding one snapshot of a small tree
// with a nested directory, three files and a symlink.
type restoreFixture struct {
	repo   backupengine.Repository
	snap   backupengine.SnapshotInfo
	source string
}

func newRestoreFixture(t *testing.T) restoreFixture {
	t.Helper()

	ctx := context.Background()
	root := t.TempDir()
	srcDir := filepath.Join(root, "source")

	mustMkdir(t, filepath.Join(srcDir, "sub", "deeper"))
	mustWrite(t, filepath.Join(srcDir, "top.txt"), []byte("the top level file"))
	mustWrite(t, filepath.Join(srcDir, "sub", "inner.txt"), randomBytes(t, 64<<10))
	mustWrite(t, filepath.Join(srcDir, "sub", "deeper", "leaf.bin"), randomBytes(t, 1<<20))

	if err := os.Symlink("top.txt", filepath.Join(srcDir, "link")); err != nil {
		t.Fatalf("creating symlink in the source tree: %v", err)
	}

	// The source root carries a mode no directory in this suite gets by
	// default, so "the restore root kept the snapshot root's mode" is a
	// comparison that can fail. A restore creates its destination with a
	// mode of its own choosing, and a restore that never applied the
	// snapshot's would otherwise be indistinguishable.
	if err := os.Chmod(srcDir, restoreFixtureRootMode); err != nil {
		t.Fatalf("setting the source root's mode: %v", err)
	}

	// Every file and directory is given one known modification time
	// before the snapshot is taken. It is what makes "the restore kept
	// the times" an assertion at all: a tree whose times were whatever
	// the clock said when the fixture ran would compare equal to a
	// restore that stamped everything with its own "now".
	stampTree(t, srcDir)

	loc := localLocation(t, root, "restore-domain")
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

	snap, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{
		Source: backupengine.Source{Host: "restore-host", User: "restore-user", Path: srcDir},
	})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	return restoreFixture{repo: rep, snap: snap, source: srcDir}
}

// TestRestoreWholeSnapshotMatchesTheSourceByHash is the first acceptance
// criterion: everything that was backed up comes back, byte for byte.
//
// It compares in both directions. Walking the source and hashing each
// file against its restored counterpart finds what the restore failed to
// write or wrote wrongly; walking the restored tree and looking for each
// path in the source finds what it wrote that nobody asked for, which is
// the shape a path-traversal bug has.
func TestRestoreWholeSnapshotMatchesTheSourceByHash(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newRestoreFixture(t)
	dest := filepath.Join(t.TempDir(), "restored")

	report, err := f.repo.Restore(ctx, f.snap.ID, backupengine.RestoreRequest{
		TargetPath:    dest,
		SkipOwners:    true,
		VerifyContent: true,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if !report.Complete {
		t.Error("Restore returned a nil error and a report that does not claim completeness")
	}

	if report.Files != 3 || report.Symlinks != 1 || report.Directories != 2 {
		t.Errorf("Restore reported %d files, %d symlinks, %d directories; the snapshot holds 3, 1 and 2",
			report.Files, report.Symlinks, report.Directories)
	}

	if report.Verified != report.Files {
		t.Errorf("Restore verified %d of %d files it wrote, with VerifyContent set", report.Verified, report.Files)
	}

	assertTreesMatch(t, f.source, dest)
}

// TestRestoreOneDirectoryWritesOnlyThatSubtree is the second granularity.
//
// The assertion that matters is the negative one: a directory restore
// that quietly restored the whole snapshot would satisfy every hash
// comparison on the files it was asked for.
func TestRestoreOneDirectoryWritesOnlyThatSubtree(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newRestoreFixture(t)
	dest := filepath.Join(t.TempDir(), "restored")

	report, err := f.repo.Restore(ctx, f.snap.ID, backupengine.RestoreRequest{
		SourcePath:    "sub",
		TargetPath:    dest,
		SkipOwners:    true,
		VerifyContent: true,
	})
	if err != nil {
		t.Fatalf("Restore of one directory: %v", err)
	}

	if !report.Complete || report.Files != 2 {
		t.Errorf("restoring sub/ reported %d files (complete=%v); it holds inner.txt and deeper/leaf.bin",
			report.Files, report.Complete)
	}

	assertTreesMatch(t, filepath.Join(f.source, "sub"), dest)

	if _, err := os.Lstat(filepath.Join(dest, "top.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("restoring sub/ also produced top.txt, which is not in it (%v)", err)
	}
}

// TestRestoreOneFileWritesThatFileOnly is the third granularity, and the
// one an operator reaches for most: somebody deleted one file and wants
// it back without unpacking a terabyte beside it.
func TestRestoreOneFileWritesThatFileOnly(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newRestoreFixture(t)
	dest := filepath.Join(t.TempDir(), "restored")

	report, err := f.repo.Restore(ctx, f.snap.ID, backupengine.RestoreRequest{
		SourcePath:    "sub/inner.txt",
		TargetPath:    dest,
		SkipOwners:    true,
		VerifyContent: true,
	})
	if err != nil {
		t.Fatalf("Restore of one file: %v", err)
	}

	if !report.Complete || report.Files != 1 || report.Directories != 0 {
		t.Errorf("restoring one file reported %d files, %d directories, complete=%v; want 1, 0, true",
			report.Files, report.Directories, report.Complete)
	}

	// The file keeps its own name and lands INSIDE the target directory,
	// which is the only placement that lets a caller name a destination
	// without also having to know what they are about to receive.
	got := filepath.Join(dest, "inner.txt")

	if hashFile(t, got) != hashFile(t, filepath.Join(f.source, "sub", "inner.txt")) {
		t.Errorf("%s does not match the file the snapshot holds", got)
	}

	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("reading the restore destination: %v", err)
	}

	if len(entries) != 1 {
		t.Errorf("a single-file restore left %d entries in the destination; want exactly one", len(entries))
	}
}

// TestRestorePathNotInSnapshotIsItsOwnRefusal separates "that restore
// point is gone" from "that file was not in it", which is the ordinary
// answer to an operator asking whether something was backed up on a
// given day.
func TestRestorePathNotInSnapshotIsItsOwnRefusal(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newRestoreFixture(t)

	_, err := f.repo.Restore(ctx, f.snap.ID, backupengine.RestoreRequest{
		SourcePath: "sub/not-here.txt",
		TargetPath: filepath.Join(t.TempDir(), "restored"),
		SkipOwners: true,
	})

	if !errors.Is(err, backupengine.ErrRestorePathNotFound) {
		t.Errorf("Restore of a path the snapshot does not hold returned %v; want ErrRestorePathNotFound", err)
	}

	if errors.Is(err, backupengine.ErrSnapshotNotFound) {
		t.Error("a missing path inside a snapshot was reported as a missing snapshot")
	}
}

// TestRestoreConflictPolicy covers the three answers to "something is
// already there", including that silence means the safe one.
func TestRestoreConflictPolicy(t *testing.T) {
	t.Parallel()

	const occupied = "this file was here first"

	for _, tc := range []struct {
		name     string
		policy   backupengine.RestoreConflict
		wantErr  error
		wantKept bool
	}{
		{name: "silence refuses", policy: "", wantErr: backupengine.ErrRestoreConflict, wantKept: true},
		{name: "refuse", policy: backupengine.ConflictRefuse, wantErr: backupengine.ErrRestoreConflict, wantKept: true},
		{name: "skip", policy: backupengine.ConflictSkip, wantKept: true},
		{name: "overwrite", policy: backupengine.ConflictOverwrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			f := newRestoreFixture(t)
			dest := filepath.Join(t.TempDir(), "restored")

			mustMkdir(t, dest)
			mustWrite(t, filepath.Join(dest, "top.txt"), []byte(occupied))

			report, err := f.repo.Restore(ctx, f.snap.ID, backupengine.RestoreRequest{
				TargetPath: dest,
				Conflict:   tc.policy,
				SkipOwners: true,
			})

			switch {
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("Restore returned %v; want %v", err, tc.wantErr)
			case tc.wantErr == nil && err != nil:
				t.Fatalf("Restore: %v", err)
			}

			if tc.wantErr != nil && report.Complete {
				t.Error("a restore that stopped on a conflict reported itself complete")
			}

			body, err := os.ReadFile(filepath.Join(dest, "top.txt"))
			if err != nil {
				t.Fatalf("reading the contested file: %v", err)
			}

			if kept := string(body) == occupied; kept != tc.wantKept {
				t.Errorf("after a %q restore the existing file was kept=%v; want %v", tc.policy, kept, tc.wantKept)
			}

			if tc.policy == backupengine.ConflictSkip {
				if report.Skipped == 0 {
					t.Error("a skip-policy restore that stepped over an existing file reported nothing skipped")
				}

				// The rest of the tree still has to arrive: skipping is
				// about the collision, not about giving up.
				if hashFile(t, filepath.Join(dest, "sub", "inner.txt")) !=
					hashFile(t, filepath.Join(f.source, "sub", "inner.txt")) {
					t.Error("a skip-policy restore did not restore the entries that were not in conflict")
				}
			}

			if tc.policy == backupengine.ConflictOverwrite {
				// Not merely "different from what was there": an
				// overwrite that truncated the file, or wrote a
				// zero-length one, or left the working file's name in
				// place, also produces bytes that are not the occupier's.
				// The claim is that the destination now holds the
				// SNAPSHOT's file.
				if hashFile(t, filepath.Join(dest, "top.txt")) != hashFile(t, filepath.Join(f.source, "top.txt")) {
					t.Error("an overwrite-policy restore replaced the existing file with something that is not the snapshot's")
				}
			}
		})
	}
}

// TestRestoreRefusesAnUnknownConflictPolicy: guessing which of three a
// misspelling meant is how "skip" becomes "overwrite".
func TestRestoreRefusesAnUnknownConflictPolicy(t *testing.T) {
	t.Parallel()

	f := newRestoreFixture(t)

	_, err := f.repo.Restore(context.Background(), f.snap.ID, backupengine.RestoreRequest{
		TargetPath: filepath.Join(t.TempDir(), "restored"),
		Conflict:   backupengine.RestoreConflict("Overwrite"),
	})

	if !errors.Is(err, backupengine.ErrUnknownRestoreConflict) {
		t.Errorf("Restore with an unknown conflict policy returned %v; want ErrUnknownRestoreConflict", err)
	}
}

// TestRestoreRefusesAnUnsafeSourcePath: the selection is untrusted input
// too. A caller cannot use it to walk out of the snapshot.
func TestRestoreRefusesAnUnsafeSourcePath(t *testing.T) {
	t.Parallel()

	f := newRestoreFixture(t)

	for _, sel := range []string{"../outside", "sub/../../elsewhere", "sub/\x00/x", "/../etc/shadow"} {
		_, err := f.repo.Restore(context.Background(), f.snap.ID, backupengine.RestoreRequest{
			SourcePath: sel,
			TargetPath: filepath.Join(t.TempDir(), "restored"),
		})

		if !errors.Is(err, backupengine.ErrUnsafeSnapshotPath) {
			t.Errorf("Restore of %q returned %v; want ErrUnsafeSnapshotPath", sel, err)
		}
	}
}

// TestRestoreSourcePathSlashesAreSpelling separates a traversal from a
// way of writing a path.
//
// A selection arrives from a surface that joined strings together: "sub",
// "/sub" and "sub/" all name one directory in the snapshot, and a leading
// slash means the snapshot's root because a snapshot has no other root to
// be relative to. Reporting any of them as an unsafe path would make an
// operator's correctly-spelled request look like an attempted escape --
// and it would teach whoever saw it that the refusal means nothing.
func TestRestoreSourcePathSlashesAreSpelling(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newRestoreFixture(t)

	for _, sel := range []string{"sub", "/sub", "sub/", "/sub/"} {
		dest := filepath.Join(t.TempDir(), "restored")

		report, err := f.repo.Restore(ctx, f.snap.ID, backupengine.RestoreRequest{
			SourcePath:    sel,
			TargetPath:    dest,
			SkipOwners:    true,
			VerifyContent: true,
		})
		if err != nil {
			t.Fatalf("Restore of %q returned %v; it names the same directory as \"sub\"", sel, err)
		}

		if !report.Complete {
			t.Errorf("Restore of %q returned a report that does not claim completeness", sel)
		}

		if hashFile(t, filepath.Join(dest, "inner.txt")) != hashFile(t, filepath.Join(f.source, "sub", "inner.txt")) {
			t.Errorf("Restore of %q did not produce the subtree's own contents", sel)
		}
	}

	// And a path that is spelled like an absolute one but names nothing
	// is a path that is not in the snapshot -- which is a different
	// answer from "that selection is unsafe", and the one an operator
	// asking whether a file was backed up needs to hear.
	_, err := f.repo.Restore(ctx, f.snap.ID, backupengine.RestoreRequest{
		SourcePath: "/etc/shadow",
		TargetPath: filepath.Join(t.TempDir(), "restored"),
	})

	if !errors.Is(err, backupengine.ErrRestorePathNotFound) {
		t.Errorf("Restore of %q returned %v; want ErrRestorePathNotFound", "/etc/shadow", err)
	}
}

// TestRestoreWithNoDestinationIsRefused: a restore that invented a
// directory would fill somebody's /tmp with a tree nobody asked for.
func TestRestoreWithNoDestinationIsRefused(t *testing.T) {
	t.Parallel()

	f := newRestoreFixture(t)

	if _, err := f.repo.Restore(context.Background(), f.snap.ID, backupengine.RestoreRequest{}); !errors.Is(err, backupengine.ErrNoRestoreDestination) {
		t.Errorf("Restore with no target returned %v; want ErrNoRestoreDestination", err)
	}
}

// TestRestoreReportsProgressPerEntry is what a surface renders while an
// operator waits, and its contract is that the running totals it carries
// are the same numbers the final report ends with rather than a second
// count kept beside them.
func TestRestoreReportsProgressPerEntry(t *testing.T) {
	t.Parallel()

	f := newRestoreFixture(t)
	dest := filepath.Join(t.TempDir(), "restored")

	var (
		mu   sync.Mutex
		seen []backupengine.RestoreProgress
	)

	report, err := f.repo.Restore(context.Background(), f.snap.ID, backupengine.RestoreRequest{
		TargetPath: dest,
		SkipOwners: true,
		Progress: func(p backupengine.RestoreProgress) {
			mu.Lock()
			defer mu.Unlock()

			seen = append(seen, p)
		},
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if len(seen) == 0 {
		t.Fatal("Restore reported no progress at all")
	}

	last := seen[len(seen)-1]
	if last.Files != report.Files || last.Bytes != report.Bytes || last.Directories != report.Directories {
		t.Errorf("the last progress reading (%d files, %d dirs, %d bytes) disagrees with the report (%d, %d, %d)",
			last.Files, last.Directories, last.Bytes, report.Files, report.Directories, report.Bytes)
	}

	var paths []string
	for _, p := range seen {
		if p.Path == "" {
			t.Error("a progress reading named no path")
		}

		paths = append(paths, p.Path)
	}

	sort.Strings(paths)

	if got := strings.Join(paths, " "); !strings.Contains(got, "sub/deeper/leaf.bin") {
		t.Errorf("progress readings named %s; the deepest file is missing", got)
	}
}

// TestCancelledRestoreStopsTheWalkAndClaimsNothing is the cancellation
// contract at the level of the WALK: a restore torn down partway reports
// the cancellation, does not claim completeness, and leaves a destination
// in which everything present is whole.
//
// It cancels on the first progress reading, which is a directory, so what
// it exercises is the walk's own context check and the state of the tree
// it stopped in. The other half -- a cancellation landing in the middle
// of one file's BYTES, where the working file has to be removed and the
// counters must not include it -- cannot be reached from here (the first
// entry noted is never a file) and is
// TestCancellingOneFilesCopyLeavesNothingBehind, in this package's
// internal tests.
func TestCancelledRestoreStopsTheWalkAndClaimsNothing(t *testing.T) {
	t.Parallel()

	f := newRestoreFixture(t)
	dest := filepath.Join(t.TempDir(), "restored")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancelled from inside the restore, after the first entry, so the
	// tear-down happens in the middle of a walk rather than before it
	// starts. Deterministic: no sleeps, no racing goroutine.
	report, err := f.repo.Restore(ctx, f.snap.ID, backupengine.RestoreRequest{
		TargetPath: dest,
		SkipOwners: true,
		Progress:   func(backupengine.RestoreProgress) { cancel() },
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled restore returned %v; want an error wrapping context.Canceled", err)
	}

	if report.Complete {
		t.Error("a cancelled restore reported itself complete")
	}

	// Every file that IS there has to be whole: the source is the
	// authority, and a file present under its real name with the wrong
	// bytes is the failure this test exists for.
	filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error { //nolint:errcheck // the walk's own findings are the assertions below.
		if err != nil || d.IsDir() {
			return nil
		}

		rel, relErr := filepath.Rel(dest, p)
		if relErr != nil {
			t.Errorf("relativising %s: %v", p, relErr)

			return nil
		}

		if strings.HasPrefix(d.Name(), ".") {
			t.Errorf("a cancelled restore left the working file %s behind", rel)

			return nil
		}

		if info, statErr := os.Lstat(p); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		if hashFile(t, p) != hashFile(t, filepath.Join(f.source, rel)) {
			t.Errorf("a cancelled restore left %s under its real name with content that is not the snapshot's", rel)
		}

		return nil
	})
}

// TestRestorePreservesModificationTimes is the fidelity half of a
// restore, and the half that fails silently.
//
// A restored tree whose every entry was modified "now" is content
// without history: nothing in it says when the data was written, every
// incremental tool pointed at it copies the whole thing again, and a diff
// against the original reports differences on files that are identical.
// Nothing errors, which is why this is asserted rather than assumed.
func TestRestorePreservesModificationTimes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newRestoreFixture(t)
	dest := filepath.Join(t.TempDir(), "restored")

	if _, err := f.repo.Restore(ctx, f.snap.ID, backupengine.RestoreRequest{
		TargetPath:    dest,
		SkipOwners:    true,
		VerifyContent: true,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// The restore ROOT is in the list because this restore created it,
	// and a directory created by a restore carries the snapshot's own
	// time like every other one. The directories are the cases a
	// per-file implementation gets wrong: their times are set after
	// their children are written, or not at all.
	for _, rel := range []string{
		".",
		"top.txt",
		"sub",
		filepath.Join("sub", "inner.txt"),
		filepath.Join("sub", "deeper"),
		filepath.Join("sub", "deeper", "leaf.bin"),
	} {
		info, err := os.Lstat(filepath.Join(dest, rel))
		if err != nil {
			t.Fatalf("the restore did not produce %s: %v", rel, err)
		}

		if !info.ModTime().Equal(restoreFixtureModTime) {
			t.Errorf("%s was restored with modification time %s; the snapshot holds %s",
				rel, info.ModTime().UTC(), restoreFixtureModTime)
		}
	}

	// The restore root also wears the snapshot root's MODE, for the same
	// reason every other directory does: it is a directory this restore
	// created, out of a directory the snapshot describes.
	info, err := os.Lstat(dest)
	if err != nil {
		t.Fatalf("the restore root is not there: %v", err)
	}

	if info.Mode().Perm() != restoreFixtureRootMode {
		t.Errorf("the restore root has mode %s; the snapshot root holds %s", info.Mode().Perm(), restoreFixtureRootMode)
	}
}

// TestRestoreAppliesDirectoryOwnershipWhenAsked is the other half of
// SkipOwners=false: a restore that chowned only files would leave every
// directory in a restored tree owned by whoever ran the restore, which is
// the one thing an operator who asked to keep ownership asked not to
// happen.
//
// It is proved with a GROUP rather than a user, because that is the one
// ownership change an unprivileged process can actually make: a
// non-member cannot be given a file, but any group this process belongs
// to can be. A machine whose user has only its primary group cannot be
// asked the question at all, and says so.
func TestRestoreAppliesDirectoryOwnershipWhenAsked(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newRestoreFixture(t)

	groups, err := os.Getgroups()
	if err != nil {
		t.Fatalf("Getgroups: %v", err)
	}

	sourceInfo, err := os.Stat(filepath.Join(f.source, "sub"))
	if err != nil {
		t.Fatalf("stat-ing the source directory: %v", err)
	}

	primary := int(sourceInfo.Sys().(*syscall.Stat_t).Gid) //nolint:forcetypeassert // a local filesystem on the platforms this suite runs on.

	secondary := -1

	for _, g := range groups {
		if g != primary {
			secondary = g

			break
		}
	}

	if secondary < 0 {
		t.Skip("this process belongs to one group only, so no ownership change it is allowed to make is observable")
	}

	// Stamped on the SOURCE, so the snapshot carries it and the restore
	// has something to reproduce that it would not have produced anyway.
	if err := os.Chown(filepath.Join(f.source, "sub"), -1, secondary); err != nil {
		t.Skipf("this process may not give %s away to group %d: %v", filepath.Join(f.source, "sub"), secondary, err)
	}

	snap, err := f.repo.Snapshot(ctx, backupengine.SnapshotRequest{
		Source: backupengine.Source{Host: "restore-host", User: "restore-user", Path: f.source},
	})
	if err != nil {
		t.Fatalf("re-snapshotting the source after the ownership change: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "restored")

	if _, err := f.repo.Restore(ctx, snap.ID, backupengine.RestoreRequest{
		TargetPath: dest,
		SkipOwners: false,
	}); err != nil {
		t.Fatalf("Restore with ownership: %v", err)
	}

	restored, err := os.Stat(filepath.Join(dest, "sub"))
	if err != nil {
		t.Fatalf("the restore did not produce the directory: %v", err)
	}

	if gid := int(restored.Sys().(*syscall.Stat_t).Gid); gid != secondary { //nolint:forcetypeassert // as above.
		t.Errorf("the restored directory belongs to group %d; the snapshot holds %d", gid, secondary)
	}
}

// TestRestoreLeavesAPreExistingDirectorysModeAlone is the boundary of
// what re-entering an existing directory permits.
//
// A restore walks into a directory that is already there under every
// conflict policy, including the refusing one, because a directory holds
// no data of its own. That is not permission to restyle it: its
// permissions are a statement its owner made about their filesystem, and
// a restore asked to REFUSE collisions rewriting them would be the
// destructive act that policy exists to prevent.
func TestRestoreLeavesAPreExistingDirectorysModeAlone(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newRestoreFixture(t)
	dest := filepath.Join(t.TempDir(), "restored")

	const prepared os.FileMode = 0o777

	mustMkdir(t, filepath.Join(dest, "sub"))

	if err := os.Chmod(filepath.Join(dest, "sub"), prepared); err != nil {
		t.Fatalf("preparing the existing directory: %v", err)
	}

	prearranged := time.Date(2019, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dest, "sub"), prearranged, prearranged); err != nil {
		t.Fatalf("stamping the existing directory: %v", err)
	}

	if _, err := f.repo.Restore(ctx, f.snap.ID, backupengine.RestoreRequest{
		TargetPath:    dest,
		Conflict:      backupengine.ConflictRefuse,
		SkipOwners:    true,
		VerifyContent: true,
	}); err != nil {
		t.Fatalf("Restore into a destination holding one of its directories: %v", err)
	}

	info, err := os.Lstat(filepath.Join(dest, "sub"))
	if err != nil {
		t.Fatalf("the prepared directory is gone: %v", err)
	}

	if info.Mode().Perm() != prepared {
		t.Errorf("the restore rewrote the prepared directory's mode to %s; it was %s", info.Mode().Perm(), prepared)
	}

	// Its contents still arrived: leaving the directory alone is about
	// the directory, not about giving up on what goes in it.
	if hashFile(t, filepath.Join(dest, "sub", "inner.txt")) != hashFile(t, filepath.Join(f.source, "sub", "inner.txt")) {
		t.Error("the restore left the prepared directory empty")
	}
}

// --- helpers -------------------------------------------------------------

func mustMkdir(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
}

func hashFile(t *testing.T, path string) string {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	sum := sha256.Sum256(body)

	return hex.EncodeToString(sum[:])
}

// assertTreesMatch compares two local trees both ways round: every entry
// of want has an identical counterpart in got, and got holds nothing want
// does not.
func assertTreesMatch(t *testing.T, want, got string) {
	t.Helper()

	wantEntries := treeFingerprint(t, want)
	gotEntries := treeFingerprint(t, got)

	for rel, sum := range wantEntries {
		other, ok := gotEntries[rel]
		if !ok {
			t.Errorf("%s is in the snapshot and the restore did not produce it", rel)

			continue
		}

		if other != sum {
			t.Errorf("%s restored as %s and the source holds %s", rel, other, sum)
		}
	}

	for rel := range gotEntries {
		if _, ok := wantEntries[rel]; !ok {
			t.Errorf("the restore wrote %s, which is not in the snapshot", rel)
		}
	}
}

// treeFingerprint maps every entry of a tree to a string that changes
// when its kind or its content does: a hash for a file, the target for a
// symlink, a marker for a directory.
func treeFingerprint(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		if rel == "." {
			return nil
		}

		switch {
		case d.IsDir():
			out[rel] = "dir"
		case d.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}

			out[rel] = "link:" + target
		default:
			out[rel] = hashFile(t, p)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	return out
}
