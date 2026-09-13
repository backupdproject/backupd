package workflowrun

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/backupdproject/backupd/core/internal/obs"
	"github.com/backupdproject/backupd/core/internal/workflow"
	"github.com/backupdproject/backupd/core/internal/workflowexec"
)

// One run's log pipeline: redact, number, persist, fan out.
//
// # The order is the contract
//
// Redaction comes FIRST, before the bytes are numbered and before they
// reach the journal or a subscriber, because a credential that has been
// written down is written down. It is streaming (obs.StreamFilter)
// rather than per chunk: a hook's output arrives in whatever a read off a
// pipe returned, so a value can straddle two chunks, and a filter applied
// to each chunk on its own matches neither half.
//
// # Why persistence is synchronous and fan-out is not
//
// They are protecting different things. A log that stopped recording
// while the step ran on, and was then reported as captured, is worse than
// a step that fails -- so a failed journal write comes back to the
// executor as a write error, which is what workflowexec.Sink's own
// contract asks for. A SUBSCRIBER, by contrast, has no such claim on the
// run: an operator who closed their laptop must not be able to stall a
// database checkpoint, so an offer to a follower's queue never blocks and
// a follower that falls behind reads the journal instead.
//
// # Why the bound drops output rather than throttling it
//
// A hook whose stdout pipe fills up BLOCKS, and a hook blocked on a pipe
// is a backup that never finishes -- the worst outcome available here. So
// the bytes keep being read whatever happens; what stops at the bound is
// the RECORDING, marked in the sequence, in position, by one truncation
// record. A log that says "this is where I stopped" is a usable artifact;
// a stuck backup is not.

// DefaultStepOutputBytes is how much of one step's output is persisted
// before the truncation marker.
//
// 256 KiB is far more than a hook that reports what it did produces, and
// far less than a `set -x` in a loop produces in a second. It bounds the
// journal against an incident rather than against ordinary use.
const DefaultStepOutputBytes int64 = 256 << 10

// logRecorder is one run's log writer: the sequence counter, the journal
// and the broker.
//
// The counter is per RUN rather than per step, because a follower's
// cursor is one integer. Steps execute serially in plan order, so a
// run-monotonic counter is also monotonic within each step, and the
// cursor therefore orders a whole run's output without the follower
// knowing anything about steps.
type logRecorder struct {
	store  Store
	broker *Broker
	runID  string
	now    func() time.Time
	bound  int64

	// ctx is the run's context with its CANCELLATION removed
	// (context.WithoutCancel). A log write has to land even while the
	// step that produced it is being killed -- that output is the
	// evidence of what the hook had done before it was stopped -- so
	// cancelling the run must not cancel the recording of it. The
	// context's VALUES are kept, so the write stays inside whatever
	// correlation the caller established.
	ctx context.Context

	mu  sync.Mutex
	seq uint64
}

// stepSink is one step's view of the recorder: the two stream filters and
// the byte budget.
//
// One sink per step, and one filter per STREAM inside it. Sharing a
// filter between stdout and stderr would interleave their held-back
// tails, which both loses the separation this product keeps on purpose
// and corrupts the redaction.
type stepSink struct {
	rec    *logRecorder
	stepID string

	// onTruncate is called once, the first time this step's recording
	// stops at the bound.
	//
	// A callback rather than a field the recorder reads afterwards,
	// because the fact is an EVENT with a target attached (observe.go)
	// and the recorder deliberately knows nothing about a step beyond
	// its id. Optional: nil is a deployment with nothing measuring.
	onTruncate func()

	mu        sync.Mutex
	filters   map[workflowexec.StreamID]*obs.StreamFilter
	written   int64
	truncated bool
	closed    bool
	chunks    uint64
}

func (r *logRecorder) stepSink(stepID string, redactor *obs.Redactor, onTruncate func()) *stepSink {
	return &stepSink{
		rec:        r,
		stepID:     stepID,
		onTruncate: onTruncate,
		filters: map[workflowexec.StreamID]*obs.StreamFilter{
			workflowexec.StreamStdout: redactor.NewStreamFilter(),
			workflowexec.StreamStderr: redactor.NewStreamFilter(),
		},
	}
}

