/**
 * Issue #795, the surface BEFORE the Activity page.
 *
 * The reported deployment's web-ui container could not reach the engine
 * container. serve-ui proxies the whole of /api/v1 to the engine,
 * /auth/session included, so the very first question this app asks on
 * load — "does this browser have a session" — was answered 502 by
 * serve-ui's own reverse proxy rather than by anything that had looked at
 * a session.
 *
 * Six provider bridges then said, in six identical copies:
 *
 *     if (!res.ok) return { authenticated: false, ... };
 *
 * and PlatformContext turned any rejection into the same thing. So the
 * operator was told they were signed out and given a sign-in form that
 * posts down the same broken hop: every attempt fails, and nothing on
 * screen says the service is unreachable. That is the same defect as the
 * Activity page's, one surface earlier and worse, because the only action
 * offered is the one that cannot work.
 *
 * Two levels are driven here, because the fix is at two levels. The
 * bridge's own read has to stop inventing a verdict, and the app has to
 * have somewhere to put the failure once it stops.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { App } from "@shared/App";
import { ApiProvider } from "@shared/api/ApiContext";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { createMockApi } from "@shared/api/mock";
import { readLocalAccountSession } from "@shared/platform/localSession";
import { BackupdError, RequestFailure } from "@shared/api/contracts";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { resetGraphForTests } from "@shared/state/graph";
import type { AuthContext, PlatformBridge } from "@shared/types/platform";

/** serve-ui's answer when its upstream is unreachable: a status line, the
 *  id it minted, and no body (webhost/serve/ui.go's ErrorHandler). */
function bodylessGateway(status = 502) {
  return {
    ok: false,
    status,
    headers: new Headers({ "x-correlation-id": "cid_proxy502" }),
    json: async () => {
      throw new SyntaxError("Unexpected end of JSON input");
    }
  };
}

function signedOut() {
  return {
    ok: false,
    status: 401,
    headers: new Headers(),
    json: async () => ({ code: "UNAUTHENTICATED", message: "not signed in" })
  };
}

function signedIn(username = "e2e-operator") {
  return {
    ok: true,
    status: 200,
    headers: new Headers({ "content-type": "application/json" }),
    json: async () => ({ username })
  };
}

/** A policy denial: something between the browser and the service refused
 *  the request outright, which no amount of signing in changes. A gateway
 *  rule, a proxy requiring a header this browser does not send, an
 *  authorisation decision made above the session. */
function policyDenied(message = "this deployment does not allow session checks from here") {
  return {
    ok: false,
    status: 403,
    headers: new Headers({ "x-correlation-id": "cid_denied403" }),
    json: async () => ({ error: { code: "FORBIDDEN", message } })
  };
}

/** A 200 whose body is not JSON: #598's other half, reached on the
 *  session route. Backupd ANSWERED, which is the fact that matters here. */
function unreadableSession() {
  return {
    ok: true,
    status: 200,
    headers: new Headers({ "content-type": "text/html", "x-correlation-id": "cid_html200" }),
    json: async () => {
      throw new SyntaxError("Unexpected token '<'");
    }
  };
}

