import { createElement } from "react";
import { act, cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ALL_BRIDGES } from "./bridges";
import { NO_CAPABILITIES } from "@shared/platform/capabilities";
import { PlatformProvider, usePlatform } from "@shared/platform/PlatformContext";
import { resetGraphForTests } from "@shared/state/graph";
import type { PlatformBridge } from "@shared/types/platform";

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
