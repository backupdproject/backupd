package kopia_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/backupengine/kopia"
	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/secretref"
)

// This file is the adversarial half of the tree port's acceptance suite:
// what happens when the SOURCE is hostile rather than merely broken.
//
// backupengine/source already proves that its own path sanitiser cannot
// be talked out of the root (FuzzSafeRelPathCannotEscapeTheRoot), and
// kopia/tree_test.go already proves that a handful of named entry names
// are refused. Neither of those is the property an operator cares about,
// because both stop one layer short of the bytes: a name that the
// sanitiser accepted, or that the refusal table did not think of, is
// still only safe if it cannot relocate anything once the REAL engine has
// stored it and the REAL restore path has written it back out. That end
// to end claim is what this file makes, and it makes it by restoring into
// a directory surrounded by canaries and then accounting for every single
// file on disk afterwards.
//
// The second half is the #782/#793 mutation verdict seen from the engine's
// side of the boundary. source/tree_test.go owns the question "did the
// source notice the tear"; this file owns the question that decides
// whether noticing mattered, which is whether a tear the source reported
// can still end up as a manifest somebody will restore from.

// --- the arena ---------------------------------------------------------

// restoreArena is a restore root with canaries around it.
//
// The layout is the whole point. A traversal attack does not need to
// reach /etc to be a catastrophe -- landing one directory up is enough to
// overwrite a sibling restore, or the operator's own files if the target
// was chosen inside their home. So the root sits two levels down inside a
// temp directory whose every other file is known, and the assertion after
// a restore is an exact accounting of that directory rather than a check
// that some particular escape did not happen. A name nobody thought of
// that lands anywhere except inside the root shows up as an unexpected
// path, which is the only form of this assertion that covers the attack
// that has not been invented yet.
type restoreArena struct {
	// base is the directory the accounting covers.
	base string

	// target is the restore root: base/enclosing/restored.
	target string
}

// arenaSkeleton is every path restoreArena creates, relative to base and
// in slash form. Anything else found under base after a restore was put
// there by the restore.
var arenaSkeleton = []string{
	"canary.txt",
	"enclosing",
	"enclosing/canary.txt",
	"enclosing/restored",
	"outside",
	"outside/passwd",
}

// canaryBody is what every canary file holds. A traversal that overwrote
// one changes its bytes, and a traversal that merely truncated it changes
// them too, so comparing content catches both.
const canaryBody = "a file the restore had no business touching\n"

// newRestoreArena builds the layout above under a fresh temp directory.
//
// The canary named "passwd" is not decoration: the seeds below aim at
// "../../outside/passwd" precisely because a target that exists is the
// case where an escape succeeds silently instead of failing on a missing
// parent directory.
func newRestoreArena(tb testing.TB) restoreArena {
	tb.Helper()

	base := tb.TempDir()

	a := restoreArena{
		base:   base,
		target: filepath.Join(base, "enclosing", "restored"),
	}

	for _, dir := range []string{
		filepath.Join(base, "outside"),
		filepath.Join(base, "enclosing", "restored"),
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			tb.Fatalf("building the restore arena: %v", err)
		}
	}

	for _, name := range []string{
		filepath.Join(base, "canary.txt"),
		filepath.Join(base, "outside", "passwd"),
		filepath.Join(base, "enclosing", "canary.txt"),
	} {
		if err := os.WriteFile(name, []byte(canaryBody), 0o600); err != nil {
			tb.Fatalf("writing a canary: %v", err)
		}
	}

	return a
}

