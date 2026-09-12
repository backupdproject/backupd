import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { UserEvent } from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
// The real stylesheet, for the same reason FieldHelp's own suite imports
// it: the hidden state of a pop-up lives in `.fieldhelp__pop[hidden]`, and
// against a stubbed sheet a permanently-open pop-up still passes.
import "@shared/design-system/components.css";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import {
  TOOLTIPS_KEY,
  TOOLTIP_PROMPTED_KEY,
  setTooltipsEnabled,
  storedTooltipsEnabled,
  tooltipsEnabledNode
} from "@shared/state/tooltipNodes";
import { HelpField } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import { TooltipOptOutDialog } from "@shared/components/TooltipOptOutDialog";
import { LoginPage } from "@shared/auth/LoginPage";
import { SettingsPage } from "@shared/pages/SettingsPage";
import { RetentionBadge } from "@shared/components/RetentionBadge";
import type { VersionInfo } from "@shared/types/operation";

/**
 * Issue #829: the global tooltip opt-out, end to end.
 *
 * Three properties are worth more than the rest and each has its own case
 * below, because an implementation can pass the easy ones without any of
 * them holding:
 *
 *   - The question is asked ONCE. A dialog on every close is nagging, and
 *     the flag that prevents it is separate persisted state, so "asked" is
 *     asserted across two different tooltips rather than twice on one.
 *   - Yes is GLOBAL. Asserted by hovering a tooltip that is not the one
 *     that was closed: an opt-out that only silences the pop-up it came
 *     from looks identical in every other test.
 *   - The sign-in screen carries none REGARDLESS of the preference, so it
 *     is asserted with tooltips explicitly on.
 *
 * The pop-up's presence is read the way FieldHelp's own suite reads it —
 * visibility against the shipped CSS, plus the close control's absence
 * from the accessibility tree while it is not on screen.
 */

const VERSION: VersionInfo = {
  api: "v1", service: "1.3.0", buildCommit: "9f4c1ab", goVersion: "go1.27.0",
  engine: "1.68.2", configRevision: "cfg_9f4c1ab", ready: true, compatible: true
};

/** Two explained fields and the app's one opt-out dialog, plus somewhere
 *  for its Settings link to land. Two fields, because "did the opt-out
 *  reach the OTHER tooltip" is the only question that separates a global
 *  preference from a local dismissal. */
function Harness() {
  return (
    <MemoryRouter>
      <Routes>
        <Route
          path="/"
          element={
            <div>
              <HelpField label="Keep" help={FIELD_HELP.tierKeep}>
                {(helpId) => <input className="input" aria-describedby={helpId} defaultValue="7" />}
              </HelpField>
              <HelpField label="Timezone" help={FIELD_HELP.retentionTimezone}>
                {(helpId) => <input className="input" aria-describedby={helpId} defaultValue="UTC" />}
              </HelpField>
            </div>
          }
        />
        <Route path="/settings" element={<p>The settings page</p>} />
      </Routes>
      <TooltipOptOutDialog />
    </MemoryRouter>
  );
}

const keepField = () => screen.getByLabelText("Keep");
const timezoneField = () => screen.getByLabelText("Timezone");
const keepPopup = () => screen.getByText(FIELD_HELP.tierKeep.effect);
const timezonePopup = () => screen.getByText(FIELD_HELP.retentionTimezone.effect);
const closeKeepHelp = () => screen.getByRole("button", { name: "Close help for Keep" });
const closeTimezoneHelp = () => screen.getByRole("button", { name: "Close help for Timezone" });
const optOutDialog = () => screen.queryByRole("dialog");

/** Closes a tooltip the way an operator does: hover it up, then press the
 *  pop-up's own "x". */
async function closeTooltipWithX(
  user: UserEvent,
  field: HTMLElement,
  close: () => HTMLElement
) {
  await user.hover(field);
  await user.click(close());
  await user.unhover(field);
}

