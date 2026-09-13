package workflowrun

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// A chunk that arrives after the step's output has been closed off is
// refused, not recorded.
//
// This is not hypothetical: a remote step whose termination this product
// could not CONFIRM is a step whose far side may still be writing, and
// its frames arrive on a socket the reader has not finished draining.
// Recording one appends to a step that already has its outcome and its
// flushed tail -- and, past the bound, output that lands AFTER the
// truncation marker which says the recording stopped. A log whose last
// record says "everything after this is missing" followed by more output
// is not a log anybody can reason about.
func TestAChunkThatArrivesAfterTheStepIsClosedIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})

	var straggler workflowexec.Sink

	h.local.outcomes["10-mount.local.sh"] = func(_ context.Context, req StepRequest) (StepOutcome, error) {
		straggler = req.Sink

		if err := req.Sink.Chunk(workflowexec.Chunk{
			Stream: workflowexec.StreamStdout, Seq: 1, Data: []byte("mounted\n"),
		}); err != nil {
			return StepOutcome{}, err
		}

		// The step ends with its termination unconfirmed, which is the
		// situation a late frame comes out of.
		return StepOutcome{
			Disposition: DispositionTransportLost,
			Detail:      "the session ended without reporting a status",
		}, nil
	}

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	before := logsOf(t, h, "run-1")
	if len(before) != 1 {
		t.Fatalf("the step recorded %d records, want 1", len(before))
	}

	err := straggler.Chunk(workflowexec.Chunk{
		Stream: workflowexec.StreamStdout, Seq: 2, Data: []byte("late output\n"),
	})
	if err == nil {
		t.Fatal("a chunk was accepted after the step's output was closed off")
	}

	after := logsOf(t, h, "run-1")
	if len(after) != len(before) {
		t.Errorf("the late chunk was recorded anyway: %d records, was %d", len(after), len(before))
	}
	for _, rec := range after {
		if strings.Contains(string(rec.Payload), "late output") {
			t.Errorf("the late output is in the journal: %q", rec.Payload)
		}
	}
}

// orderingStore records the sequence numbers in the order they reach the
// journal.
type orderingStore struct {
	Store

	mu    sync.Mutex
	order []uint64
}

func (o *orderingStore) AppendWorkflowStepLog(ctx context.Context, rec workflow.StepLog) error {
	if err := o.Store.AppendWorkflowStepLog(ctx, rec); err != nil {
		return err
	}

	o.mu.Lock()
	o.order = append(o.order, rec.Seq)
	o.mu.Unlock()

	return nil
}

// Records reach the journal in SEQUENCE order.
//
// The recorder used to number a record under its lock and then write it
// outside, on the argument that a disk write should not serialise two
// streams. What that buys is record N committing after record N+1 -- and
// a follower's cursor is precisely the claim that cannot happen: a
// consumer that has processed up to N and asks for everything after it
// never sees a record that landed later with a lower number.
func TestRecordsReachTheJournalInSequenceOrder(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})

	plan := tr.snapshot(t, "run-1")
	step := plan.Steps()[0]

	ordered := &orderingStore{Store: h.store}

	// The plan has to exist before its log can: the rows reference it.
	h.engine.Store = ordered
	t.Cleanup(func() { h.engine.Store = h.store })

	if _, err := h.run(t, plan); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rec := &logRecorder{
		store: ordered,
		runID: "run-1",
		now:   h.clock.Now,
		ctx:   context.Background(),
	}

	// Two producers, which is what the two streams of one step are, and
	// what a runner delivering frames off a socket may be.
	var wg sync.WaitGroup
	for _, stream := range []workflowexec.StreamID{workflowexec.StreamStdout, workflowexec.StreamStderr} {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range 150 {
				if err := rec.append(step.ID, stream, workflow.LogOutput, []byte("x")); err != nil {
					t.Errorf("append: %v", err)

					return
				}
			}
		}()
	}
	wg.Wait()

	ordered.mu.Lock()
	order := append([]uint64(nil), ordered.order...)
	ordered.mu.Unlock()

	for i := 1; i < len(order); i++ {
		if order[i] < order[i-1] {
			t.Fatalf("record %d reached the journal after %d; a cursor cannot survive that", order[i], order[i-1])
		}
	}
}

