import { useId } from "react";
import type { CSSProperties, ReactNode } from "react";
import type { FieldHelpCopy } from "@shared/components/fieldHelpCopy";
import { useTooltipsVisible } from "@shared/hooks/useTooltips";
import { useHoverPopover } from "@shared/tooltips/useHoverPopover";

/**
 * Issue #278: the explanatory pop-up an input carries, and what is true of
 * it that is not true of every other pop-up in the app.
 *
 * # The interaction
 *
 * Hovering an input shows its pop-up and moving away hides it again.
 * Clicking the pop-up PINS it, and a pinned pop-up stays up: that is the
 * whole point of the control, because the copy is three sentences long and
 * a hover-only pop-up cannot be read by anyone whose pointer has to travel
 * to a scrollbar. A pinned pop-up closes on a click away from it, on its
 * close control, or on Escape.
 *
 * Four visible states (hidden, hover-shown, pinned, dismissed) fall out of
 * four booleans rather than one enum. Those booleans, and the reasoning
 * behind each of them, are tooltips/useHoverPopover.ts: issue #834 put the
 * same interaction on buttons, badges and metrics, at which point it
 * stopped being this component's behaviour and became the hook both of
 * them call. What is left here is the field-shaped half — the copy's
 * three-part shape, the aria-describedby contract with the caller's
 * control, and where focus goes when the pop-up is closed.
 *
 * # It is an overlay, so it has to get out of the way
 *
 * The pop-up is absolutely positioned over the page and takes pointer
 * events, which it must, since clicking it is how it gets pinned. The cost
 * is that while it is up it covers, and swallows the clicks meant for,
 * whatever sits below the field: on these forms that is the Save button,
 * the Sign in button, the next row of the chain. So clicking the control
 * puts its own help away. That is the moment the operator has read what
 * they came to read and is heading somewhere else, and hovering or
 * focusing the field brings it straight back.
 *
 * A changed VALUE puts it away too, on the same "you acted on this
 * control" reasoning as a click, and for a reason a click alone does not
 * cover: focusing a field opens its help, and a paste, an autofill, or a
 * script's own .fill() can change a field's value without ever dispatching
 * a click. Found on the wizard's private-key field, a multi-row textarea
 * tall enough that its own pop-up, opened purely by that focus, reached
 * down over the button below it, a control a keyboard-only path into the
 * field left uncoverable by mouse until something else dismissed the help
 * first.
 *
 * That does not make the overlay disappear as a property, and the residue
 * is worth stating rather than discovering: a pointer that arrives
 * directly on a covered control, without having clicked or edited the
 * field first, still lands on the pop-up. Nothing short of giving up
 * click-to-pin fixes that, and click-to-pin is the feature.
 *
 * # Accessibility, which the interaction spec does not cover
 *
 * Hover is unavailable to a keyboard and does not exist on a touch screen,
 * so the same content arrives four ways rather than one:
 *
 *   - Focus. Focusing the control shows the pop-up, Escape dismisses it,
 *     and the close button is a real <button> that Tab reaches (it follows
 *     the control in DOM order) and Enter or Space operates. Focus moving
 *     between the control and the close button stays inside the wrapper and
 *     does not count as leaving, which is what keeps the pop-up on screen
 *     long enough to Tab into it.
 *   - Screen readers. The copy is a permanent node referenced by the
 *     control's own aria-describedby, so it is announced with the control
 *     whether or not the pop-up is on screen. It stays referenced while
 *     hidden on purpose: the accessible description computation reads a
 *     hidden node that is DIRECTLY referenced this way, which is what makes
 *     a visual pop-up and an announced description the same copy instead of
 *     two that drift. The close button sits OUTSIDE that referenced node,
 *     so its label is not read as part of the description.
 *   - Touch. There is no hover to detect, so a touch pointerdown pins
 *     directly. That is the only sensible reading of "hover shows, click
 *     pins" on a phone: one tap, and the pop-up stays until it is dismissed.
 *     A MOUSE pointerdown deliberately does nothing, so clicking into a
 *     field to type puts the help away instead of pinning it over whatever
 *     the operator is about to click next.
 *   - The close control has an accessible name naming its field ("Close
 *     help for Keep"), not a bare glyph. That name is visually hidden TEXT
 *     inside the button rather than an aria-label on it, which is not a
 *     stylistic choice: an aria-label makes the button answer to "the
 *     control labelled Keep" in every label-based lookup, browser
 *     automation and assistive-technology alike, and the control labelled
 *     Keep is the field. A button should take its name from its content,
 *     which is also what survives translation and find-in-page. The glyph
 *     itself is a literal character in a string expression rather than an
 *     escape in JSX text, which is the bug #257 exists for: `×` written as
 *     element content renders as the six characters, not the symbol.
 *
 * Hiding uses the `hidden` attribute rather than unmounting, so the pop-up
 * keeps one stable id for aria-describedby across every state change, and
 * so the close button inside it is not focusable while the pop-up is not
 * on screen. That attribute gets its display: none from the user-agent
 * stylesheet, which any author `display` on the same element overrides, so
 * design-system/components.css has to re-assert it and does; without that
 * one rule every pop-up on the page is permanently open. It is asserted
 * from the shipped stylesheet in this component's suite, because a stubbed
 * stylesheet cannot tell the difference.
 *
 * # The operator can switch all of this off (issue #829)
 *
 * Everything above describes a pop-up an operator wants. One who does not
 * can say so, once, from the "x" itself: closing a pop-up that way offers
 * the global opt-out, and a browser that has taken it shows no pop-up
 * anywhere. That preference, and the "asked already" flag beside it, live
 * on the graph (state/tooltipNodes.ts); this component only reads the one
 * boolean that falls out of them, `useTooltipsVisible()`, which also
 * covers the surface-level suppression the sign-in screen applies.
 */

