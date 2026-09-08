/**
 * Issue #591: leaving edit mode without saving.
 *
 * The page saves per box, not per form, so the interesting claim here is
 * not "the draft is gone". It is that cancel is honest about the half of
 * the session it cannot touch: a per-box Save has already written to
 * config.yaml and the engine has already hot-reloaded it, so cancel
 * discards the DRAFT and leaves persisted values exactly where they are.
 *
 * Most of these cases therefore assert on the request spy as much as on
 * the screen. "Nothing was written" and "nothing was un-written" are both
 * claims about what did NOT go out on the wire, and a cancel that quietly
 * re-sent an old value to put a field back would look identical to an
 * operator while being the worst thing this control could do.
 *
 * Every case drives the control an operator presses, in the shape
 * backup-set-inline-edit.test.tsx established, because on this page "the
 * handler works" and "this button reaches it" are different claims.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupManagerApi } from "@shared/api/contracts";
import { BackupManagerError } from "@shared/api/contracts";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import type { BackupSet } from "@shared/types/backup";
import { resetGraphForTests } from "@shared/state/graph";
import { backupSetPath } from "@shared/utilities/routes";

const CANCEL = "CANCEL & EXIT EDIT MODE";

function renderDetail(source: string, set: string, api: BackupManagerApi, readOnly = false) {
  return render(
    <MemoryRouter initialEntries={[backupSetPath(source, set)]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={readOnly} />} />
          <Route path="/sets" element={<h1>Backup sets list</h1>} />
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
 *  under test still has its own untouched copy. */
async function firstSet(): Promise<BackupSet> {
  return (await createMockApi().listSets())[0];
}

/** Waits until a per-box Save has LANDED rather than merely started: its
 *  button is disabled in both states, and only the in-flight one reads
 *  "Saving...". Same helper, same reason, as the inline-edit suite. */
async function saveLanded(label: string) {
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Save " + label }).textContent).toBe("Save")
  );
}

function dialog() {
  return screen.getByRole("dialog");
}

afterEach(() => {
  resetGraphForTests();
  resetMockFixtures();
  vi.restoreAllMocks();
});

