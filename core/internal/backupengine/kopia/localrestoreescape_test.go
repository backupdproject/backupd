package kopia

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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

// TestRestoreRefusesHostileEntryNames plants each hostile name in a real
// repository and asserts that restoring it writes nothing outside the
// destination.
//
// A refusal is the expected outcome for all but the last few, which are
// merely unusual rather than dangerous and must restore normally: a test
// that accepted "refused everything" would pass against a restore that
// does nothing at all.
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

			root := t.TempDir()
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

// TestRestoreRefusesToWriteThroughASymlinkInTheDestination covers the
// escape that needs no hostile name at all.
//
// The snapshot is ordinary; the DESTINATION has been prepared, by
// whatever put a symbolic link where a directory is about to be restored.
// A restore that creates its directories with MkdirAll, or that opens
// files without caring what the path resolves to, writes the whole
// subtree wherever the link points.
func TestRestoreRefusesToWriteThroughASymlinkInTheDestination(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep := hostileRepository(t)

	id, ok := plantTree(t, rep, "sub", "inner.txt", []byte("subtree content"))
	if !ok {
		t.Fatal("the uploader would not store an ordinary nested tree")
	}

	root := t.TempDir()
	dest := filepath.Join(root, "destination")
	elsewhere := filepath.Join(root, "elsewhere")

	if err := os.MkdirAll(dest, 0o750); err != nil {
		t.Fatalf("creating the destination: %v", err)
	}

	if err := os.MkdirAll(elsewhere, 0o750); err != nil {
		t.Fatalf("creating the directory the link points at: %v", err)
	}

	if err := os.Symlink(elsewhere, filepath.Join(dest, "sub")); err != nil {
		t.Fatalf("planting the symbolic link: %v", err)
	}

	_, err := rep.Restore(ctx, id, backupengine.RestoreRequest{
		TargetPath: dest,
		SkipOwners: true,
		Conflict:   backupengine.ConflictOverwrite,
	})

	if !errors.Is(err, backupengine.ErrUnsafeSnapshotPath) {
		t.Errorf("restoring a directory over a symbolic link returned %v; want ErrUnsafeSnapshotPath", err)
	}

	if entries, readErr := os.ReadDir(elsewhere); readErr != nil || len(entries) != 0 {
		t.Errorf("the restore wrote %d entries through the symbolic link into %s (%v)", len(entries), elsewhere, readErr)
	}
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

		root := t.TempDir()
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

// plantCanaries writes files beside the restore destination whose
// survival, byte for byte, is what "nothing escaped" means.
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

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading the directory the destination lives in: %v", err)
	}

	for _, e := range entries {
		switch filepath.Join(root, e.Name()) {
		case dest:
		default:
			if _, ok := canaries[filepath.Join(root, e.Name())]; !ok {
				t.Errorf("the restore produced %q beside its destination", e.Name())
			}
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
