/**
 * The per-backup-set half of the mock-up (issue #788), and the contrast
 * the whole epic turns on: the same page, drawn for an incremental set and
 * for an artifact set, so a reviewer can see at a glance that the two are
 * never mistaken for each other.
 *
 * Three screens. The configuration form is what an operator returns to
 * after the wizard; the two detail pages are what they look at every other
 * day. The detail pages deliberately share their chrome — header, engine
 * badge, metric strip, cards — and differ in the metrics themselves,
 * because "how much did this run read, write and reuse" is a question only
 * one of the two engines can answer, and inventing a plausible number for
 * the other is exactly the lie #783's four byte counts exist to prevent.
 *
 * Issue #852 lands here too: "delete from source after a verified backup"
 * is refused, visibly and with its reason attached, when the connection
 * test could not prove these credentials can write to the source.
 */
import { useState } from "react";
import { PageHeader } from "@shared/components/PageHeader";
import { MetricCard } from "@shared/components/MetricCard";
import { StatusBadge } from "@shared/components/StatusBadge";
import { WarningBanner } from "@shared/components/WarningBanner";
import { bytes } from "@shared/utilities/format";
import {
  ARTIFACT_SET,
  CONNECTION_CHECKS_READ_ONLY,
  CONNECTION_CHECKS_WRITABLE,
  CONSISTENCY_COPY,
  DOMAINS,
  ENGINE_COPY,
  INCREMENTAL_SET,
  RETENTION_TIERS,
  SNAPSHOTS,
  SOURCE_DELETE_COPY,
  VERIFICATION_COPY
} from "@shared/mockup/data";
import type { MockConsistencyMode, MockEngine, MockVerificationLevel } from "@shared/mockup/data";
import { measured } from "@shared/mockup/format";
import {
  Cell,
  CellGrid,
  CheckList,
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
  VerificationBadge,
  WireName
} from "@shared/mockup/parts";

/**
 * The source-deletion control, in both of the states #852 defines, in one
 * component so the two cannot drift apart.
 *
 * `writable` is the connection test's answer and nothing else — not a
 * preference, not a capability flag an operator can set. A control that
 * deletes somebody's files is armed by evidence that the deletion would
 * work, and by nothing softer than that.
 */
function SourceDeleteControl({ writable, checked, onChange }: { writable: boolean; checked: boolean; onChange(): void }) {
  return (
    <Toggle
      label={SOURCE_DELETE_COPY.label}
      note={writable ? SOURCE_DELETE_COPY.note : SOURCE_DELETE_COPY.readOnlyNote}
      checked={writable ? checked : false}
      onChange={writable ? onChange : undefined}
      disabled={!writable}
      tip={writable ? "sets.source-delete" : "sets.source-delete.read-only"}
    />
  );
}

/** The write-probe verdict as a banner, so the refusal below it is never
 *  the first the operator hears of the reason. */
function WritePermissionNotice({ writable }: { writable: boolean }) {
  return writable ? (
    <WarningBanner
      tone="ok"
      eyebrow="Connection test"
      title="These credentials can write to the source"
      dismissible={false}
    >
      A scratch file was created under /srv/shares and deleted again, so Backupd is able to remove
      files there once a backup of them is stored and verified.
    </WarningBanner>
  ) : (
    <WarningBanner
      tone="warn"
      eyebrow="Connection test"
      title="These credentials are read-only on the source"
      dismissible={false}
      actions={<button className="btn btn--sm">Re-run connection test</button>}
    >
      Backups run normally. Deleting from the source is switched off and cannot be turned on until
      the account can write to the source path.
    </WarningBanner>
  );
}

