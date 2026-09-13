package kopia

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/virtualfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/upload"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/secretref"
)

// This file is the adversarial half of the restore path (#787), and it is
// an INTERNAL test for one reason: the hostile snapshots it restores
// cannot be produced through this product's own write path.
//
// That is not a gap in the test, it is the point of it. The write path
// refuses an entry name that is not a single ordinary path element
// (kopia/tree.go's checkEntryName, backupengine/source.SafeRelPath), so a
// snapshot holding "../../etc/cron.d/x" cannot come from a backupd run.
// It can come from a repository domain shared with another tool, from an
// operator using the vendor's CLI against the same bucket, or from a
// future build of this program with a bug in it -- and a restore that is
// only safe because the writer was careful is a restore whose safety
// nobody can check. So the entries are planted with the vendor's own
// uploader, straight past our validation, and the restore is asked to
// deal with them.
//
// Every assertion is about the FILESYSTEM rather than about the returned
// error. An escape that is reported as an error and also happens is still
// an escape.

// hostileNames are the shapes a stored entry name takes when somebody is
// trying to get out of the directory an operator chose to unpack into.
//
// Every one of them is either refused or restored INSIDE the destination;
// which of the two is not asserted here, because it differs by platform
// (a name that is one legal element on Linux is a path on Windows) and
// because this table's subject is the escape rather than the spelling.
// The names whose outcome is not allowed to vary are in restorableNames
// below, and a refusal of one of those is a bug this suite must catch:
// without them, a restore that refused absolutely everything would
// satisfy every assertion in this file.
var hostileNames = []string{
	"../escaped.txt",
	"../../escaped.txt",
	"..",
	".",
	"",
	"/absolute.txt",
	"/etc/cron.d/backupd",
	`..\escaped.txt`,
	`C:\escaped.txt`,
	"C:escaped.txt",
	"sub/nested.txt",
	"sub/../../escaped.txt",
	"with\x00nul.txt",
	"./cleaned.txt",
	"trailing/",
	"..%2Fescaped.txt",
	"\xff\xfe invalid utf8",
	"....//escaped.txt",
	"ESCAPED.TXT",
	"\u202eevil.txt",
}

// restorableNames are the names #784 promises round-trip VERBATIM: each
// is one ordinary path element on the platforms this product runs on, and
// each merely LOOKS like a traversal, a drive letter or a different
// filename than it is.
//
// They must restore, with err == nil, into the destination, under exactly
// the name the snapshot holds. Refusing them would be the worse of the
// two bugs: a backup that cannot give back a file whose name contains a
// backslash is a backup that quietly loses data on the day it is read,
// and nobody finds out until then.
//
// Not in this table, deliberately: a name that is not valid UTF-8. Some
// filesystems this product restores onto (APFS) refuse to create one at
// all, so "it restored" is not a property of this code there. It stays in
// hostileNames, where either outcome is accepted and the escape is still
// asserted.
var restorableNames = []string{
	// Upper case, because a case-INSENSITIVE filesystem makes this the
	// same name as a canary sitting outside the destination, and a
	// containment check comparing strings rather than paths lets it
	// through.
	"ESCAPED.TXT",

	// A right-to-left override: what a terminal renders is not what the
	// bytes say, which is what makes it a name somebody chooses.
	"\u202eevil.txt",

	// A drive-relative path on Windows and an ordinary file name
	// everywhere else.
	"C:escaped.txt",

	// A traversal spelled with the other separator: one element here,
	// two on Windows.
	`..\escaped.txt`,
	`C:\escaped.txt`,

	// Percent-encoding is not decoded by anything in a filesystem, so
	// this is a file called exactly that.
	"..%2Fescaped.txt",
}

