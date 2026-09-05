/**
 * How a page reaches the API without being handed one.
 *
 * The provider exists so the choice between `httpApi` and the mock is made
 * once, at the composition root, rather than threaded through every page
 * as a prop. Everything below it just asks.
 */
import { createContext, useContext } from "react";
import type { ReactNode } from "react";
import type { BackupManagerApi } from "./contracts";

const Ctx = createContext<BackupManagerApi | null>(null);

/** Names the one API implementation everything under it will use. The
 *  composition root picks it (app/createApp.tsx), which is what lets the
 *  whole tree render against fixtures without a service behind it. */
export function ApiProvider({ api, children }: { api: BackupManagerApi; children: ReactNode }) {
  return <Ctx.Provider value={api}>{children}</Ctx.Provider>;
}

/**
 * The API for the surrounding tree.
 *
 * Throws rather than answering null, so callers are not written against a
 * possibly-missing API they would then have to branch on forever. A
 * missing provider is a wiring mistake in the composition root, caught the
 * first time any page renders, and it is not something an operator can
 * cause or recover from at runtime.
 */
export function useApi(): BackupManagerApi {
  const api = useContext(Ctx);
  if (!api) throw new Error("useApi must be used inside <ApiProvider>");
  return api;
}
