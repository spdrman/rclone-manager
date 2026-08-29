import { useCallback, useRef, useSyncExternalStore } from "react";
import type { Graph, Node } from "@causlts/core";

/**
 * Builds the one binding between React and a causl graph. Call this once,
 * against the app's single shared graph (see graph.ts, which does exactly
 * that and re-exports the bound `useCausl`) — every component that reads
 * graph state goes through THAT hook, never `graph.read()` directly in a
 * render body, and never a hand-rolled `graph.subscribe()` in a
 * `useEffect`.
 *
 * A factory rather than a hook that closes over a module-level graph
 * directly, so the binding logic itself stays a plain, independently
 * testable unit against any graph — see useCausl.test.tsx, which builds
 * throwaway graphs rather than importing the app singleton.
 *
 * Built on `useSyncExternalStore` because the graph is external, mutable,
 * synchronous state, which is exactly the case that hook exists for: it
 * gives tearing-free reads under concurrent rendering, so two components
 * reading the SAME node during the SAME commit always observe the SAME
 * value, even if a commit lands between their two render passes.
 *
 * Gotcha this hook exists to absorb (see the causl skill,
 * read-identity-migration): `graph.read()` is not contractually
 * guaranteed to return the same object reference twice for an
 * object-valued node, only the same VALUE at a fixed GraphTime.
 * `useSyncExternalStore` requires `getSnapshot` to return a stable
 * reference when nothing changed, or it re-renders forever. So the
 * returned hook caches the last read value keyed on `graph.now` (the
 * graph's single global GraphTime clock — every commit, on any node,
 * advances it by exactly one) and only calls `graph.read(node)` again
 * once `graph.now` has moved past the cached read. It never compares two
 * `read()` results with `===`.
 */
export function createCauslHook(graph: Graph) {
  return function useCausl<T>(node: Node<T>): T {
    const cacheRef = useRef<{ time: number; value: T } | null>(null);

    // useSyncExternalStore resubscribes whenever `subscribe`'s identity
    // changes. Both callbacks were plain function expressions before,
    // recreated on every render of every consumer of this one shared
    // hook — which meant every render tore down and re-established its
    // graph subscription for no reason. Keyed on [node] (via graph, which
    // is fixed per createCauslHook call) so identity only changes when the
    // node being read changes.
    const getSnapshot = useCallback((): T => {
      const cache = cacheRef.current;
      if (cache !== null && cache.time === graph.now) {
        return cache.value;
      }
      const value = graph.read(node);
      cacheRef.current = { time: graph.now, value };
      return value;
    }, [node]);

    const subscribe = useCallback(
      (onStoreChange: () => void) => graph.subscribe(node, onStoreChange),
      [node]
    );

    return useSyncExternalStore(subscribe, getSnapshot);
  };
}
