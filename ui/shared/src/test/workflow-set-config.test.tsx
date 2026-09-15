/**
 * One backup set's workflow surface (issue #814, screen 3), and the hold
 * that stops it running.
 *
 * Three properties, each of which a plausible implementation gets wrong:
 *
 *   - a set with NO hooks anywhere shows one sentence and nothing else.
 *     The failure mode this whole wave is one edit away from is an empty
 *     table, an empty findings list and an empty run history on every
 *     existing backup set's page.
 *   - an unresolved recovery hold makes the per-set Run control
 *     unavailable and offers both ways out. Leaving it pressable would be
 *     offering a control the engine refuses every time, which reads as
 *     broken rather than as refused; and an unreadable hold list must not
 *     be treated as "no holds".
 *   - an SFTP-only source is a supported posture, not a fault. Its
 *     capability checks report "not examined" rather than a green tick,
 *     because nothing looked.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { ApiProvider } from "@shared/api/ApiContext";
import { RetndError } from "@shared/api/contracts";
import type {
  RetndApi,
  WorkflowLintFinding,
  WorkflowScriptLint,
  WorkflowSourceExcerpt,
  WorkflowValidatedScript
} from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { BackupSetWorkflowCard } from "@shared/pages/BackupSetWorkflowCard";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import { act } from "@testing-library/react";
import { backupSetPath } from "@shared/utilities/routes";
import type { VersionInfo } from "@shared/types/operation";

/** The set whose cleanup could not be finished, so it is refusing to
 *  run at all. It is also the set with the richest hook configuration,
 *  which is deliberate: a held set must keep every read on its page,
 *  because an operator diagnosing a hold needs the configuration, the
 *  validation and the run that caused it. */
const HELD = { source: "production", set: "postgres-primary" };
/** A set that configures hooks, is not held, and whose own source
 *  connection is SFTP-only — named as its execution connection by the
 *  "source/set" spelling the engine resolves. */
const SFTP_ONLY = { source: "production", set: "billing-mysql" };
/** A set whose hooks are all local, against a runner that is not
 *  answering. */
const LOCAL_ONLY = { source: "production", set: "auth-config" };
/** A set that configures no hooks of its own. */
const QUIET = { source: "media", set: "weekly-archive" };

const VERSION: VersionInfo = {
  api: "v1",
  service: "1.3.0",
  buildCommit: "9f4c1ab",
  goVersion: "go1.27.0",
  engine: "1.68.2",
  configRevision: "cfg_9f4c1ab",
  ready: true,
  compatible: true
};

function seedVersion() {
  act(() => {
    graph.commit("test/seed-version", (tx) =>
      tx.set(versionNode, { data: VERSION, error: null, loading: false })
    );
  });
}

function renderCard(api: RetndApi, target: { source: string; set: string }) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <BackupSetWorkflowCard source={target.source} set={target.set} readOnly={false} />
      </ApiProvider>
    </MemoryRouter>
  );
}

