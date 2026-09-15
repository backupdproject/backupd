package source

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/sourceconsistency"
	"github.com/retnd/retnd/core/internal/transport"
)

// Streamer opens one object on a backup SOURCE for a single forward read.
//
// It is one method wide because that is all this package needs, and it is
// an interface rather than the rclone adapter itself for the reason ADR
// 0001 gives about boundaries: a package naming a concrete adapter
// depends on everything that adapter depends on, so the transport's whole
// backend set, its connection accounting and its dependency graph would
// arrive here through one field. internal/transport/rclone's Adapter
// satisfies this by having the method, and nothing in this file knows
// that.
type Streamer interface {
	OpenSourceStream(ctx context.Context, src transport.Source, remotePath string) (io.ReadCloser, error)
}

// Stater answers what the source says about one object, from its
// METADATA alone. It is the other half of the mutation check: the
// enumeration's answer is the "before", and this is the "after".
//
// The method is StatSource rather than Stat, and the distinct name is
// load-bearing. transport.Transport.Stat computes a content hash where
// the backend advertises one, and rclone's local backend advertises one
// by reading the whole file - so wiring that method here would make
// every post-read check re-read the object it had just streamed, and
// double the I/O of every backup on the quietest possible path. A
// separate name means the wrong one cannot be passed by accident: it
// does not satisfy the interface.
type Stater interface {
	StatSource(ctx context.Context, src transport.Source, remotePath string) (transport.RemoteArtifact, error)
}

// LinkReader is the capability a source has when it can report a symbolic
// link's target without following it. It is optional, and its absence is
// what makes SymlinkPreserve a refusal rather than a guess.
type LinkReader interface {
	ReadSourceLink(ctx context.Context, src transport.Source, remotePath string) (string, error)
}

// Object is one source object, prepared and ready to be stored: a safe
// path, the metadata the backend actually supports, and a stream of its
// bytes that has not been opened yet.
type Object struct {
	// Path is the safe, root-relative, slash-separated source path. It
	// has been through SafeRelPath; nothing downstream needs to check it
	// again.
	Path string

	// Kind is what the source said is here. A Sink only ever sees
	// KindRegular and, under SymlinkPreserve, KindSymlink; the adapter
	// applies its policies before anything reaches a sink.
	Kind sourceconsistency.Kind

	// Size is what the listing reported, and is a hint rather than a
	// promise: it is zero on a backend whose matrix does not declare
	// stable sizes, and it may be stale on any backend, which is the
	// premise of the whole mutation check. Nothing may truncate a read
	// to it.
	Size int64

	// ModTime is the modification time this backend is entitled to
	// claim, already projected through its declared precision. The zero
	// time means the backend keeps none; it is not the epoch.
	ModTime time.Time

	// Stream opens the bytes, once, forward.
	Stream backupengine.StreamSource

	// Attempt is which read of this object this is, from one. A sink
	// that logs or meters can tell a retry from a first pass.
	Attempt int
}

// Stored is what a sink reports after taking one object.
type Stored struct {
	// ID is the sink's handle for what it stored, opaque here. It is
	// what Discard is given when a read turns out to have been torn.
	ID string

	// Bytes is how many bytes the sink actually read from the stream.
	// This is the number the mutation check compares against the
	// source's own post-read size, so a sink that reports what it was
	// told rather than what it read defeats the check.
	Bytes int64

	// UploadedBytes is how much new data reached storage, which on a
	// re-run of unchanged content is near zero while Bytes is the whole
	// object.
	UploadedBytes int64
}

