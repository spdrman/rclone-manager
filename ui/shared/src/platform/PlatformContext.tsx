/**
 * The provider that puts the bridge and the signed-in identity onto the
 * graph, and the hook the rest of the app reads them back through.
 *
 * There is no React context here despite the name, and that is the point:
 * the bridge has to be readable by a derived node (capabilityCopyNode),
 * and a derived compute can only see other nodes. The name stayed because
 * every import site says what it means.
 *
 * Three things in this file look like they could be simpler and cannot,
 * and each carries its own note at the point it happens. The bridge is
 * committed during render rather than in an effect, because an effect runs
 * after children have already rendered once and a child asking on that
 * first pass would find nothing. The guard against re-committing is local
 * state rather than a read-back comparison, because the engine may swap
 * its own backend underneath and reference stability across that swap is
 * not promised. And the hook's return value is memoised, because a fresh
 * object every render is invisible until the first person puts
 * `refreshAuth` in a dependency array and gets an infinite refetch with
 * nothing pointing at this file.
 */
import { useCallback, useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import type { AuthContext as AuthCtx, PlatformBridge } from "@shared/types/platform";
import { graph, useCausl } from "@shared/state/graph";
import { asApiError } from "@shared/api/failure";
import { onSessionLost } from "@shared/api/sessionLoss";
import type { ApiError } from "@shared/api/contracts";
import {
  authErrorNode,
  authLoadingNode,
  authNode,
  bridgeNode,
  capabilityCopyNode
} from "@shared/state/platformNodes";
import type { CapabilityCopy } from "./capabilities";

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
      if (isCurrent())
        graph.commit("platform/auth-resolved", (tx) => {
          tx.set(authNode, ctx);
          tx.set(authErrorNode, null);
        });
    })
    .catch((e: unknown) => {
      // Issue #795. This used to commit `{ authenticated: false }` for
      // ANY rejection, which is a verdict about the operator's session
      // drawn from a failure that never asked one. On the reported
      // deployment the engine was unreachable from the web-ui container,
      // so the session route answered 502 and the operator was shown a
      // sign-in form — the one action that could not possibly work —
      // while their session was in fact untouched.
      //
      // A bridge that CAN tell now does: readLocalAccountSession
      // resolves `{ authenticated: false }` for the 401/403 that means
      // it, and rejects for everything else. So a rejection reaching
      // here is "the check could not be made", and is recorded as that.
      // authNode stays unauthenticated because nothing here may claim a
      // session either, and App.tsx reads the two together: an error
      // beside an unauthenticated context is a service that did not
      // answer, not a browser that is signed out.
      if (isCurrent())
        graph.commit("platform/auth-failed", (tx) => {
          tx.set(authNode, { authenticated: false, username: null, mode: "local-account" });
          tx.set(authErrorNode, asApiError(e));
        });
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
    // Issue #795. A page read refused with UNAUTHENTICATED means the
    // session this app is holding is gone - most often because the
    // engine restarted, which ends every session it was keeping
    // (apps/common/auth/local). Re-asking is the whole response: the
    // answer is "not signed in", App.tsx's own gate then renders the
    // sign-in form, and the operator has the one route out that a Try
    // again beside a page panel could never be.
    //
    // Gated on currently believing there IS a session. Without that,
    // every refused read on a browser that is already at the login page
    // (App.tsx issues the four app-wide reads above its authenticated
    // branch, by design - see signed-in-refetch.test.tsx) would ask the
    // same question again and get the same answer.
    const unsubscribe = onSessionLost(() => {
      if (!graph.read(authNode)?.authenticated) return;
      refetchAuth(bridge, () => live);
    });
    return () => {
      live = false;
      unsubscribe();
    };
  }, [bridge]);

  return <>{children}</>;
}

export function usePlatform(): {
  bridge: PlatformBridge;
  auth: AuthCtx | null;
  /** Why the auth check could not be made, when it could not be made at
   *  all (#795). Never set for a browser the service said is signed out:
   *  that is an answer, and it is in `auth`. */
  authError: ApiError | null;
  authLoading: boolean;
  capabilityCopy: CapabilityCopy[];
  refreshAuth(): void;
} {
  const bridge = useCausl(bridgeNode);
  const auth = useCausl(authNode);
  const authError = useCausl(authErrorNode);
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
    () => ({ bridge, auth, authError, authLoading, capabilityCopy, refreshAuth }),
    [bridge, auth, authError, authLoading, capabilityCopy, refreshAuth]
  );
}

export function useCapabilities() {
  return usePlatform().bridge.capabilities();
}
