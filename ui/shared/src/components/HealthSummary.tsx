import type { SystemHealth } from "@shared/types/operation";
import { Icon } from "@shared/design-system/icons";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import type { TooltipId } from "@shared/tooltips/tooltips";
import { HEALTH_PRESENTATION, StatusBadge } from "./StatusBadge";
import { bytes, relativeAge } from "@shared/utilities/format";

/** One explanation per verdict this headline can state (issue #834).
 *  Four entries rather than one "what this word means" entry, because the
 *  four words are the whole point of the headline: what an operator does
 *  next differs completely between "stale" and "failing", and a single
 *  shared sentence could only describe the scale they sit on. */
const HEADLINE_TIP: Record<SystemHealth["backupHealth"], TooltipId> = {
  healthy: "dashboard.health.healthy",
  degraded: "dashboard.health.degraded",
  stale: "dashboard.health.stale",
  failing: "dashboard.health.failing"
};

/** §8: "APPLICATION RUNNING" and "BACKUPS HEALTHY" are two different facts.
 *  The headline states the BACKUP verdict; the daemon is a supporting chip. */
export function HealthSummary({ health }: { health: SystemHealth }) {
  const p = HEALTH_PRESENTATION[health.backupHealth];
  const headline = "BACKUPS " + p.label.toUpperCase();
  const color =
    p.tone === "ok" ? "var(--ok)" : p.tone === "warn" ? "var(--warn)" : "var(--danger)";

  return (
    <section className="card" aria-label="Backup health">
      <div
        style={{
          display: "flex", alignItems: "flex-start", gap: 20, flexWrap: "wrap",
          padding: "20px 22px", borderBottom: "1px solid var(--border)"
        }}
      >
        <div style={{ flex: 1, minWidth: 300, display: "flex", flexDirection: "column", gap: 9 }}>
          <InfoTooltip id={HEADLINE_TIP[health.backupHealth]} block>
            <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
              <span aria-hidden="true" style={{ color, fontSize: 15, display: "inline-flex" }}>
                <Icon name={p.icon} />
              </span>
              <span
                style={{
                  fontFamily: "var(--font-mono)", fontSize: 17, fontWeight: 600,
                  letterSpacing: "0.04em", color
                }}
              >
                {headline}
              </span>
            </div>
          </InfoTooltip>
          <InfoTooltip id="dashboard.health.reason" block>
            <p style={{ margin: 0, fontSize: 13.5, color: "var(--text-2)", maxWidth: "66ch" }}>
              {health.backupHealthReason}
            </p>
          </InfoTooltip>
          <div style={{ display: "flex", gap: 8, flexWrap: "wrap", marginTop: 3 }}>
            {/* "Service running" and nothing more. The service answered
                this request, which is the whole claim; it used to carry an
                uptime figure that no endpoint has ever reported (#211). */}
            <InfoTooltip id="dashboard.health.service">
              <StatusBadge tone={health.serviceRunning ? "ok" : "danger"} icon={health.serviceRunning ? "status-active" : "failure"}>
                {health.serviceRunning ? "Service running" : "Service stopped"}
              </StatusBadge>
            </InfoTooltip>
            <InfoTooltip id="dashboard.health.storage-state">
              <StatusBadge
                tone={health.storageState === "nominal" ? "ok" : health.storageState === "warning" ? "warn" : "danger"}
                icon={health.storageState === "nominal" ? "status-active" : "warning"}
              >
                {"Storage " + health.storageState}
              </StatusBadge>
            </InfoTooltip>
            {health.setsStale > 0 ? (
              <InfoTooltip id="dashboard.health.sets-stale">
                <StatusBadge tone="warn" icon="warning">{health.setsStale + " set stale"}</StatusBadge>
              </InfoTooltip>
            ) : null}
            {health.setsDegraded > 0 ? (
              <InfoTooltip id="dashboard.health.sets-degraded">
                <StatusBadge tone="warn" icon="warning">{health.setsDegraded + " set degraded"}</StatusBadge>
              </InfoTooltip>
            ) : null}
            {health.setsFailing > 0 ? (
              <InfoTooltip id="dashboard.health.sets-halted">
                <StatusBadge tone="danger" icon="failure">{health.setsFailing + " set halted"}</StatusBadge>
              </InfoTooltip>
            ) : null}
            {/* Issue #282/#316: shown only when it is not zero, the same
                "a permanently-resting figure is a line an operator stops
                seeing" reasoning `status`'s own CLI output already
                follows for this exact count \u2014 most deployments never
                declare a set read-only, and this stays zero forever.
                Informational, not a problem: tone "ok", not "warn",
                because a growing count here is this manager doing
                exactly what a read-only source was declared for. */}
            {health.readOnlyRetainedCount > 0 ? (
              <InfoTooltip id="dashboard.health.read-only-retained">
                <StatusBadge tone="ok" icon="status-active">
                  {health.readOnlyRetainedCount + " retained (read-only source)"}
                </StatusBadge>
              </InfoTooltip>
            ) : null}
          </div>
        </div>

        <dl
          style={{
            flex: "none", margin: 0, display: "grid",
            gridTemplateColumns: "auto auto", gap: "9px 26px", fontSize: 13
          }}
        >
          <Row
            tip="dashboard.health.last-completed"
            label="Last completed backup"
            value={relativeAge(health.lastCompletedBackupAt)}
          />
          <Row
            tip="dashboard.health.newest-verified"
            label="Newest verified backup"
            value={relativeAge(health.newestVerifiedBackupAt)}
          />
          {/* Null is "some set has never produced a known-good backup", not
              a large number of hours. Rendering a figure there would be
              false precision an operator could act on. */}
          <Row
            tip="dashboard.health.freshness"
            label="Oldest set freshness"
            value={
              health.oldestSetFreshnessHours === null
                ? "not yet measurable"
                : health.oldestSetFreshnessHours + " hours ago"
            }
            tone={
              health.oldestSetFreshnessHours !== null && health.oldestSetFreshnessHours > 24
                ? "var(--warn)"
                : undefined
            }
          />
          <Row tip="dashboard.health.free-space" label="Free space" value={bytes(health.storageFreeBytes)} />
          {health.storageReadingsUnavailable > 0 ? (
            <Row
              tip="dashboard.health.capacity-unreadable"
              label="Capacity unreadable"
              value={health.storageReadingsUnavailable + " destination(s)"}
              tone="var(--warn)"
            />
          ) : null}
        </dl>
      </div>
    </section>
  );
}

/** A label and its reading.
 *
 *  The explanation is attached to both halves rather than to the pair,
 *  because dt and dd are the enclosing grid's own items: a host wrapped
 *  around both would become the grid item instead and the two columns
 *  would collapse into one. Both halves name the same registry entry, so
 *  there is one sentence per row however it is reached (issue #834). */
function Row({ label, value, tone, tip }: { label: string; value: string; tone?: string; tip: TooltipId }) {
  return (
    <>
      <dt style={{ color: "var(--text-2)" }}>
        <InfoTooltip id={tip}><span>{label}</span></InfoTooltip>
      </dt>
      <dd style={{ margin: 0, fontFamily: "var(--font-mono)", textAlign: "right", color: tone }}>
        <InfoTooltip id={tip} alignEnd><span>{value}</span></InfoTooltip>
      </dd>
    </>
  );
}
