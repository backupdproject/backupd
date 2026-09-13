/**
 * The follower, against a fake durable log (issue #815).
 *
 * The fake is the whole point: it is a durable, sequenced log with a
 * cursor read and a page limit, and nothing else. No client, no
 * transport, no session. Every claim #815 makes about resuming — a
 * throttled consumer, a paused follow, a reconnect, a failed read —
 * is a claim about what sequences reach the consumer, so every test
 * here ends up asserting the same two things: the consumer saw
 * 1..N, once each, in order.
 */
import { describe, expect, it, vi } from "vitest";
import { StepLogStream } from "./stream";
import type { StreamOptions } from "./stream";
import type { StepLogRecord, WorkflowStepLogPage } from "./contract";

/** A durable log that answers cursor reads, exactly as L6's route does:
 *  records strictly after `after`, at most `limit` of them, a cursor that
 *  may be ahead of the last record, and a `complete` that only becomes
 *  true when the step's output has ended. */
class FakeLog {
  records: StepLogRecord[] = [];
  ended = false;
  truncated = false;
  /** Every call's arguments, in order, so a test can assert what was
   *  asked for rather than only what came back. */
  calls: { after: number; wait: boolean }[] = [];
  /** Set to fail the next read. */
  failNext: string | null = null;
  /** How far the run's sequence counter has gone, which may be past this
   *  step's last record: the counter is per RUN. */
  runCursor = 0;
  limit = 50;
  /** Pages this many reads have been asked for. */
  get reads(): number {
    return this.calls.length;
  }

  append(count: number, stream: "stdout" | "stderr" = "stdout"): void {
    for (let i = 0; i < count; i++) {
      this.runCursor++;
      this.records.push({
        seq: this.runCursor,
        stream,
        at: "2026-09-13T02:00:00Z",
        text: "line " + this.runCursor
      });
    }
  }

  /** Advances the run's counter without giving this step a record, which
   *  is what another step's output does. */
  skip(count: number): void {
    this.runCursor += count;
  }

  workflowStepLogs = (
    _runId: string,
    _stepId: string,
    options?: { after?: number; wait?: boolean }
  ): Promise<WorkflowStepLogPage> => {
    const after = options?.after ?? 0;
    this.calls.push({ after, wait: options?.wait === true });
    if (this.failNext !== null) {
      const message = this.failNext;
      this.failNext = null;

      return Promise.reject(new Error(message));
    }
    const available = this.records.filter((record) => record.seq > after);
    const records = available.slice(0, this.limit);
    const cursor = records.length < available.length ? records[records.length - 1].seq : Math.max(after, this.runCursor);

    return Promise.resolve({
      records,
      cursor,
      complete: this.ended && records.length === available.length,
      truncated: this.truncated
    });
  };
}

/** A consumer that renders everything it is handed, and remembers it. */
class Consumer {
  seen: number[] = [];

  drain(stream: StepLogStream): void {
    for (const record of stream.take()) this.seen.push(record.seq);
  }

  /** The assertion every test here ends with: 1..n, once each, in order. */
  expectGapFree(n: number): void {
    expect(this.seen).toEqual(Array.from({ length: n }, (_, i) => i + 1));
  }
}

function build(log: FakeLog, options: Partial<StreamOptions> = {}) {
  return new StepLogStream({
    source: log,
    runId: "run-1",
    stepId: "step-1",
    sleep: () => Promise.resolve(),
    ...options
  });
}

describe("a historical read", () => {
  it("starts at the beginning and does not ask the engine to wait", async () => {
    const log = new FakeLog();
    log.append(3);
    log.ended = true;
    const stream = build(log);

    await stream.pump();

    expect(log.calls).toEqual([{ after: 0, wait: false }]);
    expect(stream.status().queued).toBe(3);
  });

  it("acknowledges only what the consumer took", async () => {
    const log = new FakeLog();
    log.append(5);
    const stream = build(log);

    await stream.pump();
    const first = stream.take(2);

    expect(first.map((r) => r.seq)).toEqual([1, 2]);
    expect(stream.status().acknowledged).toBe(2);
    expect(stream.status().queued).toBe(3);
  });
});

