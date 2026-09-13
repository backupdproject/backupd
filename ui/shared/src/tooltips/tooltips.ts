/**
 * The tooltip registry: one JSON file holding what every explained element
 * in this interface says, and the typed door onto it (issue #834).
 *
 * # Why the copy is not next to the element
 *
 * Issue #829 gave the operator one switch for hover help; #834 asks for
 * hover help on everything it could reasonably be on — every button, link
 * and nav row, and every element that STATES something: badges, status
 * chips, counts, sizes, timestamps. That is a few hundred sentences, and
 * sentences spread across a few hundred JSX call sites cannot be read as a
 * whole, cannot be reviewed for one voice, and cannot be translated.
 * Keeping them in one file makes the app's explanatory copy a document
 * somebody can actually read end to end.
 *
 * The element therefore names an id and nothing else. The id is stable and
 * the copy is not: rewording a tooltip is a JSON edit that touches no
 * component, which is the property that makes the copy maintainable by
 * somebody who does not want to open a .tsx file.
 *
 * # Why the copy is HTML
 *
 * These sentences want emphasis, a literal value in monospace and the odd
 * line break — "a set is <strong>stale</strong> when its last backup is
 * older than <code>every 24h</code>" reads as one thing and a flat string
 * reads as three. Requirement 3 of #834 says so outright: the text is
 * HTML, rendered as HTML.
 *
 * That is only safe because of where it comes from. Every string here is
 * first-party content committed to this repository and reviewed like any
 * other source; nothing at runtime — no API response, no operator input,
 * no path, no hostname — is ever concatenated into it. `InfoTooltip`
 * renders these through `dangerouslySetInnerHTML` and renders NOTHING
 * else that way. A tooltip that has to show a runtime value takes it as a
 * child element instead, where React escapes it.
 *
 * # The type
 *
 * `TooltipId` is the JSON's own key union, not `string`. A component
 * naming an id that is not in the registry does not compile, which is the
 * only check that scales to the number of call sites #834 creates: there
 * is no run of the app, and no test sweep, that visits every tooltip.
 *
 * # The two doors, and which one to use (issue #874)
 *
 * `<InfoTooltip id="…">` is the TYPED door and the default. Use it
 * wherever a component is being written: the id is checked, the wrapped
 * control is described by the copy whether or not the pop-up is open, and
 * the pop-up can be pinned and closed. An id it names cannot be deleted
 * from this file without breaking the build, which is the property that
 * keeps the registry honest.
 *
 * `data-tip="<id>"` is the RUN-TIME door (tooltips/TooltipAutoAttach.tsx,
 * one delegated layer at the app root). Use it where the compiler cannot
 * help anyway:
 *
 *   - the id is data — a health state, a backend kind, a route name —
 *     and is only an id once the page has the value;
 *   - the element is deep in dense markup (a table cell, a chip in a
 *     row) where wrapping every one of them in a host is more JSX than
 *     the explanation is worth;
 *   - copy is being rolled out across a screen and the ids are landing
 *     in this file as it goes.
 *
 * It is fail-silent by design: an id this registry does not define shows
 * NOTHING. That is what the attribute buys and what it costs — nothing
 * tells you the id was wrong, so an element that must be explained, and
 * must stay explained, wants the typed host instead. The two never
 * double up: the delegated layer leaves any element inside an
 * `<InfoTooltip>` to its host.
 */
import registry from "./tooltips.json";

/** One registry entry. */
export interface TooltipEntry {
  /** The tooltip's body, as HTML. First-party copy — see the module doc
   *  before putting anything here that did not come out of this file. */
  html: string;
  /** What this element is called, for the close control's accessible name
   *  ("Close help for Sign out") and for the icon-only host's own name.
   *  Optional: an id like `shell.sign-out` is a usable fallback, but a
   *  written label is what an operator hears. */
  label?: string;
}

/** Every id the registry defines. The keys of the JSON itself, so adding
 *  an entry is one edit and removing one breaks every call site. */
export type TooltipId = keyof typeof registry;

/**
 * The registry, typed.
 *
 * The annotation is the validation: TypeScript checks the shipped JSON
 * against `TooltipEntry` here, so an entry that spells `html` wrong, or
 * carries a number, fails `npm run lint` rather than rendering an empty
 * pop-up in front of an operator.
 */
export const TOOLTIPS: Record<TooltipId, TooltipEntry> = registry;

/** Every id, in registry order. Derived rather than written out again, so
 *  a sweep cannot pass over an entry somebody forgot to add to a list. */
export const TOOLTIP_IDS = Object.keys(TOOLTIPS) as TooltipId[];

/**
 * The entry for an id, or undefined.
 *
 * Takes a plain `string` on purpose. Callers in components use `TooltipId`
 * and are checked by the compiler; this is the other path — a id that
 * arrives as data (a status name, a nav route) and is only known at run
 * time. Answering undefined rather than throwing is what lets a host
 * render nothing at all for an unknown id, which is the correct failure:
 * a missing explanation must not take a page down.
 */
export function lookupTooltip(id: string): TooltipEntry | undefined {
  return Object.prototype.hasOwnProperty.call(TOOLTIPS, id)
    ? TOOLTIPS[id as TooltipId]
    : undefined;
}
