/**
 * The per-set configuration card, and the patch it builds (EPIC K, issue
 * #788).
 *
 * Three claims are worth a test here and the rest of the card is layout.
 *
 * The first is the one the contract makes: `engine` and
 * `repositoryDomain` are create-only, so they are on this card as FACTS
 * and there is no control for either, not even a disabled one. A screen
 * that offered them would be offering an edit UpdateBackupSetRequest
 * cannot carry, and the refusal would arrive after the operator had
 * chosen.
 *
 * The second is the save. A per-card Save that sends five fields rewrites
 * four budgets the operator never touched with values that merely
 * round-tripped through a text box, and the failure is invisible: every
 * one of them comes back holding what it already held, so nothing on
 * screen says anything happened. So the patch is asserted as a whole
 * object rather than by one key, which is what makes "and nothing else"
 * checkable.
 *
 * The third is the artifact branch, which exists so the two engines are
 * never confused for each other: an artifact set has no repository
 * domain, no consistency declaration and no verification level, and those
 * rows are ABSENT here rather than drawn empty.
 */
import { afterEach, describe, expect, it } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import type { RetndApi, BackupSetPatch } from "@shared/api/contracts";
import type { BackupSet } from "@shared/types/backup";
import { BackupSetConfigurationCard } from "@shared/pages/BackupSetConfigurationCard";
import {
  cadenceSummary,
  incrementalFieldErrors,
  incrementalPatch,
  readIncrementalDraft
} from "@shared/pages/incrementalConfigFields";
import type { IncrementalDraft } from "@shared/pages/incrementalConfigFields";

/** The fixture deployment's incremental set: sampled content at 5%, every
 *  file every 7 days, a restore drill every 30, on a quiesced source. */
const INCREMENTAL = { source: "production", set: "postgres-primary" };
/** The fixture deployment's one artifact set. */
const ARTIFACT = { source: "production", set: "auth-config" };

async function fixture(id: { source: string; set: string }): Promise<BackupSet> {
  const sets = await createMockApi().listSets();
  const found = sets.find((s) => s.source === id.source && s.set === id.set);
  if (!found) throw new Error("no fixture " + id.source + "/" + id.set);
  return found;
}

/** The mock API with every patch it is handed recorded, so a test can
 *  assert what reached the wire rather than that something did. */
function recordingApi(): { api: RetndApi; patches: BackupSetPatch[] } {
  const real = createMockApi();
  const patches: BackupSetPatch[] = [];
  const api: RetndApi = {
    ...real,
    updateBackupSet: (source, set, patch) => {
      patches.push(patch);
      return real.updateBackupSet(source, set, patch);
    }
  };
  return { api, patches };
}

async function renderCard(
  set: BackupSet,
  api: RetndApi = createMockApi(),
  readOnly = false
): Promise<void> {
  render(
    <ApiProvider api={api}>
      <BackupSetConfigurationCard set={set} readOnly={readOnly} onSaved={() => undefined} />
    </ApiProvider>
  );
}

async function openEditor(): Promise<void> {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Edit incremental settings" }));
  });
}

async function save(): Promise<void> {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Save incremental settings" }));
  });
}

function budget(label: string): HTMLElement {
  return screen.getByLabelText(label);
}

afterEach(() => {
  resetMockFixtures();
});

