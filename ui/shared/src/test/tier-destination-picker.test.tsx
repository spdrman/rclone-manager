import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SettingsPage } from "@shared/pages/SettingsPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type {
  AppSettings,
  MediumPreflight,
  StorageMedium,
  StorageMediumUsage,
  UpdateSettingsRequest
} from "@shared/api/contracts";
import { LOCAL_DESTINATION_ID } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import type { VersionInfo } from "@shared/types/operation";

/**
 * H2.2 (#622): picking a destination under a retention tier, the local
 * hard drive as an entry an operator can see and test, and the default
 * destination.
 *
 * These drive the real Settings page against a mock API rather than
 * rendering TierRow on its own, because every claim here is about what an
 * operator can reach: an option in a menu, a button beside a row, and the
 * request that leaves when they press it. A component test would prove
 * the component and not the page it has to be reachable from.
 *
 * The load-bearing case is the last one. The picker WRITES through a
 * settings save that replaces the whole retention chain, so the
 * interesting failure is not "the menu is missing an option", it is a
 * save that moves the tier the operator picked and quietly moves another
 * one too.
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
  defaultTiers: [{ name: "daily", granularity: "day", keep: 7, medium: LOCAL_DESTINATION_ID }]
};

const STORAGE = {
  verificationClasses: [],
  mediumDisclosure: "Backups that only this tier keeps will live only on that storage medium.",
  retrievalDisclosure: "Reading a copy back off a storage medium is billed by your provider."
};

/** The list every deployment has: the drive backups land on, then
 *  whatever was declared. The local entry leads, is not declared, and is
 *  the default until somebody moves it. */
const LOCAL: StorageMedium = {
  id: LOCAL_DESTINATION_ID,
  type: "local",
  bucket: "",
  path: "/srv/backups",
  storageClass: "",
  uploadVerification: "readback",
  readsRequireRestore: false,
  isLocal: true,
  isDefault: true
};

const OFFSITE: StorageMedium = {
  id: "offsite_s3", type: "s3", bucket: "nas-backups", region: "us-east-1",
  storageClass: "STANDARD_IA", uploadVerification: "readback",
  readsRequireRestore: false, isLocal: false, isDefault: false
};

function settingsFixture(over: { mediums?: StorageMedium[] } = {}): AppSettings {
  return {
    retention: {
      timezone: "UTC",
      weekStartsOn: "monday",
      tiers: [
        { name: "daily", granularity: "day", keep: 7, medium: LOCAL_DESTINATION_ID },
        { name: "monthly", granularity: "month", keep: 12, medium: LOCAL_DESTINATION_ID }
      ],
      protectLastKnownGood: true
    },
    capacity: {
      capBytes: 0, warningFreeBytes: 0, criticalFreeBytes: 0, safetyMarginBytes: 0,
      backupRoot: "/srv/backups", backupRootConfigured: false
    },
    mediums: over.mediums ?? [LOCAL, OFFSITE],
    schema: { retention: SCHEMA, storage: STORAGE }
  };
}

async function renderSettings(options: {
  settings?: AppSettings;
  updateSettings?: (req: UpdateSettingsRequest) => Promise<AppSettings>;
  preflightStorageMedium?: (id: string) => Promise<MediumPreflight>;
  getStorageMediumUsage?: (id: string) => Promise<StorageMediumUsage>;
  setDefaultStorageMedium?: (id: string) => Promise<StorageMedium>;
  listStorageMediums?: () => Promise<StorageMedium[]>;
} = {}) {
  const settings = options.settings ?? settingsFixture();
  const getSettings = vi.fn(() => Promise.resolve(settings));
  const updateSettings = vi.fn(options.updateSettings ?? (() => Promise.resolve(settings)));
  const listStorageMediums = vi.fn(options.listStorageMediums ?? (() => Promise.resolve(settings.mediums)));
  const preflightStorageMedium = vi.fn(
    options.preflightStorageMedium ??
      ((id: string) => Promise.resolve({ medium: id, ok: true, checks: [] } as MediumPreflight))
  );
  const setDefaultStorageMedium = vi.fn(
    options.setDefaultStorageMedium ?? ((id: string) => Promise.resolve({ ...OFFSITE, id, isDefault: true }))
  );
  const api = {
    ...createMockApi(),
    getSettings,
    updateSettings,
    listStorageMediums,
    preflightStorageMedium,
    setDefaultStorageMedium,
    ...(options.getStorageMediumUsage ? { getStorageMediumUsage: vi.fn(options.getStorageMediumUsage) } : {})
  };

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
  return { updateSettings, preflightStorageMedium, setDefaultStorageMedium, listStorageMediums };
}