describe("the tooltip preference", () => {
  beforeEach(() => {
    window.localStorage.clear();
    resetGraphForTests();
  });

  afterEach(() => {
    resetMockFixtures();
    cleanup();
    resetGraphForTests();
  });

  it("defaults to on, on a browser that has never said otherwise", () => {
    expect(storedTooltipsEnabled()).toBe(true);
    expect(graph.read(tooltipsEnabledNode)).toBe(true);
  });

  it("persists an opt-out, so the next load of this browser starts off", () => {
    setTooltipsEnabled(false);

    // The stored value is what a reload reads, and reading it back through
    // the loader is the whole persistence contract: the node is seeded
    // from this function at import.
    expect(window.localStorage.getItem(TOOLTIPS_KEY)).toBe("off");
    expect(storedTooltipsEnabled()).toBe(false);

    setTooltipsEnabled(true);
    expect(storedTooltipsEnabled()).toBe(true);
  });

  it("shows a pop-up with a close control while it is on", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.hover(keepField());

    expect(keepPopup()).toBeVisible();
    expect(closeKeepHelp()).toBeTruthy();
  });

  it("shows no pop-up at all while it is off", async () => {
    const user = userEvent.setup();
    setTooltipsEnabled(false);
    render(<Harness />);

    await user.hover(keepField());

    expect(keepPopup()).not.toBeVisible();
    expect(getComputedStyle(keepPopup().closest(".fieldhelp__pop") as HTMLElement).display).toBe("none");
    // Not merely invisible: there is no close control to reach either, by
    // pointer or by Tab.
    expect(screen.queryByRole("button", { name: "Close help for Keep" })).toBeNull();
  });
});

describe("closing a tooltip with its x", () => {
  beforeEach(() => {
    window.localStorage.clear();
    resetGraphForTests();
  });

  afterEach(() => {
    resetMockFixtures();
    cleanup();
    resetGraphForTests();
  });

  it("asks whether to turn tooltips off, the first time", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    expect(optOutDialog()).toBeNull();

    await closeTooltipWithX(user, keepField(), closeKeepHelp);

    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(screen.getByText("Turn off tooltips?")).toBeTruthy();
    expect(window.localStorage.getItem(TOOLTIP_PROMPTED_KEY)).toBe("1");
  });

  it("never asks again, on any tooltip, once it has been asked", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await closeTooltipWithX(user, keepField(), closeKeepHelp);
    await user.click(screen.getByRole("button", { name: "Keep showing tooltips" }));
    expect(optOutDialog()).toBeNull();

    // A different tooltip, so this cannot pass on per-field state.
    await closeTooltipWithX(user, timezoneField(), closeTimezoneHelp);

    expect(optOutDialog()).toBeNull();
  });

  it("turns every tooltip off when the answer is yes", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await closeTooltipWithX(user, keepField(), closeKeepHelp);
    await user.click(screen.getByRole("button", { name: "Yes, turn tooltips off" }));

    expect(optOutDialog()).toBeNull();
    expect(window.localStorage.getItem(TOOLTIPS_KEY)).toBe("off");

    // The tooltip that was never touched, which is where a merely local
    // dismissal would still pop up.
    await user.hover(timezoneField());
    expect(timezonePopup()).not.toBeVisible();
    expect(screen.queryByRole("button", { name: "Close help for Timezone" })).toBeNull();
  });

  it("leaves tooltips on when the answer is no", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await closeTooltipWithX(user, keepField(), closeKeepHelp);
    await user.click(screen.getByRole("button", { name: "Keep showing tooltips" }));

    await user.hover(timezoneField());
    expect(timezonePopup()).toBeVisible();
    expect(storedTooltipsEnabled()).toBe(true);
  });

  it("offers the settings page, and gets out of its way", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await closeTooltipWithX(user, keepField(), closeKeepHelp);
    await user.click(screen.getByRole("link", { name: "Settings" }));

    expect(screen.getByText("The settings page")).toBeTruthy();
    // A modal left up over the page it just navigated to is a dialog that
    // has to be dismissed before the toggle it pointed at can be used.
    expect(optOutDialog()).toBeNull();
  });
});

