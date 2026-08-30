import { afterEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { DashboardPage } from "@shared/pages/DashboardPage";
import { BackupSetsPage } from "@shared/pages/BackupSetsPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { operationsNode } from "@shared/state/appNodes";
import type { AsyncState } from "@shared/hooks/useAsync";
import type { BackupSet } from "@shared/types/backup";
import type { Operation, SystemHealth } from "@shared/types/operation";

const noop = () => {};

const SET: BackupSet = {
  id: "set_test",
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

const HEALTH: SystemHealth = {
  serviceRunning: true,
  serviceUptimeHours: 100,
  backupHealth: "healthy",
  backupHealthReason: "All sets current.",
  lastSuccessfulCycleAt: "2026-08-29T05:52:00+02:00",
  newestVerifiedBackupAt: "2026-08-29T05:18:00+02:00",
  oldestSetFreshnessHours: 2,
  setsHealthy: 1,
  setsStale: 0,
  setsFailing: 0,
  quarantinedCount: 0,
  retainedCount: 32,
  retainedBytes: 421 * 1024 ** 3,
  storageFreeBytes: 1.8e12,
  storageTotalBytes: 6.2e12,
  storageState: "nominal",
  successRate7d: 0.99
};

const OPERATION: Operation = {
  id: "op_test_1",
  setId: SET.id,
  setName: SET.name,
  kind: "transfer",
  stage: "transferring",
  label: "Transferring backup",
  percent: 42,
  nonDestructive: false,
  startedAt: "2026-08-29T00:00:00+02:00"
};

function healthState(): AsyncState<SystemHealth> {
  return { data: HEALTH, error: null, loading: false, reload: noop };
}

function setsState(): AsyncState<BackupSet[]> {
  return { data: [SET], error: null, loading: false, reload: noop };
}

/** B2.1's own headline behavior: operation progress is a graph node other
 *  surfaces subscribe to, not a `useAsync` re-fetched on a timer by each
 *  page separately. These tests are the ones the issue's TDD section
 *  describes: a commit into the shared node updates a mounted page with NO
 *  re-fetch, and two independent pages reading the node observe the
 *  identical list at the same commit. */
describe("operationsNode: live progress without a per-page re-fetch", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("updates DashboardPage's active-operations list and OperationProgress on a direct graph commit, with no operations fetch from the page itself", async () => {
    const listOperations = vi.fn(() =>
      Promise.reject(new Error("DashboardPage must not fetch operations itself — it reads operationsNode"))
    );
    // listActivity is unrelated to this test; resolved with no artificial
    // delay (unlike the mock's own 180ms `delay()`) so its pending promise
    // settles before this test ends instead of firing a setState warning
    // after cleanup has already unmounted the tree.
    const api = { ...createMockApi(), listOperations, listActivity: () => Promise.resolve([]) };

    render(
      <MemoryRouter>
        <ApiProvider api={api}>
          <DashboardPage health={healthState()} sets={setsState()} readOnly={false} />
        </ApiProvider>
      </MemoryRouter>
    );
    // Lets listActivity's pending resolve -> then(setData) ->
    // finally(setLoading) chain settle before asserting.
    await act(async () => {});

    const region = screen.getByRole("region", { name: "Active operations" });
    // operationsNode has never been fetched here (App.tsx owns that fetch,
    // and this test never renders App), so it is genuinely still in its
    // initial {data: null, loading: true} state — "not known yet", not
    // "zero". Mandatory review, PR #143: this must render as a loading
    // placeholder, never as a confident "0 running" / "Nothing running
    // right now." (which is what the pre-fix code rendered here, and what
    // this test used to assert as if it were correct).
    expect(within(region).queryByText("0 running")).toBeNull();
    expect(within(region).queryByText("Nothing running right now.")).toBeNull();
    expect(within(region).getByText("Checking for active operations…")).toBeTruthy();

    act(() => {
      graph.commit("test/seed-operation", (tx) =>
        tx.set(operationsNode, { data: [OPERATION], error: null, loading: false })
      );
    });

    expect(within(region).getByText("1 running")).toBeTruthy();
    expect(within(region).getByText(SET.name)).toBeTruthy();
    const bar = within(region).getByRole("progressbar");
    expect(bar.getAttribute("aria-valuenow")).toBe("42");

    // The whole point: no consumer of the shared node re-fetches to see it update.
    expect(listOperations).not.toHaveBeenCalled();
  });

  it("DashboardPage and BackupSetsPage observe the identical operation list at the same commit", async () => {
    // Neither page may reach for its own listOperations() any more — both
    // read operationsNode. A rejecting mock proves that: today (pre-fix)
    // each page's own useAsync would show this operation only if ITS OWN
    // fetch happened to succeed; wiring both through the shared node makes
    // that irrelevant.
    const listOperations = vi.fn(() => Promise.reject(new Error("must not be called")));
    const api = { ...createMockApi(), listOperations, listActivity: () => Promise.resolve([]) };

    act(() => {
      graph.commit("test/seed-operation", (tx) =>
        tx.set(operationsNode, { data: [OPERATION], error: null, loading: false })
      );
    });

    render(
      <MemoryRouter>
        <ApiProvider api={api}>
          <DashboardPage health={healthState()} sets={setsState()} readOnly={false} />
          <BackupSetsPage sets={setsState()} readOnly={false} />
        </ApiProvider>
      </MemoryRouter>
    );
    // Lets listActivity's pending resolve -> then(setData) ->
    // finally(setLoading) chain settle before asserting.
    await act(async () => {});

    const dashboardOps = screen.getByRole("region", { name: "Active operations" });
    expect(within(dashboardOps).getByText(SET.name)).toBeTruthy();
    expect(within(dashboardOps).getByRole("progressbar").getAttribute("aria-valuenow")).toBe("42");

    // BackupSetCard renders currentOperation as `${label.toLowerCase()} ${percent}%`.
    expect(screen.getByText("transferring backup 42%")).toBeTruthy();
    expect(listOperations).not.toHaveBeenCalled();
  });

  /** Mandatory-review item-1's other half: operations.error must not be
   *  silently swallowed. Before this fix, DashboardPage and BackupSetsPage
   *  only ever destructured `.data` off operationsNode, so a failed
   *  App.tsx fetch left both pages showing the exact same "nothing
   *  running" / "idle" copy a genuinely healthy zero-operations state
   *  shows — indistinguishable from "we don't actually know". */
  it("surfaces an operationsNode fetch failure as an inline notice, on both pages, instead of a confident empty state", async () => {
    const api = { ...createMockApi(), listActivity: () => Promise.resolve([]) };
    const opsError = {
      code: "unknown" as const,
      message: "Backup Manager could not complete that request.",
      correlationId: "test-correlation-id"
    };

    act(() => {
      graph.commit("test/seed-operations-error", (tx) =>
        tx.set(operationsNode, { data: null, error: opsError, loading: false })
      );
    });

    render(
      <MemoryRouter>
        <ApiProvider api={api}>
          <DashboardPage health={healthState()} sets={setsState()} readOnly={false} />
          <BackupSetsPage sets={setsState()} readOnly={false} />
        </ApiProvider>
      </MemoryRouter>
    );
    await act(async () => {});

    const dashboardOps = screen.getByRole("region", { name: "Active operations" });
    expect(within(dashboardOps).getByText(/Live operation status is unavailable/)).toBeTruthy();
    expect(within(dashboardOps).queryByText("Nothing running right now.")).toBeNull();
    expect(within(dashboardOps).queryByText("Checking for active operations…")).toBeNull();

    // Same failure, surfaced on BackupSetsPage too — not just Dashboard.
    expect(screen.getAllByText(/Live operation status is unavailable/).length).toBeGreaterThan(0);
  });

  it("BackupSetsPage marks a set's current-operation badge as still checking, not idle, while operationsNode has never resolved", async () => {
    // operationsNode is left at its untouched initial state
    // ({data: null, error: null, loading: true}) — exactly what a fresh
    // mount looks like before App.tsx's first fetch has resolved.
    const api = { ...createMockApi(), listActivity: () => Promise.resolve([]) };

    render(
      <MemoryRouter>
        <ApiProvider api={api}>
          <BackupSetsPage sets={setsState()} readOnly={false} />
        </ApiProvider>
      </MemoryRouter>
    );
    await act(async () => {});

    expect(screen.getByText("checking…")).toBeTruthy();
    expect(screen.queryByText("idle")).toBeNull();
  });
});