// Chunk takes one piece of a hook's output.
//
// It is called from the executor's copy goroutines, one stream at a time
// (workflowexec.Capture holds a lock across the call), and it is safe
// under its own lock anyway: the local adapter's runner delivers frames
// off a socket and nothing in that path promises the same discipline.
//
// A chunk arriving AFTER the sink has been closed is refused. That is not
// a hypothetical: a remote step whose termination this product could not
// confirm is a step whose far side may still be writing, and its frames
// arrive on a socket the reader has not finished draining. Recording one
// would append to a step that already has its outcome and its flushed
// tail, after a truncation marker that says the recording stopped -- so
// the log would carry output past the record that says where the output
// ends. Refusing tells the caller, which on this path is a reader that
// has nothing left to deliver it to.
func (s *stepSink) Chunk(c workflowexec.Chunk) error {
	if !c.Stream.Valid() {
		return fmt.Errorf("workflowrun: a chunk of step %s claims stream %s, and output nobody can attribute must not be recorded", s.stepID, c.Stream)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return fmt.Errorf("workflowrun: a chunk of step %s arrived after its output was closed off; the step's outcome and the end of its log are already recorded, so this output has nowhere it could honestly go", s.stepID)
	}

	s.chunks++

	filter := s.filters[c.Stream]
	safe := filter.Filter(c.Data)

	return s.emit(c.Stream, safe)
}

// close flushes what each filter is holding back, which is what stops the
// last few bytes of every hook's output disappearing into a redaction
// window that never completed, and shuts the sink: see Chunk.
//
// Calling it twice flushes once. The filters would release nothing the
// second time either (obs.StreamFilter.Flush is idempotent), and the flag
// is what makes the ORDER right regardless: a second close after a
// straggler would otherwise be a second pair of records.
func (s *stepSink) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true

	for _, stream := range []workflowexec.StreamID{workflowexec.StreamStdout, workflowexec.StreamStderr} {
		if err := s.emit(stream, s.filters[stream].Flush()); err != nil {
			return err
		}
	}

	return nil
}

// emit persists and publishes one already-redacted payload, applying the
// per-step bound. The caller holds s.mu.
func (s *stepSink) emit(stream workflowexec.StreamID, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}

	if s.truncated {
		// Already marked. The bytes are still being read off the pipe --
		// that is what keeps the hook from blocking -- they are simply
		// not being written down any more.
		return nil
	}

	bound := s.rec.bound
	if bound <= 0 {
		bound = DefaultStepOutputBytes
	}

	if s.written+int64(len(payload)) > bound {
		s.truncated = true

		// Announced once, here, and not from the guard above: the flag
		// is what makes this the FIRST payload that did not fit, and
		// every payload after it takes the early return. A counter
		// incremented per dropped chunk would report a hook that
		// overflowed by one byte and one that overflowed by a gigabyte
		// as wildly different numbers of truncations, when both are one
		// truncated step.
		if s.onTruncate != nil {
			s.onTruncate()
		}

		marker := fmt.Sprintf(
			"[backupd] output truncated: this step reached the %d-byte persisted-output bound for one step. The hook is still running and its output is still being read; it is no longer being recorded.",
			bound)

		return s.rec.append(s.stepID, stream, workflow.LogTruncated, []byte(marker))
	}

	s.written += int64(len(payload))

	return s.rec.append(s.stepID, stream, workflow.LogOutput, payload)
}

// append numbers one record, writes it to the journal, and offers it to
// the run's followers.
//
// The journal comes first, deliberately. A subscriber that saw a record
// the journal never got would be a follower whose catch-up read has a
// hole in it exactly where it had been told something existed.
//
// The lock is held ACROSS the write and the fan-out, not just over the
// counter. An earlier version released it after numbering, on the
// argument that a disk write should not serialise two streams -- but what
// that buys is the possibility of record N committing after record N+1,
// and a follower's cursor is exactly the claim that cannot happen: a
// consumer that has processed up to N and asks for everything after it
// would miss a record that lands later with a lower number. Nothing here
// is contended enough for it to matter anyway: a run's steps are serial,
// and one step's two streams already share the sink's own lock.
func (r *logRecorder) append(stepID string, stream workflowexec.StreamID, kind workflow.LogKind, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.seq++
	rec := workflow.StepLog{
		RunID:      r.runID,
		StepID:     stepID,
		Seq:        r.seq,
		Kind:       kind,
		Stream:     stream.String(),
		CapturedAt: r.now(),
		Payload:    payload,
	}

	if err := r.store.AppendWorkflowStepLog(r.ctx, rec); err != nil {
		return err
	}

	if r.broker != nil {
		r.broker.publish(rec)
	}

	return nil
}
