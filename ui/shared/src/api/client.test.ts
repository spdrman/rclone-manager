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
    // The other half of the pair below: no run_error on the wire is no
    // runError in the mapped result, so the assertion there is about
    // this field arriving rather than about it always being set.
    expect(result.runError).toBeUndefined();
  });

  it("createBackupSet reports a run that did not start instead of dropping it (M4, #194 review)", async () => {
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
        disabled: false,
        run_error: "the destructive gate is closed, so the run was not submitted"
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
      runImmediately: true
    });

    // 201 with no operation and a run_error is the contract's own way of
    // saying "the set exists, the run does not". A mapper that keeps only
    // the first half of that sentence tells the operator a backup is
    // running when nothing started.
    expect(result.operation).toBeUndefined();
    expect(result.runError).toBe("the destructive gate is closed, so the run was not submitted");
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

describe("httpApi issue #162: registered-validator catalog", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("listValidators GETs /validators and maps the wire shape onto the catalog entry type", async () => {
    const fetchMock = mockFetchOk({
      validators: [{ id: "trailer-marker", summary: "Confirms the artifact ends with a completion trailer." }]
    });
    vi.stubGlobal("fetch", fetchMock);

    const catalog = await httpApi.listValidators();

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit | undefined];
    expect(url).toBe("/api/v1/validators");
    expect((init?.method ?? "GET").toUpperCase()).toBe("GET");
    expect(catalog).toEqual([
      { id: "trailer-marker", summary: "Confirms the artifact ends with a completion trailer." }
    ]);
  });

  it("listValidators tolerates a backend that sends no validators at all", async () => {
    const fetchMock = mockFetchOk({ validators: [] });
    vi.stubGlobal("fetch", fetchMock);
    expect(await httpApi.listValidators()).toEqual([]);
  });

  it("createBackupSet sends validator_id, and only when one was chosen", async () => {
    const okBody = {
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
      validator_id: "trailer-marker",
      disabled: false
    };
    const fetchMock = mockFetchOk(okBody, 201);
    vi.stubGlobal("fetch", fetchMock);

    const base = {
      name: "x",
      host: "h",
      port: 22,
      user: "u",
      sshKeyId: "k",
      knownHostsLine: "line",
      remotePath: "/r",
      localPath: "/l",
      include: [],
      completionStrategy: "marker" as const
    };

    const withValidator = await httpApi.createBackupSet({ ...base, validatorId: "trailer-marker" });
    let sent = JSON.parse((fetchMock.mock.calls[0][1] as RequestInit).body as string);
    expect(sent.validator_id).toBe("trailer-marker");
    // The response carries it back, so a UI can render what it saved.
    expect(withValidator.validatorId).toBe("trailer-marker");

    await httpApi.createBackupSet(base);
    sent = JSON.parse((fetchMock.mock.calls[1][1] as RequestInit).body as string);
    expect(sent.validator_id).toBeUndefined();
  });

  it("never sends anything that could name an executable (§26 Step 5)", async () => {
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
        disabled: false
      },
      201
    );
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.createBackupSet({
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
      validatorId: "trailer-marker"
    });

    const sent = JSON.parse((fetchMock.mock.calls[0][1] as RequestInit).body as string);
    const banned = ["command", "executable", "argv", "script", "shell", "binary", "exec"];
    const offending = Object.keys(sent).filter((k) => banned.some((w) => k.toLowerCase().includes(w)));
    expect(offending).toEqual([]);

    // Positive control: the same filter over a body that DOES carry the
    // banned shape has to catch it, or the assertion above proves nothing.
    const leaky = { name: "x", validator_command: "/bin/sh" };
    expect(Object.keys(leaky).filter((k) => banned.some((w) => k.toLowerCase().includes(w)))).toEqual([
      "validator_command"
    ]);
  });
});

// ---------------------------------------------------------------------------
// Issue #211: the paths that were wrong, and the mappers that read the
// surfaces they now reach.
//
// The URL assertions are the point of the first block. Every one of these
// calls used to request a path no runtime has ever served, so a test that
// only checked the mapped result would still pass against a client asking
// the wrong server for it.
// ---------------------------------------------------------------------------

