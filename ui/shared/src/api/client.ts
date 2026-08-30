import { BackupManagerError, toApiErrorCode } from "./contracts";
import type {
  ApiError,
  BackupManagerApi,
  ConnectionTestOutcome,
  ConnectionTestParams,
  CreateBackupSetRequest,
  CreatedBackupSet,
  SSHKeyImportResult
} from "./contracts";
import type {
  BackupSet,
  CompletionMethod,
  RetentionPlan,
  RetentionVerdictAction
} from "@shared/types/backup";

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
function wireCreateBackupSetRequest(req: CreateBackupSetRequest) {
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
    stable_for_seconds: req.stableForSeconds,
    stale_after_seconds: req.staleAfterSeconds,
    disabled: req.disabled,
    run_immediately: req.runImmediately
  };
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

/** The wire shape POST /api/v1/backup-sets actually returns
 *  (handlers_backupsets.go's backupSetResponse/createBackupSetResponse). */
interface WireCreatedBackupSet {
  id: string;
  source_name: string;
  name: string;
  host: string;
  port: number;
  user: string;
  remote_path: string;
  local_path: string;
  include: string[];
  completion_strategy: string;
  disabled: boolean;
  operation?: { operation_id: string; status: string };
}

/** Translates WireCreatedBackupSet into CreatedBackupSet's camelCase
 *  shape this file's callers use. */
function fromWireCreatedBackupSet(body: WireCreatedBackupSet): CreatedBackupSet {
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
    disabled: body.disabled,
    operation: body.operation
      ? { operationId: body.operation.operation_id, status: body.operation.status }
      : undefined
  };
}

/** The wire shape GET /api/v1/backup-sets and GET /api/v1/backup-sets/{id}
 *  actually return (handlers_backupsets.go's backupSetResponse) — a
 *  narrow, persistence-facing shape carrying none of the health/
 *  retention/validation fields BackupSet (types/backup.ts) models,
 *  because nothing in this codebase computes that data yet. Before
 *  issue #146, neither route existed at all, so this file's own request()
 *  return type was cast straight to BackupSet with no mapping and no
 *  runtime check — harmless only because the request always 404'd first.
 *  #146 is what first makes these routes answer for real, which is what
 *  turned that cast into a confirmed crash the moment BackupSetDetailPage
 *  dereferences a field (s.retention.daily, s.validations.includes(...))
 *  the wire response never sent (mandatory review finding M4, PR #155). */
interface WireBackupSet {
  id: string;
  source_name: string;
  name: string;
  host: string;
  port: number;
  user: string;
  remote_path: string;
  local_path: string;
  include: string[];
  completion_strategy: string;
  disabled: boolean;
}

interface WireListBackupSetsResponse {
  backup_sets: WireBackupSet[];
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

/** The wire shape GET .../retention/preview and POST .../retention/apply
 *  actually return (apps/common/webhost/handlers_retention.go's
 *  retentionPlanResponse). */
interface WireRetentionVerdict {
  artifact: string;
  action: string;
  reason: string;
  tiers?: string[];
}

interface WireRetentionPlan {
  plan_id: string;
  backup_set_id: string;
  inventory_revision: string;
  config_revision: string;
  expires_at: string;
  keep_count: number;
  delete_count: number;
  reclaim_bytes: number;
  operation_id?: string;
  verdicts: WireRetentionVerdict[];
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
const retentionPath = (source: string, set: string) =>
  "/backup-sets/" + encodeURIComponent(source) + "/" + encodeURIComponent(set) + "/retention";

export const httpApi: BackupManagerApi = {
  getVersion: () => request("/version"),
  getHealth: () => request("/health"),

  listSets: () =>
    request<WireListBackupSetsResponse>("/backup-sets").then((r) => r.backup_sets.map(fromWireBackupSet)),
  getSet: (id) => request<WireBackupSet>("/backup-sets/" + id).then(fromWireBackupSet),
  runSet: (id) => post("/backup-sets/" + id + "/run"),
  testConnection: (id) => request("/backup-sets/" + id + "/test-connection", { method: "POST" }),
  setEnabled: (id, enabled) => post("/backup-sets/" + id + "/enabled", { enabled }),

  createBackupSet: (req) =>
    request<WireCreatedBackupSet>("/backup-sets", {
      method: "POST",
      body: JSON.stringify(wireCreateBackupSetRequest(req))
    }).then(fromWireCreatedBackupSet),
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
    request("/backups" + (setId ? "?setId=" + encodeURIComponent(setId) : "")),
  getArtifact: (id) => request("/backups/" + id),

  listOperations: () => request("/operations"),
  listActivity: () => request("/activity"),
  listQuarantine: () => request("/quarantine"),
  revalidate: (id) => post("/quarantine/" + id + "/revalidate"),
  retryIngestion: (id) => post("/quarantine/" + id + "/retry"),

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

  scanCatalog: () => request("/catalog/scan", { method: "POST" }),
  rebuildCatalog: () => post("/catalog/rebuild"),

  login: (username, password) => post("/auth/login", { username, password }),
  enrollAdministrator: (username, password) => post("/auth/enroll", { username, password }),
  rotatePassword: (currentPassword, newPassword) =>
    post("/auth/password", { currentPassword, newPassword }),
  logout: () => post("/auth/logout")
};
