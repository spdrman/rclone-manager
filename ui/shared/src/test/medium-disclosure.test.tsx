import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SettingsPage } from "@shared/pages/SettingsPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { BackupManagerError } from "@shared/api/contracts";
import type { AppSettings, StorageMedium, UpdateSettingsRequest } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import type { VersionInfo } from "@shared/types/operation";
import { DashboardPage } from "@shared/pages/DashboardPage";
import { operationsNode } from "@shared/state/appNodes";
import type { Operation, SystemHealth } from "@shared/types/operation";

/**
 * EPIC E, FR-27's consent on the Settings page, and the run-cycle counts
 * on the Dashboard (issue #240).
 *
 * The consent tests are written around one claim: the disabled Save button
 * is a courtesy, and the gate is on the server. So they check both halves,
 * that the words reach the operator and that the acknowledgment reaches
 * the wire, and they check the refusal a client gets when the write goes
 * out without it.
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
  defaultTiers: [{ name: "daily", granularity: "day", keep: 7 }]
};

/** The words the backend serves, reproduced because these tests stand in
 *  for a server. A fixture that invented different words would hide
 *  exactly the drift the served schema exists to prevent. */
const STORAGE = {
  verificationClasses: [
    {
      className: "content",
      proves: "the bytes on the medium hash to the hash this product recorded when it ingested the artifact",
      cost: "a full download of the object: time plus egress, and for an archive storage class a restore first",
      costsEgress: true
    }
  ],
  mediumDisclosure:
    "Backups that only this tier keeps will live only on that storage medium. " +
    "After a backup uploads and I verify it, I delete the copy on this machine.",
  retrievalDisclosure:
    "Reading a copy back off a storage medium is billed by your provider. " +
    "I hold no price list and no knowledge of your rates."
};

const MEDIUMS: StorageMedium[] = [
  { id: "offsite_s3", type: "s3", bucket: "nas-backups", region: "us-east-1", storageClass: "STANDARD_IA", readsRequireRestore: false },
  { id: "offsite_cold", type: "s3", bucket: "nas-archive", region: "us-east-1", storageClass: "DEEP_ARCHIVE", readsRequireRestore: true }
];

function settingsFixture(over: {
  tiers?: AppSettings["retention"]["tiers"];
  mediums?: StorageMedium[];
} = {}): AppSettings {
  return {
    retention: {
      timezone: "UTC",
      weekStartsOn: "monday",
      tiers: over.tiers ?? [
        { name: "daily", granularity: "day", keep: 7 },
        { name: "monthly", granularity: "month", keep: 12 }
      ],
      protectLastKnownGood: true
    },
    capacity: {
      capBytes: 0, warningFreeBytes: 0, criticalFreeBytes: 0, safetyMarginBytes: 0,
      backupRoot: "/data/backups", backupRootConfigured: false
    },
    mediums: over.mediums ?? MEDIUMS,
    schema: { retention: SCHEMA, storage: STORAGE }
  };
}

async function renderSettings(options: {
  settings?: AppSettings;
  updateSettings?: (req: UpdateSettingsRequest) => Promise<AppSettings>;
} = {}) {
  const settings = options.settings ?? settingsFixture();
  const getSettings = vi.fn(() => Promise.resolve(settings));
  const updateSettings = vi.fn(options.updateSettings ?? (() => Promise.resolve(settings)));
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
          <SettingsPage readOnly={false} />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
  await act(async () => {});
  return { updateSettings };
}

const tier = (n: number) => within(screen.getByRole("group", { name: "Tier " + n }));
const save = () => screen.getByRole("button", { name: "Save retention policy" });

