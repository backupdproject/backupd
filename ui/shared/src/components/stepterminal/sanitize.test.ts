/**
 * The filter, against the corpus it exists for (issue #815).
 *
 * The corpus-wide sweeps are the point of this file: every case in
 * hostile.ts is asserted to lose its sequence, keep the hook's own
 * words, and leave nothing behind that could still act. A per-case test
 * for the three or four classes somebody thought of is how the next
 * class arrives unfiltered.
 */
import { describe, expect, it } from "vitest";
import {
  CLIP_NOTICE,
  PLACEHOLDER,
  addRemovals,
  noRemovals,
  removalNotice,
  sanitizeRecord,
  sanitizeText,
  TRUNCATION_MARKER
} from "./sanitize";
import { HOSTILE_CASES } from "./hostile";

const ESC = "\u001b";
const BEL = "\u0007";

describe("the corpus", () => {
  it.each(HOSTILE_CASES.map((hostile) => [hostile.name, hostile] as const))(
    "keeps the hook's words and drops the sequence: %s",
    (_name, hostile) => {
      const { text } = sanitizeText(hostile.raw);

      for (const keep of hostile.keeps ?? []) expect(text, hostile.danger).toContain(keep);
      for (const forbid of hostile.forbids ?? []) expect(text, hostile.danger).not.toContain(forbid);
    }
  );

  it("leaves no escape introducer or control character anywhere in the corpus", () => {
    for (const hostile of HOSTILE_CASES) {
      const { text } = sanitizeText(hostile.raw);
      // ESC, DEL, the C1 range, and every C0 control except tab and the
      // newline this filter produces on purpose.
      expect([...text].filter((ch) => {
        const code = ch.codePointAt(0) ?? 0;
        if (ch === "\t" || ch === "\n") return false;

        return code < 0x20 || code === 0x7f || (code >= 0x80 && code <= 0x9f);
      }), hostile.name).toEqual([]);
    }
  });

  it("leaves no bidi override or unicode line separator anywhere in the corpus", () => {
    for (const hostile of HOSTILE_CASES) {
      const { text } = sanitizeText(hostile.raw);
      expect(text, hostile.name).not.toMatch(/[\u200e\u200f\u202a-\u202e\u2066-\u2069\u2028\u2029]/);
    }
  });
});

describe("hyperlinks", () => {
  it("swallows the URL and keeps the text a reader was shown", () => {
    const { text, removed } = sanitizeText(
      ESC + "]8;;https://evil.example/x" + BEL + "report" + ESC + "]8;;" + BEL
    );

    expect(text).toBe(PLACEHOLDER + "report" + PLACEHOLDER);
    expect(removed.hyperlink).toBe(2);
  });

  it("counts the eight-bit form as a hyperlink too", () => {
    expect(sanitizeText("\u009d8;;https://evil.example\u009ctext").removed.hyperlink).toBe(1);
  });
});

describe("titles, window control and device reports", () => {
  it("classifies OSC 0, 1 and 2 as title sequences", () => {
    const { removed } = sanitizeText(
      ESC + "]0;a" + BEL + ESC + "]1;b" + BEL + ESC + "]2;c" + BEL
    );

    expect(removed.title).toBe(3);
  });

  it("classifies CSI t as window manipulation, whatever its parameters", () => {
    expect(sanitizeText(ESC + "[8;40;120t" + ESC + "[2t" + ESC + "[21t").removed.window).toBe(3);
  });

  it("classifies device attribute and status requests as reports", () => {
    expect(sanitizeText(ESC + "[c" + ESC + "[0c" + ESC + "[6n" + ESC + "[5n").removed.report).toBe(4);
  });

  it("classifies a clipboard write as an escape rather than a title", () => {
    const { removed } = sanitizeText(ESC + "]52;c;AAAA" + BEL);

    expect(removed.escape).toBe(1);
    expect(removed.title).toBe(0);
  });
});

describe("colour", () => {
  it("removes SGR without leaving a placeholder, because nothing is hidden by dropping colour", () => {
    const { text, removed } = sanitizeText(ESC + "[31;1mFAILED" + ESC + "[0m ok");

    expect(text).toBe("FAILED ok");
    expect(removed.format).toBe(2);
  });

  it("still reports what it removed, so the reader is told the colour went", () => {
    const counts = sanitizeText(ESC + "[31mred" + ESC + "[0m").removed;

    expect(removalNotice(counts)).toBe("Removed before drawing: 2 colour sequences.");
  });
});

