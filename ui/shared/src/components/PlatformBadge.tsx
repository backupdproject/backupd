/**
 * Where this app is running, stated to the operator rather than implied.
 *
 * Two renderings of the same facts. The compact one sits at the foot of
 * the nav and answers "what am I looking at" at a glance; the full one is
 * a definition list on the Settings page, where the deployment label and
 * the storage mount are things an administrator reads carefully and may
 * need to quote. Both come from the bridge, so a provider's own answers
 * are what appear and the shared tree never guesses.
 *
 * The product's identity stays Backupd throughout. The platform is
 * context around it, never branding on it, which is why the platform name
 * appears as a value in a row and not as a title anywhere.
 */
import { usePlatform } from "@shared/platform/PlatformContext";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";

const INTEGRATION_LABEL: Record<string, string> = {
  standalone: "Standalone web app",
  native: "Native app",
  "embedded-web": "Embedded web app",
  container: "Container app"
};

/** Makes the abstraction explicit to administrators (§21). Product identity is
 *  Backupd; the platform is context, never the brand. */
export function PlatformBadge({ compact = false }: { compact?: boolean }) {
  const { bridge, auth } = usePlatform();
  const authLabel =
    auth?.mode === "native-session"
      ? bridge.name + " session"
      : "Backupd local account";

  if (compact)
    return (
      <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
        <div className="eyebrow" style={{ fontSize: "var(--text-xs)" }}>Running on</div>
        <div style={{ fontSize: 13, fontWeight: 500 }}>{bridge.name}</div>
        <div style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
          {INTEGRATION_LABEL[bridge.integration] + " \u00b7 " + authLabel}
        </div>
      </div>
    );

  return (
    <dl
      style={{
        margin: 0, display: "grid", gridTemplateColumns: "128px 1fr",
        gap: "11px 14px", fontSize: 13
      }}
    >
      {/* Issue #834: the explanation goes on the TERM, not on the value.
          A deployment label and a storage mount are strings an
          administrator is asked to quote, and what they mean is a property
          of the row rather than of today's value. The compact rendering
          carries no "i" of its own because the nav wraps the whole block
          in one tooltip already. */}
      <dt style={{ color: "var(--text-2)" }}><span>Platform</span><InfoTooltip id="platform.name" /></dt>
      <dd style={{ margin: 0, fontWeight: 500 }}>{bridge.name}</dd>
      <dt style={{ color: "var(--text-2)" }}>
        <span>Integration</span>
        <InfoTooltip id="platform.integration" />
      </dt>
      <dd style={{ margin: 0 }}>{INTEGRATION_LABEL[bridge.integration]}</dd>
      <dt style={{ color: "var(--text-2)" }}><span>Authentication</span><InfoTooltip id="platform.auth" /></dt>
      <dd style={{ margin: 0 }}>{authLabel}</dd>
      <dt style={{ color: "var(--text-2)" }}>
        <span>Deployment</span>
        <InfoTooltip id="platform.deployment" />
      </dt>
      <dd className="mono" style={{ margin: 0, fontSize: "var(--text-sm)" }}>
        {bridge.deployment.label}
      </dd>
      <dt style={{ color: "var(--text-2)" }}>
        <span>Storage mount</span>
        <InfoTooltip id="platform.storage-mount" />
      </dt>
      <dd className="mono" style={{ margin: 0, fontSize: "var(--text-sm)" }}>
        {bridge.deployment.storageMount}
      </dd>
    </dl>
  );
}
