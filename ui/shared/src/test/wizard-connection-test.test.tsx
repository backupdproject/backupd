/**
 * Issue #624 (H2.3): the add-backup-set wizard will not save a source
 * whose connection has not been proven.
 *
 * What the wizard already had was host IDENTITY. "Trust host" pins a
 * known_hosts line, which settles that the machine answering is the
 * machine whose fingerprint an operator compared, and settles nothing
 * else: not that the imported key authenticates, not that the account can
 * read the remote folder, not that anything the pipeline needs works. A
 * set could be saved, relied on, and only THEN tested, which is the order
 * this issue turns around.
 *
 * The S3 destination wizard has kept Save disabled until its candidate
 * check comes back ok since #594. These cases hold the source wizard to
 * the same shape, against the same mock api the rest of this suite runs
 * on, so the gate is proven by driving the screen rather than by reading
 * the component.
 *
 * Issue #864 moved where the refusal is SEEN without changing what it
 * refuses. The rail used to go anywhere, so an unproven source could be
 * carried all the way to Review and met with a disabled Save; now the
 * steps that read the test's verdict are not reachable until it has
 * passed, so the flow stops at the step that can fix it. handleSave's own
 * connectionProven guard, and the Save buttons' disabled state, are still
 * underneath — a handler reachable by any other route must not save a set
 * whose connection nothing proved — which is why these cases assert the
 * rail AND the button rather than swapping one for the other.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { BackupSetWizardPage } from "@shared/pages/BackupSetWizardPage";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import type { RetndApi } from "@shared/api/contracts";
import { resetGraphForTests } from "@shared/state/graph";
import { importAKeyAndTrustTheHost, railStep, walkToReview } from "./wizardWalk";

function renderWizard(api: RetndApi = createMockApi()) {
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

/** Presses the test on the step that owns it since #788 (the rail asks
 *  for the connection SECOND, because the write probe's answer decides
 *  what the later steps may offer). The caller is already on that step:
 *  since #864 it is as far as the rail goes. */
async function runTheConnectionTest() {
  await userEvent.click(screen.getByRole("button", { name: /^Test connection$/ }));
}

afterEach(() => {
  cleanup();
  resetGraphForTests();
});

describe("the wizard will not save an unproven connection", () => {
  it("does not let an untested source reach the Save buttons at all", async () => {
    renderWizard();
    // Every OTHER precondition the Save buttons ever had: a key
    // imported, a host key trusted. Deliberately stops short of the
    // test, so this case isolates that one precondition rather than
    // flipping several at once.
    await importAKeyAndTrustTheHost();

    // Review is where this flow's Save buttons are, and the steps
    // between here and there read the test's verdict (the write probe
    // decides whether step 7 may offer to delete from the source), so
    // neither is reachable yet.
    expect(railStep("Retention")).toBeDisabled();
    expect(railStep("Review")).toBeDisabled();
    // And the step an operator is left on says what has not been proven,
    // beside the button that proves it. A flow that stops without
    // saying why is the shape somebody reads as "this app is broken".
    expect(screen.getByText(/Nothing has been proven yet/i)).toBeInTheDocument();
  });

  it("enables Save once the connection test passes", async () => {
    renderWizard();
    await walkToReview();

    await waitFor(() => expect(screen.getByRole("button", { name: "Save & enable" })).toBeEnabled());
  });

  it("leaves the flow short of Save when the connection test comes back not ok", async () => {
    const api = createMockApi();
    vi.spyOn(api, "testCandidateConnection").mockResolvedValue({
      ok: false,
      message: "the remote path could not be listed",
      writable: false,
      checks: [
        { step: "list", outcome: "failed", category: "remote_path", detail: "/backups/postgresql/ could not be listed" }
      ]
    });
    renderWizard(api);
    await importAKeyAndTrustTheHost();

    await runTheConnectionTest();
    // Twice on purpose: once as the failing step's own detail, once as
    // the banner that says what it means for saving.
    expect((await screen.findAllByText(/could not be listed/i)).length).toBeGreaterThan(0);
    // A test that RAN is not a test that passed, and only a pass opens
    // the rest of the rail.
    expect(railStep("Review")).toBeDisabled();
    expect(railStep("Engine")).toBeDisabled();
  });

  it("checks the values on the form, not a default candidate", async () => {
    const api = createMockApi();
    const spy = vi.spyOn(api, "testCandidateConnection");
    renderWizard(api);
    await importAKeyAndTrustTheHost();

    await runTheConnectionTest();
    await waitFor(() => expect(spy).toHaveBeenCalled());
    // The known_hosts line the operator actually trusted, and the key
    // they actually imported. A check run against anything else would be
    // proving a connection this wizard is not about to save.
    const sent = spy.mock.calls[0][0];
    expect(sent.knownHostsLine).toBe("mock-host.internal ssh-ed25519 AAAAC3NzaC1lZDI1NTE5mock");
    expect(sent.sshKeyId).toMatch(/^key_mock_/);
    expect(sent.remotePath).toBe("/backups/postgresql/");
  });

  it("makes an edited host undo a passing test", async () => {
    renderWizard();
    await walkToReview();
    await waitFor(() => expect(screen.getByRole("button", { name: "Save & enable" })).toBeEnabled());

    // Going back and pointing the wizard at a different machine has to
    // take the proof away with it, for exactly the reason trusting a
    // host does: a result that outlived the values it was about would be
    // a green tick standing for a connection nobody ever made.
    await userEvent.click(railStep("Source"));
    const host = screen.getByLabelText(/host/i);
    await userEvent.clear(host);
    await userEvent.type(host, "other-machine.internal");

    // The step is un-ticked and Review — with the Save buttons on it —
    // is out of reach again until a test passes for this machine.
    expect(railStep("Connection test")).toHaveAttribute("data-complete", "false");
    expect(railStep("Review")).toBeDisabled();
  });
});
