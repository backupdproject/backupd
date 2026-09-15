package rclone

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"

	"github.com/retnd/retnd/core/internal/transport"
)

// OpenSourceStream opens one file on a backup SOURCE for a single forward
// read and hands back the stream itself.
//
// It is the read half CopyToLocal never exposed. CopyToLocal's contract is
// "this remote file is now a local file", which is the right contract for a
// pull-and-verify cycle and the wrong one for a backup engine that hashes
// and chunks as the bytes arrive: staging first means writing the file
// twice and needing room for it once. #791 needs the bytes, not a copy of
// them, so this method hands over rclone's own reader.
//
// What crosses the boundary is an io.ReadCloser and nothing else. No
// fs.Object, no fs.Fs, no rclone type at all -- callers get a stream whose
// backend they cannot name, which is the same promise every other method on
// this adapter makes. The reader is forward-only by construction: it is
// whatever the backend's Object.Open returned, unwrapped and unbuffered, so
// a caller that wants to re-read has to open again.
//
// Closing the reader is not optional. It is what releases the Fs, exactly
// as in OpenObject, because there is no way out of this method to defer a
// release on: the reader is still reading through it.
func (a *Adapter) OpenSourceStream(ctx context.Context, src transport.Source, remotePath string) (io.ReadCloser, error) {
	ctx = oneConnectionAtATime(ctx)

	f, err := a.fsFor(ctx, src)
	if err != nil {
		return nil, WrapCtx(ctx, "open_source_stream", err)
	}

	o, err := f.NewObject(ctx, remotePath)
	if err != nil {
		shutdownFs(ctx, f)

		return nil, WrapCtx(ctx, "open_source_stream", err)
	}

	rc, err := o.Open(ctx)
	if err != nil {
		shutdownFs(ctx, f)

		return nil, WrapCtx(ctx, "open_source_stream", err)
	}

	return &fsBoundReadCloser{ReadCloser: rc, fs: f, ctx: ctx}, nil
}

// StatSource reports what the source says about one object, reading its
// METADATA and nothing else.
//
// It exists because Stat cannot be used for this, and the reason is
// measurable rather than stylistic. Stat asks the object for a SHA-256
// when the backend advertises one, and rclone's local backend advertises
// one by computing it - which is a full read of the file. That is the
// right trade for the destination side, where a stat is how a copy is
// proven and the object was going to be read anyway. It is catastrophic
// on the source side of a backup: the streaming adapter stats every
// object again after reading it, to see whether it moved, so a hashing
// stat would read every byte of a 100 GB source a second time to answer
// a question about its size and timestamp. Doubling the I/O of a backup
// is the anti-pattern this whole path exists to avoid, arriving through
// the back door.
//
// Kind is deliberately left unset. rclone's object model has no answer:
// Fs.NewObject returns an object for a fifo (of size zero) and follows a
// symlink to its target, so "it resolved to an object" says nothing
// about what is at the path. The bounded local enumerator classifies
// what it walks, from the directory read it already performed, and that
// is where a kind comes from; a consumer that has no kind must not
// invent one.
func (a *Adapter) StatSource(ctx context.Context, src transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	ctx = oneConnectionAtATime(ctx)

	f, err := a.fsFor(ctx, src)
	if err != nil {
		return transport.RemoteArtifact{}, WrapCtx(ctx, "stat_source", err)
	}
	defer shutdownFs(ctx, f)

	o, err := f.NewObject(ctx, remotePath)
	if err != nil {
		return transport.RemoteArtifact{}, WrapCtx(ctx, "stat_source", err)
	}

	art := toArtifact(o)
	if ider, ok := o.(fs.IDer); ok {
		art.ID = ider.ID()
	}

	return art, nil
}

// sessionShutdownTimeout bounds hanging up at the end of a run.
//
// A session is closed on the way out of a backup, including the backup
// that was cancelled, so the close cannot inherit the run's context: a
// cancelled context would make Shutdown a no-op on any backend that
// consults it and leave the pool it was supposed to drain open for the
// life of the process. It gets a detached context with its own bound
// instead, because the other failure - a hang-up that never returns -
// would hold a backup window open after every byte was already stored.
const sessionShutdownTimeout = 30 * time.Second

