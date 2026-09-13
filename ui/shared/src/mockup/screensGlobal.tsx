/**
 * The deployment-level half of the mock-up (issue #788): the screens an
 * operator visits once, or once a quarter, rather than per backup set.
 *
 * Three of them, and the split is the argument. A repository domain is a
 * security boundary shared by several sets, so it cannot be configured
 * from inside any one of them; maintenance has exactly one owner per
 * domain (ADR 0017), which is a deployment fact and not a set's; and the
 * defaults exist so that the per-set wizard can be six honest questions
 * instead of sixteen.
 */
import { useState } from "react";
import { Banner } from "@shared/components/Banner";
import { PageHeader } from "@shared/components/PageHeader";
import { PasswordInput } from "@shared/components/PasswordInput";
import { StatusBadge } from "@shared/components/StatusBadge";
import { WarningBanner } from "@shared/components/WarningBanner";
import { bytes } from "@shared/utilities/format";
import {
  CONSISTENCY_COPY,
  DOMAINS,
  DOMAIN_BOUNDARIES,
  ENGINE_COPY,
  MAINTENANCE,
  RETENTION_TIERS,
  SETS,
  TOTALS,
  VERIFICATION_COPY
} from "@shared/mockup/data";
import type { MockIsolation } from "@shared/mockup/data";
import {
  Cell,
  CellGrid,
  Choice,
  DOT,
  EngineBadge,
  Field,
  FormGrid,
  MockCard,
  Note,
  Row,
  Rows,
  Select,
  Toggle,
  WireName
} from "@shared/mockup/parts";

/** The passphrase field's label id. A constant rather than useId because
 *  there is exactly one of these fields on one screen, and a stable id is
 *  what a screenshot review can point at. */
const PASSPHRASE_LABEL_ID = "mockup-domain-passphrase";

function IsolationBadge({ isolation }: { isolation: MockIsolation }) {
  return isolation === "isolated" ? (
    <StatusBadge tone="accent" icon="quarantine">
      Isolated
    </StatusBadge>
  ) : (
    <StatusBadge tone="neutral" icon="backup-sets">
      Shared
    </StatusBadge>
  );
}

/** Screen 1 — every repository domain this deployment has, what is in it,
 *  and who maintains it. */
