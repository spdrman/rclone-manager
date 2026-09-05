/**
 * The one backup set a detail page currently has open, its activity, and
 * the staleness check an inline edit is allowed to rest on.
 *
 * Nothing outside that page reads either node, so moving these fetches
 * onto the graph bought no de-duplication at all. What it bought is the
 * commit counter: an edit form needs to know whether anything landed on
 * the set between opening and saving, and a value sitting in `useAsync`'s
 * `data` has no history to ask. The snapshot pair below is that question,
 * asked with the graph's own clock rather than a revision field invented
 * for `BackupSet`.
 */
import type { ActivityEvent } from "@shared/types/operation";
import type { BackupSet } from "@shared/types/backup";
import { graph } from "./graph";
import { createResourceNode } from "./resource";

/**
 * B2.2 (#97) — BackupSetDetailPage.tsx's own useAsync(() => api.getSet(setId))
 * and useAsync(() => api.listActivity()), as graph resource nodes.
 *
 * Both are owned (fetched) by BackupSetDetailPage itself, not App.tsx —
 * unlike healthNode/setsNode/etc. (appNodes.ts), nothing else in the app
 * reads "whichever set is currently open", so there is no cross-page
 * fetch to de-duplicate here (see #95/#106's own reasoning for why THOSE
 * became app-wide). What moving to the graph buys THIS page is not fewer
 * fetches, it's a value with a real commit history behind it: this
 * issue's own acceptance criterion ("stale edits are rejected") needs
 * something to check staleness against, and a plain object sitting in
 * useAsync's opaque `data` field has no such thing — see
 * captureSetEditSnapshot/isSetEditStale below.
 *
 * A single shared node rather than one node per setId: only one detail
 * page is ever mounted at a time (route is `/sets/:source/:set`, issue
 * #285), so there is no simultaneous-multiple-sets case to isolate, and
 * every node id on this graph is a fixed top-level string literal per
 * the convention graph.ts documents. The one hazard a shared "current X"
 * node creates — B2.4 found it for a hypothetical shared artifact node — is a stale
 * flash of set A's fields under set B's url while B's fetch is still in
 * flight; BackupSetDetailPage.tsx guards against that the same way
 * BackupDetailPage.tsx does (gate render on `loading`, not just `data`).
 */
export const currentSetDetailNode = createResourceNode<BackupSet>("app.currentSetDetail");
export const currentSetActivityNode = createResourceNode<ActivityEvent[]>("app.currentSetActivity");

/**
 * A `BackupSet` read off `currentSetDetailNode`, paired with that node's
 * own per-commit version counter (`graph.stats().nodeVersion`) at the
 * moment it was captured — the "commit.time" the issue's TDD plan asks
 * for, not a bespoke revision field invented for `BackupSet` itself.
 * `set` is kept alongside the version purely for display (an edit form
 * showing what it originally opened against); staleness is decided by
 * `version` alone.
 */
export interface SetEditSnapshot {
  readonly set: BackupSet;
  readonly version: number;
}

/**
 * Call when an edit form opens (or, in principle, at any point a caller
 * wants a fresh baseline to check future submits against). Returns null
 * before the set has ever loaded — there is nothing to edit yet, so
 * nothing to snapshot.
 */
export function captureSetEditSnapshot(): SetEditSnapshot | null {
  const current = graph.read(currentSetDetailNode).data;
  if (!current) return null;
  return { set: current, version: graph.stats().nodeVersion(currentSetDetailNode) };
}

/**
 * GIVEN a snapshot captured at time T, WHEN another commit has landed on
 * `currentSetDetailNode` since (a concurrent editor's save, or this node
 * simply losing its data), THEN the edit this snapshot backs is stale
 * and must be rejected rather than allowed to silently overwrite
 * whatever is there now.
 *
 * Deliberately does NOT compare `snapshot.set` against the current value
 * field-by-field: `nodeVersion` already answers "did anything land on
 * THIS node since T", and a commit to any OTHER node (an unrelated
 * poll tick on setsNode, a quarantine refresh, ...) never touches this
 * node's counter, so there is no false-positive risk from unrelated app
 * activity — see backupSetDetailNodes.test.ts's "unrelated node" case.
 */
export function isSetEditStale(snapshot: SetEditSnapshot): boolean {
  const current = graph.read(currentSetDetailNode).data;
  if (!current) return true;
  return graph.stats().nodeVersion(currentSetDetailNode) !== snapshot.version;
}
