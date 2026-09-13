package workflowrun

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/obs"
	"github.com/backupdproject/backupd/core/internal/secretref"
	"github.com/backupdproject/backupd/core/internal/workflow"
	"github.com/backupdproject/backupd/core/internal/workflowexec"
)

// The durable log: what is written down, what is deliberately not, and
// the two properties a hook's output has that an ordinary log line does
// not -- it is unbounded, and it arrives in pieces whose boundaries mean
// nothing.

func logsOf(t *testing.T, h *harness, runID string) []workflow.StepLog {
	t.Helper()

	recs, err := h.store.WorkflowStepLogsAfter(context.Background(), runID, 0, 10_000)
	if err != nil {
		t.Fatalf("WorkflowStepLogsAfter: %v", err)
	}

	return recs
}

func TestStepOutputIsCapturedInOrderWithItsStreamAndTime(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})

	h.local.outcomes["10-mount.local.sh"] = func(_ context.Context, req StepRequest) (StepOutcome, error) {
		for i, text := range []string{"mounting\n", "still mounting\n"} {
			if err := req.Sink.Chunk(workflowexec.Chunk{
				Stream: workflowexec.StreamStdout,
				Seq:    uint64(i + 1),
				Data:   []byte(text),
			}); err != nil {
				return StepOutcome{}, err
			}
		}
		if err := req.Sink.Chunk(workflowexec.Chunk{
			Stream: workflowexec.StreamStderr,
			Seq:    3,
			Data:   []byte("warning: slow\n"),
		}); err != nil {
			return StepOutcome{}, err
		}

		return exited(0), nil
	}

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	recs := logsOf(t, h, "run-1")
	if len(recs) != 3 {
		t.Fatalf("the run captured %d records, want 3: %+v", len(recs), recs)
	}

	for i, rec := range recs {
		if rec.Seq != uint64(i+1) {
			t.Errorf("record %d has sequence %d; the cursor is not contiguous", i, rec.Seq)
		}
		if rec.CapturedAt.IsZero() {
			t.Errorf("record %d has no capture time", i)
		}
		if rec.Kind != workflow.LogOutput {
			t.Errorf("record %d is kind %q", i, rec.Kind)
		}
	}

	// The two streams stay separate. Merging them is irreversible, and
	// the sequence is what keeps "which came first" answerable anyway.
	if recs[0].Stream != workflow.LogStreamStdout || recs[2].Stream != workflow.LogStreamStderr {
		t.Errorf("the streams came back as %q, %q, %q", recs[0].Stream, recs[1].Stream, recs[2].Stream)
	}
	if !bytes.Equal(recs[2].Payload, []byte("warning: slow\n")) {
		t.Errorf("the stderr payload is %q", recs[2].Payload)
	}
}

// A hook that prints its own credential -- `set -x` around a psql
// invocation is the ordinary way it happens -- must not get that
// credential written into the journal. And the value arrives split
// across two reads of a pipe, which is the case a per-chunk filter
// cannot catch.
func TestASecretSplitAcrossChunksNeverReachesTheJournal(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3t-passphrase-value"

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})

	secretFile := filepath.Join(custodyTempDir(t), "db.pw")
	if err := os.WriteFile(secretFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatalf("writing the secret fixture: %v", err)
	}

	plan := tr.snapshotWithEnv(t, "run-1", []workflow.EnvVar{
		{Name: "PGPASSWORD", Secret: secretref.Ref{File: secretFile}},
	})

	h.local.outcomes["10-mount.local.sh"] = func(_ context.Context, req StepRequest) (StepOutcome, error) {
		// Split mid-value, exactly as a 64 KiB read off a pipe would.
		for _, piece := range []string{"+ psql --password s3cr3t-", "passphrase-value --db main\n"} {
			if err := req.Sink.Chunk(workflowexec.Chunk{
				Stream: workflowexec.StreamStderr,
				Seq:    1,
				Data:   []byte(piece),
			}); err != nil {
				return StepOutcome{}, err
			}
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

	if strings.Contains(got, secret) {
		t.Errorf("the credential is in the journal: %q", got)
	}
	for _, half := range []string{"s3cr3t-", "passphrase-value"} {
		if strings.Contains(got, half) {
			t.Errorf("the fragment %q is in the journal, so the credential can be reassembled: %q", half, got)
		}
	}
	if !strings.Contains(got, "psql --password") || !strings.Contains(got, "--db main") {
		t.Errorf("the surrounding output did not survive redaction: %q", got)
	}
}

// The credential never reaches the journal FILE either, not just the rows
// this package reads back. A redaction that left the value in a payload
// somewhere else in the row would pass the test above and fail the claim.
func TestNoRedactedValueIsAnywhereInTheJournalFile(t *testing.T) {
	t.Parallel()

	const secret = "sentinel-4f2b-never-persist-this"

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})

	secretFile := filepath.Join(custodyTempDir(t), "db.pw")
	if err := os.WriteFile(secretFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatalf("writing the secret fixture: %v", err)
	}

	plan := tr.snapshotWithEnv(t, "run-1", []workflow.EnvVar{
		{Name: "PGPASSWORD", Secret: secretref.Ref{File: secretFile}},
	})

	h.local.outcomes["10-mount.local.sh"] = func(_ context.Context, req StepRequest) (StepOutcome, error) {
		// The hook is handed the real value, so it really is in this
		// process's memory: a test that never resolved anything would
		// pass against a product that persists credentials.
		found := false
		for _, kv := range req.Environ {
			if strings.Contains(kv, secret) {
				found = true
			}
		}
		if !found {
			t.Error("the hook was not handed the resolved secret, so searching the journal for it proves nothing")
		}

		return exited(0), req.Sink.Chunk(workflowexec.Chunk{
			Stream: workflowexec.StreamStdout,
			Seq:    1,
			Data:   []byte("the password is " + secret + "\n"),
		})
	}

	if _, err := h.run(t, plan); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if err := h.store.Close(); err != nil {
		t.Fatalf("closing the journal: %v", err)
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := h.dbPath + suffix

		blob, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if bytes.Contains(blob, []byte(secret)) {
			t.Errorf("the resolved secret is in %s", filepath.Base(path))
		}
	}
}

