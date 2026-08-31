import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SettingsPage } from "@shared/pages/SettingsPage";
import { ActivityPage } from "@shared/pages/ActivityPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { graph, resetGraphForTests, useCausl } from "@shared/state/graph";
import { readOnlyNode, setsNode, versionNode } from "@shared/state/appNodes";
import type { BackupSet } from "@shared/types/backup";
import type { VersionInfo } from "@shared/types/operation";

const COMPATIBLE_VERSION: VersionInfo = {
  api: "v1", service: "1.3.0", buildCommit: "9f4c1ab", goVersion: "go1.27.0",
  engine: "1.68.2", configRevision: "cfg_9f4c1ab", ready: true, compatible: true
};

const INCOMPATIBLE_VERSION: VersionInfo = {
  ...COMPATIBLE_VERSION, service: "1.2.0", api: "v0", buildCommit: "a1b2c3d", compatible: false
};

const SET: BackupSet = {
  id: "set_test",
  source: "production",
  set: "postgres-primary",
  name: "Production PostgreSQL",
  host: "prod-db-01.internal",
  port: 22,
  username: "backup-agent",
  remoteFolder: "/backups/postgresql/",
  includePatterns: ["*.dump.zst"],
  excludePatterns: ["*.tmp"],
  completionMethod: "completion-marker",
  destination: "/data/backups/production/postgres/",
  retention: {
    daily: 7, weekly: 13, monthly: 12,
    timezone: "Europe/Berlin", weekStartsOn: "monday", protectLastKnownGood: true
  },
  validations: ["transfer", "checksum"],
  state: "healthy",
  stateNote: "Verified nightly dump.",
  enabled: true,
  halted: false,
  newestKnownGoodAt: "2026-08-29T02:01:01+02:00",
  lastRunAt: "2026-08-29T02:01:01+02:00",
  lastValidation: "passed",
  expectedIntervalHours: 24,
  retainedCount: 32,
  retainedBytes: 421 * 1024 ** 3,
  hostFingerprint: "SHA256:test-fingerprint",
  fingerprintTrustedAt: "2026-08-02T10:14:00+02:00"
};

const SET_2: BackupSet = { ...SET, id: "set_test_2", name: "Billing MySQL" };

/** Mirrors App.tsx's own wiring exactly (App.tsx, lines ~44/72): `readOnly`
 *  is `useCausl(readOnlyNode)`, threaded down to SettingsPage as a prop.
 *  Rendering it alongside SettingsPage (which reads versionNode directly)
 *  is what makes "both agree because they read the same commit" a testable
 *  claim, rather than an assertion about two components that merely
 *  happen to receive the same literal today. */
function SettingsHarness() {
  const readOnly = useCausl(readOnlyNode);
  return (
    <>
      <div data-testid="header-readonly-probe">{String(readOnly)}</div>
      <SettingsPage readOnly={readOnly} />
    </>
  );
}

/** B2.6 (#103) — SettingsPage's displayed version and App.tsx's `readOnly`
 *  derivation must always agree, because both read the one shared
 *  `versionNode` at the same commit. Before this issue, SettingsPage ran
 *  its own independent `useAsync(() => api.getVersion())`, a SECOND call
 *  to the same endpoint App.tsx already calls once for `readOnly` — so the
 *  two could disagree, briefly, whenever those two independent fetches
 *  resolved to different answers (a version bump landing between them, or
 *  just ordinary network timing). */
