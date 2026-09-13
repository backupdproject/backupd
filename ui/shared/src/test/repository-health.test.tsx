/**
 * Repository health, and the two facts a summary would destroy (EPIC K,
 * issue #788).
 *
 * A store that is READABLE and NOT WRITABLE is the case this screen
 * exists for. Every snapshot already in it can still be restored and no
 * new one can be written, and those need different people: one is a
 * restore that works, the other is a permission. A page that reduced the
 * probes to one "unavailable" would send an operator to the wrong one.
 *
 * The clock is the other. It is signed and nullable, and both halves are
 * asserted: a null drawn as "0 s" would read as a perfectly synchronised
 * repository, and a magnitude drawn without its sign hides the one
 * direction that mis-orders manifests.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupdApi } from "@shared/api/contracts";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import { RepositoryHealthPage } from "@shared/pages/RepositoryHealthPage";
import { resetGraphForTests } from "@shared/state/graph";

function renderHealth(api: BackupdApi) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <RepositoryHealthPage />
      </ApiProvider>
    </MemoryRouter>
  );
}

/** One domain's card, found by its own heading. By heading and not by
 *  text: the attention banner above names the same domains, and a text
 *  match would pick up whichever came first. */
function cardFor(domain: string): HTMLElement {
  const card = screen.getByRole("heading", { name: domain }).closest("section");
  if (card === null) throw new Error("no card for " + domain);
  return card;
}

describe("repository health", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    resetMockFixtures();
    vi.restoreAllMocks();
  });

  it("reports a readable, unwritable store as still restorable", async () => {
    const api = createMockApi();

    renderHealth(api);
    await screen.findByRole("heading", { name: "offsite-b2" });

    const degraded = within(cardFor("offsite-b2"));
    // Readable passes, writable does not, and they are two rows.
    const readable = degraded.getByText("Readable").closest("li");
    const writable = degraded.getByText("Writable").closest("li");
    expect(readable?.textContent).toContain("Pass");
    expect(writable?.textContent).toContain("Attention");
    expect(writable?.textContent).toContain("Snapshots already here can still be restored");

    // Non-vacuity: the healthy store passes the same probe, so the row
    // above is about this domain and not about a check list that always
    // says "Attention".
    const healthy = within(cardFor("primary-nas"));
    expect(healthy.getByText("Writable").closest("li")?.textContent).toContain("Pass");
  });

  it("prints the clock's SIGN, and never draws an unmeasured one as zero", async () => {
    const api = createMockApi();

    renderHealth(api);
    await screen.findByRole("heading", { name: "offsite-b2" });

    // Negative skew is the dangerous direction: it dates a new snapshot
    // before one already stored.
    expect(within(cardFor("offsite-b2")).getByText(/184 s behind the repository/)).toBeTruthy();
    // Measured and small.
    expect(within(cardFor("primary-nas")).getByText(/within 2 s/)).toBeTruthy();
    // Not measured at all, which is not the same claim as a perfect zero.
    const isolated = within(cardFor("vault-isolated"));
    expect(isolated.getByText(/not measured/)).toBeTruthy();
    expect(isolated.queryByText(/within 0 s/)).toBeNull();
  });

  it("badges a domain in the same three words a backup set's health uses", async () => {
    const api = createMockApi();

    renderHealth(api);
    await screen.findByRole("heading", { name: "offsite-b2" });

    expect(within(cardFor("offsite-b2")).getByText("Degraded")).toBeTruthy();
    expect(within(cardFor("primary-nas")).getByText("Healthy")).toBeTruthy();
    // An isolated store says so where it is decided: a second backup set
    // pointed here is refused rather than quietly admitted.
    expect(within(cardFor("vault-isolated")).getByText("Isolated")).toBeTruthy();
  });

  it("names the domains needing attention above the cards, and links to maintenance", async () => {
    const api = createMockApi();

    renderHealth(api);
    await screen.findByRole("heading", { name: "offsite-b2" });

    // The banner names the domain that needs attention, above the cards,
    // and offers the one screen that can answer "why is it overdue".
    const attention = screen.getByText("One domain needs attention").closest("div")?.parentElement;
    expect(attention?.textContent).toContain("offsite-b2");
    expect(screen.getByRole("button", { name: "Open maintenance" })).toBeTruthy();
    // One domain of the three: the banner counts, rather than firing for
    // every fleet that has any domain in it.
    expect(screen.getAllByRole("heading").length).toBeGreaterThan(2);
  });
});
