export type OperationKind =
  | "transfer"
  | "validation"
  | "retention"
  | "reconciliation"
  | "catalog-rebuild";

/**
 * The checklist a run cycle walks, in order. "clean-remote" can never
 * precede "commit".
 *
 * The first five are the stages the service actually reports while a cycle
 * is executing, and they are exactly api/v1/openapi.json's
 * OperationProgress.stage enum. "complete" is the sixth entry of the
 * on-screen checklist and is deliberately NOT one of them: an operation
 * that has completed reports no progress at all (see TransferProgress), so
 * there is nothing for the service to put a "complete" stage on. It exists
 * here so the checklist can show where the work is heading.
 */
export const TRANSFER_STAGES = [
  "discovering",
  "transferring",
  "verifying",
  "committing",
  "cleaning-remote",
  "complete"
] as const;

export type TransferStage = (typeof TRANSFER_STAGES)[number];

/**
 * The stages a RUNNING operation can report: every checklist stage except
 * "complete", which is where the checklist points rather than anything
 * the service ever reports (a completed operation reports no progress at
 * all).
 *
 * Derived from TRANSFER_STAGES rather than restated, so the on-screen
 * order and the reportable set cannot disagree. It is also exactly
 * api/v1/openapi.json's OperationProgress.stage enum, and client.ts
 * assigns the generated wire union straight into it with no cast, so the
 * two drifting apart is a type error rather than a runtime surprise.
 */
export type LiveTransferStage = Exclude<TransferStage, "complete">;

/** The durable operation record's own status, straight off the wire. */
export type OperationStatus = "queued" | "running" | "completed" | "failed";

/**
 * One live reading of an operation that is executing right now.
 *
 * Everything here is measured. There is no field for how far through the
 * whole operation the service is, because a run cycle is a pass over every
 * enabled backup set and what it will find is discovered as it goes, so no
 * honest denominator for the whole exists at any moment before the end.
 * What does exist is which set out of how many, how many artifacts have
 * been finished, and how far the one artifact currently being copied has
 * got.
 *
 * Issue #211 removed nine fields the UI displayed that nothing computed.
 * This type is written so that cannot recur: every optional field here is
 * `undefined` exactly when the service did not measure it, and the one
 * derived number (progressPercent below) is null rather than zero when it
 * cannot be computed.
 */
export interface TransferProgress {
  /** When the service took this reading. */
  observedAt: string;
  /** Increments on every reading, so a client can tell a transfer that is
   *  not moving from a service that has stopped reporting. */
  sequence: number;
  stage: LiveTransferStage;
  /** The set being processed, absent before the first one starts. */
  backupSetId?: string;
  backupSetsDone: number;
  backupSetsTotal: number;
  /** The artifact being worked on, absent during discovery. */
  artifact?: string;
  /** How many artifacts this cycle has finished. There is deliberately no
   *  total beside it; see this type's own doc. */
  artifactsDone: number;
  /** Bytes of the artifact named above, never of the whole cycle. Absent
   *  means "not being measured right now"; a zero means "measured, and
   *  nothing has arrived yet". */
  bytesDone?: number;
  bytesTotal?: number;
  bytesPerSecond?: number;
}

export interface Operation {
  id: string;
  setId: string;
  setName: string;
  kind: OperationKind;
  label: string;
  /** The durable record: queued, running, completed or failed. */
  status: OperationStatus;
  /**
   * The live reading, or null when the service has none.
   *
   * Null is "no progress is available", which is a different answer from
   * "nothing has been transferred", and the difference is the whole point
   * of this field being nullable rather than a percent that defaults to 0.
   * It is null for an operation that has finished, for one that is still
   * queued, and for one that was running when the process died and was
   * swept to failed on the next start: in none of those cases is there a
   * live transfer to report on, and a 0% bar would claim there is one and
   * that it is stalled.
   */
  progress: TransferProgress | null;
  /** True for read-only passes; the UI says so explicitly. */
  nonDestructive: boolean;
  startedAt: string;
}