describe("mapping a retention tier to a storage medium", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  // FR-35's compatibility line, on the form. A deployment that declares no
  // storage medium has nowhere else to put a backup, so it gets exactly
  // the form it already had: no extra control to read past, and no new way
  // to get its policy wrong.
  it("offers no medium picker at all when the configuration declares none", async () => {
    await renderSettings({ settings: settingsFixture({ mediums: [] }) });

    expect(tier(1).queryByLabelText("Storage medium for tier 1")).toBeNull();
    // The positive control: the tier really did render, so the absence
    // above is the absence of the picker and not of the form.
    expect((tier(1).getByLabelText("Name") as HTMLInputElement).value).toBe("daily");
  });

  it("names the storage class in the picker, and says which one needs a restore", async () => {
    await renderSettings();

    const picker = tier(2).getByLabelText("Storage medium for tier 2") as HTMLSelectElement;
    const options = Array.from(picker.options).map((o) => o.textContent);
    expect(options).toContain("Local backup root");
    expect(options).toContain("offsite_s3 (STANDARD_IA)");
    // The archive medium is labelled as one BEFORE it is chosen. An
    // operator who picks it blind finds out hours later, holding a restore
    // request they did not know they needed.
    expect(options).toContain("offsite_cold (DEEP_ARCHIVE, needs a restore to read)");
  });

  it("shows the deletion consequence, in the backend's own words, before the first mapping can be saved", async () => {
    await renderSettings();

    fireEvent.change(tier(2).getByLabelText("Storage medium for tier 2"), {
      target: { value: "offsite_s3" }
    });

    // The disclosure is the backend's text, not a paraphrase this form
    // wrote: the server refuses an unacknowledged write with the same
    // words, so the two cannot come apart.
    expect(await screen.findByText(STORAGE.mediumDisclosure)).toBeTruthy();
    expect(screen.getByText(STORAGE.retrievalDisclosure)).toBeTruthy();
    expect(screen.getByText(/Saving this sends monthly off this machine/i)).toBeTruthy();

    // And no figure comes with it. The backend has no price list, so
    // nothing here may render one.
    const panel = screen.getByRole("group", { name: "Storage medium disclosure" });
    expect(panel.textContent).not.toMatch(/\$\s?\d/);
    expect(panel.textContent).not.toMatch(/per\s+(GB|TB|gigabyte)/i);
  });

  it("keeps Save disabled until the acknowledgment is ticked, then sends it", async () => {
    const { updateSettings } = await renderSettings();

    fireEvent.change(tier(2).getByLabelText("Storage medium for tier 2"), {
      target: { value: "offsite_s3" }
    });
    await screen.findByRole("group", { name: "Storage medium disclosure" });

    // Dirty and valid, and still refused: the only thing missing is that
    // somebody has read the paragraph.
    expect((save() as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(save());
    expect(updateSettings).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("checkbox", { name: /I understand/i }));
    expect((save() as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(save());

    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));
    const req = updateSettings.mock.calls[0][0] as UpdateSettingsRequest;
    expect(req.acknowledgeMediumDisclosure).toBe(true);
    expect(req.retention?.tiers?.[1].medium).toBe("offsite_s3");
  });

  // The disclosure is per mapping, matching the rule core/service applies:
  // a configuration that already sends monthly to a medium has consented
  // to monthly leaving, and to nothing else.
  it("does not ask again for a mapping the configuration already has", async () => {
    const { updateSettings } = await renderSettings({
      settings: settingsFixture({
        tiers: [
          { name: "daily", granularity: "day", keep: 7 },
          { name: "monthly", granularity: "month", keep: 12, medium: "offsite_s3" }
        ]
      })
    });

    fireEvent.change(tier(1).getByLabelText("Keep"), { target: { value: "9" } });

    expect(screen.queryByRole("group", { name: "Storage medium disclosure" })).toBeNull();
    expect((save() as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(save());

    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));
    const req = updateSettings.mock.calls[0][0] as UpdateSettingsRequest;
    expect(req.acknowledgeMediumDisclosure).toBeUndefined();
    // And the existing mapping is sent back, not dropped. A chain write
    // replaces the whole chain, so a medium this form forgot would be a
    // medium the save deletes from the operator's configuration.
    expect(req.retention?.tiers?.[1].medium).toBe("offsite_s3");
  });

  it("asks again when a SECOND tier is sent off local disk", async () => {
    await renderSettings({
      settings: settingsFixture({
        tiers: [
          { name: "daily", granularity: "day", keep: 7 },
          { name: "monthly", granularity: "month", keep: 12, medium: "offsite_s3" }
        ]
      })
    });

    fireEvent.change(tier(1).getByLabelText("Storage medium for tier 1"), {
      target: { value: "offsite_cold" }
    });

    expect(await screen.findByText(/Saving this sends daily off this machine/i)).toBeTruthy();
    // offsite_cold is an archive class, and the panel says so on top of
    // the deletion consequence. Scoped to the panel: the same fact is also
    // in the picker's field help, and asserting on the page as a whole
    // would pass on the help text alone.
    const panel = screen.getByRole("group", { name: "Storage medium disclosure" });
    expect(within(panel).getByText(/cannot be read on demand at all/i)).toBeTruthy();
    expect((save() as HTMLButtonElement).disabled).toBe(true);
  });

  it("forgets an acknowledgment given for a different mapping", async () => {
    await renderSettings();

    fireEvent.change(tier(2).getByLabelText("Storage medium for tier 2"), {
      target: { value: "offsite_s3" }
    });
    fireEvent.click(await screen.findByRole("checkbox", { name: /I understand/i }));
    expect((save() as HTMLButtonElement).disabled).toBe(false);

    // Now point the same tier somewhere materially different: an archive
    // class nothing can read without a restore. The tick was given for the
    // other place.
    fireEvent.change(tier(2).getByLabelText("Storage medium for tier 2"), {
      target: { value: "offsite_cold" }
    });
    expect((save() as HTMLButtonElement).disabled).toBe(true);
  });

  // The gate is on the server, and this is what proves the client is not
  // pretending otherwise: a refusal that arrives anyway is shown, not
  // swallowed, and the operator is told nothing was written.
  it("shows the server's refusal when the write goes out without the acknowledgment", async () => {
    await renderSettings({
      settings: settingsFixture({
        tiers: [
          { name: "daily", granularity: "day", keep: 7 },
          { name: "monthly", granularity: "month", keep: 12, medium: "offsite_s3" }
        ]
      }),
      updateSettings: () =>
        Promise.reject(
          new BackupManagerError({
            code: "MEDIUM_DISCLOSURE_REQUIRED",
            message:
              "This write sends monthly to offsite_s3. After a backup uploads and I verify it, I delete the copy on this machine.",
            correlationId: "cid_test"
          })
        )
    });

    fireEvent.change(tier(1).getByLabelText("Keep"), { target: { value: "9" } });
    fireEvent.click(save());

    expect(
      await screen.findByText(/I delete the copy on this machine/i)
    ).toBeTruthy();
    expect(screen.getByText(/Nothing was saved/i)).toBeTruthy();
  });
});