describe("the session read answers only what it was actually told", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("reports signed out when the service said so", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(signedOut()));
    await expect(readLocalAccountSession()).resolves.toEqual({
      authenticated: false,
      username: null,
      mode: "local-account"
    });
  });

  it("reports the identity when the service answered with one", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(signedIn()));
    await expect(readLocalAccountSession()).resolves.toEqual({
      authenticated: true,
      username: "e2e-operator",
      mode: "local-account"
    });
  });

  it("refuses to call a bodyless 502 a signed-out browser", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(bodylessGateway()));
    // The whole of #795's first surface. Nothing looked at a session, so
    // there is no answer about one, and resolving `{authenticated:false}`
    // here is this frontend making one up.
    const failure = await readLocalAccountSession().then(
      (ctx) => ctx,
      (e: unknown) => e
    );
    expect(failure).toBeInstanceOf(BackupdError);
    expect((failure as BackupdError).api.status).toBe(502);
    expect((failure as BackupdError).api.correlationId).toBe("cid_proxy502");
  });

  it("refuses to call a rejected fetch a signed-out browser either", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Failed to fetch")));
    const failure = await readLocalAccountSession().then(
      (ctx) => ctx,
      (e: unknown) => e
    );
    expect(failure).toBeInstanceOf(RequestFailure);
    expect((failure as RequestFailure).kind).toBe("no-response");
    expect((failure as RequestFailure).path).toBe("/auth/session");
  });

  it("does not call a 403 a signed-out browser: no sign-in fixes a policy denial", async () => {
    // #795's review. 401 and 403 both used to resolve signed-out, and
    // only one of them is a statement about this session: the route's
    // contract is 401 for one that is absent or expired. A 403 is a
    // decision made above it — a gateway rule, a proxy demanding a header
    // this browser does not send — and sending the operator to the login
    // form for one is the same defect as the 502 above, with the one
    // action that cannot help it under it.
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(policyDenied()));
    const failure = await readLocalAccountSession().then(
      (ctx) => ctx,
      (e: unknown) => e
    );
    expect(failure).toBeInstanceOf(BackupdError);
    expect((failure as BackupdError).api.status).toBe(403);
    // The service's own words, kept: this is the most specific thing
    // anybody has about a refusal nothing else can classify.
    expect((failure as BackupdError).api.message).toContain("does not allow session checks");
    expect((failure as BackupdError).api.correlationId).toBe("cid_denied403");
  });

  it("names its attempt on the way out, like every other request this bundle makes", async () => {
    // The session read is the FIRST call the app makes and the one #795
    // was reported through, and it used to go out anonymously: a request
    // that gets no response carries no correlation id back, so without
    // this header there is nothing at all to match the browser's failure
    // against the server's own line for it (#730's mechanism, #795's
    // review for the omission). Shared with api/client.ts through
    // api/transport.ts rather than copied.
    const fetchMock = vi.fn().mockResolvedValue(signedIn());
    vi.stubGlobal("fetch", fetchMock);
    await readLocalAccountSession();

    const init = fetchMock.mock.calls[0][1] as RequestInit;
    const headers = init.headers as Record<string, string>;
    expect(headers["X-Client-Attempt-Id"]).toMatch(/^[0-9a-f]{16}$/);
  });

  // The sweep that proves all six local-account bridges really read
  // through this and not through a seventh copy lives in
  // apps/common/tests/providerConformance.test.ts, which is the one
  // place in the repository allowed to import every provider: ui/shared
  // must keep building with any single apps/<provider>/ deleted.
});

