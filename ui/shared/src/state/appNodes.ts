import type { BackupArtifact, BackupSet, RetentionPlan } from "@shared/types/backup";
import type { Operation, SystemHealth, VersionInfo } from "@shared/types/operation";
import { graph, registerInput } from "./graph";
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

/**
 * B3.1 (#96) — the retention plan RetentionPreviewDialog is currently
 * showing, as a graph resource node rather than page-local `useAsync`
 * state. This is what lets retentionPlanStaleNode below compare the plan
 * against the graph's OWN evidence instead of only ever reading a `stale`
 * field the server handed over (issue #96's "causl-ts for staleness, not a
 * boolean parsed off the response").
 */
export const retentionPlanNode = createResourceNode<RetentionPlan>("retention.plan");

export interface RetentionRevisions {
  inventoryRevision: string;
  configRevision: string;
}

/**
 * What this graph has itself most recently observed as the previewed
 * backup set's committed inventory_revision/config_revision — independent
 * of what retentionPlanNode's own captured revisions say. `null` until a
 * preview has resolved at least once.
 *
 * Seeded to match a freshly-read plan's own revisions the moment that plan
 * is committed (a plan is never stale the instant it is read — see
 * commitRetentionRevisions's call site, RetentionPreviewDialog.tsx), and
 * moved forward independently of retentionPlanNode by anything else that
 * later learns the true revisions changed — a re-preview via "Review new
 * plan", or (future wiring) a live poll/push. That gap between the two is
 * exactly what retentionPlanStaleNode below asserts on.
 */
export const retentionRevisionsNode = registerInput<RetentionRevisions | null>(
  "retention.revisions",
  null
);

/** Commits a freshly observed inventory_revision/config_revision pair —
 *  the one write path for retentionRevisionsNode, so every caller (the
 *  dialog seeding its baseline, a test simulating an external inventory
 *  change) goes through the same intent name. */
export function commitRetentionRevisions(revisions: RetentionRevisions): void {
  graph.commit("retention/revisions", (tx) => tx.set(retentionRevisionsNode, revisions));
}

/**
 * "Is the plan RetentionPreviewDialog is showing stale" — derived by
 * comparing retentionPlanNode's own captured inventory_revision/
 * config_revision against retentionRevisionsNode's current values, NOT by
 * reading a boolean off the wire (there isn't one — see RetentionPlan's
 * own doc, types/backup.ts). `current === null` means nothing has been
 * observed independently of the plan itself yet, so there is no evidence
 * of staleness to assert: false, not true.
 *
 * This is the node issue #96's own required TDD case exercises directly:
 * committing a changed inventory_revision into retentionRevisionsNode
 * (simulating the graph learning of a real inventory change) flips this to
 * true, and the dialog's apply button disables from that alone — before
 * any apply request ever reaches the API.
 */
export const retentionPlanStaleNode = graph.derived<boolean>("retention.planStale", (get) => {
  const plan = get(retentionPlanNode).data;
  const current = get(retentionRevisionsNode);
  if (!plan || !current) return false;
  return (
    current.inventoryRevision !== plan.inventoryRevision ||
    current.configRevision !== plan.configRevision
  );
});
