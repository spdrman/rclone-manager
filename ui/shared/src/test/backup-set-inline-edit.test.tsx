import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupManagerApi } from "@shared/api/contracts";
import { BackupManagerError } from "@shared/api/contracts";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import type { BackupSet } from "@shared/types/backup";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { currentSetDetailNode } from "@shared/state/backupSetDetailNodes";
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

async function openEditMode(api: BackupManagerApi, target: BackupSet) {
  renderDetail(target.source, target.set, api);
  await screen.findByText(target.name);
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  });
  await screen.findByRole("button", { name: "SAVE ALL & EXIT EDIT" });
}

/** The mock's first set, read through a SEPARATE mock instance so the one
 *  under test still has its own untouched copy: createMockApi's SETS are
 *  mutated by updateBackupSet, which is the point of that fake. */
async function firstSet(): Promise<BackupSet> {
  return (await createMockApi().listSets())[0];
}

/**
 * Waits until a per-box Save has actually LANDED, not merely started.
 *
 * The obvious condition (its Save button is disabled) is true for two
 * different reasons: the box is clean, and the box is mid-save. Waiting
 * on it therefore returns instantly while the request is still in flight,
 * which is how the first version of the SAVE-ALL test below ended up
 * asserting about a half-finished save. The button's LABEL distinguishes
 * them, because only the in-flight state reads "Saving...".
 */
async function saveLanded(label: string) {
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Save " + label }).textContent).toBe("Save")
  );
}

