package kopia

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot/snapshotfs"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
)

// This file is the verification ladder: one walk, four depths, and a
// report of what was actually done rather than of what was asked for.
//
// # Why the rungs are one code path and not four
//
// Because they have to be comparable. Every rung resolves the same tree
// through the same walker and differs only in how far it goes into each
// entry, so "the sampled run passed and the full run failed" is a
// statement about depth rather than about two implementations that happen
// to disagree. Four separate verifiers would each have their own idea of
// what an object is, and the ladder's whole value is that a higher rung
// includes everything a lower one checked.
//
// # Why this is not the vendor's Verifier
//
// snapshotfs.Verifier is close, and it was what this adapter used while
// there was only one depth to offer. Two of its decisions are wrong for a
// ladder whose rungs get WRITTEN DOWN as a claim:
//
//   - its sampling is a coin flip per file (100*rand.Float64() <
//     VerifyFilesPercent), so two runs of the same configuration read
//     different amounts, and on a small tree a sampled run reads nothing
//     at all while the catalog records content_sample. A stride reads a
//     stated fraction, deterministically, and its report means what it
//     says.
//   - it offers no way to separate "the structure resolves" from "the
//     bytes are there", which is exactly the distinction between the
//     first two rungs.
//
// What is kept is the part that is genuinely hard and genuinely theirs:
// snapshotfs.TreeWalker, which walks a snapshot tree in parallel,
// deduplicates repeated objects and bounds the error budget.

// verifyErrorBudget caps how many per-object findings one verification
// collects before it stops walking.
//
// The walker treats this as "abort after N errors", and its default is 1,
// which turns a verification report into "the first thing that is wrong".
// For a tool whose operator question is "how bad is it", the first error
// is the least useful possible answer, so this is deliberately high enough
// to describe real damage and still bounded, because a repository that is
// wrong in ten thousand places does not need the ten thousand and first
// finding to justify a restore from elsewhere.
const verifyErrorBudget = 1000

// Verify implements backupengine.Repository.
//
// The error and the report are independent, which is VerifyReport's own
// contract: a walk torn down by a cancelled context and a walk that found
// damage both come back with partial stats and a non-nil error, and only
// the first of them means nothing was learned. ctx.Err() therefore takes
// precedence over everything else.
//
// A failed verification reports NO achieved level. The field is what was
// proven, and a verification that did not pass proved nothing -- the
// caller writes that empty value into the catalog row, so a set whose
// last run failed cannot be read as still holding the level of the run
// before it.
func (r *repository) Verify(ctx context.Context, id backupengine.SnapshotID, req backupengine.VerifyRequest) (backupengine.VerifyReport, error) {
	plan, err := planVerification(req)
	if err != nil {
		return backupengine.VerifyReport{}, err
	}

	man, err := r.load(ctx, id)
	if err != nil {
		return backupengine.VerifyReport{}, err
	}

	root, err := snapshotfs.SnapshotRoot(r.rep, man)
	if err != nil {
		return backupengine.VerifyReport{}, fmt.Errorf("resolving snapshot root: %w", err)
	}

	// The blob map is read ONCE, before the walk, and it is the
	// repository-integrity half of every rung above structural: a
	// repository's index can name a pack blob that is no longer in the
	// bucket, and no amount of reading the files that happen to be
	// sampled will find the ones that are not. Reading it is one listing
	// of the storage, which is why the structural rung -- the one that
	// runs on every single backup -- does not pay for it.
	var blobs map[blob.ID]blob.Metadata

	if plan.checkBlobs {
		blobs, err = blob.ReadBlobMap(ctx, r.direct.BlobReader())
		if err != nil {
			return backupengine.VerifyReport{}, fmt.Errorf("listing the repository's blobs for verification: %w", err)
		}
	}

	tally := &verifyTally{}

	walkErr := r.walkForVerification(ctx, root, plan, blobs, tally)

	// The drill runs only if the walk is happy. Restoring a tree whose
	// structures already failed to resolve would spend a full restore to
	// rediscover what is already known, and it would do it onto somebody's
	// disk.
	if walkErr == nil && plan.restoreTo != "" {
		walkErr = r.restoreDrill(ctx, id, root, plan.restoreTo, tally)
	}

	report := tally.report()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return report, fmt.Errorf("verifying snapshot %s: %w", id, ctxErr)
	}

	if walkErr != nil {
		return report, fmt.Errorf("verifying snapshot %s at level %s: %w", id, plan.level, walkErr)
	}

	report.Level = plan.achieved()

	return report, nil
}

