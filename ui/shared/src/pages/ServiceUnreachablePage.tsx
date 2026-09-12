/**
 * Issue #795. What an operator sees when this browser could not find out
 * whether it has a session, because nothing answered the question.
 *
 * It replaces the sign-in form, and replacing it is the entire point. The
 * deployment this was reported from had a web-ui container that could not
 * resolve the engine's name, so /api/v1/auth/session came back 502 from
 * serve-ui's own reverse proxy; the app read that as "not signed in" and
 * offered a login form that posts down the same dead hop. Every attempt
 * fails, nothing on screen says why, and the operator is left believing
 * their credentials stopped working.
 *
 * No navigation, for the same reason ConfigurationSavedPage has none:
 * every route here would ask the machine that is not answering. What it
 * does offer is the one thing that can change the answer — asking again,
 * once the other container is back — and the failure's own facts, so the
 * correlation id serve-ui logged this under can be read off the screen.
 *
 * The shell's own surfaces (ActivityPage, the dashboard's Recent activity
 * panel) say the same thing one layer in, for the session that is already
 * loaded when the engine goes away. This is the layer for the reload
 * afterwards, which is the first thing anybody does.
 */
import { ErrorState } from "@shared/components/EmptyState";
import type { ApiError } from "@shared/api/contracts";

export function ServiceUnreachablePage({
  error,
  onRetry
}: {
  error: ApiError;
  onRetry(): void;
}) {
  return (
    <div style={{ maxWidth: 720, margin: "0 auto", padding: "48px 20px" }}>
      <h1 style={{ marginTop: 0 }}>Backupd is not answering</h1>
      {/* Issue #795's container run found the first draft of this
          paragraph claiming "You have not been signed out", which is a
          promise this page cannot keep. The engine holds its sessions in
          its own process (apps/common/auth/local), so the restart that
          #795's own report describes ends them: by the time the service
          answers again the operator IS signed out, and a page that told
          them otherwise a minute earlier was wrong in the most common
          case it exists for. What is actually known is only that nobody
          answered, so that is all this says. */}
      <p style={{ color: "var(--text-2)" }}>
        This page could not reach Backupd to ask whether you are signed in, so it is
        not going to guess. Signing in now would send your details down the same
        connection that is failing; when Backupd is answering again, Try again will
        put you back where you were, or ask you to sign in if the service restarted.
      </p>
      {/* The typed failure verbatim, through the same component every
          other surface reports one with, so the wording an operator
          quotes in a bug report is the wording the rest of the app uses:
          "did not answer" for a request that got no reply, "could not
          reach the Backupd service" for a proxy that answered for it. */}
      <ErrorState {...error} onRetry={onRetry} />
    </div>
  );
}
