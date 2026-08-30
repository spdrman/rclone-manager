import type { BackupManagerApi, CatalogScanPreview } from "./contracts";
import { BackupManagerError } from "./contracts";
import type { BackupArtifact, BackupSet, RetentionPlan } from "@shared/types/backup";
import type {
  ActivityEvent,
  Operation,
  SystemHealth,
  VersionInfo
} from "@shared/types/operation";

/** Development fixtures. Covers every scenario the brief requires (§42):
 *  healthy / stale / failing sets, active transfer, quarantine, retention
 *  preview, stale plan, host-key change, storage pressure, empty install,
 *  catalog recovery. Toggle scenarios with ?scenario= in the URL. */

export type Scenario = "default" | "empty" | "storage-critical" | "catalog-recovery" | "version-mismatch";

export function scenarioFromLocation(): Scenario {
  const s = new URLSearchParams(window.location.search).get("scenario");
  const allowed: Scenario[] = ["default", "empty", "storage-critical", "catalog-recovery", "version-mismatch"];
  return (allowed as string[]).includes(s ?? "") ? (s as Scenario) : "default";
}

const GB = 1024 ** 3;
const TB = 1024 ** 4;

const defaultRetention = {
  daily: 7,
  weekly: 13,
  monthly: 12,
  timezone: "Europe/Berlin",
  weekStartsOn: "monday" as const,
  protectLastKnownGood: true
};

const SETS: BackupSet[] = [
  {
    id: "set_pg_prod", name: "Production PostgreSQL",
    host: "prod-db-01.internal", port: 22, username: "backup-agent",
    remoteFolder: "/backups/postgresql/", includePatterns: ["*.dump.zst"],
    excludePatterns: ["*.tmp", "*.part"], completionMethod: "completion-marker",
    destination: "/data/backups/production/postgres/", retention: defaultRetention,
    validations: ["transfer", "checksum", "application"],
    state: "healthy",
    stateNote: "Verified nightly dump; application validation passed 42 minutes ago.",
    enabled: true, halted: false,
    newestKnownGoodAt: "2026-08-29T02:01:01+02:00",
    lastRunAt: "2026-08-29T02:01:01+02:00",
    lastValidation: "passed", expectedIntervalHours: 24,
    retainedCount: 32, retainedBytes: 421 * GB,
    hostFingerprint: "SHA256:9kQ2mVv+Rt4hLc0pXeN1sJfB7yUwZaGdQ8oT3iKrEuM",
    fingerprintTrustedAt: "2026-08-02T10:14:00+02:00"
  },
  {
    id: "set_mysql_billing", name: "Billing MySQL",
    host: "billing-db.internal", port: 22, username: "backup-agent",
    remoteFolder: "/srv/backups/mysql/", includePatterns: ["*.sql.gz"],
    excludePatterns: ["*.part"], completionMethod: "atomic-rename",
    destination: "/data/backups/production/billing/", retention: defaultRetention,
    validations: ["transfer", "checksum"],
    state: "stale",
    stateNote: "No verified backup received for 31 hours. Expected within 24 hours.",
    enabled: true, halted: false,
    newestKnownGoodAt: "2026-08-27T21:10:00+02:00",
    lastRunAt: "2026-08-28T02:00:04+02:00",
    lastValidation: "passed", expectedIntervalHours: 24,
    retainedCount: 28, retainedBytes: 96 * GB,
    hostFingerprint: "SHA256:7bTmQ4Kp+Xr9vNc2yLdE8sJf0UwZoGqR3iHuVeM1kAx",
    fingerprintTrustedAt: "2026-07-19T09:02:00+02:00"
  },
  {
    id: "set_auth_cfg", name: "Auth service config",
    host: "prod-db-01.internal", port: 22, username: "backup-agent",
    remoteFolder: "/etc/auth-service/backups/", includePatterns: ["*.tar.zst"],
    excludePatterns: [], completionMethod: "stable-size",
    destination: "/data/backups/production/auth/",
    retention: { ...defaultRetention, weekly: 4, monthly: 6 },
    validations: ["transfer", "checksum"],
    state: "failing",
    stateNote: "Halted — the SSH host key changed. Remote artifacts are untouched.",
    enabled: true, halted: true, haltReason: "host-key-changed",
    newestKnownGoodAt: "2026-08-29T00:14:00+02:00",
    lastRunAt: "2026-08-29T04:12:08+02:00",
    lastValidation: "not-run", expectedIntervalHours: 24,
    retainedCount: 19, retainedBytes: 2 * GB,
    hostFingerprint: "SHA256:1aXpQ8Lm+Nb3vRt7yKcE0dJf5UwZoGqS2iTrHuVeM4k",
    fingerprintTrustedAt: null
  },
  {
    id: "set_media", name: "Media archive",
    host: "media-01.internal", port: 2222, username: "archive",
    remoteFolder: "/export/weekly/", includePatterns: ["*.tar"],
    excludePatterns: [], completionMethod: "completion-marker",
    destination: "/data/backups/media/",
    retention: { ...defaultRetention, daily: 0, weekly: 8, monthly: 24 },
    validations: ["transfer", "checksum"],
    state: "healthy", stateNote: "Weekly cold archive; checksum verification only.",
    enabled: true, halted: false,
    newestKnownGoodAt: "2026-08-26T01:30:00+02:00",
    lastRunAt: "2026-08-26T01:30:00+02:00",
    lastValidation: "passed", expectedIntervalHours: 168,
    retainedCount: 31, retainedBytes: 3.4 * TB,
    hostFingerprint: "SHA256:4cRnW2Yk+Qp8mLb6vTdF1sJe9UzXoGhS5iNrCuJeP3t",
    fingerprintTrustedAt: "2026-05-11T14:20:00+02:00"
  }
];

