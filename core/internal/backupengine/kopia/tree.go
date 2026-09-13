package kopia

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	kopiafs "github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/upload"

	"github.com/backupdproject/backupd/core/internal/backupengine"
)

// This file is the set-scoped half of the adapter: one backup-set run
// stored as ONE snapshot, under ONE source, with the set's objects as a
// tree inside it. Issue #783 and the ADR beside it record why that shape
// replaces the per-object one in stream.go as the production path.
//
// # Why this is not stream.go with a loop around it
//
// backupengine.StreamingRepository is PUSH-shaped: the caller reads an
// object and hands the bytes over, so the caller decides when a snapshot
// starts and ends, and the only unit available is one object. Kopia's
// uploader is PULL-shaped: it walks a tree, decides what it already
// holds, and asks for the bytes it wants. backupengine.TreeRepository is
// the port that matches, and this file is the inversion -- everything
// here exists so that Kopia can ASK a backupengine.SourceDir for its
// children rather than be told.
//
// The consequence that matters is the error model. There is nothing
// per-object to delete out of a set-wide snapshot, so "store, then remove
// what could not be proven" is not available: a listing that failed, a
// stream that broke or an entry that was refused has to prevent the
// manifest from existing at all. That is what the write session below is
// for, and it is the same structural argument snapshotStreamOnce makes.

const (
	// treeSnapshotPurpose names this adapter's write session in the
	// vendor's own logs, so an interrupted session in a repository can be
	// attributed to a backup run rather than to maintenance.
	treeSnapshotPurpose = "backupd:snapshot-tree"

	// treeRootDirName is the name the set's root directory carries in the
	// manifest.
	//
	// It is not part of any restored path -- restore writes the root's
	// CONTENTS into the target directory -- so what it has to be is
	// something that reads honestly when the manifest is inspected, and
	// "." is what the vendor's own virtual directories use. Naming it
	// after the set's path instead would put the same string in two
	// places, the manifest's source identity and its root entry, where
	// they could disagree.
	treeRootDirName = "."

	// treeDirPermissions is what a source directory records. A remote
	// source's directory has no mode this process can read, so this is
	// the same choice streamPermissions makes and for the same reason:
	// an entry needs permission bits, and inventing narrower ones would
	// only look like information the source did not give us.
	treeDirPermissions = streamPermissions | os.ModeDir
)

var _ backupengine.TreeRepository = (*repository)(nil)

