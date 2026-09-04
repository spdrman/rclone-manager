import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupManagerApi } from "@shared/api/contracts";
import { BackupManagerError } from "@shared/api/contracts";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import type { BackupSet } from "@shared/types/backup";
import { graph, resetGraphForTests, useCausl } from "@shared/state/graph";
import { setsNode } from "@shared/state/appNodes";
import { backupSetPath } from "@shared/utilities/routes";
import { backupSetIdentity } from "@shared/utilities/backupSetIdentity";

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

/** The mock's first set. Every createMockApi() shares one SETS fixture,
 *  so this is the same set the api under test will remove; it is read
 *  before any removal, and listSets projects fresh objects, which is what
 *  keeps the copy this test holds honest after the fixture changes. */
async function firstSet(): Promise<BackupSet> {
  return (await createMockApi().listSets())[0];
}

/** A /sets route that reads the SAME shared node BackupSetsPage renders
 *  from, so "the list the operator lands on" is something a test can
 *  read rather than a heading it has to trust. */
function SetsFromNode() {
  const sets = useCausl(setsNode);
  return (
    <>
      <h1>Backup sets</h1>
      <ul>
        {(sets.data ?? []).map((s) => (
          <li key={s.id}>{s.name}</li>
        ))}
      </ul>
    </>
  );
}

function renderDetailWithNodeBackedList(source: string, set: string, api: BackupManagerApi) {
  return render(
    <MemoryRouter initialEntries={[backupSetPath(source, set)]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={false} />} />
          <Route path="/sets" element={<SetsFromNode />} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

const REMOVE_BUTTON = /^Remove set configuration/;
const CONFIRM_BUTTON = "Remove configuration";

/** The typed confirmation's box, found the way an operator finds it: by
 *  the label that tells them what to type. */
function phraseBox(target: BackupSet): HTMLElement {
  return screen.getByLabelText("To confirm, type " + backupSetIdentity(target));
}

function type(target: BackupSet, text: string) {
  fireEvent.change(phraseBox(target), { target: { value: text } });
}

/** Opens the dialog and satisfies the typed confirmation, for the tests
 *  whose subject is what happens AFTER the confirmation. The tests whose
 *  subject IS the confirmation do not call this. */
async function openRemoveDialog(api: BackupManagerApi, target: BackupSet, readOnly = false) {
  await openRemoveDialogUnconfirmed(api, target, readOnly);
  await act(async () => {
    type(target, backupSetIdentity(target));
  });
}

async function openRemoveDialogUnconfirmed(
  api: BackupManagerApi,
  target: BackupSet,
  readOnly = false
) {
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

  it("lands on a sets list that no longer shows the removed set", async () => {
    // BackupSetsPage renders the shared setsNode, which App fetches once
    // on mount and then only on the 30-second poll. Navigating does not
    // remount App, so a removal that only navigated landed the operator
    // on a list still showing the set they had just confirmed removing,
    // for up to thirty seconds: the original defect with the truth value
    // flipped. The create path refreshes the node before it navigates,
    // and says why; this is its mirror image.
    const api = createMockApi();
    const all = await api.listSets();
    const target = all[0];
    const other = all[1];
    if (!other) throw new Error("the mock fixture needs at least two sets for this to mean anything");
    graph.commit("test/seed-sets", (tx) => tx.set(setsNode, { data: all, error: null, loading: false }));

    renderDetailWithNodeBackedList(target.source, target.set, api);
    await screen.findByText(target.name);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: REMOVE_BUTTON }));
    });
    await screen.findByRole("button", { name: CONFIRM_BUTTON });
    await act(async () => {
      type(target, backupSetIdentity(target));
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CONFIRM_BUTTON }));
    });

    await screen.findByRole("heading", { name: "Backup sets" });
    // The control first: the other set is listed, so an absent name
    // below is a refreshed list and not an empty one.
    expect(await screen.findByText(other.name)).toBeTruthy();
    await waitFor(() => expect(screen.queryByText(target.name)).toBeNull());
  });

  it("treats a set that is already gone as removed rather than as a failure", async () => {
    // 404 BACKUP_SET_NOT_FOUND is the answer both to a name this
    // deployment never had and to a set an earlier call (a lost
    // response, a second tab) already removed. The page knows which set
    // it just asked about, so it is the one place that can tell them
    // apart, and painting a red error under a destructive dialog for a
    // removal that succeeded is the confidence failure #391 is about,
    // arriving through the retry door.
    const api = createMockApi();
    const target = await firstSet();

    await openRemoveDialog(api, target);
    // Someone else removes it first, through the same shared fixture.
    await createMockApi().removeSet(target.source, target.set);

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CONFIRM_BUTTON }));
    });

    expect(await screen.findByRole("heading", { name: "Backup sets" })).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("still reports every other refusal and stays put", async () => {
    // The branch above must be exactly one code wide. A refusal that is
    // not "it is already gone" keeps the dialog open with the reason in
    // it, and this is the case that proves the not-found branch did not
    // swallow everything.
    const api = createMockApi();
    vi.spyOn(api, "removeSet").mockRejectedValue(
      new BackupManagerError({
        code: "INTERNAL",
        message: "unused",
        correlationId: "cid_test"
      })
    );
    const target = await firstSet();

    await openRemoveDialog(api, target);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CONFIRM_BUTTON }));
    });

    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Backup sets" })).toBeNull();
  });

  // Rom asked for a typed confirmation on removal, and asked for it on
  // removal generally rather than only on the list page: one strong path
  // and one weak path to the same destructive action is worse than
  // either alone. The guard lives in ConfirmationDialog and the copy in
  // RemoveBackupSetDialog, so these are the same three assertions the
  // list-page suite makes, proved a second time through this surface's
  // own real controls.
  it("keeps the confirm button off until the set's full name is typed", async () => {
    const api = createMockApi();
    const target = await firstSet();

    await openRemoveDialogUnconfirmed(api, target);
    expect(screen.getByRole("button", { name: CONFIRM_BUTTON })).toHaveProperty("disabled", true);

    await act(async () => {
      type(target, backupSetIdentity(target));
    });
    expect(screen.getByRole("button", { name: CONFIRM_BUTTON })).toHaveProperty("disabled", false);
  });

  it("removes nothing when a near miss is typed and the confirmation is pressed anyway", async () => {
    // Asserts on the API, not on whether the button looked disabled.
    // Forcing the comparison to always pass has to be able to make this
    // go red, or it is checking the button's appearance rather than the
    // guard.
    const api = createMockApi();
    const remove = vi.spyOn(api, "removeSet");
    const target = await firstSet();

    await openRemoveDialogUnconfirmed(api, target);
    for (const near of [
      backupSetIdentity(target).toUpperCase(),
      backupSetIdentity(target) + " ",
      " " + backupSetIdentity(target),
      target.set,
      target.name
    ]) {
      await act(async () => {
        type(target, near);
      });
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: CONFIRM_BUTTON }));
      });
      expect(remove).not.toHaveBeenCalled();
    }
  });

  it("offers no removal at all on a read-only surface", async () => {
    const api = createMockApi();
    const target = await firstSet();

    renderDetailWithList(target.source, target.set, api, true);
    await screen.findByText(target.name);

    expect(screen.getByRole("button", { name: REMOVE_BUTTON })).toHaveProperty("disabled", true);
  });
});
