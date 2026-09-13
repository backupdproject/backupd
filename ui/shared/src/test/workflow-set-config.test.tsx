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
import { BackupdError } from "@shared/api/contracts";
import type { BackupdApi } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { BackupSetWorkflowCard } from "@shared/pages/BackupSetWorkflowCard";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import { act } from "@testing-library/react";
import { backupSetPath } from "@shared/utilities/routes";
import type { VersionInfo } from "@shared/types/operation";

/** The set that configures hooks and is not held. */
const HOOKED = { source: "production", set: "postgres-primary" };
/** The set whose cleanup could not be finished, so it is refusing to
 *  run, and whose source connection is SFTP-only. */
const HELD = { source: "production", set: "billing-mysql" };
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

function renderCard(api: BackupdApi, target: { source: string; set: string }) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <BackupSetWorkflowCard source={target.source} set={target.set} readOnly={false} />
      </ApiProvider>
    </MemoryRouter>
  );
}

function renderDetail(api: BackupdApi, target: { source: string; set: string }) {
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
    renderCard(createMockApi(), HOOKED);
    await screen.findByText("Discovered scripts");

    expect(screen.getByText("120s (pinned on this set)")).toBeTruthy();
  });

  it("says a set that pins nothing follows the deployment's value", async () => {
    renderCard(createMockApi(), HELD);
    await screen.findByText("Discovered scripts");

    expect(screen.getByText("300s (inherited)")).toBeTruthy();
  });

  it("offers the deployment's execution connections, and says what an SFTP-only source means", async () => {
    renderCard(createMockApi(), HELD);
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

    renderCard(api, HELD);
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

    renderCard(api, HOOKED);
    await screen.findByText("Discovered scripts");

    expect(validation).not.toHaveBeenCalled();
    expect(screen.getByText(/Nothing has been checked yet in this session/)).toBeTruthy();
  });

  it("reports a discovered script's order, target, size and hash once asked", async () => {
    const user = userEvent.setup();
    renderCard(createMockApi(), HOOKED);
    await screen.findByText("Discovered scripts");

    await user.click(screen.getByRole("button", { name: /Check this set's hooks/ }));

    expect(await screen.findByText("before/20-freeze-db.sh")).toBeTruthy();
    const row = screen.getByText("before/20-freeze-db.sh").closest("tr");
    if (row === null) throw new Error("no row for the local script");
    expect(within(row).getByText("Local Host")).toBeTruthy();
    expect(within(row).getByText("Host Workflow Runner")).toBeTruthy();
    // A seven-digit hash, off the report and not invented.
    expect(within(row).getByText("b70c1d5")).toBeTruthy();
  });

  it("reports an SFTP-only source as an error with its capability checks NOT EXAMINED", async () => {
    const user = userEvent.setup();
    renderCard(createMockApi(), HELD);
    await screen.findByText("Discovered scripts");

    await user.click(screen.getByRole("button", { name: /Check this set's hooks/ }));

    expect(await screen.findByText(/this set's source connection is SFTP-only/)).toBeTruthy();
    // The half that matters: a skipped check is drawn as "not examined"
    // and never as a pass, because nothing looked.
    const skipped = screen.getByText("exec_capability").closest("div");
    if (skipped === null) throw new Error("no finding row for exec_capability");
    expect(within(skipped).getByText("not examined")).toBeTruthy();
    expect(within(skipped).queryByText("ok")).toBeNull();
    // Two verdicts, kept apart: the backups are fine and the hooks are
    // not.
    expect(screen.getByText("Valid for backup")).toBeTruthy();
    expect(screen.getByText("Hooks not valid")).toBeTruthy();
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
    renderDetail(createMockApi(), HOOKED);

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

    await waitFor(() => expect(resume).toHaveBeenCalledWith("wfr_7b03d9"));
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
      expect(acknowledge).toHaveBeenCalledWith("wfr_7b03d9", "thawed the database by hand")
    );
  });

  it("does not offer a Run when the hold list itself could not be read", async () => {
    seedVersion();
    const api = createMockApi();
    vi.spyOn(api, "workflowRecovery").mockRejectedValue(
      new BackupdError({
        code: "WORKFLOW_ENGINE_UNAVAILABLE",
        message: "the workflow engine is not answering",
        correlationId: "cid_holds503"
      })
    );

    renderDetail(api, HOOKED);

    const runControl = await screen.findByRole("button", { name: "Run this backup set" });
    // An unreadable hold list treated as "no holds" would offer a run the
    // engine is about to refuse, so it is refused here and said out loud.
    await waitFor(() => expect(runControl).toBeDisabled());
    expect(
      screen.getByText(/Whether a workflow run is holding this backup set could not be read/)
    ).toBeTruthy();
  });
});
