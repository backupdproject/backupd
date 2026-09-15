/**
 * The step terminal as an operator and an attacker both meet it
 * (issue #815).
 *
 * Every test here runs against a STUB: one method, a fake durable log,
 * and the hostile corpus. There is no client, no transport and no
 * session in the picture, which is exactly what makes the interesting
 * assertions cheap — "the corpus rendered and nothing navigated, fetched,
 * retitled or injected" is one render away.
 *
 * The four groups map to #815's acceptance criteria: the hardened
 * profile, one mounted viewer across a hundred steps, cursor resume with
 * no gaps or duplicates, and the bounds (scrollback, truncation marker,
 * sanitised download).
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import "@shared/design-system/components.css";
import { StepLogTerminal } from "@shared/components/StepLogTerminal";
import { HARDENED_PROFILE, RENDERER } from "@shared/components/stepterminal/profile";
import { TRUNCATION_MARKER } from "@shared/components/stepterminal/sanitize";
import { HOSTILE_CASES, hostileRecords } from "@shared/components/stepterminal/hostile";
import type {
  StepLogRecord,
  WorkflowStepLogPage,
  WorkflowStepSummary
} from "@shared/components/stepterminal/contract";

/** A durable per-step log behind one method, with every call recorded. */
class FakeApi {
  steps = new Map<string, StepLogRecord[]>();
  ended = new Set<string>();
  truncated = new Set<string>();
  calls: { stepId: string; after: number; wait: boolean }[] = [];
  failNext: string | null = null;
  limit = 10_000;

  put(stepId: string, records: StepLogRecord[], options: { ended?: boolean; truncated?: boolean } = {}): void {
    this.steps.set(stepId, records);
    if (options.ended !== false) this.ended.add(stepId);
    if (options.truncated === true) this.truncated.add(stepId);
  }

  callsFor(stepId: string): { after: number; wait: boolean }[] {
    return this.calls.filter((call) => call.stepId === stepId).map(({ after, wait }) => ({ after, wait }));
  }

  workflowStepLogs = (
    _runId: string,
    stepId: string,
    options?: { after?: number; wait?: boolean }
  ): Promise<WorkflowStepLogPage> => {
    const after = options?.after ?? 0;
    this.calls.push({ stepId, after, wait: options?.wait === true });
    if (this.failNext !== null) {
      const message = this.failNext;
      this.failNext = null;

      return Promise.reject(new Error(message));
    }
    const all = this.steps.get(stepId) ?? [];
    const available = all.filter((record) => record.seq > after);
    const records = available.slice(0, this.limit);
    const last = records[records.length - 1];

    return Promise.resolve({
      records,
      cursor: last === undefined ? after : last.seq,
      complete: this.ended.has(stepId) && records.length === available.length,
      truncated: this.truncated.has(stepId)
    });
  };
}

function step(overrides: Partial<WorkflowStepSummary> = {}): WorkflowStepSummary {
  return {
    stepId: "step-1",
    scriptName: "10-quiesce-postgres.sh",
    state: "success",
    phase: "before",
    scope: "set",
    order: 1,
    target: "remote",
    executionConnectionRef: "ssh_app_server",
    remoteHost: "app-01.example.net",
    exitCode: 0,
    durationMs: 4_210,
    startedAt: "2026-09-13T02:00:00Z",
    finishedAt: "2026-09-13T02:00:04Z",
    scriptSha256: "3f786850e387550fdab836ed7e6dc881de23001b1f2b2b6a0cbf0f5c5d5b9f21",
    terminationConfirmed: true,
    ...overrides
  };
}

function line(seq: number, text: string, stream: "stdout" | "stderr" = "stdout"): StepLogRecord {
  return { seq, stream, at: "2026-09-13T02:00:0" + (seq % 10) + "Z", text };
}

const logOf = () => screen.getByRole("log");

