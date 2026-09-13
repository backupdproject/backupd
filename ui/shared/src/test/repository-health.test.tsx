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

  // The verdict the SERVICE would reach for each fixture domain, spelled
  // out here because the mock is what every UI-only run of this product
  // renders — the dev server, this suite, the browser suite — and a
  // fixture in a state the service cannot produce teaches the wrong
  // thing to everybody reading the screen and to every test written
  // against it. core/internal/app/repositoryhealth.go's
  // decideRepositoryState: any failed probe is FAILING, because a
  // repository that cannot take a backup is the case that needs somebody
  // now; the softer facts are DEGRADED.
  it("carries no domain in a state the service's own rule could not produce", async () => {
    const fleet = await createMockApi().listRepositories();
    expect(fleet.repositories.length).toBeGreaterThan(0);

    for (const domain of fleet.repositories) {
      const cannotBackUp =
        !domain.reachable || !domain.readable || !domain.writable || !domain.credentialsValid;
      const attention =
        !domain.clockSane || domain.maintenanceOverdue || domain.lastVerificationStatus === "failed";
      expect({ domain: domain.domain, state: domain.state }).toEqual({
        domain: domain.domain,
        state: cannotBackUp ? "FAILING" : attention ? "DEGRADED" : "HEALTHY"
      });
    }

    // And all three verdicts are actually exercised by the fixture: a
    // deployment fixture that only ever shows one of them leaves two
    // treatments nobody has looked at.
    const states = fleet.repositories.map((d) => d.state);
    for (const verdict of ["HEALTHY", "DEGRADED", "FAILING"]) expect(states).toContain(verdict);
  });

  it("badges a domain in the same three words a backup set's health uses", async () => {
    const api = createMockApi();

    renderHealth(api);
    await screen.findByRole("heading", { name: "offsite-b2" });

    // Unwritable, so it cannot take a backup at all: that is Failing and
    // not a shade of Degraded, whatever else is also true of it.
    expect(within(cardFor("offsite-b2")).getByText("Failing")).toBeTruthy();
    expect(within(cardFor("primary-nas")).getByText("Healthy")).toBeTruthy();
    // Degraded is the softer verdict: every probe passes and full
    // maintenance has not run inside its window, which costs storage and
    // no restore point.
    expect(within(cardFor("vault-isolated")).getByText("Degraded")).toBeTruthy();
    // An isolated store says so where it is decided: a second backup set
    // pointed here is refused rather than quietly admitted.
    expect(within(cardFor("vault-isolated")).getByText("Isolated")).toBeTruthy();
  });

  it("names the domains needing attention above the cards, and links to maintenance", async () => {
    const api = createMockApi();

    renderHealth(api);
    await screen.findByRole("heading", { name: "offsite-b2" });

    // The banner names the domains that need attention, above the cards,
    // and offers the one screen that can answer "why is it overdue".
    const attention = screen.getByText("2 domains need attention").closest("div")?.parentElement;
    expect(attention?.textContent).toContain("offsite-b2");
    expect(attention?.textContent).toContain("vault-isolated");
    expect(screen.getByRole("button", { name: "Open maintenance" })).toBeTruthy();
    // Two of the three: the banner counts, rather than firing for every
    // fleet that has any domain in it.
    expect(attention?.textContent).not.toContain("primary-nas");
  });
});
