/**
 * The terminal docked to the bottom of every page (issue #599).
 *
 * The dashboard's strips (#573) answer "what is THIS set doing", with a
 * bar and a step sentence read at a glance while looking at that set's
 * row. This answers a different question: "what has this deployment done,
 * most recently, including the thing I just clicked". Merging the two
 * would mean either losing the per-set bar or drawing one bar for a
 * machine running several sets, and neither is a reading, so the strips
 * stay exactly as they are and this reads the same endpoint.
 *
 * # Why it lives in the shell
 *
 * It mounts in AppShell, as a sibling of the nav/main row rather than a
 * child of <main>. React Router swaps what is inside <main>, so a panel
 * in there unmounts on every navigation and starts again from cursor 0.
 * A sibling never unmounts, so the buffer, the cursor, the epoch, the
 * scroll position and the collapsed state all survive every navigation in
 * the session.
 *
 * It is fixed to the browser window rather than sitting in flow (#617).
 * In flow it rode the end of the document, so on any page longer than a
 * screen the always-on panel scrolled out of view and was only there once
 * you had already reached the bottom, which is the opposite of what it is
 * for. Fixed positioning costs the property that arrangement had for
 * free: nothing below shrinks any more, so the shell reserves
 * dockReservedHeight() under the content column and the last row of a
 * long table can still be scrolled clear of the terminal.
 *
 * A full browser reload is allowed to lose the buffer. The engine holds
 * the authoritative tail and this refills from it on mount with since=0,
 * so a reload costs one poll rather than the history.
 *
 * # What it does with a restart, and why it differs from the strips
 *
 * The polling loop under this panel is the strips' own (useActivityWindow,
 * #596), which is where the cursor, the epoch check, the timer and the
 * out-of-band refresh live. This panel is two options on it: `fold`, so
 * every bucket in a reading lands in ONE window rather than one per set,
 * and `onRestart`, because what the two surfaces do about a restart is
 * deliberately opposite. A strip drops everything it holds, because a
 * progress bar and a step sentence describing a process that no longer
 * exists are active lies about work in flight. A terminal is a log, and
 * throwing away the lines leading up to a crash throws away the only
 * evidence of why it crashed. So this keeps them, draws a rule, and
 * starts a fresh buffer under it.
 *
 * It used to be a copy of that loop rather than a caller of it, and the
 * copy is where its cursor bug lived: rebuilt from the events that
 * happened to arrive, it fell to zero on every idle poll and asked the
 * service for its whole held tail again, invisibly, because the merge
 * deduplicates by sequence.
 *
 * The fresh buffer is structural, not cosmetic: the sequence counter is
 * per process and starts again at 1, so the new process's line 1 would
 * otherwise overwrite the dead process's line 1 in a buffer keyed by
 * sequence.
 *
 * # Shared log, private chrome
 *
 * The lines come from the engine's own buffers, so two people watching
 * one NAS see the same lines in the same order with the same sequence
 * numbers, and can quote a timestamp at each other. What is per viewer is
 * only what the viewer did to the panel: collapsed or expanded, the
 * filter, and how tall they dragged it. That is in localStorage and
 * nowhere else.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { usePlatform } from "@shared/platform/PlatformContext";
import { mergeActivity, useActivityWindow } from "@shared/pages/useActivityFeed";
import { activityLine, logText } from "@shared/pages/ActivityStrip";
import type { LineTone } from "@shared/pages/ActivityStrip";
import type { LiveActivity, SetActivity, SetActivityEvent } from "@shared/types/activity";
import { clock } from "@shared/utilities/format";

/** How many lines the dock holds.
 *
 * More than a strip's 300, because this aggregates every bucket rather
 * than windowing one, and because it is the surface an operator scrolls
 * back through after something went wrong. Still bounded, for the reason
 * that constant already gives: an unbounded buffer in a tab left open for
 * a week is a leak with a nice name. What is gone is said in the
 * scrollback, and the durable record is the Activity page. */
const DOCK_BUFFER = 1000;

const STORAGE = {
  open: "backup-manager.dock.open",
  height: "backup-manager.dock.height",
  filter: "backup-manager.dock.filter"
} as const;

const MIN_HEIGHT = 120;
const MAX_HEIGHT = 720;
const DEFAULT_HEIGHT = 240;

