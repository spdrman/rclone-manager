import { BackupManagerError, toApiErrorCode } from "./contracts";
// The wire shapes below are GENERATED from api/v1/openapi.json, not
// declared here. Before issue #166 this file carried its own hand-written
// copy of every snake_case response body, transcribed from the Go
// handlers - a second source of truth that compiled perfectly while
// disagreeing with the server, which is how a WireBackupSet missing the
// health and retention fields reached a page that dereferenced them (issue
// #146's review, mandatory finding M4). Re-run scripts/api/generate.sh
// after a contract change; scripts/api/check-contract-drift.sh fails CI if
// the checked-in generated module stops matching the contract.
import { API_VERSION } from "./generated/contract";
import type {
  WireActivityEvent,
  WireArtifact,
  WireArtifactReinstateResponse,
  WireBackupSet,
  WireBackupSetSpec,
  WireCatalogReportResponse,
  WireCompleteFirstRunResponse,
  WireCreateBackupSetRequest,
  WireCreateBackupSetResponse,
  WireFirstRunStatusResponse,
  WireHealthResponse,
  WireListActivityResponse,
  WireListArtifactsResponse,
  WireListBackupSetsResponse,
  WireListOperationsResponse,
  WireOperation,
  WireRetentionPlan,
  WireRetentionTier,
  WireSettingsResponse,
  WireVersionResponse
} from "./generated/contract";
import type {
  ApiError,
  AppSettings,
  BackupManagerApi,
  CatalogScanPreview,
  ConnectionTestOutcome,
  ConnectionTestParams,
  CreateBackupSetRequest,
  CreatedBackupSet,
  RetentionTierSetting,
  SSHKeyImportResult,
  UpdateSettingsRequest
} from "./contracts";
import type {
  BackupArtifact,
  BackupSet,
  CompletionMethod,
  QuarantineReason,
  RetentionClass,
  RetentionPlan,
  RetentionVerdictAction
} from "@shared/types/backup";
import type {
  ActivityEvent,
  ActivityEventType,
  Operation,
  Severity,
  SystemHealth,
  VersionInfo
} from "@shared/types/operation";

const BASE = "/api/v1";

/** Reads name's value out of document.cookie, or "" if it isn't set. */
function readCookie(name: string): string {
  const match = document.cookie.match(new RegExp("(?:^|; )" + name + "=([^;]*)"));
  return match ? decodeURIComponent(match[1]) : "";
}

/**
 * apps/common/auth/local's double-submit CSRF cookie (backend doc:
 * apps/common/auth/local/csrf.go). Every response this service sends —
 * including the very first page load — carries a bm_csrf cookie; a
 * state-changing request has to echo its value back as this header, or
 * the backend refuses it with 403 CSRF_TOKEN_MISMATCH.
 */
const CSRF_COOKIE_NAME = "bm_csrf";
const CSRF_HEADER_NAME = "X-CSRF-Token";

/**
 * apps/common/auth/local's single-use enrollment secret (backend doc:
 * apps/common/auth/local/handler.go's BootstrapTokenHeader), printed to
 * the container's own log as a link (".../enroll?token=..."). There is
 * no form field for it in EnrollmentPage.tsx — the design canvas
 * (docs/design/Backup Manager.dc.html) doesn't show one either — so it
 * travels as a URL query parameter instead, read here rather than
 * plumbed through BackupManagerApi.enrollAdministrator's own signature.
 */
