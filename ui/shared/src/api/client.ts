import { BackupManagerError } from "./contracts";
import type { ApiError, BackupManagerApi } from "./contracts";
import type { RetentionPlan, RetentionVerdictAction } from "@shared/types/backup";

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

/**
 * The wire shape GET .../retention/preview and POST .../retention/apply
 * actually return (apps/common/webhost/handlers_retention.go's
 * retentionPlanResponse) — snake_case, unlike the rest of this file's
 * BackupSet/Operation/etc. types, which this service already emits in the
 * camelCase RetentionPlan carries. toRetentionPlan below is the one place
 * that has to know the wire uses snake_case here; nothing past it does.
 */
interface RetentionVerdictWire {
  artifact: string;
  action: string;
  reason: string;
  tiers?: string[];
}

interface RetentionPlanWire {
  plan_id: string;
  backup_set_id: string;
  inventory_revision: string;
  config_revision: string;
  expires_at: string;
  keep_count: number;
  delete_count: number;
  reclaim_bytes: number;
  operation_id?: string;
  verdicts: RetentionVerdictWire[];
}

function toRetentionPlan(wire: RetentionPlanWire): RetentionPlan {
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

  // Preview is read-only end to end (router.go deliberately does not gate
  // it behind requireCSRF/requireDestructiveGate) — a plain GET, not POST.
  previewRetention: (source, set) =>
    request<RetentionPlanWire>(retentionPath(source, set) + "/preview").then(toRetentionPlan),
  // Applying by plan_id (not by recomputing) is what makes a stale plan a
  // server-side 409 rather than a silent recalculation (§17). The response
  // re-expresses the exact plan that was just applied, the same shape a
  // preview returns, so the caller never reconciles two different shapes.
  applyRetention: (source, set, planId) =>
    request<RetentionPlanWire>(retentionPath(source, set) + "/apply", {
      method: "POST",
      body: JSON.stringify({ plan_id: planId })
    }).then(toRetentionPlan),

  scanCatalog: () => request("/catalog/scan", { method: "POST" }),
  rebuildCatalog: () => post("/catalog/rebuild"),

  login: (username, password) => post("/auth/login", { username, password }),
  enrollAdministrator: (username, password) => post("/auth/enroll", { username, password }),
  rotatePassword: (currentPassword, newPassword) =>
    post("/auth/password", { currentPassword, newPassword }),
  logout: () => post("/auth/logout")
};
