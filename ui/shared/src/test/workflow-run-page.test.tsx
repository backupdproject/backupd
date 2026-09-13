/**
 * The workflow run screen (issue #814, screens 1 and 2).
 *
 * Every case here is one of the states this page exists to make readable,
 * and each is a state a simpler page would get wrong in a specific way:
 *
 *   - a SUCCEEDED backup beside a FAILED workflow. A page with one badge
 *     cannot say it, and a page that derived one verdict from the others
 *     would say the opposite. It is the whole reason L6's journal has
 *     three columns.
 *   - a run whose hooks were bypassed. Its workflow verdict says nothing
 *     about anybody's scripts, so "Success" there would be a lie and
 *     "Skipped" with no explanation reads as the engine declining.
 *   - a step signalled on its timeout whose exit was never confirmed. The
 *     script may still be running on a machine this product cannot
 *     reach, which is a sentence a table cell cannot carry.
 *   - all five stages, including one the configuration gives no
 *     directory: "did not run" and "not configured" must not look the
 *     same.
 *   - one terminal, belonging to the selection.
 *   - "Local Host", via the Host Workflow Runner, and never a claim that
 *     the engine container grew a shell.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { BackupdError } from "@shared/api/contracts";
import type { BackupdApi, WorkflowRun } from "@shared/api/contracts";
import { elapsedLabel } from "@shared/components/workflowPresentation";
import { WorkflowRunPage } from "@shared/pages/WorkflowRunPage";
import { resetGraphForTests } from "@shared/state/graph";
import { workflowRunPath } from "@shared/utilities/routes";

/** The failing fixture: backup success, workflow failed, cleanup success,
 *  one unconfirmed termination. */
const FAILED_RUN = "wfr_2f91a4";
/** The bypassed one. */
const BYPASSED_RUN = "wfr_5c40b2";
/** The live one: canceled, with its after hooks still running. */
const LIVE_RUN = "wfr_a91f07";
/** A before-stage failure: the backup is SKIPPED, never failed. */
const BEFORE_FAILED_RUN = "wfr_7b03d9";
/** A failed backup whose hooks all did their job. */
const BACKUP_FAILED_RUN = "wfr_c17e88";
/** A hold settled by resuming the cleanup, and one settled by taking
 *  responsibility for it. */
const RESUMED_RUN = "wfr_9a1c40";
const ACKNOWLEDGED_RUN = "wfr_44b2e1";

/** A running step, for the elapsed-time rule. Spelled here rather than
 *  read off a fixture because what is being checked is the rule's edges,
 *  which no fixture holds. */
const RUNNING_STEP = {
  stepId: "step_x",
  scriptName: "10-thaw-db.sh",
  phase: "after" as const,
  scope: "set" as const,
  order: 1,
  target: "local" as const,
  state: "running" as const,
  exitCode: null,
  timeoutMs: 120_000
};

