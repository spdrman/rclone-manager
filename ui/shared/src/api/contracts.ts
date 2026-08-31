import type { BackupArtifact, BackupSet, RetentionPlan } from "@shared/types/backup";
import type {
  ActivityEvent,
  Operation,
  SystemHealth,
  VersionInfo
} from "@shared/types/operation";

/**
 * Every error code this frontend's backends can actually put on the wire.
 *
 * A runtime array, not a bare type union, because client.ts has to turn an
 * arbitrary string off the network into one of these: `as ApiErrorCode` is
 * an assertion with no check behind it, so an unrecognised code used to
 * flow into `ApiError.code` and silently fail every comparison against it.
 * See toApiErrorCode below.
 *
 * Two naming conventions live here on purpose, because two Go packages do:
 * the kebab-case values are this UI's own design-canvas vocabulary, while
 * apps/common/auth/local (handler.go/csrf.go) and apps/common/webhost
 * (errors.go and every handler in that package) both emit UPPER_SNAKE_CASE
 * and are listed verbatim rather than translated. Translating would mean a
 * mapping table that has to be kept current with two packages; listing the
 * real tokens means a code either appears here or resolves to "unknown",
 * with nothing in between (issue #96's review, mandatory finding M2 — the
 * webhost half of this list was missing entirely, which is why the one
 * branch in this frontend that reads a code, the retention dialog's stale
 * banner, could never match).
 */
export const API_ERROR_CODES = [
  // This UI's own vocabulary (the design canvas's error states).
  "authentication-failed",
  "ssh-host-key-changed",
  "permission-denied",
  "remote-path-missing",
  "checksum-mismatch",
  "backup-stale",
  "storage-critical",
  "version-mismatch",
  "operation-conflict",
  "unknown",

  // apps/common/auth/local (handler.go, csrf.go).
  "UNAUTHENTICATED",
  "RATE_LIMITED",
  "INVALID_REQUEST",
  "ENROLLMENT_CLOSED",
  "BOOTSTRAP_TOKEN_INVALID",
  "INTERNAL_ERROR",
  "CSRF_TOKEN_MISSING",
  "CSRF_TOKEN_MISMATCH",

  // apps/common/webhost (handlers_*.go, errors.go). INVALID_REQUEST,
  // UNAUTHENTICATED and the two CSRF codes are shared with the list above
  // rather than repeated.
  "RETENTION_PLAN_STALE",
  "RETENTION_PLAN_NOT_FOUND",
  "RETENTION_APPLY_BUSY",
  "BACKUP_SET_NOT_FOUND",
  "OPERATION_NOT_FOUND",
  "OPERATION_ALREADY_RUNNING",
  "IDEMPOTENCY_KEY_CONFLICT",
  "CONFIG_REVISION_STALE",
  "SSH_KEY_NOT_FOUND",
  "HOST_KEY_PROBE_FAILED",
  "DESTRUCTIVE_OPERATIONS_DISABLED",
  "INTERNAL"
] as const;

/** Correlation id travels with every failure and is shown under "Advanced
 *  details". Raw stack traces are never rendered (§37). */
export type ApiErrorCode = (typeof API_ERROR_CODES)[number];

const KNOWN_API_ERROR_CODES: ReadonlySet<string> = new Set(API_ERROR_CODES);

/** Narrows a code read off the wire to ApiErrorCode, or "unknown" for
 *  anything this frontend does not know. The one place a network string
 *  becomes an ApiErrorCode: a caller comparing against a literal is then
 *  comparing against a value that really can appear, and an unrecognised
 *  code degrades to the generic error path instead of quietly matching
 *  nothing. */
export function toApiErrorCode(value: unknown): ApiErrorCode {
  return typeof value === "string" && KNOWN_API_ERROR_CODES.has(value)
    ? (value as ApiErrorCode)
    : "unknown";
}

export interface ApiError {
  code: ApiErrorCode;
  /** Operator-facing sentence. Already human. */
  message: string;
  /** What to do next, if anything. */
  remediation?: string;
  correlationId: string;
}

export class BackupManagerError extends Error {
  constructor(readonly api: ApiError) {
    super(api.message);
    this.name = "BackupManagerError";
  }
}

export interface CatalogScanPreview {
  discovered: number;
  valid: number;
  requiresReview: number;
}

/**
 * Issue #146 (B2.7): the add-backup-set wizard's (#98) real write path,
 * backed by apps/common/webhost's create-backup-set, SSH-key-import,
 * host-key-probe and connection-test endpoints.
 *
 * SSHKeyId/knownHostsLine carry a REFERENCE, never key material or an
 * unverified fingerprint directly — importSSHKey and probeHostKey are
 * what produce those references in the first place, mirroring core's own
 * config.Key (a backup set's config never carries raw key bytes, only
 * where to find them).
 */
