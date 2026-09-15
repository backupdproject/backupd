/**
 * Issue #842: two backup sets that live on ONE volume must never make the
 * dashboard report more free space than that volume holds.
 *
 * The defect was reported off a real NAS: the health panel showed "Free
 * space 21.6 TB" for a backup volume with 11.9 TB available on a 13.9 TB
 * disk. A free-space figure larger than the whole disk is not a number
 * that exists, which is exactly the rule core/service/managerstorage.go
 * already states for the storage panel ("two backup sets on the same
 * volume ... summing total_bytes across them reports twice the disk").
 * The per-set health list carries no filesystem discriminator, so every
 * set on a shared volume reports the same free space and a sum adds it up
 * again.
 *
 * The assertions here are deliberately about the physical bound rather
 * than about any one field or row, because the bound is the thing an
 * operator can check: nothing the dashboard prints may claim more bytes
 * than the volume the sets share. Written that way, the test survives the
 * fix moving where free space comes from, and still fails for any future
 * screen that sums the per-set readings again.
 *
 * The health report under test is built by the real client from a real
 * wire body, not hand-written, because the summing being asserted against
 * happens in that translation layer and a hand-written fixture would
 * assert nothing about it.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { DashboardPage } from "@shared/pages/DashboardPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { httpApi } from "@shared/api/client";
import { resetGraphForTests } from "@shared/state/graph";
import type { RetndApi, ManagerStorage } from "@shared/api/contracts";
import type { AsyncState } from "@shared/hooks/useAsync";
import type { BackupSet } from "@shared/types/backup";
import type { SystemHealth } from "@shared/types/operation";

/** The NAS from the issue: `statfs` bavail 2,898,439,509 × 4096 on
 *  pool1-volume1, the volume both sets below are configured onto. */
const VOLUME_FREE = 11_872_008_265_728;
/** The same volume's size (`df`: a 13 TB disk). The physical ceiling no
 *  figure on the page may exceed. */
const VOLUME_TOTAL = 13_869_056_000_000;

/** What the old sum produced for two sets on this one volume, and the
 *  number the issue was filed on once formatted. */
const DOUBLED = VOLUME_FREE * 2;

/** One entry of GET /api/v1/system/health's per-set list. Both sets below
 *  report the SAME volume, which is the whole case: the wire has no
 *  filesystem key, so nothing downstream can tell these two readings are
 *  one reading seen twice. */
const WIRE_SET = {
  source_name: "nas-01",
  state: "HEALTHY",
  reason: "fresh",
  stale_after_seconds: 86_400,
  current_transfers: 0,
  pending_deletes: 0,
  failures: 0,
  quarantined_count: 0,
  quarantined_lost_count: 0,
  read_only_retained_count: 0,
  free_bytes: VOLUME_FREE,
  free_bytes_known: true,
  total_bytes: VOLUME_TOTAL,
  storage_level: "OK",
  newest_good_backup_at: "2026-08-30T09:00:00Z",
  last_completed_backup_at: "2026-08-30T09:00:00Z"
};

const WIRE_HEALTH = {
  generated_at: "2026-08-30T10:00:00Z",
  backup_sets: [
    { ...WIRE_SET, backup_set_id: "nas-01/home-nightly", set_name: "home-nightly" },
    { ...WIRE_SET, backup_set_id: "nas-01/home-weekly", set_name: "home-weekly" }
  ]
};

/** GET /api/v1/system/storage measures the shared volume ONCE, which is
 *  the reading the dashboard is meant to show. */
const VOLUME: ManagerStorage = {
  known: true,
  unknownReason: "",
  measuredPath: "/home/rom/rclone-manager/backups",
  totalBytes: VOLUME_TOTAL,
  freeBytes: VOLUME_FREE,
  availableBytes: VOLUME_FREE,
  catalogBytes: VOLUME_TOTAL - VOLUME_FREE,
  catalogBytesKnown: true,
  otherBytes: 0,
  otherBytesKnown: true,
  capBytes: 0,
  denominator: "disk",
  limitBytes: VOLUME_TOTAL,
  usedBytes: VOLUME_TOTAL - VOLUME_FREE,
  headroomBytes: VOLUME_FREE,
  bindingConstraint: "disk",
  warningFreeBytes: 0,
  criticalFreeBytes: 0,
  level: "OK"
};

