import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { App } from "@shared/App";
import { ApiProvider } from "@shared/api/ApiContext";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { createMockApi } from "@shared/api/mock";
import type { BackupManagerApi } from "@shared/api/contracts";
import type { AuthContext, PlatformBridge } from "@shared/types/platform";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { resetGraphForTests } from "@shared/state/graph";
import type { BackupSet } from "@shared/types/backup";

/**
 * Issue #285: model.BackupSetID.String() (core/internal/model/ids.go)
 * joins source and set with a "/", so a real backup set id off the wire
 * looks like "production/api-server" — never the slash-free "set_x"
 * shape the old mock/api/mock.ts fixtures happened to use. A route or a
 * URL-building helper that only works for a slash-free id is not fixed,
 * it is untested, so every fixture in this file builds that shape
 * directly rather than borrowing api/mock.ts's own ids (which #285 also
 * corrects, separately — see api/mock.ts's SETS array).
 */

const AUTHENTICATED: AuthContext = { authenticated: true, username: "bm-admin", mode: "local-account" };
const bridge: PlatformBridge = { ...genericBridge, getAuthContext: () => Promise.resolve(AUTHENTICATED) };

function renderApp(api: BackupManagerApi, route = "/") {
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

async function shellIsUp() {
  await screen.findByRole("navigation", { name: "Sections" }, { timeout: 4000 });
}

function nav() {
  return screen.getByRole("navigation", { name: "Sections" });
}

async function go(label: string) {
  await userEvent.click(within(nav()).getByRole("link", { name: new RegExp(label, "i") }));
}

function slashIdSet(overrides: Partial<BackupSet> = {}): BackupSet {
  return {
    id: "production/api-server",
    source: "production",
    set: "api-server",
    name: "API server",
    host: "api-01.internal",
    port: 22,
    username: "backup-agent",
    remoteFolder: "/srv/backups/",
    includePatterns: [],
    excludePatterns: [],
    completionMethod: "atomic-rename",
    stableForSeconds: 0,
    destination: "/data/backups/production/api-server/",
    retentionIsOverride: false,
    validations: ["transfer"],
    state: "healthy",
    stateNote: "Verified nightly.",
    enabled: true,
    newestKnownGoodAt: "2026-08-29T02:01:01Z",
    lastRunAt: "2026-08-29T02:01:01Z",
    lastValidation: "passed",
    expectedIntervalHours: 24,
    retainedCount: 4,
    retainedBytes: 1024,
    hostFingerprint: "SHA256:test-fingerprint",
    fingerprintTrustedAt: "2026-08-02T10:14:00Z",
    readOnly: false,
    readOnlyRetainedCount: 0,
    ...overrides
  };
}

function apiWithSets(sets: BackupSet[]): BackupManagerApi {
  const api = createMockApi();
  vi.spyOn(api, "listSets").mockResolvedValue(sets);
  vi.spyOn(api, "getSet").mockImplementation((id) => {
    const found = sets.find((s) => s.id === id);
    return found ? Promise.resolve(found) : Promise.reject(new Error("mock getSet: no such id " + id));
  });
  return api;
}

afterEach(() => {
  cleanup();
  resetGraphForTests();
  vi.restoreAllMocks();
});

describe("a backup set whose id contains a slash is reachable (issue #285)", () => {
  it("opens from the backup sets list", async () => {
    renderApp(apiWithSets([slashIdSet()]));
    await shellIsUp();

    await go("Backup sets");
    await userEvent.click(await screen.findByRole("button", { name: "Open" }));

    expect(await screen.findByRole("heading", { name: /API server/ })).toBeInTheDocument();
    // The set's connection panel is only on the detail page, never the
    // list — finding it is proof this actually navigated, not that the
    // list's own card text satisfied the heading query above.
    expect(await screen.findByRole("heading", { name: "Connection" })).toBeInTheDocument();
  });

  it("opens from the dashboard's halted-set banner action", async () => {
    renderApp(apiWithSets([slashIdSet({ haltReason: "host-key-changed", state: "failing" })]));
    await shellIsUp();

    await userEvent.click(await screen.findByRole("button", { name: "Review fingerprint" }));

    expect(await screen.findByRole("heading", { name: /API server/ })).toBeInTheDocument();
  });
});
