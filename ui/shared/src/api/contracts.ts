import type { BackupArtifact, BackupSet, RetentionPlan } from "@shared/types/backup";
import type {
  ActivityEvent,
  Operation,
  SystemHealth,
  VersionInfo
} from "@shared/types/operation";

/** Correlation id travels with every failure and is shown under "Advanced
 *  details". Raw stack traces are never rendered (§37). */
export type ApiErrorCode =
  | "authentication-failed"
  | "ssh-host-key-changed"
  | "permission-denied"
  | "remote-path-missing"
  | "checksum-mismatch"
  | "backup-stale"
  | "storage-critical"
  | "retention-plan-stale"
  | "version-mismatch"
  | "operation-conflict"
  | "unknown"
  // apps/common/auth/local's own error codes (handler.go/csrf.go),
  // returned directly by every /api/v1/auth/* route. A different naming
  // convention (UPPER_SNAKE_CASE) from the values above, matching that
  // package's own vocabulary rather than being translated to this
  // union's existing kebab-case style - nothing in this frontend yet
  // renders these distinctly from a generic fallback message (see
  // LoginPage.tsx/EnrollmentPage.tsx), so only their PRESENCE here
  // matters for now, so client.ts's `as ApiError` assertion is honest
  // about what the backend can actually send (issue #119's review).
  | "UNAUTHENTICATED"
  | "RATE_LIMITED"
  | "INVALID_REQUEST"
  | "ENROLLMENT_CLOSED"
  | "BOOTSTRAP_TOKEN_INVALID"
  | "INTERNAL_ERROR"
  | "CSRF_TOKEN_MISSING"
  | "CSRF_TOKEN_MISMATCH";

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

export interface BackupManagerApi {
  getVersion(): Promise<VersionInfo>;
  getHealth(): Promise<SystemHealth>;

  listSets(): Promise<BackupSet[]>;
  getSet(id: string): Promise<BackupSet>;
  runSet(id: string): Promise<void>;
  testConnection(id: string): Promise<{ ok: boolean; fingerprint: string }>;
  setEnabled(id: string, enabled: boolean): Promise<void>;

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
