import { BackupManagerError } from "./contracts";
import type { ApiError } from "./contracts";

/**
 * Issue #274. The service tells this frontend why it refused, in a typed
 * envelope with a code, a sentence and the correlation id that response
 * was logged under. Every one of the callers that caught a rejection used
 * to throw all three away and substitute a fixed sentence of its own, so
 * an expired enrolment token read as "the administrator account could not
 * be created" (wrong cause, no recovery) under a correlation id, the
 * literal `cid_enroll`, that appears in no log anywhere.
 *
 * This module is the one place that turns a rejected request back into
 * something worth showing. It is not a message table: the default branch
 * shows what the service said, because a reason nobody anticipated still
 * beats a reason we invented.
 */

/** The typed envelope behind a rejected request, or null when the failure
 *  never reached the service at all (a stopped container, a dropped
 *  connection) and so carries no code and no correlation id. */
export function apiErrorOf(e: unknown): ApiError | null {
  return e instanceof BackupManagerError ? e.api : null;
}

export interface OperatorFailure {
  /** What went wrong, in one sentence, about the thing that actually
   *  went wrong. */
  message: string;
  /** What to do about it. Absent when there is genuinely nothing to
   *  suggest, which is a better answer than a suggestion that cannot
   *  work. */
  remediation?: string;
  /** The id the failing RESPONSE carried. Absent when the request never
   *  reached the service, because there is then no id in any log to
   *  match, and an id that matches nothing is worse than none (#274). */
  correlationId?: string;
}

/**
 * The failures any route can produce, translated once. Callers handle the
 * codes specific to their own operation first and fall back to this.
 *
 * `fallbackMessage` is used only where this frontend genuinely has nothing
 * better: the service reached, refused, and said nothing usable.
 */
export function describeFailure(e: unknown, fallbackMessage: string): OperatorFailure {
  const api = apiErrorOf(e);
  if (api === null) {
    return {
      message: "Backup Manager did not answer.",
      remediation:
        "The request got no reply at all, so nothing was changed. Check that the Backup Manager service is still running, then try again."
    };
  }

  const correlationId = api.correlationId;
  switch (api.code) {
    case "RATE_LIMITED":
      return {
        message: "Too many attempts from this address.",
        remediation: "Backup Manager is refusing further attempts for the moment. Wait a minute, then try again.",
        correlationId
      };
    case "CSRF_TOKEN_MISSING":
    case "CSRF_TOKEN_MISMATCH":
      return {
        message: "This page's security token is missing or out of date.",
        remediation: "Reload the page and enter the details again. Nothing was changed.",
        correlationId
      };
    case "INTERNAL":
    case "INTERNAL_ERROR":
      return {
        message: fallbackMessage,
        remediation:
          "Backup Manager reported an internal error rather than a reason it could name. Its own log holds the detail, under this correlation id.",
        correlationId
      };
    default:
      return { message: api.message || fallbackMessage, correlationId };
  }
}
