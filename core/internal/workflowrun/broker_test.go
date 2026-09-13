package workflowrun

import (
	"context"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/workflow"
	"github.com/backupdproject/backupd/core/internal/workflowexec"
)

// The fan-out, and the one thing it exists to guarantee: a follower that
// stops reading cannot slow a hook down (#811's "a slow or disconnected
// subscriber can never backpressure the script or the SSH reader").

// chattyHook is a fake hook that writes n chunks and reports how long
// writing them took, which is the wall clock the acceptance criterion is
// about.
func chattyHook(n int, took *time.Duration) func(context.Context, StepRequest) (StepOutcome, error) {
	return func(_ context.Context, req StepRequest) (StepOutcome, error) {
		start := time.Now()

		for i := range n {
			if err := req.Sink.Chunk(workflowexec.Chunk{
				Stream: workflowexec.StreamStdout,
				Seq:    uint64(i + 1),
				Data:   []byte("a line of output\n"),
			}); err != nil {
				return StepOutcome{}, err
			}
		}

		*took = time.Since(start)

		return exited(0), nil
	}
}

func TestAStalledFollowerDoesNotSlowTheHookAndCatchesUpByCursor(t *testing.T) {
	t.Parallel()

	const chunks = 400

	// The control: the same hook with nobody watching at all.
	var baseline time.Duration
	func() {
		h := newHarness(t)
		tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})
		h.local.outcomes["10-mount.local.sh"] = chattyHook(chunks, &baseline)

		if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}()

	h := newHarness(t)
	h.engine.Logs = &Broker{}

	tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})

	var stalled time.Duration
	h.local.outcomes["10-mount.local.sh"] = chattyHook(chunks, &stalled)

	// A follower with a queue of two that never reads a single record:
	// a browser tab on a laptop somebody closed.
	sub := h.engine.Logs.Subscribe("run-1", 2)
	defer sub.Close()

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The hook's wall clock is unchanged. The bound is generous
	// because a machine under load can vary by a lot; what it rules out
	// is the failure this test exists for, which is the producer
	// BLOCKING on a full queue -- and that does not take slightly
	// longer, it takes forever.
	if stalled > baseline+2*time.Second {
		t.Errorf("the hook took %s with a stalled follower and %s with none; a follower must never be in the hook's way",
			stalled, baseline)
	}

	// The follower knows it fell behind rather than silently missing
	// output.
	if !sub.Lagged() {
		t.Fatal("a follower with a queue of two that read nothing does not report having fallen behind")
	}

	// It drains whatever the queue did hold, remembers the last
	// sequence it actually processed, and catches up from the journal.
	var cursor uint64
	for {
		select {
		case rec := <-sub.Records():
			cursor = rec.Seq

			continue
		default:
		}

		break
	}

	replay, err := h.engine.LogsAfter(context.Background(), "run-1", cursor, 10_000)
	if err != nil {
		t.Fatalf("LogsAfter: %v", err)
	}

	// Gapless: the catch-up starts at the record after the cursor and
	// every sequence from there to the end is present exactly once.
	want := cursor + 1
	for _, rec := range replay {
		if rec.Seq != want {
			t.Fatalf("replay jumped from %d to %d; the follower has a hole in its output", want-1, rec.Seq)
		}
		want++
	}

	total := len(logsOf(t, h, "run-1"))
	if int(want-1) != total {
		t.Errorf("the replay ended at sequence %d and the run produced %d records", want-1, total)
	}
	if total != chunks {
		t.Errorf("the run recorded %d of %d chunks", total, chunks)
	}
}

// A follower that keeps up gets everything live, which is the whole
// point of there being a live path at all.
func TestAFollowerThatKeepsUpSeesEveryRecordLive(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engine.Logs = &Broker{}

	tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})
	h.local.output["10-mount.local.sh"] = "mounted\n"

	sub := h.engine.Logs.Subscribe("run-1", 16)
	defer sub.Close()

	received := make(chan workflow.StepLog, 16)
	done := make(chan struct{})

	go func() {
		defer close(done)

		for rec := range sub.Records() {
			received <- rec
		}
	}()

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	sub.Close()
	<-done

	if sub.Lagged() {
		t.Error("a follower that read everything reports having fallen behind")
	}

	select {
	case rec := <-received:
		if string(rec.Payload) != "mounted\n" {
			t.Errorf("the follower received %q", rec.Payload)
		}
	default:
		t.Fatal("the follower received nothing")
	}
}

// One follower's problem is not another's, and neither is the hook's.
func TestOneStalledFollowerDoesNotAffectAnother(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engine.Logs = &Broker{}

	tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})

	var took time.Duration
	h.local.outcomes["10-mount.local.sh"] = chattyHook(50, &took)

	stalled := h.engine.Logs.Subscribe("run-1", 1)
	defer stalled.Close()

	healthy := h.engine.Logs.Subscribe("run-1", 1024)
	defer healthy.Close()

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !stalled.Lagged() {
		t.Error("the stalled follower does not report having fallen behind")
	}
	if healthy.Lagged() {
		t.Error("a follower with room to spare was marked as having fallen behind because another one stalled")
	}
	if got := len(healthy.Records()); got != 50 {
		t.Errorf("the healthy follower holds %d records, want 50", got)
	}
}

// A subscription is per run: a follower watching one backup set must not
// receive another's output, which is both a privacy question and a
// correctness one (the cursors are per run).
func TestAFollowerOnlySeesItsOwnRun(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engine.Logs = &Broker{}

	tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})
	h.local.output["10-mount.local.sh"] = "mounted\n"

	other := h.engine.Logs.Subscribe("run-other", 16)
	defer other.Close()

	if _, err := h.run(t, tr.snapshot(t, "run-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := len(other.Records()); got != 0 {
		t.Errorf("a follower of another run received %d records", got)
	}
}