// OpenSourceSession opens ONE conversation with a source and hands back a
// handle every object of a run is read through.
//
// # What it costs not to have one
//
// OpenSourceStream and StatSource each build an Fs and release it, which
// on sftp is a TCP connect, a key exchange, a publickey authentication and
// an sftp subsystem start, per call. The streaming source adapter opens
// each object once and stats it at least once more, so a run over a
// thousand small files pays a couple of thousand SSH handshakes to move a
// few megabytes, and a host with a connection cap (#264) sees a run that
// looks like a fan-out rather than a backup. One session per run makes
// that one handshake, reused.
//
// # Why this is not the cached Fs shutdownFs argues against
//
// It is the same argument at a different scope, not a reversal of it. What
// that doc refuses is an Fs cached ON THE ADAPTER, because rclone captures
// the ambient ConfigInfo into an Fs at construction and never re-reads it,
// so a process-lifetime Fs would apply the first caller's bandwidth limit
// to every later caller for as long as the daemon ran. A session is owned
// by the caller that opened it, built from that caller's own context, and
// released when that caller is finished: the settings it captures are the
// settings of the run it belongs to, and no second caller can ever reach
// it. The adapter itself stays stateless.
//
// The session is safe for concurrent use, because that is the whole point:
// a run reads several objects at once through it. rclone's sftp backend
// serves concurrent operations from a pool of connections bounded by the
// source's own `connections` ceiling, and oneConnectionAtATime is applied
// here exactly as it is on every other operation.
func (a *Adapter) OpenSourceSession(ctx context.Context, src transport.Source) (transport.SourceSession, error) {
	ctx = oneConnectionAtATime(ctx)

	f, err := a.fsFor(ctx, src)
	if err != nil {
		return nil, WrapCtx(ctx, "open_source_session", err)
	}

	return &sourceSession{fs: f, cfg: ctx}, nil
}

// sourceSession is one Fs held for the length of a run.
//
// cfg is the context the Fs was built under, kept for one purpose: it
// carries the rclone ConfigInfo this session was configured with, and
// Close has no context of its own to apply it from. Per-operation contexts
// come from the caller of each method, so a cancelled object read is still
// a cancelled object read.
type sourceSession struct {
	fs  fs.Fs
	cfg context.Context //nolint:containedctx // config only; see the type doc and Close.

	once sync.Once
}

var _ transport.SourceSession = (*sourceSession)(nil)

// OpenStream opens one object for a single forward read.
//
// The reader is NOT bound to the Fs the way OpenSourceStream's is: the
// session owns the Fs and outlives every stream taken from it, so a reader
// that shut the Fs down on Close would hang up on every other object the
// run is reading at that moment.
func (s *sourceSession) OpenStream(ctx context.Context, remotePath string) (io.ReadCloser, error) {
	ctx = oneConnectionAtATime(ctx)

	o, err := s.fs.NewObject(ctx, remotePath)
	if err != nil {
		return nil, WrapCtx(ctx, "open_source_stream", err)
	}

	rc, err := o.Open(ctx)
	if err != nil {
		return nil, WrapCtx(ctx, "open_source_stream", err)
	}

	return rc, nil
}

// StatSource reports the object's metadata, and reads none of its bytes,
// for the reason the method of the same name on Adapter gives at length.
func (s *sourceSession) StatSource(ctx context.Context, remotePath string) (transport.RemoteArtifact, error) {
	ctx = oneConnectionAtATime(ctx)

	o, err := s.fs.NewObject(ctx, remotePath)
	if err != nil {
		return transport.RemoteArtifact{}, WrapCtx(ctx, "stat_source", err)
	}

	art := toArtifact(o)
	if ider, ok := o.(fs.IDer); ok {
		art.ID = ider.ID()
	}

	return art, nil
}

// Close drains the backend's connections, once, on a context that the
// run's cancellation cannot reach.
func (s *sourceSession) Close() error {
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.cfg), sessionShutdownTimeout)
		defer cancel()

		shutdownFs(ctx, s.fs)
	})

	return nil
}
