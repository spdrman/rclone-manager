/**
 * The live-activity polling loop, once, for every surface that follows
 * the feed (issue #596).
 *
 * It was DashboardActivity's own until this issue, and that file's doc
 * gave the right reason for it being there: "a fetch belongs on the graph
 * when a SECOND surface reads the answer, and nothing else reads this
 * one". This issue is that second surface. The backup set detail page
 * follows the same feed at the same cadence, so the loop comes out here
 * as a hook rather than going onto the graph, because the two surfaces
 * ask DIFFERENT questions of it: the dashboard asks about every set at
 * once and a set's own page asks about one, carrying a `backup_set`. A
 * shared graph node holds one answer, and these are two.
 *
 * Everything the loop was already careful about is careful here for the
 * same reasons, and those reasons are in DashboardActivity's own doc:
 * the service picks the interval because it is the one that knows whether
 * anything is moving; there is no held-open connection because this runs
 * on a NAS behind whatever reverse proxy the operator already had; and
 * the cursor is what keeps polling cheap.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import type { ApiError } from "@shared/api/contracts";
import type { LiveActivity, SetActivity, SetActivityEvent } from "@shared/types/activity";

/** How many lines a surface holds per set.
 *
 * The service keeps a bounded tail of its own and this keeps a bounded
 * window onto it, because an unbounded buffer in a tab left open for a
 * week is a leak with a nice name. It is deliberately larger than a strip
 * shows, so scrolling back reaches something, and deliberately far
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
 * The contract's own ceiling, deliberately. Every limit is safe now that
 * the service hands back the OLDEST slice above the cursor and says when
 * one cut a reading short, so this is about how many polls catching up
 * takes rather than about whether anything is lost. */
const POLL_LIMIT = 200;

/**
 * Folds a new reading into what a surface already holds.
 *
 * The merge is by sequence and it is what makes the cursor safe. A poll
 * that asks for "everything after 412" comes back with a slice, not a
 * tail, so appending blindly would drop the lines above it and replacing
 * blindly would throw away everything before. Deduplicating by sequence
 * also makes a repeated or overlapping response harmless, which matters
 * because a retried request is not a rare event on a NAS.
 */
export function mergeActivity(
  previous: SetActivity | undefined,
  next: SetActivity,
  buffer = PAGE_BUFFER
): SetActivity {
  const bySequence = new Map<number, SetActivityEvent>();
  for (const e of previous?.events ?? []) bySequence.set(e.sequence, e);
  for (const e of next.events) bySequence.set(e.sequence, e);
  const ordered = [...bySequence.values()].sort((a, b) => a.sequence - b.sequence);
  const events = ordered.slice(-buffer);
  return {
    ...next,
    events,
    // A gap is a fact about the buffer, not about the reading that
    // noticed it. Once lines have been lost this window has a hole in it
    // for as long as it holds those lines, and a later reading answering
    // a caught-up cursor cleanly does not fill it in. The surface's own
    // window is the third way to lose one and it counts the same:
    // trimming to `buffer` throws lines away exactly as the service's
    // ring does.
    dropped: (previous?.dropped ?? false) || next.dropped || ordered.length > events.length,
    // The surface's own window can start later than the service's buffer
    // does. Reporting the service's bound while showing fewer lines would
    // claim continuity the surface does not have.
    oldestSequence: Math.max(next.oldestSequence, events.length > 0 ? events[0].sequence : 0)
  };
}

/**
 * Whether this reading came from a different process than the one the
 * surface has been following.
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
 * counter at all: a counter climbs, so a busy new process eventually
 * passes the number the dead one left a cursor at and starts looking
 * live again while its early lines have already been skipped. A name
 * minted per process cannot do that. The sequence comparison beside it
 * is the fallback for a service too old to send one, and within its own
 * window it is sound: the highest sequence only grows inside one
 * process, so a reading below a cursor that service itself handed out is
 * a rewind no live feed can produce.
 */
export function feedRestarted(previous: { epoch: string | null; cursor: number }, next: LiveActivity): boolean {
  if (next.epoch && previous.epoch) return next.epoch !== previous.epoch;
  // A surface holding nothing has no cursor to have been rewound, and a
  // reading with no sets in it is a deployment with no sets rather than a
  // feed that went backwards.
  if (previous.cursor === 0 || next.sets.length === 0) return false;
  return next.sets.reduce((highest, s) => Math.max(highest, s.latestSequence), 0) < previous.cursor;
}

/** The highest sequence anywhere in the held buffer. It is the cursor the
 *  next poll sends, and it is taken across every set because the
 *  service's sequence counter is one counter for the whole process. */
function cursorOf(held: Map<string, SetActivity>): number {
  let highest = 0;
  for (const set of held.values()) {
    for (const e of set.events) if (e.sequence > highest) highest = e.sequence;
  }
  return highest;
}

