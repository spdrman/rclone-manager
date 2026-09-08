/**
 * The live-activity polling loop, once, for every surface that follows
 * the feed (issues #596 and #599).
 *
 * It was DashboardActivity's own until #596, and that file's doc gave the
 * right reason for it being there: "a fetch belongs on the graph when a
 * SECOND surface reads the answer, and nothing else reads this one". The
 * backup set detail page was that second surface. The loop came out here
 * as a hook rather than going onto the graph, because the surfaces ask
 * DIFFERENT questions of it: the dashboard asks about every set at once
 * and a set's own page asks about one, carrying a `backup_set`. A shared
 * graph node holds one answer, and these are two.
 *
 * The global terminal (ActivityDock, #599) was the third surface and it
 * copied the loop instead of taking it, because it holds ONE window over
 * every bucket rather than one per set, and because it keeps a restart's
 * lines rather than dropping them. Those are two differences, and the
 * forty lines around them were the same forty lines: the cursor, the
 * epoch, the timer, the out-of-band refresh. They did not stay the same.
 * The copy rebuilt its cursor from the events that happened to arrive, so
 * every idle poll reset it to zero and asked for the service's whole held
 * tail again, and nothing on screen showed it because the merge
 * deduplicates by sequence. So the two differences are options now
 * (useActivityWindow's `fold` and `onRestart`) and the loop is one loop.
 *
 * Everything the loop is careful about is careful for the reasons
 * DashboardActivity's own doc gave: the service picks the interval because
 * it is the one that knows whether anything is moving; there is no
 * held-open connection because this runs on a NAS behind whatever reverse
 * proxy the operator already had; and the cursor is what keeps polling
 * cheap.
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

/** How many events one poll asks for per bucket.
 *
 * The contract's own ceiling, deliberately. Every limit is safe now that
 * the service hands back the OLDEST slice above the cursor, says when one
 * cut a reading short, and the cursor below holds back at whichever
 * bucket said so, so this is about how many polls catching up takes
 * rather than about whether anything is lost. */
const POLL_LIMIT = 200;

/**
 * Folds a new reading into what a surface already holds.
 *
 * The merge is by sequence and it is what makes the cursor safe. A poll
 * that asks for "everything after 412" comes back with a slice, not a
 * tail, so appending blindly would drop the lines above it and replacing
 * blindly would throw away everything before. Deduplicating by sequence
 * also makes a repeated or overlapping response harmless, which matters
 * because a retried request is not a rare event on a NAS, and because
 * asking again from where a truncated bucket stopped re-sends whatever a
 * quieter bucket had already handed over.
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
  // The surface's own window is the third way to lose a line and it
  // counts the same: trimming to `buffer` throws lines away exactly as
  // the service's ring does.
  const trimmed = ordered.length > events.length;
  // Whether this window now reaches back at least as far as the service's
  // own buffer does. When it does, every line anybody still holds is on
  // screen, so there is nothing between what the page shows and what the
  // service has.
  const reachesTheServicesOldest = events.length > 0 && next.oldestSequence > 0 && events[0].sequence <= next.oldestSequence;
  return {
    ...next,
    events,
    // A gap is a fact about the buffer, not about the reading that
    // noticed it, so it is carried forward: a later reading answering a
    // caught-up cursor cleanly does not fill in a hole this window
    // already has. What it is NOT is permanent. Left unconditionally
    // sticky, one dropped=true put "earlier lines are not held here any
    // more" on the panel for the life of the tab, including on a page
    // that had since refilled from the service's whole buffer, and a gap
    // warning that fires when there is no gap teaches an operator to
    // ignore the real one.
    dropped: next.dropped || trimmed || (!reachesTheServicesOldest && (previous?.dropped ?? false)),
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
  // reading with no buckets at all is a deployment with nothing
  // configured rather than a feed that went backwards. The deployment's
  // own bucket counts as a bucket: on a fresh install it is the only one
  // there is, and a fallback that ignored it would call every reading a
  // restart.
  if (previous.cursor === 0 || (next.sets.length === 0 && !next.deployment)) return false;
  const highest = next.sets.reduce((high, s) => Math.max(high, s.latestSequence), next.deployment?.latestSequence ?? 0);
  return highest < previous.cursor;
}

/**
 * The cursor the next poll sends, from a reading and the cursor that
 * fetched it.
 *
 * Two rules, and both of them were a defect in a surface that reimplemented
 * this.
 *
 * It comes from what the service says it HOLDS, never from what happened
 * to arrive. Those differ on every idle poll, which is most polls: a
 * cursor rebuilt from the events in hand falls to zero the moment a
 * reading is empty, and zero means "send me whatever you still hold", so
 * every quiet tick asks a twenty-set deployment for its entire held tail
 * again, per tab. Nothing looks wrong while it happens, because the merge
 * deduplicates by sequence. It only shows up as the window overflowing
 * and the panel claiming, permanently, that earlier lines are gone.
 *
 * And a bucket that was cut short holds it back. The service answers a
 * cursor with the OLDEST slice above it and sets `truncated` when the
 * limit stopped it there, which is an invitation to ask again from where
 * it stopped. A cursor that jumped to the highest sequence anywhere in
 * the reading steps clean over the rest of that bucket instead: those
 * lines are still held, still inside the service's buffer, and are never
 * requested again by anybody, with nothing in any later response saying
 * they were skipped. One busy bucket beside one quiet one is all it
 * takes.
 */
