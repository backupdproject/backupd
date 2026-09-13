/**
 * The deployment-wide workflow settings card (issue #814, screen 5).
 *
 * What is asserted here is mostly what this card must NOT do, because
 * that is where the risk is:
 *
 *   - it must not offer to raise a hook's privileges. There is no
 *     elevation switch in the product, and a control here would be
 *     inventing a capability.
 *   - it must not report the runner's build or its execution user as if
 *     this API carried them. It does not; the CLI does, and this card
 *     says so rather than deriving a username from a file path.
 *   - it must not show a green global-scripts list while the Host
 *     Workflow Runner is down: a local hook's syntax check runs THROUGH
 *     the runner, so with it down nothing looked.
 *   - a cleared stage directory must reach the service as a cleared
 *     field, because that is how an operator disables a stage.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { BackupdError } from "@shared/api/contracts";
import type { BackupdApi } from "@shared/api/contracts";
import { WorkflowSettingsCard } from "@shared/pages/WorkflowSettingsCard";
import { resetGraphForTests } from "@shared/state/graph";

/**
 * The card once its read has landed.
 *
 * Awaited on a FORM FIELD and not on the card's own copy, and that is not
 * incidental: the tooltip registry's entry for this card quotes the same
 * sentence, and a registry pop-up is in the DOM (hidden) from the first
 * paint — so awaiting the sentence would resolve while the card was still
 * loading and every assertion after it would run against the wrong state.
 */
async function loaded() {
  return screen.findByLabelText("Global before directory");
}

/** Every element carrying `text` that is NOT inside a tooltip pop-up, so
 *  a claim about what the card says cannot be satisfied by the registry
 *  copy explaining the card. */
function onCard(text: string | RegExp): HTMLElement[] {
  return screen
    .queryAllByText(text)
    .filter((node) => node.closest(".tooltip__pop") === null);
}

function renderCard(api: BackupdApi) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <WorkflowSettingsCard readOnly={false} />
      </ApiProvider>
    </MemoryRouter>
  );
}

afterEach(() => {
  cleanup();
  resetGraphForTests();
  vi.restoreAllMocks();
});

