import { useCallback, useEffect } from "react";
import type { InputNode } from "@causlts/core";
import { BackupManagerError } from "@shared/api/contracts";
import type { ApiError } from "@shared/api/contracts";
import { graph, useCausl } from "./graph";

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
 *  twice for the same id throws DuplicateNodeError. */
export function createResourceNode<T>(id: string): InputNode<ResourceState<T>> {
  return graph.input<ResourceState<T>>(id, { data: null, error: null, loading: true });
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

/** Runs `fetchFn`, committing the loading/resolved/failed phases to
 *  `node`. Exported separately from useResource so `reload()` can be
 *  called from outside a render (e.g. after a mutation elsewhere calls
 *  `sets.reload()`, exactly as it did with useAsync). */
export function fetchResource<T>(node: InputNode<ResourceState<T>>, fetchFn: () => Promise<T>): void {
  graph.commit(node.id + "/loading", (tx) =>
    tx.set(node, { ...graph.read(node), loading: true, error: null })
  );
  fetchFn()
    .then((data) => {
      graph.commit(node.id + "/resolved", (tx) => tx.set(node, { data, error: null, loading: false }));
    })
    .catch((e: unknown) => {
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
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const run = useCallback(fetchFn, deps);
  const reload = useCallback(() => fetchResource(node, run), [node, run]);

  useEffect(() => {
    reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [reload]);

  return { ...state, reload };
}