/** The collapsed bar: one line, and the height the panel reserves when it
 *  is not open. Exported because the shell reserves room for it and a
 *  second copy of the number would be a second thing to keep in step. */
export const DOCK_BAR_HEIGHT = 32;

/** The room the panel needs under it. Fixed positioning takes the dock out
 *  of flow, so nothing below it shrinks on its own any more and the shell
 *  has to leave this much or the last row of a long table sits behind the
 *  terminal with no way to scroll it clear. The 8 is the resize handle and
 *  the border. */
export function dockReservedHeight(open: boolean, height: number): number {
  return open ? height + DOCK_BAR_HEIGHT + 8 : DOCK_BAR_HEIGHT;
}

/** One thing the dock draws: a line, or the rule marking where a process
 *  ended. The rule is an entry rather than a flag on the next line
 *  because it has a time of its own and belongs between two lines. */
export type DockEntry =
  | { kind: "event"; event: SetActivityEvent }
  | { kind: "restart"; at: string };

/** What is written in front of a line, and the whole reason `scope` was
 *  plumbed through in #573 and read by nothing until now. */
export interface DockPrefix {
  label: string;
  /** The set this line belongs to, or "" for a deployment-wide one. The
   *  filter reads this; the label does not always equal it, because a
   *  request line is labelled by who made it. */
  setId: string;
  /** Whether this line is an action somebody took rather than work the
   *  engine did. */
  action: boolean;
}

/**
 * Who or what a line is about.
 *
 * Three kinds, and the third is the one that needed a deliberate
 * addition. `[engine]` and `[source/set]` come straight from the event's
 * own scope. A line for an API action names the actor the session
 * identified, and renders as `[you]` when it is this session and as the
 * account name when it is not. That distinction is the point: two people
 * administering one NAS should each be able to see that the OTHER just
 * restarted a cycle, and a panel that labelled every request `[you]`
 * would tell them the opposite.
 */
export function dockPrefix(e: SetActivityEvent, viewer: string | null): DockPrefix {
  const setId = e.scope === "set" ? e.fields.backup_set ?? "" : "";
  if (e.event === "api_action") {
    const actor = e.fields.actor ?? "";
    const label = actor === "" ? "request" : viewer !== null && actor === viewer ? "you" : actor;
    return { label, setId, action: true };
  }
  if (e.scope === "deployment") return { label: "engine", setId: "", action: false };
  return { label: setId || "set", setId, action: false };
}

/** Every line the dock is holding, oldest first, bounded. */
export function dockEntries(history: DockEntry[], current: SetActivity | undefined): DockEntry[] {
  const live: DockEntry[] = (current?.events ?? []).map((event) => ({ kind: "event", event }));
  return [...history, ...live].slice(-DOCK_BUFFER);
}

/**
 * Folds one reading into the single window the dock holds.
 *
 * Every bucket in the reading is flattened into one synthetic set, and
 * mergeActivity does the rest: dedup by sequence, order by sequence, trim
 * to the window, and carry `dropped` forward once lines have been lost.
 * That is the same function the strips use and it is used for the same
 * three properties, at the dock's own buffer size.
 *
 * Deduplicating by sequence is correct and cheap because the engine's
 * counter is one counter for the whole process, which is the property
 * the cursor already relies on. What this does NOT do is decide that
 * cursor: it is a window, and a window is what happened to arrive.
 * useActivityWindow takes the cursor from what the service says it holds
 * (see nextCursor), which is a different number on every idle poll and is
 * the whole of the defect this panel shipped with. The set a line belongs to rides along as
 * its own `backup_set` field, filled in from the bucket it arrived in for
 * the events that do not already carry one: an event the service put in
 * a set's bucket IS about that set, so writing it down is recording what
 * the service already decided rather than guessing.
 */
