/**
 * Hover help that attaches itself: one delegated layer, and the attribute
 * that opts an element in (issue #874).
 *
 * # The attribute, and why it is not the element's id
 *
 * An element opts in by carrying `data-tip="<registry id>"`:
 *
 *     <button data-tip="shell.sign-out">Sign out</button>
 *
 * A dedicated attribute rather than the native `id`, because an element's
 * id belongs to the document: it is what a <label> points at, what
 * aria-describedby and aria-labelledby resolve, what a URL fragment
 * addresses and what has to stay unique. Keying help off it would mean
 * every explained element had to be named after a registry entry and no
 * two of them could share an explanation — and an element that already
 * has an id for one of those reasons could never be explained at all.
 * `data-tip` says one thing and says it about help.
 *
 * # Fail-silent, which is the whole point
 *
 * The typed host (`InfoTooltip`) takes a `TooltipId`, so an id the
 * registry does not define does not COMPILE. That guarantee is what makes
 * it the right thing to use in a component, and it is also why it cannot
 * be the only way in: it is a JSX call site per explained element, and
 * `data-tip` is an attribute somebody can add to markup they are already
 * writing, or generate from data (a health state, a destination kind, a
 * route name) that is only known at run time.
 *
 * So this layer resolves the id through `lookupTooltip` and shows a
 * pop-up IF AND ONLY IF the registry defines it. An unknown id shows
 * NOTHING: not the id, not an empty pop-up, not a console error. That is
 * not laziness about errors, it is the only failure mode that makes the
 * attribute cheap to sprinkle on markup — the alternative is an interface
 * that shows operators `repositories.backend.rclone` because a copy entry
 * was renamed. The registry stays the single source of what is explained,
 * and the orphan sweep in the registry suite is what keeps entries from
 * rotting in the other direction.
 *
 * # One layer, one pop-up, delegated
 *
 * Mounted once near the app root (app/createApp.tsx). Not a host per
 * element, because the point of the attribute is that an explained
 * element stays plain markup; and not a pop-up per element either — there
 * is one shared `TooltipPopover`, moved to whichever trigger the pointer
 * or focus arrived at last, so n explained elements cost n attributes and
 * one React subtree.
 *
 * # The three deliberate choices in the event wiring
 *
 *   1. `pointerover` / `pointerout`, not `pointerenter` / `pointerleave`.
 *      The enter/leave pair does not bubble, so delegation from a
 *      container would depend on the capture phase reaching it for an
 *      event dispatched at a descendant — true, but a subtlety to rest
 *      the whole feature on. over/out bubble by design, which is what
 *      delegation is for. The cost is that they also fire as the pointer
 *      crosses between a trigger's own children, which `relatedTarget`
 *      answers below.
 *   2. Escape is on the DOCUMENT, not on the container. A pop-up opened
 *      by a resting pointer leaves focus wherever it was — usually on
 *      <body>, which is an ANCESTOR of this container, so no listener
 *      here would ever see that key. The same reason `useHoverPopover`
 *      binds Escape to the document.
 *   3. The listeners are native and in the capture phase rather than
 *      React's synthetic ones, because this layer has to hear about
 *      events aimed at elements it did not render and does not own,
 *      including ones whose own handlers stop propagation.
 *
 * # What it will not do
 *
 *   - It leaves an element inside an explicit `<InfoTooltip>` alone (the
 *     host's `.tooltip` wrapper is the marker). Both layers acting on one
 *     element would put the same copy on screen twice, on top of itself.
 *   - It respects issue #829 in both halves: the stored preference, and a
 *     surface that carries no hover help at all. The second half is a
 *     React context (`TooltipsSuppressed`) that a delegated layer cannot
 *     read, because by the time it has a trigger it has an element and
 *     not a position in the tree — so that subtree states it in the DOM
 *     as well, with `data-tips="off"` (auth/LoginPage.tsx).
 *   - It draws no close control. An auto-attached pop-up goes away when
 *     the pointer or focus leaves, so there is nothing for an "x" to do,
 *     and as a tab stop in a portal it would blur the trigger and take
 *     the pop-up down with itself.
 */
