/**
 * What a new backup set starts with, and who maintains each repository
 * domain (EPIC K, issue #788).
 *
 * # Why a deployment-level page at all
 *
 * The mock-up's argument for it is that the per-set wizard should be six
 * honest questions rather than sixteen, which only works if the answers
 * nobody wants to give per set are given once. Two of those answers are
 * genuinely deployment-level on this contract — the retention chain and
 * the source-polling cadence — and one fact is deployment-level whether
 * anybody configures it or not: exactly one instance may maintain a
 * repository domain (ADR 0017), and this is the only screen where every
 * domain's owner is visible at once.
 *
 * # What this page will not pretend
 *
 * GET /settings declares retention, capacity and service behaviour. It
 * declares no default engine, no default repository domain, no default
 * consistency mode and no default verification budget, so this page
 * REPORTS what a new set starts from and does not offer to change it: a
 * form that saved nowhere is the exact defect issue #299 deleted three
 * controls for. The retention chain and the polling cadence are shown
 * here and edited on the Settings page, which owns their one editor —
 * two editors for one value is how two screens end up disagreeing about
 * what is configured.
 *
 * Maintenance ownership is the same shape one layer down: the contract
 * carries the owner's NAME and no route that changes it, and it carries no
 * identity for this deployment either, so this page can say who owns a
 * domain and cannot say whether that is us. Transfer is drawn, disabled,
 * with the reason attached, rather than left off the screen — an operator
 * looking for it needs to find out that it is not here yet, not that it
 * might be somewhere else.
 */
import { useNavigate } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import { useCausl } from "@shared/state/graph";
import { setsNode } from "@shared/state/appNodes";
import { PageHeader } from "@shared/components/PageHeader";
import { Banner } from "@shared/components/Banner";
import { WarningBanner } from "@shared/components/WarningBanner";
import { Cell, CellGrid, Note, Row, Rows, WireField } from "@shared/components/Definitions";
import { CONSISTENCY_COPY, ENGINE_COPY, EngineBadge, VERIFICATION_COPY } from "@shared/components/EngineBadge";
import { ErrorState } from "@shared/components/EmptyState";
import { StatusBadge } from "@shared/components/StatusBadge";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import { isNotConfigured } from "@shared/api/failure";
import { relativeAge, stamp } from "@shared/utilities/format";
import { NEW_SET_DEFAULTS } from "@shared/pages/incrementalConfigFields";
import { loadRepositoryFleet } from "@shared/pages/repositoryFleet";
import type { AppSettings } from "@shared/api/contracts";