// accountForEverything is the escape assertion.
//
// wantInside is what the restore was entitled to write, relative to the
// restore root and in slash form. Everything else under base has to be
// exactly the skeleton, byte for byte, and every path the restore did
// write has to resolve to somewhere inside the root -- which is checked
// on the CLEANED ABSOLUTE path, through EvalSymlinks, because a "../.."
// that the filesystem happily followed and a symlink the restore planted
// and then wrote through are the same escape arriving by different
// routes.
func (a restoreArena) accountForEverything(tb testing.TB, wantInside []string) {
	tb.Helper()

	// The temp directory is itself behind a symlink on macOS (/var ->
	// /private/var), so both sides of every containment comparison are
	// resolved or neither is.
	base, err := filepath.EvalSymlinks(a.base)
	if err != nil {
		tb.Fatalf("resolving the arena base: %v", err)
	}

	target, err := filepath.EvalSymlinks(a.target)
	if err != nil {
		tb.Fatalf("resolving the restore root: %v", err)
	}

	var found []string

	if err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if p == base {
			return nil
		}

		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}

		found = append(found, filepath.ToSlash(rel))

		return nil
	}); err != nil {
		tb.Fatalf("accounting for the restore arena: %v", err)
	}

	// Every canary still holds its bytes. Checked by exact path rather
	// than by name, because a restore is entitled to write a file
	// CALLED canary.txt inside its own root and that is not an escape.
	for _, rel := range []string{"canary.txt", "enclosing/canary.txt", "outside/passwd"} {
		body, rerr := os.ReadFile(filepath.Join(base, filepath.FromSlash(rel))) //nolint:gosec // the path is this test's own temp directory.
		if rerr != nil {
			tb.Errorf("the canary %s is gone: %v", rel, rerr)
		} else if string(body) != canaryBody {
			tb.Errorf("the canary %s was rewritten by the restore; something reached outside the restore root", rel)
		}
	}

	want := append([]string{}, arenaSkeleton...)

	targetRel, err := filepath.Rel(base, target)
	if err != nil {
		tb.Fatalf("locating the restore root under the arena base: %v", err)
	}

	for _, inside := range wantInside {
		want = append(want, filepath.ToSlash(filepath.Join(targetRel, inside)))
	}

	sort.Strings(want)
	sort.Strings(found)

	if strings.Join(want, "\n") != strings.Join(found, "\n") {
		tb.Fatalf("the restore arena holds\n  %s\nand should hold\n  %s",
			strings.Join(found, "\n  "), strings.Join(want, "\n  "))
	}

	// Said again, in the language of the attack: every path that is not
	// part of the skeleton resolves to somewhere under the restore root.
	skeleton := map[string]bool{}
	for _, s := range arenaSkeleton {
		skeleton[s] = true
	}

	for _, rel := range found {
		if skeleton[rel] {
			continue
		}

		abs := filepath.Clean(filepath.Join(base, filepath.FromSlash(rel)))
		if abs != target && !strings.HasPrefix(abs, target+string(os.PathSeparator)) {
			tb.Errorf("the restore wrote %s, which is not under the restore root %s", abs, target)
		}
	}
}

// restoredInside lists what a restore actually wrote, relative to the
// restore root, so that a test can compare it against what the snapshot
// claimed.
func (a restoreArena) restoredInside(tb testing.TB) []string {
	tb.Helper()

	var out []string

	if err := filepath.WalkDir(a.target, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if p == a.target {
			return nil
		}

		rel, err := filepath.Rel(a.target, p)
		if err != nil {
			return err
		}

		out = append(out, filepath.ToSlash(rel))

		return nil
	}); err != nil {
		tb.Fatalf("listing the restored tree: %v", err)
	}

	sort.Strings(out)

	return out
}

// --- a repository a fuzz target can own ---------------------------------

