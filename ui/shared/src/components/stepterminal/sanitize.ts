/**
 * The filter every captured byte passes through before this browser is
 * allowed to draw it (issue #815).
 *
 * # The rule
 *
 * Escape sequences are SWALLOWED — recognised in full, from their
 * introducer to their terminator, and removed — and the lone control
 * characters that are left are replaced by a visible placeholder. What
 * reaches the DOM is printable text, as text nodes, and the count of
 * what was taken out is handed back so the reader is told rather than
 * quietly shown less than the hook wrote.
 *
 * Swallowing whole sequences rather than neutralising their introducer
 * is the difference between this and `core/cmd/retnd`'s
 * `neutralizeTerminalControls`, and the reason is the surface. A CLI
 * printing `?]8;;http://evil?click here?]8;;?` has already won: the
 * sequence is inert because the terminal never sees an introducer. A
 * browser does not have that luxury for the class of sequences #815
 * names — OSC 8, title, window control — because the thing being
 * defended is not a terminal's parser but a reader's eye and a DOM. The
 * URL of a hyperlink is not information an operator needs to see spelt
 * out in the middle of a log line; the fact that a link was there is.
 * So the payload goes and one placeholder stays.
 *
 * # What each class becomes
 *
 *   - CSI, OSC, DCS, SOS, PM, APC and the short ESC forms: removed
 *     whole, one `?` in their place, counted under the class that says
 *     what they were trying to do. The `?` is the same placeholder the
 *     CLI uses, which keeps one vocabulary across both surfaces.
 *   - SGR (colour and attributes) is the one exception: removed with NO
 *     placeholder, because dropping colour hides nothing — every
 *     character the hook wrote is still on screen — and a log where
 *     every coloured word is wrapped in question marks is a log nobody
 *     reads. It is still counted, and the count is still shown.
 *   - C0 controls other than tab and newline, DEL, and the C1 range:
 *     one `?` each. A backspace that could overwrite the line above and
 *     a BEL that could ring are the same problem as an escape.
 *   - Bidi overrides and isolates: one `?` each. On a web surface these
 *     are the cheapest spoof available — U+202E reverses the rest of the
 *     line, so `failed` can be made to read `deliaf` or worse, a fake
 *     path can be made to read as a real one — and no legitimate log
 *     line needs them.
 *   - CR: a line break, and CRLF one break rather than two. This view
 *     does not emulate a cursor, so a carriage return cannot be allowed
 *     to overwrite: a progress bar that repainted itself forty times is
 *     shown as the forty lines it actually wrote. Honest and inert, and
 *     the scrollback bound is what keeps it from being a flood.
 *   - A line longer than `maxLineChars` is clipped with a visible
 *     notice. A megabyte without a newline is a layout denial of
 *     service, and a browser that stops responding is a worse failure
 *     than a line that says it was cut.
 *
 * Nothing here decodes, re-encodes or interprets. An unterminated
 * sequence swallows the rest of the text it is in and is counted, which
 * is the only safe reading of "a sequence whose end never arrived".
 */

import { HARDENED_PROFILE } from "./profile";
import type { LogStream, StepLogRecord } from "./contract";

/** What was taken out, by what it was trying to do. The classes are
 *  distinct because the report is read by a person deciding whether a
 *  hook is badly behaved or hostile: colour is the former, a title
 *  sequence in a backup script's output is the latter. */
export type RemovedClass =
  | "format"
  | "hyperlink"
  | "title"
  | "window"
  | "report"
  | "escape"
  | "control"
  | "bidi"
  | "invisible"
  | "clipped";

/** Every class, in the order the notice lists them. */
export const REMOVED_CLASSES: readonly RemovedClass[] = [
  "hyperlink",
  "title",
  "window",
  "report",
  "escape",
  "control",
  "bidi",
  "invisible",
  "format",
  "clipped"
];

/** How each class is named to an operator. Singular; the notice adds the
 *  count in front and pluralises with the one rule below. */
const CLASS_WORDS: Record<RemovedClass, string> = {
  hyperlink: "hyperlink sequence",
  title: "window-title sequence",
  window: "window-control sequence",
  report: "device-report sequence",
  escape: "other escape sequence",
  control: "control character",
  bidi: "text-direction override",
  invisible: "invisible formatting character",
  format: "colour sequence",
  clipped: "over-long line clipped"
};

