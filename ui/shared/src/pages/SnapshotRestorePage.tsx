/**
 * Restoring from a snapshot (EPIC K, issue #788).
 *
 * # Four questions, in this order
 *
 * Which snapshot, what inside it, where it goes, and what happens to a
 * file already sitting there. The order is the argument: the conflict
 * answer is asked beside the destination, because it is a question ABOUT
 * that directory, and it is asked before the confirmation rather than
 * inside it, because a choice that can overwrite is not a checkbox on a
 * summary.
 *
 * # Refuse is the default and stays the default
 *
 * Three answers, not a checkbox, and the one that cannot lose data is
 * selected. "Skip" is the one that makes an interrupted restore
 * resumable: what already landed stays, what is missing is written, and
 * nobody has to choose between starting again and overwriting. Each of
 * the two non-default answers explains itself on screen at the moment it
 * is chosen, not in a help page.
 *
 * # A restore never writes to the source
 *
 * The destination is a directory this deployment can reach. Restoring
 * onto the original server is a copy an operator makes from there, which
 * is what keeps a restore from ever being the thing that damages the
 * source.
 *
 * # It is one durable operation
 *
 * The commit is a single `POST /operations` with an idempotency key, and
 * what comes back is watched by id. The browser can be closed; the
 * restore keeps going, and this page finds it again by asking for the
 * operation.
 */
import { useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";

import { useApi } from "@shared/api/ApiContext";
import { Cell, CellGrid, Note } from "@shared/components/Definitions";
import { Choice, Toggle } from "@shared/components/Choice";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { VERIFICATION_COPY } from "@shared/components/EngineBadge";
import { OperationProgress } from "@shared/components/OperationProgress";
import { PageHeader } from "@shared/components/PageHeader";
import { StepBody, StepControls, StepRail } from "@shared/components/WizardStep";
import { WarningBanner } from "@shared/components/WarningBanner";
import { useAsync } from "@shared/hooks/useAsync";
import { useSnapshotOperation } from "@shared/hooks/useSnapshotOperation";
import type { RestoreConflictPolicy } from "@shared/types/snapshot";
import { bytes, measured, stamp } from "@shared/utilities/format";
import { snapshotsPath } from "@shared/utilities/routes";

const STEPS = ["Snapshot", "What to restore", "Where", "Confirm"] as const;

/** The three answers, in the order they are offered: safest first, and
 *  the destructive one last so it is never the one a keyboard lands on by
 *  default. */
const CONFLICT_OPTIONS: { value: RestoreConflictPolicy; label: string }[] = [
  { value: "refuse", label: "Refuse and stop (default)" },
  { value: "skip", label: "Skip it and carry on" },
  { value: "overwrite", label: "Overwrite it" }
];

export function SnapshotRestorePage({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const navigate = useNavigate();
  const params = useParams();
  const [query] = useSearchParams();
  const source = params.source ?? "";
  const set = params.set ?? "";
  const setId = source + "/" + set;

  const snapshots = useAsync(() => api.listSnapshots(source, set), [api, source, set]);
  const restore = useSnapshotOperation("Could not start the restore.");

  const [step, setStep] = useState(1);
  const [chosenRun, setChosenRun] = useState<string | null>(query.get("run"));
  const [showFailed, setShowFailed] = useState(false);
  const [wholeTree, setWholeTree] = useState(true);
  const [sourcePath, setSourcePath] = useState("");
  const [targetPath, setTargetPath] = useState("");
  const [conflict, setConflict] = useState<RestoreConflictPolicy>("refuse");

  // A run with no committed manifest has no id the repository can be
  // asked for, so it is not a restore source at all and is not offered.
  const restorable = (snapshots.data ?? []).filter((s) => s.snapshotId !== null);
  const offered = restorable.filter((s) => showFailed || s.verificationStatus === "passed");
  const selected =
    restorable.find((s) => s.runId === chosenRun) ??
    offered.find((s) => s.lastKnownGood) ??
    offered[0] ??
    null;

  const header = (
    <PageHeader
      back={{ label: "Cancel restore", onClick: () => navigate(snapshotsPath(source, set)) }}
      title="Restore from a snapshot"
      subtitle={setId + " \u00b7 a restore refuses to overwrite unless you choose otherwise"}
    />
  );

  if (snapshots.error)
    return (
      <>
        {header}
        <ErrorState
          message={snapshots.error.message}
          correlationId={snapshots.error.correlationId}
          onRetry={snapshots.reload}
        />
      </>
    );

  if (snapshots.data !== null && restorable.length === 0)
    return (
      <>
        {header}
        <EmptyState title="There is nothing to restore from">
          {setId + " holds no snapshot with a committed manifest, so there is no restore point " +
            "to name. A run appears here once it has stored a manifest in the repository."}
        </EmptyState>
      </>
    );

  const missingTarget = targetPath.trim() === "";
  const finishHint = !restore.ready
    ? "Waiting for this instance's configuration revision, which a restore is checked against."
    : restore.operation !== null
      ? "This restore has been submitted. It is durable: closing the browser does not stop it."
      : missingTarget
        ? "Name a directory on this deployment to restore into."
        : selected === null
          ? "Choose a snapshot to restore from."
          : undefined;

  return (
    <div style={{ maxWidth: 980, width: "100%", margin: "0 auto", display: "flex", flexDirection: "column", gap: 16 }}>
      {header}

      <StepRail steps={STEPS} step={step} onSelect={setStep} tip="snapshots.restore.step" />

      {restore.failure ? (
        <ErrorState
          message={restore.failure.message}
          remediation={restore.failure.remediation}
          correlationId={restore.failure.correlationId}
          detail={restore.failure.detail}
        />
      ) : null}

      <section className="card">
        <div style={{ padding: "20px 22px 22px" }}>
          {step === 1 ? (
            <StepBody
              title="Which snapshot"
              lede="Only snapshots that passed their verification are offered by default."
            >
              <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
                {offered.map((snapshot) => (
                  <Choice
                    key={snapshot.runId}
                    name="restore-snapshot"
                    title={
                      stamp(snapshot.startedAt) +
                      (snapshot.lastKnownGood ? " \u2014 newest known-good" : "")
                    }
                    wire={"snapshot_id=" + snapshot.snapshotId}
                    detail={
                      measured(snapshot.entriesScanned, (n) => n.toLocaleString()) +
                      " entries \u00b7 " +
                      measured(snapshot.logicalBytes, bytes) +
                      " \u00b7 " +
                      (snapshot.verificationLevelAchieved
                        ? "verified " + VERIFICATION_COPY[snapshot.verificationLevelAchieved].name
                        : snapshot.verificationStatus === "failed"
                          ? "failed verification \u2014 this run proved nothing"
                          : "not verified yet")
                    }
                    checked={selected?.runId === snapshot.runId}
                    onChange={() => setChosenRun(snapshot.runId)}
                  />
                ))}
              </div>
              <div style={{ marginTop: 14 }}>
                <Toggle
                  label="Also show snapshots that failed verification"
                  note={showFailed ? "shown" : "off"}
                  checked={showFailed}
                  onChange={() => setShowFailed(!showFailed)}
                />
              </div>
              {selected !== null && selected.verificationStatus !== "passed" ? (
                <div style={{ marginTop: 14 }}>
                  <WarningBanner
                    tone="warn"
                    eyebrow="Unverified"
                    title="This snapshot is not a proven restore point"
                    dismissible={false}
                  >
                    {"Nothing has proved that what is stored can be read back. Restoring from it " +
                      "is allowed and may be exactly what you want; it is not a restore this " +
                      "product can vouch for."}
                  </WarningBanner>
                </div>
              ) : null}
            </StepBody>
          ) : null}

          {step === 2 ? (
            <StepBody
              title="What to restore"
              lede="The whole snapshot, or one path inside it."
            >
              <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
                <Choice
                  name="restore-scope"
                  title="The whole snapshot"
                  detail="Every path this run captured, as the tree it captured them in."
                  checked={wholeTree}
                  onChange={() => setWholeTree(true)}
                />
                <Choice
                  name="restore-scope"
                  title="One path inside it"
                  wire="source_path"
                  detail="A directory or a file, written exactly as it appears inside the snapshot."
                  checked={!wholeTree}
                  onChange={() => setWholeTree(false)}
                />
              </div>
              {wholeTree ? null : (
                <label className="field" style={{ marginTop: 14, maxWidth: 460 }}>
                  <span className="field__label">Path inside the snapshot</span>
                  <input
                    className="input input--mono"
                    value={sourcePath}
                    onChange={(e) => setSourcePath(e.target.value)}
                  />
                </label>
              )}
              <div style={{ marginTop: 14 }}>
                <Note>
                  {"A path is resolved inside the snapshot, not on the source: the snapshot is " +
                    "what is being read, and the source may have changed since it was taken."}
                </Note>
              </div>
            </StepBody>
          ) : null}

          {step === 3 ? (
            <StepBody
              title="Where it goes"
              lede="A directory this deployment can reach. Restoring onto the original server is a copy you make from there, so a restore can never be the thing that damages the source."
            >
              <div style={{ display: "flex", flexDirection: "column", gap: 14, maxWidth: 520 }}>
                <label className="field">
                  <span className="field__label">Restore into</span>
                  <input
                    className="input input--mono"
                    value={targetPath}
                    onChange={(e) => setTargetPath(e.target.value)}
                  />
                </label>
                <label className="field">
                  <span className="field__label">If a file is already there</span>
                  <select
                    className="select"
                    value={conflict}
                    onChange={(e) => setConflict(e.target.value as RestoreConflictPolicy)}
                  >
                    {CONFLICT_OPTIONS.map((option) => (
                      <option key={option.value} value={option.value}>{option.label}</option>
                    ))}
                  </select>
                </label>
              </div>
              {conflict === "overwrite" ? (
                <div style={{ marginTop: 14 }}>
                  <WarningBanner
                    tone="warn"
                    eyebrow="Overwriting"
                    title="Files already in that directory will be replaced"
                    dismissible={false}
                  >
                    {"Nothing outside the paths this snapshot names is touched, but a file of " +
                      "the same name inside them is overwritten without a second prompt."}
                  </WarningBanner>
                </div>
              ) : null}
              {conflict === "skip" ? (
                <div style={{ marginTop: 14 }}>
                  <WarningBanner
                    tone="info"
                    eyebrow="Skipping"
                    title="A file already there is left exactly as it is"
                    dismissible={false}
                  >
                    {"This is the setting for finishing a restore that was interrupted: what " +
                      "already landed stays, what is missing is written, and nothing has to be " +
                      "chosen between starting again and overwriting."}
                  </WarningBanner>
                </div>
              ) : null}
            </StepBody>
          ) : null}

          {step === 4 ? (
            <StepBody
              title="Confirm"
              lede="This submits one durable operation. You can close the browser; it keeps going."
            >
              <CellGrid min={220}>
                <Cell
                  label="Snapshot"
                  value={selected?.snapshotId ?? "none chosen"}
                  wire="snapshot_id"
                  mono
                />
                <Cell label="Taken" value={selected ? stamp(selected.startedAt) : "\u2014"} />
                <Cell
                  label="Paths"
                  value={wholeTree ? "the whole snapshot" : sourcePath || "not named"}
                  wire="source_path"
                  mono
                />
                <Cell
                  label="Logical size"
                  value={selected ? measured(selected.logicalBytes, bytes) : "\u2014"}
                />
                <Cell
                  label="Destination"
                  value={targetPath || "not named"}
                  wire="target_path"
                  mono
                />
                <Cell
                  label="If a file exists"
                  value={
                    conflict === "overwrite"
                      ? "Overwrite it"
                      : conflict === "skip"
                        ? "Skip it and carry on"
                        : "Refuse and stop"
                  }
                  wire={"conflict=" + conflict}
                />
              </CellGrid>

              {restore.operation ? (
                <div style={{ marginTop: 16 }}>
                  <div
                    style={{
                      border: "1px solid var(--border)",
                      borderRadius: "var(--radius-lg)",
                      background: "var(--surface-2)",
                      padding: 14
                    }}
                  >
                    <OperationProgress operation={restore.operation} />
                  </div>
                </div>
              ) : (
                <div style={{ marginTop: 16 }}>
                  <Note>
                    {"Nothing has been submitted yet. The restore starts when you press " +
                      "Start restore, and this panel then shows the operation carrying it out."}
                  </Note>
                </div>
              )}
            </StepBody>
          ) : null}
        </div>

        <StepControls
          step={step}
          total={STEPS.length}
          onBack={() => setStep(Math.max(1, step - 1))}
          onNext={() => setStep(Math.min(STEPS.length, step + 1))}
          finishLabel={restore.busy ? "Starting\u2026" : "Start restore"}
          finishHint={finishHint}
          finishDisabled={
            readOnly ||
            restore.busy ||
            !restore.ready ||
            restore.operation !== null ||
            selected === null ||
            missingTarget
          }
          onFinish={() => {
            if (selected === null || selected.snapshotId === null) return;
            // Target: the snapshot AND where it is being written. A
            // second press with the same answers is a retry of one
            // submission; an operator who changed the destination after
            // a refusal is asking for a different restore, and re-using
            // the key would have the service replay the first.
            restore.submit(
              selected.snapshotId + " -> " + targetPath.trim(),
              ({ configRevision, idempotencyKey }) =>
              api.restoreSnapshot({
                backupSetId: setId,
                snapshotId: selected.snapshotId ?? undefined,
                // Omitted rather than sent empty: an absent source_path
                // asks for the whole tree, and "" would be a request to
                // restore a path with no name.
                ...(wholeTree || sourcePath.trim() === "" ? {} : { sourcePath: sourcePath.trim() }),
                targetPath: targetPath.trim(),
                conflict,
                configRevision,
                idempotencyKey
              })
            );
          }}
        />
      </section>
    </div>
  );
}
