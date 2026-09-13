import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
// The real stylesheet, for the reason #829's, #834's and #839's suites
// import it: a pop-up's hidden state is `.tooltip__pop[hidden]`, and
// against a stubbed sheet a permanently-open pop-up passes every
// visibility assertion there is. Here it carries a second load as well —
// `position: fixed` on the pop-up is the half of this fix that CSS owns.
import "@shared/design-system/components.css";
import { resetGraphForTests } from "@shared/state/graph";
import { setTooltipsEnabled } from "@shared/state/tooltipNodes";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import { TOOLTIPS } from "@shared/tooltips/tooltips";
import type { TooltipId } from "@shared/tooltips/tooltips";

/**
 * Issue #847: a tooltip pop-up floats above everything, inside the
 * viewport, wherever its trigger happens to sit.
 *
 * The defect was structural rather than cosmetic: the pop-up rendered
 * inside its trigger's own subtree, so the card around it clipped it
 * (`overflow: hidden`) and the next card, being later in the document,
 * painted over what was left. No z-index on the pop-up could fix either
 * half, because both are decided by ancestors it cannot see. The fix is
 * to take the pop-up out of that subtree entirely — a React portal into
 * one top-level layer — and to position it from the trigger's viewport
 * rectangle instead of from an ancestor's padding box.
 *
 * So the cases here are about WHERE the element ends up:
 *
 *   - out of the clipping container, in the shared layer;
 *   - above the trigger by default, flipped below when there is no room
 *     above, clamped so no edge of it leaves the viewport;
 *   - moved when the page scrolls or the window resizes, because a fixed
 *     element that is not recomputed detaches from the thing it explains;
 *   - still the element the trigger's aria-describedby names, which is
 *     the one property the move could silently have broken.
 *
 * The interaction itself (#834's hover gating, #839's click-to-open with
 * tooltips off, #829's opt-out) is asserted by its own suites and is
 * unchanged; the case at the bottom here is the one that would catch the
 * portal breaking "a click inside the pop-up is not a click outside it",
 * which is the one interaction rule that WAS stated in terms of the DOM
 * nesting this issue removes.
 */

/** The geometry jsdom does not have. Every rectangle in this suite comes
 *  from here: jsdom lays nothing out, so `getBoundingClientRect` answers
 *  zeroes for everything and a position computed from zeroes is not an
 *  assertion about anything. */
interface Box {
  top: number;
  left: number;
  width: number;
  height: number;
}

const VIEWPORT = { width: 1000, height: 800 };
/** The pop-up's own size, which the placement has to make room for. */
const POP: Box = { top: 0, left: 0, width: 300, height: 80 };
/** The margin this fix keeps between the pop-up and the viewport edge. */
const MARGIN = 8;

const rect = (b: Box): DOMRect =>
  ({
    x: b.left,
    y: b.top,
    top: b.top,
    left: b.left,
    width: b.width,
    height: b.height,
    bottom: b.top + b.height,
    right: b.left + b.width,
    toJSON: () => ({})
  }) as DOMRect;

/** The trigger's rectangle, moved by each case to put it in the middle of
 *  the screen, hard against the top, or hard against an edge. */
let triggerBox: Box = { top: 400, left: 120, width: 140, height: 30 };

beforeEach(() => {
  window.localStorage.clear();
  resetGraphForTests();
  setTooltipsEnabled(true);
  triggerBox = { top: 400, left: 120, width: 140, height: 30 };

  Object.defineProperty(window, "innerWidth", { configurable: true, value: VIEWPORT.width });
  Object.defineProperty(window, "innerHeight", { configurable: true, value: VIEWPORT.height });
  Object.defineProperty(Element.prototype, "getBoundingClientRect", {
    configurable: true,
    value(this: Element) {
      if (this.classList.contains("tooltip__pop")) return rect(POP);
      if (this.classList.contains("tooltip")) return rect(triggerBox);
      return rect({ top: 0, left: 0, width: 0, height: 0 });
    }
  });
});

afterEach(() => {
  cleanup();
  resetGraphForTests();
});

/** A card exactly like the ones this issue was reported against: it clips
 *  what overflows it, and another card follows it in the document. */
function Card({ id = "storage.add-destination" as TooltipId }) {
  return (
    <>
      <div className="card" data-testid="card" style={{ overflow: "hidden" }}>
        <InfoTooltip id={id}>
          <button type="button">Add a destination</button>
        </InfoTooltip>
      </div>
      <div className="card" data-testid="next-card">
        <p>the card that used to cover it</p>
      </div>
    </>
  );
}

function bodyOf(id: TooltipId): HTMLElement {
  const bodies = Array.from(document.querySelectorAll<HTMLElement>(".tooltip__body"));
  const match = bodies.find((node) => node.innerHTML === TOOLTIPS[id].html);
  if (!match) throw new Error("no tooltip body rendering " + id);
  return match;
}

const popOf = (id: TooltipId) => bodyOf(id).closest(".tooltip__pop") as HTMLElement;

/** The numbers the component wrote, as numbers. */
function placed(pop: HTMLElement) {
  const top = Number.parseFloat(pop.style.top);
  const left = Number.parseFloat(pop.style.left);
  return {
    // From the stylesheet rather than from the element: which positioning
    // scheme the pop-up uses is the CSS half of this fix, and an inline
    // `position` would assert that the test set it.
    position: getComputedStyle(pop).position,
    top,
    left,
    bottom: top + POP.height,
    right: left + POP.width
  };
}

