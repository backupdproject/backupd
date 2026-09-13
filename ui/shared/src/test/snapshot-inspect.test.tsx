/**
 * The snapshot inspector (EPIC K, issue #788).
 *
 * The case this file exists for is the failed verification. ADR 0014 says
 * a failed check proves nothing and carries no achieved level at all, and
 * the way that goes wrong on screen is not a crash: it is a page that
 * shows the level the run was ASKED for in the place the level it PROVED
 * belongs, so a failure reads as a pass at a lower depth. That is
 * asserted here against the row, not against the page, because the asked-
 * for level is legitimately on screen a few pixels away.
 *
 * The four byte counts are the other one. They are four measurements of
 * one run and none is derivable from the others, so each is asserted with
 * its own figure: a page that rendered the same number four times would
 * satisfy a test that only counted tiles.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupdApi } from "@shared/api/contracts";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import { SnapshotDetailPage } from "@shared/pages/SnapshotDetailPage";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { setsNode, versionNode } from "@shared/state/appNodes";
import type { VersionInfo } from "@shared/types/operation";
import { snapshotPath } from "@shared/utilities/routes";

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

function renderSnapshot(api: BackupdApi, runId: string, readOnly = false) {
  return render(
    <MemoryRouter initialEntries={[snapshotPath("production", "postgres-primary", runId)]}>
      <ApiProvider api={api}>
        <Routes>
          <Route
            path="/sets/:source/:set/snapshots/:runId"
            element={<SnapshotDetailPage readOnly={readOnly} />}
          />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

/** The value half of one definition row. The label is a `<dt>` and the
 *  value is the `<dd>` beside it, so asserting through this is asserting
 *  about the row rather than about the page. */
function valueOf(label: string): HTMLElement {
  const term = screen.getByText(label).closest("dt");
  const value = term?.nextElementSibling;
  if (!(value instanceof HTMLElement)) throw new Error("no value beside " + label);
  return value;
}

/** One metric tile's whole text: its label, its figure and the line under
 *  it. The tiles carry the four byte counts, and the counts are the thing
 *  that must not be one number rendered four times. */
function tileText(label: string): string {
  // The label is the tile's eyebrow; its parent is the figure that holds
  // the value and the line under it.
  const tile = screen.getByText(label).parentElement;
  if (tile === null) throw new Error("no tile " + label);
  return tile.textContent ?? "";
}