// adversarialRepository is newTreeRepository for a testing.TB.
//
// It exists only because a fuzz target is handed a *testing.F and the
// helpers in repository_test.go take a *testing.T, and because building
// the repository ONCE per fuzz target rather than once per iteration is
// the difference between a fuzz run that explores a few hundred names and
// one that spends every millisecond creating format blobs.
func adversarialRepository(tb testing.TB) backupengine.TreeRepository {
	tb.Helper()

	ctx := context.Background()

	domain, err := model.NewRepositoryDomainID("adversarial")
	if err != nil {
		tb.Fatalf("NewRepositoryDomainID: %v", err)
	}

	passphrase := filepath.Join(tb.TempDir(), "passphrase")
	if err := os.WriteFile(passphrase, []byte(testPassphrase+"\n"), 0o600); err != nil {
		tb.Fatalf("writing the passphrase file: %v", err)
	}

	loc := backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     domain,
		Root:       tb.TempDir(),
		StateDir:   filepath.Join(tb.TempDir(), "state"),
		Passphrase: secretref.Ref{File: passphrase},
	}

	eng := kopia.New()

	if err := eng.CreateRepository(ctx, loc); err != nil {
		tb.Fatalf("CreateRepository: %v", err)
	}

	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		tb.Fatalf("OpenRepository: %v", err)
	}

	tb.Cleanup(func() {
		if err := rep.Close(context.Background()); err != nil {
			tb.Errorf("Close: %v", err)
		}
	})

	tree, ok := rep.(backupengine.TreeRepository)
	if !ok {
		tb.Fatalf("%T does not implement backupengine.TreeRepository", rep)
	}

	return tree
}

// oneEntryTree is a source whose whole content is a single file with the
// name under test, which is the smallest tree that can carry an attack.
func oneEntryTree(name string, body []byte) *memDir {
	return &memDir{entries: []backupengine.SourceEntry{
		fileEntry(name, newMemStream(body)),
	}}
}

// --- the named names ----------------------------------------------------

// namesThatChooseWhereTheyLand are entry names whose acceptance would let
// the SOURCE decide where in the snapshot its content goes, plus the one
// name that decides instead that the snapshot can never be restored at
// all.
var namesThatChooseWhereTheyLand = map[string]string{
	"the parent directory":         "..",
	"the current directory":        ".",
	"a traversal":                  "../escape",
	"a nested path":                "a/b",
	"an absolute path":             "/etc/passwd",
	"a traversal in the middle":    "etc/../../passwd",
	"a doubled-up traversal":       "....//....//etc",
	"an empty name":                "",
	"a trailing separator":         "dir/",
	"a leading separator":          "/name",
	"a traversal that cleans back": "a/../../b",

	// Not a traversal, and refused for the same underlying reason: a
	// name no filesystem can create stores perfectly and restores never,
	// so accepting it would advertise a restore point that cannot exist.
	"a name with a NUL byte": "nul\x00byte",
}

// namesThatOnlyLOOKLikePaths are hostile spellings that are nonetheless
// ONE legal path element on the platforms this product runs on.
//
// They are accepted on purpose, and the contract they get is stricter
// than a refusal rather than weaker: stored verbatim, restored verbatim,
// and confined to the restore root. Refusing them instead would be the
// worse bug of the two, because a backup that silently drops every file
// whose name contains a backslash is a backup nobody can rely on, and a
// backup that RENAMES one into something harmless is a restore that puts
// the wrong bytes at the right path.
var namesThatOnlyLOOKLikePaths = map[string]string{
	"a windows traversal":    `..\..`,
	"an encoded traversal":   "..%2f..",
	"a backslash in a name":  `back\slash`,
	"three dots":             "...",
	"a leading double dot":   "..hidden",
	"a trailing double dot":  "hidden..",
	"a newline in a name":    "two\nlines",
	"a tilde":                "~",
	"a dash":                 "-",
	"an encoded nested path": "a%2fb",
}

