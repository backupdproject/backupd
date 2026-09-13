package source

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/backupdproject/backupd/core/internal/backend"
	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/sourceconsistency"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// Tree is one pull-shaped pass over a backup source, for one snapshot of
// one backup set: the production path #783 asked for, and the answer to
// the question Sink in sink.go says it cannot answer.
//
// # Why this exists at all, when Sink already reads a source safely
//
// Sink is push-shaped. The adapter walks, opens an object, hands the
// bytes over, and undoes the store with Discard when the read behind it
// turns out to have been torn. Every one of those three verbs assumes
// the caller owns the walk and that a snapshot's unit is an object.
// backupengine.TreeRepository assumes neither: its uploader owns the
// walk, asks a directory for its children when it gets to them, and
// produces ONE manifest for the whole set - so there is nothing
// per-object to discard, and no point in the flow at which this package
// could decide a snapshot starts. The control flow has to invert, and
// this type is the inversion. What does NOT change is everything that
// made the push path trustworthy: path refusal, the exclusion matcher,
// the symlink policy, the special-file skip, one transport session per
// run, and the post-read mutation check. Those are reused here from the
// same functions the push path calls, not reimplemented, which is the
// only way "the reading path survives the port change unaltered" is a
// fact rather than an aspiration.
//
// # The shape, and why it is a producer plus a cursor
//
// The source is enumerated exactly once, by transport.Enumerator, in a
// producer goroutine. The consumer - the engine - sees a tree of
// directories reconstructed from that single forward pass. Two
// properties of the enumeration make this possible and are what the
// design rests on:
//
//   - It is DEPTH-FIRST GROUPED. transport.LocalEnumerator descends into
//     a subdirectory the moment it sees it and finishes that subtree
//     before resuming the parent (its own doc says so, and its frame
//     stack is why). So a single forward cursor plus one open frame per
//     level is enough to place every entry: a path either belongs to the
//     directory being iterated, or to a subdirectory of it that is being
//     entered now, or to something the walk has already left - and the
//     third answer is the end of the current directory.
//   - It is BOUNDED. One chunk of entries per level, never one entry per
//     file, which is the allocation transport/enumerate.go exists to
//     refuse. Nothing here buffers the walk, so nothing here gives that
//     back: the handoff below is unbuffered and holds exactly one entry.
//
// The first property is an ASSUMPTION about a backend, and assumptions
// about backends are what this package refuses to make on trust. It is
// therefore enforced rather than believed: each open frame remembers the
// child directory names it has already descended into (bounded by that
// directory's subdirectory count, which is the same order of memory
// Kopia's own per-directory manifest builder holds while it writes one),
// and a path that would require re-entering a directory this walk has
// already left fails the run with ErrEnumerationNotGrouped. A
// mis-shaped snapshot - two "a" directories, or a late entry silently
// dropped - must be impossible; a loud refusal is the acceptable
// outcome, and the backend that provokes it is backed up from a path
// list instead.
//
// # Why the producer waits for the engine
//
// Each surviving entry is handed over an UNBUFFERED channel and the
// producer then WAITS until the engine has finished with it. That is not
// throttling, it is the mutation check: the post-read stat has to be
// compared against the pre-read stat plus the byte count the read
// actually produced, and it has to happen while the run is still in
// flight, because a set-wide snapshot has no per-object handle to
// withdraw afterwards. So the producer holds the entry's "before" state,
// releases when the consumer asks for the next entry, checks, and either
// carries on or fails the RUN.
//
// That is also what the consumer has to be: SERIAL. One entry is in
// flight at a time, and the next question asked of this tree has to come
// after the engine has finished with the last entry it was handed. A
// forward cursor cannot serve two pullers - where it is IS the walk's
// position - so an uploader that wants concurrency gets it below this
// boundary, on content it has already been handed, and a walk that pulls
// from two frames at once is refused by name rather than answered
// wrongly (ErrConcurrentWalk).
//
// # The retry unit is the RUN, not the object
//
// Options.MaxAttempts is therefore IGNORED here, deliberately and
// explicitly. It bounds re-reads of one object, which the push path can
// do because it owns the walk and can discard what it stored; a tree run
// cannot re-offer an entry the uploader has already consumed, and a
// second read of one file is not a second attempt at the snapshot. A
// torn read fails the whole run, the operator's remedy is another run,
// and pretending otherwise by looping here would hand the engine the
// same entry twice. Options.Concurrency is ignored for the same kind of
// reason: this walk is one cursor, and the parallelism that matters now
// lives in the uploader below the boundary.
//
// # What this bridge cannot see, stated out loud
//
// An EMPTY directory does not reach the snapshot. transport.Enumerator
// descends into a directory rather than yielding it, so a directory with
// no files anywhere beneath it is invisible to every consumer of that
// interface, this one included; the directories in the tree are the ones
// implied by the paths of the entries that arrive. The same applies to a
// directory all of whose entries were excluded or refused. This is a
// real fidelity gap, it belongs to the enumerator's contract rather than
// to this file, and it is written here because a reader of this code
// would otherwise assume the opposite.
//
// An entry that has been announced to the engine cannot be withdrawn.
// The push path counts a file that vanished between the listing and the
// read as Vanished and carries on; here the engine has already been
// handed the entry, so the failed open (or the post-read stat that finds
// nothing at the path) fails the run. That asymmetry is inherent to
// pulling, and the run report says which path it was.
type Tree struct {
	adapter *Adapter

	// run is the reporting and mutation-checking half of a backup,
	// shared verbatim with the push path: settled, refuse,
	// recordFailure, recordStored, count, note, emit and snapshot are
	// the same code producing the same Report under the same mutex.
	//
	// Its sink field is nil and stays nil. Nothing a tree run reaches
	// touches it - store, capture and handleSymlink are the push path's
	// entry points and none of them is called from this file - and a
	// tree run has nothing to push to by construction, which is the
	// whole reason this type exists. Constructing the run here rather
	// than through newRun is what that nil requires: newRun refuses a
	// missing sink, correctly, for the callers that have one.
	run *run

	// parent is the context OpenTree was given and runCtx is derived
	// from it. Holding a context in a struct is ordinarily a smell; here
	// it is the RUN's lifetime, which is exactly the thing this type is,
	// and the producer goroutine plus every transport operation the walk
	// performs has to be bounded by it. parent is kept because "who
	// cancelled" is a question fail has to answer: a cancellation the
	// caller asked for is the run's error, and one Close produced has a
	// better sentence than "context canceled".
	parent context.Context //nolint:containedctx // the run's lifetime IS this object; see above.
	runCtx context.Context //nolint:containedctx // as above.
	cancel context.CancelFunc

	// items is the unbuffered handoff. One entry is in flight at a time:
	// the producer is blocked on the consumed channel of whatever it
	// last sent, which is what puts the mutation check inside the run.
	items chan *treeItem

	// producerDone is closed when the producer goroutine has RETURNED,
	// which is the thing Close waits for. Nothing may use the transport
	// session after it closes, so "the producer has stopped" has to be
	// observable rather than assumed.
	producerDone chan struct{}

	root *treeDir

	mu        sync.Mutex
	err       error
	completed bool

	// walkMu serialises the cursor and every frame's state. The engine
	// is entitled to hold several iterators at once - it holds one per
	// level of the tree it is walking - and one lock over the cursor is
	// what makes a forward cursor safe to share between them. It is a
	// separate lock from mu because mu is read by the producer while the
	// consumer holds this one.
	walkMu  sync.Mutex
	pending *treeItem
	current *treeItem
	drained bool

	closeOnce sync.Once
}