describe("httpApi requests the paths the contract declares", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  function urlOf(fetchMock: ReturnType<typeof mockFetchOk>): string {
    return (fetchMock.mock.calls[0] as [string, RequestInit])[0];
  }

  function bodyOf(fetchMock: ReturnType<typeof mockFetchOk>): Record<string, unknown> {
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    return JSON.parse(init.body as string) as Record<string, unknown>;
  }

  it("reads the version from /system/version, not /version", async () => {
    const fetchMock = mockFetchOk({
      api_version: "v1", core_version: "1.4.0", commit: "abc1234",
      go_version: "go1.27.0", engine_version: "1.68.2",
      config_revision: "cfg_7", ready: true
    });
    vi.stubGlobal("fetch", fetchMock);

    const got = await httpApi.getVersion();

    expect(urlOf(fetchMock)).toBe("/api/v1/system/version");
    expect(got).toEqual({
      api: "v1", service: "1.4.0", buildCommit: "abc1234", goVersion: "go1.27.0",
      engine: "1.68.2", configRevision: "cfg_7", ready: true, compatible: true
    });
  });

  it("reports a service speaking a different contract version as incompatible", async () => {
    // The §38 read-only banner's whole trigger. Before issue #211 this
    // flag was read off the wire as its own boolean, which no endpoint has
    // ever sent, so it was undefined against a real backend and the banner
    // could not fire at all.
    const fetchMock = mockFetchOk({
      api_version: "v2", core_version: "9.0.0", commit: "abc1234",
      go_version: "go1.27.0", engine_version: "1.68.2",
      config_revision: "cfg_7", ready: true
    });
    vi.stubGlobal("fetch", fetchMock);

    expect((await httpApi.getVersion()).compatible).toBe(false);
  });

  it("reads health from /api/v1/system/health, not from the unauthenticated probe", async () => {
    const fetchMock = mockFetchOk({ generated_at: "2026-08-30T10:00:00Z", backup_sets: [] });
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.getHealth();

    // /health/live and /health/ready sit OUTSIDE /api/v1 and answer a
    // different question (is this process ready for traffic), so a
    // dashboard built on them would keep reporting green after backups
    // stopped landing.
    expect(urlOf(fetchMock)).toBe("/api/v1/system/health");
  });

  it("submits a run cycle to /operations with the revision the caller is showing", async () => {
    const fetchMock = mockFetchOk(undefined, 204);
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.runCycle("cfg_7");

    expect(urlOf(fetchMock)).toBe("/api/v1/operations");
    expect(bodyOf(fetchMock)).toEqual({ action: "run_cycle", config_revision: "cfg_7" });
  });

  it("tests a persisted set by id alone, on the shared test-connection route", async () => {
    const fetchMock = mockFetchOk({ ok: true });
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.testConnection("nas-a/photos");

    expect(urlOf(fetchMock)).toBe("/api/v1/backup-sets/test-connection");
    // Only the id. This client neither knows nor should echo back the key
    // reference and trusted host line the set is configured with.
    expect(bodyOf(fetchMock)).toEqual({ backup_set_id: "nas-a/photos" });
  });

  it("enables a set on its two-segment path, encoding each half independently", async () => {
    const fetchMock = mockFetchOk(undefined, 204);
    vi.stubGlobal("fetch", fetchMock);

    await httpApi.setEnabled("nas a", "photos/2026", false);

    expect(urlOf(fetchMock)).toBe("/api/v1/backup-sets/nas%20a/photos%2F2026/enabled");
    expect(bodyOf(fetchMock)).toEqual({ enabled: false });
  });

  it("passes a backup set filter as a query parameter, and omits it when absent", async () => {
    const withFilter = mockFetchOk({ artifacts: [] });
    vi.stubGlobal("fetch", withFilter);
    await httpApi.listArtifacts("nas-a/photos");
    expect(urlOf(withFilter)).toBe("/api/v1/backups?setId=nas-a%2Fphotos");

    const withoutFilter = mockFetchOk({ artifacts: [] });
    vi.stubGlobal("fetch", withoutFilter);
    await httpApi.listArtifacts();
    expect(urlOf(withoutFilter)).toBe("/api/v1/backups");
  });
});

