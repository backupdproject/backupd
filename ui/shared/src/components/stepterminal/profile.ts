/**
 * The read-only profile the step terminal runs under, written down as
 * data so it can be asserted rather than believed (issue #815).
 *
 * # What is being defended against
 *
 * The bytes this viewer draws are a hook script's standard output, and
 * the machine that produced them is a source host — the least trusted
 * computer in the deployment. Everything in the list below is a thing a
 * terminal emulator will cheerfully do on that machine's behalf if it is
 * left switched on: follow a hyperlink somebody clicks, report the
 * window title back into the stream, resize or raise the window, answer
 * a device query, or hand the output a route to an application action.
 * None of them are features here, because this is a LOG RENDERER and not
 * a terminal: there is no shell, no pty, and no path from a byte on
 * screen to anything happening.
 *
 * # Why there is no terminal library
 *
 * "Pin a production release and never load third-party terminal code at
 * runtime" is satisfied here in the strongest way available: there is no
 * third-party terminal code at all. This product already renders its
 * run-level log with its own React view (components/ActivityDock.tsx),
 * and the emulator features that view does not have are exactly the ones
 * #815 requires be switched off — an escape parser that can move a
 * cursor, an OSC handler that can set a title or build a link, a
 * reporting path that writes back. Adding an emulator in order to
 * configure those away would import a parser for sequences this surface
 * has decided to refuse, and would be a runtime dependency shipped to
 * defend against itself.
 *
 * So the renderer is first party and in tree, which pins it the way
 * nothing else can: it moves when this repository moves, it is reviewed
 * with it, and there is no version resolution, no CDN and no dynamic
 * import anywhere in its path. `RENDERER` records that claim in one
 * place so the suite can read it back.
 *
 * The cost is deliberate and worth stating: this viewer does not paint
 * colour, does not honour cursor movement and does not redraw in place.
 * A hook's SGR colours are dropped, and a progress bar that repaints
 * with carriage returns is shown as the successive lines it actually
 * wrote. Both are consequences of refusing to emulate, and both are
 * reported to the reader rather than hidden (see sanitize.ts).
 */

/** Which renderer draws the log, and on what terms. Read by the suite,
 *  and by the "about this view" line the footer carries, so the claim on
 *  screen and the claim in the code are the same string. */
export const RENDERER = {
  /** First party, in this repository, under review with it. */
  name: "retnd step log view",
  /** Bumped when the rendering or filtering rules change, so a support
   *  report can name what drew the bytes it is complaining about. */
  revision: "1",
  /** No terminal emulator is loaded, statically or dynamically. */
  thirdPartyTerminalCode: false,
  /** Nothing in this component's path is fetched at runtime. */
  runtimeLoadedCode: false
} as const;

/**
 * Every capability a terminal would offer that this one refuses, and the
 * two bounds it enforces instead.
 *
 * The flags are not configuration: nothing reads them to decide what to
 * do, because a switch that can be turned on is a switch that will be.
 * They are the profile stated once, so the component can publish it (the
 * log element carries `data-stdin="disabled"` and friends) and the suite
 * can assert the published value against the rendered surface.
 */
export const HARDENED_PROFILE = {
  /** There is no input path. The log element accepts no typing, holds no
   *  editable node, and forwards no keystroke anywhere. */
  disableStdin: true,
  /** OSC 8 hyperlinks are swallowed before rendering and no anchor is
   *  ever produced from captured bytes. No link addon, no link matcher,
   *  no click handler on output. */
  linkActivation: false,
  /** Title and icon-title sequences are swallowed. Nothing this view
   *  draws can change document.title. */
  titleIntegration: false,
  /** CSI window manipulation (resize, move, raise, iconify, and the
   *  report forms that answer back) is swallowed. */
  windowManipulation: false,
  /** Device attribute and status reports are swallowed. There is no
   *  channel to answer them on, and a viewer that answered one would be
   *  writing bytes at a script's request. */
  deviceReports: false,
  /** No request of any kind is made because of what the output says. The
   *  only call this component makes is the log read it drives itself. */
  outputDrivenRequests: false,
  /** Captured bytes reach the DOM as text nodes only. Nothing is ever
   *  assigned as markup, parsed into a fragment, or handed to React's
   *  raw-HTML escape hatch — the suite sweeps this component's own
   *  sources for every spelling of those, which is why none of them are
   *  written out here. */
  htmlInjection: false,
  /** How many lines the browser keeps for one step. Above this the
   *  oldest go and the view says so; the durable log is unaffected and a
   *  download still reads it from the cursor. */
  scrollbackLines: 5_000,
  /** How many characters of one line are drawn before it is clipped. A
   *  hook that writes a megabyte without a newline is a layout denial of
   *  service, and clipping is visible where a hang is not. */
  maxLineChars: 4_000,
  /** How many unrendered records the streaming client will hold before
   *  it throws them away and re-reads from the durable log instead. */
  queueLimit: 2_000
} as const;

/** What the merged view is honest about. stdout and stderr are captured
 *  by two readers and sequenced as they arrive here, so their relative
 *  order is this product's capture order and not the script's write
 *  order. Said on screen, in the copied text and in the download, from
 *  this one constant. */
export const CAPTURE_ORDER_NOTE =
  "stdout and stderr are shown in capture order — the order this product read them — not the order the script wrote them.";

/** What the viewer is, said where somebody is looking at it. */
export const READ_ONLY_NOTE =
  "Read-only view: no input reaches the script, links and title or window control sequences are removed before drawing, and nothing here can be clicked to act.";