// SnapshotTree implements backupengine.TreeRepository.
//
// The whole run is one Kopia write session, exactly as snapshotStreamOnce
// is, and that is what makes "no torn snapshot marked good" structural
// instead of careful: every failure path below returns an error out of the
// session callback, the session then closes WITHOUT flushing, and whatever
// content a partial walk managed to write is discarded with it. There is
// no state in which a manifest exists and the tree behind it does not.
func (r *repository) SnapshotTree(
	ctx context.Context,
	req backupengine.TreeSnapshotRequest,
) (backupengine.TreeSnapshotInfo, error) {
	si, err := sourceInfo(req.Source)
	if err != nil {
		return backupengine.TreeSnapshotInfo{}, err
	}

	// Refused before a byte of storage is touched. A nil root is not an
	// empty tree: it is a caller that did not say what to back up, and
	// storing an empty snapshot for it would advertise a restore point
	// that restores nothing.
	if req.Root == nil {
		return backupengine.TreeSnapshotInfo{}, fmt.Errorf(
			"kopia: tree snapshot request for %s carries no root directory; an absent tree is not an empty one", si.Path)
	}

	// Refused for the same reason and at the same moment. A run id is
	// how crash reconciliation tells this run's orphaned manifest from a
	// co-tenant set's or an operator's own; storing a snapshot without
	// one produces a restore point that can only ever be matched to a
	// run by its timestamp, which is the ambiguity
	// backupengine.TagKeyRun exists to remove. It cannot be fixed after
	// the fact either: nothing later can tell which run wrote a manifest
	// that did not say.
	if req.RunID == "" {
		return backupengine.TreeSnapshotInfo{}, fmt.Errorf(
			"kopia: tree snapshot request for %s carries no run id; a snapshot nothing can attribute to a run is one reconciliation can only guess at", si.Path)
	}

	// The manifest's tags, assembled before any storage is touched so
	// that the failure to attribute a run is a refusal rather than a
	// snapshot with a missing label.
	tags := treeSnapshotTags(req)

	run := &treeRun{}
	prog := &captureProgress{}

	// The set's root directory. Its modification time is this run's own
	// clock rather than anything the source said, because a backup set's
	// root is not an object on the source and has no modification time of
	// its own; the entries underneath it carry the source's times.
	root := &treeDir{
		name:    treeRootDirName,
		modTime: r.adapter.now(),
		dir:     req.Root,
		run:     run,
	}

	// Unblock a read that is already in flight when the caller cancels.
	// Kopia's copy loop only checks its cancellation flag BETWEEN reads,
	// so a reader parked on a remote socket is not reachable from inside
	// the uploader; closing it from out here is. This is the same tracker
	// the per-object path uses, for the same reason.
	runDone := make(chan struct{})
	defer close(runDone)

	go func() {
		select {
		case <-ctx.Done():
			run.streams.closeAll()
		case <-runDone:
		}
	}()

	// Belt and braces for the readers Kopia's own defer does not cover: a
	// source opened but never handed to the copy loop because the run
	// failed elsewhere first. guardedReader.Close is idempotent, so this
	// costs nothing when Kopia got there first.
	defer run.streams.closeAll()

	var (
		uploaded atomic.Int64
		man      *snapshot.Manifest
		id       manifest.ID
	)

	// The reuse measurement is a DELTA across the write session: the
	// vendor's counter is the repository handle's lifetime total, and
	// reporting a lifetime total as this run's reuse would make every
	// run look better than the one before it. Reading it here, before
	// anything is written, is the first half; see
	// contentDeduplicatedBytes for why this is read reflectively and why
	// a failure to read it is reported as "not measured".
	reusedBefore, reuseReadable := contentDeduplicatedBytes(r.rep)

	err = repo.WriteSession(ctx, r.rep, repo.WriteSessionOptions{
		Purpose:  treeSnapshotPurpose,
		OnUpload: func(n int64) { uploaded.Add(n) },
	}, func(ctx context.Context, w repo.RepositoryWriter) error {
		u := upload.NewUploader(w)
		u.Progress = prog
		u.FailFast = true
		u.DisableIgnoreRules = true

		// ParallelUploads = 1 is load-bearing here in a way it is not in
		// the per-object path, which only ever has one file.
		//
		// A backupengine.SourceDir is a FORWARD CURSOR over somebody
		// else's directory listing, and so is every reader it hands out.
		// Kopia's parallel path pulls the next entry off the iterator
		// while a worker is still reading the previous one, which for a
		// transport that multiplexes one connection means two reads
		// interleaved on a stream that can serve one. One worker makes
		// the walk strictly sequential: an entry is finished before the
		// iterator is asked for the next.
		//
		// It also has to be said explicitly, because effectiveParallelFileReads
		// falls back to the POLICY's MaxParallelFileReads, whose default
		// is the machine's CPU count.
		u.ParallelUploads = 1

		// No previous manifests, for the reason stream.go argues at
		// length and this port's own doc repeats: a source's scan-time
		// size and modification time are metadata from a live directory
		// somebody else is writing to, and Kopia's findCachedEntry would
		// reuse a streaming entry on the strength of mode and mtime
		// alone -- so a file rewritten in place with its mtime preserved
		// would keep its OLD content in every later snapshot, silently.
		// Every run therefore reads every byte the source offers, and
		// reuse has to earn itself at the content level.
		// TreeSnapshotInfo.RepositoryBytesWritten is the measurement
		// that shows it does.
		m, uerr := u.Upload(ctx, root, policy.BuildTree(nil, policy.DefaultPolicy), si)
		if uerr != nil {
			return uerr
		}

		// Two guards, because they catch different things. uploadFailure
		// catches what the uploader recorded against an entry and
		// reported only to the progress sink; run.failure catches what
		// this adapter could not report through the vendor's signatures
		// at all.
		if bad := prog.uploadFailure(m); bad != nil {
			return bad
		}

		if bad := run.failure(); bad != nil {
			return bad
		}

		m.Description = req.Description
		m.Tags = tags

		// See enginepolicy.go: a manifest this adapter saves is pinned,
		// so the engine's own retention cannot expire it.
		pinManifest(m)

		sid, serr := snapshot.SaveSnapshot(ctx, w, m)
		if serr != nil {
			return fmt.Errorf("saving snapshot of %s: %w", si.Path, serr)
		}

		man, id = m, sid

		return nil
	})
	if err != nil {
		return backupengine.TreeSnapshotInfo{}, fmt.Errorf("snapshotting the tree of %s: %w", si.Path, err)
	}

	man.ID = id

	// The second half of the delta. Both reads have to have worked and
	// the counter has to have moved forwards: a counter that went
	// backwards means something reset it underneath this run -- another
	// caller asking for a resetting snapshot, a handle reopened -- and
	// the difference is then not this run's reuse but a negative number
	// dressed up as one.
	var (
		reused        int64
		reuseMeasured bool
	)

	if reuseReadable {
		if after, ok := contentDeduplicatedBytes(r.rep); ok && after >= reusedBefore {
			reused, reuseMeasured = after-reusedBefore, true
		}
	}

	return backupengine.TreeSnapshotInfo{
		SnapshotInfo:           snapshotInfo(man),
		SourceBytesRead:        run.read.Load(),
		RepositoryBytesWritten: uploaded.Load(),
		ContentReusedBytes:     reused,
		ContentReuseMeasured:   reuseMeasured,
	}, nil
}

