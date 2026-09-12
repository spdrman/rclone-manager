/**
 * The typed-request plumbing both /api/v1 callers share.
 *
 * # Why this file exists (issue #795's review)
 *
 * There are two of those callers and there always were, which is the
 * problem. `api/client.ts`'s `request()` is the one everything above the
 * platform layer goes through, and `platform/localSession.ts` is the one
 * that reads `/auth/session` — a shape `request()` cannot express,
 * because the answer it needs is "signed in, or not", and a 401 there is
 * an ANSWER rather than a refusal. So the session reader had its own
 * fetch, and with it its own copy of the envelope parsing and its own
 * silence where `request()` has diagnostics.
 *
 * That silence was on the worst possible failure. `/auth/session` is the
 * FIRST call this app makes, the one #795 was reported through, and the
 * one whose failure leaves the operator with a sign-in form and no
 * explanation — and it was the one call that carried no
 * `X-Client-Attempt-Id`, so a request that got no response could not be
 * matched against the server's own log line for it, and wrote no
 * `request.no-response` line when diagnostics were on.
 *
 * What is here is therefore everything both callers must do IDENTICALLY,
 * and nothing either does alone: name the attempt, label the two failures
 * that are not refusals, and read a refusal's envelope and provenance.
 * The fetch itself stays at each call site on purpose — `client.ts` holds
 * the CSRF, idempotency and bootstrap-token rules, `localSession.ts` holds
 * the 401 policy, and scripts/api/check-client-paths.sh additionally
 * requires that `client.ts`'s single `fetch()` literally reads
 * `fetch(BASE + path`, so that it can prove statically which URLs this
 * bundle can request.
 */
import { BackupdError, RequestFailure, toApiErrorCode } from "./contracts";
import type { ApiError, FailureOrigin } from "./contracts";
import { debugEnvironment, debugLog, describeError } from "./debug";

/**
 * The header carrying this browser's own name for one attempt, and the
 * generator for it (issue #730's review).
 *
 * Why it exists at all: a correlation id travels on a RESPONSE, and the
 * fault this whole diagnostic was built for is a request that gets no
 * response. So the browser names the attempt on the way out, the server
 * writes that name into its line for the request
 * (apps/common/webhost/requestscope.go), and a console screenshot
 * showing `request.no-response` can then be matched against a server log
 * that proves the request arrived and what was sent back - which is
 * exactly the question "TypeError: Failed to fetch" leaves open.
 *
 * Bounded to sixteen hex characters, in the character set the server is
 * willing to write down (it drops anything longer or stranger rather
 * than truncating it), so this header can never be the reason a log line
 * is unreadable. `getRandomValues` where the browser has it and
 * `Math.random` where it does not: a NAS is reached over plain HTTP on a
 * local network, where parts of WebCrypto are unavailable, and a
 * diagnostic id is not a secret - uniqueness among a handful of
 * in-flight requests is the entire requirement.
 */
export const CLIENT_ATTEMPT_HEADER = "X-Client-Attempt-Id";

/**
 * serve-ui's own marker for a response IT wrote because it could not
 * reach the engine (apps/common/webhost/serve/ui.go's ErrorHandler, whose
 * ProxyErrorHeader is this same name).
 *
 * The one fact no status code can carry. A bodyless 502 is what that
 * proxy writes when the engine is gone, and it is also a perfectly
 * legitimate answer for a service to type itself, so a client reading
 * topology out of "502" gets one of the two wrong whichever way it
 * guesses. Lower case because `Headers.get` is case-insensitive and this
 * is only ever passed to it.
 */
export const PROXY_ERROR_HEADER = "x-backupd-proxy-error";

