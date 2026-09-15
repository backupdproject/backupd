package source

import (
	"context"
	"errors"
	"io"

	"github.com/retnd/retnd/core/internal/transport"
)

// SessionOpener is the capability a transport advertises when it can hold
// ONE conversation with a source open across a whole run.
//
// It is asked for by type assertion rather than by a field on Deps, which
// is the same shape backupengine.StreamingRepository uses and is deliberate
// here for a reason a field would get wrong: production hands this package
// the rclone adapter as its Streamer, so a transport that grew the
// capability would silently keep paying per-object connection setup until
// somebody remembered to wire a second field. A capability that has to be
// wired twice is a capability that is off in production and on in the test
// that proves it.
//
// A transport without the method is not degraded, it is per-object: the
// adapter opens and stats through Streamer and Stater exactly as before.
// That is what most fakes are, and it is what a backend whose connection
// cannot be reused would be.
type SessionOpener interface {
	OpenSourceSession(ctx context.Context, src transport.Source) (transport.SourceSession, error)
}

// sourceReader is how one run reaches its source: one session held for the
// run, or the per-object fallback. Everything above it - the walk, the
// retry bound, the mutation check - is written against this and cannot
// tell which it got, which is what keeps the session out of the reading
// path's logic.
type sourceReader interface {
	openStream(ctx context.Context, remotePath string) (io.ReadCloser, error)
	statSource(ctx context.Context, remotePath string) (transport.RemoteArtifact, error)

	// close releases whatever the run held. It runs exactly once, on
	// every path out of a run including the cancelled one.
	close()
}

// newSourceReader opens the best conversation this transport offers for
// one run.
//
// A session that cannot be opened fails the RUN rather than every object
// in it. The alternative - falling back to per-object opens when the
// session dial fails - would turn "this host is unreachable" into a
// thousand identical dial failures and a report that has to be read to
// find that out.
func (a *Adapter) newSourceReader(ctx context.Context, src transport.Source) (sourceReader, error) {
	opener, ok := a.deps.Streamer.(SessionOpener)
	if !ok {
		return perObjectReader{streamer: a.deps.Streamer, stater: a.deps.Stater, src: src}, nil
	}

	session, err := opener.OpenSourceSession(ctx, src)
	if err != nil {
		//nolint:wrapcheck // the transport wraps with its own operation name.
		return nil, err
	}

	return &sessionReader{session: session}, nil
}

// sessionReader reads every object of a run through one open session.
type sessionReader struct {
	session transport.SourceSession
}

func (s *sessionReader) openStream(ctx context.Context, remotePath string) (io.ReadCloser, error) {
	//nolint:wrapcheck // the transport wraps with its own operation name.
	return s.session.OpenStream(ctx, remotePath)
}

func (s *sessionReader) statSource(ctx context.Context, remotePath string) (transport.RemoteArtifact, error) {
	//nolint:wrapcheck // the transport wraps with its own operation name.
	return s.session.StatSource(ctx, remotePath)
}

// close hangs up. The error is dropped here and nowhere else: a session
// that would not close cleanly, after every byte of the run has already
// been stored and reported, must not turn a good backup into a failed one,
// and the transport's own Close is where the bound on how long it may take
// lives.
func (s *sessionReader) close() { _ = s.session.Close() }

// perObjectReader is what a transport that cannot hold a conversation open
// gets: a connection per operation, which is what every caller of
// Transport gets and what this package did everywhere before sessions
// existed.
type perObjectReader struct {
	streamer Streamer
	stater   Stater
	src      transport.Source
}

func (p perObjectReader) openStream(ctx context.Context, remotePath string) (io.ReadCloser, error) {
	//nolint:wrapcheck // the transport wraps with its own operation name.
	return p.streamer.OpenSourceStream(ctx, p.src, remotePath)
}

func (p perObjectReader) statSource(ctx context.Context, remotePath string) (transport.RemoteArtifact, error) {
	if p.stater == nil {
		// Every path that stats is gated on a Stater existing (see
		// newRun and BackupPaths), so this is a wiring bug rather than
		// a runtime condition, and it says so instead of panicking in
		// a worker goroutine halfway through a backup window.
		return transport.RemoteArtifact{}, errors.New("source: this run has no stater, so nothing can be re-stat'ed")
	}

	//nolint:wrapcheck // the transport wraps with its own operation name.
	return p.stater.StatSource(ctx, p.src, remotePath)
}

func (p perObjectReader) close() {}
