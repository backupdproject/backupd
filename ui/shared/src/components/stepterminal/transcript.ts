/**
 * The facts printed above one step's log, the bound on how much of the
 * log a browser keeps, and the text a copy or a download produces
 * (issue #815).
 *
 * All three are here together because they are one decision. The header
 * on screen, the clipboard and the saved file must agree — a download
 * that carried the hook's raw bytes while the screen showed them
 * sanitised would make the screen's hardening cosmetic, and a file that
 * omitted the capture-order caveat would be a file somebody reads as
 * proof of an ordering this product never claimed. So the wording lives
 * once, and the screen and the file are two renderings of it.
 */

import { CAPTURE_ORDER_NOTE, HARDENED_PROFILE, RENDERER } from "./profile";
import { removalNotice, TRUNCATION_MARKER } from "./sanitize";
import type { RemovedCounts, TerminalLine } from "./sanitize";
import type { WorkflowStepSummary } from "./contract";

/** How a step's state is worded wherever this component says it. The
 *  wire's `timed_out` is not a word, and a surface that prints the enum
 *  is a surface that made an operator learn one. */
export const STATE_WORDS: Record<WorkflowStepSummary["state"], string> = {
  pending: "Pending",
  running: "Running",
  success: "Succeeded",
  failed: "Failed",
  timed_out: "Timed out",
  canceled: "Canceled",
  skipped: "Skipped",
  interrupted: "Interrupted"
};

/**
 * Who ran this script, in the words this product uses for it.
 *
 * A local step runs under the host workflow runner — the privileged
 * helper on the NAS itself — and that is what it is called here, on
 * every screen and in every file. It is never called an engine
 * container: the engine's container is where this product's own service
 * runs, and telling an operator their `before` hook ran "in the engine
 * container" would send them looking for their script's side effects in
 * the wrong filesystem.
 *
 * A remote step names the connection it was executed over, because that
 * is the identity the configuration carries and the one an operator can
 * look up. When a host is resolved for it, the host is shown with it.
 */
export function runnerIdentity(step: WorkflowStepSummary): { label: string; identity: string } {
  if (step.target === "remote") {
    const host = step.remoteHost ?? "";
    const ref = step.executionConnectionRef ?? "";
    if (host !== "" && ref !== "" && host !== ref) return { label: "Remote", identity: ref + " · " + host };
    if (host !== "") return { label: "Remote", identity: host };
    if (ref !== "") return { label: "Remote", identity: ref };

    return { label: "Remote", identity: "connection not recorded" };
  }

  return { label: "Local", identity: "host workflow runner" };
}

/** The script's hash, short enough to read and long enough to mean
 *  something, or an em dash. Absent is the normal case: the runs API
 *  does not carry a step's hash, and a surface that filled the gap by
 *  guessing would be inventing provenance. */
export function shortSha(step: WorkflowStepSummary): string {
  const sha = step.scriptSha256 ?? "";
  if (!/^[0-9a-f]{12,64}$/i.test(sha)) return "—";

  return sha.slice(0, 12);
}

/** How long the step ran. Milliseconds under a second, because a hook
 *  that took 40ms and one that took 900ms are different facts and "0s"
 *  is neither of them. */
export function durationLabel(ms: number | undefined): string {
  if (ms === undefined || ms < 0) return "—";
  if (ms < 1_000) return Math.round(ms) + " ms";
  const seconds = ms / 1_000;
  if (seconds < 60) return seconds.toFixed(seconds < 10 ? 1 : 0) + " s";
  const whole = Math.round(seconds);
  if (whole < 3_600) return Math.floor(whole / 60) + "m " + (whole % 60) + "s";

  return Math.floor(whole / 3_600) + "h " + Math.round((whole % 3_600) / 60) + "m";
}

/** The exit code, which is a number, a deliberate null, or not known
 *  yet — three different things that must not look like each other. A
 *  step that has not finished has no code; a step that was killed before
 *  it could report one has a null; and 0 is a real answer. */
export function exitCodeLabel(step: WorkflowStepSummary): string {
  if (step.exitCode === undefined) return step.state === "running" || step.state === "pending" ? "—" : "not recorded";
  if (step.exitCode === null) return "not recorded";

  return String(step.exitCode);
}

/**
 * The lines a browser keeps, after however many arrived.
 *
 * The bound is on the RENDERED list and nothing else: the durable log is
 * untouched, and the follower's cursor is untouched, so what falls off
 * the top here is still readable with `workflow run log --cursor 0` and
 * is still in a download. What the view owes the reader is to say that
 * it dropped something, which is what the returned count is for.
 *
 * Returns the same array when nothing was dropped and nothing arrived,
 * so a re-render caused by something else does not rebuild the list.
 */
