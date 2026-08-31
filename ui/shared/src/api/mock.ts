import type {
  BackupManagerApi,
  CatalogScanPreview,
  ConnectionTestOutcome,
  ConnectionTestParams,
  CreateBackupSetRequest,
  CreatedBackupSet,
  HostKeyProbeResult,
  SSHKeyImportResult,
  ValidatorCatalogEntry
} from "./contracts";
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
    id: "set_pg_prod", source: "production", set: "postgres-primary", name: "Production PostgreSQL",
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
    id: "set_mysql_billing", source: "production", set: "billing-mysql", name: "Billing MySQL",
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
    id: "set_auth_cfg", source: "production", set: "auth-config", name: "Auth service config",
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
    id: "set_media", source: "media", set: "weekly-archive", name: "Media archive",
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

/**
 * A fresh RetentionPlan, as apps/common/webhost's real handler would return
 * it (see fromWireRetentionPlan/client.ts) — no `stale` field. `tick` fingerprints
 * "the world as of this preview" the same way the real service's
 * inventory_revision does: two previews with the same tick are the same
 * world, so applyRetention below only ever honors the MOST RECENT tick's
 * plan_id, exactly like ApplyRetentionPlan's own single-use, revision-
 * checked contract (core/service/retention.go).
 */
function retentionPlan(source: string, set: string, tick: number): RetentionPlan {
  return {
    planId: "retplan_mock_" + tick,
    backupSetId: source + "/" + set,
    inventoryRevision: "inv_" + tick,
    configRevision: "cfg_9f2a41",
    // Ten minutes out from whenever this fixture is read, matching
    // core/service's own retentionPlanTTL. A frozen literal here would
    // sooner or later be a plan that is already expired the moment it is
    // issued, which the dialog now refuses to apply (RetentionPreviewDialog's
    // handleApply) — a dev fixture that cannot be confirmed at all.
    expiresAt: new Date(Date.now() + 10 * 60 * 1000).toISOString(),
    keepCount: 4,
    deleteCount: 3,
    reclaimBytes: 55620000000,
    verdicts: [
      { artifact: "backup-20260828.dump.zst", action: "KEEP", reason: "GFS daily tier", tiers: ["DAILY", "LAST_KNOWN_GOOD"] },
      { artifact: "backup-20260827.dump.zst", action: "KEEP", reason: "GFS weekly tier", tiers: ["WEEKLY"] },
      { artifact: "backup-20260801.dump.zst", action: "KEEP", reason: "GFS monthly tier", tiers: ["MONTHLY"] },
      { artifact: "backup-20260701.dump.zst", action: "KEEP", reason: "GFS monthly tier", tiers: ["MONTHLY"] },
      { artifact: "backup-20260813.dump.zst", action: "REFUSE", reason: "sibling-prefix directory found at the computed path; refusing to delete", tiers: [] },
      { artifact: "backup-20260806.dump.zst", action: "DELETE", reason: "Not selected by current retention policy", tiers: [] },
      { artifact: "backup-20260723.dump.zst", action: "DELETE", reason: "Not selected by current retention policy", tiers: [] },
      { artifact: "backup-20260716.dump.zst", action: "DELETE", reason: "Not selected by current retention policy", tiers: [] }
    ]
  };
}

const delay = <T,>(value: T, ms = 180): Promise<T> =>
  new Promise((resolve) => setTimeout(() => resolve(value), ms));

/** Issue #146 (B2.7): a deterministic in-memory stand-in for the real
 *  create-backup-set/import/probe/test-connection endpoints, mirroring
 *  every other resource in this file (listSets/getSet, ...) — nothing
 *  here talks to a real server, exactly per this file's own module doc.
 *  mockImportedKeyFingerprint/mockProbedFingerprint are fixed, not
 *  randomised, so a test asserting on a specific displayed value stays
 *  deterministic across runs. */
const mockImportedKeyFingerprint = "SHA256:7pMwK3nRt+Vc9jXe1sHfB0oZaGdQ8yTiKrEuM4x";
const mockProbedFingerprint = "SHA256:9kQ2mVv+Rt4hLc0pXeN1sJfB7yUwZaGdQ8oT3iKrEuM";

function completionMethodFromStrategy(strategy: CreateBackupSetRequest["completionStrategy"]): BackupSet["completionMethod"] {
  if (strategy === "rename") return "atomic-rename";
  if (strategy === "stable") return "stable-size";
  return "completion-marker";
}

/** Turns a create request into a full BackupSet row (the mock's own
 *  fixture shape, see types/backup.ts), filling every field a REAL
 *  freshly-created set has no history for yet with its own honest
 *  "just created" value (0 retained bytes, no last run, "not-run"
 *  validation, ...) rather than inventing activity that never
 *  happened. */
function mockBackupSetFromCreateRequest(req: CreateBackupSetRequest): BackupSet {
  const sourceName = req.sourceName ?? "api";
  return {
    id: sourceName + "/" + req.name,
    // The same two halves core/service joins to build the id above
    // (backupsets.go: `ID: sourceName + "/" + bs.Name`), kept separate
    // here for the same reason the real client keeps them separate: the
    // retention routes key their URL by {source}/{set}, never by id.
    source: sourceName,
    set: req.name,
    name: req.name,
    host: req.host,
    port: req.port,
    username: req.user,
    remoteFolder: req.remotePath,
    includePatterns: req.include,
    excludePatterns: [],
    completionMethod: completionMethodFromStrategy(req.completionStrategy),
    destination: req.localPath,
    retention: defaultRetention,
    validations: ["transfer"],
    state: "healthy",
    stateNote: "Created just now; no runs yet.",
    enabled: !req.disabled,
    halted: false,
    newestKnownGoodAt: null,
    lastRunAt: null,
    lastValidation: "not-run",
    expectedIntervalHours: 24,
    retainedCount: 0,
    retainedBytes: 0,
    hostFingerprint: mockProbedFingerprint,
    fingerprintTrustedAt: new Date().toISOString()
  };
}