/** The formatter's own scale: base-1024 under base-10 labels (see
 *  utilities/format.ts), so a figure read off the screen converts back to
 *  the byte count that produced it. */
const UNIT: Record<string, number> = {
  B: 1,
  KB: 1024,
  MB: 1024 ** 2,
  GB: 1024 ** 3,
  TB: 1024 ** 4,
  PB: 1024 ** 5
};

/** Every byte quantity the rendered page states, as the bytes a reader
 *  would understand it to mean.
 *
 *  Text nodes are walked one at a time rather than matched against the
 *  subtree's textContent, because concatenating adjacent nodes invents
 *  digits that nothing on screen shows ("32" beside "1.8 TB" would read
 *  as 321.8 TB).
 */
function statedByteFigures(root: HTMLElement): { text: string; bytes: number }[] {
  const found: { text: string; bytes: number }[] = [];
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    for (const m of (node.textContent ?? "").matchAll(/(\d+(?:\.\d+)?)\s(B|KB|MB|GB|TB|PB)\b/g)) {
      found.push({ text: m[0], bytes: Number(m[1]) * UNIT[m[2]] });
    }
  }
  return found;
}

/** The health report the shipped client builds from the wire body above. */
async function healthForSharedVolume(): Promise<SystemHealth> {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      headers: new Headers(),
      json: async () => WIRE_HEALTH
    })
  );
  try {
    return await httpApi.getHealth();
  } finally {
    vi.unstubAllGlobals();
  }
}

/** Both sets pointed at one directory on the shared volume, so the page's
 *  own fixtures agree with the health report it is given. */
async function sharedVolumeSets(): Promise<BackupSet[]> {
  const sets = await createMockApi().listSets();
  return sets.slice(0, 2).map((set, i) => {
    const next: BackupSet = {
      ...set,
      id: "nas-01/home-" + (i === 0 ? "nightly" : "weekly"),
      source: "nas-01",
      set: i === 0 ? "home-nightly" : "home-weekly",
      name: i === 0 ? "Home nightly" : "Home weekly",
      state: "healthy",
      destination: "/home/rom/rclone-manager/backups/",
      readOnly: false,
      readOnlyRetainedCount: 0
    };
    delete next.haltReason;
    return next;
  });
}

async function renderDashboard() {
  const api: RetndApi = { ...createMockApi(), getStorage: () => Promise.resolve(VOLUME) };
  const health: AsyncState<SystemHealth> = {
    data: await healthForSharedVolume(),
    error: null,
    loading: false,
    reload: () => {}
  };
  const sets: AsyncState<BackupSet[]> = {
    data: await sharedVolumeSets(),
    error: null,
    loading: false,
    reload: () => {}
  };
  const { container } = render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <DashboardPage health={health} sets={sets} readOnly={false} />
      </ApiProvider>
    </MemoryRouter>
  );
  // The storage reading is fetched on mount; the figures under test are
  // not on screen until it lands.
  await screen.findByText(/free$/);
  return container;
}

describe("free space for two backup sets on one volume (issue #842)", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("states no quantity larger than the volume the sets share", async () => {
    const container = await renderDashboard();

    const impossible = statedByteFigures(container).filter((f) => f.bytes > VOLUME_TOTAL);

    expect(impossible.map((f) => f.text)).toEqual([]);
  });

  it("shows the volume's own free space, and the two readings added together nowhere", async () => {
    await renderDashboard();

    // The deduped reading, from the storage endpoint that measures the
    // volume once.
    expect(screen.getByText("10.8 TB free")).toBeTruthy();
    // 21.6 TB: the figure the issue was filed on, and a claim about a
    // 13 TB disk.
    expect(screen.queryByText(/21\.6 TB/)).toBeNull();
  });

  it("gives the health report no capacity figure a shared volume could inflate", async () => {
    const health = await healthForSharedVolume();

    // Whatever the health read model carries about capacity, no number in
    // it may exceed the single volume both sets measured: one volume seen
    // twice is still one volume.
    const inflated = Object.entries(health).filter(
      ([, value]) => typeof value === "number" && value > VOLUME_FREE
    );

    expect(inflated).toEqual([]);
    expect(DOUBLED).toBeGreaterThan(VOLUME_TOTAL); // the fixture really is impossible
    // The per-set verdict is a max, not a sum, and stays.
    expect(health.storageState).toBe("nominal");
    expect(health.storageReadingsUnavailable).toBe(0);
  });
});
