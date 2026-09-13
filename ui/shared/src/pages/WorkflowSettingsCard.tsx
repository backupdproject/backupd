/**
 * The deployment-wide workflow settings card (issue #814, screen 5).
 *
 * # What is editable here, and what is only reported
 *
 * The two stage directories and the default script timeout are policy, so
 * they are editable. The Host Workflow Runner's socket, its token file
 * and the account it executes as are NOT: they differ between a container
 * and a bare-metal install of the same deployment, so they are a
 * deployment-shape fact the installer writes, exactly like the SSH key,
 * the known_hosts file and the state database. A form offering to change
 * them would be offering to break the only path a local hook has.
 *
 * # There is no root-elevation control, and that is not an omission
 *
 * The execution user is REPORTED, because an operator writing a hook
 * needs to know what account it runs as and which files it can read.
 * There is no switch that raises it, because no such switch exists in the
 * product: a control here would be inventing a capability, and the one
 * thing worse than a missing feature is a UI that claims one.
 *
 * # Clearing a stage directory disables that stage
 *
 * An empty box is a real request and not an omission. It is the operator
 * saying "stop running global hooks on this side", and the answer is the
 * resolved block with the stage gone — which is why the form reads what
 * was persisted back rather than keeping what it hoped it wrote.
 *
 * # "Not examined" is not "passes"
 *
 * A local hook's syntax check runs THROUGH the runner. With the runner
 * down, the honest answer is that nothing looked, and this card says the
 * runner is not answering rather than showing a green global-scripts
 * list.
 */
import { useCallback, useEffect, useState } from "react";

import { useApi } from "@shared/api/ApiContext";
import { Banner } from "@shared/components/Banner";
import { Cell, CellGrid } from "@shared/components/Definitions";
import { ErrorState } from "@shared/components/EmptyState";
import { WarningBanner } from "@shared/components/WarningBanner";
import { WorkflowEnvironmentEditor } from "@shared/components/WorkflowEnvironmentEditor";
import { describeFailure, isNotConfigured } from "@shared/api/failure";
import { useAsync } from "@shared/hooks/useAsync";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import { bytes } from "@shared/utilities/format";
import type { WorkflowSettingsPatch } from "@shared/api/contracts";

/** The three boxes this card writes, and nothing else. Keyed so a
 *  per-box Save can name exactly what it is sending, which is the shape
 *  the backup-set editor already uses and the reason a save here cannot
 *  clobber a box the operator never touched. */
type Box = "beforeDir" | "afterDir" | "scriptTimeoutSeconds";

const BOX_LABELS: Record<Box, string> = {
  beforeDir: "Global before directory",
  afterDir: "Global after directory",
  scriptTimeoutSeconds: "Default script timeout (seconds)"
};

