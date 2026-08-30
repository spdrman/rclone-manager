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

/**
 * B2.4 — BackupDetailPage's single-artifact read, moved onto the graph the
 * same way as the four resources above. Nothing else fetches this
 * particular artifact today, so this is not eliminating a duplicate fetch
 * (unlike setsNode); it is putting the read on the same mechanism as
 * everything else app-wide state lives on, so it is testable the same way
 * (commit to the node, read it back) instead of being page-local
 * `useAsync` state.
 */
export const artifactDetailNode = createResourceNode<BackupArtifact>("app.artifactDetail");

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
