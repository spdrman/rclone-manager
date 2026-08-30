import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HealthSummary } from "@shared/components/HealthSummary";
import { LifecycleTimeline, buildPhases } from "@shared/components/LifecycleTimeline";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
import { StorageGauge } from "@shared/components/StorageGauge";
import { createMockApi } from "@shared/api/mock";
import type { SystemHealth } from "@shared/types/operation";
import type { BackupArtifact } from "@shared/types/backup";

const health: SystemHealth = {
  serviceRunning: true, serviceUptimeHours: 336,
  backupHealth: "stale",
  backupHealthReason: "No verified backup received for 31 hours. Expected within 24 hours.",
  lastSuccessfulCycleAt: new Date().toISOString(),
  newestVerifiedBackupAt: new Date().toISOString(),
  oldestSetFreshnessHours: 31,
  setsHealthy: 5, setsStale: 1, setsFailing: 0, quarantinedCount: 0,
  retainedCount: 318, retainedBytes: 4_000_000_000_000,
  storageFreeBytes: 1_800_000_000_000, storageTotalBytes: 6_200_000_000_000,
  storageState: "nominal", successRate7d: 0.964
};

describe("health rendering", () => {
  it("does not report healthy backups just because the service is running", () => {
    render(<HealthSummary health={health} />);
    expect(screen.getByText("BACKUPS STALE")).toBeTruthy();
    expect(screen.getByText(/Service running/)).toBeTruthy();
  });

  it("states the reason in words, not only colour", () => {
    render(<HealthSummary health={health} />);
    expect(screen.getByText(/No verified backup received for 31 hours/)).toBeTruthy();
  });
});

const committed: BackupArtifact = {
  id: "a1", setId: "s1", setName: "Set", filename: "x.dump",
  remoteOriginalPath: "h:/x.dump", localPath: "/data/x.dump",
  producedAt: "2026-08-28T02:00:11+02:00", receivedAt: "2026-08-28T02:00:53+02:00",
  sizeBytes: 10, checksum: "abc", checksumAlgorithm: "sha256",
  validation: "verified", retentionClasses: ["daily"],
  remoteSourceRemovedAt: "2026-08-28T02:01:01+02:00", quarantine: null
};

describe("lifecycle ordering", () => {
  it("never places remote deletion before commit", () => {
    const phases = buildPhases(committed);
    const commit = phases.findIndex((p) => p.label === "COMMITTED");
    const remote = phases.findIndex((p) => p.label === "REMOTE SOURCE DELETED");
    expect(commit).toBeLessThan(remote);
  });

  it("shows remote deletion as pending when the copy is not verified", () => {
    const unverified = { ...committed, validation: "pending" as const, remoteSourceRemovedAt: null };
    render(<LifecycleTimeline artifact={unverified} />);
    expect(screen.getByText(/the remote original is still retained/)).toBeTruthy();
  });
});

describe("destructive confirmation", () => {
  it("names the consequence in the confirm button and never says OK", async () => {
    render(
      <ConfirmationDialog
        open destructive title="Apply retention"
        confirmLabel="Delete 4 backups"
        onConfirm={() => {}} onCancel={() => {}}
      >
        <p>4 files will be removed.</p>
      </ConfirmationDialog>
    );
    expect(screen.getByRole("button", { name: "Delete 4 backups" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^OK$/ })).toBeNull();
  });

  it("focuses the safe action, not the destructive one", () => {
    render(
      <ConfirmationDialog
        open destructive title="Apply retention" confirmLabel="Delete 4 backups"
        onConfirm={() => {}} onCancel={() => {}}
      >
        <p>consequences</p>
      </ConfirmationDialog>
    );
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Cancel" }));
  });

  it("cancels on Escape", async () => {
    let cancelled = false;
    render(
      <ConfirmationDialog
        open title="T" confirmLabel="Do it"
        onConfirm={() => {}} onCancel={() => { cancelled = true; }}
      >
        <p>x</p>
      </ConfirmationDialog>
    );
    await userEvent.keyboard("{Escape}");
    expect(cancelled).toBe(true);
  });
});

describe("retention plan integrity", () => {
  it("refuses to apply a stale plan instead of recalculating", async () => {
    const api = createMockApi();
    await api.previewRetention("set_pg_prod"); // first plan is current
    const stale = await api.previewRetention("set_pg_prod"); // second goes stale
    expect(stale.stale).toBe(true);
    await expect(api.applyRetention(stale.planId)).rejects.toThrow();
  });
});

describe("private key handling", () => {
  it("never ships a private-key field in any contract", async () => {
    const api = createMockApi();
    const set = await api.getSet("set_pg_prod");
    expect(JSON.stringify(set)).not.toMatch(/privateKey|BEGIN OPENSSH PRIVATE KEY/i);
  });
});

/**
 * Issue #104 (B3.4), spec §56. The backend's internal/capacity refuses a
 * transfer that will not fit and contains no deletion path of any kind,
 * precisely so a full disk can never be "solved" by silently violating
 * retention. These tests are the frontend half of that promise: the UI
 * surfaces the refusal as a refusal, and never grows the "auto delete
 * anything until enough space" affordance §56 forbids in v1.
 */
describe("storage pressure (\u00a756)", () => {
  it("offers no operation anywhere in the API surface that frees space by deleting", () => {
    // createMockApi implements the whole BackupManagerApi interface, which
    // is the same contract the real client (api/client.ts) satisfies, so
    // enumerating it enumerates everything this frontend can ask the
    // backend to do.
    const api = createMockApi("storage-critical");
    const operations = Object.keys(api);
    // A zero here would make the assertion below vacuous, so the scan
    // proves it actually looked at a populated surface first.
    expect(operations.length).toBeGreaterThan(10);

    const freesSpaceByDeleting = /free.*space|autoDelete|deleteUntil|makeRoom|reclaim|purge|prune|evict|sweep/i;
    expect(operations.filter((name) => freesSpaceByDeleting.test(name))).toEqual([]);
  });

  it("has exactly one deletion path, and it needs an operator-reviewed plan", () => {
    const api = createMockApi("storage-critical");
    const deletes = Object.keys(api).filter((name) => /delete|remove/i.test(name));
    expect(deletes).toEqual([]);
    // applyRetention is the one call that removes anything, and it takes a
    // planId: there is no way to ask for "delete enough to fit", only to
    // apply a specific, server-computed plan the operator has seen.
    expect(api.applyRetention.length).toBe(1);
  });

  it("states the refusal instead of implying the UI could reclaim the space", async () => {
    const api = createMockApi("storage-critical");
    const critical = await api.getHealth();
    expect(critical.storageState).toBe("critical");
    expect(critical.backupHealthReason).toMatch(/paused to protect existing backups/i);
    expect(critical.backupHealthReason).not.toMatch(/delet/i);
  });

  it("renders a critical storage reading with no action attached to it", () => {
    render(<StorageGauge freeBytes={2_000_000_000} totalBytes={6_200_000_000_000} state="critical" />);
    expect(screen.getByRole("meter")).toBeTruthy();
    expect(screen.queryAllByRole("button")).toEqual([]);
    expect(screen.queryAllByRole("link")).toEqual([]);
  });
});
