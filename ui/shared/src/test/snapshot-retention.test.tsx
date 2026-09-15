/**
 * Snapshot retention and the holds that override it (EPIC K, issue #788).
 *
 * The verdict vocabulary is the whole of this file. KEEP, DELETE and
 * REFUSE are three different facts and the temptation is to draw REFUSE
 * as a shade of DELETE, which would tell an operator something is pending
 * when the pass removed nothing. The other half is the badge: last-known-
 * good protection is drawn BARE, because a parenthesised word after
 * "Protected" would read as a placement and protection is not one.
 *
 * The holds are asserted through the request, by hold id: several holds
 * can sit on one snapshot and "release the hold on this snapshot" is
 * ambiguous by exactly one hold.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { ApiProvider } from "@shared/api/ApiContext";
import { RetndError } from "@shared/api/contracts";
import type { RetndApi } from "@shared/api/contracts";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import { SnapshotRetentionPage } from "@shared/pages/SnapshotRetentionPage";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { setsNode, versionNode } from "@shared/state/appNodes";
import type { VersionInfo } from "@shared/types/operation";
import { snapshotRetentionPath } from "@shared/utilities/routes";

const VERSION: VersionInfo = {
  api: "v1", service: "1.3.0", buildCommit: "9f4c1ab", goVersion: "go1.27.0",
  engine: "1.68.2", configRevision: "cfg_9f4c1ab", ready: true, compatible: true
};

async function seed(api: RetndApi) {
  const sets = await api.listSets();
  act(() => {
    graph.commit("test/seed", (tx) => {
      tx.set(versionNode, { data: VERSION, error: null, loading: false });
      tx.set(setsNode, { data: sets, error: null, loading: false });
    });
  });
}

function renderRetention(api: RetndApi, readOnly = false) {
  return render(
    <MemoryRouter initialEntries={[snapshotRetentionPath("production", "postgres-primary")]}>
      <ApiProvider api={api}>
        <Routes>
          <Route
            path="/sets/:source/:set/snapshot-retention"
            element={<SnapshotRetentionPage readOnly={readOnly} />}
          />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

function verdictRow(runId: string): HTMLElement {
  const row = screen.getByRole("button", { name: "Place a hold on " + runId }).closest("tr");
  if (row === null) throw new Error("no verdict row for " + runId);
  return row;
}

describe("snapshot retention", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    resetMockFixtures();
    vi.restoreAllMocks();
  });

  it("draws the three verdicts as three different facts", async () => {
    const api = createMockApi();
    await seed(api);

    renderRetention(api);
    await screen.findByRole("button", { name: "Place a hold on run-2026-09-13-0400" });

    // The newest verified run is protected, and its badge is bare.
    const protectedRow = within(verdictRow("run-2026-09-13-0400"));
    expect(protectedRow.getByText("Keep")).toBeTruthy();
    expect(protectedRow.getByText("Protected")).toBeTruthy();

    // The oldest candidate really would go.
    expect(within(verdictRow("run-2026-09-12-0400")).getByText("Delete")).toBeTruthy();

    // And a held snapshot is REFUSED, not kept: it WAS a delete candidate
    // and something stopped it, which is the verdict that needs somebody.
    const refused = within(verdictRow("run-2026-09-12-1000"));
    expect(refused.getByText("Refused")).toBeTruthy();
    expect(refused.getByText(/Hold: Under investigation/)).toBeTruthy();
  });

  it("badges last-known-good protection with no placement in brackets", async () => {
    const api = createMockApi();
    await seed(api);

    renderRetention(api);
    await screen.findByRole("button", { name: "Place a hold on run-2026-09-13-0400" });

    const badge = within(verdictRow("run-2026-09-13-0400")).getByText("Protected");
    // "Protected (protection)" would read as a placement, and protection
    // is not one.
    expect(badge.textContent).toBe("Protected");

    // A tier's own selection reason is NOT FR-18's placement vocabulary:
    // snapshot retention answers "what about the snapshot the tier
    // selected it for", which may be a timestamp. It is named in words
    // beside the badge rather than forced into the bracket, which means
    // something else.
    const tiered = within(verdictRow("run-2026-09-12-1600"));
    expect(tiered.getByText("Daily").textContent).toBe("Daily");
    expect(tiered.getByText(/^selected by daily: /)).toBeTruthy();
  });

  it("lists the holds in force and releases one by ITS id", async () => {
    const api = createMockApi();
    const release = vi.spyOn(api, "releaseSnapshotHold");
    await seed(api);

    renderRetention(api);
    await screen.findByRole("button", { name: "Release hold hold_01J9Z5" });

    // Two holds on one run, and one on another: the snapshot survives
    // until the last of them is released.
    expect(screen.getByRole("button", { name: "Release hold hold_01J9Z6" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Release hold hold_01J9Z4" })).toBeTruthy();

    act(() => {
      screen.getByRole("button", { name: "Release hold hold_01J9Z5" }).click();
    });
    act(() => {
      screen.getByRole("button", { name: "Release hold" }).click();
    });

    await waitFor(() => expect(release).toHaveBeenCalledTimes(1));
    const sent = release.mock.calls[0][0];
    expect(sent.holdId).toBe("hold_01J9Z5");
    expect(sent.backupSetId).toBe("production/postgres-primary");
    expect(sent.configRevision).toBe("cfg_9f4c1ab");
    expect(sent.idempotencyKey).toBeTruthy();
  });

  it("places a hold on the row it was raised from", async () => {
    const api = createMockApi();
    const hold = vi.spyOn(api, "holdSnapshot");
    await seed(api);

    renderRetention(api);
    await screen.findByRole("button", { name: "Place a hold on run-2026-09-12-0400" });

    act(() => {
      screen.getByRole("button", { name: "Place a hold on run-2026-09-12-0400" }).click();
    });
    expect(screen.getByText(/A hold on run run-2026-09-12-0400/)).toBeTruthy();

    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "Legal hold, matter 2026-114" } });
    act(() => {
      screen.getByRole("button", { name: "Place hold" }).click();
    });

    await waitFor(() => expect(hold).toHaveBeenCalledTimes(1));
    const sent = hold.mock.calls[0][0];
    // The row's run, not the first row's: a dialog opened from a list has
    // to act on the row it was opened from.
    expect(sent.runId).toBe("run-2026-09-12-0400");
    expect(sent.reason).toBe("Legal hold, matter 2026-114");
    expect(sent.idempotencyKey).toBeTruthy();
  });

  // The defect a shared hook instance makes possible: one
  // `useSnapshotOperation` serves every row of this table, so the key it
  // keeps for a retry is a key it keeps for the WHOLE page. A hold that
  // failed on one row left it bound to that failure, and the next row's
  // hold re-sent it — which the service reads as "the same submission
  // again" and answers by replaying the first one, on the wrong
  // snapshot.
  it("mints a new idempotency key for a different row after one row's hold failed", async () => {
    const api = createMockApi();
    const refusal = () =>
      new RetndError({ code: "unknown", message: "the hold could not be recorded" });
    // Both attempts on the first row fail, so the retry stays a retry and
    // the page is still showing that failure when the operator turns to
    // another row — which is the state the defect needed.
    const hold = vi
      .spyOn(api, "holdSnapshot")
      .mockRejectedValueOnce(refusal())
      .mockRejectedValueOnce(refusal());
    await seed(api);

    renderRetention(api);
    await screen.findByRole("button", { name: "Place a hold on run-2026-09-12-0400" });

    const placeHoldOn = async (runId: string, reason: string) => {
      act(() => {
        screen.getByRole("button", { name: "Place a hold on " + runId }).click();
      });
      fireEvent.change(screen.getByLabelText("Reason"), { target: { value: reason } });
      act(() => {
        screen.getByRole("button", { name: "Place hold" }).click();
      });
    };

    await placeHoldOn("run-2026-09-12-0400", "Legal hold, matter 2026-114");
    await waitFor(() => expect(hold).toHaveBeenCalledTimes(1));
    // The failure is on screen, and the dialog is still open on the row
    // that failed: pressing Place hold again there is a RETRY and must
    // reuse the key.
    await screen.findByText(/the hold could not be recorded/i);

    await placeHoldOn("run-2026-09-12-0400", "Legal hold, matter 2026-114");
    await waitFor(() => expect(hold).toHaveBeenCalledTimes(2));
    expect(hold.mock.calls[1][0].idempotencyKey).toBe(hold.mock.calls[0][0].idempotencyKey);

    // A DIFFERENT row is a different intent, whatever happened to the
    // last one.
    await placeHoldOn("run-2026-09-12-1600", "Second look");
    await waitFor(() => expect(hold).toHaveBeenCalledTimes(3));
    expect(hold.mock.calls[2][0].runId).toBe("run-2026-09-12-1600");
    expect(hold.mock.calls[2][0].idempotencyKey).not.toBe(hold.mock.calls[0][0].idempotencyKey);
  });

  it("offers no apply control, because a snapshot-retention pass is not applied from here", async () => {
    const api = createMockApi();
    await seed(api);

    renderRetention(api);
    await screen.findByRole("button", { name: "Place a hold on run-2026-09-13-0400" });

    // api/v1 has no apply route for snapshot retention; a button here
    // would be a control that cannot do what it says.
    expect(screen.queryByRole("button", { name: /Apply/ })).toBeNull();
    expect(screen.getByText(/nothing is removed until the deployment's own pass runs/)).toBeTruthy();
  });
});
