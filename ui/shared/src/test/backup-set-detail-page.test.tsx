import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupManagerApi } from "@shared/api/contracts";
import { BackupManagerError } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import type { BackupSet } from "@shared/types/backup";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { currentSetDetailNode } from "@shared/state/backupSetDetailNodes";
import { backupSetPath } from "@shared/utilities/routes";

// Two segments (source, set), not one flat id: a real backup set id
// (model.BackupSetID.String(), core/internal/model/ids.go) is the two
// joined by "/", and the route matches that shape (issue #285).
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

// B2.2 (#97) — this page's own useAsync(() => api.getSet(setId)) and
// useAsync(() => api.listActivity()) moved onto the shared graph
// (currentSetDetailNode / currentSetActivityNode, state/backupSetDetailNodes.ts)
// so an edit form opened against this page has something real — a value
// read off the graph at a given commit — to check for staleness against,
// instead of a plain object sitting in useAsync's opaque `data` field.
describe("backup set detail page reads the set", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("fetches the set for the given id", async () => {
    const api = createMockApi();
    const sets = await createMockApi().listSets();
    const target = sets[0];

    renderDetail(target.source, target.set, api);

    await screen.findByText(target.name);
  });

  it("shows an error state when the fetch fails, never a blank page", async () => {
    const api = createMockApi();
    vi.spyOn(api, "getSet").mockRejectedValue(
      new BackupManagerError({ code: "unknown", message: "That backup set no longer exists.", correlationId: "cid_test" })
    );

    renderDetail("does-not-exist", "does-not-exist", api);

    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByText("That backup set no longer exists.")).toBeTruthy();
  });

  // Same shape as B2.4's mandatory-review fix for BackupDetailPage
  // (backup-detail-page.test.tsx): React Router does not remount this
  // component for a route-param change alone, so a fetch for set B can
  // start while set A's fields are still on screen. The fix there was
  // gating render on `loading` as well as `data`; this page needs the
  // same gate now that it reads a shared graph node instead of its own
  // page-local useAsync state.
  it("does not render set A's fields under set B's url while B is still loading (no unmount)", async () => {
    const api = createMockApi();
    const sets = await createMockApi().listSets();
    const [first, second] = sets;
    const resolvers: Record<string, () => void> = {};
    vi.spyOn(api, "getSet").mockImplementation(
      (id) =>
        new Promise<BackupSet>((resolve) => {
          resolvers[id] = () => resolve(sets.find((s) => s.id === id) ?? first);
        })
    );

    function Harness() {
      const navigate = useNavigate();
      return (
        <>
          <button onClick={() => navigate(backupSetPath(second.source, second.set))}>go to second</button>
          <Routes>
            <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={false} />} />
          </Routes>
        </>
      );
    }

    render(
      <MemoryRouter initialEntries={[backupSetPath(first.source, first.set)]}>
        <ApiProvider api={api}>
          <Harness />
        </ApiProvider>
      </MemoryRouter>
    );

    resolvers[first.id]();
    await screen.findByText(first.name);

    fireEvent.click(screen.getByText("go to second"));

    expect(screen.queryByText(first.name)).toBeNull();

    resolvers[second.id]();
    await screen.findByText(second.name);
  });
});

describe("editing a backup set (#97 acceptance: 'stale edits are rejected')", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("opens an edit form prefilled with the current set's name", async () => {
    const api = createMockApi();
    const sets = await createMockApi().listSets();
    const target = sets[0];

    renderDetail(target.source, target.set, api);
    await screen.findByText(target.name);

    fireEvent.click(screen.getByRole("button", { name: "Edit" }));

    expect(await screen.findByRole("dialog", { name: "Edit backup set" })).toBeTruthy();
    expect(screen.getByLabelText("Name")).toHaveValue(target.name);
  });

  it("GIVEN the edit form is open, WHEN another commit updates that same set before submit, THEN the submit is rejected as stale rather than silently overwriting", async () => {
    const api = createMockApi();
    const sets = await createMockApi().listSets();
    const target = sets[0];

    renderDetail(target.source, target.set, api);
    await screen.findByText(target.name);

    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    await screen.findByRole("dialog", { name: "Edit backup set" });

    // Someone else's commit lands on the graph before this form submits —
    // the same shape as a concurrent editor saving first.
    const changed: BackupSet = { ...target, name: target.name + " (renamed elsewhere)" };
    graph.commit("test/concurrent-set-edit", (tx) =>
      tx.set(currentSetDetailNode, { data: changed, error: null, loading: false })
    );

    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    expect(await screen.findByText(/changed since you opened this form/i)).toBeTruthy();
    // Rejected, not silently overwritten: the graph still holds the value
    // the concurrent commit set, not anything this form tried to save.
    expect(graph.read(currentSetDetailNode).data?.name).toBe(changed.name);
  });

  it("does not falsely reject a submit as stale when nothing else has changed", async () => {
    const api = createMockApi();
    const sets = await createMockApi().listSets();
    const target = sets[0];

    renderDetail(target.source, target.set, api);
    await screen.findByText(target.name);

    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    await screen.findByRole("dialog", { name: "Edit backup set" });

    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    expect(screen.queryByText(/changed since you opened this form/i)).toBeNull();
    // No backend endpoint exists yet to persist a backup-set edit (#146) —
    // the honest outcome of a non-stale submit is a clear "not saved"
    // notice, never a silent no-op that looks like success.
    expect(await screen.findByText(/doesn.t yet support saving/i)).toBeTruthy();
  });
});