// ErrNoEnumerator and friends are the tree path's own refusals.
var (
	// ErrNoEnumerator is the wiring mistake of asking for a tree walk
	// from an adapter that has no way to list a directory. It is
	// separate from the capability refusals because it is a bug in the
	// caller rather than a fact about a backend.
	ErrNoEnumerator = errors.New("source: the adapter has no enumerator, so it cannot walk a source as a tree")

	// ErrEnumerationNotGrouped is the refusal for a backend whose
	// enumeration is not depth-first grouped.
	//
	// It is a refusal and not a fallback because the only fallbacks are
	// worse: buffering the walk to sort it is the unbounded allocation
	// bounded enumeration exists to prevent, re-opening a directory the
	// cursor has left needs a rewind no forward listing has, and
	// materialising a second directory with the same name produces a
	// snapshot that restores to a tree the source never had. A source on
	// such a backend is backed up from a path list.
	ErrEnumerationNotGrouped = errors.New("source: this backend's enumeration is not depth-first grouped, so this bridge cannot reconstruct it as a tree")

	// ErrSourceMutated is the failure for an object that moved while it
	// was being read into a set-wide snapshot.
	//
	// It fails the RUN. The push path can store an object and take the
	// restore point away again with Discard; a tree snapshot has one
	// manifest for the whole set, so the honest answer is that the
	// manifest is never written and the operator's remedy is another
	// run. That is the retry-unit change #783 makes, in one sentinel.
	ErrSourceMutated = errors.New("source: an object moved while it was being read, and a set-wide snapshot has nothing per-object to discard")

	// ErrTreeAbandoned is what a run reports when the CONSUMER stopped
	// pulling before the source ran out: a directory iterator closed
	// short of its end, or Close called mid-walk.
	//
	// It exists so that "the walk did not finish" is distinguishable
	// from "the source failed". Both mean no snapshot may be advertised,
	// and only one of them is a fact about the operator's data.
	ErrTreeAbandoned = errors.New("source: the tree walk was abandoned before the source ran out, so nothing about it is proven")

	// ErrDirReopened is the refusal for a second pass over a directory.
	// backupengine.SourceDir states that iteration is one-shot, and this
	// is that statement enforced: a forward cursor over a remote listing
	// cannot rewind, and replaying one would mean retaining the buffer
	// this package refuses to retain.
	ErrDirReopened = errors.New("source: this directory's iteration has already begun and a source listing cannot be rewound")

	// ErrConcurrentWalk is the refusal for a consumer that pulls from
	// two places at once.
	//
	// A tree over ONE forward cursor can only serve one entry at a time:
	// where the cursor is IS the walk's position, so a second puller has
	// nothing to be served from. The reason this is a named refusal
	// rather than a comment is what would otherwise happen - the entry
	// still being read would be declared finished by somebody else's
	// Next, its post-read check would compare a partial byte count
	// against the source's full size, and the run would fail with
	// ErrSourceMutated naming a file nothing had touched. An operator
	// would then go looking for a mutation on their source that never
	// happened. An uploader that wants to parallelise does it below this
	// boundary, on content it has already been handed.
	ErrConcurrentWalk = errors.New("source: a tree over one forward source listing serves one entry at a time, and this walk asked for two")
)

