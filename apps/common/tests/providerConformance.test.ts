import { createElement } from "react";
import { act, cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ALL_BRIDGES } from "./bridges";
import { NO_CAPABILITIES } from "@shared/platform/capabilities";
import { PlatformProvider, usePlatform } from "@shared/platform/PlatformContext";
import { readLocalAccountSession } from "@shared/platform/localSession";
import { resetGraphForTests } from "@shared/state/graph";
import type { PlatformBridge } from "@shared/types/platform";
import conformance from "../../../distribution/packaging/conformance.json";

/** §45 — the provider conformance matrix. */
describe("provider conformance", () => {
  it("declares seven distinct providers", () => {
    const ids = ALL_BRIDGES.map((b) => b.id);
    expect(new Set(ids).size).toBe(7);
  });

  for (const bridge of ALL_BRIDGES) {
    describe(bridge.id, () => {
      it("has an id, a human name and an integration kind", () => {
        expect(bridge.id).toBeTruthy();
        expect(bridge.name).toBeTruthy();
        expect(bridge.integration).toBeTruthy();
      });

      it("declares every capability key explicitly", () => {
        const caps = bridge.capabilities();
        for (const key of Object.keys(NO_CAPABILITIES)) {
          expect(typeof caps[key as keyof typeof caps]).toBe("boolean");
        }
      });

      it("only claims native auth when it implements a native session", async () => {
        const caps = bridge.capabilities();
        if (!caps.nativeAuth) {
          // A local-account provider must never report a native session mode.
          const ctx = await bridge.getAuthContext().catch(() => null);
          if (ctx) expect(ctx.mode).toBe("local-account");
        }
      });

      /**
       * Issue #795. Every local-account provider held a byte-identical
       * copy of the session read, and every copy turned ANY refusal into
       * "not signed in" — so a deployment whose web-ui container could
       * not reach the engine told its operator they had been signed out
       * and offered a form that posts down the same broken hop.
       *
       * There is one reader now (ui/shared/src/platform/localSession.ts),
       * and this is what stops a seventh copy being written: identity,
       * not behaviour, because a re-implementation that happened to agree
       * on the three cases a test enumerates would still be the thing
       * that drifts.
       */
      it("reads its session through the one shared reader, when it has no native session", () => {
        if (bridge.capabilities().nativeAuth) return;
        expect(bridge.getAuthContext).toBe(readLocalAccountSession);
      });

      it("only exposes notify() when it claims native notifications", () => {
        const caps = bridge.capabilities();
        expect(Boolean(bridge.notify)).toBe(caps.nativeNotifications);
      });

      /** §22, the same refusal the Go half makes at wiring time
       *  (apps/common/platform/notify.NewPlatformSink): a provider that
       *  CLAIMS native notifications and cannot reach its host binding must
       *  reject, because a resolved promise is how every caller reads
       *  "the operator was notified". */
      it("rejects rather than resolving when its host notification binding is missing", async () => {
        const notify = bridge.notify;
        if (!notify) return;
        await expect(notify.call(bridge, "Backup is stale", "production/pg has no recent backup")).rejects.toThrow();
      });

      it("documents its deployment and storage mount", () => {
        expect(bridge.deployment.label).toBeTruthy();
        expect(bridge.deployment.storageMount.startsWith("/")).toBe(true);
        expect(bridge.deployment.adapterVersion).toBeTruthy();
      });
    });
  }

  /** Keeps the assertion above from being vacuous: if no provider declared
   *  the capability, every bridge would skip it and the suite would still be
   *  green. */
  it("keeps UGOS as the only provider claiming native notifications today", () => {
    const notifying = ALL_BRIDGES.filter((b) => b.capabilities().nativeNotifications).map((b) => b.id);
    expect(notifying).toEqual(["ugos"]);
  });

  it("delivers through the host binding when it IS present", async () => {
    const ugos = ALL_BRIDGES.find((b) => b.id === "ugos");
    if (!ugos?.notify) throw new Error("the ugos bridge no longer exposes notify()");

    const hostNotify = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("ugos", { notify: hostNotify });
    try {
      await expect(ugos.notify("Backup is stale", "production/pg")).resolves.toBeUndefined();
      expect(hostNotify).toHaveBeenCalledWith("Backup is stale", "production/pg");
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("keeps UGOS as the only native-session provider today", () => {
    const native = ALL_BRIDGES.filter((b) => b.capabilities().nativeAuth).map((b) => b.id);
    expect(native).toEqual(["ugos"]);
  });
});

/**
 * Issue #82 (B4.1)'s own TDD requirement: "a failing test that swaps the
 * platform bridge for a local-auth generic-host bridge and asserts the
 * same shared auth node updates, exercised through the same test surface
 * providerConformance.test.ts already uses" (the issue's phrasing;
 * relocated here from src/test/providerConformance.test.tsx by #106).
 *
 * Every other test in this file calls bridge.getAuthContext() directly,
 * in isolation - which proves the generic bridge's own HTTP contract, but
 * NOT that its result actually lands in the one shared `platform.auth`
 * graph node (ui/shared/src/state/platformNodes.ts) every other consumer
 * (usePlatform, capabilityCopyNode, App.tsx's own auth gate) reads. A
 * generic/local-auth bridge that instead grew its own parallel
 * useState/context for "am I signed in" would pass every test above
 * while still being exactly the second, competing state container the
 * EPIC-level constraint (#81) rules out - this is what catches that,
 * by driving the REAL PlatformProvider (ui/shared/src/platform/
 * PlatformContext.tsx) with the real genericBridge object and reading
 * the graph back out through usePlatform(), not a stand-in for either.
 */
describe("generic/local-auth bridge writes through the shared auth graph node", () => {
  const maybeGenericBridge = ALL_BRIDGES.find((b) => b.id === "generic");
  if (!maybeGenericBridge) throw new Error("ALL_BRIDGES has no \"generic\" entry");
  const genericBridge: PlatformBridge = maybeGenericBridge;

  afterEach(() => {
    // Unmount BEFORE resetting the graph, matching
    // ui/shared/src/platform/PlatformContext.test.tsx's own ordering: a
    // still-mounted component reading bridgeNode would otherwise observe
    // the reset commit it back to null mid-render.
    cleanup();
    resetGraphForTests();
    vi.unstubAllGlobals();
  });

  function renderGenericBridge() {
    let latest: ReturnType<typeof usePlatform> | undefined;
    function Reader() {
      latest = usePlatform();
      return null;
    }
    render(createElement(PlatformProvider, { bridge: genericBridge, children: createElement(Reader) }));
    return () => latest;
  }

  // Flushes the microtask chain genericBridge.getAuthContext()'s fetch
  // promise resolves through, and the graph commit each leg of
  // PlatformContext.tsx's refetchAuth makes.
  function flush() {
    return act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  }

  it("commits an authenticated session (from GET /api/v1/auth/session) into the shared platform.auth node", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        json: async () => ({ username: "bm-admin" })
      })
    );

    const getLatest = renderGenericBridge();
    await flush();

    expect(getLatest()?.auth).toEqual({
      authenticated: true,
      username: "bm-admin",
      mode: "local-account"
    });
  });

  it("commits an unauthenticated session, through the SAME shared node, when /api/v1/auth/session is refused", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 401,
        json: async () => ({})
      })
    );

    const getLatest = renderGenericBridge();
    await flush();

    expect(getLatest()?.auth).toEqual({
      authenticated: false,
      username: null,
      mode: "local-account"
    });
  });
});

