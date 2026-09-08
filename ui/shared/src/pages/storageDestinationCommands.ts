/**
 * The `backup-manager` command each storage-destination action in the
 * browser is equivalent to (G2.2, issue #594; EPIC G's standing rule).
 *
 * # Why the UI prints commands at all
 *
 * Three reasons, worth keeping separate because they fail separately.
 *
 * It teaches the CLI to somebody who started in the browser. An operator
 * who configured one destination by clicking has, by the end, read the
 * exact command that configures the next fifty.
 *
 * It makes "send me what the terminal said" a useful support request. The
 * terminal is copy-to-clipboard and exportable, so what comes back is a
 * reproducible transcript rather than a description of some clicking.
 *
 * And it turns EPIC G's CLI-parity requirement into something that fails
 * visibly. A UI action with no command to name is a parity gap, and
 * printing forces that gap to surface the first time anybody uses the
 * feature instead of the first time somebody tries to script it.
 *
 * # Nothing here can carry a secret, and that is structural
 *
 * This is the one part of the rule that would be a real security problem
 * if it were got wrong. An S3 setup carries an access key id and a secret
 * access key, and the terminal these lines go to is exportable to a file,
 * so anything printed here ends up pasted into a chat window eventually.
 *
 * Redaction is deliberately not the answer. Redaction is a policy somebody
 * has to remember to apply at every print site, and a site that forgets is
 * indistinguishable from one that had nothing to redact until the day it
 * leaks. The CLI instead has no flag that takes a secret at all: the
 * material only ever arrives on stdin, through `medium import-credentials
 * --stdin`, and stdin is not in the process table and not in shell
 * history. So there is nothing in these lines to redact, and what is
 * printed is the command that actually works, byte for byte, with nothing
 * starred out.
 *
 * That property is asserted rather than trusted: this module's own test
 * plants a canary secret and searches every rendered line for it, with a
 * positive control proving the canary was in play.
 *
 * # It is a module rather than a component
 *
 * These are strings. G1.2's global terminal is where they will be printed
 * once it exists; until then the wizard renders them itself. Either way
 * one function decides what the equivalent command IS, so the terminal and
 * the on-screen copy cannot come to name two different commands.
 */
import type { StorageMediumSpec } from "@shared/api/contracts";

/** The one command that ever handles material, and it reads it on stdin.
 *
 *  It takes no argument at all, which is the whole point: there is no
 *  access key and no secret to interpolate, so this line is safe to print
 *  before the operator has typed either. */
export function importCredentialsCommand(): string {
  return "backup-manager medium import-credentials --stdin";
}

/** `medium preflight --candidate`: the wizard's step 3, proving a
 *  destination that has not been saved. It writes nothing whatever the
 *  report says, which is why it is safe to offer as a copy-pasteable line
 *  beside a form the operator has not submitted. */
export function preflightCandidateCommand(spec: StorageMediumSpec): string {
  return ["backup-manager medium preflight --candidate", spec.id, ...specFlags(spec)].join(" ");
}

/** `medium add`: the wizard's step 4. It takes the same flags the
 *  candidate preflight above takes, deliberately, so an operator reading
 *  the two lines can see that what was proven is what is about to be
 *  written. */
export function addCommand(spec: StorageMediumSpec): string {
  return ["backup-manager medium add", spec.id, ...specFlags(spec)].join(" ");
}

/** `medium edit`: the same flags again, against a destination that
 *  already exists. */
export function editCommand(spec: StorageMediumSpec): string {
  return ["backup-manager medium edit", spec.id, ...specFlags(spec)].join(" ");
}

/** `medium preflight <id>`: the Verify button on the destinations list. */
export function preflightCommand(id: string): string {
  return `backup-manager medium preflight ${id}`;
}

/** `medium remove <id>`: the Remove button, including when it is refused.
 *
 *  Printed even for a removal that is going to be refused, on purpose. The
 *  refusal is part of what an operator has to learn about this
 *  destination, and a command that is only printed when it succeeds
 *  teaches the CLI as something that always works. */
export function removeCommand(id: string): string {
  return `backup-manager medium remove ${id}`;
}

/** `medium show <id>`: what the list's own row is a summary of. */
export function showCommand(id: string): string {
  return `backup-manager medium show ${id}`;
}

/**
 * The flags add, edit and preflight --candidate share, in the order the
 * CLI's own usage block lists them.
 *
 * A field the operator left empty produces no flag at all rather than an
 * empty one. `--prefix ''` and no `--prefix` mean the same thing to the
 * CLI, but they do not read the same way to a person: an empty flag looks
 * like something that failed to fill in, and this line is meant to be
 * copied and typed.
 */
function specFlags(spec: StorageMediumSpec): string[] {
  const out: string[] = [];
  if (spec.type) out.push("--type", spec.type);
  if (spec.region) out.push("--region", spec.region);
  if (spec.endpoint) out.push("--endpoint", spec.endpoint);
  if (spec.bucket) out.push("--bucket", spec.bucket);
  if (spec.prefix) out.push("--prefix", spec.prefix);
  if (spec.storageClass) out.push("--storage-class", spec.storageClass);
  if (spec.uploadVerification) out.push("--upload-verification", spec.uploadVerification);

  const c = spec.credentials;
  if (c?.credentialsId) out.push("--credentials-id", c.credentialsId);
  else if (c?.file) out.push("--credentials-file", c.file);
  else if (c?.env) out.push("--credentials-env", c.env);
  else if (c?.command && c.command.length > 0) {
    // Quoted as one shell word, because it is one argv array on the other
    // side and a bare `op read op://…` would be read as three flags'
    // worth of operands. The line has to be copy-pasteable as typed or it
    // teaches something that does not work.
    out.push("--credentials-command", `'${c.command.join(" ")}'`);
  }
  return out;
}
