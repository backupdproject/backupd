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
 * What this hook does NOT decide is whether a pop-up may open at all.
 * That is the #829 preference, and it is applied by the caller, because
 * the caller is also what stays mounted and referenced while hidden: see
 * `useTooltipsVisible` in hooks/useTooltips.ts.
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
  /** Whether the pop-up is on screen, before the caller's own gate. */
  shown: boolean;
  /** Goes on the wrapping element, which must be the one `hostProps` is
   *  spread onto: "outside" is decided by `contains` against this. */
  ref: MutableRefObject<T | null>;
  hostProps: HoverPopoverHostProps;
  /** Clicking the pop-up pins it. Belongs on the pop-up itself. */
  pin(): void;
  /** The close "x", and only the close "x". Escape and a click away close
   *  a pop-up too and deliberately do not come through here: #829 asks its
   *  question about the "x" because pressing it is the one exit aimed AT
   *  the pop-up rather than at the page behind it. */
  dismiss(): void;
}

export function useHoverPopover<T extends HTMLElement = HTMLElement>(options?: {
  /** Run after the "x" closes the pop-up, before the opt-out is offered.
   *  FieldHelp puts focus back on the input it was describing; a host
   *  wrapping a button has nowhere to put it and passes nothing. */
  onDismiss?: () => void;
}): HoverPopover<T> {
  const ref = useRef<T | null>(null);

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
      const node = ref.current;
      if (node && event.target instanceof Node && node.contains(event.target)) return;
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
   *  it is null when focus came from or went to nowhere. */
  const staysInside = (event: FocusEvent<HTMLElement>) => {
    const other = event.relatedTarget;
    return other instanceof Node && event.currentTarget.contains(other);
  };

  return {
    shown,
    ref,
    pin: () => setPinned(true),
    dismiss: () => {
      setPinned(false);
      setDismissed(true);
      options?.onDismiss?.();
      // Offers the global opt-out, the first time this happens in this
      // browser and never again (state/tooltipNodes.ts owns that decision,
      // so every tooltip host in the app shares one answer rather than
      // one each).
      noteTooltipClosed();
    },
    hostProps: {
      onMouseEnter: () => {
        setHovered(true);
        setDismissed(false);
      },
      onMouseLeave: () => setHovered(false),
      onFocus: (event) => {
        if (staysInside(event)) return;
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
        if (event.pointerType !== "touch") return;
        setPinned(true);
        setDismissed(false);
      }
    }
  };
}