// verifyPlan is one verification level translated into the work it means.
//
// It exists as a value rather than as a switch inside the walk so that
// "what does content_sample actually do" has one answer, in one place,
// that a reader can check against the level's own documentation.
type verifyPlan struct {
	level model.VerificationLevel

	// readStride is how often a file's stored bytes are read back: 0
	// never, 1 every file, k every kth file. See sampleStride.
	readStride int

	// checkBlobs asks for every backing blob a file's content lives in to
	// be proven present in the storage.
	checkBlobs bool

	// restoreTo is where a restore drill writes, empty at every level
	// below it.
	restoreTo string
}

// achieved is the level this plan proves when it completes.
//
// The one case where it is not the level asked for is a sample of one
// hundred percent, which is a full content read by any honest description
// and is recorded as one. Claiming less than was done would make a row
// understate the evidence behind it, and the ladder is only useful if a
// row means exactly what happened.
func (p verifyPlan) achieved() model.VerificationLevel {
	if p.level == model.LevelContentSample && p.readStride == 1 {
		return model.LevelContentFull
	}

	return p.level
}

// planVerification turns a request into a plan, or refuses it.
func planVerification(req backupengine.VerifyRequest) (verifyPlan, error) {
	switch req.Level {
	case model.LevelStructural:
		// The rung that runs on every backup: the manifest loads, the
		// tree walks, and every content identifier it names resolves
		// through the index. No blob listing and no bytes, which is what
		// makes it affordable -- and what makes it not a content check.
		return verifyPlan{level: req.Level}, nil

	case model.LevelContentSample:
		return verifyPlan{
			level:      req.Level,
			readStride: sampleStride(req.SamplePercent),
			checkBlobs: true,
		}, nil

	case model.LevelContentFull:
		return verifyPlan{level: req.Level, readStride: 1, checkBlobs: true}, nil

	case model.LevelRestoreDrill:
		if req.RestoreTarget == "" {
			return verifyPlan{}, backupengine.ErrRestoreTargetRequired
		}

		// Deliberately readStride 0: the drill reads every byte twice
		// already -- once through the restore path onto the disk, once
		// back out of the repository to compare against it -- and a third
		// full read by the walk would buy nothing the comparison does not
		// already prove.
		return verifyPlan{level: req.Level, checkBlobs: true, restoreTo: req.RestoreTarget}, nil

	default:
		return verifyPlan{}, fmt.Errorf(
			"kopia: %q is not a verification level; a verification nobody can name is a claim nobody can check", req.Level)
	}
}

// sampleStride turns a percentage of files into "read every kth file".
//
// A stride rather than a probability, for the reason the file's own doc
// gives: a sampled verification is written into a catalog row as a claim,
// and a claim whose size depends on a random number is a claim that
// occasionally covers nothing. The stride also guarantees the floor that
// matters -- the first file is always read -- so a content_sample row is
// never a structural pass wearing a different name.
func sampleStride(percent int) int {
	if percent <= 0 {
		percent = backupengine.DefaultVerifySamplePercent
	}

	if percent >= 100 {
		return 1
	}

	return int(math.Ceil(100 / float64(percent)))
}

// verifyTally is what a verification counted, safe to update from the
// walker's workers.
type verifyTally struct {
	objects   atomic.Int64
	seen      atomic.Int64
	filesRead atomic.Int64
	bytesRead atomic.Int64

	restored atomic.Int64
	matched  atomic.Int64

	mu       sync.Mutex
	blobs    map[blob.ID]struct{}
	findings []string
}

func (t *verifyTally) blob(id blob.ID) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.blobs == nil {
		t.blobs = map[blob.ID]struct{}{}
	}

	t.blobs[id] = struct{}{}
}

// finding records one thing that is wrong, up to the error budget.
func (t *verifyTally) finding(format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.findings) >= verifyErrorBudget {
		return
	}

	t.findings = append(t.findings, fmt.Sprintf(format, args...))
}

