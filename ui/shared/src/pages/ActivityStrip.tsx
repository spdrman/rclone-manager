/**
 * The strip pinned at the foot of the dashboard, one per backup set
 * (issue #573).
 *
 * The dashboard could already say what a set IS and never what it is
 * doing. Between HEALTHY and FAILING sits a cycle that discovers,
 * transfers, verifies and commits, and none of it reached the screen, so a
 * set mid-transfer looked exactly like a set sitting idle and the only way
 * to tell was to read the container's log.
 *
 * # Two signals, not one
 *
 * The bar carries a fraction and a moving gradient, and they say different
 * things on purpose. The fraction says how far through the set's own pass
 * this is; the gradient says the process is alive. A percentage on its own
 * cannot tell "slow" from "stuck", because a large artifact legitimately
 * holds the number still for a long time, and that distinction is the
 * whole reason an operator is looking at this panel.
 *
 * Which is exactly why the gradient stops when the reading does. "The
 * process is alive" is a claim about now, and while the poll behind a
 * reading is failing that is the one thing nobody knows. The `stale` prop
 * is how this strip is told, and it takes down the gradient, the pulse,
 * the spinner, the byte rate and the time remaining with it. The fraction
 * stays: it was measured, and it has not stopped being what was measured.
 *
 * # It says what it counts
 *
 * "26 of 41 artifacts" rather than "63%". The service reports two counts
 * and names their basis; the percentage is derived here for the bar's own
 * width and the words beside it say what it is a percentage OF, so nobody
 * has to infer it. No denominator means no reading at all: the bar reports
 * no value rather than drawing zero, which would read as a finished cycle.
 *
 * # It is always here
 *
 * A panel that appears only during activity teaches an operator to hunt
 * for it, and its absence then means either nothing is running or nothing
 * is reporting, with no way to tell which. So the strip is rendered for
 * every configured set, and an idle one says when its last cycle finished.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import type { CSSProperties } from "react";
import type { BackupSet } from "@shared/types/backup";
import type { SetActivity, SetActivityEvent } from "@shared/types/activity";
import { bytes, clock, rate, relativeAge } from "@shared/utilities/format";
import { StatusBadge } from "@shared/components/StatusBadge";
import type { StatusTone } from "@shared/components/StatusBadge";

/** How each line is coloured in the log. It is a display decision made
 *  here on purpose: the service carries the engine's own level and event
 *  name and composes no sentence, so that a second client is free to say
 *  something else about the same moment. */
export type LineTone = "info" | "ok" | "warn" | "error";

export interface ActivityLine {
  time: string;
  text: string;
  tone: LineTone;
}

/** The names an artifact ends a pass in and does not come back from on its
 *  own. They are spelled here rather than imported because they are the
 *  engine's lifecycle vocabulary arriving as event field values, not a
 *  type this app declares. */
const TERMINAL_FAILURES = new Set(["FAILED", "QUARANTINED", "QUARANTINED_LOST"]);

/**
 * The transitions this log paints as good news: an artifact arriving
 * somewhere good AND settled.
 *
 * It is deliberately not internal/retention's managed-complete set, it is
 * registered as not being it (notTheManagedCompleteSet, in that package's
 * managedcompleteprose_test.go), and it differs at both ends because the
 * two answer different questions. That one asks whether there is a durable
 * local backup here that FR-18 retention may consider. This one asks
 * whether a line in a scrolling log deserves to be green.
 *
 * VERIFIED is in this set and not in that one. Retention is right to
 * exclude it: an artifact that has been verified is still in flight and is
 * not yet a completed backup. But verification passing is the strongest
 * piece of good news the pipeline produces, it is the moment the content
 * was confirmed against its source, and a log that would not colour it is
 * a log that has nothing to say when things go right.
 *
 * REMOTE_DELETE_PENDING is in that set and not in this one, and that is the
 * half worth reading twice. Retention is right to include it: the local
 * commit already succeeded, so the backup exists whatever happens to the
 * remote next. It is left out here because it is the one state in the list
 * that is not a resting place. It is the inside of FR-15's delete window,
 * the moment before an irreversible act on somebody else's machine, and
 * the deed itself already gets its own uncoloured line further down
 * (remote_delete renders "remote source deleted" and takes its tone from
 * the event's level alone). Painting the intention green while the deed
 * stays grey would have this panel most enthusiastic about the step an
 * operator would most want to notice.
 *
 * Nothing here counts towards the progress bar. The numerator beside it is
 * activity.artifactsCompleted, which the engine counts over the rows a
 * pass is actually driving forward; this set only ever picks a colour.
 */
