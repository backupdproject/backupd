package kopia

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/snapshot/snapshotfs"

	"github.com/backupdproject/backupd/core/internal/backupengine"
)

// This file is the restore path: a stored snapshot, or one directory or
// file inside it, written onto a local filesystem.
//
// # Why it is not the vendor's restore.Entry
//
// It was, and the reason it is not any more is the threat model. The
// vendor's FilesystemOutput joins each entry's name onto the target and
// writes it. That is correct for a repository you wrote yourself with a
// program that validated every name on the way in, and this product
// cannot assume either half: a repository domain may be shared, an
// operator may have written to the same bucket with the vendor's own
// CLI, and the names in a snapshot came off somebody else's filesystem
// where whoever can create a file chooses what it is called. A name like
// "../../etc/cron.d/x" stored once is a write outside the restore
// destination every time anybody restores it, on a path an operator
// chose precisely because it was safe to unpack into.
//
// So every entry name is validated here by checkEntryName (tree.go) --
// literally the same function the snapshot WRITE path refuses names with,
// which is #782's rule about a name being one path element and nothing
// else -- and the path this file then assembles is checked for
// containment against the canonical destination. Two checks for one
// property, because they fail differently and catch different inputs: the
// first refuses a NAME, the second refuses a PATH, including the names
// that are one legal element here and a traversal on a platform whose
// separator differs.
//
// There are three more things this path does that the vendor's does not,
// and each is a requirement rather than a preference:
//
//   - it restores a SUBTREE or a single file, not only a whole snapshot;
//   - it never writes a partially-written file under its real name, so a
//     cancelled restore leaves a smaller tree rather than a plausible
//     wrong one;
//   - it can read back what it wrote and compare it with what the
//     repository handed over, because a restore's own statistics are the
//     one thing a broken restore path reports correctly.

// restoreWorkPrefix names the file a restore writes into before it is
// renamed to the entry's real name.
//
// The leading dot is not decoration: it keeps the working file out of the
// way of ordinary globs while it exists, and it is the marker the
// cancellation test asserts on. The rename is what publishes the file, so
// nothing under the destination ever carries a real name and partial
// content.
const restoreWorkPrefix = ".backupd-restore-"

// restoreCopyChunk is how many bytes one copy step moves before the
// extractor looks at the context again.
//
// It bounds how long a cancellation waits, which for a restore of a large
// file is the difference between stopping now and stopping when the file
// ends. A megabyte is small enough that the wait is imperceptible and
// large enough that the check costs nothing measurable per gigabyte.
const restoreCopyChunk = 1 << 20

// restoreDirWorkMode is the mode a directory is created with while its
// children are being written.
//
// A snapshot may hold a directory that is not writable by its owner, and
// creating it with that mode straight away would make its own contents
// unrestorable. The snapshot's mode is applied after the subtree is
// complete, which is the same order the vendor's restore uses and for the
// same reason.
const restoreDirWorkMode os.FileMode = 0o700

// errLinkTimesUnsupported is what lchtimes reports on a platform with no
// way to set a symbolic link's own times (restoretimes_other.go). It is
// not a restore failure: the link is restored, its timestamp is the
// restore's, and the restore says so here rather than pretending it set
// something it did not.
var errLinkTimesUnsupported = errors.New("this platform cannot set a symbolic link's own timestamps")

