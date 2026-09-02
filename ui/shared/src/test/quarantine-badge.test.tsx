import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { QuarantinePage } from "@shared/pages/QuarantinePage";
import { AppShell } from "@shared/layouts/AppShell";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { graph, resetGraphForTests, useCausl } from "@shared/state/graph";
import { countsNode, quarantineNode } from "@shared/state/appNodes";
import { useResource } from "@shared/state/resource";
import type { BackupManagerApi } from "@shared/api/contracts";
import type { BackupArtifact } from "@shared/types/backup";

function artifact(id: string, filename: string): BackupArtifact {
  return {
    id,
    setId: "set_test",
    setName: "Production PostgreSQL",
    filename,
    remoteOriginalPath: "/backups/postgresql/" + filename,
    localPath: "/data/backups/production/postgres/" + filename,
    producedAt: "2026-08-28T02:00:00+02:00",
    receivedAt: "2026-08-28T02:05:00+02:00",
    sizeBytes: 1024,
    checksum: "deadbeef",
    checksumAlgorithm: "sha256",
    validation: "failed",
    retentionClasses: [],
    remoteSourceRemovedAt: null,
    quarantine: {
      reason: "checksum-mismatch",
      detail: "sha256 mismatch: local file hashes to deadbeef, remote reports feedface",
      detectedAt: "2026-08-28T02:06:00+02:00",
      remoteSourceRetained: true
    }
  };
}

const ARTIFACT_A = artifact("art_a", "pg-2026-08-28.dump.zst");
const ARTIFACT_B = artifact("art_b", "pg-2026-08-27.dump.zst");

/** Wires QuarantinePage and the app shell's badge exactly the way App.tsx
 *  does (or is meant to, per #101): ONE useResource(quarantineNode, ...)
 *  call, its return handed to QuarantinePage as the `quarantine` prop
 *  (the same shape `sets`/`health` already use), while the badge reads
 *  `countsNode` — a pure derived() of that same node. There is
 *  structurally one fetch and one committed value feeding both; this
 *  harness is what proves that, rather than asserting it by inspection. */
function Shell({ api }: { api: BackupManagerApi }) {
  const quarantine = useResource(quarantineNode, () => api.listQuarantine(), [api]);
  const counts = useCausl(countsNode);
  return (
    <AppShell
      health={null}
      version={null}
      counts={counts}
      theme="light"
      onToggleTheme={() => {}}
      onSignOut={() => {}}
    >
      <QuarantinePage readOnly={false} quarantine={quarantine} />
    </AppShell>
  );
}

function renderShell(api: BackupManagerApi) {
  return render(
    <MemoryRouter>
      <PlatformProvider bridge={genericBridge}>
        <ApiProvider api={api}>
          <Shell api={api} />
        </ApiProvider>
      </PlatformProvider>
    </MemoryRouter>
  );
}

function quarantineBadge() {
  return within(screen.getByRole("link", { name: /Quarantine/ }));
}

function dataRowCount() {
  // getAllByRole("row") includes the header row.
  return screen.getAllByRole("row").length - 1;
}

/** #101 — App.tsx fetched quarantine data once, purely to compute the
 *  sidebar's badge count, while QuarantinePage ran its own separate,
 *  uncoordinated `api.listQuarantine()` via `useAsync`. Two independent
 *  reads of the same resource can disagree: if one reloads and the other
 *  doesn't, the badge can say one number while the page shows a different
 *  set of rows underneath it. These tests pin the fix: QuarantinePage
 *  reads the exact same `quarantineNode` App.tsx already fetches, so the
 *  two literally cannot drift. */
describe("quarantineNode: badge and list agree because they read the same node", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  it("reflects a direct commit to quarantineNode in both the list and the badge, with no fetch involved at all", async () => {
    // A promise that never resolves: if either the badge or the list were
    // still fed by their own private fetch, neither would ever show
    // anything but its loading/empty state.
    const listQuarantine = vi.fn(() => new Promise<BackupArtifact[]>(() => {}));
    const api = { ...createMockApi(), listQuarantine };
    renderShell(api);
    await act(async () => {});

    act(() => {
      graph.commit("test/seed-quarantine", (tx) =>
        tx.set(quarantineNode, { data: [ARTIFACT_A, ARTIFACT_B], error: null, loading: false })
      );
    });

    expect(dataRowCount()).toBe(2);
    expect(quarantineBadge().getByText("2")).toBeTruthy();
  });

  it("shows the same count in the sidebar badge as there are rows in the list once quarantineNode resolves", async () => {
    const api = { ...createMockApi(), listQuarantine: () => Promise.resolve([ARTIFACT_A, ARTIFACT_B]) };
    renderShell(api);
    await act(async () => {});

    expect(dataRowCount()).toBe(2);
    expect(quarantineBadge().getByText("2")).toBeTruthy();
  });

  it("updates the badge in the same render as the list when an artifact is resolved from the page, off the ONE shared reload — never a second, page-private fetch", async () => {
    let items = [ARTIFACT_A, ARTIFACT_B];
    const listQuarantine = vi.fn(() => Promise.resolve(items));
    const revalidate = vi.fn((id: string) => {
      items = items.filter((a) => a.id !== id);
      return Promise.resolve();
    });
    const api = { ...createMockApi(), listQuarantine, revalidate };

    renderShell(api);
    await act(async () => {});

    expect(dataRowCount()).toBe(2);
    expect(quarantineBadge().getByText("2")).toBeTruthy();

    const user = userEvent.setup();
    const targetRow = screen.getAllByRole("row")[1];
    await act(async () => {
      await user.click(within(targetRow).getByRole("button", { name: "Revalidate" }));
    });

    expect(revalidate).toHaveBeenCalledWith(ARTIFACT_A.id);
    // One mount fetch (Shell's useResource) plus one reload after the
    // action — never a THIRD call from a page-private fetch racing it.
    expect(listQuarantine).toHaveBeenCalledTimes(2);
    expect(dataRowCount()).toBe(1);
    expect(quarantineBadge().getByText("1")).toBeTruthy();
    expect(quarantineBadge().queryByText("2")).toBeNull();
  });
});