// treeItem is one entry in flight between the producer and the engine.
//
// It carries the "before" facts as well as the stream, because the
// producer is the thing that will check the read window and it must
// compare against what the LISTING said rather than against a fresh stat
// it could take after the read - two stats after a read prove nothing
// about what happened during it.
type treeItem struct {
	path    string
	name    string
	art     transport.RemoteArtifact
	kind    sourceconsistency.Kind
	size    int64
	modTime time.Time
	stream  backupengine.StreamSource

	// bytesRead is what the read actually produced, and closeAll closes
	// whatever readers the stream handed out. The second is how a
	// cancelled run releases a read blocked on a source that has stopped
	// answering: a context is not something a blocked Read consults.
	bytesRead func() int64
	closeAll  func()

	// consumed is closed by the consumer when the engine has finished
	// with this entry. It is the producer's cue to perform the post-read
	// check, and it is the reason the handoff is unbuffered.
	consumed chan struct{}

	// owner is the frame that handed this entry to the engine, set by
	// the consumer side and read by nothing else. In a serial
	// depth-first walk the frame asked for the next entry is ALWAYS the
	// one that produced the last: a file entry cannot be descended into,
	// so whatever the engine does with it, the next question it can ask
	// is of the directory the file was in - even when the answer is
	// end-of-directory. Anything else is two pullers, and
	// ErrConcurrentWalk says so.
	owner *treeIterator
}

// OpenTree begins one pull-shaped pass over a source and returns the tree
// the engine walks.
//
// Every refusal newRun and Backup make, this makes, and it makes them
// BEFORE anything is dialed: a capability a backend does not have is not
// a fact that improves after a connection, and a refusal that arrived
// after the session was open would have spent the dial to learn nothing.
// Enumerable is among them, which Backup also checks and BackupPaths does
// not: a tree walk is a walk, so a backend with no resumable directory
// cursor is refused here rather than discovered halfway down a source.
//
// The caller owns ctx and must keep it alive for the whole walk: it
// bounds the producer, the transport session and every read the engine
// performs through this tree. Close is required on every path out,
// including the failing ones, and is what releases the session.
func (a *Adapter) OpenTree(ctx context.Context, src transport.Source) (*Tree, error) {
	lookup := a.deps.Profiles
	if lookup == nil {
		lookup = ProfileFor
	}

	profile, err := lookup(src.Type)
	if err != nil {
		return nil, err
	}

	if err := profile.Streamable(); err != nil {
		return nil, err
	}

	if err := profile.MutationDetectable(a.opts.Mode); err != nil {
		return nil, err
	}

	// A tree walk needs bounded listing, and this is the only place that
	// can say so before the memory is spent: a walk of a backend with no
	// resumable directory cursor materialises the directory in the layer
	// underneath before any code here could count it.
	if err := profile.Enumerable(); err != nil {
		return nil, err
	}

	if !a.opts.Mode.GuaranteesPointInTime() && a.deps.Stater == nil {
		return nil, fmt.Errorf("%w: consistency mode %q does not promise the source holds still", ErrNoStater, a.opts.Mode)
	}

	if a.opts.Symlinks == SymlinkPreserve && profile.SymlinkSemantics() != backend.SymlinksStored {
		return nil, fmt.Errorf(
			"source: the symlink policy is %q and backend %q declares symlink_semantics %q, so storing a link would mean inventing a capability it does not have",
			SymlinkPreserve, profile.BackendID, profile.SymlinkSemantics())
	}

	if a.deps.Enumerator == nil {
		return nil, ErrNoEnumerator
	}

	trust := model.ClassifyMetadataTrust(profile.Signals())

	r := &run{
		adapter:  a,
		profile:  profile,
		source:   src,
		excluded: profile.ExcludeMatcher(src.ExcludePaths),
		report: Report{
			Backend: profile.BackendID,
			Trust:   trust,
			Policy:  model.VerificationPolicyFor(trust.Class, a.opts.Preset),
		},
	}

	runCtx, cancel := context.WithCancel(ctx)

	// One conversation with the source for the whole run, opened before
	// the first entry is even described and closed by Close. A session
	// that cannot be opened fails the RUN rather than every entry in it,
	// which is the same argument newSourceReader's doc makes.
	reader, err := a.newSourceReader(runCtx, src)
	if err != nil {
		cancel()

		return nil, err
	}

	r.reader = reader

	t := &Tree{
		adapter:      a,
		run:          r,
		parent:       ctx,
		runCtx:       runCtx,
		cancel:       cancel,
		items:        make(chan *treeItem),
		producerDone: make(chan struct{}),
	}
	t.root = &treeDir{tree: t, dir: ""}

	go t.produce()

	return t, nil
}