// treeSnapshotTags is the manifest's tags: what the caller passed, plus
// the run attribution this adapter owns.
//
// The caller's map is COPIED rather than written to. A request is the
// caller's own value and may well be a literal it reuses for the next
// run or a map it still reads afterwards; an adapter that added a key to
// it would be editing its caller's state to record its own, and the
// symptom -- a second run carrying the first run's id because the caller
// handed the same map back -- would look like a reconciliation bug
// rather than an aliasing one.
//
// The adapter's run id is set LAST and therefore wins over a
// backupengine.TagKeyRun the caller put in Tags itself. That is not
// politeness about precedence: TreeSnapshotRequest.RunID is the field
// the manager is required to fill in and the one SnapshotTree refused an
// empty value for, so a disagreeing tag is a caller with two ideas about
// which run this is, and the one that was validated is the one the
// manifest gets. Silently keeping the other would put an id in the
// repository that no catalog row matches, which reconciliation reads as
// "somebody else's snapshot" -- the exact failure the tag exists to
// prevent.
func treeSnapshotTags(req backupengine.TreeSnapshotRequest) map[string]string {
	tags := make(map[string]string, len(req.Tags)+1)

	for k, v := range req.Tags {
		tags[k] = v
	}

	tags[backupengine.TagKeyRun] = req.RunID

	return tags
}

// --- the reuse measurement -----------------------------------------------

// kopiaDedupCounterName is the name of Kopia's own content-level
// deduplication counter, as repo/content/content_manager_metrics.go
// registers it and repo/content/content_manager.go increments it from
// WriteContent when the content being written is already stored.
//
// It is a string looked up at runtime rather than a symbol the compiler
// checks, so the tests in tree_test.go that assert reuse on a second run
// of an unchanged tree are what pins it: if a version bump renames the
// counter, the lookup below stops finding it and those tests fail with
// "reuse was not measured" rather than the adapter quietly reporting
// nothing was ever deduplicated.
const kopiaDedupCounterName = "content_deduplicated_bytes"

