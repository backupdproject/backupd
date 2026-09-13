/**
 * Every repository domain this deployment has, what is stored in each, and
 * who maintains it (EPIC K, issue #788).
 *
 * # Why this is a deployment screen and not a per-set panel
 *
 * A repository domain is one encrypted store, and the sets inside one
 * share its key, its credential, its maintenance and its blast radius with
 * each other. That is a property of a boundary several sets sit inside, so
 * it cannot be configured — or even honestly described — from inside any
 * one of them: a panel on a set's page could only ever say "this set is in
 * primary-nas", never "and so are these two others, and one key opens all
 * three".
 *
 * # The topology is the argument
 *
 * The table answers "what is the state of each store". The topology below
 * it answers the question the table cannot: which sets are CO-TENANTS.
 * `may_share` is what decides whether a domain can take another set at
 * all, and an isolated domain is drawn as one rather than described as
 * one, because "a second set pointed here is refused" is a fact an
 * operator has to be able to see before they try it.
 */
import { useNavigate } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import { PageHeader } from "@shared/components/PageHeader";
import { Banner } from "@shared/components/Banner";
import { StatusBadge } from "@shared/components/StatusBadge";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { Note, WireField } from "@shared/components/Definitions";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import { isNotConfigured } from "@shared/api/failure";
import { relativeAge } from "@shared/utilities/format";
import { backupSetPath } from "@shared/utilities/routes";
import { clockSkew, failingProbes, loadRepositoryFleet } from "@shared/pages/repositoryFleet";
import type { DomainRecord } from "@shared/pages/repositoryFleet";
import type { RepositoryState } from "@shared/types/snapshot";

/** One severity scale per dashboard: the same three words a backup set's
 *  health uses, drawn the same three ways, so an operator never has to
 *  work out which "degraded" is the worse one. */
const STATE_PRESENTATION: Record<
  RepositoryState,
  { tone: "ok" | "warn" | "danger"; icon: "status-active" | "warning" | "failure"; label: string }
> = {
  HEALTHY: { tone: "ok", icon: "status-active", label: "Healthy" },
  DEGRADED: { tone: "warn", icon: "warning", label: "Degraded" },
  FAILING: { tone: "danger", icon: "failure", label: "Failing" }
};

