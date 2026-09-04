import type { BackupSet } from "@shared/types/backup";

/**
 * A backup set's canonical identity as one string: `source/set`.
 *
 * This is not a display convenience, it is the id core itself uses.
 * `model.BackupSetID.String()` (core/internal/model/ids.go) joins the two
 * halves with a "/", which is why a real id off the wire looks like
 * "production/api-server", why the API routes are
 * `/backup-sets/{source}/{set}/...`, and why `backupSetPath` next door
 * exists at all.
 *
 * Two places want it. The card prints it on the row, because `name` is a
 * display label two sets under different sources are free to share, and a
 * row in a list has to be able to say which set it is. The removal
 * confirmation asks for it to be retyped, for the same reason one step
 * further on: a phrase two rows can both satisfy is not confirming which
 * row, and retyping something you can only read inside the dialog doing
 * the asking proves nothing but that you can copy.
 *
 * Built from the two halves rather than read off `BackupSet.id`
 * deliberately. They agree today, and the whole point of carrying
 * `source` and `set` separately (see BackupSet's own doc) is that no
 * caller has to trust a flat id to split back the way it went together.
 */
export function backupSetIdentity(set: Pick<BackupSet, "source" | "set">): string {
  return set.source + "/" + set.set;
}
