import type {
  AppSettings,
  BackupManagerApi,
  BackupSetRetention,
  CapacitySettings,
  CatalogScanPreview,
  ConnectionTestOutcome,
  CreateBackupSetRequest,
  CreatedBackupSet,
  FirstRunResult,
  HostKeyProbeResult,
  ManagerStorage,
  RetentionOverride,
  RetentionSettings,
  SSHKeyImportResult,
  UpdateSettingsRequest,
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
 *  catalog recovery, and (issue #176) a genuinely unconfigured instance
 *  that has no config.yaml at all, which is a different thing from an
 *  "empty" one with a configuration and no backup sets. Toggle scenarios
 *  with ?scenario= in the URL. */

export type Scenario = "default" | "empty" | "storage-critical" | "catalog-recovery" | "version-mismatch" | "first-run";

export function scenarioFromLocation(): Scenario {
  const s = new URLSearchParams(window.location.search).get("scenario");
  const allowed: Scenario[] = ["default", "empty", "storage-critical", "catalog-recovery", "version-mismatch", "first-run"];
  return (allowed as string[]).includes(s ?? "") ? (s as Scenario) : "default";
}

const GB = 1024 ** 3;
const TB = 1024 ** 4;

/**
 * The deployment's retention policy this mock reports, and the one every
 * inheriting set is retained under (issue #333).
 *
 * Deliberately NOT the product's own 7/3/12 default: a bug that reaches
 * for the documented defaults instead of this deployment's policy is the
 * failure #362 was written to stop, and it is invisible against a fixture
 * where the two agree.
 *
 * The monthly tier names a storage medium, which nothing in this UI edits
 * yet. It is here because a chain write REPLACES the whole chain, so it
 * is the fixture that would catch an editor which dropped the field on
 * the way back out.
 */
const deploymentRetention: RetentionSettings = {
  timezone: "Europe/Berlin",
  weekStartsOn: "monday",
  protectLastKnownGood: true,
  tiers: [
    { name: "daily", granularity: "day", keep: 7 },
    { name: "weekly", granularity: "week", keep: 13, windowUnit: "month" },
    { name: "monthly", granularity: "month", keep: 12, medium: "cold" }
  ]
};

/**
 * Which sets declare a policy of their own, keyed by "source/set", and
 * what that raw (unresolved) policy says.
 *
 * Two fixture sets override, and each one is a different spelling: the
 * media archive names a tiers chain, the auth config names the three
 * scalars. Both are legal, so a surface that could only render one of
 * them would look correct against half the fixtures.
 */
const SET_RETENTION_OVERRIDES: ReadonlyArray<readonly [string, RetentionOverride]> = ([
  [
    "media/weekly-archive",
    {
      tiers: [
        { name: "weekly", granularity: "week", keep: 8 },
        { name: "monthly", granularity: "month", keep: 24, medium: "cold" }
      ]
    }
  ],
  ["production/auth-config", { dailyDays: 7, weeklyMonths: 4, monthlyMonths: 6 }]
] as const);

/**
 * The fixture backup sets, and the ONE piece of state in this module that
 * survives a createMockApi() call, because two of the fake's methods
 * genuinely change it: createBackupSet appends (so a set created in a
 * session shows up in a later listSets, the behaviour the real backend
 * has) and, since issue #350, updateBackupSet applies its patch (so a
 * per-box Save that sent the wrong field is visible instead of being
 * echoed back as though it had worked).
 *
 * That makes test ORDER matter, which it did not before. resetMockFixtures
 * below is the way out, and a test that asserts against a fixture's
 * current values should call it rather than assume it is looking at the
 * declaration on this page.
 */
const SETS: BackupSet[] = [
  {
    id: "production/postgres-primary", source: "production", set: "postgres-primary", name: "Production PostgreSQL",
    host: "prod-db-01.internal", port: 22, username: "backup-agent",
    remoteFolder: "/backups/postgresql/", includePatterns: ["*.dump.zst"],
    excludePatterns: ["*.tmp", "*.part"], completionMethod: "completion-marker", stableForSeconds: 0,
    destination: "/data/backups/production/postgres/", retentionIsOverride: false,
    validations: ["transfer", "checksum", "application"],
    state: "healthy",
    stateNote: "Verified nightly dump; application validation passed 42 minutes ago.",
    enabled: true, readOnly: false, readOnlyRetainedCount: 0,
    newestKnownGoodAt: "2026-08-29T02:01:01+02:00",
    lastRunAt: "2026-08-29T02:01:01+02:00",
    lastValidation: "passed", expectedIntervalHours: 24,
    retainedCount: 32, retainedBytes: 421 * GB,
    hostFingerprint: "SHA256:9kQ2mVv+Rt4hLc0pXeN1sJfB7yUwZaGdQ8oT3iKrEuM",
    fingerprintTrustedAt: "2026-08-02T10:14:00+02:00"
  },
  {
    id: "production/billing-mysql", source: "production", set: "billing-mysql", name: "Billing MySQL",
    host: "billing-db.internal", port: 22, username: "backup-agent",
    remoteFolder: "/srv/backups/mysql/", includePatterns: ["*.sql.gz"],
    excludePatterns: ["*.part"], completionMethod: "atomic-rename", stableForSeconds: 0,
    destination: "/data/backups/production/billing/", retentionIsOverride: false,
    validations: ["transfer", "checksum"],
    state: "stale",
    stateNote: "No verified backup received for 31 hours. Expected within 24 hours.",
    enabled: true, readOnly: false, readOnlyRetainedCount: 0,
    newestKnownGoodAt: "2026-08-27T21:10:00+02:00",
    lastRunAt: "2026-08-28T02:00:04+02:00",
    lastValidation: "passed", expectedIntervalHours: 24,
    retainedCount: 28, retainedBytes: 96 * GB,
    hostFingerprint: "SHA256:7bTmQ4Kp+Xr9vNc2yLdE8sJf0UwZoGqR3iHuVeM1kAx",
    fingerprintTrustedAt: "2026-07-19T09:02:00+02:00"
  },
  {
    id: "production/auth-config", source: "production", set: "auth-config", name: "Auth service config",
    host: "prod-db-01.internal", port: 22, username: "backup-agent",
    remoteFolder: "/etc/auth-service/backups/", includePatterns: ["*.tar.zst"],
    excludePatterns: [], completionMethod: "stable-size", stableForSeconds: 300,
    destination: "/data/backups/production/auth/",
    retentionIsOverride: true,
    validations: ["transfer", "checksum"],
    state: "failing",
    stateNote: "Halted — the SSH host key changed. Remote artifacts are untouched.",
    enabled: true, readOnly: false, readOnlyRetainedCount: 0, haltReason: "host-key-changed",
    newestKnownGoodAt: "2026-08-29T00:14:00+02:00",
    lastRunAt: "2026-08-29T04:12:08+02:00",
    lastValidation: "not-run", expectedIntervalHours: 24,
    retainedCount: 19, retainedBytes: 2 * GB,
    hostFingerprint: "SHA256:1aXpQ8Lm+Nb3vRt7yKcE0dJf5UwZoGqS2iTrHuVeM4k",
    fingerprintTrustedAt: null
  },
  {
    id: "media/weekly-archive", source: "media", set: "weekly-archive", name: "Media archive",
    host: "media-01.internal", port: 2222, username: "archive",
    remoteFolder: "/export/weekly/", includePatterns: ["*.tar"],
    excludePatterns: [], completionMethod: "completion-marker", stableForSeconds: 0,
    destination: "/data/backups/media/",
    retentionIsOverride: true,
    validations: ["transfer", "checksum"],
    state: "healthy", stateNote: "Weekly cold archive; checksum verification only.",
    // This fixture is the one read-only set (issue #282, #316): a cold
    // archive is exactly the shape "pull from here, never delete here"
    // was written for, and it is what exercises the wizard/detail-page
    // controls and the retained-count displays in dev mode without a
    // real backend.
    enabled: true, readOnly: true, readOnlyRetainedCount: 3,
    newestKnownGoodAt: "2026-08-26T01:30:00+02:00",
    lastRunAt: "2026-08-26T01:30:00+02:00",
    lastValidation: "passed", expectedIntervalHours: 168,
    retainedCount: 31, retainedBytes: 3.4 * TB,
    hostFingerprint: "SHA256:4cRnW2Yk+Qp8mLb6vTdF1sJe9UzXoGhS5iNrCuJeP3t",
    fingerprintTrustedAt: "2026-05-11T14:20:00+02:00"
  }
];

/** A copy of SETS as declared, taken at module load and never written to,
 *  so resetMockFixtures has something pristine to restore from. */
const PRISTINE_SETS: BackupSet[] = SETS.map((set) => ({
  ...set,
  includePatterns: [...set.includePatterns],
  excludePatterns: [...set.excludePatterns]
}));

/**
 * Puts the fixture backup sets back exactly as this module declares them.
 *
 * Call it in a test's afterEach when the test drives a mutating method
 * (createBackupSet, updateBackupSet). Without it, a suite's Nth test sees
 * whatever its predecessors wrote, which is not a hypothetical: the
 * inline-edit suite's "never send a hidden field" case first failed
 * because an earlier case in the same file had already switched the
 * fixture's completion method to the very value it was checking was
 * absent.
 *
 * The clone is deep enough for what these fixtures hold: the arrays are
 * arrays of strings, so copying them is what stops a patch that replaces
 * includePatterns leaking into the pristine copy.
 */
export function resetMockFixtures(): void {
  SETS.length = 0;
  for (const set of PRISTINE_SETS) {
    SETS.push({ ...set, includePatterns: [...set.includePatterns], excludePatterns: [...set.excludePatterns] });
  }
}

const ARTIFACTS: BackupArtifact[] = [
  {
    id: "art_01J9F4M2QK8Z", setId: "production/postgres-primary", setName: "Production PostgreSQL",
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
    id: "art_01J9F2A7BC44", setId: "production/billing-mysql", setName: "Billing MySQL",
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
    id: "art_01J9E8QP4R21", setId: "production/auth-config", setName: "Auth service config",
    filename: "auth-config-20260826.tar.zst",
    remoteOriginalPath: "prod-db-01:/etc/auth-service/backups/auth-config-20260826.tar.zst",
    localPath: "/data/backups/production/auth/quarantine/auth-config-20260826.tar.zst",
    producedAt: "2026-08-26T04:10:00+02:00", receivedAt: "2026-08-26T04:13:52+02:00",
    sizeBytes: 44040192,
    checksum: "c19f3ba7d0428e6591cf7d3b2801ea64c58a9f13d4b72e06ac931f5b8e7d2a40",
    checksumAlgorithm: "sha256", validation: "failed",
    retentionClasses: ["daily"], remoteSourceRemovedAt: null,
    // Remote original stays put. Quarantine never triggers remote deletion.
    quarantine: {
      reason: "checksum-mismatch",
      detail:
        "sha256 mismatch: local file hashes to c19f3ba7..., remote reports 91a4d02e...",
      detectedAt: "2026-08-26T04:14:10+02:00",
      remoteSourceRetained: true
    }
  },
  {
    id: "art_01J9C1XY7T09", setId: "production/billing-mysql", setName: "Billing MySQL",
    filename: "billing-20260824.sql.gz",
    remoteOriginalPath: "billing-db:/srv/backups/mysql/billing-20260824.sql.gz",
    localPath: "/data/backups/production/billing/quarantine/billing-20260824.sql.gz",
    producedAt: "2026-08-24T01:55:00+02:00", receivedAt: "2026-08-24T02:17:30+02:00",
    sizeBytes: 3543348838,
    checksum: "e42b9c8f1a370d6512cf4b7d2098ae31c67d5f0a9b8241e3c07d5b6a2f918d04",
    checksumAlgorithm: "sha256", validation: "failed",
    retentionClasses: [], remoteSourceRemovedAt: null,
    quarantine: {
      reason: "validation-failed",
      detail: "application validator rejected the artifact: restore-test hook failed: could not decompress",
      detectedAt: "2026-08-24T02:19:02+02:00",
      remoteSourceRetained: true
    }
  },
  {
    id: "art_01J98MN3V5KK", setId: "media/weekly-archive", setName: "Media archive",
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

// Two operations, chosen to exercise both answers the real API can give:
// one that is executing and reporting a live reading, and one that is
// running with NO reading available (what a client sees for an operation
// left behind by a restart). The second is deliberately not a zeroed copy
// of the first: "no progress available" and "0%" are different answers and
// the dev server has to be able to show both.
const OPERATIONS: Operation[] = [
  {
    id: "op_transfer_1", setId: "production/postgres-primary", setName: "Production PostgreSQL",
    kind: "transfer", label: "Transferring backup", status: "running",
    progress: {
      observedAt: "2026-08-29T02:01:14+02:00",
      sequence: 412,
      stage: "transferring",
      backupSetId: "production/postgres-primary",
      backupSetsDone: 1,
      backupSetsTotal: 4,
      artifact: "postgres-primary-2026-08-29.dump.zst",
      artifactsDone: 3,
      bytesDone: 9019431321,
      bytesTotal: 15246903296,
      bytesPerSecond: 123731968
    },
    nonDestructive: false, startedAt: "2026-08-29T02:00:11+02:00"
  },
  {
    id: "op_recon_1", setId: "media/weekly-archive", setName: "Media archive",
    kind: "reconciliation", label: "Reconciling catalog against storage",
    status: "running", progress: null,
    nonDestructive: true, startedAt: "2026-08-29T05:40:00+02:00"
  }
];

const ACTIVITY: ActivityEvent[] = [
  { id: "ev_1", at: "2026-08-29T04:12:08+02:00", type: "host-key-changed", severity: "error", setId: "production/auth-config", setName: "Auth service config", text: "SSH host key changed", detail: "set halted, remote artifacts untouched", correlationId: "cid_9f2a41" },
  { id: "ev_2", at: "2026-08-29T03:40:22+02:00", type: "validation-failed", severity: "warn", setId: "production/billing-mysql", setName: "Billing MySQL", text: "Backup stale", detail: "no verified backup for 31 hours", correlationId: "cid_71bc03" },
  { id: "ev_3", at: "2026-08-29T02:01:01+02:00", type: "remote-source-deleted", severity: "ok", setId: "production/postgres-primary", setName: "Production PostgreSQL", text: "Remote source deleted", detail: "after durable commit", correlationId: "cid_4ad812" },
  { id: "ev_4", at: "2026-08-29T02:01:00+02:00", type: "backup-committed", severity: "ok", setId: "production/postgres-primary", setName: "Production PostgreSQL", text: "Backup committed", detail: "14.2 GB fsynced", correlationId: "cid_4ad812" },
  { id: "ev_5", at: "2026-08-29T02:00:59+02:00", type: "verification-passed", severity: "ok", setId: "production/postgres-primary", setName: "Production PostgreSQL", text: "Verification passed", detail: "SHA-256 matched manifest", correlationId: "cid_4ad812" },
  { id: "ev_6", at: "2026-08-29T02:00:53+02:00", type: "transfer-complete", severity: "info", setId: "production/postgres-primary", setName: "Production PostgreSQL", text: "Transfer complete", detail: "1m 03s at 118 MB/s", correlationId: "cid_4ad812" },
  { id: "ev_7", at: "2026-08-29T02:00:11+02:00", type: "backup-discovered", severity: "info", setId: "production/postgres-primary", setName: "Production PostgreSQL", text: "Backup discovered", detail: "completion manifest present", correlationId: "cid_4ad812" },
  { id: "ev_8", at: "2026-08-29T01:35:40+02:00", type: "retention-completed", severity: "ok", setId: "media/weekly-archive", setName: "Media archive", text: "Retention completed", detail: "4 deleted, 51.8 GB reclaimed", correlationId: "cid_22e7f9" },
  { id: "ev_9", at: "2026-08-28T22:14:03+02:00", type: "validation-failed", severity: "warn", setId: "production/auth-config", setName: "Auth service config", text: "Validation failed", detail: "artifact quarantined", correlationId: "cid_50cc18" },
  { id: "ev_10", at: "2026-08-28T19:02:55+02:00", type: "configuration-updated", severity: "info", setId: "media/weekly-archive", setName: "Media archive", text: "Configuration updated", detail: "weekly retention 8 to 13", correlationId: "cid_1b9d64" },
  { id: "ev_11", at: "2026-08-28T12:44:17+02:00", type: "storage-critical", severity: "warn", setId: null, setName: "System", text: "Storage warning", detail: "81% of pool used", correlationId: "cid_88fa02" },
  { id: "ev_12", at: "2026-08-28T02:00:04+02:00", type: "transfer-started", severity: "info", setId: "production/billing-mysql", setName: "Billing MySQL", text: "Transfer started", detail: "3.4 GB", correlationId: "cid_71bc03" }
];

const HEALTH: SystemHealth = {
  generatedAt: "2026-08-29T06:00:00+02:00",
  serviceRunning: true,
  // The whole point of §8: the daemon is fine, the backups are not.
  backupHealth: "degraded",
  backupHealthReason:
    "One set has not produced a verified backup in 31 hours, and one set is halted after a host-key change.",
  lastCompletedBackupAt: "2026-08-29T05:52:00+02:00",
  newestVerifiedBackupAt: "2026-08-29T05:18:00+02:00",
  oldestSetFreshnessHours: 31,
  setsHealthy: 5, setsDegraded: 0, setsStale: 1, setsFailing: 1,
  quarantinedCount: 2,
  // Issue #316: the media archive fixture below is declared read-only,
  // so this is not a permanently-resting zero the way it is for most
  // deployments (BackupSet.readOnlyRetainedCount's own doc).
  readOnlyRetainedCount: 3,
  storageFreeBytes: 1.8 * TB, storageTotalBytes: 6.2 * TB,
  storageState: "nominal",
  storageReadingsUnavailable: 0
};

/**
 * Issue #286's manager-wide reading. Deliberately its own fixture rather
 * than derived from HEALTH: the two answer different questions (a
 * fresh/unconfigured instance sums HEALTH's per-set list to zero and gets
 * "0 B of 0 B used · NaN%", which is the defect this whole mechanism
 * exists to stop), so nothing here is computed FROM the other.
 *
 * "default" reports the disk itself (no cap configured, this product's
 * default): 6.2 TB total, 1.8 TB free, matching HEALTH's own numbers so
 * the two readings agree where they overlap. "empty" reports known:false
 * with no_backup_root, which is the honest answer a configuration with no
 * backup sets actually gives — not a fabricated zero. "storage-critical"
 * keeps the disk denominator (a critically full volume, not a spent cap)
 * so it lines up with HEALTH's own "storage critical" narrative for that
 * scenario.
 */
const STORAGE: ManagerStorage = {
  known: true,
  unknownReason: "",
  measuredPath: "/data/backups",
  totalBytes: 6.2 * TB,
  freeBytes: 1.8 * TB,
  availableBytes: 1.8 * TB,
  catalogBytes: 4.4 * TB,
  catalogBytesKnown: true,
  otherBytes: 0,
  otherBytesKnown: true,
  capBytes: 0,
  denominator: "disk",
  limitBytes: 6.2 * TB,
  usedBytes: 4.4 * TB,
  headroomBytes: 1.8 * TB,
  bindingConstraint: "disk",
  warningFreeBytes: 0,
  criticalFreeBytes: 0,
  level: "OK"
};

const STORAGE_CRITICAL: ManagerStorage = {
  ...STORAGE,
  freeBytes: 0.28 * TB,
  availableBytes: 0.28 * TB,
  catalogBytes: 5.92 * TB,
  usedBytes: 5.92 * TB,
  headroomBytes: 0.28 * TB,
  level: "CRITICAL"
};

/** What a configuration with no backup sets actually reports: there is no
 *  local_path anywhere to derive a backup root from, so this is not
 *  known, not a fabricated zero. Matches getHealth's own "empty" branch,
 *  which reports zero sets for the identical reason. */
const STORAGE_EMPTY: ManagerStorage = {
  known: false,
  unknownReason: "no_backup_root",
  measuredPath: "",
  totalBytes: 0,
  freeBytes: 0,
  availableBytes: 0,
  catalogBytes: 0,
  catalogBytesKnown: false,
  otherBytes: 0,
  otherBytesKnown: false,
  capBytes: 0,
  denominator: "disk",
  limitBytes: 0,
  usedBytes: 0,
  headroomBytes: 0,
  bindingConstraint: "",
  warningFreeBytes: 0,
  criticalFreeBytes: 0,
  level: ""
};

const VERSION: VersionInfo = {
  api: "v1", service: "1.3.0", buildCommit: "9f4c1ab", goVersion: "go1.27.0",
  engine: "1.68.2", configRevision: "cfg_9f4c1ab", ready: true, compatible: true
};

/**
 * Resolves one backup set's retention answer the way core/internal/config
 * does (issue #333): its own policy when it declares one, the
 * deployment's otherwise, and the calendar inherited either way.
 *
 * The inheritance is modelled here rather than faked with two flat
 * fixtures because it is the behaviour the surface is about: an override
 * that omits the timezone is reckoned in the DEPLOYMENT's, and a mock
 * that quietly answered UTC would make the UI look right against a rule
 * it was breaking.
 */
/** Projects the shared backup set fixtures through one mock instance's own
 *  override map, so the list card and the detail page agree after a write
 *  the way they would against a real backend that computes both from one
 *  configuration.
 *
 *  Read-time projection rather than a mutation of SETS, because SETS is
 *  module state shared by every createMockApi() in a test run: writing
 *  through it would make one test's override visible to the next, which
 *  is a test-order dependency nothing in the file would explain. */
function withRetentionAttribution(overrides: Map<string, RetentionOverride>, sets: BackupSet[]): BackupSet[] {
  return sets.map((s) => ({ ...s, retentionIsOverride: overrides.has(s.id) }));
}

function mockBackupSetRetention(
  overrides: Map<string, RetentionOverride>,
  source: string,
  set: string
): BackupSetRetention {
  const id = source + "/" + set;
  const override = overrides.get(id);
  if (!override) {
    return { backupSetId: id, isOverride: false, effective: deploymentRetention, deployment: deploymentRetention };
  }
  return {
    backupSetId: id,
    isOverride: true,
    override,
    deployment: deploymentRetention,
    effective: {
      timezone: override.timezone ?? deploymentRetention.timezone,
      weekStartsOn: override.weekStartsOn ?? deploymentRetention.weekStartsOn,
      protectLastKnownGood: override.protectLastKnownGood ?? deploymentRetention.protectLastKnownGood,
      tiers:
        override.tiers ??
        // The three scalars are sugar for exactly this chain
        // (config.DefaultTierChain), expanded here because the real
        // backend always reports the RESOLVED chain and a form that had
        // to know the sugar exists would need two layouts for one policy.
        [
          { name: "daily", granularity: "day", keep: override.dailyDays ?? 0 },
          { name: "weekly", granularity: "week", keep: override.weeklyMonths ?? 0, windowUnit: "month" },
          { name: "monthly", granularity: "month", keep: override.monthlyMonths ?? 0 }
        ]
    }
  };
}

/**
 * A fresh RetentionPlan, as apps/common/webhost's real handler would return
 * it (see fromWireRetentionPlan/client.ts) — no `stale` field. `tick` fingerprints
 * "the world as of this preview" the same way the real service's
 * inventory_revision does: two previews with the same tick are the same
 * world, so applyRetention below only ever honors the MOST RECENT tick's
 * plan_id, exactly like ApplyRetentionPlan's own single-use, revision-
 * checked contract (core/service/retention.go).
 */
function retentionPlan(
  overrides: Map<string, RetentionOverride>,
  source: string,
  set: string,
  tick: number
): RetentionPlan {
  const attribution = mockBackupSetRetention(overrides, source, set);
  return {
    // Issue #333: which policy decided these verdicts. Taken from the
    // same per-set state the retention sub-resource serves, so giving a
    // set its own policy in dev mode moves the preview too.
    retention: attribution.effective,
    retentionIsOverride: attribution.isOverride,
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
      { artifact: "backup-20260828.dump.zst", action: "KEEP", reason: "kept by the DAILY(both), LAST_KNOWN_GOOD tiers", tiers: [{ tier: "DAILY", selectedBy: "BOTH" }, { tier: "LAST_KNOWN_GOOD", selectedBy: "PROTECTION" }] },
      { artifact: "backup-20260827.dump.zst", action: "KEEP", reason: "kept by the WEEKLY(discovery) tier", tiers: [{ tier: "WEEKLY", selectedBy: "DISCOVERY" }] },
      // An ingested backlog's own shape: this one is inside the monthly
      // window only by the producer's own timestamp, which is exactly the
      // case FR-8 wants an operator to be able to see (issue #218).
      { artifact: "backup-20260801.dump.zst", action: "KEEP", reason: "kept by the MONTHLY(producer) tier", tiers: [{ tier: "MONTHLY", selectedBy: "PRODUCER" }] },
      { artifact: "backup-20260701.dump.zst", action: "KEEP", reason: "kept by the MONTHLY(both) tier", tiers: [{ tier: "MONTHLY", selectedBy: "BOTH" }] },
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
    stableForSeconds: req.stableForSeconds ?? 0,
    destination: req.localPath,
    // A newly created set has no policy of its own: it is retained under
    // the deployment's, which is what a set with no retention block in
    // config.yaml means.
    retentionIsOverride: false,
    validations: ["transfer"],
    state: "healthy",
    stateNote: "Created just now; no runs yet.",
    enabled: !req.disabled,
    readOnly: !!req.readOnly,
    readOnlyRetainedCount: 0,
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

/**
 * The settings fixture GET/PATCH /api/v1/settings serves here.
 *
 * `retention.tiers` is the RESOLVED default chain, spelled exactly the
 * way core/service reports it for a config file that names neither the
 * tiers list nor the legacy scalars — the weekly tier included, which
 * buckets by week but looks back over calendar months. A fixture that
 * dropped that window_unit would let a form pass here and be unable to
 * express the real default policy against the actual backend.
 *
 * `schema` mirrors core/internal/config's own constants, so the picker
 * this fixture drives offers exactly what config.Validate accepts.
 */
/** Issue #286: 0 across the board, which is this product's default (no
 *  cap, no warning line, no critical line) and matches how getStorage's
 *  own "default" fixture below reports the disk itself as the
 *  denominator. backupRoot mirrors the derivation the real backend does
 *  when an operator has not named one: the directory the fixture's own
 *  backup sets share. */
function defaultCapacitySettings(): CapacitySettings {
  return {
    capBytes: 0,
    warningFreeBytes: 0,
    criticalFreeBytes: 0,
    safetyMarginBytes: 0,
    backupRoot: "/data/backups",
    backupRootConfigured: false
  };
}

function defaultSettings(): AppSettings {
  return {
    retention: {
      timezone: "Europe/Berlin",
      weekStartsOn: "monday",
      tiers: [
        { name: "daily", granularity: "day", keep: 7 },
        { name: "weekly", granularity: "week", keep: 3, windowUnit: "month" },
        { name: "monthly", granularity: "month", keep: 12 }
      ],
      protectLastKnownGood: true
    },
    capacity: defaultCapacitySettings(),
    schema: {
      retention: {
        granularities: ["day", "week", "month", "quarter", "half_year", "year", "days"],
        windowUnits: ["day", "week", "month", "quarter", "half_year", "year"],
        tierNamePattern: "^[a-z][a-z0-9_]*$",
        reservedTierName: "last_known_good",
        keepMax: 10000,
        periodDaysMax: 3650,
        // The chain core/internal/config.DefaultRetentionTiers resolves to
        // when a config configures neither spelling. Served, not
        // hardcoded in the form, so "Restore default chain" cannot write a
        // stale copy of the product's default into a config file.
        defaultTiers: [
          { name: "daily", granularity: "day", keep: 7 },
          { name: "weekly", granularity: "week", keep: 3, windowUnit: "month" },
          { name: "monthly", granularity: "month", keep: 12 }
        ]
      }
    }
  };
}

/**
 * The operations an instance with no configuration actually serves, taken
 * from apps/common/webhost's newUnconfiguredRouter: the two reads that tell
 * a client which mode it is in, the setup flow itself, the wizard's own
 * pre-save helpers, and the auth routes, which are mounted by a different
 * package and are not gated on configuration at all. Everything else
 * answers 503 NOT_CONFIGURED.
 *
 * Issue #275: this fixture used to serve the FULL dataset in the
 * "first-run" scenario, so `?scenario=first-run` in dev, and every test
 * using it, showed an unconfigured instance behaving like a configured one.
 * That is the fixture lying about the state it exists to reproduce, and it
 * is why nothing caught what the pages do with the refusals they really
 * get.
 */
const SERVED_WHILE_UNCONFIGURED: ReadonlySet<keyof BackupManagerApi> = new Set([
  "getVersion",
  "getFirstRunStatus",
  "completeFirstRun",
  "listValidators",
  "importSSHKey",
  "probeHostKey",
  "testCandidateConnection",
  "login",
  "enrollAdministrator",
  "rotatePassword",
  "logout"
]);

function notConfigured(): BackupManagerError {
  return new BackupManagerError({
    code: "NOT_CONFIGURED",
    message:
      "this instance has not been configured yet; complete the setup flow at /api/v1/system/first-run first",
    correlationId: "cid_mock503"
  });
}

/** Wraps every operation an unconfigured instance does not serve so it
 *  refuses while `isConfigured()` is false, and stops refusing the moment
 *  setup writes a configuration, exactly as the real router's own
 *  configured/unconfigured split does on the next request. */
function refusingWhileUnconfigured(api: BackupManagerApi, isConfigured: () => boolean): BackupManagerApi {
  const wrapped = { ...api } as Record<string, unknown>;
  for (const key of Object.keys(api) as (keyof BackupManagerApi)[]) {
    if (SERVED_WHILE_UNCONFIGURED.has(key)) continue;
    const original = api[key] as (...args: unknown[]) => unknown;
    wrapped[key] = (...args: unknown[]) =>
      isConfigured() ? original(...args) : Promise.reject(notConfigured());
  }
  return wrapped as unknown as BackupManagerApi;
}

export function createMockApi(scenario: Scenario = "default"): BackupManagerApi {
  const empty = scenario === "empty";
  // Every previewRetention call advances this backup set's "inventory" by
  // one tick and issues a plan captured against it. applyRetention only
  // ever honors the plan_id from the LATEST tick — anything older is,
  // correctly, stale — mirroring ApplyRetentionPlan's real revision check.
  let retentionTick = 0;
  // Issue #333: which sets declare a policy of their own, per mock
  // instance. A copy of the fixture rather than the fixture itself, so a
  // write in one test cannot be seen by the next.
  const retentionOverrides = new Map<string, RetentionOverride>(SET_RETENTION_OVERRIDES);
  // Held per mock instance so a PATCH is visible to the next GET, the
  // same way the real backend's hot reload makes a write visible to the
  // next read (issue #140).
  const settings = defaultSettings();
  // Issue #176: a fresh app-store install has no configuration at all.
  // Mutable, because completing setup is what makes it configured — the
  // same one-way transition the real backend makes in-process.
  let configured = scenario !== "first-run";

  const api: BackupManagerApi = {
    getVersion: () =>
      delay(
        scenario === "version-mismatch"
          ? { ...VERSION, service: "1.2.0", api: "v0", compatible: false }
          : VERSION
      ),

    getFirstRunStatus: () => delay({ configured }),

    completeFirstRun: (req: CreateBackupSetRequest): Promise<FirstRunResult> => {
      if (configured)
        return Promise.reject(
          new BackupManagerError({
            code: "unknown",
            message: "This instance is already configured.",
            correlationId: "cid_mock409"
          })
        );
      const set = mockBackupSetFromCreateRequest(req);
      SETS.push(set);
      configured = true;
      return delay({
        backupSet: {
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
          readOnly: !!req.readOnly
        },
        restartRequired: false
      });
    },

    getHealth: () =>
      delay(
        empty
          ? { ...HEALTH, backupHealth: "healthy", backupHealthReason: "No backup sets configured yet.", setsHealthy: 0, setsStale: 0, setsFailing: 0, retainedCount: 0, retainedBytes: 0, quarantinedCount: 0, readOnlyRetainedCount: 0 }
          : scenario === "storage-critical"
            ? { ...HEALTH, storageState: "critical", storageFreeBytes: 0.28 * TB, backupHealth: "failing", backupHealthReason: "Storage is critically low; ingestion has been paused to protect existing backups." }
            : HEALTH
      ),

    // Issue #286. See STORAGE/STORAGE_CRITICAL/STORAGE_EMPTY's own doc
    // for why this is not derived from getHealth's own numbers.
    getStorage: () =>
      delay(empty ? STORAGE_EMPTY : scenario === "storage-critical" ? STORAGE_CRITICAL : STORAGE),

    listSets: () => delay(empty ? [] : withRetentionAttribution(retentionOverrides, SETS)),
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
      return delay(withRetentionAttribution(retentionOverrides, [found])[0]);
    },
    runCycle: () => delay(undefined),
    // The mock ECHOES the window it was given rather than a fixed one, so
    // a screen that dropped windowDays on the way to the client looks
    // wrong here rather than plausible. It says the class's published
    // figure and that a bill exists, and says neither a percentage, a
    // finishing time nor an amount, because there is nowhere in the type
    // to put one.
    restoreCopy: (req) =>
      delay({
        operationId: "op_mock_restore_1",
        status: "running",
        windowDays: req.windowDays,
        wait: "AWS publishes a standard restore from DEEP_ARCHIVE as taking up to twelve hours, and a bulk one up to forty eight; S3 reports a restore as in progress or finished and never reports a percentage or a finishing time",
        billing:
          "the provider bills for retrieving an object from DEEP_ARCHIVE, and this product has no price list, so it cannot and will not tell you the amount"
      }),
    testConnection: () => delay({ ok: true, fingerprint: SETS[0].hostFingerprint }),
    setEnabled: () => delay(undefined),
    setReadOnly: () => delay(undefined),

    // Issue #350. The mock APPLIES the patch to its own SETS entry
    // rather than echoing the request, and applies only the keys the
    // patch carries. Echoing would make every per-box Save look correct
    // in the browser suite no matter what the page sent, which is
    // exactly the class of green-by-construction the e2e harness has
    // been caught by before: a page that sent every field on every Save
    // would be indistinguishable from one that sent only the dirty box.
    updateBackupSet: (source, set, patch) => {
      const found = SETS.find((s) => s.source === source && s.set === set);
      if (!found)
        return Promise.reject(
          new BackupManagerError({
            code: "unknown", message: "That backup set no longer exists.", correlationId: "cid_mock404"
          })
        );
      if (patch.host !== undefined) found.host = patch.host;
      if (patch.port !== undefined) found.port = patch.port;
      if (patch.username !== undefined) found.username = patch.username;
      if (patch.remoteFolder !== undefined) found.remoteFolder = patch.remoteFolder;
      if (patch.destination !== undefined) found.destination = patch.destination;
      if (patch.includePatterns !== undefined) found.includePatterns = [...patch.includePatterns];
      if (patch.completionMethod !== undefined) found.completionMethod = patch.completionMethod;
      return delay({ ...found });
    },

    // Issue #391. The mock actually REMOVES the set from its own SETS
    // fixture rather than resolving and leaving it there, for the reason
    // updateBackupSet above applies the patch rather than echoing it: a
    // page that never called this would be indistinguishable, in the
    // browser suite, from one that did, which is precisely the
    // green-by-construction the no-op confirm handler already got away
    // with once. Removing it also means a test can assert the set is gone
    // afterwards instead of only that a spy was called.
    //
    // A set that is not there is rejected with the CONTRACT's code for
    // it, BACKUP_SET_NOT_FOUND, and not the "unknown" its siblings above
    // use: the detail page branches on that code to tell "already gone"
    // from every other refusal, and a mock that could not produce it
    // would leave that branch untestable through the mock, which is the
    // green-by-construction shape check-client-paths.sh's own header
    // warns about, one layer down.
    removeSet: (source: string, set: string) => {
      const at = SETS.findIndex((s) => s.source === source && s.set === set);
      if (at < 0)
        return Promise.reject(
          new BackupManagerError({
            code: "BACKUP_SET_NOT_FOUND", message: "no such backup set", correlationId: "cid_mock404"
          })
        );
      SETS.splice(at, 1);
      return delay(undefined);
    },

    // Nothing is ever running in the mock, so edit mode opens with no
    // prompt. That is the honest default rather than a convenience: a
    // fixture that claimed a transfer was in flight would make every
    // Edit press in the browser suite go through a confirmation the real
    // product only shows sometimes. A test that wants the warning stubs
    // this one call.
    getEditHold: () => delay({ held: false, running: null }),
    takeEditHold: () => delay({ expiresAt: new Date(Date.now() + 90_000).toISOString(), stopped: null }),
    releaseEditHold: () => delay(undefined),

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
        readOnly: !!req.readOnly,
        operation: runImmediately ? { operationId: "op_mock_" + set.id, status: "completed" } : undefined
      });
    },
    listValidators: (): Promise<ValidatorCatalogEntry[]> => delay(VALIDATORS.map((v) => ({ ...v }))),
    importSSHKey: (): Promise<SSHKeyImportResult> =>
      delay({ id: "key_mock_" + Math.random().toString(36).slice(2, 10), algorithm: "ssh-ed25519", fingerprint: mockImportedKeyFingerprint }),
    probeHostKey: (): Promise<HostKeyProbeResult> =>
      delay({ algorithm: "ssh-ed25519", fingerprint: mockProbedFingerprint, knownHostsLine: "mock-host.internal ssh-ed25519 AAAAC3NzaC1lZDI1NTE5mock" }),
    testCandidateConnection: (): Promise<ConnectionTestOutcome> => delay({ ok: true }),

    listArtifacts: (setId) =>
      delay(empty ? [] : ARTIFACTS.filter((a) => !a.quarantine && (!setId || a.setId === setId))),
    getArtifact: (id) => delay(ARTIFACTS.find((a) => a.id === id) ?? ARTIFACTS[0]),

    listOperations: () => delay(empty ? [] : OPERATIONS),
    listActivity: () => delay(empty ? [] : ACTIVITY),
    listQuarantine: () => delay(empty ? [] : ARTIFACTS.filter((a) => a.quarantine)),
    revalidate: () => delay(undefined),
    retryIngestion: () => delay(undefined),
    reinstate: () =>
      delay({
        reinstated: true,
        checked: true,
        passed: true,
        state: "COMMITTED",
        reason: "recomputed hash still matches the hash recorded at verification"
      }),

    previewRetention: (source, set) => {
      retentionTick += 1;
      return delay(retentionPlan(retentionOverrides, source, set, retentionTick));
    },

    // Issue #333's three per-set retention operations. The write half
    // really applies: setBackupSetRetention stores the submitted policy
    // and clearBackupSetRetention removes it, so the page re-renders from
    // state that changed rather than from an echo of its own request, and
    // a following previewRetention is decided under the new policy.
    //
    // The whole-chain rule is NOT modelled here. It lives in
    // config.Validate and is proved against the real service in
    // core/service/backupsetretention_test.go; a copy of it in a fixture
    // would be a second rule that could pass while the real one failed.
    // The one refusal this mock does carry is the one a form can produce
    // by itself, which is an empty chain.
    getBackupSetRetention: (source, set) => delay(mockBackupSetRetention(retentionOverrides, source, set)),
    setBackupSetRetention: (source, set, policy) => {
      if (policy.tiers && policy.tiers.length === 0)
        return Promise.reject(
          new BackupManagerError({
            code: "INVALID_REQUEST",
            message:
              'retention.tiers must name at least one tier; an empty chain is not "keep nothing", it reinstates the default daily/weekly/monthly policy.',
            correlationId: "cid_mockchain"
          })
        );
      retentionOverrides.set(source + "/" + set, policy);
      return delay(mockBackupSetRetention(retentionOverrides, source, set));
    },
    clearBackupSetRetention: (source, set) => {
      retentionOverrides.delete(source + "/" + set);
      return delay(mockBackupSetRetention(retentionOverrides, source, set));
    },
    applyRetention: (source, set, planId) => {
      // This has to REJECT, not throw synchronously. applyRetention is a
      // Promise, so a bare throw escapes before a promise exists and a
      // caller's .catch() never runs — the one path that must not fail open
      // for a stale retention plan.
      const current = retentionPlan(retentionOverrides, source, set, retentionTick);
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

    getSettings: () => delay(structuredClone(settings)),
    updateSettings: (req: UpdateSettingsRequest) => {
      const r = req.retention;
      const c = req.capacity;
      // The real backend refuses a write that names no setting at all, in
      // both layers and structurally: an absent section and a
      // present-but-empty one are the same request, and honouring either
      // would rewrite the config file and move the config revision for a
      // body with no content (PR #171, mandatory finding M3; issue #286
      // extended this to capacity, the same way). A fixture that resolved
      // with the current settings instead would let a component that
      // sends an empty body pass here and fail against the server.
      const retentionNamesNothing =
        r === undefined ||
        (r.timezone === undefined &&
          r.weekStartsOn === undefined &&
          r.tiers === undefined &&
          r.protectLastKnownGood === undefined);
      const capacityNamesNothing =
        c === undefined ||
        (c.capBytes === undefined &&
          c.warningFreeBytes === undefined &&
          c.criticalFreeBytes === undefined &&
          c.safetyMarginBytes === undefined);
      if (retentionNamesNothing && capacityNamesNothing)
        return Promise.reject(new BackupManagerError({
          code: "INVALID_REQUEST",
          message: "a settings write must name at least one setting to change",
          correlationId: "cid_mocksettings400"
        }));
      // A section that WAS sent but named nothing is refused even when
      // the OTHER section carries a real change: quietly dropping half a
      // request is how a settings page reports success for an edit that
      // never happened.
      if ((r !== undefined && retentionNamesNothing) || (c !== undefined && capacityNamesNothing))
        return Promise.reject(new BackupManagerError({
          code: "INVALID_REQUEST",
          message: "a settings section was sent with no field in it; omit the section instead of sending an empty one",
          correlationId: "cid_mocksettings400"
        }));

      if (r) {
        // Only the named fields move, exactly like the PATCH contract
        // (and like core/service's own applyRetentionUpdate): a mock that
        // overwrote everything would let a component pass here while
        // wiping settings it never touched against the real backend.
        if (r.timezone !== undefined) settings.retention.timezone = r.timezone;
        if (r.weekStartsOn !== undefined) settings.retention.weekStartsOn = r.weekStartsOn;
        if (r.protectLastKnownGood !== undefined) {
          settings.retention.protectLastKnownGood = r.protectLastKnownGood;
        }
        if (r.tiers !== undefined) {
          if (r.tiers.length === 0)
            // The literal refusal core/service returns for this
            // (settings.go): an emptied chain is not "keep nothing", it
            // reinstates the default policy, so it is refused rather than
            // applied. A fixture that accepted it would let a UI ship an
            // affordance the real backend rejects.
            return Promise.reject(new BackupManagerError({
              code: "INVALID_REQUEST",
              message:
                "retention.tiers must name at least one tier; an empty chain is not \"keep nothing\", it reinstates the default daily/weekly/monthly policy.",
              correlationId: "cid_mocksettings400"
            }));
          settings.retention.tiers = r.tiers.map((t) => ({ ...t }));
        }
      }

      if (c) {
        // core/internal/config.validateCapacity's own rules (issue #286):
        // nothing negative, and a cap may not sit at or below the
        // critical floor, since that combination refuses every transfer
        // forever. Checked against what the write would leave in effect
        // (the named fields folded onto the current settings), the same
        // way the real backend validates the WHOLE config rather than
        // only the fields a request happened to touch.
        const capBytes = c.capBytes ?? settings.capacity.capBytes;
        const warningFreeBytes = c.warningFreeBytes ?? settings.capacity.warningFreeBytes;
        const criticalFreeBytes = c.criticalFreeBytes ?? settings.capacity.criticalFreeBytes;
        const safetyMarginBytes = c.safetyMarginBytes ?? settings.capacity.safetyMarginBytes;

        if (capBytes < 0)
          return Promise.reject(new BackupManagerError({
            code: "INVALID_REQUEST",
            message: "capacity.cap_bytes must not be negative; use 0 for no cap",
            correlationId: "cid_mocksettings400"
          }));
        if (warningFreeBytes < 0 || criticalFreeBytes < 0 || safetyMarginBytes < 0)
          return Promise.reject(new BackupManagerError({
            code: "INVALID_REQUEST",
            message: "capacity thresholds must not be negative",
            correlationId: "cid_mocksettings400"
          }));
        if (warningFreeBytes < criticalFreeBytes)
          return Promise.reject(new BackupManagerError({
            code: "INVALID_REQUEST",
            message: "capacity.warning_free_bytes must be at or above capacity.critical_free_bytes",
            correlationId: "cid_mocksettings400"
          }));
        if (capBytes > 0 && criticalFreeBytes > 0 && capBytes <= criticalFreeBytes)
          return Promise.reject(new BackupManagerError({
            code: "INVALID_REQUEST",
            message: "capacity.cap_bytes must be above capacity.critical_free_bytes",
            correlationId: "cid_mocksettings400"
          }));

        if (c.capBytes !== undefined) settings.capacity.capBytes = c.capBytes;
        if (c.warningFreeBytes !== undefined) settings.capacity.warningFreeBytes = c.warningFreeBytes;
        if (c.criticalFreeBytes !== undefined) settings.capacity.criticalFreeBytes = c.criticalFreeBytes;
        if (c.safetyMarginBytes !== undefined) settings.capacity.safetyMarginBytes = c.safetyMarginBytes;
      }

      return delay(structuredClone(settings));
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

  return refusingWhileUnconfigured(api, () => configured);
}