beforeEach(() => {
  document.title = "retnd";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("the header", () => {
  it("states the script, the phase, where it ran, the state, the exit code, the duration and the short hash", async () => {
    const api = new FakeApi();
    api.put("step-1", [line(1, "ok")]);
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step()} api={api} />);
    await screen.findByText("ok");

    expect(screen.getByText("10-quiesce-postgres.sh")).toBeInTheDocument();
    expect(screen.getByText("before · set")).toBeInTheDocument();
    expect(screen.getByText("Remote")).toBeInTheDocument();
    expect(screen.getByText("Remote · ssh_app_server · app-01.example.net")).toBeInTheDocument();
    expect(screen.getAllByText("Succeeded").length).toBeGreaterThan(0);
    expect(screen.getByText("0")).toBeInTheDocument();
    expect(screen.getByText("4.2 s")).toBeInTheDocument();
    expect(screen.getByText("3f786850e387")).toBeInTheDocument();
  });

  it("names the host workflow runner for a local step, and never an engine container", async () => {
    const api = new FakeApi();
    api.put("step-1", [line(1, "local output")]);
    render(
      <StepLogTerminal
        runId="run-7"
        stepId="step-1"
        step={step({ target: "local", executionConnectionRef: undefined, remoteHost: undefined })}
        api={api}
      />
    );
    await screen.findByText("local output");

    expect(screen.getByText("Local · host workflow runner")).toBeInTheDocument();
    // The wording rule: a local hook runs on the NAS under the runner,
    // and telling an operator it ran "in the engine container" sends them
    // looking for its side effects in the wrong filesystem.
    expect(document.body.textContent).not.toMatch(/engine container/i);
  });

  it("says an em dash for a hash the runs API does not carry, rather than inventing one", async () => {
    const api = new FakeApi();
    api.put("step-1", [line(1, "x")]);
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step({ scriptSha256: undefined })} api={api} />);
    await screen.findByText("x");

    expect(screen.getByText("Script SHA-256").parentElement?.textContent).toContain("—");
  });

  it("distinguishes a recorded zero exit code from a code nobody recorded", async () => {
    const api = new FakeApi();
    api.put("step-1", [line(1, "x")]);
    const { rerender } = render(
      <StepLogTerminal runId="run-7" stepId="step-1" step={step({ exitCode: 0 })} api={api} />
    );
    await screen.findByText("x");
    expect(screen.getByText("0")).toBeInTheDocument();

    rerender(
      <StepLogTerminal runId="run-7" stepId="step-1" step={step({ exitCode: null, state: "interrupted" })} api={api} />
    );

    expect(screen.getByText("not recorded")).toBeInTheDocument();
  });

  it("warns when this product could not confirm the script's process had gone", async () => {
    const api = new FakeApi();
    api.put("step-1", [line(1, "x")]);
    render(
      <StepLogTerminal
        runId="run-7"
        stepId="step-1"
        step={step({ state: "timed_out", terminationConfirmed: false })}
        api={api}
      />
    );
    await screen.findByText("x");

    expect(screen.getByRole("status").textContent).toContain("could not confirm the script's process had gone");
  });
});

