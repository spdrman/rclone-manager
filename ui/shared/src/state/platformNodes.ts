import type { AuthContext, PlatformBridge } from "@shared/types/platform";
import { describeCapabilities } from "@shared/platform/capabilities";
import type { CapabilityCopy } from "@shared/platform/capabilities";
import { graph, registerInput } from "./graph";

/**
 * Platform state, as graph nodes (EPIC B — causl-ts migration of
 * PlatformContext.tsx). This used to be `createContext` + `useState` +
 * `useEffect` with a hand-rolled `nonce` refetch counter; it is now three
 * input nodes plus a derived one, all on the app's single shared graph.
 *
 * The bridge itself becomes an input node (not left in React context)
 * specifically so capabilityCopyNode can derive from it: a `derived`
 * compute can only read other graph nodes, not React context.
 */

/** The platform bridge for this running app instance. `null` until
 *  `PlatformProvider` mounts and commits it — a provider shell supplies
 *  exactly one bridge for the app's lifetime (apps/<id>/frontend/bootstrap.tsx). */
export const bridgeNode = registerInput<PlatformBridge | null>("platform.bridge", null);

/** The signed-in identity, or null before the first auth check resolves. */
export const authNode = registerInput<AuthContext | null>("platform.auth", null);

/** True until the first getAuthContext() call settles (success or
 *  failure) — the splash-screen gate in App.tsx. */
export const authLoadingNode = registerInput<boolean>("platform.authLoading", true);

/** Honest capability copy for the Settings page (§22 — never claim an
 *  unsupported capability). Pure function of the bridge, so it is a
 *  derived node rather than something recomputed by hand on every render. */
export const capabilityCopyNode = graph.derived<CapabilityCopy[]>("platform.capabilityCopy", (get) => {
  const bridge = get(bridgeNode);
  return bridge ? describeCapabilities(bridge.capabilities(), bridge.name) : [];
});
