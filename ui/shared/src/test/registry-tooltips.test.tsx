import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
// The real stylesheet, for the reason #829's suite imports it and #278's
// before that: a pop-up's hidden state is `.tooltip__pop[hidden]`, and
// against a stubbed sheet a permanently-open pop-up still passes every
// visibility assertion.
import "@shared/design-system/components.css";
import { ApiProvider } from "@shared/api/ApiContext";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { resetGraphForTests } from "@shared/state/graph";
import { setTooltipsEnabled } from "@shared/state/tooltipNodes";
import { TooltipsSuppressed } from "@shared/hooks/useTooltips";
import { TooltipOptOutDialog } from "@shared/components/TooltipOptOutDialog";
import { AppShell } from "@shared/layouts/AppShell";
import { PageHeader } from "@shared/components/PageHeader";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import { TOOLTIPS, TOOLTIP_IDS, lookupTooltip } from "@shared/tooltips/tooltips";
import type { TooltipId } from "@shared/tooltips/tooltips";
import type { BackupdApi } from "@shared/api/contracts";

/**
 * Issue #834: the central tooltip registry, and the four things that have
 * to be true of every element wired to it.
 *
 *   - The copy comes from the REGISTRY. An element names an id; what an
 *     operator reads is whatever that id says today. Asserted through a
 *     real wired element (the shell's nav), not through the component in
 *     isolation, because "is it wired" is the half that can silently rot.
 *   - It is rendered as HTML. `<strong>` has to arrive as an element; a
 *     pop-up showing the angle brackets is the failure this is here for.
 *   - The #829 preference still governs it, including the sign-in screen's
 *     blanket suppression, which is checked with the preference explicitly
 *     ON so a passing case cannot be the preference doing the work.
 *   - The #829 opt-out is still offered from the "x", now that there are
 *     several hundred more "x"s to press.
 *
 * The registry's own integrity — no dead entries, no tag outside the
 * handful the pop-up styles and can render safely — is asserted as a sweep
 * over the file and the sources, for the reason icon-artwork.test.tsx
 * gives about its own: what is being asserted is true of EVERY entry, and
 * a rendered sweep only sees the branches some test happens to drive.
 */

const quietApi = {
  getLiveActivity: () =>
    Promise.resolve({
      observedAt: "2026-09-07T14:02:41Z",
      epoch: "one-process",
      pollAfterMs: 10_000,
      sets: [],
      deployment: null
    })
} as unknown as BackupdApi;

/** The signed-in shell, which is where most of #834's wiring lives. */
function Shell({ suppressed = false }: { suppressed?: boolean }) {
  return (
    <MemoryRouter>
      <PlatformProvider bridge={genericBridge}>
        <ApiProvider api={quietApi}>
          {/* The provider the sign-in screen applies to itself
              (auth/LoginPage.tsx), around a subtree that is full of
              registry tooltips — which is the only way to tell a screen
              that suppresses them from a screen that has none. */}
          <TooltipsSuppressed.Provider value={suppressed}>
            <AppShell
              health={{ serviceRunning: true } as never}
              version={null}
              counts={{ sets: 3, backups: 12, quarantine: 1 }}
              theme="dark"
              onToggleTheme={() => {}}
              onSignOut={() => {}}
            >
              <p>a page</p>
            </AppShell>
          </TooltipsSuppressed.Provider>
          <TooltipOptOutDialog />
        </ApiProvider>
      </PlatformProvider>
    </MemoryRouter>
  );
}

/** The pop-up body carrying a registry entry's copy, found by that copy. */
function bodyOf(id: TooltipId): HTMLElement {
  const bodies = Array.from(document.querySelectorAll<HTMLElement>(".tooltip__body"));
  const match = bodies.find((node) => node.innerHTML === TOOLTIPS[id].html);
  if (!match) throw new Error("no tooltip body rendering " + id);
  return match;
}

const popOf = (id: TooltipId) => bodyOf(id).closest(".tooltip__pop") as HTMLElement;

beforeEach(() => {
  window.localStorage.clear();
  resetGraphForTests();
});

afterEach(() => {
  cleanup();
  resetGraphForTests();
});

describe("the registry loader", () => {
  it("answers an entry for an id it defines", () => {
    const entry = lookupTooltip("shell.sign-out");
    expect(entry?.label).toBe("Sign out");
    expect(entry?.html).toContain("<strong>");
  });

  it("answers undefined for an id it does not, including inherited names", () => {
    expect(lookupTooltip("nav.nothing-like-this")).toBeUndefined();
    // `toString` and `constructor` are on every object's prototype. A
    // lookup that reaches them hands a host a function where an entry
    // should be, which is a crash rather than a missing tooltip.
    expect(lookupTooltip("toString")).toBeUndefined();
    expect(lookupTooltip("constructor")).toBeUndefined();
  });

  it("renders the wrapped element and no host at all for an unknown id", () => {
    // The run-time path: an id assembled from data that has no entry yet.
    render(
      <InfoTooltip id={"nav.nothing-like-this" as TooltipId}>
        <button>Run now</button>
      </InfoTooltip>
    );

    expect(screen.getByRole("button", { name: "Run now" })).toBeTruthy();
    expect(document.querySelector(".tooltip__pop")).toBeNull();
  });
});

