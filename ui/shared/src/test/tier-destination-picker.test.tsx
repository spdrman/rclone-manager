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
  isDefault: true,
  connectionUnverified: false
};

const OFFSITE: StorageMedium = {
  id: "offsite_s3", type: "s3", bucket: "nas-backups", region: "us-east-1",
  storageClass: "STANDARD_IA", uploadVerification: "readback",
  readsRequireRestore: false, isLocal: false, isDefault: false,
  connectionUnverified: false
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
  tier(n).getByLabelText("Storage medium for tier " + n) as HTMLSelectElement;

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
    // Named, and named the same thing the destinations card names it,
    // because one function decides what a destination is called. The
    // DRIVE is not spelled out here: there is exactly one local root in a
    // deployment, so a path in a list of things to choose between says
    // nothing about the choice. It is on the destinations card, which is
    // the screen that answers "what are my destinations", and the case
    // below asserts it there.
    expect(options).toContain("Local backup root");
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

  // The other half of "without leaving the tier": make one here, and end
  // up on it. A wizard that saved and left the tier on its old
  // destination would make the operator repeat the choice they already
  // expressed by creating the thing, and would send them to the settings
  // page to finish, which is the detour the picker exists to remove.
  //
  // The whole wizard is driven rather than stubbed, because the claim is
  // about the seam between it and the tier: what the tier ends up on is
  // the destination the ENGINE answered with, not the draft, so an id the
  // engine resolved differently is what the tier points at.
  it("creates a destination from inside the tier and leaves the tier on it", async () => {
    const created: StorageMedium = { ...OFFSITE, id: "made_here" };
    // The engine carries the new destination on the NEXT read, which is
    // what the row's reload is for. Modelled rather than skipped, because
    // the tier ending up on something the list does not carry is a real
    // state with its own rendering and this case is about the ordinary
    // one.
    let declared: StorageMedium[] = [LOCAL];
    const api = {
      ...createMockApi(),
      getSettings: vi.fn(() => Promise.resolve(settingsFixture({ mediums: declared }))),
      updateSettings: vi.fn(() => Promise.resolve(settingsFixture())),
      listStorageMediums: vi.fn(() => Promise.resolve(declared)),
      importStorageCredentials: vi.fn(() => Promise.resolve("cred-1")),
      preflightStorageMediumCandidate: vi.fn(() =>
        Promise.resolve({ medium: "made_here", ok: true, checks: [] } as MediumPreflight)
      ),
      createStorageMedium: vi.fn(() => {
        declared = [LOCAL, created];
        return Promise.resolve(created);
      })
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

    fireEvent.click(tier(2).getByRole("button", { name: "Add a destination" }));

    fireEvent.change(await screen.findByLabelText("Destination id"), { target: { value: "made_here" } });
    fireEvent.change(screen.getByLabelText("Region"), { target: { value: "us-east-1" } });
    fireEvent.change(screen.getByLabelText("Bucket"), { target: { value: "nas-backups" } });
    fireEvent.click(screen.getByRole("button", { name: "Next: credentials" }));

    fireEvent.change(await screen.findByLabelText("Access key id"), { target: { value: "EXAMPLE-NOT-A-REAL-KEY" } });
    fireEvent.change(screen.getByLabelText("Secret access key"), { target: { value: "EXAMPLE-NOT-A-REAL-SECRET" } });
    fireEvent.click(screen.getByRole("button", { name: "Next: test connection" }));

    fireEvent.click(await screen.findByRole("button", { name: "Next: save" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save destination" }));

    await waitFor(() => expect(api.createStorageMedium).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(picker(2).value).toBe("made_here"));
  });

  // The stale-read half of #634. The two cards on this page hold separate
  // reads: the destinations card reloads its own list after "Make
  // default", and the retention card's copy of the settings is what
  // "Add tier" reads the default out of. Nothing joined them, so an
  // operator could move the mark, watch the badge move, add a tier, and
  // get the destination that was default when the page loaded.
  //
  // Driven through the real page and the real buttons, because that is
  // the whole of the bug: each card is correct on its own and the page is
  // not. A test that rendered one card could not see it.
  it("starts a tier added after the default moves on the NEW default", async () => {
    // The engine answers with the moved default on the next read, which
    // is what a reload is for. The reporter could not do this in the
    // browser suite because its mock lives in the page and a reload
    // rebuilds it; here the api is injected, so the state survives.
    let mediums: StorageMedium[] = [LOCAL, OFFSITE];
    const api = {
      ...createMockApi(),
      getSettings: vi.fn(() => Promise.resolve(settingsFixture({ mediums }))),
      listStorageMediums: vi.fn(() => Promise.resolve(mediums)),
      setDefaultStorageMedium: vi.fn((id: string) => {
        mediums = mediums.map((m) => ({ ...m, isDefault: m.id === id }));
        return Promise.resolve(mediums.find((m) => m.id === id)!);
      })
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

    // The control: before the move, a new tier starts on the drive.
    fireEvent.click(screen.getByRole("button", { name: "Add tier" }));
    expect(picker(3).value).toBe(LOCAL_DESTINATION_ID);

    const card = within(screen.getByRole("region", { name: "Storage destinations" }));
    await waitFor(() => expect(card.getByText("offsite_s3")).toBeTruthy());
    fireEvent.click(
      within(screen.getByRole("group", { name: "Storage destination offsite_s3" }))
        .getByRole("button", { name: "Make default" })
    );
    await waitFor(() => expect(api.setDefaultStorageMedium).toHaveBeenCalled());

    fireEvent.click(screen.getByRole("button", { name: "Add tier" }));
    await waitFor(() => expect(picker(4).value).toBe("offsite_s3"));
  });

  // A report belongs to the destination it was asked about, and to no
  // other. Change the picker while a response is in flight and the old
  // destination's answer used to render under the new selection, saying
  // "This destination is ready for a backup" about a destination nobody
  // checked. That sentence is the one an operator reads before pointing a
  // tier somewhere, so it is the wrong one to be able to get wrong.
  //
  // Driven by holding the response until after the select moves, which is
  // the whole hazard: an instant answer cannot show it, and a slow one is
  // the ordinary case for a check that writes an object to a bucket and
  // reads it back.
  it("does not show one destination's test result under another", async () => {
    let release: ((r: MediumPreflight) => void) | null = null;
    const { preflightStorageMedium } = await renderSettings({
      preflightStorageMedium: () => new Promise<MediumPreflight>((resolve) => { release = resolve; })
    });

    fireEvent.click(tier(2).getByRole("button", { name: /Test connection/ }));
    await waitFor(() => expect(preflightStorageMedium).toHaveBeenCalledWith(LOCAL_DESTINATION_ID));

    // The operator moves on while the check is still running.
    fireEvent.change(picker(2), { target: { value: "offsite_s3" } });
    await act(async () => {
      release!({ medium: LOCAL_DESTINATION_ID, ok: true, checks: [] } as MediumPreflight);
    });

    expect(tier(2).queryByText(/This destination is ready for a backup/)).toBeNull();
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

  // Issue #636. A destination declared with --no-verify and one checked
  // against a real bucket used to be the same row here, forever, which is
  // what made the flag a hole rather than an escape hatch.
  it("says out loud that a destination was never proven, and says nothing about one that carries no mark", async () => {
    await renderSettings({
      listStorageMediums: () =>
        Promise.resolve([LOCAL, { ...OFFSITE, connectionUnverified: true }])
    });

    await waitFor(() => expect(card().getByText("offsite_s3")).toBeTruthy());
    expect(row("offsite_s3").getByText("never proven")).toBeTruthy();
    expect(row("offsite_s3").getByText(/declared without a check/i)).toBeTruthy();

    // The control that clears it is the one already on the row, and there
    // is deliberately no second button for it.
    expect(row("offsite_s3").getByRole("button", { name: "Test connection" })).toBeTruthy();

    // The local hard drive is out of scope: it is not declared, has no
    // create to skip and no bucket to prove.
    expect(row("local").queryByText("never proven")).toBeNull();
  });

  // The mark is a STATE and not a scar, so the check that earns its
  // removal has to take it off the screen in the same act. A banner that
  // survived the check clearing it is the thing an operator would report
  // as a bug.
  it("re-reads the destinations after a check that passes, so the mark it just cleared goes away", async () => {
    let listed = 0;
    const { preflightStorageMedium, listStorageMediums } = await renderSettings({
      listStorageMediums: () => {
        listed += 1;
        return Promise.resolve([
          LOCAL,
          { ...OFFSITE, connectionUnverified: listed === 1 }
        ]);
      }
    });

    await waitFor(() => expect(card().getByText("offsite_s3")).toBeTruthy());
    expect(row("offsite_s3").getByText("never proven")).toBeTruthy();

    fireEvent.click(row("offsite_s3").getByRole("button", { name: "Test connection" }));
    await waitFor(() => expect(preflightStorageMedium).toHaveBeenCalledWith("offsite_s3"));
    await waitFor(() => expect(row("offsite_s3").queryByText("never proven")).toBeNull());
    expect(listStorageMediums.mock.calls.length).toBeGreaterThan(1);
  });

  // The other half of the asymmetry, and the one that gives the mark its
  // meaning: a check that FAILS leaves it where it was. Clearing on any
  // press at all would turn "this destination works" into "somebody
  // pressed the button".
  it("leaves the mark alone when the check fails", async () => {
    const { preflightStorageMedium } = await renderSettings({
      listStorageMediums: () => Promise.resolve([LOCAL, { ...OFFSITE, connectionUnverified: true }]),
      preflightStorageMedium: (id: string) =>
        Promise.resolve({
          medium: id,
          ok: false,
          checks: [
            { step: "reach", outcome: "failed", category: "transient", detail: "the endpoint could not be reached" }
          ]
        } as MediumPreflight)
    });

    await waitFor(() => expect(card().getByText("offsite_s3")).toBeTruthy());
    fireEvent.click(row("offsite_s3").getByRole("button", { name: "Test connection" }));
    await waitFor(() => expect(preflightStorageMedium).toHaveBeenCalledWith("offsite_s3"));

    expect(row("offsite_s3").getByText("never proven")).toBeTruthy();
  });
});
