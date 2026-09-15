/**
 * The deployment's Backup defaults screen (EPIC K, issue #788).
 *
 * The page states three kinds of thing and only two of them are stored
 * anywhere, so the tests are mostly about telling those apart.
 *
 * The retention chain and the polling cadence are real: they come off GET
 * /settings and are edited on the Settings page, so this screen reports
 * them and must report what the service actually says rather than a
 * plausible default written into the JSX — which is the exact defect #299
 * deleted three controls for.
 *
 * The engine, domain, consistency and verification answers are NOT stored:
 * this contract carries no deployment-wide default for any of them. The
 * page therefore has to say so, in as many words, beside the answers the
 * wizard opens with. A card that quietly presented them as configured
 * values would send an operator looking for the control that changes them.
 *
 * Maintenance ownership is the third kind: the owner is real and the
 * transfer is not implemented on this contract. So the owner has to be the
 * one the sub-resource reported, and Transfer has to be present and
 * refused — present, because an operator looking for it has to learn that
 * it is not available rather than that it is elsewhere.
 */
import { afterEach, describe, expect, it } from "vitest";
import { act, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import type { AppSettings, RetndApi } from "@shared/api/contracts";
import { resetGraphForTests } from "@shared/state/graph";
import { setsNode } from "@shared/state/appNodes";
import { useResource } from "@shared/state/resource";
import { BackupDefaultsPage } from "@shared/pages/BackupDefaultsPage";

/** The page wired to the shared sets node exactly as App.tsx wires it, so
 *  the deployment summary is reading the same list every other surface
 *  does rather than a fixture handed in here. */
function DefaultsScreen({ api }: { api: RetndApi }) {
  useResource(setsNode, () => api.listSets(), [api]);
  return <BackupDefaultsPage readOnly={false} />;
}

/** Lets every mock read land. Each one resolves on a timer, and this page
 *  makes three, so an assertion made before they arrive is an assertion
 *  about a loading card. It also drains AFTER a test: fetchResource
 *  writes into the shared sets node whenever a read lands, mounted or
 *  not, so a request still out would arrive in the middle of the next
 *  test. */
function settle(ms = 400): Promise<void> {
  // The executor form deliberately: this workspace's `lib` predates
  // Promise.withResolvers, and every other suite here drains the same way.
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function renderDefaults(api: RetndApi = createMockApi()): Promise<void> {
  render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <DefaultsScreen api={api} />
      </ApiProvider>
    </MemoryRouter>
  );
  await screen.findByRole("heading", { name: "Maintenance ownership" });
  await act(async () => {
    await settle();
  });
}

function card(name: string): HTMLElement {
  const heading = screen.getByRole("heading", { name });
  const section = heading.closest("section");
  if (!section) throw new Error("no card for " + name);
  return section;
}

afterEach(async () => {
  await act(async () => {
    await settle();
  });
  resetGraphForTests();
});

describe("what a new backup set starts with", () => {
  it("says these are the wizard's answers and not a stored deployment default", async () => {
    await renderDefaults();

    const panel = card("New backup sets");
    expect(within(panel).getByText(/no deployment-wide backup defaults/i)).toBeTruthy();
    // The four the wizard asks, reported rather than offered: no control
    // for any of them on this page.
    expect(within(panel).getByText("Artifact")).toBeTruthy();
    expect(within(panel).getByText("Live")).toBeTruthy();
    expect(within(panel).getByText("Sampled content")).toBeTruthy();
    expect(within(panel).queryByRole("radio")).toBeNull();
    expect(within(panel).queryByRole("combobox")).toBeNull();
  });

  it("reads the polling cadence off the service rather than stating a number of its own", async () => {
    const real = createMockApi();
    const loaded = await real.getSettings();
    const api: RetndApi = {
      ...real,
      getSettings: (): Promise<AppSettings> =>
        Promise.resolve({
          ...loaded,
          service: { ...loaded.service, pollIntervalSeconds: 42 * 60 }
        })
    };

    await renderDefaults(api);

    expect(within(card("New backup sets")).getByText("42 min")).toBeTruthy();
  });
});

describe("the retention defaults", () => {
  it("draws the chain the service reports, tier by tier", async () => {
    const real = createMockApi();
    const loaded = await real.getSettings();

    await renderDefaults();

    const panel = card("Retention defaults");
    for (const tier of loaded.retention.tiers) {
      expect(within(panel).getByText(tier.name)).toBeTruthy();
    }
    expect(within(panel).getByText(loaded.retention.timezone, { exact: false })).toBeTruthy();
  });

  it("says whether the newest known-good backup is protected, rather than assuming it is", async () => {
    const real = createMockApi();
    const loaded = await real.getSettings();
    const unprotected: RetndApi = {
      ...real,
      getSettings: (): Promise<AppSettings> =>
        Promise.resolve({
          ...loaded,
          retention: { ...loaded.retention, protectLastKnownGood: false }
        })
    };

    await renderDefaults(unprotected);

    expect(within(card("Retention defaults")).getByText("Expires with the chain")).toBeTruthy();
    expect(within(card("Retention defaults")).queryByText("Never expired")).toBeNull();
  });
});

describe("the maintenance ownership table", () => {
  it("names each domain's owner as its own sub-resource reported it", async () => {
    await renderDefaults();

    const panel = card("Maintenance ownership");
    // Two domains are owned by nas-01 and one by nas-02, which is the
    // answer that decides whether anything here could ever be pressed
    // from this instance.
    expect(within(panel).getAllByText("nas-01")).toHaveLength(2);
    expect(within(panel).getByText("nas-02")).toBeTruthy();
    // Two of the three are out of their maintenance window, and one of
    // those is owned elsewhere: the table's job is to let an operator see
    // which of the two they can do anything about.
    expect(within(panel).getAllByText(/Overdue/)).toHaveLength(2);
  });

  it("draws Transfer and refuses it, saying no route performs it yet", async () => {
    await renderDefaults();

    const panel = card("Maintenance ownership");
    const transfers = within(panel).getAllByRole("button", { name: /Transfer ownership/ });
    expect(transfers).toHaveLength(3);
    for (const button of transfers) expect(button).toBeDisabled();
    expect(within(panel).getByText(/not on this API yet/i)).toBeTruthy();
    // And the page does not claim to know which instance it is itself:
    // nothing on the wire tells it, so no row may say "this one".
    expect(within(panel).queryByText(/this instance/i)).toBeNull();
  });
});

describe("what the deployment is running", () => {
  it("counts the two engines apart and names each set's domain", async () => {
    await renderDefaults();

    const panel = card("What this deployment runs today");
    const incremental = within(panel).getByText("Incremental sets").parentElement as HTMLElement;
    expect(within(incremental).getByText("3")).toBeTruthy();
    const artifact = within(panel).getByText("Artifact sets").parentElement as HTMLElement;
    expect(within(artifact).getByText("1")).toBeTruthy();
    // The artifact set has no repository domain, and the row says that
    // rather than leaving the column blank.
    expect(within(panel).getByText("no repository domain")).toBeTruthy();
    expect(within(panel).getAllByText("primary-nas")).toHaveLength(2);
  });
});
