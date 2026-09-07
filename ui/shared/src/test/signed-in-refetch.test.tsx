/**
 * What the app reads once somebody signs in.
 *
 * App.tsx owns the one fetch of health, sets, quarantine and operations,
 * and it issues all four above the authenticated branch, so on a browser
 * with no session every one of them is refused before the operator has
 * typed anything. That is fine on its own. What was not fine is what
 * happened next: the deps on those four reads are `[api]`, which never
 * changes, so signing in did not re-issue any of them, and the 30 second
 * poll that would eventually have covered it is gated on the instance
 * being configured, which a fresh install is not.
 *
 * So the first thing a new operator did after creating their
 * administrator account was open Backup sets and be told they were not
 * authenticated, with no way forward but a full page load. A live browser
 * spec against a real deployment hit exactly that and worked around it by
 * reloading the page, noting in a comment that the reload was a
 * workaround rather than the flow.
 *
 * These cases drive the real shell with a real sign-in rather than
 * asserting on the hook, because every piece was individually correct: the
 * fetch worked, the refusal was right, the sign-in worked. Only the
 * sequence was broken.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { App } from "@shared/App";
import { ApiProvider } from "@shared/api/ApiContext";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { createMockApi } from "@shared/api/mock";
import { BackupManagerError } from "@shared/api/contracts";
import type { BackupManagerApi } from "@shared/api/contracts";
import type { AuthContext, PlatformBridge } from "@shared/types/platform";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { resetGraphForTests } from "@shared/state/graph";

const SIGNED_OUT: AuthContext = { authenticated: false, username: null, mode: "local-account" };
const SIGNED_IN: AuthContext = { authenticated: true, username: "bm-admin", mode: "local-account" };

function unauthenticated() {
  return new BackupManagerError({
    code: "UNAUTHENTICATED",
    message: "Sign in to continue.",
    correlationId: "cid_test"
  });
}

/**
 * A deployment that refuses every read until somebody signs in, and a
 * bridge that reports the session the same way.
 *
 * The two are wired to one mutable flag on purpose: a fixture where the
 * API and the bridge could disagree about whether there is a session
 * would be a fixture that proves nothing about the order these things
 * happen in, which is the entire subject here.
 */
function deployment(options: { configured: boolean }) {
  const session = { current: SIGNED_OUT };
  const base = createMockApi();

  const guard = <T,>(fn: () => Promise<T>) => (): Promise<T> =>
    session.current.authenticated ? fn() : Promise.reject(unauthenticated());

  const api: BackupManagerApi = {
    ...base,
    getHealth: guard(() => base.getHealth()),
    getVersion: guard(() => base.getVersion()),
    listSets: guard(() => base.listSets()),
    listQuarantine: guard(() => base.listQuarantine()),
    listOperations: guard(() => base.listOperations()),
    getFirstRunStatus: guard(() => Promise.resolve({ configured: options.configured })),
    login: () => {
      session.current = SIGNED_IN;
      return Promise.resolve();
    }
  };

  const bridge: PlatformBridge = { ...genericBridge, getAuthContext: () => Promise.resolve(session.current) };
  return { api, bridge, session };
}

function renderApp(api: BackupManagerApi, bridge: PlatformBridge, route: string) {
  return render(
    <MemoryRouter initialEntries={[route]}>
      <ApiProvider api={api}>
        <PlatformProvider bridge={bridge}>
          <App />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

async function signIn() {
  await screen.findByLabelText("Username");
  await userEvent.type(screen.getByLabelText("Username"), "bm-admin");
  await userEvent.type(screen.getByLabelText(/^Password/), "a-long-enough-passphrase");
  await userEvent.click(screen.getByRole("button", { name: /sign in/i }));
}

afterEach(() => {
  cleanup();
  resetGraphForTests();
  vi.restoreAllMocks();
});

describe("signing in", () => {
  it("leaves a fresh install on a working Backup sets page, not one that says it is not authenticated", async () => {
    // configured: false is the fresh install, and it is the case with no
    // way out. The 30 second poll that would eventually cover a signed-in
    // browser is switched off until an instance is known to be
    // configured, so on this one nothing ever asks again.
    const { api, bridge } = deployment({ configured: false });
    renderApp(api, bridge, "/sets");

    await signIn();

    await screen.findByRole("navigation", { name: "Sections" }, { timeout: 4000 });
    // The page an operator lands on after creating their account offers
    // the one action there is to take. Being told to sign in on the far
    // side of signing in is the defect.
    expect(await screen.findByRole("button", { name: /add backup set/i })).toBeInTheDocument();
    expect(screen.queryByText(/sign in to continue/i)).not.toBeInTheDocument();
  });

  it("asks for everything the shell owns, once there is a session to ask with", async () => {
    const { api, bridge } = deployment({ configured: true });
    const listSets = vi.spyOn(api, "listSets");
    const getHealth = vi.spyOn(api, "getHealth");
    const listOperations = vi.spyOn(api, "listOperations");
    const listQuarantine = vi.spyOn(api, "listQuarantine");

    renderApp(api, bridge, "/");
    await screen.findByLabelText("Username");

    // Counted from the moment before sign-in, not from zero. Asserting
    // "was called at all" would have passed against the defect itself:
    // all four ARE called, once, before there is a session, and that call
    // is the refusal. What has to be true is that each one is asked
    // AGAIN, with a session behind it.
    const before = {
      sets: listSets.mock.calls.length,
      health: getHealth.mock.calls.length,
      operations: listOperations.mock.calls.length,
      quarantine: listQuarantine.mock.calls.length
    };

    await signIn();
    await screen.findByRole("navigation", { name: "Sections" }, { timeout: 4000 });

    // All four, because all four are refused before a session exists and
    // all four are read by a page the operator can reach immediately
    // afterwards. Three out of four recovering would be the same defect
    // with a smaller blast radius.
    await waitFor(() => {
      expect(listSets.mock.calls.length).toBeGreaterThan(before.sets);
      expect(getHealth.mock.calls.length).toBeGreaterThan(before.health);
      expect(listOperations.mock.calls.length).toBeGreaterThan(before.operations);
      expect(listQuarantine.mock.calls.length).toBeGreaterThan(before.quarantine);
    });
  });

  it("does not ask before there is a session, so a signed-out browser produces no refusals at all", async () => {
    const { api, bridge } = deployment({ configured: true });
    const listSets = vi.spyOn(api, "listSets");
    const getHealth = vi.spyOn(api, "getHealth");

    renderApp(api, bridge, "/");
    await screen.findByLabelText("Username");

    // Four refused requests on every visit to the sign-in page is noise in
    // the log of whoever is reading it, and it is the same argument the
    // polling loop already makes for staying switched off on a fresh
    // install.
    expect(listSets).not.toHaveBeenCalled();
    expect(getHealth).not.toHaveBeenCalled();
  });

  it("shows the shell rather than a refusal on the dashboard immediately after signing in", async () => {
    const { api, bridge } = deployment({ configured: true });
    renderApp(api, bridge, "/");

    await signIn();
    await screen.findByRole("navigation", { name: "Sections" }, { timeout: 4000 });

    await waitFor(() => {
      expect(screen.queryByText(/sign in to continue/i)).not.toBeInTheDocument();
    });
  });
});
