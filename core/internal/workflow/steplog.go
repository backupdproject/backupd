package workflow

import (
	"fmt"
	"time"
)

// One record of a hook's output, as it is durably stored and as a
// follower replays it (#811).
//
// # Why the record is a row and not a file
//
// Because the two questions asked of a hook's output are "what did step 7
// print" and "what has happened since I last looked", and the second one
// is a cursor over an ordered sequence. A file per step answers the first
// and cannot answer the second without every follower learning a second
// mechanism (an offset into a file that is being appended to, on a
// filesystem that may be full), and a file per step is also where the
// bound has to be enforced by truncating something somebody is reading.
//
// # What is in a record, and why each field is load-bearing
//
//   - Seq is the cursor, monotonic across the WHOLE run. Steps are
//     executed serially in plan order, so one counter is also monotonic
//     within a step, and a follower holds one integer rather than one per
//     step. It starts at 1: zero is what an uninitialised field reads as,
//     and "I have seen nothing" has to be spellable.
//   - Stream keeps stdout and stderr APART. Merging them is irreversible
//     and a hook that writes a progress bar to one and a manifest to the
//     other must not have them interleaved -- while Seq keeps the answer
//     to "which came first" available anyway.
//   - CapturedAt is when this product read the bytes, not when the hook
//     wrote them. It is the honest claim, and it is what makes a hook
//     that went quiet for four minutes visible.
//   - Payload is the bytes as captured, after redaction and NOT decoded.
//     A hook's output is not required to be UTF-8 (a tar progress line, a
//     binary tool's stderr), so a store that re-encoded it would be
//     editing evidence.
//
// # The one thing that happens to the bytes before they land
//
// Redaction, streaming, before persistence. A hook that prints its own
// credential -- `set -x` over a psql invocation is the ordinary way this
// happens -- must not have written that credential into this journal, and
// a filter applied at read time is a filter that is too late. See
// obs.StreamFilter for why it cannot be a per-chunk Filter call.

// LogKind says whether a record carries output or says something about
// the output.
//
// It exists because the truncation marker has to be IN the sequence. A
// bound reached mid-hook is a fact about the log that a follower and an
// operator both have to see in the position it happened at, and a flag on
// a step row would put it at the end -- after the point where the missing
// bytes were.
type LogKind string

// The two kinds.
const (
	// LogOutput is a hook's own bytes.
	LogOutput LogKind = "output"

	// LogTruncated is this product saying it stopped persisting this
	// step's output at this position, and why. The hook keeps running
	// and its output keeps being drained; what stops is the recording.
	LogTruncated LogKind = "truncated"
)

var logKinds = []LogKind{LogOutput, LogTruncated}

// LogKinds returns the kind vocabulary in the documented order. A
// function rather than an exported slice, for StepStates' reason.
func LogKinds() []LogKind { return append([]LogKind(nil), logKinds...) }

// Valid reports whether k is a known kind.
func (k LogKind) Valid() bool {
	for _, known := range logKinds {
		if k == known {
			return true
		}
	}

	return false
}

// The two streams a hook has, spelled as the durable strings.
//
// They are plain strings rather than this package's own enum because the
// value that reaches here comes from workflowexec.StreamID.String(), and
// a second enum would be a second spelling of "stdout" that compares
// unequal at the one seam where a captured chunk becomes a stored record.
// TestTheStreamSpellingsAreTheOnesTheExecutorsProduce pins the two
// together from the side that imports both.
const (
	LogStreamStdout = "stdout"
	LogStreamStderr = "stderr"
)

// ValidLogStream reports whether s is one of the two streams a hook has.
func ValidLogStream(s string) bool { return s == LogStreamStdout || s == LogStreamStderr }

// StepLog is one durable record of one step's output.
type StepLog struct {
	RunID  string
	StepID string

	// Seq is the run-monotonic cursor. See this file's preamble.
	Seq uint64

	Kind   LogKind
	Stream string

	CapturedAt time.Time

	// Payload is the redacted bytes, undecoded. For LogTruncated it is
	// the sentence explaining what stopped being recorded.
	Payload []byte
}

// Validate reports the first way this record is not one the journal may
// store. See Run.Validate for why it does not collect.
func (l StepLog) Validate() error {
	if err := validPathComponent("run id", l.RunID); err != nil {
		return err
	}
	if err := validPathComponent("step id", l.StepID); err != nil {
		return err
	}

	if l.Seq == 0 {
		return fmt.Errorf("workflow: the log record for step %q of run %q claims sequence 0, which is what an uninitialised counter reads as; a follower's cursor cannot tell that from a record nobody numbered",
			l.StepID, l.RunID)
	}

	if !l.Kind.Valid() {
		return vocabularyError("log kind", l.Kind, LogKinds())
	}

	if !ValidLogStream(l.Stream) {
		return fmt.Errorf("workflow: the log record at sequence %d of run %q names stream %q; a hook has two, %s and %s, and a chunk nobody can attribute must not be recorded",
			l.Seq, l.RunID, l.Stream, LogStreamStdout, LogStreamStderr)
	}

	if l.CapturedAt.IsZero() {
		return fmt.Errorf("workflow: the log record at sequence %d of run %q has no capture time; when this product read the bytes is what makes a hook that went quiet visible",
			l.Seq, l.RunID)
	}

	if len(l.Payload) == 0 {
		return fmt.Errorf("workflow: the log record at sequence %d of run %q has no payload; an empty record consumes a cursor position and tells a follower nothing",
			l.Seq, l.RunID)
	}

	return nil
}