// The bound, and the thing it must NOT do.
//
// A hook that keeps printing past the bound keeps running: its output is
// still read off the pipe, because a hook blocked on a full pipe is a
// backup that never finishes. What stops is the recording, and the place
// it stopped is marked in the sequence.
func TestPersistedOutputIsBoundedWithATruncationMarker(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engine.StepOutputBytes = 64

	tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})

	const chunks = 40

	var writeErrs int
	h.local.outcomes["10-mount.local.sh"] = func(_ context.Context, req StepRequest) (StepOutcome, error) {
		for i := range chunks {
			if err := req.Sink.Chunk(workflowexec.Chunk{
				Stream: workflowexec.StreamStdout,
				Seq:    uint64(i + 1),
				Data:   []byte("0123456789abcdef"),
			}); err != nil {
				writeErrs++
			}
		}

		return exited(0), nil
	}

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if writeErrs != 0 {
		t.Errorf("%d chunks came back as write errors; a hook past the bound must keep draining, not fail", writeErrs)
	}

	recs := logsOf(t, h, "run-1")
	if len(recs) == 0 {
		t.Fatal("nothing was captured")
	}

	markers, bytesKept := 0, 0
	for _, rec := range recs {
		if rec.Kind == workflow.LogTruncated {
			markers++
			if !strings.Contains(string(rec.Payload), "truncated") {
				t.Errorf("the marker does not say what happened: %q", rec.Payload)
			}

			continue
		}
		bytesKept += len(rec.Payload)
	}

	if markers != 1 {
		t.Errorf("the step produced %d truncation markers, want exactly 1", markers)
	}
	if bytesKept > 64 {
		t.Errorf("%d bytes were persisted for a step bounded at 64", bytesKept)
	}

	// The marker is the LAST record for the step: it is where the
	// recording stopped, and a follower reading up to it knows the rest
	// is missing rather than absent.
	if recs[len(recs)-1].Kind != workflow.LogTruncated {
		t.Errorf("the truncation marker is not the last record; a follower would read the gap as the end of the output")
	}

	// The sequence is still contiguous, marker included, because the
	// marker occupies a position rather than sitting beside the stream.
	for i, rec := range recs {
		if rec.Seq != uint64(i+1) {
			t.Fatalf("record %d has sequence %d", i, rec.Seq)
		}
	}
}

// The bound is per STEP, not per run: a chatty first hook must not use up
// the recording budget of the hook that reports what actually happened.
func TestTheOutputBoundIsPerStep(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engine.StepOutputBytes = 32

	tr := newTree(t, map[stage][]string{
		globalBefore: {"10-noisy.local.sh"},
		globalAfter:  {"90-quiet.local.sh"},
	})

	h.local.outcomes["10-noisy.local.sh"] = func(_ context.Context, req StepRequest) (StepOutcome, error) {
		for range 10 {
			if err := req.Sink.Chunk(workflowexec.Chunk{
				Stream: workflowexec.StreamStdout, Seq: 1, Data: []byte("0123456789abcdef"),
			}); err != nil {
				return StepOutcome{}, err
			}
		}

		return exited(0), nil
	}
	h.local.output["90-quiet.local.sh"] = "done\n"

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	quiet := stateOf(t, h.store, "run-1", "90-quiet.local.sh")

	var kept int
	for _, rec := range logsOf(t, h, "run-1") {
		if rec.StepID == quiet.StepID && rec.Kind == workflow.LogOutput {
			kept += len(rec.Payload)
		}
	}

	if kept == 0 {
		t.Error("the second step's output was not recorded at all; the bound is being applied per run instead of per step")
	}
}

// The two vocabularies that have to agree: the stream names the
// executors produce and the strings the journal stores. They are pinned
// from here because this is the package that imports both, and a typo in
// one of them is silent everywhere else.
func TestTheStreamSpellingsAreTheOnesTheExecutorsProduce(t *testing.T) {
	t.Parallel()

	if got := workflowexec.StreamStdout.String(); got != workflow.LogStreamStdout {
		t.Errorf("the executors call stdout %q and the journal stores %q", got, workflow.LogStreamStdout)
	}
	if got := workflowexec.StreamStderr.String(); got != workflow.LogStreamStderr {
		t.Errorf("the executors call stderr %q and the journal stores %q", got, workflow.LogStreamStderr)
	}
}

// A deployment with nothing marked sensitive and a step with no secrets
// has a nil redactor all the way down, and the output still arrives
// unchanged. This is the common case, so it is the one a refactor is
// most likely to break without noticing.
func TestOutputSurvivesWithNoRedactorAtAll(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engine.Redactor = (*obs.Redactor)(nil)

	tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})
	h.local.output["10-mount.local.sh"] = "everything is fine\n"

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	recs := logsOf(t, h, "run-1")
	if len(recs) != 1 || string(recs[0].Payload) != "everything is fine\n" {
		t.Fatalf("the captured output is %+v", recs)
	}
}
