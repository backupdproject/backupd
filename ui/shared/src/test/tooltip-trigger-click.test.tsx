import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
// The real stylesheet, for the reason #829's and #834's suites import it:
// a pop-up's hidden state is `.tooltip__pop[hidden]`, and against a stubbed
// sheet a permanently-open pop-up passes every visibility assertion there
// is.
import "@shared/design-system/components.css";
import { resetGraphForTests } from "@shared/state/graph";
import { TOOLTIP_PROMPTED_KEY, setTooltipsEnabled } from "@shared/state/tooltipNodes";
import { TooltipsSuppressed } from "@shared/hooks/useTooltips";
import { TooltipOptOutDialog } from "@shared/components/TooltipOptOutDialog";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import { TOOLTIPS } from "@shared/tooltips/tooltips";
import type { TooltipId } from "@shared/tooltips/tooltips";

/**
 * Issue #839: the icon-only host (`.tooltip__trigger`) is a CLICK
 * affordance, and a click affordance is not hover help.
 *
 * #829 gave the operator one switch that means "tooltips no longer appear
 * on hover over anything", and #834 then put hover help on a few hundred
 * elements. The switch keeps its meaning here: nothing this suite does
 * with a pointer RESTING on something opens a pop-up while it is off.
 * What the switch never covered is an element whose entire reason to
 * exist is "press this to read the explanation" — the operator asking a
 * direct question, once, about one thing. Off, that is the only way in;
 * on, it is one of two.
 *
 * The case that separates a real implementation from a plausible one is
 * the opt-out: a pop-up the operator click-opened WHILE TOOLTIPS ARE OFF,
 * closed again with its own "x", must not be answered with "turn off
 * tooltips?". They are off. Asking would also burn #829's one question,
 * so the assertion is both halves — no dialog now, and the question still
 * unasked afterwards.
 */

/** The icon-only host, plus a wrapped control next to it so "hovering
 *  something that is NOT a trigger" is a question this harness can ask,
 *  and the app's one opt-out dialog so a close can be answered. */
function Harness({ suppressed = false }: { suppressed?: boolean }) {
  return (
    <MemoryRouter>
      {/* The provider the sign-in screen applies to itself
          (auth/LoginPage.tsx): a surface that carries no tooltips at all,
          whatever the stored preference says. */}
      <TooltipsSuppressed.Provider value={suppressed}>
        <InfoTooltip id="nav.sets" />
        <InfoTooltip id="shell.sign-out">
          <button type="button">Sign out</button>
        </InfoTooltip>
      </TooltipsSuppressed.Provider>
      <TooltipOptOutDialog />
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

/** The icon host for the backup-sets entry, named the way #834 names it. */
const trigger = () => screen.getByRole("button", { name: "Explain Backup sets" });

beforeEach(() => {
  window.localStorage.clear();
  resetGraphForTests();
});

afterEach(() => {
  cleanup();
  resetGraphForTests();
});

describe("a tooltip trigger while tooltips are off", () => {
  beforeEach(() => setTooltipsEnabled(false));

  it("shows nothing while the pointer merely rests on it", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.hover(trigger());

    expect(popOf("nav.sets")).not.toBeVisible();
    // Not merely invisible: there is nothing to reach by pointer or Tab.
    expect(screen.queryByRole("button", { name: "Close help for Backup sets" })).toBeNull();
  });

  it("shows its registry entry's copy when it is clicked", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.click(trigger());

    expect(popOf("nav.sets")).toBeVisible();
    expect(bodyOf("nav.sets").textContent).toContain("one source, its schedule");
    expect(screen.getByRole("button", { name: "Close help for Backup sets" })).toBeTruthy();
  });

  it("still shows nothing on hover over an element that is not a trigger", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.hover(screen.getByRole("button", { name: "Sign out" }));

    expect(popOf("shell.sign-out")).not.toBeVisible();
  });

  it("leaves the other tooltips alone when one is click-opened", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.click(trigger());

    expect(popOf("shell.sign-out")).not.toBeVisible();
  });

  it("puts it away again on a second click", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.click(trigger());
    await user.click(trigger());

    expect(popOf("nav.sets")).not.toBeVisible();
  });

  it("does not offer the opt-out when the operator closes it again", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.click(trigger());
    await user.click(screen.getByRole("button", { name: "Close help for Backup sets" }));

    expect(popOf("nav.sets")).not.toBeVisible();
    // #829's question, asked about a preference that already says no.
    expect(screen.queryByRole("dialog")).toBeNull();
    // And not silently spent either: the operator who turns tooltips back
    // on has still never been asked.
    expect(window.localStorage.getItem(TOOLTIP_PROMPTED_KEY)).toBeNull();
  });
});

describe("a tooltip trigger while tooltips are on", () => {
  beforeEach(() => setTooltipsEnabled(true));

  it("shows its copy on hover", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.hover(trigger());

    expect(popOf("nav.sets")).toBeVisible();
  });

  it("shows its copy on a click as well", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.click(trigger());

    expect(popOf("nav.sets")).toBeVisible();
  });

  it("still offers the global opt-out the first time one is closed with its x", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.click(trigger());
    await user.click(screen.getByRole("button", { name: "Close help for Backup sets" }));

    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(screen.getByText("Turn off tooltips?")).toBeTruthy();
  });
});

describe("a surface that carries no tooltips at all", () => {
  it("renders no trigger to click, with tooltips explicitly on", () => {
    setTooltipsEnabled(true);
    render(<Harness suppressed />);

    expect(screen.queryByRole("button", { name: "Explain Backup sets" })).toBeNull();
  });
});
