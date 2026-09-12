import { BackupdError, RequestFailure, describeException } from "./contracts";
import type { ApiError, FailureOrigin } from "./contracts";
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
  return e instanceof BackupdError ? e.api : null;
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
  /** Which of the five things this failure IS (#795's review). Set by
   *  every branch of `describeFailure` and by nothing else, because that
   *  function is where the question is answered; absent on the handful of
   *  failures a page writes by hand for a code it handles itself, where
   *  the page already knows. The one consumer is App.tsx's unreachable
   *  gate, through `asApiError` and `isServiceUnreachable`: a surface that
   *  says "Backupd is not answering" has to be sure that it did not. */
  origin?: FailureOrigin;
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
    // A SyntaxError is the one untyped exception that says where it came
    // from. `fetch` never rejects with one, and nothing else in this
    // frontend parses anything, so it means a response arrived and could
    // not be read: the same fact `request()` labels, reached without the
    // label because the throw came from a caller's own chain (or, in the
    // browser suite, from a mock standing in for one). A TypeError does
    // NOT say that, which is why it falls through to the branch below: a
    // failed fetch and a property read on the wrong shape are both spelled
    // TypeError, and guessing between them is the defect this module
    // exists to stop.
    if (e instanceof SyntaxError) {
      return {
        message: "Backupd answered, and this page could not read the answer.",
        remediation:
          "The service replied, so it is running, but what came back was not what this page expected. That is usually something between the browser and the service rewriting the response, or a version of the app older than the service it is talking to.",
        origin: "unreadable-body",
        detail: detailOf(describeException(e), buildLine())
      };
    }
    // An exception that came from neither the service nor `request()`.
    // In practice that is a mapper throwing inside a `.then()` chained
    // onto a request that resolved, which is the shape #598 was reported
    // as, or a test double standing in for one of the above. Nothing types
    // it, so the exception's own words are the whole of what there is to
    // show, and showing them is the entire point: "TypeError: r.events.map
    // is not a function" names the defect exactly, and the sentence that
    // used to be printed in its place named nothing.
    return {
      message: "Backupd did not answer, or answered with something this page could not read.",
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
        "This failure did not come out of the service, so it has no id in any log. Check that the Backupd service is still running, then try again.",
      // Provenance genuinely not established: this exception came from
      // neither the service nor the transport, so nothing here may name a
      // hop, and App.tsx's unreachable gate must not fire on it.
      origin: "unknown",
      detail: detailOf(describeException(e), buildLine())
    };
  }

  // Issue #795. A refusal that came from something answering ON the
  // service's behalf — serve-ui's own reverse proxy, failing to reach the
  // engine — rather than from the service. Read before the code switch
  // below, because the code on one of those is `unknown` by construction
  // and `unknown` falls to the default arm, which shows the client's own
  // "the backup service returned an unexpected response" — the one
  // machine in the deployment that had not answered at all.
  //
  // Decided on recorded provenance, never on the status or the code, so
  // this can never speak over a service that answered a 503 WITH a
  // reason: its own sentence says more than anything written here could.
  if (isGatewayRefusal(api)) return describeGatewayRefusal(api);

  // Everything from here down is the service refusing in its own
  // vocabulary. Where provenance says otherwise (an untyped non-gateway
  // response, say a 403 from a proxy with an HTML page on it), it is
  // carried through as it was recorded rather than upgraded to
  // "service": the sentence below may be this module's fallback, and a
  // surface that asks "did the service answer this" gets the truth.
  const origin = api.origin;

  const correlationId = api.correlationId;
  switch (api.code) {
    case "RATE_LIMITED":
      return {
        message: "Too many attempts from this address.",
        remediation: "Backupd is refusing further attempts for the moment. Wait a minute, then try again.",
        correlationId,
        origin
      };
    case "CSRF_TOKEN_MISSING":
    case "CSRF_TOKEN_MISMATCH":
      return {
        message: "This page's security token is missing or out of date.",
        remediation: "Reload the page and enter the details again. Nothing was changed.",
        correlationId,
        origin
      };
    case "INTERNAL":
    case "INTERNAL_ERROR":
      return {
        message: fallbackMessage,
        remediation:
          "Backupd reported an internal error rather than a reason it could name. Its own log holds the detail, under this correlation id.",
        correlationId,
        origin
      };
    default:
      return { message: api.message || fallbackMessage, correlationId, origin };
  }
}

