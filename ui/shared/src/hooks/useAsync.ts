/**
 * Fetch-into-component-state, for the pages whose data nothing else reads.
 *
 * The graph-backed `useResource` (state/resource.ts) superseded this for
 * anything shared, and deliberately kept the same `AsyncState<T>` shape so
 * the two can sit side by side. What did not survive the move is the
 * ownership question, and it is the only thing worth deciding when picking
 * between them: a fetch belongs on the graph when a second surface needs
 * the answer, and belongs here when it does not. Several pages still
 * genuinely do not, and moving them would add a globally-named node for no
 * reader.
 *
 * Both eslint suppressions below are the same underlying situation. This
 * is a wrapper around a caller-supplied dependency list, and the newer
 * rules want to see a literal array they can compare themselves, which a
 * wrapper by definition cannot show them.
 */
import { useCallback, useEffect, useState } from "react";
import { BackupManagerError } from "@shared/api/contracts";
import type { ApiError } from "@shared/api/contracts";

/** A fetch in one of its three states, plus the way to run it again.
 *  `data` survives a later failure on purpose: a poll that fails should
 *  leave the last good answer on screen under an error, not blank the page
 *  an operator was reading. */
export interface AsyncState<T> {
  data: T | null;
  error: ApiError | null;
  loading: boolean;
  reload(): void;
}

/** One place that turns a rejected promise into a typed, displayable error.
 *  No component ever touches a raw exception. */
export function useAsync<T>(fn: () => Promise<T>, deps: unknown[] = []): AsyncState<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [loading, setLoading] = useState(true);
  const [nonce, setNonce] = useState(0);

  // This wrapper's whole job is forwarding a caller-supplied deps array
  // to useCallback, which the newer react-hooks/use-memo rule can't
  // statically verify (it requires a literal array so it can compare
  // entries itself). Each call site below already lists its own real
  // dependencies in `deps`; this hook has nothing more specific to add.
  // eslint-disable-next-line react-hooks/use-memo, react-hooks/exhaustive-deps
  const run = useCallback(fn, deps);

  useEffect(() => {
    let live = true;
    // Resetting loading/error at the start of a fetch (including a
    // reload triggered by `nonce` changing) is the standard shape of this
    // pattern; the newer react-hooks/set-state-in-effect rule flags any
    // synchronous setState call here on general principle (an extra
    // render pass), but there's no external system or prop to derive
    // this from instead, and no correctness issue: `live` still guards
    // every subsequent setState against a stale/unmounted effect.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setLoading(true);
    setError(null);
    run()
      .then((value) => live && setData(value))
      .catch((e: unknown) => {
        if (!live) return;
        setError(
          e instanceof BackupManagerError
            ? e.api
            : {
                code: "unknown",
                message: "Backup Manager could not complete that request.",
                correlationId: "unavailable"
              }
        );
      })
      .finally(() => live && setLoading(false));
    return () => {
      live = false;
    };
  }, [run, nonce]);

  return { data, error, loading, reload: () => setNonce((n) => n + 1) };
}

/**
 * Calls `reload` on an interval while `enabled`.
 *
 * `reload` is a dependency, so an unstable one restarts the timer on every
 * render and the interval effectively never elapses. That is not
 * hypothetical: it is the bug state/resource.ts's own identity memo exists
 * to prevent, and it fails silently as "the dashboard stopped refreshing"
 * rather than as anything a test would notice on its own.
 *
 * `enabled` gates the timer rather than the caller, so a page can express
 * "not until the operator is signed in and this instance is configured"
 * without arranging for the hook to be called conditionally, which React
 * forbids anyway.
 */
export function usePolling(intervalMs: number, reload: () => void, enabled = true) {
  useEffect(() => {
    if (!enabled) return;
    const id = window.setInterval(reload, intervalMs);
    return () => window.clearInterval(id);
  }, [intervalMs, reload, enabled]);
}