func (t *verifyTally) report() backupengine.VerifyReport {
	t.mu.Lock()
	defer t.mu.Unlock()

	return backupengine.VerifyReport{
		ObjectsVerified: t.objects.Load(),
		FilesVerified:   t.filesRead.Load(),
		BytesVerified:   t.bytesRead.Load(),
		BlobsChecked:    int64(len(t.blobs)),
		FilesRestored:   t.restored.Load(),
		HashesMatched:   t.matched.Load(),
		Errors:          append([]string(nil), t.findings...),
	}
}

// walkForVerification walks the snapshot tree to the plan's depth.
func (r *repository) walkForVerification(
	ctx context.Context,
	root fs.Entry,
	plan verifyPlan,
	blobs map[blob.ID]blob.Metadata,
	tally *verifyTally,
) error {
	tw, err := snapshotfs.NewTreeWalker(ctx, snapshotfs.TreeWalkerOptions{
		MaxErrors: verifyErrorBudget,
		EntryCallback: func(ctx context.Context, e fs.Entry, oid object.ID, entryPath string) error {
			return r.verifyEntry(ctx, e, oid, entryPath, plan, blobs, tally)
		},
	})
	if err != nil {
		return fmt.Errorf("preparing the verification walk: %w", err)
	}

	defer tw.Close(ctx)

	if err := tw.Process(ctx, root, ""); err != nil {
		// Process returns the walk's accumulated error, which the loop
		// below reads out of the walker itself; reporting it here as well
		// would double-count every finding.
		_ = err
	}

	errs, _ := tw.GetErrors()
	for _, e := range errs {
		tally.finding("%s", e.Error())
	}

	return tw.Err() //nolint:wrapcheck // the caller names the snapshot and the level; this is the walk's own sentence.
}

// verifyEntry is one entry at the plan's depth.
func (r *repository) verifyEntry(
	ctx context.Context,
	e fs.Entry,
	oid object.ID,
	entryPath string,
	plan verifyPlan,
	blobs map[blob.ID]blob.Metadata,
	tally *verifyTally,
) error {
	// Cancellation is observed HERE, per entry, because the walker
	// underneath never checks it: its directory loop only stops on its
	// error budget, so a verification whose caller gave up went on
	// resolving every object in the snapshot and came back with a
	// COMPLETE report and a cancellation error -- work nobody was
	// waiting for, and a report that says more was proven than the
	// caller's window allowed. Returning the error here feeds the
	// walker's own budget, which stops the walk.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("verification of %s was cancelled: %w", entryPath, err)
	}

	tally.objects.Add(1)

	if e.IsDir() {
		// A directory's own manifest object is resolved by the walker
		// reading it in order to iterate it, which is the structural
		// claim for a directory: a tree whose directory object does not
		// resolve cannot be walked, and the walk is what is happening.
		return nil
	}

	contentIDs, err := r.rep.VerifyObject(ctx, oid)
	if err != nil {
		return fmt.Errorf("the stored structure of %s does not resolve: %w", entryPath, err)
	}

	if blobs != nil {
		for _, cid := range contentIDs {
			info, err := r.rep.ContentInfo(ctx, cid)
			if err != nil {
				return fmt.Errorf("content %v of %s cannot be looked up: %w", cid, entryPath, err)
			}

			if _, ok := blobs[info.PackBlobID]; !ok {
				return fmt.Errorf("%s is stored in blob %q, which this repository's storage does not hold",
					entryPath, info.PackBlobID)
			}

			tally.blob(info.PackBlobID)
		}
	}

	if plan.readStride == 0 {
		return nil
	}

	// The stride is over files SEEN rather than over a random number, so
	// a walk that finds n files reads exactly ceil(n/stride) of them
	// however the walker happens to order them.
	if n := tally.seen.Add(1) - 1; n%int64(plan.readStride) != 0 {
		return nil
	}

	read, err := readWholeObject(ctx, r.rep, oid)
	if err != nil {
		return fmt.Errorf("%s could not be read back: %w", entryPath, err)
	}

	tally.filesRead.Add(1)
	tally.bytesRead.Add(read)

	return nil
}