// TestSnapshotTreeRefusesEveryEntryNameThatChoosesWhereItLands is the
// path-traversal row of #784, proved through the real engine rather than
// against the name checker on its own.
//
// The split between the two tables IS the contract, and getting it wrong
// in either direction is a real failure: a name that can relocate content
// must be refused with a sentence naming it, and a name that merely looks
// dangerous must survive a round trip unchanged and inside the root. A
// denylist that refused the second group would quietly drop real files; a
// checker that accepted the first would let a source's directory listing
// write wherever it liked.
func TestSnapshotTreeRefusesEveryEntryNameThatChoosesWhereItLands(t *testing.T) {
	t.Parallel()

	t.Run("refused", func(t *testing.T) {
		t.Parallel()

		for label, name := range namesThatChooseWhereTheyLand {
			t.Run(label, func(t *testing.T) {
				t.Parallel()

				ctx := context.Background()
				rep := newTreeRepository(t)
				src := treeSource("/sets/hostile-names")

				_, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{
					Source: src,
					RunID:  treeRunID,
					Root:   oneEntryTree(name, []byte("payload\n")),
				})
				if err == nil {
					t.Fatalf("the entry name %q was stored instead of refused", name)
				}

				// The refusal has to name what was wrong, because the
				// operator reading it owns the source that produced the
				// name and has no other way to find it. An empty name
				// has nothing to quote, so that row asks for the word
				// instead, and a name holding a byte no terminal can
				// print is named by its quoted form -- which is the
				// form an operator can actually read.
				want := name

				switch {
				case name == "":
					want = "name"
				case strings.ContainsRune(name, '\x00'):
					// A refusal renders the name with %q, which is the
					// only readable form of a name holding a byte no
					// terminal can print.
					want = strings.Trim(strconv.Quote(name), `"`)
				}

				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %q, so nothing in it says which entry to go and fix", err, want)
				}

				assertNoSnapshots(t, ctx, rep, src)
			})
		}
	})

	t.Run("confined", func(t *testing.T) {
		t.Parallel()

		for label, name := range namesThatOnlyLOOKLikePaths {
			t.Run(label, func(t *testing.T) {
				t.Parallel()

				ctx := context.Background()
				rep := newTreeRepository(t)
				src := treeSource("/sets/hostile-names")
				body := []byte("payload for " + name + "\n")

				info, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{
					Source: src,
					RunID:  treeRunID,
					Root:   oneEntryTree(name, body),
				})
				if err != nil {
					t.Fatalf("the entry name %q is one legal path element and was refused anyway: %v", name, err)
				}

				arena := newRestoreArena(t)

				if _, err := rep.Restore(ctx, info.ID, backupengine.RestoreRequest{
					TargetPath: arena.target,
					SkipOwners: true,
				}); err != nil {
					t.Fatalf("Restore: %v", err)
				}

				arena.accountForEverything(t, []string{name})

				got, err := os.ReadFile(filepath.Join(arena.target, name)) //nolint:gosec // the path is this test's own temp directory.
				if err != nil {
					t.Fatalf("the restored entry is not at the name the source gave it: %v", err)
				}

				if !bytes.Equal(got, body) {
					t.Errorf("%q restored with different bytes: got %q, want %q", name, got, body)
				}
			})
		}
	})
}

// --- the property, over arbitrary names ---------------------------------

// fuzzSetCounter keeps every iteration's snapshot in its own backup set.
//
// One repository serves the whole fuzz target, so two iterations sharing
// a source identity would be two snapshots of the same set and the second
// one's accounting would be reading the first one's manifest.
var fuzzSetCounter atomic.Int64