const SETTLED_STATES = new Set(["VERIFIED", "COMMITTED", "COMPLETE", "REMOTE_RETAINED"]);

/** The last segment of an artifact id, which is "source/set/name". The
 *  strip is already inside one set, so repeating the first two segments on
 *  every line would be twelve wasted characters on a panel that is 132
 *  pixels tall. */
function artifactName(id: string | undefined): string {
  if (!id) return "";
  const parts = id.split("/");
  return parts[parts.length - 1] || id;
}

function pairs(fields: Record<string, string>): string {
  return Object.entries(fields)
    .map(([k, v]) => k + "=" + v)
    .join(" ");
}

/**
 * One event as an operator reads it.
 *
 * The service deliberately sends the engine's own event name, level and
 * fields and composes nothing, so the composing happens here. An event
 * name this build has no rendering for falls back to its message plus its
 * fields, which is a worse line rather than a broken one, and is what
 * keeps a new event in the engine from needing a UI release to be visible.
 */
export function activityLine(e: SetActivityEvent): ActivityLine {
  const f = e.fields;
  const name = artifactName(f.artifact);
  let text: string;
  let tone: LineTone = e.level === "error" ? "error" : e.level === "warn" ? "warn" : "info";

  switch (e.event) {
    case "cycle_start":
      text = "cycle started";
      break;
    case "cycle_end":
      if (f.error) {
        text = "cycle finished with an error: " + f.error;
        tone = tone === "info" ? "error" : tone;
      } else {
        text = "cycle finished";
        tone = tone === "info" ? "ok" : tone;
      }
      break;
    case "discovery":
      text = "discovery complete: " + (f.discovered ?? "?") + " artifacts, " + (f.pending ?? "?") + " pending";
      break;
    case "lifecycle_transition":
      if (f.to === "TRANSFERRING") text = "transfer started: " + name;
      else if (f.to === "VERIFIED") text = "verified " + name;
      else if (f.to === "COMMITTED") text = "committed " + name;
      else if (TERMINAL_FAILURES.has(f.to ?? "")) text = f.to!.toLowerCase().replace(/_/g, " ") + ": " + name;
      else text = name + ": " + (f.from ?? "?") + " to " + (f.to ?? "?");
      if (f.detail) text += "\n  " + f.detail;
      if (TERMINAL_FAILURES.has(f.to ?? "")) tone = "error";
      else if (SETTLED_STATES.has(f.to ?? "") && tone === "info") tone = "ok";
      break;
    case "transfer_stats":
      text = "transferred " + name + (f.bytes_transferred ? " (" + bytes(Number(f.bytes_transferred)) + ")" : "");
      break;
    case "hash":
      text = (f.alg ?? "hash") + " " + (f.hash ?? "") + " for " + name;
      break;
    case "validation":
      if (f.passed === "false") {
        text = "validation failed: " + name + (f.detail ? " (" + f.detail + ")" : "");
        tone = tone === "info" ? "warn" : tone;
      } else {
        text = "verified " + name;
        if (tone === "info") tone = "ok";
      }
      break;
    case "commit":
      text = "committed " + name;
      if (tone === "info") tone = "ok";
      break;
    case "remote_delete":
      text = f.error ? "remote delete failed: " + name + " (" + f.error + ")" : "remote source deleted: " + name;
      break;
    case "retention":
      text = "retention " + (f.decision ?? "?") + ": " + name + (f.tier ? " (" + f.tier + ")" : "");
      break;
    case "retry":
      text = "retrying " + (f.op ?? "?") + " (attempt " + (f.attempt ?? "?") + ", " + (f.category ?? "?") + ")" + (f.error ? ": " + f.error : "");
      break;
    case "error":
      text = (f.op ? f.op + ": " : "") + (f.error ?? e.message);
      break;
    case "api_action": {
      // Something somebody did through the API rather than something the
      // cycle did (issue #599). The engine composes the summary, the
      // refusal's own words and the `backup-manager` command line; this
      // lays them out. Rendering it here rather than only in the dock is
      // what gives a set's own strip, and every export, the same lines:
      // logText is built on this function.
      const parts = [e.message || e.event];
      if (f.detail) parts.push("  " + f.detail);
      // The wire carries the command bare and the gap as its wording plus
      // its detail, and this draws the "$ " and the "# ". The prompt is
      // the terminal's in the same way the timestamp is: a script reading
      // the journal wants a command it can hand to a shell, and a
      // `command` field that carried a prompt would be a screen it had to
      // parse. Every client draws the same prefixes from the same fields,
      // so this and `activity --follow` agree without sharing code.
      if (f.command) {
        parts.push("$ " + f.command);
        if (f.command_runnable === "false") parts.push("#   not runnable as printed: fill in the value in angle brackets");
      } else if (f.command_gap) {
        parts.push("# " + f.command_gap + (f.route ? " · " + f.route : ""));
        if (f.command_gap_detail) parts.push("#   " + f.command_gap_detail);
      }
      text = parts.join("\n");
      break;
    }
    default: {
      // An event name this build has no rendering for. Its own message
      // plus its own fields is a worse line, not a broken one, and it is
      // what keeps a new event in the engine visible without a UI release.
      const rest = pairs(f);
      text = (e.message || e.event) + (rest ? " " + rest : "");
    }
  }
  return { time: clock(e.at), text, tone };
}

