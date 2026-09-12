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
import { act, cleanup, render, screen } from "@testing-library/react";
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
    expect(screen.getByText(/have not been signed out/i)).toBeTruthy();
    expect(document.body.textContent).not.toContain("unavailable");
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
    expect(screen.queryByText(/have not been signed out/i)).toBeNull();
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
    expect(screen.queryByText(/have not been signed out/i)).toBeNull();
    expect(await screen.findByRole("navigation", { name: "Sections" }, { timeout: 4000 })).toBeTruthy();
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
