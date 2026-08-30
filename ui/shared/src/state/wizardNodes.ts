import type { CompletionMethod } from "@shared/types/backup";
import { graph, registerInput } from "./graph";
import { readOnlyNode } from "./appNodes";

/**
 * The add/edit backup set wizard's ANSWERS, as graph nodes (issue #98,
 * B2.3 — causl-ts migration). BackupSetWizardPage.tsx used to hold four
 * `useState` calls: `step`, `completion`, `hostTrusted`, `acknowledged`.
 * `step` — which of the six panels is showing — is genuinely local and
 * ephemeral, nothing outside the component reads it, and it stays
 * `useState`. The other three are the wizard's actual answers, and are
 * not really local:
 *
 * - the review step (step 6) has to read back exactly what steps 2 and 4
 *   produced, fresh, not a copy captured earlier that's never
 *   invalidated;
 * - `hostTrusted` specifically has to react if the host key this session
 *   trusted changes while the wizard is still open — the same "changed
 *   host key" fact DashboardPage already surfaces from `app.sets`
 *   (`haltReason === "host-key-changed"`, see appNodes.ts/setsNode). A
 *   host-trust decision made in this wizard and a host-key change
 *   surfaced on the dashboard are the same fact seen from two screens.
 *
 * The wizard does not yet implement a live host-key re-probe or a
 * connection test (see BackupSetWizardPage.tsx's own note on WP 2.3
 * steps 3 and 5) — `wizardHostKeyChangedNode` exists so that when one
 * does land, it has a graph node to commit to already, with the review
 * step's blocking behavior already wired against it.
 */
export const wizardCompletionNode = registerInput<CompletionMethod>("wizard.completion", "completion-marker");
export const wizardHostTrustedNode = registerInput<boolean>("wizard.hostTrusted", false);
/** Whether the fingerprint this wizard session trusted has since
 *  changed underneath it. Nothing re-probes the host yet, so today only
 *  a direct `graph.commit` sets this — standing in for the future probe
 *  (or a backend push) that will. */
export const wizardHostKeyChangedNode = registerInput<boolean>("wizard.hostKeyChanged", false);
export const wizardAcknowledgedNode = registerInput<boolean>("wizard.acknowledged", false);

/**
 * WP 2.3's "changed host key blocks operation" acceptance criterion: a
 * stale host trust blocks saving even once the remote-source-deletion
 * disclosure has been acknowledged, and even when the service is
 * otherwise writable. Combined with the app-wide `readOnlyNode` (#106)
 * so nothing has to remember to recompute `saveDisabled` by hand.
 */
export const wizardCanSaveNode = graph.derived<boolean>("wizard.canSave", (get) => {
  return !get(readOnlyNode) && get(wizardAcknowledgedNode) && !get(wizardHostKeyChangedNode);
});

const WIZARD_DEFAULTS: {
  completion: CompletionMethod;
  hostTrusted: boolean;
  hostKeyChanged: boolean;
  acknowledged: boolean;
} = {
  completion: "completion-marker",
  hostTrusted: false,
  hostKeyChanged: false,
  acknowledged: false
};

/**
 * Commits every wizard answer node back to its default. Call once, on
 * mount, from BackupSetWizardPage — these nodes live on the app's one
 * shared graph singleton (graph.ts), not in the component's own state,
 * so opening "Add backup set" a second time would otherwise silently
 * inherit whatever a previous session left behind (an unrelated set's
 * acknowledgement, a stale host-trust flag, ...). This is the
 * production-path analogue of graph.ts's own `resetGraphForTests` —
 * kept separate because calling a function named for tests from real
 * component code would be its own source of confusion.
 */
export function resetWizardAnswers(): void {
  graph.commit("wizard/reset", (tx) => {
    tx.set(wizardCompletionNode, WIZARD_DEFAULTS.completion);
    tx.set(wizardHostTrustedNode, WIZARD_DEFAULTS.hostTrusted);
    tx.set(wizardHostKeyChangedNode, WIZARD_DEFAULTS.hostKeyChanged);
    tx.set(wizardAcknowledgedNode, WIZARD_DEFAULTS.acknowledged);
  });
}
