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
    // The page's own window can start later than the service's buffer
    // does, and the strip reads this to decide whether to say lines were
    // dropped. Reporting the service's bound while showing fewer lines
    // would claim continuity the page does not have.
    oldestSequence: Math.max(next.oldestSequence, events.length > 0 ? events[0].sequence : 0)
  };
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

  const live = useAsync<LiveActivity>(() => api.getLiveActivity({ since: cursor.current }), [api]);

  useEffect(() => {
    if (!live.data) return;
    setHeld((current) => {
      const merged = new Map(current);
      for (const set of live.data!.sets) merged.set(set.setId, mergeActivity(current.get(set.setId), set));
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
      {sets.map((set, i) => (
        <div key={set.id} style={{ borderTop: i === 0 ? undefined : "1px solid var(--border)" }}>
          <ActivityStrip set={set} activity={held.get(set.id) ?? null} />
        </div>
      ))}
    </section>
  );
}