// Root is the tree's top directory, and is the same directory every time:
// two Roots would be two passes over one listing, which Open refuses.
func (t *Tree) Root() backupengine.SourceDir { return t.root }

// Report returns a consistent copy of what the run has established so
// far. It is safe during the walk and after it, under the same mutex
// discipline the push path's report is kept under, because a progress
// surface asking "how far has this got" must not read a half-updated
// counter set.
func (t *Tree) Report() Report { return t.run.snapshot() }

// Err is the run-level failure, and nil means the walk reached the end of
// the source with every entry's read window proven.
//
// It is the answer for all four ways a tree run can fail, and a caller
// branching on them can: the enumeration failed (the transport's own
// error), an object moved under its read (ErrSourceMutated), the backend
// enumerates in a shape this bridge cannot place
// (ErrEnumerationNotGrouped), or the consumer stopped pulling
// (ErrTreeAbandoned). The last one is the one worth knowing about: it
// means the engine gave up, not the source, so an operator reading the
// report is looking at the wrong end of the system.
func (t *Tree) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.err
}

// Close stops the producer and closes the transport session, exactly
// once, on every path out of a walk.
//
// The order is the contract: cancel, WAIT for the producer to return,
// then close the session. Closing the session first would leave the
// producer stat'ing over a connection that is not there any more, which
// on a real transport is an error nobody asked for, attributed to the
// source.
//
// It returns the same value Err does, so a caller that wants one error
// check can take it from the deferred Close. A walk that was closed
// before the source ran out reports ErrTreeAbandoned: a partially pulled
// tree is not a snapshot, and the one thing this must never do is let a
// consumer's own abandonment look like a clean run.
func (t *Tree) Close() error {
	t.closeOnce.Do(func() {
		t.cancel()
		<-t.producerDone

		t.run.reader.close()

		t.mu.Lock()
		if t.err == nil && !t.completed {
			t.err = fmt.Errorf("%w: the tree was closed while the source still had entries to offer", ErrTreeAbandoned)
		}
		t.mu.Unlock()
	})

	return t.Err()
}

// fail records the run's failure, keeping the FIRST one.
//
// First rather than last, because a run fails once and then everything
// downstream of it fails too: a torn read cancels the producer, which
// makes the enumerator report a cancellation, and a report that named
// the cancellation would have replaced the only sentence that explained
// anything.
//
// A cancellation of the run context while the CALLER's context is still
// live can only have come from Close, and Close has a better sentence
// for that than "context canceled" - so it is dropped here and named
// there. A cancellation the caller asked for is a real answer and is
// kept.
func (t *Tree) fail(err error) {
	if err == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.err != nil {
		return
	}

	if isCancellation(err) && t.parent.Err() == nil {
		return
	}

	t.err = err
}

func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// --- the producer --------------------------------------------------------

// produce enumerates the source once and offers what survives policy.
//
// It is the only goroutine this type starts, and it is joined by Close.
// The two closes on the way out are ordered and both matter: items first,
// so a consumer blocked waiting for an entry learns there will not be one
// and reads the error fail has already recorded, and producerDone last,
// so Close's wait means "this goroutine has gone" rather than "this
// channel is closed".
func (t *Tree) produce() {
	defer close(t.producerDone)
	defer close(t.items)

	opts := transport.EnumerateOptions{
		ChunkEntries: t.adapter.opts.ChunkEntries,
		// The same reason Backup gives: this adapter has a symlink
		// policy and a special-file policy, and a policy cannot be
		// applied to entries it is never told about.
		ReportNonRegular: true,
	}

	if err := t.adapter.deps.Enumerator.Enumerate(t.runCtx, t.run.source, opts, t.offer); err != nil {
		t.fail(err)

		return
	}

	// The enumeration reached the end of the source AND every entry it
	// offered has been read and checked, because offer does not return
	// until that has happened. There is nothing else "complete" could
	// mean here, and this is the only place it is set.
	t.mu.Lock()
	t.completed = true
	t.mu.Unlock()
}

