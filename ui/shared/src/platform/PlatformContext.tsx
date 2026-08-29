import { createContext, useContext, useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import type { AuthContext as AuthCtx, PlatformBridge } from "@shared/types/platform";
import { describeCapabilities } from "./capabilities";

interface PlatformValue {
  bridge: PlatformBridge;
  auth: AuthCtx | null;
  authLoading: boolean;
  capabilityCopy: ReturnType<typeof describeCapabilities>;
  refreshAuth(): void;
}

const Ctx = createContext<PlatformValue | null>(null);

export function PlatformProvider({
  bridge,
  children
}: {
  bridge: PlatformBridge;
  children: ReactNode;
}) {
  const [auth, setAuth] = useState<AuthCtx | null>(null);
  const [authLoading, setAuthLoading] = useState(true);
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    let live = true;
    setAuthLoading(true);
    bridge
      .getAuthContext()
      .then((ctx) => {
        if (live) setAuth(ctx);
      })
      .catch(() => {
        if (live)
          setAuth({ authenticated: false, username: null, mode: "local-account" });
      })
      .finally(() => {
        if (live) setAuthLoading(false);
      });
    return () => {
      live = false;
    };
  }, [bridge, nonce]);

  const value = useMemo<PlatformValue>(
    () => ({
      bridge,
      auth,
      authLoading,
      capabilityCopy: describeCapabilities(bridge.capabilities(), bridge.name),
      refreshAuth: () => setNonce((n) => n + 1)
    }),
    [bridge, auth, authLoading]
  );

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function usePlatform(): PlatformValue {
  const v = useContext(Ctx);
  if (!v) throw new Error("usePlatform must be used inside <PlatformProvider>");
  return v;
}

export function useCapabilities() {
  return usePlatform().bridge.capabilities();
}