export function nextCursor(previous: number, reading: LiveActivity): number {
  let highest = previous;
  // The lowest page boundary among the buckets that said they have more
  // to hand over, or 0 when none did.
  let page = 0;
  const consider = (events: SetActivityEvent[], latestSequence: number, truncated: boolean) => {
    if (latestSequence > highest) highest = latestSequence;
    for (const e of events) if (e.sequence > highest) highest = e.sequence;
    // A bucket that says it was cut short and hands over nothing names no
    // page boundary, so there is nothing to hold back to. The service
    // cannot produce that reading (it only truncates a slice it has
    // filled), and treating it as a boundary would stall the cursor on
    // the same window for ever.
    if (!truncated || events.length === 0) return;
    // Oldest first on the wire, so the last one handed over is the page
    // boundary.
    const boundary = events[events.length - 1].sequence;
    if (page === 0 || boundary < page) page = boundary;
  };
  for (const s of reading.sets) consider(s.events, s.latestSequence, s.truncated);
  // The deployment's own bucket counts for the same reason it is
  // rendered: on a deployment with nothing configured it is the only
  // bucket there is, and a cursor built from `sets` alone would sit at
  // zero for ever while every poll refetched the same window (#593).
  if (reading.deployment) {
    consider(reading.deployment.events, reading.deployment.latestSequence, reading.deployment.truncated);
  }
  // Never backwards, whatever a reading says: a cursor that rewinds
  // re-requests everything above the point it rewound to on every tick.
  if (page > 0 && page < highest) return Math.max(previous, page);
  return highest;
}

