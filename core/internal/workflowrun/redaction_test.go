package workflowrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/retnd/retnd/core/internal/hostrunner"
	"github.com/retnd/retnd/core/internal/secretref"
	"github.com/retnd/retnd/core/internal/workflow"
	"github.com/retnd/retnd/core/internal/workflowexec"
)

// The chunk-boundary half of #812's redaction requirement, asked at every
// boundary rather than at one, and asked through the two capture
// implementations a real hook's output actually comes through.
//
// TestASecretSplitAcrossChunksNeverReachesTheJournal already proves the
// property for ONE hand-chosen split of one value. Two things it cannot
// say are the ones this file is for:
//
//   - a streaming filter's hold-back logic is a function of WHERE the
//     boundary falls, so the interesting input is every position inside
//     the value, not a position somebody picked. A filter that held back
//     the longest needle minus one byte, or that compared case-sensitively
//     only on a whole-chunk match, passes a single-split test and leaks at
//     some other offset.
//   - the chunks are produced by hostrunner.Capture on the local path and
//     workflowexec.Capture on the remote one, and the two are different
//     types with different sequence bookkeeping that meet at
//     LocalExecutor's conversion. Driving synthetic workflowexec.Chunk
//     values, which every existing test does, asserts nothing about the
//     local producer or about that conversion.
//
// Both paths are driven in ONE run each, with the value emitted once per
// split position, because a harness per split would cost a real SQLite
// journal per split for no extra evidence. Emitting them back to back is
// also the harder input: each occurrence's tail arrives while the filter
// is still deciding about the previous one.

// boundarySecret is the value the hook prints. High entropy on purpose:
// the assertion below is that no EIGHT-BYTE window of it survives
// anywhere in the journal, and a value made of dictionary words would
// make that assertion fire on ordinary English in the surrounding output
// rather than on a leak.
const boundarySecret = "qZ7x-4Kd91-Wm0Tb-Rv58Ne-Jh2Ls6"

// boundaryWindow is the shortest fragment of the value this test treats as
// a leak. Eight bytes of a thirty-byte high-entropy value is already
// enough to shorten a brute force by orders of magnitude, and it is short
// enough that a filter which released half the value fails here rather
// than passing on a technicality about how much of it escaped.
const boundaryWindow = 8

func TestASecretIsUnreachableAtEveryChunkBoundaryOnBothCapturePaths(t *testing.T) {
	t.Parallel()

	for _, path := range []struct {
		name string

		// emit writes one line to the step's sink, split in two at
		// offset, through the capture implementation that path uses in
		// production.
		emit func(t *testing.T, sink workflowexec.Sink, line string, offset int)
	}{
		{
			// The remote path: bytes come off the SSH channel into
			// workflowexec.Capture, whose writer is handed straight to
			// io.Copy, so a boundary is wherever a read off the channel
			// happened to end.
			name: "remote",
			emit: func(t *testing.T, sink workflowexec.Sink, line string, offset int) {
				t.Helper()

				w := workflowexec.NewCapture(sink).Writer(workflowexec.StreamStderr)
				writeInTwo(t, w, line, offset)
			},
		},
		{
			// The local path: bytes are read inside the host runner by
			// hostrunner.Capture, framed onto the runner socket, and
			// converted back by LocalExecutor. The conversion below is
			// the one adapter.go performs, kept identical on purpose --
			// a stream id or a payload that did not survive it would
			// show up here as output that never reached the journal.
			name: "local",
			emit: func(t *testing.T, sink workflowexec.Sink, line string, offset int) {
				t.Helper()

				w := hostrunner.NewCapture(hostrunner.SinkFunc(func(c hostrunner.Chunk) error {
					return sink.Chunk(workflowexec.Chunk{
						Stream: workflowexec.StreamID(c.Stream),
						Seq:    c.Seq,
						Data:   c.Data,
					})
				})).Writer(hostrunner.StreamStderr)
				writeInTwo(t, w, line, offset)
			},
		},
	} {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})

			secretFile := filepath.Join(custodyTempDir(t), "db.pw")
			if err := os.WriteFile(secretFile, []byte(boundarySecret+"\n"), 0o600); err != nil {
				t.Fatalf("writing the secret fixture: %v", err)
			}

			plan := tr.snapshotWithEnv(t, "run-1", []workflow.EnvVar{
				{Name: "PGPASSWORD", Secret: secretref.Ref{File: secretFile}},
			})

			h.local.outcomes["10-mount.local.sh"] = func(_ context.Context, req StepRequest) (StepOutcome, error) {
				// One occurrence per boundary inside the value, plus the
				// two boundaries at its edges, which are the cases where
				// the filter has nothing held back and everything held
				// back.
				for offset := range len(boundarySecret) + 1 {
					line := "+ psql --password " + boundarySecret + " --db main\n"
					path.emit(t, req.Sink, line, len("+ psql --password ")+offset)
				}

				return exited(0), nil
			}

			if _, err := h.run(t, plan); err != nil {
				t.Fatalf("Run: %v", err)
			}

			recs := logsOf(t, h, "run-1")
			if len(recs) == 0 {
				t.Fatal("nothing was captured, so this test proves nothing")
			}

			var whole strings.Builder
			for _, rec := range recs {
				whole.Write(rec.Payload)
			}
			got := whole.String()

			// Every window of the value, not only the value itself: the
			// failure mode a per-chunk filter has is releasing the halves
			// separately, and "the whole value is absent" is true of that
			// journal.
			for i := 0; i+boundaryWindow <= len(boundarySecret); i++ {
				window := boundarySecret[i : i+boundaryWindow]
				if strings.Contains(got, window) {
					t.Fatalf("the fragment %q of the credential survived redaction at some boundary, so the value can be reassembled from the journal: %q", window, got)
				}
			}

			// And the surrounding output is still there. A filter that
			// dropped the line, or held the tail back for ever, would
			// pass every assertion above while losing the evidence the
			// log exists for.
			occurrences := strings.Count(got, "+ psql --password")
			if occurrences != len(boundarySecret)+1 {
				t.Errorf("the journal carries %d of the %d lines the hook printed, so redaction is eating output: %q", occurrences, len(boundarySecret)+1, got)
			}
			if !strings.Contains(got, "--db main") {
				t.Errorf("the text after the credential did not survive redaction: %q", got)
			}
		})
	}
}

// writeInTwo writes line to w as two writes with the boundary at offset,
// which is the whole input this test varies.
func writeInTwo(t *testing.T, w interface{ Write([]byte) (int, error) }, line string, offset int) {
	t.Helper()

	for _, piece := range []string{line[:offset], line[offset:]} {
		if piece == "" {
			continue
		}
		if _, err := w.Write([]byte(piece)); err != nil {
			t.Fatalf("writing %q to the capture: %v", piece, err)
		}
	}
}