// readWholeObject reads one stored object to the end and reports how many
// bytes came back.
//
// The bytes are discarded and that is the point: reading them is what
// decrypts them, decompresses them and checks them against the hash they
// are addressed by, so a read that completes IS the verification. Nothing
// here needs to look at the content.
func readWholeObject(ctx context.Context, rep repo.Repository, oid object.ID) (int64, error) {
	reader, err := rep.OpenObject(ctx, oid)
	if err != nil {
		return 0, fmt.Errorf("opening object %v: %w", oid, err)
	}

	defer reader.Close() //nolint:errcheck // a read-only object reader has nothing to report on close.

	n, err := io.Copy(io.Discard, reader)
	if err != nil {
		return n, fmt.Errorf("reading object %v: %w", oid, err)
	}

	return n, nil
}

// restoreDrill is the top rung: put the snapshot back on a real
// filesystem through the real restore path, then check that what landed
// there is what the repository holds.
//
// # Why the restore alone is not the drill
//
// Because a restore reports its own success. The vendor's restore has a
// mode in which it writes placeholder stubs instead of files and returns
// entirely plausible statistics (see Restore's note on
// RestoreDirEntryAtDepth), and a restore path that truncates, skips or
// mis-places a file fails in exactly the way that does not produce an
// error. The evidence is the bytes on the disk, hashed and compared
// against the bytes in the repository, plus the assertion that nothing
// else appeared under the target.
func (r *repository) restoreDrill(ctx context.Context, id backupengine.SnapshotID, root fs.Entry, target string, tally *verifyTally) error {
	if _, err := r.Restore(ctx, id, backupengine.RestoreRequest{
		TargetPath: target,

		// A drill restores as an ordinary process would have to: it does
		// not claim ownership it cannot set, and it refuses to overwrite,
		// because a drill that could overwrite is a verification that can
		// destroy data.
		SkipOwners: true,
	}); err != nil {
		return fmt.Errorf("the restore drill could not restore the snapshot: %w", err)
	}

	dir, ok := root.(fs.Directory)
	if !ok {
		return fmt.Errorf("the snapshot's root is not a directory, so there is nothing to compare a restore against")
	}

	return compareRestoredTree(ctx, dir, target, tally)
}

// compareRestoredTree checks a restored directory against the snapshot it
// came from, both ways round.
//
// Both directions are necessary and neither is sufficient. Walking the
// snapshot and looking for each entry on the disk finds what the restore
// failed to write or wrote wrongly; walking the disk and looking for each
// entry in the snapshot finds what the restore wrote that nobody asked
// for, which is the shape a path-traversal bug has.
func compareRestoredTree(ctx context.Context, root fs.Directory, target string, tally *verifyTally) error {
	expected := map[string]struct{}{}

	if err := compareRestoredDir(ctx, root, target, "", expected, tally); err != nil {
		return err
	}

	extra, err := unexpectedRestoredPaths(target, expected)
	if err != nil {
		return err
	}

	for _, p := range extra {
		tally.finding("the restore wrote %q, which is not in the snapshot", p)
	}

	if findings := tally.report().Errors; len(findings) > 0 {
		return fmt.Errorf("the restored tree does not match the repository: %d finding(s), the first being %q",
			len(findings), findings[0])
	}

	return nil
}

// compareRestoredDir compares one directory of the snapshot with one
// directory on the disk.
func compareRestoredDir(
	ctx context.Context,
	dir fs.Directory,
	target, rel string,
	expected map[string]struct{},
	tally *verifyTally,
) error {
	return fs.IterateEntries(ctx, dir, func(ctx context.Context, e fs.Entry) error { //nolint:wrapcheck // the iteration error is the walk's own and is wrapped by the caller.
		childRel := path.Join(rel, e.Name())
		childPath := filepath.Join(target, filepath.FromSlash(childRel))
		expected[childRel] = struct{}{}

		switch entry := e.(type) {
		case fs.Directory:
			info, err := os.Lstat(childPath)
			if err != nil {
				tally.finding("the snapshot holds directory %q, and the restore did not produce it: %v", childRel, err)

				return nil
			}

			if !info.IsDir() {
				tally.finding("the snapshot holds directory %q, and the restore wrote something that is not a directory", childRel)

				return nil
			}

			return compareRestoredDir(ctx, entry, target, childRel, expected, tally)

		case fs.Symlink:
			compareRestoredSymlink(ctx, entry, childRel, childPath, tally)

			return nil

		case fs.File:
			compareRestoredFile(ctx, entry, childRel, childPath, tally)

			return nil

		default:
			// An entry kind this comparison does not understand is a
			// finding rather than a silence: the drill's claim is that
			// everything in the snapshot was checked.
			tally.finding("the snapshot holds %q, whose kind this drill cannot compare", childRel)

			return nil
		}
	})
}