// Sink is where a prepared object's bytes go, for as long as an object is
// the unit a snapshot is taken of.
//
// It exists so that this package owns "read the source safely" and
// nothing else: what a stored object BECOMES is the sink's decision, and
// deciding differently must not re-open path safety, cancellation or
// mutation detection.
//
// It is deliberately not the port a tree snapshot can be written
// against, and pretending otherwise is a claim ADR 0012 used to make and
// no longer does. This interface is push-shaped: the caller walks, opens
// and hands over one object at a time, and undoes a store with Discard
// once the read behind it is found wanting. Kopia's tree upload is
// pull-shaped - the uploader walks an fs.Entry and asks for children -
// and a snapshot that spans a whole backup set has no per-object thing
// to discard, only a stream-level error to fail the run with.
//
// That port has LANDED (#783), and it is Tree in tree.go: Adapter.OpenTree
// inverts the control flow, hands the engine a
// backupengine.SourceDir it pulls from, and moves the discard decision to
// a run-level failure the uploader observes while it is pulling. Tree is
// the production path for a backup set. This interface stays because the
// per-object shape is still the thing this package's own tests read a
// source through end to end, and because the reading path both share -
// path safety, capability refusals, cancellation, mutation detection -
// is proven against it here; see RepositorySink for what that means for
// the interim implementation below.
type Sink interface {
	// Store reads obj.Stream to its end and returns what it stored. It
	// must not retry: the retry bound belongs to the adapter, and a sink
	// that retries multiplies it.
	Store(ctx context.Context, obj Object) (Stored, error)

	// Discard removes something Store returned, because the read that
	// produced it was never proven: it turned out to have been torn, or
	// the run was cancelled before anything could check. A sink that
	// cannot remove what it stored says so with an error, and the
	// adapter reports the object as incomplete AND names the artifact
	// that has to be dealt with, rather than leaving an unverified read
	// behind silently.
	//
	// It may be called with a context the run's own cancellation cannot
	// reach, which is deliberate: the discard that matters most is the
	// one a cancellation caused (see the adapter's discardUnverified).
	Discard(ctx context.Context, id string) error
}

// Tag keys every snapshot the production sink writes carries.
//
// They are how a repository that holds more than one backup set can be
// asked what is IN it: the set a snapshot belongs to, and the repository
// domain that set was admitted to (model.RepositoryRef is the pair). A
// repository counting its distinct Kopia "sources" instead would report
// this sink's per-object snapshots as thousands of tenants of one
// domain, which is a co-tenancy signal that says the opposite of the
// truth and is the reason these exist.
//
// The keys are backupengine.TagKeyBackupSet and
// backupengine.TagKeyDomain, plumbed through the snapshot port (#781).

// RepositorySink stores each object as one streamed snapshot in a backup
// repository.
//
// # This is the per-object interim, not the snapshot model
//
// One snapshot per object is what the streaming boundary offers:
// backupengine.StreamingRepository takes one stream and returns one
// snapshot id, so a backup set of 100k files becomes 100k snapshots,
// 100k manifests and 100k Kopia sources. That is affordable for the job
// this sink exists to do - prove the source-READ path end to end, over a
// real transport, into a real repository - and it is not the shape a
// backup set is stored in.
//
// The real port has LANDED (#783): one snapshot per backup-set RUN,
// under one SourceInfo (the set's model.SourceIdentity), with the
// objects as a tree inside it - backupengine.TreeRepository, fed by
// Adapter.OpenTree and the Tree in tree.go, which is what a production
// backup of a set now runs through. It is not a different
// implementation of this interface and ADR 0012 no longer claims it is.
// Kopia's tree upload is PULL-based - upload.Uploader walks an fs.Entry
// and asks it for children - while everything above this sink is
// PUSH-based: the adapter walks, hands each object to Store, checks the
// read window and Discards what moved. A tree sink cannot be written
// against Sink as declared above, which is why #783 replaced the port,
// inverted that control flow, and moved the discard decision from
// "remove the snapshot this object produced" to a run-level failure the
// uploader observes while it is pulling.
//
// What is left for this type is one job and it is not a production
// backup: it is the per-object path this package's own tests read a
// source through, including the integration test that drives a real
// repository, so that the reading path both ports share - path safety,
// capability refusals, cancellation, mutation detection - is proven
// against something that stores bytes and can take them away again.
// Tree reuses that reading path rather than reimplementing it, and this
// is where the half of it that needs a Discard to exist is exercised.
//
// The set tags below were the one piece of the #783 model seeded ahead
// of it: a snapshot that cannot be attributed to a backup set is
// invisible to the repository's own accounting, and that was worth
// fixing in the interim rather than after it.
type RepositorySink struct {
	// Repo is the open repository.
	Repo backupengine.StreamingRepository

	// Source is the identity the snapshots are stored under. Its Path is
	// the ROOT of the backup set in the repository's namespace; each
	// object is stored under it, so a source's objects list together and
	// two machines backing up the same pathname stay distinct.
	//
	// It is the repository's NAMESPACE for this set and not the set's
	// identity: two sets can legitimately be named the same way from two
	// machines, and a Kopia source is a (host, user, path) triple that
	// nothing validates. Ref is the identity.
	Source backupengine.Source

	// Ref is the backup set and the repository domain these snapshots
	// belong to, and it is required: Store refuses rather than writing a
	// snapshot nothing can attribute.
	//
	// A refusal is the right answer rather than an untagged snapshot
	// because the failure is silent in the other direction. An untagged
	// snapshot is stored, restorable and invisible to every question
	// asked by set - what does this domain hold, is this domain shared,
	// how much does this set cost - and it is invisible AFTER the backup
	// window, when the operator has already been told the run was fine.
	Ref model.RepositoryRef

	// Description and Tags are recorded with every snapshot this sink
	// writes. Tags is the caller's own selection vocabulary; it cannot
	// overwrite the identity tags, which are the repository's.
	Description string
	Tags        map[string]string
}

