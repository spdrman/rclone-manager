import { describe, expect, it } from "vitest";
import { ALL_BRIDGES } from "./bridges";
import { NO_CAPABILITIES } from "@shared/platform/capabilities";

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

      it("documents its deployment and storage mount", () => {
        expect(bridge.deployment.label).toBeTruthy();
        expect(bridge.deployment.storageMount.startsWith("/")).toBe(true);
        expect(bridge.deployment.adapterVersion).toBeTruthy();
      });
    });
  }

  it("keeps UGOS as the only native-session provider today", () => {
    const native = ALL_BRIDGES.filter((b) => b.capabilities().nativeAuth).map((b) => b.id);
    expect(native).toEqual(["ugos"]);
  });
});
