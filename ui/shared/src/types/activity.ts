/**
 * What a backup set is DOING right now, as opposed to what state it is in
 * (issue #573).
 *
 * `ActivityEvent` in types/operation.ts is the other feed and the two are
 * easy to confuse. That one is the durable lifecycle record: what happened,
 * queryable, from a table, and it has no idea a transfer is at 4 MB/s.
 * This one is a bounded tail the serving process is holding in memory,
 * which knows exactly that and has forgotten last Tuesday. Neither can be
 * built from the other, which is why there are two.
 *
 * The organising rule from types/operation.ts applies here unchanged and
 * is the reason for every `| null` below: nothing describes work that has
 * not been measured. `artifactsTotal` is null until a pass has counted the
 * rows it will walk, and rendering that as zero would draw a bar as a
 * finished cycle. `bytesPerSecond` is null when no copy is in flight, which
 * is a different fact from a measured zero.
 */
import type { LiveTransferStage } from "@shared/types/operation";

/** The severity the engine emitted a line at, carried through unchanged.
 *  It is not a display decision this app made: the emitter chose it when
 *  it decided a line was a warning rather than a note. */
export type ActivityLevel = "debug" | "info" | "warn" | "error";

/**
 * How the operation a line reports WENT, stated by the engine (issue
 * #625).
 *
 * It is not a second spelling of the level beside it. A level is an
 * ORDERING, which is why there is no "success" in it and cannot be: every
 * reader treats a level as a threshold, and success has no place on a line
 * between info and warn. So a completion that went well used to arrive at
 * info, read as a neutral note, and this app reconstructed a tone for it
 * from event names, from which lifecycle state a transition landed in, and
 * from `outcome` fields, across about eight rules all shaped "if the tone
 * is still info, make it ok". Anything the reconstruction had never heard
 * of stayed grey, so a new feature reporting a success read as a shrug and
 * nothing failed when it did.
 *
 * "info" is a completion with no verdict: it ran, it finished, and there
 * is nothing to celebrate or worry about. Absent is different again and
 * means the line states no outcome at all, which is what a start and every
 * ordinary progress note carry.
 */
export type ActivityOutcome = "success" | "warning" | "error" | "info";

/**
 * An action that announced itself and has not said how it went.
 *
 * The one state an operator most needs named, and the one nothing could
 * see: `cycle_start` and `cycle_end` were paired by the habit of spelling
 * them that way, so nothing modelled an operation as something that begins
 * and then concludes and nothing could notice the half that never
 * arrived. The engine pairs them by an action id now and reports what is
 * still open.
 *
 * An action that is legitimately still running appears here too, and that
 * is right rather than a false alarm. The honest sentence is "started four
 * minutes ago and has not reported an outcome"; whether four minutes is
 * long is a judgement the person reading it is far better placed to make.
 */
export interface UnfinishedAction {
  /** The action's stable name, as its start line carried it. */
  action: string;
  actionId: string;
  /** When the start line was emitted, which is the only thing "and it has
   *  been quiet since" can be worked out from. */
  startedAt: string;
  /** The start line's own sequence, so a reader can find the line this is
   *  about in the tail it already holds. */
  sequence: number;
}

/** Whether a line belongs to the backup set carrying it or to the whole
 *  deployment. A cycle starting covers every set, so it belongs to
 *  neither one strip nor all of them: it arrives in the reading's own
 *  `deployment` bucket, which the global terminal reads and a set's strip
 *  does not (issue #593). The sequence numbers are one counter across
 *  both, so a reader holding both can interleave them exactly where they
 *  happened. */
export type ActivityScope = "deployment" | "set";

export interface SetActivityEvent {
  /** Strictly increasing across everything the serving process has
   *  emitted, so a client can order a set's own lines against the
   *  deployment-wide ones and resume from the last one it saw. */
  sequence: number;
  at: string;
  level: ActivityLevel;
  /** The engine's own event name (`discovery`, `lifecycle_transition`,
   *  `transfer_stats`, `cycle_end`, ...). Deliberately not a union: the
   *  catalog grows, and a name this build has no special rendering for
   *  falls back to the message, which is a worse line rather than a
   *  broken one. */
  event: string;
  scope: ActivityScope;
  message: string;
  /** How the operation this line reports went, stated by the engine.
   *  Optional rather than nullable, and the two are not the same claim
   *  here: a line that states no outcome has not measured one badly, it
   *  has reported no operation at all, which is what a start and every
   *  progress note do. See ActivityOutcome for why this is not the level. */
  outcome?: ActivityOutcome;
  /** The action this line opens or closes, and the id its two halves are
   *  paired by. Both absent on every line that is neither half of a pair;
   *  an actionId with no outcome is a start, and one with an outcome
   *  closes it. */
  action?: string;
  actionId?: string;
  /** The event's own structured fields. A record rather than the wire's
   *  array of pairs because every reader here looks fields up by name;
   *  JavaScript preserves insertion order for string keys, so the
   *  fallback renderer still prints them in the order they were logged. */
  fields: Record<string, string>;
}

