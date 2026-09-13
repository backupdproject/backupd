import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { useState } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
// The real stylesheet, for the reason #834's and #829's suites import it:
// a pop-up's closed state is `.tooltip__pop[hidden]`, and against a stubbed
// sheet a permanently-open pop-up passes every visibility assertion.
import "@shared/design-system/components.css";
import { resetGraphForTests } from "@shared/state/graph";
import { setTooltipsEnabled } from "@shared/state/tooltipNodes";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import { TooltipAutoAttach } from "@shared/tooltips/TooltipAutoAttach";
import { TOOLTIPS } from "@shared/tooltips/tooltips";
import type { TooltipId } from "@shared/tooltips/tooltips";

/**
 * Issue #874: hover help that attaches itself.
 *
 * The typed host (`<InfoTooltip id>`) is the compile-time half of the
 * registry and stays exactly as it was. This is the other half: any
 * element that carries `data-tip="<id>"` gets the same pop-up from one
 * delegated layer mounted at the app root, and the rule that makes that
 * safe to sprinkle on markup is FAIL-SILENT — an id the registry does not
 * define shows nothing at all, rather than an empty pop-up or the id.
 *
 * The cases below are the contract:
 *
 *   - a registered id shows the registry's copy on hover and on focus,
 *     and the trigger points at it with aria-describedby;
 *   - an UNREGISTERED id shows nothing (the mutation-proved rule: making
 *     the layer fall back to the raw id fails "shows nothing at all");
 *   - no attribute shows nothing;
 *   - Escape, the pointer leaving and focus leaving each put it away;
 *   - there is one pop-up on screen at a time, whichever element the
 *     pointer arrived at last;
 *   - an element inside an explicit `<InfoTooltip>` is left to its host,
 *     so the two layers never both render the same entry.
 *
 * The copy is asserted against `TOOLTIPS[id].html` and never against
 * anything the fixture rendered, because "the pop-up shows the registry's
 * HTML and nothing assembled at run time" is the security property this
 * feature must not weaken (tooltips.ts module doc).
 */

/** Every pop-up on screen: `hidden` ones are the closed hosts, which are
 *  mounted all the time and are not what any of these cases is about. */
function shownPops(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>(".tooltip__pop")).filter(
    (pop) => !pop.hasAttribute("hidden")
  );
}

/** The one pop-up on screen, and a failure with a count when there is not
 *  exactly one — "only one at a time" is half of what is under test. */
function shownPop(): HTMLElement {
  const pops = shownPops();
  expect(pops.length, "pop-ups on screen").toBe(1);
  return pops[0];
}

const bodyOf = (pop: HTMLElement) => pop.querySelector(".tooltip__body") as HTMLElement;

/** A fixture, not a page: the layer is delegated and app-wide, so what it
 *  is wrapping does not matter — only the attributes do. The text of each
 *  element is deliberately NOT its tooltip's copy, so a pop-up that
 *  somehow rendered the trigger's own words could not pass for the
 *  registry's. */
function Fixture() {
  return (
    <TooltipAutoAttach>
      <button data-tip="shell.sign-out">Leave</button>
      <button data-tip="nav.sets">Sets</button>
      {/* An id no registry entry defines. The run-time path this feature
          exists to make safe. */}
      <button data-tip="nav.nothing-like-this">Mystery</button>
      {/* No attribute at all: the overwhelming majority of the app. */}
      <button>Plain</button>
      {/* A trigger with a subtree of its own, so the layer has to resolve
          the NEAREST ancestor carrying the attribute rather than the
          element the pointer happened to land on. */}
      <span data-tip="nav.backups">
        <em>Stored</em>
      </span>
      {/* Already explained by the typed host. The attribute is redundant
          here, and a layer that acted on it anyway would put two pop-ups
          with the same copy on screen. */}
      <InfoTooltip id="nav.dashboard">
        <button data-tip="nav.dashboard">Home</button>
      </InfoTooltip>
    </TooltipAutoAttach>
  );
}

beforeEach(() => {
  window.localStorage.clear();
  resetGraphForTests();
});

afterEach(() => {
  cleanup();
  resetGraphForTests();
});

