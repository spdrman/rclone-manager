import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { BackupSetsPage } from "@shared/pages/BackupSetsPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import type { AsyncState } from "@shared/hooks/useAsync";
import type { BackupSet } from "@shared/types/backup";

const noop = () => {};

function renderSets(sets: AsyncState<BackupSet[]>) {
  return render(
    <MemoryRouter>
      <ApiProvider api={createMockApi()}>
        <BackupSetsPage sets={sets} readOnly={false} />
      </ApiProvider>
    </MemoryRouter>
  );
}

describe("backup sets page", () => {
  // Regression for #141: sets.data is null (not []) while the initial
  // listSets() fetch is in flight, and the page used to treat that the
  // same as "zero sets" — rendering the EmptyState's own action button
  // alongside the header's, two identically-named "Add backup set"
  // buttons on screen at once. That transient state is exactly what
  // wizard.spec.ts's beforeEach raced against 17 times over: a click
  // landing in that window hit a strict-mode ambiguous locator.
  it("shows exactly one 'Add backup set' button while sets are still loading", () => {
    renderSets({ data: null, error: null, loading: true, reload: noop });
    expect(screen.getAllByRole("button", { name: "Add backup set" })).toHaveLength(1);
  });

  it("shows exactly one 'Add backup set' button once loaded with zero sets", () => {
    renderSets({ data: [], error: null, loading: false, reload: noop });
    expect(screen.getAllByRole("button", { name: "Add backup set" })).toHaveLength(1);
    expect(screen.getByText("No backup sets yet")).toBeTruthy();
  });

  it("shows exactly one 'Add backup set' button once loaded with sets", async () => {
    const data = await createMockApi().listSets();
    renderSets({ data, error: null, loading: false, reload: noop });
    expect(screen.getAllByRole("button", { name: "Add backup set" })).toHaveLength(1);
    expect(screen.queryByText("No backup sets yet")).toBeNull();
  });
});