describe("the hardened profile", () => {
  it("publishes the profile it is running under on the log element", async () => {
    const api = new FakeApi();
    api.put("step-1", [line(1, "x")]);
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step()} api={api} />);
    await screen.findByText("x");
    const log = logOf();

    expect(log.getAttribute("data-stdin")).toBe("disabled");
    expect(log.getAttribute("data-link-activation")).toBe("disabled");
    expect(log.getAttribute("data-title-integration")).toBe("disabled");
    expect(log.getAttribute("data-window-manipulation")).toBe("disabled");
    expect(log.getAttribute("data-device-reports")).toBe("disabled");
    expect(log.getAttribute("data-scrollback")).toBe(String(HARDENED_PROFILE.scrollbackLines));
    expect(log.getAttribute("data-renderer")).toBe(RENDERER.name + " rev " + RENDERER.revision);
    expect(log.getAttribute("aria-readonly")).toBe("true");
  });

  it("holds no input, no editable node and no control inside the log", async () => {
    const api = new FakeApi();
    api.put("step-1", hostileRecords());
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step()} api={api} />);
    await waitFor(() => expect(logOf().textContent).toContain("end of flood"));

    expect(
      logOf().querySelectorAll("input, textarea, select, button, [contenteditable], [tabindex]:not([tabindex='-1'])")
    ).toHaveLength(0);
  });

  it("takes no input: typing into it changes nothing and asks the engine for nothing", async () => {
    const user = userEvent.setup();
    const api = new FakeApi();
    api.put("step-1", [line(1, "only line")]);
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step()} api={api} />);
    await screen.findByText("only line");
    const log = logOf();
    const before = log.textContent;
    const reads = api.calls.length;

    log.focus();
    await user.keyboard("rm -rf /{Enter}whoami{Enter}");

    expect(log.textContent).toBe(before);
    expect(api.calls).toHaveLength(reads);
  });

  it("renders the whole hostile corpus inertly: no link, no fetch, no title change, no markup", async () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    const injected: string[] = [];
    const descriptor = Object.getOwnPropertyDescriptor(Element.prototype, "innerHTML");
    Object.defineProperty(Element.prototype, "innerHTML", {
      ...descriptor,
      set(value: string) {
        injected.push(String(value));
        descriptor?.set?.call(this, value);
      }
    });

    try {
      const api = new FakeApi();
      api.put("step-1", hostileRecords());
      render(<StepLogTerminal runId="run-7" stepId="step-1" step={step()} api={api} />);
      await waitFor(() => expect(logOf().textContent).toContain("end of flood"));
      const log = logOf();

      // Nothing became a link, and nothing is clickable.
      expect(log.querySelectorAll("a, [href], [onclick], iframe, img, script, object, embed")).toHaveLength(0);
      // No escape introducer survived into the DOM, and neither did any
      // character that draws nothing: a bidi override, a zero-width
      // joiner, a BOM, a soft hyphen or a tag-range smuggled letter is
      // invisible on screen and changes what the line appears to say.
      expect(log.textContent).not.toMatch(/[\u001b\u009b\u009d\u0007\u0000]/);
      expect(log.textContent).not.toMatch(
        /[\u200b-\u200f\u202a-\u202e\u2060\u2066-\u2069\ufeff\u00ad\u180e\u2028\u2029]/
      );
      expect(
        [...(log.textContent ?? "")].some((ch) => {
          const point = ch.codePointAt(0) ?? 0;

          return point >= 0xe0000 && point <= 0xe007f;
        })
      ).toBe(false);
      // The hook's own words did survive: a filter that ate the log is
      // its own failure.
      for (const hostile of HOSTILE_CASES) {
        for (const keep of hostile.keeps ?? []) expect(log.textContent, hostile.name).toContain(keep);
      }
      // And the payloads only a swallowed sequence could have carried are
      // gone. Each of these is unique to one case in the corpus, so a
      // second case legitimately printing the same words (the markup
      // case prints a URL as text) cannot mask a real leak.
      for (const payload of [
        "evil.example/steal",
        "evil.example/x",
        "all backups verified",
        "ZXZpbCBjb21tYW5k",
        "8;200;500t",
        "?1049h",
        "a title that never ends"
      ]) {
        expect(log.textContent, payload).not.toContain(payload);
      }
      // Markup in the output is TEXT: the tags are readable, and no
      // element was created from them.
      expect(log.textContent).toContain("<script>alert(1)</script>");
      expect(log.querySelector("script")).toBeNull();
      // Nothing in this component's path injects HTML.
      expect(injected).toEqual([]);
      // Nothing fetched, and the title is the one the document had.
      expect(fetchSpy).not.toHaveBeenCalled();
      expect(document.title).toBe("retnd");
      // Only the read the component drives itself was made.
      expect(api.calls.every((call) => call.stepId === "step-1")).toBe(true);
    } finally {
      if (descriptor !== undefined) Object.defineProperty(Element.prototype, "innerHTML", descriptor);
    }
  });

  it("tells the reader what it removed, rather than showing less without saying so", async () => {
    const api = new FakeApi();
    api.put("step-1", hostileRecords());
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step()} api={api} />);
    await waitFor(() => expect(logOf().textContent).toContain("end of flood"));

    const notice = screen.getByText(/Removed before drawing/);
    expect(notice.textContent).toContain("hyperlink sequence");
    expect(notice.textContent).toContain("window-title sequence");
    expect(notice.textContent).toContain("window-control sequence");
    expect(notice.textContent).toContain("device-report sequence");
    expect(notice.textContent).toContain("text-direction override");
  });

  it("labels the merged view as capture order, which is the only order it can claim", async () => {
    const api = new FakeApi();
    api.put("step-1", [line(1, "out"), line(2, "err", "stderr")]);
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step()} api={api} />);
    await screen.findByText("err");

    expect(screen.getByText(/capture order/)).toBeInTheDocument();
    // stdout and stderr stay distinguishable in text, not by colour
    // alone: the label is on every line.
    const rows = within(logOf()).getAllByText(/^std(out|err)$/);
    expect(rows.map((row) => row.textContent)).toEqual(["stdout", "stderr"]);
  });

  it("never loads terminal code at runtime and never injects markup, as a sweep over its own sources", async () => {
    const sources: Record<string, string> = import.meta.glob("./{StepLogTerminal,stepterminal/*}.{ts,tsx}", {
      query: "?raw",
      import: "default",
      eager: true
    });
    const shipped = Object.entries(sources).filter(([path]) => !path.includes(".test."));
    // A sweep that walked nothing passes vacuously.
    expect(shipped.length).toBeGreaterThan(4);

    for (const [path, text] of shipped) {
      expect(text, path).not.toMatch(/innerHTML|outerHTML|insertAdjacentHTML|dangerouslySetInnerHTML/);
      expect(text, path).not.toMatch(/createContextualFragment|document\.write|new Function|eval\(/);
      // No runtime-loaded code of any kind: no dynamic import, no script
      // element, no remote URL to fetch a renderer from.
      expect(text, path).not.toMatch(/import\s*\(|createElement\(["']script|requirejs|unpkg|jsdelivr|cdn\./);
      // No terminal emulator is depended on, so none of its options can
      // be left switched on — asserted as the whole dependency closure
      // rather than by name, which is both stronger and free of the
      // false positive a filter's own comments would otherwise cause by
      // naming the emulators whose behaviour they argue about.
      for (const [, specifier] of text.matchAll(/from\s+["']([^"']+)["']/g)) {
        const firstParty = specifier.startsWith(".") || specifier.startsWith("@shared/");
        expect(firstParty || specifier === "react", path + " imports " + specifier).toBe(true);
      }
      expect(text, path).not.toMatch(/WebLinksAddon|new Terminal\(|loadAddon/);
    }
  });
});

describe("one viewer, a hundred steps", () => {
  it("keeps exactly one instantiated log across a hundred step changes", async () => {
    const api = new FakeApi();
    for (let i = 1; i <= 100; i++) api.put("step-" + i, [line(i, "output of step " + i)]);
    const { rerender } = render(
      <StepLogTerminal runId="run-7" stepId="step-1" step={step({ stepId: "step-1" })} api={api} />
    );
    await screen.findByText("output of step 1");
    const firstNode = logOf();

    for (let i = 2; i <= 100; i++) {
      rerender(
        <StepLogTerminal
          runId="run-7"
          stepId={"step-" + i}
          step={step({ stepId: "step-" + i, scriptName: i + "-hook.sh" })}
          api={api}
        />
      );
      await screen.findByText("output of step " + i);
      // The same DOM node throughout: a viewer that remounted would have
      // built a hundred scroll containers and thrown away a hundred.
      expect(screen.getAllByRole("log")).toHaveLength(1);
      expect(logOf()).toBe(firstNode);
    }

    expect(screen.getByText("100-hook.sh")).toBeInTheDocument();
    expect(logOf().textContent).not.toContain("output of step 99");
  });

  it("stops reading the step it left, so a hundred steps are not a hundred followers", async () => {
    const api = new FakeApi();
    api.put("step-1", [line(1, "first")], { ended: false });
    api.put("step-2", [line(2, "second")]);
    const { rerender } = render(
      <StepLogTerminal runId="run-7" stepId="step-1" step={step({ stepId: "step-1" })} api={api} />
    );
    await screen.findByText("first");

    rerender(<StepLogTerminal runId="run-7" stepId="step-2" step={step({ stepId: "step-2" })} api={api} />);
    await screen.findByText("second");
    const readsOfOne = api.callsFor("step-1").length;
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 30));
    });

    expect(api.callsFor("step-1")).toHaveLength(readsOfOne);
  });

  it("switches back to a step already read without reading it again", async () => {
    const api = new FakeApi();
    api.put("step-1", [line(1, "alpha"), line(2, "beta")]);
    api.put("step-2", [line(3, "gamma")]);
    const { rerender } = render(
      <StepLogTerminal runId="run-7" stepId="step-1" step={step({ stepId: "step-1" })} api={api} />
    );
    await screen.findByText("beta");
    const readsWhileFirstSelected = api.callsFor("step-1").length;

    rerender(<StepLogTerminal runId="run-7" stepId="step-2" step={step({ stepId: "step-2" })} api={api} />);
    await screen.findByText("gamma");
    rerender(<StepLogTerminal runId="run-7" stepId="step-1" step={step({ stepId: "step-1" })} api={api} />);

    // The history is on screen at once, because it never left: this is
    // what the kept scrollback is FOR.
    expect(screen.getByText("beta")).toBeInTheDocument();

    // And it costs nothing durable. This assertion used to require the
    // opposite -- `toBeGreaterThan(1)`, a re-read on every return -- and
    // #916/#917's browser evidence is what settled which of the two the
    // cache is supposed to mean: a complete page held in full is
    // rendered from memory, so an operator comparing a failure with the
    // step before it pays one read per step and not one per click. A
    // step still running, or one whose page ended mid-log, is not held
    // "in full" and resumes its follower -- which the running-step cases
    // in this file cover.
    await act(async () => {
      // `new Promise` rather than Promise.withResolvers, because this
      // package's TypeScript lib target predates it (tsc: "Property
      // 'withResolvers' does not exist ... Try changing the 'lib'
      // compiler option to 'es2024' or later") and the test above this
      // one waits the same way.
      await new Promise((resolve) => setTimeout(resolve, 40));
    });
    expect(api.callsFor("step-1")).toHaveLength(readsWhileFirstSelected);

    // And no line is on screen twice.
    expect(screen.getAllByText("beta")).toHaveLength(1);
  });
});