const BOOTSTRAP_TOKEN_HEADER = "X-Bootstrap-Token";

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers: Record<string, string> = {
    "content-type": "application/json",
    ...((init?.headers as Record<string, string> | undefined) ?? {})
  };

  // Read-only requests never need a CSRF token (nothing to forge that
  // would matter), and gating them would only risk failing before the
  // very first response has had a chance to set the cookie at all.
  const method = (init?.method ?? "GET").toUpperCase();
  if (method !== "GET" && method !== "HEAD") {
    const csrf = readCookie(CSRF_COOKIE_NAME);
    if (csrf) headers[CSRF_HEADER_NAME] = csrf;
  }

  if (path === "/auth/enroll") {
    const bootstrapToken = new URLSearchParams(window.location.search).get("token");
    if (bootstrapToken) headers[BOOTSTRAP_TOKEN_HEADER] = bootstrapToken;
  }

  const res = await fetch(BASE + path, {
    credentials: "same-origin",
    ...init,
    // headers last: spreading ...init after a merged `headers` object
    // would otherwise silently replace it with init.headers alone
    // whenever a caller passes its own headers (none do today, but the
    // ordering bug is easy to reintroduce without noticing — see
    // client.test.ts's own coverage for this).
    headers
  });

  if (!res.ok) {
    // The service always returns a typed error envelope, but not always
    // the SAME shape: apps/common/auth/local's own routes (login, enroll,
    // password rotation, logout) answer flat — { code, message,
    // correlationId } all at the top level (see client.test.ts's own
    // ApiErrorCode coverage test for that package's exact vocabulary) —
    // while apps/common/webhost's routes (issue #146's backup-sets/
    // ssh-keys/ssh endpoints, and every future one built the same way)
    // nest code/message under an "error" key and carry the correlation
    // id only in the X-Correlation-Id response header, never the body
    // (see that package's errors.go). Both are read here, rather than
    // this file picking one shape and getting the other's errors back
    // as silently-undefined fields.
    let api: ApiError;
    try {
      const body = (await res.json()) as Record<string, unknown>;
      const headerCorrelationId = res.headers.get("x-correlation-id") ?? undefined;
      const nested = body.error;
      if (nested && typeof nested === "object") {
        const err = nested as Record<string, unknown>;
        api = {
          code: toApiErrorCode(err.code),
          message: err.message as string,
          correlationId: headerCorrelationId ?? "unavailable"
        };
      } else {
        api = {
          code: toApiErrorCode(body.code),
          message: body.message as string,
          correlationId: (body.correlationId as string) ?? headerCorrelationId ?? "unavailable"
        };
      }
    } catch {
      api = {
        code: "unknown",
        message: "The backup service returned an unexpected response.",
        correlationId: res.headers.get("x-correlation-id") ?? "unavailable"
      };
    }
    throw new BackupManagerError(api);
  }

  return res.status === 204 ? (undefined as T) : ((await res.json()) as T);
}

const post = (path: string, body?: unknown) =>
  request<void>(path, { method: "POST", body: body ? JSON.stringify(body) : undefined });

/**
 * apps/common/auth/local's routes use camelCase JSON (matching Go's
 * `json:"currentPassword"`-style tags above), but apps/common/webhost's
 * routes use snake_case (handlers_backupsets.go's, handlers_ssh.go's and
 * handlers_retention.go's own `json:"remote_path"`/`json:"plan_id"`-style
 * tags), a genuine, pre-existing split between the two packages this file
 * did not introduce. The wireX/fromWireX helpers below are the one place
 * that translation happens, so the two shapes never have to be kept in
 * sync by hand at every call site: a wireX builds a snake_case request
 * body out of one of this file's own camelCase request types, and a
 * fromWireX reads a snake_case response back into the camelCase domain
 * type the rest of the app already speaks. Nothing past these helpers
 * ever sees a snake_case key.
 */
/** The contract's BackupSetSpec: everything that DESCRIBES a backup set,
 *  and nothing that asks for one to be run. Two operations take exactly
 *  this body (POST /backup-sets and POST /system/first-run), which is why
 *  it is built here once rather than at each of them. */
function wireBackupSetSpec(req: CreateBackupSetRequest): WireBackupSetSpec {
  return {
    source_name: req.sourceName,
    name: req.name,
    host: req.host,
    port: req.port,
    user: req.user,
    ssh_key_id: req.sshKeyId,
    known_hosts_line: req.knownHostsLine,
    remote_path: req.remotePath,
    local_path: req.localPath,
    include: req.include,
    completion_strategy: req.completionStrategy,
    validator_id: req.validatorId,
    stable_for_seconds: req.stableForSeconds,
    stale_after_seconds: req.staleAfterSeconds,
    disabled: req.disabled
  };
}

function wireCreateBackupSetRequest(req: CreateBackupSetRequest): WireCreateBackupSetRequest {
  return { ...wireBackupSetSpec(req), run_immediately: req.runImmediately };
}

function wireConnectionTestParams(params: ConnectionTestParams) {
  return {
    host: params.host,
    port: params.port,
    user: params.user,
    ssh_key_id: params.sshKeyId,
    known_hosts_line: params.knownHostsLine,
    remote_path: params.remotePath
  };
}

/** Translates WireCreateBackupSetResponse into CreatedBackupSet's camelCase
 *  shape this file's callers use. */
function fromWireCreateBackupSetResponse(body: WireCreateBackupSetResponse): CreatedBackupSet {
  return {
    id: body.id,
    sourceName: body.source_name,
    name: body.name,
    host: body.host,
    port: body.port,
    user: body.user,
    remotePath: body.remote_path,
    localPath: body.local_path,
    include: body.include,
    completionStrategy: body.completion_strategy,
    validatorId: body.validator_id,
    disabled: body.disabled,
    operation: body.operation
      ? { operationId: body.operation.operation_id, status: body.operation.status }
      : undefined,
    // The contract declares run_error on this response precisely so a
    // failed immediate run is reportable without failing the create
    // (both are 201). Mapping it is not optional: unmapped, "Save,
    // enable & run" reports plain success for a run that never started.
    runError: body.run_error || undefined
  };
}

