import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupManagerApi } from "@shared/api/contracts";
import { BackupManagerError } from "@shared/api/contracts";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import type { BackupSet } from "@shared/types/backup";
import { resetGraphForTests } from "@shared/state/graph";
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

// #97's acceptance criterion, "stale edits are rejected", re-pointed at
// the surface it now lives on. Issue #350 replaced the edit dialog with
// an inline mode on this page, and the criterion did not go away with it:
// inline editing holds the page open LONGER than a dialog did, so it is
// more exposed to a concurrent save landing first, not less.
//
// The positive case (a concurrent commit is refused) lives in
// backup-set-inline-edit.test.tsx beside the rest of the mode. What is
// here is the two ways this check can be wrong in the other direction,
// each of which turns a working editor into one that refuses everything.
describe("editing a backup set (#97 acceptance: 'stale edits are rejected')", () => {
  afterEach(() => {
    resetGraphForTests();
    resetMockFixtures();
    vi.restoreAllMocks();
  });

  async function enterEditMode(api: BackupManagerApi, target: BackupSet) {
    renderDetail(target.source, target.set, api);
    await screen.findByText(target.name);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    });
    await screen.findByRole("button", { name: "SAVE ALL & EXIT EDIT" });
  }

  it("does not falsely reject a save as stale when nothing else has changed", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet");
    const target = (await createMockApi().listSets())[0];
    await enterEditMode(api, target);

    fireEvent.change(screen.getByLabelText("Host"), { target: { value: "first-change.internal" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save host" }));
    });

    expect(screen.queryByText(/changed since you opened/i)).toBeNull();
    expect(update).toHaveBeenCalledTimes(1);
  });

  // The trap this page walks straight into if the staleness snapshot is
  // taken only once, when edit mode opens. A successful per-box Save puts
  // the persisted set back on the very node isSetEditStale watches, which
  // bumps the version counter it compares against, so the SECOND save of
  // a session would report a concurrent edit that never happened and
  // every box after the first would be unsavable.
  it("does not report a concurrent edit for the second per-box save of a session", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet");
    const target = (await createMockApi().listSets())[0];
    await enterEditMode(api, target);

    fireEvent.change(screen.getByLabelText("Host"), { target: { value: "first.internal" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save host" }));
    });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Save host" }).textContent).toBe("Save")
    );

    fireEvent.change(screen.getByLabelText("User"), { target: { value: "second-user" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save user" }));
    });

    expect(screen.queryByText(/changed since you opened/i)).toBeNull();
    expect(update).toHaveBeenCalledTimes(2);
    expect(update.mock.calls[1][2]).toEqual({ username: "second-user" });
  });
});

// Issue #316's RED case for this page: before this control existed,
// there was no way to declare an already-persisted backup set read-only
// (or withdraw it) anywhere in the UI — only by hand-editing config.yaml.
describe("declaring a backup set read-only (issue #316)", () => {
  afterEach(() => {
    resetGraphForTests();
    // The mock's setReadOnly used to resolve and change nothing, so the
    // first test here could flip a set read-only and the second could
    // still go looking for "the mock's one read-only set" and find the
    // one the literal declares. It applies the change now (mock.ts says
    // why), which makes these two tests share state unless the fixture
    // is put back, exactly as resetMockFixtures' own doc requires of any
    // test that drives a mutating method.
    resetMockFixtures();
  });

  it("shows 'No' for a set that is not read-only, and flips it on with a call to api.setReadOnly", async () => {
    const api = createMockApi();
    const sets = await createMockApi().listSets();
    // The fixture's first set (postgres-primary) is not read-only —
    // see mock.ts's own SETS literal.
    const target = sets[0];
    const spy = vi.spyOn(api, "setReadOnly");

    renderDetail(target.source, target.set, api);
    await screen.findByText(target.name);

    expect(screen.getByText("No")).toBeTruthy();
    const toggle = screen.getByRole("button", { name: "Declare source read-only" });

    fireEvent.click(toggle);

    expect(spy).toHaveBeenCalledWith(target.source, target.set, true);
  });

  it("shows the retained count and offers to turn it back off for a set that is already read-only", async () => {
    const api = createMockApi();
    const sets = await createMockApi().listSets();
    // The media-archive fixture is the mock's one read-only set, with a
    // nonzero retained count — see mock.ts's own SETS literal.
    const target = sets.find((s) => s.readOnly);
    if (!target) throw new Error("fixture setup: no read-only set in the mock data");
    const spy = vi.spyOn(api, "setReadOnly");

    renderDetail(target.source, target.set, api);
    await screen.findByText(target.name);

    expect(screen.getByText(/Yes.*retained/)).toBeTruthy();
    const toggle = screen.getByRole("button", { name: "Allow remote deletion again" });

    fireEvent.click(toggle);

    expect(spy).toHaveBeenCalledWith(target.source, target.set, false);
  });
});
