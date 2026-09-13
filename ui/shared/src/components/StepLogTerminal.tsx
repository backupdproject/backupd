/**
 * One workflow step's own log terminal: read-only, hardened, and the
 * only instance the run detail page ever mounts (issue #815).
 *
 * # What it is
 *
 * A log renderer. Not a terminal emulator, not a pty, not a shell. It
 * draws text nodes and it reads pages of a durable, sequenced log by
 * cursor. There is no path from a byte a hook printed to anything
 * happening in this application: nothing is parsed into markup, nothing
 * becomes a link, nothing sets a title, nothing triggers a request.
 * profile.ts states that as data and the suite asserts it against what
 * this component renders; sanitize.ts is where the sequences a renderer
 * cannot be trusted with are removed before they reach the DOM.
 *
 * # Why it is self-contained
 *
 * The props are one step, one pair of ids and ONE method. That is the
 * whole seam: the host page (L7's run detail) owns the API client, the
 * timeline, the selection and the layout, and hands this component the
 * step it selected plus `Pick<BackupdApi, "workflowStepLogs">`. Nothing
 * here reaches for a context, a router, a store or a second call — which
 * is what lets the suite point it at a stub and a hostile corpus, and is
 * also what makes "no output-driven API calls" checkable rather than
 * promised: there is exactly one call site in this file.
 *
 * # One instance, a hundred steps
 *
 * A run has up to a hundred steps and every one of them has a logical
 * log; the browser has one viewer. The host mounts this component once
 * and changes `stepId`, and everything that is per-step lives in a ref
 * keyed by step: the follower, its cursor, its lines, its tally. So a
 * step change is a swap inside one instance rather than an unmount and a
 * mount — no DOM is thrown away, no scroll container is rebuilt, and
 * switching back to a step already read is instant AND resumes from the
 * cursor it reached instead of re-reading the log from the beginning.
 *
 * The cache is bounded (`HISTORY_DEPTH`), because "quickly switchable"
 * for a hundred steps must not mean a hundred step's lines held at once.
 * Evicted steps lose their held lines and their cursor, which costs one
 * durable re-read and nothing else.
 *
 * # What is deliberately absent
 *
 * No registry tooltips. Every explanation this surface owes its reader is
 * VISIBLE text — the read-only profile, the capture-order caveat, what
 * the filter removed, what the scrollback dropped — because each of them
 * is a load-bearing qualification of what is on screen, and a
 * qualification that only appears on hover is one a reader can miss
 * while drawing exactly the wrong conclusion.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { clock } from "@shared/utilities/format";
import { Icon } from "@shared/design-system/icons";
import type { IconName } from "@shared/design-system/icons";
import { StatusBadge } from "@shared/components/StatusBadge";
import type { StatusTone } from "@shared/components/StatusBadge";
import { CAPTURE_ORDER_NOTE, HARDENED_PROFILE, READ_ONLY_NOTE, RENDERER } from "@shared/components/stepterminal/profile";
import {
  addRemovals,
  noRemovals,
  removalNotice,
  sanitizeRecord,
  TRUNCATION_MARKER
} from "@shared/components/stepterminal/sanitize";
import type { RemovedCounts, TerminalLine } from "@shared/components/stepterminal/sanitize";
import { StepLogStream } from "@shared/components/stepterminal/stream";
import type { StreamStatus } from "@shared/components/stepterminal/stream";
import {
  boundLines,
  durationLabel,
  exitCodeLabel,
  runnerIdentity,
  shortSha,
  STATE_WORDS,
  transcriptFilename,
  transcriptText
} from "@shared/components/stepterminal/transcript";
import type { StepLogSource, WorkflowStepSummary } from "@shared/components/stepterminal/contract";

export type { StepLogSource, WorkflowStepLogPage, WorkflowStepSummary, StepLogRecord } from "@shared/components/stepterminal/contract";

/** How many steps' scrollbacks are kept for instant switching. Eight is
 *  the number of steps an operator moves between while comparing a
 *  failure with what ran before it; the ninth costs one durable read. */
const HISTORY_DEPTH = 8;

/** Everything the viewer holds for one step. One of these is alive per
 *  cached step; only the selected one's follower is running. */
interface StepHistory {
  stream: StepLogStream;
  lines: TerminalLine[];
  droppedFromHead: number;
  removed: RemovedCounts;
  /** Whether this step's log has ever been read, so an empty log can be
   *  told apart from a log nobody has asked for yet. */
  read: boolean;
}