const ARTIFACTS: BackupArtifact[] = [
  {
    id: "art_01J9F4M2QK8Z", setId: "set_pg_prod", setName: "Production PostgreSQL",
    filename: "postgres-prod-20260828.dump.zst",
    remoteOriginalPath: "prod-db-01:/backups/postgresql/postgres-prod-20260828.dump.zst",
    localPath: "/data/backups/production/postgres/2026/08/postgres-prod-20260828.dump.zst",
    producedAt: "2026-08-28T01:58:44+02:00", receivedAt: "2026-08-28T02:00:53+02:00",
    sizeBytes: 15246903296,
    checksum: "4f2a9c1e7b6d0835ae91cf4d2b7801e6c35a9f18d4b27e60ac139f5b8e2d7a04",
    checksumAlgorithm: "sha256", validation: "verified",
    retentionClasses: ["daily", "weekly", "protected"],
    remoteSourceRemovedAt: "2026-08-28T02:01:01+02:00", quarantine: null
  },
  {
    id: "art_01J9F2A7BC44", setId: "set_mysql_billing", setName: "Billing MySQL",
    filename: "billing-20260827.sql.gz",
    remoteOriginalPath: "billing-db:/srv/backups/mysql/billing-20260827.sql.gz",
    localPath: "/data/backups/production/billing/2026/08/billing-20260827.sql.gz",
    producedAt: "2026-08-27T01:55:00+02:00", receivedAt: "2026-08-27T02:00:41+02:00",
    sizeBytes: 3650722201,
    checksum: "b81c0d5f4a29e7136c8b0f2d97a4e5106d3b7c8290fa41e6b52d7c3a9018ef42",
    checksumAlgorithm: "sha256", validation: "verified",
    retentionClasses: ["daily", "weekly"],
    remoteSourceRemovedAt: "2026-08-27T02:00:48+02:00", quarantine: null
  },
  {
    id: "art_01J9E8QP4R21", setId: "set_auth_cfg", setName: "Auth service config",
    filename: "auth-config-20260826.tar.zst",
    remoteOriginalPath: "prod-db-01:/etc/auth-service/backups/auth-config-20260826.tar.zst",
    localPath: "/data/backups/production/auth/quarantine/auth-config-20260826.tar.zst",
    producedAt: "2026-08-26T04:10:00+02:00", receivedAt: "2026-08-26T04:13:52+02:00",
    sizeBytes: 44040192,
    checksum: "c19f3ba7d0428e6591cf7d3b2801ea64c58a9f13d4b72e06ac931f5b8e7d2a40",
    checksumAlgorithm: "sha256", validation: "failed",
    retentionClasses: ["daily"], remoteSourceRemovedAt: null,
    // Remote original stays put. Quarantine never triggers remote deletion.
    quarantine: { reason: "checksum-mismatch", detectedAt: "2026-08-26T04:14:10+02:00", remoteSourceRetained: true }
  },
  {
    id: "art_01J9C1XY7T09", setId: "set_mysql_billing", setName: "Billing MySQL",
    filename: "billing-20260824.sql.gz",
    remoteOriginalPath: "billing-db:/srv/backups/mysql/billing-20260824.sql.gz",
    localPath: "/data/backups/production/billing/quarantine/billing-20260824.sql.gz",
    producedAt: "2026-08-24T01:55:00+02:00", receivedAt: "2026-08-24T02:17:30+02:00",
    sizeBytes: 3543348838,
    checksum: "e42b9c8f1a370d6512cf4b7d2098ae31c67d5f0a9b8241e3c07d5b6a2f918d04",
    checksumAlgorithm: "sha256", validation: "failed",
    retentionClasses: [], remoteSourceRemovedAt: null,
    quarantine: { reason: "validation-failed", detectedAt: "2026-08-24T02:19:02+02:00", remoteSourceRetained: true }
  },
  {
    id: "art_01J98MN3V5KK", setId: "set_media", setName: "Media archive",
    filename: "media-week34.tar",
    remoteOriginalPath: "media-01:/export/weekly/media-week34.tar",
    localPath: "/data/backups/media/2026/w34/media-week34.tar",
    producedAt: "2026-08-25T01:00:00+02:00", receivedAt: "2026-08-25T04:41:19+02:00",
    sizeBytes: 1770035712819,
    checksum: "0a7c2e91b8d54f36ac1b9f0d27e4a5163d8b7c0f92a41e6b53d7c2a90187ef43",
    checksumAlgorithm: "sha256", validation: "verified",
    retentionClasses: ["weekly"],
    remoteSourceRemovedAt: "2026-08-25T04:43:02+02:00", quarantine: null
  }
];