// offer applies policy to one enumerated entry, hands what survives to
// the engine, and checks the read window once the engine is done with it.
//
// A policy skip returns nil: the entry is counted and the walk carries
// on, exactly as run.handle does. A run-level failure returns an error,
// which stops the enumeration at the entry that caused it.
func (t *Tree) offer(art transport.RemoteArtifact) error {
	item, ok, err := t.prepare(t.runCtx, art)
	if err != nil {
		t.fail(err)

		return err
	}

	if !ok {
		return nil
	}

	// closeAll on every path out, including the ones where the engine
	// never opened the stream at all: an objectStream that has been
	// closed refuses to open another reader, so an engine goroutine that
	// had not yet reached its Open cannot dial a source the run has
	// finished with.
	defer item.closeAll()

	select {
	case t.items <- item:
	case <-t.runCtx.Done():
		t.fail(t.runCtx.Err())

		return t.runCtx.Err() //nolint:wrapcheck // a cancelled run reports the cancellation.
	}

	select {
	case <-item.consumed:
	case <-t.runCtx.Done():
		// The deferred closeAll is what releases a read that is blocked
		// on a source which will never answer. This is the push path's
		// watcher goroutine, without the goroutine: the producer is
		// already parked beside the read and can do the job itself.
		t.fail(t.runCtx.Err())

		return t.runCtx.Err() //nolint:wrapcheck // as above.
	}

	// Closed before the re-stat, not after. A reader still open is a
	// read still in progress as far as the source is concerned, and the
	// question being asked is what the object looks like now that the
	// read is over.
	item.closeAll()

	return t.check(item)
}

// check compares the object's state after the read against its state
// before, plus the bytes the read actually produced, and fails the run
// when they disagree.
//
// It is run.settled doing the work, which is the point: the tree path
// changed what happens to a torn read, not how a tear is detected. The
// Stored value handed to it carries the measured byte count because
// there is no sink in a tree run to report a second number - the branch
// of settled that catches a sink lying about a length has nothing to
// compare here, and passing the measurement as both arguments says so
// rather than inventing a zero for it to reject.
func (t *Tree) check(item *treeItem) error {
	before := t.run.profile.StatOf(item.art)
	before.Kind = item.kind

	read := item.bytesRead()

	settled, why := t.run.settled(t.runCtx, item.art, &before, Stored{Bytes: read}, read)
	if !settled {
		torn := fmt.Errorf("%w: %s", ErrSourceMutated, why)
		t.run.recordFailure(item.path, item.kind, sourceconsistency.OutcomeIncomplete, torn)

		// The engine is still pulling, so it learns about this through
		// the iterator it asks next: no manifest is written for a tree
		// whose entries could not all be proven.
		err := fmt.Errorf("%s: %w", item.path, torn)
		t.fail(err)

		return err
	}

	// One attempt, always. A tree run does not re-read an object (see
	// the type's doc on why MaxAttempts does not apply), so the attempt
	// count a report carries is the number of entries it read, and there
	// is no per-object handle in a set-wide snapshot for StoredID to
	// hold.
	t.run.recordStored(item.path, item.kind, 1, Stored{Bytes: read}, why)

	return nil
}

