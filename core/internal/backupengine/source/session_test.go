package source_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/backupdproject/backupd/core/internal/backupengine/source"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// sessionSource is a fake source that can hold ONE conversation open, and
// counts what the run does with it.
//
// It embeds fakeSource so the per-object surface is the same one every
// other test drives; what it adds is the capability the adapter type-
// asserts for, plus enough bookkeeping to answer "how many sessions did a
// run open, and was anything read through a closed one".
type sessionSource struct {
	*fakeSource

	opened  atomic.Int64
	closed  atomic.Int64
	openErr error

	mu        sync.Mutex
	afterUses int // operations attempted through a closed session
}

func (s *sessionSource) OpenSourceSession(_ context.Context, src transport.Source) (transport.SourceSession, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}

	s.opened.Add(1)

	return &fakeSession{owner: s, src: src}, nil
}

// fakeSession delegates to the per-object surface, so a test comparing a
// session run with a sessionless one is comparing the session and nothing
// else.
type fakeSession struct {
	owner *sessionSource
	src   transport.Source

	mu     sync.Mutex
	closed bool
}

func (f *fakeSession) OpenStream(ctx context.Context, remotePath string) (io.ReadCloser, error) {
	f.use()

	return f.owner.fakeSource.OpenSourceStream(ctx, f.src, remotePath)
}

func (f *fakeSession) StatSource(ctx context.Context, remotePath string) (transport.RemoteArtifact, error) {
	f.use()

	return f.owner.fakeSource.StatSource(ctx, f.src, remotePath)
}

func (f *fakeSession) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()

	f.owner.closed.Add(1)

	return nil
}

// use records an operation performed through a session that has already
// been closed, which on a real transport is a read through a connection
// that is not there any more.
func (f *fakeSession) use() {
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()

	if !closed {
		return
	}

	f.owner.mu.Lock()
	f.owner.afterUses++
	f.owner.mu.Unlock()
}

func (s *sessionSource) usesAfterClose() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.afterUses
}

// A run opens ONE transport session, reads every object of the backup
// through it, and closes it once when the run is over.
//
// The cost this is about is not abstract: on SFTP the per-object shape is
// a TCP connect, a key exchange, a publickey authentication and a
// subsystem start for every open AND every stat, so a run over a few
// thousand small files spends its window dialing. The session is also the
// thing that must not leak - one unclosed session per run is an SSH
// connection held for the life of the daemon.
func TestARunOpensOneTransportSessionAndClosesIt(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	for _, name := range []string{"a.bin", "b.bin", "c/d.bin", "c/e.bin", "f.bin"} {
		f.put(name, []byte("some bytes for "+name), 1_700_000_000)
	}

	s := &sessionSource{fakeSource: f}
	sink := newRecordingSink()

	a := newAdapter(t, source.Deps{Streamer: s, Stater: s, Enumerator: s}, source.Options{
		Mode:        model.ModeLiveBestEffort,
		Preset:      model.PresetConservative,
		Concurrency: 3,
	})

	rep, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if !rep.Complete() || rep.Stored != 5 {
		t.Fatalf("the run stored %d of 5 objects: %+v", rep.Stored, rep)
	}

	if got := s.opened.Load(); got != 1 {
		t.Errorf("the run opened %d transport sessions, want 1; five objects were read and each was stat'ed after its read", got)
	}

	if got := s.closed.Load(); got != 1 {
		t.Errorf("the run closed %d sessions, want 1; an unclosed session is a connection held for the life of the process", got)
	}

	if got := s.usesAfterClose(); got != 0 {
		t.Errorf("%d operations were performed through a session the run had already closed", got)
	}

	// The session really was the path the bytes took: the fake's own
	// per-object counters are behind it.
	if got := f.opens.Load(); got != 5 {
		t.Errorf("%d object opens reached the source, want 5", got)
	}
}

// A cancelled run closes its session too. This is the path a leak hides
// on: the run is being torn down, every worker is unwinding, and the one
// connection the whole run shared is the thing nobody is looking at.
func TestACancelledRunStillClosesItsSession(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	for i := range 50 {
		f.put(string(rune('a'+i%26))+string(rune('a'+i/26))+".bin", []byte("body"), 1_700_000_000)
	}

	s := &sessionSource{fakeSource: f}

	ctx, cancel := context.WithCancel(context.Background())

	var seen atomic.Int64

	a := newAdapter(t, source.Deps{Streamer: s, Stater: s, Enumerator: s}, source.Options{
		Mode:        model.ModeLiveBestEffort,
		Preset:      model.PresetConservative,
		Concurrency: 2,
		OnResult: func(source.Result) {
			if seen.Add(1) == 3 {
				cancel()
			}
		},
	})

	defer cancel()

	if _, err := a.Backup(ctx, source.Request{Source: fakeTransportSource(), Sink: newRecordingSink()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled run returned %v, want context.Canceled", err)
	}

	if got := s.closed.Load(); got != 1 {
		t.Fatalf("a cancelled run closed %d sessions, want 1", got)
	}
}

// A transport that cannot hold a conversation open is not degraded, it is
// per-object: the run behaves exactly as it did before sessions existed.
func TestATransportWithoutSessionsStillBacksUpPerObject(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("a.bin", []byte("one"), 1_700_000_000)
	f.put("b.bin", []byte("two"), 1_700_000_001)

	sink := newRecordingSink()
	a := newAdapter(t, depsFor(f), liveOpts())

	rep, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if !rep.Complete() || rep.Stored != 2 {
		t.Fatalf("the run stored %d of 2 objects: %+v", rep.Stored, rep)
	}
}

// A session that cannot be opened fails the RUN, once, instead of
// becoming one identical dial failure per object in a report somebody has
// to read to find out the host was unreachable.
func TestASessionThatCannotBeOpenedFailsTheRun(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("a.bin", []byte("one"), 1_700_000_000)

	boom := errors.New("the source refused the connection")
	s := &sessionSource{fakeSource: f, openErr: boom}

	a := newAdapter(t, source.Deps{Streamer: s, Stater: s, Enumerator: s}, liveOpts())

	rep, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: newRecordingSink()})
	if !errors.Is(err, boom) {
		t.Fatalf("Backup returned %v, want the dial failure", err)
	}

	if rep.Entries != 0 {
		t.Fatalf("the run touched %d entries after failing to reach the source", rep.Entries)
	}
}
