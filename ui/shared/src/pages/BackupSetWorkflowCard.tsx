/**
 * One backup set's Workflow panel (issue #814, screen 3).
 *
 * # The first rule: a set with no hooks shows no new noise
 *
 * Most backup sets in a real deployment configure no hooks at all, and
 * this whole wave is one edit away from putting an empty table, a
 * findings list and a run history onto every one of their pages. So the
 * panel checks first whether there is anything workflow-shaped to say —
 * this set's own stages, or the deployment's globals, which DO run
 * against it — and when there is not it says one sentence and stops.
 *
 * # Why the validation is a button and not a poll
 *
 * `GET .../workflow/validation` captures and hashes every script, opens a
 * socket to the Host Workflow Runner and an SSH connection to the source.
 * A panel that read it on mount would probe an operator's production
 * database host every time somebody opened a page, and one that polled it
 * would do so on a timer. It is read on demand, and the panel says when
 * what it is showing was read.
 *
 * # Why the exec-connection picker is a picker over a proven list
 *
 * A set's own source connection may be SFTP-only, which is a supported
 * posture and not a fault: it can move bytes and cannot execute a hook.
 * The deployment's execution connections are what the settings read
 * reports, and whether one actually works for THIS set is what validation
 * proves — so the picker offers the declared list and the findings carry
 * the verdict, rather than the picker claiming a capability nobody
 * probed.
 *
 * # Local means the Host Workflow Runner
 *
 * Every `.local.sh` row says "Local Host" and names the runner. It does
 * not run in the engine container and no copy here may imply it did: the
 * runner exists precisely because that container has no shell for a hook.
 */
import { useCallback, useState } from "react";
import { useNavigate } from "react-router-dom";

import { useApi } from "@shared/api/ApiContext";
import { Banner } from "@shared/components/Banner";
import { Cell, CellGrid } from "@shared/components/Definitions";
import { ErrorState } from "@shared/components/EmptyState";
import { StatusBadge } from "@shared/components/StatusBadge";
import { WorkflowEnvironmentEditor } from "@shared/components/WorkflowEnvironmentEditor";
import {
  RUN_PRESENTATION,
  STATUS_PRESENTATION,
  durationLabel,
  hasWorkflowSurface,
  shortSha,
  targetLabel
} from "@shared/components/workflowPresentation";
import { apiErrorOf, describeFailure, workflowScriptRefusalOf } from "@shared/api/failure";
import { useAsync } from "@shared/hooks/useAsync";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import { bytes, stamp } from "@shared/utilities/format";
import { workflowRunPath } from "@shared/utilities/routes";
import type {
  WorkflowBlockingScript,
  WorkflowFinding,
  WorkflowLintFinding,
  WorkflowScriptLint,
  WorkflowValidation
} from "@shared/api/contracts";

/** How a finding's severity is drawn. "skipped" is neutral and says "not
 *  examined", which is the honest answer and NOT a pass: a green tick for
 *  a check nobody ran would be this product claiming it proved something
 *  it never looked at. */
const SEVERITY: Record<WorkflowFinding["severity"], { tone: "ok" | "warn" | "danger" | "neutral"; label: string }> = {
  ok: { tone: "ok", label: "ok" },
  skipped: { tone: "neutral", label: "not examined" },
  warning: { tone: "warn", label: "warning" },
  error: { tone: "danger", label: "error" }
};

/**
 * How one of backupd's own shell findings is drawn, and the ORDER the
 * panel groups them in (#906).
 *
 * The order is the point of this table. A findings list in arrival order
 * buries the one row that refuses a save under three that do not, and
 * "which of these is stopping me saving" is the only question an operator
 * arrives at this panel with. `rank` is therefore the grouping key and
 * the four severities are kept as four: collapsing `info` and `style`
 * into one "note" tone would be fine visually and would lose the
 * distinction the rules themselves make — a style finding is about the
 * file, an info finding is about a line in it.
 *
 * `blocks` records which severities REFUSE a configuration write, and it
 * is exactly one of them. Nothing here may imply a warning blocks a save:
 * that is the difference that makes this gate something an operator can
 * live with.
 */
const LINT_SEVERITY: Record<
  WorkflowLintFinding["severity"],
  { rank: number; tone: "ok" | "warn" | "danger" | "neutral"; label: string; blocks: boolean }
> = {
  error: { rank: 0, tone: "danger", label: "error", blocks: true },
  warning: { rank: 1, tone: "warn", label: "warning", blocks: false },
  info: { rank: 2, tone: "neutral", label: "info", blocks: false },
  style: { rank: 3, tone: "neutral", label: "style", blocks: false }
};

