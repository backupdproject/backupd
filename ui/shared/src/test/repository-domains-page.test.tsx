/**
 * The repository domains screen (EPIC K, issue #788).
 *
 * What is worth pinning here is what an operator would act wrongly on if
 * it were wrong.
 *
 * Co-tenancy: the page has to say which sets share a store, because a
 * shared store is a shared key and a shared blast radius, and `may_share`
 * is what decides whether another set can be pointed at it at all. A
 * topology that drew an isolated domain like a shared one would invite
 * exactly the request the service refuses.
 *
 * Ownership: the owner comes from a per-domain sub-resource, and a
 * sub-read that fails must not read as "nobody owns this". Those two are
 * different facts with opposite remedies — one is a mount or a
 * permission, the other is a domain nobody is maintaining — so the page
 * is driven against a fleet whose maintenance read refuses for one domain
 * and answers for the others.
 *
 * Clock skew: nullable and SIGNED. Null is "not measured" and is never
 * drawn as a synchronised clock, and a negative skew has to keep its sign,
 * because behind is the dangerous direction: it dates a new snapshot
 * before one already stored.
 */
import { afterEach, describe, expect, it } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { BackupdError } from "@shared/api/contracts";
import type { BackupdApi } from "@shared/api/contracts";
import { resetGraphForTests } from "@shared/state/graph";
import { RepositoryDomainNewPage } from "@shared/pages/RepositoryDomainNewPage";
import { RepositoryDomainsPage } from "@shared/pages/RepositoryDomainsPage";
import { clockSkew, failingProbes } from "@shared/pages/repositoryFleet";
import type { RepositoryHealth } from "@shared/types/snapshot";

async function renderDomains(api: BackupdApi = createMockApi()): Promise<void> {
  render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <RepositoryDomainsPage readOnly={false} />
      </ApiProvider>
    </MemoryRouter>
  );
  // The domain names the table and the topology below it, so this waits
  // for the first of them rather than for a unique one.
  await screen.findAllByText("primary-nas");
}

/** The row for one domain, found through the cell that names it. */
function domainRow(domain: string): HTMLElement {
  const cells = screen.getAllByText(domain);
  const row = cells.map((cell) => cell.closest("tr")).find((tr) => tr !== null);
  if (!row) throw new Error("no table row for " + domain);
  return row;
}

/** The topology card for one domain: the nearest ancestor panel that
 *  holds the list of co-tenant backup sets, which is what tells it from
 *  the table row carrying the same name. */
function topologyCard(domain: string): HTMLElement {
  for (const node of screen.getAllByText(domain)) {
    for (let at = node.parentElement; at; at = at.parentElement) {
      if (at.tagName === "TR") break;
      if (at.tagName === "DIV" && at.querySelector("ul") !== null) return at;
    }
  }
  throw new Error("no topology card for " + domain);
}

afterEach(() => {
  resetGraphForTests();
});

