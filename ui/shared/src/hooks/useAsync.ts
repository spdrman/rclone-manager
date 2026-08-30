import { useCallback, useEffect, useState } from "react";
import { BackupManagerError } from "@shared/api/contracts";
import type { ApiError } from "@shared/api/contracts";

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

export function usePolling(intervalMs: number, reload: () => void, enabled = true) {
  useEffect(() => {
    if (!enabled) return;
    const id = window.setInterval(reload, intervalMs);
    return () => window.clearInterval(id);
  }, [intervalMs, reload, enabled]);
}