/** How a set's last pass ENDED, which is a different fact from how many
 *  of its artifacts failed. A pass whose reconcile or discovery failed
 *  never reaches an artifact, so it counts no failures and plans no rows,
 *  and a headline drawn from those two alone paints the earliest failure
 *  there is the way it paints a set with nothing to do. "stopped" is a
 *  pass an operator stopped (an edit hold, a cancellation) and is
 *  deliberately not "failed": spelling an ordinary edit the way an
 *  unreachable source is spelled is a false alarm. */
export type PassOutcome = "ok" | "failed" | "stopped";

export interface SetActivity {
  setId: string;
  /** Whether a cycle is inside this set in the serving process right now.
   *  It is a separate fact from the fraction, and both are needed: a large
   *  artifact holds the numbers still for a long time while everything is
   *  fine, so a fraction alone cannot tell slow from stuck. */
  active: boolean;
  stage: LiveTransferStage | null;
  artifact: string | null;
  artifactsCompleted: number;
  artifactsTotal: number | null;
  /** What the fraction counts, said rather than left to be inferred.
   *  "unknown" is the window before a pass has counted its rows, and it is
   *  why artifactsTotal is null rather than zero. */
  progressBasis: "artifacts" | "unknown";
  bytesTransferred: number | null;
  bytesTotal: number | null;
  bytesPerSecond: number | null;
  /** How many of this set's artifacts ended THIS pass in a terminal
   *  failure. It resets when a new pass begins, because last night's two
   *  failures are not tonight's. */
  failures: number;
  /** How the last pass over this set ended, or null until one has. See
   *  PassOutcome: it is not derivable from `failures`. */
  outcome: PassOutcome | null;
  startedAt: string | null;
  finishedAt: string | null;
  events: SetActivityEvent[];
  /** Every action inside this set that started and has not reported an
   *  outcome, oldest first. Deliberately not filtered by the cursor the
   *  events were: a cursor asks what is new, and an action that has been
   *  quiet for ten minutes is precisely not new. */
  unfinishedActions?: UnfinishedAction[];
  /** Whether the limit cut this reading short and the rest is still held.
   *  `events` is the OLDEST slice above the cursor, never the newest, so
   *  a client that asks again from where this one ends loses nothing on
   *  the way. */
  truncated: boolean;
  /** Whether lines this cursor had not reached fell out of the service's
   *  bounded buffer first. The tail is then NOT continuous with what the
   *  page already holds, and it says so rather than drawing the hole as
   *  an unbroken log. */
  dropped: boolean;
  /** The bounds of what the service still holds for this set, not of
   *  `events`. The buffer is bounded on purpose, so a reader whose oldest
   *  line is above the first sequence knows lines were dropped and can say
   *  so instead of presenting a gap as continuity. */
  oldestSequence: number;
  latestSequence: number;
}

export interface LiveActivity {
  observedAt: string;
  /** An opaque name for the process this reading came from, changing on
   *  every start. The cursor is a sequence number and the sequence
   *  counter is per process, so a page that kept its cursor across a
   *  restart would ask for everything after a number the live process has
   *  not reached, be told there is nothing new, and go on showing a dead
   *  cycle's log. A different epoch is the signal to drop the cursor and
   *  everything held behind it. */
  epoch: string;
  /** How long the service suggests waiting before asking again. The
   *  service decides because it is the one that knows whether anything is
   *  moving: a client picking its own interval would either hammer a NAS
   *  holding a dozen idle sets or watch a transfer at a cadence that makes
   *  the bar jump. */
  pollAfterMs: number;
  sets: SetActivity[];
  /** The log that belongs to no single backup set: a cycle starting, a
   *  capacity check, an action somebody took in the browser. Null when
   *  the reading was narrowed to one set, which is a different fact from
   *  an empty bucket: one says the deployment has been quiet, the other
   *  says nobody asked. */
  deployment: DeploymentActivity | null;
}

/** The deployment-wide tail. It carries the same honesty flags a set's
 *  strip does and none of the progress counters, because there is no such
 *  thing as how far through its own pass a deployment is. */
export interface DeploymentActivity {
  events: SetActivityEvent[];
  /** The deployment-wide actions still owing an outcome. A cycle is the
   *  one this matters most for: it belongs to no single set, so this is
   *  the only bucket a cycle that went quiet can be reported in. */
  unfinishedActions?: UnfinishedAction[];
  truncated: boolean;
  dropped: boolean;
  oldestSequence: number;
  latestSequence: number;
}
