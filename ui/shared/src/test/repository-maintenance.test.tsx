/**
 * Repository maintenance (EPIC K, issue #788).
 *
 * Ownership is what this screen is for. "Nobody owns this domain" and
 * "another instance owns it" look identical if the owner is drawn as a
 * string and left there, and they are different problems: the first means
 * nothing is reclaiming, ever, and the second means a machine that may be
 * switched off. An empty owner is therefore words, not an empty cell.
 *
 * The per-domain read is the other case. Each domain is a separate
 * request, and one domain answering 404 must not blank the two that
 * answered, because those two are the ones an operator can act on.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiProvider } from "@shared/api/ApiContext";
import { BackupdError } from "@shared/api/contracts";
import type { BackupdApi } from "@shared/api/contracts";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import { RepositoryMaintenancePage } from "@shared/pages/RepositoryMaintenancePage";
import { resetGraphForTests } from "@shared/state/graph";

function renderMaintenance(api: BackupdApi) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <RepositoryMaintenancePage />
      </ApiProvider>
    </MemoryRouter>
  );
}

function cardFor(domain: string): HTMLElement {
  const card = screen.getByRole("heading", { name: domain }).closest("section");
  if (card === null) throw new Error("no card for " + domain);
  return card;
}

describe("repository maintenance", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    resetMockFixtures();
    vi.restoreAllMocks();
  });

  it("names the owner, and says what a domain owned elsewhere means", async () => {
    const api = createMockApi();

    renderMaintenance(api);
    await screen.findByRole("heading", { name: "vault-isolated" });

    const elsewhere = within(cardFor("vault-isolated"));
    expect(elsewhere.getByText("nas-02")).toBeTruthy();
    expect(elsewhere.getByText(/Only nas-02 compacts and reclaims this domain/)).toBeTruthy();
  });

  it("says outright when nobody has claimed a domain", async () => {
    const api = createMockApi();
    const real = await createMockApi().getRepositoryMaintenance("offsite-b2");
    vi.spyOn(api, "getRepositoryMaintenance").mockImplementation((domain) =>
      Promise.resolve(
        domain === "offsite-b2" ? { ...real, owner: "", ownedUntil: null } : { ...real, domain }
      )
    );

    renderMaintenance(api);
    await screen.findByRole("heading", { name: "offsite-b2" });

    const unclaimed = within(cardFor("offsite-b2"));
    expect(unclaimed.getByText("nobody has claimed it")).toBeTruthy();
    expect(unclaimed.getByText(/Nobody owns this domain, so nothing is maintaining it/)).toBeTruthy();
    // And it says what that costs, which is storage and never restores.
    expect(unclaimed.getByText(/still written and still restorable/)).toBeTruthy();
  });

  it("reports an overdue domain with the reason the service gave", async () => {
    const api = createMockApi();

    renderMaintenance(api);
    await screen.findByRole("heading", { name: "offsite-b2" });

    const overdue = within(cardFor("offsite-b2"));
    expect(overdue.getByText("Overdue")).toBeTruthy();
    expect(overdue.getByText(/full maintenance has not run inside its 7-day window/)).toBeTruthy();
    // Non-vacuity: the domain that is on schedule is badged differently.
    expect(within(cardFor("primary-nas")).getByText("On schedule")).toBeTruthy();
  });

  it("reports what the last full pass reclaimed, per domain", async () => {
    const api = createMockApi();

    renderMaintenance(api);
    await screen.findByRole("heading", { name: "primary-nas" });

    expect(within(cardFor("primary-nas")).getByText("41.0 GB")).toBeTruthy();
    // A domain whose full maintenance has not run reclaimed nothing, and
    // that IS a measurement: it is the overdue window's consequence.
    expect(within(cardFor("offsite-b2")).getByText("0 B")).toBeTruthy();
  });

  it("keeps the domains that answered when one domain's read is refused", async () => {
    const api = createMockApi();
    vi.spyOn(api, "getRepositoryMaintenance").mockImplementation((domain) =>
      domain === "offsite-b2"
        ? Promise.reject(
            new BackupdError({
              code: "REPOSITORY_DOMAIN_NOT_FOUND",
              message: "no repository domain offsite-b2 is declared",
              correlationId: "cid_test"
            })
          )
        : createMockApi().getRepositoryMaintenance(domain)
    );

    renderMaintenance(api);
    await screen.findByRole("heading", { name: "primary-nas" });

    expect(screen.getByText("no repository domain offsite-b2 is declared")).toBeTruthy();
    // The other two are still readable, which is the point.
    expect(within(cardFor("primary-nas")).getByText("nas-01")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "vault-isolated" })).toBeTruthy();
  });
});
