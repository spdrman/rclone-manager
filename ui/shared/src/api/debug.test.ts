/**
 * Issue #730's diagnostics, pinned at the two things that can go wrong
 * with a diagnostic: it is on when nobody asked (noise in every
 * deployment, forever), or it is off when somebody did (the one
 * deployment that needed it produces nothing, and the operator's console
 * screenshot is blank).
 *
 * The client-side case is the one the issue is actually about: a `fetch`
 * that rejects, which is the only failure `RequestFailure` cannot describe
 * beyond its kind.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { debugLog, describeError, isDebugEnabled } from "./debug";
import { httpApi } from "./client";
import { RequestFailure } from "./contracts";

/** The module folds `?debug=1` into storage once per page rather than
 *  re-parsing the URL on every log line, so a case that changes the URL
 *  has to load a fresh copy of it to be reading its own URL rather than
 *  the previous case's answer. A static import cannot express that: this
 *  is the module-loading boundary itself under test. */
async function freshModule() {
  vi.resetModules();
  return await import("./debug");
}

beforeEach(() => {
  window.localStorage.clear();
  window.history.pushState({}, "", "/");
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("the debug toggle", () => {
  it("is off by default, and logs nothing", () => {
    const debug = vi.spyOn(console, "debug").mockImplementation(() => {});
    const error = vi.spyOn(console, "error").mockImplementation(() => {});

    expect(isDebugEnabled()).toBe(false);
    debugLog("request.start", { method: "GET" });
    debugLog("request.no-response", { path: "/activity" }, "error");

    expect(debug).not.toHaveBeenCalled();
    expect(error).not.toHaveBeenCalled();
  });

  it("logs under the [rm-debug] prefix once the key is set", () => {
    const debug = vi.spyOn(console, "debug").mockImplementation(() => {});
    window.localStorage.setItem("rm-debug", "1");

    expect(isDebugEnabled()).toBe(true);
    debugLog("request.start", { method: "GET", url: "/api/v1/activity" });

    expect(debug).toHaveBeenCalledWith("[rm-debug]", "request.start", {
      method: "GET",
      url: "/api/v1/activity"
    });
  });

  it("reports a failure through console.error, so a browser filtered to errors still shows it", () => {
    const debug = vi.spyOn(console, "debug").mockImplementation(() => {});
    const error = vi.spyOn(console, "error").mockImplementation(() => {});
    window.localStorage.setItem("rm-debug", "1");

    debugLog("request.no-response", { path: "/activity" }, "error");

    expect(error).toHaveBeenCalledWith("[rm-debug]", "request.no-response", { path: "/activity" });
    expect(debug).not.toHaveBeenCalled();
  });

  it("turns itself on for ?debug=1, and persists so the next page keeps it", async () => {
    window.history.pushState({}, "", "/activity?debug=1");

    const { isDebugEnabled: enabled } = await freshModule();

    expect(enabled()).toBe(true);
    expect(window.localStorage.getItem("rm-debug")).toBe("1");
  });

  it("stays off for a URL that does not ask, and writes nothing", async () => {
    window.history.pushState({}, "", "/activity?debug=0");

    const { isDebugEnabled: enabled } = await freshModule();

    expect(enabled()).toBe(false);
    expect(window.localStorage.getItem("rm-debug")).toBeNull();
  });

  /**
   * The off switch, which is the half an operator needs at the END of
   * the call. Without it the only way to stop a browser logging every
   * request is to walk somebody through clearing a localStorage key in
   * the developer console, and a tab left diagnosing forever is a
   * support burden the diagnostic itself created.
   */
  it("turns itself off for ?debug=0, clearing a toggle already set", async () => {
    window.localStorage.setItem("rm-debug", "1");
    window.history.pushState({}, "", "/activity?debug=0");

    const { isDebugEnabled: enabled } = await freshModule();

    expect(enabled()).toBe(false);
    expect(window.localStorage.getItem("rm-debug")).toBeNull();
  });

  it("leaves a toggle alone for a ?debug value that means neither", async () => {
    window.localStorage.setItem("rm-debug", "1");
    window.history.pushState({}, "", "/activity?debug=maybe");

    const { isDebugEnabled: enabled } = await freshModule();

    // Only "1" and "0" mean anything. An unrelated value silently
    // clearing a toggle somebody set deliberately is worse than
    // ignoring it.
    expect(enabled()).toBe(true);
  });

  /**
   * Every API call this bundle makes goes through the one `request`
   * function that logs through here, so a detail object built whether or
   * not anybody asked for it is built on every request of every
   * deployment. The thunk form is how that cost stays at zero.
   */
  it("never builds a detail object while it is off", () => {
    const built = vi.fn(() => ({ url: "/api/v1/activity" }));

    debugLog("request.start", built);

    expect(built).not.toHaveBeenCalled();

    window.localStorage.setItem("rm-debug", "1");
    const debug = vi.spyOn(console, "debug").mockImplementation(() => {});
    debugLog("request.start", built);

    expect(built).toHaveBeenCalledTimes(1);
    expect(debug).toHaveBeenCalledWith("[rm-debug]", "request.start", { url: "/api/v1/activity" });
  });

  it("is off, rather than broken, in a browser with no usable storage", async () => {
    vi.stubGlobal("window", { location: window.location });

    const { isDebugEnabled: enabled, debugLog: log } = await freshModule();

    expect(enabled()).toBe(false);
    expect(() => log("request.start", {})).not.toThrow();
  });
});

describe("describeError", () => {
  it("keeps the three facts about a thrown Error apart", () => {
    const described = describeError(new TypeError("Failed to fetch"));

    expect(described.name).toBe("TypeError");
    expect(described.message).toBe("Failed to fetch");
    expect(described.stack).toBeTruthy();
  });

  it("describes a thrown non-Error rather than dropping it", () => {
    expect(describeError("boom")).toEqual({ name: "string", message: "boom" });
  });
});

describe("request() diagnostics for #730's rejected fetch", () => {
  /** What the NAS produces: fetch rejects, no response ever exists. */
  function rejectingFetch() {
    const fetchMock = vi.fn().mockRejectedValue(new TypeError("Failed to fetch"));
    vi.stubGlobal("fetch", fetchMock);
    return fetchMock;
  }

  it("says nothing at all when diagnostics are off", async () => {
    rejectingFetch();
    const debug = vi.spyOn(console, "debug").mockImplementation(() => {});
    const error = vi.spyOn(console, "error").mockImplementation(() => {});

    await expect(httpApi.listActivity()).rejects.toBeInstanceOf(RequestFailure);

    expect(debug).not.toHaveBeenCalled();
    expect(error).not.toHaveBeenCalled();
  });

  it("records the URL, the thrown TypeError and the page's scheme, and still throws the typed failure", async () => {
    const fetchMock = rejectingFetch();
    const error = vi.spyOn(console, "error").mockImplementation(() => {});
    window.localStorage.setItem("rm-debug", "1");

    const failure = await httpApi.listActivity().then(
      () => null,
      (e: unknown) => e
    );

    // The diagnostic is additive: the typed failure every caller above
    // branches on is unchanged.
    expect(failure).toBeInstanceOf(RequestFailure);
    expect((failure as RequestFailure).kind).toBe("no-response");

    const [prefix, event, detail] = error.mock.calls[0] as [string, string, Record<string, unknown>];
    expect(prefix).toBe("[rm-debug]");
    expect(event).toBe("request.no-response");
    expect(detail.url).toBe("/api/v1/activity?");
    expect(detail.method).toBe("GET");
    expect(detail.protocol).toBe(window.location.protocol);
    expect(detail.cause).toMatchObject({ name: "TypeError", message: "Failed to fetch" });
    expect(typeof detail.elapsedMs).toBe("number");

    // This is the whole point of the attempt id: there is no response,
    // so no correlation id exists, and the only thing that can join this
    // console line to the server's record of the same request is a name
    // the browser SENT. A logged id that is not the one on the wire
    // would be worse than none.
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const sent = (init.headers as Record<string, string>)["X-Client-Attempt-Id"];
    expect(sent).toMatch(/^[0-9a-f]{16}$/);
    expect(detail.attemptId).toBe(sent);
  });

  it("names the correlation id a refused response carried, so it can be matched to the server log", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 401,
        headers: new Headers({ "x-correlation-id": "corr-730" }),
        json: async () => ({ code: "UNAUTHENTICATED", message: "sign in" })
      })
    );
    const error = vi.spyOn(console, "error").mockImplementation(() => {});
    window.localStorage.setItem("rm-debug", "1");

    await expect(httpApi.listActivity()).rejects.toThrow();

    const statusLine = error.mock.calls.find((call) => call[1] === "request.error-status");
    expect(statusLine?.[2]).toMatchObject({ status: 401, correlationId: "corr-730" });
  });
});
