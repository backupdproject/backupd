/**
 * One workflow run, live or finished (issue #814, screens 1 and 2).
 *
 * # Why a run is routed top-level
 *
 * It is not a sub-resource of the backup set, even though every run has
 * one, and that is L6's own decision for a reason this page inherits: a
 * run outlives the configuration that produced it, so a set whose
 * configuration has been removed still has runs an operator needs to
 * read, and a run that is holding a set is found by asking "what is stuck
 * in this deployment" rather than by visiting each set in turn.
 *
 * # The three statuses stay three
 *
 * The header carries Backup, Workflow and Cleanup side by side and
 * derives none of them from the others (WorkflowStatusSplit's own doc
 * says why at length). Nothing on this page computes an overall verdict.
 *
 * # All five stages, always
 *
 * A stage the configuration gives no directory is drawn greyed and said
 * out loud rather than omitted, because "the global after stage did not
 * run" and "this deployment has no global after directory" are opposite
 * facts when a source has been left quiesced — and a ladder that hid the
 * ineligible ones makes them the same picture.
 *
 * # One terminal, mounted when a step is selected
 *
 * A run can plan dozens of scripts, so there is exactly one log panel and
 * it belongs to the selection. Selecting a step mounts it; moving the
 * selection tears the old one down. That keeps this page's cost
 * independent of how many hooks a deployment writes, and it is why the
 * slot takes the step as a prop rather than fetching it again.
 *
 * # What polls, and what does not
 *
 * While the run is live, the STEPS are polled — the run's own row moves
 * twice, at the start and at the end, and the steps are what change in
 * between. Nothing here polls the validation route, which hashes every
 * script and opens two connections in a real deployment.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";

import { useApi } from "@shared/api/ApiContext";
import { Banner } from "@shared/components/Banner";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { PageHeader } from "@shared/components/PageHeader";
import { StatusBadge } from "@shared/components/StatusBadge";
import { WarningBanner } from "@shared/components/WarningBanner";
import { WorkflowRecoveryBanner, useWorkflowHold } from "@shared/components/WorkflowRecoveryBanner";
import { WorkflowStatusSplit } from "@shared/components/WorkflowStatusSplit";
import { StepLogTerminal } from "@shared/components/StepLogTerminal";
import {
  RUN_PRESENTATION,
  STEP_PRESENTATION,
  buildStageViews,
  describeStepExecutor,
  durationLabel,
  elapsedLabel,
  exitCodeLabel,
  isRunLive,
  shortSha,
  stageStatus,
  targetLabel,
  unconfirmedTerminations
} from "@shared/components/workflowPresentation";
import type { StageView } from "@shared/components/workflowPresentation";
import { useAsync, usePolling } from "@shared/hooks/useAsync";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import { backupSetPath } from "@shared/utilities/routes";
import { stamp } from "@shared/utilities/format";
import type { WorkflowRun, WorkflowStepSummary } from "@shared/api/contracts";

/**
 * How often a live run's steps are re-read.
 *
 * Two seconds, which is faster than the activity feed's own cadence and
 * deliberately so: this page is opened to watch a hook that is either
 * quiescing or un-quiescing a production database, and the interesting
 * window is seconds long. It stops entirely the moment the run reaches a
 * terminal state, so a finished run costs nothing.
 */
const STEP_POLL_MS = 2_000;

