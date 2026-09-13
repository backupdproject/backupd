import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SettingsPage } from "@shared/pages/SettingsPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { AppSettings, UpdateSettingsRequest } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import type { VersionInfo } from "@shared/types/operation";

/**
 * Issue #845 — the deployment-wide source polling interval, on the
 * Settings page.
 *
 * The claims worth pinning are the ones a decorative control would also
 * satisfy, which is exactly what this card replaces: the value on screen
 * is the one the server reported, what leaves the browser is SECONDS
 * carrying only this section, and an interval under the floor the server
 * advertises never reaches the wire at all — with a positive control
 * beside each refusal, since a form that disabled Save unconditionally
 * would pass every refusal test and save nothing, ever.
 */

const VERSION: VersionInfo = {
  api: "v1", service: "1.3.0", buildCommit: "9f4c1ab", goVersion: "go1.27.0",
  engine: "1.68.2", configRevision: "cfg_9f4c1ab", ready: true, compatible: true
};

function settingsFixture(pollIntervalSeconds = 900): AppSettings {
  return {
    retention: {
      timezone: "Europe/Berlin",
      weekStartsOn: "monday",
      tiers: [{ name: "daily", granularity: "day", keep: 7 }],
      protectLastKnownGood: true
    },
    capacity: {
      capBytes: 0,
      warningFreeBytes: 0,
      criticalFreeBytes: 0,
      safetyMarginBytes: 0,
      backupRoot: "/data/backups",
      backupRootConfigured: false
    },
    service: { pollIntervalSeconds },
    mediums: [],
    schema: {
      storage: {
        verificationClasses: [],
        mediumDisclosure: "I delete the copy on this machine after a verified upload.",
        retrievalDisclosure: "Reading a copy back is billed by your provider."
      },
      retention: {
        granularities: ["day", "week", "month", "quarter", "half_year", "year", "days"],
        windowUnits: ["day", "week", "month", "quarter", "half_year", "year"],
        tierNamePattern: "^[a-z][a-z0-9_]*$",
        reservedTierName: "last_known_good",
        keepMax: 10000,
        periodDaysMax: 3650,
        defaultTiers: [{ name: "daily", granularity: "day", keep: 7 }]
      },
      service: { minPollIntervalSeconds: 60 }
    }
  };
}

async function renderSettings(
  options: {
    settings?: AppSettings;
    updateSettings?: (req: UpdateSettingsRequest) => Promise<AppSettings>;
  } = {}
) {
  const settings = options.settings ?? settingsFixture();
  const getSettings = vi.fn(() => Promise.resolve(settings));
  const updateSettings = vi.fn(options.updateSettings ?? (() => Promise.resolve(settings)));
  const api = { ...createMockApi(), getSettings, updateSettings };

  act(() => {
    graph.commit("test/seed-version", (tx) =>
      tx.set(versionNode, { data: VERSION, error: null, loading: false })
    );
  });

  render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <PlatformProvider bridge={genericBridge}>
          <SettingsPage readOnly={false} />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
  await screen.findByLabelText("Source polling interval (minutes)");
  return { updateSettings };
}

const field = () => screen.getByLabelText("Source polling interval (minutes)") as HTMLInputElement;
const save = () => screen.getByRole("button", { name: "Save service behaviour" });

function card() {
  const section = screen.getByText("Service behaviour").closest("section");
  if (!section) throw new Error("Service behaviour card not found");
  return within(section);
}

describe("Settings: service behaviour", () => {
  beforeEach(() => {
    resetGraphForTests();
  });
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  it("shows the running interval in minutes and saves nothing until it changes", async () => {
    await renderSettings();

    expect(field().value).toBe("15");
    expect(save()).toHaveProperty("disabled", true);
  });

  it("sends the typed minutes as seconds, in a service-only patch", async () => {
    const { updateSettings } = await renderSettings();

    fireEvent.change(field(), { target: { value: "45" } });
    fireEvent.click(save());

    expect(updateSettings).toHaveBeenCalledTimes(1);
    const req = updateSettings.mock.calls[0][0] as UpdateSettingsRequest;
    expect(req.service?.pollIntervalSeconds).toBe(2700);
    expect(req.retention).toBeUndefined();
    expect(req.capacity).toBeUndefined();
  });

  it("refuses an interval under the floor the server advertises, without a request", async () => {
    const { updateSettings } = await renderSettings();

    // 0 minutes is under the 60-second floor min_poll_interval_seconds
    // carries, and so is an empty box.
    fireEvent.change(field(), { target: { value: "0" } });
    expect(save()).toHaveProperty("disabled", true);
    expect(card().getByText(/at least 1 minute/i)).toBeTruthy();

    fireEvent.change(field(), { target: { value: "" } });
    expect(save()).toHaveProperty("disabled", true);

    // The positive control: a legal value re-enables the same button, so
    // the assertions above are about the value and not about a button
    // that is always disabled.
    fireEvent.change(field(), { target: { value: "5" } });
    expect(save()).toHaveProperty("disabled", false);
    expect(updateSettings).not.toHaveBeenCalled();
  });

  it("re-baselines against what the server says is now running", async () => {
    const { updateSettings } = await renderSettings({
      updateSettings: () => Promise.resolve(settingsFixture(3600))
    });

    fireEvent.change(field(), { target: { value: "45" } });
    fireEvent.click(save());
    await screen.findByText(/in effect now/i);

    expect(updateSettings).toHaveBeenCalledTimes(1);
    expect(field().value).toBe("60");
    expect(save()).toHaveProperty("disabled", true);
  });
});