// FuzzSnapshotAndRestoreCannotEscapeTheRestoreRoot is the property the
// named tables above are only examples of.
//
// Whatever an arbitrary entry name does to the engine, exactly one of
// three things happens, and all three are safe:
//
//   - the snapshot is refused, and nothing was stored;
//   - the snapshot is stored and the restore cannot express the name on
//     this filesystem, in which case the restore fails and still writes
//     nothing outside its root -- a name the local filesystem rejects is
//     the kernel refusing, and the property under test is escape, not
//     expressiveness;
//   - the snapshot is stored and restored, in which case the restore
//     root holds exactly the one entry the snapshot claimed, under the
//     name the source gave it, with the bytes the source gave it.
//
// In none of them does anything outside the restore root get created,
// truncated or rewritten, which is what the arena's accounting checks.
func FuzzSnapshotAndRestoreCannotEscapeTheRestoreRoot(f *testing.F) {
	// The seeds are the union of the two tables above and the corpus
	// FuzzSafeRelPathCannotEscapeTheRoot uses, because a name the
	// sanitiser one layer up already reasons about is exactly the name
	// this layer must not disagree with.
	seeds := []string{
		"runs/2026/db.dump", "../etc/shadow", "/etc/shadow", "..", ".",
		"a/../../b", "C:/x", `x\y`, "x\x00y", "\xff", "", "///",
		"a/./b", "a//b/", "....//....//etc", "AAA/../..",
		"../../outside/passwd", "../canary.txt", "../../canary.txt",
		`..\..`, "..%2f..", `back\slash`, "...", "..hidden", "~",
		strings.Repeat("a", 512),
	}

	for _, name := range namesThatChooseWhereTheyLand {
		seeds = append(seeds, name)
	}

	for _, name := range namesThatOnlyLOOKLikePaths {
		seeds = append(seeds, name)
	}

	for _, seed := range seeds {
		f.Add(seed)
	}

	// One repository for the whole target: opening one loads format
	// blobs and builds an index cache, which is far more work than the
	// snapshot each iteration performs.
	rep := adversarialRepository(f)

	f.Fuzz(func(t *testing.T, name string) {
		ctx := context.Background()

		// One tiny file per iteration, so the cost of an iteration is
		// the name under test and not the bytes behind it.
		body := []byte("payload\n")
		src := treeSource(fmt.Sprintf("/sets/fuzz/%d", fuzzSetCounter.Add(1)))

		info, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{
			Source: src,
			RunID:  treeRunID,
			Root:   oneEntryTree(name, body),
		})
		if err != nil {
			// Refused. Nothing was stored, so there is nothing that
			// could later be restored anywhere.
			if snaps, lerr := rep.ListSnapshots(ctx, src); lerr != nil {
				t.Fatalf("ListSnapshots: %v", lerr)
			} else if len(snaps) != 0 {
				t.Fatalf("the entry name %q was refused and left %d snapshot(s) behind", name, len(snaps))
			}

			return
		}

		arena := newRestoreArena(t)

		_, rerr := rep.Restore(ctx, info.ID, backupengine.RestoreRequest{
			TargetPath: arena.target,
			SkipOwners: true,
		})
		if rerr != nil {
			// The local filesystem would not express the name. What
			// matters is not that the restore failed but that its
			// failure stayed inside the root: a partially written
			// entry, or the atomic-write temporary beside it, is
			// allowed there and nowhere else.
			arena.accountForEverything(t, arena.restoredInside(t))

			return
		}

		// The accounting is what proves nothing escaped: anything the
		// restore put outside its root turns up under base and is not
		// in the expected set, whatever it was called.
		inside := arena.restoredInside(t)

		arena.accountForEverything(t, inside)

		if len(inside) != 1 {
			t.Fatalf("the snapshot of the entry name %q restored %d entries (%q); one entry in was supposed to be one entry out", name, len(inside), inside)
		}

		// Renaming is checked only for a name this filesystem can
		// express, and that is not a loophole. macOS refuses to store a
		// filename that is not valid UTF-8 verbatim and substitutes its
		// own spelling, so demanding byte equality here would make this
		// target assert the kernel's encoding rules rather than the
		// engine's promise. The promise -- that a hostile name is never
		// quietly turned into a harmless one -- is asserted on chosen
		// names in TestSnapshotTreeRefusesEveryEntryNameThatChoosesWhereItLands.
		if utf8.ValidString(name) && len(name) <= 255 && inside[0] != name {
			t.Errorf("the entry name %q came back as %q; the engine may refuse a name but may never rename one", name, inside[0])
		}

		got, err := os.ReadFile(filepath.Join(arena.target, filepath.FromSlash(inside[0]))) //nolint:gosec // the path is this test's own temp directory.
		if err != nil {
			t.Fatalf("reading the restored entry for %q: %v", name, err)
		}

		if !bytes.Equal(got, body) {
			t.Errorf("%q restored with different bytes: got %q, want %q", name, got, body)
		}
	})
}