describe("the cursor", () => {
  it("resumes from the page's cursor, which may be ahead of the last record", async () => {
    const log = new FakeLog();
    log.append(2);
    log.skip(40); // another step's output advanced the run's counter
    const stream = build(log);
    const consumer = new Consumer();

    await stream.pump();
    consumer.drain(stream);
    await stream.pump();

    expect(log.calls[1].after).toBe(42);
    consumer.expectGapFree(2);
  });

  it("never moves backwards, whatever the engine answers", async () => {
    const log = new FakeLog();
    log.append(4);
    const stream = build(log);
    const consumer = new Consumer();
    await stream.pump();
    consumer.drain(stream);

    // An engine that answered with a lower cursor would rewind a
    // follower into re-reading what it has already shown.
    log.runCursor = 1;
    await stream.pump();

    expect(log.calls[1].after).toBe(4);
    consumer.expectGapFree(4);
  });

  it("delivers nothing twice when a page replays what was already drawn", async () => {
    const log = new FakeLog();
    log.append(3);
    // An engine that ignores `after` and answers from the beginning
    // every time — the shape of every cursor bug there is.
    const amnesiac = {
      workflowStepLogs: () =>
        Promise.resolve({ records: log.records.slice(), cursor: 3, complete: false, truncated: false })
    };
    const stream = build(log, { source: amnesiac });
    const consumer = new Consumer();

    await stream.pump();
    consumer.drain(stream);
    await stream.pump();
    consumer.drain(stream);
    await stream.pump();
    consumer.drain(stream);

    consumer.expectGapFree(3);
  });

  it("hands records over in sequence order even if a page arrives unsorted", async () => {
    const log = new FakeLog();
    log.append(3);
    log.records = [log.records[2], log.records[0], log.records[1]];
    const stream = build(log);
    const consumer = new Consumer();

    await stream.pump();
    consumer.drain(stream);

    consumer.expectGapFree(3);
  });
});

describe("a consumer that falls behind", () => {
  it("drops the backlog and replays from the durable log, gap-free and without repeats", async () => {
    const log = new FakeLog();
    log.limit = 10;
    log.append(60);
    log.ended = true;
    const stream = build(log, { queueLimit: 25 });
    const consumer = new Consumer();

    // Reads with no draining at all: the throttled client. Three reads
    // put 30 records in a queue bounded at 25, and the fourth read is
    // where the backlog is noticed and thrown away.
    await stream.pump();
    await stream.pump();
    await stream.pump();
    expect(stream.status().resyncs).toBe(0);
    expect(stream.status().queued).toBe(30);
    await stream.pump();
    expect(stream.status().resyncs).toBe(1);
    // Only the page the replay read brought back, from the beginning.
    expect(stream.status().queued).toBe(10);
    expect(log.calls[3].after).toBe(0);

    // From here the consumer keeps up, and the records it never saw come
    // back from the durable log.
    for (let i = 0; i < 10; i++) {
      await stream.pump();
      consumer.drain(stream);
    }

    consumer.expectGapFree(60);
    expect(stream.status().resyncs).toBe(1);
  });

  it("collapses the read position onto the last line the consumer rendered", async () => {
    const log = new FakeLog();
    log.limit = 5;
    log.append(30);
    const stream = build(log, { queueLimit: 8 });

    await stream.pump();
    stream.take(3); // three lines drawn
    await stream.pump();
    await stream.pump();
    expect(stream.status().resyncs).toBe(0);

    // The backlog is 12 against a bound of 8, so this read drops it and
    // replays from sequence 3 — the last line that reached the screen.
    await stream.pump();

    expect(stream.status().resyncs).toBe(1);
    expect(stream.status().acknowledged).toBe(3);
    expect(log.calls[log.calls.length - 1].after).toBe(3);
  });

  it("never holds more than the bound plus the page it just read", async () => {
    const log = new FakeLog();
    log.limit = 40;
    log.append(500);
    const stream = build(log, { queueLimit: 25 });

    // A consumer that never draws anything at all, for as long as it
    // takes: memory is bounded by the rule, not by the log's size.
    for (let i = 0; i < 20; i++) await stream.pump();

    expect(stream.status().queued).toBeLessThanOrEqual(25 + log.limit);
  });
});

describe("pausing", () => {
  it("makes no request at all while paused", async () => {
    const log = new FakeLog();
    log.append(4);
    const stream = build(log);
    const consumer = new Consumer();
    await stream.pump();
    consumer.drain(stream);
    const reads = log.reads;

    stream.stop();
    await Promise.resolve();

    expect(log.reads).toBe(reads);
    expect(stream.status().following).toBe(false);
  });

  it("resumes at the last line drawn rather than replaying the log", async () => {
    const log = new FakeLog();
    log.limit = 2;
    log.append(6);
    const stream = build(log);
    const consumer = new Consumer();
    await stream.pump();
    consumer.drain(stream);
    stream.stop();

    log.append(3);
    while (consumer.seen.length < 9) {
      await stream.pump();
      consumer.drain(stream);
    }

    consumer.expectGapFree(9);
  });
});