/** Screen 4 — the per-set configuration form, incremental. */
export function SetConfigScreen() {
  const set = INCREMENTAL_SET;
  const [engine, setEngine] = useState<MockEngine>(set.engine);
  const [consistency, setConsistency] = useState<MockConsistencyMode>(set.consistency);
  const [level, setLevel] = useState<MockVerificationLevel>(set.verification);
  const [writable, setWritable] = useState(true);
  const [deleteFromSource, setDeleteFromSource] = useState(false);

  return (
    <>
      <PageHeader
        back={{ label: "Back to " + set.source + "/" + set.set, onClick: () => undefined }}
        title={"Configure " + set.source + "/" + set.set}
        subtitle={
          <span style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
            <EngineBadge engine={engine} />
            <span>{ENGINE_COPY[engine].summary}</span>
          </span>
        }
        actions={
          <>
            <button className="btn">Discard changes</button>
            <button className="btn btn--primary">Save</button>
          </>
        }
      />

      {/* The reviewer's control, not an operator's. Both states of #852's
          affordance have to be reachable in a static mock-up, and a
          fabricated switch is more honest about that than two screenshots
          nobody can get back to. */}
      <div
        style={{
          display: "flex",
          alignItems: "center",
          gap: 10,
          padding: "9px 13px",
          border: "1px dashed var(--border-strong)",
          borderRadius: "var(--radius-lg)",
          background: "var(--surface-2)",
          fontSize: "var(--text-sm)",
          color: "var(--text-2)"
        }}
      >
        <span style={{ flex: 1 }}>
          Mock-up control: what the connection test last found about write permission on the source.
        </span>
        <button className="btn btn--sm" onClick={() => setWritable(true)} disabled={writable}>
          Writable
        </button>
        <button className="btn btn--sm" onClick={() => setWritable(false)} disabled={!writable}>
          Read-only
        </button>
      </div>

      <MockCard title="Engine" tip="wizard.incremental.engine">
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(280px, 1fr))", gap: 10 }}>
          <Choice
            name="engine"
            title={ENGINE_COPY.artifact.name}
            wire="engine=artifact"
            detail={ENGINE_COPY.artifact.summary}
            checked={engine === "artifact"}
            onChange={() => setEngine("artifact")}
          />
          <Choice
            name="engine"
            title={ENGINE_COPY.kopia.name}
            wire="engine=kopia"
            detail={ENGINE_COPY.kopia.summary}
            checked={engine === "kopia"}
            onChange={() => setEngine("kopia")}
          />
        </div>
        <Note>
          Fixed once the set has run. Changing the engine of a set with history would start a second,
          unrelated one: the snapshots already stored would still exist, and nothing new would ever
          be added to them.
        </Note>
      </MockCard>

      <MockCard title="Source" >
        <FormGrid>
          <Field label="Server" value="prod-files-01.internal" mono />
          <Field label="Username" value="backup-agent" mono />
          <Field label="Directory to snapshot" value={set.root} wire="source path" mono />
          <Field label="Ignore paths matching" value="*.tmp, /srv/shares/scratch" mono />
        </FormGrid>

        <div style={{ marginTop: 16, display: "flex", flexDirection: "column", gap: 12 }}>
          <WritePermissionNotice writable={writable} />
          <SourceDeleteControl
            writable={writable}
            checked={deleteFromSource}
            onChange={() => setDeleteFromSource(!deleteFromSource)}
          />
          <details>
            <summary style={{ cursor: "pointer", fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
              What the last connection test found
            </summary>
            <div style={{ marginTop: 10 }}>
              <CheckList checks={writable ? CONNECTION_CHECKS_WRITABLE : CONNECTION_CHECKS_READ_ONLY} />
            </div>
          </details>
        </div>
      </MockCard>

      <MockCard title="Repository domain" tip="wizard.incremental.domain">
        <FormGrid>
          <Select
            label="Store snapshots in"
            wire="repository_domain"
            value={set.domain ?? ""}
            options={DOMAINS.map((domain) => ({
              value: domain.id,
              label: domain.id + " (" + domain.isolation + ")"
            }))}
            onChange={() => undefined}
          />
          <Field label="Set identity in the repository" value={set.source + "/" + set.set} wire="uuid" mono />
        </FormGrid>
        <Note>
          Fixed after creation, for the same reason the engine is: snapshots live in the domain they
          were written to, and pointing the set somewhere else would leave its history behind.
        </Note>
      </MockCard>

      <MockCard title="Source consistency" tip="wizard.incremental.consistency">
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(260px, 1fr))", gap: 10 }}>
          {Object.entries(CONSISTENCY_COPY).map(([mode, copy]) => (
            <Choice
              key={mode}
              name="consistency"
              title={copy.name}
              wire={"source_consistency=" + copy.wire}
              detail={copy.summary}
              checked={consistency === (mode as MockConsistencyMode)}
              onChange={() => setConsistency(mode as MockConsistencyMode)}
            />
          ))}
        </div>
        <Note>
          {"Point in time: " + CONSISTENCY_COPY[consistency].pointInTime + DOT +
            "Backupd records what you arranged and reports a run that contradicts it. It never guesses which mode you are in."}
        </Note>
      </MockCard>

      <MockCard title="Verification" tip="wizard.incremental.verification">
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(260px, 1fr))", gap: 10 }}>
          {Object.entries(VERIFICATION_COPY).map(([value, copy]) => (
            <Choice
              key={value}
              name="verification"
              title={copy.name}
              wire={"verification_level=" + copy.wire}
              detail={copy.finds}
              checked={level === (value as MockVerificationLevel)}
              onChange={() => setLevel(value as MockVerificationLevel)}
            >
              <span style={{ display: "block", marginTop: 4, fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
                {copy.cost}
              </span>
            </Choice>
          ))}
        </div>
        <div style={{ marginTop: 16 }}>
          <FormGrid>
            <Field label="Sampled share of files" value="5%" wire="verification_sample_percent" />
            <Field label="Read every file every" value={set.fullEvery} wire="verification_full_every_seconds" />
            <Field label="Restore drill every" value={set.drillEvery} wire="verification_restore_drill_every_seconds" />
          </FormGrid>
        </div>
        <Note>
          A run that proves less than the level above fails, and only a run that proves it can become
          this set&rsquo;s newest known-good restore point. The two cadences may raise the bar for one
          run; nothing lowers it.
        </Note>
      </MockCard>

      <MockCard title="Schedule">
        <FormGrid>
          <Select
            label="Check the source every"
            wire="poll_interval"
            value="6h"
            options={[
              { value: "30m", label: "30 minutes" },
              { value: "6h", label: "6 hours" },
              { value: "12h", label: "12 hours" },
              { value: "24h", label: "24 hours" }
            ]}
            onChange={() => undefined}
          />
          <Field label="Next run" value="today, 10:00" />
        </FormGrid>
      </MockCard>

      <MockCard
        title="Retention and holds"
        actions={<button className="btn btn--sm">Open retention view</button>}
      >
        <Rows>
          <Row label="Chain" value={RETENTION_TIERS.map((tier) => tier.name + " " + tier.keep).join(DOT)} />
          <Row label="Inherited from" value="deployment defaults" />
          <Row
            label="Holds in force"
            value={
              <span style={{ display: "inline-flex", alignItems: "center", gap: 8 }}>
                <StatusBadge tone="accent" icon="quarantine">
                  {set.holds + " snapshots held"}
                </StatusBadge>
                <span style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                  A held snapshot is never expired by retention.
                </span>
              </span>
            }
          />
        </Rows>
      </MockCard>
    </>
  );
}

/** Screen 5 — an incremental set's detail page. */
export function SetDetailIncrementalScreen() {
  const set = INCREMENTAL_SET;
  const newest = SNAPSHOTS[0];

  return (
    <>
      <PageHeader
        back={{ label: "Backup sets", onClick: () => undefined }}
        title={
          <span style={{ display: "inline-flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
            {set.source + "/" + set.set}
            <EngineBadge engine={set.engine} />
          </span>
        }
        subtitle={
          "Snapshots of " + set.root + DOT + "repository domain " + set.domain + DOT + set.pollInterval
        }
        actions={
          <>
            <button className="btn">Verify now</button>
            <button className="btn">Restore…</button>
            <button className="btn btn--primary">Back up now</button>
          </>
        }
      />

      <section className="card" aria-label="Last snapshot">
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(196px, 1fr))" }}>
          <MetricCard
            label="Entries scanned"
            value={measured(newest.entries, (n) => n.toLocaleString())}
            detail="files and directories"
          />
          <MetricCard label="Logical size" value={measured(newest.logicalBytes, bytes)} detail="as the source describes it" />
          <MetricCard label="Read from source" value={measured(newest.sourceReadBytes, bytes)} detail="this run" />
          <MetricCard label="Written to repository" value={measured(newest.writtenBytes, bytes)} detail="after deduplication" />
          <MetricCard
            label="Reused"
            tip="snapshots.reused"
            value={measured(newest.reusedBytes, bytes)}
            detail={newest.reusedBytes === null ? "the engine could not account for it" : "already in the repository"}
          />
          <MetricCard
            label="Duration"
            value={measured(newest.durationSeconds, (n) => Math.round(n / 60) + " min")}
            detail="2 hours ago"
          />
        </div>
      </section>

      <MockCard
        title="Latest snapshot"
        actions={
          <>
            <button className="btn btn--sm">Inspect</button>
            <button className="btn btn--sm">Place hold…</button>
          </>
        }
      >
        <Rows>
          <Row label="Snapshot" wire="snapshot_id" value={newest.id} mono />
          <Row label="Run" wire="run_id" value="run-2026-09-13-0400-file-server" mono />
          <Row label="Taken" value="today at 04:00, finished in 3 min 34 s" />
          <Row
            label="Verification"
            wire="verification_level_achieved"
            tip="snapshots.achieved-level"
            value={
              <span style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                <VerificationBadge
                  status={newest.verification}
                  achieved={newest.achievedLevel ? VERIFICATION_COPY[newest.achievedLevel].name : null}
                />
                <span style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                  {"asked for " + VERIFICATION_COPY[set.verification].name}
                </span>
              </span>
            }
          />
          <Row
            label="Restore point"
            value={
              <StatusBadge tone="ok" icon="success">
                Newest known-good
              </StatusBadge>
            }
          />
          <Row label="Consistency" wire="consistency_mode" value={CONSISTENCY_COPY[set.consistency].name} />
        </Rows>
      </MockCard>

      <MockCard title="Configuration" actions={<button className="btn btn--sm">Edit</button>}>
        <CellGrid>
          <Cell label="Engine" value={ENGINE_COPY[set.engine].name} wire="engine=kopia" tone="quiet" />
          <Cell label="Repository domain" value={set.domain ?? ""} wire="repository_domain" mono tone="quiet" />
          <Cell
            label="Source consistency"
            value={CONSISTENCY_COPY[set.consistency].name}
            wire="source_consistency"
            tone="quiet"
          />
          <Cell
            label="Verification level"
            value={VERIFICATION_COPY[set.verification].name}
            wire="verification_level"
            tone="quiet"
          />
          <Cell label="Schedule" value={set.pollInterval} wire="poll_interval" tone="quiet" />
          <Cell label="Snapshots kept" value={String(set.snapshots)} tone="quiet" />
        </CellGrid>
      </MockCard>

      <MockCard title="Connection" actions={<button className="btn btn--sm">Test connection</button>}>
        <CheckList checks={CONNECTION_CHECKS_WRITABLE} />
        <Note>
          The write line is the <WireName name="write_probe" /> step, and its{" "}
          <WireName name="writable" /> answer is what arms
          &ldquo;{SOURCE_DELETE_COPY.label}&rdquo; on this set&rsquo;s configuration page. A source
          Backupd cannot write to is never offered a control that deletes from it, and asking for one
          anyway is refused by the service rather than quietly ignored.
        </Note>
      </MockCard>
    </>
  );
}

/** Screen 6 — the same page for an artifact set, which is what the
 *  incremental one has to be distinguishable from. */
export function SetDetailArtifactScreen() {
  const set = ARTIFACT_SET;

  return (
    <>
      <PageHeader
        back={{ label: "Backup sets", onClick: () => undefined }}
        title={
          <span style={{ display: "inline-flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
            {set.source + "/" + set.set}
            <EngineBadge engine={set.engine} />
          </span>
        }
        subtitle={"Pulls finished files from " + set.root + DOT + set.pollInterval}
        actions={
          <>
            <button className="btn">Test connection</button>
            <button className="btn btn--primary">Run now</button>
          </>
        }
      />

      <WarningBanner tone="info" eyebrow="Different engine" title="This set keeps whole files, not snapshots">
        There is no repository domain, no snapshot list and no restore point here: each backup is one
        file this set pulled, kept whole and verified on arrival. The pages that ask about
        deduplication, reuse or held snapshots do not apply to it.
      </WarningBanner>

      <section className="card" aria-label="Last cycle">
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(196px, 1fr))" }}>
          <MetricCard label="Backups held" value="1,204" detail="whole files" />
          <MetricCard label="Pulled last cycle" value="6" detail="new artifacts" />
          <MetricCard label="Transferred" value={bytes(18 * 1024 ** 3)} detail="last cycle" />
          <MetricCard label="Quarantined" value="0" detail="failed validation" />
          <MetricCard label="Duration" value="4 min" detail="30 minutes ago" />
        </div>
      </section>

      <MockCard title="Configuration" actions={<button className="btn btn--sm">Edit</button>}>
        <CellGrid>
          <Cell label="Engine" value={ENGINE_COPY[set.engine].name} wire="engine=artifact" tone="quiet" />
          <Cell label="Repository domain" value="not applicable" wire="repository_domain absent" tone="quiet" />
          <Cell label="Completion method" value="Atomic rename" tone="quiet" />
          <Cell label="Validation" value="Checksum on arrival" tone="quiet" />
          <Cell label="Schedule" value={set.pollInterval} wire="poll_interval" tone="quiet" />
        </CellGrid>
        <Note>
          The fields an incremental set carries are absent here rather than shown empty:{" "}
          <WireName name="repository_domain" />, <WireName name="source_consistency" /> and{" "}
          <WireName name="verification_level" /> are not part of what an artifact set is.
        </Note>
      </MockCard>
    </>
  );
}
