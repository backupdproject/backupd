package workflowrun

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/workflow"
	"github.com/backupdproject/backupd/core/internal/workflowexec"
)

// The data layer's half of terminal safety (#812).
//
// A hook is an operator's own script, but its OUTPUT is not: it is
// whatever the tools that script invokes printed, and a backup hook runs
// tools that talk to networks. So a hook's output has to be treated as
// hostile text, and there are two failure modes that pull in opposite
// directions.
//
// The first is that a sanitising journal is a lying journal. Payload is
// deliberately undecoded bytes (see workflow.StepLog): a store that
// stripped escape sequences would be editing evidence, and the operator
// asking "what did step 7 print" would get an answer nobody can compare
// against what the tool actually wrote. So the bytes are kept exactly.
//
// The second is that everything else must be immune to them anyway. A
// sequence in a payload must not reach a structural field, must not be
// confusable with something this product itself said, must not survive
// serialisation in a form a consumer could echo into a terminal, and
// must not disturb the framing a follower's cursor is built on. Those
// four claims are what this file pins; rendering the bytes safely on a
// screen is the viewer's problem and is asserted where the viewer is.

// hostileSequence is one corpus entry: bytes a hook can print that a
// terminal ACTS on instead of drawing.
type hostileSequence struct {
	name    string
	payload string
}

// The corpus. One table for all four properties on purpose, so that a
// sequence added here is immediately under every one of them rather than
// under whichever test the person adding it happened to be reading.
var hostileCorpus = []hostileSequence{
	{
		// OSC 8 hyperlink: the visible text is "click me" and the target
		// is anything at all. In a terminal that supports it the log line
		// becomes a clickable link the operator did not author.
		name:    "osc8 hyperlink",
		payload: "\x1b]8;;file:///etc/shadow\x07click me\x1b]8;;\x07",
	},
	{
		// OSC 0: sets the window AND icon title, BEL-terminated. The
		// operator's terminal now claims to be something else.
		name:    "osc0 window title",
		payload: "\x1b]0;pwned\x07",
	},
	{
		// OSC 2: the same attack with the other terminator, ST rather
		// than BEL, because a parser that only knows one of them is a
		// parser with a hole in it.
		name:    "osc2 window title with st",
		payload: "\x1b]2;pwned\x1b\\",
	},
	{
		// OSC 52: writes the base64 payload into the SYSTEM CLIPBOARD.
		// The next thing the operator pastes into a root shell is chosen
		// by whatever the hook printed.
		name:    "osc52 clipboard write",
		payload: "\x1b]52;c;cHduZWQ=\x07",
	},
	{
		// CSI 8 t: resizes the window to 40 rows by 120 columns.
		// Window manipulation also has read variants that make the
		// terminal REPLY on stdin, which is input the operator did not
		// type.
		name:    "csi window manipulation",
		payload: "\x1b[8;40;120t",
	},
	{
		// DECSET 2004: turns bracketed paste on (or, as 2004l, off, which
		// is the direction that lets a pasted newline execute).
		name:    "decset bracketed paste",
		payload: "\x1b[?2004h",
	},
	{
		// DECSET 1049: switches to the alternate screen. Everything the
		// operator had on screen, including the earlier steps of this
		// run, disappears.
		name:    "decset alternate screen",
		payload: "\x1b[?1049h",
	},
	{
		// RIS: full terminal reset. Two bytes, and the scrollback
		// context of the incident being investigated is gone.
		name:    "ris full reset",
		payload: "\x1bc",
	},
	{
		// DCS: a device control string, here a sixel image. The body is
		// arbitrary and runs until ST, so a parser that loses track of
		// where it ends swallows the log lines after it.
		name:    "dcs sixel",
		payload: "\x1bPq#0;2;0;0;0#0~~@@vv@@~~$\x1b\\",
	},
	{
		// The 8-bit C1 introducer: one byte, 0x9b, meaning exactly what
		// ESC [ means. Anything that looks for 0x1b alone does not see
		// this one at all.
		name:    "c1 csi introducer",
		payload: "\x9b8;40;120t",
	},
	{
		// The same C1 introducer as a UTF-8 code point, 0xc2 0x9b, which
		// is how a hook on a UTF-8 terminal actually emits it. This is
		// the entry that makes the wire-form claim below load-bearing:
		// encoding/json escapes bytes below 0x20 and passes valid
		// multi-byte UTF-8 through untouched, so a payload that rode the
		// wire as a STRING rather than as bytes would deliver these two
		// bytes to a consumer intact and a UTF-8 terminal would decode
		// them straight back into CSI.
		name:    "utf-8 encoded c1 csi introducer",
		payload: "\u009b8;40;120t",
	},
	{
		// A carriage return with no newline overwrites the line already
		// drawn, so a terminal shows only "FORGED LINE" while the bytes
		// say both; the BEL rings the bell for good measure.
		name:    "carriage return overwrite and bel",
		payload: "honest line\rFORGED LINE\a",
	},
	{
		// Log injection: the hook prints, as ordinary output, the exact
		// sentence this product writes when it stops recording a step.
		// Spelled out here rather than derived from logs.go because that
		// is what an attacker does -- copies the string out of this
		// product's own output. Whether it is still byte-identical to
		// the genuine marker is not what makes this entry hostile, and
		// TestAForgedTruncationMarkerIsDistinguishableFromTheGenuineOneOnlyByKind
		// takes the genuine sentence at runtime rather than trusting
		// this copy.
		name:    "forged truncation marker",
		payload: "[backupd] output truncated: this step reached the 262144-byte persisted-output bound for one step. The hook is still running and its output is still being read; it is no longer being recorded.",
	},
}

