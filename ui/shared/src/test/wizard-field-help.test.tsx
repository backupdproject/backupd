import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { resetGraphForTests } from "@shared/state/graph";
import { BackupSetWizardPage } from "@shared/pages/BackupSetWizardPage";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import type { FieldHelpCopy } from "@shared/components/fieldHelpCopy";
// Since #864 the rail will not jump to a later step until the earlier
// ones are answered, so the cases that live on steps 6 and 7 walk there
// through the connection test rather than clicking straight past it.
import { proveTheSource } from "./wizardWalk";

/**
 * Issue #278's wizard follow-up (held back from the original PR while
 * #275/#288 restructured App.tsx, and cleared once #288 merged): the
 * wizard's honest fields are wired to their own copy, the same wiring
 * check field-help-pages.test.tsx runs for every other page. FieldHelp's
 * own suite proves the interaction works; this proves the RIGHT copy
 * reaches the RIGHT control on a page assembled across six steps rather
 * than one screen, and that the controls #299 later removed or turned
 * into plain non-interactive statements never gained a plausible-looking
 * pop-up in the meantime.
 *
 * Two of the honest fields (key source, completion method) explain a
 * GROUP of radios rather than one control, so their copy is asserted
 * against the group container's own aria-describedby (a radiogroup div,
 * a fieldset), not against any one radio inside it.
 */

function expectHelp(control: HTMLElement, copy: FieldHelpCopy) {
  const describedBy = control.getAttribute("aria-describedby");
  expect(describedBy, "the control carries no aria-describedby").toBeTruthy();

  const described = document.getElementById(describedBy ?? "");
  expect(described, "aria-describedby points at nothing").not.toBeNull();
  expect(described?.textContent).toContain(copy.what);
  expect(described?.textContent).toContain(copy.example);
  expect(described?.textContent).toContain(copy.effect);
}

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

describe("the wizard's honest fields are wired to their own copy", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  it("on the Source step", () => {
    renderWizard();

    expectHelp(screen.getByLabelText("Backup set name"), FIELD_HELP.wizardSetName);
    expectHelp(screen.getByLabelText("Server hostname"), FIELD_HELP.wizardHostname);
    expectHelp(screen.getByLabelText("SSH port"), FIELD_HELP.wizardSshPort);
    expectHelp(screen.getByLabelText("Username"), FIELD_HELP.wizardUsername);
  });

  it("on the Connection test step, including the group tooltip on the key-source radios", async () => {
    const user = userEvent.setup();
    renderWizard();

    await user.click(screen.getByRole("button", { name: "Connection test" }));

    // One tooltip for all three radios: the copy is a fact about the
    // GROUP ("only one of the three lets you finish this wizard"), which
    // no single radio's own label states.
    expectHelp(screen.getByRole("radiogroup", { name: "Key source" }), FIELD_HELP.wizardKeySource);

    // The private-key field only renders once "Import key" is selected;
    // the default choice is "Generate", whose own panel is a plain
    // statement of fact (#299), not a control, so it gets no tooltip.
    await user.click(screen.getByRole("radio", { name: /Import key/ }));
    expectHelp(screen.getByLabelText(/private key/i), FIELD_HELP.wizardPrivateKey);
  });

  it("on the Source and Verification steps, including the group tooltip on the completion-method fieldset", async () => {
    const user = userEvent.setup();
    renderWizard();

    // #788 moved the directory fields onto Source, where naming the
    // source happens, and left the completion method on the step that
    // asks how an artifact is proven good.
    expectHelp(screen.getByLabelText("Directory to back up"), FIELD_HELP.wizardRemoteFolder);
    expectHelp(screen.getByLabelText("Filename patterns to back up"), FIELD_HELP.wizardIncludePatterns);

    await proveTheSource();

    await user.click(screen.getByRole("button", { name: "Verification" }));
    // A <fieldset> is role "group", named by its own <legend>.
    expectHelp(screen.getByRole("group", { name: "Completion method" }), FIELD_HELP.wizardCompletionMethod);

    // Exclude patterns was removed entirely by #299, and there is still
    // nothing for the wizard to send one to: the API exposes `include`
    // and no exclude of any kind. (config.BackupSet did later gain
    // `exclude_paths` for #737, but that names DIRECTORIES to skip
    // walking and is config-file-only.) See wizard-decorative-
    // fields.test.tsx for the proof the field is gone, and
    // wizard-include-label.test.tsx (#927) for the proof the include
    // field that remains is not labelled as if it were the exclude one.
  });

  it("on the Retention and Verification steps", async () => {
    const user = userEvent.setup();
    renderWizard();
    await proveTheSource();

    await user.click(screen.getByRole("button", { name: "Retention" }));

    // Both labels also wrap a caption sentence (and, for NAS destination,
    // a button) besides their own field name, so testing-library's exact
    // label-text match sees the whole concatenated content rather than
    // just the name; { exact: false } is a substring match against that
    // same content, which the field name still uniquely picks out here.
    expectHelp(screen.getByLabelText("NAS destination", { exact: false }), FIELD_HELP.wizardNasDestination);

    await user.click(screen.getByRole("button", { name: "Verification" }));
    expectHelp(screen.getByLabelText("Application validation", { exact: false }), FIELD_HELP.wizardValidatorId);

    // #111 settled retention as one global policy; the per-set Daily/
    // Weekly/Monthly/Week-starts fields that used to draw the shape it
    // warned against were removed by #299, not merely left unexplained —
    // see wizard-decorative-fields.test.tsx for the proof they're gone.
  });

  it("on the Retention step's acknowledgement checkbox", async () => {
    const user = userEvent.setup();
    renderWizard();
    await proveTheSource();

    await user.click(screen.getByRole("button", { name: "Retention" }));

    expectHelp(
      screen.getByRole("checkbox", { name: /remote backup will be removed only after/i }),
      FIELD_HELP.wizardAcknowledge
    );
  });
});