/** The log exactly as it reads on screen, for the clipboard and for the
 *  saved file. One builder for both, so what an operator pastes into an
 *  issue is what they were looking at. */
export function logText(events: SetActivityEvent[]): string {
  return events
    .map((e) => {
      const line = activityLine(e);
      return line.time + " " + line.text;
    })
    .join("\n");
}

/**
 * The fraction as a whole percentage, or null when there is no honest
 * denominator.
 *
 * Null rather than zero, always. A bar drawn at zero for a set that has
 * not finished discovering reads as a stalled cycle, and a bar drawn at
 * zero of zero reads as a finished one. Both are claims this app cannot
 * make.
 */
export function artifactFraction(activity: SetActivity): number | null {
  if (activity.progressBasis !== "artifacts") return null;
  const total = activity.artifactsTotal;
  if (total === null || total <= 0) return null;
  return Math.round((activity.artifactsCompleted / total) * 100);
}

/** How long the artifact in flight has left, from the numbers the service
 *  actually reports. It is computed here rather than served because the
 *  service will not put a figure it cannot check on a wire that other
 *  clients then have to carry, and it is deliberately vague ("about"),
 *  because a rate sampled over seconds does not support more. */
function remaining(activity: SetActivity): string | null {
  const { bytesTransferred: done, bytesTotal: total, bytesPerSecond: perSecond } = activity;
  if (done === null || total === null || perSecond === null || perSecond <= 0 || total <= done) return null;
  const seconds = Math.round((total - done) / perSecond);
  if (seconds < 60) return "about " + seconds + "s left";
  if (seconds < 3600) return "about " + Math.round(seconds / 60) + "m left";
  return "about " + Math.round(seconds / 3600) + "h left";
}

/** "2 artifacts failed", in one place, because it is said in three. */
function failureWords(failures: number): string {
  return failures + (failures === 1 ? " artifact failed" : " artifacts failed");
}

function fractionWords(activity: SetActivity): string | null {
  if (activity.progressBasis !== "artifacts" || activity.artifactsTotal === null) return null;
  return activity.artifactsCompleted + " of " + activity.artifactsTotal + " artifacts";
}

/**
 * The pill. While a pass is running it names the step, because that is the
 * most specific true thing; when nothing is running it says how the pass
 * ended, and only then defers to the set's own health verdict, which is
 * the fact the rest of the dashboard already states.
 *
 * Two of those clauses were missing and both of them said the opposite of
 * the truth. A stale reading kept pulsing the step, which is a claim the
 * process is alive and is exactly what nobody knows while the poll is
 * failing. And a pass that ended badly with nothing to count (a reconcile
 * or a discovery that failed never reaches an artifact) fell through to
 * the set's stored health, which is still whatever last night's cycle
 * left it as.
 */