/** How each state is drawn. Colour is never the message: the badge
 *  carries the word, and the icon carries the shape. */
const STATE_PRESENTATION: Record<WorkflowStepSummary["state"], { tone: StatusTone; icon: IconName }> = {
  pending: { tone: "neutral", icon: "status-idle" },
  running: { tone: "accent", icon: "status-active" },
  success: { tone: "ok", icon: "success" },
  failed: { tone: "danger", icon: "failure" },
  timed_out: { tone: "danger", icon: "failure" },
  canceled: { tone: "warn", icon: "warning" },
  skipped: { tone: "neutral", icon: "status-idle" },
  interrupted: { tone: "warn", icon: "warning" }
};

export interface StepLogTerminalProps {
  runId: string;
  stepId: string;
  step: WorkflowStepSummary;
  /** The one call this component makes. Structurally
   *  `Pick<BackupdApi, "workflowStepLogs">`, declared as its own shape so
   *  the component compiles and tests against a stub. */
  api: StepLogSource;
}

export function StepLogTerminal({ runId, stepId, step, api }: StepLogTerminalProps) {
  // Per-step state, in a ref rather than in React state, because it is
  // the thing that must SURVIVE a step change: putting it in state keyed
  // by step would either rebuild the component (which is the single
  // mounted viewer this exists to avoid) or hold every step's lines for
  // the life of the page.
  const history = useRef(new Map<string, StepHistory>());
  const [, setRevision] = useState(0);
  const [following, setFollowing] = useState(true);
  const [wrap, setWrap] = useState(true);
  const [timestamps, setTimestamps] = useState(true);
  const [copied, setCopied] = useState(false);
  const scroller = useRef<HTMLDivElement | null>(null);
  const pinnedToEnd = useRef(true);

  const key = runId + "\u0000" + stepId;

  // The follower for the selected step: created on first selection,
  // reused afterwards. Every arriving record is sanitised and folded into
  // this step's bounded scrollback at the moment it is taken, so "taken
  // from the queue" and "on its way to the screen" are the same event.
  useEffect(() => {
    const held = history.current;
    let entry = held.get(key);
    if (entry === undefined) {
      const created: StepHistory = {
        // Assigned below: the stream's onChange has to be able to see the
        // entry it is folding into.
        stream: null as unknown as StepLogStream,
        lines: [],
        droppedFromHead: 0,
        removed: noRemovals(),
        read: false
      };
      created.stream = new StepLogStream({
        source: api,
        runId,
        stepId,
        onChange: () => {
          const records = created.stream.take();
          if (records.length > 0) {
            const arriving: TerminalLine[] = [];
            for (const record of records) {
              const sanitized = sanitizeRecord(record);
              addRemovals(created.removed, sanitized.removed);
              for (const line of sanitized.lines) arriving.push(line);
            }
            const bounded = boundLines(created.lines, arriving);
            created.lines = bounded.lines;
            created.droppedFromHead += bounded.dropped;
          }
          created.read = created.read || created.stream.status().reads > 0;
          setRevision((n) => n + 1);
        }
      });
      entry = created;
      held.set(key, created);

      // The bound on how many steps are held at once. Oldest first:
      // insertion order is Map's own iteration order.
      while (held.size > HISTORY_DEPTH) {
        const oldest = held.keys().next();
        if (oldest.done === true || oldest.value === key) break;
        held.get(oldest.value)?.stream.dispose();
        held.delete(oldest.value);
      }
    }

    const stream = entry.stream;
    pinnedToEnd.current = true;
    if (following) void stream.start();
    else void stream.pump();

    return () => {
      // Switching steps or unmounting stops the follower and keeps its
      // cursor, so coming back resumes rather than replays. Nothing else
      // in the application is polling this step.
      stream.stop();
    };
  }, [api, following, key, runId, stepId]);

  // Disposed for good on unmount: a read in flight must not deliver into
  // a stream nobody is watching.
  useEffect(
    () => () => {
      for (const entry of history.current.values()) entry.stream.dispose();
      history.current.clear();
    },
    []
  );

  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 1_500);

    return () => clearTimeout(timer);
  }, [copied]);

  const entry = history.current.get(key);
  const lines = entry?.lines ?? [];
  const removed = entry?.removed ?? noRemovals();
  const droppedFromHead = entry?.droppedFromHead ?? 0;
  const status: StreamStatus = entry?.stream.status() ?? {
    phase: "idle",
    acknowledged: 0,
    queued: 0,
    resyncs: 0,
    reads: 0,
    complete: false,
    truncated: false,
    following: false,
    error: null
  };

  // The log follows its own tail unless the reader has scrolled away from
  // it, which is the one thing that makes a scrollback usable while
  // output is still arriving.
  useEffect(() => {
    const node = scroller.current;
    if (node !== null && pinnedToEnd.current) node.scrollTop = node.scrollHeight;
  }, [lines, timestamps, wrap]);

  const onScroll = useCallback(() => {
    const node = scroller.current;
    if (node === null) return;
    pinnedToEnd.current = node.scrollHeight - node.scrollTop - node.clientHeight < 24;
  }, []);

  const transcript = useCallback(
    () =>
      transcriptText({
        runId,
        step,
        lines,
        droppedFromHead,
        removed,
        truncated: status.truncated,
        complete: status.complete
      }),
    [droppedFromHead, lines, removed, runId, status.complete, status.truncated, step]
  );

  const copy = useCallback(() => {
    void navigator.clipboard?.writeText(transcript());
    setCopied(true);
  }, [transcript]);

  const download = useCallback(() => {
    // The sanitised content, deliberately: see transcriptText. A file is
    // opened in a terminal, which is the one place the sequences this
    // view filters still work.
    const blob = new Blob([transcript()], { type: "text/plain;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = transcriptFilename(step, new Date());
    anchor.click();
    URL.revokeObjectURL(url);
  }, [step, transcript]);

  const reconnect = useCallback(() => {
    const stream = history.current.get(key)?.stream;
    if (stream === undefined) return;
    // The drop rule applied on purpose: the backlog goes, the cursor
    // collapses to the last line that reached the screen, and the next
    // read replays the durable log from there. Nothing durable is lost
    // and nothing already drawn arrives twice.
    stream.reconnect();
    if (following) void stream.start();
    else void stream.pump();
  }, [following, key]);

  const runner = runnerIdentity(step);
  const state = STATE_PRESENTATION[step.state];
  const notice = removalNotice(removed);
  const facts = useMemo(
    () => [
      { term: "Executed on", value: runner.label + " · " + runner.identity },
      { term: "State", value: STATE_WORDS[step.state] },
      { term: "Exit code", value: exitCodeLabel(step) },
      { term: "Duration", value: durationLabel(step.durationMs) },
      { term: "Script SHA-256", value: shortSha(step) }
    ],
    [runner.identity, runner.label, step]
  );

  return (
    <section
      aria-label={"Log for " + step.scriptName}
      style={{
        border: "1px solid var(--border)",
        borderRadius: "var(--radius-lg)",
        background: "var(--surface)",
        overflow: "hidden",
        display: "flex",
        flexDirection: "column",
        minWidth: 0
      }}
    >
      <div
        style={{
          display: "flex",
          flexWrap: "wrap",
          alignItems: "center",
          gap: "var(--space-2)",
          padding: "var(--space-3) var(--space-4)",
          borderBottom: "1px solid var(--border)",
          background: "var(--surface-2)",
          minWidth: 0
        }}
      >
        <span className="mono" style={{ fontWeight: 600, color: "var(--text)", wordBreak: "break-all" }}>
          {step.scriptName}
        </span>
        {step.phase === undefined ? null : (
          <span style={{ fontSize: "var(--text-xs)", color: "var(--text-2)", textTransform: "uppercase", letterSpacing: "0.06em" }}>
            {step.scope === undefined ? step.phase : step.phase + " · " + step.scope}
          </span>
        )}
        <span style={{ flex: 1 }} />
        <StatusBadge tone={step.target === "remote" ? "accent" : "neutral"} icon="repositories">
          {runner.label}
        </StatusBadge>
        <StatusBadge tone={state.tone} icon={state.icon}>
          {STATE_WORDS[step.state]}
        </StatusBadge>
      </div>

      <dl
        style={{
          display: "grid",
          gridTemplateColumns: "repeat(auto-fit, minmax(160px, 1fr))",
          gap: "var(--space-2) var(--space-4)",
          margin: 0,
          padding: "var(--space-3) var(--space-4)",
          borderBottom: "1px solid var(--border)",
          minWidth: 0
        }}
      >
        {facts.map((fact) => (
          <div key={fact.term} style={{ minWidth: 0 }}>
            <dt style={{ fontSize: "var(--text-xs)", color: "var(--text-3)", textTransform: "uppercase", letterSpacing: "0.06em" }}>
              {fact.term}
            </dt>
            <dd className="mono" style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text)", wordBreak: "break-word" }}>
              {fact.value}
            </dd>
          </div>
        ))}
      </dl>

      {step.terminationConfirmed === false && step.state !== "running" && step.state !== "pending" ? (
        <p
          role="status"
          style={{
            margin: 0,
            padding: "var(--space-2) var(--space-4)",
            borderBottom: "1px solid var(--border)",
            background: "var(--warn-quiet)",
            color: "var(--warn)",
            fontSize: "var(--text-sm)"
          }}
        >
          <Icon name="warning" />{" "}
          {
            "This product could not confirm the script's process had gone. Output below may be incomplete, and the process may still be writing."
          }
        </p>
      ) : null}

      <div
        style={{
          display: "flex",
          flexWrap: "wrap",
          alignItems: "center",
          gap: "var(--space-2)",
          padding: "var(--space-2) var(--space-4)",
          borderBottom: "1px solid var(--border)",
          background: "var(--surface-2)",
          fontSize: "var(--text-xs)",
          color: "var(--text-2)",
          minWidth: 0
        }}
      >
        <button
          type="button"
          className="btn btn--sm"
          aria-pressed={following}
          onClick={() => setFollowing((on) => !on)}
        >
          {following ? "Following output" : "Follow output"}
        </button>
        <button type="button" className="btn btn--sm" aria-pressed={wrap} onClick={() => setWrap((on) => !on)}>
          Wrap
        </button>
        <button
          type="button"
          className="btn btn--sm"
          aria-pressed={timestamps}
          onClick={() => setTimestamps((on) => !on)}
        >
          Timestamps
        </button>
        <span style={{ flex: 1 }} />
        <span aria-live="off">{phaseWords(status, lines.length)}</span>
        <button type="button" className="btn btn--sm" onClick={copy}>
          {copied ? "Copied" : "Copy"}
        </button>
        <button type="button" className="btn btn--sm" onClick={download}>
          Download .txt
        </button>
      </div>

      {status.error === null ? null : (
        <p
          role="alert"
          style={{
            margin: 0,
            padding: "var(--space-2) var(--space-4)",
            borderBottom: "1px solid var(--border)",
            background: "var(--danger-quiet)",
            color: "var(--danger)",
            fontSize: "var(--text-sm)",
            display: "flex",
            flexWrap: "wrap",
            alignItems: "center",
            gap: "var(--space-2)"
          }}
        >
          <Icon name="failure" />
          <span style={{ flex: 1, minWidth: 0 }}>
            {"The log read failed: " + status.error + ". Nothing was lost — a reconnect resumes at sequence " +
              status.acknowledged + "."}
          </span>
          <button type="button" className="btn btn--sm" onClick={reconnect}>
            Reconnect
          </button>
        </p>
      )}

      <div
        ref={scroller}
        className="activity-log"
        role="log"
        aria-label={"Captured output of " + step.scriptName}
        // The scrollback IS what the registry's `workflow.step.output`
        // entry describes, and until now nothing named it: the copy was
        // written, registered and unreachable, which the tooltip
        // registry's own test reports as an entry nothing in the app
        // names. It sits on this container rather than on the toolbar
        // above because the sentence is about these bytes -- read by
        // cursor, read-only, and rendered as text rather than as markup
        // or control sequences -- which is exactly what an operator
        // hovering a wall of hook output is asking about.
        data-tip="workflow.step.output"
        // Announced only while output is actually arriving. A log this
        // size on `polite` for a finished step is a screen reader reading
        // a wall; a hook that floods it while running is the case a
        // reader has asked to be told about.
        aria-live={following && !status.complete ? "polite" : "off"}
        aria-readonly="true"
        tabIndex={0}
        spellCheck={false}
        // The profile, published from the profile itself so the surface
        // and the declaration cannot drift apart. The suite reads these.
        data-stdin={HARDENED_PROFILE.disableStdin ? "disabled" : "enabled"}
        data-link-activation={HARDENED_PROFILE.linkActivation ? "enabled" : "disabled"}
        data-title-integration={HARDENED_PROFILE.titleIntegration ? "enabled" : "disabled"}
        data-window-manipulation={HARDENED_PROFILE.windowManipulation ? "enabled" : "disabled"}
        data-device-reports={HARDENED_PROFILE.deviceReports ? "enabled" : "disabled"}
        data-scrollback={String(HARDENED_PROFILE.scrollbackLines)}
        data-renderer={RENDERER.name + " rev " + RENDERER.revision}
        onScroll={onScroll}
        style={{
          maxHeight: 360,
          minHeight: 120,
          overflowY: "scroll",
          overflowX: wrap ? "hidden" : "auto",
          padding: "var(--space-3) var(--space-4)"
        }}
      >
        {droppedFromHead > 0 ? (
          <div style={{ color: "var(--term-time)" }}>
            {"──── " + droppedFromHead +
              " earlier lines are not held in this browser any more · the engine still has them: backupd workflow run log --cursor 0 ────"}
          </div>
        ) : null}
        {lines.length === 0 ? (
          <div style={{ color: "var(--term-time)" }}>{emptyWords(step, status)}</div>
        ) : (
          lines.map((line) => (
            <StepLogLine key={line.key} line={line} withTimestamp={timestamps} wrap={wrap} />
          ))
        )}
        {status.truncated && !lines.some((line) => line.marker) ? (
          // The engine reported truncation for this step but the marker
          // record is not in the window this browser holds. The fact is
          // about the LOG, so it is stated rather than dropped with the
          // record that carried it.
          <div style={{ color: "var(--term-warn)" }}>{TRUNCATION_MARKER}</div>
        ) : null}
      </div>

      <div
        style={{
          padding: "var(--space-2) var(--space-4)",
          borderTop: "1px solid var(--border)",
          background: "var(--surface-2)",
          fontSize: "var(--text-xs)",
          color: "var(--text-3)",
          display: "flex",
          flexDirection: "column",
          gap: 2,
          minWidth: 0
        }}
      >
        <span>{CAPTURE_ORDER_NOTE}</span>
        <span>{READ_ONLY_NOTE}</span>
        {notice === null ? null : <span style={{ color: "var(--warn)" }}>{notice}</span>}
        {status.resyncs > 0 ? (
          <span>
            {"Live delivery was dropped " + status.resyncs +
              (status.resyncs === 1 ? " time" : " times") +
              " and replayed from the durable log at the last line drawn, so nothing here is missing or repeated."}
          </span>
        ) : null}
      </div>
    </section>
  );
}