function renderApp(bridge: PlatformBridge) {
  return render(
    <MemoryRouter initialEntries={["/activity"]}>
      <ApiProvider api={createMockApi()}>
        <PlatformProvider bridge={bridge}>
          <App />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

describe("an app that could not ask does not claim the operator is signed out", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("says the service did not answer instead of drawing a sign-in form", async () => {
    renderApp({
      ...genericBridge,
      getAuthContext: () =>
        Promise.reject(
          new RequestFailure({ kind: "no-response", path: "/auth/session", cause: new TypeError("Failed to fetch") })
        )
    });
    await act(async () => {});

    // The form is the thing that must NOT be here: it posts down the same
    // hop that just failed, so every attempt fails and the operator is
    // left believing their password stopped working.
    expect(screen.queryByRole("heading", { name: "Sign in" })).toBeNull();
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("Backupd did not answer");
    expect(screen.getByText(/could not reach Backupd to ask whether you are signed in/i)).toBeTruthy();
    // And it does not promise the session survived. The engine holds its
    // sessions in its own process, so the restart this page is most
    // often shown for ends them; "you have not been signed out" was the
    // first draft, and the container run is what caught it.
    expect(document.body.textContent).not.toMatch(/have not been signed out/i);
    expect(document.body.textContent).not.toContain("unavailable");
  });

  it("says Backupd is not answering only when it did not answer", async () => {
    // #795's review: every rejection used to land on that heading, and
    // two of the four rejections this gate sees are Backupd ANSWERING.
    // A 403 is one of them. The heading claimed silence directly above an
    // ErrorState quoting what was said, which is a page an operator
    // cannot act on because it disagrees with itself.
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(policyDenied()));
    renderApp(genericBridge);
    await act(async () => {});

    // Still not the sign-in form: re-signing-in cannot lift a policy
    // denial, and offering it is what #795 is about.
    expect(screen.queryByRole("heading", { name: "Sign in" })).toBeNull();
    expect(screen.getByRole("heading", { name: "Backupd could not check your session" })).toBeTruthy();
    expect(document.body.textContent).not.toMatch(/is not answering|did not answer/i);
    // The service's own sentence, verbatim, because on this path it is
    // the most specific thing anybody has.
    expect(screen.getByRole("alert").textContent).toContain("does not allow session checks from here");
    expect(screen.getByRole("alert").textContent).toContain("cid_denied403");
    // And it does not name a hop: which machine refused is exactly what
    // is NOT established here.
    expect(document.body.textContent).not.toContain("could not reach the Backupd service");
  });

  it("does not claim silence for a 200 whose body could not be read", async () => {
    // The other answered-but-unusable case, and the one that made the
    // contradiction unmissable: the ErrorState's own first line is
    // "Backupd answered, and this page could not read the answer", under
    // a heading that used to say nothing answered at all.
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(unreadableSession()));
    renderApp(genericBridge);
    await act(async () => {});

    expect(screen.queryByRole("heading", { name: "Sign in" })).toBeNull();
    expect(screen.getByRole("heading", { name: "Backupd could not check your session" })).toBeTruthy();
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("could not read the answer");
    expect(document.body.textContent).not.toMatch(/is not answering/i);
  });

  it("names the hop when serve-ui answered 502 for an engine it could not reach", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(bodylessGateway()));
    renderApp(genericBridge);
    await act(async () => {});

    expect(screen.queryByRole("heading", { name: "Sign in" })).toBeNull();
    expect(screen.getByRole("alert").textContent).toContain("could not reach the Backupd service");
  });

  it("still shows the sign-in form when the service really said not signed in", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(signedOut()));
    renderApp(genericBridge);
    await act(async () => {});

    // The gate above must not swallow the ordinary case: a 401 is an
    // answer, and the answer is "sign in".
    expect(screen.getByRole("heading", { name: "Sign in" })).toBeTruthy();
    expect(screen.queryByText(/could not reach Backupd to ask/i)).toBeNull();
  });

  it("asks again when Try again is pressed, and gets out of the way once it works", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(bodylessGateway())
      .mockResolvedValue(signedIn());
    vi.stubGlobal("fetch", fetchMock);
    renderApp(genericBridge);
    await act(async () => {});

    expect(screen.getByRole("alert")).toBeTruthy();

    await userEvent.click(screen.getByRole("button", { name: /try again/i }));
    await act(async () => {});

    // The engine came back, so the session read succeeds and the app is
    // where the operator left it, signed in, with no reload needed.
    expect(screen.queryByText(/could not reach Backupd to ask/i)).toBeNull();
    expect(await screen.findByRole("navigation", { name: "Sections" }, { timeout: 4000 })).toBeTruthy();
  });
});

/**
 * Issue #795, found by running the four-container rig rather than by
 * reading anything.
 *
 * The engine keeps its sessions in its own process, so the restart that
 * #795's report describes — the container went away and came back — ends
 * every one of them. What an operator met when it came back was not the
 * sign-in form. It was the Activity page holding a red panel reading
 * "authentication required", with a Try again button under it that could
 * never succeed, because the thing that failed was not the read.
 *
 * Reloading produced the form, so the way out existed and was simply
 * never offered, which is the same family of defect as #795 itself: the
 * failure was surfaced as something other than what it was, and the
 * action offered could not address it.
 */