describe("the snapshot inspector", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    resetMockFixtures();
    vi.restoreAllMocks();
  });

  it("keeps the four byte counts distinct", async () => {
    const api = createMockApi();
    await seed(api);

    renderSnapshot(api, "run-2026-09-13-0400");
    await screen.findByText("Logical size");

    expect(tileText("Logical size")).toContain("1.4 TB");
    expect(tileText("Read from source")).toContain("1.4 TB");
    // The one that is genuinely different: deduplication is the whole
    // point of the engine, and a single "backed up" total would report
    // this run as 1.4 TB of growth.
    expect(tileText("Written to repository")).toContain("21.0 GB");
    expect(tileText("Reused")).toContain("1.4 TB");
  });

  it("says a run did not report its walk, rather than claiming it was incomplete", async () => {
    const api = createMockApi();
    await seed(api);

    renderSnapshot(api, "run-2026-09-12-1600");
    await screen.findByText("Source complete");

    expect(valueOf("Source complete").textContent).toContain("not measured");
    expect(tileText("Reused")).toContain("not measured");
    expect(tileText("Reused")).toContain("not accounted for on this run");
    expect(valueOf("Directories").textContent).toBe("not measured");
    // Non-vacuity: the counters this run DID report are still figures.
    expect(valueOf("Entries").textContent).toBe("148,880");
  });

  it("shows a failed verification with NO achieved level, never the level asked for", async () => {
    const api = createMockApi();
    await seed(api);

    renderSnapshot(api, "run-2026-09-12-1000");
    await screen.findByText("Proved");

    // The asked-for level IS on screen, a row above, which is exactly why
    // the absence has to be asserted against the row and not the page.
    expect(valueOf("Asked for").textContent).toContain("Sampled content");

    const proved = valueOf("Proved");
    expect(proved.textContent).toContain("Failed");
    expect(proved.textContent).toContain("no achieved level recorded");
    expect(proved.textContent).not.toContain("Sampled content");
    expect(proved.textContent).not.toContain("Structure");

    // And it is not offered as a restore point.
    expect(screen.getByText(/carries no achieved level at all/)).toBeTruthy();
  });

  it("records a run verified twice as two transitions, which the run row cannot say", async () => {
    const api = createMockApi();
    await seed(api);

    renderSnapshot(api, "run-2026-09-13-0400");
    const timeline = await screen.findByText("How this run got here");
    const card = timeline.closest("section");
    if (card === null) throw new Error("no timeline card");

    // The edge that only a transition log can carry: a verification that
    // was interrupted and restarted records VERIFICATION on BOTH sides of
    // one row. The run record, which is overwritten by every advance,
    // cannot say this happened at all.
    const rows = within(card).getAllByRole("listitem");
    const restart = rows.find((row) => row.textContent?.includes("restarted after"));
    expect(restart, "no restarted-verification transition").toBeTruthy();
    expect((restart?.textContent ?? "").split("Verifying").length - 1).toBe(2);
    // Non-vacuity: the log is the whole run, not only that one edge.
    expect(rows).toHaveLength(8);
  });

  it("places a hold as one durable operation carrying the run, the reason and a key", async () => {
    const api = createMockApi();
    const hold = vi.spyOn(api, "holdSnapshot");
    await seed(api);

    renderSnapshot(api, "run-2026-09-13-0400");
    await screen.findByText("Logical size");

    act(() => {
      screen.getByRole("button", { name: "Place hold\u2026" }).click();
    });

    const confirm = screen.getByRole("button", { name: "Place hold" });
    // The reason is required by the product and by the service: a hold
    // nobody can attribute is one nobody dares release.
    expect(confirm.hasAttribute("disabled")).toBe(true);

    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "Kept for the 2026 Q3 audit" } });
    expect(screen.getByRole("button", { name: "Place hold" }).hasAttribute("disabled")).toBe(false);

    act(() => {
      screen.getByRole("button", { name: "Place hold" }).click();
    });

    await waitFor(() => expect(hold).toHaveBeenCalledTimes(1));
    const sent = hold.mock.calls[0][0];
    expect(sent.backupSetId).toBe("production/postgres-primary");
    expect(sent.runId).toBe("run-2026-09-13-0400");
    expect(sent.reason).toBe("Kept for the 2026 Q3 audit");
    expect(sent.configRevision).toBe("cfg_9f4c1ab");
    expect(sent.idempotencyKey).toBeTruthy();
  });

  it("releases one hold by ITS id, not by the snapshot's", async () => {
    const api = createMockApi();
    const release = vi.spyOn(api, "releaseSnapshotHold");
    await seed(api);

    // Two holds sit on this run, which is what makes releasing by hold id
    // load-bearing: releasing "the hold on this snapshot" would be
    // ambiguous by exactly one hold.
    renderSnapshot(api, "run-2026-09-12-1000");
    await screen.findByRole("button", { name: "Release hold hold_01J9Z6" });

    act(() => {
      screen.getByRole("button", { name: "Release hold hold_01J9Z6" }).click();
    });
    act(() => {
      screen.getByRole("button", { name: "Release hold" }).click();
    });

    await waitFor(() => expect(release).toHaveBeenCalledTimes(1));
    expect(release.mock.calls[0][0].holdId).toBe("hold_01J9Z6");
    expect(release.mock.calls[0][0].idempotencyKey).toBeTruthy();
  });
});