export function foldReading(previous: SetActivity | undefined, next: LiveActivity): SetActivity {
  const events: SetActivityEvent[] = [];
  for (const set of next.sets) {
    for (const e of set.events) {
      events.push(e.fields.backup_set ? e : { ...e, fields: { ...e.fields, backup_set: set.setId } });
    }
  }
  for (const e of next.deployment?.events ?? []) events.push(e);
  events.sort((a, b) => a.sequence - b.sequence);

  const dropped = next.sets.some((s) => s.dropped) || (next.deployment?.dropped ?? false);
  // The earliest sequence any bucket is still holding. Zero when nothing
  // is held anywhere, which is the honest answer for a process that has
  // said nothing yet: a bound taken over an empty set of buckets would
  // otherwise come out as infinity and read as a window starting after
  // everything that has ever happened.
  const bounds = [...next.sets.map((s) => s.oldestSequence), next.deployment?.oldestSequence ?? 0].filter((n) => n > 0);
  if (events.length > 0) bounds.push(events[0].sequence);
  const oldest = bounds.length > 0 ? Math.min(...bounds) : 0;

  const reading: SetActivity = {
    setId: "",
    active: next.sets.some((s) => s.active),
    stage: null,
    artifact: null,
    artifactsCompleted: 0,
    artifactsTotal: null,
    progressBasis: "unknown",
    bytesTransferred: null,
    bytesTotal: null,
    bytesPerSecond: null,
    failures: 0,
    outcome: null,
    startedAt: null,
    finishedAt: null,
    events,
    truncated: next.sets.some((s) => s.truncated) || (next.deployment?.truncated ?? false),
    dropped,
    oldestSequence: oldest,
    // The newest sequence any bucket says it HOLDS, which is not the
    // newest that arrived: an idle reading hands over nothing while the
    // service is still holding four thousand lines. Reporting the arrival
    // here would be a window claiming to be a bound.
    latestSequence: next.sets.reduce(
      (high, s) => Math.max(high, s.latestSequence),
      Math.max(next.deployment?.latestSequence ?? 0, events.length > 0 ? events[events.length - 1].sequence : 0)
    )
  };
  return mergeActivity(previous, reading, DOCK_BUFFER);
}

/** The dock's own filters. Never drops a line from the buffer, only from
 *  what is drawn, so clearing one brings everything back. */
export type DockFilter = "all" | "engine" | "mine" | "commands" | string;

function passesFilter(entry: DockEntry, filter: DockFilter, viewer: string | null): boolean {
  if (entry.kind === "restart") return true;
  const prefix = dockPrefix(entry.event, viewer);
  switch (filter) {
    case "all":
      return true;
    case "engine":
      return prefix.setId === "";
    case "mine":
      return prefix.action && prefix.label === "you";
    case "commands":
      return entry.event.event === "api_action";
    default:
      return prefix.setId === filter;
  }
}

/** The rule a restart draws, in the one wording both the screen and the
 *  export use. */
export function restartRule(at: string): string {
  return "──── engine restarted at " + clock(at) + " · everything above belongs to a process that has gone ────";
}

/** One word as a shell would need it typed: bare when it is made only of
 *  characters a shell leaves alone, single-quoted otherwise. Mirrors
 *  core/cliecho's shellQuote for the two values this file quotes. */
