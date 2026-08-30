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
  /**
   * The two halves of core's own model.BackupSetID (core/internal/model/
   * ids.go): `source` names the configured remote source this set backs up
   * from, `set` names this particular backup set under that source.
   * apps/common/webhost's retention routes are the first to key a URL by
   * this composite shape directly (router.go: `/backup-sets/{source}/{set}/
   * retention/...`) rather than by `id` alone, so client.ts's
   * previewRetention/applyRetention take these two fields rather than
   * guessing how to split a flat `id` back into them.
   */
  source: string;
  set: string;
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

/**
 * core/service.RetentionArtifactVerdict (core/service/retention.go),
 * translated by client.ts from apps/common/webhost's snake_case wire shape
 * (handlers_retention.go's retentionVerdictResponse). "KEEP", "DELETE" or
 * "REFUSE" — internal/retention.PruneAction's own three values (FR-20):
 * REFUSE is a third, deliberate outcome distinct from KEEP, not an error —
 * an artifact policy did not select AND that fails a safety check.
 */
export type RetentionVerdictAction = "KEEP" | "DELETE" | "REFUSE";

export interface RetentionVerdict {
  /** The artifact's filename within its backup set, not an opaque id
   *  (service.RetentionArtifactVerdict.Artifact is v.Artifact.Name). */
  artifact: string;
  action: RetentionVerdictAction;
  reason: string;
  /**
   * Populated only for a KEEP verdict: which GFS tier(s) selected it
   * ("DAILY"/"WEEKLY"/"MONTHLY") and/or "LAST_KNOWN_GOOD"
   * (internal/retention.TierLastKnownGood) if last-known-good protection
   * is what kept it. Empty for DELETE/REFUSE.
   */
  tiers: string[];
}

/**
 * docs/EPIC-B-multi-nas.md §15.6's own preview/apply response shape
 * (apps/common/webhost's retentionPlanResponse, translated to camelCase by
 * client.ts). GET .../retention/preview and POST .../retention/apply both
 * return exactly this shape — a caller never has to reconcile "what would
 * happen" against a differently-shaped "what happened".
 *
 * There is deliberately no `stale` field here: whether this plan is stale
 * is derived client-side (state/appNodes.ts's retentionPlanStaleNode) by
 * comparing inventoryRevision/configRevision against what the graph has
 * itself most recently observed, not trusted as a boolean the wire hands
 * over (issue #96's own "causl-ts for staleness, not a boolean parsed off
 * the response").
 */
export interface RetentionPlan {
  /** Server-issued, immutable, single-use. The UI applies exactly this
   *  plan_id or nothing (§17). */
  planId: string;
  backupSetId: string;
  inventoryRevision: string;
  configRevision: string;
  /** RFC3339Nano. After this instant ApplyRetentionPlan always answers
   *  RETENTION_PLAN_STALE, even if nothing else changed. */
  expiresAt: string;
  keepCount: number;
  deleteCount: number;
  reclaimBytes: number;
  /** The durable operation this apply was recorded under. Empty on a plan
   *  a preview returned — a preview creates no operation. */
  operationId?: string;
  verdicts: RetentionVerdict[];
}
