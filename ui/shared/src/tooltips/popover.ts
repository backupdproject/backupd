/**
 * Where a tooltip pop-up goes, and what it hangs off.
 *
 * Issue #847. Until it, a pop-up was an absolutely-positioned child of
 * the host that owned it, which is the arrangement every hover pop-up
 * starts out as and the one that cannot be made to work. Two ancestors it
 * has no way to see decide its fate:
 *
 *   - an `overflow: hidden` anywhere above it CLIPS it, and the cards
 *     this app is built out of all clip, because a card that does not is
 *     a card whose contents can escape its rounded corner;
 *   - a later sibling card PAINTS OVER it, because the pop-up's z-index
 *     is resolved inside its own card's stacking context and no value it
 *     can hold beats the next card's.
 *
 * Neither is fixable from the pop-up. A `z-index: 9999` does nothing
 * about either, which is what makes this a structural bug rather than a
 * styling one: the element has to leave that subtree. It goes to ONE
 * layer at the end of <body> (`tooltipLayer`), where it has no clipping
 * ancestor at all and paints after every card in the document.
 *
 * Leaving the subtree costs it the ancestor it was positioned against, so
 * the other half of this module is the arithmetic that replaces it
 * (`placePopover`): viewport coordinates from the trigger's own
 * rectangle. Pure, and taking numbers rather than elements, because "does
 * it flip below when it is near the top" is a question about arithmetic,
 * and asking it of a DOM that lays nothing out is how a positioning bug
 * survives a green suite.
 *
 * The rules, in the order they are applied, because the order is the
 * whole of it:
 *
 *   1. ABOVE the trigger by preference. A pop-up below its trigger covers
 *      whatever the operator is about to read or click next, and these
 *      hang off buttons and rows that sit at the bottom of a card with
 *      the next card directly underneath.
 *   2. BELOW if there is no room above, which is the top nav, the page
 *      header and the first row of every table.
 *   3. Clamped into the viewport in both axes if neither side has room,
 *      so a pop-up is never partly off-screen: unreachable copy is worse
 *      than misplaced copy, and there is no scrollbar for a fixed
 *      element.
 *
 * The alignment preference (`alignEnd`) is a request, not a rule — the
 * clamp overrides it — because it exists to keep a host at the right-hand
 * end of a row from placing its pop-up off the page, and the clamp is
 * what actually guarantees that now.
 */

/** The gap between the pop-up and the control it explains. Enough to read
 *  as a separate surface, small enough that the pointer crossing it is
 *  not an accident. */
const GAP = 6;
/** How close to the viewport edge a pop-up may come. Non-zero so it reads
 *  as floating above the page rather than welded to its edge. */
const MARGIN = 8;

/** What the placement needs from the trigger: the part of a DOMRect that
 *  is about position. */
export interface AnchorRect {
  top: number;
  left: number;
  bottom: number;
  right: number;
}

export interface PopoverSize {
  width: number;
  height: number;
}

export interface Viewport {
  width: number;
  height: number;
}

/** Viewport coordinates for a `position: fixed` pop-up. */
export interface PopoverPlacement {
  top: number;
  left: number;
}

export function placePopover(
  anchor: AnchorRect,
  pop: PopoverSize,
  viewport: Viewport,
  alignEnd: boolean
): PopoverPlacement {
  const above = anchor.top - GAP - pop.height;
  const below = anchor.bottom + GAP;
  const roomAbove = anchor.top - MARGIN;
  const roomBelow = viewport.height - MARGIN - anchor.bottom;

  let top: number;
  if (above >= MARGIN) top = above;
  else if (below + pop.height <= viewport.height - MARGIN) top = below;
  // Neither side fits, which is a short viewport rather than an awkward
  // trigger. Take the side with more room and let the clamp below place
  // it: covering part of the trigger beats being unreadable.
  else top = roomBelow > roomAbove ? below : above;

  const left = alignEnd ? anchor.right - pop.width : anchor.left;

  return {
    top: clamp(top, viewport.height - pop.height),
    left: clamp(left, viewport.width - pop.width)
  };
}

/** Into [MARGIN, limit - MARGIN], with the near edge winning when the
 *  pop-up is larger than the space: a pop-up too wide for the viewport is
 *  read from its start, not from its middle. */
function clamp(value: number, limit: number): number {
  return Math.max(MARGIN, Math.min(value, limit - MARGIN));
}

/**
 * The one element every tooltip pop-up in the app is rendered into.
 *
 * One rather than one per host, because the point of it is to be the last
 * thing in <body>: n layers would paint in creation order, so a pop-up
 * belonging to a host mounted early would sit under a layer created by a
 * host mounted later — the same defect this issue is about, rebuilt one
 * level up. It carries no styles; the pop-ups inside it are fixed to the
 * viewport, so it has no size and nothing to inherit. It must not acquire
 * a transform, a filter or a containment of its own, any of which would
 * make it the containing block for the fixed children and hand the clip
 * back to the page.
 */
export function tooltipLayer(): HTMLElement {
  // Held rather than looked up: a page carries dozens of tooltip hosts
  // and each one asks for this once as it mounts. `isConnected` is what
  // makes holding it safe — a test environment replaces the document
  // between cases, and a detached layer would take every pop-up in the
  // next one out of the document with it.
  if (cached?.isConnected) return cached;
  cached = document.querySelector<HTMLElement>(".tooltip-layer");
  if (cached) return cached;
  cached = document.createElement("div");
  cached.className = "tooltip-layer";
  document.body.appendChild(cached);
  return cached;
}

let cached: HTMLElement | null = null;