// --- the symlink row ----------------------------------------------------

// TestRestoreWritesAPreservedLinkAsDataAndFollowsNothing is the
// symlink-escape row of #784.
//
// backupengine/source preserves a symbolic link by reading its TARGET
// STRING as the object's content, and engine.go's SourceEntry doc says
// nothing in this boundary follows a link. That makes the dangerous case
// the one where a target string gets treated as an instruction somewhere
// downstream: a link recorded as pointing at /etc/passwd, restored as a
// real symlink, turns the next verification pass that reads the restored
// tree into a read of the host's own files, and a link pointing at
// ../../outside/passwd turns a restore into a write outside its root.
//
// So the property is that the target string stays DATA all the way
// through: what comes back is a regular file holding the target verbatim,
// nothing outside the restore root is created or altered, and the restore
// root holds nothing but the entries the snapshot claimed.
func TestRestoreWritesAPreservedLinkAsDataAndFollowsNothing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep := newTreeRepository(t)
	src := treeSource("/sets/links")

	// Both of the targets an escape would use: one absolute, at a file
	// that really exists on this machine, and one relative, aimed at a
	// canary the arena really creates.
	links := map[string]string{
		"absolute.link": "/etc/passwd",
		"relative.link": "../../outside/passwd",
	}

	root := &memDir{entries: []backupengine.SourceEntry{
		fileEntry("absolute.link", newMemStream([]byte(links["absolute.link"]))),
		fileEntry("relative.link", newMemStream([]byte(links["relative.link"]))),
	}}

	info, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, RunID: treeRunID, Root: root})
	if err != nil {
		t.Fatalf("SnapshotTree: %v", err)
	}

	arena := newRestoreArena(t)

	if _, err := rep.Restore(ctx, info.ID, backupengine.RestoreRequest{
		TargetPath: arena.target,
		SkipOwners: true,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	arena.accountForEverything(t, []string{"absolute.link", "relative.link"})

	for name, target := range links {
		path := filepath.Join(arena.target, name)

		// Lstat, not Stat: the question is what was WRITTEN, and Stat
		// would answer it by following exactly the link this test
		// exists to prove was not created.
		st, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("Lstat %s: %v", name, err)
		}

		if st.Mode()&os.ModeSymlink != 0 {
			resolved, _ := os.Readlink(path)

			t.Errorf("%s came back as a symbolic link to %q; a preserved link's target is data this port records, never a path it recreates", name, resolved)

			continue
		}

		body, err := os.ReadFile(path) //nolint:gosec // the path is this test's own temp directory.
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}

		if string(body) != target {
			t.Errorf("%s holds %q, want the link target %q recorded verbatim", name, body, target)
		}
	}
}

// --- the mutation row ---------------------------------------------------

// errSourceMoved is the verdict a source's own post-read consistency
// check reports. It stands in for source.ErrSourceMutated, which this
// package may not import: backupengine/source is a CALLER of this port,
// so an adapter test that imported it would be testing a cycle the
// production code does not have.
var errSourceMoved = errors.New("the object moved under the read and nothing here holds a proven copy of it")

// postReadDir is a source directory that reports a verdict when its pass
// is RELEASED rather than while it is being served.
//
// That is where a real source reports one. backupengine/source reads a
// stream once, forward, so a tear is only detectable after the bytes have
// already gone past -- it compares the post-read stat against the
// pre-read stat and the byte count the read actually produced -- and by
// then the engine has finished with the entry. fs.DirectoryIterator.Close
// returns nothing, so this verdict has nowhere to go through the vendor's
// own signatures, and the last moment it can still stop a manifest is
// inside the write session. A tear reported here and dropped is the one
// failure that produces a snapshot which claims to be complete and is
// not.
type postReadDir struct {
	entries []backupengine.SourceEntry

	// closeErr is the post-read verdict, reported when the pass is
	// released.
	closeErr error
}

