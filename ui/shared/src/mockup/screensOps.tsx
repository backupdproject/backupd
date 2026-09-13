/**
 * The operational half of the mock-up (issue #788): what an operator looks
 * at after the set is configured — snapshots, one snapshot in detail, a
 * restore, repository health, retention with holds, and maintenance.
 *
 * Two rules run through all six and are the point of reviewing them
 * together:
 *
 *   - **Four byte counts, never one.** Scanned, read from the source,
 *     written to the repository, and reused are four different numbers
 *     (`backupengine.TreeSnapshotInfo`). A single "backed up" figure would
 *     report a deduplicating repository as growing by the size of the
 *     source every night. Where reuse was not measured the screen says so
 *     rather than drawing a zero.
 *   - **Every mutating control is a durable operation.** Restore, verify,
 *     hold, release and maintenance all submit one operation with an
 *     idempotency key and are then watched; none of them is a request the
 *     browser has to stay open for. The CLI submits the same operations,
 *     which is the parity #788 asks for.
 */
import { useState } from "react";
import { Banner } from "@shared/components/Banner";
import { MetricCard } from "@shared/components/MetricCard";
import { PageHeader } from "@shared/components/PageHeader";
import { StatusBadge } from "@shared/components/StatusBadge";
import { WarningBanner } from "@shared/components/WarningBanner";
import { Icon } from "@shared/design-system/icons";
import { bytes } from "@shared/utilities/format";
import {
  DOMAINS,
  HEALTH_CHECKS,
  INCREMENTAL_SET,
  MAINTENANCE,
  RESTORE_TREE,
  RETENTION_PROJECTION,
  RETENTION_TIERS,
  SNAPSHOTS,
  VERIFICATION_COPY
} from "@shared/mockup/data";
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
  StepControls,
  StepRail,
  Toggle,
  VerificationBadge,
  WireName
} from "@shared/mockup/parts";

/** A time as this mock-up prints one. The product formats through
 *  utilities/format; the fixtures here are fixed strings so a screenshot
 *  taken next month looks the same as one taken today. */
const WHEN: Record<string, string> = {
  "2026-09-13T04:00:00Z": "Today 04:00",
  "2026-09-12T22:00:00Z": "Yesterday 22:00",
  "2026-09-12T16:00:00Z": "Yesterday 16:00",
  "2026-09-12T10:00:00Z": "Yesterday 10:00",
  "2026-09-12T04:00:00Z": "Yesterday 04:00"
};