async function open() {
  const user = userEvent.setup();
  render(<Card />);
  await user.hover(screen.getByRole("button", { name: "Add a destination" }));
  return popOf("storage.add-destination");
}

describe("a tooltip pop-up inside a card that clips", () => {
  it("renders outside that card, in the shared top-level layer", async () => {
    const pop = await open();

    const card = screen.getByTestId("card");
    expect(card.contains(pop)).toBe(false);
    expect(pop.closest(".card")).toBeNull();
    expect(pop.closest(".tooltip")).toBeNull();
    // Somewhere it cannot be clipped or painted over: a child of the one
    // layer this app puts at the end of <body>.
    expect(pop.parentElement?.classList.contains("tooltip-layer")).toBe(true);
    expect(pop.parentElement?.parentElement).toBe(document.body);
    expect(pop).toBeVisible();
  });

  it("is still the element the trigger's aria-describedby names", async () => {
    const pop = await open();

    const described = screen
      .getByRole("button", { name: "Add a destination" })
      .getAttribute("aria-describedby");
    expect(described).toBeTruthy();
    const target = document.getElementById(described as string);
    expect(target).toBe(bodyOf("storage.add-destination"));
    expect(pop.contains(target)).toBe(true);
  });

  it("is taken out of the document when its host unmounts", async () => {
    await open();
    expect(document.querySelectorAll(".tooltip__pop").length).toBe(1);

    cleanup();

    // The layer may stay; a pop-up belonging to a component that is gone
    // may not, or every page transition leaves one behind.
    expect(document.querySelectorAll(".tooltip__pop").length).toBe(0);
  });
});

describe("where the pop-up is placed", () => {
  it("sits above its trigger, fixed to the viewport", async () => {
    const pop = await open();
    const at = placed(pop);

    expect(at.position).toBe("fixed");
    expect(at.bottom).toBeLessThanOrEqual(triggerBox.top);
    expect(at.left).toBe(triggerBox.left);
    expect(at.top).toBeGreaterThanOrEqual(MARGIN);
  });

  it("flips below a trigger with no room above it", async () => {
    triggerBox = { top: 6, left: 120, width: 140, height: 30 };

    const at = placed(await open());

    // Below, not merely pushed down: it must not cover the control it
    // explains.
    expect(at.top).toBeGreaterThanOrEqual(triggerBox.top + triggerBox.height);
    expect(at.bottom).toBeLessThanOrEqual(VIEWPORT.height - MARGIN);
  });

  it("clamps left for a trigger against the right edge", async () => {
    triggerBox = { top: 400, left: 960, width: 30, height: 30 };

    const at = placed(await open());

    expect(at.right).toBeLessThanOrEqual(VIEWPORT.width - MARGIN);
    expect(at.left).toBeGreaterThanOrEqual(MARGIN);
  });

  it("clamps right for a trigger against the left edge", async () => {
    triggerBox = { top: 400, left: 0, width: 24, height: 24 };

    const at = placed(await open());

    expect(at.left).toBeGreaterThanOrEqual(MARGIN);
  });

  it("stays inside the viewport for a trigger at the very bottom", async () => {
    triggerBox = { top: 790, left: 900, width: 90, height: 24 };

    const at = placed(await open());

    expect(at.top).toBeGreaterThanOrEqual(MARGIN);
    expect(at.bottom).toBeLessThanOrEqual(VIEWPORT.height - MARGIN);
    expect(at.left).toBeGreaterThanOrEqual(MARGIN);
    expect(at.right).toBeLessThanOrEqual(VIEWPORT.width - MARGIN);
  });

  it("follows its trigger when the page scrolls", async () => {
    const pop = await open();
    const before = placed(pop).top;

    // The page moves under a pop-up that is fixed to the viewport: unless
    // it is recomputed it stays where it was and points at nothing.
    triggerBox = { ...triggerBox, top: triggerBox.top - 120 };
    act(() => {
      fireEvent.scroll(document, {});
    });

    expect(placed(pop).top).toBe(before - 120);
  });

  it("is recomputed when the window is resized", async () => {
    triggerBox = { top: 400, left: 960, width: 30, height: 30 };
    const pop = await open();

    Object.defineProperty(window, "innerWidth", { configurable: true, value: 400 });
    act(() => {
      fireEvent(window, new Event("resize"));
    });

    expect(placed(pop).right).toBeLessThanOrEqual(400 - MARGIN);
  });
});

describe("the interaction the move could have broken", () => {
  it("keeps a pop-up open when the pop-up itself is clicked", async () => {
    // "Outside" is a DOM containment test, and the pop-up is no longer in
    // the host's subtree: a portal that the outside-click rule does not
    // know about closes the pop-up on a click aimed at reading it.
    const user = userEvent.setup();
    render(<Card />);
    await user.click(screen.getByRole("button", { name: "Add a destination" }));

    const pop = popOf("storage.add-destination");
    await user.click(bodyOf("storage.add-destination"));

    expect(pop).toBeVisible();
  });

  it("closes on a click somewhere else entirely", async () => {
    const user = userEvent.setup();
    render(<Card />);
    await user.click(screen.getByRole("button", { name: "Add a destination" }));
    await user.click(bodyOf("storage.add-destination"));

    await user.click(screen.getByText("the card that used to cover it"));

    expect(popOf("storage.add-destination")).not.toBeVisible();
  });
});