/** How a caller renders its own control: it must put `helpId` on the
 *  control's aria-describedby, which is what associates the copy with it. */
export type FieldHelpRender = (helpId: string) => ReactNode;

export interface FieldHelpProps {
  /** The field's name, used for the close control's accessible name. */
  label: string;
  help: FieldHelpCopy;
  children: FieldHelpRender;
  /** Forwarded to the positioning wrapper, for callers whose control has to
   *  participate in a grid or flex layout of their own. */
  style?: CSSProperties;
}

export function FieldHelp({ label, help, children, style }: FieldHelpProps) {
  const helpId = useId();

  // Issue #829: the input that is not this field's own. It answers a
  // question asked before any of the hook's four states — may a pop-up
  // appear here at all — and it is passed INTO the hook rather than
  // applied to its answer, because #839 gave one caller (#834's icon
  // trigger) a way in that the preference does not gate and this one no
  // such thing: off, or on the sign-in screen, nothing a field does opens
  // a pop-up. It also keeps the opt-out question from being asked about a
  // preference that already says no.
  //
  // The copy itself stays mounted and stays referenced by the control's
  // aria-describedby. That is not a loophole in "no tooltips": a screen
  // reader's description is not a pop-up, it does not cover the Save
  // button, and an operator who turned off pop-ups they can see has not
  // asked for the field's explanation to be withheld from somebody who
  // cannot. Only the visible overlay, and the close control inside it, are
  // what the preference removes.
  const tooltipsVisible = useTooltipsVisible();

  // The four-state hover interaction, which lived here until issue #834
  // put the same pop-up on buttons, badges and metrics and made it two
  // callers' behaviour rather than this component's. Nothing about the
  // rules changed; they are argued in tooltips/useHoverPopover.ts now, and
  // what stays here is the part that is specific to a labelled input.
  const { ref: wrapper, shown, hostProps, pin, dismiss } = useHoverPopover<HTMLDivElement>({
    enabled: tooltipsVisible,
    onDismiss: () => {
      // Put the operator back on the control they were describing rather
      // than dropping focus to the document, which is where a keyboard
      // user would otherwise have to Tab back from. The pop-up does not
      // re-open: this focus never leaves the wrapper, so `dismissed` is
      // not cleared.
      wrapper.current?.querySelector<HTMLElement>("input, select, textarea")?.focus();
    }
  });

  const open = shown;

  return (
    <div
      ref={wrapper}
      className="fieldhelp"
      style={style}
      {...hostProps}
    >
      {children(helpId)}

      <div
        className="fieldhelp__pop"
        hidden={!open}
        // Clicking the pop-up pins it. A click on the close button inside
        // stops before it reaches here, so closing cannot re-pin.
        onClick={pin}
      >
        {/* The described node holds the copy and nothing else, so the close
            button's own label is not concatenated into the description a
            screen reader reads for the control. */}
        <div id={helpId} className="fieldhelp__body">
          <p className="fieldhelp__what">{help.what}</p>
          <p className="fieldhelp__example">
            <span className="fieldhelp__example-lead">For example</span>{" "}
            <code>{help.example}</code>
          </p>
          <p className="fieldhelp__effect">{help.effect}</p>
        </div>
        <button
          type="button"
          className="fieldhelp__close"
          onClick={(event) => {
            event.stopPropagation();
            dismiss();
          }}
        >
          <span aria-hidden="true">{"\u00d7"}</span>
          <span className="visually-hidden">{"Close help for " + label}</span>
        </button>
      </div>
    </div>
  );
}