import { useEffect, useId, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { useTooltipsVisible } from "@shared/hooks/useTooltips";
import { lookupTooltip } from "@shared/tooltips/tooltips";
import type { TooltipEntry } from "@shared/tooltips/tooltips";
import { TooltipPopover } from "@shared/tooltips/TooltipPopover";

/** The opt-in attribute. Exported because it is the feature's public
 *  surface — markup is what uses it, and a selector is what finds it. */
export const TIP_ATTRIBUTE = "data-tip";

/** The element being explained right now, and what it says. The entry is
 *  resolved once, when the trigger is found, rather than re-looked-up on
 *  every render. */
interface ActiveTip {
  trigger: HTMLElement;
  entry: TooltipEntry;
  /** The entry's label, falling back to the id: it names the pop-up, and
   *  an id is a usable name where copy forgot to give one. */
  label: string;
}

export function TooltipAutoAttach({ children }: { children?: ReactNode }) {
  const container = useRef<HTMLDivElement | null>(null);
  // React's own id is ":r7q:", which is a legal HTML id and an illegal
  // CSS selector, and this one is written into an aria-describedby that
  // #671's suite resolves by selector. Stripping the colons keeps it
  // unique and addressable — the same fix, for the same reason, as the
  // typed host's.
  const bodyId = "tip" + useId().replace(/:/g, "");
  const visible = useTooltipsVisible();
  const [active, setActive] = useState<ActiveTip | null>(null);

  useEffect(() => {
    const root = container.current;
    // Off is off: with the preference down, no listener is installed at
    // all, so there is no path by which a pointer resting on markup can
    // open anything.
    if (!root || !visible) return;

    const onPointerOver = (event: Event) => {
      // Only ever opens or moves. A pointer arriving over markup that
      // explains nothing deliberately closes NOTHING: measured in a real
      // browser, where a pop-up opened by the keyboard vanished the
      // moment the mouse drifted across the page behind it. What the
      // pointer opened, the pointer's own departure closes — which is
      // what pointerout is, and it is precise about which element left.
      const next = resolve(event.target);
      if (next) setActive((prev) => (prev && prev.trigger === next.trigger ? prev : next));
    };

    const onPointerOut = (event: Event) => {
      const to = relatedTargetOf(event);
      setActive((prev) => {
        // An event from anywhere but the trigger currently being
        // explained says nothing about that pop-up.
        if (!prev || !(event.target instanceof Node) || !prev.trigger.contains(event.target)) return prev;
        // Movement WITHIN the trigger — across the <em> inside a
        // <span data-tip>, or onto the trigger from one of its children —
        // is not a departure. Hiding here is the defect where help
        // flickers off as the pointer crosses the thing it explains.
        return to && prev.trigger.contains(to) ? prev : null;
      });
    };

    const onFocusIn = (event: Event) => {
      const next = resolve(event.target);
      // Focus landing on something unexplained does not close a pop-up a
      // resting pointer is holding open; focus LEAVING the trigger does,
      // below.
      if (next) setActive((prev) => (prev && prev.trigger === next.trigger ? prev : next));
    };

    const onFocusOut = (event: Event) => {
      const to = relatedTargetOf(event);
      setActive((prev) => {
        if (!prev || !(event.target instanceof Node) || !prev.trigger.contains(event.target)) return prev;
        return to && prev.trigger.contains(to) ? prev : null;
      });
    };

    root.addEventListener("pointerover", onPointerOver, true);
    root.addEventListener("pointerout", onPointerOut, true);
    root.addEventListener("focusin", onFocusIn, true);
    root.addEventListener("focusout", onFocusOut, true);
    return () => {
      root.removeEventListener("pointerover", onPointerOver, true);
      root.removeEventListener("pointerout", onPointerOut, true);
      root.removeEventListener("focusin", onFocusIn, true);
      root.removeEventListener("focusout", onFocusOut, true);
      // A layer that stops listening must not leave a pop-up on screen
      // with no way to close it — which is exactly what turning the
      // preference off mid-hover would otherwise do.
      setActive(null);
    };
  }, [visible]);

  // Escape puts it away wherever focus is, which for a pop-up a resting
  // pointer opened is not inside this container at all. Nothing records
  // it as "dismissed" the way the typed host's pinned pop-up does: there
  // is no pinned state here, so the next pointer movement onto a trigger
  // is a fresh request for help and is answered.
  useEffect(() => {
    if (!active) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") setActive(null);
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [active]);

  // A trigger can also leave without any event at all: the row it is in
  // is re-rendered away, the route changes, a list refreshes. Nothing
  // then sends a pointerout, and the pop-up would be left on screen
  // explaining an element that is no longer in the document and anchored
  // to a rectangle that no longer moves. Observed only while a pop-up is
  // up, so the ordinary case costs nothing.
  useEffect(() => {
    const trigger = active?.trigger;
    if (!trigger) return;
    const watch = new MutationObserver(() => {
      if (!trigger.isConnected) setActive(null);
    });
    watch.observe(document.body, { childList: true, subtree: true });
    return () => watch.disconnect();
  }, [active]);

  // The description, written onto an element this layer did not render.
  // Only while the pop-up is up, because the copy is only in the document
  // while it is up: an aria-describedby pointing at an element that is
  // not there is a worse answer than no description. Whatever the trigger
  // already carried is appended to and then given back, since a control
  // with field help of its own has a description that is not ours to
  // drop.
  useEffect(() => {
    const trigger = active?.trigger;
    if (!trigger) return;
    const had = trigger.getAttribute("aria-describedby");
    trigger.setAttribute("aria-describedby", had === null ? bodyId : had + " " + bodyId);
    return () => {
      if (had === null) trigger.removeAttribute("aria-describedby");
      else trigger.setAttribute("aria-describedby", had);
    };
  }, [active, bodyId]);

  // A fresh box per trigger, so the pop-up's measure effect re-runs when
  // the pop-up moves to another element. `TooltipPopover` takes an anchor
  // it can read rather than an element, because the typed host has a ref
  // and this layer has an element, and a ref read during render is a
  // mutation-order question nobody should have to think about twice.
  const anchor = useMemo(() => ({ current: active?.trigger ?? null }), [active]);

  return (
    // `display: contents` because this wraps the whole application: a box
    // here would be a new block between the mount point and the shell,
    // and every layout rule below it would be answering a question about
    // this element instead. The container exists to be an event target,
    // not a thing on screen.
    <div ref={container} style={{ display: "contents" }}>
      {children}
      {active === null ? null : (
        <TooltipPopover
          entry={active.entry}
          label={active.label}
          bodyId={bodyId}
          anchor={anchor}
          open={true}
        />
      )}
    </div>
  );
}

/** The nearest ancestor of an event's target that carries a resolvable
 *  `data-tip`, or null — which is the answer for an element with no
 *  attribute, an id the registry does not define, an element the typed
 *  host already explains, and a surface that carries no hover help. */
function resolve(target: EventTarget | null): ActiveTip | null {
  if (!(target instanceof Element)) return null;
  const trigger = target.closest<HTMLElement>("[" + TIP_ATTRIBUTE + "]");
  if (!trigger) return null;
  // `.tooltip` is the typed host's wrapper, `[data-tips="off"]` a surface
  // that carries no hover help (#829). `closest` from the trigger, so
  // both questions are asked about the element being explained rather
  // than about whichever descendant the pointer happened to land on.
  if (trigger.closest('.tooltip, [data-tips="off"]')) return null;
  const id = trigger.getAttribute(TIP_ATTRIBUTE) ?? "";
  const entry = lookupTooltip(id);
  return entry ? { trigger, entry, label: entry.label ?? id } : null;
}

/** Where the pointer or the focus went, when the environment says. Read
 *  defensively rather than off a `MouseEvent`/`FocusEvent` type: these
 *  handlers are bound to native event names, and a synthesised event (a
 *  test's, an assistive technology's) can arrive as a plain `Event`
 *  carrying the property. Absent means "nowhere", which is a real answer
 *  — the pointer left the window, or focus went to no element. */
function relatedTargetOf(event: Event): Node | null {
  const to: unknown = "relatedTarget" in event ? event.relatedTarget : null;
  return to instanceof Node ? to : null;
}
