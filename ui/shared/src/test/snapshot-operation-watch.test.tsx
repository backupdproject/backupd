/**
 * What `useSnapshotOperation` does AFTER the submit (EPIC K, issue #788).
 *
 * The page suites drive the four acts and assert the request; every one
 * of them then stops at the first render of the operation that came
 * back. What none of them looks at is the watching: a restore and a
 * verify run for minutes and this hook is the only thing re-reading
 * them, so the cadence and — much more importantly — the STOP are
 * behaviour nothing else in the suite would notice losing. A poll that
 * never stopped would go on requesting a finished operation for as long
 * as the screen is open, which is a load the service does not need and a
 * defect no page test can see.
 *
 * Timers are faked here for the same reason: the interval is two seconds
 * and a real-time test of it would either be slow or be a test of
 * whatever the machine was doing at the time.
 *
 * The idempotency key is the other half. The hook's rule is that the key
 * survives a refusal — that is what makes the next press a retry — and
 * nothing else: it is dropped on success, on a different target, and on
 * dismissal, because in all three the next press is a NEW submission.
 * SnapshotRetentionPage's own suite pins the target half through the
 * screen; the two others are asserted here, where the hook can be driven
 * directly.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, renderHook } from "@testing-library/react";
import type { RenderHookResult } from "@testing-library/react";
import type { ReactNode } from "react";

import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupdApi } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { useSnapshotOperation } from "@shared/hooks/useSnapshotOperation";
import type { SnapshotAction } from "@shared/hooks/useSnapshotOperation";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import type { Operation, VersionInfo } from "@shared/types/operation";

type ActionHook = RenderHookResult<SnapshotAction, unknown>;

const VERSION: VersionInfo = {
  api: "v1", service: "1.3.0", buildCommit: "9f4c1ab", goVersion: "go1.27.0",
  engine: "1.68.2", configRevision: "cfg_9f4c1ab", ready: true, compatible: true
};

function operation(status: Operation["status"]): Operation {
  return {
    id: "op_watch_1",
    setId: "production/postgres-primary",
    setName: "production/postgres-primary",
    kind: "transfer",
    label: "restore snapshot",
    status,
    progress: null,
    nonDestructive: false,
    startedAt: "2026-09-13T04:00:00+02:00",
    cycle: null
  };
}

/** The hook reads exactly one method of the API — `getOperation` — so the
 *  double is the mock with that one replaced. Nothing here goes through
 *  the mock's own delay, which would need the fake clock advanced to
 *  resolve a promise and would make every assertion below about the
 *  fixture rather than about the hook. */
function harness(getOperation: BackupdApi["getOperation"]): ActionHook {
  const api: BackupdApi = { ...createMockApi(), getOperation };
  act(() => {
    graph.commit("test/seed", (tx) => tx.set(versionNode, { data: VERSION, error: null, loading: false }));
  });
  return renderHook(() => useSnapshotOperation("Could not start the restore."), {
    wrapper: ({ children }: { children: ReactNode }) => (
      <ApiProvider api={api}>{children}</ApiProvider>
    )
  });
}

/** Submits one act that resolves with `result`, or rejects with it, and
 *  lets the resolution land. */
async function submit(hook: ActionHook, target: string, result: Operation | Error): Promise<void> {
  await act(async () => {
    hook.result.current.submit(target, () =>
      result instanceof Error
        ? Promise.reject(result)
        : Promise.resolve({ operation: result, snapshots: [] })
    );
  });
}

async function tick(ms: number): Promise<void> {
  await act(async () => {
    vi.advanceTimersByTime(ms);
  });
}

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  resetGraphForTests();
  vi.restoreAllMocks();
});