// TestRestoreRefusesHostileEntryNames plants each hostile name in a real
// repository and asserts that restoring it writes nothing outside the
// destination.
//
// Either outcome is accepted for any ONE name -- refused, or restored
// inside the destination -- because which of the two a name gets depends
// on the platform's own path semantics. What makes that acceptance safe
// rather than empty is TestRestoreWritesTheNamesThatOnlyLookDangerous
// below: a restore that refused everything passes this test and fails
// that one.
func TestRestoreRefusesHostileEntryNames(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep := hostileRepository(t)

	for _, name := range hostileNames {
		t.Run(nameForSubtest(name), func(t *testing.T) {
			id, ok := plantSnapshot(t, rep, name, []byte("hostile payload"))
			if !ok {
				// The vendor's uploader refused to store the name at
				// all, which is a defence one layer further out and
				// leaves nothing for this restore to be asked about.
				t.Skipf("the uploader would not store an entry named %q", name)
			}

			root := restoreSandbox(t)
			dest := filepath.Join(root, "destination")
			canary := plantCanaries(t, root)

			report, err := rep.Restore(ctx, id, backupengine.RestoreRequest{
				TargetPath: dest,
				SkipOwners: true,
				Conflict:   backupengine.ConflictOverwrite,
			})

			assertNothingEscaped(t, root, dest, canary)

			if err == nil {
				// Anything accepted has to have landed under the
				// destination with a name that is still one element.
				if report.Files == 0 {
					t.Errorf("restoring an entry named %q reported success and wrote nothing", name)
				}

				return
			}

			if !errors.Is(err, backupengine.ErrUnsafeSnapshotPath) {
				t.Errorf("restoring an entry named %q failed with %v; want ErrUnsafeSnapshotPath", name, err)
			}

			if report.Complete {
				t.Errorf("restoring an entry named %q refused it and still reported the restore complete", name)
			}
		})
	}
}

// TestRestoreWritesTheNamesThatOnlyLookDangerous is the other side of the
// refusal, and the reason a refusal can be trusted.
//
// Refusing is always safe and is therefore no evidence of anything. A
// restore that answered ErrUnsafeSnapshotPath for every name in the table
// above would satisfy every assertion in this file while being completely
// broken -- so these names, each of which is one ordinary path element
// here, must come back with err == nil, under exactly the name the
// snapshot holds, INSIDE the destination.
func TestRestoreWritesTheNamesThatOnlyLookDangerous(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("a backslash and a drive letter are path syntax on this platform, so these names are refusals here rather than files")
	}

	ctx := context.Background()
	rep := hostileRepository(t)
	payload := []byte("a file whose name only looks like an escape")

	for _, name := range restorableNames {
		t.Run(nameForSubtest(name), func(t *testing.T) {
			id, ok := plantSnapshot(t, rep, name, payload)
			if !ok {
				t.Fatalf("the uploader would not store an entry named %q, which #784 says round-trips verbatim", name)
			}

			root := restoreSandbox(t)
			dest := filepath.Join(root, "destination")
			canary := plantCanaries(t, root)

			report, err := rep.Restore(ctx, id, backupengine.RestoreRequest{
				TargetPath: dest,
				SkipOwners: true,
			})
			if err != nil {
				t.Fatalf("restoring an entry named %q failed with %v; it is one path element and must restore", name, err)
			}

			if !report.Complete || report.Files != 1 {
				t.Errorf("restoring %q reported %d files and complete=%v; want one file and a complete restore",
					name, report.Files, report.Complete)
			}

			assertNothingEscaped(t, root, dest, canary)

			// Inside the destination, under the snapshot's own name, with
			// the snapshot's own bytes. Lstat rather than Stat: a link
			// pointing at the right content is not the same answer.
			landed := filepath.Join(dest, name)

			info, err := os.Lstat(landed)
			if err != nil {
				t.Fatalf("the restore reported success and %s is not there: %v", landed, err)
			}

			if !info.Mode().IsRegular() {
				t.Fatalf("%s was restored as %s rather than as a regular file", landed, info.Mode())
			}

			body, err := os.ReadFile(landed) //nolint:gosec // a path built from the name under test, inside a temporary directory.
			if err != nil {
				t.Fatalf("reading the restored file: %v", err)
			}

			if !bytes.Equal(body, payload) {
				t.Errorf("%s holds %q; the snapshot holds %q", landed, body, payload)
			}
		})
	}
}