describe("a reconnect", () => {
  it("loses nothing durable: it resumes at the last line drawn", async () => {
    const log = new FakeLog();
    log.limit = 4;
    log.append(12);
    const stream = build(log);
    const consumer = new Consumer();

    await stream.pump();
    consumer.drain(stream);
    await stream.pump(); // fetched, never drawn — the dropped connection
    stream.reconnect();

    expect(stream.status().queued).toBe(0);
    while (consumer.seen.length < 12) {
      await stream.pump();
      consumer.drain(stream);
    }

    consumer.expectGapFree(12);
  });

  it("cannot be delivered into by a read that was already in flight", async () => {
    const log = new FakeLog();
    log.append(5);
    // A gate rather than a bare `let`: the resolver is captured in a
    // property, which TypeScript can still see as callable. (This
    // program's lib is ES2022, so Promise.withResolvers is not typed.)
    const gate = { open: () => {} };
    const held = new Promise<void>((resolve) => {
      gate.open = resolve;
    });
    const slow = {
      workflowStepLogs: async (runId: string, stepId: string, options?: { after?: number; wait?: boolean }) => {
        await held;

        return log.workflowStepLogs(runId, stepId, options);
      }
    };
    const stream = build(log, { source: slow });

    const inFlight = stream.pump();
    stream.reconnect();
    gate.open();
    await inFlight;

    expect(stream.status().queued).toBe(0);
    expect(stream.take()).toEqual([]);
  });
});

describe("a failed read", () => {
  it("keeps the cursor and says what went wrong", async () => {
    const log = new FakeLog();
    log.append(3);
    const stream = build(log);
    const consumer = new Consumer();
    await stream.pump();
    consumer.drain(stream);

    log.failNext = "the engine is not reachable";
    await stream.pump();

    expect(stream.status().error).toBe("the engine is not reachable");
    expect(stream.status().phase).toBe("error");
    expect(stream.status().acknowledged).toBe(3);
  });

  it("retries from the same cursor and clears the error", async () => {
    const log = new FakeLog();
    log.append(2);
    const stream = build(log);
    const consumer = new Consumer();
    await stream.pump();
    consumer.drain(stream);
    log.failNext = "gone";
    await stream.pump();

    log.append(2);
    await stream.pump();
    consumer.drain(stream);

    expect(stream.status().error).toBeNull();
    consumer.expectGapFree(4);
  });
});

describe("following", () => {
  it("asks the engine to hold the read only while it is following an unfinished step", async () => {
    const log = new FakeLog();
    log.append(1);
    const stream = build(log);

    await stream.pump();
    expect(log.calls[0].wait).toBe(false);

    log.ended = true;
    const loop = stream.start();
    await loop;

    expect(log.calls[1].wait).toBe(true);
    expect(stream.status().phase).toBe("complete");
  });

  it("stops the loop when the step's output has ended", async () => {
    const log = new FakeLog();
    log.limit = 3;
    log.append(9);
    log.ended = true;
    const stream = build(log, { queueLimit: 1_000 });

    await stream.start();

    expect(stream.status().complete).toBe(true);
    expect(stream.take().map((r) => r.seq)).toEqual([1, 2, 3, 4, 5, 6, 7, 8, 9]);
  });

  it("waits between empty reads rather than spinning", async () => {
    const log = new FakeLog();
    let stream: StepLogStream | null = null;
    // Stopping from inside the first wait is what makes this
    // deterministic: the loop is proven to have reached a wait, and it
    // cannot run a second read after it.
    const sleep = vi.fn(() => {
      stream?.stop();

      return Promise.resolve();
    });
    stream = build(log, { sleep, idleDelayMs: 250 });

    await stream.start();

    expect(sleep).toHaveBeenCalledWith(250);
    expect(log.reads).toBe(1);
  });

  it("reports truncation as soon as any page says so, and keeps reporting it", async () => {
    const log = new FakeLog();
    log.append(2);
    log.truncated = true;
    const stream = build(log);

    await stream.pump();
    expect(stream.status().truncated).toBe(true);

    log.truncated = false;
    await stream.pump();

    expect(stream.status().truncated).toBe(true);
  });
});

describe("disposal", () => {
  it("stops a disposed stream from ever delivering again", async () => {
    const log = new FakeLog();
    log.append(3);
    const stream = build(log);

    stream.dispose();
    await stream.pump();

    expect(stream.take()).toEqual([]);
    expect(stream.status().queued).toBe(0);
  });
});