describe("following, pausing and reconnecting", () => {
  it("asks the engine to hold the read while following, and stops asking when paused", async () => {
    const user = userEvent.setup();
    const api = new FakeApi();
    api.put("step-1", [line(1, "running output")], { ended: false });
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step({ state: "running" })} api={api} />);
    await screen.findByText("running output");
    await waitFor(() => expect(api.callsFor("step-1").some((call) => call.wait)).toBe(true));

    await user.click(screen.getByRole("button", { name: "Following output" }));
    const reads = api.callsFor("step-1").length;
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 30));
    });

    expect(screen.getByRole("button", { name: "Follow output" })).toHaveAttribute("aria-pressed", "false");
    expect(api.callsFor("step-1")).toHaveLength(reads);
    expect(screen.getByText(/paused at sequence 1/)).toBeInTheDocument();
  });

  it("resumes a paused follow from the last line drawn, with no gap and no repeat", async () => {
    const user = userEvent.setup();
    const api = new FakeApi();
    api.put("step-1", [line(1, "one"), line(2, "two")], { ended: false });
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step({ state: "running" })} api={api} />);
    await screen.findByText("two");

    await user.click(screen.getByRole("button", { name: "Following output" }));
    api.put("step-1", [line(1, "one"), line(2, "two"), line(3, "three"), line(4, "four")]);
    await user.click(screen.getByRole("button", { name: "Follow output" }));

    await screen.findByText("four");
    expect(logOf().textContent).toBe(
      Array.from(logOf().querySelectorAll(":scope > div"))
        .map((row) => row.textContent)
        .join("")
    );
    const drawn = Array.from(logOf().querySelectorAll(":scope > div")).map((row) =>
      (row.textContent ?? "").replace(/^\d\d:\d\d:\d\dstd(out|err)/, "")
    );
    expect(drawn).toEqual(["one", "two", "three", "four"]);
  });

  it("offers a reconnect after a failed read, and loses nothing durable when it is taken", async () => {
    const user = userEvent.setup();
    const api = new FakeApi();
    api.put("step-1", [line(1, "before the drop")], { ended: false });
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step({ state: "running" })} api={api} />);
    await screen.findByText("before the drop");

    api.failNext = "the connection was reset";
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("the connection was reset"), {
      timeout: 3_000
    });
    expect(screen.getByRole("alert").textContent).toContain("resumes at sequence 1");

    api.put("step-1", [line(1, "before the drop"), line(2, "after the drop")]);
    await user.click(screen.getByRole("button", { name: "Reconnect" }));

    await screen.findByText("after the drop");
    expect(screen.getAllByText("before the drop")).toHaveLength(1);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByText(/replayed from the durable log/)).toBeInTheDocument();
  });
});

