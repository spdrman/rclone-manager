import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { httpApi } from "./client";
import { BackupManagerError, toApiErrorCode } from "./contracts";
import type { ApiErrorCode } from "./contracts";

/** Sets document.cookie the way a browser would after the server issued
 *  a Set-Cookie header for bm_csrf — jsdom's document.cookie setter
 *  accepts the same "name=value" assignment form. */
function setCsrfCookie(value: string) {
  document.cookie = "bm_csrf=" + value;
}

function mockFetchOk(body: unknown = undefined, status = 200) {
  return vi.fn().mockResolvedValue({
    ok: true,
    status,
    headers: new Headers(),
    json: async () => body
  });
}

describe("httpApi CSRF/bootstrap-token wiring", () => {
  beforeEach(() => {
    // Clear any cookie a previous test left behind.
    document.cookie = "bm_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT";
    window.history.pushState({}, "", "/");
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("attaches X-CSRF-Token on a state-changing request when the cookie is present", async () => {
    setCsrfCookie("csrf-value-123");
    const fetchMock = mockFetchOk(undefined, 204);
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.login("bm-admin", "hunter22222222");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const headers = init.headers as Record<string, string>;
    expect(headers["X-CSRF-Token"]).toBe("csrf-value-123");

    vi.unstubAllGlobals();
  });

  it("does not attach X-CSRF-Token on a GET request", async () => {
    setCsrfCookie("csrf-value-123");
    const fetchMock = mockFetchOk({});
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.getVersion();

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const headers = init.headers as Record<string, string>;
    expect(headers["X-CSRF-Token"]).toBeUndefined();

    vi.unstubAllGlobals();
  });

  it("omits X-CSRF-Token when no cookie has been issued yet", async () => {
    const fetchMock = mockFetchOk(undefined, 204);
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.login("bm-admin", "hunter22222222");

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const headers = init.headers as Record<string, string>;
    expect(headers["X-CSRF-Token"]).toBeUndefined();

    vi.unstubAllGlobals();
  });

  it("posts currentPassword/newPassword to /auth/password and attaches X-CSRF-Token", async () => {
    setCsrfCookie("csrf-value-123");
    const fetchMock = mockFetchOk(undefined, 204);
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.rotatePassword("old-password-value", "new-password-value");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/auth/password");
    expect(JSON.parse(init.body as string)).toEqual({
      currentPassword: "old-password-value",
      newPassword: "new-password-value"
    });
    const headers = init.headers as Record<string, string>;
    expect(headers["X-CSRF-Token"]).toBe("csrf-value-123");

    vi.unstubAllGlobals();
  });

  it("attaches X-Bootstrap-Token, read from the URL's ?token= param, only for enrollAdministrator", async () => {
    window.history.pushState({}, "", "/enroll?token=bootstrap-abc");
    const fetchMock = mockFetchOk(undefined, 204);
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.enrollAdministrator("bm-admin", "hunter22222222");

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const headers = init.headers as Record<string, string>;
    expect(headers["X-Bootstrap-Token"]).toBe("bootstrap-abc");

    vi.unstubAllGlobals();
  });

  it("does not attach X-Bootstrap-Token to routes other than enroll, even with ?token= present", async () => {
    window.history.pushState({}, "", "/enroll?token=bootstrap-abc");
    const fetchMock = mockFetchOk(undefined, 204);
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.login("bm-admin", "hunter22222222");

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const headers = init.headers as Record<string, string>;
    expect(headers["X-Bootstrap-Token"]).toBeUndefined();

    vi.unstubAllGlobals();
  });
});

describe("retention preview/apply: wire contract (apps/common/webhost/handlers_retention.go)", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  const WIRE_VERDICT = { artifact: "a.dump", action: "KEEP", reason: "GFS daily tier", tiers: ["DAILY"] };

  it("previewRetention issues a plain GET against the two-segment {source}/{set} route and maps snake_case to camelCase", async () => {
    const fetchMock = mockFetchOk({
      plan_id: "retplan_abc",
      backup_set_id: "production/postgres-primary",
      inventory_revision: "inv_1",
      config_revision: "cfg_1",
      expires_at: "2026-08-29T06:09:48Z",
      keep_count: 1,
      delete_count: 1,
      reclaim_bytes: 4096,
      verdicts: [WIRE_VERDICT, { artifact: "b.dump", action: "DELETE", reason: "not selected" }]
    });
    vi.stubGlobal("fetch", fetchMock);

    const plan = await httpApi.previewRetention("production", "postgres-primary");

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/backup-sets/production/postgres-primary/retention/preview");
    expect(init.method ?? "GET").toBe("GET");
    expect(plan).toEqual({
      planId: "retplan_abc",
      backupSetId: "production/postgres-primary",
      inventoryRevision: "inv_1",
      configRevision: "cfg_1",
      expiresAt: "2026-08-29T06:09:48Z",
      keepCount: 1,
      deleteCount: 1,
      reclaimBytes: 4096,
      operationId: undefined,
      verdicts: [
        { artifact: "a.dump", action: "KEEP", reason: "GFS daily tier", tiers: ["DAILY"] },
        // tiers defaults to [] when the wire omits it (DELETE/REFUSE never carry one).
        { artifact: "b.dump", action: "DELETE", reason: "not selected", tiers: [] }
      ]
    });

    vi.unstubAllGlobals();
  });

  it("applyRetention POSTs {plan_id} with a CSRF token attached, against the same two-segment route", async () => {
    setCsrfCookie("csrf-value-123");
    const fetchMock = mockFetchOk({
      plan_id: "retplan_abc",
      backup_set_id: "production/postgres-primary",
      inventory_revision: "inv_1",
      config_revision: "cfg_1",
      expires_at: "2026-08-29T06:09:48Z",
      keep_count: 1,
      delete_count: 1,
      reclaim_bytes: 4096,
      operation_id: "op_1",
      verdicts: [WIRE_VERDICT]
    });
    vi.stubGlobal("fetch", fetchMock);

    const plan = await httpApi.applyRetention("production", "postgres-primary", "retplan_abc");

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/backup-sets/production/postgres-primary/retention/apply");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({ plan_id: "retplan_abc" });
    const headers = init.headers as Record<string, string>;
    expect(headers["X-CSRF-Token"]).toBe("csrf-value-123");
    expect(plan.operationId).toBe("op_1");

    vi.unstubAllGlobals();
  });

  it("URL-encodes source/set independently, so a literal '/' or space in either half cannot smuggle an extra path segment", async () => {
    const fetchMock = mockFetchOk({
      plan_id: "x", backup_set_id: "a b/c/d", inventory_revision: "i", config_revision: "c",
      expires_at: "t", keep_count: 0, delete_count: 0, reclaim_bytes: 0, verdicts: []
    });
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.previewRetention("a b", "c/d");

    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/backup-sets/a%20b/c%2Fd/retention/preview");

    vi.unstubAllGlobals();
  });
});

describe("ApiErrorCode covers every code apps/common/auth/local actually emits", () => {
  // Mirrors the literal strings passed to writeAuthError across
  // apps/common/auth/local/handler.go and csrf.go - kept here by hand,
  // since nothing generates this list automatically from the Go source.
  // The type annotation below is what actually enforces "subset of the
  // declared union": a code emitted there but missing from ApiErrorCode
  // fails typecheck here, not just silently passes at runtime (issue
  // #119's review, finding 9 - client.ts's own `as ApiError` assertion
  // has no runtime check, so this is the only thing that would catch a
  // future drift between the two).
  const localAuthErrorCodes: ApiErrorCode[] = [
    "UNAUTHENTICATED",
    "RATE_LIMITED",
    "INVALID_REQUEST",
    "ENROLLMENT_CLOSED",
    "BOOTSTRAP_TOKEN_INVALID",
    "INTERNAL_ERROR",
    "CSRF_TOKEN_MISSING",
    "CSRF_TOKEN_MISMATCH"
  ];

  it("lists every code this frontend's own backend can actually return (this test would pass vacuously on an empty array)", () => {
    expect(localAuthErrorCodes.length).toBeGreaterThan(0);
    expect(new Set(localAuthErrorCodes).size).toBe(localAuthErrorCodes.length);
  });
});

/**
 * Issue #146 (B2.7): apps/common/auth/local's routes answer a FLAT error
 * envelope ({ code, message, correlationId } all top-level), while
 * apps/common/webhost's routes (every one this issue adds) nest
 * code/message under "error" and carry the correlation id only in the
 * X-Correlation-Id header. Before this issue, request()'s error handling
 * only actually worked for the flat shape (the only one this file had
 * ever been exercised against) — casting a nested body straight to
 * ApiError would leave code/message/correlationId all undefined. These
 * tests pin both shapes so a future change to either envelope is caught
 * here, not by a wizard silently showing "undefined" for a save failure.
 */
describe("httpApi error envelope handling", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("parses apps/common/webhost's nested { error: { code, message } } shape, reading correlationId from the X-Correlation-Id header", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 400,
      headers: new Headers({ "x-correlation-id": "cid_test123" }),
      json: async () => ({ error: { code: "INVALID_REQUEST", message: "name is required" } })
    });
    vi.stubGlobal("fetch", fetchMock);

    let caught: unknown;
    try {
      await httpApi.createBackupSet({} as never);
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(BackupManagerError);
    const err = caught as BackupManagerError;
    expect(err.api.code).toBe("INVALID_REQUEST");
    expect(err.api.message).toBe("name is required");
    expect(err.api.correlationId).toBe("cid_test123");
  });

  it("parses apps/common/auth/local's flat { code, message, correlationId } shape", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 401,
      headers: new Headers(),
      json: async () => ({ code: "UNAUTHENTICATED", message: "authentication required", correlationId: "cid_flat456" })
    });
    vi.stubGlobal("fetch", fetchMock);

    let caught: unknown;
    try {
      await httpApi.login("bm-admin", "hunter22222222");
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(BackupManagerError);
    const err = caught as BackupManagerError;
    expect(err.api.code).toBe("UNAUTHENTICATED");
    expect(err.api.correlationId).toBe("cid_flat456");
  });

  it("falls back to a synthesised error when the body isn't JSON at all, using the header if present", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 502,
      headers: new Headers({ "x-correlation-id": "cid_nojson" }),
      json: async () => {
        throw new Error("not json");
      }
    });
    vi.stubGlobal("fetch", fetchMock);

    let caught: unknown;
    try {
      await httpApi.getVersion();
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(BackupManagerError);
    expect((caught as BackupManagerError).api.correlationId).toBe("cid_nojson");
  });
});