// contentDeduplicatedBytes reads Kopia's lifetime count of bytes it did
// not have to store because it already held the content, or reports that
// it could not.
//
// # Why the vendor's number and not arithmetic
//
// The tempting definition of reuse is SourceBytesRead minus
// RepositoryBytesWritten, and it is wrong in a way that gets worse the
// better the repository works: that difference also contains compression
// and the pack and index overhead of storing anything at all. A first
// snapshot of a directory of text and logs, which has by definition
// reused nothing, compresses to a fraction of its size and would be
// reported as mostly deduplicated. Only the content manager knows which
// bytes it recognised, and this is where it says so.
//
// # Why reflection
//
// The counter is reachable only through repo.DirectRepository's Metrics
// accessor (repo/repository.go), whose return type *metrics.Registry
// lives in github.com/kopia/kopia/internal/metrics. An internal package
// cannot be imported from here, which means the type cannot be named --
// not in an import, not in a type assertion, not in a function
// signature. Reflection is not a style choice here, it is the only route
// the vendor leaves open, and it is confined to this one function so
// that the day Kopia exports a registry type this is the only thing that
// changes.
//
// # Why a failure is "not measured" and not zero
//
// Every step below is guarded and every guard leads to the same answer:
// false. A missing method, a changed signature, a nil registry, a
// snapshot that is not a struct, a Counters field that is not a
// string-keyed int64 map, an absent counter -- none of them are evidence
// that this run deduplicated nothing, and returning a confident zero
// would turn a vendor upgrade into an operator hunting a backup that is
// working. The caller reports that distinction all the way out as
// TreeSnapshotInfo.ContentReuseMeasured.
//
// The snapshot is taken WITHOUT reset. The registry belongs to the
// repository handle and the vendor's own reporting reads it too; a reset
// here would zero somebody else's counters to save this function a
// subtraction.
//
// One honest limitation: the counter is per open repository, not per
// session, so two concurrent SnapshotTree calls on the SAME handle would
// each see the other's reuse inside their delta. Nothing in this product
// runs two snapshots of one repository at once -- a run holds the
// repository for its duration -- and the alternative, a per-session
// counter, is not something the vendor offers at any level of API.
func contentDeduplicatedBytes(rep repo.Repository) (int64, bool) {
	if rep == nil {
		return 0, false
	}

	metricsMethod := reflect.ValueOf(rep).MethodByName("Metrics")
	if !metricsMethod.IsValid() {
		return 0, false
	}

	if mt := metricsMethod.Type(); mt.NumIn() != 0 || mt.NumOut() != 1 {
		return 0, false
	}

	registry := metricsMethod.Call(nil)[0]
	if registry.Kind() != reflect.Pointer || registry.IsNil() {
		return 0, false
	}

	snapshotMethod := registry.MethodByName("Snapshot")
	if !snapshotMethod.IsValid() {
		return 0, false
	}

	st := snapshotMethod.Type()
	if st.NumIn() != 1 || st.In(0).Kind() != reflect.Bool || st.NumOut() != 1 {
		return 0, false
	}

	snap := snapshotMethod.Call([]reflect.Value{reflect.ValueOf(false)})[0]
	if snap.Kind() != reflect.Struct {
		return 0, false
	}

	counters := snap.FieldByName("Counters")
	if !counters.IsValid() || counters.Kind() != reflect.Map {
		return 0, false
	}

	ct := counters.Type()
	if ct.Key().Kind() != reflect.String || ct.Elem().Kind() != reflect.Int64 {
		return 0, false
	}

	// Converted rather than passed straight, because a map keyed by a
	// named string type would panic on a plain string key, and a panic
	// out of a measurement is not an outcome a backup run may have.
	value := counters.MapIndex(reflect.ValueOf(kopiaDedupCounterName).Convert(ct.Key()))
	if !value.IsValid() {
		return 0, false
	}

	return value.Int(), true
}

// treeRun is the state one SnapshotTree call keeps outside the vendor's
// types.
//
// Two of its three fields exist because the vendor's interfaces have
// nowhere to put them. fs.DirectoryIterator.Close returns NOTHING, so a
// source that only discovers its listing was truncated when the pass is
// released has no way to say so through Kopia; deferred holds that error
// until the session callback can check it, which is before SaveSnapshot
// and therefore in time to prevent the manifest. read is an accumulator
// because bytes pulled from the source are counted in the file entries
// and reported once at the end.
type treeRun struct {
	// streams tracks the readers handed to Kopia so a cancelled context
	// can close one that is blocked in Read. See openStreams.
	streams openStreams

	// read counts bytes actually pulled from the source's own readers.
	// It is atomic because Kopia may be reading and hashing on another
	// goroutine even at ParallelUploads = 1.
	read atomic.Int64

	mu sync.Mutex
	// +checklocks:mu
	deferred error
}

// fail records an error that could not be returned where it happened.
// The first one wins: it is the one with the original cause in it, and
// everything after it is likely to be the teardown it triggered.
func (t *treeRun) fail(err error) {
	if err == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.deferred == nil {
		t.deferred = err
	}
}

// failure reports why a manifest must not be saved, or nil if nothing was
// deferred.
func (t *treeRun) failure() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.deferred
}

