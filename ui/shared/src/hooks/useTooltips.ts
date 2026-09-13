/**
 * Issue #829: the two questions every tooltip host asks before it shows
 * anything, and why they are two.
 *
 * "Does this operator want tooltips" is one global preference, on the
 * graph (state/tooltipNodes.ts). "May a tooltip be shown HERE" is not the
 * same question: the sign-in screen carries none regardless of the
 * preference, because it is the one screen where an operator has not yet
 * told us who they are and the one screen this product must not clutter
 * (see LoginPage's own module doc on why it deliberately looks like
 * nothing else). That second half is a property of WHERE a tooltip host
 * is mounted, not of anything the app stores, so it is a context and not
 * a node: nothing outside the subtree it wraps can observe it, and
 * nothing has to.
 *
 * A tooltip host therefore reads `useTooltipsVisible()` and not the node.
 * Two hosts read the halves apart, and both have a reason: the Settings
 * toggle reads `useTooltipsEnabled()` because a checkbox has to show the
 * stored preference rather than whatever is true in the subtree it
 * happens to sit in, and #834's icon host reads
 * `useTooltipsSuppressed()` because since #839 a press on it opens its
 * pop-up whatever the preference says — so "this surface carries none"
 * is the one half that still has to reach it.
 */
import { createContext, useContext } from "react";
import { useCausl } from "@shared/state/graph";
import { tooltipsEnabledNode } from "@shared/state/tooltipNodes";

/** Set true by a subtree that carries no hover help at all whatever the
 *  preference says — the sign-in screen (#829's requirement 2). */
export const TooltipsSuppressed = createContext(false);

/** This browser's stored "show tooltips" preference, on or off. For the
 *  control that EDITS it; a tooltip host wants `useTooltipsVisible`. */
export function useTooltipsEnabled(): boolean {
  return useCausl(tooltipsEnabledNode);
}

/** Whether a tooltip may appear where this is called: the preference, and
 *  the surface not having opted out of hover help entirely. */
export function useTooltipsVisible(): boolean {
  const enabled = useCausl(tooltipsEnabledNode);
  const suppressed = useContext(TooltipsSuppressed);
  return enabled && !suppressed;
}

/** Whether this surface carries no tooltips at all whatever the
 *  preference says. For a host that can be opened without the preference:
 *  #834's icon trigger, which #839 made a press rather than hover help.
 *  Everything else wants `useTooltipsVisible`. */
export function useTooltipsSuppressed(): boolean {
  return useContext(TooltipsSuppressed);
}

/**
 * The native `title` a hover-help attribute should carry here, which is
 * nothing when tooltips are off.
 *
 * Returns a function rather than taking the text directly, because most
 * of these titles are rendered inside a `.map()` (one per badge, one per
 * retention tier) where a hook cannot be called. The caller resolves the
 * preference once at the top of its component and applies it per element.
 *
 * A browser-drawn `title` cannot have a close "x" and does not need one —
 * it has no affordance to close, it disappears with the pointer, and
 * requirement 1 is about the pop-ups that do stay up. What it must honour
 * is the preference: "tooltips no longer appear on hover over anything"
 * is not true of a page still handing the browser titles to draw.
 */
export function useHoverTitle(): (text: string | undefined) => string | undefined {
  const visible = useTooltipsVisible();
  return (text) => (visible ? text : undefined);
}