// TestRestoreRefusesToWriteThroughASymlinkInTheDestination covers the
// escape that needs no hostile name at all.
//
// The snapshot is ordinary; the DESTINATION has been prepared, by
// whatever put a symbolic link where a directory is about to be restored.
// A restore that creates its directories with MkdirAll, or that opens
// files without caring what the path resolves to, writes the whole
// subtree wherever the link points.
//
// The second case is the same escape on a filesystem that does not
// distinguish case -- APFS by default on macOS, NTFS, which is where an
// operator's restore destination often is. There the snapshot's "SUB" IS
// the "sub" somebody put a link at, and a check written as a string
// comparison is exactly the one that misses it.
func TestRestoreRefusesToWriteThroughASymlinkInTheDestination(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep := hostileRepository(t)

	for _, tc := range []struct {
		name      string
		stored    string
		planted   string
		needsFold bool
	}{
		{name: "the same name", stored: "sub", planted: "sub"},
		{name: "a name that only case-folds onto it", stored: "SUB", planted: "sub", needsFold: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := plantTree(t, rep, tc.stored, "inner.txt", []byte("subtree content"))
			if !ok {
				t.Fatal("the uploader would not store an ordinary nested tree")
			}

			root := restoreSandbox(t)
			dest := filepath.Join(root, "destination")
			elsewhere := filepath.Join(root, "elsewhere")

			if err := os.MkdirAll(dest, 0o750); err != nil {
				t.Fatalf("creating the destination: %v", err)
			}

			if err := os.MkdirAll(elsewhere, 0o750); err != nil {
				t.Fatalf("creating the directory the link points at: %v", err)
			}

			if tc.needsFold && !foldsCase(t, dest) {
				t.Skip("this filesystem is case-sensitive, so these are two different names here and the case above covers the escape")
			}

			if err := os.Symlink(elsewhere, filepath.Join(dest, tc.planted)); err != nil {
				t.Fatalf("planting the symbolic link: %v", err)
			}

			_, err := rep.Restore(ctx, id, backupengine.RestoreRequest{
				TargetPath: dest,
				SkipOwners: true,
				Conflict:   backupengine.ConflictOverwrite,
			})

			if !errors.Is(err, backupengine.ErrUnsafeSnapshotPath) {
				t.Errorf("restoring %q over a symbolic link called %q returned %v; want ErrUnsafeSnapshotPath",
					tc.stored, tc.planted, err)
			}

			if entries, readErr := os.ReadDir(elsewhere); readErr != nil || len(entries) != 0 {
				t.Errorf("the restore wrote %d entries through the symbolic link into %s (%v)", len(entries), elsewhere, readErr)
			}
		})
	}
}

// foldsCase reports whether dir's filesystem treats two spellings of one
// name as the same file. It asks the filesystem rather than the GOOS,
// because either answer is reachable on both platforms this runs on: a
// case-sensitive APFS volume and a case-insensitive one are both ordinary
// macOS, and ext4 beside a mounted exFAT is ordinary Linux.
func foldsCase(t *testing.T, dir string) bool {
	t.Helper()

	probe := filepath.Join(dir, ".case-probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatalf("writing the case probe: %v", err)
	}

	defer func() {
		if err := os.Remove(probe); err != nil {
			t.Errorf("removing the case probe: %v", err)
		}
	}()

	_, err := os.Lstat(filepath.Join(dir, ".CASE-PROBE"))

	return err == nil
}

