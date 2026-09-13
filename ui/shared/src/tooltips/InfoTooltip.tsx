/**
 * The tooltip host: give it a registry id and whatever the tooltip is
 * about, and it explains that thing on hover (issue #834).
 *
 * # Two shapes, one component
 *
 * Most explained things on these pages already draw themselves — a
 * button, a nav row, a status chip, a number. Those wrap:
 *
 *     <InfoTooltip id="shell.sign-out"><button …>Sign out</button></InfoTooltip>
 *
 * The host adds no ink of its own there. Putting a small "i" icon beside
 * every button and every badge in the product would be a few hundred new
 * glyphs on screen, which is not a more explained interface, it is a
 * noisier one. The icon appears INSIDE the pop-up instead, where it says
 * "this is an explanation" at the moment there is something to explain.
 *
 * The other shape is for things that do not have a control to hang off —
 * a column header, a card title, a term in a definition list:
 *
 *     <dt><span>Storage mount</span><InfoTooltip id="platform.storage-mount" /></dt>
 *
 * With no children it draws the icon itself, as a real button, so the
 * explanation is reachable by keyboard and by touch where there is no
 * hover at all.
 *
 * # Where the icon host may NOT go, and why that is not a style rule
 *
 * Never inside an <h1>-<h6>, a <label>, a <button> or an <a>. The
 * accessible name of those elements is computed by walking their subtree,
 * and an embedded control contributes ITS name to the result: an icon
 * host inside a heading turns "Capacity" into "Capacity Explain
 * Capacity", and in a test environment with no stylesheet the closed
 * pop-up is not display:none either, so the whole tooltip's copy lands in
 * the name and in the element's text as well. Measured, not theorised.
 *
 * A heading therefore WRAPS instead — the pop-up is then a sibling of the
 * heading rather than a descendant, and nothing about the heading
 * changes:
 *
 *     <InfoTooltip id="settings.interface"><h2>Interface</h2></InfoTooltip>
 *
 * And where an icon does sit beside bare text inside a container, the
 * text keeps an element of its own (the <span> in the <dt> above), so
 * something on the page still reads exactly "Storage mount".
 *
 * # What it does about the wrapped control's accessibility
 *
 * The copy is a permanent node (hidden with the `hidden` attribute rather
 * than unmounted) carrying one stable id, and the host points the control
 * at it with aria-describedby — by cloning the child element when there is
 * exactly one, which is the shape of nearly every call site, and otherwise
 * by describing the wrapper. The accessible-description computation reads
 * a hidden node that is DIRECTLY referenced this way, so what a screen
 * reader announces and what the pop-up shows are the same copy rather than
 * two that drift, and the announcement does not depend on the pop-up being
 * open. HTML tags in the copy are not announced; the text between them is,
 * which is why the registry's copy has to read as a sentence with its tags
 * removed.
 *
 * # The preference (issue #829) gates hover, and since #839 only hover
 *
 * `useTooltipsVisible()` answers both halves of "may a tooltip appear
 * here on its own": the operator's stored preference, and whether this
 * subtree is one that carries no hover help at all whatever the
 * preference says — the sign-in screen. Off, and nothing a pointer
 * RESTING on the host, focus arriving or a touch tap does opens anything.
 *
 * The icon host is the exception, because it is not hover help. It is a
 * control whose whole reason to exist is "press this to read the
 * explanation", and an operator who presses it has asked a direct
 * question about one thing rather than opted back in to pop-ups
 * following their pointer around the page. So it always renders and its
 * press always opens (issue #839); with tooltips off it is the only way
 * in, and a second press takes it back.
 *
 * A surface that suppresses tooltips entirely draws no icon at all. That
 * is what "the sign-in screen carries none" has to mean once a press
 * outranks the preference: a trigger there would be a way in that nothing
 * could close off. The copy still stays in the DOM and stays referenced
 * wherever a control was wrapped: an operator who turned off pop-ups they
 * can see has not asked for the explanation to be withheld from somebody
 * who cannot see them.
 *
 * Closing a pop-up with its own "x" offers the global opt-out, once, the
 * same way a field's pop-up does — both go through the same hook, so
 * #829's one-question promise holds however many tooltips #834 adds. Not
 * when tooltips are already off, which since #839 is a state a pop-up on
 * screen can be in: see `useHoverPopover`.
 *
 * # Where the pop-up actually lives (issue #847)
 *
 * Not in the host's subtree. It is portalled into one layer at the end of
 * <body> and positioned from the host's viewport rectangle, because the
 * cards these hosts sit in clip what overflows them and the next card
 * paints over what is left — see tooltips/popover.ts, which argues that
 * and owns the arithmetic. A portal changes nothing about the React tree,
 * so the state, the context reads, the cloned aria-describedby and every
 * synthetic handler here are the same as they were; the two rules that
 * ask the DOM directly rather than React (a click outside, focus leaving)
 * are given `popRef` so they still see one pop-up in two subtrees.
 *
 * The pop-up itself is `TooltipPopover` rather than markup here, because
 * since issue #874 there are two ways into it: this host, which is the
 * TYPED one and the one that makes an id a compile-time fact, and the
 * delegated `data-tip` layer (`TooltipAutoAttach`), which resolves an id
 * at run time and shows nothing when the registry does not define it.
 * What they share is exactly one pop-up implementation — the portal, the
 * placement and the single injection of the registry's HTML. What stays
 * here is everything about WHEN it opens: the hover/pin/dismiss machine,
 * the icon trigger, the cloned aria-describedby, and a pop-up that is
 * mounted while closed so the control it explains always has an element
 * to point at.
 */
