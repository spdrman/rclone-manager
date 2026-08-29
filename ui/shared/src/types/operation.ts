export type OperationKind =
  | "transfer"
  | "validation"
  | "retention"
  | "reconciliation"
  | "catalog-rebuild";

/** Ordered. "clean-remote" can never precede "commit". */
export const TRANSFER_STAGES = [
  "discovering",
  "transferring",
  "verifying",
  "committing",
  "cleaning-remote",
  "complete"
] as const;

export type TransferStage = (typeof TRANSFER_STAGES)[number];

export interface Operation {
  id: string;
  setId: string;
  setName: string;
  kind: OperationKind;
  stage: TransferStage | null;
  label: string;
  percent: number;
  bytesDone?: number;
  bytesTotal?: number;
  bytesPerSecond?: number;
  etaSeconds?: number;
  itemsDone?: number;
  itemsTotal?: number;
  /** True for read-only passes; the UI says so explicitly. */
  nonDestructive: boolean;
  startedAt: string;
}

export type Severity = "info" | "ok" | "warn" | "error";

export type ActivityEventType =
  | "backup-discovered"
  | "transfer-started"
  | "transfer-complete"
  | "verification-passed"
  | "backup-committed"
  | "remote-source-deleted"
  | "retention-completed"
  | "validation-failed"
  | "host-key-changed"
  | "storage-critical"
  | "configuration-updated";

export interface ActivityEvent {
  id: string;
  at: string;
  type: ActivityEventType;
  severity: Severity;
  setId: string | null;
  setName: string;
  text: string;
  detail: string;
  correlationId: string;
}

export interface SystemHealth {
  /** The daemon. Deliberately separate from backupHealth (§8). */
  serviceRunning: boolean;
  serviceUptimeHours: number;
  /** The backups. A running daemon with stale backups is NOT healthy. */
  backupHealth: "healthy" | "degraded" | "stale" | "failing";
  backupHealthReason: string;
  lastSuccessfulCycleAt: string;
  newestVerifiedBackupAt: string;
  oldestSetFreshnessHours: number;
  setsHealthy: number;
  setsStale: number;
  setsFailing: number;
  quarantinedCount: number;
  retainedCount: number;
  retainedBytes: number;
  storageFreeBytes: number;
  storageTotalBytes: number;
  storageState: "nominal" | "warning" | "critical";
  successRate7d: number;
}

export interface VersionInfo {
  ui: string;
  service: string;
  core: string;
  rclone: string;
  schema: number;
  architecture: string;
  buildCommit: string;
  /** When false the UI disables all management actions (§38). */
  compatible: boolean;
}
