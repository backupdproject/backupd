/**
 * Repository maintenance, per domain (EPIC K, issue #788).
 *
 * # Ownership is the load-bearing fact, so it is the first thing on every
 * card
 *
 * Several deployments may share one repository and exactly one of them
 * may maintain it. An operator looking at a repository that is not being
 * maintained has to be able to tell "nobody owns it" from "the owner is
 * somebody else", because those are two different problems: the first is
 * a claim nobody made, and the second is a machine that may be switched
 * off. An unowned domain is the one that quietly stops being maintained.
 *
 * # Maintenance never touches a snapshot
 *
 * Every snapshot retnd wrote is pinned, and only retention removes one.
 * What maintenance removes is content nothing references any more, which
 * is why an overdue domain is a cost problem and not a data-loss one —
 * and why this page says so rather than letting "overdue" read as
 * "losing backups".
 *
 * # There is no Run maintenance button here, and that is not an omission
 *
 * api/v1 carries no mutating maintenance route: `.../maintenance` is a
 * read. A control that looked like it ran a compaction and did nothing
 * would be worse than no control, and "press it and find out" is how two
 * instances end up compacting one store at once. What this page owes is
 * the state, in enough detail that an operator can tell whether the
 * schedule is working.
 */
import { useApi } from "@shared/api/ApiContext";
import { Banner } from "@shared/components/Banner";
import { Cell, CellGrid, Note, Row, Rows } from "@shared/components/Definitions";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { PageHeader } from "@shared/components/PageHeader";
import { StatusBadge } from "@shared/components/StatusBadge";
import { useAsync } from "@shared/hooks/useAsync";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import { loadRepositoryFleet } from "@shared/pages/repositoryFleet";
import type { DomainRecord } from "@shared/pages/repositoryFleet";
import type { RepositoryMaintenance } from "@shared/types/snapshot";
import { bytes, relativeAge, stamp } from "@shared/utilities/format";

export function RepositoryMaintenancePage() {
  const api = useApi();

  // The fleet-and-maintenance join lives in one place (repositoryFleet.ts,
  // whose own doc says why): this page had a second copy of it, with its
  // own per-domain failure handling, which is two behaviours for one
  // question the moment either is touched.
  const maintenance = useAsync(() => loadRepositoryFleet(api), [api]);

  const domains = maintenance.data?.domains ?? [];

  const header = (
    <PageHeader
      title="Repository maintenance"
      subtitle="Compaction and reclamation, per domain. One instance owns each."
      actions={<button className="btn" onClick={maintenance.reload}>Re-read</button>}
    />
  );

  if (maintenance.error)
    return (
      <>
        {header}
        <ErrorState
          message={maintenance.error.message}
          correlationId={maintenance.error.correlationId}
          onRetry={maintenance.reload}
        />
      </>
    );

  return (
    <>
      {header}

      <Banner tone="info" dismissible={false}>
        <span style={{ fontSize: 13 }}>
          {"Maintenance never touches a snapshot retnd wrote: every one of them is pinned, and " +
            "only retention removes them. What it removes is content nothing references any more."}
        </span>
      </Banner>

      {maintenance.data !== null && domains.length === 0 ? (
        <EmptyState title="No repository domains are declared">
          {"Maintenance is per repository domain, and none is declared on this deployment yet."}
        </EmptyState>
      ) : null}

      {domains.map((domain) => (
        <DomainCard key={domain.health.domain} state={domain} />
      ))}
    </>
  );
}

function DomainCard({ state }: { state: DomainRecord }) {
  const heading = <h2 className="eyebrow mono">{state.health.domain}</h2>;
  const record = state.maintenance;
  return (
    <section className="card">
      <div
        className="card__header"
        style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 12 }}
      >
        <InfoTooltip id="repositories.maintenance-owner">{heading}</InfoTooltip>
        {record === null ? null : <ScheduleBadge record={record} />}
      </div>
      <div className="card__body">
        {record === null ? (
          <ErrorState
            message={state.maintenanceError?.message ?? "This domain's maintenance record could not be read."}
            remediation={state.maintenanceError?.remediation}
            correlationId={state.maintenanceError?.correlationId}
          />
        ) : (
          <>
            <CellGrid min={185}>
              {/* "" is not a name and is not drawn as one: an unclaimed
                  domain is the case where maintenance silently is not
                  happening, so it says exactly that. */}
              <Cell
                label="Owner"
                wire="owner"
                mono={record.owner !== ""}
                value={record.owner === "" ? "nobody has claimed it" : record.owner}
              />
              <Cell
                label="Last quick"
                wire="last_quick_at"
                value={record.lastQuickAt === null ? "never" : relativeAge(record.lastQuickAt)}
              />
              <Cell
                label="Last full"
                wire="last_full_at"
                value={record.lastFullAt === null ? "never" : relativeAge(record.lastFullAt)}
              />
              <Cell
                label="Next eligible"
                wire="next_eligible_at"
                value={record.nextEligibleAt === null ? "not scheduled" : stamp(record.nextEligibleAt)}
              />
              <Cell
                label="Reclaimed, last full"
                wire="reclaimed_bytes"
                value={bytes(record.reclaimedBytes)}
              />
            </CellGrid>
            <div style={{ marginTop: 14 }}>
              <Rows>
                <Row
                  label="Due"
                  wire="due"
                  value={
                    record.due
                      ? (record.dueMode === "" ? "Yes" : "Yes \u2014 " + record.dueMode) +
                        (record.dueReason === "" ? "" : ": " + record.dueReason)
                      : "No \u2014 nothing is owed before the next eligible time"
                  }
                />
                <Row
                  label="Runs"
                  wire="runs"
                  mono
                  value={
                    record.runs.toLocaleString() +
                    " \u00b7 " +
                    record.failures.toLocaleString() +
                    (record.failures === 1 ? " failure" : " failures")
                  }
                />
              </Rows>
            </div>
            {record.owner === "" ? (
              <div style={{ marginTop: 12 }}>
                <Note>
                  {"Nobody owns this domain, so nothing is maintaining it. Snapshots are still " +
                    "written and still restorable; content nothing references is never " +
                    "reclaimed until an instance claims maintenance."}
                </Note>
              </div>
            ) : (
              <div style={{ marginTop: 12 }}>
                <Note>
                  {"Only " + record.owner + " compacts and reclaims this domain. Another " +
                    "instance reads and writes snapshots here and will not maintain it, so a " +
                    "domain whose owner is switched off stops being maintained until ownership " +
                    "is transferred."}
                </Note>
              </div>
            )}
          </>
        )}
      </div>
    </section>
  );
}

/** Overdue, failing, or on schedule. Three states and not two: a domain
 *  whose last runs FAILED is not the same as one that simply has not run,
 *  and the remedy differs. */
function ScheduleBadge({ record }: { record: RepositoryMaintenance }) {
  if (record.failing)
    return <StatusBadge tone="danger" icon="failure">Failing</StatusBadge>;
  if (record.overdue)
    return <StatusBadge tone="warn" icon="warning">Overdue</StatusBadge>;
  if (record.due)
    return <StatusBadge tone="accent" icon="status-active">Due</StatusBadge>;
  return <StatusBadge tone="ok" icon="status-active">On schedule</StatusBadge>;
}