// prepare applies every decision that can be made without reading, and
// describes what survives as an entry the engine can pull.
//
// It is the same cascade run.handle applies, in the same order, for the
// same reasons - refuse the path before it is opened, exclusion before
// policy, kind before content - and it stops where run.handle hands over
// to the push path's store. The duplication is the inversion: run.handle
// ends by CALLING the thing that reads, and this ends by describing
// something that will be read later by somebody else.
func (t *Tree) prepare(ctx context.Context, art transport.RemoteArtifact) (*treeItem, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err //nolint:wrapcheck // a cancelled run reports the cancellation.
	}

	safe, err := SafeRelPath(art.Path)
	if err != nil {
		// Refused before it is opened: a name designed to escape the
		// root never reaches the layer that would resolve it.
		t.run.refuse(art.Path, err)

		return nil, false, nil
	}

	art.Path = safe

	if t.run.excluded(safe) {
		t.run.count(func(rep *Report) { rep.Entries++; rep.SkippedExcluded++ })

		return nil, false, nil
	}

	kind := KindOf(art.Kind)
	switch kind {
	case sourceconsistency.KindSymlink:
		return t.prepareSymlink(ctx, art)
	case sourceconsistency.KindOther:
		// A socket, a fifo or a device node, which is never opened -
		// and "never" is the point rather than the tidiness, because a
		// fifo with no writer blocks in Read until something writes to
		// it. Describing one as an entry would hand the engine a stream
		// that hangs the backup window.
		t.run.count(func(rep *Report) { rep.Entries++; rep.SkippedSpecial++ })
		t.run.emit(Result{
			Path:    safe,
			Kind:    kind,
			Outcome: sourceconsistency.OutcomeNotAFile,
			Reason:  "a socket, fifo or device node, which holds no content this product copies and is never opened",
		})

		return nil, false, nil
	case sourceconsistency.KindDir:
		// A directory never arrives here: the enumerator descends into
		// one rather than yielding it, and the directories in this tree
		// are the ones the arriving PATHS imply. It is named so the
		// switch is exhaustive over the vocabulary rather than falling
		// through silently if a future enumerator does yield one.
		t.run.count(func(rep *Report) { rep.Entries++; rep.SkippedSpecial++ })

		return nil, false, nil
	case sourceconsistency.KindRegular:
		stream := newObjectStream(t.run.reader, safe, t.run.profile.ModTime(art.ModTime))

		return t.newItem(art, kind, stream, stream.BytesRead, stream.closeAll, t.entrySize(art)), true, nil
	}

	// KindOf answers one of the four above and nothing else, so this is
	// the line the compiler needs rather than a case anything reaches.
	// It fails the run instead of dropping the entry: an entry nobody
	// has a policy for is a hole in a backup, and a hole that reports
	// itself is the only kind worth having.
	return nil, false, fmt.Errorf(
		"source: the enumerator reported kind %q for %q, which this walk has no policy for", art.Kind, safe)
}

// prepareSymlink applies the symlink policy. Nothing here follows a link
// under either policy; the difference is whether the link itself becomes
// an entry.
func (t *Tree) prepareSymlink(ctx context.Context, art transport.RemoteArtifact) (*treeItem, bool, error) {
	if t.adapter.opts.Symlinks == SymlinkIgnore {
		t.run.count(func(rep *Report) { rep.Entries++; rep.SkippedSymlink++ })
		t.run.emit(Result{
			Path:    art.Path,
			Kind:    sourceconsistency.KindSymlink,
			Outcome: sourceconsistency.OutcomeNotAFile,
			Reason:  "a symbolic link, which this run's symlink policy skips and which is never followed",
		})

		return nil, false, nil
	}

	target, err := t.adapter.deps.Links.ReadSourceLink(ctx, t.run.source, art.Path)
	if err != nil {
		// The link is not in the snapshot and the report says so. It
		// does not fail the run: a link this process could not read is
		// one entry the backup does not contain, which Report.Complete
		// already refuses to call complete, and failing a whole set's
		// snapshot over it would be a bigger answer than the question.
		t.run.recordFailure(art.Path, sourceconsistency.KindSymlink, statOutcome(err), err)

		return nil, false, nil
	}

	if err := linkTargetWithinRoot(art.Path, target); err != nil {
		t.run.refuse(art.Path, err)

		return nil, false, nil
	}

	// The link is described as its target, which is what a symbolic link
	// IS: a small file whose content is a path. Nothing resolves it, so a
	// link to a file that does not exist is stored exactly as faithfully
	// as one that does.
	stream := newLiteralStream([]byte(target), t.run.profile.ModTime(art.ModTime))

	// The size is the target's length rather than whatever the source
	// reports for a link, because the target's length is what the engine
	// is about to read. A link's own reported size is the target length
	// on one backend and zero on another, and the scanned-bytes
	// accounting would be wrong on one of them either way.
	return t.newItem(art, sourceconsistency.KindSymlink, stream, stream.BytesRead, func() {}, int64(len(target))), true, nil
}

func (t *Tree) newItem(
	art transport.RemoteArtifact,
	kind sourceconsistency.Kind,
	stream backupengine.StreamSource,
	bytesRead func() int64,
	closeAll func(),
	size int64,
) *treeItem {
	return &treeItem{
		path:      art.Path,
		name:      path.Base(art.Path),
		art:       art,
		kind:      kind,
		size:      size,
		modTime:   stream.ModTime(),
		stream:    stream,
		bytesRead: bytesRead,
		closeAll:  closeAll,
		consumed:  make(chan struct{}),
	}
}

// entrySize is what the listing said, or -1 where the backend's reported
// size does not describe the bytes a read returns.
//
// The negative is the honest answer and backupengine.SourceEntry asks for
// it by name: a number carried from a backend whose matrix says its sizes
// are not stable would be read downstream as a fact about content, which
// is the fabrication Profile exists to prevent.
func (t *Tree) entrySize(art transport.RemoteArtifact) int64 {
	if !t.run.profile.StableSize() {
		return -1
	}

	return art.Size
}

