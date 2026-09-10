/**
 * What makes an instance name legal, and what to say when it is not
 * (EPIC I, #664; issue #668's step 2).
 *
 * # Why the naming step owns this
 *
 * Choosing a backend and naming an instance are separate steps because
 * several instances of one backend is the normal case rather than an edge.
 * That makes the naming step the one place the uniqueness rule applies, so
 * it is the place the rule is checked — and checked BEFORE anything is
 * sent. A wizard that lets an operator type `local` and then fails at the
 * API is a worse experience than one that says so in the field: the
 * refusal arrives after the operator has moved on, attached to a screen
 * that has already been left, and it teaches nothing about what to type
 * instead.
 *
 * # Refusals by shape, never by content
 *
 * No refusal here repeats what was typed. That is
 * transport/rclone/mediumcreds.go's doctrine ("refusals by SHAPE, never by
 * content") carried onto the browser side, and it is not defensive
 * decoration: a name field is one clipboard mistake away from holding a
 * secret, this code cannot tell whether it is holding one, and a message
 * that echoes its input is a message that renders the secret back onto the
 * screen and into any screenshot of it. Every sentence below is derivable
 * from the RULE and never from the value.
 *
 * # Why the rules are arguments rather than constants
 *
 * `pattern` and `reservedId` come from the server, in the same response
 * that serves the backend catalogue (BackendCatalog). RetentionSchema's
 * `tierNamePattern` established the reason: a form has to refuse exactly
 * what a hand-edited configuration file would be refused for, and a copy
 * of the rule in this file would go stale in one direction only — silently
 * accepting a name the engine then rejects, which is the failure the
 * client-side check exists to prevent.
 */

/** Why a name was refused. The caller renders `reason`; this is here so a
 *  test, and a future surface with more room, can distinguish the four
 *  cases without matching on prose. */
export type InstanceNameRefusalKind = "empty" | "shape" | "reserved" | "collision";

export interface InstanceNameRefusal {
  kind: InstanceNameRefusalKind;
  /** One sentence, safe to render. It never contains the rejected value. */
  reason: string;
}

export interface InstanceNameRules {
  /** Anchored regular expression source from BackendCatalog. */
  pattern: string;
  /** The one id no operator may choose, from BackendCatalog. */
  reservedId: string;
  /** Every instance id that already exists, across every backend. */
  existingIds: readonly string[];
}

/**
 * Refuses a proposed instance name, or returns null when it is usable.
 *
 * The order of the checks is deliberate and is the order an operator can
 * act on: what is missing, then what is malformed, then what is taken.
 * Reporting "already exists" for a name that is also malformed would send
 * somebody looking for a destination that is not there.
 */
export function refuseInstanceName(
  name: string,
  rules: InstanceNameRules
): InstanceNameRefusal | null {
  if (name === "") {
    return {
      kind: "empty",
      reason: "This instance needs a name. It is what a retention tier will use to send backups here."
    };
  }

  // The reserved id is checked BEFORE the pattern, even though it satisfies
  // the pattern, because it is the more specific refusal and the one that
  // needs explaining: an operator who types it has typed something legal
  // in every way except that it is already the name of something else.
  if (name === rules.reservedId) {
    return {
      kind: "reserved",
      reason:
        `"${rules.reservedId}" is reserved. It names the drive this deployment's backups already land on, ` +
        "which the first installation wrote and which is not declared in the configuration at all, so a " +
        "second destination cannot answer to it. Any other name is fine."
    };
  }

  if (!new RegExp(rules.pattern).test(name)) {
    return {
      kind: "shape",
      reason:
        "Instance names are lower_snake_case: letters, digits and underscores, starting with a letter. " +
        "No spaces, no dashes, no capitals."
    };
  }

  // Across every backend, not only the chosen one. An id is one namespace:
  // it is what a placement record stores against every artifact and what a
  // retention tier names, and neither carries a backend beside it, so two
  // destinations answering to one name on different backends would be a
  // placement nothing can interpret.
  if (rules.existingIds.includes(name)) {
    return {
      kind: "collision",
      reason:
        "A destination with this name already exists. Names are one namespace across every backend, " +
        "because a retention tier and a stored copy name a destination and nothing else, so a name in " +
        "use on one backend cannot be reused on another."
    };
  }

  return null;
}