/** Screen 7 — every snapshot a set holds. */
export function SnapshotsScreen() {
  const set = INCREMENTAL_SET;
  return (
    <>
      <PageHeader
        back={{ label: "Back to " + set.source + "/" + set.set, onClick: () => undefined }}
        title="Snapshots"
        subtitle={
          <span style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
            <EngineBadge engine={set.engine} />
            <span>
              {set.source + "/" + set.set + DOT + set.snapshots + " snapshots" + DOT + "repository domain " + set.domain}
            </span>
          </span>
        }
        actions={
          <>
            <button className="btn">Verify latest</button>
            <button className="btn btn--primary">Back up now</button>
          </>
        }
      />

      <MockCard title="Snapshots">
        <div className="table-scroll">
          <table className="table">
            <thead>
              <tr>
                <th>Taken</th>
                <th>Snapshot</th>
                <th>Entries</th>
                <th>Logical</th>
                <th>Read</th>
                <th>Written</th>
                <th>Reused</th>
                <th>Duration</th>
                <th>Verification</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {SNAPSHOTS.map((snapshot) => (
                <tr key={snapshot.id}>
                  <td>
                    <div>{WHEN[snapshot.startedAt] ?? snapshot.startedAt}</div>
                    {snapshot.lastKnownGood ? (
                      <div style={{ marginTop: 4 }}>
                        <StatusBadge tone="ok" icon="success">
                          Newest known-good
                        </StatusBadge>
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
                    {snapshot.id}
                  </td>
                  <td className="mono">{snapshot.entries.toLocaleString()}</td>
                  <td className="mono">{bytes(snapshot.logicalBytes)}</td>
                  <td className="mono">{bytes(snapshot.sourceReadBytes)}</td>
                  <td className="mono">{bytes(snapshot.writtenBytes)}</td>
                  <td className="mono" style={{ color: snapshot.reuseMeasured ? undefined : "var(--text-3)" }}>
                    {snapshot.reuseMeasured ? bytes(snapshot.reusedBytes) : "not measured"}
                  </td>
                  <td className="mono">{Math.round(snapshot.durationSeconds / 60) + "m " + (snapshot.durationSeconds % 60) + "s"}</td>
                  <td>
                    <VerificationBadge
                      status={snapshot.verification}
                      achieved={snapshot.achievedLevel ? VERIFICATION_COPY[snapshot.achievedLevel].name : null}
                    />
                  </td>
                  <td>
                    <div style={{ display: "flex", gap: 6, justifyContent: "flex-end" }}>
                      <button className="btn btn--sm">Inspect</button>
                      <button className="btn btn--sm">Restore…</button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <Note>
          Four byte counts, never one. <WireName name="logical_bytes" /> is the size of the tree,{" "}
          <WireName name="source_bytes_read" /> is what this run pulled off the server,{" "}
          <WireName name="repository_bytes_written" /> is what landed in storage, and{" "}
          <WireName name="content_reused_bytes" /> is what the repository already had. A run whose
          engine could not account for reuse says so; it never shows a zero.
        </Note>
      </MockCard>
    </>
  );
}

/** Screen 8 — one snapshot, in full. */
export function SnapshotScreen() {
  const set = INCREMENTAL_SET;
  const snapshot = SNAPSHOTS[0];
  const failed = SNAPSHOTS[3];

  return (
    <>
      <PageHeader
        back={{ label: "Back to snapshots", onClick: () => undefined }}
        title={
          <span className="mono" style={{ fontSize: 22 }}>
            {snapshot.id}
          </span>
        }
        subtitle={set.source + "/" + set.set + DOT + (WHEN[snapshot.startedAt] ?? "") + DOT + "3 min 34 s"}
        actions={
          <>
            <button className="btn">Verify now</button>
            <button className="btn">Place hold…</button>
            <button className="btn btn--primary">Restore…</button>
          </>
        }
      />

      <section className="card" aria-label="What this run did">
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(196px, 1fr))" }}>
          <MetricCard label="Entries scanned" value={snapshot.entries.toLocaleString()} detail={snapshot.files.toLocaleString() + " files"} />
          <MetricCard label="Logical size" value={bytes(snapshot.logicalBytes)} detail="the tree as described" />
          <MetricCard label="Read from source" value={bytes(snapshot.sourceReadBytes)} detail="every byte offered" />
          <MetricCard label="Written to repository" value={bytes(snapshot.writtenBytes)} detail="after deduplication" />
          <MetricCard
            label="Reused"
            tip="snapshots.reused"
            value={snapshot.reuseMeasured ? bytes(snapshot.reusedBytes) : "not measured"}
            detail={snapshot.reuseMeasured ? "content already stored" : "not accounted for on this run"}
          />
          <MetricCard label="Duration" value="3m 34s" detail="04:00 to 04:03" />
        </div>
      </section>

      <MockCard title="Snapshot">
        <Rows>
          <Row label="Snapshot" wire="snapshot_id" value={snapshot.id} mono />
          <Row label="Run" wire="run_id" value="run-2026-09-13-0400-file-server" mono />
          <Row label="Operation" wire="operation_id" value="op_01J9Z4M2QK7T" mono />
          <Row label="Backup set" wire="backup_set_id" value={set.source + "/" + set.set} mono />
          <Row label="Repository domain" wire="repository_domain" value={set.domain ?? ""} mono />
          <Row label="State" wire="phase" value={<StatusBadge tone="ok" icon="success">Stored and verified</StatusBadge>} />
          <Row label="Directories" value={snapshot.directories.toLocaleString()} mono />
          <Row label="Source complete" wire="source_complete" value="Yes — every entry the walk found was captured" />
        </Rows>
      </MockCard>

      <MockCard title="Verification" tip="snapshots.achieved-level">
        <Rows>
          <Row label="Asked for" wire="verification_level" value={VERIFICATION_COPY[set.verification].name} />
          <Row
            label="Proved"
            wire="verification_level_achieved"
            value={
              <VerificationBadge
                status={snapshot.verification}
                achieved={snapshot.achievedLevel ? VERIFICATION_COPY[snapshot.achievedLevel].name : null}
              />
            }
          />
          <Row label="Checked" value="today at 04:05, 7,065 of 141,286 files read and hash-checked" />
          <Row
            label="Restore point"
            value={
              <StatusBadge tone="ok" icon="success">
                Newest known-good for this set
              </StatusBadge>
            }
          />
        </Rows>
        <Note>
          The level a run <em>proves</em> is not always the level it was asked for, so both are shown.
          A failed check proves nothing at all and carries no level.
        </Note>
      </MockCard>

      <MockCard title="Contents">
        <div className="table-scroll">
          <table className="table">
            <thead>
              <tr>
                <th>Path</th>
                <th>Entries</th>
                <th>Logical</th>
              </tr>
            </thead>
            <tbody>
              <tr>
                <td className="mono">/srv/shares/finance</td>
                <td className="mono">8,204</td>
                <td className="mono">{bytes(84 * 1024 ** 3)}</td>
              </tr>
              <tr>
                <td className="mono">/srv/shares/engineering</td>
                <td className="mono">96,411</td>
                <td className="mono">{bytes(902 * 1024 ** 3)}</td>
              </tr>
              <tr>
                <td className="mono">/srv/shares/marketing</td>
                <td className="mono">44,287</td>
                <td className="mono">{bytes(426 * 1024 ** 3)}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </MockCard>

      <MockCard title="A run that did not pass, for contrast">
        <Banner tone="danger" dismissible={false} style={{ flexDirection: "column" }}>
          <div style={{ display: "flex", gap: 12 }}>
            <span aria-hidden="true" style={{ color: "var(--danger)", lineHeight: 1.5 }}>
              <Icon name="failure" />
            </span>
            <div style={{ fontSize: 13 }}>
              <div style={{ fontWeight: 600 }}>
                {"Snapshot " + failed.id + " failed verification, and is not a restore point"}
              </div>
              <div style={{ marginTop: 4, color: "var(--text-2)" }}>{failed.consistencyViolation}</div>
              <div style={{ marginTop: 6 }}>
                <WireName name="verification_level_achieved absent" />
              </div>
            </div>
          </div>
        </Banner>
        <Note>
          A failed verification writes no achieved level at all, so a failure cannot inherit the
          claim of the run before it.
        </Note>
      </MockCard>
    </>
  );
}

const RESTORE_STEPS = ["Snapshot", "What to restore", "Where", "Confirm"] as const;

/** Screen 9 — restoring from a snapshot, as a short guided flow with a
 *  durable operation at the end of it. */
export function RestoreScreen() {
  const [step, setStep] = useState(1);
  const [overwrite, setOverwrite] = useState(false);
  const set = INCREMENTAL_SET;
  const selectedBytes = RESTORE_TREE.filter((entry) => entry.selected && entry.kind === "file").reduce(
    (n, entry) => n + entry.size,
    0
  );

  return (
    <div style={{ maxWidth: 980, width: "100%", margin: "0 auto", display: "flex", flexDirection: "column", gap: 16 }}>
      <PageHeader
        back={{ label: "Cancel restore", onClick: () => undefined }}
        title="Restore from a snapshot"
        subtitle={set.source + "/" + set.set + DOT + "restores never overwrite unless you say so"}
      />

      <StepRail steps={RESTORE_STEPS} step={step} onSelect={setStep} />

      <section className="card">
        <div style={{ padding: "20px 22px 22px" }}>
          {step === 1 ? (
            <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
              <div>
                <h2>Which snapshot</h2>
                <p style={{ margin: "5px 0 0", fontSize: 13, color: "var(--text-2)" }}>
                  Only snapshots that passed their verification are offered by default.
                </p>
              </div>
              <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
                {SNAPSHOTS.filter((snapshot) => snapshot.verification === "passed").map((snapshot, index) => (
                  <Choice
                    key={snapshot.id}
                    name="restore-snapshot"
                    title={(WHEN[snapshot.startedAt] ?? snapshot.startedAt) + (snapshot.lastKnownGood ? " — newest known-good" : "")}
                    wire={"run_id=" + snapshot.id}
                    detail={
                      snapshot.entries.toLocaleString() +
                      " entries" +
                      DOT +
                      bytes(snapshot.logicalBytes) +
                      DOT +
                      "verified " +
                      VERIFICATION_COPY[snapshot.achievedLevel ?? "structural"].name
                    }
                    checked={index === 0}
                    onChange={() => undefined}
                  />
                ))}
              </div>
              <Toggle
                label="Also show snapshots that failed verification"
                note="off"
                checked={false}
                onChange={() => undefined}
              />
            </div>
          ) : null}

          {step === 2 ? (
            <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
              <div>
                <h2>What to restore</h2>
                <p style={{ margin: "5px 0 0", fontSize: 13, color: "var(--text-2)" }}>
                  The whole snapshot, or a path inside it.
                </p>
              </div>
              <ul style={{ margin: 0, padding: 0, listStyle: "none" }}>
                {RESTORE_TREE.map((entry) => (
                  <li
                    key={entry.path + entry.depth}
                    style={{
                      display: "flex",
                      alignItems: "center",
                      gap: 10,
                      padding: "8px 10px",
                      paddingLeft: 10 + entry.depth * 22,
                      borderBottom: "1px solid var(--border)",
                      fontSize: 13
                    }}
                  >
                    <input type="checkbox" checked={entry.selected} readOnly style={{ accentColor: "var(--accent)" }} />
                    <span aria-hidden="true" style={{ color: "var(--text-3)", display: "inline-flex" }}>
                      <Icon name={entry.kind === "dir" ? "backup-sets" : "backups"} size={13} />
                    </span>
                    <span className="mono" style={{ flex: 1 }}>
                      {entry.path}
                    </span>
                    <span className="mono" style={{ color: "var(--text-2)" }}>
                      {bytes(entry.size)}
                    </span>
                  </li>
                ))}
              </ul>
              <Note>{"Selected: " + bytes(selectedBytes) + " across 2 paths."}</Note>
            </div>
          ) : null}

          {step === 3 ? (
            <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
              <div>
                <h2>Where it goes</h2>
                <p style={{ margin: "5px 0 0", fontSize: 13, color: "var(--text-2)" }}>
                  A directory on this NAS. Restoring onto the original server is a copy you make from
                  there, so a restore can never be the thing that damages the source.
                </p>
              </div>
              <FormGrid>
                <Field label="Restore into" value="/data/restores/file-server-2026-09-13" wire="destination_path" mono />
                <Select
                  label="If a file is already there"
                  wire="overwrite"
                  value={overwrite ? "overwrite" : "refuse"}
                  options={[
                    { value: "refuse", label: "Refuse and stop" },
                    { value: "overwrite", label: "Overwrite it" }
                  ]}
                  onChange={(next) => setOverwrite(next === "overwrite")}
                />
              </FormGrid>
              {overwrite ? (
                <WarningBanner
                  tone="warn"
                  eyebrow="Overwriting"
                  title="Files already in that directory will be replaced"
                  dismissible={false}
                >
                  Nothing outside the paths this snapshot names is touched, but a file of the same
                  name inside them is overwritten without a second prompt.
                </WarningBanner>
              ) : null}
            </div>
          ) : null}

          {step === 4 ? (
            <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
              <div>
                <h2>Confirm</h2>
                <p style={{ margin: "5px 0 0", fontSize: 13, color: "var(--text-2)" }}>
                  This submits one durable operation. You can close the browser; it keeps going.
                </p>
              </div>
              <CellGrid min={220}>
                <Cell label="Snapshot" value={SNAPSHOTS[0].id} mono tone="quiet" />
                <Cell label="Paths" value="2 selected" tone="quiet" />
                <Cell label="To restore" value={bytes(selectedBytes)} tone="quiet" />
                <Cell label="Destination" value="/data/restores/file-server-2026-09-13" mono tone="quiet" />
                <Cell label="If a file exists" value={overwrite ? "Overwrite" : "Refuse and stop"} tone="quiet" />
                <Cell label="Free space after" value={bytes(402 * 1024 ** 3)} tone="quiet" />
              </CellGrid>

              <div
                style={{
                  border: "1px solid var(--border)",
                  borderRadius: "var(--radius-lg)",
                  background: "var(--surface-2)",
                  padding: 14
                }}
              >
                <div className="eyebrow" style={{ fontSize: 10.5 }}>
                  In progress
                </div>
                <div style={{ marginTop: 8, display: "flex", alignItems: "center", gap: 10 }}>
                  <div className="activity-bar" style={{ flex: 1 }}>
                    <div className="activity-bar__fill activity-bar__fill--busy" style={{ width: "38%" }} />
                  </div>
                  <span className="mono" style={{ fontSize: "var(--text-sm)" }}>
                    38%
                  </span>
                </div>
                <div style={{ marginTop: 8, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                  {"Restoring 3,204 of 8,204 entries" + DOT + bytes(32 * 1024 ** 3) + " of " + bytes(84 * 1024 ** 3) + DOT + "operation op_01J9Z58RB3XQ"}
                </div>
              </div>
            </div>
          ) : null}
        </div>

        <StepControls
          step={step}
          total={RESTORE_STEPS.length}
          onBack={() => setStep(Math.max(1, step - 1))}
          onNext={() => setStep(Math.min(RESTORE_STEPS.length, step + 1))}
          finishLabel="Start restore"
        />
      </section>
    </div>
  );
}

/** Screen 10 — repository health, per domain. */
export function HealthScreen() {
  return (
    <>
      <PageHeader
        title="Repository health"
        subtitle={"Checked 2 minutes ago" + DOT + DOMAINS.length + " domains"}
        actions={<button className="btn">Check now</button>}
      />

      <WarningBanner
        tone="warn"
        eyebrow="One domain needs attention"
        title="offsite-b2 has not had full maintenance inside its window"
        actions={<button className="btn btn--sm">Open maintenance</button>}
      >
        Snapshots are still being written and can still be restored. What is not happening is
        reclamation, so the store keeps paying for content nothing references any more.
      </WarningBanner>

      {DOMAINS.map((domain) => (
        <MockCard
          key={domain.id}
          title={domain.id}
          actions={
            <>
              <StatusBadge tone={domain.health === "ok" ? "ok" : "warn"} icon={domain.health === "ok" ? "status-active" : "warning"}>
                {domain.health === "ok" ? "Healthy" : "Attention"}
              </StatusBadge>
              <button className="btn btn--sm">Check now</button>
            </>
          }
        >
          <CellGrid min={190}>
            <Cell label="Reachable" value="Yes" wire="reachable" tone="quiet" />
            <Cell label="Readable" value="Yes" wire="readable" tone="quiet" />
            <Cell label="Writable" value="Yes" wire="writable" tone="quiet" />
            <Cell label="Credentials" value="Valid" wire="credentials_valid" tone="quiet" />
            <Cell
              label="Clock"
              value="Within 2 s"
              wire="clock_skew_seconds"
              tone="quiet"
            />
            <Cell
              label="Maintenance"
              value={domain.health === "ok" ? "On schedule" : "3 days overdue"}
              wire="maintenance_overdue"
              tone="quiet"
            />
          </CellGrid>
          <div style={{ marginTop: 12 }}>
            <Rows>
              <Row label="Last snapshot" wire="last_snapshot_at" value="2 hours ago, completed" />
              <Row label="Last verification" wire="last_verification_status" value="2 hours ago, passed" />
              <Row label="Backup sets here" wire="backup_sets" value={domain.sets.join(DOT)} mono />
              <Row label="Detail" wire="detail" value={domain.healthNote} />
            </Rows>
          </div>
        </MockCard>
      ))}

      <MockCard title="What was checked">
        <CheckList checks={HEALTH_CHECKS} />
        <Note>
          Reachability is proved by a read and a write, not inferred from an open handle: a
          repository handle survives a network partition and every call on it would fail.
        </Note>
      </MockCard>
    </>
  );
}

/** Screen 11 — retention for snapshots, and the holds that override it. */
export function RetentionScreen() {
  const [dialog, setDialog] = useState<"none" | "hold" | "release">("none");
  const set = INCREMENTAL_SET;

  return (
    <>
      <PageHeader
        back={{ label: "Back to " + set.source + "/" + set.set, onClick: () => undefined }}
        title="Retention"
        subtitle={"What the next retention pass would do" + DOT + "nothing here has run yet"}
        actions={
          <>
            <button className="btn">Preview again</button>
            <button className="btn btn--caution">Apply retention now</button>
          </>
        }
      />

      <MockCard title="Chain">
        <div className="table-scroll">
          <table className="table">
            <thead>
              <tr>
                <th>Tier</th>
                <th>Keeps</th>
                <th>Holding</th>
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
      </MockCard>

      <MockCard title="What the next pass would do">
        <div className="table-scroll">
          <table className="table">
            <thead>
              <tr>
                <th>Taken</th>
                <th>Snapshot</th>
                <th>Verdict</th>
                <th>Why</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {RETENTION_PROJECTION.map((row) => (
                <tr key={row.id}>
                  <td className="mono">{row.takenAt}</td>
                  <td className="mono" style={{ fontSize: "var(--text-sm)" }}>
                    {row.id}
                  </td>
                  <td>
                    {row.verdict === "keep" ? (
                      <StatusBadge tone="ok" icon="success">
                        Keep
                      </StatusBadge>
                    ) : (
                      <StatusBadge tone="warn" icon="warning">
                        Delete
                      </StatusBadge>
                    )}
                  </td>
                  <td style={{ fontSize: 13, color: "var(--text-2)" }}>{row.reason}</td>
                  <td>
                    <div style={{ display: "flex", gap: 6, justifyContent: "flex-end" }}>
                      <button className="btn btn--sm" onClick={() => setDialog("hold")}>
                        Hold…
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <Note>
          A verdict names what selected the snapshot, or what protects it. &ldquo;Delete&rdquo; here
          is a projection: nothing is removed until the pass is applied, and a hold placed before then
          changes the answer.
        </Note>
      </MockCard>

      <MockCard title="Holds" actions={<button className="btn btn--sm" onClick={() => setDialog("hold")}>Place a hold…</button>}>
        <div className="table-scroll">
          <table className="table">
            <thead>
              <tr>
                <th>Snapshot</th>
                <th>Reason</th>
                <th>Placed by</th>
                <th>Placed</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {SNAPSHOTS.flatMap((snapshot) =>
                snapshot.holds.map((hold) => (
                  <tr key={hold.id}>
                    <td className="mono" style={{ fontSize: "var(--text-sm)" }}>
                      {snapshot.id}
                    </td>
                    <td>{hold.reason}</td>
                    <td className="mono">{hold.placedBy}</td>
                    <td className="mono">{WHEN[snapshot.startedAt] ?? hold.placedAt}</td>
                    <td>
                      <div style={{ display: "flex", justifyContent: "flex-end" }}>
                        <button className="btn btn--sm btn--destructive" onClick={() => setDialog("release")}>
                          Release
                        </button>
                      </div>
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
        <Note>
          A hold is one person&rsquo;s recorded decision, with a reason and a name against it. Several
          may sit on one snapshot, and the snapshot survives until the last of them is released.
        </Note>
      </MockCard>

      {dialog === "none" ? null : (
        <div className="dialog-scrim">
          <div className={"dialog" + (dialog === "release" ? " dialog--destructive" : "")} role="dialog" aria-modal="true">
            <h2 style={{ marginTop: 0 }}>{dialog === "hold" ? "Place a hold" : "Release this hold"}</h2>
            {dialog === "hold" ? (
              <>
                <p style={{ fontSize: 13, color: "var(--text-2)" }}>
                  A held snapshot is never expired by retention, whatever the chain says, until the
                  hold is released.
                </p>
                <Field label="Reason" value="Kept for the 2026 Q3 audit" wire="reason" />
              </>
            ) : (
              <p style={{ fontSize: 13, color: "var(--text-2)" }}>
                Releasing the legal hold on 2026-09-12 04:00 leaves the snapshot governed by the
                retention chain again. The next pass may delete it.
              </p>
            )}
            <div style={{ marginTop: 18, display: "flex", gap: 10, justifyContent: "flex-end" }}>
              <button className="btn" onClick={() => setDialog("none")}>
                Cancel
              </button>
              <button
                className={dialog === "hold" ? "btn btn--primary" : "btn btn--destructive-confirm"}
                onClick={() => setDialog("none")}
              >
                {dialog === "hold" ? "Place hold" : "Release hold"}
              </button>
            </div>
          </div>
        </div>
      )}
    </>
  );
}

/** Screen 12 — maintenance, per domain, with its owner in the first
 *  column because that is what decides whether anything on the row can be
 *  pressed. */
export function MaintenanceScreen() {
  return (
    <>
      <PageHeader
        title="Repository maintenance"
        subtitle="Compaction and reclamation, per domain. One instance owns each."
        tip="nav.settings"
      />

      <Banner tone="info" dismissible={false}>
        <span style={{ fontSize: 13 }}>
          Maintenance never touches a snapshot Backupd wrote: every one of them is pinned, and only
          retention removes them. What it removes is content nothing references any more.
        </span>
      </Banner>

      {MAINTENANCE.map((row) => (
        <MockCard
          key={row.domain}
          title={row.domain}
          tip="repositories.maintenance-owner"
          actions={
            <>
              {row.state === "overdue" ? (
                <StatusBadge tone="warn" icon="warning">
                  Overdue
                </StatusBadge>
              ) : row.state === "refused" ? (
                <StatusBadge tone="neutral" icon="status-idle">
                  Owned elsewhere
                </StatusBadge>
              ) : (
                <StatusBadge tone="ok" icon="status-active">
                  On schedule
                </StatusBadge>
              )}
              <button className="btn btn--sm" disabled={!row.ownedHere}>
                Run quick
              </button>
              <button className="btn btn--sm btn--caution" disabled={!row.ownedHere}>
                Run full
              </button>
            </>
          }
        >
          <CellGrid min={185}>
            <Cell label="Owner" value={row.owner} wire="owner" tone="quiet" />
            <Cell label="Last quick" value={row.lastQuick} wire="last_quick_at" tone="quiet" />
            <Cell label="Last full" value={row.lastFull} wire="last_full_at" tone="quiet" />
            <Cell label="Next due" value={row.due} wire="next_eligible_at" tone="quiet" />
            <Cell label="Reclaimed, last full" value={bytes(41 * 1024 ** 3)} wire="reclaimed_bytes" tone="quiet" />
            <Cell label="Failures" value="0" wire="failures" tone="quiet" />
          </CellGrid>
          <Note>{row.note}</Note>
          {row.ownedHere ? null : (
            <div style={{ marginTop: 12 }}>
              <button className="btn btn--sm btn--caution">Transfer ownership to this instance…</button>
            </div>
          )}
        </MockCard>
      ))}
    </>
  );
}