// Restore implements backupengine.Repository.
//
// The report and the error carry different halves of the answer and both
// have to be read. A restore stopped by a cancellation or a conflict
// returns what it had written by then -- those files really are there --
// and RestoreReport.Complete stays false, which is the only value that
// says whether the tree on the disk is the tree that was asked for.
func (r *repository) Restore(ctx context.Context, id backupengine.SnapshotID, req backupengine.RestoreRequest) (backupengine.RestoreReport, error) {
	policy, err := backupengine.ParseRestoreConflict(string(req.Conflict))
	if err != nil {
		return backupengine.RestoreReport{}, err
	}

	if strings.TrimSpace(req.TargetPath) == "" {
		return backupengine.RestoreReport{}, backupengine.ErrNoRestoreDestination
	}

	man, err := r.load(ctx, id)
	if err != nil {
		return backupengine.RestoreReport{}, err
	}

	root, err := snapshotfs.SnapshotRoot(r.rep, man)
	if err != nil {
		return backupengine.RestoreReport{}, fmt.Errorf("resolving snapshot root: %w", err)
	}

	selected, err := selectRestoreEntry(ctx, root, req.SourcePath)
	if err != nil {
		return backupengine.RestoreReport{}, err
	}

	target, err := filepath.Abs(req.TargetPath)
	if err != nil {
		return backupengine.RestoreReport{}, fmt.Errorf("resolving restore target %s: %w", req.TargetPath, err)
	}

	// Whether the destination already existed is asked BEFORE it is
	// created, because it decides whether the snapshot root's own mode
	// and modification time are applied to it. A directory an operator
	// prepared is theirs; a restore that rewrote its permissions would
	// be changing something nobody asked about. MkdirAll cannot tell the
	// two apart, so the question is asked separately.
	_, statErr := os.Stat(target)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return backupengine.RestoreReport{}, fmt.Errorf("inspecting restore target %s: %w", target, statErr)
	}

	if err := os.MkdirAll(target, 0o750); err != nil {
		return backupengine.RestoreReport{}, fmt.Errorf("creating restore target %s: %w", target, err)
	}

	// The containment check below compares paths, so it has to compare
	// the ones the kernel will actually resolve. A destination reached
	// through a symbolic link -- /var on macOS, /home on plenty of Linux
	// deployments -- is a different string from the directory it names,
	// and a prefix test against the un-resolved one is a test that passes
	// for the wrong reason and fails for the right one.
	canonical, err := filepath.EvalSymlinks(target)
	if err != nil {
		return backupengine.RestoreReport{}, fmt.Errorf("resolving restore target %s: %w", target, err)
	}

	x := newExtractor(canonical, policy, req, errors.Is(statErr, os.ErrNotExist))

	if err := x.extract(ctx, selected); err != nil {
		return x.report(false), err
	}

	return x.report(true), nil
}

// selectRestoreEntry resolves what a request asked for inside a snapshot:
// the whole thing, or one entry named by a slash path.
//
// The selection is untrusted input too -- it arrives from an API request
// -- so each of its elements goes through the same rule a stored entry
// name does, one element at a time. Per element rather than as a whole
// path, because that is what the selection actually addresses: a stored
// entry may legitimately be called `back\slash`, and a whole-path
// sanitiser that refused such a spelling would make a file this product
// backs up on purpose unnameable in a restore request.
func selectRestoreEntry(ctx context.Context, root fs.Entry, selection string) (fs.Entry, error) {
	// A leading or trailing slash is spelling rather than traversal:
	// "sub/", "/sub" and "sub" all name the same entry, and reporting
	// the first two as an unsafe path would refuse a request whose only
	// fault is how a surface joined its own strings together.
	selection = strings.Trim(selection, "/")

	if selection == "" {
		return root, nil
	}

	entry := root

	for _, name := range strings.Split(selection, "/") {
		if _, err := safeEntryName(name); err != nil {
			return nil, err
		}

		dir, ok := entry.(fs.Directory)
		if !ok {
			return nil, fmt.Errorf("%w: %q, because %q is not a directory",
				backupengine.ErrRestorePathNotFound, selection, entry.Name())
		}

		child, err := dir.Child(ctx, name)
		if err != nil {
			if errors.Is(err, fs.ErrEntryNotFound) {
				return nil, fmt.Errorf("%w: %q", backupengine.ErrRestorePathNotFound, selection)
			}

			return nil, fmt.Errorf("resolving %q inside the snapshot: %w", selection, err)
		}

		entry = child
	}

	return entry, nil
}

// extractor is one restore in flight: where it may write, what it does
// about collisions, and what it has written so far.
//
// The counters are plain fields rather than atomics because the walk is
// sequential, deliberately. A parallel restore would have to decide what
// "cancelled cleanly" means for entries in flight on other goroutines,
// and the bottleneck of a restore is the disk it is writing to rather
// than the walk over the tree.
type extractor struct {
	root   string
	policy backupengine.RestoreConflict

	// rootCreated records whether this restore made the destination
	// directory. It decides whether the snapshot root's own metadata is
	// applied to it, on the same rule every other directory follows: a
	// directory that was already there belongs to whoever put it there.
	rootCreated bool

	skipOwners bool
	verify     bool
	progress   func(backupengine.RestoreProgress)

	// buf is the one copy buffer this restore uses -- for every file it
	// writes and every file it reads back to verify. One per restore
	// rather than one per file, because the walk is sequential: a
	// per-file allocation hands the collector a megabyte per file in the
	// tree for nothing.
	buf []byte

	files       int64
	directories int64
	symlinks    int64
	skipped     int64
	bytes       int64
	verified    int64
}

