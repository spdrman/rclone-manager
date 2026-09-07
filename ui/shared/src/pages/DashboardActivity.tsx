/**
 * The activity strips at the foot of the dashboard: one per backup set,
 * kept current (issue #573).
 *
 * # Why it polls, and why the service picks the interval
 *
 * The service answers with a `pollAfterMs`, and this asks again after
 * exactly that. It is the service that knows whether anything is moving,
 * so it is the service that gets to say how often to look: a client
 * picking its own number either hammers a NAS holding a dozen idle sets or
 * watches a live transfer at a cadence that makes the bar jump. Between a
 * running cycle and a quiet one that number differs by an order of
 * magnitude, and nothing here has to know which.
 *
 * There is no held-open connection, and that is a deployment decision
 * rather than a shortcut. This product runs on a NAS behind whatever
 * reverse proxy the operator already had, plus one of our own in the
 * two-container topology, and a stream is at the mercy of every buffering
 * and idle-timeout default in that path. A plain GET is not. The cursor is
 * what keeps polling cheap: a repeat call carries the highest sequence
 * already seen and gets only what is newer.
 *
 * # Why this owns its own fetch
 *
 * `useAsync` rather than a graph node, by the rule state/resource.ts
 * states: a fetch belongs on the graph when a SECOND surface reads the
 * answer, and nothing else reads this one. It also polls on a cadence of
 * its own, several times faster than the app-wide 30 second tick, which is
 * exactly the kind of thing a shared node should not be dragged into.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import { ErrorState } from "@shared/components/EmptyState";
import { ActivityStrip } from "@shared/pages/ActivityStrip";
import type { BackupSet } from "@shared/types/backup";
import type { LiveActivity, SetActivity, SetActivityEvent } from "@shared/types/activity";

/** How many lines the page holds per set.
 *
 * The service keeps a bounded tail of its own and this keeps a bounded
 * window onto it, because an unbounded buffer in a tab left open for a
 * week is a leak with a nice name. It is deliberately larger than the
 * strip shows, so scrolling back reaches something, and deliberately far
 * smaller than the durable record: the full history is the Activity page,
 * one click away. */
const PAGE_BUFFER = 300;

/** The cadence used when the service has not answered yet, or answered
 *  with nothing usable. Slow on purpose: the fast cadence is a claim that
 *  something is moving, and before the first answer nothing has claimed
 *  that. */
const FALLBACK_POLL_MS = 10_000;

/** How many events one poll asks for per set.
 *
 * The contract's own ceiling, deliberately, and it is the number that
 * keeps the cursor whole. The service hands back the OLDEST slice above
 * the cursor and says when a limit cut it short, so a smaller number is
 * safe but means catching up over several polls; asking for the ceiling
 * means the answer is never cut short at all, because the service's own
 * buffer holds no more than this. */
const POLL_LIMIT = 200;

/**
 * Folds a new reading into what the page already holds.
 *
 * The merge is by sequence and it is what makes the cursor safe. A poll
 * that asks for "everything after 412" comes back with a slice, not a
 * tail, so appending blindly would drop the lines above it and replacing
 * blindly would throw away everything before. Deduplicating by sequence
 * also makes a repeated or overlapping response harmless, which matters
 * because a retried request is not a rare event on a NAS.
 */
export function mergeActivity(previous: SetActivity | undefined, next: SetActivity): SetActivity {
  if (!previous) return { ...next, events: next.events.slice(-PAGE_BUFFER) };
  const bySequence = new Map<number, SetActivityEvent>();
  for (const e of previous.events) bySequence.set(e.sequence, e);
  for (const e of next.events) bySequence.set(e.sequence, e);
  const events = [...bySequence.values()].sort((a, b) => a.sequence - b.sequence).slice(-PAGE_BUFFER);
  return {
    ...next,
    events,
    // A gap is a fact about the buffer, not about the reading that
    // noticed it. Once lines have been lost this page's window has a hole
    // in it for as long as it holds those lines, and a later reading
    // answering a caught-up cursor cleanly does not fill it in.
    dropped: previous.dropped || next.dropped,
    // The page's own window can start later than the service's buffer
    // does, and the strip reads this to decide whether to say lines were
    // dropped. Reporting the service's bound while showing fewer lines
    // would claim continuity the page does not have.
    oldestSequence: Math.max(next.oldestSequence, events.length > 0 ? events[0].sequence : 0)
  };
}

/**
 * Whether this reading came from a different process than the one the
 * page has been following.
 *
 * The cursor is a sequence number and the sequence counter is per
 * process: it starts again at zero on every start. So a tab that reached
 * 412 asks a freshly started service for everything after 412, is told
 * correctly that there is nothing, and goes on showing a dead cycle's log
 * for as long as it stays open. Restarts are routine here, which makes
 * this the difference between a panel that says what is happening and one
 * that says what was happening the last time the engine was up.
 *
 * The epoch is the answer that cannot be fooled, because it is not a
 * counter and never climbs back past its old value. The sequence
 * comparison beside it is the fallback for a service too old to send one,
 * and it is sound for the same reason: within one process the highest
 * sequence only grows, so a reading below a cursor that service itself
 * handed out is a rewind no live feed can produce.
 */
