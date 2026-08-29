import { createContext, useContext } from "react";
import type { ReactNode } from "react";
import type { BackupManagerApi } from "./contracts";

const Ctx = createContext<BackupManagerApi | null>(null);

export function ApiProvider({ api, children }: { api: BackupManagerApi; children: ReactNode }) {
  return <Ctx.Provider value={api}>{children}</Ctx.Provider>;
}

export function useApi(): BackupManagerApi {
  const api = useContext(Ctx);
  if (!api) throw new Error("useApi must be used inside <ApiProvider>");
  return api;
}