const COMPLETION_STRATEGY_TO_METHOD: Record<string, CompletionMethod> = {
  rename: "atomic-rename",
  marker: "completion-marker",
  stable: "stable-size"
};

/**
 * Maps a WireBackupSet onto BackupSet's full shape. Every field the wire
 * response actually carries maps across (with a name/polarity fix:
 * `user` -> `username`, `remote_path` -> `remoteFolder`,
 * `completion_strategy` -> `completionMethod`'s own vocabulary, and
 * `disabled` -> the INVERSE of `enabled`, not the same boolean under a
 * different name).
 *
 * Every field the backend does not yet compute — health/retention/
 * validation/last-run data, none of which exists anywhere in
 * core/service yet — gets an honest, clearly-labeled placeholder instead
 * of being left `undefined` against a type that declares it required:
 * `state: "stale"` with a stateNote that says why, zeroed counters,
 * empty arrays, null timestamps. This is deliberately NOT the richer fix
 * (enriching the Go response to compute real health/retention data) —
 * that data does not exist yet anywhere in this codebase to enrich FROM
 * — so BackupSetsPage/BackupSetDetailPage render a correctly-typed, if
 * visibly incomplete, page against a real deployment instead of
 * throwing, until a future issue actually computes this data server-side.
 */
function fromWireBackupSet(bs: WireBackupSet): BackupSet {
  return {
    id: bs.id,
    // BackupSet.source/BackupSet.set are model.BackupSetID's own two
    // halves, and the wire response already carries both separately
    // (`source_name` and `name`, which core/service joins with a "/" to
    // build the very `id` above). Taking them from those two fields, not
    // by splitting `id` back apart, is what keeps the retention routes'
    // `{source}/{set}` URL correct for a name that itself contains
    // anything id-splitting would get wrong.
    source: bs.source_name,
    set: bs.name,
    name: bs.name,
    host: bs.host,
    port: bs.port,
    username: bs.user,
    remoteFolder: bs.remote_path,
    includePatterns: bs.include,
    excludePatterns: [],
    completionMethod: COMPLETION_STRATEGY_TO_METHOD[bs.completion_strategy] ?? "atomic-rename",
    destination: bs.local_path,
    retention: {
      daily: 0,
      weekly: 0,
      monthly: 0,
      timezone: "UTC",
      weekStartsOn: "monday",
      protectLastKnownGood: false
    },
    validations: [],
    state: "stale",
    stateNote: "Health details are not yet reported by the server for this backup set.",
    enabled: !bs.disabled,
    halted: false,
    newestKnownGoodAt: null,
    lastRunAt: null,
    lastValidation: "not-run",
    expectedIntervalHours: 0,
    retainedCount: 0,
    retainedBytes: 0,
    hostFingerprint: "",
    fingerprintTrustedAt: null
  };
}

function fromWireRetentionPlan(wire: WireRetentionPlan): RetentionPlan {
  return {
    planId: wire.plan_id,
    backupSetId: wire.backup_set_id,
    inventoryRevision: wire.inventory_revision,
    configRevision: wire.config_revision,
    expiresAt: wire.expires_at,
    keepCount: wire.keep_count,
    deleteCount: wire.delete_count,
    reclaimBytes: wire.reclaim_bytes,
    operationId: wire.operation_id,
    verdicts: wire.verdicts.map((v) => ({
      artifact: v.artifact,
      action: v.action as RetentionVerdictAction,
      reason: v.reason,
      tiers: v.tiers ?? []
    }))
  };
}

/** apps/common/webhost/router.go's `{source}/{set}` route params
 *  (model.BackupSetID's own composite shape), URL-encoded independently —
 *  see BackupSet.source/BackupSet.set's own doc (types/backup.ts). */
function fromWireTier(t: WireRetentionTier): RetentionTierSetting {
  return {
    name: t.name,
    granularity: t.granularity,
    // The backend omits both optional keys rather than sending a zero or
    // an empty string (their `omitempty` tags), so they come back
    // undefined here and stay undefined rather than being normalised to
    // 0/"" — which would send a stray period_days back on the next write
    // and get the whole policy refused.
    periodDays: t.period_days,
    keep: t.keep,
    windowUnit: t.window_unit
  };
}

function wireTier(t: RetentionTierSetting): WireRetentionTier {
  return {
    name: t.name,
    granularity: t.granularity,
    // Same rule in the other direction: only a positive period_days and a
    // non-empty window_unit are legal to send at all, so anything else is
    // omitted rather than sent as 0/"".
    period_days: t.periodDays && t.periodDays > 0 ? t.periodDays : undefined,
    keep: t.keep,
    window_unit: t.windowUnit ? t.windowUnit : undefined
  };
}