describe("what an incremental set's configuration card offers", () => {
  it("states the engine and the repository domain, and offers no control for either", async () => {
    await renderCard(await fixture(INCREMENTAL));

    // Both are on screen as values, in the cell each belongs to.
    const engineCell = screen.getByText("Engine").parentElement as HTMLElement;
    expect(within(engineCell).getAllByText("Incremental").length).toBeGreaterThan(0);
    const domainCell = screen.getByText("Repository domain").parentElement as HTMLElement;
    expect(within(domainCell).getAllByText("primary-nas").length).toBeGreaterThan(0);
    expect(screen.getByText(/fixed after creation/i)).toBeTruthy();

    await openEditor();

    // And neither is editable, in edit mode, where every editable field
    // is. The consistency and verification radios ARE here, so this is
    // not passing because the form failed to open.
    expect(screen.getByRole("radio", { name: /Quiesced/ })).toBeTruthy();
    expect(screen.queryByRole("radio", { name: /Artifact/ })).toBeNull();
    expect(screen.queryByRole("radio", { name: /^Incremental/ })).toBeNull();
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.queryByRole("textbox", { name: /repository domain/i })).toBeNull();
    expect(screen.queryByRole("spinbutton", { name: /repository domain/i })).toBeNull();
  });

  it("reports an inherited budget as inherited rather than as a number", async () => {
    // billing-mysql inherits every part of its budget: null percent, null
    // cadences. A card that rendered those as 0 would be claiming the
    // deployment never reads a file on a schedule.
    await renderCard(await fixture({ source: "production", set: "billing-mysql" }));

    // The sample share says so in the row's own words; the two cadences
    // say so in cadenceSummary's.
    expect(screen.getByText("Inherited from the deployment")).toBeTruthy();
    expect(screen.getAllByText("inherited from the deployment")).toHaveLength(2);
    expect(screen.queryByText("0%")).toBeNull();
    expect(screen.queryByText("never on a cadence")).toBeNull();
  });

  it("draws a cadence of zero as never, which is not the same as inherited", async () => {
    // media/weekly-archive has a restore-drill cadence of exactly 0.
    await renderCard(await fixture({ source: "media", set: "weekly-archive" }));

    expect(screen.getByText("never on a cadence")).toBeTruthy();
    expect(screen.getByText("every 1 day")).toBeTruthy();
  });

  it("leaves the editor unavailable when management actions are refused", async () => {
    await renderCard(await fixture(INCREMENTAL), createMockApi(), true);

    expect(screen.getByRole("button", { name: "Edit incremental settings" })).toBeDisabled();
  });
});

describe("what a Save sends", () => {
  it("sends the one budget that changed and nothing else", async () => {
    const { api, patches } = recordingApi();
    await renderCard(await fixture(INCREMENTAL), api);
    await openEditor();

    await act(async () => {
      fireEvent.change(budget("Sampled share of files (%)"), { target: { value: "12" } });
    });
    await save();

    await waitFor(() => expect(patches).toHaveLength(1));
    // The whole object: a patch carrying the four untouched fields would
    // pass a per-key assertion and fail this one.
    expect(patches[0]).toEqual({ verificationSamplePercent: 12 });
  });

  it("sends a cadence in seconds, and an explicit zero as a zero", async () => {
    const { api, patches } = recordingApi();
    await renderCard(await fixture(INCREMENTAL), api);
    await openEditor();

    await act(async () => {
      fireEvent.change(budget("Read every file every (days)"), { target: { value: "14" } });
    });
    await act(async () => {
      fireEvent.change(budget("Restore drill every (days)"), { target: { value: "0" } });
    });
    await save();

    await waitFor(() => expect(patches).toHaveLength(1));
    expect(patches[0]).toEqual({
      verificationFullEverySeconds: 14 * 86_400,
      verificationRestoreDrillEverySeconds: 0
    });
  });

  it("sends the consistency declaration alone when only that was answered", async () => {
    const { api, patches } = recordingApi();
    await renderCard(await fixture(INCREMENTAL), api);
    await openEditor();

    await act(async () => {
      fireEvent.click(screen.getByRole("radio", { name: /Frozen image/ }));
    });
    await save();

    await waitFor(() => expect(patches).toHaveLength(1));
    expect(patches[0]).toEqual({ sourceConsistency: "external_snapshot" });
  });

  it("refuses to save a cleared cadence, and says why instead of sending a zero", async () => {
    const { api, patches } = recordingApi();
    await renderCard(await fixture(INCREMENTAL), api);
    await openEditor();

    await act(async () => {
      fireEvent.change(budget("Read every file every (days)"), { target: { value: "" } });
    });

    expect(screen.getByRole("button", { name: "Save incremental settings" })).toBeDisabled();
    expect(screen.getByText(/no way to say .*inherit again/i)).toBeTruthy();
    expect(patches).toHaveLength(0);
  });

  it("keeps Save inert until something actually changes", async () => {
    await renderCard(await fixture(INCREMENTAL));
    await openEditor();

    expect(screen.getByRole("button", { name: "Save incremental settings" })).toBeDisabled();
    expect(screen.getByText("Nothing has changed yet.")).toBeTruthy();
  });
});

