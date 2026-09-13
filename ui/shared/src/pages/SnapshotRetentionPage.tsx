/**
 * What the next snapshot-retention pass would do, and the holds that
 * override it (EPIC K, issue #788).
 *
 * # This is a PREVIEW, and it has no apply button
 *
 * Nothing on this page removes anything. Snapshot retention is applied by
 * the deployment's own pass, and the api/v1 contract has no apply route
 * for it at all — `.../snapshot-retention` is a plain GET, deliberately
 * distinct from FR-18's artifact retention plan under `.../retention`,
 * which does have one. A button here would either be a lie or a second
 * meaning for one word.
 *
 * # REFUSE is not a third shade of DELETE
 *
 * KEEP is a tier selecting a snapshot, DELETE is a candidate with nothing
 * protecting it, and REFUSE means the pass DECIDED to delete and
 * something stopped it. That last one is the only one of the three that
 * needs somebody to look at it, and it is drawn neutral rather than in a
 * deleting colour: the pass removed nothing, so nothing is pending.
 *
 * # The badges are the product's own
 *
 * `RetentionTierBadges` draws the verdict's `{tier, selected_by}` pairs,
 * which is what keeps this page's vocabulary the same as the artifact
 * retention dialog's. Last-known-good protection carries no placement and
 * is badged bare as **Protected**, because a parenthesised word after it
 * would read as a placement.
 */
import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";

import { useApi } from "@shared/api/ApiContext";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
import { Note } from "@shared/components/Definitions";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { PageHeader } from "@shared/components/PageHeader";
import { RetentionTierBadges } from "@shared/components/RetentionBadge";
import { StatusBadge } from "@shared/components/StatusBadge";
import { useAsync } from "@shared/hooks/useAsync";
import { useSnapshotOperation } from "@shared/hooks/useSnapshotOperation";
import type { SnapshotHold, SnapshotRetentionVerdict } from "@shared/types/snapshot";
import { relativeAge, stamp } from "@shared/utilities/format";
import { backupSetPath, snapshotsPath } from "@shared/utilities/routes";
import { snapshotTierSelections, unplacedSelections } from "@shared/pages/snapshotPresentation";

