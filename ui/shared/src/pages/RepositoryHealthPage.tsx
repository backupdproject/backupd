/**
 * Every repository domain's own health (EPIC K, issue #788).
 *
 * # This is a different question from a backup set's health
 *
 * A set can be perfectly fresh while the repository holding its snapshots
 * is unwritable, out of maintenance, or reachable only by a process whose
 * clock has drifted far enough to mis-order manifests. So this page
 * exists, and it speaks the SAME three words a set's health does —
 * HEALTHY, DEGRADED, FAILING — because one severity scale per dashboard
 * is what stops an operator working out which "degraded" is the worse
 * one.
 *
 * # Every probe separately
 *
 * Reachable, readable, writable, credentials, clock and maintenance are
 * six rows and not one boolean, because the REMEDIES differ: unreachable
 * is a mount, unwritable is a permission, invalid credentials is a
 * passphrase, and overdue maintenance is a schedule. Readable-but-not-
 * writable is the case that proves the point, and it is the one a single
 * "unavailable" would hide: everything already stored can still be
 * restored.
 *
 * # The clock's sign is the message
 *
 * `clock_skew_seconds` is signed and nullable. Negative is a clock behind
 * the repository, which dates a new snapshot before one already stored,
 * and is the dangerous direction; null is "not measured" and is never
 * drawn as a perfect zero. Both live in snapshotPresentation, with the
 * maintenance page, so the two screens cannot word it differently.
 */
import { useNavigate } from "react-router-dom";

import { useApi } from "@shared/api/ApiContext";
import { CheckList } from "@shared/components/CheckList";
import { Note, Row, Rows } from "@shared/components/Definitions";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { PageHeader } from "@shared/components/PageHeader";
import { StatusBadge } from "@shared/components/StatusBadge";
import { WarningBanner } from "@shared/components/WarningBanner";
import { useAsync } from "@shared/hooks/useAsync";
import type { RepositoryHealth } from "@shared/types/snapshot";
import { relativeAge, stamp } from "@shared/utilities/format";
import { REPOSITORY_STATE_PRESENTATION, repositoryChecks } from "@shared/pages/snapshotPresentation";

export function RepositoryHealthPage() {
  const api = useApi();
  const navigate = useNavigate();
  const fleet = useAsync(() => api.listRepositories(), [api]);

  const domains = fleet.data?.repositories ?? [];
  const needAttention = domains.filter((d) => d.state !== "HEALTHY" || d.maintenanceOverdue);

  const header = (
    <PageHeader
      title="Repository health"
      subtitle={
        fleet.data === null
          ? "Reading every repository domain"
          : "Checked " + relativeAge(fleet.data.generatedAt) + " \u00b7 " + domains.length + " domains"
      }
      actions={
        <>
          <button className="btn" onClick={() => navigate("/repositories/maintenance")}>
            Maintenance
          </button>
          <button className="btn" onClick={fleet.reload}>Check now</button>
        </>
      }
    />
  );

  if (fleet.error)
    return (
      <>
        {header}
        <ErrorState
          message={fleet.error.message}
          correlationId={fleet.error.correlationId}
          onRetry={fleet.reload}
        />
      </>
    );

  return (
    <>
      {header}

      {needAttention.length > 0 ? (
        <WarningBanner
          tone="warn"
          eyebrow={
            needAttention.length === 1 ? "One domain needs attention" : needAttention.length + " domains need attention"
          }
          title={needAttention.map((d) => d.domain).join(", ")}
          actions={
            <button className="btn btn--sm" onClick={() => navigate("/repositories/maintenance")}>
              Open maintenance
            </button>
          }
        >
          {"A domain listed here is not necessarily one that has lost anything. Read its probes " +
            "below: a store that is readable and not writable still serves every restore, and a " +
            "store out of its maintenance window is still writing snapshots \u2014 what is not " +
            "happening is reclamation, so it keeps paying for content nothing references."}
        </WarningBanner>
      ) : null}

      {fleet.data !== null && domains.length === 0 ? (
        <EmptyState title="No repository domains are declared">
          {"A repository domain is the encrypted store an incremental backup set writes its " +
            "snapshots into. None is declared on this deployment yet, so there is nothing to " +
            "probe."}
        </EmptyState>
      ) : null}

      {domains.map((domain) => (
        <DomainCard key={domain.domain} health={domain} />
      ))}

      {domains.length > 0 ? (
        <Note>
          {"Reachability is proved by a read and a write, not inferred from an open handle: a " +
            "repository handle survives a network partition, and every call on it would fail."}
        </Note>
      ) : null}
    </>
  );
}

/** One domain, with its verdict in the header and its probes in the body.
 *  The verdict is a summary of the probes and never a replacement for
 *  them: two domains can both be DEGRADED for reasons that need different
 *  people. */
function DomainCard({ health }: { health: RepositoryHealth }) {
  const state = REPOSITORY_STATE_PRESENTATION[health.state];
  return (
    <section className="card">
      <div
        className="card__header"
        style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 12 }}
      >
        <h2 className="eyebrow mono">{health.domain}</h2>
        <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
          {health.mayShare ? null : (
            <StatusBadge tone="neutral" icon="status-idle">Isolated</StatusBadge>
          )}
          <StatusBadge tone={state.tone} icon={state.icon}>{state.word}</StatusBadge>
        </div>
      </div>
      <div className="card__body">
        <CheckList checks={repositoryChecks(health)} />
        <div style={{ marginTop: 14 }}>
          <Rows>
            <Row
              label="Last snapshot"
              wire="last_snapshot_at"
              value={
                health.lastSnapshotAt === null
                  ? "no snapshot has been written here"
                  : stamp(health.lastSnapshotAt) + " \u00b7 " + health.lastSnapshotStatus
              }
            />
            <Row
              label="Last verification"
              wire="last_verification_status"
              value={
                health.lastVerificationAt === null
                  ? "nothing stored here has been verified"
                  : stamp(health.lastVerificationAt) + " \u00b7 " + health.lastVerificationStatus
              }
            />
            <Row
              label="Last maintenance"
              wire="last_maintenance_at"
              value={
                health.lastMaintenanceAt === null
                  ? "maintenance has never run here"
                  : stamp(health.lastMaintenanceAt) +
                    (health.lastMaintenanceResult ? " \u00b7 " + health.lastMaintenanceResult : "")
              }
            />
            <Row
              label="Backup sets here"
              wire="backup_sets"
              mono
              value={
                health.backupSets.length === 0
                  ? "none yet"
                  : health.backupSets.join(" \u00b7 ")
              }
            />
            <Row label="Detail" wire="detail" value={health.detail} />
          </Rows>
        </div>
      </div>
    </section>
  );
}
