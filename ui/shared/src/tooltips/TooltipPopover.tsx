/**
 * One registry pop-up: the portalled surface, where it sits, and the copy
 * inside it. Everything two tooltip layers have to agree about.
 *
 * Issue #874 added the second layer. Until it there was one host
 * (`InfoTooltip`), and the pop-up was 60 lines in the middle of it: the
 * overlay layer, the measure-on-open-scroll-and-resize effect, the
 * `dangerouslySetInnerHTML` of the registry's copy, the info glyph. A
 * second layer that attaches itself to `data-tip` markup needs all of
 * that and none of the rest of the host, and a copy of it is the wrong
 * answer twice over: two pop-ups that drift apart visually are an obvious
 * defect, and two places that inject HTML are a security surface that has
 * to be argued twice instead of once.
 *
 * So this component is the pop-up and nothing else. It decides:
 *
 *   - WHERE: `placePopover` against the anchor's current rectangle,
 *     re-measured on scroll (in the capture phase, because these pages
 *     scroll a main element whose scroll does not bubble) and on resize;
 *   - WHAT IT LOOKS LIKE: `.tooltip__pop` and its furniture, so the
 *     auto-attached pop-up and the typed host's are the same object to an
 *     operator;
 *   - WHAT THE COPY IS: `entry.html`, injected. This is the ONLY
 *     `dangerouslySetInnerHTML` in the tooltip system, and the registry's
 *     module doc is where the rule that keeps it safe is argued: every
 *     string comes from the committed JSON, and nothing from an API, an
 *     operator, a path or the DOM is ever concatenated into it. Taking a
 *     `TooltipEntry` rather than a string is part of that — a caller
 *     cannot hand this component copy it assembled, only an entry it
 *     looked up.
 *
 * It decides nothing about WHEN it is open, which stays with the caller:
 * the typed host has a four-boolean hover/pin/dismiss machine
 * (`useHoverPopover`) and keeps its pop-up mounted while closed so the
 * control it describes always has an element to point at, while #874's
 * delegated layer mounts one pop-up on hover and unmounts it on leave.
 * Those are genuinely different lifecycles and neither belongs here.
 *
 * Its own controls stay with the caller too, as `children`: the typed
 * host's close "x" exists because its pop-up can be pinned open, and an
 * auto-attached pop-up that disappears when the pointer leaves has
 * nothing to close — a close button on it would be unreachable by pointer
 * and, worse, a tab stop that blurs the trigger and takes the pop-up down
 * with it.
 */
import { createPortal } from "react-dom";
import { useLayoutEffect, useRef, useState } from "react";
import type { MutableRefObject, ReactNode } from "react";
import { Icon } from "@shared/design-system/icons";
import type { TooltipEntry } from "@shared/tooltips/tooltips";
import { placePopover, tooltipLayer } from "@shared/tooltips/popover";
import type { PopoverPlacement } from "@shared/tooltips/popover";

/** The element the pop-up hangs off, as the thing both callers already
 *  have: a ref for the host that renders its own anchor, and a box around
 *  the current trigger for the delegated layer, which is handed an
 *  element by an event. Read-only, because this component positions
 *  against the anchor and never touches it. */
export interface TooltipAnchor {
  readonly current: HTMLElement | null;
}

export interface TooltipPopoverProps {
  /** The registry entry to show. An entry, not a string: see the module
   *  doc on why the injected HTML cannot be assembled by a caller. */
  entry: TooltipEntry;
  /** The id of the body element, for the trigger's aria-describedby. The
   *  caller owns it because the caller is what puts it on the trigger. */
  bodyId: string;
  anchor: TooltipAnchor;
  /** Whether it is on screen. A closed pop-up stays mounted and hidden,
   *  which is what lets a trigger describe itself while closed. */
  open: boolean;
  /** Pins the pop-up's right edge to the anchor's rather than its left,
   *  for an anchor at the right-hand end of a row. A request, not a rule:
   *  the viewport clamp overrides it (tooltips/popover.ts). */
  alignEnd?: boolean;
  /** Receives the pop-up element. Not optional for a caller that answers
   *  "did the operator click outside" by DOM containment, since a
   *  portalled pop-up is not inside its own host. */
  popRef?: MutableRefObject<HTMLElement | null>;
  /** An accessible name for a pop-up that nothing else names, which makes
   *  it a `tooltip` in the accessibility tree as well. #874's layer
   *  passes the entry's label; the typed host passes nothing, because its
   *  pop-up is reached through the aria-describedby of the control it
   *  explains and a second name for the same copy is noise. */
  label?: string;
  /** A click on the pop-up itself — the typed host pins it here so copy
   *  can be read while the pointer travels. */
  onClick?: () => void;
  /** The caller's own controls, rendered after the copy. */
  children?: ReactNode;
}

