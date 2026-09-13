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
import { describeFailure } from "@shared/api/failure";
import { useAsync } from "@shared/hooks/useAsync";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import { bytes, stamp } from "@shared/utilities/format";
import { workflowRunPath } from "@shared/utilities/routes";
import type { WorkflowFinding, WorkflowValidation } from "@shared/api/contracts";

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
          chosen={workflow.data.remoteExecConnectionRef}
          available={settings.data?.execConnections ?? []}
          readOnly={readOnly}
          onChoose={(ref) =>
            api
              .patchBackupSetWorkflow(source, set, { remoteExecConnectionRef: ref })
              .then(() => workflow.reload())
          }
        />

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
            {"Checking reads every script, hashes it and parses it with bash -n. It executes no hook, " +
              "and it is read on demand rather than on a timer because it opens a connection to the " +
              "source host and to the Host Workflow Runner."}
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
          <WorkflowEnvironmentEditor
            scope="set"
            global={globalEnv.data?.variables ?? []}
            set={setEnv.data?.variables ?? []}
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
                    {"backup " +
                      STATUS_PRESENTATION[run.backupStatus].label.toLowerCase() +
                      " \u00b7 workflow " +
                      STATUS_PRESENTATION[run.workflowStatus].label.toLowerCase()}
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
  chosen,
  available,
  readOnly,
  onChoose
}: {
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
              setFailure(describeFailure(err, "that connection was not saved").message);
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
        {available.map((ref) => (
          <option key={ref} value={ref}>
            {ref}
          </option>
        ))}
      </select>
      <p style={{ margin: 0, fontSize: "var(--text-xs)", color: "var(--text-3)", maxWidth: "76ch" }}>
        {"A backup set's own source connection is used only when it can execute a command. An " +
          "SFTP-only source can move bytes and cannot run a hook, which is a supported setup and " +
          "not a fault: name another connection here, and the check below proves whether it works."}
      </p>
      {failure ? (
        <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--danger)" }}>{failure}</p>
      ) : null}
    </div>
  );
}

/** The report, as read. Two verdicts, kept apart, and the findings in the
 *  order the engine reported them. */
function ValidationReport({ report, readAt }: { report: WorkflowValidation; readAt: string | null }) {
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
            </tr>
          </thead>
          <tbody>
            {report.scripts.map((script) => (
              <tr key={script.stepId + script.scriptName}>
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
              </tr>
            ))}
          </tbody>
        </table>
      )}

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
