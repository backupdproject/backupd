/**
 * The workflow environment editor and the merged inheritance preview
 * (issue #814, screen 4).
 *
 * # The rule this whole component is built around
 *
 * A secret is a LOCATION and never a value. Every read on this API
 * carries a file path, a variable NAME or an argv, and there is no field
 * anywhere on the contract a resolved secret could arrive in — the engine
 * resolves a reference at the moment a hook is about to run and nothing
 * carries the result back. So this editor has no reveal control, no
 * "show value", and no cell a secret's value could be rendered into, and
 * that is a property of the types rather than a discipline: there is
 * nothing to render.
 *
 * # Why two tables
 *
 * Because the question is precedence, and one table cannot answer it. The
 * first table is what is CONFIGURED here, and it is editable. The second
 * is what a hook will actually RECEIVE, with the scope that won and what
 * it shadowed. An operator who cannot see that a backup-set `PGHOST` is
 * hiding the deployment's has no way to explain a hook connecting to the
 * wrong database, and that is exactly the support question this screen
 * exists to make answerable.
 *
 * # Why the built-ins are in the preview and not in the editor
 *
 * They are not configuration. `BACKUPD_*` is reserved: the service
 * refuses a write to any name in that namespace, because a hook reading
 * `BACKUPD_BACKUP_STATUS` has to be reading what this product observed
 * rather than a value somebody wrote into a config file. They appear in
 * the preview, read-only, because they ARE part of what a hook receives
 * and a hook author writes against them.
 *
 * # An empty literal is not a missing one
 *
 * `value: ""` is a deliberately empty variable, which operators write.
 * No literal at all is a variable whose value comes from a reference. The
 * two are drawn differently, and the write direction keeps them apart:
 * the form sends a literal when the literal field was used, including an
 * empty one, and sends a reference only when a location was given.
 */
import { useCallback, useMemo, useState } from "react";

import { StatusBadge } from "@shared/components/StatusBadge";
import { describeFailure } from "@shared/api/failure";
import {
  ENV_SOURCE_LABELS,
  isReservedEnvName,
  mergeWorkflowEnvironment
} from "@shared/components/workflowPresentation";
import type { EnvSource, MergedEnvVariable } from "@shared/components/workflowPresentation";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import type { WorkflowEnvVariable, WorkflowEnvVariableInput } from "@shared/api/contracts";

/** Which scope this editor writes to. The preview always shows both, so
 *  the deployment-wide editor and one set's differ only in where a write
 *  lands and in whether the set column has anything in it. */
export type EditorScope = "global" | "set";

/** Where a value comes from, as the form collects it. Three, because the
 *  API's own secret reference is exactly one of file, env or command, and
 *  a form that let an operator fill two would be collecting a request the
 *  service refuses as a contradiction. */
type ValueKind = "literal" | "file" | "env" | "command";

const KIND_LABELS: Record<ValueKind, string> = {
  literal: "A value typed here",
  file: "Read from a file on the host",
  env: "Read from the service's own environment",
  command: "Output of a command"
};

