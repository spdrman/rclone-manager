import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SettingsPage } from "@shared/pages/SettingsPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { BackupManagerError } from "@shared/api/contracts";
import type { AppSettings, CapacitySettings, UpdateSettingsRequest } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import type { VersionInfo } from "@shared/types/operation";

/**
 * Issue #286 — the storage cap and its two FR-21 thresholds, the write
 * half of CapacityCard.tsx.
 *
 * The decisive claims this file exists to pin: an operator can type a
 * cap in MB or GB and the request that leaves the browser is bytes; 0 is
 * a value this form saves deliberately, never a disabled state; and the
 * three combinations core/internal/config.validateCapacity refuses
 * (a negative amount, a warning line under the critical floor, a cap at
 * or under the critical floor) disable Save here too, so a doomed
 * request never reaches the wire — with a positive control for every one
 * of them, because a form that disabled Save unconditionally would pass
 * every refusal test and no save would ever happen.
 */

const GB = 1024 ** 3;
const MB = 1024 ** 2;

const VERSION: VersionInfo = {
  api: "v1", service: "1.3.0", buildCommit: "9f4c1ab", goVersion: "go1.27.0",
  engine: "1.68.2", configRevision: "cfg_9f4c1ab", ready: true, compatible: true
};

function retentionFixture(): AppSettings["retention"] {
  return {
    timezone: "Europe/Berlin",
    weekStartsOn: "monday",
    tiers: [{ name: "daily", granularity: "day", keep: 7 }],
    protectLastKnownGood: true
  };
}

function capacityFixture(overrides: Partial<CapacitySettings> = {}): CapacitySettings {
  return {
    capBytes: 0,
    warningFreeBytes: 0,
    criticalFreeBytes: 0,
    safetyMarginBytes: 0,
    backupRoot: "/data/backups",
    backupRootConfigured: false,
    ...overrides
  };
}

function settingsFixture(capacity: Partial<CapacitySettings> = {}): AppSettings {
  return {
    retention: retentionFixture(),
    capacity: capacityFixture(capacity),
    mediums: [],
    schema: {
      storage: {
        verificationClasses: [
          { className: "content", proves: "the bytes hash to what was recorded", requires: "a full download", downloadsObject: true },
          { className: "attested", proves: "the provider's checksum matches", requires: "one metadata call", downloadsObject: false },
          { className: "existence", proves: "an object exists at the recorded size", requires: "one HEAD request", downloadsObject: false }
        ],
        mediumDisclosure: "I delete the copy on this machine after a verified upload.",
        retrievalDisclosure: "Reading a copy back is billed by your provider."
      },
      retention: {
        granularities: ["day", "week", "month", "quarter", "half_year", "year", "days"],
        windowUnits: ["day", "week", "month", "quarter", "half_year", "year"],
        tierNamePattern: "^[a-z][a-z0-9_]*$",
        reservedTierName: "last_known_good",
        keepMax: 10000,
        periodDaysMax: 3650,
        defaultTiers: [{ name: "daily", granularity: "day", keep: 7 }]
      }
    }
  };
}

interface Harness {
  updateSettings: ReturnType<typeof vi.fn>;
}

async function renderSettings(
  options: {
    settings?: AppSettings;
    updateSettings?: (req: UpdateSettingsRequest) => Promise<AppSettings>;
  } = {}
): Promise<Harness> {
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
  await screen.findByLabelText("Storage cap");
  return { updateSettings };
}

const amount = (label: string) => screen.getByLabelText(label) as HTMLInputElement;
const unit = (label: string) => screen.getByLabelText(label + " unit") as HTMLSelectElement;
const save = () => screen.getByRole("button", { name: "Save storage capacity" });
/** The Storage capacity card's own section, scoped because the Platform
 *  card beside it also names a mount path in its own "Storage mount"
 *  row, and the two must not be confused with each other by a query. */
function capacityCard() {
  const section = screen.getByText("Storage capacity").closest("section");
  if (!section) throw new Error("Storage capacity card section not found");
  return within(section);
}

