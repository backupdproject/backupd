/**
 * Issue #864: the add-backup-set wizard's rail cannot be used to skip a
 * step, and does not claim a step was finished because the cursor went
 * past it.
 *
 * Both halves of that were real. The rail let any step be clicked at any
 * time, so an operator could go straight from "Source" to "Review" —
 * past the connection test the later steps' own controls depend on — and
 * every step behind the cursor drew a green check whether or not anything
 * had been filled in. A tick that means "you looked at this" beside seven
 * steps nobody answered is worse than no tick at all: it is the wizard
 * telling an operator their configuration is done.
 *
 * The load-bearing case is the connection test. Step 2 is the only step
 * on this rail that proves something about the world rather than
 * recording an answer, and steps 3 onwards read its verdict (the write
 * probe is what decides whether step 7 may offer to delete from the
 * source at all). So the gate has to hold in both directions: locked
 * until the test passes, and locked AGAIN the moment the form points at
 * a host the passing test was not about.
 *
 * Everything here drives the real page through the rail and the footer,
 * the way an operator does, rather than reading the component's state.
 */
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { BackupSetWizardPage } from "@shared/pages/BackupSetWizardPage";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import type { BackupdApi } from "@shared/api/contracts";
import { resetGraphForTests } from "@shared/state/graph";
import { importAKeyAndTrustTheHost, proveTheSource, railStep as rail } from "./wizardWalk";

