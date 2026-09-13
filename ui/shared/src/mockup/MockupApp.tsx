/**
 * The dev-only mock-up shell (issue #788), mounted at `/mockup`.
 *
 * # Why it is a real application and not a picture
 *
 * The thing being reviewed is whether a set of new screens BELONGS in this
 * product, and that question is about typography, spacing, tone of voice
 * and the shape of a card as much as it is about the fields on it. A
 * drawing in another tool answers none of them, and every mock-up this
 * team has reviewed as an image has had to be re-litigated once it was
 * built. So the mock-up renders inside the real `AppShell`, with the real
 * design tokens, the real `Card`, `PageHeader`, `MetricCard`,
 * `StatusBadge`, `Banner` and the real tooltip registry: what is on screen
 * is what the production UI wave will look like, because it is made of the
 * same parts.
 *
 * # Why it is above the auth gate
 *
 * `App` renders this before it asks whether anyone is signed in, which
 * looks like a hole and is the opposite of one. The mock-up talks to
 * nothing: no session, no API, no service. Putting it behind the gate
 * would mean a designer needs a running backend and an account to look at
 * a static design, and every screen below would be one network failure
 * away from being unreviewable. `import.meta.env.DEV` is what keeps it out
 * of a shipped bundle: it is statically false in a production build, so
 * the branch and this whole module are dropped by the bundler.
 *
 * # What it is not
 *
 * It is not the production UI, and no page here is wired to anything. When
 * the UI wave lands, this directory goes with it: the design doc
 * (docs/design/788-incremental-ui-mockup.md) is what survives.
 */
import { NavLink, Navigate, Route, Routes } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import { AppShell } from "@shared/layouts/AppShell";
import { WarningBanner } from "@shared/components/WarningBanner";
import { DefaultsScreen, DomainNewScreen, DomainsScreen } from "@shared/mockup/screensGlobal";
import { SetConfigScreen, SetDetailArtifactScreen, SetDetailIncrementalScreen } from "@shared/mockup/screensSet";
import { WizardScreen } from "@shared/mockup/screensWizard";
import {
  HealthScreen,
  MaintenanceScreen,
  RestoreScreen,
  RetentionScreen,
  SnapshotScreen,
  SnapshotsScreen
} from "@shared/mockup/screensOps";

/** Every screen in the mock-up, in the order a reviewer should walk them:
 *  the deployment is configured before a set is, a set is configured
 *  before it runs, and the operational screens are what running produces. */
const SCREENS: { path: string; label: string; group: string; element: JSX.Element }[] = [
  { path: "domains", label: "Repository domains", group: "Deployment", element: <DomainsScreen /> },
  { path: "domain-new", label: "Define a domain", group: "Deployment", element: <DomainNewScreen /> },
  { path: "defaults", label: "Backup defaults", group: "Deployment", element: <DefaultsScreen /> },
  { path: "wizard", label: "Add backup set (wizard)", group: "Per set", element: <WizardScreen /> },
  { path: "set-config", label: "Set configuration", group: "Per set", element: <SetConfigScreen /> },
  { path: "set-incremental", label: "Set detail — incremental", group: "Per set", element: <SetDetailIncrementalScreen /> },
  { path: "set-artifact", label: "Set detail — artifact", group: "Per set", element: <SetDetailArtifactScreen /> },
  { path: "snapshots", label: "Snapshots", group: "Operating", element: <SnapshotsScreen /> },
  { path: "snapshot", label: "Snapshot detail", group: "Operating", element: <SnapshotScreen /> },
  { path: "restore", label: "Restore", group: "Operating", element: <RestoreScreen /> },
  { path: "health", label: "Repository health", group: "Operating", element: <HealthScreen /> },
  { path: "retention", label: "Retention and holds", group: "Operating", element: <RetentionScreen /> },
  { path: "maintenance", label: "Maintenance", group: "Operating", element: <MaintenanceScreen /> }
];

const GROUPS = ["Deployment", "Per set", "Operating"];

export function MockupApp({ theme, onToggleTheme }: { theme: "light" | "dark"; onToggleTheme(): void }) {
  const api = useApi();
  // The dev build's mock API, which is where the shell's own status badge
  // and nav counts come from. Nothing on a mock-up SCREEN reads it: those
  // are fixtures, so a screenshot is reproducible.
  const health = useAsync(() => api.getHealth(), [api]);
  const version = useAsync(() => api.getVersion(), [api]);

  return (
    <AppShell
      health={health.data}
      version={version.data}
      counts={{ sets: 4, backups: 1204, quarantine: 0 }}
      theme={theme}
      onToggleTheme={onToggleTheme}
      onSignOut={() => undefined}
    >
      <WarningBanner
        tone="info"
        eyebrow="Design mock-up"
        title="Incremental backup — the design gate for issue #788"
        dismissible={false}
      >
        Every screen below is static, built from the real design system so it can be reviewed as the
        product rather than as a picture. Nothing here calls the service, and no button does
        anything. The production UI follows this; it is not this.
      </WarningBanner>

      <nav aria-label="Mock-up screens" className="card">
        <div className="card__body" style={{ display: "flex", flexDirection: "column", gap: 12 }}>
          {GROUPS.map((group) => (
            <div key={group} style={{ display: "flex", alignItems: "baseline", gap: 12, flexWrap: "wrap" }}>
              <span className="eyebrow" style={{ fontSize: 10.5, minWidth: 92 }}>
                {group}
              </span>
              <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
                {SCREENS.filter((screen) => screen.group === group).map((screen) => (
                  <NavLink
                    key={screen.path}
                    to={"/mockup/" + screen.path}
                    style={({ isActive }) => ({
                      padding: "5px 11px",
                      borderRadius: "var(--radius-pill)",
                      border: "1px solid " + (isActive ? "var(--accent)" : "var(--border-strong)"),
                      background: isActive ? "var(--accent-quiet)" : "var(--surface-2)",
                      color: isActive ? "var(--text)" : "var(--text-2)",
                      fontSize: "var(--text-sm)",
                      fontWeight: isActive ? 600 : 400,
                      textDecoration: "none"
                    })}
                  >
                    {screen.label}
                  </NavLink>
                ))}
              </div>
            </div>
          ))}
        </div>
      </nav>

      <Routes>
        {SCREENS.map((screen) => (
          <Route key={screen.path} path={"/mockup/" + screen.path} element={screen.element} />
        ))}
        <Route path="*" element={<Navigate to="/mockup/domains" replace />} />
      </Routes>
    </AppShell>
  );
}