function fromWireSettingsResponse(body: WireSettingsResponse): AppSettings {
  return {
    retention: {
      timezone: body.retention.timezone,
      weekStartsOn: body.retention.week_starts_on,
      tiers: (body.retention.tiers ?? []).map(fromWireTier),
      protectLastKnownGood: body.retention.protect_last_known_good
    },
    schema: {
      retention: {
        granularities: body.schema.retention.granularities,
        windowUnits: body.schema.retention.window_units,
        tierNamePattern: body.schema.retention.tier_name_pattern,
        reservedTierName: body.schema.retention.reserved_tier_name,
        keepMax: body.schema.retention.keep_max,
        periodDaysMax: body.schema.retention.period_days_max,
        defaultTiers: (body.schema.retention.default_tiers ?? []).map(fromWireTier)
      }
    }
  };
}

/** Builds the PATCH body, carrying "the caller did not name this field"
 *  through as an ABSENT key rather than a null or a zero. The backend
 *  reads an absent key as "leave this alone" and an explicitly empty
 *  tiers list as a refusable request, so collapsing the two here would
 *  silently turn one into the other. */
function wireUpdateSettings(req: UpdateSettingsRequest) {
  if (!req.retention) return {};
  const r = req.retention;
  const retention: Record<string, unknown> = {};
  if (r.timezone !== undefined) retention.timezone = r.timezone;
  if (r.weekStartsOn !== undefined) retention.week_starts_on = r.weekStartsOn;
  if (r.tiers !== undefined) retention.tiers = r.tiers.map(wireTier);
  if (r.protectLastKnownGood !== undefined) {
    retention.protect_last_known_good = r.protectLastKnownGood;
  }
  return { retention };
}

/** RFC3339 or "" off the wire, as a nullable timestamp. The API omits a
 *  timestamp for an event that has not happened rather than sending a zero
 *  date, so an absent key is the ordinary case, not an error. */
const stampOrNull = (value: string | undefined): string | null => value || null;

/** The later of two nullable timestamps. */
function laterOf(a: string | null, b: string | null): string | null {
  if (!a) return b;
  if (!b) return a;
  return Date.parse(a) >= Date.parse(b) ? a : b;
}

function fromWireVersion(body: WireVersionResponse): VersionInfo {
  return {
    api: body.api_version,
    service: body.core_version,
    buildCommit: body.commit,
    goVersion: body.go_version,
    engine: body.engine_version,
    configRevision: body.config_revision,
    ready: body.ready,
    // §38's compatibility check, made concrete: the service names the
    // /api/v1 contract version it speaks, and this UI was generated
    // against exactly one. Before issue #211 this flag came off the wire
    // as its own boolean, which no endpoint ever sent, so it was
    // undefined against a real backend and the read-only banner could
    // never fire.
    compatible: body.api_version === API_VERSION
  };
}

/** Worst-first, so a deployment's headline is its least healthy set.
 *  Order matters: a FAILING set next to nine HEALTHY ones is not a
 *  healthy deployment. */
const HEALTH_ORDER = ["FAILING", "STALE", "DEGRADED", "HEALTHY"] as const;

const HEALTH_STATE: Record<string, SystemHealth["backupHealth"]> = {
  HEALTHY: "healthy",
  DEGRADED: "degraded",
  STALE: "stale",
  FAILING: "failing"
};

const HOUR_MS = 3_600_000;

/**
 * Collapses the per-set health report into the one summary the dashboard
 * renders.
 *
 * The aggregation lives here, in the one translation layer, rather than in
 * a component: two screens reading the same report must not be able to
 * disagree about what "the deployment is stale" means.
 *
 * Two rules are worth stating out loud, because getting either one wrong
 * produces a confidently wrong screen rather than a visibly broken one:
 *
 *   - The headline is the WORST set's verdict and its reason, never an
 *     average and never the first set's. A deployment with one failing set
 *     is failing.
 *   - A set that has never produced a known-good backup makes
 *     oldestSetFreshnessHours null, not a large number. "How stale is the
 *     least fresh set" has no answer when a set has no fresh point to
 *     measure from, and rendering a made-up number there is exactly the
 *     kind of false precision an operator would act on.
 */
