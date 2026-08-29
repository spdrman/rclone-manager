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

  // eslint-disable-next-line react-hooks/exhaustive-deps
  const run = useCallback(fn, deps);

  useEffect(() => {
    let live = true;
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
