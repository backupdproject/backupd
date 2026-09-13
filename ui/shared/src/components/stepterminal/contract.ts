/**
 * The shapes the step terminal is handed, declared where the terminal
 * lives (issue #815).
 *
 * # Why these are not imported from api/contracts.ts
 *
 * The viewer is deliberately self-contained: it reads one method and
 * knows nothing else about the API surface. Declaring the four shapes it
 * touches here, rather than importing them, buys two things that are
 * worth more than the single definition would be.
 *
 * The first is that the component can be built, typechecked and tested
 * against a stub — and it is, over a hostile corpus and a fake stream,
 * with no client, no transport and no session in the picture. A viewer
 * whose tests need the real API client is a viewer whose tests cannot
 * cheaply express "what if the bytes are malicious", which is the only
 * interesting question about it.
 *
 * The second is structural typing. TypeScript matches these by shape, so
 * the host page hands over its own `WorkflowStepSummary` and its own
 * client method and the assignment is checked at the seam by the
 * compiler. Nothing needs to re-export anything, and the day the wire
 * grows a field, this file does not have to hear about it.
 *
 * Every field but the three the header cannot do without is optional, so
 * a host whose types are narrower still satisfies it. `scriptSha256` is
 * the sharp case: L6's `WireWorkflowStep` carries no hash (only
 * `WireWorkflowValidatedScript` does), so it is normally absent and the
 * header says so with an em dash rather than inventing a join.
 */

/** Which of a process's two output streams a record came from. Kept
 *  apart everywhere: merging them is irreversible, and a hook that
 *  writes a progress bar to one and a manifest to the other must not
 *  have them interleaved beyond recovery. */
export type LogStream = "stdout" | "stderr";

/** Whether a record carries a hook's bytes or says something about them.
 *  The truncation marker is IN the sequence rather than a flag on the
 *  page, because a bound reached mid-hook is a fact about a POSITION. */
export type LogKind = "output" | "truncated";

/** One captured record of one step's output, as the log read returns it. */
export interface StepLogRecord {
  seq: number;
  stream: LogStream;
  at: string;
  text: string;
  kind?: LogKind;
}

/** One page of one step's captured output: the records, the cursor to
 *  resume from, and whether the step's output has ended. */
export interface WorkflowStepLogPage {
  records: StepLogRecord[];
  cursor: number;
  complete: boolean;
  truncated: boolean;
}

/** The one call this viewer makes. `after` is the last sequence the
 *  VIEWER processed, never a position it derived itself; `wait` asks the
 *  engine's own fan-out to hold the read briefly rather than turning a
 *  tail into a poll loop. Structurally identical to the host client's
 *  `Pick<BackupdApi, "workflowStepLogs">`. */
export interface StepLogSource {
  workflowStepLogs(
    runId: string,
    stepId: string,
    options?: { after?: number; wait?: boolean }
  ): Promise<WorkflowStepLogPage>;
}

/** The lifecycle of one step, as every surface spells it. */
export type StepState =
  | "pending"
  | "running"
  | "success"
  | "failed"
  | "timed_out"
  | "canceled"
  | "skipped"
  | "interrupted";

/** One step of one workflow run: one hook script, executed once. The
 *  header draws this and nothing else — the viewer never reads the run. */
export interface WorkflowStepSummary {
  stepId: string;
  scriptName: string;
  state: StepState;
  phase?: "before" | "after";
  scope?: "global" | "set";
  order?: number;
  target?: "local" | "remote";
  executionConnectionRef?: string;
  remoteHost?: string;
  exitCode?: number | null;
  durationMs?: number;
  startedAt?: string;
  finishedAt?: string;
  timeoutMs?: number;
  terminationConfirmed?: boolean;
  scriptSha256?: string;
}