export function BackupDefaultsPage({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const navigate = useNavigate();
  const settings = useAsync<AppSettings>(() => api.getSettings(), [api]);
  const fleet = useAsync(() => loadRepositoryFleet(api), [api]);
  const sets = useCausl(setsNode).data ?? [];

  const incrementalSets = sets.filter((set) => set.engine === "kopia");

  return (
    <>
      <PageHeader
        back={{ label: "Settings", onClick: () => navigate("/settings") }}
        title="Backup defaults"
        tip="defaults.page"
        subtitle="What a new backup set starts with, and who maintains each repository domain."
      />

      <section className="card">
        <div className="card__header">
          <InfoTooltip id="defaults.new-sets">
            <h2 className="eyebrow">New backup sets</h2>
          </InfoTooltip>
        </div>
        <div className="card__body">
          <WarningBanner
            tone="info"
            eyebrow="Answered per set, not per deployment"
            title="These are the answers Add backup set opens with"
            dismissible={false}
          >
            This contract carries no deployment-wide backup defaults:{" "}
            <WireField name="GET /settings" /> declares retention, capacity and service behaviour
            and nothing else. Every field below is chosen for each set in the wizard and changed
            afterwards on that set&rsquo;s own page, so what this card reports is where the wizard
            starts rather than a value stored anywhere.
          </WarningBanner>

          <div style={{ marginTop: 14 }}>
            <CellGrid min={200}>
              <Cell
                label="Engine"
                value={<EngineBadge engine={NEW_SET_DEFAULTS.engine} />}
                wire={"engine=" + ENGINE_COPY[NEW_SET_DEFAULTS.engine].wire}
                tip="wizard.incremental.engine"
                boxed
              />
              <Cell
                label="Repository domain"
                value={
                  fleet.data === null || fleet.data.domains.length === 0
                    ? "chosen on step 4"
                    : "chosen from " + fleet.data.domains.length + " declared"
                }
                wire="repository_domain"
                tip="wizard.incremental.domain"
                boxed
              />
              <Cell
                label="Source consistency"
                value={CONSISTENCY_COPY[NEW_SET_DEFAULTS.sourceConsistency].name}
                wire={"source_consistency=" + NEW_SET_DEFAULTS.sourceConsistency}
                tip="wizard.incremental.consistency"
                boxed
              />
              <Cell
                label="Verification level"
                value={VERIFICATION_COPY[NEW_SET_DEFAULTS.verificationLevel].name}
                wire={"verification_level=" + NEW_SET_DEFAULTS.verificationLevel}
                tip="wizard.incremental.verification"
                boxed
              />
              <Cell
                label="Sampled share of files"
                value={NEW_SET_DEFAULTS.samplePercent + "%"}
                wire="verification_sample_percent"
                mono
                boxed
              />
              <Cell
                label="Read every file every"
                value={NEW_SET_DEFAULTS.fullEveryDays + " days"}
                wire="verification_full_every_seconds"
                mono
                boxed
              />
              <Cell
                label="Restore drill every"
                value={NEW_SET_DEFAULTS.drillEveryDays + " days"}
                wire="verification_restore_drill_every_seconds"
                mono
                boxed
              />
              <Cell
                label="Check the source every"
                value={
                  settings.data === null
                    ? "loading…"
                    : Math.round(settings.data.service.pollIntervalSeconds / 60) + " min"
                }
                wire="poll_interval_seconds"
                mono
                boxed
              />
            </CellGrid>
          </div>
          <Note>
            The polling cadence IS a deployment value and is edited under Settings, on the Service
            behaviour card. A set may be given its own.
          </Note>
          <div style={{ marginTop: 12, display: "flex", gap: 9, flexWrap: "wrap" }}>
            <button className="btn" disabled={readOnly} onClick={() => navigate("/sets/new")}>
              Add backup set
            </button>
            <button className="btn" onClick={() => navigate("/settings")}>
              Edit service behaviour
            </button>
          </div>
        </div>
      </section>

      <section className="card">
        <div className="card__header">
          <InfoTooltip id="defaults.retention">
            <h2 className="eyebrow">Retention defaults</h2>
          </InfoTooltip>
        </div>
        <div className="card__body">
          {isNotConfigured(settings.error) ? (
            <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)" }}>
              The retention chain is part of a configuration this instance has not been given yet.
              It appears once the first backup set has been added.
            </p>
          ) : settings.error ? (
            <ErrorState
              message={settings.error.message}
              remediation="The deployment's retention policy could not be read."
              correlationId={settings.error.correlationId}
              onRetry={settings.reload}
            />
          ) : settings.data === null ? (
            <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>Loading retention policy…</p>
          ) : (
            <>
              <div className="table-scroll">
                <table className="table">
                  <thead>
                    <tr>
                      <th>Tier</th>
                      <th>Keeps</th>
                      <th>Granularity</th>
                      <th>Stored on</th>
                    </tr>
                  </thead>
                  <tbody>
                    {settings.data.retention.tiers.map((tier) => (
                      <tr key={tier.name}>
                        <td>{tier.name}</td>
                        <td className="mono">{tier.keep}</td>
                        <td className="mono">
                          {tier.granularity +
                            (tier.periodDays ? " \u00b7 " + tier.periodDays + " days" : "")}
                        </td>
                        <td className="mono">{tier.medium ?? "local"}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <div style={{ marginTop: 12 }}>
                <Rows>
                  <Row
                    label="Newest known-good backup"
                    value={
                      settings.data.retention.protectLastKnownGood ? (
                        <StatusBadge tone="ok" icon="success">
                          Never expired
                        </StatusBadge>
                      ) : (
                        <StatusBadge tone="warn" icon="warning">
                          Expires with the chain
                        </StatusBadge>
                      )
                    }
                  />
                  <Row
                    label="Timezone"
                    value={settings.data.retention.timezone + " \u00b7 week starts " + settings.data.retention.weekStartsOn}
                    mono
                  />
                </Rows>
              </div>
              <Note>
                A backup set may carry a chain of its own, on its own page. This one is what every
                set without an override is retained by, and it is edited under Settings, on the
                Retention plan card.
              </Note>
            </>
          )}
        </div>
      </section>

      <section className="card">
        <div className="card__header">
          <InfoTooltip id="repositories.maintenance-transfer">
            <h2 className="eyebrow">Maintenance ownership</h2>
          </InfoTooltip>
        </div>
        <div className="card__body">
          {fleet.error && !isNotConfigured(fleet.error) ? (
            <ErrorState
              message={fleet.error.message}
              remediation="The repository domains could not be read, so no ownership can be reported."
              correlationId={fleet.error.correlationId}
              onRetry={fleet.reload}
            />
          ) : fleet.data === null ? (
            <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
              Loading maintenance ownership…
            </p>
          ) : fleet.data.domains.length === 0 ? (
            <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)" }}>
              No repository domain is declared, so there is no maintenance to own. Only incremental
              backup sets use one.
            </p>
          ) : (
            <>
              <div className="table-scroll">
                <table className="table">
                  <thead>
                    <tr>
                      <th>Domain</th>
                      <th>Owner</th>
                      <th>Last full</th>
                      <th>Due</th>
                      <th />
                    </tr>
                  </thead>
                  <tbody>
                    {fleet.data.domains.map(({ health, maintenance, maintenanceError }) => (
                      <tr key={health.domain}>
                        <td className="mono">{health.domain}</td>
                        <td style={{ fontSize: "var(--text-sm)" }}>
                          {maintenanceError ? (
                            <span style={{ color: "var(--warn)" }}>{maintenanceError}</span>
                          ) : maintenance === null ? (
                            "\u2014"
                          ) : maintenance.owner === "" ? (
                            <StatusBadge tone="warn" icon="warning">
                              Nobody has claimed it
                            </StatusBadge>
                          ) : (
                            <span className="mono">{maintenance.owner}</span>
                          )}
                        </td>
                        <td className="mono" style={{ fontSize: "var(--text-sm)" }}>
                          {relativeAge(maintenance?.lastFullAt ?? null)}
                        </td>
                        <td style={{ fontSize: "var(--text-sm)" }}>
                          {maintenance === null
                            ? "\u2014"
                            : maintenance.overdue
                              ? "Overdue \u2014 " + (maintenance.dueReason || "no reason given")
                              : maintenance.due
                                ? "Due now (" + (maintenance.dueMode || "maintenance") + ")"
                                : // A DATE, not an age. `next_eligible_at` is in
                                  // the future for a domain that is not due, and
                                  // an age renders a future instant as "just now"
                                  // or, once the window has opened without a run,
                                  // as "8 hours ago" under the word "Next".
                                  maintenance.nextEligibleAt === null
                                  ? "Not scheduled"
                                  : "Eligible from " + stamp(maintenance.nextEligibleAt)}
                        </td>
                        <td>
                          <div style={{ display: "flex", gap: 6, justifyContent: "flex-end" }}>
                            <button
                              className="btn btn--sm"
                              onClick={() => navigate("/repositories/maintenance")}
                            >
                              Maintenance
                            </button>
                            <InfoTooltip id="repositories.maintenance-transfer" alignEnd>
                              <button
                                className="btn btn--sm btn--caution"
                                disabled
                                title="No API route transfers maintenance ownership yet"
                              >
                                Transfer ownership…
                              </button>
                            </InfoTooltip>
                          </div>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <Banner tone="info" dismissible={false} style={{ fontSize: "var(--text-sm)", marginTop: 12 }}>
                <span>
                  Ownership moves by transfer, never by claim: two instances compacting one store at
                  once is what that rule exists to prevent. The transfer itself is not on this API
                  yet — <WireField name="/repositories/{domain}/maintenance" /> is a GET and there is
                  no action for it on <WireField name="POST /operations" /> — so the control above is
                  drawn and refused rather than hidden. The owner is named as the service reports it;
                  nothing on the wire tells this interface which instance it is itself, so no row
                  claims to be you.
                </span>
              </Banner>
            </>
          )}
        </div>
      </section>

      <section className="card">
        <div className="card__header">
          <h2 className="eyebrow">What this deployment runs today</h2>
        </div>
        <div className="card__body">
          <CellGrid min={180}>
            <Cell label="Incremental sets" value={String(incrementalSets.length)} boxed />
            <Cell label="Artifact sets" value={String(sets.length - incrementalSets.length)} boxed />
            <Cell
              label="Repository domains"
              value={fleet.data === null ? "\u2014" : String(fleet.data.domains.length)}
              boxed
            />
          </CellGrid>
          <div style={{ marginTop: 14 }}>
            <Rows>
              {sets.map((set) => (
                <Row
                  key={set.id}
                  label={set.id}
                  value={
                    <span style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                      <EngineBadge engine={set.engine} />
                      <span className="mono" style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                        {set.incremental?.repositoryDomain ?? "no repository domain"}
                      </span>
                    </span>
                  }
                />
              ))}
            </Rows>
            {sets.length === 0 ? (
              <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
                No backup set is configured yet.
              </p>
            ) : null}
          </div>
        </div>
      </section>
    </>
  );
}
