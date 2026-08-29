import { BackupManagerError } from "./contracts";
import type { ApiError, BackupManagerApi } from "./contracts";

const BASE = "/api/v1";

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(BASE + path, {
    credentials: "same-origin",
    headers: { "content-type": "application/json", ...(init?.headers ?? {}) },
    ...init
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