describe("an element carrying data-tip for a registered id", () => {
  it("shows that entry's copy on hover", async () => {
    const user = userEvent.setup();
    render(<Fixture />);

    expect(shownPops()).toEqual([]);
    await user.hover(screen.getByRole("button", { name: "Leave" }));

    const pop = shownPop();
    expect(pop).toBeVisible();
    // From the registry, byte for byte, and not from anything on screen.
    expect(bodyOf(pop).innerHTML).toBe(TOOLTIPS["shell.sign-out"].html);
    expect(bodyOf(pop).querySelector("strong")?.textContent).toBe("not");
    expect(bodyOf(pop).textContent).not.toContain("<strong>");
    expect(bodyOf(pop).textContent).not.toContain("Leave");
  });

  it("names the pop-up with the entry's label", async () => {
    const user = userEvent.setup();
    render(<Fixture />);

    await user.hover(screen.getByRole("button", { name: "Leave" }));

    expect(screen.getByRole("tooltip", { name: "Sign out" })).toBe(shownPop());
  });

  it("points the trigger at the copy with aria-describedby while it is up", async () => {
    const user = userEvent.setup();
    render(<Fixture />);

    const trigger = screen.getByRole("button", { name: "Leave" });
    expect(trigger.getAttribute("aria-describedby")).toBeNull();

    await user.hover(trigger);

    const described = trigger.getAttribute("aria-describedby");
    expect(described).toBeTruthy();
    expect(document.getElementById(described as string)).toBe(bodyOf(shownPop()));

    // And gives it back on the way out: an id pointing at an element that
    // is no longer in the document is a broken description, which is
    // worse for a screen reader than no description at all.
    await user.unhover(trigger);
    expect(trigger.getAttribute("aria-describedby")).toBeNull();
  });

  it("shows the same copy for the keyboard, on focus", () => {
    render(<Fixture />);

    fireEvent.focusIn(screen.getByRole("button", { name: "Sets" }));

    expect(bodyOf(shownPop()).innerHTML).toBe(TOOLTIPS["nav.sets"].html);
  });

  it("resolves the nearest ancestor carrying the attribute", async () => {
    const user = userEvent.setup();
    render(<Fixture />);

    await user.hover(screen.getByText("Stored"));

    expect(bodyOf(shownPop()).innerHTML).toBe(TOOLTIPS["nav.backups"].html);
  });
});

describe("an element carrying data-tip for an id the registry does not define", () => {
  /** The rule this whole feature rests on. `data-tip` is meant to be
   *  cheap to add, including from data — a status name, a route — so the
   *  unknown id has to be a no-op and not a visible defect. */
  it("shows nothing at all on hover", async () => {
    const user = userEvent.setup();
    render(<Fixture />);

    const trigger = screen.getByRole("button", { name: "Mystery" });
    await user.hover(trigger);

    expect(shownPops()).toEqual([]);
    // Not the id, not an empty pop-up, and not a dangling description.
    expect(document.body.textContent).not.toContain("nav.nothing-like-this");
    expect(trigger.getAttribute("aria-describedby")).toBeNull();
  });

  it("shows nothing at all on focus", () => {
    render(<Fixture />);

    fireEvent.focusIn(screen.getByRole("button", { name: "Mystery" }));

    expect(shownPops()).toEqual([]);
  });
});

describe("an element carrying no data-tip", () => {
  it("is left alone", async () => {
    const user = userEvent.setup();
    render(<Fixture />);

    const trigger = screen.getByRole("button", { name: "Plain" });
    await user.hover(trigger);
    fireEvent.focusIn(trigger);

    expect(shownPops()).toEqual([]);
    expect(trigger.getAttribute("aria-describedby")).toBeNull();
  });
});

