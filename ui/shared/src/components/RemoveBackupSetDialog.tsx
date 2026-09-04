import { useEffect, useRef, useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { ConfirmationDialog } from "./ConfirmationDialog";
import { apiErrorOf, describeFailure } from "@shared/api/failure";
import { bytes } from "@shared/utilities/format";
import type { BackupSet } from "@shared/types/backup";

/**
 * The exact string an operator has to retype to remove this set.
 *
 * It is the set's canonical identity, `source/set`: the same two path
 * segments `removeSet` itself takes, the same pair the URL of the set's
 * own page carries, and the only name for a backup set that is unique
 * across the whole deployment. `name` is a display label, so two sets
 * under different sources are free to share one, and a phrase two rows
 * can both satisfy is not confirming which row.
 *
 * Exported so the card that offers removal can print the same string on
 * the row. Retyping something you can only read inside the dialog that is
 * asking for it proves you can copy; retyping the identity that is on the
 * row you clicked proves you are on the row you meant.
 */
export function removalPhrase(set: Pick<BackupSet, "source" | "set">): string {
  return set.source + "/" + set.set;
}

/**
 * Removing one backup set's configuration, with the promise that removal
 * makes, in one place.
 *
 * # Why this is a component and not a copy on each page
 *
 * Two surfaces offer this now: the set's own detail page, and the list
 * (issue #391 built the first, and an operator managing more than a
 * couple of sets should not have to open each one to turn it off or throw
 * it away). The dialog is the whole safety story of a destructive action
 * that cannot be undone by a button, and the parts of it that matter are
 * exactly the parts that rot when they are written twice:
 *
 *   - the promise. Collection stops; every backup already taken stays on
 *     NAS storage and stays listed under Backups. That is enforced by
 *     `service.RemoveBackupSet`, and it is the reason this action is
 *     allowed to be one click behind a confirmation at all. Two copies of
 *     that sentence is one copy that can drift into a promise the backend
 *     does not keep.
 *   - the 404. `BACKUP_SET_NOT_FOUND` answers both "no such set" and "a
 *     call you already made removed it", and from a caller that knows
 *     which set it just asked about the second reading is a removal that
 *     WORKED. Getting that wrong paints a red error under a destructive
 *     dialog for an operator whose action succeeded.
 *   - the typed confirmation. See `removalPhrase` above.
 *
 * What is NOT here is where to go afterwards, because the two surfaces
 * genuinely differ: the detail page is showing a set that no longer
 * exists so it has to leave, and the list has nowhere to go and has to
 * refresh itself instead. That is `onRemoved`'s job.
 */
export function RemoveBackupSetDialog({
  set,
  open,
  onCancel,
  onRemoved
}: {
  set: BackupSet;
  open: boolean;
  onCancel(): void;
  /** Called once the set is gone, INCLUDING when it was already gone.
   *  The caller decides what "gone" means for its own screen. */
  onRemoved(): void;
}) {
  const api = useApi();
  // A ref as well as the `removing` state, because they answer different
  // questions. The state turns the confirm button off and changes its
  // label, which is what an operator sees; the ref refuses a second call
  // from a second click that was dispatched before that re-render
  // happened, which React's batching makes an ordinary double-click
  // rather than a rare race. Removal is the one action here where the
  // second request's honest answer is a 404 for a set the operator did
  // nothing wrong to lose.
  const inFlight = useRef(false);
  const [removing, setRemoving] = useState(false);
  // A refusal is shown inside the dialog and the dialog stays open, so
  // the operator is never looking at a screen that implies the set went
  // away when it did not.
  const [error, setError] = useState<string | null>(null);

  // A dialog reopened after a failure starts clean. Without this the
  // previous attempt's red line is already on screen before this attempt
  // has been made.
  useEffect(() => {
    setError(null);
  }, [open, set.id]);

  const phrase = removalPhrase(set);

  return (
    <ConfirmationDialog
      open={open}
      destructive
      eyebrow="Destructive action"
      title="Remove backup set configuration"
      confirmLabel={removing ? "Removing..." : "Remove configuration"}
      disabled={removing}
      confirmPhrase={phrase}
      confirmPhraseLabel={
        <>
          {"To confirm, type "}
          <span className="mono" style={{ color: "var(--text)" }}>{phrase}</span>
        </>
      }
      onCancel={onCancel}
      onConfirm={() => {
        if (inFlight.current) return;
        inFlight.current = true;
        setRemoving(true);
        setError(null);
        void api
          .removeSet(set.source, set.set)
          .then(onRemoved)
          .catch((e: unknown) => {
            // 404 BACKUP_SET_NOT_FOUND is one code for two situations: a
            // name this deployment never had, and a set an earlier call
            // already removed (a lost response, a second tab, a retry).
            // The route cannot tell them apart and says so; a caller
            // knows exactly which set it asked about, so it is the one
            // place that can, and the second case is a removal that
            // worked. Every other refusal keeps the dialog open with the
            // reason in it, which is the other half of the promise.
            if (apiErrorOf(e)?.code === "BACKUP_SET_NOT_FOUND") {
              onRemoved();
              return;
            }
            setError(
              describeFailure(e, "Backup Manager could not remove this backup set's configuration.")
                .message
            );
          })
          .finally(() => {
            inFlight.current = false;
            setRemoving(false);
          });
      }}
    >
      <p style={{ margin: 0 }}>
        {"Backup Manager will stop collecting backups for " + set.name + "."}
      </p>
      <p style={{ margin: 0, color: "var(--text-2)" }}>
        {set.retainedCount + " retained backups (" + bytes(set.retainedBytes) + ") stay on NAS storage and remain listed under Backups."}
      </p>
      <p style={{ margin: 0, color: "var(--text-2)" }}>
        {"Creating a backup set with this source and name again takes those backups back, along with their retention history."}
      </p>
      {error ? (
        <p role="alert" style={{ margin: 0, color: "var(--danger)" }}>
          {error}
        </p>
      ) : null}
    </ConfirmationDialog>
  );
}
