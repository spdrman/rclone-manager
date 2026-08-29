import type { BackupArtifact, BackupSet } from "@shared/types/backup";
import type { SystemHealth, VersionInfo } from "@shared/types/operation";
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