export type RemovedCounts = Record<RemovedClass, number>;

/** A zeroed tally. A full record rather than a sparse one, so no caller
 *  has to decide what an absent key means. */
export function noRemovals(): RemovedCounts {
  return {
    format: 0,
    hyperlink: 0,
    title: 0,
    window: 0,
    report: 0,
    escape: 0,
    control: 0,
    bidi: 0,
    invisible: 0,
    clipped: 0
  };
}

/** Adds `from` into `into`, in place. The viewer keeps one tally for the
 *  whole step, so a page arriving is a fold and not a replacement. */
export function addRemovals(into: RemovedCounts, from: RemovedCounts): RemovedCounts {
  for (const key of REMOVED_CLASSES) into[key] += from[key];
  return into;
}

/**
 * The tally as one sentence, or null when there is nothing to say.
 *
 * It is aggregated on purpose. A per-occurrence marker in the log turns
 * a hook that prints one coloured word per line into a log that is half
 * markers, and the question an operator actually has is "was anything
 * taken out of this, and was it the kind of thing a backup script has
 * any business writing".
 */
export function removalNotice(counts: RemovedCounts): string | null {
  const parts: string[] = [];
  for (const key of REMOVED_CLASSES) {
    const n = counts[key];
    if (n > 0) parts.push(n + " " + CLASS_WORDS[key] + (n === 1 ? "" : "s"));
  }
  if (parts.length === 0) return null;

  return "Removed before drawing: " + parts.join(", ") + ".";
}

/** The placeholder a swallowed sequence or a neutralised control leaves
 *  behind. The same character `core/cmd/retnd` writes, so an operator
 *  comparing `workflow run log` with this screen sees one convention. */
export const PLACEHOLDER = "?";

/** What a clipped line ends with. */
export const CLIP_NOTICE = " \u2026 [line clipped]";

/** The wording the truncation marker draws. The engine's own marker
 *  record is a statement about a POSITION in the log, so it is rendered
 *  where it happened and in this product's words, never in the script's:
 *  a marker whose text came from the hook is a marker a hook can forge. */
export const TRUNCATION_MARKER =
  "──── this product stopped recording here · the log limit for this step was reached · the script kept running ────";

export interface Sanitized {
  text: string;
  removed: RemovedCounts;
}

/** The class a CSI sequence belongs to, from its final byte. */
function csiClass(final: string): RemovedClass {
  if (final === "m") return "format";
  if (final === "t") return "window";
  if (final === "c" || final === "n") return "report";

  return "escape";
}

/** The class an OSC sequence belongs to, from its leading numeric
 *  parameter. 8 builds a hyperlink; 0, 1 and 2 set the icon name and the
 *  window title. Everything else — 52 writes the clipboard, 10 through
 *  19 set and report colours — is swallowed as an escape. */
function oscClass(payload: string): RemovedClass {
  const ps = /^([0-9]{0,4})/.exec(payload)?.[1] ?? "";
  if (ps === "8") return "hyperlink";
  if (ps === "0" || ps === "1" || ps === "2") return "title";

  return "escape";
}

/**
 * One captured chunk, made safe to draw.
 *
 * Returns the text to render and what was taken out of it. Newlines
 * survive as newlines: splitting into lines is the caller's business
 * (see `sanitizeRecord`), because a record is a chunk of a stream and
 * not a promise about line boundaries.
 */