function renderRun(api: BackupdApi, runId: string) {
  return render(
    <MemoryRouter initialEntries={[workflowRunPath(runId)]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/workflow-runs/:runId" element={<WorkflowRunPage readOnly={false} />} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

/** One stage's block, found by the stage label on its head, so a claim
 *  about a stage is scoped to that stage rather than to the page. */
function stageSection(label: string): HTMLElement {
  const block = stageHead(label).parentElement;
  if (!block) throw new Error("no stage block for " + label);
  return block;
}

/** Just the stage's head row, which is where its own verdict badge is.
 *  Scoped this tightly on purpose: a stage whose steps are pending also
 *  has pending STEP badges, and a claim about the stage must not be
 *  satisfiable by one of those. */
function stageHead(label: string): HTMLElement {
  const head = screen
    .getAllByText(label)
    .find((node) => node.classList.contains("eyebrow"));
  if (!head?.parentElement) throw new Error("no stage head for " + label);
  return head.parentElement;
}

/** One verdict cell, by the group name the component gives it, so the
 *  three are told apart by name rather than by position. */
function verdict(label: string): HTMLElement {
  return screen.getByRole("group", { name: label + " status" });
}

describe("the three verdicts stay three", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("reports a successful backup beside a failed workflow, and names the cause", async () => {
    renderRun(createMockApi(), FAILED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    expect(within(verdict("Backup")).getByText("Success")).toBeTruthy();
    expect(within(verdict("Workflow")).getByText("Failed")).toBeTruthy();
    // The cause, in the same cell, because "Failed" alone sends an
    // operator to the step list to run a search this page can do.
    expect(within(verdict("Workflow")).getByText(/40-quiesce-remote\.remote\.sh/)).toBeTruthy();
    // And the pair this whole feature exists to be able to say: the
    // archive landed and the cleanup did not, so a machine may be sitting
    // quiesced with a good backup beside it.
    expect(within(verdict("Cleanup")).getByText("Failed")).toBeTruthy();
    expect(within(verdict("Cleanup")).getByText(/may still be quiesced/)).toBeTruthy();
  });

  it("does not let a failed workflow contaminate the backup verdict, or the reverse", async () => {
    // The negative control for the case above: it would pass equally well
    // if both cells rendered whichever verdict was worst, so this asserts
    // the cells are actually independent by inverting one of them.
    const api = createMockApi();
    const real = await createMockApi().workflowRun(FAILED_RUN);
    const inverted: WorkflowRun = { ...real, backupStatus: "failed", workflowStatus: "success" };
    vi.spyOn(api, "workflowRun").mockResolvedValue(inverted);

    renderRun(api, FAILED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    expect(within(verdict("Backup")).getByText("Failed")).toBeTruthy();
    expect(within(verdict("Workflow")).getByText("Success")).toBeTruthy();
  });

  it("says a bypassed run executed no hook rather than showing a green workflow", async () => {
    renderRun(createMockApi(), BYPASSED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    expect(screen.getByText("Scripts bypassed")).toBeTruthy();
    expect(within(verdict("Workflow")).getByText("Skipped")).toBeTruthy();
    expect(within(verdict("Workflow")).getByText(/Hooks were bypassed for this run/)).toBeTruthy();
    // And the backup still reports its own outcome: bypassing hooks does
    // not make the backup unknown.
    expect(within(verdict("Backup")).getByText("Success")).toBeTruthy();
  });

  it("reports a verdict this build cannot read as not reported, never as success", async () => {
    const api = createMockApi();
    const real = await createMockApi().workflowRun(FAILED_RUN);
    // What an engine newer than this build looks like from here: the
    // client narrows an unrecognised status onto "unknown", and the page
    // must draw that as an absence rather than as a pass.
    vi.spyOn(api, "workflowRun").mockResolvedValue({ ...real, cleanupStatus: "unknown" });

    renderRun(api, FAILED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    expect(within(verdict("Cleanup")).getByText("Not reported")).toBeTruthy();
  });
});

describe("the status combinations the engine can actually produce", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("reports a before-stage failure as a SKIPPED backup, never a failed one", async () => {
    // core/internal/workflowrun/engine.go's skipBackup: nothing was
    // attempted, so the backup is skipped. A page rendering "failed"
    // there would be reporting a backup that was never tried as one that
    // broke — and the set scope was still entered, so its after hooks ran
    // and the cleanup verdict is a success on a run whose workflow
    // failed.
    renderRun(createMockApi(), BEFORE_FAILED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    expect(within(verdict("Backup")).getByText("Skipped")).toBeTruthy();
    expect(within(verdict("Workflow")).getByText("Failed")).toBeTruthy();
    expect(within(verdict("Cleanup")).getByText("Success")).toBeTruthy();
  });

  it("reports a failed backup whose hooks all did their job", async () => {
    // The flagship inverted, and the reason both fixtures exist: a
    // surface that derived one verdict from another cannot draw both.
    renderRun(createMockApi(), BACKUP_FAILED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    expect(within(verdict("Backup")).getByText("Failed")).toBeTruthy();
    expect(within(verdict("Cleanup")).getByText("Success")).toBeTruthy();
  });

  it("tells a hold settled by resuming the cleanup from one settled by acknowledgement", async () => {
    // The two are the same run state and differ in the CLEANUP verdict,
    // which is the whole record of which way it was settled: a resume
    // runs the owed hooks, so cleanup succeeds; an acknowledgement
    // executes nothing and leaves it failed.
    renderRun(createMockApi(), RESUMED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });
    expect(screen.getAllByText("Recovered").length).toBeGreaterThan(0);
    expect(within(verdict("Cleanup")).getByText("Success")).toBeTruthy();

    cleanup();

    renderRun(createMockApi(), ACKNOWLEDGED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });
    expect(screen.getAllByText("Recovered").length).toBeGreaterThan(0);
    expect(within(verdict("Cleanup")).getByText("Failed")).toBeTruthy();
  });

  it("names every script the way the engine requires one to be named", async () => {
    // core/internal/workflow/script.go refuses a plain *.sh outright, so
    // a row named without a target is a row describing a run that could
    // not have been planned. Asserted over every row rather than one,
    // because what is being checked is true of all of them.
    renderRun(createMockApi(), FAILED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    const names = screen
      .getAllByRole("button")
      .map((node) => node.textContent ?? "")
      .flatMap((text) => text.match(/[\w.-]+\.sh/g) ?? []);
    expect(names.length).toBeGreaterThan(3);
    for (const name of names) {
      expect(name, name).toMatch(/\.(local|remote)\.sh$/);
    }
  });

  it("keeps a live run polling its steps, and keeps the page when a poll fails", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const api = createMockApi();
    const steps = vi.spyOn(api, "workflowSteps");

    renderRun(api, LIVE_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });
    const first = steps.mock.calls.length;

    // The poll's own callback has to be stable for this to advance at
    // all: an identity that changes each render re-arms the interval
    // every render and it never elapses.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4_500);
    });
    expect(steps.mock.calls.length).toBeGreaterThan(first);

    vi.useRealTimers();
  });

  it("keeps the run on screen when a later read fails, and says the read failed", async () => {
    const api = createMockApi();
    renderRun(api, FAILED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    vi.spyOn(api, "workflowRun").mockRejectedValue(
      new BackupdError({
        code: "WORKFLOW_ENGINE_UNAVAILABLE",
        message: "the workflow engine is not answering",
        correlationId: "cid_poll503"
      })
    );
    await userEvent.click(screen.getByRole("button", { name: "Re-read" }));

    await waitFor(() => expect(screen.getByText(/This run could not be re-read/)).toBeTruthy());
    // The page an operator was reading is still there.
    expect(within(verdict("Backup")).getByText("Success")).toBeTruthy();
  });
});

describe("a step whose termination was never confirmed", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("says so at the page level, and names the script that may still be running", async () => {
    renderRun(createMockApi(), FAILED_RUN);

    const banner = await screen.findByText(
      /One step was signalled and this product cannot confirm it stopped/
    );
    expect(banner).toBeTruthy();
    // The script is named in the warning's own body, not merely somewhere
    // on the page: it also appears as a step row, and an operator reading
    // the warning needs to know which script it is about without hunting.
    const body = screen.getByText(/may still be running on the machine it was sent to/);
    expect(body.textContent).toMatch(/40-quiesce-remote\.remote\.sh/);
    expect(body.textContent).toMatch(/Nothing here can end it/);
  });

  it("says nothing of the kind for a run whose steps all confirmed", async () => {
    // The positive control. Without it, the assertion above would pass
    // against a page that showed the banner unconditionally.
    const api = createMockApi();
    const real = await createMockApi().workflowRun(FAILED_RUN);
    vi.spyOn(api, "workflowRun").mockResolvedValue({
      ...real,
      steps: real.steps.map((step) => ({ ...step, terminationConfirmed: true }))
    });
    vi.spyOn(api, "workflowSteps").mockResolvedValue(
      real.steps.map((step) => ({ ...step, terminationConfirmed: true }))
    );

    renderRun(api, FAILED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    expect(screen.queryByText(/cannot confirm it stopped/)).toBeNull();
  });
});

describe("the stage ladder", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("draws all five stages, with the backup between the before and after halves", async () => {
    renderRun(createMockApi(), FAILED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    for (const label of [
      "Global before",
      "Backup set before",
      "Backup",
      "Backup set after",
      "Global after"
    ]) {
      expect(screen.getAllByText(label).length).toBeGreaterThan(0);
    }
  });

  it("tells a stage with no directory apart from one that was skipped", async () => {
    const api = createMockApi();
    // A deployment that configures no global AFTER directory, with a run
    // that planned no step there either: the stage is ineligible, and the
    // page has to say that rather than drawing an empty stage that reads
    // as "these hooks did not run".
    const workflow = await createMockApi().getBackupSetWorkflow("production", "postgres-primary");
    vi.spyOn(api, "getBackupSetWorkflow").mockResolvedValue({
      ...workflow,
      stages: workflow.stages.filter((stage) => !(stage.scope === "global" && stage.phase === "after"))
    });
    const steps = (await createMockApi().workflowSteps(FAILED_RUN)).filter(
      (step) => !(step.scope === "global" && step.phase === "after")
    );
    vi.spyOn(api, "workflowSteps").mockResolvedValue(steps);

    renderRun(api, FAILED_RUN);
    await screen.findByText(/No directory is configured for this stage/);

    expect(screen.getByText(/That is different from a stage that was skipped/)).toBeTruthy();
    // The skipped STEP in an eligible stage still reads as skipped.
    expect(screen.getAllByText("Skipped").length).toBeGreaterThan(0);
  });

  it("marks a live run's current phase, so which stage is executing is legible at a glance", async () => {
    renderRun(createMockApi(), LIVE_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    // The run-level state names the phase, not merely "running".
    expect(screen.getAllByText("Cleanup running").length).toBeGreaterThan(0);
    expect(screen.getByText(/This run is still going: cleanup running/)).toBeTruthy();

    // The STAGE that is executing says so on its own head. Without this
    // the answer to "which of the five is running" is only discoverable
    // by scanning every script row, which is the reading this screen
    // exists to save.
    const running = stageSection("Backup set after");
    expect(within(stageHead("Backup set after")).getByText("Running now")).toBeTruthy();
    // And the stages either side of it carry their own verdicts rather
    // than nothing: "done" and "pending" are what make "running now"
    // mean something.
    expect(within(stageHead("Global before")).getByText("Done")).toBeTruthy();
    expect(within(stageHead("Global after")).getByText("Pending")).toBeTruthy();

    // The step actually executing is the one drawn as running, and the
    // one after it is still pending — the two must not collapse.
    expect(within(running).getByText("Running")).toBeTruthy();
    // The row after it has not started, and says so rather than being
    // folded into the running one.
    expect(within(running).getByText("Pending")).toBeTruthy();
    // A running step has no recorded duration, so the row reports what
    // this browser can measure rather than an em dash beside a spinner.
    expect(within(running).getByText(/so far$/)).toBeTruthy();
    // And it reports nothing at all when the two clocks disagree by more
    // than the bound the step would have been killed at, because that
    // figure is not a measurement of the run.
    expect(elapsedLabel({ ...RUNNING_STEP, startedAt: "2020-01-01T00:00:00Z" })).toBe("\u2014");
    expect(elapsedLabel({ ...RUNNING_STEP, startedAt: "2099-01-01T00:00:00Z" })).toBe("\u2014");
    expect(elapsedLabel({ ...RUNNING_STEP, startedAt: new Date(Date.now() - 5_000).toISOString() })).toBe(
      "5.0s so far"
    );
    // And a canceled pass does not claim a backup happened.
    expect(within(verdict("Backup")).getByText("Skipped")).toBeTruthy();
  });
});

// The run page's job here is the slot: which step is selected, that one
// terminal is open at a time, that it is handed the right run/step/step
// object, and that a stale selection is dropped. The terminal's own
// rendering + streaming is covered by StepLogTerminal.test.tsx, so it is
// stubbed to a prop-recorder keyed by the same accessible region name.
vi.mock("@shared/components/StepLogTerminal", () => ({
  StepLogTerminal: ({ runId, stepId, step }: { runId: string; stepId: string; step: { scriptName: string; state: string; target: string } }) => (
    <section role="region" aria-label={"Output of " + step.scriptName}>
      <div>{"run " + runId + " \u00b7 step " + stepId}</div>
      <div>{step.state}</div>
      <div>{step.target}</div>
    </section>
  )
}));

describe("the step rows and the terminal slot", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("names the Host Workflow Runner for a local step and never the engine container", async () => {
    renderRun(createMockApi(), FAILED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    const local = screen.getByRole("button", { name: /20-freeze-db\.local\.sh/ });
    expect(within(local).getByText("Local Host")).toBeTruthy();
    expect(within(local).getByText(/Host Workflow Runner/)).toBeTruthy();

    // The wording rule for the whole feature, asserted as an absence
    // because that is what it is: nothing on this page may suggest the
    // engine's own container executes a hook.
    expect(document.body.textContent).not.toMatch(/engine container/i);
  });

  it("identifies a remote step by the connection it executed over", async () => {
    renderRun(createMockApi(), FAILED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    const remote = screen.getByRole("button", { name: /10-flush-cache\.remote\.sh/ });
    expect(within(remote).getByText("Remote")).toBeTruthy();
    expect(within(remote).getByText("postgres-exec")).toBeTruthy();
  });

  it("reports no exit code for a step that never produced one, rather than zero", async () => {
    renderRun(createMockApi(), FAILED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    // The skipped step never ran. "exit 0" here would read as a clean
    // success, which is the one reading this cell must never produce.
    const skipped = screen.getByRole("button", { name: /50-release-lock\.remote\.sh/ });
    expect(within(skipped).getByText("exit \u2014")).toBeTruthy();
    // Nor does a step that was signalled and never confirmed its exit.
    const timedOut = screen.getByRole("button", { name: /40-quiesce-remote\.remote\.sh/ });
    expect(within(timedOut).getByText("exit \u2014")).toBeTruthy();
    // While a step that really exited non-zero carries its code.
    const succeeded = screen.getByRole("button", { name: /20-freeze-db\.local\.sh/ });
    expect(within(succeeded).getByText("exit 0")).toBeTruthy();
  });

  it("opens the selected step's terminal, and only one at a time", async () => {
    const user = userEvent.setup();
    renderRun(createMockApi(), FAILED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    // Nothing is open before a selection: a page that mounted a terminal
    // per row would hold one poll per script of a run that can plan
    // dozens.
    expect(screen.getByText("No step is selected")).toBeTruthy();

    await user.click(screen.getByRole("button", { name: /20-freeze-db\.local\.sh/ }));

    await waitFor(() =>
      expect(screen.getByRole("region", { name: "Output of 20-freeze-db.local.sh" })).toBeTruthy()
    );
    expect(screen.getByRole("button", { name: /20-freeze-db\.local\.sh/ }).getAttribute("aria-pressed")).toBe(
      "true"
    );

    await user.click(screen.getByRole("button", { name: /10-flush-cache\.remote\.sh/ }));

    await waitFor(() =>
      expect(screen.getByRole("region", { name: "Output of 10-flush-cache.remote.sh" })).toBeTruthy()
    );
    // The previous one is gone, not merely hidden.
    expect(screen.queryByRole("region", { name: "Output of 20-freeze-db.local.sh" })).toBeNull();
  });

  it("hands the slot the step it is about, with the run and step ids it needs to follow", async () => {
    const user = userEvent.setup();
    renderRun(createMockApi(), FAILED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    await user.click(screen.getByRole("button", { name: /40-quiesce-remote\.remote\.sh/ }));

    const slot = within(await screen.findByRole("region", { name: "Output of 40-quiesce-remote.remote.sh" }));
    expect(slot.getByText(new RegExp("run " + FAILED_RUN + " . step step_quiesce"))).toBeTruthy();
    // The step's own facts, from the prop rather than a second fetch.
    expect(slot.getByText("timed_out")).toBeTruthy();
    expect(slot.getByText("remote")).toBeTruthy();
  });

  it("drops a selection whose step no longer exists rather than following a stale id", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    renderRun(api, FAILED_RUN);
    await screen.findByRole("group", { name: "Workflow run verdicts" });

    await user.click(screen.getByRole("button", { name: /20-freeze-db\.local\.sh/ }));
    await screen.findByRole("region", { name: "Output of 20-freeze-db.local.sh" });

    // A re-planned run mints new step ids. A slot left pointing at the
    // old one would poll a log route that answers
    // WORKFLOW_STEP_NOT_FOUND for as long as the page stayed open.
    const steps = await createMockApi().workflowSteps(FAILED_RUN);
    vi.spyOn(api, "workflowSteps").mockResolvedValue(
      steps.map((step) => ({ ...step, stepId: step.stepId + "_v2" }))
    );
    await user.click(screen.getByRole("button", { name: "Re-read" }));

    await waitFor(() => expect(screen.getByText("No step is selected")).toBeTruthy());
  });
});

describe("a run that cannot be read", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("says what the service said, with its correlation id, and offers a retry", async () => {
    const api = createMockApi();
    vi.spyOn(api, "workflowRun").mockRejectedValue(
      new BackupdError({
        code: "WORKFLOW_RUN_NOT_FOUND",
        message: "this deployment has no workflow run with that id",
        correlationId: "cid_run404"
      })
    );

    renderRun(api, "wfr_nope");

    expect(
      await screen.findByText(/this deployment has no workflow run with that id/)
    ).toBeTruthy();
    expect(screen.getByText(/cid_run404/)).toBeTruthy();
  });
});
