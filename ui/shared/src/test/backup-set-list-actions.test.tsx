import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { BackupSetsPage } from "@shared/pages/BackupSetsPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupManagerApi } from "@shared/api/contracts";
import { BackupManagerError } from "@shared/api/contracts";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import { backupSetIdentity } from "@shared/utilities/backupSetIdentity";
import type { BackupSet } from "@shared/types/backup";
import { resetGraphForTests } from "@shared/state/graph";
import { setsNode } from "@shared/state/appNodes";
import { useResource } from "@shared/state/resource";

/**
 * Enable, disable and remove, from the page that LISTS backup sets.
 *
 * All three were already service methods with routes and generated
 * bindings, and all three meant opening the set first. An operator with a
 * dozen sets who wanted to pause one had to navigate in, act, and
 * navigate back.
 *
 * Putting them on the row puts a DESTRUCTIVE action in a list, which is
 * the arrangement that gets the wrong row acted on, so most of this file
 * is about which row. Every test drives the real control the operator
 * presses, never a handler, because "the handler works" and "pressing
 * that button on THAT row reaches it with THAT set" are different claims
 * and only the second one is what a list can get wrong.
 */

/** The page, wired to the shared node exactly as App.tsx wires it, so
 *  `sets.reload()` is a real re-read and not a spy. Without this the
 *  "the list refreshes in place" tests would be asserting against a
 *  fixture the page never re-fetched. */
function ListScreen({ api, readOnly = false }: { api: BackupManagerApi; readOnly?: boolean }) {
  const sets = useResource(setsNode, () => api.listSets(), [api]);
  return <BackupSetsPage sets={sets} readOnly={readOnly} />;
}

async function renderList(api: BackupManagerApi, readOnly = false): Promise<BackupSet[]> {
  const expected = await createMockApi().listSets();
  render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <ListScreen api={api} readOnly={readOnly} />
      </ApiProvider>
    </MemoryRouter>
  );
  await screen.findByRole("button", { name: rowButtonName(expected[0], "Remove") });
  return expected;
}

/** A row's own controls, found through the accessible name that names the
 *  set. This is the query an operator's screen reader makes, and it is
 *  the reason the buttons carry the set's name at all: without it there
 *  are four identical "Disable" buttons on the page and no test (and no
 *  assistive technology) can say which is which. */
function rowButton(set: BackupSet, action: "Disable" | "Enable" | "Remove"): HTMLElement {
  return screen.getByRole("button", { name: rowButtonName(set, action) });
}

function rowButtonName(set: BackupSet, action: "Disable" | "Enable" | "Remove"): string {
  return action === "Remove"
    ? "Remove\u2026 set configuration for " + set.name
    : action + " backup set " + set.name;
}

function phraseBox(set: BackupSet): HTMLElement {
  return screen.getByLabelText("To confirm, type " + backupSetIdentity(set));
}

const CONFIRM = "Remove configuration";

async function openRemoval(set: BackupSet) {
  await act(async () => {
    fireEvent.click(rowButton(set, "Remove"));
  });
  await screen.findByRole("button", { name: CONFIRM });
}

async function typePhrase(set: BackupSet, text: string) {
  await act(async () => {
    fireEvent.change(phraseBox(set), { target: { value: text } });
  });
}