describe("issue #350: Edit is an inline mode, not a dialog", () => {
  afterEach(() => {
    resetGraphForTests();
    resetMockFixtures();
    vi.restoreAllMocks();
  });

  it("turns every editable field into an input, each with its own disabled Save, and relabels Edit", async () => {
    const api = createMockApi();
    const target = await firstSet();

    await openEditMode(api, target);

    // Every editable field is an input holding its current value.
    expect((screen.getByLabelText("Host") as HTMLInputElement).value).toBe(target.host);
    expect((screen.getByLabelText("Port") as HTMLInputElement).value).toBe(String(target.port));
    expect((screen.getByLabelText("User") as HTMLInputElement).value).toBe(target.username);
    expect((screen.getByLabelText("Remote folder") as HTMLInputElement).value).toBe(target.remoteFolder);
    expect((screen.getByLabelText("Local destination") as HTMLInputElement).value).toBe(target.destination);
    expect((screen.getByLabelText("Include patterns") as HTMLInputElement).value).toBe(
      target.includePatterns.join(", ")
    );

    // Each box has its own Save, and every one of them starts disabled:
    // nothing differs from what was loaded yet.
    for (const name of ["Save host", "Save port", "Save user", "Save remote folder", "Save local destination", "Save include patterns", "Save completion method"]) {
      const button = screen.getByRole("button", { name }) as HTMLButtonElement;
      expect(button.disabled, name + " starts enabled").toBe(true);
    }

    // And the Edit button itself is now the exit control.
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
  });

  it("enables one box's Save only once that box differs from the value it loaded", async () => {
    const api = createMockApi();
    const target = await firstSet();
    await openEditMode(api, target);

    const host = screen.getByLabelText("Host") as HTMLInputElement;
    const save = () => screen.getByRole("button", { name: "Save host" }) as HTMLButtonElement;

    fireEvent.change(host, { target: { value: target.host + "x" } });
    expect(save().disabled).toBe(false);

    // Typing a character and deleting it leaves Save inactive: the
    // comparison is against the value LOADED, not against the last
    // keystroke.
    fireEvent.change(host, { target: { value: target.host } });
    expect(save().disabled).toBe(true);

    // And an unrelated box stays inactive throughout.
    expect((screen.getByRole("button", { name: "Save port" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("sends only that box's field when its Save is pressed, and keeps other boxes' unsaved edits", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet");
    const target = await firstSet();
    await openEditMode(api, target);

    fireEvent.change(screen.getByLabelText("Host"), { target: { value: "moved.internal" } });
    fireEvent.change(screen.getByLabelText("User"), { target: { value: "someone-else" } });

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save host" }));
    });

    expect(update).toHaveBeenCalledTimes(1);
    const [, , patch] = update.mock.calls[0];
    expect(patch).toEqual({ host: "moved.internal" });

    // The other box keeps the edit nobody saved yet, rather than being
    // reset by the reload the save triggered.
    expect((screen.getByLabelText("User") as HTMLInputElement).value).toBe("someone-else");
    // And its Save is still armed, while the saved box's has gone quiet
    // BECAUSE it is clean rather than because it is still saving.
    expect((screen.getByRole("button", { name: "Save user" }) as HTMLButtonElement).disabled).toBe(false);
    await saveLanded("host");
    expect((screen.getByRole("button", { name: "Save host" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Save user" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("saves everything still dirty on SAVE ALL & EXIT EDIT and returns to view mode", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet");
    const target = await firstSet();
    await openEditMode(api, target);

    fireEvent.change(screen.getByLabelText("Host"), { target: { value: "both-a.internal" } });
    fireEvent.change(screen.getByLabelText("User"), { target: { value: "both-b" } });

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "SAVE ALL & EXIT EDIT" }));
    });

    expect(update).toHaveBeenCalledTimes(1);
    const [, , patch] = update.mock.calls[0];
    expect(patch).toEqual({ host: "both-a.internal", username: "both-b" });

    await screen.findByRole("button", { name: "Edit" });
    expect(screen.queryByLabelText("Host")).toBeNull();
  });

  it("exits without re-saving what a per-box Save already wrote", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet");
    const target = await firstSet();
    await openEditMode(api, target);

    fireEvent.change(screen.getByLabelText("Host"), { target: { value: "already-saved.internal" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save host" }));
    });
    expect(update).toHaveBeenCalledTimes(1);
    // The save has to have LANDED before SAVE ALL is pressed, or this
    // would be asserting about a request still in flight rather than
    // about what SAVE ALL considers dirty. The page disables SAVE ALL
    // while a per-box save is running for the same reason.
    await saveLanded("host");

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "SAVE ALL & EXIT EDIT" }));
    });

    // Still one call: SAVE ALL is not a second chance to re-send what is
    // already persisted.
    expect(update).toHaveBeenCalledTimes(1);
    await screen.findByRole("button", { name: "Edit" });
  });

  it("exits on SAVE ALL & EXIT EDIT even when nothing was dirty", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet");
    const target = await firstSet();
    await openEditMode(api, target);

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "SAVE ALL & EXIT EDIT" }));
    });

    expect(update).not.toHaveBeenCalled();
    await screen.findByRole("button", { name: "Edit" });
  });

  it("keeps edit mode, the typed value and a stated reason when a save fails", async () => {
    const api = createMockApi();
    vi.spyOn(api, "updateBackupSet").mockRejectedValue(
      new BackupManagerError({
        code: "INVALID_REQUEST",
        message: "remote_path must be an absolute path",
        correlationId: "cid_test"
      })
    );
    const target = await firstSet();
    await openEditMode(api, target);

    fireEvent.change(screen.getByLabelText("Remote folder"), { target: { value: "not/absolute" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save remote folder" }));
    });

    // Still in edit mode.
    expect(screen.getByRole("button", { name: "SAVE ALL & EXIT EDIT" })).toBeTruthy();
    // The typed value is still there: dropping back to the old one would
    // discard the operator's work and show them the previous value as
    // though nothing had happened.
    expect((screen.getByLabelText("Remote folder") as HTMLInputElement).value).toBe("not/absolute");
    // And it says what failed.
    expect(screen.getByText("remote_path must be an absolute path")).toBeTruthy();
  });

  it("refuses rather than clobbering when the underlying set changed while edit mode was open", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet");
    const target = await firstSet();
    await openEditMode(api, target);

    fireEvent.change(screen.getByLabelText("Host"), { target: { value: "mine.internal" } });

    // Somebody else's change lands on the very node this page reads,
    // which is exactly what isSetEditStale watches.
    act(() => {
      graph.commit("test/concurrent-save", (tx) =>
        tx.set(currentSetDetailNode, {
          data: { ...target, host: "theirs.internal" },
          error: null,
          loading: false
        })
      );
    });

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save host" }));
    });

    expect(update).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText(/changed since you opened/i)).toBeTruthy();
  });

  it("leaves Edit unavailable for a read-only surface", async () => {
    const api = createMockApi();
    const target = await firstSet();

    renderDetail(target.source, target.set, api, true);
    await screen.findByText(target.name);

    expect((screen.getByRole("button", { name: "Edit" }) as HTMLButtonElement).disabled).toBe(true);
  });

  // The completion method's window is the one box that only exists for
  // one value of another box. Without it, choosing "Stable file size"
  // would produce a Save that can only fail: core refuses a set whose
  // strategy is "stable" and whose window is zero, exactly as it refuses
  // one at creation. So this checks both halves, that it appears with the
  // method and that it is never part of a patch while it is hidden.
  it("reveals the stable-size window when that method is chosen, and never sends it otherwise", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet");
    const target = await firstSet();
    await openEditMode(api, target);

    // The fixture's first set is not stable-size, so the window is absent.
    expect(target.completionMethod).not.toBe("stable-size");
    expect(screen.queryByLabelText("Stable for (seconds)")).toBeNull();

    fireEvent.change(screen.getByLabelText("Completion method"), { target: { value: "stable-size" } });
    const window = (await screen.findByLabelText("Stable for (seconds)")) as HTMLInputElement;
    expect(window.value).toBe(String(target.stableForSeconds));

    fireEvent.change(window, { target: { value: "300" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "SAVE ALL & EXIT EDIT" }));
    });

    expect(update.mock.calls[0][2]).toEqual({ completionMethod: "stable-size", stableForSeconds: 300 });
  });

  it("does not send the stable-size window as part of a patch that hides it", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet");
    const target = await firstSet();
    await openEditMode(api, target);

    // Turn the conditional box on, dirty it, then turn it back off. Its
    // draft value is still non-baseline, so a dirty check that walked the
    // whole field table rather than the visible one would ship it.
    fireEvent.change(screen.getByLabelText("Completion method"), { target: { value: "stable-size" } });
    fireEvent.change(await screen.findByLabelText("Stable for (seconds)"), { target: { value: "999" } });
    fireEvent.change(screen.getByLabelText("Completion method"), {
      target: { value: target.completionMethod }
    });
    expect(screen.queryByLabelText("Stable for (seconds)")).toBeNull();

    fireEvent.change(screen.getByLabelText("Host"), { target: { value: "elsewhere.internal" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "SAVE ALL & EXIT EDIT" }));
    });

    expect(update.mock.calls[0][2]).toEqual({ host: "elsewhere.internal" });
  });

  // React Router does not remount this page for a :source/:set change, so
  // edit mode has to notice the change itself. It does not, and A's draft
  // stays on screen under B's heading, a Save there writes A's values to
  // B, which is the worst outcome available on this page.
  it("closes edit mode, and releases the hold, when the route moves to another set", async () => {
    const api = createMockApi();
    const release = vi.spyOn(api, "releaseEditHold");
    const sets = await createMockApi().listSets();
    const [first, second] = sets;

    // A real in-router navigation, the same shape backup-set-detail-page's
    // own "no unmount" case uses: React Router keeps this component
    // mounted across it, which is the whole point.
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
    await screen.findByText(first.name);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    });
    await screen.findByRole("button", { name: "SAVE ALL & EXIT EDIT" });
    fireEvent.change(screen.getByLabelText("Host"), { target: { value: "belongs-to-the-first-set" } });

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "go to second" }));
    });

    await screen.findByText(second.name);
    expect(screen.queryByRole("button", { name: "SAVE ALL & EXIT EDIT" })).toBeNull();
    expect(screen.queryByDisplayValue("belongs-to-the-first-set")).toBeNull();
    await waitFor(() => expect(release).toHaveBeenCalledWith(first.source, first.set));
  });

  // Two per-box Saves can overlap: press one, press another before the
  // first answers. Each button has to keep reporting its OWN request, or
  // the first one springs back to "Save", enabled, while it is still in
  // flight, inviting exactly the double submit the disabled state exists
  // to prevent.
  it("keeps each box's Save reporting its own request when two overlap", async () => {
    const api = createMockApi();
    const resolvers: (() => void)[] = [];
    const target = await firstSet();
    await openEditMode(api, target);

    vi.spyOn(api, "updateBackupSet").mockImplementation(
      () => new Promise((resolve) => resolvers.push(() => resolve({ ...target })))
    );

    fireEvent.change(screen.getByLabelText("Host"), { target: { value: "overlap-a.internal" } });
    fireEvent.change(screen.getByLabelText("User"), { target: { value: "overlap-b" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save host" }));
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save user" }));
    });

    expect(screen.getByRole("button", { name: "Save host" }).textContent).toBe("Saving\u2026");
    expect(screen.getByRole("button", { name: "Save user" }).textContent).toBe("Saving\u2026");

    // Let the FIRST one answer. The second is still in flight, so its
    // button must still say so.
    await act(async () => {
      resolvers[0]();
    });
    expect(screen.getByRole("button", { name: "Save user" }).textContent).toBe("Saving\u2026");
    expect(screen.getByRole("button", { name: "Save host" }).textContent).toBe("Save");
  });

  it("has no edit dialog left to open", async () => {
    const api = createMockApi();
    const target = await firstSet();
    await openEditMode(api, target);

    expect(screen.queryByRole("dialog", { name: /edit backup set/i })).toBeNull();
  });
});

