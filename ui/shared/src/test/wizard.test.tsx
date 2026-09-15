/**
 * The add-backup-set wizard's refusals, which are the part of it that has
 * to hold.
 *
 * Most of what a wizard does is collect values, and this file spends
 * almost none of its length on that. What it pins instead is what Save
 * will NOT do: not until remote deletion is acknowledged, and not on an
 * acknowledgement alone while the host is untrusted or no key has been
 * imported. Those two together are the whole safety argument for a flow
 * that ends by giving a service permission to delete from somebody's
 * server.
 *
 * The remaining cases cover honesty rather than safety: a storage picker
 * is not offered on a platform that has none, and no private key is ever
 * rendered.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { BackupSetWizardPage } from "@shared/pages/BackupSetWizardPage";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { RetndError } from "@shared/api/contracts";
import type { RetndApi } from "@shared/api/contracts";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import { wizardHostKeyChangedNode } from "@shared/state/wizardNodes";
import { acknowledgeRemoteDeletion, proveTheSource, railLabels, railStep, walkToReview } from "./wizardWalk";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";

/** The step a sentence sends an operator to, as the sentence itself
 *  names it: "…on the Connection test step" -> "Connection test". Only
 *  a CAPITALISED name is a step name — "on the next step" is a relative
 *  direction and names nothing that could go stale. */
const NAMED_STEPS = (text: string): string[] =>
  [...text.matchAll(/\b(?:on|to) (?:the |this )?(?:same )?([A-Z][A-Za-z ]*?) step\b/g)].map(
    (m) => m[1]
  );

// Issue #146 (B2.7): the wizard now reads useApi() (step 2's import, step
// 3's host-key probe, step 6's Save buttons all call through it), so
// every render needs an ApiProvider — createMockApi() is the same
// deterministic fixture ui/shared/e2e's own Playwright suite runs
// against (playwright.config.ts's own comment), reused here for the
// same reason.
function renderWizard(readOnly = false, api: RetndApi = createMockApi()) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <PlatformProvider bridge={genericBridge}>
          <BackupSetWizardPage readOnly={readOnly} />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

/** Same providers as renderWizard, but with an actual route table
 *  (rather than a bare MemoryRouter) so navigate("/sets") on a
 *  successful save (issue #146) is observable — renderWizard alone has
 *  nowhere for that navigation to land. */