// corpusScript is the one hook every test here installs.
const corpusScript = "10-hostile.local.sh"

// corpusStream alternates the two streams across the corpus so that the
// stream field is a value under test rather than a constant: a record
// that picked up its stream from the payload, or a sink that routed on
// content, is only visible if the expected answer varies.
func corpusStream(i int) workflowexec.StreamID {
	if i%2 == 1 {
		return workflowexec.StreamStderr
	}

	return workflowexec.StreamStdout
}

// recordCorpus runs one step that prints every corpus entry as its own
// chunk, and returns the durable records oldest first together with the
// step id the ENGINE assigned -- read back out of the step table, so a
// record's step id is checked against another table's answer rather than
// against itself.
func recordCorpus(t *testing.T, h *harness, runID string) ([]workflow.StepLog, string) {
	t.Helper()

	tr := newTree(t, map[stage][]string{globalBefore: {corpusScript}})

	h.local.outcomes[corpusScript] = func(_ context.Context, req StepRequest) (StepOutcome, error) {
		for i, entry := range hostileCorpus {
			if err := req.Sink.Chunk(workflowexec.Chunk{
				Stream: corpusStream(i),
				Seq:    uint64(i + 1),
				Data:   []byte(entry.payload),
			}); err != nil {
				return StepOutcome{}, err
			}
		}

		return exited(0), nil
	}

	if _, err := h.run(t, tr.snapshot(t, runID)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	return logsOf(t, h, runID), stateOf(t, h.store, runID, corpusScript).StepID
}

// The bytes survive. This is the deliberate half of terminal safety and
// not a hole: a hook's output is evidence, and a journal that stripped
// escape sequences out of it would answer "what did step 7 print" with
// something step 7 did not print. Every entry goes through the real
// redaction path, the real SQLite blob column and back, and comes out
// byte for byte.
//
// The bug this catches is somebody "fixing" #812 in the wrong layer:
// a strip, an escape, or a UTF-8 normalisation anywhere between the sink
// and the row turns this red.
func TestEveryHostileTerminalSequenceRoundTripsThroughTheJournalByteForByte(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	recs, _ := recordCorpus(t, h, "run-1")
	if len(recs) != len(hostileCorpus) {
		t.Fatalf("the run recorded %d records for %d chunks", len(recs), len(hostileCorpus))
	}

	for i, entry := range hostileCorpus {
		if !bytes.Equal(recs[i].Payload, []byte(entry.payload)) {
			t.Errorf("%s came back from the journal as %q, want %q; a store that rewrites a hook's output is editing evidence",
				entry.name, recs[i].Payload, entry.payload)
		}
	}
}

// No structural field of a record picks up a byte of the payload. The
// cursor, the attribution and the kind are what every consumer routes
// on, and they stay exactly what the engine assigned however hostile the
// bytes beside them are.
//
// The kind is the interesting one. A hook that prints this product's own
// truncation sentence is recorded as OUTPUT, so the two are separated by
// a field and never by their text -- see the next test for the other
// half of that claim.
func TestNoStructuralFieldOfARecordPicksUpAByteOfAHostileTerminalSequence(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	recs, stepID := recordCorpus(t, h, "run-1")
	if len(recs) != len(hostileCorpus) {
		t.Fatalf("the run recorded %d records for %d chunks", len(recs), len(hostileCorpus))
	}

	for i, entry := range hostileCorpus {
		rec := recs[i]

		if rec.RunID != "run-1" {
			t.Errorf("%s was attributed to run %q", entry.name, rec.RunID)
		}
		if rec.StepID != stepID {
			t.Errorf("%s was attributed to step %q, but the step table says %q", entry.name, rec.StepID, stepID)
		}
		if want := corpusStream(i).String(); rec.Stream != want {
			t.Errorf("%s was recorded on stream %q, want %q", entry.name, rec.Stream, want)
		}
		if rec.Kind != workflow.LogOutput {
			t.Errorf("%s was recorded as kind %q; every one of these is a hook's own bytes", entry.name, rec.Kind)
		}
	}
}

// A hook can print this product's truncation sentence verbatim, and the
// journal still says which of the two is this product speaking.
//
// The genuine sentence is taken from a real truncation at runtime rather
// than spelled out here, so the test is about the sentences being the
// SAME rather than about either of them being any particular text: the
// forged record carries byte-identical text and kind "output", the
// genuine one carries kind "truncated". A consumer that decided "the
// recording stopped here" by matching on the words would believe a hook
// that told it the rest of the run's output is missing.
func TestAForgedTruncationMarkerIsDistinguishableFromTheGenuineOneOnlyByKind(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engine.StepOutputBytes = 64

	tr := newTree(t, map[stage][]string{globalBefore: {corpusScript}})

	h.local.outcomes[corpusScript] = func(_ context.Context, req StepRequest) (StepOutcome, error) {
		for i := range 8 {
			if err := req.Sink.Chunk(workflowexec.Chunk{
				Stream: workflowexec.StreamStdout,
				Seq:    uint64(i + 1),
				Data:   []byte("0123456789abcdef"),
			}); err != nil {
				return StepOutcome{}, err
			}
		}

		return exited(0), nil
	}

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var genuine []byte
	for _, rec := range logsOf(t, h, "run-1") {
		if rec.Kind == workflow.LogTruncated {
			genuine = rec.Payload
		}
	}
	if len(genuine) == 0 {
		t.Fatal("the bounded step produced no truncation marker, so there is no genuine sentence to forge")
	}

	// The second run's hook prints that sentence as ordinary output.
	// The bound goes back up so the forgery is not itself truncated,
	// which would make the test pass for the wrong reason.
	h.engine.StepOutputBytes = DefaultStepOutputBytes
	h.local.outcomes[corpusScript] = emitting(string(genuine))

	if _, err := h.run(t, tr.snapshot(t, "run-2")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	forged := logsOf(t, h, "run-2")
	if len(forged) != 1 {
		t.Fatalf("the forging step produced %d records, want 1: %+v", len(forged), forged)
	}

	if !bytes.Equal(forged[0].Payload, genuine) {
		t.Fatalf("the forgery was not stored verbatim (%q), so this test is not exercising the ambiguity it is about", forged[0].Payload)
	}
	if forged[0].Kind != workflow.LogOutput {
		t.Errorf("a hook's own copy of the truncation sentence was recorded as kind %q; this product would be quoting a hook as itself",
			forged[0].Kind)
	}
}

// nonPrintable returns the index of the first byte of b outside printable
// ASCII, or -1.
//
// The whole printable range rather than just the introducers the corpus
// uses, because the dangerous case is one that a search for 0x1b does
// not find. encoding/json escapes bytes below 0x20 and passes everything
// above it through, so the two bytes 0xc2 0x9b -- the UTF-8 spelling of
// CSI, which is how a hook on a UTF-8 terminal emits the C1 introducer
// -- would ride the wire untouched in a string field and arrive at a
// consumer as a working control sequence.
func nonPrintable(b []byte) int {
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			return i
		}
	}

	return -1
}