/** The severities in the order the panel groups them. Derived from the
 *  table above rather than written out again, so a fifth severity cannot
 *  be added to the contract and silently omitted from the panel. */
const LINT_SEVERITY_ORDER = (
  Object.keys(LINT_SEVERITY) as WorkflowLintFinding["severity"][]
).sort((a, b) => LINT_SEVERITY[a].rank - LINT_SEVERITY[b].rank);

/** A finding's position, in the one form that is useful: the form an
 *  editor jumps to. */
function positionLabel(line: number, col: number): string {
  return line + ":" + col;
}

/**
 * The state one script's verification is in, as a badge.
 *
 * Five states and the order they are tested in is the whole contract:
 *
 *   1. NOT EXAMINED wins over everything. A script nobody read has no
 *      verdict, and the findings list on it is empty for that reason
 *      rather than because it is clean — so testing "no findings" first
 *      would paint an unread script green. This is the honesty gate.
 *   2. Does not parse. The file is not a shell program; its findings are
 *      empty because the rules never ran on a tree that does not exist.
 *   3. Any error-severity finding, because that is what refuses a save
 *      and it outranks any number of warnings.
 *   4. Warnings.
 *   5. Notes (info and style together in the COUNT, because the badge is
 *      a count and "1 note, 1 note" would be nonsense; the panel keeps
 *      them apart).
 *
 * Only a script that reaches the end — read, parsed, nothing reported —
 * is drawn as clean.
 */
function lintBadge(lint: WorkflowScriptLint): {
  tone: "ok" | "warn" | "danger" | "neutral";
  icon: "success" | "failure" | "warning" | "info" | "status-idle";
  label: string;
} {
  if (!lint.examined) return { tone: "neutral", icon: "status-idle", label: "not examined" };
  if (!lint.parsed) return { tone: "danger", icon: "failure", label: "does not parse" };
  const errors = lint.findings.filter((f) => f.severity === "error").length;
  if (errors > 0) {
    return { tone: "danger", icon: "failure", label: errors + (errors === 1 ? " error" : " errors") };
  }
  const warnings = lint.findings.filter((f) => f.severity === "warning").length;
  if (warnings > 0) {
    return {
      tone: "warn",
      icon: "warning",
      label: warnings + (warnings === 1 ? " warning" : " warnings")
    };
  }
  const notes = lint.findings.length;
  if (notes > 0) {
    return { tone: "neutral", icon: "info", label: notes + (notes === 1 ? " note" : " notes") };
  }
  return { tone: "ok", icon: "success", label: "clean" };
}

/**
 * The refusal banner's body: what was refused, per script, and the one
 * sentence an operator needs before they go looking.
 *
 * Exported because BOTH workflow cards write a stage directory — this one
 * per set, WorkflowSettingsCard deployment-wide — and both are answered
 * by the same 409. Two copies of this list would be two chances for one
 * of them to start reading as though a warning had refused the save,
 * which is the one thing the copy here must never say.
 *
 * `blocking` may be empty on a refusal from an engine that predates the
 * structured list, which is why the caller keeps the service's own
 * message beside it rather than only this list: the sentence the service
 * writes names the same scripts and positions, and a banner that fell
 * back to nothing would turn an older engine's refusal into a save that
 * silently did not happen.
 */
export function WorkflowSaveRefusal({
  blocking,
  fallback
}: {
  blocking: WorkflowBlockingScript[];
  fallback: string;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
      <span style={{ fontSize: "var(--text-sm)" }}>
        {"This configuration was NOT saved. Backupd's own shell rules refuse a write that points " +
          "at a hook which does not parse, or which carries an error-severity finding. A warning, " +
          "a note or a style finding is reported and does not block a save."}
      </span>
      {blocking.length === 0 ? (
        <span style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>{fallback}</span>
      ) : (
        blocking.map((script) => (
          <div
            key={script.dir + "/" + script.scriptName}
            style={{ display: "flex", flexDirection: "column", gap: 4 }}
          >
            <span className="mono" style={{ fontSize: 12.5 }}>
              {script.scriptName}
            </span>
            <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
              {script.dir}
            </span>
            {script.parseError ? (
              <span style={{ fontSize: 12.5, color: "var(--text-2)" }}>
                {"does not parse at " +
                  positionLabel(script.parseErrorLine ?? 0, script.parseErrorCol ?? 0) +
                  ": " +
                  script.parseError}
              </span>
            ) : null}
            {script.findings.map((finding) => (
              <span
                key={finding.code + finding.line + ":" + finding.col}
                style={{ fontSize: 12.5, color: "var(--text-2)" }}
              >
                <span className="mono">
                  {finding.code + " at " + positionLabel(finding.line, finding.col)}
                </span>
                {": " + finding.message}
              </span>
            ))}
          </div>
        ))
      )}
    </div>
  );
}