export function DomainsScreen() {
  return (
    <>
      <PageHeader
        title="Repository domains"
        subtitle={
          TOTALS.domains +
          " domains" +
          DOT +
          TOTALS.snapshots.toLocaleString() +
          " snapshots" +
          DOT +
          bytes(TOTALS.physicalBytes) +
          " stored"
        }
        actions={
          <>
            <button className="btn">Run maintenance</button>
            <button className="btn btn--primary">Define repository domain</button>
          </>
        }
      />

      <Banner tone="info" dismissible={false}>
        <span style={{ fontSize: 13 }}>
          A repository domain is one encrypted store. Backup sets that share one store the content
          they have in common only once, and share its key, its credential, its maintenance and its
          blast radius with each other.
        </span>
      </Banner>

      <MockCard title="Domains">
        <div className="table-scroll">
          <table className="table">
            <thead>
              <tr>
                <th>Domain</th>
                <th>Backup sets</th>
                <th>Snapshots</th>
                <th>Stored</th>
                <th>Maintenance</th>
                <th>State</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {DOMAINS.map((domain) => (
                <tr key={domain.id}>
                  <td>
                    <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                      <span className="mono">{domain.id}</span>
                      <IsolationBadge isolation={domain.isolation} />
                    </div>
                    <div style={{ marginTop: 3, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                      {domain.description}
                    </div>
                  </td>
                  <td>
                    <div style={{ display: "flex", flexDirection: "column", gap: 2, fontSize: "var(--text-sm)" }}>
                      {domain.sets.map((id) => (
                        <span key={id} className="mono">
                          {id}
                        </span>
                      ))}
                    </div>
                  </td>
                  <td className="mono">{domain.snapshots.toLocaleString()}</td>
                  <td className="mono">{bytes(domain.physicalBytes)}</td>
                  <td style={{ fontSize: "var(--text-sm)" }}>{domain.maintenanceOwner}</td>
                  <td>
                    {domain.health === "ok" ? (
                      <StatusBadge tone="ok" icon="status-active">
                        Healthy
                      </StatusBadge>
                    ) : (
                      <StatusBadge tone="warn" icon="warning">
                        Attention
                      </StatusBadge>
                    )}
                  </td>
                  <td>
                    <div style={{ display: "flex", gap: 6, justifyContent: "flex-end" }}>
                      <button className="btn btn--sm">Health</button>
                      <button className="btn btn--sm">Maintenance</button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <Note>
          Read from <WireName name="GET /api/v1/repositories" />, one row per{" "}
          <WireName name="repositories[].domain" />.
        </Note>
      </MockCard>

      <MockCard title="Topology">
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(260px, 1fr))", gap: 12 }}>
          {DOMAINS.map((domain) => (
            <div
              key={domain.id}
              style={{
                border:
                  domain.isolation === "isolated"
                    ? "1.5px solid var(--accent)"
                    : "1px solid var(--border-strong)",
                borderRadius: "var(--radius-xl)",
                background: "var(--surface-2)",
                padding: 14
              }}
            >
              <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                <span className="mono" style={{ fontSize: 13, fontWeight: 600 }}>
                  {domain.id}
                </span>
                <IsolationBadge isolation={domain.isolation} />
              </div>
              <div style={{ marginTop: 4, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                {domain.location}
              </div>
              <ul style={{ margin: "12px 0 0", padding: 0, listStyle: "none", display: "flex", flexDirection: "column", gap: 6 }}>
                {domain.sets.map((id) => (
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
              </ul>
              <div style={{ marginTop: 10, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                {domain.isolation === "isolated"
                  ? "One set only. A second set here would be refused."
                  : domain.sets.length + " sets deduplicate against each other here."}
              </div>
            </div>
          ))}
        </div>
      </MockCard>
    </>
  );
}

/** Screen 2 — defining a domain, which is where the co-tenancy decision
 *  is actually made and therefore where it has to be stated. */
export function DomainNewScreen() {
  const [isolation, setIsolation] = useState<MockIsolation>("shared");
  const [owner, setOwner] = useState("this");

  return (
    <>
      <PageHeader
        back={{ label: "Cancel and return to repository domains", onClick: () => undefined }}
        title="Define a repository domain"
        subtitle="A new encrypted store that backup sets can be pointed at."
      />

      <MockCard title="Identity">
        <FormGrid>
          <Field label="Domain id" value="offsite-b2" wire="domain" mono />
          <Field label="Description" value="Second copy, off site" />
          <Field label="Storage location" value="b2://acme-backups/primary" mono />
          {/* PasswordInput takes the id of the label it belongs to rather
              than wrapping itself in one, because the reveal control
              inside it would otherwise contribute its own name to the
              field's. */}
          <label className="field">
            <span className="field__label" id={PASSPHRASE_LABEL_ID}>
              Encryption passphrase
            </span>
            <PasswordInput
              label="Encryption passphrase"
              labelledBy={PASSPHRASE_LABEL_ID}
              value="correct-horse-battery-staple"
              onChange={() => undefined}
              autoComplete="new-password"
            />
          </label>
        </FormGrid>
        <Note>
          The passphrase is stored on this NAS and never displayed again. Losing it loses every
          snapshot in the domain: nothing else can open the store.
        </Note>
      </MockCard>

      <MockCard title="Sharing">
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(280px, 1fr))", gap: 10 }}>
          <Choice
            name="isolation"
            title="Shared"
            wire="isolation=shared"
            detail="Several backup sets may store snapshots here and deduplicate against each other."
            checked={isolation === "shared"}
            onChange={() => setIsolation("shared")}
          />
          <Choice
            name="isolation"
            title="Isolated"
            wire="isolation=isolated"
            detail="Exactly one backup set. A second set pointed here is refused rather than quietly admitted."
            checked={isolation === "isolated"}
            onChange={() => setIsolation("isolated")}
          />
        </div>

        <WarningBanner
          tone={isolation === "shared" ? "warn" : "info"}
          eyebrow="What sharing means"
          title={
            isolation === "shared"
              ? "Sets in this domain share all six of these"
              : "An isolated domain shares none of these with any other set"
          }
          dismissible={false}
        >
          <ul style={{ margin: "6px 0 0", paddingLeft: 18, fontSize: 13, display: "flex", flexDirection: "column", gap: 3 }}>
            {DOMAIN_BOUNDARIES.map((boundary) => (
              <li key={boundary.title}>
                <strong>{boundary.title}</strong>
                {" — " + boundary.detail}
              </li>
            ))}
          </ul>
        </WarningBanner>
      </MockCard>

      <MockCard title="Maintenance ownership" tip="repositories.maintenance-owner">
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(280px, 1fr))", gap: 10 }}>
          <Choice
            name="owner"
            title="This instance maintains it"
            detail="nas-01 compacts and reclaims storage for this domain on the schedule below."
            checked={owner === "this"}
            onChange={() => setOwner("this")}
          />
          <Choice
            name="owner"
            title="Another instance maintains it"
            detail="This instance reads and writes snapshots but never maintains the store."
            checked={owner === "other"}
            onChange={() => setOwner("other")}
          />
        </div>
        <div style={{ marginTop: 14 }}>
          <FormGrid>
            <Select
              label="Full maintenance window"
              value="weekly-sun"
              wire="maintenance window"
              options={[
                { value: "weekly-sun", label: "Weekly, Sunday 02:00" },
                { value: "weekly-wed", label: "Weekly, Wednesday 02:00" },
                { value: "monthly", label: "Monthly, first Sunday 02:00" }
              ]}
              onChange={() => undefined}
            />
            <Select
              label="Quick maintenance"
              value="after-pass"
              options={[
                { value: "after-pass", label: "After every backup pass" },
                { value: "daily", label: "Once a day" }
              ]}
              onChange={() => undefined}
            />
          </FormGrid>
        </div>
        <Note>
          Exactly one instance may maintain a domain. Ownership moves by transfer, never by claim:
          an instance that simply decided it was the owner is how two of them compact one store at
          once.
        </Note>
      </MockCard>

      <div style={{ display: "flex", gap: 10, justifyContent: "flex-end" }}>
        <button className="btn">Cancel</button>
        <button className="btn btn--primary">Create domain</button>
      </div>
    </>
  );
}

/** Screen 3 — the deployment's defaults, plus the maintenance ownership
 *  table, which is the one place every domain's owner is visible at once. */
export function DefaultsScreen() {
  const [engine, setEngine] = useState<"artifact" | "kopia">("kopia");

  return (
    <>
      <PageHeader
        title="Backup defaults"
        subtitle="What a new backup set starts with. Every one of these can be overridden per set."
        actions={<button className="btn btn--primary">Save defaults</button>}
      />

      <MockCard title="New backup sets" tip="wizard.incremental.engine">
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(280px, 1fr))", gap: 10 }}>
          <Choice
            name="default-engine"
            title={ENGINE_COPY.artifact.name}
            wire="engine=artifact"
            detail={ENGINE_COPY.artifact.summary}
            checked={engine === "artifact"}
            onChange={() => setEngine("artifact")}
          />
          <Choice
            name="default-engine"
            title={ENGINE_COPY.kopia.name}
            wire="engine=kopia"
            detail={ENGINE_COPY.kopia.summary}
            checked={engine === "kopia"}
            onChange={() => setEngine("kopia")}
          />
        </div>

        <div style={{ marginTop: 16 }}>
          <FormGrid>
            <Select
              label="Default repository domain"
              wire="repository_domain"
              value="primary-nas"
              options={DOMAINS.map((domain) => ({ value: domain.id, label: domain.id }))}
              onChange={() => undefined}
              tip="wizard.incremental.domain"
            />
            <Select
              label="Default source consistency"
              wire="source_consistency"
              value="live_best_effort"
              options={Object.values(CONSISTENCY_COPY).map((copy) => ({ value: copy.wire, label: copy.name }))}
              onChange={() => undefined}
              tip="wizard.incremental.consistency"
            />
            <Select
              label="Default verification level"
              wire="verification_level"
              value="content_sample"
              options={Object.values(VERIFICATION_COPY).map((copy) => ({ value: copy.wire, label: copy.name }))}
              onChange={() => undefined}
              tip="wizard.incremental.verification"
            />
            <Field label="Sampled share of files" value="5%" wire="verification_sample_percent" />
            <Field label="Read every file every" value="7 days" wire="verification_full_every_seconds" />
            <Field label="Restore drill every" value="30 days" wire="verification_restore_drill_every_seconds" />
            <Field label="Check the source every" value="6 hours" wire="poll_interval" />
          </FormGrid>
        </div>
      </MockCard>

      <MockCard title="Retention defaults">
        <div className="table-scroll">
          <table className="table">
            <thead>
              <tr>
                <th>Tier</th>
                <th>Keeps</th>
                <th>Currently holding</th>
                <th>Next expiry</th>
              </tr>
            </thead>
            <tbody>
              {RETENTION_TIERS.map((tier) => (
                <tr key={tier.name}>
                  <td>{tier.name}</td>
                  <td className="mono">{tier.keep}</td>
                  <td className="mono">{tier.kept}</td>
                  <td className="mono">{tier.nextExpiry}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <div style={{ marginTop: 12 }}>
          <Toggle
            label="Never expire the newest verified backup of a set"
            note="recommended"
            checked
            onChange={() => undefined}
          />
        </div>
      </MockCard>

      <MockCard title="Maintenance ownership" tip="repositories.maintenance-owner">
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
              {MAINTENANCE.map((row) => (
                <tr key={row.domain}>
                  <td className="mono">{row.domain}</td>
                  <td>{row.owner}</td>
                  <td className="mono">{row.lastFull}</td>
                  <td className="mono">{row.due}</td>
                  <td>
                    <div style={{ display: "flex", gap: 6, justifyContent: "flex-end" }}>
                      <button className="btn btn--sm" disabled={!row.ownedHere}>
                        Run now
                      </button>
                      <button className="btn btn--sm btn--caution">Transfer ownership…</button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <Note>
          A domain another instance owns is not maintained from here, and the button says so rather
          than failing after it is pressed.
        </Note>
      </MockCard>

      <MockCard title="What this deployment runs today">
        <CellGrid min={180}>
          <Cell label="Incremental sets" value={String(TOTALS.incrementalSets)} tone="quiet" />
          <Cell label="Artifact sets" value={String(TOTALS.artifactSets)} tone="quiet" />
          <Cell label="Repository domains" value={String(TOTALS.domains)} tone="quiet" />
          <Cell label="Stored" value={bytes(TOTALS.physicalBytes)} tone="quiet" />
        </CellGrid>
        <div style={{ marginTop: 14 }}>
          <Rows>
            {SETS.map((set) => (
              <Row
                key={set.source + "/" + set.set}
                label={set.source + "/" + set.set}
                value={
                  <span style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                    <EngineBadge engine={set.engine} />
                    <span className="mono" style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                      {set.domain ?? "no repository domain"}
                    </span>
                  </span>
                }
              />
            ))}
          </Rows>
        </div>
      </MockCard>
    </>
  );
}
