import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupdApi } from "@shared/api/contracts";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import type { BackupSet } from "@shared/types/backup";
import { resetGraphForTests } from "@shared/state/graph";
import { backupSetPath } from "@shared/utilities/routes";

/**
 * Issue #845 — the per-set half: one backup set polling on its own
 * cadence while the rest of the deployment follows the global default.
 *
 * The claim that needs a test rather than a screenshot is what an EMPTY
 * box means. It is not "no interval"; it is "follow the deployment's",
 * and the request that says so is an explicit zero. A form that sent an
 * absent key instead would answer 200 and leave the override exactly
 * where it was, which is a save that looks like it worked.
 */

async function openEditMode(api: BackupdApi, target: BackupSet) {
  render(
    <MemoryRouter initialEntries={[backupSetPath(target.source, target.set)]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={false} />} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
  await screen.findByText(target.name);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  });
  await screen.findByRole("button", { name: "SAVE ALL & EXIT EDIT" });
}

const box = () => screen.getByLabelText("Polling interval (minutes)") as HTMLInputElement;

describe("issue #845: a backup set's own polling interval", () => {
  afterEach(() => {
    resetGraphForTests();
    resetMockFixtures();
    vi.restoreAllMocks();
  });

  it("draws an inheriting set as an empty box naming what it inherits", async () => {
    const api = createMockApi();
    const sets = await api.listSets();
    const inheriting = sets.find((s) => s.pollIntervalSeconds === null);
    if (!inheriting) throw new Error("the fixture has no inheriting set");

    await openEditMode(api, inheriting);

    expect(box().value).toBe("");
    expect(box().placeholder).toBe("Inherit global (15m)");
  });

  it("sends a typed override as seconds, and only that field", async () => {
    const api = createMockApi();
    const updateBackupSet = vi.spyOn(api, "updateBackupSet");
    const sets = await api.listSets();
    const inheriting = sets.find((s) => s.pollIntervalSeconds === null);
    if (!inheriting) throw new Error("the fixture has no inheriting set");

    await openEditMode(api, inheriting);
    fireEvent.change(box(), { target: { value: "5" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save polling interval (minutes)" }));
    });

    await waitFor(() => expect(updateBackupSet).toHaveBeenCalledTimes(1));
    const [, , patch] = updateBackupSet.mock.calls[0];
    expect(patch).toEqual({ pollIntervalSeconds: 300 });
  });

  it("clearing the box asks to inherit again, as an explicit zero", async () => {
    const api = createMockApi();
    const updateBackupSet = vi.spyOn(api, "updateBackupSet");
    const sets = await api.listSets();
    const overriding = sets.find((s) => s.pollIntervalSeconds === 5 * 60);
    if (!overriding) throw new Error("the fixture has no overriding set");

    await openEditMode(api, overriding);
    expect(box().value).toBe("5");

    fireEvent.change(box(), { target: { value: "" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save polling interval (minutes)" }));
    });

    await waitFor(() => expect(updateBackupSet).toHaveBeenCalledTimes(1));
    const [, , patch] = updateBackupSet.mock.calls[0];
    expect(patch).toEqual({ pollIntervalSeconds: 0 });
  });

  it("refuses a fraction of a minute here rather than sending one", async () => {
    const api = createMockApi();
    const updateBackupSet = vi.spyOn(api, "updateBackupSet");
    const sets = await api.listSets();
    const inheriting = sets.find((s) => s.pollIntervalSeconds === null);
    if (!inheriting) throw new Error("the fixture has no inheriting set");

    await openEditMode(api, inheriting);
    fireEvent.change(box(), { target: { value: "0.5" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save polling interval (minutes)" }));
    });

    expect(await screen.findByRole("alert")).toHaveTextContent(/whole number of minutes/i);
    expect(updateBackupSet).not.toHaveBeenCalled();
  });
});