export function WorkflowSettingsCard({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const settings = useAsync(() => api.getWorkflowSettings(), [api]);
  const globalEnv = useAsync(() => api.listWorkflowEnvironment(), [api]);

  const [draft, setDraft] = useState<Record<Box, string> | null>(null);
  const [saving, setSaving] = useState<Box | null>(null);
  const [failure, setFailure] = useState<string | null>(null);

  // The draft is seeded from what was persisted, and re-seeded whenever a
  // read lands: a box showing a value the service does not have is the
  // defect a settings form is most likely to ship with.
  useEffect(() => {
    if (settings.data) {
      setDraft({
        beforeDir: settings.data.beforeDir,
        afterDir: settings.data.afterDir,
        scriptTimeoutSeconds: String(settings.data.scriptTimeoutSeconds)
      });
    }
  }, [settings.data]);

  const save = useCallback(
    (box: Box) => {
      if (!draft) return;
      const patch: WorkflowSettingsPatch =
        box === "scriptTimeoutSeconds"
          ? { scriptTimeoutSeconds: Number(draft.scriptTimeoutSeconds) }
          : box === "beforeDir"
            ? { beforeDir: draft.beforeDir }
            : { afterDir: draft.afterDir };
      setSaving(box);
      setFailure(null);
      api
        .patchWorkflowSettings(patch)
        .then(() => {
          setSaving(null);
          // Re-read rather than trust the echo: the response is the
          // RESOLVED block, and a cleared directory changes which stages
          // exist at all.
          settings.reload();
        })
        .catch((e: unknown) => {
          setSaving(null);
          setFailure(describeFailure(e, "that setting was not saved").message);
        });
    },
    [api, draft, settings]
  );

  const heading = (
    <InfoTooltip id="settings.workflow">
      <h2 className="eyebrow">Workflow</h2>
    </InfoTooltip>
  );

  // An instance with no configuration at all refuses this read with a
  // 503, and that is a step nobody has taken rather than a read that
  // failed (#275). A red banner with a Try again button would be wrong
  // twice: there is nothing to retry, and the service's own sentence
  // quotes an API path at an operator who has not finished setup.
  if (isNotConfigured(settings.error)) {
    return (
      <section className="card">
        <div className="card__header">{heading}</div>
        <div className="card__body">
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "76ch" }}>
            {"Workflow hooks are part of a configuration this instance has not been given yet. " +
              "They become editable once the first backup set has been added."}
          </p>
        </div>
      </section>
    );
  }

  if (settings.error) {
    return (
      <section className="card">
        <div className="card__header">{heading}</div>
        <div className="card__body">
          <ErrorState
            message={settings.error.message}
            remediation={settings.error.remediation}
            correlationId={settings.error.correlationId}
            onRetry={settings.reload}
          />
        </div>
      </section>
    );
  }

  if (settings.data === null || draft === null) {
    return (
      <section className="card">
        <div className="card__header">{heading}</div>
        <div className="card__body">
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
            Reading this deployment's workflow configuration…
          </p>
        </div>
      </section>
    );
  }

  // Bound once for the reason the run page binds its own: every read
  // below is of the same answer, and a re-read of `settings.data` is a
  // value TypeScript cannot prove is still non-null.
  const loaded = settings.data;
  const runner = loaded.runner;

  return (
    <section className="card">
      <div
        className="card__header"
        style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 12 }}
      >
        {heading}
        <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
          Global scripts wrap every backup-set run.
        </span>
      </div>
      <div className="card__body" style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        {runner.configured ? null : (
          <WarningBanner
            tone="warn"
            eyebrow="Host Workflow Runner"
            title="The Host Workflow Runner is not answering"
            tip="workflow.settings.runner"
            dismissible={false}
          >
            {"This is the component that executes a .local.sh hook on the machine Backupd is " +
              "installed on. Until it answers, every local hook fails and every check that needs " +
              "it reports as not examined rather than as passing. Its socket and token path are " +
              "written by the installer and are not editable here."}
          </WarningBanner>
        )}

        <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "78ch" }}>
          {"Global scripts wrap every backup-set run: the before directory runs ahead of each set's " +
            "own before stage, and the after directory runs after each set's own after stage, " +
            "whatever the backup did. A script named *.local.sh runs on this machine through the " +
            "Host Workflow Runner; anything else runs on the backup set's source host."}
        </p>

        <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
          {(Object.keys(BOX_LABELS) as Box[]).map((box) => (
            <SettingBox
              key={box}
              box={box}
              value={draft[box]}
              dirty={draft[box] !== persistedValue(loaded, box)}
              saving={saving === box}
              readOnly={readOnly}
              onChange={(value) => setDraft({ ...draft, [box]: value })}
              onSave={() => save(box)}
            />
          ))}
        </div>

        {failure ? (
          <Banner tone="danger" dismissKey={failure}>
            <span style={{ fontSize: "var(--text-sm)" }}>{failure}</span>
          </Banner>
        ) : null}

        {/* Wider than the page's usual tile, because every value here is
            a filesystem path: at 190px they break mid-token
            ("/run/backu" / "pd/hooks.sock"), and a path an operator has
            to compare against their own host is the one string that
            must not be split arbitrarily. */}
        <CellGrid min={280}>
          {/* The verdict and the path are two cells, not one string: a
              status joined to a socket path is long enough that the path
              wraps mid-token, and a path an operator compares against
              their own host is the one value that must not be split
              arbitrarily. */}
          <Cell
            label="Host Workflow Runner"
            tip="workflow.settings.runner-status"
            value={runner.configured ? "Answering" : "Not answering"}
          />
          <Cell
            label="Runner socket"
            tip="workflow.settings.runner-socket"
            value={runner.socket || "Not configured"}
            mono
          />
          <Cell
            label="Runner credential"
            tip="workflow.settings.runner-token"
            value={runner.tokenFile || "Not reported"}
            mono
          />
          <Cell
            label="Runner version and execution user"
            tip="workflow.settings.execution-user"
            value="Not on this API"
          />
          <Cell
            label="Maximum script size"
            tip="workflow.settings.max-size"
            value={loaded.maxScriptSizeBytes > 0 ? bytes(loaded.maxScriptSizeBytes) : "Not reported"}
            mono
          />
          <Cell
            label="Timeout source"
            tip="workflow.settings.timeout-source"
            value={loaded.scriptTimeoutConfigured ? "Configured" : "Product default"}
          />
        </CellGrid>

        <p style={{ margin: 0, fontSize: "var(--text-xs)", color: "var(--text-3)", maxWidth: "78ch" }}>
          {"The runner's socket and its token file are reported and not editable: they differ " +
            "between a container and a bare-metal install of the same deployment, so the installer " +
            "writes them. Its build version and the account it executes a hook as are not on this " +
            "API at all \u2014 "}
          <code className="mono">backupd workflow-runner status</code>
          {" reports both, on the machine the runner is installed on. There is no control here " +
            "that raises a hook's privileges, and there is not one to add: this product has no " +
            "elevation switch."}
        </p>

        <div>
          <InfoTooltip id="workflow.settings.environment" block>
            <div className="eyebrow" style={{ fontSize: 10.5, marginBottom: 8 }}>
              Global environment
            </div>
          </InfoTooltip>
          <WorkflowEnvironmentEditor
            scope="global"
            global={globalEnv.data?.variables ?? loaded.environment}
            set={null}
            readOnly={readOnly}
            onSet={(name, entry) =>
              api.setWorkflowEnvironment(name, entry).then((next) => {
                globalEnv.reload();
                return next;
              })
            }
            onUnset={(name) =>
              api.unsetWorkflowEnvironment(name).then((next) => {
                globalEnv.reload();
                return next;
              })
            }
          />
        </div>
      </div>
    </section>
  );
}

