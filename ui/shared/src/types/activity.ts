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

/** Whether a line belongs to the backup set carrying it or to the whole
 *  deployment. A cycle starting covers every set, so it reaches every
 *  strip marked "deployment" rather than being dropped (which would hide
 *  it) or pinned to one set (which would put it on the wrong screen). */
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
  /** The event's own structured fields. A record rather than the wire's
   *  array of pairs because every reader here looks fields up by name;
   *  JavaScript preserves insertion order for string keys, so the
   *  fallback renderer still prints them in the order they were logged. */
  fields: Record<string, string>;
}

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
  startedAt: string | null;
  finishedAt: string | null;
  events: SetActivityEvent[];
  /** The bounds of what the service still holds for this set, not of
   *  `events`. The buffer is bounded on purpose, so a reader whose oldest
   *  line is above the first sequence knows lines were dropped and can say
   *  so instead of presenting a gap as continuity. */
  oldestSequence: number;
  latestSequence: number;
}

export interface LiveActivity {
  observedAt: string;
  /** How long the service suggests waiting before asking again. The
   *  service decides because it is the one that knows whether anything is
   *  moving: a client picking its own interval would either hammer a NAS
   *  holding a dozen idle sets or watch a transfer at a cadence that makes
   *  the bar jump. */
  pollAfterMs: number;
  sets: SetActivity[];
}