describe("a session that ended while the app was open sends the operator to sign in", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  /** Signed in at first, and signed out from the moment the engine says
   *  so — which is what a restarted engine really answers to both the
   *  page read and the session check. */
  function restartingDeployment() {
    let sessionAlive = true;
    const api = createMockApi();
    vi.spyOn(api, "listActivity").mockImplementation(() => {
      if (sessionAlive) return Promise.resolve({ events: [] });
      return Promise.reject(
        new BackupdError({ code: "UNAUTHENTICATED", message: "authentication required", correlationId: "cid_gone1" })
      );
    });
    const bridge: PlatformBridge = {
      ...genericBridge,
      getAuthContext: () =>
        Promise.resolve(
          sessionAlive
            ? { authenticated: true, username: "e2e-operator", mode: "local-account" as const }
            : { authenticated: false, username: null, mode: "local-account" as const }
        )
    };
    return { api, bridge, restart: () => { sessionAlive = false; } };
  }

  it("does not leave a Try again beside a failure a retry cannot fix", async () => {
    const { api, bridge, restart } = restartingDeployment();
    render(
      <MemoryRouter initialEntries={["/activity"]}>
        <ApiProvider api={api}>
          <PlatformProvider bridge={bridge}>
            <App />
          </PlatformProvider>
        </ApiProvider>
      </MemoryRouter>
    );
    expect(await screen.findByRole("navigation", { name: "Sections" }, { timeout: 4000 })).toBeTruthy();

    // The engine restarts under the loaded app, and the page re-reads.
    restart();
    await userEvent.click(
      within(screen.getByRole("navigation", { name: "Sections" })).getByRole("link", { name: /Dashboard/i })
    );
    await act(async () => {});

    // The one thing that must be on screen is the way back in. A panel
    // saying "authentication required" with a button that re-issues the
    // same refused read is a dead end with a button on it.
    expect(await screen.findByRole("heading", { name: "Sign in" }, { timeout: 4000 })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /try again/i })).toBeNull();
  });

  it("leaves an ordinary page failure retryable, which is what the button is for", async () => {
    // The guard against the fix above turning every failure into a
    // sign-out: a read that failed for any other reason keeps its panel
    // and its Try again, and the operator stays where they were.
    const api = createMockApi();
    vi.spyOn(api, "listActivity").mockRejectedValue(
      new BackupdError({ code: "INTERNAL", message: "failed to list activity", correlationId: "cid_internal1" })
    );
    render(
      <MemoryRouter initialEntries={["/activity"]}>
        <ApiProvider api={api}>
          <PlatformProvider
            bridge={{
              ...genericBridge,
              getAuthContext: () =>
                Promise.resolve({ authenticated: true, username: "e2e-operator", mode: "local-account" as const })
            }}
          >
            <App />
          </PlatformProvider>
        </ApiProvider>
      </MemoryRouter>
    );
    expect(await screen.findByRole("navigation", { name: "Sections" }, { timeout: 4000 })).toBeTruthy();

    expect(screen.queryByRole("heading", { name: "Sign in" })).toBeNull();
    expect(screen.getByRole("button", { name: /try again/i })).toBeTruthy();
  });
});

describe("a native-session provider is untouched by any of this", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("keeps rendering the shell for a host-supplied identity", async () => {
    // UGOS owns the identity, so an unreachable engine cannot make this
    // browser look signed out — which is why the reported NAS reached the
    // Activity page at all, and why that page had to say something.
    const native: AuthContext = { authenticated: true, username: "ugreen-admin", mode: "native-session" };
    renderApp({ ...genericBridge, getAuthContext: () => Promise.resolve(native) });
    await act(async () => {});

    expect(await screen.findByRole("navigation", { name: "Sections" }, { timeout: 4000 })).toBeTruthy();
  });
});