/** What a surface following the feed gets back. */
export interface ActivityFeed {
  /** Every set in the reading, keyed by id, merged across polls. */
  held: Map<string, SetActivity>;
  /** The interval the SERVICE asked to be polled at, or the slow
   *  fallback before it has answered. */
  pollAfterMs: number;
  /** The last poll's failure, or null. A surface keeps its last good
   *  reading on screen under this and marks it stale; blanking a panel
   *  somebody is reading is a worse answer than an old one that says it
   *  is old. */
  error: ApiError | null;
  /** Ask again now, out of band with the timer.
   *
   * It is what makes an action feel immediate without a second channel:
   * the engine records what it did before it answers the request, so a
   * poll issued the moment that response lands picks up the lines it
   * emitted, and the same lines reach every other window on the next
   * ordinary tick. A client that rendered the response itself would draw
   * those lines twice for the person who pressed the button and once for
   * everybody else.
   */
  refresh: () => void;
}

/**
 * Follows the live activity feed, optionally narrowed to one backup set.
 *
 * `setId` goes onto the request as `backup_set`, which the route already
 * takes and refuses with BACKUP_SET_NOT_FOUND for a set that does not
 * exist, because an empty feed for a set that does not exist reads
 * exactly like a quiet set.
 */
export function useActivityFeed(setId?: string): ActivityFeed {
  const api = useApi();

  // The cursor, the process it belongs to, and the SET it was taken
  // about, on one ref.
  //
  // A ref rather than state for the reason DashboardActivity's own doc
  // gives: it changes on every poll, nothing renders from it, and a fetch
  // that re-created itself each time its cursor moved would restart the
  // poll timer below before it ever elapsed, which fails silently as "the
  // panel stopped refreshing".
  //
  // The `setId` on it is what makes a narrowed feed safe. A narrowed
  // reading is a different question, and the sequence counter is one
  // counter across every bucket in the process, so carrying set A's
  // cursor into a poll about set B would ask for "everything after 412"
  // about a set whose own lines stop at 90 and be told, correctly, that
  // there is nothing. Tagging the cursor with what it was taken about
  // means a change of set simply does not match, so nothing has to be
  // reset from an effect or during a render.
  const feedCursor = useRef<{ setId: string | undefined; since: number; epoch: string | null }>({
    setId,
    since: 0,
    epoch: null
  });

  const [held, setHeld] = useState<{ setId: string | undefined; sets: Map<string, SetActivity> }>({
    setId,
    sets: new Map()
  });

  const live = useAsync<LiveActivity>(() => {
    const cursor = feedCursor.current;
    const since = cursor.setId === setId ? cursor.since : 0;
    return api.getLiveActivity({ since, limit: POLL_LIMIT, ...(setId ? { setId } : {}) });
  }, [api, setId]);

  useEffect(() => {
    const data = live.data;
    if (!data) return;
    const cursor = feedCursor.current;
    const sameQuestion = cursor.setId === setId;
    // Decided out here rather than inside the updater, because it is a
    // decision about two readings rather than about the state, and an
    // updater React is free to run twice must not be where this is
    // settled.
    const restarted = sameQuestion && feedRestarted({ epoch: cursor.epoch, cursor: cursor.since }, data);
    setHeld((current) => {
      // Three ways to start a fresh window and all of them mean the
      // reading cannot be folded into what is held: a different process
      // (its sequence numbers start again at 1 and would land on top of
      // the dead one's), a different set, or nothing held yet. A restart
      // drops everything, because lines describing a cycle in a process
      // that no longer exists under a live panel are the panel saying
      // work is in flight that is not.
      const fresh = restarted || !sameQuestion || current.setId !== setId;
      const merged = fresh ? new Map<string, SetActivity>() : new Map(current.sets);
      for (const set of data.sets) merged.set(set.setId, mergeActivity(merged.get(set.setId), set));
      feedCursor.current = { setId, since: cursorOf(merged), epoch: data.epoch || null };
      return { setId, sets: merged };
    });
  }, [live.data, setId]);

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

  // A reading held about a DIFFERENT set is not this surface's answer,
  // so it is not handed out. Without this the frame between a route
  // change and the first reading for the new set would draw the old
  // set's log under the new set's heading, which is exactly the defect
  // BackupSetDetailPage resets its edit state synchronously to avoid.
  const sets = held.setId === setId ? held.sets : EMPTY_SETS;

  return { held: sets, pollAfterMs, error: live.error, refresh: tick };
}

/** One shared empty map, so the "not this set's answer yet" branch above
 *  hands back a stable reference rather than a new object every render,
 *  which would re-run every useMemo keyed on it. */
const EMPTY_SETS: Map<string, SetActivity> = new Map();