describe("putting an auto-attached pop-up away", () => {
  it("closes on Escape", async () => {
    const user = userEvent.setup();
    render(<Fixture />);

    await user.hover(screen.getByRole("button", { name: "Leave" }));
    expect(shownPops().length).toBe(1);

    await user.keyboard("{Escape}");

    expect(shownPops()).toEqual([]);
  });

  it("closes when the pointer leaves", async () => {
    const user = userEvent.setup();
    render(<Fixture />);

    const trigger = screen.getByRole("button", { name: "Leave" });
    await user.hover(trigger);
    await user.unhover(trigger);

    expect(shownPops()).toEqual([]);
  });

  it("stays up while the pointer is inside the trigger's own subtree", async () => {
    const user = userEvent.setup();
    render(<Fixture />);

    const inner = screen.getByText("Stored");
    await user.hover(inner);
    // The pointer crossing from the <em> to its wrapper is a pointerout
    // on the child, not a departure: hiding here is the bug where help
    // flickers off as the pointer moves across the thing it explains.
    //
    // Dispatched as a MouseEvent, which PointerEvent extends and which
    // is as close as this environment gets: jsdom implements no
    // PointerEvent at all, so fireEvent.pointerOut falls back to a plain
    // Event and drops `relatedTarget` — the one property this case is
    // about. Measured, not assumed.
    fireEvent(inner, new MouseEvent("pointerout", { bubbles: true, relatedTarget: inner.parentElement }));

    expect(bodyOf(shownPop()).innerHTML).toBe(TOOLTIPS["nav.backups"].html);
  });

  it("stays up while focus moves within the trigger's own subtree", () => {
    render(<Fixture />);

    const trigger = screen.getByText("Stored").parentElement as HTMLElement;
    fireEvent.focusIn(trigger);
    expect(shownPops().length).toBe(1);

    fireEvent.focusOut(trigger, { relatedTarget: screen.getByText("Stored") });

    expect(bodyOf(shownPop()).innerHTML).toBe(TOOLTIPS["nav.backups"].html);
  });

  it("closes when focus moves away", () => {
    render(<Fixture />);

    const trigger = screen.getByRole("button", { name: "Sets" });
    fireEvent.focusIn(trigger);
    expect(shownPops().length).toBe(1);

    fireEvent.focusOut(trigger, { relatedTarget: screen.getByRole("button", { name: "Plain" }) });

    expect(shownPops()).toEqual([]);
  });

  /** Measured in a real browser before it was a rule here: a pop-up the
   *  KEYBOARD opened must survive the mouse drifting across the page it
   *  is written over. The pointer arriving somewhere unexplained is not a
   *  statement about help somebody asked for another way; the pointer
   *  LEAVING the trigger is, and that is pointerout. */
  it("survives the pointer wandering over unexplained markup", async () => {
    const user = userEvent.setup();
    render(<Fixture />);

    fireEvent.focusIn(screen.getByRole("button", { name: "Sets" }));
    await user.hover(screen.getByRole("button", { name: "Plain" }));

    expect(bodyOf(shownPop()).innerHTML).toBe(TOOLTIPS["nav.sets"].html);
  });

  /** The exit with no event: a row re-renders away, a route changes, a
   *  list refreshes. No pointer left and no focus moved, so the only
   *  thing that can notice is the layer watching the document — and
   *  without it the pop-up stays on screen explaining an element that is
   *  no longer in it, anchored to a rectangle that will never move
   *  again. Nothing is hovered or focused in this case, so no other exit
   *  can account for it passing. */
  it("goes away when the element it explains is removed from the document", async () => {
    function Vanishing() {
      const [there, setThere] = useState(true);
      return (
        <TooltipAutoAttach>
          {there ? <button data-tip="nav.sets">Sets</button> : null}
          <button onClick={() => setThere(false)}>Remove</button>
        </TooltipAutoAttach>
      );
    }
    render(<Vanishing />);

    fireEvent.focusIn(screen.getByRole("button", { name: "Sets" }));
    expect(shownPops().length).toBe(1);

    fireEvent.click(screen.getByRole("button", { name: "Remove" }));

    await waitFor(() => expect(shownPops()).toEqual([]));
  });
});

describe("the one shared pop-up", () => {
  it("moves to the element the pointer arrived at, rather than stacking", async () => {
    const user = userEvent.setup();
    render(<Fixture />);

    await user.hover(screen.getByRole("button", { name: "Leave" }));
    await user.hover(screen.getByRole("button", { name: "Sets" }));

    // shownPop() asserts the count: one pop-up, and it is the second
    // element's.
    expect(bodyOf(shownPop()).innerHTML).toBe(TOOLTIPS["nav.sets"].html);
  });

  it("leaves an element inside an explicit InfoTooltip to its own host", async () => {
    const user = userEvent.setup();
    render(<Fixture />);

    const trigger = screen.getByRole("button", { name: "Home" });
    await user.hover(trigger);

    // One pop-up, carrying the entry once — the host's, which is the one
    // with a close control. A layer that also acted on the attribute
    // would render the same copy twice, on top of itself.
    const pop = shownPop();
    expect(bodyOf(pop).innerHTML).toBe(TOOLTIPS["nav.dashboard"].html);
    expect(pop.querySelector(".tooltip__close")).toBeTruthy();
    expect(
      Array.from(document.querySelectorAll<HTMLElement>(".tooltip__body")).filter(
        (body) => body.innerHTML === TOOLTIPS["nav.dashboard" as TooltipId].html
      ).length
    ).toBe(1);
  });
});

describe("the preference (issue #829)", () => {
  /** "Tooltips no longer appear on hover over anything" has to be true of
   *  a layer that attaches itself to markup, or #829's switch stops
   *  meaning what it says the first time somebody adds an attribute. */
  it("shows nothing on hover while tooltips are off", async () => {
    setTooltipsEnabled(false);
    const user = userEvent.setup();
    render(<Fixture />);

    await user.hover(screen.getByRole("button", { name: "Leave" }));

    expect(shownPops()).toEqual([]);
  });

  it("shows nothing on a surface that carries no hover help", async () => {
    const user = userEvent.setup();
    render(
      <TooltipAutoAttach>
        {/* What LoginPage marks its subtree with (hooks/useTooltips.ts'
            TooltipsSuppressed, in the DOM where a delegated layer can
            read it). */}
        <div data-tips="off">
          <button data-tip="shell.sign-out">Leave</button>
        </div>
      </TooltipAutoAttach>
    );

    await user.hover(screen.getByRole("button", { name: "Leave" }));

    expect(shownPops()).toEqual([]);
  });
});
