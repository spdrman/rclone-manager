/**
 * The lines this browser writes into the terminals, as opposed to the
 * ones the engine sends.
 *
 * # Why there has to be a second road into the same feed
 *
 * A refusal the engine produced can be emitted through `obs` and arrives
 * in the live activity ring like any other line. A refusal produced in
 * FRONT of the engine never reaches that ring at all: the destructive
 * gate, the CSRF check and a dead connection all refuse before any
 * handler runs, so there is nothing on the server that could log them.
 * Those are exactly the refusals that were invisible (issue #597: every
 * run submitted from a browser has been answered 403 on every build this
 * project has shipped, and the two live call sites did
 * `.then(reload)` with no `.catch`, so a 403, a 400, a 409 and a dead
 * engine were pixel-identical to a button with no handler at all).
 *
 * So the client writes them here, marked as coming from the browser
 * rather than from the engine, and the terminals read this node beside
 * the live feed. That is the epic's "refusals are part of the feature"
 * rule with a mechanism under it.
 *
 * # This is the seam G1.2 and G1.3 read
 *
 * The global terminal (G1.2) renders every notice; a per-set terminal
 * (G1.3) renders the ones whose `backupSetIds` name its set. Neither has
 * to know anything about run controls, and this file has to know nothing
 * about either terminal, which is why the two lanes can land in either
 * order.
 *
 * Both have landed, and the second one arrived late enough to be worth
 * recording: the per-set terminal read this node from the day it shipped
 * (#596) and the global one did not, so for two releases the dock's
 * "This browser" chip filtered a buffer that could not contain a browser
 * line and a deployment-wide refusal appeared in a banner and nowhere
 * else. The lesson is in this doc's own shape. A seam that names its
 * readers cannot tell you whether they turned up, and nothing failed when
 * one of them did not.
 *
 * RunControlNotice (components/) still renders the newest notice for a
 * scope as a banner. It was written as an interim surface for exactly
 * that gap, and it has outlived the gap; whether it stays is a decision
 * about screens rather than about this seam, and it is made there.
 */
import type { ApiErrorCode } from "@shared/api/contracts";
import { registerInput, graph } from "./graph";

/**
 * One line the browser wrote, about an action an operator took here.
 *
 * `code` is the typed code the service refused with, NOT a status. That
 * distinction is the whole reason this type exists: 403 is both
 * DESTRUCTIVE_OPERATIONS_DISABLED and a stale CSRF token, and 409 covers
 * three refusals an operator resolves in three different places. A
 * surface rendering by status cannot tell any of them apart.
 */
export interface BrowserNotice {
  /** Unique per notice, so a list can key on it. */
  id: string;
  /** When the browser wrote it, as epoch milliseconds. */
  at: number;
  /** ok for an accepted submission, refused for one that was not, and
   *  unreachable for a request that never got an answer at all. Three
   *  rather than two because "the service refused" and "there was no
   *  service" call for different next steps, and only the first of them
   *  can promise that nothing was changed. */
  outcome: "ok" | "refused" | "unreachable";
  /** The service's own typed code, or "unknown" when the failure never
   *  reached a service that could name one. */
  code: ApiErrorCode | "unknown";
  /** One sentence about what actually happened. */
  message: string;
  /** What to do about it, absent when there is genuinely nothing to
   *  suggest (api/failure.ts's OperatorFailure makes the same
   *  distinction, and for the same reason). */
  remediation?: string;
  /** The id the failing RESPONSE carried, absent when the request never
   *  reached the service and so appears in no log anywhere. */
  correlationId?: string;
  /**
   * Every backup set this line is about.
   *
   * One entry for a per-set run. For a deployment-wide run it is every
   * enabled set, because every one of them is a set the operator just
   * asked to have backed up and did not, and a refusal that landed only
   * on a dashboard would be invisible to somebody looking at the set
   * they care about. Empty means the line belongs to no set in
   * particular and only the global terminal shows it.
   */
  backupSetIds: string[];
  /**
   * The `backupd` command line this action is equivalent to, copy-pasteable
   * exactly as written.
   *
   * The epic's standing rule, and it earns its keep hardest on a refused
   * run: while the destructive gate is shut this command is the only
   * remaining way to start a backup, so printing it turns "a run has to
   * be started from the command line" into a sentence with something
   * actionable under it. It never carries a credential and cannot: the
   * connection details live in the configuration.
   */
  command?: string;
}

/**
 * The notices this session has written, oldest first.
 *
 * Capped, because it is a session-scoped ring and not a log: the durable
 * account of what happened lives in the operation journal and in the
 * engine's own event stream, and an unbounded array behind a page that
 * stays open for a week is a leak with no reader.
 */
export const browserNoticesNode = registerInput<BrowserNotice[]>("app.browserNotices", []);

/** How many notices the ring keeps. */
export const MAX_BROWSER_NOTICES = 200;

let sequence = 0;

/**
 * Writes one notice into the ring.
 *
 * The id and the timestamp are filled in here rather than by the caller,
 * so two notices written in the same millisecond still differ and a
 * caller cannot accidentally reuse an id.
 */
export function emitBrowserNotice(notice: Omit<BrowserNotice, "id" | "at">): BrowserNotice {
  sequence += 1;
  const written: BrowserNotice = { ...notice, id: "n" + sequence, at: Date.now() };
  graph.commit("browserNotices/emit", (tx) => {
    const next = [...graph.read(browserNoticesNode), written];
    tx.set(
      browserNoticesNode,
      next.length > MAX_BROWSER_NOTICES ? next.slice(next.length - MAX_BROWSER_NOTICES) : next
    );
  });
  return written;
}

/**
 * The notices about one backup set, oldest first.
 *
 * A line belongs to a set only by naming it. A deployment-wide refusal
 * therefore lists every enabled set explicitly rather than being matched
 * by an empty list, so a set that was never part of the run (disabled,
 * held for editing) is not told its backup was refused when it was never
 * going to run.
 */
export function noticesForBackupSet(notices: BrowserNotice[], backupSetId: string): BrowserNotice[] {
  return notices.filter((n) => n.backupSetIds.includes(backupSetId));
}

/** Test-only: empties the ring. Production never clears it; the cap
 *  above is what bounds it. */
export function clearBrowserNoticesForTests(): void {
  sequence = 0;
  graph.commit("browserNotices/clear", (tx) => tx.set(browserNoticesNode, []));
}
