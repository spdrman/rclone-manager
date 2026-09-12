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
 * So: 401 is the answer that means "not signed in", and everything else
 * throws, labelled the way api/client.ts labels it, so PlatformContext
 * can hold onto the failure and App.tsx can say what is actually known
 * rather than inventing a verdict about the operator's session.
 *
 * # 401, and deliberately not 403 (#795's review)
 *
 * Both used to resolve signed-out, and 403 does not belong there. The
 * route's contract is 401 for an absent or expired session
 * (apps/common/auth/local), which is precisely the refusal a sign-in
 * fixes. A 403 is a policy denial — a gateway rule, a proxy that requires
 * a header the browser does not send, an authorisation decision made
 * above the session — and re-signing-in cannot fix any of them. Sending
 * an operator to the login form for one is #795's own shape again: a
 * failure rendered as a different fact, under the one action that cannot
 * address it. It goes down the typed path with every other refusal, where
 * the service's own words are shown.
 */
import { apiErrorFromResponse, nameAttempt, noResponse, refusal, unreadableBody } from "@shared/api/transport";
import type { AuthContext } from "@shared/types/platform";

/** The route, spelled once. `request()` in api/client.ts does not expose a
 *  session call (the shape a bridge needs is not the shape the wire
 *  answers with), so this is the one other place in the frontend that
 *  talks to /api/v1 directly. Everything about HOW it talks is shared
 *  with `request()` through api/transport.ts, which is what makes the two
 *  label their failures identically instead of nearly identically. */
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
  // The attempt header, named the same way every other request in this
  // bundle names one. This call is the FIRST the app makes and the one
  // #795 was reported through, and it was the only one that went out
  // anonymously: a request that gets no response has no correlation id
  // to quote, so without this there was nothing at all to match the
  // browser's failure against the server's own line for it.
  const headers: Record<string, string> = {};
  const attemptId = nameAttempt(headers);

  let res: Response;
  try {
    res = await fetch(SESSION_URL, { credentials: "same-origin", headers });
  } catch (cause) {
    // Nothing answered. Whether there is a session is simply unknown,
    // and answering the question anyway is the bug this file exists for.
    throw noResponse({
      path: SESSION_PATH,
      url: SESSION_URL,
      method: "GET",
      attemptId,
      cause
    });
  }

  // The service looked and said no. This is the ONLY refusal that means
  // the operator is not signed in.
  if (res.status === 401) return SIGNED_OUT;

  if (!res.ok) {
    // A refusal that is not an answer about this session: the proxy could
    // not reach the engine (502, and it marks its own responses now), the
    // engine is restarting (503), the engine broke (500), or something
    // between the two denied the request outright (403). The provenance
    // travels with it, so api/failure.ts names the hop that failed only
    // where it really is the hop that failed.
    throw refusal(
      await apiErrorFromResponse(
        res,
        "Backupd could not answer whether this browser is signed in."
      ),
      { path: SESSION_PATH, attemptId }
    );
  }

  try {
    const body = (await res.json()) as { username: string };
    return { authenticated: true, username: body.username, mode: "local-account" };
  } catch (cause) {
    // A 200 whose body will not parse is #598's other half, and it is no
    // more evidence of being signed out than a 502 is.
    throw unreadableBody({ path: SESSION_PATH, res, attemptId, cause });
  }
}