describe("the same card for an artifact set", () => {
  it("draws the artifact configuration and none of the incremental fields", async () => {
    await renderCard(await fixture(ARTIFACT));

    expect(screen.getByText("Artifact")).toBeTruthy();
    expect(screen.getByText("Stable size")).toBeTruthy();
    // The three an artifact set does not have, absent rather than empty.
    expect(screen.queryByText("Repository domain")).toBeNull();
    expect(screen.queryByText("Source consistency")).toBeNull();
    expect(screen.queryByText("Verification level")).toBeNull();
    expect(screen.queryByRole("button", { name: "Edit incremental settings" })).toBeNull();
  });

  it("says the incremental fields are absent rather than unset", async () => {
    await renderCard(await fixture(ARTIFACT));

    const note = screen.getByText(/absent here rather than shown empty/i);
    expect(within(note).getByText(/repository domain/i)).toBeTruthy();
  });
});

describe("the patch builder", () => {
  const baseline: IncrementalDraft = {
    sourceConsistency: "externally_quiesced",
    verificationLevel: "content_sample",
    samplePercent: "5",
    fullEveryDays: "7",
    drillEveryDays: "30"
  };

  it("reads a stored block into the boxes an operator sees", async () => {
    const set = await fixture(INCREMENTAL);
    expect(readIncrementalDraft(set.incremental!)).toEqual(baseline);
  });

  it("reads every absent field as inherited rather than as a value", async () => {
    const set = await fixture({ source: "production", set: "billing-mysql" });
    expect(readIncrementalDraft(set.incremental!)).toEqual({
      sourceConsistency: "live_best_effort",
      verificationLevel: "structural",
      samplePercent: "",
      fullEveryDays: "",
      drillEveryDays: ""
    });
  });

  it("emits nothing for a draft nobody edited", () => {
    expect(incrementalPatch(baseline, { ...baseline })).toEqual({});
  });

  it("emits one key per genuine change", () => {
    expect(incrementalPatch(baseline, { ...baseline, verificationLevel: "restore_drill" })).toEqual({
      verificationLevel: "restore_drill"
    });
  });

  it("leaves an inherited field inherited rather than sending what it inherits", () => {
    const inherited: IncrementalDraft = { ...baseline, samplePercent: "", fullEveryDays: "" };
    expect(incrementalPatch(inherited, { ...inherited })).toEqual({});
  });

  it("refuses a percentage outside the range the contract declares", () => {
    expect(incrementalFieldErrors(baseline, { ...baseline, samplePercent: "0" }).samplePercent).toBeTruthy();
    expect(incrementalFieldErrors(baseline, { ...baseline, samplePercent: "101" }).samplePercent).toBeTruthy();
    expect(incrementalFieldErrors(baseline, { ...baseline, samplePercent: "1" }).samplePercent).toBeUndefined();
    expect(incrementalFieldErrors(baseline, { ...baseline, samplePercent: "100" }).samplePercent).toBeUndefined();
  });

  it("refuses clearing a field that is set, and allows one that was already inherited", () => {
    expect(incrementalFieldErrors(baseline, { ...baseline, fullEveryDays: "" }).fullEveryDays).toBeTruthy();
    const inherited: IncrementalDraft = { ...baseline, fullEveryDays: "" };
    expect(incrementalFieldErrors(inherited, { ...inherited }).fullEveryDays).toBeUndefined();
  });

  it("refuses a cadence that does not land on whole seconds", () => {
    // A third of a day is 28800.0000000000004 seconds of intent and no
    // number of seconds the wire can carry.
    expect(
      incrementalFieldErrors(baseline, { ...baseline, fullEveryDays: "0.0000001" }).fullEveryDays
    ).toBeTruthy();
    expect(incrementalFieldErrors(baseline, { ...baseline, fullEveryDays: "0.5" }).fullEveryDays).toBeUndefined();
  });

  it("states a cadence in the three answers it can actually have", () => {
    expect(cadenceSummary(null)).toBe("inherited from the deployment");
    expect(cadenceSummary(0)).toBe("never on a cadence");
    expect(cadenceSummary(7 * 86_400)).toBe("every 7 days");
    expect(cadenceSummary(6 * 3600)).toBe("every 6 hours");
  });
});