describe("the bounds", () => {
  it("renders the truncation marker where the engine put it in the sequence", async () => {
    const api = new FakeApi();
    api.put(
      "step-1",
      [
        line(1, "first half"),
        { seq: 2, stream: "stdout", at: "2026-09-13T02:00:02Z", kind: "truncated", text: "" },
        line(3, "still running")
      ],
      { truncated: true }
    );
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step()} api={api} />);
    await screen.findByText("still running");

    const rows = Array.from(logOf().querySelectorAll(":scope > div")).map((row) => row.textContent ?? "");
    const marker = rows.findIndex((row) => row.includes("stopped recording here"));
    expect(marker).toBeGreaterThan(-1);
    // In position: the missing bytes are after it, not before.
    expect(rows[marker - 1]).toContain("first half");
    expect(rows[marker + 1]).toContain("still running");
    expect(screen.getAllByText(TRUNCATION_MARKER)).toHaveLength(1);
  });

  it("states the truncation even when the marker record is not in the window it holds", async () => {
    const api = new FakeApi();
    api.put("step-1", [line(1, "tail of a truncated log")], { truncated: true });
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step()} api={api} />);
    await screen.findByText("tail of a truncated log");

    expect(screen.getByText(TRUNCATION_MARKER)).toBeInTheDocument();
  });

  it("bounds the browser's scrollback and says what it dropped, naming what still has it", async () => {
    const api = new FakeApi();
    const overflow = HARDENED_PROFILE.scrollbackLines + 120;
    api.put(
      "step-1",
      Array.from({ length: overflow }, (_, i) => line(i + 1, "line " + (i + 1)))
    );
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step()} api={api} />);
    // Five thousand rows in jsdom is slow, and the bound is the thing
    // being asserted, so it is rendered for real rather than shrunk.
    await screen.findByText("line " + overflow, undefined, { timeout: 30_000 });

    expect(screen.getByText(/earlier lines are not held in this browser any more/).textContent).toContain(
      "retnd workflow run log --cursor 0"
    );
    // The oldest went, the newest stayed, and the count is the bound.
    expect(screen.queryByText("line 1")).toBeNull();
    expect(logOf().querySelectorAll(":scope > div")).toHaveLength(HARDENED_PROFILE.scrollbackLines + 1);
  });
});

