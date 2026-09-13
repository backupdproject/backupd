/**
 * The interaction every hover pop-up in this app performs, in one place.
 *
 * Issue #278 worked this out for the pop-up an input field carries
 * (components/FieldHelp.tsx, whose module doc argues each rule at length
 * and is still the place to read WHY these are the rules). Issue #834
 * puts the same pop-up on buttons, links, badges, metrics and status
 * chips, which is the point at which the state machine stops belonging to
 * the field component and becomes the thing both of them call.
 *
 * The four booleans, in short, because the long version is in FieldHelp:
 *
 *   - `hovered` / `focused` are inputs from the environment. Either one
 *     shows the pop-up, and hover alone would leave a keyboard with no way
 *     to read it.
 *   - `pinned` is the operator's decision to keep it up (a click, or on a
 *     touch screen a tap, since there is no hover to open it with first).
 *   - `dismissed` is the operator's decision to put it away, and it is the
 *     one that is easy to leave out and not optional: without it, closing
 *     a pinned pop-up while the pointer is still over the host re-opens it
 *     instantly as a hover pop-up, so the "x" and Escape both appear to do
 *     nothing. It is cleared by the next thing that ASKS for the pop-up —
 *     a pointer arriving, focus arriving, a touch tap — rather than by the
 *     next thing that would hide it.
 *
 * Not four states in an enum: two of these are the environment and two are
 * the operator, and an enum has to re-derive "is the pointer still inside"
 * on every exit from the pinned state. Getting that wrong is exactly how a
 * pop-up ends up stuck on screen.
 *
 * What this hook does NOT decide is what the pop-up looks like, or where
 * the copy lives while it is closed: the caller is what stays mounted and
 * referenced while hidden.
 *
 * What it DOES decide, since issue #839, is the half of the #829
 * preference that is about opening. `enabled` is that preference as it
 * applies where the host is mounted (`useTooltipsVisible`), and it gates
 * the AMBIENT ways in — a pointer resting on the host, focus arriving,
 * a touch tap — because those are the ones #829 means by "tooltips no
 * longer appear on hover over anything". It deliberately does not gate
 * `toggle`, which is an explicit press of a control that exists for no
 * other purpose than to answer one question about one thing (#834's
 * icon host, `.tooltip__trigger`). Off, that press is the only way in;
 * on, it is one of two.
 *
 * It gates one more thing, which is why this is a parameter and not
 * something the caller could keep to itself: #829's opt-out question is
 * raised from `dismiss` here, and "turn off tooltips?" is not a question
 * to ask an operator who has already turned them off.
 */
import { useEffect, useRef, useState } from "react";
import type { FocusEvent, MutableRefObject, PointerEvent } from "react";
import { noteTooltipClosed } from "@shared/state/tooltipNodes";

/** The handlers that belong on the element wrapping the control and its
 *  pop-up. Spread, rather than picked apart: leaving one out is how a
 *  pop-up gets stuck, and there is no call site that wants a subset. */
export interface HoverPopoverHostProps {
  onMouseEnter(): void;
  onMouseLeave(): void;
  onFocus(event: FocusEvent<HTMLElement>): void;
  onBlur(event: FocusEvent<HTMLElement>): void;
  onClick(): void;
  onChange(): void;
  onPointerDown(event: PointerEvent<HTMLElement>): void;
}

/** Parameterised by the element the caller hangs it on, so the ref it
 *  hands back can go straight onto that element. A ref callback writing
 *  through a wider ref would be a mutation during render, which React's
 *  own lint rule refuses and is right to. */
export interface HoverPopover<T extends HTMLElement = HTMLElement> {
  /** Whether the pop-up is on screen. */
  shown: boolean;
  /** Goes on the wrapping element, which must be the one `hostProps` is
   *  spread onto: "outside" is decided by `contains` against this. */
  ref: MutableRefObject<T | null>;
  /** Goes on the pop-up itself, and is not optional for a caller whose
   *  pop-up leaves the host's subtree.
   *
   *  "Outside" and "focus moved away" are DOM containment questions, and
   *  since issue #847 the registry pop-up is portalled to a layer at the
   *  end of <body> to escape the cards that clipped it. Its own close
   *  button is then not inside the host by `contains`, and neither is the
   *  copy an operator clicks to pin — so without this ref, reading a
   *  pop-up closes it. The React tree is unchanged by a portal and every
   *  synthetic handler still arrives here; it is only the two rules that
   *  consult the DOM directly that need telling.
   *
   *  A caller whose pop-up is a real descendant (FieldHelp) may leave it
   *  unset: `contains` already answers for it. */
  popRef: MutableRefObject<HTMLElement | null>;
  hostProps: HoverPopoverHostProps;
  /** Clicking the pop-up pins it. Belongs on the pop-up itself. */
  pin(): void;
  /** The explicit "show me this one" press: #834's icon host, whose only
   *  purpose is this pop-up. Opens whatever the preference says (#839),
   *  and closes again if this is what opened it — a second press on the
   *  control that asked the question is the operator taking it back.
   *  A pop-up that is merely hovered is not click-CLOSED by it, so the
   *  common "point at the icon, then press it" does not toggle straight
   *  back off. */
  toggle(): void;
  /** The close "x", and only the close "x". Escape and a click away close
   *  a pop-up too and deliberately do not come through here: #829 asks its
   *  question about the "x" because pressing it is the one exit aimed AT
   *  the pop-up rather than at the page behind it. */
  dismiss(): void;
}