// treeDir presents one backupengine.SourceDir as a Kopia fs.Directory.
//
// # Why iteration is one-shot and says so twice
//
// SupportsMultipleIterations returns false because a source listing is a
// forward cursor over a remote -- transport.DirReader, and every backend
// that declares bounded_listing -- and cannot be rewound. That is the
// vendor's own way of asking, and answering it honestly is not enough on
// its own: nothing in Kopia is obliged to consult it, and the caller that
// would ignore it is not hypothetical. Uploader.startDataSizeEstimation
// walks the WHOLE TREE a second time, concurrently with the upload, to
// estimate a progress total, and it is skipped here only because
// captureProgress embeds NullUploadProgress whose Enabled() reports
// false. That is a coincidence of two files agreeing, which is exactly
// the kind of agreement that stops holding during a version bump.
//
// So Iterate also refuses a second pass outright. A double walk then
// fails the run loudly instead of interleaving two consumers on one
// cursor and producing a snapshot missing whichever entries the other
// pass swallowed.
type treeDir struct {
	name    string
	modTime time.Time
	dir     backupengine.SourceDir
	run     *treeRun

	iterated atomic.Bool
}

var _ kopiafs.Directory = (*treeDir)(nil)

func (d *treeDir) Name() string               { return d.name }
func (d *treeDir) Size() int64                { return 0 }
func (d *treeDir) Mode() os.FileMode          { return treeDirPermissions }
func (d *treeDir) ModTime() time.Time         { return d.modTime }
func (d *treeDir) IsDir() bool                { return true }
func (d *treeDir) Sys() any                   { return nil }
func (d *treeDir) Owner() kopiafs.OwnerInfo   { return kopiafs.OwnerInfo{} }
func (d *treeDir) Device() kopiafs.DeviceInfo { return kopiafs.DeviceInfo{} }

// LocalFilesystemPath returns "" because a source directory is not on this
// machine's filesystem. Its emptiness is also what stops Kopia running
// before-folder and after-folder actions against a path that does not
// exist.
func (d *treeDir) LocalFilesystemPath() string { return "" }

// Close is required by fs.Entry and must be idempotent. The directory
// itself holds nothing open; the pass it handed out is released through
// the iterator's own Close.
func (d *treeDir) Close() {}

// SupportsMultipleIterations reports false: see the type doc.
func (d *treeDir) SupportsMultipleIterations() bool { return false }

// Child refuses rather than answering.
//
// The honest implementation would start a fresh pass and scan it, which
// is precisely the second walk of a one-shot cursor this type exists to
// prevent, and the tempting one would keep a buffer of everything seen so
// far, which turns a set of a million objects into a million entries in
// memory. Nothing on the snapshot path calls it: Kopia asks Child only of
// the directories in PREVIOUS manifests, and SnapshotTree passes none.
// The vendor's own streaming directory refuses here too.
func (d *treeDir) Child(context.Context, string) (kopiafs.Entry, error) {
	return nil, fmt.Errorf(
		"kopia: directory %q of a backup source cannot be asked for a child by name; its listing is a forward cursor, not an index", d.name)
}

// Iterate begins the one pass this directory has.
func (d *treeDir) Iterate(ctx context.Context) (kopiafs.DirectoryIterator, error) {
	if !d.iterated.CompareAndSwap(false, true) {
		return nil, fmt.Errorf(
			"kopia: directory %q was asked for a second pass; a backup source's listing cannot be rewound", d.name)
	}

	it, err := d.dir.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing source directory %q: %w", d.name, err)
	}

	return &treeDirIterator{name: d.name, it: it, run: d.run}, nil
}

// treeDirIterator is one pass over one source directory, in Kopia's own
// three-state shape: (entry, nil) for progress, (nil, nil) for the end,
// (nil, err) for a failure.
//
// The translation from backupengine's (entry, ok, err) is the whole point
// of the type. Those are the same three states said differently, and
// collapsing them -- reporting the end of a directory as an error, or a
// failed listing as the end of one -- would turn an unreadable source
// into a snapshot that claims the directory was empty.
type treeDirIterator struct {
	name string
	it   backupengine.SourceDirIterator
	run  *treeRun

	closeOnce sync.Once
}

var _ kopiafs.DirectoryIterator = (*treeDirIterator)(nil)