/**
 * One entry in the registered application-validator catalog
 * (apps/common/webhost's GET /api/v1/validators, backed by
 * core/service's own RegisteredValidators).
 *
 * An id and a label, and deliberately nothing else. The wizard's step 5
 * picklist sends `id` back as CreateBackupSetRequest.validatorId; the
 * script it resolves to is a server-side path this frontend never learns
 * and could not use (docs/EPIC-B-multi-nas.md §26 Step 5: the API/UI
 * layer selects a validator by id, never by naming an executable).
 */
export interface ValidatorCatalogEntry {
  id: string;
  /** One operator-facing sentence: what this validator checks. */
  summary: string;
}

export interface CreateBackupSetRequest {
  sourceName?: string;
  name: string;
  host: string;
  port: number;
  user: string;
  sshKeyId: string;
  knownHostsLine: string;
  remotePath: string;
  localPath: string;
  include: string[];
  completionStrategy: "rename" | "marker" | "stable";
  stableForSeconds?: number;
  staleAfterSeconds?: number;
  /** The registered application validator to run against every artifact
   *  in this set (listValidators), or omitted for none — which is what
   *  every request before issue #162 meant, and still the default. */
  validatorId?: string;
  /** "Save disabled" — excludes the set from every run cycle until an
   *  operator re-enables it. */
  disabled?: boolean;
  /** "Save, enable & run" — submits a run_cycle operation immediately
   *  after this set is persisted. Ignored (never runs anything) when
   *  disabled is true. */
  runImmediately?: boolean;
}

/** What a submitted run_cycle operation looks like from
 *  createBackupSet's own response — deliberately NOT the richer
 *  UI-progress Operation type (types/operation.ts): that type models a
 *  transfer's on-screen progress (stage, percent, bytes/sec, ...), a
 *  different shape from what the backend's run_cycle operation record
 *  itself carries (docs/EPIC-B-multi-nas.md §14). */
export interface RunCycleSubmission {
  operationId: string;
  status: string;
}

export interface CreatedBackupSet {
  id: string;
  sourceName: string;
  name: string;
  host: string;
  port: number;
  user: string;
  remotePath: string;
  localPath: string;
  include: string[];
  completionStrategy: string;
  /** The registered validator this set was saved with, echoed back so a
   *  caller can render what it just persisted without a second fetch.
   *  Empty when none was chosen. */
  validatorId?: string;
  disabled: boolean;
  /** Present only when the request's runImmediately was set AND
   *  honoured (never when disabled was also set — see
   *  CreateBackupSetRequest.runImmediately's own doc). */
  operation?: RunCycleSubmission;
}

export interface SSHKeyImportResult {
  id: string;
  algorithm: string;
  fingerprint: string;
}

export interface HostKeyProbeResult {
  algorithm: string;
  fingerprint: string;
  knownHostsLine: string;
}

export interface ConnectionTestOutcome {
  ok: boolean;
  message?: string;
}

/** The subset of CreateBackupSetRequest's SSH-facing fields a pre-save
 *  connection test needs — everything a subsequent createBackupSet call
 *  would carry, minus the fields that only matter once a set actually
 *  exists (name, paths, completion, ...). */
export interface ConnectionTestParams {
  host: string;
  port: number;
  user: string;
  sshKeyId: string;
  knownHostsLine: string;
  remotePath?: string;
}

/**
 * Issue #140 (B3.7): the server-side settings surface, backed by
 * apps/common/webhost's GET/PATCH /api/v1/settings.
 *
 * One retention tier, exactly as core/internal/config's RetentionTier
 * models it (FR-18's chain, generalized from three hardcoded tiers by
 * issue #156). `periodDays` is required by, and only legal on,
 * granularity "days"; `windowUnit` is optional and empty means "the same
 * as granularity", which is the ordinary case — but it is not decoration:
 * the default weekly tier buckets by week and looks back over calendar
 * MONTHS, so a form without it cannot express the default policy.
 */
export interface RetentionTierSetting {
  name: string;
  granularity: string;
  periodDays?: number;
  keep: number;
  windowUnit?: string;
}

/** The FR-18/FR-19 policy as it is actually deciding. `tiers` is always
 *  the RESOLVED chain: a config file written with the legacy
 *  daily_days/weekly_months/monthly_months sugar reports the three tiers
 *  those keys stand for, so this UI renders one shape for one policy and
 *  never has to know the sugar exists. */
export interface RetentionSettings {
  timezone: string;
  weekStartsOn: string;
  tiers: RetentionTierSetting[];
  /** FR-19. Turning this off is what core/internal/retention calls a
   *  materially more dangerous configuration, and SettingsPage confirms
   *  it before the write. */
  protectLastKnownGood: boolean;
}

/**
 * The closed value sets and bounds the backend validates a retention
 * chain against, served alongside the values themselves.
 *
 * This is read from the server rather than hardcoded here on purpose: the
 * lists come from core/internal/config's own constants, so a granularity
 * added there reaches this form without a second copy in this file
 * silently going stale.
 */
