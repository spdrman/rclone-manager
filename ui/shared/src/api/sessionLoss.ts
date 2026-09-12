/**
 * The one signal for "this read was refused because the session is gone",
 * and the reason it is a signal rather than a per-page branch.
 *
 * # What went wrong (issue #795, found in the container run)
 *
 * The engine's sessions live in that process (apps/common/auth/local),
 * so a restart ends every one of them. #795's own story is a restart: the
 * engine container went away and came back. What an operator met when it
 * came back was the Activity page holding a red panel reading
 * "authentication required" with a Try again button under it — and Try
 * again cannot succeed, because the thing that failed is not the read.
 * A page-shaped retry for a session-shaped failure is a dead end with a
 * button on it, which is worse than a dead end without one.
 *
 * Reloading DID produce the sign-in form, so the way out existed and was
 * simply never offered. This is what offers it: a refusal that says the
 * session is gone re-asks the one question the app shell gates on, and
 * the shell then renders the sign-in form the way it does for any other
 * browser without a session.
 *
 * # Why a module-level listener
 *
 * One app, one session, exactly as PlatformContext's own `authRequestSeq`
 * counter is a module-level number for the same reason. The alternative —
 * threading a callback from PlatformProvider down through ApiContext into
 * both fetch hooks — would put a platform concern in the signature of
 * every read in the app to express something there is only ever one of.
 *
 * The api layer reports, the platform layer decides. Nothing here knows
 * what "gone" should cause; it only knows that a refusal said so.
 */

/** The subscriber, or null before a PlatformProvider has mounted (tests
 *  that drive a page in isolation never register one, and a report with
 *  nobody listening is correctly a no-op). */
let listener: (() => void) | null = null;

/**
 * Registers the one listener and answers with its removal, for a
 * provider's effect cleanup. A second registration replaces the first
 * rather than accumulating: two listeners would mean two auth refetches
 * per refusal, and the second provider is the live one.
 */
export function onSessionLost(callback: () => void): () => void {
  listener = callback;
  return () => {
    if (listener === callback) listener = null;
  };
}

/** Called by the two fetch hooks when a read was refused with
 *  UNAUTHENTICATED. Deliberately says nothing about what follows. */
export function reportSessionLost(): void {
  listener?.();
}