// The wire form neutralises the sequence without losing it.
//
// Every consumer that is not the deliberate read-only viewer -- an API
// response, an event on the bus, a line in somebody's jq pipeline --
// sees the JSON encoding of the record, and that encoding is printable
// ASCII: Payload is []byte, so it base64s, and every string field is
// escaped. Nothing a consumer echoes can move a cursor.
//
// And the viewer is not lied to either: the payload decodes back to the
// exact bytes the hook wrote. Both halves matter, which is why they are
// asserted together -- a change that made the record human-readable on
// the wire would satisfy the second and break the first.
func TestTheWireFormOfAHostileRecordCarriesNoRawControlByteAndStillDecodesExactly(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	recs, _ := recordCorpus(t, h, "run-1")
	if len(recs) != len(hostileCorpus) {
		t.Fatalf("the run recorded %d records for %d chunks", len(recs), len(hostileCorpus))
	}

	for i, entry := range hostileCorpus {
		wire, err := json.Marshal(recs[i])
		if err != nil {
			t.Fatalf("marshalling the record for %s: %v", entry.name, err)
		}

		if at := nonPrintable(wire); at >= 0 {
			t.Errorf("the wire form of %s carries byte %#02x at offset %d; a consumer that echoed this JSON would re-emit the sequence into a terminal: %q",
				entry.name, wire[at], at, wire)
		}

		var back workflow.StepLog
		if err := json.Unmarshal(wire, &back); err != nil {
			t.Fatalf("unmarshalling the record for %s: %v", entry.name, err)
		}

		if !bytes.Equal(back.Payload, []byte(entry.payload)) {
			t.Errorf("%s decoded back to %q, want %q; a viewer reading this record would be shown something the hook did not print",
				entry.name, back.Payload, entry.payload)
		}
	}
}