export interface RetentionSchema {
  granularities: string[];
  /** Every granularity except the custom period, which can never measure
   *  a window (config.RetentionTier.windowUnit's own rule). */
  windowUnits: string[];
  /** Anchored regular expression source; safe to pass to `new RegExp`. */
  tierNamePattern: string;
  /** The one name a configured tier may not claim, because FR-19's
   *  protected term already occupies it. */
  reservedTierName: string;
  keepMax: number;
  periodDaysMax: number;
  /** The chain a configuration that spells neither the explicit tier list
   *  nor the legacy scalars resolves to, straight from
   *  core/internal/config.DefaultRetentionTiers.
   *
   *  Served rather than written into this UI because "restore the default
   *  chain" is not a display string: saving it writes an explicit tiers
   *  list, which clears the legacy scalars and permanently migrates a
   *  config that would have tracked the product's default onto a frozen
   *  copy of it. A stale copy here could therefore narrow a real retention
   *  window, silently and in the dangerous direction. */
  defaultTiers: RetentionTierSetting[];
}

export interface AppSettings {
  retention: RetentionSettings;
  schema: { retention: RetentionSchema };
}

/** A PARTIAL update: only the fields named here change, everything else
 *  keeps whatever the config file currently says. Omitting `tiers`
 *  deliberately leaves the chain (and a legacy file's own spelling of it)
 *  untouched, which is what lets a caller flip one toggle without
 *  rewriting a policy it never edited. */
export interface UpdateRetentionSettings {
  timezone?: string;
  weekStartsOn?: string;
  tiers?: RetentionTierSetting[];
  protectLastKnownGood?: boolean;
}

export interface UpdateSettingsRequest {
  retention?: UpdateRetentionSettings;
}

export interface BackupManagerApi {
  getVersion(): Promise<VersionInfo>;
  getHealth(): Promise<SystemHealth>;

  listSets(): Promise<BackupSet[]>;
  getSet(id: string): Promise<BackupSet>;
  runSet(id: string): Promise<void>;
  testConnection(id: string): Promise<{ ok: boolean; fingerprint: string }>;
  setEnabled(id: string, enabled: boolean): Promise<void>;

  /** Issue #146 (B2.7): the wizard's three Save buttons. */
  createBackupSet(req: CreateBackupSetRequest): Promise<CreatedBackupSet>;
  /** Issue #162: the registered application-validator catalog, read by
   *  the wizard's step 5 picklist. Read-only — there is no route that
   *  adds to it, by design. */
  listValidators(): Promise<ValidatorCatalogEntry[]>;
  /** The wizard's "Import key" step (#98 step 2). Sent once; the
   *  caller discards its own copy of privateKeyPem the instant this
   *  resolves, per that step's own on-screen copy. */
  importSSHKey(privateKeyPem: string): Promise<SSHKeyImportResult>;
  /** The wizard's "Verify server" step (#98 step 3): fetches a real
   *  fingerprint for host:port, trusting nothing yet. */
  probeHostKey(host: string, port: number): Promise<HostKeyProbeResult>;
  /** A pre-save reachability/auth check, run before createBackupSet —
   *  distinct from testConnection(id) above, which checks an ALREADY
   *  persisted set. */
  testCandidateConnection(params: ConnectionTestParams): Promise<ConnectionTestOutcome>;

  listArtifacts(setId?: string): Promise<BackupArtifact[]>;
  getArtifact(id: string): Promise<BackupArtifact>;

  listOperations(): Promise<Operation[]>;
  listActivity(): Promise<ActivityEvent[]>;
  listQuarantine(): Promise<BackupArtifact[]>;
  revalidate(artifactId: string): Promise<void>;
  retryIngestion(artifactId: string): Promise<void>;

  /**
   * Server computes and owns the plan. The UI may only apply it by id.
   * `source`/`set` are BackupSet's own two-part identity (core's
   * model.BackupSetID) — apps/common/webhost/router.go's
   * `/backup-sets/{source}/{set}/retention/...` routes key on exactly
   * these, not on BackupSet.id. applyRetention still takes `source`/`set`
   * to build the same URL, even though `planId` alone is what the backend
   * actually resolves the plan by (service.ApplyRetentionPlan's own doc).
   */
  previewRetention(source: string, set: string): Promise<RetentionPlan>;
  applyRetention(source: string, set: string, planId: string): Promise<RetentionPlan>;

  /** Issue #140 (B3.7): the settings surface. getSettings reads the
   *  policy in effect plus the schema it is validated against;
   *  updateSettings applies only the fields the request names and returns
   *  the settings that are now running, so a caller renders what was
   *  actually persisted rather than echoing its own request back. */
  getSettings(): Promise<AppSettings>;
  updateSettings(req: UpdateSettingsRequest): Promise<AppSettings>;

  scanCatalog(): Promise<CatalogScanPreview>;
  rebuildCatalog(): Promise<void>;

  login(username: string, password: string): Promise<void>;
  enrollAdministrator(username: string, password: string): Promise<void>;
  /** apps/common/auth/local's POST /password (issue #128). Requires an
   *  already-authenticated session; rotates the stored password hash and
   *  revokes every other live session for this administrator. */
  rotatePassword(currentPassword: string, newPassword: string): Promise<void>;
  logout(): Promise<void>;
}
