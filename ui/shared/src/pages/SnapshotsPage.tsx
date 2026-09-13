/**
 * Every snapshot one backup set holds (EPIC K, issue #788).
 *
 * # Four byte counts, never one
 *
 * The table reports logical size, what the run read off the source, what
 * landed in the repository and what the repository already had, as four
 * separate columns. A single "backed up" total would report a
 * deduplicating repository as growing by the size of the source every
 * night, which is the number an operator would then go looking for a
 * fault in.
 *
 * # Absent is not zero
 *
 * Every counter here is nullable on the wire and `measured()` is the only
 * thing that renders one. A run whose engine could not account for reuse
 * says "not measured"; it never shows a 0, because "reused 0 bytes" is a
 * measurement and it describes a repository that deduplicated nothing.
 *
 * # The run is the name
 *
 * Rows are addressed by run id, which exists from the moment a pass
 * starts. The engine's manifest id exists only once a manifest was
 * committed, so a failed run has the first and not the second, and a
 * screen that keyed on the manifest id could not show the runs most worth
 * looking at (types/snapshot.ts).
 */
import { useNavigate, useParams } from "react-router-dom";

import { useApi } from "@shared/api/ApiContext";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { EngineBadge, VerificationBadge } from "@shared/components/EngineBadge";
import { Note, WireField } from "@shared/components/Definitions";
import { OperationProgress } from "@shared/components/OperationProgress";
import { PageHeader } from "@shared/components/PageHeader";
import { StatusBadge } from "@shared/components/StatusBadge";
import { useAsync } from "@shared/hooks/useAsync";
import { useSnapshotOperation } from "@shared/hooks/useSnapshotOperation";
import { useCausl } from "@shared/state/graph";
import { setsNode } from "@shared/state/appNodes";
import type { Snapshot } from "@shared/types/snapshot";
import { bytes, duration, measured, stamp } from "@shared/utilities/format";
import { backupSetPath, restorePath, snapshotPath, snapshotRetentionPath } from "@shared/utilities/routes";
import { phasePresentation } from "@shared/pages/snapshotPresentation";

