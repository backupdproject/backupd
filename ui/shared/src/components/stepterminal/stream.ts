/**
 * The follower: one step's output, read by cursor, held in a bounded
 * queue, and dropped rather than allowed to pile up (issue #815).
 *
 * # Two positions, one rule
 *
 * Everything this class does follows from holding two numbers apart:
 *
 *   - `acknowledged` is the highest sequence the VIEW has rendered. It
 *     only moves in `take`, when the consumer has the records in hand.
 *   - `read` is the highest sequence (or page cursor) this client has
 *     asked for and received. It is where the next read starts.
 *
 * `read - acknowledged` is therefore exactly the backlog, and the rule
 * is: before each read, if the queue is already holding `queueLimit`
 * records, throw it away and set `read` back to `acknowledged`. The read
 * then asks the durable log for everything after the last sequence that
 * was actually drawn. Checked before rather than after, because a check
 * after the read discards records the consumer never had a chance to
 * take — one page bigger than the bound would be read and dropped for
 * ever.
 *
 * That single rule is what makes the three hard cases the same case:
 *
 *   - A slow or throttled consumer cannot grow memory without bound and
 *     cannot lose anything, because what is dropped is a copy of
 *     durable records that will be read again.
 *   - A paused follow makes no requests at all, and resuming starts
 *     from `acknowledged`.
 *   - A reconnect — a tab wake, a dropped response, a new component
 *     mount for the same step — is `reconnect()`, which is the drop
 *     rule applied on purpose. There is nothing to recover, because
 *     nothing was ever acknowledged that was not drawn.
 *
 * No duplicate can be delivered, because a read starts strictly after a
 * sequence and `take` never hands back a record at or below
 * `acknowledged`. No gap can open, because the only position a read ever
 * resumes from is one the consumer confirmed.
 *
 * # Why the cursor is the engine's and not ours
 *
 * A page's `cursor` may be AHEAD of its last record: the sequence
 * counter is per run, and a page filtered to one step legitimately
 * skips the records of other steps. So `read` takes the page's cursor
 * and never derives one from the records, and it only ever moves
 * forward — an engine answering with a lower cursor cannot rewind a
 * follower into re-reading what it has already shown. This is the same
 * rule `core/cmd/retnd`'s `followStepLogs` states for the CLI.
 *
 * # What this is NOT
 *
 * It is not backpressure on the engine. The engine's fan-out is
 * non-blocking and its `wait` is bounded and clamped; a follower that
 * stops reading, or one that drops everything and starts again, cannot
 * hold a hook up for a microsecond. All the buffering here is in this
 * browser and all of it is bounded — which is the whole of #815's "a
 * follower must not change a script's wall-clock duration".
 */

import { HARDENED_PROFILE } from "./profile";
import type { StepLogRecord, StepLogSource } from "./contract";

/** What the follower is doing, for the toolbar to draw. */
export type StreamPhase = "idle" | "reading" | "waiting" | "complete" | "error";

/** Everything a view needs to know about the follower, recomputed on
 *  every change rather than diffed: it is nine fields. */
export interface StreamStatus {
  phase: StreamPhase;
  /** The highest sequence the view has rendered. The resume cursor. */
  acknowledged: number;
  /** How many records are fetched but not yet rendered. */
  queued: number;
  /** How many times the backlog was thrown away and re-read from the
   *  durable log. Shown, because a viewer that silently re-reads is a
   *  viewer nobody can debug when a line appears twice. */
  resyncs: number;
  /** How many reads have been made, live or historical. */
  reads: number;
  /** The engine says this step's output has ended. */
  complete: boolean;
  /** The engine says this step's log was truncated. */
  truncated: boolean;
  /** Whether the follower is asking for more, or paused. */
  following: boolean;
  /** The last read's failure, in the words it arrived in, or null. */
  error: string | null;
}

export interface StreamOptions {
  source: StepLogSource;
  runId: string;
  stepId: string;
  /** Called whenever `status()` would answer differently, or records
   *  arrived. The view re-reads; nothing is handed through the callback,
   *  so a missed notification cannot lose data. */
  onChange?: () => void;
  /** How many unrendered records to hold before dropping the backlog and
   *  re-reading from the durable log. */
  queueLimit?: number;
  /** How long to wait between reads once the current read came back
   *  empty and the step is still running. The engine's own `wait` does
   *  most of the waiting; this is the floor under a run of empty pages. */
  idleDelayMs?: number;
  /** How long to wait after a failed read, before trying again from the
   *  same cursor. */
  retryDelayMs?: number;
  /** Injected so the loop is testable without wall-clock time. */
  sleep?: (ms: number) => Promise<void>;
}