// FuzzRestoreCannotEscapeTheDestination is the property, over names
// nobody thought of: whatever a snapshot calls an entry, nothing the
// restore writes lands outside the directory it was given.
//
// One repository is created for the whole run and one snapshot is planted
// per input, because the expensive part of the fixture is the repository
// and the interesting part is the name.
func FuzzRestoreCannotEscapeTheDestination(f *testing.F) {
	for _, name := range hostileNames {
		f.Add(name)
	}

	f.Add("ordinary.txt")
	f.Add("a b c.txt")

	ctx := context.Background()
	rep := hostileRepository(f)

	f.Fuzz(func(t *testing.T, name string) {
		if len(name) > 512 {
			t.Skip("a name longer than any filesystem accepts says nothing about extraction")
		}

		id, ok := plantSnapshot(t, rep, name, []byte("fuzz payload"))
		if !ok {
			t.Skip("the uploader would not store this name")
		}

		root := restoreSandbox(t)
		dest := filepath.Join(root, "destination")
		canary := plantCanaries(t, root)

		report, err := rep.Restore(ctx, id, backupengine.RestoreRequest{
			TargetPath: dest,
			SkipOwners: true,
			Conflict:   backupengine.ConflictOverwrite,
		})

		assertNothingEscaped(t, root, dest, canary)

		if err != nil && report.Complete {
			t.Errorf("restoring %q failed with %v and still reported the restore complete", name, err)
		}

		if err != nil && !errors.Is(err, backupengine.ErrUnsafeSnapshotPath) && !errors.Is(err, backupengine.ErrRestoreConflict) {
			// Anything else is a bug rather than a refusal: this restore
			// has a real repository, a writable destination and one
			// entry in it.
			t.Errorf("restoring %q failed with an unexpected error: %v", name, err)
		}
	})
}

// --- fixtures ------------------------------------------------------------

// hostileRepository opens one repository the planting helpers write into.
func hostileRepository(tb testing.TB) *repository {
	tb.Helper()

	ctx := context.Background()
	root := tb.TempDir()

	domain, err := model.NewRepositoryDomainID("hostile")
	if err != nil {
		tb.Fatalf("NewRepositoryDomainID: %v", err)
	}

	passphrase := filepath.Join(root, "passphrase")
	if err := os.WriteFile(passphrase, []byte("hostile-passphrase\n"), 0o600); err != nil {
		tb.Fatalf("writing the passphrase file: %v", err)
	}

	loc := backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     domain,
		Root:       filepath.Join(root, "backups"),
		StateDir:   filepath.Join(root, "state"),
		Passphrase: secretref.Ref{File: passphrase},
	}

	eng := New()

	if err := eng.CreateRepository(ctx, loc); err != nil {
		tb.Fatalf("CreateRepository: %v", err)
	}

	opened, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		tb.Fatalf("OpenRepository: %v", err)
	}

	tb.Cleanup(func() {
		if err := opened.Close(context.Background()); err != nil {
			tb.Errorf("Close: %v", err)
		}
	})

	rep, ok := opened.(*repository)
	if !ok {
		tb.Fatalf("OpenRepository returned %T, not this package's own handle", opened)
	}

	return rep
}

// plantSnapshot stores a snapshot whose root holds one entry with exactly
// the name given, bypassing every check this product makes on the way in.
//
// It reports false when the vendor's own uploader refuses the name, which
// is a defence further out and not something the restore can be asked
// about.
func plantSnapshot(tb testing.TB, r *repository, name string, payload []byte) (backupengine.SnapshotID, bool) {
	tb.Helper()

	return uploadStaticRoot(tb, r, []fs.Entry{
		virtualfs.StreamingFileFromReader(name, io.NopCloser(bytes.NewReader(payload))),
	})
}

// plantTree stores a snapshot holding one directory with one file in it.
func plantTree(tb testing.TB, r *repository, dirName, fileName string, payload []byte) (backupengine.SnapshotID, bool) {
	tb.Helper()

	return uploadStaticRoot(tb, r, []fs.Entry{
		virtualfs.NewStaticDirectory(dirName, []fs.Entry{
			virtualfs.StreamingFileFromReader(fileName, io.NopCloser(bytes.NewReader(payload))),
		}),
	})
}

