/**
 * Driving the add-backup-set wizard's rail, for the suites whose subject
 * is somewhere in the middle of it.
 *
 * Before issue #864 a test that wanted step 7 clicked "Retention" and
 * was there, because the rail went anywhere at any time — which was the
 * defect: so could an operator, straight past the connection test every
 * step after it reads. Now a step is reachable only once the steps
 * before it are answered, so getting to the middle of the flow means
 * walking it, and four suites needed the same walk. It lives here once
 * rather than four times: a copy per file is four chances to walk it
 * differently, and the walk is the product's own order of operations.
 *
 * Everything here drives the real controls the way an operator does.
 * Nothing pokes the component's state, and nothing asserts anything
 * except that each control it needs actually became available — a
 * helper that silently failed to unlock the rail would turn every case
 * that uses it into a test of the Source step.
 */
import { expect } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

/** Synthetic fixture only — nothing resembling real key material ever
 *  appears in a test or a commit in this project. */
export const FIXTURE_PRIVATE_KEY = "FAKE-TEST-KEY-MATERIAL-not-a-real-key-0123456789";

/** A rail button, by the step label it carries as its accessible name. */
export const railStep = (label: string) => screen.getByRole("button", { name: label });

/**
 * Step 2's credentials and host key: import a key, wait for the host-key
 * probe to answer, trust what it fetched. Leaves the wizard on step 2,
 * which is as far as the rail goes until the connection test has run.
 */
export async function importAKeyAndTrustTheHost() {
  await userEvent.click(railStep("Connection test"));
  await userEvent.click(screen.getByRole("radio", { name: /Import key/ }));
  await userEvent.type(screen.getByLabelText(/private key/i), FIXTURE_PRIVATE_KEY);
  await userEvent.click(screen.getByRole("button", { name: "Import key" }));
  await screen.findByText(/key imported/i);

  // "Trust host" stays disabled until the (mocked) probe resolves — see
  // BackupSetWizardPage's probeHost.
  await waitFor(() => expect(screen.getByRole("button", { name: "Trust host" })).toBeEnabled());
  await userEvent.click(screen.getByRole("button", { name: "Trust host" }));
}

/**
 * Everything step 2 asks for, including the connection test itself
 * (issue #624), which is what unlocks steps 3 onwards. Still on step 2
 * when it returns: where the caller goes next is the caller's subject.
 */
export async function proveTheSource() {
  await importAKeyAndTrustTheHost();
  await userEvent.click(screen.getByRole("button", { name: /^Test connection$/ }));
  await waitFor(() => expect(screen.getByText(/This source has been proven/i)).toBeInTheDocument());
}

/**
 * The remote-source handling answer step 7 wants, given as the
 * acknowledgement rather than the read-only declaration. Leaves the
 * wizard on step 7.
 */
export async function acknowledgeRemoteDeletion() {
  await userEvent.click(railStep("Retention"));
  await userEvent.click(
    screen.getByRole("checkbox", { name: /remote backup will be removed only after/i })
  );
}

/** The whole flow, ending on Review, where the save controls are. */
export async function walkToReview() {
  await proveTheSource();
  await acknowledgeRemoteDeletion();
  await userEvent.click(railStep("Review"));
  await screen.findByRole("heading", { name: "Review" });
}