describe("cursor movement and overwriting", () => {
  it("turns a carriage return into a line break rather than emulating a repaint", () => {
    const { text } = sanitizeText("uploading 99%\rdone");

    expect(text).toBe("uploading 99%\ndone");
  });

  it("treats CRLF as one break", () => {
    expect(sanitizeText("a\r\nb").text).toBe("a\nb");
  });

  it("neutralises a backspace, so a digit cannot be rubbed out", () => {
    const { text, removed } = sanitizeText("exit code 1\u0008\u00080");

    expect(text).toBe("exit code 1" + PLACEHOLDER + PLACEHOLDER + "0");
    expect(removed.control).toBe(2);
  });

  it("keeps tabs, because a hook printing a table is printing information", () => {
    expect(sanitizeText("a\tb").text).toBe("a\tb");
  });
});

describe("sequences that never end", () => {
  it("swallows the rest of the chunk after an unterminated OSC", () => {
    const { text, removed } = sanitizeText("before" + ESC + "]0;never ends");

    expect(text).toBe("before" + PLACEHOLDER);
    expect(removed.escape).toBe(1);
  });

  it("swallows the rest of the chunk after an unterminated CSI", () => {
    expect(sanitizeText("before" + ESC + "[38;2;255").text).toBe("before" + PLACEHOLDER);
  });

  it("neutralises a lone ESC at a chunk boundary", () => {
    expect(sanitizeText("edge" + ESC).text).toBe("edge" + PLACEHOLDER);
  });
});

describe("the line bound", () => {
  it("clips a line that would lock up layout and says so", () => {
    const { text, removed } = sanitizeText("A".repeat(5_000), 100);

    expect(text).toBe("A".repeat(100) + CLIP_NOTICE);
    expect(removed.clipped).toBe(1);
  });

  it("bounds each line on its own", () => {
    const { text, removed } = sanitizeText("A".repeat(200) + "\n" + "B".repeat(200) + "\nshort", 50);

    expect(removed.clipped).toBe(2);
    expect(text.split("\n")[2]).toBe("short");
  });

  it("leaves a line under the bound alone", () => {
    expect(sanitizeText("plain output", 100)).toEqual({ text: "plain output", removed: noRemovals() });
  });
});

describe("records", () => {
  it("splits an embedded newline into lines that all carry the record's own stream and time", () => {
    const { lines } = sanitizeRecord({ seq: 7, stream: "stderr", at: "2026-09-13T02:00:00Z", text: "one\ntwo\n" });

    expect(lines.map((line) => line.text)).toEqual(["one", "two"]);
    expect(lines.every((line) => line.stream === "stderr" && line.seq === 7)).toBe(true);
    expect(new Set(lines.map((line) => line.key)).size).toBe(2);
  });

  it("draws a blank line for a hook that printed one", () => {
    expect(sanitizeRecord({ seq: 1, stream: "stdout", at: "2026-09-13T02:00:00Z", text: "" }).lines).toHaveLength(1);
  });

  it("renders a truncation record in this product's words and never the script's", () => {
    const { lines } = sanitizeRecord({
      seq: 9,
      stream: "stdout",
      at: "2026-09-13T02:00:00Z",
      kind: "truncated",
      text: "backupd: everything is fine, keep going"
    });

    expect(lines).toHaveLength(1);
    expect(lines[0].marker).toBe(true);
    expect(lines[0].text).toBe(TRUNCATION_MARKER);
    expect(lines[0].text).not.toContain("everything is fine");
  });
});

describe("the tally", () => {
  it("says nothing when nothing was removed", () => {
    expect(removalNotice(noRemovals())).toBeNull();
  });

  it("names every class it counted, and pluralises", () => {
    const counts = noRemovals();
    counts.hyperlink = 1;
    counts.control = 3;

    expect(removalNotice(counts)).toBe("Removed before drawing: 1 hyperlink sequence, 3 control characters.");
  });

  it("folds one page's removals into the step's running total", () => {
    const total = noRemovals();
    total.title = 2;
    const page = noRemovals();
    page.title = 1;
    page.bidi = 4;

    addRemovals(total, page);

    expect([total.title, total.bidi]).toEqual([3, 4]);
  });
});
