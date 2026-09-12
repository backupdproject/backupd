/**
 * Issue #829: whether this browser shows hover tooltips at all, and the
 * one-time prompt that offers to turn them off.
 *
 * # Why localStorage and not the service
 *
 * "Show me tooltips" is a property of the person reading the screen, not
 * of the deployment: two operators sharing one instance can disagree about
 * it and both be right, and an operator who turns them off on their own
 * laptop has not made a configuration change anyone else should inherit.
 * So it lives where the theme, `rm-debug` and the activity dock's own
 * chrome live — in this browser — and never reaches the /settings API.
 *
 * # Why the graph and not a useState in App.tsx
 *
 * The theme is one component's state read by one component. This is read
 * by every tooltip host on the page and written from two places that are
 * nowhere near each other in the tree (a tooltip's own "x", via the
 * dialog, and the Settings toggle), which is precisely the case
 * state/graph.ts says belongs on the graph rather than in a context.
 * Flipping the Settings toggle has to stop the pop-ups on a page that
 * does not re-render for any other reason, and a graph node is what makes
 * that one commit rather than a prop drilled through six components.
 *
 * # The two persisted keys, and why the second one exists
 *
 * `backupd.tooltips` is the preference. Absent means ON: a fresh browser
 * shows tooltips, and the only thing that turns them off is somebody
 * saying so. Any value other than "off" is read as on, so a corrupted or
 * hand-edited key cannot silently take the help away.
 *
 * `backupd.tooltips.prompted` records that the opt-out dialog has been
 * offered. It is separate from the preference on purpose, because the two
 * answer different questions and one cannot be derived from the other:
 * "tooltips are still on" does not tell you whether the operator was ever
 * asked, and asking again on every close is nagging about something they
 * have already declined. It is set when the dialog OPENS, not when it is
 * answered, so dismissing it with Escape or the scrim also counts as
 * having been asked — the alternative re-opens a dialog somebody just
 * waved away.
 */
import { graph, registerInput } from "./graph";

/** This browser's tooltip preference. Absent means on. */
export const TOOLTIPS_KEY = "backupd.tooltips";

/** Whether the opt-out dialog has already been offered once. */
export const TOOLTIP_PROMPTED_KEY = "backupd.tooltips.prompted";

/** A browser with site data blocked THROWS on property access rather
 *  than answering null, and jsdom ships no Storage until src/test/setup.ts
 *  installs one. Both are read as "nothing stored", which lands on the
 *  defaults: tooltips on, never prompted. Same precaution ActivityDock and
 *  api/debug.ts take, for the same two reasons. */
function readStored(key: string): string | null {
  try {
    return window.localStorage.getItem(key);
  } catch {
    return null;
  }
}

function writeStored(key: string, value: string): void {
  try {
    window.localStorage.setItem(key, value);
  } catch {
    /* see readStored: a browser that cannot persist the preference still
       has to honour it for this page, which the graph node below does. */
  }
}

/** The stored preference, defaulting to ON. This is the load path: the
 *  node below is seeded from it at import, which is what makes an opt-out
 *  survive a reload. */
export function storedTooltipsEnabled(): boolean {
  return readStored(TOOLTIPS_KEY) !== "off";
}

/** Whether this browser has already been offered the opt-out dialog. */
export function storedTooltipPrompted(): boolean {
  return readStored(TOOLTIP_PROMPTED_KEY) === "1";
}

/** Whether hover tooltips are shown anywhere in this browser. Read
 *  through `useTooltipsVisible` (hooks/useTooltips.ts), never directly in
 *  a render body. */
export const tooltipsEnabledNode = registerInput<boolean>(
  "app.tooltipsEnabled",
  storedTooltipsEnabled()
);

/** Whether the opt-out dialog is on screen. One node for the whole app,
 *  because the dialog is mounted once (App.tsx) rather than once per
 *  tooltip: a modal per pop-up would be one dialog per field on the page,
 *  and the pop-up that asked is gone by the time it is answered. */
export const tooltipOptOutPromptNode = registerInput<boolean>("app.tooltipOptOutPrompt", false);

/** Turns tooltips on or off for this browser, everywhere, and remembers
 *  it. The one write path: the Settings toggle and the opt-out dialog both
 *  come through here, so neither can persist a preference the other cannot
 *  read back. */
export function setTooltipsEnabled(enabled: boolean): void {
  writeStored(TOOLTIPS_KEY, enabled ? "on" : "off");
  graph.commit("tooltips/enabled", (tx) => tx.set(tooltipsEnabledNode, enabled));
}

/**
 * A tooltip was closed by its own "x". Opens the opt-out dialog the FIRST
 * time that happens in this browser and never again.
 *
 * The flag is written here, as the dialog opens, rather than by whichever
 * button answers it. Every exit from the dialog — Yes, "Keep showing
 * them", Escape, a click on the scrim — is then equally final, which is
 * what "asked once" has to mean: an operator who waved the question away
 * has answered it.
 */
export function noteTooltipClosed(): void {
  if (storedTooltipPrompted()) return;
  writeStored(TOOLTIP_PROMPTED_KEY, "1");
  graph.commit("tooltips/opt-out-prompt", (tx) => tx.set(tooltipOptOutPromptNode, true));
}

/** Puts the opt-out dialog away, whatever the answer was. */
export function closeTooltipOptOutPrompt(): void {
  graph.commit("tooltips/opt-out-answered", (tx) => tx.set(tooltipOptOutPromptNode, false));
}