func newExtractor(root string, policy backupengine.RestoreConflict, req backupengine.RestoreRequest, rootCreated bool) *extractor {
	return &extractor{
		root:        root,
		policy:      policy,
		rootCreated: rootCreated,
		skipOwners:  req.SkipOwners,
		verify:      req.VerifyContent,
		progress:    req.Progress,
		buf:         make([]byte, restoreCopyChunk),
	}
}

func (x *extractor) report(complete bool) backupengine.RestoreReport {
	return backupengine.RestoreReport{
		Files:       x.files,
		Directories: x.directories,
		Symlinks:    x.symlinks,
		Bytes:       x.bytes,
		Skipped:     x.skipped,
		Verified:    x.verified,
		Complete:    complete,
	}
}

// note reports one finished entry to whoever is watching.
func (x *extractor) note(rel string) {
	if x.progress == nil {
		return
	}

	x.progress(backupengine.RestoreProgress{
		Path:        rel,
		Files:       x.files,
		Directories: x.directories,
		Symlinks:    x.symlinks,
		Skipped:     x.skipped,
		Bytes:       x.bytes,
	})
}

// extract writes whatever was selected into the destination.
//
// A directory is restored as a tree UNDER the destination, and a file or
// a link is restored INSIDE it keeping its own name. That asymmetry is
// what makes a single-file restore usable without the caller having to
// know what they are about to receive: they name a directory, and the
// file appears in it.
func (x *extractor) extract(ctx context.Context, entry fs.Entry) error {
	if dir, ok := entry.(fs.Directory); ok {
		if err := x.dir(ctx, dir, x.root, ""); err != nil {
			return err
		}

		if x.rootCreated {
			return x.applyDirMetadata(x.root, entry, ".")
		}

		return nil
	}

	name, err := safeEntryName(entry.Name())
	if err != nil {
		return err
	}

	dst, err := x.join(x.root, name)
	if err != nil {
		return err
	}

	switch e := entry.(type) {
	case fs.Symlink:
		return x.symlink(ctx, e, dst, name)
	case fs.File:
		return x.file(ctx, e, dst, name)
	default:
		return unsupportedEntry(name, entry)
	}
}

// dir restores one directory's children into dstDir.
func (x *extractor) dir(ctx context.Context, dir fs.Directory, dstDir, rel string) error {
	err := fs.IterateEntries(ctx, dir, func(ctx context.Context, e fs.Entry) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("restoring %q: %w", rel, err)
		}

		name, err := safeEntryName(e.Name())
		if err != nil {
			return fmt.Errorf("%w (in %q)", err, path.Join("/", rel))
		}

		dst, err := x.join(dstDir, name)
		if err != nil {
			return err
		}

		childRel := path.Join(rel, name)

		switch entry := e.(type) {
		case fs.Directory:
			created, err := x.makeDir(dst, childRel)
			if err != nil {
				return err
			}

			x.directories++
			x.note(childRel)

			if err := x.dir(ctx, entry, dst, childRel); err != nil {
				return err
			}

			// The snapshot's own metadata goes on last, once nothing
			// else has to be written inside it -- and only on a
			// directory this restore created, because rewriting the
			// mode or the timestamp of one that was already there is a
			// change to the operator's filesystem that restoring a
			// file INTO it does not authorise.
			if created {
				if err := x.applyDirMetadata(dst, entry, childRel); err != nil {
					return err
				}
			}

			return nil

		case fs.Symlink:
			return x.symlink(ctx, entry, dst, childRel)

		case fs.File:
			return x.file(ctx, entry, dst, childRel)

		default:
			return unsupportedEntry(childRel, e)
		}
	})
	if err != nil {
		return fmt.Errorf("restoring the contents of %q: %w", path.Join("/", rel), err)
	}

	return nil
}

