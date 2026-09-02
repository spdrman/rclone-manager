import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupManagerApi } from "@shared/api/contracts";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import type { BackupSet } from "@shared/types/backup";
import { resetGraphForTests } from "@shared/state/graph";
import { backupSetPath } from "@shared/utilities/routes";

function renderDetail(source: string, set: string, api: BackupManagerApi, readOnly = false) {
  return render(
    <MemoryRouter initialEntries={[backupSetPath(source, set)]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={readOnly} />} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

async function firstSet(): Promise<BackupSet> {
  return (await createMockApi().listSets())[0];
}

async function openEditMode(api: BackupManagerApi, target: BackupSet) {
  renderDetail(target.source, target.set, api);
  await screen.findByText(target.name);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  });
  await screen.findByRole("button", { name: "SAVE ALL & EXIT EDIT" });
}

/**
 * Issue #333's UI half. The rule this is drawn under is #299's: a field
 * nothing reads must not be drawn, which is why per-set retention was
 * REMOVED from the wizard once and is only being added back now that
 * there is a config key, a service method, a CLI verb and a route behind
 * it.
 */
describe("issue #333: a backup set's own retention policy on its detail page", () => {
  afterEach(() => {
    resetGraphForTests();
    resetMockFixtures();
    vi.restoreAllMocks();
  });

  it("says the set inherits, and shows the chain it is actually retained under", async () => {
    const api = createMockApi();
    const target = await firstSet();
    renderDetail(target.source, target.set, api);
    await screen.findByText(target.name);

    await screen.findByText(/retained under the deployment's policy/i);
    // The RESOLVED chain, not a raw block: the deployment fixture's own
    // three tiers, each named with what it keeps.
    await waitFor(() => expect(screen.getByText(/daily · keep 7 day/)).toBeTruthy());
    expect(screen.getByText(/weekly · keep 3 month/)).toBeTruthy();
  });

  it("draws no retention controls outside edit mode", async () => {
    const api = createMockApi();
    const target = await firstSet();
    renderDetail(target.source, target.set, api);
    await screen.findByText(target.name);
    await screen.findByText(/retained under the deployment's policy/i);

    expect(screen.queryByRole("button", { name: "Give this set its own policy" })).toBeNull();
  });

  it("gives the set its own policy, seeded from the deployment's, and then says so", async () => {
    const api = createMockApi();
    const set = vi.spyOn(api, "setBackupSetRetention");
    const target = await firstSet();
    await openEditMode(api, target);

    const declare = await screen.findByRole("button", { name: "Give this set its own policy" });
    await act(async () => {
      fireEvent.click(declare);
    });

    // Seeded from the deployment's chain, so nothing about what is
    // retained changes on the day the override is declared.
    expect(set).toHaveBeenCalledTimes(1);
    const sent = set.mock.calls[0][2];
    expect(sent.tiers.map((t) => t.name)).toEqual(["daily", "weekly", "monthly"]);
    expect(sent.timezone).toBe("Europe/Berlin");

    await screen.findByText(/retained under its own policy/i);
    expect(screen.queryByRole("button", { name: "Give this set its own policy" })).toBeNull();
  });

  it("edits the set's own chain through one whole-policy save", async () => {
    const api = createMockApi();
    const target = await firstSet();
    await openEditMode(api, target);
    await act(async () => {
      fireEvent.click(await screen.findByRole("button", { name: "Give this set its own policy" }));
    });
    await screen.findByText(/retained under its own policy/i);

    const set = vi.spyOn(api, "setBackupSetRetention");
    const keeps = await screen.findAllByLabelText("Keep");
    fireEvent.change(keeps[0], { target: { value: "30" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save this set's retention policy" }));
    });

    expect(set).toHaveBeenCalledTimes(1);
    // The WHOLE chain, not the one tier that changed: an override names a
    // whole policy or it does not exist, and a partial one would resolve
    // its missing half to the product default rather than to the
    // deployment's.
    const sent = set.mock.calls[0][2];
    expect(sent.tiers).toHaveLength(3);
    expect(sent.tiers[0].keep).toBe(30);
    expect(sent.tiers[2].name).toBe("monthly");
    await waitFor(() => expect(screen.getByText(/daily · keep 30 day/)).toBeTruthy());
  });

  it("puts the set back on the deployment's policy", async () => {
    const api = createMockApi();
    const target = await firstSet();
    await openEditMode(api, target);
    await act(async () => {
      fireEvent.click(await screen.findByRole("button", { name: "Give this set its own policy" }));
    });
    await screen.findByText(/retained under its own policy/i);

    const clear = vi.spyOn(api, "clearBackupSetRetention");
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Use the deployment’s policy" }));
    });

    expect(clear).toHaveBeenCalledTimes(1);
    await screen.findByText(/retained under the deployment's policy/i);
  });

  it("says what failed and changes nothing when the write is refused", async () => {
    const api = createMockApi();
    vi.spyOn(api, "setBackupSetRetention").mockRejectedValue(
      new Error("boom") as unknown as never
    );
    const target = await firstSet();
    await openEditMode(api, target);

    await act(async () => {
      fireEvent.click(await screen.findByRole("button", { name: "Give this set its own policy" }));
    });

    await screen.findByText("Could not change this set's retention policy");
    // Still inheriting, and the affordance is still there to try again:
    // a failed write that left the page claiming an override would be
    // reporting a policy nothing is retained under.
    expect(screen.getByText(/retained under the deployment's policy/i)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Give this set its own policy" })).toBeTruthy();
  });
});
