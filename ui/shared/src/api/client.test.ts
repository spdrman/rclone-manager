import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { httpApi } from "./client";

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