const OPERATIONS: Operation[] = [
  {
    id: "op_transfer_1", setId: "set_pg_prod", setName: "Production PostgreSQL",
    kind: "transfer", stage: "transferring", label: "Transferring backup",
    percent: 59, bytesDone: 9019431321, bytesTotal: 15246903296,
    bytesPerSecond: 123731968, etaSeconds: 63,
    nonDestructive: false, startedAt: "2026-08-29T02:00:11+02:00"
  },
  {
    id: "op_recon_1", setId: "set_media", setName: "Media archive",
    kind: "reconciliation", stage: null, label: "Reconciling catalog against storage",
    percent: 38, itemsDone: 1204, itemsTotal: 3190,
    nonDestructive: true, startedAt: "2026-08-29T05:40:00+02:00"
  }
];

const ACTIVITY: ActivityEvent[] = [
  { id: "ev_1", at: "2026-08-29T04:12:08+02:00", type: "host-key-changed", severity: "error", setId: "set_auth_cfg", setName: "Auth service config", text: "SSH host key changed", detail: "set halted, remote artifacts untouched", correlationId: "cid_9f2a41" },
  { id: "ev_2", at: "2026-08-29T03:40:22+02:00", type: "validation-failed", severity: "warn", setId: "set_mysql_billing", setName: "Billing MySQL", text: "Backup stale", detail: "no verified backup for 31 hours", correlationId: "cid_71bc03" },
  { id: "ev_3", at: "2026-08-29T02:01:01+02:00", type: "remote-source-deleted", severity: "ok", setId: "set_pg_prod", setName: "Production PostgreSQL", text: "Remote source deleted", detail: "after durable commit", correlationId: "cid_4ad812" },
  { id: "ev_4", at: "2026-08-29T02:01:00+02:00", type: "backup-committed", severity: "ok", setId: "set_pg_prod", setName: "Production PostgreSQL", text: "Backup committed", detail: "14.2 GB fsynced", correlationId: "cid_4ad812" },
  { id: "ev_5", at: "2026-08-29T02:00:59+02:00", type: "verification-passed", severity: "ok", setId: "set_pg_prod", setName: "Production PostgreSQL", text: "Verification passed", detail: "SHA-256 matched manifest", correlationId: "cid_4ad812" },
  { id: "ev_6", at: "2026-08-29T02:00:53+02:00", type: "transfer-complete", severity: "info", setId: "set_pg_prod", setName: "Production PostgreSQL", text: "Transfer complete", detail: "1m 03s at 118 MB/s", correlationId: "cid_4ad812" },
  { id: "ev_7", at: "2026-08-29T02:00:11+02:00", type: "backup-discovered", severity: "info", setId: "set_pg_prod", setName: "Production PostgreSQL", text: "Backup discovered", detail: "completion manifest present", correlationId: "cid_4ad812" },
  { id: "ev_8", at: "2026-08-29T01:35:40+02:00", type: "retention-completed", severity: "ok", setId: "set_media", setName: "Media archive", text: "Retention completed", detail: "4 deleted, 51.8 GB reclaimed", correlationId: "cid_22e7f9" },
  { id: "ev_9", at: "2026-08-28T22:14:03+02:00", type: "validation-failed", severity: "warn", setId: "set_auth_cfg", setName: "Auth service config", text: "Validation failed", detail: "artifact quarantined", correlationId: "cid_50cc18" },
  { id: "ev_10", at: "2026-08-28T19:02:55+02:00", type: "configuration-updated", severity: "info", setId: "set_media", setName: "Media archive", text: "Configuration updated", detail: "weekly retention 8 to 13", correlationId: "cid_1b9d64" },
  { id: "ev_11", at: "2026-08-28T12:44:17+02:00", type: "storage-critical", severity: "warn", setId: null, setName: "System", text: "Storage warning", detail: "81% of pool used", correlationId: "cid_88fa02" },
  { id: "ev_12", at: "2026-08-28T02:00:04+02:00", type: "transfer-started", severity: "info", setId: "set_mysql_billing", setName: "Billing MySQL", text: "Transfer started", detail: "3.4 GB", correlationId: "cid_71bc03" }
];