describe("issue #591: CANCEL & EXIT EDIT MODE", () => {
  it("offers the control only while edit mode is open", async () => {
    const api = createMockApi();
    const target = await firstSet();

    renderDetail(target.source, target.set, api);
    await screen.findByText(target.name);

    // Nothing to cancel out of yet, so the control is not on the page at
    // all. Not disabled: a disabled button is a promise that pressing it
    // would do something here, and outside edit mode there is nothing.
    expect(screen.queryByRole("button", { name: CANCEL })).toBeNull();

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    });
    await screen.findByRole("button", { name: "SAVE ALL & EXIT EDIT" });

    const cancel = screen.getByRole("button", { name: CANCEL }) as HTMLButtonElement;
    // Available with nothing dirty: "I only came to look" is a legitimate
    // reason to be in edit mode and a legitimate reason to leave it.
    expect(cancel.disabled).toBe(false);
    expect(cancel.className).toContain("btn--caution");
  });

  it("leaves without asking when nothing is dirty and nothing was saved", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet");
    const release = vi.spyOn(api, "releaseEditHold");
    const target = await firstSet();
    await openEditMode(api, target);

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CANCEL }));
    });

    // No confirmation: there is nothing to warn about and asking would be
    // noise on the one press that costs nothing.
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();
    expect(screen.queryByLabelText("Host")).toBeNull();
    expect(update).not.toHaveBeenCalled();
    // The hold is tied to the mode, so this exit gives the schedule back
    // exactly as SAVE ALL & EXIT EDIT does.
    await waitFor(() => expect(release).toHaveBeenCalledWith(target.source, target.set));
  });

  it("asks before discarding a dirty draft, names the box, and writes nothing when confirmed", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet");
    const release = vi.spyOn(api, "releaseEditHold");
    const target = await firstSet();
    await openEditMode(api, target);

    fireEvent.change(screen.getByLabelText("User"), { target: { value: "rclone-2" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CANCEL }));
    });

    const discarded = within(dialog()).getByRole("group", { name: /discarded/i });
    // The value typed and the value it returns to, so the operator can see
    // what they are giving up rather than a count of it.
    expect(within(discarded).getByText("User")).toBeTruthy();
    expect(within(discarded).getByText(/rclone-2/)).toBeTruthy();
    expect(within(discarded).getByText(new RegExp(target.username))).toBeTruthy();

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /Discard 1 change and exit/ }));
    });

    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();
    // The whole point: a discard sends NOTHING. Not the new value, and not
    // the old one either.
    expect(update).not.toHaveBeenCalled();
    // And the schedule comes back, exactly as it does for the exit that
    // saves: the hold is tied to the mode, not to the button.
    await waitFor(() => expect(release).toHaveBeenCalledWith(target.source, target.set));

    // Re-opening shows what is persisted, which is what it always was.
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    });
    expect((await screen.findByLabelText("User") as HTMLInputElement).value).toBe(target.username);
  });

  it("keeps the draft when the confirmation is declined", async () => {
    const api = createMockApi();
    const target = await firstSet();
    await openEditMode(api, target);

    fireEvent.change(screen.getByLabelText("User"), { target: { value: "rclone-2" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CANCEL }));
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Keep editing" }));
    });

    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByRole("button", { name: "SAVE ALL & EXIT EDIT" })).toBeTruthy();
    expect((screen.getByLabelText("User") as HTMLInputElement).value).toBe("rclone-2");
  });

  it("does not revert a field a per-box Save already wrote, and says so", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet");
    const target = await firstSet();
    await openEditMode(api, target);

    // One box saved on its own...
    fireEvent.change(screen.getByLabelText("User"), { target: { value: "backup-agent-2" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save user" }));
    });
    await saveLanded("user");
    expect(update).toHaveBeenCalledTimes(1);

    // ...and one box typed and left alone.
    fireEvent.change(screen.getByLabelText("Remote folder"), { target: { value: "/srv/elsewhere" } });

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CANCEL }));
    });

    const discarded = within(dialog()).getByRole("group", { name: /discarded/i });
    const kept = within(dialog()).getByRole("group", { name: /already saved/i });
    expect(within(discarded).getByText("Remote folder")).toBeTruthy();
    expect(within(discarded).queryByText("User")).toBeNull();
    // The group that is the whole point of the dialog: this box is in the
    // configuration the engine is running right now.
    expect(within(kept).getByText("User")).toBeTruthy();
    expect(within(kept).getByText(/backup-agent-2/)).toBeTruthy();
    expect(within(dialog()).getByText(/not an undo/i)).toBeTruthy();

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /Discard 1 change and exit/ }));
    });

    // No second request. A cancel that re-sent the old username to put it
    // back would be a fresh write dressed up as an undo.
    expect(update).toHaveBeenCalledTimes(1);
    expect(update.mock.calls[0][2]).toEqual({ username: "backup-agent-2" });

    // And the saved value is still the persisted one on the way back in.
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    });
    expect((await screen.findByLabelText("User") as HTMLInputElement).value).toBe("backup-agent-2");
  });

  it("still confirms when nothing is dirty but something was saved this session", async () => {
    const api = createMockApi();
    const target = await firstSet();
    await openEditMode(api, target);

    fireEvent.change(screen.getByLabelText("User"), { target: { value: "backup-agent-3" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save user" }));
    });
    await saveLanded("user");

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CANCEL }));
    });

    // "I pressed cancel so nothing happened" is exactly the belief that
    // gets somebody into trouble, so the ledger appears even when the
    // already-saved group is the only one in it.
    const kept = within(dialog()).getByRole("group", { name: /already saved/i });
    expect(within(kept).getByText("User")).toBeTruthy();
    expect(within(dialog()).queryByRole("group", { name: /discarded/i })).toBeNull();
  });

  it("drops a pending refusal, its acknowledgement and its refused body", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet").mockRejectedValueOnce(
      new BackupManagerError({
        code: "BACKUP_SET_HOST_KEY_CHANGE_NOT_ACKNOWLEDGED",
        message:
          "service: this backup set trusts ssh-ed25519 SHA256:oldoldoldold and the line offered is ssh-ed25519 SHA256:newnewnewnew",
        correlationId: "cid_hostkey_cancel"
      })
    );
    const target = await firstSet();
    await openEditMode(api, target);

    fireEvent.change(screen.getByLabelText("Trusted host key"), {
      target: { value: "prod-db-01.internal ssh-ed25519 AAAAnew" }
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save trusted host key" }));
    });
    expect(screen.getByRole("button", { name: "Save anyway" })).toBeTruthy();

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: CANCEL }));
    });

    // A half-answered trust decision is the one thing on this page an
    // operator will remember being in the middle of, so the dialog says
    // what happens to it rather than leaving it to a box count.
    expect(within(dialog()).getByText(/known_hosts/)).toBeTruthy();

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /Discard 1 change and exit/ }));
    });

    // The refusal is gone WITH the mode, and no acknowledgement was spent
    // on the way out.
    expect(screen.queryByRole("button", { name: "Save anyway" })).toBeNull();
    expect(screen.queryByText(/SHA256:newnewnewnew/)).toBeNull();
    expect(update).toHaveBeenCalledTimes(1);
    expect(update.mock.calls[0][2].acknowledgeHostKeyChange).toBeUndefined();
  });

  it("leaves the back link alone outside edit mode", async () => {
    const api = createMockApi();
    const target = await firstSet();

    renderDetail(target.source, target.set, api);
    await screen.findByText(target.name);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /Backup sets/ }));
    });

    // Nothing was being composed, so there is nothing to ask about and
    // the link is the plain link it has always been.
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(await screen.findByText("Backup sets list")).toBeTruthy();
  });

  it("sends the header's back link through the same confirmation while the draft is dirty", async () => {
    const api = createMockApi();
    const update = vi.spyOn(api, "updateBackupSet");
    const target = await firstSet();
    await openEditMode(api, target);

    fireEvent.change(screen.getByLabelText("User"), { target: { value: "rclone-9" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /Backup sets/ }));
    });

    // A warning that only guards the button an operator chose
    // deliberately is guarding the wrong door.
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(screen.queryByText("Backup sets list")).toBeNull();

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /Discard 1 change and exit/ }));
    });

    expect(await screen.findByText("Backup sets list")).toBeTruthy();
    expect(update).not.toHaveBeenCalled();
  });
});
