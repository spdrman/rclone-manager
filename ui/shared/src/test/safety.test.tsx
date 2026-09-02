import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HealthSummary } from "@shared/components/HealthSummary";
import { LifecycleTimeline, buildPhases } from "@shared/components/LifecycleTimeline";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
import { StorageGauge } from "@shared/components/StorageGauge";
import { createMockApi } from "@shared/api/mock";
import { httpApi } from "@shared/api/client";
import type { ManagerStorage } from "@shared/api/contracts";
import type { SystemHealth } from "@shared/types/operation";
import type { BackupArtifact } from "@shared/types/backup";

const health: SystemHealth = {
  generatedAt: new Date().toISOString(),
  serviceRunning: true,
  backupHealth: "stale",
  backupHealthReason: "No verified backup received for 31 hours. Expected within 24 hours.",
  lastCompletedBackupAt: new Date().toISOString(),
  newestVerifiedBackupAt: new Date().toISOString(),
  oldestSetFreshnessHours: 31,
  setsHealthy: 5, setsDegraded: 0, setsStale: 1, setsFailing: 0, quarantinedCount: 0,
  readOnlyRetainedCount: 0,
  storageFreeBytes: 1_800_000_000_000, storageTotalBytes: 6_200_000_000_000,
  storageState: "nominal", storageReadingsUnavailable: 0
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

  // Issue #316's RED case for this component: before this badge existed,
  // readOnlyRetainedCount had nowhere on this page to be shown at all,
  // even though the field already carries a real number.
  it("shows the read-only-retained badge only when the count is nonzero", () => {
    render(<HealthSummary health={{ ...health, readOnlyRetainedCount: 4 }} />);
    expect(screen.getByText(/4 retained \(read-only source\)/)).toBeTruthy();
  });

  it("omits the read-only-retained badge when nothing is retained under it", () => {
    render(<HealthSummary health={health} />);
    expect(screen.queryByText(/retained \(read-only source\)/)).toBeNull();
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
    const set = await api.getSet("production/postgres-primary");
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
  // httpApi, not createMockApi: the mock and the real client both satisfy
  // BackupManagerApi, but TypeScript's structural typing lets the real
  // client carry a method the mock does not, and a scan of the mock's key
  // set would never see it. httpApi is the surface a browser can actually
  // reach, so it is the one worth scanning.
  it("offers no operation anywhere in the API surface that frees space by deleting", () => {
    const operations = Object.keys(httpApi);
    // A zero here would make the assertion below vacuous, so the scan
    // proves it actually looked at a populated surface first.
    expect(operations.length).toBeGreaterThan(10);

    const freesSpaceByDeleting = /free.*space|autoDelete|deleteUntil|makeRoom|reclaim|purge|prune|evict|sweep/i;
    expect(operations.filter((name) => freesSpaceByDeleting.test(name))).toEqual([]);
    // Positive control: the same filter over the same surface DOES find
    // things when the pattern names something that is really there, so an
    // empty result above means "no such operation exists", not "the scan
    // matched nothing at all".
    expect(operations.filter((name) => /retention/i.test(name))).toContain("applyRetention");
  });

  it("has no deletion or removal operation at all", () => {
    const deletes = Object.keys(httpApi).filter((name) => /delete|remove/i.test(name));
    expect(deletes).toEqual([]);
  });

  // applyRetention is the one call in the whole surface that removes
  // anything. The property that matters is not how many arguments it
  // takes: it is that the only thing it can be asked to do is apply a
  // specific, server-computed plan the operator has already seen. There is
  // no "delete enough to fit" to ask for, and a plan the server does not
  // recognise is refused rather than recalculated.
  //
  // Asserting that behaviourally, rather than through Function.length,
  // also survives applyRetention growing a source and set argument on the
  // sibling B3.1 branch, which the arity check would have broken against
  // while quietly weakening the claim to "takes three of something".
  it("can only be asked to apply a plan the server already issued", async () => {
    // The whole ask, URL plus method plus body: B3.1 routes the backup set
    // through the path and carries the plan id in the body, so checking the
    // URL alone would no longer see the plan id at all.
    const requested: string[] = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requested.push(String(input) + " " + (init?.method ?? "GET") + " " + String(init?.body ?? ""));
      return new Response(JSON.stringify({ error: { code: "RETENTION_PLAN_STALE", message: "no such plan" } }), {
        status: 409,
        headers: { "Content-Type": "application/json" }
      });
    });
    vi.stubGlobal("fetch", fetchMock);
    try {
      await expect(
        httpApi.applyRetention("production", "postgres-primary", "plan_the_server_never_issued")
      ).rejects.toThrow();
    } finally {
      vi.unstubAllGlobals();
    }

    // One request, naming the plan id, and nothing that could mean "free
    // up space": the backup set and the plan id are the entire payload of
    // the ask.
    expect(requested).toHaveLength(1);
    expect(requested[0]).toContain("plan_the_server_never_issued");
    expect(requested[0]).not.toMatch(/bytes|free|reclaim|until/i);

    // Positive control for the rejection above: the identical call against
    // a server that accepts the plan resolves, so the rejection is the
    // server's refusal being propagated rather than applyRetention being
    // unable to succeed at all. The body has to be a real plan now that
    // applyRetention returns one rather than void.
    const okMock = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            plan_id: "plan_the_server_did_issue",
            backup_set_id: "production/postgres-primary",
            inventory_revision: "inv_1",
            config_revision: "cfg_1",
            expires_at: "2026-08-29T06:09:48Z",
            keep_count: 1,
            delete_count: 1,
            reclaim_bytes: 4096,
            operation_id: "op_1",
            retention: { timezone: "UTC", week_starts_on: "monday", protect_last_known_good: true, tiers: [] },
            retention_is_override: false,
            verdicts: []
          }),
          { status: 200, headers: { "Content-Type": "application/json" } }
        )
    );
    vi.stubGlobal("fetch", okMock);
    try {
      await expect(
        httpApi.applyRetention("production", "postgres-primary", "plan_the_server_did_issue")
      ).resolves.not.toThrow();
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("states the refusal instead of implying the UI could reclaim the space", async () => {
    const api = createMockApi("storage-critical");
    const critical = await api.getHealth();
    expect(critical.storageState).toBe("critical");
    expect(critical.backupHealthReason).toMatch(/paused to protect existing backups/i);
    expect(critical.backupHealthReason).not.toMatch(/delet/i);
  });

  it("renders a critical storage reading with no action attached to it", () => {
    const critical: ManagerStorage = {
      known: true, unknownReason: "", measuredPath: "/data/backups",
      totalBytes: 6_200_000_000_000, freeBytes: 2_000_000_000, availableBytes: 2_000_000_000,
      catalogBytes: 6_198_000_000_000, catalogBytesKnown: true, otherBytes: 0, otherBytesKnown: true,
      capBytes: 0, denominator: "disk", limitBytes: 6_200_000_000_000, usedBytes: 6_198_000_000_000,
      headroomBytes: 2_000_000_000, bindingConstraint: "disk",
      warningFreeBytes: 0, criticalFreeBytes: 0, level: "CRITICAL"
    };
    render(<StorageGauge storage={critical} />);
    expect(screen.getByRole("meter")).toBeTruthy();
    expect(screen.queryAllByRole("button")).toEqual([]);
    expect(screen.queryAllByRole("link")).toEqual([]);
  });

  it("says capacity is not known yet instead of rendering a NaN percentage", () => {
    // Issue #286: the exact defect this whole mechanism exists to stop.
    // An unconfigured instance's reading is known: false with every byte
    // count at 0, and a component that computed 1 - 0/0 anyway would put
    // "NaN%" on screen. This proves the guard, not just the honest copy:
    // no meter is rendered at all, so there is no aria-valuenow for a NaN
    // to hide inside.
    const unknown: ManagerStorage = {
      known: false, unknownReason: "no_backup_root", measuredPath: "",
      totalBytes: 0, freeBytes: 0, availableBytes: 0,
      catalogBytes: 0, catalogBytesKnown: false, otherBytes: 0, otherBytesKnown: false,
      capBytes: 0, denominator: "disk", limitBytes: 0, usedBytes: 0,
      headroomBytes: 0, bindingConstraint: "",
      warningFreeBytes: 0, criticalFreeBytes: 0, level: ""
    };
    render(<StorageGauge storage={unknown} />);
    expect(screen.queryByRole("meter")).toBeNull();
    expect(screen.queryByText(/NaN/)).toBeNull();
    expect(screen.getByText(/not known yet/i)).toBeTruthy();
  });
});
