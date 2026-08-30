import type { BackupArtifact, BackupSet } from "@shared/types/backup";
import type { Operation, SystemHealth, VersionInfo } from "@shared/types/operation";
import { graph } from "./graph";
import { createResourceNode } from "./resource";

/**
 * App.tsx's four independent fetches, as graph inputs (EPIC B — causl-ts
 * migration). `counts` and `readOnly` were a `useMemo` and an inline
 * boolean; they are now `derived()` nodes computed from these same four
 * inputs, so anything else that needs them can read the graph directly
 * instead of receiving them as props threaded down from App().
 */
export const healthNode = createResourceNode<SystemHealth>("app.health");
export const versionNode = createResourceNode<VersionInfo>("app.version");
export const setsNode = createResourceNode<BackupSet[]>("app.sets");
export const quarantineNode = createResourceNode<BackupArtifact[]>("app.quarantine");

/**
 * B2.1 (#95) — the durable-operation-polling equivalent of the four nodes
 * above. `DashboardPage` and `BackupSetsPage` (#97) each used to run their
 * own `useAsync(() => api.listOperations())`, so the two could disagree
 * with each other and neither updated without its own re-fetch. `App.tsx`
 * is this node's one fetch owner (`useResource`, folded into the existing
 * 30s `reloadAll` poll, same as healthNode/setsNode); every page that
 * needs live operation progress reads this node directly via `useCausl`,
 * exactly like quarantineNode is fetched once and meant to be read
 * directly rather than re-fetched per page. */
export const operationsNode = createResourceNode<Operation[]>("app.operations");

export interface AppCounts {
  sets: number | undefined;
  backups: number | undefined;
  quarantine: number | undefined;
}

export const countsNode = graph.derived<AppCounts>("app.counts", (get) => ({
  sets: get(setsNode).data?.length,
  backups: get(healthNode).data?.retainedCount,
  quarantine: get(quarantineNode).data?.length
}));

/** §38 — an incompatible service disables every management action but
 *  leaves read-only information visible. */
export const readOnlyNode = graph.derived<boolean>("app.readOnly", (get) => {
  const version = get(versionNode).data;
  return version ? !version.compatible : false;
});
