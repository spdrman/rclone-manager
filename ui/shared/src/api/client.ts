import { BackupManagerError } from "./contracts";
import type { ApiError, BackupManagerApi } from "./contracts";

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
    // The service always returns a typed error envelope. If it doesn't, we
    // synthesise one — we never surface a raw body or stack trace.
    let api: ApiError;
    try {
      api = (await res.json()) as ApiError;
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

export const httpApi: BackupManagerApi = {
  getVersion: () => request("/version"),
  getHealth: () => request("/health"),

  listSets: () => request("/backup-sets"),
  getSet: (id) => request("/backup-sets/" + id),
  runSet: (id) => post("/backup-sets/" + id + "/run"),
  testConnection: (id) => request("/backup-sets/" + id + "/test-connection", { method: "POST" }),
  setEnabled: (id, enabled) => post("/backup-sets/" + id + "/enabled", { enabled }),

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
  logout: () => post("/auth/logout")
};
