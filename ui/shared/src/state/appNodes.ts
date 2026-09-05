/**
 * The state the whole app shares: what the service says about its health
 * and version, the backup sets, the quarantine, the running operations,
 * whether this instance is configured at all, and the retention plan a
 * dialog is currently showing.
 *
 * A node earns a place in this file by being read somewhere other than
 * where it is fetched. That is the bar every note below argues from, and
 * it is why `App.tsx` owns the fetches while pages only read: two pages
 * each running their own `listOperations()` could disagree with each
 * other, and did.
 *
 * The retention nodes at the bottom are the exception worth knowing about
 * before reading them. `retentionPlanStaleNode` derives staleness from
 * evidence rather than from a wire boolean, which is the right mechanism,
 * but nothing in production writes the evidence yet, so in a running app
 * it is a constant false. Its own doc says so at length rather than
 * letting the shape imply a guard that is not armed.
 */
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

/**
 * Whether this instance has a configuration at all: GET /system/first-run's
 * own answer, `null` until it arrives.
 *
 * Issue #275. This was App.tsx's own useState, with a comment saying a
 * plain local was enough because "exactly one component reads it". That
 * stopped being true the moment an unconfigured instance became navigable:
 * a page with no fetch of its own (catalog recovery) cannot learn it from
 * a refusal, because it makes no request until the operator presses a
 * button, and the settings page needs it to stop advertising a scan of
 * storage that has not been configured.
 *
 * Pages that DO fetch should keep reading their own refusal instead
 * (isNotConfigured, api/failure.ts): the 503 is the service's answer about
 * the exact call that was made, and this node is one page's summary of a
 * different call.
 */
export const configuredNode = registerInput<boolean | null>("app.configured", null);

export interface AppCounts {
  sets: number | undefined;
  backups: number | undefined;
  quarantine: number | undefined;
}

export const countsNode = graph.derived<AppCounts>("app.counts", (get) => ({
  sets: get(setsNode).data?.length,
  // The health report counts backup SETS, not the artifacts inside them,
  // so the header's backup count comes from the health report's own
  // per-set totals rather than from a retained-artifact figure. Nothing
  // has ever computed that figure server-side: it came off the dev-server
  // mock, and against a real backend it was undefined (issue #211).
  backups: (() => {
    const health = get(healthNode).data;
    if (!health) return undefined;
    return health.setsHealthy + health.setsDegraded + health.setsStale + health.setsFailing;
  })(),
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
 * commitRetentionRevisions's call site, RetentionPreviewDialog.tsx).
 *
 * # This node has no producer yet, so the gate below cannot fire
 *
 * That seed is currently the ONLY non-test caller of
 * commitRetentionRevisions: nothing polls the revisions, nothing pushes
 * them, and no other page moves this node. The two values are therefore
 * identical by construction, and retentionPlanStaleNode below is a
 * constant false outside tests. Stated plainly because the mechanism reads
 * like a live guard and is not one (issue #96's review, mandatory finding
 * M9): today the server's own 409 RETENTION_PLAN_STALE, re-checked in
 * RetentionPreviewDialog's handleApply, is the only staleness detection
 * that can actually refuse a real apply. The derived node landed ahead of
 * its producer, deliberately and with its own tests, which drive it by
 * committing here directly.
 *
 * Anything wiring a real producer (a poll, a push, GET /system/version's
 * own config_revision) has one prerequisite: this is a single global, not
 * keyed by backup set, so revisions left over from one set would read as
 * staleness against another set's plan. Key it before feeding it.
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
 *
 * That case is a test committing the change by hand, standing in for a
 * producer that does not exist yet: see retentionRevisionsNode's own doc
 * above. In a running app this node is always false, so treat it as the
 * mechanism for a staleness signal rather than as a staleness signal that
 * is currently arriving.
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