import { cloneElement, isValidElement, useId } from "react";
import type { CSSProperties, ReactElement, ReactNode } from "react";
import { Icon } from "@shared/design-system/icons";
import { useTooltipsSuppressed, useTooltipsVisible } from "@shared/hooks/useTooltips";
import { lookupTooltip } from "@shared/tooltips/tooltips";
import type { TooltipId } from "@shared/tooltips/tooltips";
import { TooltipPopover } from "@shared/tooltips/TooltipPopover";
import { useHoverPopover } from "@shared/tooltips/useHoverPopover";

export interface InfoTooltipProps {
  /** The registry entry to show. Checked by the compiler: an id the
   *  registry does not define is not of type `TooltipId`. */
  id: TooltipId;
  /** What the tooltip is about. Omitted, the host draws the "i" itself. */
  children?: ReactNode;
  /** Lays the host out as a block rather than inline, for wrapping a card,
   *  a row or anything else that must keep its own width. */
  block?: boolean;
  /** Pins the pop-up's right edge to the host's instead of its left, for
   *  hosts that sit at the right-hand end of a row where a left-aligned
   *  pop-up would run off the page. */
  alignEnd?: boolean;
  style?: CSSProperties;
}

/** The one element that can be described by cloning, narrowed so the clone
 *  is a type change rather than an `any`. */
type DescribableChild = ReactElement<{ "aria-describedby"?: string }>;

export function InfoTooltip({ id, children, block, alignEnd, style }: InfoTooltipProps) {
  // React's own id is ":r7q:", which is a legal HTML id and an illegal CSS
  // selector: `querySelector("#" + id)` throws on it. That matters here in
  // a way it does not for a field's own pop-up, because this id is
  // APPENDED to whatever aria-describedby the wrapped control already
  // carried, and #671's suite walks those lists resolving each id by
  // selector. Stripping the colons keeps the id unique and makes it
  // addressable by everything that looks for it.
  const bodyId = "tip" + useId().replace(/:/g, "");
  const visible = useTooltipsVisible();
  const suppressed = useTooltipsSuppressed();
  const {
    ref: host,
    popRef,
    shown,
    hostProps,
    pin,
    toggle,
    dismiss
  } = useHoverPopover<HTMLSpanElement>({ enabled: visible });
  // The hook is the gate: with the preference off it opens for a press of
  // the icon and for nothing else (#839), so there is nothing here to
  // disagree with it.
  const open = shown;

  const entry = lookupTooltip(id);

  // An id with no entry renders the thing it was wrapping and no host at
  // all. `TooltipId` makes that unreachable from a component, so this is
  // the guard for the other path: an id assembled at run time from data
  // (a health state, a destination kind) that has no entry yet. A missing
  // explanation must not take a page down, and must not leave an empty
  // pop-up behind either.
  if (!entry) return <>{children}</>;

  const label = entry.label ?? id;
  // No icon on a surface that carries none — the sign-in screen. Its press
  // outranks the preference, so not drawing it is the only thing that
  // still closes that surface off.
  const trigger = children === undefined && !suppressed;

  // Exactly one element child is the common case, and the one where the
  // description can land on the control itself rather than on a wrapper a
  // screen reader has no reason to visit.
  const single = isValidElement(children) ? (children as DescribableChild) : undefined;
  const described = single
    ? cloneElement(single, {
        "aria-describedby": [single.props["aria-describedby"], bodyId]
          .filter(Boolean)
          .join(" ")
      })
    : children;

  return (
    <span
      ref={host}
      className={"tooltip" + (block ? " tooltip--block" : "")}
      style={style}
      aria-describedby={single ? undefined : bodyId}
      {...hostProps}
    >
      {trigger ? (
        <button
          type="button"
          className="tooltip__trigger"
          // Named the same way, and for the same reason, as the close
          // control below: an icon host sits beside a term or a column
          // header, and hidden TEXT naming that term would give the page a
          // second element reading it.
          aria-label={"Explain " + label}
          aria-describedby={bodyId}
          // A press opens, rather than dismissing the way a click on a
          // wrapped control does: this button exists for no other purpose
          // than to show this pop-up, so the click that reaches it is a
          // request to keep it up, and `pinned` beating the wrapper's own
          // dismissal is what makes that hold. It opens whatever the
          // preference says (#839) and a second press takes it back.
          onClick={toggle}
        >
          <Icon name="info" size={13} />
        </button>
      ) : (
        described
      )}

      <TooltipPopover
        entry={entry}
        bodyId={bodyId}
        anchor={host}
        popRef={popRef}
        open={open}
        alignEnd={alignEnd}
        // Clicking the pop-up pins it, so copy can be read while the
        // pointer travels to a scrollbar. A click on the close button
        // inside stops before it reaches here, so closing cannot re-pin.
        onClick={pin}
      >
        {/* Named by aria-label rather than by visually-hidden text, which
            is where this parts company with FieldHelp's own close control
            and why. That name has to contain the label of the thing being
            explained, because a page now carries dozens of these and
            "Close help" on all of them names none of them. As TEXT, that
            label would be a second element on the page reading "No
            activity yet" or "Everything" — every getByText and every
            find-in-page for a short label would match the close button of
            its own tooltip. A label the browser does not put in the
            document is the only form of this name that does not do that.
            FieldHelp's is text because its one control sits inside a
            <label> whose accessible name an aria-label would capture. */}
        <button
          type="button"
          className="tooltip__close"
          aria-label={"Close help for " + label}
          onClick={(event) => {
            event.stopPropagation();
            dismiss();
          }}
        >
          {/* A literal character in a string expression rather than an
              escape in JSX text: `&times;` written as element content
              renders as the six characters (issue #257). */}
          <span aria-hidden="true">{"\u00d7"}</span>
        </button>
      </TooltipPopover>
    </span>
  );
}