var _ Sink = RepositorySink{}

// Store writes one object as one streamed snapshot.
//
// MaxAttempts is pinned at 1, and that single field is this package's
// retry-boundary contract made structural. The engine will re-open a
// broken stream if asked; the adapter also retries; the transport's own
// backend retries underneath both. Three bounds of three multiply to
// twenty-seven reads of an object that is never going to be readable,
// with a backup window spent on one file and a report that says
// "attempted 3". The adapter is the one authority, so everything below it
// is asked for exactly one attempt.
func (s RepositorySink) Store(ctx context.Context, obj Object) (Stored, error) {
	tags, err := s.snapshotTags()
	if err != nil {
		return Stored{}, fmt.Errorf("storing %q: %w", obj.Path, err)
	}

	src := s.Source
	src.Path = repositoryPath(s.Source.Path, obj.Path)

	info, err := s.Repo.SnapshotStream(ctx, backupengine.StreamSnapshotRequest{
		Source:      src,
		Stream:      obj.Stream,
		Description: s.Description,
		Tags:        tags,
		MaxAttempts: 1,
	})
	if err != nil {
		return Stored{}, fmt.Errorf("storing %q: %w", obj.Path, err)
	}

	return Stored{
		ID:            string(info.ID),
		Bytes:         info.Bytes,
		UploadedBytes: info.UploadedBytes,
	}, nil
}

// snapshotTags is the caller's tags plus the two this sink owns.
//
// The identity tags are written LAST, so a caller that passes
// "backupd.set" itself does not get to say which set this is. They are
// the repository's answer about its own contents, and a sink that let a
// tag map overwrite them would make the co-tenancy signal something a
// configuration file could lie about.
func (s RepositorySink) snapshotTags() (map[string]string, error) {
	if err := s.Ref.Validate(); err != nil {
		return nil, fmt.Errorf(
			"this sink cannot attribute a snapshot to a backup set (%w), and an untagged snapshot is one no accounting of this repository can see", err)
	}

	tags := make(map[string]string, len(s.Tags)+2)
	for k, v := range s.Tags {
		tags[k] = v
	}

	tags[backupengine.TagKeyBackupSet] = s.Ref.Set.String()
	tags[backupengine.TagKeyDomain] = s.Ref.Domain.String()

	return tags, nil
}

// Discard removes a snapshot written from a read that turned out to be
// torn. It is the half of "never store a torn file as verified" that a
// streaming path needs: a stream is read once, so the tear is only
// visible after the bytes have already been stored, and the honest
// response is to take the restore point away again.
func (s RepositorySink) Discard(ctx context.Context, id string) error {
	if err := s.Repo.DeleteSnapshot(ctx, backupengine.SnapshotID(id)); err != nil {
		return fmt.Errorf("discarding the torn snapshot %q: %w", id, err)
	}

	return nil
}

// repositoryPath joins a source object's path onto the backup set's root
// in the repository's namespace.
//
// It is slash-based and rooted, because that is what the engine requires
// of a streamed object's identity, and because these are remote object
// paths: splitting them with the host platform's separator would make a
// path stored by one build unfindable by another.
func repositoryPath(root, object string) string {
	root = trimSlash(root)
	object = trimSlash(object)

	if root == "" {
		return "/" + object
	}

	return "/" + root + "/" + object
}

func trimSlash(p string) string {
	for len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}

	for len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}

	return p
}