/**
 * Issue #795's two halves of one question: is this refusal the service's,
 * or is it something in front of the service answering because the
 * service could not be reached.
 *
 * The reported deployment is the reason there is a difference worth
 * drawing. Its web-ui container could not resolve the engine's name
 * ("dial tcp: lookup rclone-manager: no such host"), so every /api/v1
 * call was answered 502 by the proxy inside serve-ui with no body on it
 * at all, and the Activity page told the operator that the backup
 * service had returned something unexpected. It had returned nothing; it
 * had never been spoken to. The next step for that fault is in the OTHER
 * container, and the sentence on screen pointed away from it.
 *
 * What this reads is PROVENANCE, recorded by the code that held the
 * response (api/transport.ts), and never the status or the code (#795's
 * review). The first draft asked `code === "unknown" && status is a
 * gateway status`, and both halves of that are wrong in the same
 * direction: `unknown` is also what `toApiErrorCode` returns for a valid
 * code this BUNDLE has not heard of, and a gateway status is something a
 * service may answer with itself. Between them they took a typed 503 from
 * a service one version newer than this bundle — carrying an actionable
 * sentence of its own — and rewrote it as a proxy that could not reach
 * the engine. The service's own words are always worth more than
 * anything written here.
 */
function isGatewayRefusal(api: ApiError): boolean {
  // serve-ui said so on the response itself, which is the only positive
  // identification there is.
  if (api.origin === "gateway") return true;
  // A typed envelope, or a provenance nothing established: in neither
  // case may this name a hop. "service" is the service refusing in its
  // own words; absent is a hand-built ApiError, a mock, or a page's own
  // state, and inventing a topology for one of those is guessing.
  if (api.origin !== "unknown") return false;
  // An untyped response, unmarked, with a gateway status. That is either
  // serve-ui too old to mark its own proxy errors — this marker is newer
  // than the fix — or another proxy between the browser and the service
  // doing the same job; the wording below fits both, because in both
  // cases nothing in the response came from the service.
  return api.status === 502 || api.status === 503 || api.status === 504;
}

/**
 * Whether NOTHING from the service reached this browser.
 *
 * Exported for App.tsx, which has one gate to decide with it (#795's
 * review): the "Backupd is not answering" surface replaces the sign-in
 * form, and it may only do that for a failure where that sentence is
 * true. A 200 with an unreadable body, a typed refusal, a 403 from
 * something between the browser and the service — those are all Backupd
 * answering, and a heading saying otherwise above an ErrorState saying so
 * is a page contradicting itself.
 */
export function isServiceUnreachable(api: ApiError | null | undefined): boolean {
  return api?.origin === "no-response" || api?.origin === "gateway";
}

function describeGatewayRefusal(api: ApiError): OperatorFailure {
  return {
    message: "Backupd's web interface could not reach the Backupd service.",
    // Says which half is known to be working, because that is what makes
    // this actionable: the operator is reading a page, so the web
    // interface is up, and the thing to go and look at is the service
    // container behind it.
    remediation:
      "The page you are reading was served, so Backupd's web interface is running. It could not reach the service behind it, which is where this answer had to come from. Check that the Backupd service is running and that the web interface can still resolve it, then try again.",
    // A real id, unlike the no-response case: the web interface answered,
    // and it wrote this same id into its own log line for the failure
    // (webhost's proxy_error event).
    correlationId: api.correlationId,
    // Restated rather than copied from `api`: this branch is reached
    // both from serve-ui's own marker and from an unmarked, untyped
    // gateway status, and what the surfaces above need to know is the
    // conclusion — nothing in this response came from the service.
    origin: "gateway",
    detail: detailOf(api.status === undefined ? undefined : "status " + api.status, buildLine())
  };
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
      message: "Backupd did not answer.",
      remediation:
        "The request got no reply at all, so whether it was carried out is unknown. Check that the Backupd service is still running, then try again.",
      // No response, so no id. apiErrorOf's own rule, one failure over: an
      // id that matches nothing in any log is a false lead.
      origin: "no-response",
      detail: detailOf(requestedPath(e.path), describeException(e.cause), buildLine())
    };
  }
  return {
    message: "Backupd answered, and this page could not read the answer.",
    remediation:
      "The service replied, so it is running, but what came back was not what this page expected. That is usually something between the browser and the service rewriting the response, or a version of the app older than the service it is talking to.",
    correlationId: e.correlationId,
    origin: "unreadable-body",
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
  const failure = describeFailure(e, api?.message || "Backupd could not complete that request.");
  return {
    code: api?.code ?? "unknown",
    message: failure.message,
    remediation: failure.remediation,
    correlationId: failure.correlationId,
    // The RESOLVED provenance, which is the whole reason it is on this
    // shape: App.tsx's unreachable gate reads an ApiError, not an
    // OperatorFailure, and `describeFailure` above is the one place that
    // decides which of the five things a failure is.
    origin: failure.origin,
    detail: failure.detail
  };
}