func (d *postReadDir) Open(context.Context) (backupengine.SourceDirIterator, error) {
	return &postReadIterator{dir: d}, nil
}

type postReadIterator struct {
	dir  *postReadDir
	next int
}

func (it *postReadIterator) Next(context.Context) (backupengine.SourceEntry, bool, error) {
	it.next++

	if it.next > len(it.dir.entries) {
		return backupengine.SourceEntry{}, false, nil
	}

	return it.dir.entries[it.next-1], true, nil
}

func (it *postReadIterator) Close() error { return it.dir.closeErr }

// driftingStream is an object whose content is different every time it is
// opened.
//
// A stream that changed once could be laundered by a retry; this one
// cannot, which is what makes the outcome below a property of the run
// rather than of how many attempts it made.
type driftingStream struct {
	revisions [][]byte

	opens atomic.Int64
}

func (s *driftingStream) ModTime() time.Time { return time.Unix(1700000000, 0).UTC() }

func (s *driftingStream) Open(context.Context) (io.ReadCloser, error) {
	n := int(s.opens.Add(1)) - 1
	if n >= len(s.revisions) {
		n = len(s.revisions) - 1
	}

	return io.NopCloser(bytes.NewReader(s.revisions[n])), nil
}

// claimedSizeEntry is a listing that states a size the stream underneath
// does not honour, which is what a live remote directory looks like when
// somebody is writing to it.
func claimedSizeEntry(name string, size int64, src backupengine.StreamSource) backupengine.SourceEntry {
	return backupengine.SourceEntry{
		Name:    name,
		Size:    size,
		ModTime: time.Unix(1700000000, 0).UTC(),
		Stream:  src,
	}
}

// TestASourceThatMutatesUnderTheReadIsDeterministic is the #782/#793 row
// at the ENGINE boundary.
//
// source/tree_test.go already owns the verdict: a file that moves under
// its own read fails the run, because there is nothing per-object to
// discard out of a set-wide snapshot. What that leaves unproven is the
// half that decides whether the verdict matters -- whether a tear the
// source reported can still reach a manifest. The answer has to be no on
// every route the verdict can arrive by, and it has to be the SAME answer
// twice, because a fail-closed rule that only holds on a repository's
// first run is a rule that stops holding the moment an operator retries.
//
// So each row runs twice against one repository and asserts the identical
// outcome, and then asserts the thing an operator would check: the
// repository advertises no restore point at all.
func TestASourceThatMutatesUnderTheReadIsDeterministic(t *testing.T) {
	t.Parallel()

	settled := []byte(strings.Repeat("settled content, 64 bytes worth", 100))

	cases := map[string]struct {
		root backupengine.SourceDir
		want error
	}{
		// The size changed. The engine can see this one -- it read a
		// different number of bytes than the listing promised -- and it
		// must still not decide for itself that a shorter read is fine.
		"the object shrank under the read": {
			root: &postReadDir{
				closeErr: errSourceMoved,
				entries: []backupengine.SourceEntry{
					claimedSizeEntry("moving.bin", int64(len(settled)), &driftingStream{
						revisions: [][]byte{settled[:len(settled)/2]},
					}),
				},
			},
			want: errSourceMoved,
		},

		// The size did not change and the bytes did. This is the row
		// that cannot be caught anywhere except at the source, so the
		// engine's only correct behaviour is to carry somebody else's
		// verdict -- and an adapter that dropped the iterator's Close
		// error would store this one as a complete snapshot of a file
		// nobody ever held a coherent copy of.
		"the object changed at a stable size": {
			root: &postReadDir{
				closeErr: errSourceMoved,
				entries: []backupengine.SourceEntry{
					claimedSizeEntry("rewritten.bin", int64(len(settled)), &driftingStream{
						revisions: [][]byte{
							bytes.Repeat([]byte("a"), len(settled)),
							bytes.Repeat([]byte("b"), len(settled)),
						},
					}),
				},
			},
			want: errSourceMoved,
		},

		// The read itself gave up partway. The bytes that did arrive
		// are a prefix of a file, which is the most plausible-looking
		// hole a backup can acquire.
		"the read failed partway": {
			root: func() backupengine.SourceDir {
				s := newMemStream(settled)
				s.breakAfter = len(settled) / 3

				return &postReadDir{entries: []backupengine.SourceEntry{fileEntry("broken.bin", s)}}
			}(),
			want: errStreamBroke,
		},

		// A tear reported after the last entry was already served and
		// accepted. Nothing in the walk failed, so this is the run that
		// a careless adapter completes.
		"the verdict arrives after the walk succeeded": {
			root: &postReadDir{
				closeErr: errSourceMoved,
				entries: []backupengine.SourceEntry{
					fileEntry("clean.bin", newMemStream(settled)),
				},
			},
			want: errSourceMoved,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			rep := newTreeRepository(t)
			src := treeSource("/sets/moving")

			for attempt := 1; attempt <= 2; attempt++ {
				_, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{
					Source: src,
					RunID:  treeRunID,
					Root:   tc.root,
				})
				if err == nil {
					t.Fatalf("attempt %d stored a snapshot of a source that was not holding still", attempt)
				}

				if !errors.Is(err, tc.want) {
					t.Fatalf("attempt %d failed with %v, which does not carry the source's own verdict (%v)", attempt, err, tc.want)
				}

				assertNoSnapshots(t, ctx, rep, src)
			}
		})
	}
}