describe("httpApi issue #146 (B2.7) endpoints", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("createBackupSet posts snake_case fields to POST /backup-sets and maps the response back to camelCase", async () => {
    const fetchMock = mockFetchOk(
      {
        id: "api/postgres-primary",
        source_name: "api",
        name: "postgres-primary",
        host: "prod-db-01.internal",
        port: 22,
        user: "backup-agent",
        remote_path: "/backups/postgresql",
        local_path: "/data/backups/postgres",
        include: ["*.dump.zst"],
        completion_strategy: "marker",
        disabled: false,
        operation: { operation_id: "op_1", status: "queued" }
      },
      201
    );
    vi.stubGlobal("fetch", fetchMock);

    const result = await httpApi.createBackupSet({
      name: "postgres-primary",
      host: "prod-db-01.internal",
      port: 22,
      user: "backup-agent",
      sshKeyId: "key_1",
      knownHostsLine: "prod-db-01.internal ssh-ed25519 AAAAtest",
      remotePath: "/backups/postgresql",
      localPath: "/data/backups/postgres",
      include: ["*.dump.zst"],
      completionStrategy: "marker",
      runImmediately: true
    });

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/backup-sets");
    const sentBody = JSON.parse(init.body as string);
    expect(sentBody.ssh_key_id).toBe("key_1");
    expect(sentBody.known_hosts_line).toBe("prod-db-01.internal ssh-ed25519 AAAAtest");
    expect(sentBody.remote_path).toBe("/backups/postgresql");
    expect(sentBody.completion_strategy).toBe("marker");
    expect(sentBody.run_immediately).toBe(true);

    expect(result.id).toBe("api/postgres-primary");
    expect(result.sourceName).toBe("api");
    expect(result.remotePath).toBe("/backups/postgresql");
    expect(result.operation).toEqual({ operationId: "op_1", status: "queued" });
  });

  it("createBackupSet's response has no operation field when the backend omits one", async () => {
    const fetchMock = mockFetchOk(
      {
        id: "api/x",
        source_name: "api",
        name: "x",
        host: "h",
        port: 22,
        user: "u",
        remote_path: "/r",
        local_path: "/l",
        include: [],
        completion_strategy: "marker",
        disabled: true
      },
      201
    );
    vi.stubGlobal("fetch", fetchMock);

    const result = await httpApi.createBackupSet({
      name: "x",
      host: "h",
      port: 22,
      user: "u",
      sshKeyId: "k",
      knownHostsLine: "line",
      remotePath: "/r",
      localPath: "/l",
      include: [],
      completionStrategy: "marker",
      disabled: true
    });
    expect(result.operation).toBeUndefined();
  });

  it("listSets maps the wire backup_sets array onto BackupSet's full shape, with honest placeholders for fields the backend does not yet send (M4, #146 review)", async () => {
    const fetchMock = mockFetchOk(
      {
        backup_sets: [
          {
            id: "api/postgres-primary",
            source_name: "api",
            name: "postgres-primary",
            host: "prod-db-01.internal",
            port: 22,
            user: "backup-agent",
            remote_path: "/backups/postgresql",
            local_path: "/data/backups/postgres",
            include: ["*.dump.zst"],
            completion_strategy: "marker",
            disabled: true
          }
        ]
      },
      200
    );
    vi.stubGlobal("fetch", fetchMock);

    const result = await httpApi.listSets();

    expect(result).toHaveLength(1);
    const s = result[0];
    // Fields the backend actually sends: name AND polarity both mapped
    // correctly (user -> username, remote_path -> remoteFolder,
    // disabled: true -> enabled: false, not the same boolean re-read
    // under the wrong name).
    expect(s.id).toBe("api/postgres-primary");
    expect(s.username).toBe("backup-agent");
    expect(s.remoteFolder).toBe("/backups/postgresql");
    expect(s.completionMethod).toBe("completion-marker");
    expect(s.enabled).toBe(false);

    // Fields the backend does NOT yet send: present, correctly typed and
    // never undefined — this is exactly what crashed
    // BackupSetDetailPage's s.retention.daily / s.validations.includes(...)
    // (a real TypeError) the first time these routes returned real data
    // instead of 404ing.
    expect(s.retention).toEqual({
      daily: 0,
      weekly: 0,
      monthly: 0,
      timezone: "UTC",
      weekStartsOn: "monday",
      protectLastKnownGood: false
    });
    expect(s.validations).toEqual([]);
    expect(s.state).toBe("stale");
    expect(s.newestKnownGoodAt).toBeNull();
    expect(s.lastRunAt).toBeNull();
  });

  it("getSet maps the same wire shape for a single backup set", async () => {
    const fetchMock = mockFetchOk(
      {
        id: "api/x",
        source_name: "api",
        name: "x",
        host: "h",
        port: 22,
        user: "u",
        remote_path: "/r",
        local_path: "/l",
        include: [],
        completion_strategy: "rename",
        disabled: false
      },
      200
    );
    vi.stubGlobal("fetch", fetchMock);

    const s = await httpApi.getSet("api/x");

    expect(s.id).toBe("api/x");
    expect(s.completionMethod).toBe("atomic-rename");
    expect(s.enabled).toBe(true);
    expect(s.retention.daily).toBe(0);
    expect(s.validations).toEqual([]);
  });

  it("importSSHKey posts private_key_pem and returns id/algorithm/fingerprint", async () => {
    const fetchMock = mockFetchOk({ id: "key_1", algorithm: "ssh-ed25519", fingerprint: "SHA256:abc" }, 201);
    vi.stubGlobal("fetch", fetchMock);

    const result = await httpApi.importSSHKey("-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----");

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/ssh-keys");
    expect(JSON.parse(init.body as string).private_key_pem).toContain("BEGIN OPENSSH PRIVATE KEY");
    expect(result).toEqual({ id: "key_1", algorithm: "ssh-ed25519", fingerprint: "SHA256:abc" });
  });

  it("probeHostKey posts host/port and maps known_hosts_line to knownHostsLine", async () => {
    const fetchMock = mockFetchOk(
      { algorithm: "ssh-ed25519", fingerprint: "SHA256:def", known_hosts_line: "h ssh-ed25519 AAAA" },
      200
    );
    vi.stubGlobal("fetch", fetchMock);

    const result = await httpApi.probeHostKey("prod-db-01.internal", 22);

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/ssh/host-key-probe");
    expect(JSON.parse(init.body as string)).toEqual({ host: "prod-db-01.internal", port: 22 });
    expect(result).toEqual({ algorithm: "ssh-ed25519", fingerprint: "SHA256:def", knownHostsLine: "h ssh-ed25519 AAAA" });
  });

  it("testCandidateConnection posts to the static /backup-sets/test-connection path, not a per-id one", async () => {
    const fetchMock = mockFetchOk({ ok: true }, 200);
    vi.stubGlobal("fetch", fetchMock);

    const result = await httpApi.testCandidateConnection({
      host: "prod-db-01.internal",
      port: 22,
      user: "backup-agent",
      sshKeyId: "key_1",
      knownHostsLine: "h ssh-ed25519 AAAA",
      remotePath: "/backups"
    });

    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/backup-sets/test-connection");
    expect(result).toEqual({ ok: true });
  });
});

