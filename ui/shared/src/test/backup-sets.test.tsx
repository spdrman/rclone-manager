/**
 * Two things the backup sets page has to get right no matter what state
 * it is in: how many "Add backup set" buttons exist, and where the run
 * control lives.
 *
 * The button-count cases look trivial and are not. A null list while the
 * first fetch is in flight is not an empty list, and treating the two the
 * same rendered the empty state's own button beside the header's, which is
 * two identically named controls on screen and an ambiguous locator for
 * anything selecting by name. That raced the browser suite repeatedly
 * before it was pinned here, which is why all three states are asserted
 * rather than just the loaded one.
 *
 * The run control case is about scope. A deployment-wide pass drawn inside
 * a per-set card reads as a per-set run whatever its label says, so its
 * absence from the card is asserted as directly as its presence in the
 * header.
 */
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

  // Regression for #231. Every card carried its own "Run now" button, and
  // its handler was api.runCycle(): one pass over EVERY enabled backup
  // set. So the button named for one set started all of them, and a list
  // of four sets rendered four copies of the same deployment-wide action.
  // #214 had already renamed that button in both page headers that carry
  // it and given it a tooltip saying how wide it reaches; the card is
  // where the rename did not follow the rewire.
  //
  // The fix is not a third rename. A control inside a card reads as the
  // card's whatever it is labelled, so the run control moved out of the
  // card and into the page header, where there is one of it.
  it("offers the run control once, in the page header, and never on a card", async () => {
    const data = await createMockApi().listSets();
    expect(data.length).toBeGreaterThan(1);
    renderSets({ data, error: null, loading: false, reload: noop });

    expect(screen.queryAllByRole("button", { name: "Run now" })).toHaveLength(0);

    const run = screen.getAllByRole("button", { name: "Run all due sets" });
    expect(run).toHaveLength(1);
    // The label alone cannot say how far the action reaches, so the
    // sentence that does is asserted with it. Without this, a later
    // rewire could pass under the new name the way it passed under the
    // old one.
    expect(run[0].getAttribute("title")).toMatch(
      /every enabled backup set, not only this one/
    );

    // The control for the zero and the one above: the same query shape,
    // on a button that IS on every card, finds one per set. Without it a
    // page that rendered no cards at all would satisfy the assertions
    // above just as well.
    expect(screen.getAllByRole("button", { name: "Open" })).toHaveLength(data.length);
  });
});
