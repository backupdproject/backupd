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
import type { UserEvent } from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import { BackupdError } from "@shared/api/contracts";
import type { BackupdApi } from "@shared/api/contracts";
import { resetGraphForTests } from "@shared/state/graph";
import { RepositoryDomainNewPage } from "@shared/pages/RepositoryDomainNewPage";
import { RepositoryDomainsPage } from "@shared/pages/RepositoryDomainsPage";
import { clockSkew, failingProbes } from "@shared/pages/repositoryFleet";
import type { CreateRepositoryDomainRequest, RepositoryHealth } from "@shared/types/snapshot";

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
  // createRepositoryDomain WRITES the domain into the fleet fixture, the
  // same way the route writes it into the configuration, so without this
  // a later case declaring the same id meets a REPOSITORY_DOMAIN_EXISTS
  // its own scenario never set up.
  resetMockFixtures();
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

describe("declaring a domain", () => {
  /** The screen, with whatever API a case needs behind it. */
  function renderDefine(api: BackupdApi = createMockApi()) {
    render(
      <MemoryRouter initialEntries={["/repositories/new"]}>
        <ApiProvider api={api}>
          <Routes>
            <Route path="/repositories/new" element={<RepositoryDomainNewPage />} />
            <Route path="/repositories" element={<div>the repository domains page</div>} />
          </Routes>
        </ApiProvider>
      </MemoryRouter>
    );
  }

  /** Fill in the two answers a declaration cannot be made without. */
  async function fillIdentity(user: UserEvent, id = "offsite-c2") {
    await user.type(screen.getByLabelText("Domain id"), id);
    await user.type(screen.getByLabelText("Passphrase file on this NAS"), "/etc/backupd/" + id);
  }

  /** The same wizard, landing on the REAL fleet page rather than a stub.
   *  Issue #862's acceptance criterion is that the declared domain
   *  "appears on the fleet", and a stub div proves only that navigate()
   *  was called: the screen it lands on re-reads the fleet on mount, so
   *  a create whose domain never reached the list it navigates to passes
   *  every assertion made against a placeholder. */
  function renderDefineOntoTheFleet(api: BackupdApi) {
    render(
      <MemoryRouter initialEntries={["/repositories/new"]}>
        <ApiProvider api={api}>
          <Routes>
            <Route path="/repositories/new" element={<RepositoryDomainNewPage />} />
            <Route path="/repositories" element={<RepositoryDomainsPage readOnly={false} />} />
          </Routes>
        </ApiProvider>
      </MemoryRouter>
    );
  }

  it("lands on the fleet list with the new domain on it", async () => {
    const user = userEvent.setup();
    renderDefineOntoTheFleet(createMockApi());

    await fillIdentity(user, "offsite-c4");
    await user.click(screen.getByRole("radio", { name: /Isolated/ }));
    await user.click(screen.getByRole("button", { name: "Create domain" }));

    // The fleet page, proven by a domain the wizard never mentioned.
    await waitFor(() => expect(screen.getAllByText("primary-nas").length).toBeGreaterThan(0));

    // And the declared one, on the list the operator was sent to. It is
    // not a green row: the store is written by the first backup run into
    // the domain, so what the fleet read reports is a location that
    // answers and holds no repository yet.
    const row = domainRow("offsite-c4");
    expect(within(row).getByText("Failing")).toBeTruthy();
    // Which probe, not just that something is wrong: a store that has
    // never been written is not readable, and the row must not say
    // "unreachable" -- the location answered, and sending an operator to
    // check a mount that is fine is the whole cost of that word.
    expect(within(row).getByText(/not readable/)).toBeTruthy();
    expect(within(row).queryByText(/unreachable/)).toBeNull();
  });

  it("sends a well-formed declaration and leaves for the fleet list", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    let sent: CreateRepositoryDomainRequest | null = null;
    api.createRepositoryDomain = (req) => {
      sent = req;
      return createMockApi().createRepositoryDomain(req);
    };
    renderDefine(api);

    await fillIdentity(user);
    await user.type(screen.getByLabelText("Description"), "Second copy, off site");
    await user.click(screen.getByRole("radio", { name: /Isolated/ }));
    await user.click(screen.getByRole("radio", { name: /Another instance maintains it/ }));
    await user.click(screen.getByRole("button", { name: "Create domain" }));

    await waitFor(() => expect(screen.getByText("the repository domains page")).toBeTruthy());

    expect(sent).toEqual({
      domain: "offsite-c2",
      description: "Second copy, off site",
      isolation: "isolated",
      // The passphrase is a REFERENCE. A request carrying the secret
      // itself is the one failure on this screen that cannot be undone
      // by editing a form, because it is already in an access log.
      passphrase: { file: "/etc/backupd/offsite-c2" },
      location: "",
      maintenanceOwner: "another-instance"
    });
  });

  it("sends the environment variable's NAME when that is the source chosen", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    let sent: CreateRepositoryDomainRequest | null = null;
    api.createRepositoryDomain = (req) => {
      sent = req;
      return createMockApi().createRepositoryDomain(req);
    };
    renderDefine(api);

    await user.type(screen.getByLabelText("Domain id"), "offsite-c3");
    await user.click(screen.getByRole("button", { name: "An environment variable" }));
    await user.type(
      screen.getByLabelText("Passphrase environment variable"),
      "RETND_OFFSITE_C3_PASSPHRASE"
    );
    await user.click(screen.getByRole("button", { name: "Create domain" }));

    await waitFor(() => expect(sent).not.toBeNull());
    expect(sent!.passphrase).toEqual({ env: "RETND_OFFSITE_C3_PASSPHRASE" });
  });

  it("explains a deployment that does not run the incremental engine, and stays put", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    api.createRepositoryDomain = () =>
      Promise.reject(
        new BackupdError({
          code: "INCREMENTAL_ENGINE_DISABLED",
          message:
            "the incremental (kopia) backup engine is disabled in this deployment: set incremental_engine.enabled: true in config.yaml",
          correlationId: "cid_gate"
        })
      );
    renderDefine(api);

    await fillIdentity(user);
    await user.click(screen.getByRole("button", { name: "Create domain" }));

    // The refusal's own sentence, which is the one place the operator is
    // told which key to set. A generic "could not save" would leave them
    // editing a form that is already correct.
    expect(await screen.findByText(/incremental_engine.enabled: true/)).toBeTruthy();
    expect(screen.getByText(/does not run the incremental engine/i)).toBeTruthy();
    // And nothing navigated: the deployment refused, the form is intact.
    expect(screen.queryByText("the repository domains page")).toBeNull();
    expect(screen.getByLabelText("Domain id")).toHaveValue("offsite-c2");
  });

  it("renders a duplicate id as the refusal it is", async () => {
    const user = userEvent.setup();
    renderDefine();

    // primary-nas is a domain the fixture already declares, and the mock
    // refuses the second declaration exactly as the route does.
    await fillIdentity(user, "primary-nas");
    await user.click(screen.getByRole("button", { name: "Create domain" }));

    expect(await screen.findByText(/already declares a repository domain of that id/)).toBeTruthy();
    expect(screen.queryByText("the repository domains page")).toBeNull();
  });

  it("will not offer Create until the domain has an id and somewhere to read its key from", async () => {
    const user = userEvent.setup();
    renderDefine();

    expect(screen.getByRole("button", { name: "Create domain" })).toBeDisabled();
    await user.type(screen.getByLabelText("Domain id"), "offsite-c2");
    expect(screen.getByRole("button", { name: "Create domain" })).toBeDisabled();
    await user.type(screen.getByLabelText("Passphrase file on this NAS"), "/etc/backupd/p");
    expect(screen.getByRole("button", { name: "Create domain" })).toBeEnabled();
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

  it("says ownership moves by transfer, and that neither answer records anything", async () => {
    const user = userEvent.setup();
    renderDefine();

    expect(screen.getByText(/ownership moves by transfer, never by claim/i)).toBeTruthy();
    // maintenance_owner is a gate on this one write: the field is
    // persisted nowhere, so a screen promising that this deployment
    // "then never maintains the store" would be promising something no
    // configuration records.
    expect(screen.getByText(/records nothing/i)).toBeTruthy();

    await user.click(screen.getByRole("radio", { name: /Another instance maintains it/ }));

    expect(screen.getByText(/ownership is transferred by the instance that holds it/i)).toBeTruthy();
    expect(screen.getByText(/not written into the configuration at all/i)).toBeTruthy();
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