describe("acting on a backup set from the list", () => {
  afterEach(async () => {
    // Drain before resetting, not after. Every mock call resolves on a
    // 180ms timer and fetchResource writes its answer into the SHARED
    // setsNode whenever it lands, whether or not anything is still
    // mounted. A test that ended with one still out would otherwise have
    // the previous fixture arrive in the middle of the next test, and
    // the next test would read a list it never asked for. That is not
    // hypothetical: it is what made "every set is enabled" fail on a
    // page that had only ever been handed enabled sets.
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 400));
    });
    resetGraphForTests();
    resetMockFixtures();
    vi.restoreAllMocks();
  });

  // ------------------------------------------------------------------
  // Which sets are off, without pressing anything
  // ------------------------------------------------------------------

  it("shows a set that is off as Disabled, and says how many on the header", async () => {
    const api = createMockApi();
    const all = await api.listSets();
    const off = all[1];
    await api.setEnabled(off.source, off.set, false);

    await renderList(api);

    const card = screen.getByText(off.name).closest("article");
    expect(card).not.toBeNull();
    expect(within(card as HTMLElement).getByText("Disabled")).toBeTruthy();
    // The control: exactly one card carries it, so a badge painted on
    // every card would fail here rather than pass the assertion above.
    expect(screen.getAllByText("Disabled")).toHaveLength(1);
    expect(screen.getByText(/1 disabled/)).toBeTruthy();
  });

  it("says nothing about disabled sets when every set is on", async () => {
    const api = createMockApi();
    await renderList(api);

    expect(screen.queryAllByText("Disabled")).toHaveLength(0);
    expect(screen.queryByText(/disabled/)).toBeNull();
  });

  // ------------------------------------------------------------------
  // Enable and disable
  // ------------------------------------------------------------------

  it("turns off the set whose row was pressed, and no other", async () => {
    const api = createMockApi();
    const setEnabled = vi.spyOn(api, "setEnabled");
    const all = await renderList(api);
    const target = all[1];

    await act(async () => {
      fireEvent.click(rowButton(target, "Disable"));
    });

    await waitFor(() => expect(setEnabled).toHaveBeenCalledTimes(1));
    expect(setEnabled).toHaveBeenCalledWith(target.source, target.set, false);
  });

  it("turns a disabled set back on from the same place", async () => {
    const api = createMockApi();
    const all = await api.listSets();
    const off = all[2];
    await api.setEnabled(off.source, off.set, false);
    const setEnabled = vi.spyOn(api, "setEnabled");

    await renderList(api);
    await act(async () => {
      fireEvent.click(rowButton(off, "Enable"));
    });

    await waitFor(() => expect(setEnabled).toHaveBeenCalledTimes(1));
    expect(setEnabled).toHaveBeenCalledWith(off.source, off.set, true);
  });

  it("shows the new state without the operator reloading anything", async () => {
    // setEnabled resolves to nothing, so a page that did not re-read the
    // list would go on showing a set as enabled after turning it off,
    // for up to the thirty seconds of App's poll. The mock applies the
    // change to its own fixture, so this asserts against a real re-read
    // rather than against a spy.
    const api = createMockApi();
    const all = await renderList(api);
    const target = all[0];

    expect(screen.queryAllByText("Disabled")).toHaveLength(0);
    await act(async () => {
      fireEvent.click(rowButton(target, "Disable"));
    });

    const card = await waitFor(() => {
      const c = screen.getByText(target.name).closest("article") as HTMLElement;
      expect(within(c).getByText("Disabled")).toBeTruthy();
      return c;
    });
    expect(within(card).getByRole("button", { name: rowButtonName(target, "Enable") })).toBeTruthy();
  });

  // ------------------------------------------------------------------
  // A row mid-operation is not actionable twice
  // ------------------------------------------------------------------

  it("refuses a second press while the first request is still out", async () => {
    // React batches, so two clicks dispatched before the re-render both
    // see a button that is still enabled. The `disabled` attribute alone
    // does not stop this; the guard inside the handler does, and this is
    // the test that tells them apart.
    const api = createMockApi();
    const setEnabled = vi.spyOn(api, "setEnabled").mockReturnValue(new Promise<void>(() => {}));
    const all = await renderList(api);
    const target = all[1];

    await act(async () => {
      const button = rowButton(target, "Disable");
      fireEvent.click(button);
      fireEvent.click(button);
      fireEvent.click(button);
    });

    expect(setEnabled).toHaveBeenCalledTimes(1);
  });

  it("turns the rest of that row off while its request is out, and leaves other rows alone", async () => {
    const api = createMockApi();
    vi.spyOn(api, "setEnabled").mockReturnValue(new Promise<void>(() => {}));
    const all = await renderList(api);
    const target = all[1];
    const other = all[2];

    await act(async () => {
      fireEvent.click(rowButton(target, "Disable"));
    });

    expect(rowButton(target, "Disable")).toHaveProperty("disabled", true);
    expect(rowButton(target, "Remove")).toHaveProperty("disabled", true);
    // The control. Without it a page that disabled EVERY row's controls
    // would pass the two assertions above just as well.
    expect(rowButton(other, "Disable")).toHaveProperty("disabled", false);
    expect(rowButton(other, "Remove")).toHaveProperty("disabled", false);
  });

  it("will not let a disable race the removal it is being asked to confirm", async () => {
    const api = createMockApi();
    const setEnabled = vi.spyOn(api, "setEnabled");
    const all = await renderList(api);
    const target = all[1];

    await openRemoval(target);

    expect(rowButton(target, "Disable")).toHaveProperty("disabled", true);
    await act(async () => {
      fireEvent.click(rowButton(target, "Disable"));
    });
    expect(setEnabled).not.toHaveBeenCalled();
  });

  it("sends one DELETE however many times the confirm button is pressed", async () => {
    const api = createMockApi();
    const remove = vi.spyOn(api, "removeSet").mockReturnValue(new Promise<void>(() => {}));
    const all = await renderList(api);
    const target = all[1];

    await openRemoval(target);
    await typePhrase(target, backupSetIdentity(target));
    await act(async () => {
      const button = screen.getByRole("button", { name: CONFIRM });
      fireEvent.click(button);
      fireEvent.click(button);
    });

    expect(remove).toHaveBeenCalledTimes(1);
  });

  // ------------------------------------------------------------------
  // The removal names, and is guarded by, the row it came from
  // ------------------------------------------------------------------

  it("asks about the row it was opened from, by name and by identity", async () => {
    const api = createMockApi();
    const all = await renderList(api);
    const target = all[1];
    const other = all[0];

    await openRemoval(target);

    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText(new RegExp("stop collecting backups for " + target.name))).toBeTruthy();
    expect(within(dialog).getByText(backupSetIdentity(target))).toBeTruthy();
    expect(within(dialog).queryByText(other.name)).toBeNull();
    expect(within(dialog).queryByText(backupSetIdentity(other))).toBeNull();
  });

  it("keeps the promise the detail page already made", async () => {
    const api = createMockApi();
    const all = await renderList(api);
    const target = all[1];

    await openRemoval(target);

    const dialog = screen.getByRole("dialog");
    expect(
      within(dialog).getByText(/stay on NAS storage and remain listed under Backups/)
    ).toBeTruthy();
  });

  it("keeps the confirm button off until the row's own identity is typed", async () => {
    const api = createMockApi();
    const all = await renderList(api);
    const target = all[1];

    await openRemoval(target);
    expect(screen.getByRole("button", { name: CONFIRM })).toHaveProperty("disabled", true);

    await typePhrase(target, backupSetIdentity(target));
    expect(screen.getByRole("button", { name: CONFIRM })).toHaveProperty("disabled", false);
  });

  // The two tests below assert on the API and on the list, and
  // deliberately NOT on whether the confirm button looks disabled. That
  // is the difference between checking the guard and checking the
  // button's appearance: with the comparison forced to always pass, a
  // test that asserts `disabled` first goes red on the button and never
  // reaches the question of whether a set was removed. These reach it.
  it("removes nothing when a wrong name is typed and the confirmation is pressed anyway", async () => {
    const api = createMockApi();
    const remove = vi.spyOn(api, "removeSet");
    const all = await renderList(api);
    const target = all[1];

    await openRemoval(target);
    for (const wrong of [
      "",
      backupSetIdentity(target).toUpperCase(),
      backupSetIdentity(target) + " ",
      " " + backupSetIdentity(target),
      backupSetIdentity(target).slice(0, -1),
      target.set,
      target.name
    ]) {
      await typePhrase(target, wrong);
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: CONFIRM }));
      });
      expect(remove).not.toHaveBeenCalled();
    }

    await waitFor(() => expect(screen.getByText(target.name)).toBeTruthy());
  });

  it("does not accept a NEIGHBOURING set's identity, which is the mistake a list invites", async () => {
    // The whole reason the phrase is the row's own. Typing the identity
    // of the set above or below the one you meant is the exact shape of
    // the accident this guard exists for, and a guard that compared
    // against "any set's identity" would pass every other test in this
    // file. Again: what is asserted is that nothing was removed, not
    // that a button looked a certain way.
    const api = createMockApi();
    const remove = vi.spyOn(api, "removeSet");
    const all = await renderList(api);
    const target = all[1];
    const neighbour = all[2];

    await openRemoval(target);
    await typePhrase(target, backupSetIdentity(neighbour));
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CONFIRM }));
    });

    expect(remove).not.toHaveBeenCalled();
    expect(screen.getByText(target.name)).toBeTruthy();
    expect(screen.getByText(neighbour.name)).toBeTruthy();
  });

  it("starts empty again when a second row's removal is opened", async () => {
    // A confirmation that arrived pre-satisfied with the PREVIOUS row's
    // phrase still in the box would be one click from removing a set
    // nobody typed the name of.
    const api = createMockApi();
    const all = await renderList(api);
    const first = all[1];
    const second = all[2];

    await openRemoval(first);
    await typePhrase(first, backupSetIdentity(first));
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    });

    await openRemoval(second);
    expect(phraseBox(second)).toHaveProperty("value", "");
    // And the consequence, which is the part that matters: pressing
    // confirm on a box nobody has typed into removes nothing.
    const remove = vi.spyOn(api, "removeSet");
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CONFIRM }));
    });
    expect(remove).not.toHaveBeenCalled();
  });

  // ------------------------------------------------------------------
  // What removal does
  // ------------------------------------------------------------------

  it("removes the set the row named, once, and refreshes the list in place", async () => {
    const api = createMockApi();
    const remove = vi.spyOn(api, "removeSet");
    const all = await renderList(api);
    const target = all[1];
    const survivor = all[2];

    await openRemoval(target);
    await typePhrase(target, backupSetIdentity(target));
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CONFIRM }));
    });

    await waitFor(() => expect(remove).toHaveBeenCalledTimes(1));
    expect(remove).toHaveBeenCalledWith(target.source, target.set);
    // There is nowhere to navigate to from here, so the list itself has
    // to stop showing the set. The survivor is the control: an absent
    // name below is a refreshed list and not an emptied one.
    expect(screen.getByText(survivor.name)).toBeTruthy();
    await waitFor(() => expect(screen.queryByText(target.name)).toBeNull());
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("treats a set that is already gone as removed rather than as a failure", async () => {
    const api = createMockApi();
    const all = await renderList(api);
    const target = all[1];

    await openRemoval(target);
    await typePhrase(target, backupSetIdentity(target));
    // Somebody else removes it first, through the same shared fixture.
    await createMockApi().removeSet(target.source, target.set);

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CONFIRM }));
    });

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.queryByRole("alert")).toBeNull();
    await waitFor(() => expect(screen.queryByText(target.name)).toBeNull());
  });

  it("says so and keeps the dialog open when the removal is refused", async () => {
    const api = createMockApi();
    vi.spyOn(api, "removeSet").mockRejectedValue(
      new BackupManagerError({
        code: "unknown",
        message: "The configuration file is not writable.",
        correlationId: "cid_test"
      })
    );
    const all = await renderList(api);
    const target = all[1];

    await openRemoval(target);
    await typePhrase(target, backupSetIdentity(target));
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CONFIRM }));
    });

    expect(await screen.findByText(/The configuration file is not writable\./)).toBeTruthy();
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(screen.getByText(target.name)).toBeTruthy();
  });

  it("removes the last set under a source without refusing it", async () => {
    // #391's panel settled this and relaxed config.Validate for it: a
    // source with no sets left is allowed, and the source stays. A guard
    // added here would put the refusal back one layer up.
    const api = createMockApi();
    const remove = vi.spyOn(api, "removeSet");
    const all = await renderList(api);
    const only = all.filter((s) => all.filter((o) => o.source === s.source).length === 1)[0];
    expect(only).toBeTruthy();

    await openRemoval(only);
    await typePhrase(only, backupSetIdentity(only));
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CONFIRM }));
    });

    await waitFor(() => expect(remove).toHaveBeenCalledWith(only.source, only.set));
    await waitFor(() => expect(screen.queryByText(only.name)).toBeNull());
    expect(screen.queryByRole("alert")).toBeNull();
  });

  // ------------------------------------------------------------------
  // Read-only surface
  // ------------------------------------------------------------------

  it("offers neither on a read-only surface", async () => {
    const api = createMockApi();
    const all = await renderList(api, true);

    for (const set of all) {
      expect(rowButton(set, "Disable")).toHaveProperty("disabled", true);
      expect(rowButton(set, "Remove")).toHaveProperty("disabled", true);
    }
  });
});
