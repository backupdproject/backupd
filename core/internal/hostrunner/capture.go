package hostrunner

import (
	"io"
	"sync"
)

// The shape output takes on its way back to the engine, agreed with
// core/internal/remoteexec (#810) so that a hook's log reads identically
// whether it ran here or over SSH.
//
// Two decisions, both of which a simpler design gets wrong.
//
// STREAMS STAY APART. stdout and stderr are separate fields on a step's
// record (internal/workflow's Step.StdoutLogRef and StderrLogRef), and a
// hook that writes its progress to stdout and its warnings to stderr is
// the normal case. Merging them into one text and splitting it later is
// not possible -- there is no marker to split on -- so they are never
// merged.
//
// ORDER IS RECOVERABLE ANYWAY. Keeping the streams apart loses the one
// thing a human reading a failed hook actually wants: whether the warning
// came before or after the line that looks like the cause. So every chunk
// of both streams takes its sequence number from ONE counter. Two
// separate per-stream counters would have made the numbers meaningless
// for exactly this question, which is the whole reason to number them.
//
// The order is the order this process READ the two pipes, which is not
// quite the order the hook wrote them: stdout and stderr are different
// pipes with different buffering, and no reader anywhere can do better
// than that without a pty (which would merge them, see above) or a
// cooperating child. That is a real limit and it is written down here
// rather than implied by a field called Seq.

// StreamID says which of a process's two output streams a chunk came
// from. The values are fixed on the wire because they are also the file
// descriptor numbers, which is the least surprising thing they could be.
type StreamID uint8

const (
	// StreamStdout is file descriptor 1.
	StreamStdout StreamID = 1
	// StreamStderr is file descriptor 2.
	StreamStderr StreamID = 2
)

// String renders a stream as the word the journal and the operator
// surfaces use.
func (s StreamID) String() string {
	switch s {
	case StreamStdout:
		return "stdout"
	case StreamStderr:
		return "stderr"
	default:
		return "unknown"
	}
}

// Valid reports whether this is one of the two streams. A chunk arriving
// over the socket claiming stream 7 is a malformed frame, not a third
// stream.
func (s StreamID) Valid() bool { return s == StreamStdout || s == StreamStderr }

// Chunk is one piece of one stream, with its place in the global order.
type Chunk struct {
	// Stream is which pipe these bytes came out of.
	Stream StreamID `json:"stream"`

	// Seq is the chunk's position across BOTH streams, from 1. See this
	// file's preamble for what it does and does not prove.
	Seq uint64 `json:"seq"`

	// Data is the bytes, exactly as read. Nothing here decodes them:
	// a hook's output is not required to be UTF-8 (a `tar` progress
	// line, a binary tool's stderr), and a runner that sanitized it
	// would be editing evidence.
	Data []byte `json:"data"`
}

// Sink is where captured output goes. The server implements it by writing
// frames to the connection; a test implements it by appending to a slice.
//
// It returns an error so that a sink which has lost its destination --
// the engine went away mid-hook -- can say so, rather than swallowing
// every chunk and leaving the step looking like it produced nothing.
//
// A Sink is NEVER called from two goroutines at once, and is called in
// ascending Seq order. Both are guaranteed by Capture rather than left to
// each implementation; see streamWriter.Write for why that is where the
// guarantee has to live.
type Sink interface {
	Chunk(Chunk) error
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(Chunk) error

// Chunk calls f.
func (f SinkFunc) Chunk(c Chunk) error { return f(c) }

// Capture hands out one io.Writer per stream, all sharing one sequence
// counter and one lock.
//
// The lock is what makes the counter mean anything: the two writers are
// driven by two goroutines reading two pipes, so an unsynchronised
// counter would number chunks in an order no reader could rely on, which
// is worse than not numbering them.
type Capture struct {
	sink Sink

	mu  sync.Mutex
	seq uint64
	err error
}

// NewCapture returns a Capture delivering to sink.
func NewCapture(sink Sink) *Capture { return &Capture{sink: sink} }

// Writer returns the io.Writer for one stream.
func (c *Capture) Writer(stream StreamID) io.Writer { return streamWriter{capture: c, stream: stream} }

// Err reports the first error a sink returned, if any.
//
// The first rather than the last, and it is remembered rather than
// returned from every subsequent Write: once the destination has failed,
// every later chunk fails for the same reason, and a step whose failure
// is reported as "the fourteen-thousandth chunk could not be written"
// tells nobody anything.
func (c *Capture) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Seq reports how many chunks have been numbered so far.
func (c *Capture) Seq() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq
}

type streamWriter struct {
	capture *Capture
	stream  StreamID
}

// Write numbers one chunk and delivers it.
//
// The lock is held ACROSS the delivery, not just across the increment,
// and that is the difference between a sequence number and a decoration.
// Numbering under a lock and then delivering outside it lets the chunk
// numbered 5 reach the engine before the chunk numbered 4, so the
// ordering the number exists to record is the one thing the wire would
// not have. It also means a Sink is never called from two goroutines at
// once, which is a contract every implementation would otherwise have to
// discover -- the first one to fail it was this package's own test
// collector, under -race.
//
// The cost is that a slow sink backs up both pipes rather than one. That
// is the correct direction: the alternative is unbounded reordering plus
// unbounded buffering of a hook's output in this process's memory.
//
// It COPIES p. os/exec hands the same buffer back on the next read, so a
// sink that queues chunks (a test collecting them, a writer batching
// them) would otherwise be holding a slice whose contents change
// underneath it. That bug does not reproduce under light load, which is
// the kind worth spending one allocation to make impossible.
//
// It always reports len(p) written, even on a sink error. The contract
// io.Writer states for a short write is that the caller may retry, and
// retrying a chunk whose destination is gone would turn a dead engine
// into a spin.
func (w streamWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c := w.capture

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err != nil {
		return len(p), c.err
	}
	c.seq++
	chunk := Chunk{Stream: w.stream, Seq: c.seq, Data: append([]byte(nil), p...)}

	if err := c.sink.Chunk(chunk); err != nil {
		c.err = err
		return len(p), err
	}
	return len(p), nil
}
