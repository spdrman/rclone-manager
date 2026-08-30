import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HealthSummary } from "@shared/components/HealthSummary";
import { LifecycleTimeline, buildPhases } from "@shared/components/LifecycleTimeline";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
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
    const first = await api.previewRetention("production", "postgres-primary");
    // A second preview moves the mock's own "inventory" forward one tick —
    // `first`'s plan_id is no longer the current one, exactly like a real
    // inventory change would make it stale (core/service.ApplyRetentionPlan's
    // own revision check).
    await api.previewRetention("production", "postgres-primary");
    await expect(
      api.applyRetention("production", "postgres-primary", first.planId)
    ).rejects.toThrow();
  });
});

describe("private key handling", () => {
  it("never ships a private-key field in any contract", async () => {
    const api = createMockApi();
    const set = await api.getSet("set_pg_prod");
    expect(JSON.stringify(set)).not.toMatch(/privateKey|BEGIN OPENSSH PRIVATE KEY/i);
  });
});