export function BackupSetWorkflowCard({
  source,
  set,
  readOnly
}: {
  source: string;
  set: string;
  readOnly: boolean;
}) {
  const api = useApi();
  const navigate = useNavigate();
  const setId = source + "/" + set;

  const workflow = useAsync(() => api.getBackupSetWorkflow(source, set), [api, source, set]);
  const settings = useAsync(() => api.getWorkflowSettings(), [api]);
  const globalEnv = useAsync(() => api.listWorkflowEnvironment(), [api]);
  const setEnv = useAsync(() => api.listBackupSetWorkflowEnvironment(source, set), [api, source, set]);
  const runs = useAsync(
    () => api.workflowRuns({ backupSetId: setId, limit: 5 }),
    [api, setId]
  );

  const [validation, setValidation] = useState<WorkflowValidation | null>(null);
  const [validatedAt, setValidatedAt] = useState<string | null>(null);
  const [validating, setValidating] = useState(false);
  const [validationError, setValidationError] = useState<string | null>(null);
  /** The last workflow write this card was refused over, kept until the
   *  next write: a banner that vanished on the next render would leave an
   *  operator with a picker that snapped back and no reason why. */
  const [refusal, setRefusal] = useState<{
    blocking: WorkflowBlockingScript[];
    message: string;
  } | null>(null);

  /**
   * One workflow write, with the save gate's refusal separated from every
   * other failure.
   *
   * It re-throws either way, because the control that issued the write
   * has its own "saving" state to unwind and its own short sentence to
   * show; this only adds the structured refusal that a one-line sentence
   * cannot carry.
   */
  const writeWorkflow = useCallback(
    (patch: Parameters<typeof api.patchBackupSetWorkflow>[2]) =>
      api
        .patchBackupSetWorkflow(source, set, patch)
        .then((result) => {
          setRefusal(null);
          workflow.reload();
          return result;
        })
        .catch((e: unknown) => {
          const failure = describeFailure(e, "this workflow configuration was not saved");
          if (apiErrorOf(e)?.code === "WORKFLOW_SCRIPT_REJECTED") {
            setRefusal({ blocking: workflowScriptRefusalOf(e), message: failure.message });
          } else {
            setRefusal(null);
          }
          throw e;
        }),
    [api, source, set, workflow]
  );

  const validate = useCallback(() => {
    setValidating(true);
    setValidationError(null);
    api
      .getBackupSetWorkflowValidation(source, set)
      .then((report) => {
        setValidation(report);
        setValidatedAt(new Date().toISOString());
        setValidating(false);
      })
      .catch((e: unknown) => {
        setValidating(false);
        setValidationError(describeFailure(e, "this set's hooks could not be checked").message);
      });
  }, [api, source, set]);

  const heading = (
    <InfoTooltip id="sets.detail.workflow">
      <h2 className="eyebrow">Workflow</h2>
    </InfoTooltip>
  );

  if (workflow.error) {
    return (
      <section className="card">
        <div className="card__header">{heading}</div>
        <div className="card__body">
          <ErrorState
            message={workflow.error.message}
            remediation={workflow.error.remediation}
            correlationId={workflow.error.correlationId}
            onRetry={workflow.reload}
          />
        </div>
      </section>
    );
  }

  if (workflow.data === null) {
    return (
      <section className="card">
        <div className="card__header">{heading}</div>
        <div className="card__body">
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
            Reading this set's workflow configuration…
          </p>
        </div>
      </section>
    );
  }

  // The deployment's globals are only knowable once that read lands. A
  // page that treated "loading" or "failed" as "none configured" would
  // tell an operator this set runs no hooks at a moment when it has no
  // idea, which is the one sentence on this card that must never be a
  // guess.
  const globalsKnown = settings.data !== null;
  const globalStages = (settings.data?.beforeDir || settings.data?.afterDir)
    ? [
        ...(settings.data?.beforeDir
          ? [{ scope: "global" as const, phase: "before" as const, dir: settings.data.beforeDir }]
          : []),
        ...(settings.data?.afterDir
          ? [{ scope: "global" as const, phase: "after" as const, dir: settings.data.afterDir }]
          : [])
      ]
    : [];

  if (!globalsKnown && !workflow.data.configured && workflow.data.stages.length === 0) {
    return (
      <section className="card">
        <div className="card__header">{heading}</div>
        <div className="card__body">
          {settings.error ? (
            <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "76ch" }}>
              {"This set configures no hooks of its own. Whether this deployment configures any " +
                "globally could not be read (" + settings.error.message + "), so whether anything " +
                "runs around this set's backups is unknown."}
            </p>
          ) : (
            <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
              Reading this deployment's workflow configuration…
            </p>
          )}
        </div>
      </section>
    );
  }

  // The quiet path. One sentence, no table, no findings, no history.
  if (!hasWorkflowSurface(workflow.data, globalStages)) {
    return (
      <section className="card">
        <div className="card__header">{heading}</div>
        <div className="card__body">
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "76ch" }}>
            {"No hooks are configured for this backup set, and this deployment configures none " +
              "globally, so nothing runs before or after its backups. Hook directories are set " +
              "under Settings \u203a Workflow and per set here."}
          </p>
        </div>
      </section>
    );
  }

  const timeoutPinned = workflow.data.scriptTimeoutSeconds !== undefined;

  return (
    <section className="card">
      <div
        className="card__header"
        style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 12 }}
      >
        {heading}
        <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
          {workflow.data.configured ? setId : "inheriting the deployment's global stages only"}
        </span>
      </div>
      <div className="card__body" style={{ display: "flex", flexDirection: "column", gap: 16 }}>
        {/* Paths again, so the same width the deployment-wide card uses
            and for the same reason. */}
        <CellGrid min={280}>
          <Cell
            label="Before directory"
            tip="workflow.set.before-dir"
            value={workflow.data.beforeDir || "Not configured"}
            mono
          />
          <Cell
            label="After directory"
            tip="workflow.set.after-dir"
            value={workflow.data.afterDir || "Not configured"}
            mono
          />
          <Cell
            label="Script timeout"
            tip="workflow.set.timeout"
            value={
              workflow.data.effectiveScriptTimeoutSeconds +
              "s " +
              (timeoutPinned ? "(pinned on this set)" : "(inherited)")
            }
            mono
          />
          <Cell
            label="Remote exec connection"
            tip="workflow.set.exec-connection"
            value={workflow.data.remoteExecConnectionRef || "None chosen"}
            mono
          />
        </CellGrid>

        <ExecConnectionPicker
          ownSourceRef={setId}
          chosen={workflow.data.remoteExecConnectionRef}
          available={settings.data?.execConnections ?? []}
          readOnly={readOnly}
          onChoose={(ref) => writeWorkflow({ remoteExecConnectionRef: ref })}
        />

        {refusal ? (
          <Banner tone="danger" dismissKey={refusal.message} tip="workflow.set.save-refused">
            <WorkflowSaveRefusal blocking={refusal.blocking} fallback={refusal.message} />
          </Banner>
        ) : null}

        <div>
          <div
            style={{
              display: "flex",
              alignItems: "center",
              justifyContent: "space-between",
              gap: 10,
              flexWrap: "wrap"
            }}
          >
            <InfoTooltip id="workflow.set.discovered" block>
              <div className="eyebrow" style={{ fontSize: 10.5 }}>
                Discovered scripts
              </div>
            </InfoTooltip>
            <InfoTooltip id="workflow.set.validate" alignEnd>
              <button className="btn btn--sm" disabled={validating} onClick={validate}>
                {validating ? "Checking\u2026" : validation ? "Check again" : "Check this set's hooks"}
              </button>
            </InfoTooltip>
          </div>
          <p style={{ margin: "6px 0 0", fontSize: "var(--text-xs)", color: "var(--text-3)", maxWidth: "76ch" }}>
            {"Checking reads every script, hashes it, and verifies its shell in this process with " +
              "a Go shell parser. It executes no hook, and it is read on demand rather than on a " +
              "timer because it opens a connection to the source host and to the Host Workflow " +
              "Runner, which is also where the executor's own bash -n capability check happens."}
          </p>
          {/* Said once, next to the control, because all three sentences
              are things an operator will otherwise assume wrongly: that
              these are a general shell linter's findings (they are this
              product's own small rule set, with its own BSH codes), that
              something ran (nothing did), and that every finding is a
              blocker (only two kinds are). */}
          <p
            data-tip="workflow.set.findings"
            style={{ margin: "6px 0 0", fontSize: "var(--text-xs)", color: "var(--text-3)", maxWidth: "76ch" }}
          >
            {"The findings below are backupd's own shell rules \u2014 the BSH codes \u2014 and not " +
              "a general shell linter: the set is deliberately small, and an operator who wants a " +
              "general linter should run one. Nothing in a script was executed to produce them. A " +
              "script that does not parse, or a finding at error severity, is what REFUSES a save " +
              "of this workflow configuration; a warning, an info and a style finding are reported " +
              "and save fine."}
          </p>
          {validationError ? (
            <Banner tone="danger" dismissKey={validationError} style={{ marginTop: 8 }}>
              <span style={{ fontSize: "var(--text-sm)" }}>{validationError}</span>
            </Banner>
          ) : null}
          {validation ? (
            <ValidationReport report={validation} readAt={validatedAt} />
          ) : (
            <p style={{ margin: "8px 0 0", fontSize: 13, color: "var(--text-3)" }}>
              Nothing has been checked yet in this session.
            </p>
          )}
        </div>

        <div>
          <InfoTooltip id="workflow.set.environment" block>
            <div className="eyebrow" style={{ fontSize: 10.5, marginBottom: 8 }}>
              Environment
            </div>
          </InfoTooltip>
          {setEnv.error || globalEnv.error ? (
            <Banner tone="danger" dismissKey={(setEnv.error ?? globalEnv.error)?.message}>
              <span style={{ fontSize: "var(--text-sm)" }}>
                {"This set's workflow environment could not be read (" +
                  (setEnv.error ?? globalEnv.error)?.message +
                  "), so what a hook will see cannot be shown."}
              </span>
            </Banner>
          ) : setEnv.data === null || globalEnv.data === null ? (
            <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
              Reading the environment a hook will see…
            </p>
          ) : (
          <WorkflowEnvironmentEditor
            scope="set"
            global={globalEnv.data.variables}
            set={setEnv.data.variables}
            readOnly={readOnly}
            onSet={(name, entry) =>
              api.setBackupSetWorkflowEnvironment(source, set, name, entry).then((next) => {
                setEnv.reload();
                return next;
              })
            }
            onUnset={(name) =>
              api.unsetBackupSetWorkflowEnvironment(source, set, name).then((next) => {
                setEnv.reload();
                return next;
              })
            }
          />
          )}
        </div>

        <div>
          <InfoTooltip id="workflow.set.runs" block>
            <div className="eyebrow" style={{ fontSize: 10.5, marginBottom: 8 }}>
              Recent workflow runs
            </div>
          </InfoTooltip>
          {runs.error ? (
            <Banner tone="danger" dismissKey={runs.error.message}>
              <span style={{ fontSize: "var(--text-sm)" }}>
                {"This set's workflow runs could not be read (" + runs.error.message + ")."}
              </span>
            </Banner>
          ) : (runs.data ?? []).length === 0 ? (
            <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
              No workflow run has been recorded for this backup set yet.
            </p>
          ) : (
            <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
              {(runs.data ?? []).map((run) => (
                <button
                  key={run.runId}
                  type="button"
                  className="btn"
                  onClick={() => navigate(workflowRunPath(run.runId))}
                  style={{
                    height: "auto",
                    padding: "8px 11px",
                    display: "grid",
                    gridTemplateColumns: "repeat(auto-fit, minmax(120px, 1fr))",
                    gap: "6px 10px",
                    alignItems: "center",
                    textAlign: "left"
                  }}
                >
                  <span className="mono" style={{ fontSize: 12.5 }}>
                    {run.runId}
                  </span>
                  <StatusBadge
                    tone={RUN_PRESENTATION[run.state].tone}
                    icon={RUN_PRESENTATION[run.state].icon}
                  >
                    {RUN_PRESENTATION[run.state].label}
                  </StatusBadge>
                  {/* Both verdicts on the row, because the pair is the
                      point: a green backup beside a failed workflow is
                      the state this history exists to surface. */}
                  <span style={{ fontSize: "var(--text-xs)", color: "var(--text-2)" }}>
                    {/* All THREE verdicts, because a history row that
                        showed two would hide the one an operator is
                        scanning for: a run whose cleanup failed is the
                        row that means a machine may still be
                        quiesced. */}
                    {"backup " +
                      STATUS_PRESENTATION[run.backupStatus].label.toLowerCase() +
                      " \u00b7 workflow " +
                      STATUS_PRESENTATION[run.workflowStatus].label.toLowerCase() +
                      " \u00b7 cleanup " +
                      STATUS_PRESENTATION[run.cleanupStatus].label.toLowerCase()}
                  </span>
                  <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
                    {run.startedAt ? stamp(run.startedAt) : "not started"}
                  </span>
                  <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
                    {durationLabel(run.durationMs)}
                  </span>
                  {run.bypassed ? (
                    <StatusBadge tone="warn" icon="warning">
                      Bypassed
                    </StatusBadge>
                  ) : null}
                </button>
              ))}
            </div>
          )}
        </div>
      </div>
    </section>
  );
}