// --- the consumer's side: a tree over one forward cursor -----------------

// treeDir is one directory of the reconstructed tree.
//
// It holds a path and nothing else. There is no buffer of children here
// and there cannot be: the children arrive from the cursor when the
// engine gets to them, which is what makes this bridge bounded by the
// tree's DEPTH rather than by its size.
type treeDir struct {
	tree *Tree
	dir  string

	// opened is guarded by tree.walkMu. It is what makes iteration
	// one-shot, and it is also read by the parent frame to tell "the
	// engine has not descended into this directory yet" from "this
	// backend enumerates out of order", which are two very different
	// accusations.
	opened bool
}

// Open begins this directory's one pass.
func (d *treeDir) Open(_ context.Context) (backupengine.SourceDirIterator, error) {
	t := d.tree

	if err := t.Err(); err != nil {
		return nil, err
	}

	t.walkMu.Lock()
	defer t.walkMu.Unlock()

	if d.opened {
		return nil, fmt.Errorf("%w: %s", ErrDirReopened, describeDir(d.dir))
	}

	d.opened = true

	return &treeIterator{tree: t, dir: d.dir, descended: map[string]struct{}{}}, nil
}

// treeIterator is one pass over one directory, served from the run's
// single forward cursor.
type treeIterator struct {
	tree *Tree
	dir  string

	// descended is the child directory names this frame has already
	// entered. It is the enforcement of the depth-first-grouped
	// assumption, and it is bounded by this directory's subdirectory
	// count: the same order of memory Kopia's own per-directory manifest
	// builder holds while it writes one, and nothing like a set of every
	// path in the source.
	descended map[string]struct{}

	// lastChild is the subdirectory this frame handed out most recently,
	// kept only so that "the engine never descended" can be told apart
	// from "the source came back to a directory we had left".
	lastChild *treeDir

	ended  bool
	closed bool
}

// Next returns the next child of this directory.
//
// The three answers are the three relationships an arriving path can
// have with this directory, and that is the whole algorithm: the path is
// IN it (a file entry), the path is BELOW it (the subdirectory being
// entered now), or the path is somewhere else entirely - which, from a
// depth-first grouped enumeration, means this directory is finished.
func (it *treeIterator) Next(ctx context.Context) (backupengine.SourceEntry, bool, error) {
	t := it.tree

	t.walkMu.Lock()
	defer t.walkMu.Unlock()

	// A run that has already failed hands out nothing else, whatever the
	// cursor happens to be holding. The engine is pulling, so this is
	// where it learns; anything else would let a snapshot be assembled
	// out of entries collected after the run was known to be broken.
	if err := t.Err(); err != nil {
		return backupengine.SourceEntry{}, false, err
	}

	if it.closed {
		return backupengine.SourceEntry{}, false, fmt.Errorf(
			"source: %s was asked for another entry after its pass was closed", describeDir(it.dir))
	}

	if it.ended {
		return backupengine.SourceEntry{}, false, nil
	}

	if err := t.ensurePending(ctx, it); err != nil {
		return backupengine.SourceEntry{}, false, err
	}

	if t.pending == nil {
		// The source ran out. Every frame still open ends here, which is
		// how the engine unwinds out of a tree it walked to the bottom
		// of.
		it.ended = true

		return backupengine.SourceEntry{}, false, nil
	}

	item := t.pending
	parent := parentDir(item.path)

	switch {
	case parent == it.dir:
		t.pending = nil
		item.owner = it
		t.current = item

		return backupengine.SourceEntry{
			Name:    item.name,
			ModTime: item.modTime,
			Size:    item.size,
			Stream:  item.stream,
		}, true, nil

	case Contains(it.dir, parent):
		name := firstElementBelow(it.dir, parent)

		if _, seen := it.descended[name]; seen {
			return backupengine.SourceEntry{}, false, it.refuseRepeat(name, item.path)
		}

		it.descended[name] = struct{}{}

		child := &treeDir{tree: t, dir: joinDir(it.dir, name)}
		it.lastChild = child

		// The cursor does not move: this entry is a directory the
		// arriving path IMPLIES, and the path itself is delivered once
		// the engine has descended far enough to hold the frame it
		// belongs in.
		//
		// A directory has no metadata here at all. The enumerator
		// descends into one rather than yielding it, so there is no
		// stat to carry: the zero time means the source reports none,
		// and -1 means the same about a size.
		return backupengine.SourceEntry{Name: name, Size: -1, Dir: child}, true, nil

	default:
		it.ended = true

		return backupengine.SourceEntry{}, false, nil
	}
}

