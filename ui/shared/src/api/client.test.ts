import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { httpApi } from "./client";
import type { ApiErrorCode } from "./contracts";

/** Sets document.cookie the way a browser would after the server issued
 *  a Set-Cookie header for bm_csrf — jsdom's document.cookie setter
 *  accepts the same "name=value" assignment form. */
function setCsrfCookie(value: string) {
  document.cookie = "bm_csrf=" + value;
}

function mockFetchOk(body: unknown = undefined, status = 200) {
  return vi.fn().mockResolvedValue({
    ok: true,
    status,
    headers: new Headers(),
    json: async () => body
  });
}

describe("httpApi CSRF/bootstrap-token wiring", () => {
  beforeEach(() => {
    // Clear any cookie a previous test left behind.
    document.cookie = "bm_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT";
    window.history.pushState({}, "", "/");
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("attaches X-CSRF-Token on a state-changing request when the cookie is present", async () => {
    setCsrfCookie("csrf-value-123");
    const fetchMock = mockFetchOk(undefined, 204);
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.login("bm-admin", "hunter22222222");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const headers = init.headers as Record<string, string>;
    expect(headers["X-CSRF-Token"]).toBe("csrf-value-123");

    vi.unstubAllGlobals();
  });

  it("does not attach X-CSRF-Token on a GET request", async () => {
    setCsrfCookie("csrf-value-123");
    const fetchMock = mockFetchOk({});
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.getVersion();

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const headers = init.headers as Record<string, string>;
    expect(headers["X-CSRF-Token"]).toBeUndefined();

    vi.unstubAllGlobals();
  });

  it("omits X-CSRF-Token when no cookie has been issued yet", async () => {
    const fetchMock = mockFetchOk(undefined, 204);
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.login("bm-admin", "hunter22222222");

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const headers = init.headers as Record<string, string>;
    expect(headers["X-CSRF-Token"]).toBeUndefined();

    vi.unstubAllGlobals();
  });

  it("attaches X-Bootstrap-Token, read from the URL's ?token= param, only for enrollAdministrator", async () => {
    window.history.pushState({}, "", "/enroll?token=bootstrap-abc");
    const fetchMock = mockFetchOk(undefined, 204);
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.enrollAdministrator("bm-admin", "hunter22222222");

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const headers = init.headers as Record<string, string>;
    expect(headers["X-Bootstrap-Token"]).toBe("bootstrap-abc");

    vi.unstubAllGlobals();
  });

  it("does not attach X-Bootstrap-Token to routes other than enroll, even with ?token= present", async () => {
    window.history.pushState({}, "", "/enroll?token=bootstrap-abc");
    const fetchMock = mockFetchOk(undefined, 204);
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.login("bm-admin", "hunter22222222");

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const headers = init.headers as Record<string, string>;
    expect(headers["X-Bootstrap-Token"]).toBeUndefined();

    vi.unstubAllGlobals();
  });
});

describe("ApiErrorCode covers every code apps/common/auth/local actually emits", () => {
  // Mirrors the literal strings passed to writeAuthError across
  // apps/common/auth/local/handler.go and csrf.go - kept here by hand,
  // since nothing generates this list automatically from the Go source.
  // The type annotation below is what actually enforces "subset of the
  // declared union": a code emitted there but missing from ApiErrorCode
  // fails typecheck here, not just silently passes at runtime (issue
  // #119's review, finding 9 - client.ts's own `as ApiError` assertion
  // has no runtime check, so this is the only thing that would catch a
  // future drift between the two).
  const localAuthErrorCodes: ApiErrorCode[] = [
    "UNAUTHENTICATED",
    "RATE_LIMITED",
    "INVALID_REQUEST",
    "ENROLLMENT_CLOSED",
    "BOOTSTRAP_TOKEN_INVALID",
    "INTERNAL_ERROR",
    "CSRF_TOKEN_MISSING",
    "CSRF_TOKEN_MISMATCH"
  ];

  it("lists every code this frontend's own backend can actually return (this test would pass vacuously on an empty array)", () => {
    expect(localAuthErrorCodes.length).toBeGreaterThan(0);
    expect(new Set(localAuthErrorCodes).size).toBe(localAuthErrorCodes.length);
  });
});