describe("the domains table", () => {
  it("lists every declared domain with the state the service reported", async () => {
    await renderDomains();

    expect(within(domainRow("primary-nas")).getByText("Healthy")).toBeTruthy();
    // Unwritable: a store that cannot take a backup at all is Failing,
    // which is the verdict the service reaches for these probes.
    expect(within(domainRow("offsite-b2")).getByText("Failing")).toBeTruthy();
    // Every probe passing, out of its maintenance window: Degraded.
    expect(within(domainRow("vault-isolated")).getByText("Degraded")).toBeTruthy();
  });

  it("names the probes a degraded domain is actually failing", async () => {
    await renderDomains();

    // Not "degraded" repeated: the remedies differ, so the row says which
    // ones. offsite-b2 is readable and unwritable with a skewed clock and
    // overdue maintenance.
    const row = domainRow("offsite-b2");
    expect(within(row).getByText(/not writable/)).toBeTruthy();
    expect(within(row).getByText(/maintenance overdue/)).toBeTruthy();
    expect(within(domainRow("primary-nas")).queryByText(/not writable/)).toBeNull();
  });

  it("keeps a clock skew's sign, and does not draw an unmeasured one as zero", async () => {
    await renderDomains();

    expect(within(domainRow("offsite-b2")).getByText(/behind this deployment/)).toBeTruthy();
    expect(within(domainRow("primary-nas")).getByText(/ahead of this deployment/)).toBeTruthy();
    expect(within(domainRow("vault-isolated")).getByText("not measured")).toBeTruthy();
  });

  it("names the maintenance owner each domain's own sub-resource reports", async () => {
    await renderDomains();

    expect(within(domainRow("primary-nas")).getByText("Owned by nas-01")).toBeTruthy();
    // The one owned by another instance, which is the answer that decides
    // whether anything here could be pressed.
    expect(within(domainRow("vault-isolated")).getByText("Owned by nas-02")).toBeTruthy();
  });

  it("reports a maintenance read that failed as a failure, not as an unowned domain", async () => {
    const real = createMockApi();
    const api: BackupdApi = {
      ...real,
      getRepositoryMaintenance: (domain) =>
        domain === "offsite-b2"
          ? Promise.reject(
              new BackupdError({
                code: "unknown",
                message: "the maintenance record could not be read",
                correlationId: "cid_test"
              })
            )
          : real.getRepositoryMaintenance(domain)
    };

    await renderDomains(api);

    await waitFor(() =>
      expect(within(domainRow("offsite-b2")).getByText(/could not be read/)).toBeTruthy()
    );
    expect(within(domainRow("offsite-b2")).queryByText(/Nobody has claimed it/)).toBeNull();
    // And the rest of the fleet is still on screen: one failed sub-read
    // does not take three working domains off the page.
    expect(within(domainRow("primary-nas")).getByText("Owned by nas-01")).toBeTruthy();
  });
});

describe("the topology", () => {
  it("draws a shared domain as its co-tenants and says they deduplicate against each other", async () => {
    await renderDomains();

    const card = topologyCard("primary-nas");
    expect(within(card).getByText("production/postgres-primary")).toBeTruthy();
    expect(within(card).getByText("production/billing-mysql")).toBeTruthy();
    expect(within(card).getByText(/2 sets deduplicate against each other here/)).toBeTruthy();
    expect(within(card).getAllByText("Shared").length).toBeGreaterThan(0);
  });

  it("says an isolated domain would refuse a second set", async () => {
    await renderDomains();

    const card = topologyCard("vault-isolated");
    expect(within(card).getAllByText("Isolated").length).toBeGreaterThan(0);
    // The page says it, and the service's own `detail` for this fixture
    // says it too; both belong on the card, so this asserts presence
    // rather than uniqueness.
    expect(within(card).getAllByText(/A second set pointed here is refused/).length).toBeGreaterThan(0);
    expect(within(card).getByText("No backup set writes here yet.")).toBeTruthy();
    expect(within(card).queryByText(/deduplicate against each other/)).toBeNull();
  });
});

describe("what the page says when there is nothing to say", () => {
  it("tells a deployment with no domain apart from one whose read failed", async () => {
    const real = createMockApi();
    const empty: BackupdApi = {
      ...real,
      listRepositories: () => Promise.resolve({ generatedAt: "2026-09-13T04:00:00+02:00", repositories: [] })
    };

    render(
      <MemoryRouter>
        <ApiProvider api={empty}>
          <RepositoryDomainsPage readOnly={false} />
        </ApiProvider>
      </MemoryRouter>
    );

    expect(await screen.findByText("No repository domain is declared")).toBeTruthy();
    expect(screen.queryByText(/could not be read/)).toBeNull();
  });

  it("reports a fleet read that failed as a failure", async () => {
    const real = createMockApi();
    const broken: BackupdApi = {
      ...real,
      listRepositories: () =>
        Promise.reject(
          new BackupdError({
            code: "unknown",
            message: "the repository fleet could not be read",
            correlationId: "cid_test"
          })
        )
    };

    render(
      <MemoryRouter>
        <ApiProvider api={broken}>
          <RepositoryDomainsPage readOnly={false} />
        </ApiProvider>
      </MemoryRouter>
    );

    expect(await screen.findByText(/the repository fleet could not be read/)).toBeTruthy();
    expect(screen.queryByText("No repository domain is declared")).toBeNull();
  });
});