/**
 * Which connection a remote hook executes over.
 *
 * The empty option is spelled out rather than left implicit: choosing
 * none is a real configuration, and it means remote hooks for this set
 * cannot run at all. The copy says what an SFTP-only source means,
 * because that is the state an operator arrives here in and it reads like
 * a fault when it is not one.
 */
function ExecConnectionPicker({
  ownSourceRef,
  chosen,
  available,
  readOnly,
  onChoose
}: {
  /** This set's own id, which is also the reference that names its own
   *  source connection: remoteexec.Resolve accepts a "source/set" ref and
   *  resolves it to the set's own remote, and `backup-set workflow patch
   *  --exec-connection source/set` is the CLI spelling of exactly that.
   *  A picker without it would be a parity gap, and one that dropped a
   *  ref it did not recognise would PATCH an operator's choice away on
   *  the next save. */
  ownSourceRef: string;
  chosen: string;
  available: string[];
  readOnly: boolean;
  onChoose(ref: string): Promise<unknown>;
}) {
  const [saving, setSaving] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      <InfoTooltip id="workflow.set.exec-picker" block>
        <label htmlFor="workflow-exec-connection" className="eyebrow" style={{ fontSize: 10.5 }}>
          Execute remote hooks over
        </label>
      </InfoTooltip>
      <select
        id="workflow-exec-connection"
        value={chosen}
        disabled={readOnly || saving}
        onChange={(e) => {
          const next = e.target.value;
          setSaving(true);
          setFailure(null);
          onChoose(next)
            .then(() => setSaving(false))
            .catch((err: unknown) => {
              setSaving(false);
              // The save gate's refusal is drawn in full by the card's
              // own banner, which names every blocking script, its stage
              // directory and each finding's position. Repeating the
              // service's multi-line sentence on this line as well would
              // put one refusal on screen twice and read as two separate
              // things having gone wrong.
              setFailure(
                apiErrorOf(err)?.code === "WORKFLOW_SCRIPT_REJECTED"
                  ? null
                  : describeFailure(err, "that connection was not saved").message
              );
            });
        }}
        style={{
          font: "inherit",
          fontSize: 13,
          height: 30,
          padding: "0 9px",
          border: "1px solid var(--border-strong)",
          borderRadius: "var(--radius-md)",
          background: "var(--surface)",
          color: "var(--text)",
          maxWidth: 320
        }}
      >
        <option value="">None — remote hooks cannot run for this set</option>
        <option value={ownSourceRef}>
          {"This set's own source connection (only if it can execute) — " + ownSourceRef}
        </option>
        {available.map((ref) => (
          <option key={ref} value={ref}>
            {ref}
          </option>
        ))}
        {/* The currently chosen reference, ALWAYS, even when this
            deployment no longer declares it. A <select> whose value
            matches no option renders as the first one, so a set pointed
            at a connection somebody has since removed would read as
            "None" — and the next save would send that, silently throwing
            away the configuration an operator is trying to fix. */}
        {chosen !== "" && chosen !== ownSourceRef && !available.includes(chosen) ? (
          <option value={chosen}>{chosen + " — not declared in this deployment"}</option>
        ) : null}
      </select>
      <p
        data-tip="workflow.set.exec-own-source"
        style={{ margin: 0, fontSize: "var(--text-xs)", color: "var(--text-3)", maxWidth: "76ch" }}
      >
        {"A backup set's own source connection is used only when it can execute a command. An " +
          "SFTP-only source can move bytes and cannot run a hook, which is a supported setup and " +
          "not a fault: the hook check below reports that as a capability failure, and naming a " +
          "declared execution connection instead is the way out of it."}
      </p>
      {chosen !== "" && chosen !== ownSourceRef && !available.includes(chosen) ? (
        <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--warn)", maxWidth: "76ch" }}>
          {"This set names " + chosen + ", which this deployment no longer declares. Its remote " +
            "hooks cannot run until that connection is declared again or another one is chosen " +
            "here."}
        </p>
      ) : null}
      {failure ? (
        <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--danger)" }}>{failure}</p>
      ) : null}
    </div>
  );
}