// ------------------------------------------------------ the cycle counts ---

const HEALTH: SystemHealth = {
  generatedAt: "2026-08-29T06:00:00Z",
  serviceRunning: true,
  backupHealth: "healthy",
  backupHealthReason: "every set is fresh",
  newestVerifiedBackupAt: "2026-08-29T02:00:00Z",
  lastCompletedBackupAt: "2026-08-29T02:00:00Z",
  oldestSetFreshnessHours: 4,
  setsHealthy: 1, setsDegraded: 0, setsStale: 0, setsFailing: 0,
  quarantinedCount: 0, readOnlyRetainedCount: 0,
  storageFreeBytes: 1e12, storageTotalBytes: 4e12,
  storageState: "nominal", storageReadingsUnavailable: 0
};

function operation(over: Partial<Operation> = {}): Operation {
  return {
    id: "op_1", setId: "", setName: "All backup sets",
    kind: "transfer", label: "run cycle", status: "completed",
    progress: null, nonDestructive: false, startedAt: "2026-08-29T01:00:00Z",
    cycle: null,
    ...over
  };
}

async function renderDashboard(operations: Operation[]) {
  const api = createMockApi();
  // The dashboard early-returns a "no backup sets yet" empty state when
  // the list is empty, so a real one is needed for the panels below it to
  // render at all.
  const sets = await api.listSets();
  act(() => {
    graph.commit("test/seed", (tx) => {
      tx.set(operationsNode, { data: operations, error: null, loading: false });
    });
  });
  render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <DashboardPage
          readOnly={false}
          health={{ data: HEALTH, error: null, loading: false, reload: () => {} }}
          sets={{ data: sets, error: null, loading: false, reload: () => {} }}
        />
      </ApiProvider>
    </MemoryRouter>
  );
  await act(async () => {});
}

describe("what the last run cycle got done", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  // Issue #361's shape, rendered. The operation completed, because the
  // cycle ran to the end. Twelve backups were walked and none of them
  // ended with their bytes anywhere durable, and without these two
  // numbers that is indistinguishable on this page from a cycle that
  // backed everything up.
  it("says so when a cycle walked backups and got none of them through", async () => {
    await renderDashboard([
      operation({ cycle: { backupSetsProcessed: 3, artifactsWalked: 12, artifactsThrough: 0 } })
    ]);

    const panel = screen.getByRole("region", { name: "Last run cycle" });
    expect(within(panel).getByText("Nothing got through")).toBeTruthy();
    expect(within(panel).getByText("12")).toBeTruthy();
    expect(within(panel).getByText("0")).toBeTruthy();
    expect(
      within(panel).getByText(/none of them ended it with their bytes on durable storage/i)
    ).toBeTruthy();
  });

  it("says so when every backup got through", async () => {
    await renderDashboard([
      operation({ cycle: { backupSetsProcessed: 3, artifactsWalked: 12, artifactsThrough: 12 } })
    ]);

    const panel = screen.getByRole("region", { name: "Last run cycle" });
    expect(within(panel).getByText("All through")).toBeTruthy();
    expect(within(panel).queryByText("Nothing got through")).toBeNull();
  });

  // The absent case. A cycle still running has no counts, and a pair of
  // zeroes drawn for it would be the loudest possible wrong answer: it
  // would report the worst outcome this panel can express, about a cycle
  // that has not finished doing anything yet.
  it("renders no counts at all for a cycle nobody has measured yet", async () => {
    await renderDashboard([operation({ status: "running", cycle: null })]);

    expect(screen.queryByRole("region", { name: "Last run cycle" })).toBeNull();
    // The positive control: the dashboard rendered, so the absence above
    // is the absence of the panel and not of the page.
    expect(screen.getByRole("region", { name: "Active operations" })).toBeTruthy();
  });
});