// makeDir creates one restored directory, refusing to write through
// anything that is already there and is not one.
//
// An existing directory is accepted under every conflict policy,
// including the refusing one, and that is deliberate: a directory holds
// no data of its own, so re-entering one is not a replacement of
// anything. Collisions are decided at the leaves, where the bytes are.
//
// Which is why the return value says whether this restore CREATED the
// directory: accepting one that was already there is not the same as
// owning it, and its mode, ownership and timestamp stay the operator's.
//
// An existing SYMBOLIC LINK is refused under every policy. Following it
// would place the rest of the subtree wherever it points, which is the
// escape this whole file exists to prevent, and it is refused rather than
// replaced because a link an operator put there is a statement about
// their filesystem that a restore does not get to overrule.
func (x *extractor) makeDir(dst, rel string) (bool, error) {
	info, err := os.Lstat(dst)

	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return false, fmt.Errorf("%w: %q is a symbolic link in the destination, and restoring %q through it would write wherever it points",
			backupengine.ErrUnsafeSnapshotPath, dst, rel)

	case err == nil && info.IsDir():
		return false, nil

	case err == nil:
		return false, fmt.Errorf("%w: %q is a directory in the snapshot and something else in the destination", backupengine.ErrRestoreConflict, rel)

	case !errors.Is(err, os.ErrNotExist):
		return false, fmt.Errorf("inspecting the restore destination for %q: %w", rel, err)
	}

	if err := os.Mkdir(dst, restoreDirWorkMode); err != nil {
		return false, fmt.Errorf("creating restored directory %q: %w", rel, err)
	}

	return true, nil
}

// file restores one file, writing it under a working name and renaming it
// into place so that nothing partially written ever carries the entry's
// real name.
func (x *extractor) file(ctx context.Context, f fs.File, dst, rel string) error {
	// The collision is decided BEFORE any bytes are read, so a skip
	// policy over a mostly-restored tree costs listings rather than a
	// second download of everything in it.
	resolved, err := x.resolveCollision(dst, rel)
	if err != nil || resolved == collisionSkip {
		return err
	}

	src, err := f.Open(ctx)
	if err != nil {
		return fmt.Errorf("reading %q out of the repository: %w", rel, err)
	}

	defer src.Close() //nolint:errcheck // a read handle's close error says nothing about what was restored.

	tmp, err := os.CreateTemp(filepath.Dir(dst), restoreWorkPrefix+"*")
	if err != nil {
		return fmt.Errorf("creating a working file for %q: %w", rel, err)
	}

	tmpName := tmp.Name()

	// Everything below this point either renames the working file into
	// place or removes it. A restore that is cancelled, refused or fails
	// on a read leaves the destination without it rather than with a
	// plausible partial file somebody would later trust.
	published := false

	defer func() {
		if !published {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	digest := sha256.New()

	written, err := x.copy(ctx, io.MultiWriter(tmp, digest), src)
	if err != nil {
		return fmt.Errorf("writing %q: %w", rel, err)
	}

	// Verified BEFORE the snapshot's mode goes on, and that order is the
	// whole of it: a stored file whose mode denies its owner a read --
	// 0200, 0000, both of which a backed-up tree really holds -- would
	// make this re-read fail with EACCES for every process that is not
	// root, and the durable restore operation always asks for
	// verification. The working file keeps CreateTemp's 0600 until the
	// comparison is done.
	if x.verify {
		if err := x.verifyWritten(tmp, digest.Sum(nil), rel); err != nil {
			return err
		}
	}

	if err := x.applyMetadata(tmp, f, rel); err != nil {
		return err
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing the working file for %q: %w", rel, err)
	}

	// After the last write to the file and before the rename: a
	// modification time set while the descriptor was still open would be
	// the time of the close.
	if err := applyModTime(tmpName, rel, f); err != nil {
		return err
	}

	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("placing restored file %q: %w", rel, err)
	}

	published = true

	// Counted only now. A file that was cancelled, or whose bytes did
	// not survive the write, was removed rather than restored, and a
	// report whose Bytes included it would be describing a tree that
	// does not exist.
	x.files++
	x.bytes += written

	if x.verify {
		x.verified++
	}

	x.note(rel)

	return nil
}

// symlink restores one symbolic link.
//
// The target is written as the string the snapshot holds and is never
// resolved, followed or rewritten. A link whose target points outside the
// destination is the snapshot's content rather than an escape: what would
// make it one is this process following it, which nothing here does.
func (x *extractor) symlink(ctx context.Context, l fs.Symlink, dst, rel string) error {
	resolved, err := x.resolveCollision(dst, rel)
	if err != nil || resolved == collisionSkip {
		return err
	}

	target, err := l.Readlink(ctx)
	if err != nil {
		return fmt.Errorf("reading the target of %q out of the repository: %w", rel, err)
	}

	if resolved == collisionReplace {
		if err := os.Remove(dst); err != nil {
			return fmt.Errorf("replacing %q: %w", rel, err)
		}
	}

	if err := os.Symlink(target, dst); err != nil {
		return fmt.Errorf("restoring symbolic link %q: %w", rel, err)
	}

	// A link's own metadata is set through the link rather than through
	// it: os.Chmod, os.Chown and os.Chtimes all follow one, so applying
	// a link's mode or time with them would rewrite the metadata of
	// whatever it points at -- which is exactly the write this file
	// refuses everywhere else. A link has no mode of its own to restore
	// on the platforms this runs on; ownership and times go through the
	// l-variants, and where the platform has no l-variant for times the
	// time is skipped rather than applied to the target.
	if !x.skipOwners {
		owner := l.Owner()
		if err := os.Lchown(dst, int(owner.UserID), int(owner.GroupID)); err != nil {
			return fmt.Errorf("setting the ownership of restored symbolic link %q: %w", rel, err)
		}
	}

	if mod := l.ModTime(); !mod.IsZero() {
		if err := lchtimes(dst, mod); err != nil && !errors.Is(err, errLinkTimesUnsupported) {
			return fmt.Errorf("setting the modification time of restored symbolic link %q: %w", rel, err)
		}
	}

	x.symlinks++
	x.note(rel)

	return nil
}

// collisionOutcome is what resolveCollision decided about one entry.
type collisionOutcome int

const (
	// collisionNone means nothing is in the way.
	collisionNone collisionOutcome = iota

	// collisionReplace means something is in the way and the policy says
	// to replace it. A file is replaced by the rename that publishes it;
	// a link has to be removed first, because a link is not renamed over
	// by a symlink call.
	collisionReplace

	// collisionSkip means something is in the way and is to be left
	// alone.
	collisionSkip
)

// resolveCollision applies the conflict policy to one destination path.
func (x *extractor) resolveCollision(dst, rel string) (collisionOutcome, error) {
	if _, err := os.Lstat(dst); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return collisionNone, nil
		}

		return collisionNone, fmt.Errorf("inspecting the restore destination for %q: %w", rel, err)
	}

	switch x.policy {
	case backupengine.ConflictSkip:
		x.skipped++
		x.note(rel)

		return collisionSkip, nil

	case backupengine.ConflictOverwrite:
		return collisionReplace, nil

	default:
		return collisionNone, fmt.Errorf("%w: %q is already in the destination", backupengine.ErrRestoreConflict, rel)
	}
}

