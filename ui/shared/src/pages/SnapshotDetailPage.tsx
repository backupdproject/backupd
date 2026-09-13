/**
 * One snapshot, in full (EPIC K, issue #788).
 *
 * # The four byte counts stay four
 *
 * Logical size, read from source, written to repository and reused are
 * four different measurements of one run and none of them is derivable
 * from the others. They are four tiles here for the same reason they are
 * four columns on the list: one "backed up" figure would describe a
 * deduplicating repository as growing by the size of the source every
 * night.
 *
 * # Asked for, and proved
 *
 * ADR 0014's rule, on the screen it is about. The level a run was ASKED
 * for is configuration; the level it PROVED is the answer, and the two
 * are shown side by side because they differ in the ordinary case — a
 * sampled check that was interrupted proves the structure and no more. A
 * FAILED verification proves nothing at all and carries no achieved level
 * whatsoever, which this page states outright rather than leaving as an
 * empty cell somebody reads as the level of the run before it.
 *
 * # The transition log is not the run record
 *
 * A run row is overwritten by every advance, so it can say what a run IS
 * and never how it got there. The timeline below is what tells a run
 * verified twice from one verified once, which is the difference between
 * a check that was interrupted and restarted and a check that never
 * happened.
 */
import { useState } from "react";
import type { ReactNode } from "react";
import { useNavigate, useParams } from "react-router-dom";

import { useApi } from "@shared/api/ApiContext";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
import { Note, Row, Rows } from "@shared/components/Definitions";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { CONSISTENCY_COPY, VERIFICATION_COPY, VerificationBadge } from "@shared/components/EngineBadge";
import { MetricCard } from "@shared/components/MetricCard";
import { OperationProgress } from "@shared/components/OperationProgress";
import { PageHeader } from "@shared/components/PageHeader";
import { StatusBadge } from "@shared/components/StatusBadge";
import { WarningBanner } from "@shared/components/WarningBanner";
import { Icon } from "@shared/design-system/icons";
import { useAsync } from "@shared/hooks/useAsync";
import { useSnapshotOperation } from "@shared/hooks/useSnapshotOperation";
import { useCausl } from "@shared/state/graph";
import { setsNode } from "@shared/state/appNodes";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import type { TooltipId } from "@shared/tooltips/tooltips";
import type { SnapshotHold, SnapshotTransition } from "@shared/types/snapshot";
import { bytes, duration, measured, stamp } from "@shared/utilities/format";
import { restorePath, snapshotsPath } from "@shared/utilities/routes";
import { phasePresentation, sourceCompleteSentence } from "@shared/pages/snapshotPresentation";