function renderDetail(api: RetndApi, target: { source: string; set: string }) {
  return render(
    <MemoryRouter initialEntries={[backupSetPath(target.source, target.set)]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={false} />} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

/** One finding's row, by the check id in it, so a claim about a check is
 *  scoped to that check rather than to the report. */
function findingRow(check: string): HTMLElement {
  const row = screen.getByText(check).closest("div");
  if (row === null) throw new Error("no finding row for " + check);
  return row;
}

afterEach(() => {
  cleanup();
  resetGraphForTests();
  vi.restoreAllMocks();
});

describe("a backup set with no hooks", () => {
  it("says so in one sentence and renders no table, findings or history", async () => {
    const api = createMockApi();
    // The deployment's own global stages are what make this interesting:
    // a set can configure nothing and still have hooks run against it, so
    // the quiet path has to be the case where BOTH are empty.
    const settings = await createMockApi().getWorkflowSettings();
    vi.spyOn(api, "getWorkflowSettings").mockResolvedValue({
      ...settings,
      beforeDir: "",
      afterDir: ""
    });

    renderCard(api, QUIET);

    expect(await screen.findByText(/No hooks are configured for this backup set/)).toBeTruthy();
    expect(screen.queryByText("Discovered scripts")).toBeNull();
    expect(screen.queryByText("Recent workflow runs")).toBeNull();
    expect(screen.queryByRole("button", { name: /Check this set's hooks/ })).toBeNull();
  });

  it("still shows the panel when the deployment wraps every set in global hooks", async () => {
    // The positive control for the case above: a set configuring nothing
    // of its own is NOT a set with nothing to say, because the global
    // stages run against it.
    renderCard(createMockApi(), QUIET);

    expect(await screen.findByText(/inheriting the deployment's global stages only/)).toBeTruthy();
    expect(screen.getByText("Discovered scripts")).toBeTruthy();
  });
});

describe("the configuration a set pins", () => {
  it("tells a pinned script timeout from an inherited one", async () => {
    renderCard(createMockApi(), HELD);
    await screen.findByText("Discovered scripts");

    expect(screen.getByText("120s (pinned on this set)")).toBeTruthy();
  });

  it("says a set that pins nothing follows the deployment's value", async () => {
    renderCard(createMockApi(), SFTP_ONLY);
    await screen.findByText("Discovered scripts");

    expect(screen.getByText("300s (inherited)")).toBeTruthy();
  });

  it("offers the deployment's execution connections, and says what an SFTP-only source means", async () => {
    renderCard(createMockApi(), SFTP_ONLY);
    const picker = await screen.findByLabelText("Execute remote hooks over");

    expect(
      within(picker).getByRole("option", { name: /None — remote hooks cannot run for this set/ })
    ).toBeTruthy();
    expect(within(picker).getByRole("option", { name: "postgres-exec" })).toBeTruthy();
    expect(screen.getByText(/An SFTP-only source can move bytes and cannot run a hook/)).toBeTruthy();
  });

  it("writes exactly the connection chosen, and nothing else about the set", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    const patch = vi.spyOn(api, "patchBackupSetWorkflow");

    renderCard(api, SFTP_ONLY);
    const picker = await screen.findByLabelText("Execute remote hooks over");
    await user.selectOptions(picker, "billing-exec");

    await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
    expect(patch.mock.calls[0][2]).toEqual({ remoteExecConnectionRef: "billing-exec" });
  });
});

describe("the hook check", () => {
  it("is not run on mount, because it opens connections to the source host", async () => {
    const api = createMockApi();
    const validation = vi.spyOn(api, "getBackupSetWorkflowValidation");

    renderCard(api, HELD);
    await screen.findByText("Discovered scripts");

    expect(validation).not.toHaveBeenCalled();
    expect(screen.getByText(/Nothing has been checked yet in this session/)).toBeTruthy();
  });

  it("reports a discovered script's order, target, size and hash once asked", async () => {
    const user = userEvent.setup();
    renderCard(createMockApi(), HELD);
    await screen.findByText("Discovered scripts");

    await user.click(screen.getByRole("button", { name: /Check this set's hooks/ }));

    expect(await screen.findByText("before/20-freeze-db.local.sh")).toBeTruthy();
    const row = screen.getByText("before/20-freeze-db.local.sh").closest("tr");
    if (row === null) throw new Error("no row for the local script");
    expect(within(row).getByText("Local Host")).toBeTruthy();
    expect(within(row).getByText("Host Workflow Runner")).toBeTruthy();
    // A seven-digit hash, off the report and not invented.
    expect(within(row).getByText("b70c1d5")).toBeTruthy();
  });

  it("reports an SFTP-only source the way the validator does: the connection resolves and the capability fails", async () => {
    const user = userEvent.setup();
    renderCard(createMockApi(), SFTP_ONLY);
    await screen.findByText("Discovered scripts");

    await user.click(screen.getByRole("button", { name: /Check this set's hooks/ }));

    expect(await screen.findByText(/restricted to SFTP/)).toBeTruthy();
    // Which check carries which verdict is the half that matters, and it
    // is the validator's own shape: the connection RESOLVES (a
    // "source/set" reference is a real spelling), the CAPABILITY probe is
    // what discovers the far side will not open an exec channel, and the
    // syntax check is then skipped with the validator's own sentence.
    expect(within(findingRow("exec_connection")).getByText("ok")).toBeTruthy();
    expect(within(findingRow("exec_capability")).getByText("error")).toBeTruthy();
    const syntax = findingRow("remote_bash_syntax");
    expect(within(syntax).getByText("not examined")).toBeTruthy();
    expect(within(syntax).queryByText("ok")).toBeNull();
    // Two verdicts, kept apart: the backups are fine and the hooks are
    // not.
    expect(screen.getByText("Valid for backup")).toBeTruthy();
    expect(screen.getByText("Hooks not valid")).toBeTruthy();
  });
});

describe("the runner-unavailable report", () => {
  it("reports a runner that did not answer, with its local syntax check NOT EXAMINED", async () => {
    const user = userEvent.setup();
    renderCard(createMockApi(), LOCAL_ONLY);
    await screen.findByText("Discovered scripts");

    await user.click(screen.getByRole("button", { name: /Check this set's hooks/ }));

    // Liveness is reported HERE and not on the Settings card, because
    // this is the read that opens the socket.
    expect(await screen.findByText(/did not answer on \/run\/backupd\/hooks\.sock/)).toBeTruthy();
    expect(within(findingRow("runner_health")).getByText("error")).toBeTruthy();
    const local = findingRow("local_bash_syntax");
    expect(within(local).getByText("not examined")).toBeTruthy();
    expect(within(local).queryByText("ok")).toBeNull();
    // And a source that could not be reached at all, which is a
    // different sentence from an SFTP-only account refusing exec.
    expect(screen.getByText(/did not open a connection: dial tcp/)).toBeTruthy();
  });
});

describe("the exec-connection picker's own-source option", () => {
  it("offers this set's own source connection, by the reference the engine resolves", async () => {
    renderCard(createMockApi(), SFTP_ONLY);
    const picker = await screen.findByLabelText("Execute remote hooks over");

    // `backup-set workflow patch --exec-connection source/set` is the CLI
    // spelling of this, so a picker without it is a parity gap.
    const own = within(picker).getByRole("option", {
      name: /This set's own source connection \(only if it can execute\)/
    }) as HTMLOptionElement;
    expect(own.value).toBe("production/billing-mysql");
    // And it is what this set currently names, so the picker shows the
    // real configuration rather than "None".
    expect((picker as HTMLSelectElement).value).toBe("production/billing-mysql");
  });

  it("keeps a chosen connection this deployment no longer declares, rather than reading as None", async () => {
    const api = createMockApi();
    const workflow = await createMockApi().getBackupSetWorkflow("production", "postgres-primary");
    vi.spyOn(api, "getBackupSetWorkflow").mockResolvedValue({
      ...workflow,
      remoteExecConnectionRef: "retired-exec"
    });

    renderCard(api, HELD);
    const picker = (await screen.findByLabelText("Execute remote hooks over")) as HTMLSelectElement;

    // A select whose value matches no option renders as the first one, so
    // this set would read as "None" and the next save would PATCH the
    // operator's configuration away.
    expect(picker.value).toBe("retired-exec");
    expect(within(picker).getByRole("option", { name: /retired-exec — not declared/ })).toBeTruthy();
    expect(screen.getByText(/which this deployment no longer declares/)).toBeTruthy();
  });
});

describe("an unresolved recovery hold", () => {
  it("makes the per-set Run control unavailable and says why", async () => {
    seedVersion();
    renderDetail(createMockApi(), HELD);

    const runControl = await screen.findByRole("button", { name: "Run this backup set" });
    await waitFor(() => expect(runControl).toBeDisabled());
    expect(
      screen.getByText(/This backup set will not run until a workflow run is accounted for/)
    ).toBeTruthy();
    expect(screen.getByText(/may still be quiesced, mounted or paused/)).toBeTruthy();
  });

  it("leaves a set with no hold running normally", async () => {
    // The positive control. Without it, the assertion above would pass
    // against a page that disabled Run for every set.
    seedVersion();
    renderDetail(createMockApi(), SFTP_ONLY);

    const runControl = await screen.findByRole("button", { name: "Run this backup set" });
    await waitFor(() => expect(runControl).toBeEnabled());
    expect(screen.queryByText(/will not run until a workflow run is accounted for/)).toBeNull();
  });

  it("gives back the Run control once the cleanup is resumed", async () => {
    const user = userEvent.setup();
    seedVersion();
    const api = createMockApi();
    const resume = vi.spyOn(api, "resumeWorkflowCleanup");
    renderDetail(api, HELD);

    const runControl = await screen.findByRole("button", { name: "Run this backup set" });
    await waitFor(() => expect(runControl).toBeDisabled());

    await user.click(screen.getByRole("button", { name: "Resume cleanup" }));

    await waitFor(() => expect(resume).toHaveBeenCalledWith("wfr_2f91a4"));
    await waitFor(() => expect(runControl).toBeEnabled(), { timeout: 3_000 });
    expect(screen.queryByText(/will not run until a workflow run is accounted for/)).toBeNull();
  });

  it("refuses to record an acknowledgement with no reason, and sends the reason typed", async () => {
    const user = userEvent.setup();
    seedVersion();
    const api = createMockApi();
    const acknowledge = vi.spyOn(api, "acknowledgeWorkflowRecovery");
    renderDetail(api, HELD);

    await screen.findByText(/This backup set will not run until a workflow run is accounted for/);
    await user.click(screen.getByRole("button", { name: /Acknowledge/ }));

    const confirm = screen.getByRole("button", { name: "Record acknowledgement" });
    // The service requires a reason, so a request that cannot succeed
    // never leaves the browser.
    expect(confirm).toBeDisabled();

    await user.type(
      screen.getByLabelText("What was done about this run"),
      "thawed the database by hand"
    );
    await waitFor(() => expect(confirm).toBeEnabled());
    await user.click(confirm);

    await waitFor(() =>
      expect(acknowledge).toHaveBeenCalledWith("wfr_2f91a4", "thawed the database by hand")
    );
  });

  it("does not offer a Run when the hold list itself could not be read", async () => {
    seedVersion();
    const api = createMockApi();
    vi.spyOn(api, "workflowRecovery").mockRejectedValue(
      new RetndError({
        code: "WORKFLOW_ENGINE_UNAVAILABLE",
        message: "the workflow engine is not answering",
        correlationId: "cid_holds503"
      })
    );

    renderDetail(api, SFTP_ONLY);

    const runControl = await screen.findByRole("button", { name: "Run this backup set" });
    // An unreadable hold list treated as "no holds" would offer a run the
    // engine is about to refuse, so it is refused here and said out loud.
    await waitFor(() => expect(runControl).toBeDisabled());
    expect(
      screen.getByText(/Whether a workflow run is holding this backup set could not be read/)
    ).toBeTruthy();
  });
});

/**
 * retnd's own shell verification on this card (issue #906).
 *
 * The rules these cases exist for, in order of how badly they read when
 * broken:
 *
 *   - a script NOBODY READ must never draw as clean. Its findings list is
 *     empty because no rule ran, and a surface that tests "no findings"
 *     before "not examined" paints an unread hook green. That is this
 *     product claiming it proved something it never looked at.
 *   - a script that DOES NOT PARSE must say so with its position. The
 *     findings list is empty there too, for a second unrelated reason.
 *   - the ERROR group comes first. Only a parse error and an
 *     error-severity finding refuse a save; a panel in arrival order
 *     buries the one row an operator came to find under three that do
 *     not block anything.
 *   - a refused save has to name every blocking script, its directory and
 *     each finding's position, because that is what the operator has to
 *     go and edit. A one-line "not saved" is a dead end.
 */
const NO_SOURCE: WorkflowSourceExcerpt = { lines: [] };

const RICH_FINDINGS: WorkflowLintFinding[] = [
  {
    code: "BSH001",
    severity: "info",
    line: 14,
    col: 12,
    message: "this variable expansion is not quoted, so the shell splits its value on whitespace",
    // The line AFTER the reported one is cut, which is the case that has
    // to be visible rather than silent: an operator comparing this
    // against their file would otherwise be comparing against a line
    // their file does not contain.
    excerpt: {
      lines: [
        { number: 13, text: "# ship it while the source is still frozen", truncated: false },
        { number: 14, text: "  rsync -a $dest \"$REMOTE_HOST:/srv/\"", truncated: false },
        { number: 15, text: "  # keep this in step with the retention job, which expects", truncated: true }
      ]
    }
  },
  {
    code: "BSH003",
    severity: "error",
    line: 12,
    col: 8,
    message: "this recursive, forced delete targets /var whenever the expansion in it is empty",
    // Column 8 of line 12 is the quote that opens the expansion, so the
    // caret the panel draws lands under it and nowhere else.
    excerpt: {
      lines: [
        { number: 11, text: "# clear anything the last run left behind", truncated: false },
        { number: 12, text: "rm -rf \"$STAGING/var\"", truncated: false },
        { number: 13, text: "mkdir -p \"$STAGING/var\"", truncated: false }
      ]
    }
  },
  {
    code: "BSH002",
    severity: "warning",
    line: 9,
    col: 1,
    message: "this cd does not check whether it worked, and nothing in this script does either",
    // No source at all, deliberately: an engine that carries no excerpt
    // for a finding must cost that finding nothing, and certainly not an
    // empty dark box under it.
    excerpt: NO_SOURCE
  },
  {
    code: "BSH006",
    severity: "style",
    line: 1,
    col: 1,
    message: "this script has no #! interpreter line",
    excerpt: NO_SOURCE
  }
];

/** Every source excerpt block the panel is currently drawing. The count
 *  is the assertion that matters: a panel that rendered an empty box per
 *  excerpt-less finding would draw four of these for the two findings
 *  that have one. */
function sourceBlocks(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('[data-tip="workflow.source-excerpt"]'));
}

/** getByText with NO whitespace normalization. The default matcher trims
 *  and collapses, which on this surface throws away the thing being
 *  asserted: a source line's own indentation, and the padding that puts a
 *  caret under a particular column. With the default normalizer, a caret
 *  drawn at column 1 passes an assertion about column 24. */
const VERBATIM = { normalizer: (value: string) => value };

/** One discovered script carrying the verification result a case is
 *  about. Every other field is the shape a remote hook's row has, so the
 *  case is about the lint and nothing else. */
function scriptWith(scriptName: string, lint: WorkflowScriptLint): WorkflowValidatedScript {
  return {
    stepId: "step_" + scriptName,
    scriptName,
    phase: "before",
    scope: "set",
    order: 1,
    target: "remote",
    executionConnectionRef: "postgres-exec",
    sha256: "7c9f1a77b4e05d2286aa4f1c9de0b7318c5ad4419e6f0b2c7d8e91a0f3b6c245",
    sizeBytes: 1_024,
    timeoutMs: 300_000,
    lint
  };
}

/**
 * The card with the check already run, over a report whose SCRIPTS are
 * the case's and whose every other field is the mock's.
 *
 * The engine's own FINDINGS list is emptied, and that is the point: it
 * carries one sentence per script that is not clean, so a case asserting
 * "the panel says X" against the full report would pass on the findings
 * list having said it. What each check contributes to that list is
 * asserted separately, against the fixture that produces it.
 */
async function checkedWith(scripts: WorkflowValidatedScript[]) {
  const api = createMockApi();
  const report = await createMockApi().getBackupSetWorkflowValidation(HELD.source, HELD.set);
  vi.spyOn(api, "getBackupSetWorkflowValidation").mockResolvedValue({
    ...report,
    scripts,
    findings: []
  });

  const user = userEvent.setup();
  renderCard(api, HELD);
  await screen.findByText("Discovered scripts");
  await user.click(screen.getByRole("button", { name: /Check this set's hooks/ }));
  await screen.findByText(scripts[0].scriptName);
  return { api, user };
}

/** Every BSH code the panel is currently showing, in the order it shows
 *  them. getAllByText answers in document order, which is what makes this
 *  an assertion about GROUPING rather than about membership. */
function panelCodes(): string[] {
  return screen.queryAllByText(/^BSH\d{3}$/).map((node) => node.textContent ?? "");
}

describe("the shell verification's findings panel", () => {
  it("lists every code, position, message and severity, with the error group first", async () => {
    const { user } = await checkedWith([scriptWith("before/10-prune.remote.sh", {
      examined: true,
      parsed: true,
      parseErrorExcerpt: NO_SOURCE,
      findings: RICH_FINDINGS
    })]);

    // Nothing is expanded until the badge is used: the panel is the
    // badge's disclosure and not a second copy of the table.
    expect(panelCodes()).toEqual([]);
    await user.click(screen.getByRole("button", { name: "1 error, 3 more" }));

    // Grouped error, warning, info, style — not the order the engine
    // reported them in, which is the order they are declared above.
    expect(panelCodes()).toEqual(["BSH003", "BSH002", "BSH001", "BSH006"]);
    for (const finding of RICH_FINDINGS) {
      expect(screen.getByText(finding.line + ":" + finding.col)).toBeTruthy();
      expect(screen.getByText(finding.message)).toBeTruthy();
    }
    for (const label of ["error", "warning", "info", "style"]) {
      expect(screen.getAllByText(label).length).toBeGreaterThan(0);
    }
  });

  it("draws a finding's own source line, its line number and a caret at the reported column", async () => {
    const { user } = await checkedWith([scriptWith("before/10-prune.remote.sh", {
      examined: true,
      parsed: true,
      parseErrorExcerpt: NO_SOURCE,
      findings: [RICH_FINDINGS[1]]
    })]);

    await user.click(screen.getByRole("button", { name: "1 error" }));

    const source = sourceBlocks();
    expect(source.length).toBe(1);
    const block = within(source[0]);
    // The reported line and its neighbours, with the gutter numbers an
    // operator counts against their own copy of the file.
    expect(block.getByText("rm -rf \"$STAGING/var\"")).toBeTruthy();
    expect(block.getByText("# clear anything the last run left behind")).toBeTruthy();
    expect(block.getByText("12")).toBeTruthy();
    // The column, MARKED, and marked in the right place: the caret is
    // padded to the reported column, so asserting the padding verbatim is
    // what pins it under the 8th character rather than merely somewhere
    // on the row. `VERBATIM` is why — the default matcher trims, and a
    // trimmed caret row would pass with the caret at column 1.
    //
    // It says "column 8" in words as well as with the mark, because a row
    // of spaces and a caret is nothing to a screen reader.
    expect(block.getByText(" ".repeat(7) + "^ column 8", VERBATIM)).toBeTruthy();
  });

  it("marks a truncated excerpt line rather than cutting it silently", async () => {
    const { user } = await checkedWith([scriptWith("before/10-prune.remote.sh", {
      examined: true,
      parsed: true,
      parseErrorExcerpt: NO_SOURCE,
      findings: [RICH_FINDINGS[0]]
    })]);

    await user.click(screen.getByRole("button", { name: "1 note" }));

    const block = within(sourceBlocks()[0]);
    expect(block.getByText(/keep this in step with the retention job/)).toBeTruthy();
    // A line that merely stopped would look like a line that ended, and
    // an operator comparing it against their file would be comparing
    // against a line the file does not contain.
    expect(block.getByText(/is cut here/)).toBeTruthy();
  });

  it("draws no source block at all for a finding the service carried no excerpt for", async () => {
    const { user } = await checkedWith([scriptWith("before/10-prune.remote.sh", {
      examined: true,
      parsed: true,
      parseErrorExcerpt: NO_SOURCE,
      findings: RICH_FINDINGS
    })]);

    await user.click(screen.getByRole("button", { name: "1 error, 3 more" }));

    // Four findings, two of which carry source. The other two get
    // nothing: an empty dark box is this panel drawing a rectangle to
    // say "no source here", which is worse than saying nothing.
    expect(panelCodes().length).toBe(4);
    expect(sourceBlocks().length).toBe(2);
  });

  it("reports a script that does not parse with its position, and never as clean", async () => {
    const { user } = await checkedWith([scriptWith("before/10-broken.remote.sh", {
      examined: true,
      parsed: false,
      parseError: "unexpected EOF while looking for matching `\"'",
      parseErrorLine: 18,
      parseErrorCol: 24,
      // Column 24 of line 18 is the quote nothing closes.
      parseErrorExcerpt: {
        lines: [
          { number: 17, text: "  # move the rendered config into place", truncated: false },
          { number: 18, text: "  mv \"$STAGE/auth.yml\" \"$OUT_DIR", truncated: false },
          { number: 19, text: "fi", truncated: false }
        ]
      },
      findings: []
    })]);

    await user.click(screen.getByRole("button", { name: "does not parse" }));

    expect(screen.getByText("18:24")).toBeTruthy();
    expect(screen.getByText(/unexpected EOF while looking for matching/)).toBeTruthy();
    // The consequence, not just the position: a file that is not a shell
    // program does not partly run.
    expect(screen.getByText(/nothing in it would run/)).toBeTruthy();
    // The negative that matters. An empty findings list here means the
    // rules never ran, and must not read as a pass anywhere on the card.
    expect(screen.queryAllByText("clean")).toEqual([]);
    // The parser's own line, which is the one excerpt an operator cannot
    // get any other way from this product: a file that does not parse
    // carries no findings, because no rule ran on a tree that does not
    // exist.
    const block = within(sourceBlocks()[0]);
    expect(block.getByText("  mv \"$STAGE/auth.yml\" \"$OUT_DIR", VERBATIM)).toBeTruthy();
    expect(block.getByText("18")).toBeTruthy();
    expect(block.getByText(" ".repeat(23) + "^ column 24", VERBATIM)).toBeTruthy();
  });

  it("reports a NOT EXAMINED script with its reason, and never as clean", async () => {
    const { user } = await checkedWith([scriptWith("before/10-enormous.remote.sh", {
      examined: false,
      notExaminedReason: "this script is 4.1 MB, larger than the 1 MB the shell verification reads",
      parsed: false,
      parseErrorExcerpt: NO_SOURCE,
      findings: []
    })]);

    expect(screen.getByRole("button", { name: "not examined" })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "not examined" }));

    expect(screen.getByText(/larger than the 1 MB the shell verification reads/)).toBeTruthy();
    // The honesty gate, stated on screen as well as implied by the badge.
    expect(screen.getByText(/This is not a pass/)).toBeTruthy();
    expect(screen.queryAllByText("clean")).toEqual([]);
    expect(panelCodes()).toEqual([]);
  });

  it("reports a clean script as clean, with no findings rows", async () => {
    const { user } = await checkedWith([scriptWith("before/10-fine.remote.sh", {
      examined: true,
      parsed: true,
      parseErrorExcerpt: NO_SOURCE,
      findings: []
    })]);

    await user.click(screen.getByRole("button", { name: "clean" }));

    expect(
      screen.getByText(/This script parses, and retnd's own shell rules reported nothing about it/)
    ).toBeTruthy();
    expect(panelCodes()).toEqual([]);
  });

  it("states the worst severity and counts the rest, and opens only that script's findings", async () => {
    const { user } = await checkedWith([
      scriptWith("before/10-mixed.remote.sh", {
        examined: true,
        parsed: true,
        parseErrorExcerpt: NO_SOURCE,
        findings: [
          ...RICH_FINDINGS,
          {
            code: "BSH005",
            severity: "warning",
            line: 21,
            col: 6,
            message: "unquoted operand in [ ... ]",
            excerpt: NO_SOURCE
          }
        ]
      }),
      scriptWith("before/20-other.remote.sh", {
        examined: true,
        parsed: true,
        parseErrorExcerpt: NO_SOURCE,
        findings: [
          {
            code: "BSH004",
            severity: "warning",
            line: 6,
            col: 1,
            message: "set -e with a pipeline and no pipefail",
            excerpt: NO_SOURCE
          }
        ]
      })
    ]);

    // One error beside four other findings is an ERROR badge that also
    // counts the four. "5 findings" would make the error and the missing
    // shebang look like the same fact; "1 error" on its own would not say
    // there were four more things to read.
    expect(screen.getByRole("button", { name: "1 error, 4 more" })).toBeTruthy();
    // And no tail where the worst severity accounts for everything.
    expect(screen.getByRole("button", { name: "1 warning" })).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "1 error, 4 more" }));

    expect(panelCodes()).toEqual(["BSH003", "BSH002", "BSH005", "BSH001", "BSH006"]);
    // And not the other script's, which has its own badge and its own
    // panel: a shared panel would show findings from a file the operator
    // did not select.
    expect(panelCodes()).not.toContain("BSH004");
  });
});

describe("a workflow save the shell rules refused", () => {
  it("names every blocking script, its directory, each code and position, and says nothing was saved", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    vi.spyOn(api, "patchBackupSetWorkflow").mockRejectedValue(
      new RetndError({
        code: "WORKFLOW_SCRIPT_REJECTED",
        message:
          "this configuration was not saved: 2 hook scripts it points at would not run",
        correlationId: "cid_wfscript409",
        status: 409,
        blockingScripts: [
          {
            scriptName: "10-quiesce.remote.sh",
            dir: "/srv/hooks/billing-mysql/before",
            scope: "set",
            phase: "before",
            parseError: "unexpected EOF while looking for matching `\"'",
            parseErrorLine: 18,
            parseErrorCol: 24,
            parseErrorExcerpt: {
              lines: [
                { number: 17, text: "  # quiesce before anything touches the volume", truncated: false },
                { number: 18, text: "  mysql -e \"FLUSH TABLES WITH READ LOCK", truncated: false },
                { number: 19, text: "fi", truncated: false }
              ]
            },
            findings: []
          },
          {
            scriptName: "20-prune-cache.local.sh",
            dir: "/srv/hooks/billing-mysql/after",
            scope: "set",
            phase: "after",
            // A refusal that names the set it is about. The gate reached
            // this script through a set's own stage, and a deployment-wide
            // write can do that for a set nobody was editing.
            backupSetId: "production/billing-mysql",
            parseErrorExcerpt: NO_SOURCE,
            findings: [
              {
                code: "BSH003",
                severity: "error",
                line: 12,
                col: 8,
                message: "this recursive, forced delete targets /var whenever the expansion in it is empty",
                excerpt: {
                  lines: [
                    { number: 11, text: "# clear anything the last run left behind", truncated: false },
                    { number: 12, text: "rm -rf \"$STAGING/var\"", truncated: false },
                    { number: 13, text: "mkdir -p \"$STAGING/var\"", truncated: false }
                  ]
                }
              }
            ]
          }
        ]
      })
    );

    renderCard(api, SFTP_ONLY);
    const picker = await screen.findByLabelText("Execute remote hooks over");
    await user.selectOptions(picker, "billing-exec");
    // The sentence an operator reads first: the write did not happen, and
    // the warnings they can also see on this card are not why.
    const notice = await screen.findByText(/This configuration was NOT saved/);
    const banner = notice.closest(".banner");
    if (banner === null) throw new Error("the refusal is not drawn in a banner");
    expect(within(banner as HTMLElement).getByText(/does not block a save/)).toBeTruthy();

    // Both scripts, both directories, IN THE BANNER: the set's own
    // before directory is already a cell on this card, so an unscoped
    // match would pass on the configuration rather than on the refusal.
    const refusal = within(banner as HTMLElement);
    expect(refusal.getByText("10-quiesce.remote.sh")).toBeTruthy();
    expect(refusal.getByText("20-prune-cache.local.sh")).toBeTruthy();
    expect(refusal.getByText("/srv/hooks/billing-mysql/before")).toBeTruthy();
    expect(refusal.getByText("/srv/hooks/billing-mysql/after")).toBeTruthy();

    // The parse error with its position, and the blocking finding with
    // its code and position — read off the structured field rather than
    // out of the service's sentence.
    expect(refusal.getByText(/does not parse at 18:24/)).toBeTruthy();
    expect(refusal.getByText("BSH003 at 12:8")).toBeTruthy();
    expect(refusal.getByText(/this recursive, forced delete targets \/var/)).toBeTruthy();

    // The line each refusal is about, in the banner, so the operator does
    // not have to reach the machine the hook lives on to read it.
    expect(refusal.getByText("  mysql -e \"FLUSH TABLES WITH READ LOCK", VERBATIM)).toBeTruthy();
    expect(refusal.getByText(" ".repeat(23) + "^ column 24", VERBATIM)).toBeTruthy();
    expect(refusal.getByText("rm -rf \"$STAGING/var\"", VERBATIM)).toBeTruthy();
    expect(refusal.getByText(" ".repeat(7) + "^ column 8", VERBATIM)).toBeTruthy();

    // And WHOSE stage the second one is. "20-prune-cache.local.sh in
    // after" does not say which set's after directory to go and look in,
    // and a deployment-wide write re-resolves every set's stages.
    expect(refusal.getByText("backup set production/billing-mysql")).toBeTruthy();
    expect(refusal.getByText(/a refusal can be about a set you were not editing/)).toBeTruthy();
  });

  it("falls back to the service's own sentence when the refusal carried no structured list", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    // An engine that refuses with the code and the prose, and predates
    // the structured field. The banner has to stay readable: a refusal
    // rendered as an empty list is a save that silently did not happen.
    vi.spyOn(api, "patchBackupSetWorkflow").mockRejectedValue(
      new RetndError({
        code: "WORKFLOW_SCRIPT_REJECTED",
        message: "10-quiesce.remote.sh does not parse: unexpected EOF at 18:24",
        correlationId: "cid_wfscript409old",
        status: 409
      })
    );

    renderCard(api, SFTP_ONLY);
    const picker = await screen.findByLabelText("Execute remote hooks over");
    await user.selectOptions(picker, "billing-exec");

    expect(await screen.findByText(/This configuration was NOT saved/)).toBeTruthy();
    expect(screen.getByText(/10-quiesce.remote.sh does not parse: unexpected EOF at 18:24/)).toBeTruthy();
  });
});

/**
 * The masking rule, on the surface #906 adds.
 *
 * A finding carries the script's OWN text — a position, and now the lines
 * around it — and that is all this card may draw. The risk the new panel
 * introduces is not that it leaks a secret it was given: an excerpt is
 * the script's own bytes, so an unexpanded `$(cat …)` in a hook is text
 * somebody typed and not a value anything resolved. The risk is that a
 * card showing what is inside a hook grows a habit of resolving things —
 * an env reference, a script body. There is no endpoint that returns
 * either, and this pins that the card asks for neither, and that what it
 * WAS given reaches the screen as text rather than as markup.
 */
describe("what the findings panel is allowed to show", () => {
  it("renders a finding's own text verbatim and resolves nothing it was not given", async () => {
    const secretish = "PGPASSWORD=$(cat /etc/backupd/secrets/pg)";
    const { user } = await checkedWith([scriptWith("before/10-export.remote.sh", {
      examined: true,
      parsed: true,
      parseErrorExcerpt: NO_SOURCE,
      findings: [
        {
          code: "BSH001",
          severity: "info",
          line: 7,
          col: 3,
          message: "this variable expansion is not quoted: " + secretish,
          excerpt: {
            lines: [
              { number: 6, text: "# export the dump", truncated: false },
              { number: 7, text: "  " + secretish + " pg_dump <b>db</b>", truncated: false },
              { number: 8, text: "", truncated: false }
            ]
          }
        }
      ]
    })]);

    await user.click(screen.getByRole("button", { name: "1 note" }));

    // The finding's text, exactly as handed over: not expanded, not
    // executed, not interpreted as markup.
    expect(screen.getByText(/this variable expansion is not quoted: PGPASSWORD=\$\(cat/)).toBeTruthy();

    // And the excerpt likewise: the script's own characters, including
    // the ones that look like markup and the ones that look like a
    // command substitution. Nothing here was expanded, run, or parsed as
    // HTML — getByText matches a text node, so a <b> that had been
    // rendered as an element would not be found as part of this string.
    expect(screen.getByText("  " + secretish + " pg_dump <b>db</b>", VERBATIM)).toBeTruthy();

    // And the environment beside it still states a secret as a LOCATION.
    // The value is something no read on this API carries, so there is
    // nothing for the card to show and no control that offers to.
    expect(await screen.findByText(/from file \/etc\/backupd\/secrets\/pg/)).toBeTruthy();
    expect(screen.getAllByText(/never shown/).length).toBeGreaterThan(0);
    expect(screen.queryByRole("button", { name: /reveal|show value/i })).toBeNull();
  });
});