function fromWireHealth(body: WireHealthResponse, now: number): SystemHealth {
  const sets = body.backup_sets ?? [];

  const counts = { healthy: 0, degraded: 0, stale: 0, failing: 0 };
  let newestVerified: string | null = null;
  let lastCompleted: string | null = null;
  let quarantined = 0;
  let freeBytes = 0;
  let totalBytes = 0;
  let unavailable = 0;
  let worstStorage = 0; // index into STORAGE_ORDER
  let oldestFreshnessHours: number | null = 0;

  for (const set of sets) {
    counts[HEALTH_STATE[set.state] ?? "degraded"] += 1;
    newestVerified = laterOf(newestVerified, stampOrNull(set.newest_good_backup_at));
    lastCompleted = laterOf(lastCompleted, stampOrNull(set.last_completed_backup_at));
    quarantined += set.quarantined_count + set.quarantined_lost_count;

    if (set.free_bytes_known) {
      freeBytes += set.free_bytes ?? 0;
      totalBytes += set.total_bytes ?? 0;
    } else {
      unavailable += 1;
    }
    worstStorage = Math.max(worstStorage, STORAGE_ORDER.indexOf(set.storage_level ?? "OK"));

    if (oldestFreshnessHours !== null) {
      const newest = stampOrNull(set.newest_good_backup_at);
      oldestFreshnessHours = newest
        ? Math.max(oldestFreshnessHours, Math.floor((now - Date.parse(newest)) / HOUR_MS))
        : null;
    }
  }

  const worst = HEALTH_ORDER.find((state) => sets.some((set) => set.state === state));
  const worstSet = worst ? sets.find((set) => set.state === worst) : undefined;

  return {
    generatedAt: body.generated_at,
    // The service answered this request, so it is running. That is the
    // whole claim, and §8 is emphatic that it says nothing about the
    // backups: backupHealth beside it is the verdict that matters.
    serviceRunning: true,
    backupHealth: worst ? HEALTH_STATE[worst] : "healthy",
    backupHealthReason:
      worstSet?.reason ?? "No backup sets are configured yet, so there is nothing to report on.",
    newestVerifiedBackupAt: newestVerified,
    lastCompletedBackupAt: lastCompleted,
    oldestSetFreshnessHours: sets.length ? oldestFreshnessHours : null,
    setsHealthy: counts.healthy,
    setsDegraded: counts.degraded,
    setsStale: counts.stale,
    setsFailing: counts.failing,
    quarantinedCount: quarantined,
    storageFreeBytes: freeBytes,
    storageTotalBytes: totalBytes,
    storageState: STORAGE_STATE[STORAGE_ORDER[worstStorage]],
    storageReadingsUnavailable: unavailable
  };
}

/** Least severe first, so Math.max over the indices finds the worst. */
const STORAGE_ORDER = ["OK", "WARNING", "CRITICAL"] as const;

const STORAGE_STATE: Record<string, SystemHealth["storageState"]> = {
  OK: "nominal",
  WARNING: "warning",
  CRITICAL: "critical"
};

/**
 * Maps a wire artifact onto BackupArtifact.
 *
 * `quarantine` is a nested record or null rather than a pair of loose
 * fields, because "is this quarantined" and "why" must not be able to
 * disagree: a client cannot render a reason for an artifact that is not
 * held, or hold one with no reason.
 *
 * The reason itself is narrowed to the closed vocabulary the UI presents.
 * The wire carries a free-text explanation (whatever routed the artifact
 * into quarantine), which is deliberately NOT a closed enum server-side:
 * the reasons an artifact can be distrusted are not a fixed list, and
 * pretending otherwise on the wire would mean either a lossy enum or a
 * contract change per new reason. So the free text is kept verbatim as the
 * detail and the category is derived here, defaulting to
 * "validation-failed", which is the honest general case rather than a
 * specific claim about checksums.
 */
function fromWireArtifact(a: WireArtifact): BackupArtifact {
  return {
    id: a.id,
    setId: a.backup_set_id,
    setName: a.set_name,
    filename: a.name,
    remoteOriginalPath: a.remote_path,
    localPath: a.local_path,
    producedAt: a.discovered_at,
    receivedAt: a.updated_at,
    sizeBytes: a.size_bytes,
    checksum: a.checksum ?? "",
    checksumAlgorithm: a.checksum_algorithm ?? "",
    validation:
      a.validation === "passed" ? "verified" : a.validation === "failed" ? "failed" : "pending",
    // The backend records which retention tier last selected an artifact,
    // as ONE tier name rather than the classification set this type
    // models. Reporting the one it actually has is the honest mapping;
    // inventing the others would be a claim about a policy this response
    // does not carry.
    retentionClasses: retentionClassesFor(a.retention_tier),
    remoteSourceRemovedAt: stampOrNull(a.remote_source_removed_at),
    quarantine: a.quarantined
      ? {
          reason: quarantineReasonFor(a),
          detectedAt: a.updated_at,
          remoteSourceRetained: true
        }
      : null
  };
}

