import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupManagerApi } from "@shared/api/contracts";
import { BackupManagerError } from "@shared/api/contracts";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import type { BackupSet } from "@shared/types/backup";
import { resetGraphForTests } from "@shared/state/graph";
import { backupSetPath } from "@shared/utilities/routes";

/**
 * Issue #391.
 *
 * "Remove set configuration" opened a destructive confirmation whose
 * confirm handler was `onConfirm={() => setRemoveOpen(false)}`. It closed
 * the dialog and called nothing, and there was nothing below it to call:
 * no service method, no route, no CLI verb. So an operator confirmed a
 * destructive action, watched the dialog close, and reasonably believed
 * the set was gone while it went on discovering, transferring, applying
 * retention and, for a set that is not read-only, deleting from the
 * source.
 *
 * Every test here drives the REAL confirm button rather than calling a
 * handler, because "the handler works" and "pressing the button reaches
 * the handler" are different claims and only the second one is the bug.
 */

/** The detail page plus a real /sets route to land on, so "navigated away"
 *  is something the test can see rather than something it has to trust a
 *  spy about. */
function renderDetailWithList(source: string, set: string, api: BackupManagerApi, readOnly = false) {
  return render(
    <MemoryRouter initialEntries={[backupSetPath(source, set)]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={readOnly} />} />
          <Route path="/sets" element={<h1>Backup sets</h1>} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

/** The mock's first set, read through a SEPARATE mock instance so the one
 *  under test keeps its own untouched copy: removeSet mutates the fixture,
 *  which is the point of that fake. */
async function firstSet(): Promise<BackupSet> {
  return (await createMockApi().listSets())[0];
}

const REMOVE_BUTTON = /^Remove set configuration/;
const CONFIRM_BUTTON = "Remove configuration";

async function openRemoveDialog(api: BackupManagerApi, target: BackupSet, readOnly = false) {
  renderDetailWithList(target.source, target.set, api, readOnly);
  await screen.findByText(target.name);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: REMOVE_BUTTON }));
  });
  await screen.findByRole("button", { name: CONFIRM_BUTTON });
}

describe("removing a backup set from the detail page", () => {
  afterEach(() => {
    resetGraphForTests();
    resetMockFixtures();
    vi.restoreAllMocks();
  });

  it("actually removes the set when the confirmation is confirmed", async () => {
    const api = createMockApi();
    const remove = vi.spyOn(api, "removeSet");
    const target = await firstSet();

    await openRemoveDialog(api, target);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CONFIRM_BUTTON }));
    });

    await waitFor(() => expect(remove).toHaveBeenCalledTimes(1));
    expect(remove).toHaveBeenCalledWith(target.source, target.set);
  });

  it("leaves the set alone when the confirmation is declined", async () => {
    const api = createMockApi();
    const remove = vi.spyOn(api, "removeSet");
    const target = await firstSet();

    await openRemoveDialog(api, target);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    });

    expect(remove).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: CONFIRM_BUTTON })).toBeNull();
  });

  it("goes back to the list instead of re-reading a set that is gone", async () => {
    // The failure this catches is specific: a page that reloaded the set
    // after removing it would fetch a 404 and put an error state in front
    // of an operator whose action had just succeeded.
    const api = createMockApi();
    const target = await firstSet();

    await openRemoveDialog(api, target);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CONFIRM_BUTTON }));
    });

    expect(await screen.findByRole("heading", { name: "Backup sets" })).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("says so and stays put when the removal is refused", async () => {
    // The other half of the same promise. A dialog that closed on a
    // failure would be the original defect with a network call in the
    // middle: the operator would still be looking at a screen that
    // implied the set was gone.
    const api = createMockApi();
    vi.spyOn(api, "removeSet").mockRejectedValue(
      new BackupManagerError({
        code: "unknown",
        message: "The configuration file is not writable.",
        correlationId: "cid_test"
      })
    );
    const target = await firstSet();

    await openRemoveDialog(api, target);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CONFIRM_BUTTON }));
    });

    expect(await screen.findByText(/The configuration file is not writable\./)).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Backup sets" })).toBeNull();
    expect(screen.getByRole("button", { name: CONFIRM_BUTTON })).toBeTruthy();
  });

  it("offers no removal at all on a read-only surface", async () => {
    const api = createMockApi();
    const target = await firstSet();

    renderDetailWithList(target.source, target.set, api, true);
    await screen.findByText(target.name);

    expect(screen.getByRole("button", { name: REMOVE_BUTTON })).toHaveProperty("disabled", true);
  });
});
