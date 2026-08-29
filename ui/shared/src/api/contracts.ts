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
  | "unknown";

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

  /** Server computes and owns the plan. The UI may only apply it by id. */
  previewRetention(setId: string): Promise<RetentionPlan>;
  applyRetention(planId: string): Promise<void>;

  scanCatalog(): Promise<CatalogScanPreview>;
  rebuildCatalog(): Promise<void>;

  login(username: string, password: string): Promise<void>;
  enrollAdministrator(username: string, password: string): Promise<void>;
  logout(): Promise<void>;
}