const tier = (n: number) => within(screen.getByRole("group", { name: "Tier " + n }));
const picker = (n: number) =>
  tier(n).getByLabelText("Storage destination for tier " + n) as HTMLSelectElement;

describe("picking a destination under a retention tier (#622)", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  // The picker is there even when nothing but the local hard drive is,
  // which is the reversal #622 asks for. It used to be hidden entirely on
  // a deployment that declared no S3 medium, on the reasoning that there
  // was nowhere else for a backup to go. That reasoning was right about
  // the choices and wrong about the operator: with the picker absent
  // there was no way to find out where a tier's backups DO go, and no
  // affordance for putting them somewhere else.
  it("offers a picker on every tier, defaulting to the local hard drive", async () => {
    await renderSettings({ settings: settingsFixture({ mediums: [LOCAL] }) });

    expect(picker(1).value).toBe(LOCAL_DESTINATION_ID);
    const options = Array.from(picker(1).options).map((o) => o.textContent);
    // The drive it writes to, not merely the word "local". A picker that
    // said only "local" would leave an operator with two NAS volumes no
    // better off than before.
    expect(options.some((o) => o?.includes("/srv/backups"))).toBe(true);
  });

  it("lists every declared destination beside the local one", async () => {
    await renderSettings();

    const options = Array.from(picker(2).options).map((o) => o.value);
    expect(options).toContain(LOCAL_DESTINATION_ID);
    expect(options).toContain("offsite_s3");
  });

  // Reachable from the tier, which is the second of the three gaps the
  // issue's own comment names: the point of the picker is choosing a
  // destination without leaving the tier, and a destination chosen there
  // needs the same check before it is trusted.
  it("tests the connection to the destination a tier names, from the tier", async () => {
    const { preflightStorageMedium } = await renderSettings();

    fireEvent.change(picker(2), { target: { value: "offsite_s3" } });
    fireEvent.click(tier(2).getByRole("button", { name: /Test connection/ }));

    await waitFor(() => expect(preflightStorageMedium).toHaveBeenCalledWith("offsite_s3"));
  });

  // And on the local entry, which is #622's added acceptance: it used to
  // have nothing to test because no network was involved, and a local
  // destination can still be missing, unwritable or full.
  it("tests the connection to the local hard drive too", async () => {
    const { preflightStorageMedium } = await renderSettings({
      settings: settingsFixture({ mediums: [LOCAL] })
    });

    fireEvent.click(tier(1).getByRole("button", { name: /Test connection/ }));

    await waitFor(() => expect(preflightStorageMedium).toHaveBeenCalledWith(LOCAL_DESTINATION_ID));
  });

  // EPIC G's standing rule, on the control this issue adds. The line an
  // operator reads under the picker is the command that reproduces the
  // click, so somebody who moved one tier by clicking has read the
  // command that moves the next fifty.
  it("echoes the command that moves this tier's destination", async () => {
    await renderSettings();

    fireEvent.change(picker(2), { target: { value: "offsite_s3" } });

    // The acknowledgment is part of the line, because the server refuses
    // this write without it: a tier moving off local disk for the first
    // time is exactly what FR-27's disclosure stands in front of, and a
    // line an operator pastes has to be the line that works.
    expect(
      tier(2).getByText(
        "backup-manager settings patch --tier-medium monthly=offsite_s3 --acknowledge-medium-disclosure"
      )
    ).toBeTruthy();
  });

  // The load-bearing case. A settings save replaces the WHOLE chain, so
  // the failure worth catching is not a missing option, it is a save that
  // moves a tier nobody touched.
  it("sends the whole chain with only the picked tier moved", async () => {
    const { updateSettings } = await renderSettings();

    fireEvent.change(picker(2), { target: { value: "offsite_s3" } });
    fireEvent.click(screen.getByLabelText("Storage medium disclosure").querySelector("input[type=checkbox]")!);
    fireEvent.click(screen.getByRole("button", { name: "Save retention policy" }));

    await waitFor(() => expect(updateSettings).toHaveBeenCalled());
    const sent = (updateSettings.mock.calls[0][0] as UpdateSettingsRequest).retention!.tiers!;
    expect(sent).toHaveLength(2);
    expect(sent[0]).toMatchObject({ name: "daily", medium: LOCAL_DESTINATION_ID });
    expect(sent[1]).toMatchObject({ name: "monthly", medium: "offsite_s3" });
  });

  // A tier moved BACK has to be expressible, or the first move to S3 is
  // one-way. It is sent as the reserved local id rather than as an absent
  // field, because the backend is where that becomes the absence a
  // configuration file spells local with.
  it("sends the local hard drive by name when a tier is moved back", async () => {
    const { updateSettings } = await renderSettings({
      settings: (() => {
        const s = settingsFixture();
        s.retention.tiers[1].medium = "offsite_s3";
        return s;
      })()
    });

    fireEvent.change(picker(2), { target: { value: LOCAL_DESTINATION_ID } });
    fireEvent.click(screen.getByRole("button", { name: "Save retention policy" }));

    await waitFor(() => expect(updateSettings).toHaveBeenCalled());
    const sent = (updateSettings.mock.calls[0][0] as UpdateSettingsRequest).retention!.tiers!;
    expect(sent[1].medium).toBe(LOCAL_DESTINATION_ID);
  });

  // A new tier starts on the DEFAULT destination, which is the whole of
  // what "default" governs. It says nothing about where anything already
  // is: the tiers above it keep whatever they named.
  it("starts a newly added tier on the default destination", async () => {
    await renderSettings({
      settings: settingsFixture({
        mediums: [{ ...LOCAL, isDefault: false }, { ...OFFSITE, isDefault: true }]
      })
    });

    fireEvent.click(screen.getByRole("button", { name: "Add tier" }));

    expect(picker(3).value).toBe("offsite_s3");
    // And nothing else moved.
    expect(picker(1).value).toBe(LOCAL_DESTINATION_ID);
    expect(picker(2).value).toBe(LOCAL_DESTINATION_ID);
  });
});