// applyMetadata puts the snapshot's mode, and optionally its ownership,
// on the working file before it is published.
//
// Ownership is attempted only when the caller asked for it, because an
// unprivileged process cannot set it and a restore that failed for that
// reason would be a restore refused over metadata nobody asked to keep.
func (x *extractor) applyMetadata(tmp *os.File, e fs.Entry, rel string) error {
	if err := tmp.Chmod(e.Mode().Perm()); err != nil {
		return fmt.Errorf("setting the mode of restored file %q: %w", rel, err)
	}

	if x.skipOwners {
		return nil
	}

	owner := e.Owner()
	if err := tmp.Chown(int(owner.UserID), int(owner.GroupID)); err != nil {
		return fmt.Errorf("setting the ownership of restored file %q: %w", rel, err)
	}

	return nil
}

// applyDirMetadata puts a restored directory's own mode, ownership and
// modification time on it, once its whole subtree is written.
//
// The order is the order that survives the modes a snapshot really
// holds. The time is set first, while this process still certainly owns a
// directory it can write; ownership second, because handing a directory
// to another uid is the step that can take away the right to do either of
// the others; the mode last, because a directory whose stored mode denies
// its owner a write would otherwise make everything after it fail.
func (x *extractor) applyDirMetadata(dst string, e fs.Entry, rel string) error {
	if err := applyModTime(dst, rel, e); err != nil {
		return err
	}

	if !x.skipOwners {
		owner := e.Owner()
		if err := os.Chown(dst, int(owner.UserID), int(owner.GroupID)); err != nil {
			return fmt.Errorf("setting the ownership of restored directory %q: %w", rel, err)
		}
	}

	if err := os.Chmod(dst, e.Mode().Perm()); err != nil {
		return fmt.Errorf("setting the mode of restored directory %q: %w", rel, err)
	}

	return nil
}

