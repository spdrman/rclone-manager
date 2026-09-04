import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SettingsPage } from "@shared/pages/SettingsPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { BackupManagerError } from "@shared/api/contracts";
import type { AppSettings, UpdateSettingsRequest } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import type { VersionInfo } from "@shared/types/operation";

/**
 * B3.7 (#140) — the retention policy form on the Settings page, and the
 * confirmation that stands in front of disabling FR-19's last-known-good
 * protection.
 *
 * The form targets issue #156's generalized chain (an operator-configured
 * ordered list of named tiers), not the three scalars it replaced, and
 * every closed value set it renders comes from the schema the endpoint
 * serves alongside the values — see `SCHEMA` below, which is exactly what
 * apps/common/webhost puts on the wire.
 */

const VERSION: VersionInfo = {
  api: "v1", service: "1.3.0", buildCommit: "9f4c1ab", goVersion: "go1.27.0",
  engine: "1.68.2", configRevision: "cfg_9f4c1ab", ready: true, compatible: true
};

const SCHEMA = {
  granularities: ["day", "week", "month", "quarter", "half_year", "year", "days"],
  windowUnits: ["day", "week", "month", "quarter", "half_year", "year"],
  tierNamePattern: "^[a-z][a-z0-9_]*$",
  reservedTierName: "last_known_good",
  keepMax: 10000,
  periodDaysMax: 3650,
  defaultTiers: [
    { name: "daily", granularity: "day", keep: 7 },
    { name: "weekly", granularity: "week", keep: 3, windowUnit: "month" },
    { name: "monthly", granularity: "month", keep: 12 }
  ]
};

/** This file is about the retention form only; every fixture here carries
 *  the product default for capacity (no cap, no thresholds), the same
 *  shape CapacityCard's own suite exercises in isolation. */
function defaultCapacityFixture(): AppSettings["capacity"] {
  return {
    capBytes: 0,
    warningFreeBytes: 0,
    criticalFreeBytes: 0,
    safetyMarginBytes: 0,
    backupRoot: "/data/backups",
    backupRootConfigured: false
  };
}

/** The ladder and the two disclosures, as core/service serves them.
 *  Reproduced here rather than imported because these tests stand in for
 *  a SERVER, and a fixture that invented different words would hide the
 *  drift the real surface exists to prevent. */
export const STORAGE_SCHEMA: AppSettings["schema"]["storage"] = {
  verificationClasses: [
    {
      className: "content",
      proves: "the bytes on the medium hash to the hash this product recorded when it ingested the artifact",
      requires: "a full download of the object: time plus egress, and for an archive storage class a restore first",
      downloadsObject: true
    },
    {
      className: "attested",
      proves: "the provider's stored full-object checksum equals the recorded hash",
      requires: "one metadata call, no egress, trusting the endpoint's own checksum",
      downloadsObject: false
    },
    {
      className: "existence",
      proves: "an object exists at the recorded key, at the recorded size",
      requires: "one HEAD request, which says nothing about the bytes",
      downloadsObject: false
    }
  ],
  mediumDisclosure:
    "Backups that only this tier keeps will live only on that storage medium. " +
    "After a backup uploads and I verify it, I delete the copy on this machine.",
  retrievalDisclosure:
    "Reading a copy back off a storage medium is billed by your provider. " +
    "I hold no price list and no knowledge of your rates."
};

function settingsFixture(
  overrides: Partial<AppSettings["retention"]> = {},
  schema: AppSettings["schema"]["retention"] = SCHEMA,
  mediums: AppSettings["mediums"] = []
): AppSettings {
  return {
    retention: {
      timezone: "Europe/Berlin",
      weekStartsOn: "monday",
      tiers: [
        { name: "daily", granularity: "day", keep: 7 },
        { name: "weekly", granularity: "week", keep: 3, windowUnit: "month" },
        { name: "monthly", granularity: "month", keep: 12 }
      ],
      protectLastKnownGood: true,
      ...overrides
    },
    capacity: defaultCapacityFixture(),
    mediums,
    schema: { retention: schema, storage: STORAGE_SCHEMA }
  };
}