describe("the storage destinations card (#622)", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  const card = () => within(screen.getByRole("region", { name: "Storage destinations" }));
  const row = (id: string) => within(screen.getByRole("group", { name: "Storage destination " + id }));

  it("always lists the local hard drive, naming the drive it writes to", async () => {
    await renderSettings({ listStorageMediums: () => Promise.resolve([LOCAL]) });

    await waitFor(() => expect(card().getByText(LOCAL_DESTINATION_ID)).toBeTruthy());
    expect(row(LOCAL_DESTINATION_ID).getByText(/\/srv\/backups/)).toBeTruthy();
  });

  // The two controls that make no sense for an entry nothing declared.
  // They are absent rather than disabled: a disabled Edit invites an
  // operator to work out what would enable it, and nothing will.
  it("offers no edit or remove on the local hard drive", async () => {
    await renderSettings({ listStorageMediums: () => Promise.resolve([LOCAL, OFFSITE]) });

    await waitFor(() => expect(card().getByText(LOCAL_DESTINATION_ID)).toBeTruthy());
    expect(row(LOCAL_DESTINATION_ID).queryByRole("button", { name: "Edit" })).toBeNull();
    expect(row(LOCAL_DESTINATION_ID).queryByRole("button", { name: "Remove" })).toBeNull();
    // The positive control: a declared destination still has both.
    expect(row("offsite_s3").getByRole("button", { name: "Edit" })).toBeTruthy();
    expect(row("offsite_s3").getByRole("button", { name: "Remove" })).toBeTruthy();
  });

  it("marks the default and offers to move it", async () => {
    const { setDefaultStorageMedium } = await renderSettings({
      listStorageMediums: () => Promise.resolve([LOCAL, OFFSITE])
    });

    await waitFor(() => expect(card().getByText(LOCAL_DESTINATION_ID)).toBeTruthy());
    expect(row(LOCAL_DESTINATION_ID).getByText(/Default/)).toBeTruthy();
    // The one that IS the default offers no button to make it the
    // default, which would be a control that does nothing.
    expect(row(LOCAL_DESTINATION_ID).queryByRole("button", { name: "Make default" })).toBeNull();

    fireEvent.click(row("offsite_s3").getByRole("button", { name: "Make default" }));
    await waitFor(() => expect(setDefaultStorageMedium).toHaveBeenCalledWith("offsite_s3"));
  });

  // The refusal is in the backend, and this is the courtesy in front of
  // it: a Remove that would be refused is not offered.
  it("does not offer to remove the default", async () => {
    await renderSettings({
      listStorageMediums: () => Promise.resolve([{ ...LOCAL, isDefault: false }, { ...OFFSITE, isDefault: true }])
    });

    await waitFor(() => expect(card().getByText("offsite_s3")).toBeTruthy());
    expect(row("offsite_s3").queryByRole("button", { name: "Remove" })).toBeNull();
  });

  // The one that matters most on the local entry: the drive backups land
  // on has stopped answering, and the operator needs the count and the
  // sentence that stops them doing something drastic.
  //
  // The count is real rather than skipped. A local copy IS a placement in
  // the journal (FR-29 records "local" against every artifact in every
  // deployment written before EPIC E), so asking what is affected answers
  // something, and it answers about the most consequential destination
  // there is. What is NOT said is the removal advice: local is not
  // declared and cannot be removed, so the sentence about editing and
  // removing would be advice nobody can take.
  it("names what is on the local hard drive when it stops answering, without offering to remove it", async () => {
    await renderSettings({
      listStorageMediums: () => Promise.resolve([LOCAL]),
      getStorageMediumUsage: () =>
        Promise.resolve({
          medium: LOCAL_DESTINATION_ID,
          placements: 12,
          backupSets: [{ set: "production/postgres-primary", placements: 12, onlyCopyHere: 12 }]
        }),
      preflightStorageMedium: () =>
        Promise.resolve({
          medium: LOCAL_DESTINATION_ID,
          ok: false,
          checks: [
            {
              step: "reach",
              outcome: "failed",
              category: "not_found",
              detail: "The directory this deployment's backups land in is not there."
            }
          ]
        } as MediumPreflight)
    });

    await waitFor(() => expect(card().getByText(LOCAL_DESTINATION_ID)).toBeTruthy());
    fireEvent.click(row(LOCAL_DESTINATION_ID).getByRole("button", { name: "Test connection" }));

    await waitFor(() => expect(screen.getByText(/12 copies are recorded there/)).toBeTruthy());
    const shown = document.body.textContent ?? "";
    expect(shown).toContain("Nothing has been deleted and nothing will be");
    expect(shown).toContain("nothing here to edit or remove");
    expect(shown).not.toContain("Removing it is refused while a copy names it");
  });

  // The naming half. Three names for one idea is three things to learn,
  // and this is the surface that called it "Verify".
  it("calls the check Test connection, and echoes the verb that matches", async () => {
    const { preflightStorageMedium } = await renderSettings({
      listStorageMediums: () => Promise.resolve([LOCAL, OFFSITE])
    });

    await waitFor(() => expect(card().getByText("offsite_s3")).toBeTruthy());
    expect(row("offsite_s3").queryByRole("button", { name: "Verify" })).toBeNull();
    fireEvent.click(row("offsite_s3").getByRole("button", { name: "Test connection" }));
    await waitFor(() => expect(preflightStorageMedium).toHaveBeenCalledWith("offsite_s3"));

    expect(row("offsite_s3").getByText("backup-manager medium test-connection offsite_s3")).toBeTruthy();
  });
});