describe("the deployment-wide workflow card", () => {
  it("states that global scripts wrap every backup-set run", async () => {
    renderCard(createMockApi());
    await loaded();

    expect(onCard("Global scripts wrap every backup-set run.").length).toBeGreaterThan(0);
  });

  it("says a local hook runs on this machine through the Host Workflow Runner", async () => {
    renderCard(createMockApi());
    await loaded();

    expect(onCard(/runs on this machine through the\s+Host Workflow Runner/).length).toBeGreaterThan(0);
    // The wording rule: nothing here may suggest the engine's own
    // container executes a hook.
    expect(document.body.textContent).not.toMatch(/engine container/i);
  });

  it("offers no control that raises a hook's privileges", async () => {
    renderCard(createMockApi());
    await loaded();

    for (const control of screen.getAllByRole("button")) {
      expect(control.textContent ?? "").not.toMatch(/root|elevat|sudo|privile/i);
    }
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(onCard(/There is no control here\s+that raises a hook's privileges/).length).toBe(1);
  });

  it("does not invent the runner's liveness, version or execution user, and names what reports them", async () => {
    renderCard(createMockApi());
    await loaded();

    expect(onCard("Not on this read").length).toBe(1);
    expect(onCard("backupd workflow-runner status").length).toBe(1);
    // And it points at the read that DOES open the socket, rather than
    // leaving the CLI as the only answer.
    expect(onCard(/Check this set/).length).toBeGreaterThan(0);
  });

  /**
   * The semantic this card must not overstate.
   *
   * `runner.configured` is the PRESENCE of both halves of the runner's
   * address — a socket path and a credential file (config.WorkflowRunner's
   * own Configured()) — and nothing on this read contacts the runner. A
   * card that said "Answering" would therefore report a
   * configured-but-dead runner as healthy, which is the one claim about
   * this component that matters and the one it cannot make.
   */
  it("reports the runner address as configured, and never claims it is answering", async () => {
    renderCard(createMockApi());
    await loaded();

    // Two cells legitimately read "Configured": the runner address and
    // the timeout source. What matters is that neither says "Answering".
    expect(onCard("Configured").length).toBe(2);
    expect(onCard("/run/backupd/hooks.sock").length).toBe(1);
    expect(onCard(/Answering/i).length).toBe(0);
    expect(onCard(/is not answering/i).length).toBe(0);
    // And it says outright that nothing here contacted it.
    expect(onCard(/Nothing on this card contacts the runner/).length).toBe(1);
  });

  it("says a MISSING runner address is missing, and what that costs", async () => {
    const api = createMockApi();
    const settings = await createMockApi().getWorkflowSettings();
    vi.spyOn(api, "getWorkflowSettings").mockResolvedValue({
      ...settings,
      // Both halves are required, so this is the shape of a deployment
      // that never told the engine how to reach a runner.
      runner: { configured: false, socket: "", tokenFile: "" }
    });

    renderCard(api);
    await loaded();

    expect(
      onCard("This deployment has not been told how to reach the Host Workflow Runner").length
    ).toBe(1);
    expect(onCard(/a .local.sh hook has nothing to run on/).length).toBe(1);
    expect(onCard("Not configured").length).toBeGreaterThan(0);
  });

  it("tells a configured timeout from the product's own default", async () => {
    const api = createMockApi();
    const settings = await createMockApi().getWorkflowSettings();
    vi.spyOn(api, "getWorkflowSettings").mockResolvedValue({
      ...settings,
      scriptTimeoutConfigured: false
    });

    renderCard(api);
    await loaded();

    expect(onCard("Product default").length).toBe(1);
  });

  it("sends a cleared stage directory as a cleared field, and nothing else", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    const patch = vi.spyOn(api, "patchWorkflowSettings");

    renderCard(api);
    const before = await screen.findByLabelText("Global before directory");

    await user.clear(before);
    // Per-box Save: exactly the field named, so a save here cannot carry
    // the after directory or the timeout an operator never touched.
    const saves = screen.getAllByRole("button", { name: "Save" });
    await waitFor(() => expect(saves[0]).toBeEnabled());
    await user.click(saves[0]);

    await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
    expect(patch.mock.calls[0][0]).toEqual({ beforeDir: "" });
  });

  it("keeps Save unavailable until a box actually differs from what is persisted", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    const patch = vi.spyOn(api, "patchWorkflowSettings");

    renderCard(api);
    const timeout = await screen.findByLabelText("Default script timeout (seconds)");
    const saves = screen.getAllByRole("button", { name: "Save" });

    expect(saves[2]).toBeDisabled();
    await user.type(timeout, "0");
    await waitFor(() => expect(saves[2]).toBeEnabled());
    await user.click(saves[2]);

    await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
    expect(patch.mock.calls[0][0]).toEqual({ scriptTimeoutSeconds: 3000 });
  });

  it("says the stage directories are what a cleared box disables", async () => {
    renderCard(createMockApi());
    await loaded();

    expect(onCard(/no global before hooks run for any backup set/).length).toBe(1);
    expect(onCard(/no global after hooks run for any backup set/).length).toBe(1);
  });

  it("reports a failed read with the service's words rather than an empty form", async () => {
    const api = createMockApi();
    vi.spyOn(api, "getWorkflowSettings").mockRejectedValue(
      new BackupdError({
        code: "WORKFLOW_ENGINE_UNAVAILABLE",
        message: "the workflow engine is not answering",
        correlationId: "cid_wfs503"
      })
    );

    renderCard(api);

    expect(await screen.findByText(/the workflow engine is not answering/)).toBeTruthy();
    expect(screen.getByText(/cid_wfs503/)).toBeTruthy();
  });

  /**
   * The save gate, on the card whose stage directories wrap EVERY backup
   * set (#906).
   *
   * Driven through the mock's own refusal rather than a stubbed rejection,
   * because the parity this feature is about is that the CLI and the web
   * UI report the same refusal: a mock that could never produce one is a
   * panel nobody can develop or demonstrate against.
   */
  it("names every blocking script and says the configuration was not saved", async () => {
    const user = userEvent.setup();
    const api = createMockApi();

    renderCard(api);
    const before = await screen.findByLabelText("Global before directory");
    await user.clear(before);
    // createMockApi's documented known-broken stage directory: the one
    // path its workflow patches refuse.
    await user.type(before, "/srv/hooks/known-broken");

    const saves = screen.getAllByRole("button", { name: "Save" });
    await waitFor(() => expect(saves[0]).toBeEnabled());
    await user.click(saves[0]);

    const notice = await screen.findByText(/This configuration was NOT saved/);
    const banner = notice.closest(".banner");
    if (banner === null) throw new Error("the refusal is not drawn in a banner");
    const refusal = within(banner as HTMLElement);

    // What an operator has to go and edit: the file, the directory it is
    // in, and where in it the rule fired.
    expect(refusal.getByText("10-quiesce.remote.sh")).toBeTruthy();
    expect(refusal.getByText("20-prune-cache.local.sh")).toBeTruthy();
    expect(refusal.getAllByText("/srv/hooks/known-broken").length).toBe(2);
    expect(refusal.getByText(/does not parse at 18:24/)).toBeTruthy();
    expect(refusal.getByText("BSH003 at 12:8")).toBeTruthy();
    // The line each refusal is about. A global stage directory wraps
    // every backup set, so the operator refused here is often not the
    // person who wrote the hook: a position they have to go and resolve
    // on somebody else's host is a dead end.
    expect(refusal.getByText("mysql -e \"FLUSH TABLES WITH READ LOCK")).toBeTruthy();
    expect(refusal.getByText("^ column 24")).toBeTruthy();
    expect(refusal.getByText("rm -rf \"$STAGING/var\"")).toBeTruthy();
    expect(refusal.getByText("^ column 8")).toBeTruthy();
    // And whose stage the second script came out of. This card writes
    // DEPLOYMENT-WIDE directories, so a refusal naming a set is a refusal
    // about a set nobody was editing on this screen.
    expect(refusal.getByText("backup set production/billing-mysql")).toBeTruthy();
    // And the half a refusal must never overstate: warnings save fine.
    expect(refusal.getByText(/does not block a save/)).toBeTruthy();
  });

  it("leaves the persisted value alone when the write was refused", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    const before = (await createMockApi().getWorkflowSettings()).beforeDir;

    renderCard(api);
    const box = await screen.findByLabelText("Global before directory");
    await user.clear(box);
    await user.type(box, "/srv/hooks/known-broken");
    const saves = screen.getAllByRole("button", { name: "Save" });
    await waitFor(() => expect(saves[0]).toBeEnabled());
    await user.click(saves[0]);
    await screen.findByText(/This configuration was NOT saved/);

    // The read the card does after a successful save is the one thing
    // that must not happen here: a refused write changed nothing, so the
    // service still holds the old directory.
    expect((await api.getWorkflowSettings()).beforeDir).toBe(before);
  });
});
