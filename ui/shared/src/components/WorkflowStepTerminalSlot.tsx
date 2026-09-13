/**
 * Where the selected step's log terminal mounts (issue #814).
 *
 * # What this is, and what it is not
 *
 * It is the SEAM. The hardened, read-only step terminal is issue #815's
 * component (`components/StepLogTerminal.tsx`), and it is not this file:
 * the two were built in parallel against a fixed prop shape —
 * `{ runId, stepId, step, api }`, with `api` narrowed to the one method
 * a terminal needs — so that neither had to wait for the other and
 * neither could quietly grow a dependency on the other's internals.
 *
 * What is here is the slot itself: the header identifying the step whose
 * output is about to be shown, and a panel naming the component that
 * fills it. The `api` prop is accepted and deliberately not called from
 * here — the terminal owns every request to the log route, including the
 * cursor it resumes from, because two components polling one step's log
 * with two cursors is two terminals disagreeing about what the script
 * said.
 *
 * # Why the slot has a header at all
 *
 * Because the header is a property of the SELECTION, not of the
 * streaming: which script, where it ran, what it exited with. A page that
 * left all of it to the terminal would have nothing to show while the
 * terminal was mounting, and an operator clicking a row would see a blank
 * box.
 *
 * # Why one, mounted lazily
 *
 * A run can plan dozens of scripts. A terminal per row means a poll per
 * row against a service that answers each one with a real read, so there
 * is exactly one on the page: it is created when a step is selected and
 * torn down when the selection moves, which is what makes the run page's
 * cost independent of how many hooks a deployment writes.
 */
import { StatusBadge } from "@shared/components/StatusBadge";
import {
  STEP_PRESENTATION,
  describeStepExecutor,
  durationLabel,
  exitCodeLabel,
  shortSha,
  targetLabel
} from "@shared/components/workflowPresentation";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import type { BackupdApi, WorkflowStepSummary } from "@shared/api/contracts";

/**
 * The contract the step terminal is mounted against.
 *
 * `api` is `Pick<BackupdApi, "workflowStepLogs">` and not the whole
 * client, which is the half of this shape worth defending: a terminal
 * that held the full API could read anything, and narrowing it here means
 * the component that renders operator-authored script output has access
 * to exactly one read and no write at all.
 */
export interface StepTerminalProps {
  runId: string;
  stepId: string;
  step: WorkflowStepSummary;
  api: Pick<BackupdApi, "workflowStepLogs">;
}

export function WorkflowStepTerminalSlot({ runId, stepId, step, api }: StepTerminalProps) {
  // Referenced so the narrowed capability is a real prop rather than
  // decoration: the slot proves the one method is present and hands it
  // on, and nothing here calls it, because the cursor belongs to the
  // terminal.
  const capability = typeof api.workflowStepLogs === "function" ? "ready" : "unavailable";
  const presentation = STEP_PRESENTATION[step.state];
  return (
    <section
      className="card"
      aria-label={"Output of " + step.scriptName}
      style={{ borderColor: "var(--accent)" }}
    >
      <div
        className="card__header"
        style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 12, flexWrap: "wrap" }}
      >
        <InfoTooltip id="workflow.step.output">
          <h2 className="eyebrow mono" style={{ textTransform: "none" }}>
            {step.scriptName}
          </h2>
        </InfoTooltip>
        <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
          <StatusBadge tone={step.target === "local" ? "accent" : "neutral"} icon="info">
            {targetLabel(step.target)}
          </StatusBadge>
          <StatusBadge tone={presentation.tone} icon={presentation.icon}>
            {presentation.label}
          </StatusBadge>
        </div>
      </div>
      <div className="card__body" style={{ display: "flex", flexDirection: "column", gap: 10 }}>
        <dl
          style={{
            margin: 0,
            display: "grid",
            gridTemplateColumns: "repeat(auto-fit, minmax(150px, 1fr))",
            gap: "10px 16px",
            fontSize: 13
          }}
        >
          <SlotFact label="Stage" value={(step.scope === "global" ? "Global" : "Backup set") + " " + step.phase} />
          <SlotFact label="Runs on" value={describeStepExecutor(step)} />
          <SlotFact label="Exit code" value={exitCodeLabel(step.exitCode)} mono />
          <SlotFact label="Duration" value={durationLabel(step.durationMs)} mono />
          <SlotFact label="Script hash" value={shortSha(step.scriptSha256)} mono />
        </dl>
        <div
          style={{
            border: "1px dashed var(--border-strong)",
            borderRadius: "var(--radius-lg)",
            background: "var(--surface-2)",
            padding: "16px 18px",
            display: "flex",
            flexDirection: "column",
            gap: 6
          }}
        >
          <div style={{ fontSize: 13, fontWeight: 600 }}>Step terminal loads here</div>
          <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-2)", maxWidth: "76ch" }}>
            {"The hardened read-only log terminal for this step mounts in this panel and follows " +
              "its output by cursor. It is the only component that reads this step's log, so what it " +
              "shows and where it resumes from cannot disagree with anything else on this page."}
          </p>
          <p style={{ margin: 0, fontSize: "var(--text-xs)", color: "var(--text-3)" }} className="mono">
            {"run " + runId + " \u00b7 step " + stepId + " \u00b7 log read " + capability}
          </p>
        </div>
      </div>
    </section>
  );
}

function SlotFact({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div>
      <dt className="eyebrow" style={{ fontSize: 10, marginBottom: 3 }}>
        {label}
      </dt>
      <dd className={mono ? "mono" : undefined} style={{ margin: 0 }}>
        {value}
      </dd>
    </div>
  );
}