describe("issue #350: entering edit mode stops the cycle, and says so first", () => {
  afterEach(() => {
    resetGraphForTests();
    resetMockFixtures();
    vi.restoreAllMocks();
  });

  it("opens with no prompt, and holds the set, when nothing is running", async () => {
    const api = createMockApi();
    const take = vi.spyOn(api, "takeEditHold");
    const target = await firstSet();

    await openEditMode(api, target);

    expect(screen.queryByRole("dialog")).toBeNull();
    expect(take).toHaveBeenCalledWith(target.source, target.set);
  });

  it("warns, naming the artifact and stage, and does not open until confirmed", async () => {
    const api = createMockApi();
    vi.spyOn(api, "getEditHold").mockResolvedValue({
      held: false,
      running: { artifact: "2026-09-01T02-00.dump", stage: "transferring" }
    });
    const take = vi.spyOn(api, "takeEditHold");
    const target = await firstSet();

    renderDetail(target.source, target.set, api);
    await screen.findByText(target.name);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    });

    // The warning names what will be stopped, not a bare "are you sure".
    expect(await screen.findByRole("dialog")).toBeTruthy();
    expect(screen.getByText(/2026-09-01T02-00\.dump/)).toBeTruthy();
    expect(screen.getByText(/transferring/i)).toBeTruthy();

    // Nothing has been held and edit mode has not opened yet.
    expect(take).not.toHaveBeenCalled();
    expect(screen.queryByLabelText("Host")).toBeNull();

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Stop it and edit" }));
    });

    expect(take).toHaveBeenCalledWith(target.source, target.set);
    await screen.findByRole("button", { name: "SAVE ALL & EXIT EDIT" });
  });

  it("leaves the cycle running and stays in view mode when the warning is declined", async () => {
    const api = createMockApi();
    vi.spyOn(api, "getEditHold").mockResolvedValue({
      held: false,
      running: { artifact: "in-flight.dump", stage: "transferring" }
    });
    const take = vi.spyOn(api, "takeEditHold");
    const target = await firstSet();

    renderDetail(target.source, target.set, api);
    await screen.findByText(target.name);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    });
    await screen.findByRole("dialog");

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Keep backing up" }));
    });

    expect(take).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();
    expect(screen.queryByLabelText("Host")).toBeNull();
  });

  it("releases the hold when edit mode is left through SAVE ALL & EXIT EDIT", async () => {
    const api = createMockApi();
    const release = vi.spyOn(api, "releaseEditHold");
    const target = await firstSet();
    await openEditMode(api, target);

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "SAVE ALL & EXIT EDIT" }));
    });

    await waitFor(() => expect(release).toHaveBeenCalledWith(target.source, target.set));
  });

  it("releases the hold when the page goes away without pressing anything", async () => {
    const api = createMockApi();
    const release = vi.spyOn(api, "releaseEditHold");
    const target = await firstSet();

    const { unmount } = render(
      <MemoryRouter initialEntries={[backupSetPath(target.source, target.set)]}>
        <ApiProvider api={api}>
          <Routes>
            <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={false} />} />
          </Routes>
        </ApiProvider>
      </MemoryRouter>
    );
    await screen.findByText(target.name);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    });
    await screen.findByRole("button", { name: "SAVE ALL & EXIT EDIT" });

    await act(async () => {
      unmount();
    });

    // A set left permanently paused because somebody closed a tab is a
    // backup silently not happening, which is this product's worst
    // failure mode.
    await waitFor(() => expect(release).toHaveBeenCalledWith(target.source, target.set));
  });
});

