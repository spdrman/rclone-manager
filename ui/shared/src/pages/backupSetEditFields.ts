import type { BackupSet, CompletionMethod } from "@shared/types/backup";
import type { BackupSetPatch } from "@shared/api/contracts";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import type { FieldHelpCopy } from "@shared/components/fieldHelpCopy";

/**
 * Issue #350: the editable surface of a backup set, as data.
 *
 * Every field an operator can change in place is one entry here, and
 * BackupSetDetailPage renders, dirty-checks, saves and error-reports them
 * by walking this list. That is deliberate rather than tidy-minded: the
 * issue's contract is that a per-box Save writes ONLY that box, and the
 * cheapest way to make that true for a seventh field is for there to be
 * exactly one code path that builds a patch from exactly one key.
 * Hand-writing a save handler per field is how the sixth one ends up
 * sending the fifth one's value too.
 *
 * `read` and `parse` are inverses across the string an <input> actually
 * holds. Everything is edited as text, including the port and the include
 * list, because the dirty check the issue specifies compares against the
 * value LOADED rather than against the last keystroke, and comparing
 * strings is the only comparison that gives the same answer for "typed a
 * character and deleted it" as for "never touched it".
 *
 * What is NOT here: the set's name and source. A backup set's identity
 * keys every journal row, artifact id and recovery manifest it has ever
 * produced, so renaming one is a migration rather than a field on a form
 * (core/service/backupsetupdate.go's own package doc). The detail page
 * shows the name as its heading, which is what it has always been.
 *
 * # The two write-only boxes (issue #572)
 *
 * `sshKeyId` and `knownHostsLine` do not read anything back, and that is
 * the one place this table breaks its own "read and parse are inverses"
 * shape. There is nothing to read: the API answers with the set, and the
 * set carries a reference to a key and a path to a trust anchor, neither
 * of which is a value an operator typed or could act on. So both `read`
 * as "", both are dirty only once something is typed into them, and a
 * save that never touched them cannot carry them. That is also what makes
 * them safe to sit beside six ordinary boxes: SAVE ALL walks the dirty
 * ones, and an untouched empty box is not dirty.
 */
export type EditFieldKey =
  | "host"
  | "port"
  | "user"
  | "remotePath"
  | "localPath"
  | "include"
  | "completion"
  | "stableFor"
  | "sshKeyId"
  | "knownHostsLine";

export interface ParsedField {
  /** The patch this field contributes, or undefined when `error` is set. */
  patch?: BackupSetPatch;
  /** A problem this field can decide on its own, before any request. It
   *  is deliberately a short list: the server owns validation (it is the
   *  same config.Validate a hand-edited file goes through), and a second
   *  copy of those rules here would be one that drifts. Only values that
   *  cannot be expressed on the wire at all are caught here. */
  error?: string;
}

/** One editable field, described completely enough that the page needs no
 *  knowledge of it: how to label it, how to help with it, how to draw it,
 *  how to read it out of a set and how to turn what was typed back into a
 *  patch. A field that needed a special case in the page would defeat the
 *  point of the list. */
export interface EditField {
  key: EditFieldKey;
  label: string;
  help: FieldHelpCopy;
  /** "select" renders a picklist of `options`; everything else is a text
   *  input with that HTML input type. */
  control: "text" | "number" | "select";
  options?: { value: string; label: string }[];
  /** When present, this field is only rendered (and only dirty-checked,
   *  and only ever saved) while it returns true for the CURRENT draft.
   *  The draft rather than the persisted set, so choosing a completion
   *  method reveals its window immediately instead of after a save. */
  shownWhen?(draft: Record<EditFieldKey, string>): boolean;
  /** This box sends a value but can never read one back, so its `read` is
   *  a constant "" rather than the set's own value; see this file's own
   *  doc for the two of them. Anything that reports on a field has to know
   *  which kind it is holding: an empty baseline on an ordinary box means
   *  the value is empty, and on one of these it means "keep whatever this
   *  set already uses", which are two different sentences to put in front
   *  of an operator (#591). */
  writeOnly?: boolean;
  /** Fields this one cannot be persisted without, added to any save that
   *  carries it (withCompanions below). Only for boxes that are really one
   *  setting the server takes as two; see the completion pair for the
   *  whole argument. Companions are added only while they are on screen,
   *  so this can never resurrect a hidden box. */
  savesWith?: EditFieldKey[];
  read(set: BackupSet): string;
  parse(raw: string): ParsedField;
}