// The bound is per STEP and covers both of its streams together, which
// is the only reading that bounds anything: a hook that writes a
// megabyte to stderr has written a megabyte whatever it did on stdout.
//
// And what happens at the bound is one marker, in position, followed by
// nothing -- including nothing from the CLOSE, which flushes whatever
// each filter was still holding back. A flush that wrote after the
// marker would put output past the record saying the output ends.
func TestTheBoundCoversBothStreamsAndTheFlushAfterIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engine.StepOutputBytes = 64

	tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})

	h.local.outcomes["10-mount.local.sh"] = func(_ context.Context, req StepRequest) (StepOutcome, error) {
		for i := range 20 {
			stream := workflowexec.StreamStdout
			if i%2 == 1 {
				stream = workflowexec.StreamStderr
			}

			if err := req.Sink.Chunk(workflowexec.Chunk{
				Stream: stream, Seq: uint64(i + 1), Data: []byte("0123456789abcdef"),
			}); err != nil {
				return StepOutcome{}, err
			}
		}

		return exited(0), nil
	}

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	recs := logsOf(t, h, "run-1")
	if len(recs) == 0 {
		t.Fatal("nothing was captured")
	}

	markers, kept := 0, 0
	for _, rec := range recs {
		if rec.Kind == workflow.LogTruncated {
			markers++

			continue
		}
		kept += len(rec.Payload)
	}

	if markers != 1 {
		t.Errorf("the step produced %d truncation markers, want exactly 1", markers)
	}
	if kept > 64 {
		t.Errorf("%d bytes were persisted across the two streams for a step bounded at 64", kept)
	}
	if recs[len(recs)-1].Kind != workflow.LogTruncated {
		t.Errorf("the last record is %q; the marker must be where the recording stopped, flush included", recs[len(recs)-1].Kind)
	}
}

// The bound and the redaction are on the same path, and the bound must
// not be the thing that lets a credential through.
//
// A secret straddling the truncation point is the case: the filter holds
// its first half back, the second half arrives, the whole value is
// redacted -- and THEN the payload crosses the bound. What gets written
// is a marker, not a half-redacted credential, and nothing after it.
func TestASecretStraddlingTheBoundIsStillNeverPersisted(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3t-passphrase-value"

	h := newHarness(t)
	h.engine.StepOutputBytes = 48

	tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})

	secretFile := filepath.Join(custodyTempDir(t), "db.pw")
	if err := os.WriteFile(secretFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatalf("writing the secret fixture: %v", err)
	}

	plan := tr.snapshotWithEnv(t, "run-1", []workflow.EnvVar{
		{Name: "PGPASSWORD", Secret: secretref.Ref{File: secretFile}},
	})

	h.local.outcomes["10-mount.local.sh"] = func(_ context.Context, req StepRequest) (StepOutcome, error) {
		// Enough output to pass the bound, with the credential split
		// across two chunks right at it.
		for _, piece := range []string{
			"0123456789abcdef0123456789abcdef",
			"+ psql --password s3cr3t-",
			"passphrase-value --db main\n",
			"and more output after the bound\n",
		} {
			if err := req.Sink.Chunk(workflowexec.Chunk{
				Stream: workflowexec.StreamStderr, Seq: 1, Data: []byte(piece),
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
		t.Fatal("nothing was captured")
	}

	var whole strings.Builder
	markers := 0
	for _, rec := range recs {
		if rec.Kind == workflow.LogTruncated {
			markers++
		}
		whole.Write(rec.Payload)
	}

	got := whole.String()
	if strings.Contains(got, secret) {
		t.Errorf("the credential is in the journal: %q", got)
	}
	for _, half := range []string{"s3cr3t-", "passphrase-value"} {
		if strings.Contains(got, half) {
			t.Errorf("the fragment %q is in the journal, so the value can be reassembled: %q", half, got)
		}
	}
	if markers != 1 {
		t.Errorf("the step produced %d truncation markers, want 1", markers)
	}
}