/**
 * The one refusal on the update path (core/service/backupsetrepoint.go).
 * Changing the host, the remote folder or the destination on a set that
 * already holds artifacts is not the same edit as correcting a typo
 * minutes after the wizard: the artifacts already on record stay with
 * the set and are not repointed with it. This block is about what the
 * page does with that refusal, which is ask rather than either write it
 * silently or make it impossible.
 */
describe("issue #350: repointing a set that already has history", () => {
  afterEach(() => {
    resetGraphForTests();
    resetMockFixtures();
    vi.restoreAllMocks();
  });

  it("asks before repointing a set at different data, and writes nothing until it is confirmed", async () => {
    const api = createMockApi();
    const update = vi
      .spyOn(api, "updateBackupSet")
      .mockRejectedValueOnce(
        new BackupManagerError({
          code: "BACKUP_SET_REPOINT_NOT_ACKNOWLEDGED",
          message:
            'service: this edit would point the backup set at different data: this would move local_path from "/data/old" to "/data/new" while 32 artifact(s) are on record',
          correlationId: "cid_repoint"
        })
      );
    const target = await firstSet();
    await openEditMode(api, target);

    fireEvent.change(screen.getByLabelText("Local destination"), { target: { value: "/data/new" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save local destination" }));
    });

    // The service's own sentence, not one this page invented: it names
    // the field, both paths and the count, which is what the operator
    // needs to decide.
    expect(screen.getByText(/32 artifact\(s\) are on record/)).toBeTruthy();
    // Still in edit mode, still holding what was typed.
    expect((screen.getByLabelText("Local destination") as HTMLInputElement).value).toBe("/data/new");
    // And this is a decision, not a sentence under the box: it comes
    // with the two answers.
    expect(screen.getByRole("button", { name: "Save anyway" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Leave it as it was" })).toBeTruthy();
    expect(update).toHaveBeenCalledTimes(1);
    expect(update.mock.calls[0][2].acknowledgeRepoint).toBeUndefined();
  });

  it("re-sends the same keys WITH the acknowledgement when the repoint is confirmed", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet").mockRejectedValueOnce(
      new BackupManagerError({
        code: "BACKUP_SET_REPOINT_NOT_ACKNOWLEDGED",
        message: "this would move local_path",
        correlationId: "cid_repoint"
      })
    );
    const target = await firstSet();
    await openEditMode(api, target);

    fireEvent.change(screen.getByLabelText("Local destination"), { target: { value: "/data/new" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save local destination" }));
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save anyway" }));
    });

    expect(update).toHaveBeenCalledTimes(2);
    // The retry carries the SAME single key, plus the acknowledgement.
    // Re-sending every field here would turn one confirmed decision into
    // a whole-form write.
    expect(update.mock.calls[1][2]).toEqual({ destination: "/data/new", acknowledgeRepoint: true });
    // The refusal is gone once it is answered.
    expect(screen.queryByRole("button", { name: "Save anyway" })).toBeNull();
  });

  it("declining a repoint writes nothing and leaves the form as it was", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet").mockRejectedValueOnce(
      new BackupManagerError({
        code: "BACKUP_SET_REPOINT_NOT_ACKNOWLEDGED",
        message: "this would move local_path",
        correlationId: "cid_repoint"
      })
    );
    const target = await firstSet();
    await openEditMode(api, target);

    fireEvent.change(screen.getByLabelText("Local destination"), { target: { value: "/data/new" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save local destination" }));
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Leave it as it was" }));
    });

    expect(update).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("button", { name: "Save anyway" })).toBeNull();
    // Still in edit mode with the typed value: declining answers the
    // question, it does not throw the operator's work away.
    expect(screen.getByRole("button", { name: "SAVE ALL & EXIT EDIT" })).toBeTruthy();
    expect((screen.getByLabelText("Local destination") as HTMLInputElement).value).toBe("/data/new");
  });

  it("confirming a repoint that SAVE ALL asked for leaves edit mode, as SAVE ALL promised", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet").mockRejectedValueOnce(
      new BackupManagerError({
        code: "BACKUP_SET_REPOINT_NOT_ACKNOWLEDGED",
        message: "this would move local_path",
        correlationId: "cid_repoint"
      })
    );
    const target = await firstSet();
    await openEditMode(api, target);

    fireEvent.change(screen.getByLabelText("Local destination"), { target: { value: "/data/new" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "SAVE ALL & EXIT EDIT" }));
    });
    // Refused, so it did NOT exit: exiting on a refusal would leave the
    // operator on the view page believing a change landed.
    expect(screen.getByRole("button", { name: "SAVE ALL & EXIT EDIT" })).toBeTruthy();

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save anyway" }));
    });
    expect(update).toHaveBeenCalledTimes(2);
    await screen.findByRole("button", { name: "Edit" });
  });
});