/**
 * Narrows a recorded retention tier name onto RetentionClass, which is a
 * CLOSED four-value vocabulary while FR-18's tier chain is operator-defined
 * and open (core/internal/config's Retention.Tiers, so "SEMI_ANNUAL" or
 * "FORTNIGHTLY" are ordinary values).
 *
 * An unrecognised tier therefore yields NO class rather than being forced
 * into one. Forcing it would have to pick a value, and every value here is
 * a specific claim: "protected" in particular is FR-19's last-known-good
 * protection, which means "retention will never delete this", and claiming
 * that for a tier this UI simply does not recognise is the one direction
 * that is actively dangerous.
 *
 * The open vocabulary is rendered elsewhere, by RetentionTierBadges
 * (components/RetentionBadge.tsx), which badges an unknown tier under its
 * own name. This field is not that: it is the closed badge row, and the
 * detail page already renders an empty one as "unclassified".
 */
const RETENTION_CLASS_BY_TIER: Record<string, RetentionClass> = {
  daily: "daily",
  weekly: "weekly",
  monthly: "monthly",
  last_known_good: "protected"
};

function retentionClassesFor(tier: string | undefined): RetentionClass[] {
  const known = tier ? RETENTION_CLASS_BY_TIER[tier.toLowerCase()] : undefined;
  return known ? [known] : [];
}

function quarantineReasonFor(a: WireArtifact): QuarantineReason {
  const reason = (a.quarantine_reason ?? "").toLowerCase();
  if (reason.includes("hash") || reason.includes("checksum")) return "checksum-mismatch";
  if (reason.includes("identity") || reason.includes("host key")) return "remote-identity-changed";
  if (reason.includes("incomplete") || reason.includes("transfer")) return "incomplete-transfer";
  if (reason.includes("unexpected")) return "unexpected-artifact";
  return "validation-failed";
}

/**
 * Maps one recorded lifecycle transition onto the activity feed's own
 * vocabulary.
 *
 * The severity and the headline are derived HERE and not read off the
 * wire, deliberately. Which moves deserve an operator's attention, and
 * what to call them, is presentation: baking it into the contract would
 * freeze one client's editorial judgement for every other client, and the
 * API has no business deciding that a transfer completing is "ok" while a
 * discovery is "info".
 *
 * A transition this table does not name still appears in the feed, as an
 * "info" event captioned with the states themselves. Dropping it would be
 * worse than showing it plainly: an unexplained gap in an audit trail is
 * indistinguishable from nothing having happened.
 */
const ACTIVITY_BY_STATE: Record<string, { type: ActivityEventType; severity: Severity; text: string }> = {
  DISCOVERED: { type: "backup-discovered", severity: "info", text: "Backup discovered on the source" },
  TRANSFERRING: { type: "transfer-started", severity: "info", text: "Transfer started" },
  TRANSFERRED: { type: "transfer-complete", severity: "ok", text: "Transfer complete" },
  VERIFIED: { type: "verification-passed", severity: "ok", text: "Verification passed" },
  COMMITTED: { type: "backup-committed", severity: "ok", text: "Backup committed" },
  COMPLETE: { type: "remote-source-deleted", severity: "ok", text: "Remote source released" },
  QUARANTINED: { type: "validation-failed", severity: "error", text: "Quarantined for review" },
  QUARANTINED_LOST: { type: "validation-failed", severity: "error", text: "Quarantined, with no source left to recover from" },
  FAILED: { type: "validation-failed", severity: "warn", text: "Attempt failed" }
};

function fromWireActivityEvent(e: WireActivityEvent): ActivityEvent {
  const known = ACTIVITY_BY_STATE[e.to];
  return {
    // The transition log has no id column of its own on the wire, and one
    // artifact legitimately appears many times, so the key is the artifact
    // plus the moment plus the state entered. That is unique for the same
    // reason the journal's own append-only ordering is.
    id: e.artifact_id + "@" + e.occurred_at + ":" + e.to,
    at: e.occurred_at,
    type: known?.type ?? "backup-discovered",
    severity: known?.severity ?? "info",
    setId: e.backup_set_id,
    setName: e.set_name,
    text: known?.text ?? e.to,
    detail: e.detail || (e.from ? e.from + " to " + e.to : e.to),
    // The transition log carries no correlation id; it is a property of a
    // REQUEST, and these are records of work the service did on its own
    // schedule. Empty rather than a fabricated value, so the "Advanced
    // details" panel shows nothing instead of showing something wrong.
    correlationId: ""
  };
}

/**
 * Maps a durable operation record onto the UI's progress model.
 *
 * The two are genuinely different things, and the gap is not hidden here.
 * A durable operation records that a run cycle was submitted, started and
 * finished; the UI's Operation models a transfer's on-screen progress
 * (stage, percent, bytes per second, ETA). The backend computes none of
 * the second kind, so percent is 0 while an operation runs and 100 once it
 * has finished, and the byte and item counters are left undefined rather
 * than zeroed: undefined renders as "unknown", a zero renders as "nothing
 * has been transferred", and only one of those is true.
 */
