/**
 * The `rbm` command each storage-destination action in the browser is
 * equivalent to (G2.2, issue #594; EPIC G's standing rule).
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
 * These are strings. They are printed in two places now: beside the
 * control, by CommandEcho, and in the global terminal that landed with
 * #599. One function decides what the equivalent command IS, which is
 * what stops the terminal and the on-screen copy naming two different
 * commands.
 */
import { LOCAL_DESTINATION_ID } from "@shared/api/contracts";
import type { StorageMediumSpec } from "@shared/api/contracts";

/** The one command that ever handles material, and it reads it on stdin.
 *
 *  It takes no argument at all, which is the whole point: there is no
 *  access key and no secret to interpolate, so this line is safe to print
 *  before the operator has typed either. */
export function importCredentialsCommand(): string {
  return "rbm medium import-credentials --stdin";
}

/** `medium test-connection --candidate`: the wizard's step 3, proving a
 *  destination that has not been saved. It writes nothing whatever the
 *  report says, which is why it is safe to offer as a copy-pasteable line
 *  beside a form the operator has not submitted.
 *
 *  The verb changed name in #622 and the check did not: `preflight` is
 *  the same entry in the same dispatch table and still runs, kept as an
 *  alias so anything scripted against it goes on working. What these
 *  lines print is the name the buttons above them now carry, because a
 *  command printed under a button that says something else teaches the
 *  wrong word. */
export function testConnectionCandidateCommand(spec: StorageMediumSpec): string {
  return ["rbm medium test-connection --candidate", spec.id, ...specFlags(spec)].join(" ");
}

/** `medium add`: the wizard's step 4. It takes the same flags the
 *  candidate preflight above takes, deliberately, so an operator reading
 *  the two lines can see that what was proven is what is about to be
 *  written. */
export function addCommand(spec: StorageMediumSpec): string {
  return ["rbm medium add", spec.id, ...specFlags(spec)].join(" ");
}

/** `medium edit`: the same flags again, against a destination that
 *  already exists. */
export function editCommand(spec: StorageMediumSpec): string {
  return ["rbm medium edit", spec.id, ...specFlags(spec)].join(" ");
}

/** `medium test-connection <id>`: the Test connection button, on the
 *  destinations list and under a retention tier's picker.
 *
 *  It takes the local hard drive's id like any other, which is the point
 *  of #622's local entry: the destination an operator is most likely to
 *  be on is the one that used to have nothing to check. */
export function testConnectionCommand(id: string): string {
  return `rbm medium test-connection ${id}`;
}

/** `medium default <id>`: the Make default button.
 *
 *  Printed for the destination being MOVED TO, never for the one that is
 *  already the default, because a command that would change nothing is a
 *  command an operator learns nothing from. */
export function setDefaultCommand(id: string): string {
  return `rbm medium default ${id}`;
}

/** `settings patch --tier-medium NAME=MEDIUM_ID`: the picker under a
 *  retention tier.
 *
 *  One tier per line, because that is what the picker changes and what
 *  the flag takes. The acknowledgment is appended when the destination is
 *  not the local hard drive, which is the same condition the server puts
 *  the FR-27 disclosure behind: a tier moving somewhere other than local
 *  for the first time is refused without it, and a line an operator
 *  pastes has to be the line that works.
 *
 *  It is a whole-chain write on the wire (RetentionUpdate.Tiers replaces
 *  the chain), and this flag is the CLI's own way of spelling "change one
 *  tier and leave the rest", so the command and the click produce the
 *  same request rather than merely the same outcome. */
export function tierMediumCommand(tier: string, mediumId: string): string {
  const parts = ["rbm settings patch --tier-medium", `${tier}=${mediumId}`];
  if (mediumId !== LOCAL_DESTINATION_ID) parts.push("--acknowledge-medium-disclosure");
  return parts.join(" ");
}

/** `medium remove <id>`: the Remove button, including when it is refused.
 *
 *  Printed even for a removal that is going to be refused, on purpose. The
 *  refusal is part of what an operator has to learn about this
 *  destination, and a command that is only printed when it succeeds
 *  teaches the CLI as something that always works. */
export function removeCommand(id: string): string {
  return `rbm medium remove ${id}`;
}

/** `medium show <id>`: what the list's own row is a summary of. */
export function showCommand(id: string): string {
  return `rbm medium show ${id}`;
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