describe("defining a domain", () => {
  /** The screen with no write behind it. It talks to nothing, so it takes
   *  no API at all. */
  function renderDefine() {
    render(
      <MemoryRouter>
        <ApiProvider api={createMockApi()}>
          <RepositoryDomainNewPage />
        </ApiProvider>
      </MemoryRouter>
    );
  }

  it("collects nothing it cannot store, and says why Create is refused", () => {
    renderDefine();

    expect(screen.getByText(/no route that creates a repository domain/i)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Create domain" })).toBeDisabled();
    // Every identity field, the passphrase above all, is disabled: a box
    // that takes a passphrase it cannot save is the worst lie on the
    // screen.
    for (const label of ["Domain id", "Storage location", "Encryption passphrase"]) {
      expect(screen.getByLabelText(label)).toBeDisabled();
    }
  });

  it("says which controls are dead, rather than calling live ones disabled", () => {
    renderDefine();

    // The sharing and ownership radios ARE live: choosing between them is
    // the whole of what this screen is for until a create route exists,
    // and each answer changes what the page says. A banner claiming
    // every control below is disabled tells an operator not to touch the
    // one thing that works.
    for (const name of [/Shared/, /Isolated/, /This instance maintains it/, /Another instance maintains it/]) {
      expect(screen.getByRole("radio", { name })).toBeEnabled();
    }
    expect(screen.getByText(/no route that creates a repository domain/i)).toBeTruthy();
    expect(document.body.textContent).not.toContain("every control below is disabled");
  });

  it("states what each sharing answer commits every set in the domain to", async () => {
    const user = userEvent.setup();
    renderDefine();

    // Shared is the default, and the six boundaries are stated as what is
    // shared rather than as a warning nobody can act on.
    expect(screen.getByText(/share all six of these/i)).toBeTruthy();
    expect(screen.getByText("One encryption key")).toBeTruthy();
    expect(screen.getByText("One corruption blast radius")).toBeTruthy();

    await user.click(screen.getByRole("radio", { name: /Isolated/ }));

    // The same six, with the claim inverted: this is the one control on
    // the screen that changes what it says.
    expect(screen.getByText(/shares none of these/i)).toBeTruthy();
    expect(screen.getByText("One encryption key")).toBeTruthy();
  });

  it("says ownership moves by transfer whichever answer is chosen", async () => {
    const user = userEvent.setup();
    renderDefine();

    expect(screen.getByText(/ownership moves by transfer, never by claim/i)).toBeTruthy();

    await user.click(screen.getByRole("radio", { name: /Another instance maintains it/ }));

    expect(screen.getByText(/ownership is transferred by the instance that holds it, never taken/i)).toBeTruthy();
  });
});

describe("the probe and skew readings themselves", () => {
  const healthy: RepositoryHealth = {
    domain: "d",
    mayShare: true,
    state: "HEALTHY",
    reachable: true,
    readable: true,
    writable: true,
    credentialsValid: true,
    clockSane: true,
    clockSkewSeconds: 0,
    maintenanceOverdue: false,
    lastMaintenanceAt: null,
    lastMaintenanceResult: "",
    lastSnapshotAt: null,
    lastSnapshotStatus: "",
    lastVerificationAt: null,
    lastVerificationStatus: "",
    backupSets: [],
    detail: ""
  };

  it("reports nothing for a domain passing every probe", () => {
    expect(failingProbes(healthy)).toEqual([]);
  });

  it("reports each failing probe separately, because the remedies differ", () => {
    expect(failingProbes({ ...healthy, writable: false })).toEqual(["not writable"]);
    expect(failingProbes({ ...healthy, credentialsValid: false, maintenanceOverdue: true })).toEqual([
      "credentials rejected",
      "maintenance overdue"
    ]);
  });

  it("tells a measured zero from an unmeasured skew", () => {
    expect(clockSkew(0)).toBe("0 s (in step)");
    expect(clockSkew(null)).toBe("not measured");
  });

  it("keeps the direction of a skew, in the unit that reads", () => {
    expect(clockSkew(45)).toBe("45 s ahead of this deployment");
    expect(clockSkew(-184)).toBe("3.1 min behind this deployment");
    expect(clockSkew(7200)).toBe("2 h ahead of this deployment");
  });
});