/**
 * The report, as read. Two verdicts, kept apart, the discovered scripts
 * with what the shell verification made of each, and the engine's own
 * findings in the order it reported them.
 *
 * The selected script is LOCAL STATE and deliberately not a route. A
 * findings panel is a detail of one reading of one card — it is gone the
 * moment the operator checks again — so a URL that could restore it would
 * be restoring a selection into a report that no longer exists.
 */
function ValidationReport({ report, readAt }: { report: WorkflowValidation; readAt: string | null }) {
  const [selected, setSelected] = useState<string | null>(null);
  const chosen = report.scripts.find((s) => s.stepId + s.scriptName === selected) ?? null;
  return (
    <div style={{ marginTop: 10, display: "flex", flexDirection: "column", gap: 12 }}>
      <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
        <InfoTooltip id="workflow.set.valid-for-backup">
          <StatusBadge
            tone={report.validForBackup ? "ok" : "danger"}
            icon={report.validForBackup ? "success" : "failure"}
          >
            {report.validForBackup ? "Valid for backup" : "Not valid for backup"}
          </StatusBadge>
        </InfoTooltip>
        <InfoTooltip id="workflow.set.workflow-valid">
          <StatusBadge
            tone={report.workflowValid ? "ok" : "warn"}
            icon={report.workflowValid ? "success" : "warning"}
          >
            {report.workflowValid ? "Hooks valid" : "Hooks not valid"}
          </StatusBadge>
        </InfoTooltip>
        {readAt ? (
          <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
            {"read " + stamp(readAt)}
          </span>
        ) : null}
      </div>

      {report.scripts.length === 0 ? (
        <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
          {"The configured directories exist and hold no scripts, so nothing would run."}
        </p>
      ) : (
        <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 12.5 }}>
          <thead>
            <tr>
              <Th>Order</Th>
              <Th>Script</Th>
              <Th>Where</Th>
              <Th>Runs on</Th>
              <Th>Size</Th>
              <Th>Hash</Th>
              <Th>Findings</Th>
            </tr>
          </thead>
          <tbody>
            {report.scripts.map((script) => {
              const key = script.stepId + script.scriptName;
              const badge = lintBadge(script.lint);
              return (
                <tr key={key}>
                  <Td mono>{script.order}</Td>
                  <Td mono>{script.scriptName}</Td>
                  <Td>
                    <StatusBadge tone={script.target === "local" ? "accent" : "neutral"} icon="info">
                      {targetLabel(script.target)}
                    </StatusBadge>
                  </Td>
                  <Td>
                    {script.target === "local"
                      ? "Host Workflow Runner"
                      : (script.executionConnectionRef ?? "no execution connection")}
                  </Td>
                  <Td mono>{bytes(script.sizeBytes)}</Td>
                  <Td mono>{shortSha(script.sha256)}</Td>
                  <Td>
                    {/* A real affordance and not a decoration: the badge
                        states a verdict and the panel below is the only
                        place the verdict's reasons exist, so the two have
                        to be one control. A count with no way to see what
                        it counted is a number an operator cannot act
                        on. */}
                    <button
                      type="button"
                      data-tip="workflow.set.finding-severity"
                      aria-expanded={selected === key}
                      onClick={() => setSelected((current) => (current === key ? null : key))}
                      style={{
                        font: "inherit",
                        padding: 0,
                        border: "none",
                        background: "none",
                        cursor: "pointer",
                        color: "inherit"
                      }}
                    >
                      <StatusBadge tone={badge.tone} icon={badge.icon}>
                        {badge.label}
                      </StatusBadge>
                    </button>
                  </Td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}

      {chosen ? <LintPanel scriptName={chosen.scriptName} lint={chosen.lint} /> : null}

      <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
        {report.findings.map((finding) => (
          <div
            key={finding.check + finding.detail}
            style={{ display: "flex", gap: 10, alignItems: "baseline", flexWrap: "wrap" }}
          >
            <StatusBadge
              tone={SEVERITY[finding.severity].tone}
              icon={
                finding.severity === "ok"
                  ? "success"
                  : finding.severity === "error"
                    ? "failure"
                    : finding.severity === "warning"
                      ? "warning"
                      : "status-idle"
              }
            >
              {SEVERITY[finding.severity].label}
            </StatusBadge>
            <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-2)" }}>
              {finding.check}
            </span>
            <span style={{ fontSize: 12.5, color: "var(--text-2)", flex: 1, minWidth: 220 }}>
              {finding.detail}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}

/**
 * One script's shell verification, in full.
 *
 * Three shapes, and which one is drawn is decided in the same order the
 * badge decides its label, for the same reason: a script nobody read has
 * no findings and a script that does not parse has none either, and in
 * both cases the empty list means "the rules never ran" rather than
 * "nothing to report". Rendering the grouped list first and the reason
 * afterwards would put an empty findings area above a sentence saying
 * nothing was examined, which reads as a clean result.
 *
 * The parse error is its own row and comes FIRST, because it is not a
 * finding: it is the statement that the file is not a shell program, and
 * every rule below it is one the parser never got far enough to apply.
 */
function LintPanel({ scriptName, lint }: { scriptName: string; lint: WorkflowScriptLint }) {
  return (
    <div
      style={{
        display: "flex",
        flexDirection: "column",
        gap: 8,
        padding: "10px 12px",
        border: "1px solid var(--border)",
        borderRadius: "var(--radius-md)",
        background: "var(--surface-2, var(--surface))"
      }}
    >
      <div className="mono" style={{ fontSize: 12.5 }}>
        {scriptName}
      </div>

      {!lint.examined ? (
        <p style={{ margin: 0, fontSize: 12.5, color: "var(--text-2)", maxWidth: "76ch" }}>
          {/* An em dash and not a full stop, because the reason is the
              engine's own clause and starts lower case: "Not examined.
              this script is 4.1 MB" reads as a typo. */}
          {"Not examined \u2014 " +
            (lint.notExaminedReason ||
              "this build was not told why, so nothing is known about this script's contents") +
            ". This is not a pass: no rule was applied to these bytes."}
        </p>
      ) : !lint.parsed ? (
        <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
          <div style={{ display: "flex", gap: 10, alignItems: "baseline", flexWrap: "wrap" }}>
            <StatusBadge tone="danger" icon="failure">
              does not parse
            </StatusBadge>
            <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-2)" }}>
              {positionLabel(lint.parseErrorLine ?? 0, lint.parseErrorCol ?? 0)}
            </span>
            <span style={{ fontSize: 12.5, color: "var(--text-2)", flex: 1, minWidth: 220 }}>
              {lint.parseError || "the shell parser refused this file and named no reason."}
            </span>
          </div>
          <p style={{ margin: 0, fontSize: "var(--text-xs)", color: "var(--text-3)", maxWidth: "76ch" }}>
            {"This file is not a shell program, so nothing in it would run: the hook would fail " +
              "at the first line. A save that points a stage directory at it is refused."}
          </p>
        </div>
      ) : lint.findings.length === 0 ? (
        <p style={{ margin: 0, fontSize: 12.5, color: "var(--text-2)", maxWidth: "76ch" }}>
          {"This script parses, and backupd's own shell rules reported nothing about it."}
        </p>
      ) : (
        LINT_SEVERITY_ORDER.map((severity) => {
          // Sorted by position WITHIN the group, because a group is read
          // top to bottom against the file: line order is the order an
          // operator walks the script in, and arrival order is not.
          const group = lint.findings
            .filter((f) => f.severity === severity)
            .sort((a, b) => (a.line === b.line ? a.col - b.col : a.line - b.line));
          if (group.length === 0) return null;
          return (
            <div key={severity} style={{ display: "flex", flexDirection: "column", gap: 5 }}>
              {group.map((finding) => (
                <div
                  key={finding.code + positionLabel(finding.line, finding.col)}
                  style={{ display: "flex", gap: 10, alignItems: "baseline", flexWrap: "wrap" }}
                >
                  <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-2)" }}>
                    {finding.code}
                  </span>
                  <StatusBadge
                    tone={LINT_SEVERITY[severity].tone}
                    icon={
                      severity === "error" ? "failure" : severity === "warning" ? "warning" : "info"
                    }
                  >
                    {LINT_SEVERITY[severity].label}
                  </StatusBadge>
                  <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
                    {positionLabel(finding.line, finding.col)}
                  </span>
                  <span style={{ fontSize: 12.5, color: "var(--text-2)", flex: 1, minWidth: 220 }}>
                    {finding.message}
                  </span>
                </div>
              ))}
            </div>
          );
        })
      )}
    </div>
  );
}

function Th({ children }: { children: React.ReactNode }) {
  return (
    <th
      className="eyebrow"
      style={{
        textAlign: "left",
        fontSize: 10,
        padding: "6px 10px 6px 0",
        borderBottom: "1px solid var(--border)",
        color: "var(--text-3)"
      }}
    >
      {children}
    </th>
  );
}

function Td({ children, mono }: { children: React.ReactNode; mono?: boolean }) {
  return (
    <td
      className={mono ? "mono" : undefined}
      style={{ padding: "7px 10px 7px 0", borderBottom: "1px solid var(--border)", verticalAlign: "top" }}
    >
      {children}
    </td>
  );
}