// TestSnapshotTreeStoresTheBytesItReadAndNotTheSizeTheListingClaimed is
// the other half of the mutation contract, and the half that is easy to
// get wrong in the safe-looking direction.
//
// A listing's size is advisory: it comes from a live directory somebody
// else is writing to, and tree.go's treeFileEntry.Size says so. An
// adapter that trusted it would either truncate a longer read or pad a
// shorter one to match, and both produce a snapshot whose content is
// something no source ever held -- a torn file that every later
// verification will call healthy, because its length agrees with the
// manifest.
//
// So when nothing reports a tear, the bytes stored are exactly the bytes
// that came off the stream, whichever way the listing was wrong.
func TestSnapshotTreeStoresTheBytesItReadAndNotTheSizeTheListingClaimed(t *testing.T) {
	t.Parallel()

	body := []byte(strings.Repeat("bytes that actually arrived", 200))

	for label, claimed := range map[string]int64{
		"the listing claimed more than the stream produced": int64(len(body)) * 4,
		"the listing claimed less than the stream produced": 7,
		"the listing claimed nothing at all":                0,
	} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			rep := newTreeRepository(t)
			src := treeSource("/sets/advisory-size")

			root := &memDir{entries: []backupengine.SourceEntry{
				claimedSizeEntry("object.bin", claimed, &driftingStream{revisions: [][]byte{body}}),
			}}

			info, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{Source: src, RunID: treeRunID, Root: root})
			if err != nil {
				t.Fatalf("SnapshotTree: %v", err)
			}

			if info.SourceBytesRead != int64(len(body)) {
				t.Errorf("the run reports %d bytes read off the source, want the %d the stream produced", info.SourceBytesRead, len(body))
			}

			arena := newRestoreArena(t)

			if _, err := rep.Restore(ctx, info.ID, backupengine.RestoreRequest{
				TargetPath: arena.target,
				SkipOwners: true,
			}); err != nil {
				t.Fatalf("Restore: %v", err)
			}

			arena.accountForEverything(t, []string{"object.bin"})

			got, err := os.ReadFile(filepath.Join(arena.target, "object.bin")) //nolint:gosec // the path is this test's own temp directory.
			if err != nil {
				t.Fatalf("reading the restored object: %v", err)
			}

			if sum(got) != sum(body) {
				t.Errorf("the restored object is %d bytes with SHA-256 %s, want the %d bytes the stream produced (%s)",
					len(got), sum(got), len(body), sum(body))
			}
		})
	}
}
