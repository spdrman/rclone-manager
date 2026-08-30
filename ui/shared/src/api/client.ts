import { BackupManagerError } from "./contracts";
import type {
  ApiError,
  ApiErrorCode,
  BackupManagerApi,
  ConnectionTestOutcome,
  ConnectionTestParams,
  CreateBackupSetRequest,
  CreatedBackupSet,
  SSHKeyImportResult
} from "./contracts";

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
          code: err.code as ApiErrorCode,
          message: err.message as string,
          correlationId: headerCorrelationId ?? "unavailable"
        };
      } else {
        api = {
          code: body.code as ApiErrorCode,
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
 * routes — everything issue #146 (B2.7) adds — use snake_case
 * (handlers_backupsets.go/handlers_ssh.go's own `json:"remote_path"`-
 * style tags), a genuine, pre-existing split between the two packages
 * this file did not introduce. wireCreateBackupSetRequest/
 * wireConnectionTestParams below are the one place that translation
 * happens for this file's own camelCase request types, so the two
 * shapes never have to be kept in sync by hand at every call site.
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

export const httpApi: BackupManagerApi = {
  getVersion: () => request("/version"),
  getHealth: () => request("/health"),

  listSets: () => request("/backup-sets"),
  getSet: (id) => request("/backup-sets/" + id),
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

  previewRetention: (setId) => request("/backup-sets/" + setId + "/retention/preview", { method: "POST" }),
  // Applying by planId is what makes a stale plan a server-side 409 rather
  // than a silent recalculation (§17).
  applyRetention: (planId) => post("/retention/plans/" + planId + "/apply"),

  scanCatalog: () => request("/catalog/scan", { method: "POST" }),
  rebuildCatalog: () => post("/catalog/rebuild"),

  login: (username, password) => post("/auth/login", { username, password }),
  enrollAdministrator: (username, password) => post("/auth/enroll", { username, password }),
  rotatePassword: (currentPassword, newPassword) =>
    post("/auth/password", { currentPassword, newPassword }),
  logout: () => post("/auth/logout")
};
