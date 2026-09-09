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
import type { ActivityResult, SetActivity, SetActivityEvent, UnfinishedAction } from "@shared/types/activity";
import { bytes, clock, rate, relativeAge } from "@shared/utilities/format";
import { StatusBadge } from "@shared/components/StatusBadge";
import type { StatusTone } from "@shared/components/StatusBadge";

/** How each line is coloured in the log. It is a display decision made
 *  here on purpose: the service carries the engine's own level, outcome
 *  and event name and composes no sentence, so that a second client is
 *  free to say something else about the same moment. */
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
 * the deed itself has a line of its own further down: remote_delete says
 * "remote source deleted" and states its own success or failure since
 * issue #625. So the deed is where the colour belongs, and colouring the
 * INTENTION as well would put this panel's loudest good news on the step
 * before the one an operator would most want to notice.
 *
 * Nothing here counts towards the progress bar. The numerator beside it is
 * activity.artifactsCompleted, which the engine counts over the rows a
 * pass is actually driving forward; this set only ever picks a colour.
 *
 * This is one of the two derivations left after #625 took the rest of
 * them out, and it survived on the argument it was already making: the
 * wire carries the state an artifact moved into, which is the fact, and
 * WHICH resting states in the FR-10 machine deserve to read as good news
 * is a decision about a screen. internal/obs declines to make it for the
 * same reason, in the note above its event catalog.
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
 * The tone a line takes before any event-specific rendering touches it
 * (issue #625).
 *
 * This is the whole of the fix. The engine states how the operation a line
 * reports WENT, so the tone is read off that, and the level decides only
 * for a line that states no result (a start, or an ordinary progress
 * note) or where it is the louder of the two. What it replaces was a
 * reconstruction: about eight rules
 * keyed on event names, on which lifecycle state a transition landed in
 * and on `outcome` fields, every one of them shaped "if the tone is still
 * info, make it ok". That reads fine for the fifteen event names somebody
 * remembered to write a case for and silently draws everything else grey,
 * so a new feature reporting a success looked exactly like a note about
 * nothing, and no test anywhere failed when it did.
 *
 * The default is now correct rather than merely neutral: an event name
 * this build has never heard of, reporting a success, is green, and one
 * reporting an error is red, with nobody writing a case for it.
 *
 * The result and the level are allowed to differ, and where they do the
 * LOUDER of them wins. Both are the engine's own words, so this is
 * combining two stated facts rather than inventing a third.
 *
 * It matters in both directions. A connection test that correctly reports
 * a host as unreachable is logged as a warning, because the engine
 * reserves its error severity for the manager failing at something rather
 * than for correctly reporting a problem it found; the outcome of that
 * step is a failure and a failure is what an operator has to see, so the
 * result wins. And an emitter that succeeded at what it was asked while
 * logging loudly about it has asked for attention on purpose, the way a
 * retention pass that refused every deletion has: drawing that green
 * because a field says "success" would put this panel's most reassuring
 * colour on the line the emitter went out of its way to raise.
 *
 * Success ranks beside a note rather than above it, so a success stated
 * on an ordinary info line still reads as good news.
 */
const TONE_RANK: Record<LineTone, number> = { ok: 0, info: 0, warn: 1, error: 2 };

function toneOf(e: SetActivityEvent): LineTone {
  const byLevel: LineTone = e.level === "error" ? "error" : e.level === "warn" ? "warn" : "info";
  if (e.result === undefined) return byLevel;
  const stated = statedTone(e.result);
  // A tie goes to the result, which is what lets "success" beat "info".
  return TONE_RANK[byLevel] > TONE_RANK[stated] ? byLevel : stated;
}

/**
 * One stated result as a tone.
 *
 * No `default` clause, and the `never` at the end is why. A default would
 * take a fifth result the engine grows, compile clean, and fall through
 * to grey, which is precisely the failure this whole change exists to
 * remove: a value nobody wrote a case for reading as a note about
 * nothing. This way the compiler names the file and the line instead.
 *
 * The absent case is the caller's, so this is total over the values the
 * type actually has.
 */
function statedTone(result: ActivityResult): LineTone {
  switch (result) {
    case "success":
      return "ok";
    case "warn":
      return "warn";
    case "error":
      return "error";
    case "info":
      return "info";
  }
  const unhandled: never = result;
  throw new Error("unhandled activity result: " + String(unhandled));
}

/**
 * One event as an operator reads it.
 *
 * The service deliberately sends the engine's own event name, level,
 * outcome and fields and composes nothing, so the composing happens here.
 * An event name this build has no rendering for falls back to its message
 * plus its fields, which is a worse line rather than a broken one, and is
 * what keeps a new event in the engine from needing a UI release to be
 * visible. Since #625 it is a worse line with the RIGHT COLOUR, which is
 * what makes that fallback worth relying on rather than merely surviving.
 *
 * The cases below are down to wording and two derivations. Everything
 * that used to reach in here to fix up a tone is gone, because the engine
 * says it: a cycle end, a validation, a commit and a connection-test step
 * all state their own outcome now. What is left is `lifecycle_transition`,
 * where the fact on the wire is a state name and which resting states
 * deserve to read as good news is a decision about a screen (see
 * SETTLED_STATES, and internal/obs's own note on why it declines to make
 * it), and `browser_notice`, which this browser composed and the engine
 * never saw.
 */
export function activityLine(e: SetActivityEvent): ActivityLine {
  const f = e.fields;
  const name = artifactName(f.artifact);
  let text: string;
  let tone: LineTone = toneOf(e);

  switch (e.event) {
    case "cycle_start":
      text = "cycle started";
      break;
    case "cycle_end":
      text = f.error ? "cycle finished with an error: " + f.error : "cycle finished";
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
      // Both of these are floors rather than the primary rule, and the
      // first one is easy to read as dead code and delete.
      //
      // The engine states an error result on a transition into a failure
      // state since #625, so against the CURRENT engine the first line
      // changes nothing. It is here for an engine OLDER than that change:
      // this app and the engine are two containers and can be two
      // versions, and a strip that stopped painting a FAILED artifact red
      // when talking to last month's build would be the exact defect this
      // issue is about, arriving silently.
      //
      // The second is not a floor at all and never becomes one. Which
      // resting states read as GOOD news is a decision about a screen
      // (see SETTLED_STATES), the engine deliberately declines to make
      // it, and this is where it is made.
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
      text =
        f.passed === "false"
          ? "validation failed: " + name + (f.detail ? " (" + f.detail + ")" : "")
          : "verified " + name;
      break;
    case "commit":
      text = "committed " + name;
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
    case "connection_test": {
      // One step of a `Test Connection` (issue #596). The engine emits
      // six of these, one per step, and the whole point of the feature is
      // that they are six lines rather than one verdict: `host_key
      // failed` and `authenticate skipped` send an operator to two
      // different places, and 0.3.2 spelled both of them "could not
      // connect and list the remote path".
      //
      // The step name is padded so the outcomes line up in a column, the
      // way `medium preflight` already prints a Report. The detail is the
      // engine's own sentence, never a transport error's text, and it may
      // carry its own indented lines (the two fingerprints in a host key
      // mismatch), so it is joined below rather than flattened.
      // The two lines that BRACKET the test carry no step, because they
      // are about the whole of it: the test starting, and the test
      // finishing with a verdict (issue #625). They are the same event
      // name on purpose, since a start and a completion are told apart
      // by whether they state an outcome rather than by being spelled
      // differently, so this reads the message rather than padding an
      // absent step into a column of question marks.
      if (!f.step) {
        text = e.message || e.event;
        break;
      }
      const step = f.step.padEnd(13);
      // The step's own finer word (passed, skipped, failed), under the
      // name this event has carried since #596. The COLOUR comes from the
      // line's stated result like every other line's does, and the two
      // are the same fact at two grains rather than two answers: the
      // engine's four-value vocabulary goes under `result`, so this field
      // never had to give up its name.
      text = step + (f.outcome ?? "?") + (f.detail ? "  " + f.detail : "");
      break;
    }
    case "browser_notice": {
      // A line this BROWSER wrote, not one the engine sent (issue #596,
      // over G1.4's state/browserNotices seam). A refusal produced in
      // front of the engine (a CSRF failure, a dead connection) never
      // reaches the engine's ring at all, so nothing on the server could
      // have logged it, and a panel that showed only what the engine said
      // would show nothing at all for exactly the presses that need
      // explaining.
      //
      // Marked as coming from the browser, always. "The engine said this"
      // and "your browser could not reach the engine to ask" are
      // different facts, and only one of them is evidence about the NAS.
      const parts = ["[browser] " + (e.message || "a request from this browser did not go through")];
      if (f.remediation) parts.push("  " + f.remediation);
      if (f.correlation_id) parts.push("  correlation id " + f.correlation_id);
      // With the prompt, the same way an api_action's is drawn a few
      // lines down. The wire carries a command bare and the "$ " belongs
      // to the terminal, and until the dock read this seam the two never
      // met on one screen: a notice only ever appeared in the per-set
      // panel. They meet now, and one log printing `$ rbm run` for a
      // request the engine served and `rbm run` for one it refused is two
      // renderings of the same thing three lines apart. RunControlNotice
      // draws the prompt too, so this is the third surface agreeing
      // rather than a new convention.
      if (f.command) parts.push("$ " + f.command);
      text = parts.join("\n");
      break;
    }
    case "api_action": {
      // Something somebody did through the API rather than something the
      // cycle did (issue #599). The engine composes the summary, the
      // refusal's own words and the `rbm` command line; this
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

/**
 * The line above the log for an action that announced itself and has not
 * said how it went (issue #625).
 *
 * It is drawn where somebody is already looking rather than on a page
 * they would have to go and find, because the whole failure it addresses
 * is an operator who pressed something and cannot tell whether anything
 * is happening. That is also why it says how long it has been quiet
 * rather than deciding for them: an action four seconds old is a running
 * one and an action four hours old is a stuck one, both render the same
 * way here, and the difference is in a number the reader is far better
 * placed to judge than this panel is.
 *
 * A warning tone rather than an error one, on purpose. Nothing has gone
 * wrong yet; something has failed to say that it has not.
 */
export function UnfinishedActionsNotice({ actions }: { actions: UnfinishedAction[] }) {
  if (actions.length === 0) return null;
  return (
    <div style={{ padding: "0 var(--space-5) var(--space-3)", color: "var(--warn)", fontSize: "var(--text-xs)" }}>
      {actions.map((a) => (
        <div key={a.actionId}>
          {a.action} started {relativeAge(a.startedAt)} and has not reported an outcome.
        </div>
      ))}
    </div>
  );
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
  stale = false,
  heading = true
}: {
  set: BackupSet;
  activity: SetActivity | null;
  stale?: boolean;
  /**
   * Whether to draw the set's name, its remote and its state pill above
   * the bar.
   *
   * True on the dashboard, where a column of these is the only thing
   * saying which strip is which. False on the set's OWN page (issue
   * #596), where the page header two lines up already carries the name,
   * the host, the remote folder, the read-only flag and the health
   * badge, and repeating all five inside the panel is five lines of
   * chrome between an operator and the log they opened the page for.
   *
   * The aria-label is unchanged either way, so the panel is still found
   * and announced as "Activity for <set>" with the heading off.
   */
  heading?: boolean;
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
      {heading ? (
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
      ) : null}

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

      <UnfinishedActionsNotice actions={activity.unfinishedActions ?? []} />

      <ActivityToolbar events={activity.events} open={open} onToggle={() => setOpen((v) => !v)} setId={activity.setId} />
      {open ? <ActivityLogView events={activity.events} /> : null}
    </section>
  );
}