export function TooltipPopover({
  entry,
  bodyId,
  anchor,
  open,
  alignEnd,
  popRef,
  label,
  onClick,
  children
}: TooltipPopoverProps) {
  // The shared overlay layer, resolved once per pop-up: `useState`'s lazy
  // initialiser rather than a call in the body, so a re-render does not
  // go looking for it on every pass.
  const [layer] = useState(tooltipLayer);

  // The pop-up's own element, for measuring it. Held here rather than
  // taken from the caller because measurement is this component's job;
  // `popRef` is a copy for the caller that needs one.
  const pop = useRef<HTMLElement | null>(null);

  // Where the portalled pop-up sits in the viewport, `null` until it has
  // been measured at least once — which is what the hidden first frame
  // below is about, since an unmeasured fixed element paints in the
  // corner of the screen.
  //
  // A closed pop-up keeps the last numbers rather than clearing them.
  // Not laziness: this runs BEFORE paint, so re-opening measures again in
  // the same commit and the stale position is never on screen, and
  // clearing it would be a setState in an effect body for a value nothing
  // can see.
  const [placement, setPlacement] = useState<PopoverPlacement | null>(null);
  useLayoutEffect(() => {
    if (!open) return;
    const measure = () => {
      const host = anchor.current;
      const surface = pop.current;
      if (!host || !surface) return;
      const next = placePopover(
        host.getBoundingClientRect(),
        surface.getBoundingClientRect(),
        { width: window.innerWidth, height: window.innerHeight },
        alignEnd === true
      );
      // Same numbers, same object: a scroll fires this dozens of times a
      // second and a fresh object each time re-renders every open tooltip
      // for nothing.
      setPlacement((prev) => (prev && prev.top === next.top && prev.left === next.left ? prev : next));
    };
    measure();
    // Capture, because the thing that scrolls is usually not the window:
    // these pages scroll a main element, and a scroll event from one does
    // not bubble. Fixed to the viewport, the pop-up would otherwise stay
    // where it was while the control it explains moved out from under it.
    window.addEventListener("scroll", measure, true);
    window.addEventListener("resize", measure);
    return () => {
      window.removeEventListener("scroll", measure, true);
      window.removeEventListener("resize", measure);
    };
  }, [open, alignEnd, anchor]);

  return createPortal(
    <span
      ref={(node) => {
        pop.current = node;
        if (popRef) popRef.current = node;
      }}
      className="tooltip__pop"
      hidden={!open}
      role={label === undefined ? undefined : "tooltip"}
      aria-label={label}
      // The position CSS cannot know: the anchor's rectangle in the
      // viewport, recomputed above whenever that rectangle moves. Hidden
      // until it has one, because the first paint of an unmeasured pop-up
      // is at the top-left corner of the screen.
      style={{
        top: (placement?.top ?? 0) + "px",
        left: (placement?.left ?? 0) + "px",
        visibility: placement ? undefined : "hidden"
      }}
      onClick={onClick}
    >
      <span className="tooltip__icon" aria-hidden="true">
        <Icon name="info" size={13} />
      </span>
      {/* The registry's HTML, rendered as HTML. First-party copy from a
          committed JSON file and never anything from an API, an operator
          or a URL — see the registry's module doc, which is where that
          rule is argued and where it has to keep being true. */}
      <span id={bodyId} className="tooltip__body" dangerouslySetInnerHTML={{ __html: entry.html }} />
      {children}
    </span>,
    // Out of this subtree entirely: the card around it clips, and the
    // card after it paints later (#847).
    layer
  );
}