describe("the registry's contents", () => {
  it("gives every entry copy to show", () => {
    for (const id of TOOLTIP_IDS) {
      expect(TOOLTIPS[id].html.replace(/<[^>]*>/g, "").trim().length, id).toBeGreaterThan(0);
    }
  });

  /** The copy is injected as HTML, so what it is allowed to contain is a
   *  safety property and not a style one. Only the tags the pop-up styles
   *  are permitted, none of which can run anything or fetch anything —
   *  there is no <script>, no <img>, no attribute at all. */
  it("uses only the inline tags the pop-up can render safely", () => {
    const ALLOWED: Record<string, true> = { strong: true, em: true, code: true, br: true };
    for (const id of TOOLTIP_IDS) {
      for (const [, tag] of TOOLTIPS[id].html.matchAll(/<\/?([a-zA-Z0-9-]*)[^>]*>/g)) {
        expect(ALLOWED[tag.toLowerCase()] === true, id + " uses <" + tag + ">").toBe(true);
      }
      expect(TOOLTIPS[id].html, id).not.toMatch(/<[a-zA-Z]+\s+[a-zA-Z-]+=/);
    }
  });

  /** Dead copy is worse than no copy: it reads as a description of
   *  something on screen and describes something that was deleted. Both
   *  directions are covered — a missing id does not compile, and an entry
   *  nothing names fails here. */
  it("has no entry that nothing in the app names", () => {
    const sources: Record<string, string> = import.meta.glob("../**/*.{ts,tsx}", {
      query: "?raw",
      import: "default",
      eager: true
    });
    const wiring = Object.entries(sources)
      .filter(([path]) => !path.includes("/test/") && !path.endsWith(".test.ts") && !path.endsWith(".test.tsx"))
      .map(([, text]) => text)
      .join("\n");

    const orphans = TOOLTIP_IDS.filter((id) => !wiring.includes('"' + id + '"'));
    expect(orphans).toEqual([]);
  });
});

describe("a wired element", () => {
  it("shows its registry entry's copy on hover", async () => {
    const user = userEvent.setup();
    render(<Shell />);

    await user.hover(screen.getByRole("link", { name: /Backup sets/ }));

    expect(popOf("nav.sets")).toBeVisible();
    expect(bodyOf("nav.sets").textContent).toContain("one source, its schedule");
  });

  it("renders the entry's HTML as markup rather than as text", async () => {
    const user = userEvent.setup();
    render(<Shell />);

    await user.hover(screen.getByRole("button", { name: "Sign out" }));

    const body = bodyOf("shell.sign-out");
    expect(body.querySelector("strong")?.textContent).toBe("not");
    // The failure this case exists for: the copy arriving escaped, so an
    // operator reads the tag names.
    expect(body.textContent).not.toContain("<strong>");
  });

  it("describes the control it wraps, so the copy is announced with it", () => {
    render(<Shell />);

    const described = screen.getByRole("button", { name: "Sign out" }).getAttribute("aria-describedby");
    expect(described).toBeTruthy();
    expect(document.getElementById(described as string)).toBe(bodyOf("shell.sign-out"));
  });

  /**
   * The rule that is not obvious and was measured rather than assumed: an
   * accessible name is computed by walking the element's subtree, and an
   * embedded control contributes its own name to the result. A tooltip
   * host inside a heading, a label or a link therefore renames it — to
   * "Backup sets Explain Backup sets", and in an environment without the
   * stylesheet to the whole tooltip's copy as well. Every page and every
   * nav row in this app is found by that name.
   */
  it("leaves the name of the thing it explains exactly as it was", () => {
    render(<Shell />);

    // The nav rows wrap; the pop-up is a sibling of the link, not a
    // descendant of it.
    // Exactly the label and the count the row already rendered before
    // #834, and nothing the tooltip added.
    expect(screen.getByRole("link", { name: "Backup sets 3" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Quarantine 1" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Sign out" })).toBeTruthy();
  });

  it("keeps a page heading's name when the page carries an explanation", () => {
    render(
      <MemoryRouter>
        <PageHeader title="Backups" tip="nav.backups" />
      </MemoryRouter>
    );

    expect(screen.getByRole("heading", { level: 1, name: "Backups" })).toBeTruthy();
    expect(bodyOf("nav.backups")).toBeTruthy();
  });
});

describe("the preference (issue #829)", () => {
  it("shows nothing on hover while tooltips are off", async () => {
    const user = userEvent.setup();
    setTooltipsEnabled(false);
    render(<Shell />);

    await user.hover(screen.getByRole("button", { name: "Sign out" }));

    expect(popOf("shell.sign-out")).not.toBeVisible();
    expect(getComputedStyle(popOf("shell.sign-out")).display).toBe("none");
    // Not merely invisible: there is no close control to reach either, by
    // pointer or by Tab.
    expect(screen.queryByRole("button", { name: "Close help for Sign out" })).toBeNull();
  });

  it("shows nothing on a suppressed surface even with tooltips on", async () => {
    const user = userEvent.setup();
    setTooltipsEnabled(true);
    render(<Shell suppressed />);

    await user.hover(screen.getByRole("link", { name: /Quarantine/ }));

    expect(popOf("nav.quarantine")).not.toBeVisible();
    expect(screen.queryByRole("button", { name: "Close help for Quarantine" })).toBeNull();
  });
});

describe("closing a registry tooltip with its x", () => {
  it("offers the global opt-out, and taking it silences every other one", async () => {
    const user = userEvent.setup();
    render(<Shell />);

    await user.hover(screen.getByRole("button", { name: "Sign out" }));
    await user.click(screen.getByRole("button", { name: "Close help for Sign out" }));

    // #829's question, raised from #834's tooltip.
    expect(screen.getByRole("dialog")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Yes, turn tooltips off" }));

    // A different tooltip entirely: an opt-out that only silenced the
    // pop-up it came from would look identical without this.
    await user.hover(screen.getByRole("link", { name: /Quarantine/ }));
    expect(popOf("nav.quarantine")).not.toBeVisible();
  });
});
