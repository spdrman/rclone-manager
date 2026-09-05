import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { httpApi } from "@shared/api/client";
import { DashboardPage } from "@shared/pages/DashboardPage";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { operationsNode } from "@shared/state/appNodes";
import type { Operation, SystemHealth } from "@shared/types/operation";

/**
 * FR-30's moves on the Dashboard (EPIC E).
 *
 * A deployment where every single move is refused, which is what one
 * unset credential or one bucket that is not there produces, ran a cycle
 * that completed, backed everything up, and left every artifact somewhere
 * other than where the operator's retention chain says it belongs. On this
 * page that cycle was indistinguishable from a perfect one, and the only
 * record of the refusal was a line in a log.
 *
 * That is issue #361's shape one layer up, so the panel that already
 * exists for #361 is where it goes, in the same words: a denominator, a
 * numerator, and a sentence.
 */

const HEALTH: SystemHealth = {
  generatedAt: "2026-08-29T06:00:00Z",
  serviceRunning: true,
  backupHealth: "healthy",
  backupHealthReason: "every set is fresh",
  newestVerifiedBackupAt: "2026-08-29T02:00:00Z",
  lastCompletedBackupAt: "2026-08-29T02:00:00Z",
  oldestSetFreshnessHours: 4,
  setsHealthy: 1, setsDegraded: 0, setsStale: 0, setsFailing: 0,
  quarantinedCount: 0, readOnlyRetainedCount: 0,
  storageFreeBytes: 1e12, storageTotalBytes: 4e12,
  storageState: "nominal", storageReadingsUnavailable: 0
};

function operation(over: Partial<Operation> = {}): Operation {
  return {
    id: "op_1", setId: "", setName: "All backup sets",
    kind: "transfer", label: "run cycle", status: "completed",
    progress: null, nonDestructive: false, startedAt: "2026-08-29T01:00:00Z",
    cycle: null,
    ...over
  };
}

async function renderDashboard(operations: Operation[]) {
  const api = createMockApi();
  const sets = await api.listSets();
  act(() => {
    graph.commit("test/seed", (tx) => {
      tx.set(operationsNode, { data: operations, error: null, loading: false });
    });
  });
  render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <DashboardPage
          readOnly={false}
          health={{ data: HEALTH, error: null, loading: false, reload: () => {} }}
          sets={{ data: sets, error: null, loading: false, reload: () => {} }}
        />
      </ApiProvider>
    </MemoryRouter>
  );
  await act(async () => {});
}

// A cycle whose ingestion was flawless. Everything this block asserts is
// therefore about the moves and nothing else, which is the whole danger:
// on the counts that already existed, this cycle is perfect.
const PERFECT_INGESTION = { backupSetsProcessed: 3, artifactsWalked: 12, artifactsThrough: 12 };

describe("what the last run cycle moved", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  it("says so when artifacts were due to move and none arrived", async () => {
    await renderDashboard([
      operation({ cycle: { ...PERFECT_INGESTION, moves: { attempted: 4, landed: 0 } } })
    ]);

    const panel = screen.getByRole("region", { name: "Last run cycle" });
    expect(within(panel).getByText("Nothing moved")).toBeTruthy();
    expect(
      within(panel).getByText(/4 backups were due to move to the medium their retention tier names, and none arrived/i)
    ).toBeTruthy();
  });

  it("says so when some arrived and some did not", async () => {
    await renderDashboard([
      operation({ cycle: { ...PERFECT_INGESTION, moves: { attempted: 4, landed: 1 } } })
    ]);

    const panel = screen.getByRole("region", { name: "Last run cycle" });
    expect(within(panel).getByText(/1 of them arrived/i)).toBeTruthy();
    expect(within(panel).queryByText("Nothing moved")).toBeNull();
  });

  // The control for the two above. Without it, a panel that had started
  // shouting on every cycle would pass both of them.
  it("says nothing alarming when every move arrived", async () => {
    await renderDashboard([
      operation({ cycle: { ...PERFECT_INGESTION, moves: { attempted: 4, landed: 4 } } })
    ]);

    const panel = screen.getByRole("region", { name: "Last run cycle" });
    expect(within(panel).queryByText("Nothing moved")).toBeNull();
    expect(within(panel).getByText(/all 4 arrived/i)).toBeTruthy();
  });

  // FR-35's compatibility promise on this page. Every deployment written
  // before EPIC E has no medium, so no cycle of theirs ever has anything
  // to move, and a permanent "0 of 0 moved" row on their dashboard would
  // be a new thing to explain that says nothing.
  it("renders no move row at all for a deployment with nothing to move", async () => {
    await renderDashboard([
      operation({ cycle: { ...PERFECT_INGESTION, moves: { attempted: 0, landed: 0 } } })
    ]);

    const panel = screen.getByRole("region", { name: "Last run cycle" });
    expect(within(panel).queryByText(/due to move/i)).toBeNull();
    expect(within(panel).queryByText(/arrived/i)).toBeNull();
    // The control: the panel itself is rendering, so the queries above
    // are looking at something.
    expect(within(panel).getByText("All through")).toBeTruthy();
  });

  // The absent case, which is a different answer from a pair of zeroes: a
  // cycle recorded by a build that did not write these counts has not
  // moved nothing, it has not said. Drawing zeroes for it would report
  // the worst outcome this panel can express about a cycle nobody
  // measured.
  it("renders no move row for a cycle whose summary predates the counts", async () => {
    await renderDashboard([operation({ cycle: { ...PERFECT_INGESTION, moves: null } })]);

    const panel = screen.getByRole("region", { name: "Last run cycle" });
    expect(within(panel).queryByText(/due to move/i)).toBeNull();
    expect(within(panel).queryByText("Nothing moved")).toBeNull();
    expect(within(panel).getByText("All through")).toBeTruthy();
  });
});

/**
 * The hop the panel tests above cannot see.
 *
 * Every one of them builds an Operation by hand, so a client that never
 * read `cycle.moves` off the wire, or helpfully filled in a pair of
 * zeroes for an absent one, would leave all five of them green while the
 * real page rendered nothing (or the wrong thing) against a real server.
 */
describe("reading the move outcome off the wire", () => {
  afterEach(() => vi.unstubAllGlobals());

  function respondWith(cycle: unknown) {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({ operations: [{ operation_id: "op_1", status: "completed", action: "run_cycle", cycle }] }),
            { status: 200, headers: { "Content-Type": "application/json" } }
          )
      )
    );
  }

  it("carries the counts a completed cycle reported", async () => {
    respondWith({
      backup_sets_processed: 2, artifacts_walked: 6, artifacts_through: 6,
      moves: { attempted: 4, landed: 1 }
    });

    const [op] = await httpApi.listOperations();
    expect(op.cycle?.moves).toEqual({ attempted: 4, landed: 1 });
  });

  it("leaves an absent pair absent rather than filling in zeroes", async () => {
    respondWith({ backup_sets_processed: 2, artifacts_walked: 6, artifacts_through: 6 });

    const [op] = await httpApi.listOperations();
    expect(op.cycle?.moves).toBeNull();
    // The control: the counts the response DID carry came through, so
    // this is not passing against a cycle that failed to parse at all.
    expect(op.cycle?.artifactsWalked).toBe(6);
  });
});