export function SnapshotDetailPage({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const navigate = useNavigate();
  const params = useParams();
  const source = params.source ?? "";
  const set = params.set ?? "";
  const runId = params.runId ?? "";
  const setId = source + "/" + set;

  const sets = useCausl(setsNode);
  const configured = (sets.data ?? []).find((s) => s.id === setId) ?? null;

  const detail = useAsync(() => api.getSnapshot(source, set, runId), [api, source, set, runId]);
  const verify = useSnapshotOperation("Could not start the verification.");
  const hold = useSnapshotOperation("Could not place the hold.");
  const release = useSnapshotOperation("Could not release the hold.");

  const [holdReason, setHoldReason] = useState("");
  const [holdOpen, setHoldOpen] = useState(false);
  const [releasing, setReleasing] = useState<SnapshotHold | null>(null);

  if (detail.error)
    return (
      <>
        <PageHeader
          back={{ label: "Back to snapshots", onClick: () => navigate(snapshotsPath(source, set)) }}
          title="Snapshot"
          subtitle={setId}
        />
        <ErrorState
          message={detail.error.message}
          correlationId={detail.error.correlationId}
          onRetry={detail.reload}
        />
      </>
    );

  if (detail.data === null)
    return (
      <>
        <PageHeader
          back={{ label: "Back to snapshots", onClick: () => navigate(snapshotsPath(source, set)) }}
          title="Snapshot"
          subtitle={setId}
        />
        <EmptyState title="Loading this snapshot">
          {"Reading run " + runId + " from " + setId + "."}
        </EmptyState>
      </>
    );

  const snapshot = detail.data.snapshot;
  const phase = phasePresentation(snapshot.phase);
  const failed = snapshot.verificationStatus === "failed";
  const askedFor = snapshot.verificationLevel ?? configured?.incremental?.verificationLevel ?? null;

  return (
    <>
      <PageHeader
        back={{ label: "Back to snapshots", onClick: () => navigate(snapshotsPath(source, set)) }}
        title={
          <span className="mono" style={{ fontSize: 22 }}>
            {snapshot.snapshotId ?? snapshot.runId}
          </span>
        }
        subtitle={
          setId +
          " \u00b7 " +
          stamp(snapshot.startedAt) +
          " \u00b7 " +
          measured(snapshot.durationSeconds, duration)
        }
        actions={
          <>
            <button
              className="btn"
              disabled={readOnly || verify.busy || !verify.ready}
              onClick={() =>
                verify.submit(
                  snapshot.runId,
                  ({ configRevision, idempotencyKey }) =>
                    api.verifySnapshot({
                      backupSetId: setId,
                      runId: snapshot.runId,
                      configRevision,
                      idempotencyKey
                    }),
                  () => detail.reload()
                )
              }
            >
              {verify.busy ? "Starting\u2026" : "Verify now"}
            </button>
            <button
              className="btn"
              disabled={readOnly || hold.busy || !hold.ready}
              onClick={() => {
                setHoldReason("");
                setHoldOpen(true);
              }}
            >
              {"Place hold\u2026"}
            </button>
            {/* Withheld, not disabled, for a run with no committed
                manifest: there is no id to ask the repository for, so
                there is nothing this button could send. */}
            {snapshot.snapshotId ? (
              <button
                className="btn btn--primary"
                onClick={() => navigate(restorePath(source, set, snapshot.runId))}
              >
                {"Restore\u2026"}
              </button>
            ) : null}
          </>
        }
      />

      {[verify.failure, hold.failure, release.failure].map((failure, index) =>
        failure ? (
          <div key={index} style={{ marginBottom: 14 }}>
            <ErrorState
              message={failure.message}
              remediation={failure.remediation}
              correlationId={failure.correlationId}
              detail={failure.detail}
            />
          </div>
        ) : null
      )}

      {verify.operation ? (
        <section className="card" aria-label="Verification in progress">
          <div className="card__body">
            <OperationProgress operation={verify.operation} />
          </div>
        </section>
      ) : null}

      {/* A failed verification is the headline of this page when it
          happened, above every panel, because everything below it
          describes a snapshot that is NOT a restore point. */}
      {failed ? (
        <WarningBanner
          tone="danger"
          eyebrow="Not a restore point"
          title={"This run failed verification, and carries no achieved level at all"}
          dismissible={false}
        >
          <p style={{ margin: 0 }}>{snapshot.reason}</p>
          <p style={{ margin: "6px 0 0" }}>
            {"A failed check proves nothing, so verification_level_achieved is absent here " +
              "rather than carrying the level of the run before it. The snapshot is stored " +
              "and can be inspected; it is not offered as a restore point."}
          </p>
        </WarningBanner>
      ) : null}

      <section className="card" aria-label="What this run did">
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(196px, 1fr))" }}>
          <MetricCard
            label="Entries scanned"
            value={measured(snapshot.entriesScanned, (n) => n.toLocaleString())}
            detail={measured(snapshot.files, (n) => n.toLocaleString() + " files")}
          />
          <MetricCard
            label="Logical size"
            value={measured(snapshot.logicalBytes, bytes)}
            detail="the tree as described"
          />
          <MetricCard
            label="Read from source"
            value={measured(snapshot.sourceBytesRead, bytes)}
            detail="every byte the source offered"
          />
          <MetricCard
            label="Written to repository"
            value={measured(snapshot.repositoryBytesWritten, bytes)}
            detail="after deduplication"
          />
          <MetricCard
            label="Reused"
            tip="snapshots.reused"
            value={measured(snapshot.contentReusedBytes, bytes)}
            detail={
              snapshot.contentReusedBytes === null
                ? "not accounted for on this run"
                : "content the repository already held"
            }
          />
          <MetricCard
            label="Duration"
            value={measured(snapshot.durationSeconds, duration)}
            detail={
              snapshot.completedAt === null
                ? "this run has not finished"
                : stamp(snapshot.startedAt) + " to " + stamp(snapshot.completedAt)
            }
          />
        </div>
      </section>

      <Card title="Snapshot">
        <Rows>
          <Row
            label="Snapshot"
            wire="snapshot_id"
            mono
            value={snapshot.snapshotId ?? "no manifest was committed by this run"}
          />
          <Row label="Run" wire="run_id" mono value={snapshot.runId} />
          <Row
            label="Operation"
            wire="operation_id"
            mono
            value={snapshot.operationId ?? "not recorded against an operation"}
          />
          <Row label="Backup set" wire="backup_set_id" mono value={setId} />
          <Row
            label="Repository domain"
            wire="repository_domain"
            mono
            value={snapshot.repositoryDomain ?? "not recorded"}
          />
          <Row
            label="State"
            wire="phase"
            value={<StatusBadge tone={phase.tone} icon={phase.icon}>{phase.word}</StatusBadge>}
          />
          <Row
            label="Source complete"
            wire="source_complete"
            value={sourceCompleteSentence(snapshot.sourceComplete)}
          />
          <Row
            label="Restore point"
            wire="last_known_good"
            value={
              snapshot.lastKnownGood ? (
                <StatusBadge tone="ok" icon="success">Newest known-good for this set</StatusBadge>
              ) : (
                "No \u2014 a newer verified run holds that place, or this one was never verified"
              )
            }
          />
        </Rows>
      </Card>

      <Card title="Verification" tip="snapshots.achieved-level">
        <Rows>
          <Row
            label="Asked for"
            wire="verification_level"
            value={askedFor === null ? "inherited from the deployment default" : VERIFICATION_COPY[askedFor].name}
          />
          <Row
            label="Proved"
            wire="verification_level_achieved"
            value={
              <span style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                <VerificationBadge
                  status={snapshot.verificationStatus}
                  achieved={snapshot.verificationLevelAchieved}
                />
                {snapshot.verificationLevelAchieved === null ? (
                  <span style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                    no achieved level recorded
                  </span>
                ) : null}
              </span>
            }
          />
          <Row
            label="Consistency declared"
            wire="source_consistency"
            value={
              snapshot.consistencyMode === null
                ? "not declared for this run"
                : CONSISTENCY_COPY[snapshot.consistencyMode].name +
                  " \u2014 " +
                  CONSISTENCY_COPY[snapshot.consistencyMode].summary
            }
          />
        </Rows>
        <Note>
          {"The level a run proves is not always the level it was asked for, so both are shown. " +
            "A failed check proves nothing at all and carries no level."}
        </Note>
      </Card>

      <Card title="Contents">
        <Rows>
          <Row
            label="Tree walked"
            mono
            value={configured ? configured.remoteFolder : "the backup set's configured source"}
          />
          {/* The counts, together, in the card that is about what the
              snapshot HOLDS. The tiles above report the same run as
              sizes; this is the tree. */}
          <Row label="Entries" mono value={measured(snapshot.entriesScanned, (n) => n.toLocaleString())} />
          <Row label="Files" mono value={measured(snapshot.files, (n) => n.toLocaleString())} />
          <Row label="Directories" mono value={measured(snapshot.directories, (n) => n.toLocaleString())} />
        </Rows>
        <Note>
          {"What a snapshot holds is the tree the run walked, counted here. A per-path listing " +
            "is not part of the snapshot read: the repository holds it, and the restore flow is " +
            "where a path inside the snapshot is named."}
        </Note>
      </Card>

      <Card title="Holds">
        {snapshot.holds.length === 0 ? (
          <Note>
            {"No hold is in force. Retention decides this snapshot's fate on the chain alone, " +
              "and a hold placed before the next pass changes that answer."}
          </Note>
        ) : (
          <div className="table-scroll">
            <table className="table">
              <thead>
                <tr>
                  <th scope="col">Reason</th>
                  <th scope="col">Placed by</th>
                  <th scope="col">Placed</th>
                  <th scope="col" style={{ textAlign: "right" }}>Actions</th>
                </tr>
              </thead>
              <tbody>
                {snapshot.holds.map((h) => (
                  <tr key={h.holdId}>
                    <td>{h.reason}</td>
                    <td className="mono">{h.placedBy}</td>
                    <td className="mono">{stamp(h.placedAt)}</td>
                    <td>
                      <div style={{ display: "flex", justifyContent: "flex-end" }}>
                        <button
                          className="btn btn--sm btn--destructive"
                          aria-label={"Release hold " + h.holdId}
                          disabled={readOnly || release.busy || !release.ready}
                          onClick={() => setReleasing(h)}
                        >
                          Release
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <Note>
          {"A hold is one person's recorded decision, with a reason and a name against it. " +
            "Several may sit on one snapshot, and the snapshot survives until the last of them " +
            "is released."}
        </Note>
      </Card>

      <Card title="How this run got here">
        <Timeline transitions={detail.data.transitions} />
        <Note>
          {"The run record is overwritten by every advance, so it says what this run IS and " +
            "never how it got there. This log is what tells a run verified twice from one " +
            "verified once."}
        </Note>
      </Card>

      <ConfirmationDialog
        open={holdOpen}
        eyebrow="Retention will not delete a held snapshot"
        title="Place a hold"
        confirmLabel="Place hold"
        disabled={hold.busy || holdReason.trim() === ""}
        onCancel={() => setHoldOpen(false)}
        onConfirm={() =>
          hold.submit(
            snapshot.runId,
            ({ configRevision, idempotencyKey }) =>
              api.holdSnapshot({
                backupSetId: setId,
                runId: snapshot.runId,
                reason: holdReason.trim(),
                configRevision,
                idempotencyKey
              }),
            () => {
              setHoldOpen(false);
              detail.reload();
            }
          )
        }
      >
        <p style={{ margin: "0 0 12px" }}>
          {"A held snapshot is never expired by retention, whatever the chain says, until the " +
            "hold is released. The reason is required: a hold nobody can attribute is one " +
            "nobody dares release."}
        </p>
        <label className="field">
          <span className="field__label">Reason</span>
          <input
            className="input"
            value={holdReason}
            onChange={(e) => setHoldReason(e.target.value)}
          />
        </label>
      </ConfirmationDialog>

      <ConfirmationDialog
        open={releasing !== null}
        destructive
        eyebrow="The retention chain governs it again"
        title="Release this hold"
        confirmLabel="Release hold"
        disabled={release.busy}
        onCancel={() => setReleasing(null)}
        onConfirm={() => {
          const target = releasing;
          if (target === null) return;
          release.submit(
            target.holdId,
            ({ configRevision, idempotencyKey }) =>
              api.releaseSnapshotHold({
                backupSetId: setId,
                holdId: target.holdId,
                configRevision,
                idempotencyKey
              }),
            () => {
              setReleasing(null);
              detail.reload();
            }
          );
        }}
      >
        <p style={{ margin: 0 }}>
          {releasing === null
            ? ""
            : "Releasing \"" + releasing.reason + "\" leaves this snapshot governed by the " +
              "retention chain again. The next pass may delete it. Releasing deletes nothing " +
              "by itself, and any other hold on this snapshot still holds it."}
        </p>
      </ConfirmationDialog>
    </>
  );
}

/** One card on this page. The tooltip host WRAPS the heading rather than
 *  sitting inside it: a control inside a heading contributes its own name
 *  to the heading's, and this page is found by those names. */
function Card({
  title,
  tip,
  children
}: {
  title: string;
  tip?: TooltipId;
  children: ReactNode;
}) {
  const heading = <h2 className="eyebrow">{title}</h2>;
  return (
    <section className="card">
      <div className="card__header">{tip ? <InfoTooltip id={tip}>{heading}</InfoTooltip> : heading}</div>
      <div className="card__body">{children}</div>
    </section>
  );
}

/** The state machine's edges, oldest first, exactly as they happened.
 *  A repeated `to` is not a rendering bug: a run whose verification was
 *  interrupted and restarted really does record VERIFICATION twice, and
 *  that is the fact this log exists to carry. */
function Timeline({ transitions }: { transitions: SnapshotTransition[] }) {
  if (transitions.length === 0)
    return <Note>This run recorded no transitions.</Note>;
  return (
    <ol style={{ margin: 0, padding: 0, listStyle: "none", display: "flex", flexDirection: "column", gap: 2 }}>
      {transitions.map((transition, index) => (
        <li
          key={index}
          style={{
            display: "grid",
            gridTemplateColumns: "minmax(120px, auto) minmax(180px, auto) 1fr",
            gap: 12,
            padding: "7px 0",
            borderBottom: "1px solid var(--border)",
            fontSize: 13
          }}
        >
          <span className="mono" style={{ color: "var(--text-2)" }}>{stamp(transition.at)}</span>
          {/* The arrow is artwork, not a character (#621): a glyph here
              would be announced as "rightwards arrow" between two state
              names, and the two names are the sentence. */}
          <span className="mono" style={{ display: "inline-flex", alignItems: "center", gap: 7 }}>
            {transition.from === null ? "\u2014" : phasePresentation(transition.from).word}
            <span aria-hidden="true" style={{ color: "var(--text-3)", display: "inline-flex" }}>
              <Icon name="arrow-right" size={11} />
            </span>
            {phasePresentation(transition.to).word}
          </span>
          <span style={{ color: "var(--text-2)" }}>{transition.detail}</span>
        </li>
      ))}
    </ol>
  );
}
