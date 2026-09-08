import { BackupManagerError, RequestFailure, describeException } from "./contracts";
import type { ApiError } from "./contracts";
import { API_BASE_PATH, API_VERSION } from "./generated/contract";

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

/**
 * Issue #275, the same lesson one refusal over. An instance with no
 * configuration serves a deliberately tiny route table
 * (apps/common/webhost's newUnconfiguredRouter) and answers everything
 * else 503 NOT_CONFIGURED. That is a fact about this deployment, not a
 * failure of the request: it means "there is nothing behind this page
 * yet", which a page can say far better than a red banner quoting an API
 * path at an operator who never asked for one.
 */
export function isNotConfigured(error: ApiError | null | undefined): boolean {
  return error?.code === "NOT_CONFIGURED";
}

/** The typed envelope behind a rejected request, or null when the failure
 *  never reached the service at all (a stopped container, a dropped
 *  connection) and so carries no code and no correlation id. */
export function apiErrorOf(e: unknown): ApiError | null {
  return e instanceof BackupManagerError ? e.api : null;
}

/**
 * A refusal, split into the three things a surface renders separately.
 *
 * It is three fields rather than one sentence because the caller decides
 * where each goes: the message belongs in the banner, the remediation
 * belongs under it or not at all, and the correlation id belongs behind
 * "Advanced details" where it can be copied into a support message. A
 * single pre-joined string would force every surface to render all three
 * the same way, and would make "there is nothing useful to suggest"
 * impossible to express.
 */
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
  /** The technical facts, for the Advanced details panel: what was asked
   *  for, what came back, and what the exception said about it. Issue
   *  #598: this is what the fallback used to throw away, and throwing it
   *  away is what made a dropped connection, a truncated body and a mapper
   *  that threw indistinguishable from each other on screen. Never a stack
   *  trace and never a source path (§37). */
  detail?: string;
}

/** The path in the form an operator reads it, which is the one they can
 *  put in a curl or paste into a bug report. `request()` records the path
 *  without the version prefix; the prefix is what makes it a URL. */
function requestedPath(path: string): string {
  return "GET " + API_BASE_PATH + path;
}

/** Which contract this bundle speaks. A diagnostic that does not say
 *  which build produced it is one nobody can act on, and a page talking to
 *  a service newer than itself is exactly the shape an unreadable body
 *  takes. This module can say it for the frontend half honestly, from a
 *  generated constant; the SERVICE's own build is a fact only the version
 *  endpoint has, and it is already on the Settings page under it. */
function buildLine(): string {
  return "web ui speaks api " + API_VERSION;
}

/** Joins the facts a failure carries into the one block the Advanced
 *  details panel shows and the copy button copies. One fact per line: an
 *  operator pastes this into a message, and a paragraph does not survive
 *  that as well as lines do. */
function detailOf(...parts: (string | undefined)[]): string {
  return parts.filter((p): p is string => !!p).join("\n");
}

/**
 * The failures any route can produce, translated once. Callers handle the
 * codes specific to their own operation first and fall back to this.
 *
 * `fallbackMessage` is used only where this frontend genuinely has nothing
 * better: the service reached, refused, and said nothing usable.
 */
export function describeFailure(e: unknown, fallbackMessage: string): OperatorFailure {
  if (e instanceof RequestFailure) return describeRequestFailure(e);

  const api = apiErrorOf(e);
  if (api === null) {
    // An exception that came from neither the service nor `request()`.
    // In practice that is a mapper throwing inside a `.then()` chained
    // onto a request that resolved, which is the shape #598 was reported
    // as, or a test double standing in for one of the above. Nothing types
    // it, so the exception's own words are the whole of what there is to
    // show, and showing them is the entire point: "TypeError: r.events.map
    // is not a function" names the defect exactly, and the sentence that
    // used to be printed in its place named nothing.
    return {
      message: "Backup Manager did not answer, or answered with something this page could not read.",
      // Deliberately does NOT say nothing was changed. A request that got
      // no reply may still have been carried out, with only the response
      // lost, and claiming otherwise would be this module's own version of
      // the defect it exists to fix. The CSRF branch below can say it,
      // because requireCSRF refuses before any handler runs.
      // Deliberately does not use the words "correlation id" here. There
      // is none, and a sentence saying so still puts the phrase on screen,
      // which is what auth-failure-reporting.test.tsx checks the absence
      // of: an operator scanning a banner for an id finds the words either
      // way.
      remediation:
        "This failure did not come out of the service, so it has no id in any log. Check that the Backup Manager service is still running, then try again.",
      detail: detailOf(describeException(e), buildLine())
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

/**
 * The two failures that are not refusals, said apart.
 *
 * This is the branch #598 exists for. The service refusing is already well
 * covered above: it names a code, a sentence and an id, and all three
 * reach the operator. What had no branch at all was the request never
 * coming back and the response body not parsing, and those two used to
 * arrive on screen as one another.
 */
function describeRequestFailure(e: RequestFailure): OperatorFailure {
  if (e.kind === "no-response") {
    return {
      message: "Backup Manager did not answer.",
      remediation:
        "The request got no reply at all, so whether it was carried out is unknown. Check that the Backup Manager service is still running, then try again.",
      // No response, so no id. apiErrorOf's own rule, one failure over: an
      // id that matches nothing in any log is a false lead.
      detail: detailOf(requestedPath(e.path), describeException(e.cause), buildLine())
    };
  }
  return {
    message: "Backup Manager answered, and this page could not read the answer.",
    remediation:
      "The service replied, so it is running, but what came back was not what this page expected. That is usually something between the browser and the service rewriting the response, or a version of the app older than the service it is talking to.",
    correlationId: e.correlationId,
    detail: detailOf(
      requestedPath(e.path),
      e.status === undefined ? undefined : "status " + e.status,
      e.contentType === undefined ? undefined : "content-type " + e.contentType,
      describeException(e.cause),
      buildLine()
    )
  };
}

/**
 * The one conversion the two fetch hooks use (useAsync, state/resource).
 *
 * They hold an `ApiError`, not an `OperatorFailure`, because `.code` is
 * what `isNotConfigured` and every per-code branch above them reads. So
 * this is describeFailure with the code carried alongside, and with the
 * service's own sentence as the fallback rather than one written here: an
 * INTERNAL refusal that says "failed to list activity" is more use to
 * whoever is reading it than anything this module could substitute.
 *
 * Before #598 both hooks had a byte-identical copy of a fallback that
 * dropped the exception and minted `correlationId: "unavailable"`. There
 * is now one of these, and it never mints an id.
 */
export function asApiError(e: unknown): ApiError {
  const api = apiErrorOf(e);
  const failure = describeFailure(e, api?.message || "Backup Manager could not complete that request.");
  return {
    code: api?.code ?? "unknown",
    message: failure.message,
    remediation: failure.remediation,
    correlationId: failure.correlationId,
    detail: failure.detail
  };
}