/** The pause between empty reads. Small, because the engine's bounded
 *  `wait` is what actually keeps a tail off the poll treadmill; this only
 *  stops a spin when `wait` is not honoured. */
const IDLE_DELAY_MS = 1_000;

/** The pause after a failed read. Longer, because the interesting case
 *  is an engine that has gone away and a browser tab that will sit there
 *  retrying for an hour. */
const RETRY_DELAY_MS = 5_000;

export class StepLogStream {
  private readonly source: StepLogSource;
  private readonly runId: string;
  private readonly stepId: string;
  private readonly onChange: () => void;
  private readonly queueLimit: number;
  private readonly idleDelayMs: number;
  private readonly retryDelayMs: number;
  private readonly sleep: (ms: number) => Promise<void>;

  /** Fetched, not yet rendered. Always in ascending sequence order. */
  private queue: StepLogRecord[] = [];
  private ack = 0;
  private read = 0;
  private phase: StreamPhase = "idle";
  private resyncCount = 0;
  private readCount = 0;
  private isComplete = false;
  private isTruncated = false;
  private following = false;
  private failure: string | null = null;
  /** Bumped by `reconnect` and `dispose`, so a read already in flight
   *  cannot deliver into a stream the view has moved on from. */
  private generation = 0;
  private running = false;
  /** Abandoned for good: the view unmounted or dropped this step. */
  private disposed = false;

  constructor(options: StreamOptions) {
    this.source = options.source;
    this.runId = options.runId;
    this.stepId = options.stepId;
    this.onChange = options.onChange ?? (() => {});
    this.queueLimit = options.queueLimit ?? HARDENED_PROFILE.queueLimit;
    this.idleDelayMs = options.idleDelayMs ?? IDLE_DELAY_MS;
    this.retryDelayMs = options.retryDelayMs ?? RETRY_DELAY_MS;
    this.sleep =
      options.sleep ??
      ((ms: number) =>
        new Promise<void>((resolve) => {
          setTimeout(resolve, ms);
        }));
  }

  status(): StreamStatus {
    return {
      phase: this.phase,
      acknowledged: this.ack,
      queued: this.queue.length,
      resyncs: this.resyncCount,
      reads: this.readCount,
      complete: this.isComplete,
      truncated: this.isTruncated,
      following: this.following,
      error: this.failure
    };
  }

  /**
   * Drains up to `max` records to the consumer and acknowledges them.
   *
   * Acknowledgement is the act of taking, not a separate call, because a
   * consumer that could take without acknowledging is a consumer that
   * can lose a record and still advance the cursor — and then the gap is
   * silent and permanent. The view renders what this returns before it
   * asks again, so "taken" and "drawn" are the same moment.
   */
  take(max: number = Number.MAX_SAFE_INTEGER): StepLogRecord[] {
    if (this.queue.length === 0) return [];
    const batch = this.queue.length <= max ? this.queue : this.queue.slice(0, max);
    this.queue = this.queue.length <= max ? [] : this.queue.slice(max);
    const last = batch[batch.length - 1];
    if (last.seq > this.ack) this.ack = last.seq;

    return batch;
  }

