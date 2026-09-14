/**
 * Issue #927: the add-backup-set wizard's glob field said "Ignore paths
 * matching" and was an INCLUDE list.
 *
 * Nothing in the product agreed with that label. The state is
 * `includePatterns`, the request field is `include`, the API contract has
 * `include` and no exclude of any kind, the field's own help copy says
 * "which filenames ... count as this backup set's artifacts", and the two
 * other surfaces that draw the same data call it "Include patterns" and
 * "Include". The label was the sole outlier, and it said the opposite.
 *
 * What that cost is the reason this file exists. `core/internal/discovery`
 * drops a basename the include list does not match with a bare `continue`
 * — not rejected, not pending, deliberately silent — so an operator who
 * believed the label and typed `*.tmp` here got a set that backs up only
 * `*.tmp`. No refusal, no failed run, nothing to read: the symptom is
 * empty backups, days later.
 *
 * # The rule is asserted, not the wording
 *
 * A test that pinned "Filename patterns to back up" would pass for the
 * wrong reason: it would guard one string rather than the agreement
 * between a control's NAME and what that control SUBMITS. So this file
 * never names the control it is about. It finds it by behaviour — the one
 * step-1 box whose contents arrive in `createBackupSet`'s `include`,
 * proven causally by typing a sentinel into it and watching that sentinel
 * come out on the wire — and only then reads the name an operator sees.
 *
 * Rewording the field stays free. Naming an include control with
 * exclusion vocabulary, on any of the surfaces that give it its
 * accessible name, does not.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { BackupSetWizardPage } from "@shared/pages/BackupSetWizardPage";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import { resetGraphForTests } from "@shared/state/graph";
import { walkToReview } from "./wizardWalk";

/**
 * Everything an operator can read as this control's name, concatenated.
 *
 * Not just the `<span class="field__label">` the design system happens to
 * render today: an `aria-label`, an `aria-labelledby` target, a
 * placeholder or a title would each be the field's name (or its fallback)
 * to a screen reader or to browser automation, so putting "ignore" in any
 * of them would reintroduce exactly this defect while a label-only check
 * stayed green.
 */
function namingText(control: HTMLElement): string {
  const labelledBy = (control.getAttribute("aria-labelledby") ?? "")
    .split(/\s+/)
    .filter(Boolean)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");

  return [
    control.getAttribute("aria-label") ?? "",
    labelledBy,
    // The wizard's Field wraps its input in the <label> itself, so the
    // label's own text is the accessible name when there is no explicit
    // one. Excluding the input's value: that is the operator's data, not
    // the field's name.
    control.closest("label")?.textContent ?? "",
    control.getAttribute("placeholder") ?? "",
    control.getAttribute("title") ?? ""
  ].join(" \u00b7 ");
}

/** The description the control's own aria-describedby resolves to. */
function describedText(control: HTMLElement): string {
  const id = control.getAttribute("aria-describedby") ?? "";
  return document.getElementById(id)?.textContent ?? "";
}

/**
 * Vocabulary that tells an operator a box is a list of things to LEAVE
 * OUT. A whitelist control named with any of these is the #927 defect,
 * whatever the exact sentence around it.
 */
const EXCLUSION_WORDING =
  /\b(?:ignor\w*|exclud\w*|exclusion\w*|omit\w*|skipp?\w*|except|blacklist\w*|forbid\w*|deny|denied|filter\s+out|leave\s+out|keep\s+out)\b/i;

/** And the other half: the name has to say what it actually does, or a
 *  neutral "Patterns" would satisfy the check above while telling an
 *  operator nothing about which way the filter runs. */
const INCLUSION_WORDING = /\b(?:includ\w*|back(?:s|ed|ing)?\s+up|artifacts?|keep|kept)\b/i;

const SENTINEL = "sentinel-only-this-*.zst";

function renderWizard() {
  const api = createMockApi();
  const create = vi.spyOn(api, "createBackupSet");
  render(
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
  return create;
}

/**
 * One pass through the wizard. Reads every box the Source step offers
 * (recording each one's value, name and description BEFORE the save
 * navigates the page away), optionally retypes one of them, then walks to
 * Review and saves, handing back what went on the wire.
 *
 * `typeInto` is an index into the boxes in DOM order, never a label: it
 * is how the caller pushes a value through a control it identified by
 * behaviour rather than by name.
 */
async function fillAndSave(typeInto?: { index: number; value: string }) {
  const create = renderWizard();

  const boxes = screen.getAllByRole("textbox");
  if (typeInto) {
    await userEvent.clear(boxes[typeInto.index]);
    await userEvent.type(boxes[typeInto.index], typeInto.value);
  }

  const fields = screen.getAllByRole("textbox").map((box) => ({
    value: (box as HTMLInputElement).value,
    name: namingText(box),
    described: describedText(box)
  }));

  await walkToReview();
  await userEvent.click(screen.getByRole("button", { name: /^Save & enable$/ }));
  await screen.findByText("SETS LIST PAGE");

  expect(create).toHaveBeenCalledTimes(1);
  return { fields, request: create.mock.calls[0][0] };
}

describe("the wizard's glob field is named for what it submits (issue #927)", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  it("names the control that writes `include` as an include list, not as an ignore list", async () => {
    // Pass 1: save the wizard's own defaults and find the box whose
    // contents came back out as `include`. No label is involved in
    // choosing it.
    const first = await fillAndSave();
    const wire = first.request.include ?? [];
    expect(wire.length, "the wizard sent no include patterns to identify a control by").toBeGreaterThan(0);

    const holders = first.fields
      .map((field, index) => ({ index, field }))
      .filter(
        ({ field }) =>
          JSON.stringify(
            field.value
              .split(",")
              .map((p) => p.trim())
              .filter(Boolean)
          ) === JSON.stringify(wire)
      );

    // Exactly one, or the identification below would be a coincidence
    // between two boxes that happen to hold the same text.
    expect(holders.length, "no single Source-step box holds what was sent as `include`").toBe(1);
    const includeControl = holders[0].index;

    cleanup();
    resetGraphForTests();

    // Pass 2: prove the binding is causal rather than a matching default.
    // A sentinel typed into that box has to be what `include` carries.
    const second = await fillAndSave({ index: includeControl, value: SENTINEL });
    expect(second.request.include).toEqual([SENTINEL]);

    // Only now is it fair to read the name, and this is the assertion the
    // defect would have failed: "Ignore paths matching" on the box that
    // decides which files are backed up AT ALL.
    const { name, described } = second.fields[includeControl];
    expect(
      name,
      "the control that submits `include` is named with exclusion wording: " +
        "an operator who types a pattern here to omit it gets a set that backs up only that pattern"
    ).not.toMatch(EXCLUSION_WORDING);
    expect(name, "the control that submits `include` does not say that it includes").toMatch(
      INCLUSION_WORDING
    );

    // And the help behind it is still the include copy, so the name, the
    // explanation and the wire field are three statements of one thing
    // rather than two of one and one of another.
    expect(described).toContain(FIELD_HELP.wizardIncludePatterns.what);
  });
});