export function WorkflowEnvironmentEditor({
  scope,
  global,
  set,
  readOnly,
  onSet,
  onUnset
}: {
  scope: EditorScope;
  /** The deployment-wide layer. Always supplied, even in the per-set
   *  editor, because the preview cannot answer "what will a hook see"
   *  without it. */
  global: WorkflowEnvVariable[];
  /** This backup set's own layer, or null in the deployment-wide
   *  editor — which is a different thing from an empty list: null means
   *  "no set is in scope here", and an empty array means "this set
   *  configures nothing of its own". */
  set: WorkflowEnvVariable[] | null;
  readOnly: boolean;
  onSet(name: string, entry: WorkflowEnvVariableInput): Promise<unknown>;
  onUnset(name: string): Promise<unknown>;
}) {
  const [draftName, setDraftName] = useState("");
  const [kind, setKind] = useState<ValueKind>("literal");
  const [draftValue, setDraftValue] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const [failure, setFailure] = useState<string | null>(null);

  const merged = useMemo(() => mergeWorkflowEnvironment(global, set ?? []), [global, set]);

  const nameRefused = draftName !== "" && isReservedEnvName(draftName);

  const save = useCallback(() => {
    const name = draftName.trim();
    if (name === "") return;
    const entry: WorkflowEnvVariableInput =
      kind === "literal"
        ? // The literal travels exactly as typed, empty string included.
          { value: draftValue }
        : kind === "command"
          ? // An argv and not a shell line: the service executes the
            // vector, so splitting on whitespace here is what makes it a
            // vector rather than a string somebody's shell would have to
            // re-parse.
            { secret: { command: draftValue.split(/\s+/).filter((part) => part !== "") } }
          : kind === "file"
            ? { secret: { file: draftValue } }
            : { secret: { env: draftValue } };
    setBusy(name);
    setFailure(null);
    onSet(name, entry)
      .then(() => {
        setBusy(null);
        setDraftName("");
        setDraftValue("");
      })
      .catch((e: unknown) => {
        setBusy(null);
        setFailure(describeFailure(e, "that variable was not saved").message);
      });
  }, [draftName, draftValue, kind, onSet]);

  const remove = useCallback(
    (name: string) => {
      setBusy(name);
      setFailure(null);
      onUnset(name)
        .then(() => setBusy(null))
        .catch((e: unknown) => {
          setBusy(null);
          setFailure(describeFailure(e, "that variable was not removed").message);
        });
    },
    [onUnset]
  );

  const editable = scope === "global" ? global : (set ?? []);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
      <div>
        <InfoTooltip id="workflow.env.configured" block>
          <div className="eyebrow" style={{ fontSize: 10.5, marginBottom: 7 }}>
            {scope === "global" ? "Deployment-wide variables" : "This backup set's variables"}
          </div>
        </InfoTooltip>
        {editable.length === 0 ? (
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
            {scope === "global"
              ? "No deployment-wide workflow variables are configured. Hooks still receive the BACKUPD_* built-ins below."
              : "This backup set configures no variables of its own, so its hooks receive the deployment-wide ones below."}
          </p>
        ) : (
          <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
            <thead>
              <tr>
                <Th>Variable</Th>
                <Th>Value</Th>
                <Th>Scope</Th>
                {/* Named rather than blank: a column of controls whose
                    header says nothing is a column a screen reader
                    announces as an empty cell. */}
                <Th>Action</Th>
              </tr>
            </thead>
            <tbody>
              {editable.map((entry) => (
                <tr key={entry.name}>
                  <Td mono>{entry.name}</Td>
                  <Td>
                    <ValueCell entry={entry} />
                  </Td>
                  <Td>
                    <StatusBadge tone={scope === "set" ? "accent" : "neutral"} icon="info">
                      {ENV_SOURCE_LABELS[scope]}
                    </StatusBadge>
                  </Td>
                  <Td>
                    <InfoTooltip id="workflow.env.unset" alignEnd>
                      <button
                        className="btn btn--sm"
                        disabled={readOnly || busy !== null}
                        onClick={() => remove(entry.name)}
                      >
                        {busy === entry.name ? "Working\u2026" : "Unset"}
                      </button>
                    </InfoTooltip>
                  </Td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      <div
        style={{
          border: "1px solid var(--border)",
          borderRadius: "var(--radius-lg)",
          padding: "12px 13px",
          display: "flex",
          flexDirection: "column",
          gap: 9,
          background: "var(--surface-2)"
        }}
      >
        <InfoTooltip id="workflow.env.add" block>
          <div className="eyebrow" style={{ fontSize: 10.5 }}>
            Set a variable
          </div>
        </InfoTooltip>
        <div style={{ display: "flex", gap: 9, flexWrap: "wrap", alignItems: "flex-end" }}>
          <Field label="Name" htmlFor="workflow-env-name">
            <input
              id="workflow-env-name"
              className="mono"
              value={draftName}
              onChange={(e) => setDraftName(e.target.value.toUpperCase())}
              placeholder="PGHOST"
              style={INPUT_STYLE}
            />
          </Field>
          <Field label="Where the value comes from" htmlFor="workflow-env-kind">
            <select
              id="workflow-env-kind"
              value={kind}
              onChange={(e) => setKind(e.target.value as ValueKind)}
              style={INPUT_STYLE}
            >
              {(Object.keys(KIND_LABELS) as ValueKind[]).map((option) => (
                <option key={option} value={option}>
                  {KIND_LABELS[option]}
                </option>
              ))}
            </select>
          </Field>
          <Field
            label={kind === "literal" ? "Value" : kind === "command" ? "Command" : "Location"}
            htmlFor="workflow-env-value"
          >
            <input
              id="workflow-env-value"
              className={kind === "literal" ? undefined : "mono"}
              value={draftValue}
              onChange={(e) => setDraftValue(e.target.value)}
              placeholder={
                kind === "literal"
                  ? "db-primary.internal"
                  : kind === "file"
                    ? "/etc/backupd/secrets/pg"
                    : kind === "env"
                      ? "PGPASSWORD_SOURCE"
                      : "/usr/local/bin/read-secret pg"
              }
              style={INPUT_STYLE}
            />
          </Field>
          <InfoTooltip id="workflow.env.save">
            <button
              className="btn btn--primary btn--sm"
              disabled={readOnly || busy !== null || draftName.trim() === "" || nameRefused}
              onClick={save}
            >
              Save variable
            </button>
          </InfoTooltip>
        </div>
        {nameRefused ? (
          <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--danger)" }}>
            {"Every BACKUPD_* name is set by this product from the run it belongs to, so it cannot " +
              "be configured here. Choose a name outside that namespace."}
          </p>
        ) : null}
        {kind === "literal" ? (
          <p style={{ margin: 0, fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
            {"An empty value is saved as an empty variable, which is different from a variable " +
              "whose value comes from a secret reference."}
          </p>
        ) : (
          <p style={{ margin: 0, fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
            {"This records WHERE the value comes from. The value itself is read when a hook is " +
              "about to run and is never stored, never logged and never shown back here."}
          </p>
        )}
        {failure ? (
          <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--danger)" }}>{failure}</p>
        ) : null}
      </div>

      <div>
        <InfoTooltip id="workflow.env.preview" block>
          <div className="eyebrow" style={{ fontSize: 10.5, marginBottom: 4 }}>
            What a hook will see
          </div>
        </InfoTooltip>
        <p style={{ margin: "0 0 7px", fontSize: "var(--text-xs)", color: "var(--text-3)" }} className="mono">
          sanitized baseline &lt; deployment &lt; backup set &lt; BACKUPD_* built-ins
        </p>
        <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
          <thead>
            <tr>
              <Th>Variable</Th>
              <Th>Effective value</Th>
              <Th>Comes from</Th>
              <Th>Overrides</Th>
            </tr>
          </thead>
          <tbody>
            {merged.map((row) => (
              <PreviewRow key={row.name} row={row} />
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

const INPUT_STYLE = {
  font: "inherit",
  fontSize: 13,
  height: 30,
  padding: "0 9px",
  border: "1px solid var(--border-strong)",
  borderRadius: "var(--radius-md)",
  background: "var(--surface)",
  color: "var(--text)",
  minWidth: 190
} as const;

/** The cell a value is drawn into, and the one place the secret rule is
 *  enforced visually: a secret-backed entry renders its LOCATION, in the
 *  colour of quiet text, and there is no branch that could print a
 *  value because no value ever arrives. */
function ValueCell({ entry }: { entry: WorkflowEnvVariable }) {
  if (entry.secret) {
    const location = entry.secret.file
      ? "file " + entry.secret.file
      : entry.secret.env
        ? "the service's own " + entry.secret.env
        : (entry.secret.command ?? []).join(" ");
    return (
      <span className="mono" style={{ color: "var(--text-3)" }}>
        {"from " + location + " \u00b7 never shown"}
      </span>
    );
  }
  if (entry.value === "") {
    return <em style={{ color: "var(--text-3)" }}>(empty)</em>;
  }
  return <span className="mono">{entry.value ?? ""}</span>;
}

function PreviewRow({ row }: { row: MergedEnvVariable }) {
  const builtin = row.source === "builtin";
  return (
    <tr>
      <Td mono>{row.name}</Td>
      <Td>
        {builtin ? (
          <span className="mono" style={{ color: "var(--text-3)" }}>
            set per run
          </span>
        ) : row.entry ? (
          // The same cell the editor uses, deliberately: a secret-backed
          // entry renders its LOCATION here too, because "which file
          // does this hook's password come from" is exactly the question
          // an inheritance preview is read for.
          <ValueCell entry={row.entry} />
        ) : null}
      </Td>
      <Td>
        <StatusBadge
          tone={row.source === "set" ? "accent" : row.source === "builtin" ? "neutral" : "neutral"}
          icon="info"
        >
          {ENV_SOURCE_LABELS[row.source]}
        </StatusBadge>
      </Td>
      <Td>
        {builtin ? (
          <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
            {row.shadowed.length > 0
              ? "hides a " + sourceList(row.shadowed) + " entry that cannot take effect"
              : "cannot be overridden"}
          </span>
        ) : row.shadowed.length > 0 ? (
          <span style={{ fontSize: "var(--text-xs)", color: "var(--warn)" }}>
            {"shadows the " + sourceList(row.shadowed) + " value"}
          </span>
        ) : (
          <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>{"\u2014"}</span>
        )}
      </Td>
    </tr>
  );
}

function sourceList(sources: EnvSource[]): string {
  return sources.map((source) => ENV_SOURCE_LABELS[source].toLowerCase()).join(" and ");
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

function Field({
  label,
  htmlFor,
  children
}: {
  label: string;
  htmlFor: string;
  children: React.ReactNode;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
      <label htmlFor={htmlFor} className="eyebrow" style={{ fontSize: 10 }}>
        {label}
      </label>
      {children}
    </div>
  );
}
