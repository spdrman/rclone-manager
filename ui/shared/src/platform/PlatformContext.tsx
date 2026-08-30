import { useCallback, useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import type { AuthContext as AuthCtx, PlatformBridge } from "@shared/types/platform";
import { graph, useCausl } from "@shared/state/graph";
import { authLoadingNode, authNode, bridgeNode, capabilityCopyNode } from "@shared/state/platformNodes";
import { describeCapabilities } from "./capabilities";

/** Guards against the stale-response race: two auth fetches (the mount
 *  effect and a manual refreshAuth(), or two refreshAuth() calls in a row)
 *  can resolve out of order, and only the response to the LAST one issued
 *  is allowed to land. A plain module-level counter is enough — there is
 *  only ever one auth per app, unlike resource.ts's per-node WeakMap. */
let authRequestSeq = 0;

/** Runs (and re-runs, on refreshAuth()) the one auth fetch, committing its
 *  three phases (loading, resolved-or-failed, settled) to the graph. Kept
 *  as a plain function, not a hook, so `refreshAuth()` can call it directly
 *  instead of going through a `nonce` counter and a dependent effect.
 *  `isLive` additionally gates on the calling PlatformProvider instance
 *  still being mounted (the effect-cleanup case); `isCurrent` below folds
 *  that together with "no newer refetchAuth call has been issued since". */
function refetchAuth(bridge: PlatformBridge, isLive: () => boolean) {
  const seq = ++authRequestSeq;
  const isCurrent = () => isLive() && seq === authRequestSeq;

  graph.commit("platform/auth-loading", (tx) => tx.set(authLoadingNode, true));

  bridge
    .getAuthContext()
    .then((ctx) => {
      if (isCurrent()) graph.commit("platform/auth-resolved", (tx) => tx.set(authNode, ctx));
    })
    .catch(() => {
      if (isCurrent())
        graph.commit("platform/auth-failed", (tx) =>
          tx.set(authNode, { authenticated: false, username: null, mode: "local-account" })
        );
    })
    .finally(() => {
      if (isCurrent()) graph.commit("platform/auth-settled", (tx) => tx.set(authLoadingNode, false));
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
  //
  // Guarded against local state, NOT `graph.read(bridgeNode) !== bridge`:
  // @causlts/core's `createCausl()` wraps every graph in an auto-adapt
  // layer that does a live, in-place swap to a WASM backend once
  // commit/subscriber/timing thresholds trip, and `graph.read()`'s
  // reference stability is not contractually guaranteed to survive that
  // (see useCausl.ts's read-identity comment). Comparing against it here
  // would risk firing on every render post-swap, cascading a derived
  // recompute and a re-render through every usePlatform() consumer while
  // itself feeding the very commit-count stats that trigger the
  // migration — exactly the trap useCausl.ts was written to avoid.
  //
  // useState, not useRef: this is React's own documented "adjusting state
  // when a prop changes" pattern (a set function called during render is
  // explicitly safe there, since React re-renders immediately with the
  // new value before committing anything to the screen). A ref mutated
  // during render is not safe the same way — a render React discards
  // (StrictMode's double-invoke, an interrupted concurrent update) leaves
  // the mutation applied anyway, since nothing about a ref is tied to
  // whether its owning render actually commits. That's the same class of
  // bug this whole guard exists to avoid, just moved from the graph to
  // this component's own local state, and it doesn't survive eslint's
  // react-hooks/refs rule either.
  const [lastCommittedBridge, setLastCommittedBridge] = useState<PlatformBridge | null>(null);
  if (lastCommittedBridge !== bridge) {
    graph.commit("platform/bridge-mounted", (tx) => tx.set(bridgeNode, bridge));
    setLastCommittedBridge(bridge);
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

  const refreshAuth = useCallback(() => refetchAuth(bridge, () => true), [bridge]);

  // Referential stability restored (it existed before the causl
  // migration, via useMemo, and was lost when this was rewritten): a
  // fresh object and a fresh refreshAuth closure on every render is
  // invisible today, but it is a live trap for a future page author who
  // puts refreshAuth in a useEffect/useCallback dependency array per
  // standard React hook hygiene, and gets an infinite refetch loop with
  // no reason to suspect usePlatform() itself.
  return useMemo(
    () => ({ bridge, auth, authLoading, capabilityCopy, refreshAuth }),
    [bridge, auth, authLoading, capabilityCopy, refreshAuth]
  );
}

export function useCapabilities() {
  return usePlatform().bridge.capabilities();
}