describe("the Settings toggle", () => {
  beforeEach(() => {
    window.localStorage.clear();
    resetGraphForTests();
  });

  afterEach(() => {
    resetMockFixtures();
    cleanup();
    resetGraphForTests();
  });

  const toggle = () => screen.getByRole("checkbox", { name: /Show tooltips/ });

  async function renderSettings() {
    act(() => {
      graph.commit("test/seed-version", (tx) =>
        tx.set(versionNode, { data: VERSION, error: null, loading: false })
      );
    });

    render(
      <MemoryRouter>
        <ApiProvider api={createMockApi()}>
          <PlatformProvider bridge={genericBridge}>
            <SettingsPage readOnly={false} />
          </PlatformProvider>
        </ApiProvider>
      </MemoryRouter>
    );
    await screen.findByLabelText("Timezone");
  }

  it("shows the stored preference and writes both answers back", async () => {
    const user = userEvent.setup();
    await renderSettings();

    expect(toggle()).toBeChecked();

    await user.click(toggle());
    expect(toggle()).not.toBeChecked();
    expect(window.localStorage.getItem(TOOLTIPS_KEY)).toBe("off");
    expect(graph.read(tooltipsEnabledNode)).toBe(false);

    await user.click(toggle());
    expect(toggle()).toBeChecked();
    expect(storedTooltipsEnabled()).toBe(true);
  });

  it("starts off for a browser that had already opted out", async () => {
    setTooltipsEnabled(false);

    await renderSettings();

    expect(toggle()).not.toBeChecked();
  });
});

describe("the sign-in screen", () => {
  beforeEach(() => {
    window.localStorage.clear();
    resetGraphForTests();
  });

  afterEach(() => {
    resetMockFixtures();
    cleanup();
    resetGraphForTests();
  });

  it("shows no tooltip on hover, with the preference explicitly on", async () => {
    const user = userEvent.setup();
    setTooltipsEnabled(true);

    render(
      <MemoryRouter>
        <ApiProvider api={createMockApi()}>
          <LoginPage onSignedIn={() => {}} />
        </ApiProvider>
      </MemoryRouter>
    );

    await user.hover(screen.getByLabelText("Username"));
    expect(screen.getByText(FIELD_HELP.loginUsername.effect)).not.toBeVisible();
    expect(screen.queryByRole("button", { name: "Close help for Username" })).toBeNull();

    await user.hover(screen.getByLabelText("Password"));
    expect(screen.getByText(FIELD_HELP.loginPassword.effect)).not.toBeVisible();
    expect(screen.queryByRole("button", { name: "Close help for Password" })).toBeNull();
  });

  it("cannot be talked into one by focusing a field either", async () => {
    const user = userEvent.setup();

    render(
      <MemoryRouter>
        <ApiProvider api={createMockApi()}>
          <LoginPage onSignedIn={() => {}} />
        </ApiProvider>
      </MemoryRouter>
    );

    await user.click(screen.getByLabelText("Username"));

    expect(screen.getByText(FIELD_HELP.loginUsername.effect)).not.toBeVisible();
  });
});

/**
 * The other kind of tooltip on these pages: hover copy handed to the
 * browser as a `title` attribute rather than drawn by FieldHelp. #829 says
 * tooltips "no longer appear on hover over anything", and a page still
 * handing the browser titles to draw does not satisfy that — so the
 * attribute itself has to go, not merely a pop-up element.
 */
describe("browser-drawn hover titles", () => {
  beforeEach(() => {
    window.localStorage.clear();
    resetGraphForTests();
  });

  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  it("carries its title while tooltips are on", () => {
    render(<RetentionBadge kind="protected" />);

    expect(screen.getByText(/Protected/).getAttribute("title")).toBe(
      "Newest known-good backup \u2014 never deleted by retention"
    );
  });

  it("withholds the title once tooltips are off, and keeps the badge", () => {
    setTooltipsEnabled(false);

    render(<RetentionBadge kind="protected" />);

    // The mark is not conditional — it is the badge, and it says something
    // about the backup that is true whether or not help is wanted.
    const badge = screen.getByText(/Protected/);
    expect(badge).toBeVisible();
    expect(badge.hasAttribute("title")).toBe(false);
  });
});