function pill(set: BackupSet, activity: SetActivity, stale: boolean): { tone: StatusTone; glyph: string; label: string; pulse: boolean } {
  if (activity.active) {
    // "Not reporting", in the panel's own words. The distinction between
    // that and "nothing is running" is the reason this panel exists, and
    // a failing poll is precisely the moment it has to be drawn.
    if (stale) return { tone: "warn", glyph: "▲", label: "NOT REPORTING", pulse: false };
    // A running pass with failures behind it still names its step, and
    // still says something is wrong. Waiting until the cycle ends means a
    // whole cycle in which the headline gives an operator no reason to
    // look closer.
    const tone: StatusTone = activity.failures > 0 ? "danger" : "accent";
    const glyph = activity.failures > 0 ? "✕" : "●";
    return { tone, glyph, label: (activity.stage ?? "working").toUpperCase(), pulse: true };
  }
  if (activity.failures > 0 || activity.outcome === "failed") {
    return { tone: "danger", glyph: "✕", label: "NEEDS ATTENTION", pulse: false };
  }
  if (activity.outcome === "stopped") {
    return { tone: "warn", glyph: "▲", label: "STOPPED", pulse: false };
  }
  if (set.state === "healthy") return { tone: "ok", glyph: "●", label: "HEALTHY", pulse: false };
  if (set.state === "failing") return { tone: "danger", glyph: "✕", label: "FAILING", pulse: false };
  return { tone: "warn", glyph: "▲", label: set.state.toUpperCase(), pulse: false };
}

/**
 * What the bar is coloured by, which is what the pass actually did rather
 * than what its failure count happens to be.
 *
 * Four answers, and three of them used to collapse into one. A pass that
 * failed at reconcile counts no failures, so it drew ok-toned and green;
 * a pass somebody stopped mid-walk drew the same, with a bar sitting at
 * whatever fraction it reached; and a reading that stopped refreshing
 * kept sweeping, which is the animation whose whole job is to say the
 * process is alive.
 *
 * Stopped is set inline rather than by class because the design system
 * has no warn-toned fill and adding one for a single caller is a rule
 * that exists for nobody else.
 */
function barFill(activity: SetActivity, live: boolean, stale: boolean): { className: string; style: CSSProperties } {
  if (live) return { className: " activity-bar__fill--busy", style: {} };
  // A pass that was in flight when the readings stopped is not a
  // finished one. It keeps the accent a moving bar wears and loses only
  // the sweep, which was the part claiming it is still moving.
  if (activity.active && stale) return { className: "", style: {} };
  if (activity.failures > 0 || activity.outcome === "failed") {
    return { className: " activity-bar__fill--danger", style: {} };
  }
  if (activity.outcome === "stopped") return { className: "", style: { background: "var(--warn)" } };
  return { className: " activity-bar__fill--ok", style: {} };
}

/**
 * The sentence under the bar: what is happening, named concretely.
 *
 * Never a spinner over the word "Working". An operator who can read
 * "Transferring dpkg.status.2.gz, 26 of 41 artifacts, 4 MB/s, about 40s
 * left" knows whether to wait or intervene, and that is the only thing
 * this line is for.
 */
function stepSentence(activity: SetActivity, stale: boolean): { lead: string; subject: string | null; trail: string } {
  const fraction = fractionWords(activity);
  const stage = activity.stage ?? "working";
  const stageWords = stage.replace(/-/g, " ");
  if (activity.active) {
    // A reading nobody can refresh keeps its fraction and loses every
    // number that is a claim about now. A rate is measured over the last
    // few seconds and a time remaining is computed from it, so both of
    // them assert that the process was alive a moment ago, which is the
    // one fact a failing poll has taken away.
    if (stale) {
      const parts: string[] = [];
      if (fraction) parts.push(fraction);
      if (activity.failures > 0) parts.push(failureWords(activity.failures));
      parts.push("this is the last reading, and it is not refreshing");
      const lead = "Last seen " + stageWords;
      return { lead: activity.artifact ? lead : lead + "…", subject: activity.artifact, trail: parts.join(" · ") };
    }
    const parts: string[] = [];
    if (fraction) parts.push(fraction);
    if (activity.bytesPerSecond !== null) parts.push(rate(activity.bytesPerSecond));
    const left = remaining(activity);
    if (left) parts.push(left);
    if (activity.failures > 0) parts.push(failureWords(activity.failures) + " so far");
    parts.push("in progress");
    const lead = stage.charAt(0).toUpperCase() + stage.slice(1).replace(/-/g, " ");
    return { lead: activity.artifact ? lead : lead + "…", subject: activity.artifact, trail: parts.join(" · ") };
  }
  if (activity.failures > 0) {
    const how = fraction ? "Stopped after " + fraction : "Stopped";
    return {
      lead: how,
      subject: null,
      trail: failureWords(activity.failures) + " · nothing will retry them on its own"
    };
  }
  // The two passes that end badly with nothing to count. Neither reaches
  // the clause above, because neither leaves a failed artifact behind: a
  // reconcile or discovery that failed never got to one, and a pass
  // somebody stopped left its artifact pre-durable rather than failed.
  if (activity.outcome === "failed") {
    return {
      lead: fraction ? "Stopped after " + fraction : "Stopped",
      subject: null,
      trail: "this pass did not finish · the log below says where it stopped"
    };
  }
  if (activity.outcome === "stopped") {
    return {
      lead: fraction ? "Stopped after " + fraction : "Stopped",
      subject: null,
      trail: "this pass was stopped before it finished · the rest was not attempted"
    };
  }
  if (activity.finishedAt) {
    return {
      lead: "Idle",
      subject: null,
      trail: "last cycle finished " + relativeAge(activity.finishedAt) + (fraction ? " · " + fraction : "")
    };
  }
  return { lead: "Idle", subject: null, trail: "no cycle has run since this service started" };
}