function newClientAttemptId(): string {
  const bytes = new Uint8Array(8);
  const c = globalThis.crypto;
  if (c && typeof c.getRandomValues === "function") c.getRandomValues(bytes);
  else for (let i = 0; i < bytes.length; i++) bytes[i] = Math.floor(Math.random() * 256);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

/** Names one attempt on the outgoing headers and returns the name, so the
 *  caller can put it in its own diagnostics. Minted per ATTEMPT,
 *  deliberately unlike an Idempotency-Key, which is per logical
 *  submission and is reused across retries: what this answers is "which
 *  of my three tries is the line in your log", and a value shared by all
 *  three cannot. */
export function nameAttempt(headers: Record<string, string>): string {
  const attemptId = newClientAttemptId();
  headers[CLIENT_ATTEMPT_HEADER] = attemptId;
  return attemptId;
}

/**
 * `fetch` rejected: nothing came back, so there is no status, no content
 * type and no id, and whether the request was carried out is unknown.
 *
 * Returned rather than thrown so the call site keeps its `throw` and the
 * control flow stays readable there. The diagnostic is written here,
 * because it is the same line for both callers and because it is the
 * line #730 exists for: one deployment's Activity page reaches this with
 * `TypeError: Failed to fetch` while curl to the same route answers 401.
 * The typed failure is all an operator sees; this is everything the
 * browser knew and could not put in it, and it is written only when
 * diagnostics were asked for.
 */
export function noResponse(info: {
  /** The API path asked for, WITHOUT the base prefix. */
  path: string;
  /** The URL actually requested, which the path alone does not give. */
  url: string;
  method: string;
  attemptId: string;
  cause: unknown;
  /** `performance.now()` from just before the fetch, or undefined when
   *  the caller was not timing it. Reported only when it really ran:
   *  zero for "nobody timed this" would claim an instant failure for
   *  exactly the request whose duration is the interesting part. */
  startedAt?: number;
}): RequestFailure {
  debugLog(
    "request.no-response",
    () => ({
      path: info.path,
      url: info.url,
      method: info.method,
      attemptId: info.attemptId,
      cause: describeError(info.cause),
      ...debugEnvironment(),
      ...(info.startedAt === undefined
        ? {}
        : { elapsedMs: Math.round(performance.now() - info.startedAt) })
    }),
    "error"
  );
  return new RequestFailure({ kind: "no-response", path: info.path, cause: info.cause });
}

/** A response arrived and this build could not read its body. The status
 *  and the content type are what separate a proxy's HTML error page from
 *  a body that was cut off mid-transfer, and the correlation id is read
 *  on the SUCCESS path as well as on a refusal (#598) so a body that
 *  fails to parse can still name the response it came from. */
export function unreadableBody(info: {
  path: string;
  res: Response;
  attemptId: string;
  cause: unknown;
}): RequestFailure {
  const status = info.res.status;
  const contentType = info.res.headers.get("content-type") ?? undefined;
  const correlationId = info.res.headers.get("x-correlation-id") ?? undefined;
  debugLog(
    "request.unreadable-body",
    () => ({
      path: info.path,
      status,
      contentType,
      correlationId,
      attemptId: info.attemptId,
      cause: describeError(info.cause)
    }),
    "error"
  );
  return new RequestFailure({
    kind: "unreadable-body",
    path: info.path,
    status,
    contentType,
    correlationId,
    cause: info.cause
  });
}

/**
 * A refusal, read off the response, with its PROVENANCE recorded rather
 * than left to be guessed at downstream (#795's review).
 *
 * The service always returns a typed error envelope, but not always the
 * SAME shape: apps/common/auth/local's own routes (login, enroll,
 * password rotation, logout) answer flat — { code, message,
 * correlationId } all at the top level — while apps/common/webhost's
 * routes nest code/message under an "error" key and carry the correlation
 * id only in the X-Correlation-Id response header, never the body (see
 * that package's errors.go). Both are read here, rather than one shape
 * being picked and the other's errors arriving as silently-undefined
 * fields.
 *
 * `origin` is the part that is new and the part that matters. "A typed
 * envelope was parsed" is a fact about the response and is knowable only
 * here; `code === "unknown"` was standing in for it and is not the same
 * thing, because that is also what an unrecognised-but-valid code
 * degrades to. A service one version ahead, refusing a 503 with an
 * actionable sentence in it, was being told it was a proxy that could not
 * reach itself.
 */
export async function apiErrorFromResponse(
  res: Response,
  fallbackMessage: string
): Promise<ApiError> {
  const headerCorrelationId = res.headers.get("x-correlation-id") ?? undefined;
  // The marker outranks the body, and can, because only serve-ui's own
  // ErrorHandler is ever allowed to set it: ModifyResponse deletes any
  // copy an upstream sent, so a response carrying it cannot have come
  // from the engine.
  const marked = res.headers.get(PROXY_ERROR_HEADER) !== null;

  let body: Record<string, unknown> | null = null;
  try {
    body = (await res.json()) as Record<string, unknown>;
  } catch {
    // A bodyless 502 from serve-ui's ErrorHandler is exactly this, and
    // it is the reported case: nothing to parse, because nothing was
    // written.
  }

  const nested = body?.error;
  const envelope =
    nested && typeof nested === "object" ? (nested as Record<string, unknown>) : body;
  const code = envelope?.code;
  const message = envelope?.message;
  // What makes a body an ENVELOPE rather than merely JSON: the service
  // names the refusal, or states it in words. A JSON body with neither is
  // not the service refusing in its own vocabulary, and is treated as the
  // untyped response it is.
  const typed = typeof code === "string" || typeof message === "string";

  const origin: FailureOrigin = marked ? "gateway" : typed ? "service" : "unknown";

  return {
    code: toApiErrorCode(code),
    message: typeof message === "string" ? message : fallbackMessage,
    // The nested shape never carries an id in the body; the flat one
    // does, and the header is the fallback for both.
    correlationId:
      (nested && typeof nested === "object"
        ? undefined
        : (body?.correlationId as string | undefined)) ?? headerCorrelationId,
    status: res.status,
    origin
  };
}

/** The refusal, thrown, with the debug line that joins it to the
 *  server's own record of it. #730: the correlation id is the one string
 *  that connects the two, and until that issue it only ever reached the
 *  screen — a refusal that renders as "Failed to fetch" in a bug report
 *  is one nobody can match up; a logged id is. */
export function refusal(api: ApiError, info: { path: string; attemptId: string }): BackupdError {
  debugLog(
    "request.error-status",
    () => ({
      path: info.path,
      status: api.status,
      code: api.code,
      origin: api.origin,
      correlationId: api.correlationId,
      attemptId: info.attemptId
    }),
    "error"
  );
  return new BackupdError(api);
}