describe("SettingsPage reads the shared version node", () => {
  afterEach(() => {
    // Unmount BEFORE resetting the graph (PlatformContext.test.tsx's own
    // fix for this): resetting versionNode while SettingsPage is still
    // mounted commits it back to its initial state mid-render, which is a
    // test-isolation artifact, not the thing under test.
    cleanup();
    resetGraphForTests();
  });

  it("displays the version already committed to versionNode, with no independent getVersion() fetch", async () => {
    const getVersion = vi.fn(() =>
      Promise.reject(new Error("SettingsPage must not fetch its own version — it reads versionNode"))
    );
    const api = { ...createMockApi(), getVersion };

    act(() => {
      graph.commit("test/seed-version", (tx) =>
        tx.set(versionNode, { data: COMPATIBLE_VERSION, error: null, loading: false })
      );
    });

    render(
      <MemoryRouter>
        <ApiProvider api={api}>
          <PlatformProvider bridge={genericBridge}>
            <SettingsPage readOnly={false} />
          </PlatformProvider>
        </ApiProvider>
      </MemoryRouter>
    );
    await act(async () => {});

    expect(screen.getByText("9f4c1ab")).toBeTruthy();
    expect(getVersion).not.toHaveBeenCalled();
  });

  it("SettingsPage's version and the readOnly derivation observe the identical version at the same commit, and both update on a graph commit with no new fetch", async () => {
    const getVersion = vi.fn(() =>
      Promise.reject(new Error("must not be called — SettingsPage and App.tsx share versionNode"))
    );
    const api = { ...createMockApi(), getVersion };

    render(
      <MemoryRouter>
        <ApiProvider api={api}>
          <PlatformProvider bridge={genericBridge}>
            <SettingsHarness />
          </PlatformProvider>
        </ApiProvider>
      </MemoryRouter>
    );
    await act(async () => {});

    // versionNode has never resolved here — readOnly must not default to a
    // false confidence that happens to look like "compatible".
    expect(screen.getByTestId("header-readonly-probe").textContent).toBe("false");
    expect(screen.queryByText("1.2.0")).toBeNull();

    act(() => {
      graph.commit("test/seed-version-incompatible", (tx) =>
        tx.set(versionNode, { data: INCOMPATIBLE_VERSION, error: null, loading: false })
      );
    });

    // Same commit, two surfaces: the header-level readOnly flag flips to
    // true and SettingsPage's own "Service version" row shows the
    // incompatible version, in the same render pass — neither can show
    // one without the other, because both read versionNode, not their own
    // fetch of it.
    expect(screen.getByTestId("header-readonly-probe").textContent).toBe("true");
    // INCOMPATIBLE_VERSION deliberately gives service and core the same
    // value (mirrors mock.ts's own version-mismatch fixture), so both the
    // "Service version" and "Core version" rows legitimately show it.
    expect(screen.getAllByText("1.2.0").length).toBeGreaterThan(0);
    expect(getVersion).not.toHaveBeenCalled();
  });

  it("shows a loading placeholder instead of a blank panel while versionNode has never resolved", async () => {
    const api = createMockApi();

    render(
      <MemoryRouter>
        <ApiProvider api={api}>
          <PlatformProvider bridge={genericBridge}>
            <SettingsPage readOnly={false} />
          </PlatformProvider>
        </ApiProvider>
      </MemoryRouter>
    );
    await act(async () => {});

    expect(screen.getByText("Loading version information…")).toBeTruthy();
  });

  it("surfaces a versionNode fetch failure as an inline notice, not a silently blank panel", async () => {
    const api = createMockApi();
    const versionError = {
      code: "unknown" as const,
      message: "Backup Manager could not complete that request.",
      correlationId: "test-correlation-id"
    };

    act(() => {
      graph.commit("test/seed-version-error", (tx) =>
        tx.set(versionNode, { data: null, error: versionError, loading: false })
      );
    });

    render(
      <MemoryRouter>
        <ApiProvider api={api}>
          <PlatformProvider bridge={genericBridge}>
            <SettingsPage readOnly={false} />
          </PlatformProvider>
        </ApiProvider>
      </MemoryRouter>
    );
    await act(async () => {});

    expect(screen.getByText(/Version information is unavailable/)).toBeTruthy();
    expect(screen.queryByText("Loading version information…")).toBeNull();
  });
});

/** B2.6 (#103) — ActivityPage's "Backup set" filter reads the same shared
 *  `sets` node BackupSetsPage/DashboardPage/BackupsPage already read,
 *  instead of running a fifth independent `listSets()` fetch just to
 *  populate a dropdown. */
describe("ActivityPage reads the shared sets node", () => {
  afterEach(() => {
    // Unmount BEFORE resetting the graph (PlatformContext.test.tsx's own
    // fix for this): resetting versionNode while SettingsPage is still
    // mounted commits it back to its initial state mid-render, which is a
    // test-isolation artifact, not the thing under test.
    cleanup();
    resetGraphForTests();
  });

  it("populates the backup-set filter from setsNode, with no independent listSets() fetch", async () => {
    const listSets = vi.fn(() =>
      Promise.reject(new Error("ActivityPage must not fetch its own sets list — it reads setsNode"))
    );
    const api = { ...createMockApi(), listSets, listActivity: () => Promise.resolve([]) };

    act(() => {
      graph.commit("test/seed-sets", (tx) =>
        tx.set(setsNode, { data: [SET], error: null, loading: false })
      );
    });

    render(
      <MemoryRouter>
        <ApiProvider api={api}>
          <ActivityPage />
        </ApiProvider>
      </MemoryRouter>
    );
    await act(async () => {});

    const select = screen.getByLabelText("Backup set");
    expect(within(select).getByRole("option", { name: SET.name })).toBeTruthy();
    expect(listSets).not.toHaveBeenCalled();
  });

  it("updates the backup-set filter options when setsNode changes, with no new fetch from the page", async () => {
    const listSets = vi.fn(() => Promise.reject(new Error("must not be called")));
    const api = { ...createMockApi(), listSets, listActivity: () => Promise.resolve([]) };

    render(
      <MemoryRouter>
        <ApiProvider api={api}>
          <ActivityPage />
        </ApiProvider>
      </MemoryRouter>
    );
    await act(async () => {});

    const select = screen.getByLabelText("Backup set");
    // setsNode has never resolved yet — only the "All backup sets" default.
    expect(within(select).getAllByRole("option").length).toBe(1);

    act(() => {
      graph.commit("test/seed-sets", (tx) =>
        tx.set(setsNode, { data: [SET, SET_2], error: null, loading: false })
      );
    });

    expect(within(select).getByRole("option", { name: SET.name })).toBeTruthy();
    expect(within(select).getByRole("option", { name: SET_2.name })).toBeTruthy();
    expect(listSets).not.toHaveBeenCalled();
  });
});