/** The fields on screen for a given draft: everything unconditional, plus
 *  whichever conditional ones this draft has turned on. Dirty-checking,
 *  SAVE ALL and the per-box Saves all walk THIS rather than EDIT_FIELDS,
 *  so a hidden field can never be part of a patch. */
export function visibleEditFields(draft: Record<EditFieldKey, string>): EditField[] {
  return EDIT_FIELDS.filter((f) => !f.shownWhen || f.shownWhen(draft));
}

/**
 * The keys a save of `keys` actually has to carry.
 *
 * The completion method and its window are one setting the server takes as
 * two fields, and either one sent alone is a save that cannot work. The
 * method alone is refused, because a stable set with a zero window is
 * invalid on this path exactly as it is at creation, and there is no
 * default for core to invent: too short a window copies a half-written
 * file, so the number has to come from the operator. The window alone is
 * refused too, because core clears the window of any set not on the stable
 * strategy, and it used to answer 200 for the value it had just thrown
 * away. Between those two, the pair was reachable only through SAVE ALL,
 * which is a thing an operator finds out by failing twice.
 *
 * The expansion happens here, in the one place every save goes through,
 * rather than on the per-box button, for two reasons that are not the same
 * one. SAVE ALL walks the dirty fields, so a set already on stable-size
 * whose window alone was edited would send the window on its own. And
 * expanding in one place is what keeps the acknowledgement retry, which
 * re-sends the keys a refused save carried, from re-splitting a pair the
 * first attempt had joined.
 *
 * What it also buys, which is worth saying because it looks like a
 * regression until you see it: a save of the method now parses the window
 * too, so choosing stable-size on a set whose window is still 0 fails
 * locally, on the window box, instead of making a request core answers
 * with a sentence about stable_for rendered under the method box.
 *
 * A companion that is not on screen is never added, which is the same rule
 * dirty-checking already follows: a hidden box must not be able to reach a
 * patch. The key asked for is always kept, companion or not.
 */
export function withCompanions(
  keys: EditFieldKey[],
  draft: Record<EditFieldKey, string>
): EditFieldKey[] {
  const onScreen = new Set(visibleEditFields(draft).map((f) => f.key));
  const out: EditFieldKey[] = [];
  const add = (key: EditFieldKey) => {
    if (!out.includes(key)) out.push(key);
  };
  for (const key of keys) {
    add(key);
    for (const companion of EDIT_FIELDS.find((f) => f.key === key)?.savesWith ?? []) {
      if (onScreen.has(companion)) add(companion);
    }
  }
  return out;
}

const COMPLETION_OPTIONS: { value: CompletionMethod; label: string }[] = [
  { value: "atomic-rename", label: "Atomic rename" },
  { value: "completion-marker", label: "Completion marker / manifest" },
  { value: "stable-size", label: "Stable file size / timestamp" }
];

/** Every field an operator can change in place, in the order the page
 *  draws them. Adding one here is the whole change: rendering, the dirty
 *  check, the per-box save and the error reporting all walk this list, so
 *  there is no second place for a new field to be forgotten. */
