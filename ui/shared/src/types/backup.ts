export type HealthState = "healthy" | "degraded" | "stale" | "failing";

export type CompletionMethod =
  | "atomic-rename"
  | "completion-marker"
  | "stable-size";

export type RetentionClass = "daily" | "weekly" | "monthly" | "protected";

export type ValidationKind = "transfer" | "checksum" | "application";

/**
 * Mirrors core's config.Retention (core/internal/config/config.go) shape,
 * field for field.
 *
 * This type is modeled per BackupSet below, and mock.ts's fixtures give
 * different backup sets different values, but that is not evidence a
 * per-set override is a real, working capability: the actual backend
 * (internal/config, internal/retention) has exactly one Retention block
 * for the whole Config, applied to every backup set. Issue #111 (B3.6)
 * decided, explicitly, to keep retention policy global for now rather
 * than let this type's already-drawn per-set shape settle the question by
 * accident; see config.go's own "Global, not per-backup-set" doc for the
 * full reasoning. A real per-backup-set override is a legitimate future
 * capability, but it needs its own schema/validation/resolution-order
 * design on the backend first, which this type alone does not provide.
 */
export interface RetentionPolicy {
  daily: number;
  weekly: number;
  monthly: number;
  timezone: string;
  weekStartsOn: "monday" | "sunday";
  protectLastKnownGood: boolean;
}

export interface BackupSet {
  id: string;
  name: string;
  host: string;
  port: number;
  username: string;
  remoteFolder: string;
  includePatterns: string[];
  excludePatterns: string[];
  completionMethod: CompletionMethod;
  destination: string;
  retention: RetentionPolicy;
  validations: ValidationKind[];
  state: HealthState;
  /** Human sentence explaining the state. Never rely on colour alone. */
  stateNote: string;
  enabled: boolean;
  halted: boolean;
  haltReason?: "host-key-changed" | "storage-critical" | "manual";
  newestKnownGoodAt: string | null;
  lastRunAt: string | null;
  lastValidation: "passed" | "failed" | "not-run";
  expectedIntervalHours: number;
  retainedCount: number;
  retainedBytes: number;
  hostFingerprint: string;
  fingerprintTrustedAt: string | null;
}

export interface BackupArtifact {
  id: string;
  setId: string;
  setName: string;
  filename: string;
  remoteOriginalPath: string;
  localPath: string;
  producedAt: string;
  receivedAt: string;
  sizeBytes: number;
  checksum: string;
  checksumAlgorithm: "sha256";
  validation: "verified" | "failed" | "pending";
  retentionClasses: RetentionClass[];
  /** Remote deletion is a lifecycle FACT, never a user action. */
  remoteSourceRemovedAt: string | null;
  quarantine: QuarantineRecord | null;
}

export type QuarantineReason =
  | "checksum-mismatch"
  | "validation-failed"
  | "unexpected-artifact"
  | "remote-identity-changed"
  | "incomplete-transfer";

export interface QuarantineRecord {
  reason: QuarantineReason;
  detectedAt: string;
  /** Quarantined artifacts never trigger remote deletion. */
  remoteSourceRetained: true;
}

export interface RetentionPlan {
  /** Server-issued, immutable. The UI applies exactly this or nothing. */
  planId: string;
  setId: string;
  createdAt: string;
  stale: boolean;
  keep: Array<{ artifactId: string; date: string; classes: RetentionClass[] }>;
  delete: Array<{ artifactId: string; date: string; reason: string }>;
  reclaimBytes: number;
  protectedArtifactId: string | null;
}
