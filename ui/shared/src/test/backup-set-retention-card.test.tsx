import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupManagerApi, RetentionOverride } from "@shared/api/contracts";
import { BackupManagerError } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { resetGraphForTests } from "@shared/state/graph";
import { backupSetPath } from "@shared/utilities/routes";

/**
 * Issue #333's UI half.
 *
 * Two fixture sets carry the two cases the whole feature turns on:
 * production/postgres-primary inherits the deployment's policy, and
 * media/weekly-archive declares one of its own. Both are driven here,
 * because a card that answered "your own policy" for everything would
 * pass every assertion aimed only at the override.
 */
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

describe("a backup set's retention policy, on its own page", () => {
  afterEach(() => {
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("says an inheriting set is retained under the deployment's policy, and that editing that policy moves it", async () => {
    renderDetail("production", "postgres-primary", createMockApi());

    await screen.findByText(/Retained under the deployment's retention policy/);
    expect(screen.getByText(/Editing that policy changes this set too/)).toBeTruthy();
    // Every tier of the chain actually in force, not three fixed numbers.
    // The old card rendered "0 kept" three times here, against a policy
    // it had never read.
    await screen.findByText("weekly");
    expect(screen.getByText(/Europe\/Berlin/)).toBeTruthy();
  });

  it("says an overriding set decides for itself, and that the deployment's policy no longer moves it", async () => {
    renderDetail("media", "weekly-archive", createMockApi());

    await screen.findByText(/Retained under this backup set's own policy/);
    expect(
      screen.getByText(/Editing the deployment's retention policy will not change it/)
    ).toBeTruthy();
  });

  /**
   * FR-19's protection is the one clause in a retention policy that is a
   * promise about a deletion rather than a description of a schedule, and
   * this page carried it as a banner before #333 replaced its Retention
   * section. The rewrite dropped it, and the browser suite caught that,
   * which means nothing in this file was watching. Now something is.
   *
   * Both branches are driven from the same page. A card that rendered the
   * reassuring sentence unconditionally would pass the first half and
   * tell an operator with protection turned off exactly the wrong thing.
   */
  it("states plainly whether the newest known-good backup is protected", async () => {
    const api = createMockApi();
    renderDetail("production", "postgres-primary", api);

    await screen.findByText(/Newest known-good backup is protected from deletion/);
    expect(screen.queryByText(/is NOT protected from deletion/)).toBeNull();
  });

  it("says so when the policy in force does not protect the newest known-good backup", async () => {
    const api = createMockApi();
    const real = api.getBackupSetRetention.bind(api);
    api.getBackupSetRetention = async (source: string, set: string) => {
      const r = await real(source, set);
      return { ...r, effective: { ...r.effective, protectLastKnownGood: false } };
    };
    renderDetail("production", "postgres-primary", api);

    await screen.findByText(/Newest known-good backup is NOT protected from deletion/);
    expect(screen.queryByText(/is protected from deletion$/)).toBeNull();
  });

  it("shows an overriding set the deployment's chain beside its own, which is what clearing would go back to", async () => {
    renderDetail("media", "weekly-archive", createMockApi());

    const disclosure = await screen.findByText(/The deployment's policy, which this set would go back to/);
    fireEvent.click(disclosure);
    // The deployment's daily tier is not in this set's own chain, so
    // finding it is evidence the OTHER policy is being rendered rather
    // than the same one twice.
    await screen.findByText("daily");
  });

  it("does not show an inheriting set the same policy twice", async () => {
    renderDetail("production", "postgres-primary", createMockApi());

    await screen.findByText(/Retained under the deployment's retention policy/);
    expect(screen.queryByText(/which this set would go back to/)).toBeNull();
  });

  it("starts a new override from the deployment's whole chain, so a first save cannot be half a policy", async () => {
    const api = createMockApi();
    const submitted: RetentionOverride[] = [];
    const spy = vi.spyOn(api, "setBackupSetRetention").mockImplementation((source, set, policy) => {
      submitted.push(policy);
      return createMockApi().setBackupSetRetention(source, set, policy);
    });

    renderDetail("production", "postgres-primary", api);
    fireEvent.click(await screen.findByRole("button", { name: "Give this set its own policy" }));

    // The editor opened on the deployment's three tiers, not on nothing.
    await screen.findByRole("group", { name: "Tier 3" });

    fireEvent.click(screen.getByRole("button", { name: "Save this set's policy" }));

    await waitFor(() => expect(spy).toHaveBeenCalled());
    expect(submitted).toHaveLength(1);
    const policy = submitted[0];
    // A WHOLE chain, every time. Half of one is what config.Validate
    // refuses, and this form must not be able to produce one at all.
    expect(policy.tiers).toHaveLength(3);
    // And no calendar: an override that names none inherits the
    // deployment's, which is the state a set that has never overridden is
    // already in. Pinning it here would silently stop this set following
    // a later change to the deployment's timezone.
    expect(policy.timezone).toBeUndefined();
    expect(policy.weekStartsOn).toBeUndefined();
    expect(policy.protectLastKnownGood).toBeUndefined();
  });

  it("carries a tier's storage medium back out untouched, so a save does not delete it", async () => {
    const api = createMockApi();
    const submitted: RetentionOverride[] = [];
    vi.spyOn(api, "setBackupSetRetention").mockImplementation((source, set, policy) => {
      submitted.push(policy);
      return createMockApi().setBackupSetRetention(source, set, policy);
    });

    renderDetail("production", "postgres-primary", api);
    fireEvent.click(await screen.findByRole("button", { name: "Give this set its own policy" }));

    const monthly = await screen.findByRole("group", { name: "Tier 3" });
    // Change something else entirely, which is the whole point: a lossy
    // editor deletes the medium as a side effect of an unrelated edit.
    fireEvent.change(within(monthly).getByLabelText("Keep"), { target: { value: "24" } });
    fireEvent.click(screen.getByRole("button", { name: "Save this set's policy" }));

    await waitFor(() => expect(submitted).toHaveLength(1));
    const tiers = submitted[0].tiers ?? [];
    expect(tiers[2]?.keep).toBe(24);
    expect(tiers[2]?.medium).toBe("offsite_s3");
    // And a mapping the set already inherits is not a decision about
    // where backups live, so no consent was asked for or sent.
    expect(submitted[0].acknowledgeMediumDisclosure).toBeUndefined();
    expect(screen.queryByRole("group", { name: "Storage medium disclosure" })).toBeNull();
  });

  it("pins the calendar only when the operator turns inheritance off", async () => {
    const api = createMockApi();
    const submitted: RetentionOverride[] = [];
    vi.spyOn(api, "setBackupSetRetention").mockImplementation((source, set, policy) => {
      submitted.push(policy);
      return createMockApi().setBackupSetRetention(source, set, policy);
    });

    renderDetail("production", "postgres-primary", api);
    fireEvent.click(await screen.findByRole("button", { name: "Give this set its own policy" }));

    const inherit = await screen.findByRole("checkbox", {
      name: /Reckon this policy in the deployment.s calendar/
    });
    // The label has to name what inheriting currently resolves to, or an
    // operator cannot tell it from "UTC by default", which is the exact
    // confusion the config layer refuses to allow one level down.
    expect(screen.getByText(/Currently Europe\/Berlin, weeks start monday/)).toBeTruthy();

    fireEvent.click(inherit);
    fireEvent.change(screen.getByLabelText("Timezone"), { target: { value: "America/Vancouver" } });
    fireEvent.click(screen.getByRole("button", { name: "Save this set's policy" }));

    await waitFor(() => expect(submitted).toHaveLength(1));
    expect(submitted[0].timezone).toBe("America/Vancouver");
    expect(submitted[0].weekStartsOn).toBe("monday");
    expect(submitted[0].protectLastKnownGood).toBe(true);
  });

  it("cannot empty the chain, because an empty chain is not 'keep nothing'", async () => {
    renderDetail("media", "weekly-archive", createMockApi());
    fireEvent.click(await screen.findByRole("button", { name: "Edit this set's policy" }));

    // This set's own chain has two tiers, so both Remove buttons are
    // live; removing one has to disable the other.
    fireEvent.click(await screen.findByRole("button", { name: "Remove tier 2" }));
    const last = await screen.findByRole("button", { name: "Remove tier 1" });
    expect((last as HTMLButtonElement).disabled).toBe(true);
  });

  it("will not save a policy nothing changed, so opening the editor cannot rewrite the file", async () => {
    const api = createMockApi();
    const spy = vi.spyOn(api, "setBackupSetRetention");

    renderDetail("media", "weekly-archive", api);
    fireEvent.click(await screen.findByRole("button", { name: "Edit this set's policy" }));

    const save = await screen.findByRole("button", { name: "Save this set's policy" });
    expect((save as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(save);
    expect(spy).not.toHaveBeenCalled();
  });

  it("confirms before returning a set to the deployment's policy, and shows both chains first", async () => {
    const api = createMockApi();
    renderDetail("media", "weekly-archive", api);

    fireEvent.click(await screen.findByRole("button", { name: "Return to the deployment's policy" }));

    const confirm = await screen.findByRole("dialog");
    expect(within(confirm).getByText(/This set’s own policy, now/)).toBeTruthy();
    expect(within(confirm).getByText(/The deployment’s policy, after/)).toBeTruthy();
    // The direction that is not obvious: going back can retain LESS.
    expect(within(confirm).getByText(/widens what a later retention apply may delete/)).toBeTruthy();
  });

  it("returns the set to the deployment's policy, and the page says so afterwards", async () => {
    const api = createMockApi();
    renderDetail("media", "weekly-archive", api);

    fireEvent.click(await screen.findByRole("button", { name: "Return to the deployment's policy" }));
    const confirm = await screen.findByRole("dialog");
    fireEvent.click(within(confirm).getByRole("button", { name: "Return to the deployment's policy" }));

    await screen.findByText(/Retained under the deployment's retention policy/);
    // The control is gone, because there is nothing left to clear.
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Return to the deployment's policy" })).toBeNull()
    );
  });

  it("shows the server's own refusal rather than a wording of its own", async () => {
    const api = createMockApi();
    vi.spyOn(api, "setBackupSetRetention").mockRejectedValue(
      new BackupManagerError({
        code: "INVALID_REQUEST",
        message:
          "invalid config: sources[0].backup_sets[0].retention: a backup set's own policy replaces the deployment's whole chain",
        correlationId: "cid_test"
      })
    );

    renderDetail("production", "postgres-primary", api);
    fireEvent.click(await screen.findByRole("button", { name: "Give this set its own policy" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save this set's policy" }));

    await screen.findByText(/replaces the deployment's whole chain/);
  });

  it("offers no write control at all in read-only mode", async () => {
    renderDetail("media", "weekly-archive", createMockApi(), true);

    await screen.findByText(/Retained under this backup set's own policy/);
    expect((screen.getByRole("button", { name: "Edit this set's policy" }) as HTMLButtonElement).disabled).toBe(true);
    expect(
      (screen.getByRole("button", { name: "Return to the deployment's policy" }) as HTMLButtonElement).disabled
    ).toBe(true);
  });
});

describe("the retention preview says which policy decided it", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("names the deployment's policy for an inheriting set and the set's own for an overriding one", async () => {
    renderDetail("production", "postgres-primary", createMockApi());
    fireEvent.click(await screen.findByRole("button", { name: "Preview retention plan" }));
    await screen.findByText(/Decided under the deployment's retention policy/);
  });
});

/**
 * #240 (EPIC E, FR-27) on this card. An override is a whole chain, and the
 * config layer lets it send a tier's backups to a storage medium exactly as
 * the deployment's policy can, so the same picker, the same disclosure and
 * the same server-side gate stand here. What these prove is the courtesy
 * half (the words reach the operator, Save waits for the tick, the tick
 * reaches the wire) and that the gate is decided against the chain in
 * force for THIS set, not against the deployment's policy or against
 * nothing.
 */
describe("mapping one backup set's tier to a storage medium", () => {
  afterEach(() => {
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  const tier = (n: number) => within(screen.getByRole("group", { name: "Tier " + n }));
  const panel = () => screen.getByRole("group", { name: "Storage medium disclosure" });
  const save = () => screen.getByRole("button", { name: "Save this set's policy" });

  async function openEditor(api: BackupManagerApi, source = "production", set = "postgres-primary") {
    renderDetail(source, set, api);
    fireEvent.click(await screen.findByRole("button", { name: "Give this set its own policy" }));
    await screen.findByRole("group", { name: "Tier 1" });
  }

  it("offers the same medium picker the Settings page has, naming each medium's class and which one needs a restore", async () => {
    await openEditor(createMockApi());

    const picker = tier(1).getByLabelText("Storage medium for tier 1") as HTMLSelectElement;
    const options = Array.from(picker.options).map((o) => o.textContent);
    expect(options).toContain("Local backup root");
    expect(options).toContain("offsite_s3 (STANDARD_IA)");
    expect(options).toContain("offsite_cold (DEEP_ARCHIVE, needs a restore to read)");
    // The inherited mapping is what the picker starts on: the editor
    // pre-fills from the chain in force, medium included.
    expect((tier(3).getByLabelText("Storage medium for tier 3") as HTMLSelectElement).value).toBe("offsite_s3");
  });

  it("offers no medium picker at all for a deployment that never heard of storage mediums", async () => {
    await openEditor(createMockApi("no-medium"));

    expect(tier(1).queryByLabelText("Storage medium for tier 1")).toBeNull();
    expect(screen.queryByRole("group", { name: "Storage medium disclosure" })).toBeNull();
  });

  it("shows the deletion consequence in the backend's words, keeps Save disabled until it is acknowledged, and then sends the acknowledgment", async () => {
    const api = createMockApi();
    const submitted: RetentionOverride[] = [];
    vi.spyOn(api, "setBackupSetRetention").mockImplementation((source, set, policy) => {
      submitted.push(policy);
      return createMockApi().setBackupSetRetention(source, set, policy);
    });
    await openEditor(api);

    fireEvent.change(tier(1).getByLabelText("Storage medium for tier 1"), { target: { value: "offsite_s3" } });

    const words = panel().textContent ?? "";
    expect(words).toMatch(/daily/);
    expect(words).toMatch(/offsite_s3/);
    expect(words).toMatch(/I delete the copy on this machine/);
    expect(words).toMatch(/billed by your provider/i);
    // No figure comes with it (rclone-manager#211).
    expect(words).not.toMatch(/\$\s?\d/);
    // The chain is edited and valid, and Save still waits for the tick.
    expect((save() as HTMLButtonElement).disabled).toBe(true);

    fireEvent.click(within(panel()).getByRole("checkbox"));
    expect((save() as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(save());

    await waitFor(() => expect(submitted).toHaveLength(1));
    expect(submitted[0].acknowledgeMediumDisclosure).toBe(true);
    expect(submitted[0].tiers?.[0]?.medium).toBe("offsite_s3");
    // And the page re-renders from the answer: the set now overrides.
    await screen.findByText(/Retained under this backup set's own policy/);
  });

  it("forgets an acknowledgment given for a different destination, and says when that destination cannot be read on demand", async () => {
    await openEditor(createMockApi());
    const picker = tier(1).getByLabelText("Storage medium for tier 1");

    fireEvent.change(picker, { target: { value: "offsite_s3" } });
    fireEvent.click(within(panel()).getByRole("checkbox"));
    expect((save() as HTMLButtonElement).disabled).toBe(false);

    fireEvent.change(picker, { target: { value: "offsite_cold" } });
    expect((save() as HTMLButtonElement).disabled).toBe(true);
    expect((within(panel()).getByRole("checkbox") as HTMLInputElement).checked).toBe(false);
    expect(panel().textContent).toMatch(/cannot be read on demand at all/);
    expect(panel().textContent).not.toMatch(/\b\d+\s*(hours|minutes)\b/);
  });

  it("shows the server's refusal in the server's own words when it still disagrees about consent", async () => {
    const api = createMockApi();
    vi.spyOn(api, "setBackupSetRetention").mockRejectedValue(
      new BackupManagerError({
        code: "MEDIUM_DISCLOSURE_REQUIRED",
        message:
          "This write sends daily -> offsite_s3. After a backup uploads and I verify it, I delete the copy on this machine.",
        correlationId: "cid_test"
      })
    );
    await openEditor(api);

    fireEvent.change(tier(1).getByLabelText("Storage medium for tier 1"), { target: { value: "offsite_s3" } });
    fireEvent.click(within(panel()).getByRole("checkbox"));
    fireEvent.click(save());

    // The refusal's own first sentence, which the disclosure panel above
    // it does not contain, so this is the server's message being rendered
    // and not the panel's copy of the same paragraph.
    await screen.findByText(/This write sends daily -> offsite_s3/);
    // Still in the editor, nothing pretended to succeed.
    expect(screen.queryByText(/Retained under this backup set's own policy/)).toBeNull();
  });

  it("says where a tier's backups go, in the policy it renders and in the deployment's beside it", async () => {
    renderDetail("media", "weekly-archive", createMockApi());

    await screen.findByText(/Retained under this backup set's own policy/);
    expect(screen.getByText("keeps 24 months, on offsite_cold")).toBeTruthy();
    fireEvent.click(screen.getByText(/The deployment's policy, which this set would go back to/));
    await screen.findByText("keeps 12 months, on offsite_s3");
    // A local tier says nothing about where, which is how the file
    // spells local too.
    expect(screen.getByText("keeps 8 weeks")).toBeTruthy();
  });
});