func (i *treeDirIterator) Next(ctx context.Context) (kopiafs.Entry, error) {
	entry, ok, err := i.it.Next(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading source directory %q: %w", i.name, err)
	}

	if !ok {
		return nil, nil
	}

	// Every entry the engine will ever see passes through here, which is
	// why the refusals live here rather than in a pre-pass: a pre-pass
	// would have to walk the tree first, and the whole tree is what this
	// port refuses to hold in memory. A refusal here ends the listing,
	// which ends the run, which means nothing was stored.
	if err := checkTreeEntry(entry); err != nil {
		return nil, fmt.Errorf("in source directory %q: %w", i.name, err)
	}

	if entry.IsDir() {
		return &treeDir{name: entry.Name, modTime: entry.ModTime, dir: entry.Dir, run: i.run}, nil
	}

	return &treeFileEntry{
		name:    entry.Name,
		size:    entry.Size,
		modTime: entry.ModTime,
		src:     entry.Stream,
		run:     i.run,
	}, nil
}

// Close releases the pass.
//
// fs.DirectoryIterator.Close returns nothing, and a source's Close is
// entitled to report that the listing it just served was truncated, so
// the error is parked on the run instead of dropped. The session callback
// checks it before SaveSnapshot, which is the last moment at which a
// truncated listing can still be prevented from becoming a snapshot that
// claims a directory held fewer entries than it did.
func (i *treeDirIterator) Close() {
	i.closeOnce.Do(func() {
		if err := i.it.Close(); err != nil {
			i.run.fail(fmt.Errorf("closing the listing of source directory %q: %w", i.name, err))
		}
	})
}

// checkEntryName refuses a stored entry name that is not exactly one
// ordinary path element.
//
// It is shared by the two ends of the same threat, and that is the whole
// reason it is a function rather than four lines inside checkTreeEntry.
// The WRITE path refuses such a name before it is stored, which is the
// point at which it costs nothing; the RESTORE path (localrestore.go)
// refuses it again on the way out, because a repository is not a trust
// boundary -- a domain may be shared, an operator may have written to the
// same bucket with the vendor's own CLI, and a snapshot that is only safe
// because our writer was careful is a snapshot whose safety nobody can
// check.
//
// What it does NOT refuse is as load-bearing as what it does. A backslash,
// a newline, a colon, a leading "..", bytes that are not valid UTF-8: each
// of those is one legal path element on the platforms this product runs
// on, so each is stored verbatim and restored verbatim (#784's
// namesThatOnlyLOOKLikePaths). Refusing them would be the worse bug of the
// two -- a backup that silently drops every file whose name contains a
// backslash is a backup nobody can rely on. Where those names could still
// mean something dangerous is when they are JOINED to a directory, on a
// platform whose separator differs, and that is caught where it happens:
// the restore proves the assembled path is still under the destination.
func checkEntryName(name string) error {
	switch {
	case name == "":
		return errors.New("an entry arrived with an empty name; an entry with no name cannot be placed in a tree")
	case strings.ContainsRune(name, '/') || strings.ContainsRune(name, os.PathSeparator):
		return fmt.Errorf(
			"entry name %q contains a path separator; an entry name is one path element and may not choose where in the snapshot it lands", name)
	case name == "." || name == "..":
		return fmt.Errorf("entry name %q is a directory traversal, not the name of an entry", name)
	case strings.ContainsRune(name, '\x00'):
		return fmt.Errorf(
			"entry name %q contains a NUL byte; it would store cleanly and then be unrestorable, which is a restore point this engine must not advertise", name)
	}

	return nil
}

// checkTreeEntry refuses a source entry that cannot honestly be stored.
//
// Each of these could be stored as SOMETHING, and that is the problem. An
// entry with no content is storable as a zero-byte file, which is how a
// hole in a backup gets hidden behind a plausible-looking restore. The
// name half is checkEntryName above, shared with the restore that has to
// refuse the same names on the way out; what is left here is the half
// that is about an entry's CONTENT rather than its name.
func checkTreeEntry(e backupengine.SourceEntry) error {
	if err := checkEntryName(e.Name); err != nil {
		return fmt.Errorf("this source entry cannot be stored: %w", err)
	}

	switch {
	case e.Dir == nil && e.Stream == nil:
		return fmt.Errorf(
			"source entry %q has neither a directory nor a stream; there is nothing to read it from and a zero-byte file is not an honest answer", e.Name)
	case e.Dir != nil && e.Stream != nil:
		return fmt.Errorf(
			"source entry %q carries both a directory and a stream; which one is its content is exactly what the engine must not guess", e.Name)
	}

	return nil
}