export function WorkflowRunPage({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const navigate = useNavigate();
  const { runId = "" } = useParams<{ runId: string }>();

  const run = useAsync(() => api.workflowRun(runId), [api, runId]);
  // The steps are their own read for the reason the API made them their
  // own route. They also start from the run's own copy, so the first
  // paint is not empty while a second request is in flight.
  const steps = useAsync(() => api.workflowSteps(runId), [api, runId]);
  const workflow = useAsync(
    () =>
      run.data
        ? api.getBackupSetWorkflow(...splitSetId(run.data.backupSetId))
        : Promise.resolve(null),
    [api, run.data?.backupSetId]
  );

  const [selectedStepId, setSelectedStepId] = useState<string | null>(null);

  const live = run.data !== null && isRunLive(run.data);

  // The poll's callback has to be STABLE, and this is not a style
  // preference: usePolling takes `reload` as an effect dependency, so an
  // identity that changes every render clears and re-arms the interval
  // every render and it never elapses. useAsync hands out a fresh
  // `reload` closure per render, so the two are kept behind a ref and the
  // callback the interval holds is created once. The failure this
  // prevents is silent in exactly the way that matters here: a live run
  // that simply stops updating.
  const latest = useRef({ run, steps });
  useEffect(() => {
    latest.current = { run, steps };
  }, [run, steps]);
  const reloadLive = useCallback(() => {
    latest.current.steps.reload();
    latest.current.run.reload();
  }, []);
  usePolling(STEP_POLL_MS, reloadLive, live);

  const hold = useWorkflowHold(run.data?.backupSetId ?? "");

  const stepList = steps.data ?? run.data?.steps ?? [];
  const stages = useMemo(
    () => buildStageViews(stepList, workflow.data?.stages ?? []),
    [stepList, workflow.data]
  );
  const unconfirmed = useMemo(() => unconfirmedTerminations(stepList), [stepList]);
  const selected = stepList.find((step) => step.stepId === selectedStepId) ?? null;

  // A selection that no longer exists is dropped rather than left
  // pointing at nothing: a run being re-planned changes its step ids, and
  // a slot holding a stale one would poll a log route that answers
  // WORKFLOW_STEP_NOT_FOUND for as long as the page stayed open.
  useEffect(() => {
    if (selectedStepId !== null && !stepList.some((step) => step.stepId === selectedStepId)) {
      setSelectedStepId(null);
    }
  }, [selectedStepId, stepList]);

  // Bound once, because the header reads it several times and TypeScript
  // cannot know a field it re-read is still the same object.
  const loaded = run.data;

  const header = (
    <PageHeader
      back={
        loaded
          ? {
              label: "Backup set",
              onClick: () => navigate(backupSetPath(...splitSetId(loaded.backupSetId)))
            }
          : undefined
      }
      tip="workflow.run.page"
      title={
        <span style={{ display: "inline-flex", alignItems: "center", gap: 11, flexWrap: "wrap" }}>
          Workflow run
          {run.data ? (
            <InfoTooltip id="workflow.run.state">
              <StatusBadge
                tone={RUN_PRESENTATION[run.data.state].tone}
                icon={RUN_PRESENTATION[run.data.state].icon}
              >
                {RUN_PRESENTATION[run.data.state].label}
              </StatusBadge>
            </InfoTooltip>
          ) : null}
          {run.data?.bypassed ? (
            // Prominent and beside the state, not a footnote: a run whose
            // hooks were skipped has a workflow verdict that says nothing
            // about anybody's hooks.
            <InfoTooltip id="workflow.run.bypassed">
              <StatusBadge tone="warn" icon="warning">
                Scripts bypassed
              </StatusBadge>
            </InfoTooltip>
          ) : null}
        </span>
      }
      subtitle={
        run.data ? (
          <span className="mono">
            {run.data.runId +
              " \u00b7 " +
              run.data.backupSetId +
              " \u00b7 " +
              (run.data.startedAt ? "started " + stamp(run.data.startedAt) : "not started") +
              (run.data.finishedAt ? " \u00b7 finished " + stamp(run.data.finishedAt) : "") +
              " \u00b7 " +
              durationLabel(run.data.durationMs) +
              " \u00b7 " +
              run.data.scriptCount +
              " scripts planned"}
          </span>
        ) : undefined
      }
      actions={
        <InfoTooltip id="workflow.run.reread" alignEnd>
          <button className="btn" onClick={reloadLive}>
            Re-read
          </button>
        </InfoTooltip>
      }
    />
  );

  // Only when there is nothing to show. A run already on screen whose
  // NEXT poll failed must not be replaced by an error page: the failure
  // is transient by construction (a two-second poll), and blanking the
  // page an operator is reading in order to report it is worse than
  // reporting it beside what they were reading.
  if (run.error && run.data === null) {
    return (
      <>
        {header}
        <ErrorState
          message={run.error.message}
          remediation={run.error.remediation}
          correlationId={run.error.correlationId}
          onRetry={run.reload}
        />
      </>
    );
  }

  if (loaded === null) {
    return (
      <>
        {header}
        <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>Loading this workflow run…</p>
      </>
    );
  }

  return (
    <>
      {header}

      {run.error ? (
        <Banner tone="danger" dismissKey={run.error.message}>
          <span style={{ fontSize: "var(--text-sm)" }}>
            {"This run could not be re-read (" + run.error.message +
              "), so what is below is the last answer that arrived."}
          </span>
        </Banner>
      ) : null}

      {hold.hold ? (
        <div style={{ marginBottom: 14 }}>
          <WorkflowRecoveryBanner
            hold={hold.hold}
            readOnly={readOnly}
            onSettled={() => {
              hold.reload();
              reloadLive();
            }}
          />
        </div>
      ) : null}

      {unconfirmed.length > 0 ? (
        <div style={{ marginBottom: 14 }}>
          {/* A page-level banner and not a table cell, because this is a
              fact about a machine rather than about a row: the script may
              still be running on the host it was sent to, and nothing in
              this product can end it. */}
          <WarningBanner
            tone="warn"
            eyebrow="Termination unconfirmed"
            title={
              unconfirmed.length === 1
                ? "One step was signalled and this product cannot confirm it stopped"
                : unconfirmed.length + " steps were signalled and this product cannot confirm they stopped"
            }
            tip="workflow.run.termination-unconfirmed"
            dismissible={false}
          >
            <span>
              {unconfirmed.map((step) => step.scriptName).join(", ") +
                " \u2014 the execution channel closed before the process reported its own exit, so " +
                "the script may still be running on the machine it was sent to. Nothing here can end it."}
            </span>
          </WarningBanner>
        </div>
      ) : null}

      <div style={{ marginBottom: 14 }}>
        <WorkflowStatusSplit run={loaded} />
      </div>

      {live ? (
        <Banner tone="info" dismissible={false}>
          <span style={{ fontSize: 13 }}>
            {"This run is still going: " +
              RUN_PRESENTATION[loaded.state].label.toLowerCase() +
              ". The stage marked as running below is the one executing now, and these steps " +
              "re-read every two seconds."}
          </span>
        </Banner>
      ) : null}

      <section className="card" style={{ marginTop: 14 }}>
        <div className="card__header">
          <InfoTooltip id="workflow.run.stages">
            <h2 className="eyebrow">Stages</h2>
          </InfoTooltip>
          <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
            {"five stages, in the order a run executes them"}
          </span>
        </div>
        <div className="card__body" style={{ display: "flex", flexDirection: "column", gap: 0 }}>
          {steps.error ? (
            <Banner tone="danger" dismissKey={steps.error.message}>
              <span style={{ fontSize: "var(--text-sm)" }}>
                {"This run's steps could not be re-read (" + steps.error.message + ")."}
              </span>
            </Banner>
          ) : null}
          {stages.map((stage) => (
            <Stage
              key={stage.key}
              stage={stage}
              run={loaded}
              selectedStepId={selectedStepId}
              onSelect={setSelectedStepId}
            />
          ))}
        </div>
      </section>

      <div style={{ marginTop: 14 }}>
        {selected ? (
          <StepLogTerminal
            runId={loaded.runId}
            stepId={selected.stepId}
            step={selected}
            api={api}
          />
        ) : (
          <EmptyState title="No step is selected" tip="workflow.run.no-selection">
            {stepList.length === 0
              ? "This run recorded no steps. A run whose hooks were bypassed has none by definition."
              : "Choose a script above to read what it printed. One terminal is open at a time."}
          </EmptyState>
        )}
      </div>
    </>
  );
}

/** One stage: its head, and either its steps or the reason it has none. */
function Stage({
  stage,
  run,
  selectedStepId,
  onSelect
}: {
  stage: StageView;
  run: WorkflowRun;
  selectedStepId: string | null;
  onSelect(stepId: string): void;
}) {
  const backup = stage.key === "backup";
  const status = stageStatus(stage);
  // The stage that is executing is ranked above its own rows, and not by
  // the badge alone: the badge is the same pill a STEP carries, so on its
  // own it reads as one more row rather than as the answer to "which
  // stage". The inset rule and the tinted head are what make the answer
  // visible before any text is read — and they are a rule plus a tint,
  // never a tint alone, so the ranking survives without colour.
  const executing = !backup && (status.label === "Running now" || status.label === "In progress");
  return (
    <div
      style={{
        borderTop: "1px solid var(--border)",
        paddingTop: 10,
        marginTop: 10,
        ...(executing
          ? {
              boxShadow: "inset 3px 0 0 var(--accent)",
              paddingLeft: 11,
              marginLeft: -11,
              marginRight: -11,
              paddingRight: 11,
              background: "var(--accent-quiet)",
              borderRadius: "var(--radius-md)"
            }
          : {})
      }}
    >
      <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
        <div
          className="eyebrow"
          style={{
            fontSize: executing ? 12 : 11,
            fontWeight: executing ? 800 : 700,
            color: executing
              ? "var(--accent)"
              : stage.eligible
                ? "var(--text-2)"
                : "var(--text-3)"
          }}
        >
          {stage.label}
        </div>
        {/* The stage's own verdict, on its head. Without it, "which of
            the five stages is executing" is a question an operator can
            only answer by scanning every script row, which is the
            reading this screen exists to save them. */}
        {backup ? null : (
          <StatusBadge tone={status.tone} icon={status.icon}>
            {status.label}
          </StatusBadge>
        )}
        {backup ? (
          <StatusBadge
            tone={
              run.backupStatus === "success"
                ? "ok"
                : run.backupStatus === "failed"
                  ? "danger"
                  : run.backupStatus === "running"
                    ? "accent"
                    : "neutral"
            }
            icon={run.backupStatus === "success" ? "success" : run.backupStatus === "failed" ? "failure" : "info"}
          >
            {"Backup " + run.backupStatus}
          </StatusBadge>
        ) : null}
        {stage.dir ? (
          <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
            {stage.dir}
          </span>
        ) : null}
      </div>

      {backup ? (
        <p style={{ margin: "6px 0 0", fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
          {"The transfer itself, wrapped by the hook stages either side of it. It is shown here so the " +
            "order the hooks ran in is readable, and its verdict is the Backup column above."}
        </p>
      ) : !stage.eligible ? (
        <p style={{ margin: "6px 0 0", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
          {"No directory is configured for this stage, so it had nothing to run. That is different " +
            "from a stage that was skipped."}
        </p>
      ) : stage.steps.length === 0 ? (
        <p style={{ margin: "6px 0 0", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
          {"This stage has a directory and this run planned no scripts in it."}
        </p>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 6, marginTop: 8 }}>
          {stage.steps.map((step) => (
            <StepRow
              key={step.stepId}
              step={step}
              selected={step.stepId === selectedStepId}
              onSelect={onSelect}
            />
          ))}
        </div>
      )}
    </div>
  );
}

/** One script, executed once.
 *
 *  A real button, so the selection is reachable by keyboard and announced
 *  as pressed: `aria-pressed` is what makes "which step's output am I
 *  reading" answerable without sight, and the inset rule beside the tint
 *  is what makes it answerable without colour. */
function StepRow({
  step,
  selected,
  onSelect
}: {
  step: WorkflowStepSummary;
  selected: boolean;
  onSelect(stepId: string): void;
}) {
  const presentation = STEP_PRESENTATION[step.state];
  return (
    <button
      type="button"
      aria-pressed={selected}
      onClick={() => onSelect(step.stepId)}
      style={{
        font: "inherit",
        textAlign: "left",
        cursor: "pointer",
        display: "grid",
        gridTemplateColumns: "repeat(auto-fit, minmax(130px, 1fr))",
        gap: "6px 12px",
        alignItems: "center",
        padding: "9px 11px",
        border: "1px solid " + (selected ? "var(--accent)" : "var(--border)"),
        borderLeftWidth: 3,
        borderLeftColor: selected ? "var(--accent)" : "var(--border)",
        borderRadius: "var(--radius-md)",
        background: selected ? "var(--accent-quiet)" : "var(--surface)",
        color: "var(--text)"
      }}
    >
      <span className="mono" style={{ fontSize: 12.5, minWidth: 0, overflowWrap: "anywhere" }}>
        {step.scriptName}
      </span>
      <StatusBadge tone={step.target === "local" ? "accent" : "neutral"} icon="info">
        {targetLabel(step.target)}
      </StatusBadge>
      <span style={{ fontSize: "var(--text-xs)", color: "var(--text-2)", overflowWrap: "anywhere" }}>
        {describeStepExecutor(step)}
      </span>
      <StatusBadge tone={presentation.tone} icon={presentation.icon}>
        {presentation.label}
      </StatusBadge>
      <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
        {/* Elapsed, for a step that has not finished: the engine records
            a duration when a step ends, so a running hook would
            otherwise show an em dash and say nothing about whether it
            has been hanging for three seconds or three minutes. */}
        {elapsedLabel(step)}
      </span>
      <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
        {"exit " + exitCodeLabel(step.exitCode)}
      </span>
      <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
        {shortSha(step.scriptSha256)}
      </span>
      <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
        {step.startedAt ? stamp(step.startedAt) : "not started"}
      </span>
    </button>
  );
}

/** A backup set id is source and set joined by "/" (model.BackupSetID),
 *  and every per-set route takes the two halves. Split here rather than
 *  at each call site, and tolerant of a malformed id: a run that outlived
 *  its configuration can still carry one, and a page that threw on it
 *  would be unreadable for exactly the run most worth reading. */
function splitSetId(id: string): [string, string] {
  const cut = id.indexOf("/");
  return cut < 0 ? [id, ""] : [id.slice(0, cut), id.slice(cut + 1)];
}