export function boundLines(
  held: TerminalLine[],
  arriving: TerminalLine[],
  limit: number = HARDENED_PROFILE.scrollbackLines
): { lines: TerminalLine[]; dropped: number } {
  if (arriving.length === 0) return { lines: held, dropped: 0 };
  const combined = held.length === 0 ? arriving : held.concat(arriving);
  if (combined.length <= limit) return { lines: combined, dropped: 0 };
  const dropped = combined.length - limit;

  return { lines: combined.slice(dropped), dropped };
}

export interface TranscriptInput {
  runId: string;
  step: WorkflowStepSummary;
  lines: TerminalLine[];
  /** How many lines the browser dropped off the top of its scrollback. */
  droppedFromHead: number;
  /** What the sanitiser took out of everything it has seen. */
  removed: RemovedCounts;
  /** The engine says this step's log was truncated. */
  truncated: boolean;
  /** The engine says this step's output has ended. */
  complete: boolean;
}

/**
 * One step's log as text, for the clipboard and for the saved file.
 *
 * It is the SANITISED content, deliberately: the durable log is already
 * redacted by the engine (secrets never reach it), and what this adds is
 * the same escape filtering the screen applies. A download that handed
 * back the raw bytes would be a download that re-armed every sequence
 * this component exists to disarm — in a file an operator will open in
 * a terminal, which is the one place where OSC 8 and a title sequence
 * still work.
 *
 * The preamble is the header, in the same words, plus what the reader of
 * a file cannot see on screen: which view produced it, what it filtered,
 * and what its ordering does and does not claim.
 */
export function transcriptText(input: TranscriptInput): string {
  const { step } = input;
  const runner = runnerIdentity(step);
  const out: string[] = [
    "# retnd workflow step log",
    "# run           " + input.runId,
    "# step          " + step.stepId,
    "# script        " + step.scriptName,
    "# phase         " + (step.phase ?? "—") + (step.scope === undefined ? "" : " · " + step.scope),
    "# executed on   " + runner.label + " · " + runner.identity,
    "# state         " + STATE_WORDS[step.state],
    "# exit code     " + exitCodeLabel(step),
    "# duration      " + durationLabel(step.durationMs),
    "# script sha256 " + shortSha(step),
    "# rendered by   " + RENDERER.name + " rev " + RENDERER.revision,
    "# " + CAPTURE_ORDER_NOTE,
    "# Escape sequences, control characters and text-direction overrides were removed before this file was written."
  ];
  if (step.terminationConfirmed === false && step.state !== "running" && step.state !== "pending") {
    out.push(
      "# WARNING: this product could not confirm the script's process had gone. Output may be incomplete, and the process may still be running."
    );
  }
  const notice = removalNotice(input.removed);
  if (notice !== null) out.push("# " + notice);
  if (input.droppedFromHead > 0) {
    out.push(
      "# " +
        input.droppedFromHead +
        " earlier lines were dropped from this browser's scrollback and are not in this file. The engine still holds them: read them with `retnd workflow run log --cursor 0`."
    );
  }
  if (!input.complete) out.push("# This step's output had not ended when this file was written.");
  out.push("");

  for (const line of input.lines) {
    if (line.marker) {
      out.push("# " + TRUNCATION_MARKER);

      continue;
    }
    // The full capture time on every line, whether or not the screen is
    // showing times: a file is read away from the screen that produced
    // it, and the stream label is the half of #815 that must survive the
    // trip.
    out.push(line.at + "  " + line.stream.padEnd(6) + "  " + line.text);
  }
  if (input.truncated) out.push("# " + TRUNCATION_MARKER);

  return out.join("\n") + "\n";
}

/** What a saved log is called. The script and the step, so two files
 *  from one run do not collide, and a timestamp, so two reads of one
 *  step do not either. */
export function transcriptFilename(step: WorkflowStepSummary, at: Date): string {
  const slug = (step.scriptName.replace(/[^a-zA-Z0-9._-]+/g, "-").replace(/^-+|-+$/g, "") || "script").slice(0, 60);

  return (
    "retnd-step-" + slug + "-" + step.stepId.replace(/[^a-zA-Z0-9._-]+/g, "-").slice(0, 24) + "-" +
    at.toISOString().replace(/[:.]/g, "-") + ".txt"
  );
}