export function sanitizeText(raw: string, maxLineChars: number = HARDENED_PROFILE.maxLineChars): Sanitized {
  const removed = noRemovals();
  let out = "";
  let lineChars = 0;
  let clipped = false;
  let i = 0;

  /** Appends drawable text, honouring the per-line bound. */
  const put = (s: string) => {
    if (s === "\n") {
      out += "\n";
      lineChars = 0;
      clipped = false;

      return;
    }
    if (clipped) return;
    if (lineChars + s.length > maxLineChars) {
      out += CLIP_NOTICE;
      removed.clipped++;
      clipped = true;

      return;
    }
    out += s;
    lineChars += s.length;
  };

  /** Swallows one sequence starting at `start` and returns the index
   *  after it. Every exit counts exactly one occurrence. */
  const swallow = (start: number): number => {
    const code = raw.charCodeAt(start);
    let kind: "csi" | "osc" | "string";
    let body: number;

    if (code === 0x1b) {
      const next = raw[start + 1];
      if (next === undefined) {
        // A lone ESC at the end of the chunk. It cannot be completed by
        // anything this function will see, so it goes on its own.
        removed.escape++;
        put(PLACEHOLDER);

        return start + 1;
      }
      if (next === "[") {
        kind = "csi";
        body = start + 2;
      } else if (next === "]") {
        kind = "osc";
        body = start + 2;
      } else if (next === "P" || next === "X" || next === "^" || next === "_") {
        kind = "string";
        body = start + 2;
      } else {
        // The short forms: a charset designator or a private-use
        // sequence takes one more byte (ESC ( B, ESC # 8, ESC SP F), and
        // everything else is complete in two (ESC c is a full reset,
        // ESC 7 and ESC 8 save and restore the cursor).
        const twoByteIntermediate = next === "#" || next === "(" || next === ")" ||
          next === "*" || next === "+" || next === "%" || next === " ";
        removed.escape++;
        put(PLACEHOLDER);

        return start + (twoByteIntermediate ? 3 : 2);
      }
    } else if (code === 0x9b) {
      kind = "csi";
      body = start + 1;
    } else if (code === 0x9d) {
      kind = "osc";
      body = start + 1;
    } else if (code === 0x90 || code === 0x98 || code === 0x9e || code === 0x9f) {
      kind = "string";
      body = start + 1;
    } else {
      // Any other C1: a control in its own right, not an introducer.
      removed.control++;
      put(PLACEHOLDER);

      return start + 1;
    }

    if (kind === "csi") {
      // Parameters, then intermediates, then one final byte in
      // 0x40..0x7e. A run that ends before the final byte arrives is
      // unterminated and takes the rest of the chunk with it.
      let at = body;
      while (at < raw.length) {
        const c = raw.charCodeAt(at);
        if (c >= 0x30 && c <= 0x3f) {
          at++;

          continue;
        }
        if (c >= 0x20 && c <= 0x2f) {
          at++;

          continue;
        }
        break;
      }
      if (at >= raw.length) {
        removed.escape++;
        put(PLACEHOLDER);

        return raw.length;
      }
      const final = raw[at];
      const cls = csiClass(final);
      removed[cls]++;
      // Colour and attributes leave nothing behind: every character the
      // hook wrote is still drawn, so a placeholder would be noise.
      if (cls !== "format") put(PLACEHOLDER);

      return at + 1;
    }

    // OSC and the string sequences (DCS, SOS, PM, APC) both end at a
    // string terminator: ESC \, the eight-bit ST, or — for OSC only, and
    // it is what xterm and every shell's prompt actually emit — a BEL.
    let at = body;
    let end = -1;
    let after = -1;
    while (at < raw.length) {
      const c = raw.charCodeAt(at);
      if (kind === "osc" && c === 0x07) {
        end = at;
        after = at + 1;
        break;
      }
      if (c === 0x9c) {
        end = at;
        after = at + 1;
        break;
      }
      if (c === 0x1b && raw[at + 1] === "\\") {
        end = at;
        after = at + 2;
        break;
      }
      at++;
    }
    if (end < 0) {
      // Unterminated. Everything after the introducer is part of a
      // sequence whose end never came, so none of it is drawable.
      removed.escape++;
      put(PLACEHOLDER);

      return raw.length;
    }
    const cls = kind === "osc" ? oscClass(raw.slice(body, end)) : "escape";
    removed[cls]++;
    put(PLACEHOLDER);

    return after;
  };

  while (i < raw.length) {
    const code = raw.charCodeAt(i);

    // ESC and the C1 range, which carries the eight-bit CSI, OSC and
    // string introducers.
    if (code === 0x1b || (code >= 0x80 && code <= 0x9f)) {
      i = swallow(i);

      continue;
    }
    if (code === 0x0a) {
      put("\n");
      i++;

      continue;
    }
    if (code === 0x0d) {
      // CRLF is one break. A lone CR is a break too: see the header.
      put("\n");
      i += raw.charCodeAt(i + 1) === 0x0a ? 2 : 1;

      continue;
    }
    if (code === 0x09) {
      // A tab cannot move the cursor anywhere dangerous and a hook
      // printing a table is printing information.
      put("\t");
      i++;

      continue;
    }
    if (code < 0x20 || code === 0x7f) {
      removed.control++;
      put(PLACEHOLDER);
      i++;

      continue;
    }
    // The bidi overrides and isolates, LRM and RLM included: one mark
    // is enough to reorder a mixed-script line.
    if (
      code === 0x200e ||
      code === 0x200f ||
      (code >= 0x202a && code <= 0x202e) ||
      (code >= 0x2066 && code <= 0x2069)
    ) {
      removed.bidi++;
      put(PLACEHOLDER);
      i++;

      continue;
    }
    if (code === 0x2028 || code === 0x2029) {
      // Unicode's own line and paragraph separators. Invisible breaks in
      // the middle of a log line are a spoof, not a formatting choice.
      removed.control++;
      put(PLACEHOLDER);
      i++;

      continue;
    }
    // The zero-width and invisible-format class. None of them draw
    // anything, and all of them change what a line APPEARS to say:
    // ZWSP and ZWNJ break a word an operator is searching for or
    // reading as one token (`/srv/back\u200bups` reads as the real
    // path and is not it), ZWJ and the word joiner glue tokens
    // together, a soft hyphen vanishes until the line wraps, a BOM in
    // the middle of a line is a zero-width nothing, and the TAG range
    // U+E0000..U+E007F is a whole invisible ASCII alphabet — the
    // "smuggled text" trick — that can carry a second, unreadable
    // message inside a line this product will be quoted on. A log line
    // is evidence, so every code point in it has to be visible.
    //
    // The TAG range is above the BMP, so it arrives as a surrogate
    // pair: this is the one place the scan has to read a code point
    // rather than a code unit, and it consumes both halves.
    if (code >= 0xd800 && code <= 0xdbff) {
      const point = raw.codePointAt(i) ?? code;
      if (point >= 0xe0000 && point <= 0xe007f) {
        removed.invisible++;
        put(PLACEHOLDER);
        i += 2;

        continue;
      }
    }
    if (
      code === 0x200b ||
      code === 0x200c ||
      code === 0x200d ||
      code === 0x2060 ||
      code === 0xfeff ||
      code === 0x00ad ||
      code === 0x180e
    ) {
      removed.invisible++;
      put(PLACEHOLDER);
      i++;

      continue;
    }

    put(raw[i]);
    i++;
  }

  return { text: out, removed };
}