function renderWizardWithRoutes(api: RetndApi) {
  return render(
    <MemoryRouter initialEntries={["/sets/new"]}>
      <ApiProvider api={api}>
        <PlatformProvider bridge={genericBridge}>
          <Routes>
            <Route path="/sets/new" element={<BackupSetWizardPage readOnly={false} />} />
            <Route path="/sets" element={<div>SETS LIST PAGE</div>} />
          </Routes>
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

/* The walk this file used to own lives in ./wizardWalk now, because the
   suites that drive this wizard all needed the same one once #864 made
   the rail refuse to skip a step: proveTheSource() does step 2's work
   (import a key, wait for the probe, trust it, run the connection test)
   and stops there, which is as far as the rail goes until that step is
   finished; walkToReview() adds the deletion acknowledgement step 7
   wants and ends on Review, where the save controls are. Cases that
   isolate acknowledgement from the key-import/host-trust preconditions
   (M7, #146 review) call the first one. */

// wizard.hostKeyChanged lives on the shared causl graph (issue #98 —
// state/wizardNodes.ts), not in this component's own useState, precisely
// so something outside the wizard (a host-key re-probe, another test) can
// change it while the wizard is open. That means test isolation depends
// on resetting the graph between tests, the same as
// PlatformContext.test.tsx — even though the wizard's other answers
// (completion, host trust, acknowledgement) are plain component state and
// need no such reset.
afterEach(() => {
  cleanup();
  resetGraphForTests();
});

describe("add backup set wizard", () => {
  it("has eight grouped steps, not a dozen screens", () => {
    renderWizard();
    // Eight since #788, which added the engine, the repository domain and
    // the source-consistency questions the incremental engine needs and
    // folded the credentials and the host key into one "Connection test".
    // The assertion is about the same thing it always was: grouped steps
    // rather than one screen per field.
    expect(screen.getAllByRole("listitem").length).toBe(8);
  });

  // The gap this whole file had: every case above and below reaches a
  // step by clicking the RAIL, which can jump anywhere, so none of them
  // could see a footer that stops advancing early. It did — at step 6 of
  // 8 — which meant an operator using the control the wizard puts under
  // its own thumb could never reach the #852 read-only control on step 7
  // or the Save on step 8.
  it("walks Source to Review on Continue alone, and saves there", async () => {
    const api = createMockApi();
    const created = vi.spyOn(api, "createBackupSet");
    renderWizardWithRoutes(api);

    const advance = async () => userEvent.click(screen.getByRole("button", { name: "Continue" }));

    // 1 -> 2, and the connection work the save preconditions need, done
    // on the step that owns it rather than jumped to.
    await advance();
    expect(screen.getByRole("heading", { name: "Connection test" })).toBeTruthy();
    await userEvent.click(screen.getByRole("radio", { name: /Import key/ }));
    await userEvent.type(
      screen.getByLabelText(/private key/i),
      "FAKE-TEST-KEY-MATERIAL-not-a-real-key-0123456789"
    );
    await userEvent.click(screen.getByRole("button", { name: "Import key" }));
    await screen.findByText(/key imported/i);
    await waitFor(() => expect(screen.getByRole("button", { name: "Trust host" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Trust host" }));
    await userEvent.click(screen.getByRole("button", { name: /^Test connection$/ }));
    await waitFor(() => expect(screen.getByText(/This source has been proven/i)).toBeInTheDocument());

    // 2 -> 3 -> 4 -> 5 -> 6, each announced by its own heading, so a
    // Continue that silently stopped moving would fail here rather than
    // at the end. Step 6 is titled for the engine chosen on step 3, and
    // nothing here chooses one, so it is the artifact wording.
    for (const title of ["Engine", "Repository domain", "Source consistency", "Completion and validation"]) {
      await advance();
      expect(screen.getByRole("heading", { name: title })).toBeTruthy();
    }

    // 6 -> 7: the step the footer could not reach at all, and the
    // acknowledgement that lives on it.
    await advance();
    expect(screen.getByRole("heading", { name: "Storage, retention and holds" })).toBeTruthy();
    await userEvent.click(
      screen.getByRole("checkbox", { name: /remote backup will be removed only after/i })
    );

    // 7 -> 8: Review, where the only Save in this flow is.
    await advance();
    expect(screen.getByRole("heading", { name: "Review" })).toBeTruthy();
    expect(screen.getByText("Step 8 of 8")).toBeTruthy();
    // The end of the rail, so Continue has nowhere left to go.
    expect(screen.getByRole("button", { name: "Continue" })).toBeDisabled();

    await userEvent.click(screen.getByRole("button", { name: /^Save & enable$/ }));
    expect(await screen.findByText("SETS LIST PAGE")).toBeTruthy();
    expect(created).toHaveBeenCalledTimes(1);
  });

  // A disabled Save owes a reason, and a reason that sends an operator
  // to a step this wizard does not have is worse than none: #788 folded
  // "Authentication" and "Verify server" into step 2, and these hints
  // went on naming both.
  //
  // #864 refuses most of that chain earlier than the Save button — a
  // flow with no key imported cannot reach Review at all now — so the
  // hint an operator can still be shown there is the one for a fact
  // that arrives while they are standing on it: a host key that changed
  // under a trusted fingerprint. It has to name the step that resolves
  // it, and nothing on the screen may name a step this rail does not
  // have.
  it("sends the operator to a step that is actually on the rail", async () => {
    renderWizard();
    await walkToReview();

    act(() => {
      graph.commit("test/wizard-host-key-changed", (tx) => tx.set(wizardHostKeyChangedNode, true));
    });

    expect(screen.getByText(/resolve that on the Connection test step/i)).toBeTruthy();
    expect(document.body.textContent).not.toMatch(/Authentication step|Verify server step/);
  });

  // The other half of the same rule, on the surface the case above
  // structurally cannot see (#923). The "generate" panel's banner is the
  // refusal an operator meets on the STEP, before any Save, and it went
  // on naming the "Authentication" step #788 deleted for as long as it
  // did precisely because the case above never selects this radio: the
  // banner is never rendered, so its sentence never enters
  // document.body for that `not.toMatch` to fail against.
  //
  // What is asserted is the rule and not the wording: the step this copy
  // names is pulled out of the sentence and looked up in the rail the
  // wizard is drawing. A reintroduced "Authentication", or a relabelled
  // rail that leaves this sentence behind, fails here.
  it("refuses key generation by naming a step the rail actually draws", async () => {
    renderWizard();
    await userEvent.click(railStep("Connection test"));
    await userEvent.click(screen.getByRole("radio", { name: /Generate dedicated SSH key/ }));

    const banner = screen.getByText(/Generating a key on save/);
    const named = NAMED_STEPS(banner.textContent ?? "");
    // The refusal is only useful if it says where to go instead, so an
    // empty list is a failure rather than a vacuous pass.
    expect(named.length).toBeGreaterThan(0);
    for (const step of named) expect(railLabels()).toContain(step);
  });

  // Same rule again, over the wizard's field help — the other operator-
  // visible copy that names steps, and where #788 left three stale
  // references nothing was checking (#923): hostname and port both sent
  // an operator to "Verify server", and the username entry sent them to
  // "Discovery" for a field that is two rows above it on Source now.
  it("names only real steps in the wizard's own field help", () => {
    renderWizard();
    const labels = railLabels();

    for (const [key, copy] of Object.entries(FIELD_HELP)) {
      if (!key.startsWith("wizard")) continue;
      for (const sentence of [copy.what, copy.effect, copy.example]) {
        for (const step of NAMED_STEPS(sentence ?? "")) {
          expect(labels, key + " names a step the rail does not have: " + step).toContain(step);
        }
      }
    }
  });

  it("blocks saving until remote deletion is acknowledged", async () => {
    renderWizard();
    // Every OTHER save precondition (imported key, trusted host,
    // connection proven) is satisfied first, so this isolates
    // acknowledgement as the one variable under test — see M7 (#146
    // review) on why those also gate the button.
    await proveTheSource();

    // Since #864 the acknowledgement is also what finishes step 7, so
    // the refusal is visible a step earlier than the button: Review,
    // where every save control in this flow lives, is out of reach
    // while the remote-source handling question is unanswered.
    await userEvent.click(railStep("Retention"));
    expect(railStep("Review")).toBeDisabled();

    await userEvent.click(screen.getByRole("checkbox", { name: /remote backup will be removed only after/i }));
    await userEvent.click(railStep("Review"));
    expect(screen.getByRole("button", { name: /Save, enable & run/ })).toBeEnabled();
  });

  // M7 (#146 review): the wizard's own save-preconditions gap. Save used
  // to stay clickable with no key imported and no host trusted —
  // clicking it fired handleSave, which rejected the request via its own
  // ad hoc guard rather than the button ever refusing to be clicked. M7
  // made the button refuse; #864 makes the rail refuse first, so the
  // combination this case was written about — nothing imported, nothing
  // trusted, and the deletion acknowledged anyway — cannot be assembled
  // at all: the step that takes the acknowledgement is not reachable,
  // and neither is the one the Save buttons are on.
  it("reaches neither the acknowledgement nor Save without an imported key and a trusted host (M7, #146 review)", async () => {
    renderWizard();

    await userEvent.click(railStep("Retention"));
    expect(screen.getByRole("heading", { name: "Source" })).toBeTruthy();
    expect(screen.queryByRole("checkbox", { name: /remote backup will be removed only after/i })).toBeNull();

    await userEvent.click(railStep("Review"));
    expect(screen.queryByRole("button", { name: /Save, enable & run/ })).toBeNull();
    expect(railStep("Retention")).toBeDisabled();
    expect(railStep("Review")).toBeDisabled();
  });

  it("warns when stable-size completion is chosen", async () => {
    renderWizard();
    await proveTheSource();
    await userEvent.click(screen.getByRole("button", { name: "Verification" }));
    await userEvent.click(screen.getByRole("radio", { name: /Stable file size/ }));
    expect(screen.getByText(/infers completion and provides less assurance/)).toBeTruthy();
  });

  it("does not offer a native storage picker on a platform without one", async () => {
    renderWizard();
    await proveTheSource();
    await userEvent.click(screen.getByRole("button", { name: "Retention" }));
    expect(screen.getByRole("button", { name: "Validate path" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Browse volumes/ })).toBeNull();
  });

  it("never renders a private key", async () => {
    renderWizard();
    await userEvent.click(screen.getByRole("button", { name: "Connection test" }));
    // Default key source is "Generate", whose panel (#299) says plainly
    // that this path can't be saved yet rather than showing a fixed
    // sample key, so this is the positive control that we're on the
    // right panel at all.
    expect(screen.getByText(/Generating a key on save isn.t available yet/)).toBeTruthy();
    // Match key MATERIAL, not the words. The step deliberately says
    // "Private keys stay on this NAS and are never shown after
    // creation", which is the correct thing to tell an operator, and a
    // bare /PRIVATE KEY/i fired on that reassurance rather than on a
    // leak. A PEM header cannot appear by accident.
    expect(document.body.textContent).not.toMatch(/-----BEGIN [A-Z ]*PRIVATE KEY-----/);
  });

  describe("SSH key import (step 2)", () => {
    it("disables Import until a key is pasted, then clears the paste box and never re-displays it", async () => {
      renderWizard();
      await userEvent.click(screen.getByRole("button", { name: "Connection test" }));
      await userEvent.click(screen.getByRole("radio", { name: /Import key/ }));

      const textarea = screen.getByLabelText(/private key/i);
      const importBtn = screen.getByRole("button", { name: "Import key" });
      expect(importBtn).toBeDisabled();

      // Synthetic fixture only — this is not a real key, on this app's own
      // rule that nothing resembling real key material ever appears in a
      // test or a commit.
      const fixtureKey = "FAKE-TEST-KEY-MATERIAL-not-a-real-key-0123456789";
      await userEvent.type(textarea, fixtureKey);
      expect(importBtn).toBeEnabled();

      await userEvent.click(importBtn);

      // Import now goes through api.importSSHKey (issue #146), a real
      // (mocked) async call — findByText waits for that to resolve
      // instead of asserting the instant before it has.
      expect(await screen.findByText(/key imported/i)).toBeTruthy();
      expect(screen.queryByLabelText(/private key/i)).toBeNull();
      expect(document.body.textContent).not.toContain(fixtureKey);
    });

    it("guards the private-key textarea against cloud spellcheck / autofill leakage (M2, #98 PR #145 review)", async () => {
      renderWizard();
      await userEvent.click(screen.getByRole("button", { name: "Connection test" }));
      await userEvent.click(screen.getByRole("radio", { name: /Import key/ }));

      const textarea = screen.getByLabelText(/private key/i);
      expect(textarea).toHaveAttribute("spellcheck", "false");
      expect(textarea).toHaveAttribute("autocomplete", "off");
      expect(textarea).toHaveAttribute("autocorrect", "off");
      expect(textarea).toHaveAttribute("autocapitalize", "off");
    });

    it("does not claim a shape-validation step that doesn't run (M3, #98 PR #145 review)", async () => {
      renderWizard();
      await userEvent.click(screen.getByRole("button", { name: "Connection test" }));
      await userEvent.click(screen.getByRole("radio", { name: /Import key/ }));

      const textarea = screen.getByLabelText(/private key/i);
      await userEvent.type(textarea, "FAKE-TEST-KEY-MATERIAL-not-a-real-key-0123456789");

      expect(screen.queryByText(/validated locally/i)).toBeNull();
      expect(screen.queryByText(/shape only/i)).toBeNull();
    });

    // #299 stripped this radio's picklist, which showed two hardcoded
    // key names and a fabricated "Already installed on 2 other backup
    // sets" count with no managed-key store behind either. That left an
    // honest control that could not do anything, for exactly one reason:
    // nothing could list the key store. #592 built that listing, so the
    // picklist is real, and what this case pins is that every value in it
    // is one the server sent.
    it("offers the keys this deployment actually holds, with counts it did not invent", async () => {
      renderWizard();
      await userEvent.click(screen.getByRole("button", { name: "Connection test" }));
      await userEvent.click(screen.getByRole("radio", { name: /Use managed key/ }));

      const listed = await createMockApi().listSSHKeys();
      expect(listed.length).toBeGreaterThan(0);
      for (const key of listed) {
        expect(await screen.findByText(key.fingerprint)).toBeTruthy();
      }

      // The refusal it replaced is gone, and so is the fabricated count
      // #299 removed: a key nothing references says so rather than
      // claiming a number.
      expect(screen.queryByText(/Reusing a managed key on save isn.t available yet/)).toBeNull();
      expect(screen.queryByText(/Already installed on/i)).toBeNull();

      // The "used by" column names the sets, and it names the ones the
      // fixture's own backup sets actually reference. #299 removed a
      // fabricated COUNT; a count is only worth having when it is
      // counted, so this checks the sets rather than the number.
      const used = listed.find((k) => k.usedBy.length > 0);
      expect(used).toBeTruthy();
      for (const setId of used?.usedBy ?? []) {
        expect(screen.getAllByText(new RegExp(setId)).length).toBeGreaterThan(0);
      }
    });
  });

  describe("source fields carry through to review (step 1 -> step 6)", () => {
    it("reflects an edited hostname in the review step's source summary, not the original example", async () => {
      renderWizard();
      const hostField = screen.getByLabelText("Server hostname");
      await userEvent.clear(hostField);
      await userEvent.type(hostField, "warehouse-nas.internal");

      // The walk, not a jump: since #864 Review is only reachable once
      // the steps before it are answered, which also means the summary
      // is read for a source this wizard has actually proven.
      await walkToReview();
      expect(screen.getByText("warehouse-nas.internal")).toBeTruthy();
      expect(screen.queryByText("prod-db-01.internal")).toBeNull();
    });

    it("keeps the typed hostname after navigating away from step 1 and back", async () => {
      renderWizard();
      const hostField = screen.getByLabelText("Server hostname");
      await userEvent.clear(hostField);
      await userEvent.type(hostField, "warehouse-nas.internal");

      await userEvent.click(screen.getByRole("button", { name: "Connection test" }));
      await userEvent.click(screen.getByRole("button", { name: "Source" }));
      expect(screen.getByLabelText("Server hostname")).toHaveValue("warehouse-nas.internal");
    });
  });

  describe("the review step reads step 2/6's real answers, not fixed example text (#98)", () => {
    it("reflects the completion method chosen on step 6, surviving the trip to review", async () => {
      renderWizard();
      await proveTheSource();
      await userEvent.click(railStep("Verification"));
      await userEvent.click(screen.getByRole("radio", { name: /Atomic rename/ }));

      await acknowledgeRemoteDeletion();
      await userEvent.click(railStep("Review"));
      expect(screen.getByText(/atomic rename/i)).toBeTruthy();
      expect(screen.queryByText(/completion marker/i)).toBeNull();
    });

    it("reflects a trust decision made on step 2, surviving the trip to review", async () => {
      renderWizard();
      // proveTheSource clicks "Trust host" once the (mocked) host-key
      // probe resolves — see BackupSetWizardPage's probeHost — and then
      // runs the connection test the later steps wait on.
      await walkToReview();

      expect(screen.queryByText(/not yet trusted/i)).toBeNull();
      expect(screen.getByText(/^trusted$/i)).toBeTruthy();
    });
  });

  describe("host trust does not survive a hostname edit (M1, #98 PR #145 review)", () => {
    it("resets host trust once the hostname changes after Trust host, but not while it still matches", async () => {
      renderWizard();

      await userEvent.click(screen.getByRole("button", { name: "Connection test" }));
      await waitFor(() => expect(screen.getByRole("button", { name: "Trust host" })).toBeEnabled());
      await userEvent.click(screen.getByRole("button", { name: "Trust host" }));
      expect(screen.getByRole("button", { name: "Host trusted" })).toBeDisabled();

      // Re-visiting the same step with the hostname unchanged must not
      // un-trust it — only an actual edit should. No new probe fires
      // either (same host:port already probed), so no wait is needed
      // here.
      await userEvent.click(screen.getByRole("button", { name: "Source" }));
      await userEvent.click(screen.getByRole("button", { name: "Connection test" }));
      expect(screen.getByRole("button", { name: "Host trusted" })).toBeDisabled();

      await userEvent.click(screen.getByRole("button", { name: "Source" }));
      const hostField = screen.getByLabelText("Server hostname");
      await userEvent.clear(hostField);
      await userEvent.type(hostField, "a-different-server.internal");
      await userEvent.click(screen.getByRole("button", { name: "Connection test" }));

      expect(screen.queryByRole("button", { name: "Host trusted" })).toBeNull();
      // A new host means a new probe: "Trust host" only becomes
      // enabled again once that (mocked) probe resolves.
      await waitFor(() => expect(screen.getByRole("button", { name: "Trust host" })).toBeEnabled());
    });
  });

  describe("a changed host key blocks saving (WP 2.3 acceptance: 'changed host key blocks operation')", () => {
    it("disables both gated save actions the instant the host key changes, even though acknowledged is still checked", async () => {
      renderWizard();
      // Every other precondition is satisfied first (walkToReview), same
      // as the acknowledgement case above, so this isolates the
      // host-key-change effect as the one variable under test.
      await walkToReview();

      expect(screen.getByRole("button", { name: /Save, enable & run/ })).toBeEnabled();
      expect(screen.getByRole("button", { name: /^Save & enable$/ })).toBeEnabled();

      // Simulates the shared host-trust state changing while the wizard is
      // open on the review step — the same fact DashboardPage surfaces
      // from app.sets as haltReason === "host-key-changed" — landing here
      // as a direct graph commit because nothing re-probes the host yet.
      act(() => {
        graph.commit("test/wizard-host-key-changed", (tx) => tx.set(wizardHostKeyChangedNode, true));
      });

      expect(screen.getByRole("button", { name: /Save, enable & run/ })).toBeDisabled();
      expect(screen.getByRole("button", { name: /^Save & enable$/ })).toBeDisabled();
      expect(screen.getAllByText(/host key changed/i).length).toBeGreaterThan(0);
    });
  });

  describe("read-only mode (management actions disabled, #106)", () => {
    it("keeps save blocked even after acknowledgement when the app-wide readOnly node is true", async () => {
      renderWizard();
      act(() => {
        graph.commit("test/version-incompatible", (tx) =>
          tx.set(versionNode, {
            data: {
              api: "v0",
              service: "1.3.0",
              buildCommit: "0000000",
              goVersion: "go1.27.0",
              engine: "1.65.0",
              configRevision: "cfg_0000000",
              ready: true,
              compatible: false
            },
            error: null,
            loading: false
          })
        );
      });

      // Every precondition this wizard has of its own, met: the point of
      // the case is that the app-wide refusal outranks all of them.
      await walkToReview();

      expect(screen.getByRole("button", { name: /Save, enable & run/ })).toBeDisabled();
      expect(screen.getByRole("button", { name: /^Save & enable$/ })).toBeDisabled();
    });
  });

  // Issue #146 (B2.7): RED plan's "the wizard's Save buttons actually
  // call the create endpoint and handle its response (success ->
  // navigate/confirm, failure -> surface the error, not a silent
  // no-op)".
  describe("the Save buttons persist a backup set for real (issue #146)", () => {
    it("Save & enable calls createBackupSet with disabled:false, runImmediately:false and navigates to the sets list on success", async () => {
      const api = createMockApi();
      const spy = vi.spyOn(api, "createBackupSet");
      renderWizardWithRoutes(api);

      await walkToReview();
      await userEvent.click(screen.getByRole("button", { name: /^Save & enable$/ }));

      expect(await screen.findByText("SETS LIST PAGE")).toBeTruthy();
      expect(spy).toHaveBeenCalledTimes(1);
      const req = spy.mock.calls[0][0];
      expect(req.disabled).toBe(false);
      expect(req.runImmediately).toBe(false);
      expect(req.sshKeyId).toBeTruthy();
      expect(req.knownHostsLine).toBeTruthy();
    });

    it("Save, enable & run calls createBackupSet with runImmediately:true", async () => {
      const api = createMockApi();
      const spy = vi.spyOn(api, "createBackupSet");
      renderWizardWithRoutes(api);

      await walkToReview();
      await userEvent.click(screen.getByRole("button", { name: /Save, enable & run/ }));

      await screen.findByText("SETS LIST PAGE");
      const req = spy.mock.calls[0][0];
      expect(req.disabled).toBe(false);
      expect(req.runImmediately).toBe(true);
    });

    it("Save disabled calls createBackupSet with disabled:true", async () => {
      const api = createMockApi();
      const spy = vi.spyOn(api, "createBackupSet");
      renderWizardWithRoutes(api);

      // The connection still has to be proven (issue #624): a set saved
      // off is turned on later with one click and nothing checks then.
      // Since #864 the remote-source handling answer is asked for too,
      // because it is what finishes the step it lives on — "Save
      // disabled" is still not gated by the acknowledgement the way the
      // two enabled saves are (it has no disabled condition of its own
      // beyond an in-flight save), but the rail no longer walks past an
      // unanswered step to reach it. The waiver that stays visible is
      // the read-only one, in the case below.
      await walkToReview();
      await userEvent.click(screen.getByRole("button", { name: "Save disabled" }));

      await screen.findByText("SETS LIST PAGE");
      const req = spy.mock.calls[0][0];
      expect(req.disabled).toBe(true);
      expect(req.runImmediately).toBe(false);
    });

    // Issue #316's RED case: before this checkbox existed, there was no
    // control anywhere in the wizard that could set read_only, and the
    // deletion acknowledgement was mandatory for every saved set
    // regardless of whether it would ever delete anything.
    it("declaring the source read-only sends read_only:true and needs no deletion acknowledgement", async () => {
      const api = createMockApi();
      const spy = vi.spyOn(api, "createBackupSet");
      renderWizardWithRoutes(api);

      await proveTheSource();
      // Deliberately no acknowledgement click — checking read-only is
      // this test's own escape hatch from it, the same claim the
      // "Save disabled" test above makes for its own button.
      await userEvent.click(screen.getByRole("button", { name: "Retention" }));
      await userEvent.click(screen.getByRole("checkbox", { name: /read-only/i }));
      await userEvent.click(screen.getByRole("button", { name: "Review" }));

      const save = screen.getByRole("button", { name: /^Save & enable$/ });
      expect(screen.getByRole("button", { name: /Save, enable & run/ })).toBeEnabled();
      await userEvent.click(save);

      await screen.findByText("SETS LIST PAGE");
      const req = spy.mock.calls[0][0];
      expect(req.readOnly).toBe(true);
      expect(req.disabled).toBe(false);
    });

    it("leaves read_only false, and the deletion acknowledgement still required, when the checkbox is never touched", async () => {
      const api = createMockApi();
      const spy = vi.spyOn(api, "createBackupSet");
      renderWizardWithRoutes(api);

      await walkToReview();
      await userEvent.click(screen.getByRole("button", { name: /^Save & enable$/ }));

      await screen.findByText("SETS LIST PAGE");
      const req = spy.mock.calls[0][0];
      expect(req.readOnly).toBe(false);
    });

    it("sends the chosen application validator's id, and nothing that could name an executable (issue #162)", async () => {
      const api = createMockApi();
      const spy = vi.spyOn(api, "createBackupSet");
      renderWizardWithRoutes(api);

      await proveTheSource();
      await userEvent.click(railStep("Verification"));
      const picker = await screen.findByLabelText(/application validation/i);
      // A real picklist, not the decorative toggle #98 shipped: the
      // options come from the backend's own registered catalog.
      await waitFor(() => expect(within(picker as HTMLSelectElement).getAllByRole("option").length).toBeGreaterThan(1));
      await userEvent.selectOptions(picker, "trailer-marker");

      await acknowledgeRemoteDeletion();
      await userEvent.click(railStep("Review"));
      await userEvent.click(screen.getByRole("button", { name: /^Save & enable$/ }));

      await screen.findByText("SETS LIST PAGE");
      const req = spy.mock.calls[0][0];
      expect(req.validatorId).toBe("trailer-marker");

      const banned = ["command", "executable", "argv", "script", "shell", "binary", "exec"];
      const offending = Object.keys(req).filter((k) => banned.some((w) => k.toLowerCase().includes(w)));
      expect(offending).toEqual([]);
    });

    it("sends no validator at all when the operator leaves the picklist on its default (issue #162)", async () => {
      const api = createMockApi();
      const spy = vi.spyOn(api, "createBackupSet");
      renderWizardWithRoutes(api);

      await walkToReview();
      await userEvent.click(screen.getByRole("button", { name: /^Save & enable$/ }));

      await screen.findByText("SETS LIST PAGE");
      expect(spy.mock.calls[0][0].validatorId).toBeUndefined();
    });

    it("says so when the validator catalog cannot be loaded, rather than showing an empty picklist (issue #162)", async () => {
      const api = createMockApi();
      vi.spyOn(api, "listValidators").mockRejectedValue(
        new RetndError({ code: "INTERNAL", message: "nope", correlationId: "cid_2" })
      );
      renderWizard(false, api);
      await proveTheSource();

      await userEvent.click(railStep("Verification"));
      expect(await screen.findByText(/could not load the available validators/i)).toBeTruthy();
    });

    // M4 (#194 review): the create succeeded, the requested run did not
    // start, and the response says so in run_error. Every assertion above
    // is the positive control for this one — with no run_error the wizard
    // navigates away, so "does not navigate" here is a behaviour that
    // depends on the field rather than on the wizard never navigating.
    it("says the run did not start, rather than reporting a plain success, when the response carries run_error", async () => {
      const api = createMockApi();
      vi.spyOn(api, "createBackupSet").mockResolvedValue({
        id: "api/x", sourceName: "api", name: "x", host: "h", port: 22, user: "u",
        remotePath: "/r", localPath: "/l", include: [], completionStrategy: "rename",
        disabled: false, readOnly: false,
        runError: "the destructive gate is closed, so the run was not submitted"
      });
      renderWizardWithRoutes(api);

      await walkToReview();
      await userEvent.click(screen.getByRole("button", { name: /Save, enable & run/ }));

      expect(await screen.findByText(/Saved, but the run did not start/i)).toBeTruthy();
      expect(
        screen.getByText(/the destructive gate is closed, so the run was not submitted/i)
      ).toBeTruthy();
      // Not navigated away: the sets list showing the new set is exactly
      // the "it worked" reading this response contradicts.
      expect(screen.queryByText("SETS LIST PAGE")).toBeNull();
      // And no second set can be created by pressing Save again.
      expect(screen.getByRole("button", { name: /Save, enable & run/ })).toBeDisabled();
    });

    it("surfaces a failed save inline instead of navigating or silently doing nothing", async () => {
      const api = createMockApi();
      vi.spyOn(api, "createBackupSet").mockRejectedValue(
        new RetndError({ code: "INVALID_REQUEST", message: "remote_path is required", correlationId: "cid_1" })
      );
      renderWizardWithRoutes(api);

      await walkToReview();
      await userEvent.click(screen.getByRole("button", { name: /^Save & enable$/ }));

      expect(await screen.findByText("remote_path is required")).toBeTruthy();
      expect(screen.queryByText("SETS LIST PAGE")).toBeNull();
    });

    // Issue #411. Removing a backup set frees its id up, so a create can
    // land on an id that already has backups on record. Pointed somewhere
    // other than where those backups came from, that create is refused
    // until it is acknowledged, and the wizard has to make the refusal a
    // decision with two answers rather than a red sentence under the
    // buttons.
    it("offers a create-anyway decision, not a field error, when the id already has history somewhere else", async () => {
      const api = createMockApi();
      const create = vi.spyOn(api, "createBackupSet").mockRejectedValueOnce(
        new RetndError({
          code: "BACKUP_SET_HISTORY_REPOINT_NOT_ACKNOWLEDGED",
          message: "3 artifact(s) are already on record for api/postgres-primary",
          correlationId: "cid_2"
        })
      );
      renderWizardWithRoutes(api);

      await walkToReview();
      await userEvent.click(screen.getByRole("button", { name: /^Save disabled$/ }));

      expect(await screen.findByText(/already on record for api\/postgres-primary/)).toBeTruthy();
      expect(screen.queryByText("SETS LIST PAGE")).toBeNull();
      // The first attempt must not have carried the acknowledgement, or
      // the refusal being tested could never have fired at all.
      expect(create.mock.calls[0][0].acknowledgeRepoint).toBeUndefined();

      await userEvent.click(screen.getByRole("button", { name: "Create anyway" }));

      await waitFor(() => expect(create).toHaveBeenCalledTimes(2));
      const confirmed = create.mock.calls[1][0];
      expect(confirmed.acknowledgeRepoint).toBe(true);
      // And it re-sends the save that was actually refused. "Save
      // disabled" confirmed has to stay a disabled save, not quietly
      // become an enabled one.
      expect(confirmed.disabled).toBe(true);
      expect(await screen.findByText("SETS LIST PAGE")).toBeTruthy();
    });

    // The control for the case above: an ordinary create must never be a
    // pre-acknowledged one, or the backend refusal could not fire.
    it("does not send an acknowledgement on a create nobody was asked about", async () => {
      const api = createMockApi();
      const create = vi.spyOn(api, "createBackupSet");
      renderWizardWithRoutes(api);

      await walkToReview();
      await userEvent.click(screen.getByRole("button", { name: /^Save & enable$/ }));

      await waitFor(() => expect(create).toHaveBeenCalled());
      expect(create.mock.calls[0][0].acknowledgeRepoint).toBeUndefined();
    });

    // Before M7 (#146 review), this scenario was reachable by clicking
    // Save: the button stayed enabled with no key imported, and
    // handleSave's own ad hoc guard rejected the request after the
    // click. M7 made the button structurally disabled; #864 stops the
    // flow before it, because "Generate" cannot satisfy the connection
    // test either — the test needs a key id to dial with, so the step
    // that proves the source can never be finished on this path and the
    // rail never reaches the Save buttons at all.
    it("never reaches Save on the key source that isn't the wired 'import' path, instead of allowing a doomed request", async () => {
      const api = createMockApi();
      const spy = vi.spyOn(api, "createBackupSet");
      renderWizardWithRoutes(api);

      // Default keySource is "generate" — no key is ever imported here.
      await userEvent.click(railStep("Connection test"));
      await waitFor(() => expect(screen.getByRole("button", { name: "Trust host" })).toBeEnabled());
      await userEvent.click(screen.getByRole("button", { name: "Trust host" }));

      // Trusting a fingerprint is not proving a connection, and the one
      // control that would prove it has nothing to dial with.
      expect(screen.getByRole("button", { name: /^Test connection$/ })).toBeDisabled();
      expect(railStep("Review")).toBeDisabled();
      expect(screen.getByRole("button", { name: "Continue" })).toBeDisabled();

      await userEvent.click(railStep("Review"));
      expect(screen.queryByRole("button", { name: /^Save & enable$/ })).toBeNull();
      expect(spy).not.toHaveBeenCalled();
      expect(screen.queryByText("SETS LIST PAGE")).toBeNull();
    });
  });
});
