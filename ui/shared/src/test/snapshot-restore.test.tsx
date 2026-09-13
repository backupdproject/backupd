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

/** Whether a control refuses to act. A locked rail step and a refused
 *  Continue both carry the real `disabled` attribute, which is what
 *  keeps them out of the tab order as well as out of reach of a
 *  pointer. */
function locked(name: string) {
  return screen.getByRole("button", { name }).hasAttribute("disabled");
}

/** Whether the rail draws that step as answered. The mark itself is a
 *  decorative glyph with no accessible name, so the rail states the
 *  same fact in an attribute. */
function marked(name: string) {
  return screen.getByRole("button", { name }).getAttribute("data-complete");
}

describe("the restore flow", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    resetMockFixtures();
    vi.useRealTimers();
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

  // The gate, issue #864. This flow used to advance on any press: the
  // rail moved the cursor wherever it was clicked and Continue counted
  // up unconditionally, so an operator could land on Confirm over a
  // destination nobody had named — with checks drawn against every step
  // behind them, because a check meant "the cursor has passed this"
  // rather than "this is answered". The four cases below are that gate.
  it("keeps a step whose question is unanswered out of reach", async () => {
    const api = createMockApi();
    const restore = vi.spyOn(api, "restoreSnapshot");
    await seed(api);

    renderRestore(api);
    await screen.findByText(/newest known-good/);

    // The snapshot (the newest known-good one, in hand) and the scope
    // (the whole tree, the default) are answered, so Where is as far as
    // this flow goes. Confirm is not reachable: no destination.
    expect(locked("Snapshot")).toBe(false);
    expect(locked("What to restore")).toBe(false);
    expect(locked("Where")).toBe(false);
    expect(locked("Confirm")).toBe(true);

    // Not merely drawn shut: the press does not move the cursor.
    press("Confirm");
    expect(screen.getByRole("heading", { name: "Which snapshot" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Start restore" })).toBeNull();

    press("Where");
    expect(locked("Continue")).toBe(true);
    // Walking back out of a step is never gated: that is how a wrong
    // answer gets corrected.
    expect(locked("Back")).toBe(false);
    // A disabled control announces nothing about why it is disabled.
    expect(screen.getByText("Name a directory on this deployment to restore into.")).toBeTruthy();

    fireEvent.change(screen.getByLabelText("Restore into"), {
      target: { value: "/data/restores/gate" }
    });

    expect(locked("Continue")).toBe(false);
    expect(locked("Confirm")).toBe(false);
    // Nothing was submitted on the way through: the commit is still a
    // press on Start restore, and it was never reachable.
    expect(restore).not.toHaveBeenCalled();
  });

  it("does not count a path-inside-the-snapshot answer with no path in it", async () => {
    const api = createMockApi();
    await seed(api);

    renderRestore(api);
    await screen.findByText(/newest known-good/);

    press("What to restore");
    act(() => {
      screen.getByLabelText(/One path inside it/).click();
    });

    // The scope question is now open again: "one path inside it" with no
    // path named is not an answer, and the steps that depend on it shut.
    expect(marked("What to restore")).toBe("false");
    expect(locked("Continue")).toBe(true);
    expect(locked("Where")).toBe(true);
    expect(locked("Confirm")).toBe(true);

    press("Where");
    expect(screen.getByRole("heading", { name: "What to restore" })).toBeTruthy();

    fireEvent.change(screen.getByLabelText("Path inside the snapshot"), {
      target: { value: "/srv/shares/finance" }
    });

    expect(marked("What to restore")).toBe("true");
    expect(locked("Continue")).toBe(false);
    expect(locked("Where")).toBe(false);
    // Still not Confirm: the destination is a question of its own.
    expect(locked("Confirm")).toBe(true);
  });

  it("offers nothing past the first step until a snapshot is in hand", async () => {
    const api = createMockApi();
    const real = await createMockApi().listSnapshots("production", "postgres-primary");
    // Every run failed its verification, so none is offered by default
    // and the flow has no restore point selected at all.
    vi.spyOn(api, "listSnapshots").mockResolvedValue(
      real.map((snapshot) => ({
        ...snapshot,
        verificationStatus: "failed" as const,
        verificationLevelAchieved: null,
        lastKnownGood: false
      }))
    );
    await seed(api);

    renderRestore(api);

    const toggle = screen.getByRole("checkbox", {
      name: /Also show snapshots that failed verification/
    });
    await waitFor(() => expect(locked("What to restore")).toBe(true));
    expect(marked("Snapshot")).toBe("false");
    expect(locked("Where")).toBe(true);
    expect(locked("Confirm")).toBe(true);
    expect(locked("Continue")).toBe(true);

    // Asking for the unverified runs puts one in hand, and the step
    // after it opens. The flow still says the snapshot proves nothing.
    act(() => {
      toggle.click();
    });
    expect(await screen.findByText("snapshot_id=b83d15a0ce9f4721")).toBeTruthy();
    expect(screen.getByText(/This snapshot is not a proven restore point/)).toBeTruthy();

    expect(marked("Snapshot")).toBe("true");
    expect(locked("What to restore")).toBe(false);
    expect(locked("Continue")).toBe(false);
  });

  it("marks the steps that are answered, not the ones the cursor walked past", async () => {
    const api = createMockApi();
    await seed(api);

    renderRestore(api, "run-2026-09-13-0400");
    await screen.findByText(/newest known-good/);

    // On step one, and the scope step is already answered by its own
    // default: the mark is about the answer, not about the cursor.
    expect(marked("Snapshot")).toBe("true");
    expect(marked("What to restore")).toBe("true");
    expect(marked("Where")).toBe("false");
    expect(marked("Confirm")).toBe("false");

    press("Where");
    // Walked onto a step, and it is still unanswered.
    expect(marked("Where")).toBe("false");

    fireEvent.change(screen.getByLabelText("Restore into"), {
      target: { value: "/data/restores/marks" }
    });
    expect(marked("Where")).toBe("true");

    // Emptying it again takes the mark back rather than leaving a check
    // over a destination that is no longer named.
    fireEvent.change(screen.getByLabelText("Restore into"), { target: { value: "   " } });
    expect(marked("Where")).toBe("false");
    expect(locked("Confirm")).toBe(true);

    // The last step is answered by the submission, and nothing has been
    // submitted.
    fireEvent.change(screen.getByLabelText("Restore into"), { target: { value: "/data/restores/marks" } });
    press("Confirm");
    expect(marked("Confirm")).toBe("false");
    expect(screen.getByRole("button", { name: "Start restore" }).hasAttribute("disabled")).toBe(false);
  });

  // The gate is about unanswered questions, and a submitted restore has
  // none left. Emptying a field afterwards must not lock an operator
  // out of the one panel reporting the restore that is running.
  it("keeps the submitted restore in reach after its fields are edited", async () => {
    const api = createMockApi();
    await seed(api);

    renderRestore(api, "run-2026-09-13-0400");
    await screen.findByText(/newest known-good/);

    press("Where");
    fireEvent.change(screen.getByLabelText("Restore into"), {
      target: { value: "/data/restores/inflight" }
    });
    press("Confirm");
    press("Start restore");
    expect(await screen.findByText("restore snapshot")).toBeTruthy();

    press("Back");
    fireEvent.change(screen.getByLabelText("Restore into"), { target: { value: "" } });

    expect(locked("Confirm")).toBe(false);
    press("Confirm");
    expect(screen.getByText("restore snapshot")).toBeTruthy();
    // And it is not a second submission waiting to happen.
    expect(screen.getByRole("button", { name: "Start restore" }).hasAttribute("disabled")).toBe(true);
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

  // The other end of the watch. The case above proves the flow shows the
  // operation it submitted; this one proves it goes on telling the truth
  // when that operation dies, rather than leaving a running panel up for
  // a restore that stopped. The cadence itself is pinned on the hook
  // (snapshot-operation-watch.test.tsx); what matters here is the screen.
  it("reports a restore the service failed as stopped", async () => {
    const api = createMockApi();
    await seed(api);

    renderRestore(api, "run-2026-09-13-0400");
    await screen.findByText(/newest known-good/);

    press("Where");
    fireEvent.change(screen.getByLabelText("Restore into"), { target: { value: "/data/restores/failed" } });
    press("Confirm");

    // Faked from here, so the two-second watch can be waited out without
    // waiting two seconds. Installed after the page has loaded, because
    // the fixture's own reads are on real timers up to this point.
    vi.useFakeTimers();
    press("Start restore");
    await act(async () => {
      vi.advanceTimersByTime(300);
    });
    expect(screen.getByText("restore snapshot")).toBeTruthy();

    vi.spyOn(api, "getOperation").mockImplementation((id) =>
      Promise.resolve({
        id,
        setId: "production/postgres-primary",
        setName: "production/postgres-primary",
        kind: "transfer",
        label: "restore snapshot",
        status: "failed",
        progress: null,
        nonDestructive: false,
        startedAt: "2026-09-13T04:00:00+02:00",
        cycle: null
      })
    );

    await act(async () => {
      vi.advanceTimersByTime(2_000);
    });

    // The panel says the operation stopped instead of drawing a bar for a
    // transfer that is not happening.
    expect(screen.getByText(/Stopped\. Progress is reported only while an operation is running\./)).toBeTruthy();
  });
});