// A hostile payload cannot break the framing or the cursor.
//
// A follower holds one integer and asks for everything after it, so the
// sequence has to stay 1..N with no gap and no duplicate, and the number
// of records has to be the number of chunks plus whatever markers this
// product itself added. A payload that split into two records, or that
// was merged with its neighbour, or that consumed a sequence number
// twice, is a log a cursor cannot page through -- and the bytes that
// would do it are exactly the ones a naive implementation treats as
// delimiters.
func TestAHostileTerminalSequenceCannotBreakTheRecordFramingOrTheCursor(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	recs, _ := recordCorpus(t, h, "run-1")

	if len(recs) != len(hostileCorpus) {
		t.Fatalf("the run recorded %d records for %d chunks; one chunk is one record", len(recs), len(hostileCorpus))
	}

	seen := map[uint64]bool{}
	for i, rec := range recs {
		if rec.Seq != uint64(i+1) {
			t.Errorf("record %d has sequence %d; the cursor is not contiguous", i, rec.Seq)
		}
		if seen[rec.Seq] {
			t.Errorf("sequence %d names two records", rec.Seq)
		}
		seen[rec.Seq] = true

		if rec.Kind == workflow.LogTruncated {
			t.Errorf("record %d is a truncation marker; the corpus is far inside the default bound, so this product invented a claim about the output",
				i)
		}
	}

	// And the accounting the bound does is over BYTES, not over anything
	// the payload could influence. A second run bounded at exactly the
	// first two entries takes those two, marks the position, and stops:
	// three records, still numbered 1, 2, 3. A bound that measured runes,
	// or a sanitised length, would let a different number of entries
	// through.
	bounded := newHarness(t)
	bounded.engine.StepOutputBytes = int64(len(hostileCorpus[0].payload) + len(hostileCorpus[1].payload))

	boundedRecs, _ := recordCorpus(t, bounded, "run-1")
	if len(boundedRecs) != 3 {
		t.Fatalf("the bounded run recorded %d records, want the two entries that fit plus one marker: %+v", len(boundedRecs), boundedRecs)
	}

	for i, rec := range boundedRecs {
		if rec.Seq != uint64(i+1) {
			t.Errorf("bounded record %d has sequence %d; the marker did not take a position in the sequence", i, rec.Seq)
		}
	}
	for i := range 2 {
		if boundedRecs[i].Kind != workflow.LogOutput || !bytes.Equal(boundedRecs[i].Payload, []byte(hostileCorpus[i].payload)) {
			t.Errorf("bounded record %d is kind %q with payload %q, want %s verbatim",
				i, boundedRecs[i].Kind, boundedRecs[i].Payload, hostileCorpus[i].name)
		}
	}
	if boundedRecs[2].Kind != workflow.LogTruncated {
		t.Errorf("the third bounded record is kind %q, want the truncation marker", boundedRecs[2].Kind)
	}
	if !strings.Contains(string(boundedRecs[2].Payload), "truncated") {
		t.Errorf("the marker does not say what happened: %q", boundedRecs[2].Payload)
	}
}