export function feedRestarted(previous: { epoch: string | null; cursor: number }, next: LiveActivity): boolean {
  if (next.epoch && previous.epoch) return next.epoch !== previous.epoch;
  // A page holding nothing has no cursor to have been rewound, and a
  // reading with no sets in it is a deployment with no sets rather than a
  // feed that went backwards.
  if (previous.cursor === 0 || next.sets.length === 0) return false;
  return next.sets.reduce((highest, s) => Math.max(highest, s.latestSequence), 0) < previous.cursor;
}

/** The highest sequence anywhere in the page's buffer. It is the cursor
 *  the next poll sends, and it is taken across every set because the
 *  service's sequence counter is one counter for the whole process. */
function cursorOf(held: Map<string, SetActivity>): number {
  let highest = 0;
  for (const set of held.values()) {
    for (const e of set.events) if (e.sequence > highest) highest = e.sequence;
  }
  return highest;
}

export function DashboardActivity({ sets }: { sets: BackupSet[] | null }) {
  const api = useApi();
  const [held, setHeld] = useState<Map<string, SetActivity>>(new Map());

  // The cursor rides on a ref rather than in the dependency list. It
  // changes on every poll, and a fetch that re-created itself each time
  // its cursor moved would restart the timer below on every tick and
  // never actually elapse, which is the exact failure usePolling's own doc
  // warns about and which fails silently as "the strip stopped
  // refreshing".
  const cursor = useRef(0);

  // The process the cursor above belongs to. It rides on a ref for the
  // same reason the cursor does, and it is read before the cursor is
  // used rather than after: see feedRestarted.
  const epoch = useRef<string | null>(null);

  const live = useAsync<LiveActivity>(
    () => api.getLiveActivity({ since: cursor.current, limit: POLL_LIMIT }),
    [api]
  );

  useEffect(() => {
    const data = live.data;
    if (!data) return;
    // Decided out here rather than inside the updater, because it is a
    // decision about two readings rather than about the state, and an
    // updater React is free to run twice must not be where a ref is
    // rewound.
    const restarted = feedRestarted({ epoch: epoch.current, cursor: cursor.current }, data);
    epoch.current = data.epoch || null;
    if (restarted) cursor.current = 0;
    setHeld((current) => {
      // A restart drops everything held: those lines describe a cycle in
      // a process that no longer exists, and keeping them under a live
      // panel is the panel saying work is in flight that is not.
      const merged = restarted ? new Map<string, SetActivity>() : new Map(current);
      for (const set of data.sets) merged.set(set.setId, mergeActivity(merged.get(set.setId), set));
      cursor.current = cursorOf(merged);
      return merged;
    });
  }, [live.data]);

  // useAsync hands back a fresh `reload` on every render, so it cannot be
  // handed to an interval directly: the effect would tear down and rebuild
  // the timer before it ever fired. The ref makes one stable callback that
  // always calls the current one.
  const reload = useRef(live.reload);
  useEffect(() => {
    reload.current = live.reload;
  });
  const tick = useCallback(() => reload.current(), []);

  const pollAfterMs = live.data?.pollAfterMs && live.data.pollAfterMs > 0 ? live.data.pollAfterMs : FALLBACK_POLL_MS;
  useEffect(() => {
    const id = window.setInterval(tick, pollAfterMs);
    return () => window.clearInterval(id);
  }, [pollAfterMs, tick]);

  if (!sets || sets.length === 0) return null;

  return (
    <section className="card" aria-label="Activity">
      <div className="card__header">
        <h2 className="eyebrow">Activity</h2>
        <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
          {live.error ? "not refreshing" : "refreshing every " + Math.round(pollAfterMs / 1000) + "s"}
        </span>
      </div>
      {/* A failed poll states itself and leaves the strips where they are.
          The last good reading stays on screen under the notice, because
          blanking the panel an operator is reading is a worse answer than
          an old one that says it is old. */}
      {live.error ? (
        <div style={{ padding: "var(--space-3) var(--space-5) 0" }}>
          <ErrorState {...live.error} onRetry={live.reload} />
        </div>
      ) : null}
      {/* A failed poll makes every reading below it a reading from the
          past, and the strips are told so rather than left drawing a
          pulsing bar and a byte rate that both assert the process is
          alive. That is the one thing nobody knows while the poll is
          failing, and it is the whole distinction this panel exists to
          draw. */}
      {sets.map((set, i) => (
        <div key={set.id} style={{ borderTop: i === 0 ? undefined : "1px solid var(--border)" }}>
          <ActivityStrip set={set} activity={held.get(set.id) ?? null} stale={live.error !== null} />
        </div>
      ))}
    </section>
  );
}