/** The registered application-validator catalog this fixture serves,
 *  mirroring core/service's own RegisteredValidators entry for entry. It
 *  is a literal rather than a fetch because that is what it is on the
 *  backend too: a code-defined catalog fixed at build time, not
 *  per-deployment data. */
const VALIDATORS: ValidatorCatalogEntry[] = [
  {
    id: "trailer-marker",
    summary:
      "Confirms the artifact's own content ends with the completion trailer its producer appends when it has finished writing."
  }
];

export function createMockApi(scenario: Scenario = "default"): BackupManagerApi {
  const empty = scenario === "empty";
  // Every previewRetention call advances this backup set's "inventory" by
  // one tick and issues a plan captured against it. applyRetention only
  // ever honors the plan_id from the LATEST tick — anything older is,
  // correctly, stale — mirroring ApplyRetentionPlan's real revision check.
  let retentionTick = 0;

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
      // A rejected promise, not a synchronous throw: getSet's own type
      // signature promises Promise<BackupSet>, and every caller (useAsync,
      // useResource/fetchResource) reaches for fetchFn().then/.catch to
      // turn a failure into a typed error, never a try/catch around the
      // call itself — a synchronous throw here escaped that entirely and
      // crashed the caller instead of producing the error state the UI
      // is built to show (found while wiring #97's error-state test).
      if (!found)
        return Promise.reject(
          new BackupManagerError({
            code: "unknown", message: "That backup set no longer exists.", correlationId: "cid_mock404"
          })
        );
      return delay(found);
    },
    runSet: () => delay(undefined),
    testConnection: () => delay({ ok: true, fingerprint: SETS[0].hostFingerprint }),
    setEnabled: () => delay(undefined),

    createBackupSet: (req: CreateBackupSetRequest): Promise<CreatedBackupSet> => {
      const set = mockBackupSetFromCreateRequest(req);
      // SETS is declared const, not let: pushing onto it (rather than
      // reassigning the binding) is what makes a freshly created set
      // show up in a later listSets() call within the same session,
      // exactly the "no manual refetch" behaviour appNodes.ts's
      // setsNode/fetchResource gives the real backend.
      SETS.push(set);
      const runImmediately = req.runImmediately && !req.disabled;
      return delay({
        id: set.id,
        sourceName: req.sourceName ?? "api",
        name: set.name,
        host: set.host,
        port: set.port,
        user: set.username,
        remotePath: set.remoteFolder,
        localPath: set.destination,
        include: set.includePatterns,
        completionStrategy: req.completionStrategy,
        validatorId: req.validatorId,
        disabled: !!req.disabled,
        operation: runImmediately ? { operationId: "op_mock_" + set.id, status: "completed" } : undefined
      });
    },
    listValidators: (): Promise<ValidatorCatalogEntry[]> => delay(VALIDATORS.map((v) => ({ ...v }))),
    importSSHKey: (): Promise<SSHKeyImportResult> =>
      delay({ id: "key_mock_" + Math.random().toString(36).slice(2, 10), algorithm: "ssh-ed25519", fingerprint: mockImportedKeyFingerprint }),
    probeHostKey: (): Promise<HostKeyProbeResult> =>
      delay({ algorithm: "ssh-ed25519", fingerprint: mockProbedFingerprint, knownHostsLine: "mock-host.internal ssh-ed25519 AAAAC3NzaC1lZDI1NTE5mock" }),
    testCandidateConnection: (_params: ConnectionTestParams): Promise<ConnectionTestOutcome> => delay({ ok: true }),

    listArtifacts: (setId) =>
      delay(empty ? [] : ARTIFACTS.filter((a) => !a.quarantine && (!setId || a.setId === setId))),
    getArtifact: (id) => delay(ARTIFACTS.find((a) => a.id === id) ?? ARTIFACTS[0]),

    listOperations: () => delay(empty ? [] : OPERATIONS),
    listActivity: () => delay(empty ? [] : ACTIVITY),
    listQuarantine: () => delay(empty ? [] : ARTIFACTS.filter((a) => a.quarantine)),
    revalidate: () => delay(undefined),
    retryIngestion: () => delay(undefined),

    previewRetention: (source, set) => {
      retentionTick += 1;
      return delay(retentionPlan(source, set, retentionTick));
    },
    applyRetention: (source, set, planId) => {
      // This has to REJECT, not throw synchronously. applyRetention is a
      // Promise, so a bare throw escapes before a promise exists and a
      // caller's .catch() never runs — the one path that must not fail open
      // for a stale retention plan.
      const current = retentionPlan(source, set, retentionTick);
      if (planId !== current.planId)
        return Promise.reject(new BackupManagerError({
          // The literal code apps/common/webhost/handlers_retention.go
          // writes for this refusal, not a fixture-only spelling: a mock
          // that invents its own vocabulary lets every test pass against
          // a wire contract the real backend never speaks (issue #96's
          // review, mandatory finding M2).
          code: "RETENTION_PLAN_STALE",
          message: "The backup inventory changed after this preview was created.",
          remediation: "No files were deleted. Review the updated retention plan before continuing.",
          correlationId: "cid_stale991"
        }));
      return delay({ ...current, operationId: "op_mock_retention_" + retentionTick });
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