/** One drawable line: what it says, which stream it came from, when it
 *  was captured, and whether it is the product's own marker rather than
 *  the hook's output. */
export interface TerminalLine {
  /** Stable across re-renders and unique within a step: a record's
   *  sequence, plus which line of that record this is. */
  key: string;
  seq: number;
  stream: LogStream;
  at: string;
  text: string;
  /** True for this product's own words (the truncation marker), which
   *  are drawn differently and are never a hook's bytes. */
  marker: boolean;
}

/**
 * One record, sanitised and split into the lines it draws as.
 *
 * A record carrying two newlines draws as three lines, all attributed to
 * the same sequence, stream and capture time — which is why an embedded
 * newline is not the spoofing risk here that it is in the CLI. The
 * stream label and the timestamp are DOM, drawn from the record, so a
 * hook cannot write itself a different prefix by writing a newline.
 *
 * An empty record draws one empty line rather than nothing, because a
 * hook printing a blank line printed something.
 */
export function sanitizeRecord(
  record: StepLogRecord,
  maxLineChars: number = HARDENED_PROFILE.maxLineChars
): { lines: TerminalLine[]; removed: RemovedCounts } {
  if (record.kind === "truncated") {
    // The engine's marker. Its own text is not drawn: the marker says
    // one thing, in this product's words, so no hook can forge it.
    return {
      lines: [
        {
          key: record.seq + ":m",
          seq: record.seq,
          stream: record.stream,
          at: record.at,
          text: TRUNCATION_MARKER,
          marker: true
        }
      ],
      removed: noRemovals()
    };
  }

  const { text, removed } = sanitizeText(record.text ?? "", maxLineChars);
  // A trailing newline ends the last line rather than starting an empty
  // one: a hook's `echo` writes one and means one line.
  const body = text.endsWith("\n") ? text.slice(0, -1) : text;
  const parts = body.split("\n");
  const lines = parts.map((part, index) => ({
    key: record.seq + ":" + index,
    seq: record.seq,
    stream: record.stream,
    at: record.at,
    text: part,
    marker: false
  }));

  return { lines, removed };
}