// refuseRepeat names the two ways a frame can be asked for a child
// directory it has already handed out, and they are not the same fault.
//
// If the engine never opened the directory it was given, the engine is
// the one that stopped walking and the source is fine. If it did open it
// and the cursor has come back, the ENUMERATION is not depth-first
// grouped and this bridge cannot place what is arriving. Reporting the
// second sentence for the first situation would send somebody to audit a
// backend over a consumer's bug.
func (it *treeIterator) refuseRepeat(name, arriving string) error {
	left := joinDir(it.dir, name)

	if it.lastChild != nil && it.lastChild.dir == left && !it.lastChild.opened {
		err := fmt.Errorf(
			"%w: %s was offered as a child of %s and never opened, so %q has nowhere to go",
			ErrTreeAbandoned, describeDir(left), describeDir(it.dir), arriving)
		it.tree.fail(err)

		return err
	}

	err := fmt.Errorf(
		"%w: %q arrived after the walk had already finished %s, so placing it would need a second %q directory or would drop it",
		ErrEnumerationNotGrouped, arriving, describeDir(left), name)
	it.tree.fail(err)

	return err
}

// Close releases this pass.
//
// A pass closed before its directory ended is the consumer giving up,
// and it is recorded as such: the engine has stopped walking a directory
// the source had more of, so nothing assembled from this walk is a
// complete snapshot. It still returns nil, because the engine closes
// iterators in a defer on its own error paths and an error here would
// replace the explanation it already has.
//
// The recording is skipped once the run context is done, and that is
// deliberate: at that point the run already has a cancellation or a
// failure to report, and an abandonment note racing against it would
// make which of the two Err returns a matter of scheduling.
func (it *treeIterator) Close() error {
	t := it.tree

	t.walkMu.Lock()
	defer t.walkMu.Unlock()

	if it.closed {
		return nil
	}

	it.closed = true

	if !it.ended && t.runCtx.Err() == nil {
		t.fail(fmt.Errorf("%w: the pass over %s was closed while the source still had entries for it",
			ErrTreeAbandoned, describeDir(it.dir)))
	}

	return nil
}

// ensurePending makes sure the cursor is holding the next entry, and is
// the point at which the previous one is declared finished.
//
// Called with walkMu held, by the frame that is asking. Releasing the
// producer HERE - when the engine asks for the next entry, rather than
// when it closes a reader - is what makes the post-read check
// meaningful: the engine has read what it wanted of that entry and moved
// on, so the bytes the read produced are final, and the producer can
// compare them against what the source says now while the run is still
// in flight.
//
// Which frame is asking therefore matters, and is checked rather than
// assumed: releasing an entry because a DIFFERENT frame wanted something
// would declare a read finished while it was still running, and the
// post-read check would then blame the source for a byte count the
// engine had not finished producing.
func (t *Tree) ensurePending(ctx context.Context, asking *treeIterator) error {
	if t.pending != nil {
		return nil
	}

	if t.current != nil {
		if t.current.owner != asking {
			err := fmt.Errorf(
				"%w: %s was asked for an entry while %q, handed to %s, was still being read",
				ErrConcurrentWalk, describeDir(asking.dir), t.current.path, describeDir(t.current.owner.dir))
			t.fail(err)

			return err
		}

		close(t.current.consumed)
		t.current = nil
	}

	if t.drained {
		return t.Err()
	}

	select {
	case item, ok := <-t.items:
		if !ok {
			// The producer has finished. Either it reached the end of
			// the source, or it failed - and if it failed it recorded
			// the reason BEFORE closing this channel, so the error is
			// already there to be read.
			t.drained = true

			return t.Err()
		}

		t.pending = item

		return nil
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // the engine's own context is the engine's own answer.
	case <-t.runCtx.Done():
		return t.runCtx.Err() //nolint:wrapcheck // a cancelled run reports the cancellation.
	}
}

// --- path arithmetic over the cursor ------------------------------------

// parentDir is the directory an entry belongs to, with the root spelled
// as the empty string rather than "." so that one comparison works at
// every level.
func parentDir(p string) string {
	d := path.Dir(p)
	if d == "." || d == "/" {
		return ""
	}

	return d
}

// firstElementBelow is the single name that leads from dir towards
// parent, which is the subdirectory the walk is entering now. Both are
// already-safe slash paths and parent is strictly below dir.
func firstElementBelow(dir, parent string) string {
	rest := parent
	if dir != "" {
		rest = parent[len(dir)+1:]
	}

	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}

	return rest
}

func joinDir(dir, name string) string {
	if dir == "" {
		return name
	}

	return dir + "/" + name
}

// describeDir names a directory in an error an operator will read. The
// root has no name of its own in the source's namespace, and calling it
// "" in a sentence is how a message becomes unreadable.
func describeDir(dir string) string {
	if dir == "" {
		return "the source root"
	}

	return fmt.Sprintf("%q", dir)
}