interface Harness {
  updateSettings: ReturnType<typeof vi.fn>;
  getSettings: ReturnType<typeof vi.fn>;
}

async function renderSettings(
  options: {
    settings?: AppSettings;
    readOnly?: boolean;
    updateSettings?: (req: UpdateSettingsRequest) => Promise<AppSettings>;
  } = {}
): Promise<Harness> {
  const settings = options.settings ?? settingsFixture();
  const getSettings = vi.fn(() => Promise.resolve(settings));
  const updateSettings = vi.fn(
    options.updateSettings ?? (() => Promise.resolve(settings))
  );
  const api = { ...createMockApi(), getSettings, updateSettings };

  act(() => {
    graph.commit("test/seed-version", (tx) =>
      tx.set(versionNode, { data: VERSION, error: null, loading: false })
    );
  });

  render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <PlatformProvider bridge={genericBridge}>
          <SettingsPage readOnly={options.readOnly ?? false} />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
  await act(async () => {});
  return { updateSettings, getSettings };
}

const tier = (n: number) => within(screen.getByRole("group", { name: "Tier " + n }));
const save = () => screen.getByRole("button", { name: "Save retention policy" });

describe("SettingsPage retention policy form", () => {
  beforeEach(() => {
    resetGraphForTests();
  });
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  it("renders the running chain, window_unit included", async () => {
    await renderSettings();

    expect((tier(1).getByLabelText("Name") as HTMLInputElement).value).toBe("daily");
    expect((tier(1).getByLabelText("Granularity") as HTMLSelectElement).value).toBe("day");
    expect((tier(1).getByLabelText("Keep") as HTMLInputElement).value).toBe("7");

    // window_unit is load-bearing: the default weekly tier buckets by week
    // and looks back over calendar MONTHS. A form that dropped it could
    // not express the default policy at all, and every save would quietly
    // narrow the weekly window from three months to three weeks.
    expect((tier(2).getByLabelText("Window unit") as HTMLSelectElement).value).toBe("month");
    // The control: a tier that does NOT set one renders as "same as
    // granularity" rather than inventing a unit.
    expect((tier(1).getByLabelText("Window unit") as HTMLSelectElement).value).toBe("");

    expect((screen.getByLabelText("Timezone") as HTMLInputElement).value).toBe("Europe/Berlin");
    expect((screen.getByLabelText("Week starts on") as HTMLSelectElement).value).toBe("monday");
  });

  it("offers exactly the granularities the server validates against, and never offers the custom period as a window unit", async () => {
    await renderSettings();

    const granularity = tier(1).getByLabelText("Granularity") as HTMLSelectElement;
    expect([...granularity.options].map((o) => o.value)).toEqual(SCHEMA.granularities);

    const windowUnit = tier(1).getByLabelText("Window unit") as HTMLSelectElement;
    const offered = [...windowUnit.options].map((o) => o.value);
    // "" is the "same as granularity" default; every other option must be
    // a legal window unit, and "days" is never one.
    expect(offered).toEqual(["", ...SCHEMA.windowUnits]);
    expect(offered).not.toContain("days");
  });

  it("sends the whole edited chain, and omits every field the operator did not touch", async () => {
    const { updateSettings } = await renderSettings();

    fireEvent.change(tier(1).getByLabelText("Keep"), { target: { value: "10" } });
    fireEvent.click(save());
    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));

    const req = updateSettings.mock.calls[0][0] as UpdateSettingsRequest;
    expect(req.retention?.tiers).toEqual([
      { name: "daily", granularity: "day", keep: 10, periodDays: undefined, windowUnit: undefined },
      { name: "weekly", granularity: "week", keep: 3, periodDays: undefined, windowUnit: "month" },
      { name: "monthly", granularity: "month", keep: 12, periodDays: undefined, windowUnit: undefined }
    ]);
    // Untouched scalars are absent, not resent: the endpoint reads an
    // absent key as "leave this alone".
    expect(req.retention).not.toHaveProperty("timezone");
    expect(req.retention).not.toHaveProperty("weekStartsOn");
    expect(req.retention).not.toHaveProperty("protectLastKnownGood");
  });

  it("omits the chain entirely when only a scalar changed, so a legacy config keeps its own spelling", async () => {
    const { updateSettings } = await renderSettings();

    fireEvent.change(screen.getByLabelText("Timezone"), { target: { value: "UTC" } });
    fireEvent.click(save());
    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));

    const req = updateSettings.mock.calls[0][0] as UpdateSettingsRequest;
    expect(req.retention?.timezone).toBe("UTC");
    // The whole point: sending an unchanged chain back would rewrite a
    // daily_days/weekly_months/monthly_months config into the general
    // tiers spelling for a change that had nothing to do with it.
    expect(req.retention).not.toHaveProperty("tiers");
  });

  it("adds and removes tiers, and never lets the chain be emptied", async () => {
    const { updateSettings } = await renderSettings();

    fireEvent.click(screen.getByRole("button", { name: "Add tier" }));
    expect(screen.getByRole("group", { name: "Tier 4" })).toBeTruthy();

    fireEvent.change(tier(4).getByLabelText("Name"), { target: { value: "annual" } });
    fireEvent.change(tier(4).getByLabelText("Granularity"), { target: { value: "year" } });
    fireEvent.change(tier(4).getByLabelText("Keep"), { target: { value: "5" } });

    fireEvent.click(tier(1).getByRole("button", { name: "Remove tier 1" }));
    fireEvent.click(tier(1).getByRole("button", { name: "Remove tier 1" }));
    fireEvent.click(tier(1).getByRole("button", { name: "Remove tier 1" }));

    // One tier left, and its remove control is disabled: an empty chain
    // does not mean "keep nothing", it reinstates the default 7/3/12
    // policy, so the form never offers the operator that misreading.
    expect(screen.queryByRole("group", { name: "Tier 2" })).toBeNull();
    expect(tier(1).getByRole("button", { name: "Remove tier 1" })).toHaveProperty("disabled", true);
    expect(
      screen.getByText(/reinstates the default daily\/weekly\/monthly policy/i)
    ).toBeTruthy();
    expect(screen.getByText(/not running a retention pass/i)).toBeTruthy();

    fireEvent.click(save());
    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));
    const req = updateSettings.mock.calls[0][0] as UpdateSettingsRequest;
    expect(req.retention?.tiers).toEqual([
      { name: "annual", granularity: "year", keep: 5, periodDays: undefined, windowUnit: undefined }
    ]);
  });

  it("restores the default chain through a positive control rather than by emptying the list", async () => {
    // Started from a policy that is NOT the default, so restoring it is a
    // real change with something to save. (Restoring the default onto a
    // config that already runs it correctly leaves nothing to write, and
    // Save stays disabled — which is the next assertion below.)
    const { updateSettings } = await renderSettings({
      settings: settingsFixture({ tiers: [{ name: "annual", granularity: "year", keep: 5 }] })
    });

    fireEvent.click(screen.getByRole("button", { name: "Restore default chain" }));

    expect((tier(1).getByLabelText("Keep") as HTMLInputElement).value).toBe("7");
    // The default weekly tier's window unit survives the restore: without
    // it the restored policy would look right and silently narrow the
    // weekly window from three months to three weeks.
    expect((tier(2).getByLabelText("Window unit") as HTMLSelectElement).value).toBe("month");
    expect((tier(3).getByLabelText("Keep") as HTMLInputElement).value).toBe("12");

    fireEvent.click(save());
    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));
    const req = updateSettings.mock.calls[0][0] as UpdateSettingsRequest;
    expect(req.retention?.tiers?.map((t) => [t.name, t.keep, t.windowUnit])).toEqual([
      ["daily", 7, undefined],
      ["weekly", 3, "month"],
      ["monthly", 12, undefined]
    ]);
  });

  it("restores the default chain the SERVER serves, not one written into this file", async () => {
    // Mandatory review finding M5 on PR #171: defaultChain() used to
    // return a literal 7/3/12 chain, a second spelling of something
    // config.DefaultTierChain's own doc says has exactly one. A stale copy
    // there does not merely display the wrong thing. Saving it writes an
    // explicit tiers list, which clears the legacy scalars and permanently
    // migrates a config that would have tracked the product's default onto
    // a frozen, possibly NARROWER, policy.
    //
    // The served default here is deliberately unreal, so a component that
    // still hardcoded 7/3/12 fails this and the assertion above passes for
    // both. That is what makes the pair a measurement: the previous test
    // proves the real default round-trips, this one proves the value came
    // from the schema.
    const { updateSettings } = await renderSettings({
      settings: settingsFixture({ tiers: [{ name: "annual", granularity: "year", keep: 5 }] }, {
        ...SCHEMA,
        defaultTiers: [
          { name: "hourly", granularity: "days", periodDays: 1, keep: 48 },
          { name: "biennial", granularity: "year", keep: 2, windowUnit: "year" }
        ]
      })
    });

    fireEvent.click(screen.getByRole("button", { name: "Restore default chain" }));

    expect(screen.queryByRole("group", { name: "Tier 3" })).toBeNull();
    expect((tier(1).getByLabelText("Name") as HTMLInputElement).value).toBe("hourly");
    expect((tier(1).getByLabelText("Keep") as HTMLInputElement).value).toBe("48");

    fireEvent.click(save());
    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));
    const req = updateSettings.mock.calls[0][0] as UpdateSettingsRequest;
    expect(req.retention?.tiers?.map((t) => [t.name, t.keep, t.periodDays, t.windowUnit])).toEqual([
      ["hourly", 48, 1, undefined],
      ["biennial", 2, undefined, "year"]
    ]);
  });

  it("has nothing to save until something actually changes", async () => {
    // The control for every "it sent exactly this" assertion above: Save
    // is inert on an untouched form, so a request in those tests is
    // evidence of the edit rather than of a button that always fires.
    const { updateSettings } = await renderSettings();
    expect(save()).toHaveProperty("disabled", true);

    fireEvent.click(save());
    await act(async () => {});
    expect(updateSettings).not.toHaveBeenCalled();

    fireEvent.change(tier(1).getByLabelText("Keep"), { target: { value: "8" } });
    expect(save()).toHaveProperty("disabled", false);

    // And editing back to the loaded value makes it inert again, so
    // "dirty" is a real comparison rather than a one-way latch.
    fireEvent.change(tier(1).getByLabelText("Keep"), { target: { value: "7" } });
    expect(save()).toHaveProperty("disabled", true);
  });

  it("shows and clears the custom period field with the granularity that needs it", async () => {
    const { updateSettings } = await renderSettings();

    expect(tier(1).queryByLabelText("Period (days)")).toBeNull();

    fireEvent.change(tier(1).getByLabelText("Granularity"), { target: { value: "days" } });
    fireEvent.change(tier(1).getByLabelText("Name"), { target: { value: "fortnightly" } });
    fireEvent.change(tier(1).getByLabelText("Period (days)"), { target: { value: "14" } });
    // A custom period measures its own window, so it must not also carry
    // a window unit — the server refuses that combination outright.
    expect(tier(1).queryByLabelText("Window unit")).toBeNull();

    fireEvent.click(save());
    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));
    const sent = (updateSettings.mock.calls[0][0] as UpdateSettingsRequest).retention?.tiers?.[0];
    expect(sent).toEqual({
      name: "fortnightly", granularity: "days", keep: 7, periodDays: 14, windowUnit: undefined
    });

    // Switching back off the custom period drops period_days rather than
    // sending a stray value the server refuses on every other granularity.
    fireEvent.change(tier(1).getByLabelText("Granularity"), { target: { value: "week" } });
    expect(tier(1).queryByLabelText("Period (days)")).toBeNull();
    fireEvent.click(save());
    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(2));
    const resent = (updateSettings.mock.calls[1][0] as UpdateSettingsRequest).retention?.tiers?.[0];
    expect(resent?.periodDays).toBeUndefined();
  });

  describe("client-side validation against the served schema", () => {
    const cases: { name: string; edit: () => void; message: RegExp }[] = [
      {
        name: "a tier name outside lower_snake_case",
        edit: () => fireEvent.change(tier(1).getByLabelText("Name"), { target: { value: "Daily" } }),
        message: /^Tier names are lower_snake_case/
      },
      {
        name: "the reserved last-known-good name",
        edit: () =>
          fireEvent.change(tier(1).getByLabelText("Name"), { target: { value: "last_known_good" } }),
        message: /is reserved for last-known-good protection/
      },
      {
        name: "a duplicate tier name",
        edit: () => fireEvent.change(tier(1).getByLabelText("Name"), { target: { value: "weekly" } }),
        message: /is already used by tier/
      },
      {
        name: "a zero keep window",
        edit: () => fireEvent.change(tier(1).getByLabelText("Keep"), { target: { value: "0" } }),
        message: /^Keep at least 1 look-back unit/
      },
      {
        name: "a keep window past the ceiling",
        edit: () =>
          fireEvent.change(tier(1).getByLabelText("Keep"), { target: { value: String(SCHEMA.keepMax + 1) } }),
        message: new RegExp(String(SCHEMA.keepMax))
      }
    ];

    for (const c of cases) {
      it("refuses " + c.name + " before it is ever sent", async () => {
        const { updateSettings } = await renderSettings();
        c.edit();

        expect(screen.getByText(c.message)).toBeTruthy();
        expect(save()).toHaveProperty("disabled", true);

        fireEvent.click(save());
        await act(async () => {});
        expect(updateSettings).not.toHaveBeenCalled();
      });
    }

    it("positive control: the same fields, spelled legally, save", async () => {
      const { updateSettings } = await renderSettings();

      fireEvent.change(tier(1).getByLabelText("Name"), { target: { value: "hourly_ish" } });
      fireEvent.change(tier(1).getByLabelText("Keep"), { target: { value: String(SCHEMA.keepMax) } });

      expect(save()).toHaveProperty("disabled", false);
      fireEvent.click(save());
      await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));
    });
  });

  describe("disabling last-known-good protection", () => {
    const lkg = () =>
      screen.getByRole("checkbox", { name: /Protect the newest known-good backup/ });

    it("warns, and does not write, until the operator confirms", async () => {
      const { updateSettings } = await renderSettings();

      fireEvent.click(lkg());

      // The inline warning appears the moment the toggle moves, ahead of
      // any save, in internal/retention's own words.
      expect(screen.getByText(/materially more dangerous configuration/i)).toBeTruthy();

      fireEvent.click(save());

      const dialog = await screen.findByRole("dialog", { name: /last-known-good/i });
      expect(within(dialog).getByText(/materially more dangerous configuration/i)).toBeTruthy();
      // Nothing has been written yet: the confirmation stands BEFORE the
      // change takes effect, which is #111's own acceptance criterion.
      expect(updateSettings).not.toHaveBeenCalled();

      fireEvent.click(within(dialog).getByRole("button", { name: /Disable protection/i }));
      await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));
      const req = updateSettings.mock.calls[0][0] as UpdateSettingsRequest;
      expect(req.retention?.protectLastKnownGood).toBe(false);
    });

    it("cancelling the confirmation writes nothing and leaves the toggle where the operator left it", async () => {
      const { updateSettings } = await renderSettings();

      fireEvent.click(lkg());
      fireEvent.click(save());

      const dialog = await screen.findByRole("dialog", { name: /last-known-good/i });
      fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));

      await act(async () => {});
      expect(updateSettings).not.toHaveBeenCalled();
      expect(screen.queryByRole("dialog", { name: /last-known-good/i })).toBeNull();
      expect((lkg() as HTMLInputElement).checked).toBe(false);
    });

    it("does not confirm a save that leaves protection on, or one that turns it back on", async () => {
      // Positive control for the two tests above: the confirmation is
      // specific to the dangerous direction, not shown on every save.
      const { updateSettings } = await renderSettings();
      fireEvent.change(tier(1).getByLabelText("Keep"), { target: { value: "10" } });
      fireEvent.click(save());
      await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));
      expect(screen.queryByRole("dialog", { name: /last-known-good/i })).toBeNull();

      cleanup();

      const off = await renderSettings({ settings: settingsFixture({ protectLastKnownGood: false }) });
      fireEvent.click(screen.getByRole("checkbox", { name: /Protect the newest known-good backup/ }));
      fireEvent.click(screen.getByRole("button", { name: "Save retention policy" }));
      await waitFor(() => expect(off.updateSettings).toHaveBeenCalledTimes(1));
      expect(screen.queryByRole("dialog", { name: /last-known-good/i })).toBeNull();
      const req = off.updateSettings.mock.calls[0][0] as UpdateSettingsRequest;
      expect(req.retention?.protectLastKnownGood).toBe(true);
    });

    it("keeps warning while protection is already off in the saved config", async () => {
      await renderSettings({ settings: settingsFixture({ protectLastKnownGood: false }) });
      expect(screen.getByText(/materially more dangerous configuration/i)).toBeTruthy();
    });
  });

  it("surfaces a server refusal without pretending the write succeeded", async () => {
    const { updateSettings } = await renderSettings({
      updateSettings: () =>
        Promise.reject(
          new BackupManagerError({
            code: "INVALID_REQUEST",
            message: "retention.tiers[0]: keep must be a positive number of look-back units (got 0)",
            correlationId: "cid_test400"
          })
        )
    });

    fireEvent.change(tier(1).getByLabelText("Keep"), { target: { value: "10" } });
    fireEvent.click(save());
    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));

    expect(await screen.findByText(/keep must be a positive number of look-back units/)).toBeTruthy();
    expect(screen.queryByText(/Retention policy saved/)).toBeNull();
  });

  it("confirms a successful save", async () => {
    await renderSettings();
    fireEvent.change(tier(1).getByLabelText("Keep"), { target: { value: "10" } });
    fireEvent.click(save());
    expect(await screen.findByText(/Retention policy saved/)).toBeTruthy();
  });

  it("renders read-only without any writable control when the service is incompatible", async () => {
    await renderSettings({ readOnly: true });

    expect(save()).toHaveProperty("disabled", true);
    expect(screen.getByRole("button", { name: "Add tier" })).toHaveProperty("disabled", true);
    expect(tier(1).getByLabelText("Name")).toHaveProperty("disabled", true);
    expect(tier(1).getByLabelText("Keep")).toHaveProperty("disabled", true);
    expect(
      screen.getByRole("checkbox", { name: /Protect the newest known-good backup/ })
    ).toHaveProperty("disabled", true);
  });

  it("shows an error instead of an empty form when the settings read fails", async () => {
    // RetentionPolicyCard and CapacityCard each fetch getSettings()
    // independently (like the real per-set health/status cards, neither
    // shares the other's request), so a read failure here is not one
    // card's problem: both show it, honestly, rather than one failing
    // silently while the other renders as if nothing were wrong.
    const getSettings = vi.fn(() =>
      Promise.reject(
        new BackupManagerError({
          code: "INTERNAL",
          message: "failed to read settings",
          correlationId: "cid_test500"
        })
      )
    );
    const api = { ...createMockApi(), getSettings };
    act(() => {
      graph.commit("test/seed-version", (tx) =>
        tx.set(versionNode, { data: VERSION, error: null, loading: false })
      );
    });
    render(
      <MemoryRouter>
        <ApiProvider api={api}>
          <PlatformProvider bridge={genericBridge}>
            <SettingsPage readOnly={false} />
          </PlatformProvider>
        </ApiProvider>
      </MemoryRouter>
    );
    await act(async () => {});

    const retentionCard = screen.getByText("Retention policy").closest("section");
    const capacityCard = screen.getByText("Storage capacity").closest("section");
    if (!retentionCard || !capacityCard) throw new Error("expected card sections not found");

    expect(within(retentionCard).getByText(/failed to read settings/)).toBeTruthy();
    expect(within(capacityCard).getByText(/failed to read settings/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Save retention policy" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Save storage capacity" })).toBeNull();
  });
});