const HEALTH: SystemHealth = {
  serviceRunning: true,
  serviceUptimeHours: 336,
  // The whole point of §8: the daemon is fine, the backups are not.
  backupHealth: "degraded",
  backupHealthReason:
    "One set has not produced a verified backup in 31 hours, and one set is halted after a host-key change.",
  lastSuccessfulCycleAt: "2026-08-29T05:52:00+02:00",
  newestVerifiedBackupAt: "2026-08-29T05:18:00+02:00",
  oldestSetFreshnessHours: 31,
  setsHealthy: 5, setsStale: 1, setsFailing: 1,
  quarantinedCount: 2,
  retainedCount: 318, retainedBytes: 4.42 * TB,
  storageFreeBytes: 1.8 * TB, storageTotalBytes: 6.2 * TB,
  storageState: "nominal",
  successRate7d: 0.964
};

const VERSION: VersionInfo = {
  ui: "1.3.0", service: "1.3.0", core: "1.3.0", rclone: "1.68.2",
  schema: 41, architecture: "linux/arm64", buildCommit: "9f4c1ab", compatible: true
};

function plan(stale: boolean): RetentionPlan {
  return {
    planId: stale ? "plan_stale_7712" : "plan_current_4410",
    setId: "set_pg_prod",
    createdAt: "2026-08-29T05:59:48+02:00",
    stale,
    keep: [
      { artifactId: "art_01J9F4M2QK8Z", date: "2026-08-28", classes: ["daily", "weekly", "protected"] },
      { artifactId: "art_01J9F1B22XQ0", date: "2026-08-27", classes: ["daily"] },
      { artifactId: "art_01J9880PLM31", date: "2026-08-01", classes: ["monthly"] },
      { artifactId: "art_01J8T40RRV18", date: "2026-07-01", classes: ["monthly"] }
    ],
    delete: [
      { artifactId: "art_01J9AA10ZK52", date: "2026-08-13", reason: "Not selected by current policy" },
      { artifactId: "art_01J99120TT77", date: "2026-08-06", reason: "Not selected by current policy" },
      { artifactId: "art_01J8YY55MM10", date: "2026-07-23", reason: "Not selected by current policy" },
      { artifactId: "art_01J8WW33KK09", date: "2026-07-16", reason: "Not selected by current policy" }
    ],
    reclaimBytes: 55620000000,
    protectedArtifactId: "art_01J9F4M2QK8Z"
  };
}

