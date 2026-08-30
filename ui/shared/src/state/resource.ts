import { useCallback, useEffect, useMemo } from "react";
import type { InputNode } from "@causlts/core";
import { BackupManagerError } from "@shared/api/contracts";
import type { ApiError } from "@shared/api/contracts";
import { graph, registerInput, useCausl } from "./graph";

/** Mirrors useAsync's AsyncState<T> shape, so pages that already accept an
 *  AsyncState<T> prop (BackupSetsPage, DashboardPage, ...) do not change
 *  when the thing feeding them becomes a graph node instead of React state. */
export interface ResourceState<T> {
  data: T | null;
  error: ApiError | null;
  loading: boolean;
}

/** Registers one fetched-resource input node. Call exactly once, at module
 *  load (see appNodes.ts) — like every `graph.input` call, calling this
 *  twice for the same id throws DuplicateNodeError. Goes through
 *  `registerInput` (not `graph.input` directly) so `resetGraphForTests()`
 *  knows how to put it back. */
export function createResourceNode<T>(id: string): InputNode<ResourceState<T>> {
  return registerInput<ResourceState<T>>(id, { data: null, error: null, loading: true });
}

function toApiError(e: unknown): ApiError {
  return e instanceof BackupManagerError
    ? e.api
    : {
        code: "unknown",
        message: "Backup Manager could not complete that request.",
        correlationId: "unavailable"
      };
}

/** Tracks the sequence number of the most recently issued fetch per node,
 *  so an out-of-order resolution can tell it is stale and drop itself
 *  instead of overwriting a newer response. This is the guard useAsync had
 *  via a `live` flag tied to effect cleanup, which did not carry over when
 *  fetchResource replaced it — see the mandatory-review fix for the
 *  stale-response race. A plain counter, not an AbortController: fetchFn
 *  is a bare `() => Promise<T>` with no signal to thread through, and
 *  dropping a late response is all the contract here promises (it does
 *  not cancel the in-flight request itself). */
const latestSeqByNode = new WeakMap<InputNode<unknown>, number>();

/** Runs `fetchFn`, committing the loading/resolved/failed phases to
 *  `node`. Exported separately from useResource so `reload()` can be
 *  called from outside a render (e.g. after a mutation elsewhere calls
 *  `sets.reload()`, exactly as it did with useAsync). */
export function fetchResource<T>(node: InputNode<ResourceState<T>>, fetchFn: () => Promise<T>): void {
  const key = node as InputNode<unknown>;
  const seq = (latestSeqByNode.get(key) ?? 0) + 1;
  latestSeqByNode.set(key, seq);
  const isLatest = () => latestSeqByNode.get(key) === seq;

  graph.commit(node.id + "/loading", (tx) =>
    tx.set(node, { ...graph.read(node), loading: true, error: null })
  );
  fetchFn()
    .then((data) => {
      // Two fetches to the same node can resolve out of order (a 30s poll
      // tick overlapping a manual reload, or a post-mutation reload racing
      // the poll); only the response to the LAST call issued is allowed to
      // land, never whichever happens to resolve last.
      if (!isLatest()) return;
      graph.commit(node.id + "/resolved", (tx) => tx.set(node, { data, error: null, loading: false }));
    })
    .catch((e: unknown) => {
      if (!isLatest()) return;
      graph.commit(node.id + "/failed", (tx) =>
        tx.set(node, { ...graph.read(node), error: toApiError(e), loading: false })
      );
    });
}

/** Reads a resource node reactively and fetches it on mount / whenever
 *  `deps` changes — the graph-backed replacement for useAsync. `deps`
 *  works exactly like useAsync's: it is NOT allowed to change on every
 *  render (pass `[api]`, not an inline arrow function's own identity). */
export function useResource<T>(
  node: InputNode<ResourceState<T>>,
  fetchFn: () => Promise<T>,
  deps: unknown[] = []
): ResourceState<T> & { reload(): void } {
  const state = useCausl(node);
  // This wrapper's whole job is forwarding a caller-supplied deps array
  // to useCallback, which the newer react-hooks/use-memo rule can't
  // statically verify (it requires a literal array so it can compare
  // entries itself). Each call site below already lists its own real
  // dependencies in `deps`; this hook has nothing more specific to add.
  // eslint-disable-next-line react-hooks/use-memo, react-hooks/exhaustive-deps
  const run = useCallback(fetchFn, deps);
  const reload = useCallback(() => fetchResource(node, run), [node, run]);

  useEffect(() => {
    reload();
  }, [reload]);

  // `state` (useCausl) and `reload` (useCallback above) are both already
  // referentially stable when nothing changed — useCausl caches its read
  // by graph.now (see useCausl.ts), and reload's own deps (node, run) only
  // change when the caller's `deps` array does. Without this useMemo,
  // though, `{ ...state, reload }` was a fresh object literal on every
  // call regardless, so a caller like App.tsx's `reloadAll` — a
  // useCallback keyed on health/sets/operations — churned identity on
  // every render, tearing down and rebuilding usePolling's setInterval
  // instead of letting it run for a full 30s (mandatory review, PR #143).
  return useMemo(() => ({ ...state, reload }), [state, reload]);
}