function fromWireOperation(op: WireOperation): Operation {
  const finished = op.status === "completed" || op.status === "failed";
  return {
    id: op.operation_id,
    setId: op.backup_set_id ?? "",
    setName: op.backup_set_id ?? "All backup sets",
    kind: "transfer",
    stage: finished ? "complete" : null,
    label: op.action ? op.action.replace(/_/g, " ") : "operation",
    percent: finished ? 100 : 0,
    // A run cycle reads from every source and writes only to this
    // deployment's own storage; it releases a remote source only after a
    // durable local copy is committed and verified. It is not a
    // read-only pass, so this is false rather than a comforting default.
    nonDestructive: false,
    startedAt: op.started_at ?? op.created_at ?? ""
  };
}

function fromWireCatalogReport(body: WireCatalogReportResponse): CatalogScanPreview {
  return {
    discovered: body.scanned,
    // "Valid" is what the journal already holds and the pass left alone;
    // "requires review" is what it had to reconstruct, plus whatever it
    // could not read at all. A manifest that failed outright is exactly
    // what a review is for, so folding it in here is not padding: leaving
    // it out would report a clean preview for a catalog with unreadable
    // records in it.
    valid: body.already_present,
    requiresReview: body.reconstructed + body.failures.length
  };
}

/** apps/common/webhost/router.go's `{source}/{set}` route params
 *  (model.BackupSetID's own composite shape), URL-encoded independently.
 *  The contract spells the same thing as one `{id}` that spans segments;
 *  the router registers two, because chi matches a parameter per segment.
 *  See BackupSet.source/BackupSet.set's own doc (types/backup.ts). */
const backupSetPath = (source: string, set: string) =>
  "/backup-sets/" + encodeURIComponent(source) + "/" + encodeURIComponent(set);

const retentionPath = (source: string, set: string) => backupSetPath(source, set) + "/retention";