/**
 * One line of captured output.
 *
 * The stream label is always drawn, never implied by colour: "stdout and
 * stderr remain distinguishable" has to hold for a reader who cannot
 * tell the two tones apart, and it has to hold in a screenshot pasted
 * into an issue. The text is a child — a text node, never markup — which
 * is the DOM half of the injection rule the sanitiser is the other half
 * of.
 */
function StepLogLine({ line, withTimestamp, wrap }: { line: TerminalLine; withTimestamp: boolean; wrap: boolean }) {
  if (line.marker) {
    return <div style={{ color: "var(--term-warn)", whiteSpace: wrap ? "pre-wrap" : "pre" }}>{line.text}</div>;
  }

  return (
    <div style={{ display: "flex", gap: "var(--space-2)", alignItems: "flex-start", minWidth: 0 }}>
      {withTimestamp ? (
        <span className="activity-log__time" style={{ flex: "none" }}>
          {clock(line.at)}
        </span>
      ) : null}
      <span
        className="activity-log__time"
        style={{ flex: "none", color: line.stream === "stderr" ? "var(--term-warn)" : "var(--term-time)" }}
      >
        {line.stream}
      </span>
      <span
        style={{
          whiteSpace: wrap ? "pre-wrap" : "pre",
          wordBreak: wrap ? "break-word" : "normal",
          color: line.stream === "stderr" ? "var(--term-warn)" : "var(--term-text)",
          minWidth: 0
        }}
      >
        {line.text}
      </span>
    </div>
  );
}

/** What the toolbar says the follower is doing, and how much it is
 *  holding. One string, because it is read at a glance. */
function phaseWords(status: StreamStatus, lines: number): string {
  const held = lines + (lines === 1 ? " line" : " lines");
  if (status.error !== null) return held + " · read failed";
  if (status.complete) return held + " · output ended";
  if (!status.following) return held + " · paused at sequence " + status.acknowledged;
  if (status.reads === 0) return held + " · reading";

  return held + " · following from sequence " + status.acknowledged;
}

/** What an empty log says. The four cases are different facts and a
 *  single "no output" would misreport three of them. */
function emptyWords(step: WorkflowStepSummary, status: StreamStatus): string {
  if (step.state === "pending") return "This step has not run yet.";
  if (step.state === "skipped") return "This step was skipped, so it wrote nothing.";
  if (status.reads === 0) return "Reading this step's log…";
  if (status.complete) return "This step wrote nothing to stdout or stderr.";

  return "No output captured yet.";
}
