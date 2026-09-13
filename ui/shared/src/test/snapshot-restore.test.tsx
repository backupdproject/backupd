/**
 * The restore flow (EPIC K, issue #788).
 *
 * The parameter that matters most here is the one nobody looks at: the
 * conflict policy. It defaults to "refuse" because that is the only one
 * of the three that cannot lose data, and the failure mode is a flow that
 * quietly sends nothing (letting a server default decide) or sends the
 * last thing a control happened to hold. It is asserted on the request,
 * not on the select, for exactly that reason.
 *
 * The other one is the snapshot: a restore names the engine's MANIFEST
 * id, because the repository and not the catalog decides whether the id
 * names anything, and that is precisely the situation somebody is
 * restoring in.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupdApi } from "@shared/api/contracts";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import { SnapshotRestorePage } from "@shared/pages/SnapshotRestorePage";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { setsNode, versionNode } from "@shared/state/appNodes";
import type { VersionInfo } from "@shared/types/operation";
import { restorePath } from "@shared/utilities/routes";

const VERSION: VersionInfo = {
  api: "v1", service: "1.3.0", buildCommit: "9f4c1ab", goVersion: "go1.27.0",
  engine: "1.68.2", configRevision: "cfg_9f4c1ab", ready: true, compatible: true
};

async function seed(api: BackupdApi) {
  const sets = await api.listSets();
  act(() => {
    graph.commit("test/seed", (tx) => {
      tx.set(versionNode, { data: VERSION, error: null, loading: false });
      tx.set(setsNode, { data: sets, error: null, loading: false });
    });
  });
}

function renderRestore(api: BackupdApi, runId?: string) {
  return render(
    <MemoryRouter initialEntries={[restorePath("production", "postgres-primary", runId)]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/sets/:source/:set/restore" element={<SnapshotRestorePage readOnly={false} />} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

function press(name: string | RegExp) {
  act(() => {
    screen.getByRole("button", { name }).click();
  });
}

describe("the restore flow", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    resetMockFixtures();
    vi.restoreAllMocks();
  });

  it("submits the chosen snapshot, the destination and refuse-by-default as one durable operation", async () => {
    const api = createMockApi();
    const restore = vi.spyOn(api, "restoreSnapshot");
    await seed(api);

    renderRestore(api, "run-2026-09-13-0400");
    // The rail renders immediately; the snapshot list does not, and a
    // flow with no snapshot chosen correctly refuses to submit. Waiting
    // for a choice is what keeps the cases below about the request.
    await screen.findByText(/newest known-good/);

    press("Where");
    fireEvent.change(screen.getByLabelText("Restore into"), {
      target: { value: "/data/restores/postgres-2026-09-13" }
    });
    press("Confirm");
    press("Start restore");

    await waitFor(() => expect(restore).toHaveBeenCalledTimes(1));
    const sent = restore.mock.calls[0][0];
    expect(sent.backupSetId).toBe("production/postgres-primary");
    // The manifest id, not the run id: the repository decides whether
    // this names anything, and it is what the contract takes.
    expect(sent.snapshotId).toBe("k7f3a91c22e8b40d");
    expect(sent.targetPath).toBe("/data/restores/postgres-2026-09-13");
    // Sent explicitly rather than left to a server default, and the
    // default the flow sends is the one that cannot lose data.
    expect(sent.conflict).toBe("refuse");
    // The whole tree: an absent source_path, never an empty string.
    expect(sent.sourcePath).toBeUndefined();
    expect(sent.configRevision).toBe("cfg_9f4c1ab");
    expect(sent.idempotencyKey).toBeTruthy();
  });

  it("sends the conflict answer the operator actually chose", async () => {
    const api = createMockApi();
    const restore = vi.spyOn(api, "restoreSnapshot");
    await seed(api);

    renderRestore(api, "run-2026-09-13-0400");
    // The rail renders immediately; the snapshot list does not, and a
    // flow with no snapshot chosen correctly refuses to submit. Waiting
    // for a choice is what keeps the cases below about the request.
    await screen.findByText(/newest known-good/);

    press("Where");
    fireEvent.change(screen.getByLabelText("Restore into"), { target: { value: "/data/restores/resume" } });
    fireEvent.change(screen.getByLabelText("If a file is already there"), { target: { value: "skip" } });

    // Skip is the answer that makes an interrupted restore resumable, and
    // the flow says so where it is chosen rather than in a help page.
    expect(screen.getByText(/finishing a restore that was interrupted/)).toBeTruthy();

    press("Confirm");
    press("Start restore");

    await waitFor(() => expect(restore).toHaveBeenCalledTimes(1));
    expect(restore.mock.calls[0][0].conflict).toBe("skip");
  });

  it("restores one path inside the snapshot when one is named", async () => {
    const api = createMockApi();
    const restore = vi.spyOn(api, "restoreSnapshot");
    await seed(api);

    renderRestore(api, "run-2026-09-13-0400");
    await screen.findByText(/newest known-good/);

    press("What to restore");
    act(() => {
      screen.getByLabelText(/One path inside it/).click();
    });
    fireEvent.change(screen.getByLabelText("Path inside the snapshot"), {
      target: { value: "/srv/shares/finance" }
    });
    press("Where");
    fireEvent.change(screen.getByLabelText("Restore into"), { target: { value: "/data/restores/finance" } });
    press("Confirm");
    press("Start restore");

    await waitFor(() => expect(restore).toHaveBeenCalledTimes(1));
    expect(restore.mock.calls[0][0].sourcePath).toBe("/srv/shares/finance");
  });

  it("offers no snapshot that failed its verification until asked for one", async () => {
    const api = createMockApi();
    await seed(api);

    renderRestore(api);
    await screen.findByText(/newest known-good/);

    // run-2026-09-12-1000 failed against a declared frozen image, so it
    // is not a proven restore point and is not in the list by default.
    expect(screen.queryByText("snapshot_id=b83d15a0ce9f4721")).toBeNull();

    act(() => {
      screen.getByRole("checkbox", { name: /Also show snapshots that failed verification/ }).click();
    });

    expect(await screen.findByText("snapshot_id=b83d15a0ce9f4721")).toBeTruthy();
  });

  it("refuses to submit without a destination, and says why beside the button", async () => {
    const api = createMockApi();
    const restore = vi.spyOn(api, "restoreSnapshot");
    await seed(api);

    renderRestore(api, "run-2026-09-13-0400");
    await screen.findByText(/newest known-good/);

    press("Confirm");

    const start = screen.getByRole("button", { name: "Start restore" });
    expect(start.hasAttribute("disabled")).toBe(true);
    // A disabled control announces nothing about why it is disabled.
    expect(screen.getByText("Name a directory on this deployment to restore into.")).toBeTruthy();
    expect(restore).not.toHaveBeenCalled();
  });

  it("watches the operation it submitted rather than claiming the restore is done", async () => {
    const api = createMockApi();
    await seed(api);

    renderRestore(api, "run-2026-09-13-0400");
    // The rail renders immediately; the snapshot list does not, and a
    // flow with no snapshot chosen correctly refuses to submit. Waiting
    // for a choice is what keeps the cases below about the request.
    await screen.findByText(/newest known-good/);

    press("Where");
    fireEvent.change(screen.getByLabelText("Restore into"), { target: { value: "/data/restores/watch" } });
    press("Confirm");
    press("Start restore");

    // The mock's restore lands as `running` with a live reading, which is
    // what the real route does; the flow shows that operation rather than
    // a success message it has no evidence for.
    expect(await screen.findByText("restore snapshot")).toBeTruthy();
    expect(screen.queryByText(/Restore complete/)).toBeNull();
  });
});