/** What a surface holding its own kind of window gets back. */
export interface ActivityWindow<T> {
  /** What `fold` has built out of every reading so far, or undefined
   *  before the first one has landed. */
  held: T | undefined;
  /** The last reading itself, for the facts that are about the reading
   *  rather than about the window: which sets the deployment has right
   *  now, what the service said it was doing. Null before the first one. */
  reading: LiveActivity | null;
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

/** What a surface following the per-set feed gets back. */
export interface ActivityFeed extends Omit<ActivityWindow<never>, "held"> {
  /** Every set in the reading, keyed by id, merged across polls. */
  held: Map<string, SetActivity>;
}

export interface ActivityWindowOptions<T> {
  /** The set to narrow the reading to, or undefined for the whole
   *  deployment. It goes onto the request as `backup_set`, which the
   *  route already refuses with BACKUP_SET_NOT_FOUND for a set that does
   *  not exist, because an empty feed for a set that does not exist reads
   *  exactly like a quiet set. */
  setId?: string;
  /** Folds one reading into whatever this surface holds. Called with
   *  undefined whenever the window has to start again: nothing held yet,
   *  a different set, or a restart. */
  fold: (previous: T | undefined, reading: LiveActivity) => T;
  /** Called just before a restart starts a fresh window, with the reading
   *  that revealed it and the window about to be dropped.
   *
   *  It exists because the two surfaces do deliberately opposite things
   *  here. A strip drops what it holds, because a progress bar and a step
   *  sentence describing a process that no longer exists are active lies
   *  about work in flight. A terminal is a log, and the lines leading up
   *  to a crash are the only evidence of why it crashed, so it freezes
   *  them and draws a rule. What neither may do is merge across the
   *  boundary: the new process's counter starts again at 1 and would land
   *  on top of the dead one's lines. */
  onRestart?: (reading: LiveActivity, held: T | undefined) => void;
}

/**
 * Follows the live activity feed, folding each reading into a window of
 * the caller's own shape.
 *
 * Everything that is the same for every surface is here: the cursor, the
 * epoch, the restart check, the timer at the service's own cadence and
 * the out-of-band refresh. What is not the same is the two options.
 */
export function useActivityWindow<T>(options: ActivityWindowOptions<T>): ActivityWindow<T> {
  const { setId } = options;
  const api = useApi();

  // The fold and the restart policy ride on refs so a caller may pass a
  // fresh closure on every render (they usually will) without this
  // effect, and the poll timer under it, being torn down and rebuilt
  // before it ever fires. That failure is silent and reads as "the panel
  // stopped refreshing".
  const fold = useRef(options.fold);
  const onRestart = useRef(options.onRestart);
  useEffect(() => {
    fold.current = options.fold;
    onRestart.current = options.onRestart;
  });

  // The cursor, the process it belongs to, and the SET it was taken
  // about, on one ref.
  //
  // A ref rather than state for the reason DashboardActivity's own doc
  // gives: it changes on every poll, nothing renders from it, and a fetch
  // that re-created itself each time its cursor moved would restart the
  // poll timer below before it ever elapsed.
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

  // The window itself, on a ref beside its state, because the fold is
  // computed out here rather than inside the updater (see below) and
  // needs the value it is folding into.
  const heldWindow = useRef<{ setId: string | undefined; value: T | undefined }>({ setId, value: undefined });
  const [held, setHeld] = useState<{ setId: string | undefined; value: T | undefined }>({ setId, value: undefined });

  // The last reading this loop has already folded.
  //
  // A reading is answered once, under the question it was asked. Without
  // this, changing `setId` re-runs the effect below with the PREVIOUS
  // set's reading still in hand (a fetch in flight deliberately does not
  // blank what is on screen), and that reading would move the cursor for
  // the new set: the counter is one counter for the whole process, so a
  // set that has been running all night hands the cursor 412 and the
  // quiet set it moves to is then asked for everything after 412, about
  // lines that stop at 5. It is also what makes re-entry harmless in
  // general, which a cursor moved from inside a poll had better be.
  const folded = useRef<LiveActivity | null>(null);

  const live = useAsync<LiveActivity>(() => {
    const cursor = feedCursor.current;
    const since = cursor.setId === setId ? cursor.since : 0;
    return api.getLiveActivity({ since, limit: POLL_LIMIT, ...(setId ? { setId } : {}) });
  }, [api, setId]);

  useEffect(() => {
    const data = live.data;
    if (!data || folded.current === data) return;
    folded.current = data;
    const cursor = feedCursor.current;
    const sameQuestion = cursor.setId === setId;
    // Settled out here rather than inside a state updater, because it is
    // a decision about two readings rather than about the state, and an
    // updater React is free to run twice must not be where a cursor is
    // moved. The guard above is the other half of that: one reading moves
    // the cursor once.
    const restarted = sameQuestion && feedRestarted({ epoch: cursor.epoch, cursor: cursor.since }, data);
    // Two ways to start a fresh window and both mean the reading cannot be
    // folded into what is held: a different process (its sequence numbers
    // start again at 1 and would land on top of the dead one's) or a
    // different set.
    const fresh = restarted || !sameQuestion || heldWindow.current.setId !== setId;
    if (restarted) onRestart.current?.(data, heldWindow.current.value);
    // A restart reading is the one reading whose bounds may not be
    // believed. It was fetched with the DEAD process's cursor, which the
    // live one cannot honour, so it answered "nothing above 412" while
    // holding seven lines of its own below it: taking its latestSequence
    // would skip every one of them, permanently, and the panel would look
    // caught up. The cursor goes back to the beginning instead and the
    // next poll refills from whatever the live process holds. A set
    // change is not the same case: that request already went out with no
    // cursor, so its answer is a first reading and may be believed.
    feedCursor.current = {
      setId,
      since: restarted ? 0 : nextCursor(sameQuestion ? cursor.since : 0, data),
      epoch: data.epoch || null
    };
    heldWindow.current = { setId, value: fold.current(fresh ? undefined : heldWindow.current.value, data) };
    setHeld(heldWindow.current);
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

  // A window held about a DIFFERENT set is not this surface's answer, so
  // it is not handed out. Without this the frame between a route change
  // and the first reading for the new set would draw the old set's log
  // under the new set's heading, which is exactly the defect
  // BackupSetDetailPage resets its edit state synchronously to avoid.
  return {
    held: held.setId === setId ? held.value : undefined,
    reading: live.data ?? null,
    pollAfterMs,
    error: live.error,
    refresh: tick
  };
}

/**
 * Follows the live activity feed as one merged window per backup set,
 * optionally narrowed to one of them.
 *
 * The shape the strips want: they draw one panel per set and look their
 * set up by id.
 */
export function useActivityFeed(setId?: string): ActivityFeed {
  const feed = useActivityWindow<Map<string, SetActivity>>({ setId, fold: foldSets });
  return {
    held: feed.held ?? EMPTY_SETS,
    reading: feed.reading,
    pollAfterMs: feed.pollAfterMs,
    error: feed.error,
    refresh: feed.refresh
  };
}

/** Every set in the reading, merged into what is already held for it.
 *
 * The deployment's own bucket is deliberately not folded in here: these
 * are per-set strips, and the lines that belong to no set are the global
 * terminal's (#593). It still moves the cursor, because the sequence
 * counter is one counter for the whole process. */
function foldSets(previous: Map<string, SetActivity> | undefined, reading: LiveActivity): Map<string, SetActivity> {
  const merged = new Map(previous ?? []);
  for (const set of reading.sets) merged.set(set.setId, mergeActivity(merged.get(set.setId), set));
  return merged;
}

/** One shared empty map, so the "not this set's answer yet" branch above
 *  hands back a stable reference rather than a new object every render,
 *  which would re-run every useMemo keyed on it. */
const EMPTY_SETS: Map<string, SetActivity> = new Map();
