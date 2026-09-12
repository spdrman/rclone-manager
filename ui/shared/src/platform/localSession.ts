/**
 * The session read every local-account provider does, written once.
 *
 * # Why this exists (issue #795)
 *
 * Six provider bridges held a byte-identical copy of this, and every copy
 * said:
 *
 *     const res = await fetch("/api/v1/auth/session", ...);
 *     if (!res.ok) return { authenticated: false, ... };
 *
 * which reads any refusal at all as "this browser is not signed in". On
 * the deployment #795 was reported from, the web-ui container could not
 * reach the engine ("dial tcp: lookup rclone-manager: no such host"), so
 * every /api/v1 call — this one included — was answered 502 by serve-ui's
 * own reverse proxy. An operator who reloaded the page was therefore told
 * they were signed out, which is a claim this frontend had no evidence
 * for and which is false: their session was untouched, and the machine
 * that would have said otherwise was the one that could not be reached.
 *
 * It is the same defect as the one on the Activity page, one surface
 * earlier and worse: a transport failure rendered as a completely
 * different fact, with the sign-in form as the only thing offered and
 * signing in again as the one action guaranteed not to work.
 *
 * So: 401 and 403 are the answers that mean "not signed in", because they
 * are the answers the service gives when it has looked. Everything else
 * throws, labelled the way api/client.ts labels it, so PlatformContext
 * can hold onto the failure and App.tsx can say the service did not
 * answer rather than inventing a verdict about the operator's session.
 */
import { BackupdError, RequestFailure, toApiErrorCode } from "@shared/api/contracts";
import type { AuthContext } from "@shared/types/platform";

/** The route, spelled once. `request()` in api/client.ts does not expose a
 *  session call (the shape a bridge needs is not the shape the wire
 *  answers with), so this is the one other place in the frontend that
 *  talks to /api/v1 directly, and it labels its failures identically. */
const SESSION_PATH = "/auth/session";
const SESSION_URL = "/api/v1" + SESSION_PATH;

const SIGNED_OUT: AuthContext = { authenticated: false, username: null, mode: "local-account" };

/**
 * The signed-in identity on a provider whose identity IS the service's
 * own session cookie.
 *
 * Resolves `{ authenticated: false }` only when the service actually
 * said so. Rejects for everything else, with the same two typed shapes
 * api/client.ts throws, so api/failure.ts turns them into the same
 * sentences an operator already reads elsewhere.
 */
export async function readLocalAccountSession(): Promise<AuthContext> {
  let res: Response;
  try {
    res = await fetch(SESSION_URL, { credentials: "same-origin" });
  } catch (cause) {
    // Nothing answered. Whether there is a session is simply unknown,
    // and answering the question anyway is the bug this file exists for.
    throw new RequestFailure({ kind: "no-response", path: SESSION_PATH, cause });
  }

  // The service looked and said no. This is the ONLY refusal that means
  // the operator is not signed in.
  if (res.status === 401 || res.status === 403) return SIGNED_OUT;

  if (!res.ok) {
    // A refusal that is not about this session: the proxy could not
    // reach the engine (502), the engine is restarting (503), or the
    // engine broke (500). The status travels so api/failure.ts can tell
    // the gateway ones apart and name the hop that failed.
    let code: unknown;
    let message: unknown;
    try {
      const body = (await res.json()) as Record<string, unknown>;
      const nested = body.error;
      const from = nested && typeof nested === "object" ? (nested as Record<string, unknown>) : body;
      code = from.code;
      message = from.message;
    } catch {
      // A bodyless 502 from serve-ui's ErrorHandler is exactly this, and
      // it is the reported case.
    }
    throw new BackupdError({
      code: toApiErrorCode(code),
      message: typeof message === "string" ? message : "Backupd could not answer whether this browser is signed in.",
      correlationId: res.headers.get("x-correlation-id") ?? undefined,
      status: res.status
    });
  }

  try {
    const body = (await res.json()) as { username: string };
    return { authenticated: true, username: body.username, mode: "local-account" };
  } catch (cause) {
    // A 200 whose body will not parse is #598's other half, and it is no
    // more evidence of being signed out than a 502 is.
    throw new RequestFailure({
      kind: "unreadable-body",
      path: SESSION_PATH,
      status: res.status,
      contentType: res.headers.get("content-type") ?? undefined,
      correlationId: res.headers.get("x-correlation-id") ?? undefined,
      cause
    });
  }
}