const delay = <T,>(value: T, ms = 180): Promise<T> =>
  new Promise((resolve) => setTimeout(() => resolve(value), ms));

export function createMockApi(scenario: Scenario = "default"): BackupManagerApi {
  const empty = scenario === "empty";
  let planIsStale = false;

  return {
    getVersion: () =>
      delay(
        scenario === "version-mismatch"
          ? { ...VERSION, service: "1.2.0", core: "1.2.0", compatible: false }
          : VERSION
      ),

    getHealth: () =>
      delay(
        empty
          ? { ...HEALTH, backupHealth: "healthy", backupHealthReason: "No backup sets configured yet.", setsHealthy: 0, setsStale: 0, setsFailing: 0, retainedCount: 0, retainedBytes: 0, quarantinedCount: 0 }
          : scenario === "storage-critical"
            ? { ...HEALTH, storageState: "critical", storageFreeBytes: 0.28 * TB, backupHealth: "failing", backupHealthReason: "Storage is critically low; ingestion has been paused to protect existing backups." }
            : HEALTH
      ),

    listSets: () => delay(empty ? [] : SETS),
    getSet: (id) => {
      const found = SETS.find((s) => s.id === id);
      if (!found)
        throw new BackupManagerError({
          code: "unknown", message: "That backup set no longer exists.", correlationId: "cid_mock404"
        });
      return delay(found);
    },
    runSet: () => delay(undefined),
    testConnection: () => delay({ ok: true, fingerprint: SETS[0].hostFingerprint }),
    setEnabled: () => delay(undefined),

    listArtifacts: (setId) =>
      delay(empty ? [] : ARTIFACTS.filter((a) => !a.quarantine && (!setId || a.setId === setId))),
    getArtifact: (id) => delay(ARTIFACTS.find((a) => a.id === id) ?? ARTIFACTS[0]),

    listOperations: () => delay(empty ? [] : OPERATIONS),
    listActivity: () => delay(empty ? [] : ACTIVITY),
    listQuarantine: () => delay(empty ? [] : ARTIFACTS.filter((a) => a.quarantine)),
    revalidate: () => delay(undefined),
    retryIngestion: () => delay(undefined),

    previewRetention: () => {
      const p = plan(planIsStale);
      // Every second preview goes stale, so the stale-plan UI is exercised.
      planIsStale = !planIsStale;
      return delay(p);
    },
    applyRetention: (planId) => {
      // This has to REJECT, not throw synchronously. applyRetention is typed
      // Promise<void>, so a bare throw escapes before a promise exists and a
      // caller's .catch() never runs, which is the one path that must not fail
      // open for a stale retention plan.
      if (planId.startsWith("plan_stale"))
        return Promise.reject(new BackupManagerError({
          code: "retention-plan-stale",
          message: "The backup inventory changed after this preview was created.",
          remediation: "No files were deleted. Review the updated retention plan before continuing.",
          correlationId: "cid_stale991"
        }));
      return delay(undefined);
    },

    scanCatalog: () =>
      delay<CatalogScanPreview>({ discovered: 47, valid: 45, requiresReview: 2 }, 900),
    rebuildCatalog: () => delay(undefined, 600),

    login: () => delay(undefined),
    enrollAdministrator: () => delay(undefined),
    rotatePassword: (currentPassword) =>
      // Mirrors apps/common/auth/local's handleRotatePassword: only this
      // one sentinel current-password value is ever "wrong" here, so the
      // rejected-rotation UI path has something to exercise against a
      // dev-fixture backend that otherwise has no real stored password.
      currentPassword === "wrong-current-password"
        ? Promise.reject(new BackupManagerError({
            code: "UNAUTHENTICATED",
            message: "Current password is incorrect.",
            correlationId: "cid_mockpw401"
          }))
        : delay(undefined),
    logout: () => delay(undefined)
  };
}