describe("ApiErrorCode covers every code apps/common/webhost actually emits", () => {
  // The literal strings passed to writeError across apps/common/webhost's
  // handlers (handlers_retention.go, handlers_operations.go,
  // handlers_backupsets.go, handlers_ssh.go, errors.go, router.go's own
  // middleware) - kept here by hand, since nothing generates this list
  // from the Go source. Not one of them was in the union before issue
  // #96's review: the retention dialog's stale branch, the only place in
  // this frontend that reads a code at all, compared against a kebab-case
  // value no Go source has ever written.
  const webhostErrorCodes: ApiErrorCode[] = [
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
    "INVALID_REQUEST",
    "UNAUTHENTICATED",
    "CSRF_TOKEN_MISSING",
    "CSRF_TOKEN_MISMATCH",
    "INTERNAL"
  ];

  it("lists every code webhost can return (this test would pass vacuously on an empty array)", () => {
    expect(webhostErrorCodes.length).toBeGreaterThan(0);
    expect(new Set(webhostErrorCodes).size).toBe(webhostErrorCodes.length);
  });

  it("carries a webhost error code through the nested envelope verbatim, so a caller can branch on it", async () => {
    // Byte-for-byte the body apps/common/webhost/errors.go writes and
    // handlers_retention_test.go asserts for a stale plan: a nested
    // error object, the code in UPPER_SNAKE_CASE, and the correlation id
    // in a header rather than the body.
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 409,
        headers: new Headers({ "x-correlation-id": "cid_stale_1" }),
        json: async () => ({
          error: {
            code: "RETENTION_PLAN_STALE",
            message: "service: retention plan is stale: backup set production/postgres-primary changed"
          }
        })
      })
    );

    await expect(
      httpApi.applyRetention("production", "postgres-primary", "retplan_abc")
    ).rejects.toMatchObject({
      api: { code: "RETENTION_PLAN_STALE", correlationId: "cid_stale_1" }
    });

    vi.unstubAllGlobals();
  });

  it("degrades a code it does not know to \"unknown\" instead of asserting it into the union", () => {
    // The `as ApiErrorCode` this replaces had no runtime check at all, so
    // an unrecognised code became an ApiErrorCode that silently matched
    // no branch anywhere.
    expect(toApiErrorCode("SOMETHING_NEW")).toBe("unknown");
    expect(toApiErrorCode(undefined)).toBe("unknown");
    expect(toApiErrorCode(42)).toBe("unknown");
    // Positive control: a real code is passed through, so the assertions
    // above are not just "this function always returns unknown".
    expect(toApiErrorCode("RETENTION_PLAN_STALE")).toBe("RETENTION_PLAN_STALE");
  });
});