  /**
   * One read, start to finish: check the backlog, ask the durable log
   * for what is after `read`, and enqueue what comes back.
   *
   * The drop rule is applied BEFORE the read and not after, and the
   * order is the whole of it. A check afterwards throws away records the
   * consumer never had a chance to take — a single page bigger than the
   * bound would be dropped, re-read and dropped again for ever — while a
   * check beforehand only ever discards a backlog the consumer has
   * already been given time to drain. The consequence is that `queued`
   * may briefly exceed the bound by one page: the engine's own page
   * limit is what bounds that, and one page in memory is not the failure
   * mode this rule exists for.
   *
   * Exposed rather than private because it is the unit the suite drives:
   * a test that has to start a loop and wait for timers to assert a
   * cursor is a test that is about timers.
   */
  async pump(): Promise<void> {
    // A disposed follower reads nothing at all. The generation guard
    // below covers a read that was already in flight; this covers the
    // one a caller starts afterwards.
    if (this.disposed) return;
    if (this.queue.length >= this.queueLimit) {
      // The consumer is behind. Live delivery goes, the read position
      // collapses to what was actually drawn, and the read below replays
      // from the durable log at that cursor.
      this.queue = [];
      this.read = this.ack;
      this.resyncCount++;
    }
    const generation = this.generation;
    this.phase = this.following && !this.isComplete ? "waiting" : "reading";
    this.onChange();

    let page;
    try {
      page = await this.source.workflowStepLogs(this.runId, this.stepId, {
        after: this.read,
        // Only a follow asks the engine to hold the read. A historical
        // read wants the page that exists now.
        wait: this.following && !this.isComplete
      });
    } catch (error) {
      if (generation !== this.generation) return;
      this.failure = error instanceof Error ? error.message : String(error);
      this.phase = "error";
      this.onChange();

      return;
    }
    // A read that landed after the view moved to another step is thrown
    // away whole: its records belong to a stream nobody is watching.
    if (generation !== this.generation) return;

    this.readCount++;
    this.failure = null;
    this.isComplete = page.complete;
    this.isTruncated = this.isTruncated || page.truncated;

    const fresh = page.records.filter((record) => record.seq > this.ack);
    // Sequences are monotonic in the log, but a follower that trusts
    // that without checking is a follower one bad page turns into a view
    // where line 40 is above line 39.
    fresh.sort((a, b) => a.seq - b.seq);
    this.queue = this.queue.concat(fresh);
    // The engine's cursor, never one derived from the records, and only
    // ever forward.
    this.read = Math.max(this.read, page.cursor, this.ack);


    this.phase = this.isComplete ? "complete" : "reading";
    this.onChange();
  }

  /**
   * Reads until the step's output ends, the follower is stopped, or the
   * consumer stops following.
   *
   * One loop, not a timer: every wait in it is awaited, so a stop takes
   * effect at the next boundary and no read is ever in flight twice.
   */
  async start(): Promise<void> {
    if (this.running || this.disposed) return;
    this.running = true;
    this.following = true;
    const generation = this.generation;
    this.onChange();

    try {
      while (this.running && generation === this.generation) {
        const before = this.read;
        await this.pump();
        if (!this.running || generation !== this.generation) return;
        // The engine, not the queue, decides when there is no more to
        // read: a loop that also waited for the consumer to drain would
        // never return for a consumer that has stopped drawing.
        if (this.isComplete) {
          this.phase = "complete";
          this.onChange();

          return;
        }
        if (this.failure !== null) {
          await this.sleep(this.retryDelayMs);

          continue;
        }
        // An empty read means the engine's bounded wait expired with
        // nothing new. Pause before asking again, so an engine that does
        // not honour `wait` is not asked in a tight loop.
        if (this.read === before) await this.sleep(this.idleDelayMs);
      }
    } finally {
      if (generation === this.generation) this.running = false;
    }
  }

  /** Stops reading. The cursor stays where it is, so a `start` after
   *  this resumes rather than replays. */
  stop(): void {
    this.running = false;
    this.following = false;
    if (this.phase !== "complete" && this.phase !== "error") this.phase = "idle";
    this.onChange();
  }

  /**
   * Throws the backlog away and reads again from the last acknowledged
   * sequence.
   *
   * This is what a reconnect is: the same drop rule, applied because the
   * transport is suspect rather than because the consumer is slow. It is
   * also what makes "a browser reconnect never loses durable output"
   * true by construction — the position it resumes from is the last
   * sequence that reached the screen, and everything after it is still
   * in the durable log.
   */
  reconnect(): void {
    this.generation++;
    this.running = false;
    this.queue = [];
    this.read = this.ack;
    this.resyncCount++;
    this.failure = null;
    if (this.phase !== "complete") this.phase = "idle";
    this.onChange();
  }

  /** Abandons this stream for good: the view has moved to another step
   *  or unmounted. A read in flight can no longer deliver. */
  dispose(): void {
    this.disposed = true;
    this.generation++;
    this.running = false;
    this.following = false;
    this.queue = [];
  }
}