/**
 * How far through the artifact currently being copied the transfer is, as
 * a whole percent, or null when that cannot be computed.
 *
 * Null, never 0, when the size of the artifact is not known: a bar drawn
 * at 0% is a claim that nothing has moved, and "we do not know how big
 * this is" is not that claim. A caller renders null as an indeterminate
 * state, not as an empty bar.
 *
 * It is a percentage of ONE artifact, not of the operation. A caller must
 * label it as such; see OperationProgress.tsx.
 */
export function progressPercent(progress: TransferProgress): number | null {
  const { bytesDone, bytesTotal } = progress;
  if (bytesDone === undefined || bytesTotal === undefined || bytesTotal <= 0) return null;
  const pct = Math.round((bytesDone / bytesTotal) * 100);
  return Math.max(0, Math.min(100, pct));
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

/**
 * FR-24's backup-freshness picture, as GET /api/v1/system/health reports
 * it, aggregated across every configured backup set by client.ts.
 *
 * Every field here is something the service actually computes. Before
 * issue #211 this type carried five more (service uptime, retained
 * artifact count and bytes, a seven-day success rate), and the shared UI
 * rendered all of them from `createMockApi`; no backend has ever computed
 * any of them. Against a real deployment they would each have rendered a
 * confident zero, which for a success rate and a retained-bytes figure is
 * not a missing value but a wrong one.
 */
export interface SystemHealth {
  /** When the service computed this. */
  generatedAt: string;
  /** The service answered, so it is running. Deliberately separate from
   *  backupHealth (§8): a running daemon is not evidence of anything
   *  about the backups. */
  serviceRunning: boolean;
  /** The backups. A running daemon with stale backups is NOT healthy. */
  backupHealth: "healthy" | "degraded" | "stale" | "failing";
  backupHealthReason: string;
  /** The newest known-good restore point across every set, or null when
   *  no set has produced one yet. */
  newestVerifiedBackupAt: string | null;
  /** The newest backup that finished across every set, trustworthy or
   *  not, or null. */
  lastCompletedBackupAt: string | null;
  /** Hours since the LEAST fresh set's newest known-good backup, or null
   *  when some set has never produced one at all (which is not a large
   *  number of hours; it is an unanswerable question). */
  oldestSetFreshnessHours: number | null;
  setsHealthy: number;
  setsDegraded: number;
  setsStale: number;
  setsFailing: number;
  /** Every quarantined artifact, including the irrecoverable ones. */
  quarantinedCount: number;
  /** Summed across the destinations whose capacity could be read.
   *  storageReadingsUnavailable says how many could not be. */
  storageFreeBytes: number;
  storageTotalBytes: number;
  storageState: "nominal" | "warning" | "critical";
  storageReadingsUnavailable: number;
}

/**
 * What GET /api/v1/system/version reports, as client.ts maps it.
 *
 * Like SystemHealth above, this carries only what the service actually
 * knows. The fields issue #211 removed (a separate UI version, a "core"
 * version distinct from the service's, a database schema number, a build
 * architecture) had no source anywhere in the running system: the version
 * endpoint has never reported any of them, so every value the shared UI
 * showed for them came from the dev-server mock.
 */
export interface VersionInfo {
  /** The /api/v1 contract version this service speaks. */
  api: string;
  /** The running service's own version. */
  service: string;
  /** The build it was made from. */
  buildCommit: string;
  goVersion: string;
  /** The transfer engine's version string, deliberately not named after
   *  the engine (the contract forbids naming an implementation on the
   *  public schema, and this field mirrors it). */
  engine: string;
  /** The optimistic-concurrency token a write echoes back. */
  configRevision: string;
  /** False until the service has finished its startup sequence (§46.1). */
  ready: boolean;
  /** Whether the service speaks the contract version this UI was built
   *  against. When false the UI disables all management actions (§38). */
  compatible: boolean;
}
