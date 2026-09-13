/**
 * Telling an Artifact set from an Incremental one where an operator
 * actually stands (EPIC K, issue #788, design screens 6 and 7).
 *
 * The epic's own decision is that the two engines are told apart by a
 * BADGE and a different set of metrics, not by a different layout. That
 * only works if the badge is where the sets are: the snapshot list and
 * the deployment defaults had it, and the two screens an operator running
 * both engines side by side actually uses — the sets list and a set's own
 * page — did not, so the answer to "which engine is this one" was three
 * cards down inside a configuration panel.
 *
 * The metric strip is the other half of screen 6. An incremental set's
 * newest snapshot carries the five figures the whole epic is about, and a
 * detail page that reported none of them made the engine a label rather
 * than a difference. Absent is not zero here either: the strip goes
 * through `measured()` like every other surface, so a run whose engine
 * accounted for no reuse says so.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupdApi } from "@shared/api/contracts";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { BackupSetsPage } from "@shared/pages/BackupSetsPage";
import type { AsyncState } from "@shared/hooks/useAsync";
import type { BackupSet } from "@shared/types/backup";
import { resetGraphForTests } from "@shared/state/graph";
import { backupSetPath } from "@shared/utilities/routes";

const noop = () => {};

function renderDetail(source: string, set: string, api: BackupdApi) {
  return render(
    <MemoryRouter initialEntries={[backupSetPath(source, set)]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={false} />} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

function renderSets(sets: AsyncState<BackupSet[]>) {
  return render(
    <MemoryRouter>
      <ApiProvider api={createMockApi()}>
        <BackupSetsPage sets={sets} readOnly={false} />
      </ApiProvider>
    </MemoryRouter>
  );
}

/** One set's card in the list, found through the identity the card
 *  prints. */
function cardFor(id: string): HTMLElement {
  const card = screen.getByText(id).closest("article");
  if (card === null) throw new Error("no card for " + id);
  return card;
}

afterEach(() => {
  cleanup();
  resetGraphForTests();
  resetMockFixtures();
  vi.restoreAllMocks();
});

describe("the sets list says which engine each set runs", () => {
  it("badges both engines on their own cards", async () => {
    const data = await createMockApi().listSets();
    renderSets({ data, error: null, loading: false, reload: noop });

    // production/postgres-primary is incremental and production/auth-config
    // is not, and the whole point is that a list holding both says so.
    expect(within(cardFor("production/postgres-primary")).getByText("Incremental")).toBeTruthy();
    expect(within(cardFor("production/auth-config")).getByText("Artifact")).toBeTruthy();
    // Neither card leaks the contract's word for the engine.
    expect(within(cardFor("production/postgres-primary")).queryByText(/kopia/i)).toBeNull();
  });
});

describe("a set's own page says which engine it runs, in its header", () => {
  it("badges an incremental set beside its name", async () => {
    const api = createMockApi();
    renderDetail("production", "postgres-primary", api);

    const heading = await screen.findByRole("heading", { name: /Production PostgreSQL/ });
    expect(within(heading).getByText("Incremental")).toBeTruthy();
  });

  it("badges an artifact set the same way, in the same place", async () => {
    const api = createMockApi();
    renderDetail("production", "auth-config", api);

    const heading = await screen.findByRole("heading", { name: /Auth service config/ });
    expect(within(heading).getByText("Artifact")).toBeTruthy();
  });
});

describe("the incremental detail page reports its newest snapshot", () => {
  it("shows the five figures of the last run, and what the run proved", async () => {
    const api = createMockApi();
    renderDetail("production", "postgres-primary", api);

    const strip = await screen.findByRole("region", { name: "Newest snapshot" });
    // Each figure read through the label above it. Two of them are the
    // same number for this fixture — a tree of 1.4 TB almost all of which
    // the repository already held, which is the incremental engine's
    // whole claim — so a bare text match would prove nothing about which
    // cell is which.
    const figure = (label: string): string => {
      const cell = within(strip).getByText(label).parentElement;
      if (cell === null) throw new Error("no metric cell for " + label);
      return cell.textContent ?? "";
    };

    // Four byte counts, never one: a single total would report a
    // deduplicating repository as growing by the size of the source.
    expect(figure("Entries scanned")).toContain("148,951");
    expect(figure("Logical size")).toContain("1.4 TB");
    expect(figure("Read from source")).toContain("1.4 TB");
    expect(figure("Written to repository")).toContain("21.0 GB");
    expect(figure("Reused")).toContain("1.4 TB");
    expect(figure("Duration")).toContain("3m 34s");
    // And what the run proved, which is the achieved level and not the
    // one it was asked for.
    expect(strip.textContent).toContain("Sampled content");
  });

  it("says not measured rather than zero for a run nobody measured", async () => {
    const api = createMockApi();
    const [newest, ...rest] = await createMockApi().listSnapshots("production", "postgres-primary");
    vi.spyOn(api, "listSnapshots").mockResolvedValue([
      { ...newest, logicalBytes: null, contentReusedBytes: null, entriesScanned: null },
      ...rest
    ]);

    renderDetail("production", "postgres-primary", api);

    const strip = await screen.findByRole("region", { name: "Newest snapshot" });
    expect(within(strip).getAllByText("not measured").length).toBeGreaterThan(0);
    expect(within(strip).queryByText("0 B")).toBeNull();
  });

  it("draws no snapshot strip at all for an artifact set", async () => {
    const api = createMockApi();
    const snapshots = vi.spyOn(api, "listSnapshots");

    renderDetail("production", "auth-config", api);
    await screen.findByRole("heading", { name: /Auth service config/ });

    // Not an empty strip, and not a refused read drawn as an error: an
    // artifact set has no snapshot history at all, so the page does not
    // ask for one.
    expect(screen.queryByRole("region", { name: "Newest snapshot" })).toBeNull();
    expect(snapshots).not.toHaveBeenCalled();
  });
});