/** What the service currently holds for one box, so "dirty" is a
 *  comparison against the persisted value and not against a snapshot
 *  taken when the card mounted. */
function persistedValue(
  settings: { beforeDir: string; afterDir: string; scriptTimeoutSeconds: number },
  box: Box
): string {
  if (box === "beforeDir") return settings.beforeDir;
  if (box === "afterDir") return settings.afterDir;
  return String(settings.scriptTimeoutSeconds);
}

/** One box with its own Save, which is the shape the backup-set editor
 *  established: a per-box save writes exactly the field it names, so a
 *  save cannot carry a value an operator never touched. */
function SettingBox({
  box,
  value,
  dirty,
  saving,
  readOnly,
  onChange,
  onSave
}: {
  box: Box;
  value: string;
  dirty: boolean;
  saving: boolean;
  readOnly: boolean;
  onChange(value: string): void;
  onSave(): void;
}) {
  const id = "workflow-setting-" + box;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 5 }}>
      <InfoTooltip id={TIPS[box]} block>
        <label htmlFor={id} className="eyebrow" style={{ fontSize: 10.5 }}>
          {BOX_LABELS[box]}
        </label>
      </InfoTooltip>
      <div style={{ display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap" }}>
        <input
          id={id}
          className="mono"
          value={value}
          inputMode={box === "scriptTimeoutSeconds" ? "numeric" : undefined}
          onChange={(e) => onChange(e.target.value)}
          placeholder={box === "scriptTimeoutSeconds" ? "300" : "/etc/backupd/workflows/before"}
          style={{
            font: "inherit",
            fontSize: 13,
            height: 30,
            padding: "0 9px",
            border: "1px solid var(--border-strong)",
            borderRadius: "var(--radius-md)",
            background: "var(--surface)",
            color: "var(--text)",
            flex: 1,
            minWidth: 240
          }}
        />
        <button
          className="btn btn--sm"
          disabled={readOnly || saving || !dirty}
          onClick={onSave}
        >
          {saving ? "Saving\u2026" : "Save"}
        </button>
      </div>
      {box === "scriptTimeoutSeconds" ? null : (
        <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
          {"Leaving this empty disables the stage: no global " +
            (box === "beforeDir" ? "before" : "after") +
            " hooks run for any backup set."}
        </span>
      )}
    </div>
  );
}

const TIPS: Record<Box, "workflow.settings.before-dir" | "workflow.settings.after-dir" | "workflow.settings.timeout"> = {
  beforeDir: "workflow.settings.before-dir",
  afterDir: "workflow.settings.after-dir",
  scriptTimeoutSeconds: "workflow.settings.timeout"
};
