/**
 * Issue #852: a source these SSH credentials cannot write to cannot be
 * deleted from, so the control that enables deleting the original after
 * backup is unavailable and says why.
 *
 * Both states are driven, in both surfaces, because only the pair proves
 * anything: a page that disabled the control unconditionally would pass
 * the read-only case, and today's page (which never looked) passes the
 * writable one.
 *
 * The fixture is the mock api's own `read-only-source` scenario, which
 * answers the connection test ok:true with writable:false — a PASSING
 * test whose one inverted answer is the whole point. Nothing here stubs
 * the wizard's own state: the screens are driven the way an operator
 * drives them.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { BackupSetWizardPage } from "@shared/pages/BackupSetWizardPage";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import type { BackupdApi } from "@shared/api/contracts";
import { BackupdError } from "@shared/api/contracts";
import { resetGraphForTests } from "@shared/state/graph";

function renderWizard(api: BackupdApi) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <PlatformProvider bridge={genericBridge}>
          <BackupSetWizardPage readOnly={false} />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

/** The wizard, walked to the point where the remote-source handling box
 *  is on screen and a connection test has answered. */
async function walkToRemoteHandling() {
  await userEvent.click(screen.getByRole("button", { name: "Connection test" }));
  await userEvent.click(screen.getByRole("radio", { name: /Import key/ }));
  await userEvent.type(screen.getByLabelText(/private key/i), "FAKE-TEST-KEY-MATERIAL-not-a-real-key-0123456789");
  await userEvent.click(screen.getByRole("button", { name: "Import key" }));
  await screen.findByText(/key imported/i);

  await waitFor(() => expect(screen.getByRole("button", { name: "Trust host" })).toBeEnabled());
  await userEvent.click(screen.getByRole("button", { name: "Trust host" }));

  // The test button is on the Connection test step since #788 reordered
  // the rail: its verdict is what steps 3 to 7 depend on, so it is asked
  // before them rather than on Review.
  await userEvent.click(screen.getByRole("button", { name: /^Test connection$/ }));
  // Wait for the verdict before leaving the step: the write probe's
  // answer is what arms or refuses the control this suite is about, and
  // reading the control before the answer landed would test nothing.
  await screen.findByText(/scratch file was created|credentials are read-only on the source/i);

  await userEvent.click(screen.getByRole("button", { name: "Retention" }));
}

/** The declaration lives on the Retention step since #788 put it beside
 *  the retention chain and the source-deletion control it governs. */
const readOnlyCheckbox = () => screen.getByRole("checkbox", { name: /This source is read-only/i });


afterEach(() => {
  cleanup();
  resetGraphForTests();
});

describe("the wizard's delete-from-source control follows what the write probe proved", () => {
  it("disables it, checked, with the reason, against a read-only source", async () => {
    renderWizard(createMockApi("read-only-source"));
    await walkToRemoteHandling();

    await waitFor(() => expect(readOnlyCheckbox()).toBeDisabled());
    // Checked as well as disabled: the state it is left in is the answer,
    // and this set will not delete from the source.
    expect(readOnlyCheckbox()).toBeChecked();
    // The reason, in the words the tooltip and the service both use.
    expect(screen.getByText(/read-only on the source/i)).toBeInTheDocument();
    expect(
      screen.getByText(/until the account is granted write permission on the source/i)
    ).toBeInTheDocument();
    // And the deletion walkthrough is gone, because there is no deletion
    // to walk through: an acknowledgement for a deletion that cannot
    // happen is a checkbox that means nothing.
    expect(screen.queryByRole("checkbox", { name: /remote backup will be removed only after/i })).toBeNull();
  });

  it("leaves it enabled and unchecked against a writable source", async () => {
    renderWizard(createMockApi());
    await walkToRemoteHandling();

    await waitFor(() => expect(readOnlyCheckbox()).toBeEnabled());
    expect(readOnlyCheckbox()).not.toBeChecked();
    expect(screen.queryByText(/read-only on the source/i)).toBeNull();
  });

  it("still saves, as a read-only set, against a read-only source", async () => {
    const api = createMockApi("read-only-source");
    const create = vi.spyOn(api, "createBackupSet");
    renderWizard(api);
    await walkToRemoteHandling();

    // The whole point of forcing rather than refusing on this screen: a
    // read-only source is savable, and Save is not gated on a deletion
    // acknowledgement nobody can give.
    await userEvent.click(screen.getByRole("button", { name: "Review" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Save & enable" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Save & enable" }));
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(create.mock.calls[0][0].readOnly).toBe(true);
  });
});

function renderDetail(api: BackupdApi, source: string, set: string) {
  return render(
    <MemoryRouter initialEntries={["/sets/" + source + "/" + set]}>
      <ApiProvider api={api}>
        <PlatformProvider bridge={genericBridge}>
          <Routes>
            <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={false} />} />
          </Routes>
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

describe("the per-set form's delete-from-source control follows the same answer", () => {
  it("disables 'Allow remote deletion again' once a test proves the source read-only", async () => {
    const api = createMockApi("read-only-source");
    const target = (await createMockApi().listSets())[0];
    // The control this is about only exists on a set that IS read-only:
    // it is the one that would take it back out of read-only.
    vi.spyOn(api, "getSet").mockResolvedValue({ ...target, readOnly: true });

    renderDetail(api, target.source, target.set);
    const allow = await screen.findByRole("button", { name: "Allow remote deletion again" });
    // Enabled before anything was proven: "not checked yet" is not
    // "read-only", and a page that disabled it up front would hide a
    // control the operator may well be entitled to use.
    expect(allow).toBeEnabled();

    await userEvent.click(screen.getAllByRole("button", { name: /^Test connection$/ })[0]);

    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Allow remote deletion again" })).toBeDisabled()
    );
    expect(screen.getByText(/read-only on the source/i)).toBeInTheDocument();
  });

  it("leaves it enabled when the test proves the source writable", async () => {
    const api = createMockApi();
    const target = (await createMockApi().listSets())[0];
    vi.spyOn(api, "getSet").mockResolvedValue({ ...target, readOnly: true });

    renderDetail(api, target.source, target.set);
    await screen.findByRole("button", { name: "Allow remote deletion again" });
    await userEvent.click(screen.getAllByRole("button", { name: /^Test connection$/ })[0]);

    await waitFor(() => expect(screen.queryByText(/read-only on the source/i)).toBeNull());
    expect(screen.getByRole("button", { name: "Allow remote deletion again" })).toBeEnabled();
  });

  it("explains the server's refusal even when no test was run first", async () => {
    const api = createMockApi();
    const target = (await createMockApi().listSets())[0];
    vi.spyOn(api, "getSet").mockResolvedValue({ ...target, readOnly: true });
    // What the API answers for this exact request: 409
    // BACKUP_SET_SOURCE_NOT_WRITABLE. A page that swallowed it would
    // leave a button that does nothing, which is what this asserts is
    // not shipped.
    vi.spyOn(api, "setReadOnly").mockRejectedValue(
      new BackupdError({
        code: "BACKUP_SET_SOURCE_NOT_WRITABLE",
        message: "these credentials cannot write to this source, so deleting from it cannot be enabled"
      })
    );

    renderDetail(api, target.source, target.set);
    await userEvent.click(await screen.findByRole("button", { name: "Allow remote deletion again" }));

    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Allow remote deletion again" })).toBeDisabled()
    );
    expect(screen.getByText(/read-only on the source/i)).toBeInTheDocument();
  });
});