export function RepositoryDomainsPage({ readOnly }: { readOnly: boolean }) {
  const navigate = useNavigate();
  const api = useApi();
  const fleet = useAsync(() => loadRepositoryFleet(api), [api]);
  const domains = fleet.data?.domains ?? [];

  return (
    <>
      <PageHeader
        title="Repository domains"
        tip="repositories.page"
        subtitle={
          fleet.data === null
            ? "The encrypted stores this deployment's incremental backup sets write to."
            : domains.length +
              (domains.length === 1 ? " domain" : " domains") +
              " \u00b7 " +
              domains.reduce((total, d) => total + d.health.backupSets.length, 0) +
              " backup sets stored in them"
        }
        actions={
          <>
            <InfoTooltip id="repositories.fleet-health">
              <button className="btn" onClick={() => navigate("/repositories/health")}>
                Repository health
              </button>
            </InfoTooltip>
            <InfoTooltip id="repositories.maintenance-view">
              <button className="btn" onClick={() => navigate("/repositories/maintenance")}>
                Maintenance
              </button>
            </InfoTooltip>
            <InfoTooltip id="repositories.define" alignEnd>
              <button
                className="btn btn--primary"
                disabled={readOnly}
                onClick={() => navigate("/repositories/new")}
              >
                Define repository domain
              </button>
            </InfoTooltip>
          </>
        }
      />

      <Banner tone="info" dismissible={false} style={{ fontSize: "var(--text-sm)" }}>
        <span>
          A repository domain is one encrypted store. Backup sets that share one store the content
          they have in common only once, and share its key, its credential, its maintenance and its
          blast radius with each other.
        </span>
      </Banner>

      {isNotConfigured(fleet.error) ? (
        <section className="card">
          <div className="card__body">
            <EmptyState title="This instance has no configuration yet">
              Repository domains are declared by the backup sets that write to them. Add the first
              incremental backup set and the domain it names appears here.
            </EmptyState>
          </div>
        </section>
      ) : fleet.error ? (
        <section className="card">
          <div className="card__body">
            <ErrorState
              message={fleet.error.message}
              remediation="The declared repository domains could not be read, so nothing below is current."
              correlationId={fleet.error.correlationId}
              onRetry={fleet.reload}
            />
          </div>
        </section>
      ) : fleet.loading && fleet.data === null ? (
        <section className="card">
          <div className="card__body">
            <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
              Loading repository domains…
            </p>
          </div>
        </section>
      ) : domains.length === 0 ? (
        <section className="card">
          <div className="card__body">
            <EmptyState title="No repository domain is declared">
              Only incremental backup sets use one. A deployment running artifact sets alone has
              none, and that is an ordinary state rather than a fault.
            </EmptyState>
          </div>
        </section>
      ) : (
        <>
          <section className="card">
            <div className="card__header">
              <h2 className="eyebrow">Domains</h2>
            </div>
            <div className="card__body">
              <div className="table-scroll">
                <table className="table">
                  <thead>
                    <tr>
                      <th>Domain</th>
                      <th>Backup sets</th>
                      <th>Last snapshot</th>
                      <th>Maintenance</th>
                      <th>Clock</th>
                      <th>State</th>
                    </tr>
                  </thead>
                  <tbody>
                    {domains.map((record) => (
                      <DomainRow key={record.health.domain} record={record} />
                    ))}
                  </tbody>
                </table>
              </div>
              <Note>
                One row per <WireField name="repositories[].domain" /> from{" "}
                <WireField name="GET /repositories" />, with the owner read from{" "}
                <WireField name="GET /repositories/{domain}/maintenance" />.
              </Note>
            </div>
          </section>

          <section className="card">
            <div className="card__header">
              <InfoTooltip id="repositories.topology">
                <h2 className="eyebrow">Topology</h2>
              </InfoTooltip>
            </div>
            <div className="card__body">
              <div
                style={{
                  display: "grid",
                  gridTemplateColumns: "repeat(auto-fit, minmax(260px, 1fr))",
                  gap: 12
                }}
              >
                {domains.map(({ health }) => (
                  <div
                    key={health.domain}
                    style={{
                      border: health.mayShare
                        ? "1px solid var(--border-strong)"
                        : "1.5px solid var(--accent)",
                      borderRadius: "var(--radius-xl)",
                      background: "var(--surface-2)",
                      padding: 14
                    }}
                  >
                    <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                      <span className="mono" style={{ fontSize: 13, fontWeight: 600 }}>
                        {health.domain}
                      </span>
                      <IsolationBadge mayShare={health.mayShare} />
                    </div>
                    <ul
                      style={{
                        margin: "12px 0 0",
                        padding: 0,
                        listStyle: "none",
                        display: "flex",
                        flexDirection: "column",
                        gap: 6
                      }}
                    >
                      {health.backupSets.map((id) => (
                        <li
                          key={id}
                          className="mono"
                          style={{
                            fontSize: "var(--text-sm)",
                            padding: "6px 9px",
                            borderRadius: "var(--radius-md)",
                            background: "var(--surface)",
                            border: "1px solid var(--border)"
                          }}
                        >
                          {id}
                        </li>
                      ))}
                      {health.backupSets.length === 0 ? (
                        <li style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                          No backup set writes here yet.
                        </li>
                      ) : null}
                    </ul>
                    <p style={{ margin: "10px 0 0", fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                      {health.mayShare
                        ? health.backupSets.length > 1
                          ? health.backupSets.length +
                            " sets deduplicate against each other here, under one key."
                          : "Shared: another backup set may join this store and deduplicate against what is already in it."
                        : "One set only. A second set pointed here is refused rather than quietly admitted."}
                    </p>
                    {health.detail ? (
                      <p style={{ margin: "8px 0 0", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                        {health.detail}
                      </p>
                    ) : null}
                  </div>
                ))}
              </div>
              <Note>
                <WireField name="may_share" /> is what decides this: a domain that may not be shared
                holds exactly one backup set for as long as it exists, which is what makes it a
                blast radius of one.
              </Note>
            </div>
          </section>
        </>
      )}
    </>
  );
}

function DomainRow({ record }: { record: DomainRecord }) {
  const navigate = useNavigate();
  const { health, maintenance, maintenanceError } = record;
  const state = STATE_PRESENTATION[health.state];
  const probes = failingProbes(health);

  return (
    <tr>
      <td>
        <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
          <span className="mono">{health.domain}</span>
          <IsolationBadge mayShare={health.mayShare} />
        </div>
        {probes.length > 0 ? (
          <div style={{ marginTop: 3, fontSize: "var(--text-sm)", color: "var(--warn)" }}>
            {probes.join(" \u00b7 ")}
          </div>
        ) : null}
      </td>
      <td>
        <div style={{ display: "flex", flexDirection: "column", gap: 2, fontSize: "var(--text-sm)" }}>
          {health.backupSets.map((id) => {
            const [source = "", set = ""] = id.split("/");
            return (
              <button
                key={id}
                className="btn btn--quiet mono"
                style={{
                  height: "auto",
                  padding: 0,
                  border: "none",
                  background: "none",
                  color: "var(--accent)",
                  fontSize: "var(--text-sm)",
                  textAlign: "left"
                }}
                onClick={() => navigate(backupSetPath(source, set))}
              >
                {id}
              </button>
            );
          })}
          {health.backupSets.length === 0 ? (
            <span style={{ color: "var(--text-3)" }}>none</span>
          ) : null}
        </div>
      </td>
      <td className="mono">{relativeAge(health.lastSnapshotAt)}</td>
      <td style={{ fontSize: "var(--text-sm)" }}>
        {maintenanceError ? (
          <span style={{ color: "var(--warn)" }}>{maintenanceError.message}</span>
        ) : maintenance === null ? (
          <span style={{ color: "var(--text-3)" }}>—</span>
        ) : (
          <>
            <div>{maintenance.owner === "" ? "Nobody has claimed it" : "Owned by " + maintenance.owner}</div>
            <div style={{ color: "var(--text-3)" }}>
              {"full " + relativeAge(maintenance.lastFullAt) + (maintenance.overdue ? " \u00b7 overdue" : "")}
            </div>
          </>
        )}
      </td>
      <td className="mono" style={{ fontSize: "var(--text-sm)" }}>
        {clockSkew(health.clockSkewSeconds)}
      </td>
      <td>
        <StatusBadge tone={state.tone} icon={state.icon}>
          {state.label}
        </StatusBadge>
      </td>
    </tr>
  );
}

/** Shared or isolated, as a badge, because it is the one property of a
 *  domain that decides what may be done to it next. */
function IsolationBadge({ mayShare }: { mayShare: boolean }) {
  return mayShare ? (
    <InfoTooltip id="repositories.may-share">
      <StatusBadge tone="neutral" icon="backup-sets">
        Shared
      </StatusBadge>
    </InfoTooltip>
  ) : (
    <InfoTooltip id="repositories.may-share">
      <StatusBadge tone="accent" icon="quarantine">
        Isolated
      </StatusBadge>
    </InfoTooltip>
  );
}