describe("httpApi maps the wire shapes onto the domain types", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  const WIRE_ARTIFACT = {
    id: "nas-a/photos/2026-08-30.tar",
    backup_set_id: "nas-a/photos",
    source_name: "nas-a",
    set_name: "photos",
    name: "2026-08-30.tar",
    remote_path: "/out/2026-08-30.tar",
    local_path: "/data/2026-08-30.tar",
    state: "COMPLETE",
    discovered_at: "2026-08-30T09:00:00Z",
    updated_at: "2026-08-30T09:05:00Z",
    size_bytes: 4096,
    checksum: "abc",
    checksum_algorithm: "sha256",
    validation: "passed" as const,
    quarantined: false,
    quarantine_irrecoverable: false
  };

  it("refuses to badge an unrecognised retention tier as protected", async () => {
    // FR-18's tier chain is operator-defined, so "semi_annual" is an
    // ordinary value; RetentionClass is a closed four-value vocabulary.
    // Forcing an unknown tier into it would have to pick one, and
    // "protected" means FR-19 will never delete this backup, which is the
    // one direction that is actively dangerous to claim by accident.
    vi.stubGlobal("fetch", mockFetchOk({
      artifacts: [
        { ...WIRE_ARTIFACT, id: "a/b/known", retention_tier: "weekly" },
        { ...WIRE_ARTIFACT, id: "a/b/protected", retention_tier: "last_known_good" },
        { ...WIRE_ARTIFACT, id: "a/b/unknown", retention_tier: "semi_annual" }
      ]
    }));

    const got = await httpApi.listArtifacts();

    expect(got[0].retentionClasses).toEqual(["weekly"]);
    expect(got[1].retentionClasses).toEqual(["protected"]);
    expect(got[2].retentionClasses).toEqual([]);
  });

  it("reports the hash algorithm the server named rather than assuming one", async () => {
    vi.stubGlobal("fetch", mockFetchOk({
      artifacts: [
        { ...WIRE_ARTIFACT, id: "a/b/hashed" },
        { ...WIRE_ARTIFACT, id: "a/b/unhashed", checksum: undefined, checksum_algorithm: undefined }
      ]
    }));

    const got = await httpApi.listArtifacts();

    expect(got[0].checksumAlgorithm).toBe("sha256");
    expect(got[1].checksumAlgorithm).toBe("");
    expect(got[1].checksum).toBe("");
  });

  it("maps a healthy artifact, leaving quarantine null rather than a half-filled record", async () => {
    vi.stubGlobal("fetch", mockFetchOk({ artifacts: [WIRE_ARTIFACT] }));

    const [got] = await httpApi.listArtifacts();

    expect(got.id).toBe("nas-a/photos/2026-08-30.tar");
    expect(got.setId).toBe("nas-a/photos");
    expect(got.filename).toBe("2026-08-30.tar");
    expect(got.validation).toBe("verified");
    expect(got.sizeBytes).toBe(4096);
    // An omitted timestamp is null, never a zero date: the API omits the
    // key entirely for an event that has not happened.
    expect(got.remoteSourceRemovedAt).toBeNull();
    expect(got.quarantine).toBeNull();
  });

  it("gives a quarantined artifact a reason, and derives the category from what the server said", async () => {
    vi.stubGlobal("fetch", mockFetchOk({
      artifacts: [{
        ...WIRE_ARTIFACT,
        state: "QUARANTINED",
        validation: "failed" as const,
        quarantined: true,
        quarantine_reason: "local final file now hashes to something else"
      }]
    }));

    const [got] = await httpApi.listQuarantine();

    expect(got.validation).toBe("failed");
    expect(got.quarantine).not.toBeNull();
    expect(got.quarantine?.reason).toBe("checksum-mismatch");
    expect(got.quarantine?.remoteSourceRetained).toBe(true);
  });

  it("falls back to validation-failed for a quarantine reason it cannot categorise", async () => {
    // The positive control for the assertion above: without it, a mapper
    // that always answered "checksum-mismatch" would look correct.
    vi.stubGlobal("fetch", mockFetchOk({
      artifacts: [{ ...WIRE_ARTIFACT, quarantined: true, quarantine_reason: "the validator said no" }]
    }));

    const [got] = await httpApi.listQuarantine();
    expect(got.quarantine?.reason).toBe("validation-failed");
  });

  it("collapses the per-set health report to the deployment's WORST verdict", async () => {
    vi.stubGlobal("fetch", mockFetchOk({
      generated_at: "2026-08-30T10:00:00Z",
      backup_sets: [
        {
          backup_set_id: "a/one", source_name: "a", set_name: "one",
          state: "HEALTHY", reason: "fresh", stale_after_seconds: 86400,
          current_transfers: 0, pending_deletes: 0, failures: 0,
          quarantined_count: 0, quarantined_lost_count: 0,
          free_bytes: 100, free_bytes_known: true, total_bytes: 1000, storage_level: "OK",
          newest_good_backup_at: "2026-08-30T09:00:00Z"
        },
        {
          backup_set_id: "a/two", source_name: "a", set_name: "two",
          state: "FAILING", reason: "an artifact was lost", stale_after_seconds: 86400,
          current_transfers: 0, pending_deletes: 0, failures: 1,
          quarantined_count: 1, quarantined_lost_count: 1,
          free_bytes: 5, free_bytes_known: true, total_bytes: 1000, storage_level: "CRITICAL",
          newest_good_backup_at: "2026-08-29T10:00:00Z"
        }
      ]
    }));

    const got = await httpApi.getHealth();

    // A deployment with one failing set is failing, and the headline is
    // that set's own reason rather than the first set's or an average.
    expect(got.backupHealth).toBe("failing");
    expect(got.backupHealthReason).toBe("an artifact was lost");
    expect(got.setsHealthy).toBe(1);
    expect(got.setsFailing).toBe(1);
    expect(got.quarantinedCount).toBe(2);
    expect(got.storageFreeBytes).toBe(105);
    expect(got.storageTotalBytes).toBe(2000);
    expect(got.storageState).toBe("critical");
    expect(got.newestVerifiedBackupAt).toBe("2026-08-30T09:00:00Z");
    expect(got.storageReadingsUnavailable).toBe(0);
  });

  it("reports an unmeasurable freshness as null, not as a large number of hours", async () => {
    vi.stubGlobal("fetch", mockFetchOk({
      generated_at: "2026-08-30T10:00:00Z",
      backup_sets: [{
        backup_set_id: "a/one", source_name: "a", set_name: "one",
        state: "DEGRADED", reason: "no backup has ever landed", stale_after_seconds: 86400,
        current_transfers: 0, pending_deletes: 0, failures: 0,
        quarantined_count: 0, quarantined_lost_count: 0,
        free_bytes_known: false
      }]
    }));

    const got = await httpApi.getHealth();

    expect(got.oldestSetFreshnessHours).toBeNull();
    expect(got.setsDegraded).toBe(1);
    // A capacity reading that could not be taken is counted, not reported
    // as zero free bytes.
    expect(got.storageReadingsUnavailable).toBe(1);
    expect(got.storageFreeBytes).toBe(0);
  });

  it("says so when nothing is configured, rather than declaring an empty deployment healthy in silence", async () => {
    vi.stubGlobal("fetch", mockFetchOk({ generated_at: "2026-08-30T10:00:00Z", backup_sets: [] }));

    const got = await httpApi.getHealth();

    expect(got.backupHealth).toBe("healthy");
    expect(got.backupHealthReason).toMatch(/no backup sets/i);
    expect(got.oldestSetFreshnessHours).toBeNull();
  });

  it("maps a transition into a captioned activity event, and keeps an unrecognised one", async () => {
    vi.stubGlobal("fetch", mockFetchOk({
      events: [
        {
          artifact_id: "a/one/x.tar", backup_set_id: "a/one", source_name: "a",
          set_name: "one", artifact_name: "x.tar",
          from: "COMMITTED", to: "COMPLETE", occurred_at: "2026-08-30T09:05:00Z",
          detail: "source released"
        },
        {
          artifact_id: "a/one/x.tar", backup_set_id: "a/one", source_name: "a",
          set_name: "one", artifact_name: "x.tar",
          to: "SOMETHING_NEW", occurred_at: "2026-08-30T09:00:00Z"
        }
      ]
    }));

    const got = await httpApi.listActivity();

    expect(got[0].type).toBe("remote-source-deleted");
    expect(got[0].severity).toBe("ok");
    expect(got[0].detail).toBe("source released");
    expect(got[0].id).not.toBe(got[1].id);
    // An unrecognised transition still appears: an unexplained gap in an
    // audit trail is indistinguishable from nothing having happened.
    expect(got[1].text).toBe("SOMETHING_NEW");
    expect(got[1].severity).toBe("info");
  });

  it("leaves an operation's byte counters undefined rather than reporting zero transferred", async () => {
    vi.stubGlobal("fetch", mockFetchOk({
      operations: [
        { operation_id: "op_2", status: "running", action: "run_cycle", created_at: "2026-08-30T09:01:00Z" },
        { operation_id: "op_1", status: "completed", action: "run_cycle", created_at: "2026-08-30T09:00:00Z", finished_at: "2026-08-30T09:00:30Z" }
      ]
    }));

    const got = await httpApi.listOperations();

    expect(got[0].id).toBe("op_2");
    expect(got[0].percent).toBe(0);
    expect(got[1].percent).toBe(100);
    // undefined renders as "unknown"; a zero renders as "nothing has been
    // transferred", and only one of those is true.
    expect(got[0].bytesDone).toBeUndefined();
    expect(got[0].bytesTotal).toBeUndefined();
    expect(got[0].nonDestructive).toBe(false);
  });

  it("counts an unreadable recovery manifest as needing review, not as a clean scan", async () => {
    vi.stubGlobal("fetch", mockFetchOk({
      dry_run: true, scanned: 47, reconstructed: 2, already_present: 45,
      failures: [{ backup_set_id: "a/one", path: "/m/bad.json", reason: "unreadable" }]
    }));

    const got = await httpApi.scanCatalog();

    expect(got).toEqual({ discovered: 47, valid: 45, requiresReview: 3 });
  });
});