// treeFileEntry presents one backupengine.StreamSource, named by the
// listing that produced it, as a Kopia fs.StreamingFile.
//
// # Why this is not streamingEntry
//
// The per-object type in stream.go answers the same two vendor interfaces
// and shares none of this one's inputs. Its metadata comes from the
// STREAM: it reports the stream's own ModTime and a Size of zero, because
// a single streamed object arrives with no listing to consult. A tree
// entry's metadata comes from the LISTING, which is the authority on what
// an entry is called and when it changed, while the stream underneath is
// only its content. And this one counts the bytes it pulls into the run's
// accumulator, because TreeSnapshotInfo reports source bytes read and
// StreamSnapshotInfo has no such field.
//
// Folding the two together would mean a nil-able run pointer, a branch
// deciding where ModTime comes from and a branch deciding whether Size is
// meaningful, on a type whose entire value is that it has no branches and
// therefore cannot be routed down Kopia's seekable path by accident. See
// streamingEntry's doc for that hazard: newDirEntry type-switches
// `case fs.File, fs.StreamingFile` with fs.File FIRST, so an entry that
// grew an Open method would silently leave the streaming path. Neither
// type has one.
type treeFileEntry struct {
	name    string
	size    int64
	modTime time.Time
	src     backupengine.StreamSource
	run     *treeRun
}

var _ kopiafs.StreamingFile = (*treeFileEntry)(nil)

func (e *treeFileEntry) Name() string               { return e.name }
func (e *treeFileEntry) Mode() os.FileMode          { return streamPermissions }
func (e *treeFileEntry) ModTime() time.Time         { return e.modTime }
func (e *treeFileEntry) IsDir() bool                { return false }
func (e *treeFileEntry) Sys() any                   { return nil }
func (e *treeFileEntry) Owner() kopiafs.OwnerInfo   { return kopiafs.OwnerInfo{} }
func (e *treeFileEntry) Device() kopiafs.DeviceInfo { return kopiafs.DeviceInfo{} }

// Size reports what the LISTING said, and a source that reports no size
// reports zero here rather than a negative number, which is not a size
// and which os.FileInfo has no way to express.
//
// It is advisory in both directions. Kopia overwrites DirEntry.FileSize
// with the number of bytes it actually copied
// (uploadStreamingFileInternal), so a listing that lied does not produce
// a manifest that lies; and nothing here reuses an entry on the strength
// of this number, because no previous manifests are passed.
func (e *treeFileEntry) Size() int64 {
	if e.size < 0 {
		return 0
	}

	return e.size
}

// LocalFilesystemPath returns "" because there is no local path. This is
// the method a staging implementation would have to fill in, and its
// emptiness is what says none exists: the source's bytes go from the
// transport into the repository without ever being a file on this
// machine.
func (e *treeFileEntry) LocalFilesystemPath() string { return "" }

// Close is required by fs.Entry and must be idempotent. The entry holds
// nothing; the reader it handed out is closed through its own Close.
func (e *treeFileEntry) Close() {}

// GetReader opens the source, once, when Kopia reaches this entry.
//
// Opening lazily rather than when the listing produced the entry is what
// keeps a failed run from having touched every object in the set: if the
// upload fails before it gets here, this source was never opened and
// there is nothing to leak.
func (e *treeFileEntry) GetReader(ctx context.Context) (io.ReadCloser, error) {
	rc, err := e.src.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening source entry %q: %w", e.name, err)
	}

	// guardedReader first, counter outside it: the counter then reports
	// bytes that actually reached the uploader, and the guard still gets
	// to convert a torn-down read into the caller's own cancellation
	// error rather than "use of closed network connection".
	g := &guardedReader{ctx: ctx, rc: rc, track: &e.run.streams}
	e.run.streams.add(g)

	return &countingReader{rc: g, read: &e.run.read}, nil
}

// countingReader is the SourceBytesRead half of TreeSnapshotInfo.
//
// It exists because "what did this run pull off the source" is a
// different number from both "how big is the tree" and "what did we push
// into storage", and the only place it can be observed is here, between
// the source's reader and the uploader's copy loop. A run that reported
// the tree's logical size as bytes read would be reporting an estimate
// made before anything was read.
type countingReader struct {
	rc   io.ReadCloser
	read *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.read.Add(int64(n))
	}

	//nolint:wrapcheck // the reader below is the source's; its errors are its own.
	return n, err
}

func (c *countingReader) Close() error {
	//nolint:wrapcheck // the reader below is the source's; its errors are its own.
	return c.rc.Close()
}
