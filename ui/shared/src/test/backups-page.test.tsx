/**
 * The backups page against the shared sets node, and the two moments
 * where an in-flight fetch could show something untrue.
 *
 * The first group proves the set filter is fed by the one shared node
 * rather than a fetch of its own, and it proves it the only way that
 * distinguishes the two: by committing to the node directly and expecting
 * the dropdown to follow with no request made anywhere.
 *
 * The other two groups are both about a list that is loading. An empty
 * result and a result that has not arrived look identical on screen and
 * mean opposite things, and rows belonging to the filter an operator just
 * changed away from are worse than no rows at all, because they are
 * clickable.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { BackupsPage } from "@shared/pages/BackupsPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupManagerApi } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { setsNode } from "@shared/state/appNodes";
import type { BackupArtifact, BackupSet } from "@shared/types/backup";

function renderBackups(api: BackupManagerApi) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <BackupsPage readOnly={false} />
      </ApiProvider>
    </MemoryRouter>
  );
}

function seedSets(sets: BackupSet[]) {
  act(() => {
    graph.commit("test/seed-sets", (tx) => tx.set(setsNode, { data: sets, error: null, loading: false }));
  });
}

// #106 landed a single shared `sets` node so App.tsx, BackupSetsPage,
// BackupsPage and (eventually) ActivityPage stop each keeping their own
// independent copy of the same server resource. These tests prove
// BackupsPage's filter is wired to that ONE node rather than issuing its
// own `listSets()` call — see docs/EPIC-B-multi-nas.md, B2.4.
describe("backups page reads the shared sets node", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("populates its backup-set filter from setsNode, not its own fetch", async () => {
    const api = createMockApi();
    const listSetsSpy = vi.spyOn(api, "listSets");
    const allSets = await createMockApi().listSets();
    seedSets(allSets);

    renderBackups(api);

    const select = await screen.findByLabelText("Filter by backup set");
    // "All backup sets" plus one option per seeded set.
    expect(select.querySelectorAll("option")).toHaveLength(allSets.length + 1);
    expect(listSetsSpy).not.toHaveBeenCalled();
  });

  it("updates the filter when something else commits to setsNode, without BackupsPage re-fetching anything", async () => {
    const api = createMockApi();
    const listSetsSpy = vi.spyOn(api, "listSets");
    const allSets = await createMockApi().listSets();
    seedSets(allSets);

    renderBackups(api);
    const select = await screen.findByLabelText("Filter by backup set");
    expect(select.querySelectorAll("option")).toHaveLength(allSets.length + 1);

    // Simulate a change this page never triggered itself — the #106 30s
    // poll, or a mutation made from another page — landing on the ONE
    // shared node (here: a set gets renamed after being disabled).
    const renamed = { ...allSets[0], name: "Production PostgreSQL (disabled)" };
    seedSets([renamed, ...allSets.slice(1)]);

    expect(await within(select).findByText("Production PostgreSQL (disabled)")).toBeTruthy();
    expect(select.querySelectorAll("option")).toHaveLength(allSets.length + 1);
    expect(listSetsSpy).not.toHaveBeenCalled();
  });
});

// Regression: rows.length === 0 is true both while artifacts are still
// loading (data is null) AND once they have genuinely loaded to zero —
// BackupSetsPage hit exactly this conflation for `sets` (#141) and had to
// distinguish "still loading" from "genuinely empty" there. BackupsPage
// had the same bug for `artifacts`.
describe("backups page loading vs. empty", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("does not flash the empty state while artifacts are still loading", async () => {
    const api = createMockApi();
    let resolveArtifacts!: (value: BackupArtifact[]) => void;
    vi.spyOn(api, "listArtifacts").mockReturnValue(
      new Promise((resolve) => {
        resolveArtifacts = resolve;
      })
    );
    seedSets(await createMockApi().listSets());

    renderBackups(api);

    expect(screen.queryByText("No backups yet")).toBeNull();

    act(() => resolveArtifacts([]));
    await screen.findByText("No backups yet");
  });
});

// Mandatory review on #144: the filter dropdown re-fetches via
// `useAsync(..., [api, setFilter])`, and useAsync never resets `data` back
// to null on reload — only `loading`/`error`. Without also gating on
// `artifacts.loading`, the previous filter's rows stayed on screen, fully
// clickable, for the whole duration of the new filter's fetch. Reachable on
// every ordinary filter change.
describe("backups page filter change mid-flight", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("does not show or allow clicking the previous filter's rows while the new filter's fetch is in flight", async () => {
    const api = createMockApi();
    const allSets = await createMockApi().listSets();
    const allArtifacts = await createMockApi().listArtifacts();
    seedSets(allSets);

    const resolvers: Array<(value: BackupArtifact[]) => void> = [];
    vi.spyOn(api, "listArtifacts").mockImplementation(
      () =>
        new Promise((resolve) => {
          resolvers.push(resolve);
        })
    );

    renderBackups(api);

    act(() => resolvers[0](allArtifacts));
    const target = allArtifacts[0];
    await screen.findByText(target.filename);

    const select = await screen.findByLabelText("Filter by backup set");
    fireEvent.change(select, { target: { value: target.setId } });

    // The refetch triggered by the filter change is in flight: the
    // previous filter's row must not still be on screen (and therefore not
    // clickable through to the wrong artifact).
    expect(screen.queryByText(target.filename)).toBeNull();

    act(() =>
      resolvers[1](allArtifacts.filter((a) => a.setId === target.setId))
    );
    await screen.findByText(target.filename);
  });
});