export function SnapshotsPage({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const navigate = useNavigate();
  const params = useParams();
  const source = params.source ?? "";
  const set = params.set ?? "";
  const setId = source + "/" + set;

  const sets = useCausl(setsNode);
  const configured = (sets.data ?? []).find((s) => s.id === setId) ?? null;

  const snapshots = useAsync(() => api.listSnapshots(source, set), [api, source, set]);
  const verify = useSnapshotOperation("Could not start the verification.");

  const rows = snapshots.data ?? [];

  const header = (
    <PageHeader
      back={{ label: "Back to " + setId, onClick: () => navigate(backupSetPath(source, set)) }}
      title="Snapshots"
      subtitle={
        <span style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
          {configured ? <EngineBadge engine={configured.engine} /> : null}
          <span>
            {setId +
              (snapshots.data === null ? "" : " \u00b7 " + rows.length + " snapshots") +
              (configured?.incremental?.repositoryDomain
                ? " \u00b7 repository domain " + configured.incremental.repositoryDomain
                : "")}
          </span>
        </span>
      }
      actions={
        <>
          <button
            className="btn"
            onClick={() => navigate(snapshotRetentionPath(source, set))}
          >
            Retention and holds
          </button>
          {/* Omits run_id on purpose: an unnamed verify checks the newest
              snapshot, which is what "Verify latest" means, and naming
              the row's run here would make the button quietly wrong the
              moment a run finished while the page was open. */}
          <button
            className="btn btn--primary"
            disabled={readOnly || verify.busy || !verify.ready || rows.length === 0}
            onClick={() =>
              verify.submit(
                ({ configRevision, idempotencyKey }) =>
                  api.verifySnapshot({ backupSetId: setId, configRevision, idempotencyKey }),
                () => snapshots.reload()
              )
            }
          >
            {verify.busy ? "Starting\u2026" : "Verify latest"}
          </button>
        </>
      }
    />
  );

  // BACKUP_SET_NOT_INCREMENTAL is a different screen from "no snapshots
  // yet", and collapsing the two would tell an operator to wait for a
  // first run that can never happen: an artifact set stores whole files
  // and has no snapshot history at all, whatever it does next.
  if (snapshots.error?.code === "BACKUP_SET_NOT_INCREMENTAL")
    return (
      <>
        {header}
        <EmptyState title="This backup set has no snapshots">
          {setId + " stores whole artifacts, one file at a time, so there is no snapshot " +
            "history to show. Snapshots belong to the incremental engine, and a set's engine " +
            "is chosen once when it is created."}
        </EmptyState>
      </>
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

  return (
    <>
      {header}

      {verify.failure ? (
        <div style={{ marginBottom: 14 }}>
          <ErrorState
            message={verify.failure.message}
            remediation={verify.failure.remediation}
            correlationId={verify.failure.correlationId}
            detail={verify.failure.detail}
          />
        </div>
      ) : null}

      {verify.operation ? (
        <section className="card" aria-label="Verification in progress">
          <div className="card__body">
            <OperationProgress operation={verify.operation} />
          </div>
        </section>
      ) : null}

      {rows.length === 0 ? (
        <EmptyState title="No snapshots yet">
          {"Nothing has run against " + setId + " yet, or every run so far was removed by " +
            "retention. A snapshot appears here as soon as a pass commits a manifest."}
        </EmptyState>
      ) : (
        <section className="card">
          <div className="table-scroll">
            <table className="table" style={{ minWidth: 1100 }}>
              <thead>
                <tr>
                  <th scope="col">Taken</th>
                  <th scope="col">Snapshot</th>
                  <th scope="col">State</th>
                  <th scope="col">Entries</th>
                  <th scope="col">Logical</th>
                  <th scope="col">Read</th>
                  <th scope="col">Written</th>
                  <th scope="col">Reused</th>
                  <th scope="col">Duration</th>
                  <th scope="col">Verification</th>
                  <th scope="col" style={{ textAlign: "right" }}>Actions</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((snapshot) => (
                  <SnapshotRow
                    key={snapshot.runId}
                    snapshot={snapshot}
                    onInspect={() => navigate(snapshotPath(source, set, snapshot.runId))}
                    onRestore={() => navigate(restorePath(source, set, snapshot.runId))}
                  />
                ))}
              </tbody>
            </table>
          </div>
          <div style={{ padding: "0 18px 16px" }}>
            <Note>
              <>
                {"Four byte counts, never one. "}
                <WireField name="logical_bytes" />
                {" is the size of the tree, "}
                <WireField name="source_bytes_read" />
                {" is what this run pulled off the server, "}
                <WireField name="repository_bytes_written" />
                {" is what landed in storage, and "}
                <WireField name="content_reused_bytes" />
                {" is what the repository already had. A run whose engine could not account " +
                  "for reuse says so; it never shows a zero."}
              </>
            </Note>
          </div>
        </section>
      )}
    </>
  );
}

/** One run. Split out because the row carries three conditional badges
 *  and two actions, and the table above reads as a table without them
 *  inlined into it. */
function SnapshotRow({
  snapshot,
  onInspect,
  onRestore
}: {
  snapshot: Snapshot;
  onInspect(): void;
  onRestore(): void;
}) {
  const phase = phasePresentation(snapshot.phase);
  // A run with no committed manifest cannot be restored from: there is no
  // id to ask the repository for. The control is withheld rather than
  // offered and refused.
  const restorable = snapshot.snapshotId !== null;
  return (
    <tr>
      <td>
        <div>{stamp(snapshot.startedAt)}</div>
        {snapshot.lastKnownGood ? (
          <div style={{ marginTop: 4 }}>
            <StatusBadge tone="ok" icon="success">Newest known-good</StatusBadge>
          </div>
        ) : null}
        {snapshot.holds.length > 0 ? (
          <div style={{ marginTop: 4 }}>
            <StatusBadge tone="accent" icon="quarantine">
              {snapshot.holds.length === 1 ? "Held" : snapshot.holds.length + " holds"}
            </StatusBadge>
          </div>
        ) : null}
      </td>
      <td className="mono" style={{ fontSize: "var(--text-sm)" }}>
        {snapshot.snapshotId ?? (
          <span style={{ color: "var(--text-3)" }}>no manifest committed</span>
        )}
      </td>
      <td>
        <StatusBadge tone={phase.tone} icon={phase.icon}>{phase.word}</StatusBadge>
      </td>
      <td className="mono">{measured(snapshot.entriesScanned, (n) => n.toLocaleString())}</td>
      <td className="mono">{measured(snapshot.logicalBytes, bytes)}</td>
      <td className="mono">{measured(snapshot.sourceBytesRead, bytes)}</td>
      <td className="mono">{measured(snapshot.repositoryBytesWritten, bytes)}</td>
      {/* Absent is drawn quietly rather than as a figure: it is the one
          cell in the row that is not a number, and it must not be
          mistaken for one. */}
      <td
        className="mono"
        style={{ color: snapshot.contentReusedBytes === null ? "var(--text-3)" : undefined }}
      >
        {measured(snapshot.contentReusedBytes, bytes)}
      </td>
      <td
        className="mono"
        style={{ color: snapshot.durationSeconds === null ? "var(--text-3)" : undefined }}
      >
        {measured(snapshot.durationSeconds, duration)}
      </td>
      <td>
        <VerificationBadge
          status={snapshot.verificationStatus}
          achieved={snapshot.verificationLevelAchieved}
        />
      </td>
      <td>
        {/* The visible label is the verb; the accessible name carries the
            run, because a table of ten identical "Inspect" buttons is
            unnavigable by anything that reads by name. */}
        <div style={{ display: "flex", gap: 6, justifyContent: "flex-end" }}>
          <button className="btn btn--sm" aria-label={"Inspect " + snapshot.runId} onClick={onInspect}>
            Inspect
          </button>
          {restorable ? (
            <button
              className="btn btn--sm"
              aria-label={"Restore from " + snapshot.runId}
              onClick={onRestore}
            >
              {"Restore\u2026"}
            </button>
          ) : null}
        </div>
      </td>
    </tr>
  );
}
