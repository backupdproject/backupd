/**
 * The three verdicts of one workflow run, side by side (issue #814).
 *
 * # Why this component exists at all
 *
 * Because the alternative is a single badge, and a single badge cannot
 * say the one thing this feature exists to report. A run carries a BACKUP
 * status, a WORKFLOW status and a CLEANUP status, none of them derived
 * from the others, and "the backup succeeded and the cleanup did not" is
 * the most operationally important sentence in EPIC L: it means a machine
 * may be sitting quiesced, mounted or paused with a good backup beside
 * it. Any surface that folded the three into one verdict would make
 * exactly that state unsayable, which is why L6's journal has three
 * columns and why this component has three cells.
 *
 * # The cause belongs beside the verdict
 *
 * A failed workflow names the script that ended it, in the same cell,
 * because "Failed" on its own sends an operator to the step list to do a
 * search this component can do for them. The cause is the script's
 * basename and never a path: a path here would be a claim about a
 * filesystem this page did not read.
 *
 * # Bypass is a fact about the verdict, not a footnote
 *
 * A run whose hooks were skipped is drawn with the bypass stated inside
 * the workflow cell, because a "Skipped" verdict with no explanation
 * reads as an engine that chose not to run hooks rather than an operator
 * who asked for that.
 */
import { StatusBadge } from "@shared/components/StatusBadge";
import { STATUS_PRESENTATION } from "@shared/components/workflowPresentation";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import type { TooltipId } from "@shared/tooltips/tooltips";
import type { WorkflowRun } from "@shared/api/contracts";

/** One cell's own explanation, so each verdict says what it is a verdict
 *  ABOUT rather than sharing one sentence with the other two. */
const TIPS: Record<"backup" | "workflow" | "cleanup", TooltipId> = {
  backup: "workflow.status.backup",
  workflow: "workflow.status.workflow",
  cleanup: "workflow.status.cleanup"
};

export function WorkflowStatusSplit({ run }: { run: WorkflowRun }) {
  return (
    <div
      role="group"
      aria-label="Workflow run verdicts"
      style={{
        display: "grid",
        // Collapses to one column on a narrow layout rather than
        // shrinking three cells past the width of the words in them.
        gridTemplateColumns: "repeat(auto-fit, minmax(220px, 1fr))",
        gap: 12
      }}
    >
      <Verdict
        label="Backup"
        status={run.backupStatus}
        tip={TIPS.backup}
        detail="Whether the archive transferred and verified. Not derived from the two beside it."
      />
      <Verdict
        label="Workflow"
        status={run.workflowStatus}
        tip={TIPS.workflow}
        detail={
          run.bypassed
            ? "Hooks were bypassed for this run, so no hook was executed."
            : run.failedScript
              ? "Cause: " + run.failedScript
              : "Whether every hook this run planned ran and succeeded."
        }
      />
      <Verdict
        label="Cleanup"
        status={run.cleanupStatus}
        tip={TIPS.cleanup}
        detail={
          run.cleanupStatus === "failed"
            ? "The \u201cafter\u201d hooks were attempted and did not finish, so this source may still be quiesced."
            : "Whether the \u201cafter\u201d hooks this run owed were completed."
        }
      />
    </div>
  );
}

function Verdict({
  label,
  status,
  detail,
  tip
}: {
  label: string;
  status: WorkflowRun["backupStatus"];
  detail: string;
  tip: TooltipId;
}) {
  const presentation = STATUS_PRESENTATION[status];
  return (
    <div
      // A group with a name, so which verdict this is stays answerable
      // without sight and without relying on column position.
      role="group"
      aria-label={label + " status"}
      style={{
        border: "1px solid var(--border)",
        borderRadius: "var(--radius-lg)",
        background: "var(--surface-2)",
        padding: "11px 13px",
        minWidth: 0
      }}
    >
      <InfoTooltip id={tip} block>
        <div className="eyebrow" style={{ fontSize: 10.5, marginBottom: 6 }}>
          {label}
        </div>
      </InfoTooltip>
      <StatusBadge tone={presentation.tone} icon={presentation.icon}>
        {presentation.label}
      </StatusBadge>
      <p style={{ margin: "6px 0 0", fontSize: "var(--text-sm)", color: "var(--text-2)" }}>{detail}</p>
    </div>
  );
}