func uploadStaticRoot(tb testing.TB, r *repository, entries []fs.Entry) (backupengine.SnapshotID, bool) {
	tb.Helper()

	ctx := context.Background()
	si := snapshot.SourceInfo{Host: "hostile-host", UserName: "hostile-user", Path: "/planted"}

	policyTree, err := policy.TreeForSource(ctx, r.rep, si)
	if err != nil {
		tb.Fatalf("resolving a policy for the planted source: %v", err)
	}

	var id backupengine.SnapshotID

	err = repo.WriteSession(ctx, r.rep, repo.WriteSessionOptions{Purpose: "backupd:test-plant"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			man, err := upload.NewUploader(w).Upload(ctx, virtualfs.NewStaticDirectory("planted", entries), policyTree, si)
			if err != nil {
				return err //nolint:wrapcheck // reported by the caller as "the uploader refused this".
			}

			saved, err := snapshot.SaveSnapshot(ctx, w, man)
			if err != nil {
				return err //nolint:wrapcheck // as above.
			}

			id = backupengine.SnapshotID(saved)

			return nil
		})
	if err != nil {
		return "", false
	}

	return id, true
}

// restoreSandbox is a directory to put a restore destination inside, with
// a parent of its own.
//
// The parent is the point. "Nothing escaped" has to be asserted one level
// further out than the destination's own directory -- a `../../x` that
// only got out by one is still out -- and that enclosing directory has to
// hold nothing but this sandbox and its canaries, which a shared t.TempDir
// parent (numbered siblings, one per parallel subtest) could not promise.
func restoreSandbox(tb testing.TB) string {
	tb.Helper()

	root := filepath.Join(tb.TempDir(), "sandbox")
	if err := os.Mkdir(root, 0o750); err != nil {
		tb.Fatalf("creating the sandbox: %v", err)
	}

	return root
}

// plantCanaries writes files beside the restore destination, and one
// level further out again, whose survival byte for byte is what "nothing
// escaped" means.
func plantCanaries(tb testing.TB, root string) map[string]string {
	tb.Helper()

	canaries := map[string]string{
		filepath.Join(root, "escaped.txt"):               "a file the escape would overwrite",
		filepath.Join(root, "destination.txt"):           "a neighbour with a confusable name",
		filepath.Join(filepath.Dir(root), "escaped.txt"): "two levels out",
	}

	for p, body := range canaries {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			tb.Fatalf("planting the canary %s: %v", p, err)
		}
	}

	return canaries
}

// assertNothingEscaped is the one assertion this file exists for.
//
// Three things, because an escape shows up differently depending on how
// far it got: a canary rewritten (it landed on something), an unexpected
// entry beside the destination (it got out by one), and an unexpected
// entry in the directory above THAT (it got out by two, which is what
// "../../" asks for). Reading only the destination's own directory would
// miss the last of those entirely.
func assertNothingEscaped(t *testing.T, root, dest string, canaries map[string]string) {
	t.Helper()

	for p, want := range canaries {
		body, err := os.ReadFile(p) //nolint:gosec // a path this test wrote.
		if err != nil {
			t.Errorf("the canary %s is gone: %v", p, err)

			continue
		}

		if string(body) != want {
			t.Errorf("the canary %s was rewritten by the restore", p)
		}
	}

	assertOnlyHolds(t, root, canaries, dest)
	assertOnlyHolds(t, filepath.Dir(root), canaries, root)
}

// assertOnlyHolds reports any entry of dir that is neither a canary nor
// one of the paths that belong there.
func assertOnlyHolds(t *testing.T, dir string, canaries map[string]string, allowed ...string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	for _, e := range entries {
		p := filepath.Join(dir, e.Name())

		if slices.Contains(allowed, p) {
			continue
		}

		if _, ok := canaries[p]; !ok {
			t.Errorf("the restore produced %q in %s, where only %v and the canaries belong", e.Name(), dir, allowed)
		}
	}
}

// nameForSubtest keeps a hostile name readable in test output without
// letting it name a directory of its own.
func nameForSubtest(name string) string {
	if name == "" {
		return "empty"
	}

	if !utf8.ValidString(name) {
		return "invalid-utf8"
	}

	return strings.NewReplacer("/", "_", `\`, "_", "\x00", "_").Replace(name)
}
