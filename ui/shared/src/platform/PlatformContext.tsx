import { useEffect } from "react";
import type { ReactNode } from "react";
import type { AuthContext as AuthCtx, PlatformBridge } from "@shared/types/platform";
import { graph, useCausl } from "@shared/state/graph";
import { authLoadingNode, authNode, bridgeNode, capabilityCopyNode } from "@shared/state/platformNodes";
import { describeCapabilities } from "./capabilities";

/** Runs (and re-runs, on refreshAuth()) the one auth fetch, committing its
 *  three phases (loading, resolved-or-failed, settled) to the graph. Kept
 *  as a plain function, not a hook, so `refreshAuth()` can call it directly
 *  instead of going through a `nonce` counter and a dependent effect. */
function refetchAuth(bridge: PlatformBridge, isLive: () => boolean) {
  graph.commit("platform/auth-loading", (tx) => tx.set(authLoadingNode, true));

  bridge
    .getAuthContext()
    .then((ctx) => {
      if (isLive()) graph.commit("platform/auth-resolved", (tx) => tx.set(authNode, ctx));
    })
    .catch(() => {
      if (isLive())
        graph.commit("platform/auth-failed", (tx) =>
          tx.set(authNode, { authenticated: false, username: null, mode: "local-account" })
        );
    })
    .finally(() => {
      if (isLive()) graph.commit("platform/auth-settled", (tx) => tx.set(authLoadingNode, false));
    });
}

export function PlatformProvider({
  bridge,
  children
}: {
  bridge: PlatformBridge;
  children: ReactNode;
}) {
  // Committed synchronously during render, not in an effect: an effect
  // runs after children have already rendered once, so a child calling
  // usePlatform() on that first pass would see bridgeNode still `null`.
  // Guarded by the read so a second render (StrictMode's double-invoke,
  // or an unrelated re-render) with the same bridge is a no-op.
  if (graph.read(bridgeNode) !== bridge) {
    graph.commit("platform/bridge-mounted", (tx) => tx.set(bridgeNode, bridge));
  }

  useEffect(() => {
    let live = true;
    refetchAuth(bridge, () => live);
    return () => {
      live = false;
    };
  }, [bridge]);

  return <>{children}</>;
}

export function usePlatform(): {
  bridge: PlatformBridge;
  auth: AuthCtx | null;
  authLoading: boolean;
  capabilityCopy: ReturnType<typeof describeCapabilities>;
  refreshAuth(): void;
} {
  const bridge = useCausl(bridgeNode);
  const auth = useCausl(authNode);
  const authLoading = useCausl(authLoadingNode);
  const capabilityCopy = useCausl(capabilityCopyNode);

  if (!bridge) {
    throw new Error("usePlatform must be used inside <PlatformProvider>");
  }

  return {
    bridge,
    auth,
    authLoading,
    capabilityCopy,
    refreshAuth: () => refetchAuth(bridge, () => true)
  };
}

export function useCapabilities() {
  return usePlatform().bridge.capabilities();
}