/** What HelpField hands its caller about the label it renders, for a
 *  control that has to point at it by id rather than lean on the wrapping
 *  <label>.
 *
 *  A <label> that wraps its control gives that control a name by walking
 *  its own subtree, and per the accessible-name algorithm an embedded
 *  control in that subtree contributes ITS text alternative too. So the
 *  moment a field puts a second control inside this label (PasswordInput's
 *  reveal toggle is the first), the field's name silently grows to include
 *  the button's. Measured in Chromium: "Password Show password Minimum 12
 *  characters." for one input. Pointing the control's aria-labelledby at
 *  `id` fixes it, because aria-labelledby resolves before the algorithm
 *  ever walks the label.
 *
 *  Handing the `label` string back too is not redundancy: it is the one
 *  source a caller can name its own inner control from, instead of writing
 *  the same string twice per call site and hoping the two never drift. */
export interface HelpFieldLabel {
  /** The id of the `.field__label` span, for aria-labelledby. */
  id: string;
  /** The same string that was passed in as `label`. */
  label: string;
}

/** A HelpField's caller gets the same `helpId` FieldHelp gives, plus the
 *  label it rendered. Ignoring the second argument is fine and is what most
 *  call sites do: it only matters for a control that puts something else
 *  inside the label alongside itself. */
export type HelpFieldRender = (helpId: string, field: HelpFieldLabel) => ReactNode;

export interface HelpFieldProps extends Omit<FieldHelpProps, "children"> {
  children: HelpFieldRender;
  /** Forwarded to the <label>, matching the plain `.field` usage this
   *  replaces (several call sites need a grid span or a max width). */
  labelStyle?: CSSProperties;
}

/**
 * The common case: a `.field` label with its control, plus the pop-up. This
 * is a drop-in for the
 *
 *   <label className="field"><span className="field__label">…</span>…</label>
 *
 * shape every form in this UI already uses, so adopting it on a page is a
 * wrapper change rather than a rewrite of the control inside it.
 */
export function HelpField({ label, help, children, style, labelStyle }: HelpFieldProps) {
  const labelId = useId();
  return (
    <FieldHelp label={label} help={help} style={style}>
      {(helpId) => (
        <label className="field" style={labelStyle}>
          <span className="field__label" id={labelId}>
            {label}
          </span>
          {children(helpId, { id: labelId, label })}
        </label>
      )}
    </FieldHelp>
  );
}
