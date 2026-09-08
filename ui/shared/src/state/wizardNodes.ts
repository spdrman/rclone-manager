/**
 * The little the add-backup-set wizard keeps outside its own component.
 *
 * Almost nothing qualifies, and the note below spends most of its length
 * on what stayed local and why, because the tempting mistake in a
 * graph-backed app is to move state onto the graph because it can be
 * moved. The one fact that genuinely belongs to more than the wizard is a
 * changed host key, which the dashboard also reports, from the same
 * underlying event.
 */
import { graph, registerInput } from "./graph";
import { readOnlyNode } from "./appNodes";

/**
 * The add/edit backup set wizard's cross-component state, as graph nodes
 * (issue #98, B2.3 — causl-ts migration).
 *
 * BackupSetWizardPage.tsx holds several `useState` calls of its own —
 * `step`, `source`, `keySource`, `importPasted`/`importedFingerprint`,
 * `completion`, `hostTrusted`, `acknowledged`. None of those belong here:
 * nothing outside the wizard reads any of them while it's open, so
 * plain component state is the correct, simpler home for all of them —
 * the same reasoning that already kept `step` local. A value earns a
 * spot on the shared graph only when something outside this one
 * component actually needs to read or write it, which is the same bar
 * appNodes.ts and platformNodes.ts hold every other node on this graph
 * to.
 *
 * `wizardHostKeyChangedNode` clears that bar: it's the same "changed
 * host key" fact DashboardPage already surfaces from `app.sets`
 * (`haltReason === "host-key-changed"`, see appNodes.ts/setsNode) — a
 * host-trust decision made in this wizard and a host-key change
 * surfaced on the dashboard are the same fact seen from two screens.
 * Today only a direct `graph.commit` sets it, standing in for a live
 * host-key re-probe or a backend push.
 *
 * The wizard's connection test (issue #624) is deliberately NOT here, and
 * this note used to say a connection test did not exist yet. It does now:
 * the Review step runs the six-step check and Save stays disabled until it
 * passes. It stayed local state because it fails the bar above rather than
 * because nobody got to it — the result is about the values on one open
 * form, nothing outside the wizard reads it, and the wizard is closed by
 * the time anything else could care. A changed host key is a fact about a
 * machine two screens report; a candidate check is a fact about a form.
 */
export const wizardHostKeyChangedNode = registerInput<boolean>("wizard.hostKeyChanged", false);

/**
 * WP 2.3's "changed host key blocks operation" acceptance criterion: a
 * changed host key blocks saving even when the service is otherwise
 * writable. Folds in the app-wide `readOnlyNode` (#106) too, so this is
 * the one place that answers "is saving structurally possible at all" —
 * BackupSetWizardPage combines it with the session's own (local, not
 * graph) acknowledgement answer to get the actual save-button gate,
 * rather than checking `readOnly` a second time itself.
 */
export const wizardCanSaveNode = graph.derived<boolean>("wizard.canSave", (get) => {
  return !get(readOnlyNode) && !get(wizardHostKeyChangedNode);
});

/**
 * Resets `wizardHostKeyChangedNode` back to its default. Call once, on
 * mount, from BackupSetWizardPage — this node lives on the app's one
 * shared graph singleton (graph.ts), not in the component's own state,
 * so opening "Add backup set" a second time would otherwise silently
 * inherit whatever a previous session (or a previous test) left behind.
 * This is the production-path analogue of graph.ts's own
 * `resetGraphForTests` — kept separate because calling a function named
 * for tests from real component code would be its own source of
 * confusion.
 */
export function resetWizardAnswers(): void {
  graph.commit("wizard/reset", (tx) => tx.set(wizardHostKeyChangedNode, false));
}