export function useHoverPopover<T extends HTMLElement = HTMLElement>(options: {
  /** #829's preference where this host is mounted (`useTooltipsVisible`).
   *  Gates the ambient opens and the opt-out question, never `toggle`. */
  enabled: boolean;
  /** Run after the "x" closes the pop-up, before the opt-out is offered.
   *  FieldHelp puts focus back on the input it was describing; a host
   *  wrapping a button has nowhere to put it and passes nothing. */
  onDismiss?: () => void;
}): HoverPopover<T> {
  const { enabled } = options;
  const ref = useRef<T | null>(null);
  const popRef = useRef<HTMLElement | null>(null);

  /** Whether a node is part of this pop-up's own furniture — the host and
   *  the pop-up, which are one thing to the operator and two subtrees to
   *  the DOM since #847 portalled the second one out of the first. */
  const ours = (node: Node) =>
    ref.current?.contains(node) === true || popRef.current?.contains(node) === true;

  const [hovered, setHovered] = useState(false);
  const [focused, setFocused] = useState(false);
  const [pinned, setPinned] = useState(false);
  const [dismissed, setDismissed] = useState(false);

  const shown = pinned || ((hovered || focused) && !dismissed);

  // A pinned pop-up closes on a click anywhere outside it. Registered in
  // the CAPTURE phase so this runs before React's own delegated handlers
  // at the root container: the alternative depends on whether a nested
  // handler stopped propagation, which is a needlessly fragile thing for
  // "did the operator click somewhere else" to rest on.
  useEffect(() => {
    if (!pinned) return;
    const onDocumentClick = (event: MouseEvent) => {
      if (event.target instanceof Node && ours(event.target)) return;
      setPinned(false);
      setDismissed(true);
    };
    document.addEventListener("click", onDocumentClick, true);
    return () => document.removeEventListener("click", onDocumentClick, true);
  }, [pinned]);

  // Escape dismisses whatever is showing. On the document rather than on
  // the wrapper, so it works for a pop-up opened by hover with the focus
  // somewhere else entirely, not only for the keyboard case.
  useEffect(() => {
    if (!shown) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      setPinned(false);
      setDismissed(true);
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [shown]);

  /** Focus moving between the control and the close button is movement
   *  WITHIN this pop-up, not away from it. relatedTarget is the element
   *  focus is arriving at (on focusout) or leaving from (on focusin), and
   *  it is null when focus came from or went to nowhere. The close button
   *  is in the portalled pop-up rather than in the host's subtree, so
   *  `contains` alone no longer answers this. */
  const staysInside = (event: FocusEvent<HTMLElement>) => {
    const other = event.relatedTarget;
    return other instanceof Node && (event.currentTarget.contains(other) || ours(other));
  };

  return {
    shown,
    ref,
    popRef,
    pin: () => setPinned(true),
    toggle: () => {
      // Reads `pinned` rather than `shown`: the state this takes back is
      // the one a previous press put there. A pop-up that is only up
      // because the pointer is resting on the icon has not been asked for
      // yet, and a mouse click necessarily arrives with the pointer over
      // its own target — so toggling on `shown` would make the ordinary
      // "move to the icon, press it" close the thing it was meant to
      // open.
      if (pinned) {
        setPinned(false);
        setDismissed(true);
        return;
      }
      setPinned(true);
      setDismissed(false);
    },
    dismiss: () => {
      setPinned(false);
      setDismissed(true);
      options.onDismiss?.();
      // Offers the global opt-out, the first time this happens in this
      // browser and never again (state/tooltipNodes.ts owns that decision,
      // so every tooltip host in the app shares one answer rather than
      // one each).
      //
      // Only where tooltips are on. Since #839 a pop-up can be on screen
      // while the preference says no — the operator pressed the icon that
      // exists to show it — and answering that with "turn off tooltips?"
      // both asks about something already true and spends #829's single
      // question, leaving the operator who turns them back on later never
      // to be asked at all.
      if (enabled) noteTooltipClosed();
    },
    hostProps: {
      onMouseEnter: () => {
        if (!enabled) return;
        setHovered(true);
        setDismissed(false);
      },
      onMouseLeave: () => setHovered(false),
      onFocus: (event) => {
        if (!enabled || staysInside(event)) return;
        setFocused(true);
        setDismissed(false);
      },
      onBlur: (event) => {
        if (staysInside(event)) return;
        setFocused(false);
      },
      // Acting on the control puts its help away. This is not cosmetic:
      // the pop-up is a real overlay, so while it is up it covers, and
      // takes the clicks meant for, whatever sits below the host. Once the
      // operator has clicked the control they have read what they were
      // going to read and are on their way somewhere else, so this is the
      // moment to get out of that way. Hovering or focusing again brings
      // it straight back.
      //
      // Every click inside the host lands here, the pop-up's own included,
      // and none of them needs excluding: `pinned` wins over `dismissed`
      // in `shown` above, so a click that pins survives the dismissal it
      // also records. That ordering is what makes a touch tap work too,
      // since a tap is a pointerdown that pins followed by a click here.
      onClick: () => setDismissed(true),
      // The same rule, for the one way a control changes value WITHOUT a
      // click ever firing: a paste that lands via keyboard, a browser
      // autofill, or a test's own .fill(), all of which set the value and
      // dispatch input/change directly.
      onChange: () => setDismissed(true),
      onPointerDown: (event) => {
        // Touch only. A tap is the pinned case, because there is no hover
        // to show a pop-up with first. A MOUSE pointerdown deliberately
        // does nothing, so clicking a control puts its help away instead
        // of pinning it over whatever is about to be clicked next.
        if (!enabled || event.pointerType !== "touch") return;
        setPinned(true);
        setDismissed(false);
      }
    }
  };
}
