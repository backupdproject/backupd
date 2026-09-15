/**
 * The snapshot list (EPIC K, issue #788).
 *
 * Three properties, and each of them is a way this screen could look
 * finished and be wrong:
 *
 *   - a counter the engine did not measure drawn as a zero. "Reused 0
 *     bytes" is a measurement and it describes a repository that
 *     deduplicated nothing, which sends an operator hunting a fault in a
 *     backup that is working.
 *   - a verify button with no idempotency key, or a fresh key per press.
 *     The key describes the RETRY, so a second press after a refusal has
 *     to carry the same one or the service cannot tell a dropped response
 *     from a second request.
 *   - an artifact set shown an empty table. "This set has no snapshots
 *     yet" and "this set can never have snapshots" are different screens,
 *     and the first one tells an operator to wait for a run that will
 *     never produce one.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { ApiProvider } from "@shared/api/ApiContext";
import { RetndError } from "@shared/api/contracts";
import type { RetndApi } from "@shared/api/contracts";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import { SnapshotsPage } from "@shared/pages/SnapshotsPage";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { setsNode, versionNode } from "@shared/state/appNodes";
import type { VersionInfo } from "@shared/types/operation";
import { snapshotsPath } from "@shared/utilities/routes";

const VERSION: VersionInfo = {
  api: "v1", service: "1.3.0", buildCommit: "9f4c1ab", goVersion: "go1.27.0",
  engine: "1.68.2", configRevision: "cfg_9f4c1ab", ready: true, compatible: true
};

/** The graph owns the version and the set list app-wide, so a page
 *  rendered on its own has to be given both: without the version there is
 *  no configuration revision and every mutating control is correctly
 *  refused, which would make the submission cases below vacuous. */
async function seed(api: RetndApi) {
  const sets = await api.listSets();
  act(() => {
    graph.commit("test/seed", (tx) => {
      tx.set(versionNode, { data: VERSION, error: null, loading: false });
      tx.set(setsNode, { data: sets, error: null, loading: false });
    });
  });
}

function renderList(api: RetndApi, source: string, set: string, readOnly = false) {
  return render(
    <MemoryRouter initialEntries={[snapshotsPath(source, set)]}>
      <ApiProvider api={api}>
        <Routes>
          <Route
            path="/sets/:source/:set/snapshots"
            element={<SnapshotsPage readOnly={readOnly} />}
          />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

/** The row for one run, found by the accessible name of its own Inspect
 *  control, which is the only thing on the row that carries the run id. */
function rowFor(runId: string): HTMLElement {
  const button = screen.getByRole("button", { name: "Inspect " + runId });
  const row = button.closest("tr");
  if (row === null) throw new Error("no row for " + runId);
  return row;
}

describe("the snapshot list", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    resetMockFixtures();
    vi.restoreAllMocks();
  });

  it("draws an unmeasured counter as words and never as a zero", async () => {
    const api = createMockApi();
    await seed(api);

    renderList(api, "production", "postgres-primary");

    await screen.findByRole("button", { name: "Inspect run-2026-09-12-1600" });

    // run-2026-09-12-1600 committed a manifest and died before the
    // accounting was written: no reuse figure and no duration.
    const unmeasured = within(rowFor("run-2026-09-12-1600"));
    expect(unmeasured.getAllByText("not measured")).toHaveLength(2);
    expect(unmeasured.queryByText("0 B")).toBeNull();
    expect(unmeasured.queryByText("0m 0s")).toBeNull();

    // Non-vacuity: a run that DID report its reuse prints the figure, so
    // the case above is about the absent counter and not about a table
    // that prints "not measured" everywhere. Logical, read and reused are
    // all near 1.4 TB on this run, which is what a deduplicating
    // repository looks like: 21 GB was actually written.
    const measured = within(rowFor("run-2026-09-13-0400"));
    expect(measured.queryAllByText("not measured")).toHaveLength(0);
    expect(measured.getAllByText("1.4 TB").length).toBeGreaterThan(0);
    expect(measured.getByText("21.0 GB")).toBeTruthy();
  });

  it("badges the level a run PROVED, not the one it was asked for", async () => {
    const api = createMockApi();
    await seed(api);

    renderList(api, "production", "postgres-primary");

    await screen.findByRole("button", { name: "Inspect run-2026-09-13-0400" });

    // Asked for a sampled read, proved only the structure: the badge
    // reports what was achieved, which is the answer (ADR 0014).
    expect(within(rowFor("run-2026-09-12-2200")).getByText("Structure")).toBeTruthy();
    // Non-vacuity from the other side: the run that proved what it was
    // asked for badges that level instead.
    expect(within(rowFor("run-2026-09-13-0400")).getByText("Sampled content")).toBeTruthy();
    // And the failure carries no level at all.
    expect(within(rowFor("run-2026-09-12-1000")).getByText("Failed")).toBeTruthy();
  });

  it("submits one verify carrying the configuration revision and an idempotency key", async () => {
    const api = createMockApi();
    const verify = vi.spyOn(api, "verifySnapshot");
    await seed(api);

    renderList(api, "production", "postgres-primary");
    await screen.findByRole("button", { name: "Inspect run-2026-09-13-0400" });

    act(() => {
      screen.getByRole("button", { name: "Verify latest" }).click();
    });

    await waitFor(() => expect(verify).toHaveBeenCalledTimes(1));
    const sent = verify.mock.calls[0][0];
    expect(sent.backupSetId).toBe("production/postgres-primary");
    expect(sent.configRevision).toBe("cfg_9f4c1ab");
    expect(sent.idempotencyKey).toBeTruthy();
    // No run id: "Verify latest" means the newest snapshot, and naming a
    // row's run here would make the button wrong the moment a run
    // finished while the page was open.
    expect(sent.runId).toBeUndefined();
  });

  it("retries a refused verify under the SAME idempotency key", async () => {
    const api = createMockApi();
    const verify = vi
      .spyOn(api, "verifySnapshot")
      .mockRejectedValue(
        new RetndError({
          code: "unknown",
          message: "The engine is not accepting work.",
          correlationId: "cid_test"
        })
      );
    await seed(api);

    renderList(api, "production", "postgres-primary");
    await screen.findByRole("button", { name: "Inspect run-2026-09-13-0400" });

    act(() => {
      screen.getByRole("button", { name: "Verify latest" }).click();
    });
    await screen.findByText("The engine is not accepting work.");

    act(() => {
      screen.getByRole("button", { name: "Verify latest" }).click();
    });
    await waitFor(() => expect(verify).toHaveBeenCalledTimes(2));

    // The whole value of the header: the service can see that the second
    // press is the same submission, not a second verification.
    expect(verify.mock.calls[1][0].idempotencyKey).toBe(verify.mock.calls[0][0].idempotencyKey);
  });

  it("tells an artifact set it can never have snapshots, rather than showing it an empty table", async () => {
    const api = createMockApi();
    await seed(api);

    renderList(api, "production", "auth-config");

    expect(await screen.findByText(/stores whole artifacts/)).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });
});