describe("watching a submitted operation", () => {
  it("re-reads a running operation every two seconds, and not before the first two", async () => {
    const getOperation = vi.fn().mockResolvedValue(operation("running"));
    const hook = harness(getOperation);

    await submit(hook, "run-a", operation("running"));
    // Nothing is polled by the submit itself: the record it answered with
    // is the first reading.
    expect(getOperation).not.toHaveBeenCalled();

    await tick(1_999);
    expect(getOperation).not.toHaveBeenCalled();

    await tick(1);
    expect(getOperation).toHaveBeenCalledTimes(1);
    expect(getOperation).toHaveBeenCalledWith("op_watch_1");

    await tick(4_000);
    expect(getOperation).toHaveBeenCalledTimes(3);
  });

  it("stops polling the moment the record reaches a terminal status", async () => {
    const getOperation = vi.fn().mockResolvedValue(operation("completed"));
    const hook = harness(getOperation);

    await submit(hook, "run-a", operation("running"));
    await tick(2_000);
    expect(getOperation).toHaveBeenCalledTimes(1);
    expect(hook.result.current.operation?.status).toBe("completed");

    // Ten more intervals, and not one more request: the operation is
    // finished, and a poll that kept running would go on asking the
    // service about it for as long as the page is open.
    await tick(20_000);
    expect(getOperation).toHaveBeenCalledTimes(1);
  });

  it("stops on a failure too, and goes on reporting it as failed", async () => {
    const getOperation = vi.fn().mockResolvedValue(operation("failed"));
    const hook = harness(getOperation);

    await submit(hook, "run-a", operation("running"));
    await tick(2_000);

    expect(hook.result.current.operation?.status).toBe("failed");
    expect(getOperation).toHaveBeenCalledTimes(1);

    await tick(20_000);
    expect(getOperation).toHaveBeenCalledTimes(1);
    // A failed operation is not cleared by the poll stopping: what the
    // page draws is still the failure.
    expect(hook.result.current.operation?.status).toBe("failed");
  });

  it("leaves the last good reading on screen when a poll is refused", async () => {
    const getOperation = vi
      .fn()
      .mockRejectedValueOnce(new Error("the re-read was refused"))
      .mockResolvedValue(operation("completed"));
    const hook = harness(getOperation);

    await submit(hook, "run-a", operation("running"));
    await tick(2_000);

    // Still running, still watched: a durable operation does not stop
    // existing because one re-read failed.
    expect(hook.result.current.operation?.status).toBe("running");
    await tick(2_000);
    expect(hook.result.current.operation?.status).toBe("completed");
  });
});

describe("the pending idempotency key", () => {
  /** Submits one act that records the key it was handed, and either
   *  succeeds or is refused. */
  async function submitRecording(
    hook: ActionHook,
    target: string,
    seen: string[],
    outcome: "ok" | "refused"
  ): Promise<void> {
    await act(async () => {
      hook.result.current.submit(target, ({ idempotencyKey }) => {
        seen.push(idempotencyKey);
        return outcome === "ok"
          ? Promise.resolve({ operation: operation("running"), snapshots: [] })
          : Promise.reject(new Error("refused"));
      });
    });
  }

  it("re-sends the same key for a retry of the same target, and a new one after success", async () => {
    const hook = harness(vi.fn().mockResolvedValue(operation("running")));
    const seen: string[] = [];

    await submitRecording(hook, "run-a", seen, "refused");
    await submitRecording(hook, "run-a", seen, "refused");
    // Two presses against one refusal is one submission retried, which is
    // what the header is for.
    expect(seen[1]).toBe(seen[0]);

    await submitRecording(hook, "run-a", seen, "ok");
    expect(seen[2]).toBe(seen[0]);

    // That act succeeded, so the next press is a new logical submission
    // even on the same target.
    await submitRecording(hook, "run-a", seen, "refused");
    expect(seen[3]).not.toBe(seen[0]);
  });

  it("drops the key when the outcome is dismissed", async () => {
    const hook = harness(vi.fn().mockResolvedValue(operation("running")));
    const seen: string[] = [];

    await submitRecording(hook, "run-a", seen, "refused");
    act(() => hook.result.current.dismiss());
    await submitRecording(hook, "run-a", seen, "refused");

    // The refusal the first key belonged to is off the screen. Pressing
    // again is a fresh ask, not a retry of something nobody can see.
    expect(seen[1]).not.toBe(seen[0]);
  });
});
