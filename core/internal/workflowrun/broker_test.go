package workflowrun

import (
	"context"
	"reflect"
	"sync"
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

// A follower that disconnects while a hook is producing output must not
// take the daemon down with it.
//
// The bug this is about is a race with one outcome: offer checked
// whether the subscription was closed, released the lock, and only then
// sent on the channel, so a Close landing in that window closed the
// channel under an in-flight send -- a send on a closed channel, which
// is a panic, in the goroutine that is reading a hook's stdout. The
// window is a few instructions wide, so this exercises it many times
// rather than once, and a panic anywhere in it fails the whole test
// binary rather than this test, which is exactly the failure a daemon
// would suffer.
func TestAFollowerDisconnectingMidOutputNeverPanicsTheProducer(t *testing.T) {
	t.Parallel()

	const (
		rounds  = 300
		records = 400
	)

	b := &Broker{}

	for range rounds {
		sub := b.Subscribe("run-1", 4)

		var (
			wg      sync.WaitGroup
			started = make(chan struct{})
			once    sync.Once
		)

		wg.Add(3)

		// The producer: a hook's copy goroutine, offering every record
		// it reads.
		go func() {
			defer wg.Done()

			for i := range records {
				if i == records/8 {
					once.Do(func() { close(started) })
				}
				b.publish(workflow.StepLog{RunID: "run-1", StepID: "s", Seq: uint64(i + 1)})
			}

			once.Do(func() { close(started) })
		}()

		// A reader keeping the queue drained, so the producer reaches
		// the SEND on every offer rather than stopping at a full queue.
		// Without it the window being exercised is almost never open.
		go func() {
			defer wg.Done()

			for range sub.Records() { //nolint:revive // draining is the point
			}
		}()

		// And the follower hanging up, mid-stream.
		go func() {
			defer wg.Done()

			<-started
			sub.Close()
		}()

		wg.Wait()

		// Closing twice is one operator closing a tab while a shutdown
		// closes everything, and it must not close the channel twice
		// either.
		sub.Close()
	}
}

// The whole run's output is still recorded while that happens: a
// follower going away is not allowed to cost the journal a byte.
func TestAFollowerClosingMidRunCostsTheJournalNothing(t *testing.T) {
	t.Parallel()

	const chunks = 300

	h := newHarness(t)
	h.engine.Logs = &Broker{}

	tr := newTree(t, map[stage][]string{globalBefore: {"10-mount.local.sh"}})

	sub := h.engine.Logs.Subscribe("run-1", 8)

	var took time.Duration
	chatty := chattyHook(chunks, &took)
	h.local.outcomes["10-mount.local.sh"] = func(ctx context.Context, req StepRequest) (StepOutcome, error) {
		// The follower hangs up while the hook is mid-stream, from
		// another goroutine, which is what a closed browser tab is.
		go sub.Close()

		return chatty(ctx, req)
	}

	res, err := h.run(t, tr.snapshot(t, "run-1"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != workflow.StateSuccess {
		t.Errorf("the run ended %q, want success", res.State)
	}

	recs := logsOf(t, h, "run-1")
	if len(recs) != chunks {
		t.Fatalf("the journal holds %d of %d chunks", len(recs), chunks)
	}
	for i, rec := range recs {
		if rec.Seq != uint64(i+1) {
			t.Fatalf("record %d has sequence %d; the journal's sequence is not contiguous", i, rec.Seq)
		}
	}
}

// A follower that has been DROPPED gets nothing more on the live path,
// and that is what makes its cursor usable.
//
// The alternative -- keep offering, and let whatever fits go in -- is
// worse than dropping the follower, because what it produces is a queue
// holding 1, 2, then 301: a consumer reading it sees three records in
// order with no way to know that the third is not the next one. The
// contract is that the LIVE stream is contiguous up to the drop and the
// journal is the authority after it.
func TestADroppedFollowerReceivesNothingMoreSoItsCursorHasNoHole(t *testing.T) {
	t.Parallel()

	b := &Broker{}
	sub := b.Subscribe("run-1", 2)
	defer sub.Close()

	for i := range 3 {
		b.publish(workflow.StepLog{RunID: "run-1", StepID: "s", Seq: uint64(i + 1)})
	}

	if !sub.Lagged() {
		t.Fatal("a follower whose queue of two overflowed does not report having fallen behind")
	}

	// The follower processes one record, which makes room again. The
	// producer keeps going.
	first := <-sub.Records()
	if first.Seq != 1 {
		t.Fatalf("the first live record is %d, want 1", first.Seq)
	}

	for i := 3; i < 10; i++ {
		b.publish(workflow.StepLog{RunID: "run-1", StepID: "s", Seq: uint64(i + 1)})
	}

	sub.Close()

	var live []uint64
	for rec := range sub.Records() {
		live = append(live, rec.Seq)
	}

	want := []uint64{2}
	if !reflect.DeepEqual(live, want) {
		t.Errorf("after the drop the live stream delivered %v, want %v -- everything after the hole belongs to the journal, not the queue", live, want)
	}
}