function ActivityLogView({ events }: { events: SetActivityEvent[] }) {
  const scroller = useRef<HTMLDivElement | null>(null);
  const latest = events.length > 0 ? events[events.length - 1].sequence : 0;

  // Follow the tail. A log that has to be dragged to the bottom on every
  // poll is a log nobody watches, and the panel is short enough that
  // whatever an operator scrolled back to is one drag away again.
  useEffect(() => {
    const node = scroller.current;
    if (node) node.scrollTop = node.scrollHeight;
  }, [latest]);

  return (
    <div className="activity-log" role="log" aria-live="polite" ref={scroller}>
      {events.map((e) => {
        const line = activityLine(e);
        return (
          <div key={e.sequence}>
            <span className="activity-log__time">{line.time}</span>{" "}
            <span className={"activity-log__line--" + line.tone} style={{ whiteSpace: "pre-wrap" }}>
              {line.text}
            </span>
          </div>
        );
      })}
    </div>
  );
}

/**
 * The toolbar under the log: collapse, a line count, and the two ways to
 * take the text somewhere else.
 *
 * Copy and Save exist because the next thing an operator does with a
 * failure is paste it into an issue or a message, and re-typing an md5
 * pair off a screen is how a report ends up wrong.
 */
function ActivityToolbar({
  events,
  open,
  onToggle,
  setId
}: {
  events: SetActivityEvent[];
  open: boolean;
  onToggle(): void;
  setId: string;
}) {
  const [copied, setCopied] = useState(false);

  const copy = useCallback(() => {
    const text = logText(events);
    // Optional chaining rather than a guard: a browser without the async
    // clipboard (an http:// origin, most notably) should leave the button
    // inert rather than throw into a panel an operator is reading.
    void navigator.clipboard?.writeText(text);
    setCopied(true);
  }, [events]);

  useEffect(() => {
    if (!copied) return;
    const id = window.setTimeout(() => setCopied(false), 2000);
    return () => window.clearTimeout(id);
  }, [copied]);

  const save = useCallback(() => {
    const blob = new Blob([logText(events)], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = setId.replace(/\//g, "-") + "-activity.txt";
    anchor.click();
    URL.revokeObjectURL(url);
  }, [events, setId]);

  return (
    <div className="activity-toolbar">
      <button type="button" className="activity-toolbar__toggle" onClick={onToggle} aria-expanded={open}>
        {open ? "Hide log" : "Show log"}
      </button>
      <span style={{ fontFamily: "var(--font-mono)", color: "var(--text-3)" }}>
        {events.length} {events.length === 1 ? "line" : "lines"}
      </span>
      <span className="activity-toolbar__spacer" />
      {copied ? <span style={{ color: "var(--ok)", fontWeight: 600 }}>copied</span> : null}
      <button type="button" className="activity-toolbar__button" onClick={copy}>
        Copy
      </button>
      <button type="button" className="activity-toolbar__button" onClick={save}>
        Save .txt
      </button>
    </div>
  );
}

/**
 * One backup set's strip.
 *
 * `activity` is null while nothing has loaded, and that renders as
 * "checking", never as idle: "nothing is running" is a claim, and a fetch
 * that has not resolved does not support it.
 *
 * `stale` is the same discipline one step along: the poll behind this
 * reading is failing, so the reading is the last good one rather than a
 * current one. Keeping it on screen is right (blanking a panel somebody
 * is reading is a worse answer than an old one), but every signal that
 * says "alive right now" has to stop: the pulse, the sweep, the spinner,
 * the byte rate and the time remaining. The fraction stays, because it is
 * still the last thing that was actually measured.
 */
export function ActivityStrip({
  set,
  activity,
  stale = false
}: {
  set: BackupSet;
  activity: SetActivity | null;
  stale?: boolean;
}) {
  const [open, setOpen] = useState(true);
  const label = "Activity for " + set.name;

  if (activity === null) {
    return (
      <section aria-label={label} style={{ padding: "var(--space-4) var(--space-5)" }}>
        <div style={{ fontWeight: 600, fontSize: "var(--text-md)" }}>{set.name}</div>
        <div style={{ color: "var(--text-2)", fontSize: "var(--text-sm)", marginTop: "var(--space-2)" }}>
          Checking what this set is doing{"…"}
        </div>
      </section>
    );
  }

  const fraction = artifactFraction(activity);
  const words = fractionWords(activity);
  const live = activity.active && !stale;
  const step = stepSentence(activity, stale);
  const badge = pill(set, activity, stale);
  // Three ways to have lost lines and all of them mean the same thing to
  // a reader: the service's own ring threw something away above this
  // cursor, the page trimmed its window, or both. mergeActivity folds all
  // three into this one flag.
  //
  // It used to read `activity.oldestSequence > 1` as well, as a proxy for
  // the page's window starting after the feed's. That proxy stopped being
  // one when a set's feed became the set's own ring (issue #593): the
  // sequence counter is shared across every bucket, so a set whose first
  // line happens to be the ninth event this process emitted starts at 9
  // with nothing missing at all, and every strip on a healthy deployment
  // would have claimed lines were dropped.
  const dropped = activity.dropped;
  const fill = barFill(activity, live, stale);
  const barLabel =
    fraction === null
      ? set.name + ": nothing discovered yet, so progress is not measurable"
      : set.name + ": " + words;

  return (
    <section aria-label={label}>
      <div
        style={{
          display: "flex", alignItems: "center", justifyContent: "space-between",
          gap: "var(--space-3)", padding: "var(--space-3) var(--space-5)",
          borderBottom: "1px solid var(--border)"
        }}
      >
        <div style={{ minWidth: 0 }}>
          <div style={{ fontWeight: 600, fontSize: "var(--text-md)" }}>{set.name}</div>
          <div style={{ color: "var(--text-3)", fontSize: "var(--text-xs)", fontFamily: "var(--font-mono)" }}>
            {set.remoteFolder}
            {set.readOnly ? " · read-only" : ""}
          </div>
        </div>
        <StatusBadge tone={badge.tone} glyph={badge.glyph}>
          {badge.label}
        </StatusBadge>
      </div>

      <div style={{ display: "flex", alignItems: "center", gap: "var(--space-3)", padding: "var(--space-3) var(--space-5) 0" }}>
        <div
          className="activity-bar"
          style={{ flex: 1 }}
          role="progressbar"
          aria-valuemin={0}
          aria-valuemax={100}
          {...(fraction === null ? {} : { "aria-valuenow": fraction })}
          aria-label={barLabel}
        >
          <div
            className={"activity-bar__fill" + fill.className}
            style={{ width: (fraction ?? 0) + "%", ...fill.style }}
          />
        </div>
        <span
          style={{
            fontFamily: "var(--font-mono)", fontSize: "var(--text-sm)", color: "var(--text-2)",
            minWidth: 42, textAlign: "right", fontVariantNumeric: "tabular-nums"
          }}
        >
          {fraction === null ? "—" : fraction + "%"}
        </span>
      </div>

      <div
        style={{
          padding: "var(--space-2) var(--space-5) var(--space-3)",
          color: "var(--text-2)", fontSize: "var(--text-sm)",
          display: "flex", alignItems: "center", gap: "var(--space-2)", flexWrap: "wrap"
        }}
      >
        {live ? <span className="activity-spinner" aria-hidden="true" /> : null}
        <span>{step.lead}</span>
        {step.subject ? <strong style={{ color: "var(--text)" }}>{step.subject}</strong> : null}
        <span>
          {step.trail ? "· " : ""}
          {step.trail}
        </span>
      </div>

      {dropped ? (
        <div
          style={{
            padding: "0 var(--space-5) var(--space-3)",
            color: "var(--text-3)", fontSize: "var(--text-xs)"
          }}
        >
          Earlier lines are not held here any more. The full record is on the Activity page.
        </div>
      ) : null}

      <ActivityToolbar events={activity.events} open={open} onToggle={() => setOpen((v) => !v)} setId={activity.setId} />
      {open ? <ActivityLogView events={activity.events} /> : null}
    </section>
  );
}