describe("CapacityCard", () => {
  beforeEach(() => {
    resetGraphForTests();
  });
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  it("converts the loaded byte counts into the friendlier unit", async () => {
    await renderSettings({
      settings: settingsFixture({
        capBytes: 100 * GB,
        warningFreeBytes: 20 * GB,
        criticalFreeBytes: 512 * MB
      })
    });

    expect(amount("Storage cap").value).toBe("100");
    expect(unit("Storage cap").value).toBe("GB");
    expect(amount("Storage warning threshold").value).toBe("20");
    expect(unit("Storage warning threshold").value).toBe("GB");
    // Under 1 GB displays in MB rather than a fractional "0.5" GB.
    expect(amount("Storage critical threshold").value).toBe("512");
    expect(unit("Storage critical threshold").value).toBe("MB");
  });

  it("a cap of 0 reads back as 0, not as an empty or disabled field", async () => {
    await renderSettings({ settings: settingsFixture({ capBytes: 0 }) });

    expect(amount("Storage cap").value).toBe("0");
    expect(save()).toHaveProperty("disabled", true); // nothing changed yet
  });

  it("names the filesystem the cap is measured against", async () => {
    await renderSettings({
      settings: settingsFixture()
    });
    const card = capacityCard();
    expect(card.getByText(/data\/backups/)).toBeTruthy();
    expect(card.getByText(/derived from your configured backup sets/)).toBeTruthy();
  });

  it("says an operator-configured root differently from a derived one", async () => {
    await renderSettings({
      settings: settingsFixture({ backupRoot: "/volume1/backups/rclone-manager", backupRootConfigured: true })
    });
    const card = capacityCard();
    expect(card.getByText(/volume1\/backups\/rclone-manager/)).toBeTruthy();
    expect(card.queryByText(/derived from your configured backup sets/)).toBeNull();
  });

  it("sends a typed MB amount as bytes, and only the field that changed", async () => {
    const { updateSettings } = await renderSettings();

    fireEvent.change(amount("Storage cap"), { target: { value: "512" } });
    fireEvent.change(unit("Storage cap"), { target: { value: "MB" } });
    fireEvent.click(save());

    expect(updateSettings).toHaveBeenCalledWith({
      capacity: { capBytes: 512 * MB }
    });
  });

  it("sends a typed GB amount as bytes", async () => {
    const { updateSettings } = await renderSettings();

    fireEvent.change(unit("Storage cap"), { target: { value: "GB" } });
    fireEvent.change(amount("Storage cap"), { target: { value: "100" } });
    fireEvent.click(save());

    expect(updateSettings).toHaveBeenCalledWith({
      capacity: { capBytes: 100 * GB }
    });
  });

  it("switching the unit converts the displayed amount rather than reinterpreting the number", async () => {
    await renderSettings();

    fireEvent.change(unit("Storage cap"), { target: { value: "GB" } });
    fireEvent.change(amount("Storage cap"), { target: { value: "1" } });
    fireEvent.change(unit("Storage cap"), { target: { value: "MB" } });

    expect(amount("Storage cap").value).toBe("1024");
  });

  it("saves an explicit 0 deliberately, as a real request field rather than an omission", async () => {
    const { updateSettings } = await renderSettings({
      settings: settingsFixture({ capBytes: 100 * GB })
    });

    fireEvent.change(unit("Storage cap"), { target: { value: "MB" } });
    fireEvent.change(amount("Storage cap"), { target: { value: "0" } });
    fireEvent.click(save());

    expect(updateSettings).toHaveBeenCalledWith({ capacity: { capBytes: 0 } });
  });

  it("leaves the two thresholds out of the request when only the cap changed", async () => {
    const { updateSettings } = await renderSettings({
      settings: settingsFixture({ warningFreeBytes: 20 * GB, criticalFreeBytes: 10 * GB })
    });

    fireEvent.change(unit("Storage cap"), { target: { value: "GB" } });
    fireEvent.change(amount("Storage cap"), { target: { value: "50" } });
    fireEvent.click(save());

    const body = updateSettings.mock.calls[0][0] as UpdateSettingsRequest;
    expect(body.capacity).toEqual({ capBytes: 50 * GB });
    expect(body.capacity).not.toHaveProperty("warningFreeBytes");
    expect(body.capacity).not.toHaveProperty("criticalFreeBytes");
  });

  it("disables Save while the warning threshold sits under the critical floor", async () => {
    await renderSettings({ settings: settingsFixture({ criticalFreeBytes: 20 * GB }) });

    fireEvent.change(amount("Storage warning threshold"), { target: { value: "5" } });
    fireEvent.change(unit("Storage warning threshold"), { target: { value: "GB" } });

    expect(save()).toHaveProperty("disabled", true);
    expect(capacityCard().getByText(/warning line below the critical floor/)).toBeTruthy();
  });

  it("positive control: the identical warning edit saves once it clears the critical floor", async () => {
    // Without this, the refusal above could pass on a form that disables
    // Save unconditionally.
    const { updateSettings } = await renderSettings({
      settings: settingsFixture({ criticalFreeBytes: 5 * GB })
    });

    fireEvent.change(unit("Storage warning threshold"), { target: { value: "GB" } });
    fireEvent.change(amount("Storage warning threshold"), { target: { value: "20" } });

    expect(save()).toHaveProperty("disabled", false);
    fireEvent.click(save());
    expect(updateSettings).toHaveBeenCalledWith({ capacity: { warningFreeBytes: 20 * GB } });
  });

  it("disables Save while the cap sits at or under the critical floor", async () => {
    await renderSettings({ settings: settingsFixture({ criticalFreeBytes: 20 * GB }) });

    fireEvent.change(unit("Storage cap"), { target: { value: "GB" } });
    fireEvent.change(amount("Storage cap"), { target: { value: "20" } });

    expect(save()).toHaveProperty("disabled", true);
    expect(capacityCard().getByText(/at or under the critical floor/)).toBeTruthy();
  });

  it("positive control: the identical cap saves once it clears the critical floor", async () => {
    const { updateSettings } = await renderSettings({
      // warningFreeBytes must be at or above criticalFreeBytes on its
      // own terms, or the fixture itself would already be invalid before
      // the cap is ever touched, and this test would be proving nothing
      // about the cap-versus-critical rule.
      settings: settingsFixture({ warningFreeBytes: 20 * GB, criticalFreeBytes: 20 * GB })
    });

    fireEvent.change(unit("Storage cap"), { target: { value: "GB" } });
    fireEvent.change(amount("Storage cap"), { target: { value: "21" } });

    expect(save()).toHaveProperty("disabled", false);
    fireEvent.click(save());
    expect(updateSettings).toHaveBeenCalledWith({ capacity: { capBytes: 21 * GB } });
  });

  it("a cap of 0 is exempt from the above-the-critical-floor rule, since 0 means no cap", async () => {
    const { updateSettings } = await renderSettings({
      settings: settingsFixture({
        capBytes: 50 * GB, warningFreeBytes: 20 * GB, criticalFreeBytes: 20 * GB
      })
    });

    fireEvent.change(unit("Storage cap"), { target: { value: "MB" } });
    fireEvent.change(amount("Storage cap"), { target: { value: "0" } });

    expect(save()).toHaveProperty("disabled", false);
    fireEvent.click(save());
    expect(updateSettings).toHaveBeenCalledWith({ capacity: { capBytes: 0 } });
  });

  it("disables Save on a negative amount instead of clamping it", async () => {
    await renderSettings();

    fireEvent.change(amount("Storage cap"), { target: { value: "-5" } });

    expect(save()).toHaveProperty("disabled", true);
    expect(screen.getByText(/0 or more/)).toBeTruthy();
  });

  it("shows the server's refusal and leaves the running policy unchanged on a failed save", async () => {
    const updateSettings = vi.fn(() =>
      Promise.reject(
        new BackupManagerError({
          code: "INVALID_REQUEST",
          message: "capacity.cap_bytes must be above capacity.critical_free_bytes",
          correlationId: "cid_test_cap"
        })
      )
    );
    await renderSettings({ updateSettings });

    fireEvent.change(unit("Storage cap"), { target: { value: "GB" } });
    fireEvent.change(amount("Storage cap"), { target: { value: "50" } });
    fireEvent.click(save());

    expect(await screen.findByText(/must be above capacity.critical_free_bytes/)).toBeTruthy();
    expect(screen.getByText(/on disk are unchanged/)).toBeTruthy();
  });

  it("re-baselines against the server's response so Save disables again after a successful save", async () => {
    await renderSettings();

    fireEvent.change(unit("Storage cap"), { target: { value: "GB" } });
    fireEvent.change(amount("Storage cap"), { target: { value: "50" } });
    fireEvent.click(save());

    expect(await screen.findByText(/Storage capacity settings saved/)).toBeTruthy();
    expect(save()).toHaveProperty("disabled", true);
  });

  it("read-only disables every control without disabling the whole page", async () => {
    const settings = settingsFixture({ capBytes: 10 * GB });
    const getSettings = vi.fn(() => Promise.resolve(settings));
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
            <SettingsPage readOnly />
          </PlatformProvider>
        </ApiProvider>
      </MemoryRouter>
    );
    await screen.findByLabelText("Storage cap");

    expect(amount("Storage cap")).toHaveProperty("disabled", true);
    expect(unit("Storage cap")).toHaveProperty("disabled", true);
    expect(save()).toHaveProperty("disabled", true);
  });
});