/**
 * Issue #877 — the workflow/runner/container capability, per provider.
 *
 * This suite is the one place in the repository that imports every
 * provider bridge, and that is exactly why the local-hook question
 * belongs here as well as in distribution/packaging: the two sides see
 * DIFFERENT provider sets. Seven providers ship a bridge; eleven are
 * supported. The four that ship none (Portainer, Dockge, CasaOS, ZimaOS)
 * select the generic runtime profile, so the running process reports
 * generic's capabilities and cannot tell their hosts apart — which is
 * why their answer can only live in a document, and why a check that
 * only ever looked at bridges would report those four as decided when
 * nothing had decided them.
 *
 * The declarations are read from the matrix rather than restated, so this
 * file cannot become a second opinion about which providers support what.
 * The Go half (distribution/packaging/workflowruntime_test.go) pins that
 * same file to apps/common/platform/capabilities, which is the authority.
 */
describe("local workflow hooks, per provider (#877)", () => {
  type RunnerAnswer = { localHooks?: string; platform?: string; doc?: string };
  const answers = conformance.providers as Record<string, { workflowRunner?: RunnerAnswer }>;
  const bridgeIds = ALL_BRIDGES.map((b) => b.id);

  it("answers the question for every provider the frontend knows about", () => {
    for (const id of bridgeIds) {
      const answer = answers[id]?.workflowRunner;
      expect(answer, `no provider ${id} in the conformance matrix`).toBeTruthy();
      expect(["available", "unavailable"]).toContain(answer?.localHooks);
      expect(answer?.doc, `${id} names no document an operator can read`).toBeTruthy();
    }
  });

  /**
   * The cross-side invariant, and the one neither half can check alone: a
   * provider has its OWN runtime answer exactly when it ships a bridge.
   * It fails in both directions that matter — a new bridge whose
   * local-hook row nobody added, and a store adapter quietly given a
   * platform row it cannot deliver, since its deployment reports itself
   * as generic to every consumer.
   */
  it("gives a provider its own platform answer exactly when it ships a bridge", () => {
    const withOwnPlatform = Object.entries(answers)
      .filter(([, p]) => Boolean(p.workflowRunner?.platform))
      .map(([id]) => id)
      .sort();
    expect(withOwnPlatform).toEqual([...bridgeIds].sort());
  });

  /**
   * A provider that claims the capability without a Docker path must
   * fail. On this side "a Docker path" means a platform row of its own
   * whose answer is `available`: a store adapter cannot have one, because
   * nothing it ships can install a host unit or grant a group.
   */
  const claimsLocalHooks = (p: RunnerAnswer | undefined) =>
    p?.localHooks === "available" && Boolean(p.platform);

  it("refuses a claim with no platform behind it", () => {
    // The positive control. Without it, the assertion below would pass
    // just as happily against a helper that always returns true.
    expect(claimsLocalHooks({ localHooks: "available", platform: "generic" })).toBe(true);
    expect(claimsLocalHooks({ localHooks: "available" })).toBe(false);
    expect(claimsLocalHooks({ localHooks: "unavailable", platform: "synology" })).toBe(false);
    expect(claimsLocalHooks(undefined)).toBe(false);
  });

  it("keeps the generic host, OpenMediaVault and Proxmox as the only providers that run them", () => {
    const running = Object.entries(answers)
      .filter(([, p]) => claimsLocalHooks(p.workflowRunner))
      .map(([id]) => id)
      .sort();
    expect(running).toEqual(["generic", "openmediavault", "proxmox"]);
  });

  /**
   * §22, applied to the capability model itself. Local-hook readiness is
   * a HOST fact: it is decided by a systemd unit that is not this process
   * and not this browser, so it must never appear in the browser-host
   * capability set every bridge declares and GET
   * /api/v1/system/capabilities reports. Adding it there would be the
   * emulated claim apps/common/platform/profile's
   * UndeliverableCapabilities already refuses server-side — a flag the
   * deployment reporting it cannot deliver.
   */
  it("declares no local-hook flag in the browser-host capability model", () => {
    for (const bridge of ALL_BRIDGES) {
      const keys = Object.keys(bridge.capabilities());
      expect(keys.sort()).toEqual(Object.keys(NO_CAPABILITIES).sort());
      for (const key of keys) {
        expect(key.toLowerCase()).not.toMatch(/hook|docker|runner|workflow/);
      }
    }
  });
});
