/**
 * One backup set's activity terminal, on that set's own page (issue
 * #596).
 *
 * # The three surfaces, and why none of them is the other two
 *
 * The DASHBOARD strip (#573) answers "which of my sets is doing
 * something", read at a glance across a row of them. The GLOBAL DOCK
 * (#599) answers "what has this deployment done, most recently, including
 * the thing I just clicked", and it is where you look when you do not yet
 * know which set is in trouble. This one is where you look when you DO.
 *
 * They read one buffer in one process, so a line cannot appear in one and
 * not the others, and the narrowing is a query parameter the route
 * already takes rather than a second feed. Keeping this one rather than
 * saying "the dock is enough" is about legibility: on a deployment with a
 * dozen sets mid-cycle, the dock is a wall, and the whole difference
 * between a useful log and a wall is being able to read one set's traffic
 * without scanning every other set's.
 *
 * It renders ActivityStrip, unchanged, which is what makes it the same
 * progress bar, the same step sentence, the same per-event rendering and
 * the same Copy and Save as the dashboard. A second renderer here would
 * be a second answer to "what does this event say", and the export
 * everybody pastes into an issue would depend on which screen they were
 * on.
 *
 * # What the browser writes here
 *
 * A refusal the engine produced arrives on the feed like any other line.
 * A refusal produced in FRONT of the engine never reaches that ring at
 * all: a CSRF failure is answered by middleware and a dead connection is
 * answered by nobody, so there is nothing on the server that could have
 * logged it. Those come from state/browserNotices, the seam G1.4 built
 * for exactly this, and they are folded into the same log MARKED as
 * coming from the browser rather than from the engine. Marked, because
 * "the engine said this" and "your browser could not reach the engine to
 * ask" are different facts and only one of them is evidence about the
 * NAS.
 */
import { useMemo } from "react";
import { ActivityStrip } from "@shared/pages/ActivityStrip";
import { ErrorState } from "@shared/components/EmptyState";
import { useCausl } from "@shared/state/graph";
import { browserNoticesNode, noticesForBackupSet } from "@shared/state/browserNotices";
import type { BrowserNotice } from "@shared/state/browserNotices";
import type { ActivityFeed } from "@shared/pages/useActivityFeed";
import type { BackupSet } from "@shared/types/backup";
import type { SetActivity, SetActivityEvent } from "@shared/types/activity";

/** The event name a line this browser wrote carries on the feed.
 *
 * Its own name rather than reusing `api_action`: an api_action is a line
 * the ENGINE emitted about a request it served, and one of the two
 * things this panel has to keep straight is that a browser notice is the
 * opposite, a request that never got that far. ActivityStrip's
 * activityLine renders it, so it reads the same here, in the dock and in
 * every export. */
export const BROWSER_NOTICE_EVENT = "browser_notice";

/**
 * One browser notice as a line on the feed.
 *
 * The sequence is NEGATIVE and descending, which is not a trick: the
 * engine's counter is strictly positive, so nothing this browser writes
 * can ever collide with a real sequence number, and the log's `key` and
 * its scroll-follow both keep working. It also means a notice can never
 * be mistaken for something the engine handed out, which is the property
 * that matters if one of these ever reaches an export somebody quotes.
 */
export function noticeAsEvent(notice: BrowserNotice, index: number): SetActivityEvent {
  const fields: Record<string, string> = { outcome: notice.outcome, code: notice.code };
  if (notice.remediation) fields.remediation = notice.remediation;
  if (notice.correlationId) fields.correlation_id = notice.correlationId;
  if (notice.command) fields.command = notice.command;
  return {
    sequence: -(index + 1),
    at: new Date(notice.at).toISOString(),
    level: notice.outcome === "ok" ? "info" : "warn",
    // Stated, in the same vocabulary the engine states its own results
    // in (issue #625), so the renderer colours a browser notice by
    // reading a field rather than by carrying a rule about this event.
    // The notice's own finer word (ok, refused, unreachable) stays in
    // the fields, which is where the browser's vocabulary belongs.
    result: notice.outcome === "ok" ? "success" : "warn",
    event: BROWSER_NOTICE_EVENT,
    scope: "set",
    message: notice.message,
    fields
  };
}

/**
 * The set's engine feed with this browser's own notices folded in, in
 * the order the two happened.
 *
 * Ordered by TIME rather than by sequence, because the two sides do not
 * share a counter and cannot: a notice is written in a browser that has
 * no access to the engine's. Time is the only thing they do share, and
 * it is what an operator is comparing anyway when they read a log.
 */
export function foldNotices(activity: SetActivity | null, notices: BrowserNotice[]): SetActivity | null {
  if (activity === null) {
    // Nothing has loaded yet. Returning a synthetic activity here would
    // draw a progress bar and a state for a set nothing has reported on,
    // which is the one claim ActivityStrip's own null branch exists to
    // refuse. A notice waits for the first reading, which is one poll.
    return null;
  }
  if (notices.length === 0) return activity;
  const written = notices.map(noticeAsEvent);
  const merged = [...activity.events, ...written].sort((a, b) => Date.parse(a.at) - Date.parse(b.at));
  return { ...activity, events: merged };
}

/**
 * The panel.
 *
 * `feed` is passed in rather than polled here, so the page that owns the
 * `Test Connection` button can ask the feed for a fresh reading the
 * moment that request answers. The engine records every step before it
 * replies, so one poll at that moment is the difference between a
 * terminal that fills in immediately and one that fills in when the timer
 * next happens to fire.
 */
export function SetActivityPanel({ set, feed }: { set: BackupSet; feed: ActivityFeed }) {
  const notices = useCausl(browserNoticesNode);
  const mine = useMemo(() => noticesForBackupSet(notices, set.id), [notices, set.id]);
  const activity = useMemo(() => foldNotices(feed.held.get(set.id) ?? null, mine), [feed.held, set.id, mine]);

  return (
    <div>
      <div
        style={{
          display: "flex", justifyContent: "flex-end",
          fontSize: "var(--text-xs)", color: "var(--text-3)", paddingBottom: "var(--space-2)"
        }}
      >
        {feed.error ? "not refreshing" : "refreshing every " + Math.round(feed.pollAfterMs / 1000) + "s"}
      </div>
      {/* A failed poll states itself and leaves the strip where it is.
          Blanking a panel somebody is reading is a worse answer than an
          old one that says it is old. */}
      {feed.error ? (
        <div style={{ paddingBottom: "var(--space-3)" }}>
          <ErrorState {...feed.error} onRetry={feed.refresh} />
        </div>
      ) : null}
      {/* `heading={false}`: the page header two lines up already carries
          this set's name, host, remote folder, read-only flag and health
          badge, and the strip's own version of all five would be five
          lines of chrome between an operator and the log they opened the
          page for. That is also what the mockup draws.

          `stale` is what takes down the pulse, the sweep, the spinner,
          the byte rate and the time remaining while the poll behind this
          reading is failing. The fraction stays: it was measured, and it
          has not stopped being what was measured. */}
      <ActivityStrip set={set} activity={activity} stale={feed.error !== null} heading={false} />
    </div>
  );
}