export const EDIT_FIELDS: EditField[] = [
  {
    key: "host",
    label: "Host",
    help: FIELD_HELP.editSetHost,
    control: "text",
    read: (s) => s.host,
    parse: (raw) => ({ patch: { host: raw.trim() } })
  },
  {
    key: "port",
    label: "Port",
    help: FIELD_HELP.editSetPort,
    control: "number",
    read: (s) => String(s.port),
    parse: (raw) => {
      const trimmed = raw.trim();
      const value = Number(trimmed);
      // Caught here rather than left to the server because there is no
      // request that expresses it: the wire field is an integer, and
      // sending NaN would either be rejected as malformed JSON or, worse,
      // serialise as null and read as "leave the port alone", which is a
      // silent no-op reported as a success.
      if (trimmed === "" || !Number.isInteger(value) || value < 0 || value > 65535) {
        return { error: "Port must be a whole number between 0 and 65535. 0 selects the default port." };
      }
      return { patch: { port: value } };
    }
  },
  {
    key: "user",
    label: "User",
    help: FIELD_HELP.editSetUser,
    control: "text",
    read: (s) => s.username,
    parse: (raw) => ({ patch: { username: raw.trim() } })
  },
  {
    key: "remotePath",
    label: "Remote folder",
    help: FIELD_HELP.editSetRemotePath,
    control: "text",
    read: (s) => s.remoteFolder,
    parse: (raw) => ({ patch: { remoteFolder: raw.trim() } })
  },
  {
    key: "localPath",
    label: "Local destination",
    help: FIELD_HELP.editSetLocalPath,
    control: "text",
    read: (s) => s.destination,
    parse: (raw) => ({ patch: { destination: raw.trim() } })
  },
  {
    key: "include",
    label: "Include patterns",
    help: FIELD_HELP.editSetInclude,
    control: "text",
    read: (s) => s.includePatterns.join(", "),
    parse: (raw) => ({
      // An empty box is an empty list, not an absent field: clearing the
      // include patterns is a thing an operator can mean, and the sparse
      // patch can express it, because the key is present with [] rather
      // than missing.
      patch: {
        includePatterns: raw
          .split(",")
          .map((p) => p.trim())
          .filter((p) => p !== "")
      }
    })
  },
  {
    key: "completion",
    label: "Completion method",
    help: FIELD_HELP.editSetCompletion,
    control: "select",
    options: COMPLETION_OPTIONS,
    // The window rides with it whenever it is on screen. Core refuses a
    // set whose strategy is "stable" and whose window is zero, exactly as
    // it refuses one at creation, so a save carrying the method alone is
    // one that can only fail on every set not already on stable-size:
    // moving TO stable-size means arriving with a window, and there is no
    // default for core to invent, because too short a window copies a
    // half-written file.
    savesWith: ["stableFor"],
    read: (s) => s.completionMethod,
    parse: (raw) => ({ patch: { completionMethod: raw as CompletionMethod } })
  },
  {
    key: "stableFor",
    label: "Stable for (seconds)",
    help: FIELD_HELP.editSetStableFor,
    control: "number",
    // Shown only while the completion method in the DRAFT is
    // stable-size. Not because it is noise otherwise, but because the
    // alternative was a Save that could only fail: core refuses a backup
    // set whose strategy is "stable" and whose window is zero, exactly as
    // it refuses one at creation, so a completion-method box offered
    // without this one is a control whose "Stable file size" option is
    // unusable on every set that is not already on it.
    shownWhen: (draft) => draft.completion === "stable-size",
    // And the method rides back, which is the other half of the same
    // defect. Core clears the window of any set not on "stable", so a save
    // carrying the window alone, made while the DRAFT says stable-size but
    // the persisted set still says rename, is a write the file discards.
    // It used to answer 200 for that; it now refuses, and this is what
    // keeps the box from having to be refused at all.
    savesWith: ["completion"],
    read: (s) => String(s.stableForSeconds),
    parse: (raw) => {
      const trimmed = raw.trim();
      const value = Number(trimmed);
      if (trimmed === "" || !Number.isInteger(value) || value <= 0) {
        return { error: "Stable for must be a whole number of seconds greater than zero." };
      }
      return { patch: { stableForSeconds: value } };
    }
  },
  {
    key: "sshKeyId",
    label: "SSH key",
    help: FIELD_HELP.editSetSSHKey,
    control: "text",
    // Write-only: see this file's own doc. "" is not the key's value, it
    // is the absence of an instruction, and the dirty check is what keeps
    // those two from being confused.
    writeOnly: true,
    read: () => "",
    parse: (raw) => {
      const trimmed = raw.trim();
      // Caught here rather than left to the server for the reason the
      // port is: there is no request that expresses it. An empty
      // ssh_key_id is refused by core, so sending one would spend a round
      // trip to be told what this box already knows.
      if (trimmed === "") {
        return { error: "Paste the id of an imported key, or leave the box empty to keep the key this set already uses." };
      }
      return { patch: { sshKeyId: trimmed } };
    }
  },
  {
    key: "knownHostsLine",
    label: "Trusted host key",
    help: FIELD_HELP.editSetKnownHostsLine,
    control: "text",
    writeOnly: true,
    read: () => "",
    parse: (raw) => {
      const trimmed = raw.trim();
      if (trimmed === "") {
        return { error: "Paste the known_hosts line to trust, or leave the box empty to keep trusting the key this set already trusts." };
      }
      // Nothing beyond emptiness is checked here. Whether the line parses,
      // and whether it pins a key different from the one on record, are
      // both decided by the service against what is actually persisted,
      // and a second opinion about a host key formed in a browser would be
      // one that can be wrong in the permissive direction.
      return { patch: { knownHostsLine: trimmed } };
    }
  }
];

/** Every field's current persisted value, as the strings the inputs hold.
 *  Taken once when edit mode opens, and again for whichever fields a save
 *  actually persisted; see BackupSetDetailPage for why the second one
 *  reads the SERVER's answer rather than the text that was sent. */
export function readEditFields(set: BackupSet): Record<EditFieldKey, string> {
  const out = {} as Record<EditFieldKey, string>;
  for (const field of EDIT_FIELDS) out[field.key] = field.read(set);
  return out;
}
