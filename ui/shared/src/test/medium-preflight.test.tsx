import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SettingsPage } from "@shared/pages/SettingsPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { BackupManagerError } from "@shared/api/contracts";
import type { AppSettings, MediumPreflight, StorageMedium } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import type { VersionInfo } from "@shared/types/operation";

/**
 * Issue #443's acceptance line: the settings form can run the preflight
 * BEFORE the first save that points a tier at a medium.
 *
 * That "before" is the whole point, so these tests drive the real form
 * rather than the component in isolation: the button has to be reachable
 * from the state an operator is actually in, which is having just chosen
 * a medium for a tier and not yet saved.
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

const STORAGE = {
  verificationClasses: [
    {
      className: "content",
      proves: "the bytes on the medium hash to the hash this product recorded at ingestion",
      requires: "a full download of the object",
      downloadsObject: true
    }
  ],
  mediumDisclosure: "Backups that only this tier keeps will live only on that storage medium.",
  retrievalDisclosure: "Reading a copy back off a storage medium is billed by your provider."
};

const MEDIUMS: StorageMedium[] = [
  { id: "offsite_s3", type: "s3", bucket: "nas-backups", region: "us-east-1", storageClass: "STANDARD_IA", readsRequireRestore: false }
];

function settingsFixture(): AppSettings {
  return {
    retention: {
      timezone: "UTC",
      weekStartsOn: "monday",
      tiers: [
        { name: "daily", granularity: "day", keep: 7 },
        { name: "monthly", granularity: "month", keep: 12 }
      ],
      protectLastKnownGood: true
    },
    capacity: {
      capBytes: 0, warningFreeBytes: 0, criticalFreeBytes: 0, safetyMarginBytes: 0,
      backupRoot: "/data/backups", backupRootConfigured: false
    },
    mediums: MEDIUMS,
    schema: { retention: SCHEMA, storage: STORAGE }
  };
}

function workingReport(): MediumPreflight {
  return {
    medium: "offsite_s3",
    ok: true,
    checks: [
      { step: "credentials", outcome: "passed", detail: "the credential was obtained and the endpoint accepted it" },
      { step: "reach", outcome: "passed", detail: 'the endpoint answered and holds bucket "nas-backups"' },
      { step: "deliverable", outcome: "passed", detail: "storage class STANDARD_IA reads on demand" },
      { step: "write", outcome: "passed", detail: "an object was written" },
      { step: "read_back", outcome: "passed", detail: "the object was read back and is byte for byte what was written" },
      { step: "storage_class", outcome: "passed", detail: "the endpoint stored the object as STANDARD_IA" },
      { step: "verification", outcome: "passed", detail: "the content class is what the read-back step just did" },
      { step: "delete", outcome: "passed", detail: "the probe object was deleted, and the endpoint confirms it is gone" }
    ]
  };
}

async function renderSettings(preflight: ReturnType<typeof vi.fn>) {
  const settings = settingsFixture();
  const api = {
    ...createMockApi(),
    getSettings: vi.fn(() => Promise.resolve(settings)),
    updateSettings: vi.fn(() => Promise.resolve(settings)),
    preflightStorageMedium: preflight
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
  return api;
}

const tier = (n: number) => within(screen.getByRole("group", { name: "Tier " + n }));

async function pointMonthlyAtTheMedium() {
  fireEvent.change(tier(2).getByLabelText("Storage medium for tier 2"), {
    target: { value: "offsite_s3" }
  });
  await screen.findByRole("group", { name: "Storage medium disclosure" });
}

describe("checking a storage medium before the first save that points a tier at it", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("offers the check at the moment the tier is pointed at a medium, before anything is saved", async () => {
    const preflight = vi.fn(() => Promise.resolve(workingReport()));
    const api = await renderSettings(preflight);

    await pointMonthlyAtTheMedium();

    const button = screen.getByRole("button", { name: /Check offsite_s3 now/i });
    fireEvent.click(button);

    await waitFor(() => expect(preflight).toHaveBeenCalledWith("offsite_s3"));

    // Nothing was saved on the way. The whole value of this check is that
    // it happens BEFORE the write that starts sending backups there.
    expect(api.updateSettings).not.toHaveBeenCalled();

    expect(await screen.findByText(/This medium is ready for a backup/i)).toBeTruthy();
  });

  it("renders every step, so an operator sees which things were established rather than a green tick", async () => {
    const preflight = vi.fn(() => Promise.resolve(workingReport()));
    await renderSettings(preflight);
    await pointMonthlyAtTheMedium();

    fireEvent.click(screen.getByRole("button", { name: /Check offsite_s3 now/i }));
    await screen.findByText(/This medium is ready for a backup/i);

    const panel = screen.getByRole("group", { name: "Storage medium disclosure" });
    for (const step of [
      "credentials", "reach", "deliverable", "write",
      "read_back", "storage_class", "verification", "delete"
    ]) {
      expect(panel.textContent).toContain(step);
    }
  });

  it("names the failing step and its category, and still lets the operator save", async () => {
    const preflight = vi.fn(() =>
      Promise.resolve<MediumPreflight>({
        medium: "offsite_s3",
        ok: false,
        checks: [
          { step: "credentials", outcome: "passed", detail: "the credential was obtained" },
          {
            step: "reach",
            outcome: "failed",
            category: "configuration",
            detail: 'the endpoint answered and does not have bucket "nas-backups"'
          },
          { step: "deliverable", outcome: "skipped", detail: "the endpoint could not be reached" },
          { step: "write", outcome: "skipped", detail: "nothing was written" },
          { step: "read_back", outcome: "skipped", detail: "nothing was written" },
          { step: "storage_class", outcome: "skipped", detail: "nothing was written" },
          { step: "verification", outcome: "skipped", detail: "nothing was written" },
          { step: "delete", outcome: "skipped", detail: "nothing was written" }
        ]
      })
    );
    await renderSettings(preflight);
    await pointMonthlyAtTheMedium();

    fireEvent.click(screen.getByRole("button", { name: /Check offsite_s3 now/i }));
    await screen.findByText(/This medium is not ready/i);

    const panel = screen.getByRole("group", { name: "Storage medium disclosure" });
    expect(panel.textContent).toContain("configuration");
    expect(panel.textContent).toContain("does not have bucket");
    // A skipped write is rendered as skipped and never as a pass: telling
    // somebody their bucket is writable on the strength of a step nothing
    // ran is the defect this whole report shape exists to avoid.
    expect(panel.textContent).toContain("skipped");

    // The check informs; it does not gate. An operator who is about to go
    // and create the bucket is not served by a form that refuses to save.
    fireEvent.click(screen.getByRole("checkbox", { name: /I understand/i }));
    const save = screen.getByRole("button", { name: "Save retention policy" }) as HTMLButtonElement;
    expect(save.disabled).toBe(false);
  });

  it("reports a refusal without pretending the medium passed", async () => {
    const preflight = vi.fn(() =>
      Promise.reject(
        new BackupManagerError({
          code: "MEDIUM_NOT_FOUND",
          message: "this configuration declares no storage medium with that id",
          correlationId: "cid_1"
        })
      )
    );
    await renderSettings(preflight);
    await pointMonthlyAtTheMedium();

    fireEvent.click(screen.getByRole("button", { name: /Check offsite_s3 now/i }));
    expect(await screen.findByText(/declares no storage medium with that id/i)).toBeTruthy();
    expect(screen.queryByText(/This medium is ready for a backup/i)).toBeNull();
  });

  it("is not offered when no tier is being newly pointed at a medium", async () => {
    const preflight = vi.fn(() => Promise.resolve(workingReport()));
    await renderSettings(preflight);

    // The form as loaded: no tier maps to a medium, so there is nothing
    // this check would be about yet.
    expect(screen.queryByRole("button", { name: /Check offsite_s3 now/i })).toBeNull();
    expect(preflight).not.toHaveBeenCalled();
  });
});
