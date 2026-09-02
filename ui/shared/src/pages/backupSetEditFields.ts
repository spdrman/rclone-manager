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
 */
export type EditFieldKey =
  | "host"
  | "port"
  | "user"
  | "remotePath"
  | "localPath"
  | "include"
  | "completion";

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

export interface EditField {
  key: EditFieldKey;
  label: string;
  help: FieldHelpCopy;
  /** "select" renders a picklist of `options`; everything else is a text
   *  input with that HTML input type. */
  control: "text" | "number" | "select";
  options?: { value: string; label: string }[];
  read(set: BackupSet): string;
  parse(raw: string): ParsedField;
}

const COMPLETION_OPTIONS: { value: CompletionMethod; label: string }[] = [
  { value: "atomic-rename", label: "Atomic rename" },
  { value: "completion-marker", label: "Completion marker / manifest" },
  { value: "stable-size", label: "Stable file size / timestamp" }
];

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
    read: (s) => s.completionMethod,
    parse: (raw) => ({ patch: { completionMethod: raw as CompletionMethod } })
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