function renderWizard(api: BackupdApi = createMockApi()) {
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

/** The footer's own way forward, which has to refuse whatever the rail
 *  refuses or it is the way around it. */
const continueButton = () => screen.getByRole("button", { name: "Continue" });

/** The steps that depend on step 2's verdict, by rail label. */
const AFTER_THE_TEST = ["Engine", "Repository", "Consistency", "Verification", "Retention", "Review"];

afterEach(() => {
  cleanup();
  resetGraphForTests();
});

describe("the rail will not skip a step it has not been given", () => {
  it("locks every step past the first unfinished one, and a click on a locked step does not move the wizard", async () => {
    renderWizard();

    // Step 1 opens with every field it needs already filled in (the
    // wizard's own example values), so it is the first UNFINISHED step
    // that stops the rail, not the first step.
    expect(rail("Source")).toHaveAttribute("data-complete", "true");
    expect(rail("Connection test")).toBeEnabled();
    expect(rail("Connection test")).toHaveAttribute("data-complete", "false");

    for (const label of AFTER_THE_TEST) {
      expect(rail(label), label + " should be locked behind the connection test").toBeDisabled();
      // Not a lookalike: a real disabled button, which is what also
      // takes it out of the tab order, so there is no keyboard route
      // around the lock either.
      expect(rail(label)).toHaveAttribute("aria-disabled", "true");
      rail(label).focus();
      expect(rail(label), label + " should not be focusable while locked").not.toHaveFocus();
    }

    // The skip this issue is about: five steps ahead, in one click.
    await userEvent.click(rail("Consistency"));
    expect(screen.getByRole("heading", { name: "Source" })).toBeTruthy();
    // And the rail still says where the operator actually is.
    expect(rail("Source")).toHaveAttribute("aria-current", "step");
    expect(rail("Consistency")).not.toHaveAttribute("aria-current");
  });

  it("refuses Continue on a step whose own answers are missing", async () => {
    renderWizard();

    expect(continueButton()).toBeEnabled();

    // Step 1 stops being finished the moment a field it needs is empty,
    // and the step after it stops being reachable with it.
    await userEvent.clear(screen.getByLabelText("Server hostname"));
    expect(continueButton()).toBeDisabled();
    expect(rail("Source")).toHaveAttribute("data-complete", "false");
    expect(rail("Connection test")).toBeDisabled();

    await userEvent.type(screen.getByLabelText("Server hostname"), "warehouse-nas.internal");
    expect(continueButton()).toBeEnabled();

    // And on the step that is genuinely unfinished, the footer refuses
    // too: nothing has been proven about this source yet.
    await userEvent.click(continueButton());
    expect(screen.getByRole("heading", { name: "Connection test" })).toBeTruthy();
    await waitFor(() => expect(screen.getByRole("button", { name: "Trust host" })).toBeEnabled());
    expect(continueButton()).toBeDisabled();
  });

  it("refuses a port that is not one", async () => {
    renderWizard();

    const port = screen.getByLabelText("SSH port");
    await userEvent.clear(port);
    await userEvent.type(port, "not-a-port");

    expect(rail("Source")).toHaveAttribute("data-complete", "false");
    expect(rail("Connection test")).toBeDisabled();
    expect(continueButton()).toBeDisabled();
  });
});

describe("the connection test is the gate the later steps wait on", () => {
  it("keeps steps 3 onwards locked until the test passes, then unlocks them and ticks step 2", async () => {
    renderWizard();

    await importAKeyAndTrustTheHost();

    // A key and a trusted host key are not the verification. Trusting a
    // fingerprint settles which machine answers and settles nothing
    // about whether the key authenticates or the folder can be read.
    expect(rail("Connection test")).toHaveAttribute("data-complete", "false");
    for (const label of AFTER_THE_TEST) {
      expect(rail(label), label + " should still be locked").toBeDisabled();
    }
    expect(continueButton()).toBeDisabled();

    await userEvent.click(screen.getByRole("button", { name: /^Test connection$/ }));
    await waitFor(() => expect(screen.getByText(/This source has been proven/i)).toBeInTheDocument());

    expect(rail("Connection test")).toHaveAttribute("data-complete", "true");
    expect(rail("Engine")).toBeEnabled();
    expect(continueButton()).toBeEnabled();
  });

  it("re-locks the later steps when the host is edited after a passing test", async () => {
    renderWizard();
    await proveTheSource();
    expect(rail("Engine")).toBeEnabled();

    // Pointing the form at a different machine makes the passing result
    // an answer about a connection this wizard is no longer about, so
    // every step that reads it goes back out of reach.
    await userEvent.click(rail("Source"));
    const host = screen.getByLabelText("Server hostname");
    await userEvent.clear(host);
    await userEvent.type(host, "other-machine.internal");

    expect(rail("Connection test")).toHaveAttribute("data-complete", "false");
    for (const label of AFTER_THE_TEST) {
      expect(rail(label), label + " should be re-locked by the edited host").toBeDisabled();
    }
  });

  it("re-locks the later steps when the port is edited after a passing test", async () => {
    renderWizard();
    await proveTheSource();

    await userEvent.click(rail("Source"));
    const port = screen.getByLabelText("SSH port");
    await userEvent.clear(port);
    await userEvent.type(port, "2222");

    expect(rail("Connection test")).toHaveAttribute("data-complete", "false");
    expect(rail("Engine")).toBeDisabled();
  });

  // The sharpest form of the gate, because it is the only re-lock that
  // NOTHING else on step 2 could explain: the host and the port are also
  // what the trusted fingerprint is pinned to, so editing either takes
  // host trust down with it. The directory is not — the host key stays
  // trusted and the key stays imported — and the step still goes back to
  // unfinished, because the test proved a folder could be listed and
  // this is a different folder.
  it("re-locks the later steps when the directory to back up is edited after a passing test", async () => {
    renderWizard();
    await proveTheSource();
    expect(rail("Engine")).toBeEnabled();

    await userEvent.click(rail("Source"));
    const folder = screen.getByLabelText("Directory to back up");
    await userEvent.clear(folder);
    await userEvent.type(folder, "/backups/elsewhere/");

    // Host trust is untouched — the machine is the same one — so this
    // can only be the connection test expiring.
    await userEvent.click(rail("Connection test"));
    expect(screen.getByRole("button", { name: "Host trusted" })).toBeDisabled();
    expect(rail("Connection test")).toHaveAttribute("data-complete", "false");
    for (const label of AFTER_THE_TEST) {
      expect(rail(label), label + " should be re-locked by the edited directory").toBeDisabled();
    }
  });
});

describe("a step is ticked for its own answers, not for the cursor going past it", () => {
  it("unlocks Review only once the acknowledgement the Retention step asks for is given", async () => {
    renderWizard();
    await proveTheSource();

    // The artifact engine's own steps are all answered by their
    // defaults, so the rail opens up to the one step that still wants
    // something: the deletion acknowledgement.
    await userEvent.click(rail("Retention"));
    expect(screen.getByRole("heading", { name: "Storage, retention and holds" })).toBeTruthy();
    expect(rail("Retention")).toHaveAttribute("data-complete", "false");
    expect(rail("Review")).toBeDisabled();
    expect(continueButton()).toBeDisabled();

    await userEvent.click(
      screen.getByRole("checkbox", { name: /remote backup will be removed only after/i })
    );

    expect(rail("Retention")).toHaveAttribute("data-complete", "true");
    expect(rail("Review")).toBeEnabled();

    await userEvent.click(rail("Review"));
    expect(screen.getByRole("heading", { name: "Review" })).toBeTruthy();
  });

  it("also accepts a read-only source instead of the acknowledgement", async () => {
    renderWizard();
    await proveTheSource();

    await userEvent.click(rail("Retention"));
    await userEvent.click(screen.getByRole("checkbox", { name: /This source is read-only/i }));

    // There is nothing to acknowledge deleting once this is declared,
    // which is the same waiver the Save button already gives it.
    expect(rail("Retention")).toHaveAttribute("data-complete", "true");
    expect(rail("Review")).toBeEnabled();
  });

  it("locks an emptied NAS destination back up again", async () => {
    renderWizard();
    await proveTheSource();

    await userEvent.click(rail("Retention"));
    await userEvent.click(
      screen.getByRole("checkbox", { name: /remote backup will be removed only after/i })
    );
    expect(rail("Review")).toBeEnabled();

    await userEvent.clear(screen.getByLabelText("NAS destination", { exact: false }));
    expect(rail("Retention")).toHaveAttribute("data-complete", "false");
    expect(rail("Review")).toBeDisabled();
  });
});

describe("an engine is never blocked by another engine's steps", () => {
  it("leaves the artifact engine's rail open through the incremental-only steps", async () => {
    renderWizard();
    await proveTheSource();

    // Artifact is the default engine (a set's engine cannot be changed
    // later, so the wizard does not pre-select the one with a
    // repository). The repository domain and the source consistency are
    // not questions about an artifact set, and a step that does not
    // apply cannot be a step that blocks.
    for (const label of ["Engine", "Repository", "Consistency", "Verification", "Retention"]) {
      expect(rail(label), label + " should be reachable for an artifact set").toBeEnabled();
    }
    expect(rail("Repository")).toHaveAttribute("data-complete", "true");
    expect(rail("Consistency")).toHaveAttribute("data-complete", "true");
  });

  it("does ask the incremental engine for the repository domain it genuinely needs", async () => {
    renderWizard();
    await proveTheSource();

    await userEvent.click(rail("Engine"));
    await userEvent.click(screen.getByRole("radio", { name: /Incremental/ }));

    // The same step that auto-satisfies for an artifact set is a real
    // question for this one, and unanswered it stops the rail.
    expect(rail("Repository")).toHaveAttribute("data-complete", "false");
    expect(rail("Consistency")).toBeDisabled();

    await userEvent.click(rail("Repository"));
    await userEvent.click(await screen.findByRole("radio", { name: /primary-nas/ }));

    expect(rail("Repository")).toHaveAttribute("data-complete", "true");
    expect(rail("Consistency")).toBeEnabled();
  });

  it("takes a newly named domain as an answer, and an empty name as no answer", async () => {
    renderWizard();
    await proveTheSource();

    await userEvent.click(rail("Engine"));
    await userEvent.click(screen.getByRole("radio", { name: /Incremental/ }));
    await userEvent.click(rail("Repository"));
    await userEvent.click(await screen.findByRole("radio", { name: /Define a new domain/ }));

    // The radio alone says "a domain of its own" without saying which.
    expect(rail("Repository")).toHaveAttribute("data-complete", "false");

    await userEvent.type(screen.getByLabelText("New domain id"), "archive-tier");
    expect(rail("Repository")).toHaveAttribute("data-complete", "true");
    expect(rail("Consistency")).toBeEnabled();
  });
});