export const httpApi: BackupManagerApi = {
  getVersion: () => request<WireVersionResponse>("/system/version").then(fromWireVersion),
  // GET /system/health, NOT /health/ready. The two answer different
  // questions and only one of them is this one: /health/live and
  // /health/ready sit outside /api/v1, carry no authentication, and exist
  // for an orchestrator deciding whether to send traffic here. A ready
  // process with no fresh backups is ready and unhealthy at the same time
  // (failure-safety invariant 14), so a dashboard built on the probe would
  // keep reporting green after backups stopped landing.
  getHealth: () =>
    request<WireHealthResponse>("/system/health").then((r) => fromWireHealth(r, Date.now())),

  getFirstRunStatus: () =>
    request<WireFirstRunStatusResponse>("/system/first-run").then((r) => ({
      configured: r.configured
    })),
  // The body is the SPEC, not the create request: POST /system/first-run
  // declares BackupSetSpec, which carries no run_immediately, because
  // there is no service running yet to run anything. This method still
  // takes the wizard's whole answer set, since the operator fills in one
  // form either way, and drops that one field here, in the single place
  // this boundary is crossed, rather than sending a key the contract does
  // not declare and the server would ignore.
  completeFirstRun: (req) =>
    request<WireCompleteFirstRunResponse>("/system/first-run", {
      method: "POST",
      body: JSON.stringify(wireBackupSetSpec(req))
    }).then((r) => ({
      backupSet: fromWireCreateBackupSetResponse(r.backup_set),
      restartRequired: r.restart_required
    })),

  listSets: () =>
    request<WireListBackupSetsResponse>("/backup-sets").then((r) => r.backup_sets.map(fromWireBackupSet)),
  getSet: (id) => request<WireBackupSet>("/backup-sets/" + id).then(fromWireBackupSet),
  // POST /operations with a run_cycle action, not a per-set run route.
  // There has never been one, and the reason is not an oversight: a run
  // cycle is deployment-wide (it walks every enabled backup set), which is
  // why the durable operation record carries no backup set id either. The
  // config_revision is what makes the submission optimistically
  // concurrent: a stale value is refused server-side rather than running
  // against a configuration the caller has not seen.
  runCycle: (configRevision) =>
    post("/operations", { action: "run_cycle", config_revision: configRevision }),
  // The persisted-set mode of the shared test-connection route. Sending
  // only the id is the point: this client neither knows nor should have to
  // echo back the key reference and trusted host line the set is
  // configured with.
  testConnection: (id) =>
    request<ConnectionTestOutcome>("/backup-sets/test-connection", {
      method: "POST",
      body: JSON.stringify({ backup_set_id: id })
    }),
  setEnabled: (source, set, enabled) => post(backupSetPath(source, set) + "/enabled", { enabled }),

  createBackupSet: (req) =>
    request<WireCreateBackupSetResponse>("/backup-sets", {
      method: "POST",
      body: JSON.stringify(wireCreateBackupSetRequest(req))
    }).then(fromWireCreateBackupSetResponse),
  listValidators: () =>
    request<{ validators?: { id: string; summary: string }[] }>("/validators").then((r) =>
      (r.validators ?? []).map((v) => ({ id: v.id, summary: v.summary }))
    ),
  importSSHKey: (privateKeyPem) =>
    request<SSHKeyImportResult>("/ssh-keys", {
      method: "POST",
      body: JSON.stringify({ private_key_pem: privateKeyPem })
    }),
  probeHostKey: (host, port) =>
    request<{ algorithm: string; fingerprint: string; known_hosts_line: string }>("/ssh/host-key-probe", {
      method: "POST",
      body: JSON.stringify({ host, port })
    }).then((r) => ({ algorithm: r.algorithm, fingerprint: r.fingerprint, knownHostsLine: r.known_hosts_line })),
  testCandidateConnection: (params) =>
    request<ConnectionTestOutcome>("/backup-sets/test-connection", {
      method: "POST",
      body: JSON.stringify(wireConnectionTestParams(params))
    }),

  listArtifacts: (setId) =>
    request<WireListArtifactsResponse>(
      "/backups" + (setId ? "?setId=" + encodeURIComponent(setId) : "")
    ).then((r) => r.artifacts.map(fromWireArtifact)),
  getArtifact: (id) => request<WireArtifact>("/backups/" + id).then(fromWireArtifact),

  listOperations: () =>
    request<WireListOperationsResponse>("/operations").then((r) => r.operations.map(fromWireOperation)),
  listActivity: () =>
    request<WireListActivityResponse>("/activity").then((r) => r.events.map(fromWireActivityEvent)),
  listQuarantine: () =>
    request<WireListArtifactsResponse>("/quarantine").then((r) => r.artifacts.map(fromWireArtifact)),
  revalidate: (id) => post("/quarantine/" + id + "/revalidate"),
  retryIngestion: (id) => post("/quarantine/" + id + "/retry"),
  // Unlike its two siblings this one reads its response. A reinstate that
  // reaches the backend and comes back saying the copy is bad is a 200,
  // not a rejection, so a caller that ignored the body could not tell that
  // from a success.
  reinstate: (id) =>
    request<WireArtifactReinstateResponse>("/quarantine/" + id + "/reinstate", { method: "POST" }).then((r) => ({
      reinstated: r.reinstated,
      checked: r.checked,
      passed: r.passed,
      state: r.state ?? "",
      reason: r.reason ?? ""
    })),

  // Preview is read-only end to end (router.go deliberately does not gate
  // it behind requireCSRF/requireDestructiveGate) — a plain GET, not POST.
  previewRetention: (source, set) =>
    request<WireRetentionPlan>(retentionPath(source, set) + "/preview").then(fromWireRetentionPlan),
  // Applying by plan_id (not by recomputing) is what makes a stale plan a
  // server-side 409 rather than a silent recalculation (§17). The response
  // re-expresses the exact plan that was just applied, the same shape a
  // preview returns, so the caller never reconciles two different shapes.
  applyRetention: (source, set, planId) =>
    request<WireRetentionPlan>(retentionPath(source, set) + "/apply", {
      method: "POST",
      body: JSON.stringify({ plan_id: planId })
    }).then(fromWireRetentionPlan),

  getSettings: () => request<WireSettingsResponse>("/settings").then(fromWireSettingsResponse),
  // PATCH, not POST or PUT: this applies exactly the settings the body
  // names and leaves the rest alone (apps/common/webhost/router.go
  // registers no other verb on this path, so a wrong one 405s rather
  // than looking like it worked).
  updateSettings: (req) =>
    request<WireSettingsResponse>("/settings", {
      method: "PATCH",
      body: JSON.stringify(wireUpdateSettings(req))
    }).then(fromWireSettingsResponse),

  scanCatalog: () =>
    request<WireCatalogReportResponse>("/catalog/scan", { method: "POST" }).then(fromWireCatalogReport),
  rebuildCatalog: () => post("/catalog/rebuild"),

  login: (username, password) => post("/auth/login", { username, password }),
  enrollAdministrator: (username, password) => post("/auth/enroll", { username, password }),
  rotatePassword: (currentPassword, newPassword) =>
    post("/auth/password", { currentPassword, newPassword }),
  logout: () => post("/auth/logout")
};
