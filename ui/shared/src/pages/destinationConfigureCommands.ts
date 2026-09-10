/**
 * The `rbm` command each step of the configure-a-destination flow is
 * equivalent to (issue #669; EPIC G's standing rule, argued in full in
 * storageDestinationCommands.ts).
 *
 * # Nothing here can carry a secret, and there is nothing to redact
 *
 * The same structural property the sibling module has, for the same
 * reason: this CLI has no flag that takes credential material at all.
 * Material only ever arrives on stdin, through `medium
 * import-credentials --stdin`, which is not in the process table and not
 * in shell history. So every line below is the command that actually
 * works, byte for byte, with nothing starred out - and the reason it is
 * safe to print before the operator has typed a key is that there is no
 * key in it.
 *
 * # A gap is printed as a gap, never as a plausible flag
 *
 * This is the part worth reading. `rbm medium edit` takes the flags it
 * was written with - `--type`, `--region`, `--endpoint`, `--bucket`,
 * `--prefix`, `--storage-class`, `--upload-verification` and the four
 * credential spellings (core/cmd/backup-manager/medium.go:219) - and a
 * manifest can declare a field none of them names. `local_volume`'s
 * `path` is exactly that today: there is no `--path`, so a local volume
 * cannot be configured from a terminal at all.
 *
 * The tempting thing is to print `--path /mnt/backups` anyway, because
 * it reads correctly and nobody would notice until they ran it. What
 * this module does instead is say which fields have no flag. That is the
 * whole point of EPIC G's rule as its own docblock states it: printing
 * "turns the CLI-parity requirement into something that fails visibly",
 * and a gap papered over with an invented flag is a requirement that
 * fails silently instead. The gap is real, it belongs to the CLI rather
 * than to this screen, and #669's PR body names it.
 */
import type { BackendManifest } from "@shared/api/contracts";

/**
 * The long flags `medium add` and `medium edit` actually parse, measured
 * from their own flag switch rather than assumed.
 *
 * Keyed by the manifest FIELD ID each one sets, which is possible at all
 * because #665 gave a manifest's fields "deliberately the same spelling
 * the config schema already uses" (Field.ID's own doc) - so the flag is
 * the field id with underscores hyphenated, and this map only has to say
 * which of them exist. It is the CLI's vocabulary and not a backend's:
 * no entry here is reached by asking which backend is being configured.
 */
const EDIT_FLAG_FIELDS: Record<string, true> = {
  region: true,
  endpoint: true,
  bucket: true,
  prefix: true,
  storage_class: true,
  upload_verification: true
};

/** `medium test-connection <id>`: the engine's own check, by id.
 *
 *  Labelled by the caller as what it is - a check of what is ALREADY
 *  saved - because that is the honest difference between it and the
 *  wizard's step 2, which checks values that have not been written yet.
 *  The two answer the same eight steps about the same destination; they
 *  differ in which configuration they answer about, and a line printed
 *  as if they were the same command would teach that difference away. */
export function testConnectionCommand(destinationId: string): string {
  return `rbm medium test-connection ${destinationId}`;
}

/**
 * `medium edit <id>` with one flag per configured field, and the fields
 * no flag can carry.
 *
 * Returned together deliberately. A caller that got only the line would
 * print a command that silently configures less than the screen above it
 * is about to save, which is the "verifies green, saves something
 * slightly different" defect in its printed form.
 */
export function editConfigurationCommand(
  manifest: BackendManifest,
  destinationId: string,
  values: Record<string, string>
): { command: string; fieldsWithNoFlag: string[] } {
  const parts = [`rbm medium edit ${destinationId}`];
  const fieldsWithNoFlag: string[] = [];

  for (const field of manifest.fields) {
    if (field.kind === "credential") continue;
    const value = values[field.id];
    if (!value) continue;
    if (!EDIT_FLAG_FIELDS[field.id]) {
      fieldsWithNoFlag.push(field.id);
      continue;
    }
    parts.push(`--${field.id.replace(/_/g, "-")} ${value}`);
  }

  return { command: parts.join(" "), fieldsWithNoFlag };
}

/** `medium import-credentials --stdin`: the one command that ever
 *  handles material, and it takes no argument at all - which is the
 *  point, and why this line is printable before anything is typed. */
export function importCredentialsCommand(): string {
  return "rbm medium import-credentials --stdin";
}