export function SnapshotRetentionPage({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const navigate = useNavigate();
  const params = useParams();
  const source = params.source ?? "";
  const set = params.set ?? "";
  const setId = source + "/" + set;

  const preview = useAsync(() => api.getSnapshotRetention(source, set), [api, source, set]);
  const holds = useAsync(() => api.listSnapshotHolds(source, set), [api, source, set]);
  const hold = useSnapshotOperation("Could not place the hold.");
  const release = useSnapshotOperation("Could not release the hold.");

  // The run a hold is being placed on, and the hold being released. Both
  // hold the row rather than a boolean, so each dialog can name what it
  // is about: a confirmation opened from one row of many has to prove
  // which row it will act on.
  const [holding, setHolding] = useState<SnapshotRetentionVerdict | null>(null);
  const [holdReason, setHoldReason] = useState("");
  const [releasing, setReleasing] = useState<SnapshotHold | null>(null);

  const reloadBoth = () => {
    preview.reload();
    holds.reload();
  };

  const header = (
    <PageHeader
      back={{ label: "Back to " + setId, onClick: () => navigate(backupSetPath(source, set)) }}
      title="Snapshot retention"
      subtitle={
        preview.data === null
          ? setId
          : setId +
            " \u00b7 what the next pass would do, generated " +
            relativeAge(preview.data.generatedAt) +
            " \u00b7 nothing here has run"
      }
      actions={
        <>
          <button className="btn" onClick={() => navigate(snapshotsPath(source, set))}>
            Snapshots
          </button>
          <button className="btn" onClick={reloadBoth}>Preview again</button>
        </>
      }
    />
  );

  if (preview.error?.code === "BACKUP_SET_NOT_INCREMENTAL")
    return (
      <>
        {header}
        <EmptyState title="This backup set has no snapshot retention">
          {setId + " stores whole artifacts, so its retention is the artifact chain on the set's " +
            "own page. Snapshot retention belongs to the incremental engine."}
        </EmptyState>
      </>
    );

  if (preview.error)
    return (
      <>
        {header}
        <ErrorState
          message={preview.error.message}
          correlationId={preview.error.correlationId}
          onRetry={reloadBoth}
        />
      </>
    );

  const verdicts = preview.data?.verdicts ?? [];
  const activeHolds = (holds.data ?? []).filter((h) => h.active);

  return (
    <>
      {header}

      {[hold.failure, release.failure].map((failure, index) =>
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

      <section className="card">
        <div className="card__header">
          <h2 className="eyebrow">What the next pass would do</h2>
        </div>
        {verdicts.length === 0 ? (
          <div className="card__body">
            <Note>
              {"No snapshot has a verdict yet. A verdict appears once there is a snapshot for a " +
                "retention pass to have an opinion about."}
            </Note>
          </div>
        ) : (
          <>
            <div className="table-scroll">
              <table className="table" style={{ minWidth: 940 }}>
                <thead>
                  <tr>
                    <th scope="col">Taken</th>
                    <th scope="col">Snapshot</th>
                    <th scope="col">Action</th>
                    <th scope="col">Kept by</th>
                    <th scope="col">Why</th>
                    <th scope="col" style={{ textAlign: "right" }}>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {verdicts.map((verdict) => (
                    <tr key={verdict.runId}>
                      <td className="mono">{stamp(verdict.startedAt)}</td>
                      <td className="mono" style={{ fontSize: "var(--text-sm)" }}>
                        {verdict.snapshotId ?? (
                          <span style={{ color: "var(--text-3)" }}>no manifest committed</span>
                        )}
                      </td>
                      <td><ActionBadge verdict={verdict} /></td>
                      <td>
                        <div style={{ display: "flex", flexDirection: "column", gap: 5, alignItems: "flex-start" }}>
                          {/* "unclassified" is RetentionTierBadges'
                              wording for "no tier claims this", which is
                              the DELETE case. A held snapshot with no
                              tier is not unclassified in any useful
                              sense: the hold below is what keeps it, and
                              printing both would name two answers to one
                              question. */}
                          {verdict.tiers.length > 0 || verdict.holdReason === null ? (
                            <RetentionTierBadges tiers={snapshotTierSelections(verdict.tiers)} />
                          ) : null}
                          {verdict.holdReason ? (
                            <StatusBadge tone="accent" icon="quarantine">
                              {"Hold: " + verdict.holdReason}
                            </StatusBadge>
                          ) : null}
                          {unplacedSelections(verdict.tiers).map((named) => (
                            <span
                              key={named}
                              className="mono"
                              style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}
                            >
                              {"selected by " + named}
                            </span>
                          ))}
                        </div>
                      </td>
                      <td style={{ fontSize: 13, color: "var(--text-2)" }}>{verdict.reason}</td>
                      <td>
                        <div style={{ display: "flex", gap: 6, justifyContent: "flex-end" }}>
                          <button
                            className="btn btn--sm"
                            aria-label={"Place a hold on " + verdict.runId}
                            disabled={readOnly || hold.busy || !hold.ready}
                            onClick={() => {
                              setHoldReason("");
                              setHolding(verdict);
                            }}
                          >
                            {"Hold\u2026"}
                          </button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <div style={{ padding: "0 18px 16px" }}>
              <Note>
                {"A verdict names what selected the snapshot, or what protects it. \u201cDelete\u201d " +
                  "here is a projection: nothing is removed until the deployment's own pass runs, " +
                  "and a hold placed before then changes the answer."}
              </Note>
            </div>
          </>
        )}
      </section>

      <section className="card">
        <div className="card__header">
          <h2 className="eyebrow">Holds</h2>
        </div>
        {holds.error ? (
          <div className="card__body">
            <ErrorState
              message={holds.error.message}
              correlationId={holds.error.correlationId}
              onRetry={holds.reload}
            />
          </div>
        ) : activeHolds.length === 0 ? (
          <div className="card__body">
            <Note>
              {"No hold is in force on this backup set. Every snapshot here is governed by the " +
                "retention chain alone."}
            </Note>
          </div>
        ) : (
          <>
            <div className="table-scroll">
              <table className="table">
                <thead>
                  <tr>
                    <th scope="col">Snapshot run</th>
                    <th scope="col">Reason</th>
                    <th scope="col">Placed by</th>
                    <th scope="col">Placed</th>
                    <th scope="col" style={{ textAlign: "right" }}>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {activeHolds.map((h) => (
                    <tr key={h.holdId}>
                      <td className="mono" style={{ fontSize: "var(--text-sm)" }}>{h.runId}</td>
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
            <div style={{ padding: "0 18px 16px" }}>
              <Note>
                {"A hold is one person's recorded decision, with a reason and a name against it. " +
                  "Several may sit on one snapshot, and the snapshot survives until the last of " +
                  "them is released."}
              </Note>
            </div>
          </>
        )}
      </section>

      <ConfirmationDialog
        open={holding !== null}
        eyebrow="Retention will not delete a held snapshot"
        title="Place a hold"
        confirmLabel="Place hold"
        disabled={hold.busy || holdReason.trim() === ""}
        onCancel={() => setHolding(null)}
        onConfirm={() => {
          const target = holding;
          if (target === null) return;
          // The run is the target the pending idempotency key is filed
          // under: one hook instance serves every row here, so a hold
          // that failed on one run must not lend its key to the next.
          hold.submit(
            target.runId,
            ({ configRevision, idempotencyKey }) =>
              api.holdSnapshot({
                backupSetId: setId,
                runId: target.runId,
                reason: holdReason.trim(),
                configRevision,
                idempotencyKey
              }),
            () => {
              setHolding(null);
              reloadBoth();
            }
          );
        }}
      >
        <p style={{ margin: "0 0 12px" }}>
          {holding === null
            ? ""
            : "A hold on run " + holding.runId + " stops retention expiring it, whatever the " +
              "chain says, until the hold is released. The reason is required: a hold nobody can " +
              "attribute is one nobody dares release."}
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
              reloadBoth();
            }
          );
        }}
      >
        <p style={{ margin: 0 }}>
          {releasing === null
            ? ""
            : "Releasing \u201c" + releasing.reason + "\u201d leaves run " + releasing.runId +
              " governed by the retention chain again, and the next pass may delete it. " +
              "Releasing deletes nothing by itself, and any other hold on that snapshot still " +
              "holds it."}
        </p>
      </ConfirmationDialog>
    </>
  );
}

/** KEEP, DELETE and REFUSE, badged. Three tones for three different
 *  facts, and REFUSE deliberately neutral: it is not a severity of
 *  delete, it is the pass declining to. */
function ActionBadge({ verdict }: { verdict: SnapshotRetentionVerdict }) {
  if (verdict.action === "KEEP")
    return <StatusBadge tone="ok" icon="success">Keep</StatusBadge>;
  if (verdict.action === "DELETE")
    return <StatusBadge tone="warn" icon="warning">Delete</StatusBadge>;
  return <StatusBadge tone="neutral" icon="status-idle">Refused</StatusBadge>;
}
