/**
 * Issue #795. The two surfaces for "this browser could not find out
 * whether it has a session", and why there are two of them.
 *
 * Either one replaces the sign-in form, and replacing it is the entire
 * point. The deployment this was reported from had a web-ui container that
 * could not resolve the engine's name, so /api/v1/auth/session came back
 * 502 from serve-ui's own reverse proxy; the app read that as "not signed
 * in" and offered a login form that posts down the same dead hop. Every
 * attempt fails, nothing on screen says why, and the operator is left
 * believing their credentials stopped working.
 *
 * Neither offers navigation, for the same reason ConfigurationSavedPage
 * has none: every route here would ask the machine that just failed to
 * answer. What they do offer is the one thing that can change the answer —
 * asking again — and the failure's own facts, so the correlation id the
 * service or the proxy logged can be read off the screen.
 *
 * # Why the heading is not always "Backupd is not answering"
 *
 * Because sometimes it answered (#795's review). The session read rejects
 * for four different reasons, and only two of them mean nothing came back:
 * a `fetch` that got no response, and a refusal written by the proxy in
 * front of the service. The other two — a 200 whose body would not parse,
 * and a typed refusal that is not a verdict about the session (a 403 from
 * a gateway rule, a 500 the engine typed) — are Backupd ANSWERING, and the
 * first draft of this page put "Backupd is not answering" as an h1 above
 * an ErrorState whose own first line said the opposite. A page that
 * contradicts itself is a page an operator cannot act on, so the one that
 * claims nothing came back is now shown only when nothing came back.
 *
 * The shell's own surfaces (ActivityPage, the dashboard's Recent activity
 * panel) say the same things one layer in, for the session that is already
 * loaded when the engine goes away. This is the layer for the reload
 * afterwards, which is the first thing anybody does.
 */
import type { ReactNode } from "react";
import { ErrorState } from "@shared/components/EmptyState";
import type { ApiError } from "@shared/api/contracts";

/** The frame both surfaces share, so the only difference between them is
 *  the two things that genuinely differ: the heading and the paragraph
 *  under it. The typed failure itself is rendered by the same component
 *  every other surface reports one with, which is what keeps the wording
 *  an operator quotes in a bug report the wording the rest of the app
 *  uses. */
function SessionCheckFrame({
  heading,
  children,
  error,
  onRetry
}: {
  heading: string;
  children: ReactNode;
  error: ApiError;
  onRetry(): void;
}) {
  return (
    <div style={{ maxWidth: 720, margin: "0 auto", padding: "48px 20px" }}>
      <h1 style={{ marginTop: 0 }}>{heading}</h1>
      <p style={{ color: "var(--text-2)" }}>{children}</p>
      <ErrorState {...error} onRetry={onRetry} />
    </div>
  );
}

/**
 * Nothing came back at all: the request got no response, or the response
 * was written by the proxy in front of the service and carries none of the
 * service's own words (api/failure.ts's `isServiceUnreachable`).
 */
export function ServiceUnreachablePage({
  error,
  onRetry
}: {
  error: ApiError;
  onRetry(): void;
}) {
  return (
    <SessionCheckFrame heading="Backupd is not answering" error={error} onRetry={onRetry}>
      {/* Issue #795's container run found the first draft of this
          paragraph claiming "You have not been signed out", which is a
          promise this page cannot keep. The engine holds its sessions in
          its own process (apps/common/auth/local), so the restart that
          #795's own report describes ends them: by the time the service
          answers again the operator IS signed out, and a page that told
          them otherwise a minute earlier was wrong in the most common
          case it exists for. What is actually known is only that nobody
          answered, so that is all this says. */}
      {"This page could not reach Backupd to ask whether you are signed in, so it is " +
        "not going to guess. Signing in now would send your details down the same " +
        "connection that is failing; when Backupd is answering again, Try again will " +
        "put you back where you were, or ask you to sign in if the service restarted."}
    </SessionCheckFrame>
  );
}

/**
 * Backupd answered, and what came back was not an answer about this
 * session: a body that would not parse, or a refusal that is about
 * something other than being signed in (a policy denial, an internal
 * error).
 *
 * Deliberately says less than the page above. It does not name a hop,
 * because which machine produced the refusal is exactly what is not
 * established here, and it does not promise that signing in would fail,
 * because for some of these it might work — the difference is that the app
 * has no grounds to put a login form up as if the operator's session were
 * simply absent. The typed failure under it carries the service's own
 * words, which on this path are the most specific thing anybody has.
 */
export function SessionCheckFailedPage({
  error,
  onRetry
}: {
  error: ApiError;
  onRetry(): void;
}) {
  return (
    <SessionCheckFrame
      heading="Backupd could not check your session"
      error={error}
      onRetry={onRetry}
    >
      {"Backupd answered, but not with an answer about whether you are signed in, so " +
        "this page is not going to guess either way. What it did say is below. Try " +
        "again re-asks; if the answer keeps coming back like this, it is the " +
        "deployment that needs looking at rather than your password."}
    </SessionCheckFrame>
  );
}