function shellWord(s: string): string {
  if (s === "") return "''";
  if (/^[A-Za-z0-9_@%+=:,./-]+$/.test(s)) return s;
  return "'" + s.replace(/'/g, "'\\''") + "'";
}

/**
 * What every command below needs beyond its own argv, said once at the
 * top of the panel rather than repeated on every line.
 *
 * It is composed here and not in the engine, because the engine does not
 * have the information. It knows what address it is listening on, and
 * behind a reverse proxy or inside a Synology package that is not the
 * address the reader would type at their own shell. The browser does
 * know it: it is the origin this page was loaded from. And it knows the
 * session's own name, which is the account the commands would run as.
 *
 * The password is a placeholder for the reason usage() already gives for
 * these being environment variables at all: a password on a command line
 * is in every process listing on the host. This panel is exportable and
 * copy-to-clipboard by design, which is the same argument one step
 * further along, so the line never carries one and never could.
 */
export function environmentPreamble(origin: string, viewer: string | null): string {
  const url = shellWord(origin === "" ? "http://127.0.0.1:8080" : origin);
  const user = viewer === null || viewer === "" ? "<the administrator you sign in as>" : shellWord(viewer);
  return "export BACKUP_MANAGER_API_URL=" + url + " BACKUP_MANAGER_API_USERNAME=" + user + " BACKUP_MANAGER_API_PASSWORD=<your password>";
}

/** The dock's text, for the clipboard and for the saved file, honouring
 *  whatever filter is on.
 *
 * logText is the strips' own builder and it is reused unchanged for the
 * argument its own doc already makes: one builder, so what an operator
 * pastes into an issue is what they were looking at. The prefixes and the
 * restart rules are laid in around it here, because they are this panel's
 * and no strip has them. The preamble, when given, is the first line for
 * the same reason it is the first line on screen: the commands under it
 * are only runnable with it said once above them. */
export function dockText(entries: DockEntry[], viewer: string | null, preamble: string | null = null): string {
  const lines = entries.map((entry) => {
    if (entry.kind === "restart") return restartRule(entry.at);
    const prefix = dockPrefix(entry.event, viewer);
    const [first, ...rest] = logText([entry.event]).split("\n");
    const head = first.replace(/^(\S+)\s/, "$1 [" + prefix.label + "] ");
    return [head, ...rest].join("\n");
  });
  return (preamble === null ? lines : [preamble, ...lines]).join("\n");
}

function readStored(key: string): string | null {
  try {
    return window.localStorage.getItem(key);
  } catch {
    // A browser with site data blocked is a browser where the panel opens
    // at its default, not one where the panel throws into a page an
    // operator is reading.
    return null;
  }
}

function writeStored(key: string, value: string): void {
  try {
    window.localStorage.setItem(key, value);
  } catch {
    /* see readStored */
  }
}

export function ActivityDock() {
  const { auth } = usePlatform();
  const viewer = auth?.username ?? null;

  const [history, setHistory] = useState<DockEntry[]>([]);

  const [open, setOpen] = useState(() => readStored(STORAGE.open) !== "0");
  const [height, setHeight] = useState(() => Number(readStored(STORAGE.height)) || DEFAULT_HEIGHT);
  const [filter, setFilter] = useState<DockFilter>(() => readStored(STORAGE.filter) ?? "all");
  const [copied, setCopied] = useState(false);

  // Freeze what the dead process left, with a rule under it, and let the
  // loop start a fresh window. Both halves matter: the lines leading up
  // to a crash are the evidence, and the new process's sequence numbers
  // start again at 1 and would otherwise land on top of them.
  const onRestart = useCallback(
    (reading: LiveActivity, held: SetActivity | undefined) =>
      setHistory((current) => [...dockEntries(current, held), { kind: "restart" as const, at: reading.observedAt }].slice(-DOCK_BUFFER)),
    []
  );

  // The cursor, the epoch, the timer and the out-of-band refresh are the
  // strips' own loop, unchanged (#596). What is this panel's is the two
  // options: one window over every bucket rather than one per set, and a
  // restart that keeps its lines instead of dropping them.
  const feed = useActivityWindow<SetActivity>({ fold: foldReading, onRestart });
  const held = feed.held;

  useEffect(() => writeStored(STORAGE.open, open ? "1" : "0"), [open]);
  useEffect(() => writeStored(STORAGE.height, String(height)), [height]);
  useEffect(() => writeStored(STORAGE.filter, filter), [filter]);

  // Hand the reserved height to the layout. A custom property rather than
  // a prop or a context because the only reader is a padding rule one
  // level up, and the panel is resizable and collapsible: whatever
  // reserves the room has to move when either changes, and the number is
  // already here.
  useEffect(() => {
    const root = document.documentElement;
    root.style.setProperty("--dock-height", dockReservedHeight(open, height) + "px");
    return () => {
      root.style.removeProperty("--dock-height");
    };
  }, [open, height]);
  useEffect(() => {
    if (!copied) return;
    const id = window.setTimeout(() => setCopied(false), 2000);
    return () => window.clearTimeout(id);
  }, [copied]);

  // The chips are a function of the latest reading rather than state of
  // their own: a set is configured or it is not, and the reading says so
  // on every poll.
  const sets = useMemo(() => (feed.reading?.sets ?? []).map((s) => s.setId), [feed.reading]);
  const all = useMemo(() => dockEntries(history, held), [history, held]);
  const shown = useMemo(() => all.filter((e) => passesFilter(e, filter, viewer)), [all, filter, viewer]);
  // The origin this page was loaded from is the address the reader would
  // type, which is the one fact the engine cannot know (see
  // environmentPreamble). Read once per viewer rather than per render.
  const preamble = useMemo(() => environmentPreamble(window.location.origin, viewer), [viewer]);

  const problems = useMemo(() => {
    let errors = 0;
    let warnings = 0;
    for (const entry of all) {
      if (entry.kind !== "event") continue;
      if (entry.event.level === "error") errors++;
      else if (entry.event.level === "warn") warnings++;
    }
    return { errors, warnings };
  }, [all]);

  const copy = useCallback(() => {
    void navigator.clipboard?.writeText(dockText(shown, viewer, preamble));
    setCopied(true);
  }, [shown, viewer, preamble]);

  const save = useCallback(() => {
    const blob = new Blob([dockText(shown, viewer, preamble)], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    // The deployment and the time, not a set id: this file is every set.
    anchor.download = "backup-manager-terminal-" + new Date().toISOString().replace(/[:.]/g, "-") + ".txt";
    anchor.click();
    URL.revokeObjectURL(url);
  }, [shown, viewer, preamble]);

  const dropped = held?.dropped ?? false;

  return (
    <section
      aria-label="Terminal"
      style={{
        position: "fixed",
        left: 0,
        right: 0,
        bottom: 0,
        zIndex: 30,
        display: "flex",
        flexDirection: "column",
        borderTop: "1px solid var(--border)",
        background: "var(--surface)",
        maxHeight: open ? dockReservedHeight(true, height) : DOCK_BAR_HEIGHT
      }}
    >
      <div
        style={{
          flex: "none",
          height: 32,
          display: "flex",
          alignItems: "center",
          gap: "var(--space-3)",
          padding: "0 var(--space-4)",
          fontSize: "var(--text-xs)"
        }}
      >
        <button
          type="button"
          className="activity-toolbar__toggle"
          onClick={() => setOpen((v) => !v)}
          aria-expanded={open}
        >
          {open ? "Hide terminal" : "Show terminal"}
        </button>

        {/* Collapsed it keeps the newest line and a count of what went
            wrong, because a terminal that collapses to nothing teaches an
            operator to stop opening it. */}
        {!open ? (
          <span className="mono" style={{ color: "var(--text-3)", flex: 1, minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
            {newestLine(all, viewer)}
          </span>
        ) : (
          <span style={{ flex: 1, display: "flex", gap: 6, flexWrap: "wrap" }}>
            {chips(sets).map((chip) => (
              <button
                key={chip.value}
                type="button"
                className="activity-toolbar__button"
                aria-pressed={filter === chip.value}
                onClick={() => setFilter(chip.value)}
                style={filter === chip.value ? { background: "var(--accent-quiet)", color: "var(--text)" } : undefined}
              >
                {chip.label}
              </button>
            ))}
          </span>
        )}

        {problems.errors > 0 || problems.warnings > 0 ? (
          <span className="mono" style={{ color: problems.errors > 0 ? "var(--danger)" : "var(--warn)" }}>
            {problems.errors > 0 ? problems.errors + " error" + (problems.errors === 1 ? "" : "s") : ""}
            {problems.errors > 0 && problems.warnings > 0 ? " · " : ""}
            {problems.warnings > 0 ? problems.warnings + " warning" + (problems.warnings === 1 ? "" : "s") : ""}
          </span>
        ) : null}

        {/* Never a spinner over a word: while the poll is failing the dock
            says the reading is not refreshing rather than drawing
            something that claims the process is alive. */}
        <span className="mono" style={{ color: feed.error ? "var(--warn)" : "var(--text-3)" }}>
          {feed.error ? "not refreshing" : shown.length + (shown.length === 1 ? " line" : " lines")}
        </span>

        {open ? (
          <>
            {copied ? <span style={{ color: "var(--ok)", fontWeight: 600 }}>copied</span> : null}
            <button type="button" className="activity-toolbar__button" onClick={copy}>
              Copy
            </button>
            <button type="button" className="activity-toolbar__button" onClick={save}>
              Save .txt
            </button>
          </>
        ) : null}
      </div>

      {open ? (
        <DockLog
          entries={shown}
          viewer={viewer}
          height={height}
          onResize={setHeight}
          dropped={dropped}
          preamble={preamble}
        />
      ) : null}
    </section>
  );
}

/** The newest line, for the collapsed bar. */
function newestLine(entries: DockEntry[], viewer: string | null): string {
  for (let i = entries.length - 1; i >= 0; i--) {
    const entry = entries[i];
    if (entry.kind !== "event") continue;
    const prefix = dockPrefix(entry.event, viewer);
    const line = activityLine(entry.event);
    return line.time + " [" + prefix.label + "] " + line.text.split("\n")[0];
  }
  return "nothing has happened since this page was opened";
}

function chips(sets: string[]): { value: DockFilter; label: string }[] {
  return [
    { value: "all", label: "Everything" },
    { value: "engine", label: "Engine" },
    ...sets.map((id) => ({ value: id as DockFilter, label: id })),
    { value: "mine", label: "This browser" },
    { value: "commands", label: "Commands only" }
  ];
}

/**
 * The scrollback.
 *
 * It follows the tail and STOPS following the moment somebody scrolls up,
 * which is the fix for what ActivityLogView does today: pinning scrollTop
 * on every new sequence fights an operator reading back through a
 * failure. Following resumes when they reach the bottom again, so nobody
 * has to find a control for it.
 */
function DockLog({
  entries,
  viewer,
  height,
  onResize,
  dropped,
  preamble
}: {
  entries: DockEntry[];
  viewer: string | null;
  height: number;
  onResize(next: number): void;
  dropped: boolean;
  preamble: string;
}) {
  const scroller = useRef<HTMLDivElement | null>(null);
  const following = useRef(true);
  const last = entries.length > 0 ? entries.length : 0;

  useEffect(() => {
    const node = scroller.current;
    if (node && following.current) node.scrollTop = node.scrollHeight;
  }, [last]);

  const onScroll = useCallback(() => {
    const node = scroller.current;
    if (!node) return;
    following.current = node.scrollHeight - node.scrollTop - node.clientHeight < 24;
  }, []);

  const startResize = useCallback(
    (event: React.PointerEvent) => {
      const startY = event.clientY;
      const startHeight = height;
      const move = (e: PointerEvent) => {
        const next = Math.min(MAX_HEIGHT, Math.max(MIN_HEIGHT, startHeight + (startY - e.clientY)));
        onResize(next);
      };
      const stop = () => {
        window.removeEventListener("pointermove", move);
        window.removeEventListener("pointerup", stop);
      };
      window.addEventListener("pointermove", move);
      window.addEventListener("pointerup", stop);
    },
    [height, onResize]
  );

  return (
    <>
      <div
        onPointerDown={startResize}
        role="separator"
        aria-label="Resize terminal"
        aria-orientation="horizontal"
        style={{ height: 4, cursor: "row-resize", background: "var(--border)", flex: "none" }}
      />
      <div
        className="activity-log"
        role="log"
        aria-live="polite"
        ref={scroller}
        onScroll={onScroll}
        style={{ height, maxHeight: height, overflow: "auto" }}
      >
        {/* The environment every command below needs, said once here
            rather than on every line, so each command stays clean and
            runnable under it. Muted, because it is a header and not an
            event: nothing happened at it. */}
        <div className="activity-log__time" style={{ color: "var(--text-3)" }}>
          {preamble}
        </div>
        {/* One line at the top of the scrollback rather than a toast that
            disappears: what is gone is gone, and the durable record has a
            name. */}
        {dropped ? (
          <div style={{ color: "var(--text-3)" }}>
            {"──── earlier lines are not held here any more · the full record is on the Activity page ────"}
          </div>
        ) : null}
        {entries.map((entry, i) =>
          entry.kind === "restart" ? (
            <div key={"restart-" + i} style={{ color: "var(--warn)" }}>
              {restartRule(entry.at)}
            </div>
          ) : (
            <DockLine key={entry.event.sequence + "-" + i} event={entry.event} viewer={viewer} />
          )
        )}
      </div>
    </>
  );
}

function DockLine({ event, viewer }: { event: SetActivityEvent; viewer: string | null }) {
  const prefix = dockPrefix(event, viewer);
  const line = activityLine(event);
  const tone: LineTone = line.tone;
  return (
    <div>
      <span className="activity-log__time">{line.time}</span>{" "}
      <span
        className="activity-log__time"
        style={{ color: prefix.action ? "var(--accent)" : "var(--text-3)" }}
      >
        {"[" + prefix.label + "]"}
      </span>{" "}
      <span className={"activity-log__line--" + tone} style={{ whiteSpace: "pre-wrap" }}>
        {line.text}
      </span>
    </div>
  );
}