// compareRestoredFile hashes one file out of the repository and the file
// the restore wrote, and records a finding if they differ.
func compareRestoredFile(ctx context.Context, file fs.File, rel, restoredPath string, tally *verifyTally) {
	stored, n, err := hashStoredFile(ctx, file)
	if err != nil {
		tally.finding("%q could not be read out of the repository during the drill: %v", rel, err)

		return
	}

	tally.filesRead.Add(1)
	tally.bytesRead.Add(n)

	written, err := hashLocalFile(restoredPath)
	if err != nil {
		tally.finding("%q is in the snapshot and the restore did not produce a readable file for it: %v", rel, err)

		return
	}

	tally.restored.Add(1)

	if stored != written {
		tally.finding("%q restored as %s, and the repository holds %s", rel, written, stored)

		return
	}

	tally.matched.Add(1)
}

// compareRestoredSymlink checks that a link the snapshot holds was
// restored as a link to the same place.
//
// The target is compared as a string and the link is never followed:
// resolving it would ask the filesystem to walk wherever the link points,
// which for a hostile or simply broken snapshot is exactly the escape a
// restore is supposed not to perform.
func compareRestoredSymlink(ctx context.Context, link fs.Symlink, rel, restoredPath string, tally *verifyTally) {
	want, err := link.Readlink(ctx)
	if err != nil {
		tally.finding("the link %q could not be read out of the repository during the drill: %v", rel, err)

		return
	}

	got, err := os.Readlink(restoredPath)
	if err != nil {
		tally.finding("%q is a link in the snapshot and the restore did not produce one: %v", rel, err)

		return
	}

	tally.restored.Add(1)

	if got != want {
		tally.finding("the link %q was restored pointing at %q, and the snapshot holds %q", rel, got, want)

		return
	}

	tally.matched.Add(1)
}

// hashStoredFile reads one file out of the repository and hashes it,
// reporting how many bytes it read.
func hashStoredFile(ctx context.Context, file fs.File) (string, int64, error) {
	reader, err := file.Open(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("opening the stored file: %w", err)
	}

	defer reader.Close() //nolint:errcheck // a read-only reader has nothing to report on close.

	h := sha256.New()

	n, err := io.Copy(h, reader)
	if err != nil {
		return "", n, fmt.Errorf("reading the stored file: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// hashLocalFile hashes what the restore actually wrote.
func hashLocalFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // the path is composed from the snapshot's own entry names under a caller-chosen target.
	if err != nil {
		return "", fmt.Errorf("opening the restored file: %w", err)
	}

	defer f.Close() //nolint:errcheck // a read-only file has nothing to report on close.

	h := sha256.New()

	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("reading the restored file: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// unexpectedRestoredPaths is everything under target that the snapshot
// did not name, sorted so a report is stable.
//
// This is the half of the drill that notices a restore writing outside
// what it was asked for. A traversal that escaped the target entirely
// would not show up here, which is why the caller's own test asserts on a
// canary outside the directory as well; what this finds is the more
// likely version -- a stray file, a placeholder stub, a partial write left
// behind under the target itself.
func unexpectedRestoredPaths(target string, expected map[string]struct{}) ([]string, error) {
	var extra []string

	err := filepath.WalkDir(target, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(target, p)
		if err != nil {
			return err
		}

		if rel == "." {
			return nil
		}

		slashed := filepath.ToSlash(rel)
		if _, ok := expected[slashed]; !ok {
			extra = append(extra, slashed)

			if d.IsDir() {
				// Its children are unexpected for the same reason, and
				// listing them all would bury the directory that is the
				// actual finding.
				return filepath.SkipDir
			}
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading the restore drill's output at %s: %w", target, err)
	}

	sort.Strings(extra)

	return extra, nil
}
