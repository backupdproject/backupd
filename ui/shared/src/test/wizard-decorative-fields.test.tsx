import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { BackupSetWizardPage } from "@shared/pages/BackupSetWizardPage";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { resetGraphForTests } from "@shared/state/graph";
// Since #864 a later step is only reachable once the earlier ones are
// answered. Every "this field is gone" assertion below is a queryBy that
// passes on any screen it is not on, so the cases that live past the
// connection test walk there and state which step they arrived at — a
// test that stayed on Source would go on passing while the field it is
// about came back.
import { proveTheSource, walkToReview } from "./wizardWalk";

/**
 * Issue #299 — proves each of the wizard's decorative fields is actually
 * GONE from the rendered page, not merely still there and unread. Every
 * one of these used to be a real DOM control (uncontrolled, no onChange,
 * or a fabricated static fact) with nothing on the create-backup-set path
 * ever reading it back; see fieldHelpCopy.ts's module doc and this file's
 * companion comments in BackupSetWizardPage.tsx for the per-field reasoning.
 */

function renderWizard() {
  render(
    <MemoryRouter>
      <ApiProvider api={createMockApi()}>
        <PlatformProvider bridge={genericBridge}>
          <BackupSetWizardPage readOnly={false} />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

describe("the wizard no longer renders the decorative fields #299 removed", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  it("has no Exclude patterns field on the Verification step", async () => {
    const user = userEvent.setup();
    renderWizard();
    await proveTheSource();

    await user.click(screen.getByRole("button", { name: "Verification" }));
    expect(screen.getByRole("heading", { name: "Completion and validation" })).toBeTruthy();

    expect(screen.queryByLabelText("Exclude patterns")).toBeNull();
    expect(screen.queryByText("Exclude patterns")).toBeNull();
  });

  it("has no per-set retention controls on the Retention step", async () => {
    const user = userEvent.setup();
    renderWizard();
    await proveTheSource();

    await user.click(screen.getByRole("button", { name: "Retention" }));
    expect(screen.getByRole("heading", { name: "Storage, retention and holds" })).toBeTruthy();

    // #788 gave this step the deployment's chain to REPORT — one global
    // policy (#111), read from GET /settings — which is why the
    // assertions below are about editable controls rather than about the
    // word "Retention" appearing at all. A field here would be the
    // per-set chain #299 removed, offered again.
    expect(screen.queryByLabelText("Daily")).toBeNull();
    expect(screen.queryByLabelText("Weekly")).toBeNull();
    expect(screen.queryByLabelText("Monthly")).toBeNull();
    expect(screen.queryByLabelText("Week starts")).toBeNull();
    expect(screen.queryByText("Protect newest known-good backup — never deleted by retention")).toBeNull();
  });

  it("has no Checksum verification toggle, and a Transfer verification indicator that cannot be unchecked", async () => {
    const user = userEvent.setup();
    renderWizard();
    await proveTheSource();

    await user.click(screen.getByRole("button", { name: "Verification" }));

    expect(screen.queryByText("Checksum verification")).toBeNull();
    expect(screen.queryByText("SHA-256")).toBeNull();

    const transferVerification = screen.getByRole("checkbox", { name: /Transfer verification/ });
    expect(transferVerification).toBeChecked();
    expect(transferVerification).toBeDisabled();
  });

  it("has no fabricated sample public key or authorized_keys instruction on the default Generate key-source panel", async () => {
    const user = userEvent.setup();
    renderWizard();

    await user.click(screen.getByRole("button", { name: "Connection test" }));

    // Default key source is "Generate" — confirmed by the panel below
    // rendering without clicking any radio first.
    expect(screen.queryByText(/ssh-ed25519 AAAA/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Copy public key" })).toBeNull();
    expect(screen.queryByText(/authorized_keys/)).toBeNull();
    expect(screen.getByText(/Generating a key on save isn.t available yet/)).toBeTruthy();
  });

  // #299 removed a picklist of two hardcoded key names and a fabricated
  // "Already installed on 2 other backup sets" count. #592 gave the radio
  // a REAL listing, so what this case pins is that the fabrications did
  // not come back with it: the panel has rows again, and not one of them
  // is a value this frontend made up.
  it("has no hardcoded managed-key picklist or fabricated in-use count on the Use managed key panel", async () => {
    const user = userEvent.setup();
    renderWizard();

    await user.click(screen.getByRole("button", { name: "Connection test" }));
    await user.click(screen.getByRole("radio", { name: /Use managed key/ }));

    expect(screen.queryByLabelText("Managed key")).toBeNull();
    expect(screen.queryByText(/nas-01-postgres/)).toBeNull();
    expect(screen.queryByText(/Already installed on/i)).toBeNull();
  });

  it("has no Retention summary and no SHA-256 claim on the Review step", async () => {
    renderWizard();
    await walkToReview();

    // Not a bare "Retention" check any more: #788's rail has a step by
    // that name, so matching the word would pin the rail label rather
    // than the fabricated summary #299 removed. The invented figures
    // below are what that summary actually was.
    expect(screen.queryByText("7 daily")).toBeNull();
    expect(screen.queryByText("13 weekly")).toBeNull();
    expect(screen.queryByText("12 monthly")).toBeNull();
    expect(screen.queryByText("SHA-256")).toBeNull();
  });
});