describe("copy and download", () => {
  it("downloads the sanitised content, with the caveats a file read away from this screen needs", async () => {
    const user = userEvent.setup();
    // Recorded at the Blob rather than read back from it: jsdom's Blob
    // has no text(), and what is being asserted is the bytes the
    // component handed the browser to save.
    const written: string[] = [];
    class RecordingBlob {
      constructor(parts: string[]) {
        written.push(parts.join(""));
      }
    }
    vi.stubGlobal("Blob", RecordingBlob);
    vi.stubGlobal("URL", { ...URL, createObjectURL: () => "blob:step-log", revokeObjectURL: () => {} });
    const clicked = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    const api = new FakeApi();
    api.put("step-1", hostileRecords());
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step()} api={api} />);
    await waitFor(() => expect(logOf().textContent).toContain("end of flood"));

    await user.click(screen.getByRole("button", { name: "Download .txt" }));

    expect(clicked).toHaveBeenCalledTimes(1);
    const saved = written[0];
    // The file is the sanitised content: a download is opened in a
    // terminal, which is the one place these sequences still work.
    expect(saved).not.toMatch(/[\u001b\u009b\u009d\u0007]/);
    expect(saved).not.toContain("evil.example/steal");
    expect(saved).not.toContain("all backups verified");
    // And it still carries the hook's words, the step's facts and what
    // the ordering does not claim.
    expect(saved).toContain("end of flood");
    expect(saved).toContain("10-quiesce-postgres.sh");
    expect(saved).toContain("run-7");
    expect(saved).toContain("3f786850e387");
    expect(saved).toContain("capture order");
    expect(saved).toContain("stdout");
    expect(saved).toContain("stderr");
  });

  it("names the saved file after the script and the step", async () => {
    const user = userEvent.setup();
    vi.stubGlobal("URL", { ...URL, createObjectURL: () => "blob:x", revokeObjectURL: () => {} });
    let name = "";
    const clicked = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      name = this.download;
    });
    const api = new FakeApi();
    api.put("step-1", [line(1, "x")]);
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step()} api={api} />);
    await screen.findByText("x");

    await user.click(screen.getByRole("button", { name: "Download .txt" }));

    expect(clicked).toHaveBeenCalled();
    expect(name).toMatch(/^retnd-step-10-quiesce-postgres\.sh-step-1-.*\.txt$/);
  });

  it("copies exactly what the file would hold, so the two cannot disagree", async () => {
    const user = userEvent.setup();
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    const api = new FakeApi();
    api.put("step-1", [line(1, "copy me"), line(2, "and me", "stderr")]);
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step()} api={api} />);
    await screen.findByText("and me");

    await user.click(screen.getByRole("button", { name: "Copy" }));

    const copied = writeText.mock.calls[0][0] as string;
    expect(copied).toContain("copy me");
    expect(copied).toContain("and me");
    expect(copied).toContain("# retnd workflow step log");
    expect(await screen.findByRole("button", { name: "Copied" })).toBeInTheDocument();
  });
});

