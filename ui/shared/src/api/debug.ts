/**
 * The browser half of issue #730's diagnostics, and nothing else.
 *
 * #730 is a `fetch('/api/v1/activity')` that rejects with
 * `TypeError: Failed to fetch` on one real UGREEN NAS deployment while
 * `curl` to the same route answers a clean 401. A rejected fetch is the
 * one failure a browser tells JavaScript almost nothing about: there is no
 * status, no headers and no correlation id, so `RequestFailure` carries
 * exactly what it can and no more (contracts.ts). Everything ELSE the
 * browser knows at that instant — which URL was asked for, how long the
 * attempt lasted, whether the tab thought it was online, which scheme the
 * page is on, and what the thrown value actually was — is knowable only
 * here, at the call site, and was previously thrown away.
 *
 * So it is written to the console instead, under an operator-controlled
 * toggle:
 *
 *   - `localStorage["rm-debug"] = "1"`, or
 *   - open any page with `?debug=1`, which persists the same key so the
 *     toggle survives the navigations that follow (an operator on a NAS
 *     is usually reading this over someone's shoulder on a phone).
 *
 * Off is the default and off is silent: `debugLog` self-gates, so a
 * caller never needs an `if` around it and a deployment that has not
 * asked for diagnostics gets byte-identical behaviour and no console
 * output. Nothing here runs at import time either — the toggle is read on
 * first use, not on module load, so importing this file changes nothing.
 *
 * This is a DIAGNOSTIC channel, not a user-facing one. What an operator
 * reads on screen still comes from api/failure.ts, which renders
 * `describeException` (a sentence) rather than the structured objects
 * below (a console tree). The two exist for different readers and
 * deliberately do not share a format.
 */

/** Where the toggle is persisted, and the query parameter that sets it.
 *  Spelled once: the backend's own half of this switch is `RM_DEBUG=1`,
 *  and the two names are meant to read as one feature. */
const DEBUG_KEY = "rm-debug";
const DEBUG_PARAM = "debug";

/** The prefix every line carries, so that a browser console filter of
 *  `rm-debug` shows this channel and nothing else. */
const PREFIX = "[rm-debug]";

/** Whether `?debug=1` has already been folded into storage. The URL is
 *  read once per page rather than on every log line: the query parameter
 *  is a way to TURN the toggle on, and the toggle itself lives in
 *  storage. */
let queryPromoted = false;

/**
 * `window.localStorage`, or null where there isn't one.
 *
 * Both halves of that are real: this module is imported by code that also
 * runs under vitest's jsdom (which ships no Storage until src/test/setup.ts
 * installs one) and a browser with site data blocked THROWS on property
 * access rather than answering null. ActivityDock's own persistence takes
 * the same precaution for the same reason.
 */
function usableStorage(): Storage | null {
  try {
    if (typeof window === "undefined") return null;
    return window.localStorage ?? null;
  } catch {
    return null;
  }
}

/**
 * Whether this browser has been asked for diagnostics.
 *
 * Read live rather than cached, so an operator who sets the key in the
 * console sees the next failure logged without reloading the page. The
 * cost is one `getItem` per call, which is why every call site goes
 * through `debugLog` and not through a hand-rolled `if`.
 */
export function isDebugEnabled(): boolean {
  const store = usableStorage();
  if (!store) return false;

  if (!queryPromoted) {
    queryPromoted = true;
    try {
      if (new URLSearchParams(window.location.search).get(DEBUG_PARAM) === "1") store.setItem(DEBUG_KEY, "1");
    } catch {
      // A location this build cannot parse is not a reason to fail the
      // request that was being diagnosed.
    }
  }

  try {
    return store.getItem(DEBUG_KEY) === "1";
  } catch {
    return false;
  }
}

/** Which console channel a line belongs on. A failure is reported through
 *  `console.error` so it survives a browser log level set to errors only,
 *  which is what an operator following instructions over the phone
 *  usually has. */
export type DebugLevel = "debug" | "error";

/**
 * One diagnostic line, written only when diagnostics are on.
 *
 * The gate is INSIDE this function on purpose: a caller that had to ask
 * first is a caller that can forget to, and a `console.debug` shipped
 * without a gate is noise in every deployment that never asked for it.
 */
export function debugLog(event: string, detail: Record<string, unknown>, level: DebugLevel = "debug"): void {
  if (!isDebugEnabled()) return;
  if (level === "error") console.error(PREFIX, event, detail);
  else console.debug(PREFIX, event, detail);
}

/** A thrown value as three readable facts. */
export interface DescribedError {
  name: string;
  message: string;
  /** Present for a real `Error`. Console-only: §37 forbids a stack
   *  reaching the SCREEN, and this never renders — it is what tells a
   *  `TypeError: Failed to fetch` raised by the network stack apart from
   *  one raised inside this bundle. */
  stack?: string;
}

/**
 * Structured counterpart to contracts.ts's `describeException`.
 *
 * That one builds the single line an operator reads under "Advanced
 * details"; this one keeps the fields apart so the console can expand
 * them, and adds the stack that the on-screen form must never carry.
 */
export function describeError(e: unknown): DescribedError {
  if (e instanceof Error) return { name: e.name, message: e.message, stack: e.stack };
  return { name: typeof e, message: String(e) };
}

/**
 * The two ambient facts that turn "fetch rejected" into something
 * actionable: whether the tab believed it had a network at all, and which
 * scheme the page was served over (#730's deployment is behind an
 * operator-owned TLS front proxy, and a mixed-content or HTTP/2 refusal
 * looks exactly like a dropped connection from JavaScript).
 *
 * Guarded, and returning absent fields rather than throwing, because this
 * is read inside a `catch` block on the way to rethrowing a typed failure.
 * An exception raised HERE would replace #730's own error with a
 * diagnostic's error, which is the one outcome worse than no diagnostic.
 */
export function debugEnvironment(): { online?: boolean; protocol?: string } {
  return {
    online: typeof navigator === "undefined" ? undefined : navigator.onLine,
    protocol: typeof location === "undefined" ? undefined : location.protocol
  };
}