// applyModTime puts a snapshot entry's modification time on what was
// restored from it.
//
// Restoring content without times is a silent loss of fidelity, and the
// kind nobody notices until they compare: a restored tree in which every
// entry was modified "now" tells an operator nothing about when their
// data was written, and makes every incremental tool pointed at it copy
// the whole thing again.
//
// A zero time is left alone. It is what a snapshot holds for an entry
// that never had one, and inventing 1970 for it would be worse than
// leaving the filesystem's own answer.
func applyModTime(path, rel string, e fs.Entry) error {
	mod := e.ModTime()
	if mod.IsZero() {
		return nil
	}

	if err := os.Chtimes(path, mod, mod); err != nil {
		return fmt.Errorf("setting the modification time of restored %q: %w", rel, err)
	}

	return nil
}

// join builds one destination path and proves it is still under the
// restore root.
//
// This is the second of the two checks, and it is not redundant with
// safeEntryName: that one refuses a NAME, and this one refuses a PATH
// this file built. They fail differently, which is the point -- a future
// edit that assembles a path some other way is still caught here.
func (x *extractor) join(dir, name string) (string, error) {
	dst := filepath.Join(dir, name)

	if dst != x.root && !strings.HasPrefix(dst, x.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q would be written to %q, which is outside %q",
			backupengine.ErrUnsafeSnapshotPath, name, dst, x.root)
	}

	return dst, nil
}

// safeEntryName refuses any stored entry name that is not exactly one
// ordinary path element.
//
// The rule is checkEntryName, shared verbatim with the snapshot WRITE path
// (tree.go), and sharing it is the point: a restore that applied a
// different rule from the writer would either refuse names this product
// stores on purpose or accept ones it refuses to store, and both of those
// are found out on the day somebody is restoring.
//
// It deliberately does not refuse the names that merely LOOK like paths --
// a backslash, a colon, a leading "..", bytes that are not valid UTF-8 --
// because each of those is one legal path element here and #784's contract
// is that they are stored and restored verbatim. What makes such a name
// dangerous is joining it to a directory on a platform whose separator
// differs, and that is refused by the containment check in join, on the
// assembled path, where the platform's own semantics apply.
func safeEntryName(name string) (string, error) {
	if err := checkEntryName(name); err != nil {
		return "", fmt.Errorf("%w: %s", backupengine.ErrUnsafeSnapshotPath, err)
	}

	return name, nil
}

// unsupportedEntry refuses an entry kind this restore cannot write.
//
// A refusal rather than a silent skip: a restore that omits what it did
// not understand is a restore that reports success over a tree missing
// something, and the operator finds out when they go looking for it.
func unsupportedEntry(rel string, e fs.Entry) error {
	return fmt.Errorf("the snapshot holds %q with mode %s, which this restore cannot write", rel, e.Mode())
}

// copy moves bytes while staying answerable to the context.
//
// io.Copy would be shorter and would finish the file it is in the middle
// of whatever the caller asked for, which for a restore of a large file
// means a cancellation that is observed minutes after it was requested.
func (x *extractor) copy(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	var total int64

	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}

		n, readErr := src.Read(x.buf)

		if n > 0 {
			written, writeErr := dst.Write(x.buf[:n])
			total += int64(written)

			if writeErr != nil {
				return total, fmt.Errorf("writing restored bytes: %w", writeErr)
			}
		}

		if errors.Is(readErr, io.EOF) {
			return total, nil
		}

		if readErr != nil {
			return total, fmt.Errorf("reading restored bytes out of the repository: %w", readErr)
		}
	}
}

// verifyWritten reads back what was just written and checks it against
// the digest of what the repository handed over.
//
// It reads the WORKING file, through the descriptor that wrote it and
// before the rename, so a file whose bytes did not survive the write is
// never published under its real name at all. Through the descriptor
// rather than by re-opening the path, because the path may be about to
// carry a mode that denies this process a read, and a verification that
// only works on readable modes is a verification that refuses legitimate
// snapshots.
//
// A mismatch is a failure of the restore rather than a finding on it. The
// file is present, it is wrong, and nothing downstream is ever going to
// look at it again: the next reader is a person or a program that
// believes a completed restore.
func (x *extractor) verifyWritten(f *os.File, want []byte, rel string) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("re-reading restored file %q to verify it: %w", rel, err)
	}

	digest := sha256.New()

	if _, err := io.CopyBuffer(digest, f, x.buf); err != nil {
		return fmt.Errorf("re-reading restored file %q to verify it: %w", rel, err)
	}

	got := digest.Sum(nil)

	if !bytes.Equal(got, want) {
		return fmt.Errorf("restored file %q holds %s and the repository holds %s",
			rel, hex.EncodeToString(got), hex.EncodeToString(want))
	}

	return nil
}