describe("the toolbar", () => {
  it("turns timestamps off without losing the stream labels", async () => {
    const user = userEvent.setup();
    const api = new FakeApi();
    api.put("step-1", [line(1, "with a time")]);
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step()} api={api} />);
    await screen.findByText("with a time");
    expect(within(logOf()).getByText(/^\d\d:\d\d:\d\d$/)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Timestamps" }));

    expect(within(logOf()).queryByText(/^\d\d:\d\d:\d\d$/)).toBeNull();
    expect(within(logOf()).getByText("stdout")).toBeInTheDocument();
  });

  it("toggles wrapping on the drawn line rather than on the text", async () => {
    const user = userEvent.setup();
    const api = new FakeApi();
    api.put("step-1", [line(1, "a long line that a reader may want to see unwrapped")]);
    render(<StepLogTerminal runId="run-7" stepId="step-1" step={step()} api={api} />);
    const text = await screen.findByText("a long line that a reader may want to see unwrapped");
    expect(text.style.whiteSpace).toBe("pre-wrap");

    await user.click(screen.getByRole("button", { name: "Wrap" }));

    expect(screen.getByText("a long line that a reader may want to see unwrapped").style.whiteSpace).toBe("pre");
  });

  it("says what an empty log means, which is four different things", async () => {
    const api = new FakeApi();
    api.put("step-1", []);
    const { rerender } = render(
      <StepLogTerminal runId="run-7" stepId="step-1" step={step({ state: "pending" })} api={api} />
    );
    expect(await screen.findByText("This step has not run yet.")).toBeInTheDocument();

    rerender(<StepLogTerminal runId="run-7" stepId="step-1" step={step({ state: "skipped" })} api={api} />);
    expect(screen.getByText("This step was skipped, so it wrote nothing.")).toBeInTheDocument();

    rerender(<StepLogTerminal runId="run-7" stepId="step-1" step={step({ state: "success" })} api={api} />);
    await waitFor(() =>
      expect(screen.getByText("This step wrote nothing to stdout or stderr.")).toBeInTheDocument()
    );
  });
});
