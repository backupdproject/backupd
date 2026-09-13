package workflowexec

import (
	"fmt"
	"io"
	"sync"
)

// StreamID names one of a hook's two output streams.
//
// It is a typed constant rather than the string "stdout", because these
// values are compared on a path that decides which log a chunk lands in,
// and a typo in a string comparison there is silent.
type StreamID uint8

const (
	// StreamStdout is fd 1 of the hook.
	StreamStdout StreamID = 1
	// StreamStderr is fd 2 of the hook.
	StreamStderr StreamID = 2
)

// String is the operator-facing spelling, and the one the log layer stores.
func (s StreamID) String() string {
	switch s {
	case StreamStdout:
		return "stdout"
	case StreamStderr:
		return "stderr"
	default:
		return fmt.Sprintf("stream(%d)", uint8(s))
	}
}

// Valid reports whether this is one of the two streams a hook has.
func (s StreamID) Valid() bool { return s == StreamStdout || s == StreamStderr }

// Chunk is one piece of a hook's output, as the engine's log layer receives
// it: which stream it came from, where it sits in the run's capture order,
// and the bytes.
//
// Seq is the whole reason this type exists rather than two io.Writers. The
// streams stay SEPARATE, because a hook that writes a progress bar to
// stderr and a manifest to stdout must not have them merged, and merging is
// irreversible. But "separate" on its own loses the one thing the merged
// form had: which line came first. One counter across both streams keeps
// that answer available without giving up the separation.
type Chunk struct {
	Stream StreamID
	Seq    uint64
	Data   []byte
}

// Sink is what the engine's log layer implements. #812 owns where the bytes
// land; this is the shape they arrive in.
//
// An error from Chunk comes back to the executor as a write error, which is
// deliberate: a log that stopped recording while the step ran on and was
// reported as captured is worse than a step that fails.
type Sink interface {
	Chunk(Chunk) error
}

// Capture hands out one io.Writer per stream and numbers everything they
// receive from a single counter.
//
// The mutex is not contention-shy about it: the two streams really are
// copied concurrently (one goroutine each, which is how os/exec and
// golang.org/x/crypto/ssh both work), so the ordering has to be decided
// under a lock or the sequence numbers are a lie. The critical section is
// one increment and one call.
type Capture struct {
	sink Sink

	mu  sync.Mutex
	seq uint64
}

// NewCapture returns a Capture that numbers from 1. Zero is left unused on
// purpose: it is what an uninitialised sequence field reads as, and a chunk
// that claims position zero would be indistinguishable from one that was
// never numbered at all.
func NewCapture(sink Sink) *Capture { return &Capture{sink: sink} }

// Writer returns the io.Writer for one stream. It is safe to call from
// several goroutines and the writers it returns are safe to use
// concurrently with each other.
func (c *Capture) Writer(stream StreamID) io.Writer {
	return &streamWriter{capture: c, stream: stream}
}

// Sequence is the highest sequence number issued so far, for an executor
// recording how much output a step produced. It is a count of chunks, never
// of bytes.
func (c *Capture) Sequence() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.seq
}

type streamWriter struct {
	capture *Capture
	stream  StreamID
}

// Write numbers these bytes and hands them to the sink.
//
// The copy is required rather than tidy. io.Copy reuses one buffer for
// every read, so a sink that retained the slice would see every chunk it
// already accepted change under it on the next read.
func (w *streamWriter) Write(p []byte) (int, error) {
	if !w.stream.Valid() {
		return 0, fmt.Errorf("workflowexec: %s is not one of a hook's two output streams, and a chunk nobody can attribute must not be recorded", w.stream)
	}
	if len(p) == 0 {
		return 0, nil
	}

	data := make([]byte, len(p))
	copy(data, p)

	w.capture.mu.Lock()
	w.capture.seq++
	chunk := Chunk{Stream: w.stream, Seq: w.capture.seq, Data: data}
	err := w.capture.sink.Chunk(chunk)
	w.capture.mu.Unlock()

	if err != nil {
		return 0, err
	}

	return len(p), nil
}

// TerminationCertainty is what this product actually PROVED about a step it
// stopped waiting for.
//
// Three values and no fourth. A step that ran to completion was never
// asked to stop (TerminationNotRequested); a step this product asked to
// stop either demonstrably finished (TerminationConfirmed) or did not
// (TerminationUnconfirmed). There is no spelling of "probably gone",
// because the whole reason this field exists is that a hook still touching
// the thing it was quiescing, while the backup proceeds, is the worst case
// a workflow has -- and a hedge recorded there would be read as a pass.
type TerminationCertainty string

const (
	// TerminationNotRequested is the zero value: nothing was asked to
	// stop, so there is nothing to be certain about.
	TerminationNotRequested TerminationCertainty = ""
	// TerminationConfirmed means the executor observed the step finish
	// after termination was requested.
	TerminationConfirmed TerminationCertainty = "confirmed"
	// TerminationUnconfirmed means it did not. Something of the step may
	// still be running, and saying so is the only honest report.
	TerminationUnconfirmed TerminationCertainty = "unconfirmed"
)

// Valid reports whether this is one of the three answers.
func (t TerminationCertainty) Valid() bool {
	switch t {
	case TerminationNotRequested, TerminationConfirmed, TerminationUnconfirmed:
		return true
	default:
		return false
	}
}
