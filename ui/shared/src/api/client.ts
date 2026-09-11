/**
 * The one BackupManagerApi implementation that talks to a running
 * service, and the wire-to-domain translation that lets everything above
 * it forget a wire exists.
 *
 * Pages import `httpApi` and the domain types in `@shared/types`; no page
 * ever names a `Wire*` type. That is the whole point of the file. A
 * contract change lands in `generated/contract.ts` and then here, and the
 * compiler walks the rest of the tree for us.
 *
 * The mapping is not mechanical, and the parts that are not are where the
 * bugs were. Two habits run through it, and they pull in opposite
 * directions on purpose. An array the wire OMITS is normalised to `[]`,
 * because the domain types make those required and "the server did not
 * say" is not a state any surface can render: a deployment with no
 * storage medium simply has no moves, and asking every consumer to
 * distinguish an absent list from an empty one buys nothing. A SCALAR the
 * wire omits stays absent, or becomes null, because for those the absence
 * IS the fact. An unverified copy has no verification class, and
 * defaulting it to the weakest rung would put a claim on screen that
 * nobody made. `RetentionVerdict.medium` is the sharpest instance and
 * carries its own note at the field: resolving an absent medium to the
 * implicit local one is right for a DELETE and a lie on the two REFUSE
 * shapes, which name no place at all.
 *
 * The other recurring shape is the lookup table with no default. Halt
 * reasons, health states, storage states and retention classes all map a
 * server word onto a closed union, and an unrecognised word falls off the
 * end into undefined rather than into the nearest neighbour. A newer
 * service saying something this build has never heard of should draw
 * nothing, not draw the wrong thing confidently.
 */
import { BackupManagerError, RequestFailure, toApiErrorCode } from "./contracts";
// Issue #730's diagnostics, opt-in and silent unless an operator turns
// them on. Imported rather than inlined because the gate, the console
// format and the non-browser guards belong to one module, not to the
// three catch blocks below.
import { debugEnvironment, debugLog, describeError } from "./debug";
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
  WireBackupSetRetention,
  WireBackupSetEditHold,
  WireBackupSetEditHoldState,
  WireBackupSetHealth,
  WireBackupSetSpec,
  WireCapacitySettings,
  WireCatalogReportResponse,
  WireCompleteFirstRunResponse,
  WireCreateBackupSetRequest,
  WireCreateBackupSetResponse,
  WireFirstRunStatusResponse,
  WireHealthResponse,
  WireListActivityResponse,
  WireLiveActivityResponse,
  WireLiveActivitySet,
  WireLiveActivityAction,
  WireLiveActivityEvent,
  WireListArtifactsResponse,
  WireListBackupSetsResponse,
  WireListOperationsResponse,
  WireListSSHKeyCandidatesResponse,
  WireListSSHKeysResponse,
  WireListStorageStatusResponse,
  WireManagerStorage,
  WireImportStorageCredentialsResponse,
  WireBackendManifest,
  WireListBackendsResponse,
  WireListStorageMediumsResponse,
  WireMediumPreflightResponse,
  WireMediumConfigurationResponse,
  WireStorageMediumSummary,
  WireStorageMediumUsageResponse,
  WireOperation,
  WirePlacement,
  WireRetentionOverride,
  WireRetentionPlan,
  WireRetentionSettings,
  WireSSHKey,
  WireTestConnectionResponse,
  WireRetentionTier,
  WireRunningWork,
  WireSettingsResponse,
  WireUpdateCapacitySettings,
  WireVersionResponse
} from "./generated/contract";
import type {
  ApiError,
  AppSettings,
  BackendManifest,
  BackupManagerApi,
  BackupSetRetention,
  BackupSetPatch,
  CapacitySettings,
  CatalogScanPreview,
  ConnectionTestOutcome,
  StorageMedium,
  StorageMediumConfiguration,
  StorageMediumSpec,
  ConnectionTestParams,
  CreateBackupSetRequest,
  CreatedBackupSet,
  ManagerStorage,
  MediumPreflight,
  RetentionOverride,
  RetentionSettings,
  RetentionTierSetting,
  RunningWork,
  SSHKeyImportResult,
  SSHKeyListing,
  UpdateSettingsRequest
} from "./contracts";
import type {
  ArtifactRetentionPolicy,
  BackupArtifact,
  BackupPlacement,
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
  OperationStatus,
  Severity,
  SystemHealth,
  TransferProgress,
  VersionInfo
} from "@shared/types/operation";
import type { LiveActivity, SetActivity, SetActivityEvent, UnfinishedAction } from "@shared/types/activity";

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

/**
 * The bootstrap token this browser was handed, or null when the page was
 * opened without one.
 *
 * Exported because the enrollment page has to tell those two cases apart
 * to say anything useful about a refusal (#274): the service answers
 * BOOTSTRAP_TOKEN_INVALID whether the token was missing, expired or
 * already spent, and "you opened a link with no token in it" is the one
 * of the three the browser can settle by itself, without asking.
 */
export function bootstrapTokenFromLocation(): string | null {
  const token = new URLSearchParams(window.location.search).get("token");
  return token === null || token === "" ? null : token;
}

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
    const bootstrapToken = bootstrapTokenFromLocation();
    if (bootstrapToken) headers[BOOTSTRAP_TOKEN_HEADER] = bootstrapToken;
  }

  // Issue #598. Everything from here down is the one place that can tell
  // this API's three failures apart, so it is the one place that labels
  // them. A `fetch` that rejects and a 2xx body that will not parse used
  // to escape as whatever the browser threw, and the callers above could
  // then only say something generic about them.
  let res: Response;
  // Issue #730. The URL asked for and how long the attempt lasted are
  // knowable only here, and a rejected `fetch` carries neither. The timer
  // runs whether or not diagnostics are on: one `performance.now()` is
  // cheaper than asking the toggle an extra time, and a reading nobody
  // logs costs nothing.
  const url = BASE + path;
  const startedAt = performance.now();
  debugLog("request.start", { method, url });
  try {
    // `BASE + path` spelled out again here rather than passing `url`:
    // scripts/api/check-client-paths.sh reduces every path expression in
    // this file statically and then REFUSES to trust its own result
    // unless the single fetch() in it literally reads `fetch(BASE +
    // path`, because a fetch given anything else could be requesting a
    // URL the gate never saw. `url` above reads identically at runtime
    // and still failed that check, which is how #730's diagnostics
    // commit turned a CI step red.
    res = await fetch(BASE + path, {
      credentials: "same-origin",
      ...init,
      // headers last: spreading ...init after a merged `headers` object
      // would otherwise silently replace it with init.headers alone
      // whenever a caller passes its own headers (none do today, but the
      // ordering bug is easy to reintroduce without noticing — see
      // client.test.ts's own coverage for this).
      headers
    });
  } catch (cause) {
    // No response at all, so no status, no content type and no id. It
    // deliberately does NOT claim nothing was changed: a request that got
    // no reply may still have been carried out with only the response
    // lost.
    //
    // This is #730's exact site: one deployment's Activity page reaches
    // here with `TypeError: Failed to fetch` while curl to the same route
    // answers 401. The typed failure below is all an operator sees; the
    // line above it is everything the browser knew and could not put in
    // it, and it is written only when diagnostics were asked for.
    debugLog(
      "request.no-response",
      {
        path,
        url,
        method,
        cause: describeError(cause),
        ...debugEnvironment(),
        elapsedMs: Math.round(performance.now() - startedAt)
      },
      "error"
    );
    throw new RequestFailure({ kind: "no-response", path, cause });
  }

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
          correlationId: headerCorrelationId
        };
      } else {
        api = {
          code: toApiErrorCode(body.code),
          message: body.message as string,
          correlationId: (body.correlationId as string) ?? headerCorrelationId
        };
      }
    } catch {
      api = {
        code: "unknown",
        message: "The backup service returned an unexpected response.",
        correlationId: res.headers.get("x-correlation-id") ?? undefined
      };
    }
    // #730: the correlation id is the one string that joins this refusal
    // to the server's own log line for it, and until now it only ever
    // reached the screen. A refusal that renders as "Failed to fetch" in
    // a bug report is one nobody can match up; a logged id is.
    debugLog(
      "request.error-status",
      { path, status: res.status, code: api.code, correlationId: api.correlationId },
      "error"
    );
    throw new BackupManagerError(api);
  }

  if (res.status === 204) return undefined as T;
  try {
    return (await res.json()) as T;
  } catch (cause) {
    // The response arrived and this build could not read it. The status
    // and the content type are what separate a proxy's HTML error page
    // from a body that was cut off mid-transfer, and the correlation id is
    // read here on the SUCCESS path as well as on a refusal (#598) so a
    // body that fails to parse can still name the response it came from.
    const status = res.status;
    const contentType = res.headers.get("content-type") ?? undefined;
    const correlationId = res.headers.get("x-correlation-id") ?? undefined;
    debugLog(
      "request.unreadable-body",
      { path, status, contentType, correlationId, cause: describeError(cause) },
      "error"
    );
    throw new RequestFailure({ kind: "unreadable-body", path, status, contentType, correlationId, cause });
  }
}

/**
 * `headers` is the third parameter rather than a fourth call shape,
 * because without it this helper could not send an Idempotency-Key and
 * nothing did.
 *
 * That was issue #597's third layer: both generated contracts declare the
 * header required on POST /operations and the handler refuses without it,
 * but every state-changing call in this file went through a helper with
 * no way to set one. The refusal was a 400 that nothing rendered, so it
 * looked exactly like the dashboard's unwired button. contract.
 * conformance.test.ts now asserts the header on every operation whose
 * contract row says it is required, which is what stops the class coming
 * back rather than this one fix.
 */
const post = (path: string, body?: unknown, headers?: Record<string, string>) =>
  request<void>(path, {
    method: "POST",
    body: body ? JSON.stringify(body) : undefined,
    headers
  });

/**
 * The header name POST /operations requires, spelled once.
 *
 * Its uniqueness namespace is the whole deployment rather than one route
 * (apps/common/webhost's package doc spells out why that is easy to get
 * wrong), and it describes the RETRY rather than the operation, which is
 * why it travels as a header and not as a body field.
 */
export const IDEMPOTENCY_KEY_HEADER = "Idempotency-Key";

/**
 * A fresh idempotency key for one LOGICAL submission.
 *
 * Called once per thing an operator asked for, and the SAME key is then
 * reused on every retry of it, which is the entire point of the header: a
 * client that minted a new key per attempt would turn a dropped response
 * into a second backup run, and the service would have no way to know the
 * two requests were the same intent. See useRunControls, which owns that
 * lifetime, rather than this file, which cannot know what a retry is.
 *
 * randomUUID where the browser has it, and a random fallback where it
 * does not: crypto.randomUUID is unavailable on a plain-HTTP origin in
 * some browsers, which is exactly how a NAS on a local network is
 * reached, so a hard dependency on it would break the header on the
 * deployments this product is for.
 */
export function newIdempotencyKey(): string {
  const c = globalThis.crypto;
  if (c && typeof c.randomUUID === "function") return c.randomUUID();
  if (c && typeof c.getRandomValues === "function") {
    const bytes = c.getRandomValues(new Uint8Array(16));
    return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  }
  return "k-" + Date.now().toString(36) + "-" + Math.random().toString(36).slice(2, 12);
}

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
    disabled: req.disabled,
    read_only: req.readOnly
  };
}

function wireCreateBackupSetRequest(req: CreateBackupSetRequest): WireCreateBackupSetRequest {
  const body: WireCreateBackupSetRequest = {
    ...wireBackupSetSpec(req),
    run_immediately: req.runImmediately
  };
  // Only when the caller actually set it, exactly as wireBackupSetPatch
  // does for the edit path's copy of this field: a create that carried it
  // unconditionally would be pre-acknowledged, and the refusal it answers
  // could then never fire at all.
  if (req.acknowledgeRepoint !== undefined) body.acknowledge_repoint = req.acknowledgeRepoint;
  return body;
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
    readOnly: body.read_only,
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
 * `health` is that set's entry from GET /system/health, when the report
 * could be read (issue #245). This type has always been a view model
 * rather than a copy of one endpoint: `state`, `stateNote` and
 * `haltReason` are runtime facts sitting on a shape whose other half is
 * configuration, and before the join they were placeholders this mapper
 * invented, so a set the transport refused to connect to rendered as
 * `stale` with "Health details are not yet reported by the server for
 * this backup set." beside it. The verdict, its sentence and any standing
 * connection refusal now come from the server that computed them.
 *
 * `health` is undefined when the report could not be read, or carried no
 * entry for this set. That case keeps the old placeholder rather than
 * guessing: a health endpoint nobody could ask is not evidence the set is
 * fine. `haltReason` stays ABSENT there, which is the honest answer, and
 * absent is a claim this type is allowed to make where a boolean was not
 * (issue #231).
 *
 * `stale_after_seconds` is on the wire response now (issue #555 put it
 * there, so the API stopped being able to write a field it could not read
 * back) and is deliberately still not taken here. It would map onto
 * `expectedIntervalHours`, which is the same join issue #245 refused from
 * the health report, and taking it is its own change with its own naming
 * question rather than a side effect of the field appearing.
 *
 * The join stops just past the verdict, and where it stops moved once.
 * `newest_good_backup_at` IS taken now, onto `newestKnownGoodAt`. It used
 * to be left out on the argument that taking it would leave two real
 * dates beside one invented null, which was a tidiness argument and was
 * fine until a live browser spec watched a set that had just committed
 * three artifacts render a Healthy badge with "Newest known-good: never"
 * under it. That is one card giving two contradictory answers about one
 * set, and "never" is the single worst thing this product can say wrongly,
 * because it says a backup set has no restore point at all. The field is
 * computed end to end (internal/health aggregates it, core/service
 * carries it, handlers_health serialises it) and this join was already
 * fetching it, so the card was contradicting data the client had in hand.
 *
 * Nothing else moved with it, and the reasons differ per field.
 * `last_completed_backup_at` is NOT `lastRunAt` (a cycle that ran and
 * found nothing is a run with no completed backup), so it is a naming
 * question rather than a mapping. `stale_after_seconds` onto
 * `expectedIntervalHours` is the join issue #245 refused, unchanged.
 * Validations, the retained counters and the host fingerprint stay
 * placeholders because nothing anywhere in core/service computes them
 * yet, so there is no field to take: those are a contract change, not a
 * mapper change. Retention used to be on that list and is not any more:
 * `retention_is_override` is computed, so it is read rather than invented
 * (issue #333).
 */
function fromWireBackupSet(bs: WireBackupSet, health?: WireBackupSetHealth): BackupSet {
  const haltReason = health ? HALT_REASON[health.halt_reason ?? ""] : undefined;
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
    stableForSeconds: bs.stable_for_seconds,
    destination: bs.local_path,
    // Issue #333. This used to be a hardcoded zero policy, which the
    // card and the detail page both drew: "0 / 0 / 0" and "0 kept",
    // against every real deployment. That is exactly the decorative field
    // #299 removed from the wizard, still here. What the server actually
    // computes is which of the two policies is in force, and that is what
    // this now carries; the chain itself is a separate, on-demand read
    // (getBackupSetRetention) on the one page that can render a whole
    // chain.
    retentionIsOverride: bs.retention_is_override,
    validations: [],
    state: health ? HEALTH_STATE[health.state] ?? "degraded" : "stale",
    stateNote: health
      ? health.reason
      : "Health details are not yet reported by the server for this backup set.",
    enabled: !bs.disabled,
    readOnly: bs.read_only,
    // Issue #624. Omitted on the wire when false, and an engine built
    // before this field omits it always, so ?? false is the right
    // default: absence means "nothing here says this set's connection was
    // skipped", never "this set was proven".
    connectionUnverified: bs.connection_unverified ?? false,
    // 0, not undefined, when health could not be read for this set — the
    // same "old placeholder rather than a guess" choice this mapper's own
    // doc above makes for state/stateNote, applied to a count instead of
    // a verdict: a health endpoint nobody could ask is not evidence this
    // set is retaining nothing.
    readOnlyRetainedCount: health?.read_only_retained_count ?? 0,
    // Spread, not `haltReason: undefined`. A key that is present and
    // undefined still reads as the mapper having an opinion; this way a
    // set with no refusal on record simply does not carry the field.
    ...(haltReason ? { haltReason } : {}),
    // Read, not defaulted. Absent means the report genuinely carries no
    // known-good backup for this set, which is the one case where the
    // card's "never" is the truth.
    newestKnownGoodAt: health?.newest_good_backup_at ?? null,
    lastRunAt: null,
    // Four gaps, said as gaps. Every one of these was a literal here
    // ("not-run", 0, 0, 0) that no wire field fed, and every one of them
    // renders somewhere as a confident value nobody chose: a card that
    // reports "Last validation: Not run" for a validator that has been
    // failing for a month, an "every 0h" cadence, and a
    // remove-configuration dialog that says "0 retained backups stay on
    // NAS storage" in the same breath as promising that removing
    // configuration deletes nothing.
    //
    // Nothing on GET /backup-sets or GET /system/health carries any of
    // them, so the fix is not a different literal, it is a type that can
    // say so: BackupSet.lastValidation admits "unknown" and the three
    // numbers admit null, which forces the render sites to print
    // something honest. When a wire field for one of them lands, this is
    // where it is read, and the render sites need no second change.
    lastValidation: "unknown",
    expectedIntervalHours: null,
    retainedCount: null,
    retainedBytes: null,
    // Issue #572: what this set's known_hosts actually pins, read by the
    // service from that file. ABSENT on the wire means the service could
    // not report it, and [] is how that arrives here, which
    // FingerprintDisplay renders as "not reported" rather than as a blank
    // fingerprint under a confident algorithm.
    trustedHostKeys: (bs.trusted_host_keys ?? []).map((k) => ({
      algorithm: k.algorithm,
      fingerprint: k.fingerprint
    })),
    trustedHostKeyRecordedAt: bs.trusted_host_key_recorded_at ?? null,
    // Issue #592: which key in the store this set uses. "" is a real
    // answer meaning "a key this deployment does not manage", which is
    // every set pointing at a mounted or hand-provisioned key file, and
    // the `?? ""` covers a server that predates the field. Both arrive
    // here as "", and the render site says so rather than showing a blank
    // where an id goes.
    sshKeyId: bs.ssh_key_id ?? ""
  };
}

/**
 * Issue #592: one stored key, described without a path because the server
 * sends none. Every optional field is defaulted here rather than left
 * undefined, so a render site never has to distinguish "the server did
 * not say" from "there is nothing to say": both mean the row shows what
 * it can and says why it cannot show the rest.
 */
function fromWireSSHKey(k: WireSSHKey): SSHKeyListing {
  return {
    id: k.id,
    algorithm: k.algorithm,
    fingerprint: k.fingerprint,
    publicKey: k.public_key,
    importedAt: k.imported_at,
    passphraseProtected: k.passphrase_protected,
    // Spread rather than `problem: undefined`, the same discipline
    // fromWireBackupSet uses for haltReason: a key that is present and
    // undefined still reads as the mapper having an opinion.
    ...(k.problem ? { problem: k.problem } : {}),
    usedBy: k.used_by ?? []
  };
}

/**
 * Issues #592 and #596: the six steps a connection test ran, whichever
 * mode asked for it.
 *
 * ONE mapper on both `testConnection` and `testCandidateConnection`,
 * because there is one array on the wire. Two mappers would be two
 * shapes again, and the caller would be back to remembering which
 * request it sent to know what it is holding.
 *
 * `checks` defaults to [] rather than to six fabricated failures. A
 * deployment that predates the field reports nothing, and a surface says
 * "this engine does not report a breakdown" instead of drawing six reds
 * for steps that were never run.
 *
 * Every step comes across, skipped ones included. Filtering to the
 * interesting ones here is how a surface ends up drawing five steps and
 * letting a reader assume the sixth passed.
 *
 * `durationMs` is spread rather than defaulted to 0: absent means this
 * step was not measured on its own (authenticate and list come out of
 * one call), and a 0 there renders as "instantly" when it means "nobody
 * looked".
 */
function fromWireConnectionTestOutcome(r: WireTestConnectionResponse): ConnectionTestOutcome {
  return {
    ok: r.ok,
    ...(r.message ? { message: r.message } : {}),
    checks: (r.checks ?? []).map((c) => ({
      step: c.step,
      outcome: c.outcome,
      ...(c.category ? { category: c.category } : {}),
      detail: c.detail ?? "",
      ...(c.duration_ms === undefined ? {} : { durationMs: c.duration_ms })
    }))
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
    // Issue #333: the policy these verdicts were decided under, and
    // whether it was this set's own or the deployment's. Read from the
    // plan rather than fetched separately, because a plan is pinned to
    // the configuration revision it was computed against and a second
    // read is not: fetching the attribution on its own could render a
    // chain beside the wrong source.
    retention: fromWireRetentionSettings(wire.retention),
    retentionIsOverride: wire.retention_is_override,
    // Issue #430: EPIC E's placement facts. Both arrays are normalised to
    // [], because the wire OMITS them for a deployment that declares no
    // storage medium and an optional array has a third reading ("the
    // server did not say") that is never true here.
    moves: (wire.moves ?? []).map((m) => ({
      artifact: m.artifact,
      fromMedium: m.from_medium,
      toMedium: m.to_medium
    })),
    unconfirmedPlacements: wire.unconfirmed_placements ?? [],
    verdicts: wire.verdicts.map((v) => ({
      artifact: v.artifact,
      action: v.action as RetentionVerdictAction,
      reason: v.reason,
      // Carried through exactly as it arrives, undefined included: see
      // RetentionVerdict.medium (types/backup.ts). Undefined means the
      // implicit local medium on a DELETE, and means "nothing was
      // established" on the two REFUSE shapes that name no place at all,
      // so defaulting it here would turn the second into a claim.
      medium: v.medium,
      // tier_selections, not tiers: the two carry the same tiers in the
      // same order and this UI needs the placement on every one of them
      // (issue #218), so reading the bare list as well would be a second
      // copy that could disagree with the one actually rendered.
      tiers: (v.tier_selections ?? []).map((sel) => ({
        tier: sel.tier,
        selectedBy: sel.selected_by
      }))
    }))
  };
}

/** apps/common/webhost/router.go's `{source}/{set}` route params
 *  (model.BackupSetID's own composite shape), URL-encoded independently —
 *  see BackupSet.source/BackupSet.set's own doc (types/backup.ts). */
function fromWireRetentionSettings(r: WireRetentionSettings): RetentionSettings {
  return {
    timezone: r.timezone,
    weekStartsOn: r.week_starts_on,
    tiers: (r.tiers ?? []).map(fromWireTier),
    protectLastKnownGood: r.protect_last_known_good
  };
}

/** Issue #333: the raw override, unresolved. Every key the server omitted
 *  stays undefined here rather than being normalised to "" or 0, which is
 *  the same rule fromWireTier follows for a tier's two optional numbers
 *  and for the same reason: an omitted field INHERITS, and sending a zero
 *  back would turn an inherited field into an explicit one. */
function fromWireRetentionOverride(o: WireRetentionOverride): RetentionOverride {
  return {
    timezone: o.timezone,
    weekStartsOn: o.week_starts_on,
    dailyDays: o.daily_days,
    weeklyMonths: o.weekly_months,
    monthlyMonths: o.monthly_months,
    tiers: o.tiers ? o.tiers.map(fromWireTier) : undefined,
    protectLastKnownGood: o.protect_last_known_good
  };
}

function wireRetentionOverride(o: RetentionOverride): WireRetentionOverride {
  return {
    timezone: o.timezone || undefined,
    week_starts_on: o.weekStartsOn || undefined,
    daily_days: o.dailyDays || undefined,
    weekly_months: o.weeklyMonths || undefined,
    monthly_months: o.monthlyMonths || undefined,
    // `tiers` is passed through unchanged when it is present, INCLUDING an
    // empty array. An absent key and an empty list mean different things
    // on this route (the second is "I removed every tier", which the
    // server refuses by name because emptying a chain widens the policy
    // rather than disabling it), and collapsing them here would turn a
    // refusal into a silent no-op.
    tiers: o.tiers ? o.tiers.map(wireTier) : undefined,
    protect_last_known_good: o.protectLastKnownGood,
    // Sent only when it is true, for the reason wireUpdateSettings gives:
    // a consent, not a setting, and a literal false on every save would
    // read as an operator repeatedly declining something nobody asked.
    acknowledge_medium_disclosure: o.acknowledgeMediumDisclosure ? true : undefined
  };
}

function fromWireBackupSetRetention(r: WireBackupSetRetention): BackupSetRetention {
  return {
    backupSetId: r.backup_set_id,
    isOverride: r.is_override,
    effective: fromWireRetentionSettings(r.effective),
    deployment: fromWireRetentionSettings(r.deployment),
    override: r.override ? fromWireRetentionOverride(r.override) : undefined
  };
}

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
    windowUnit: t.window_unit,
    // Carried through, unedited, for the reason RetentionTierSetting.medium
    // documents: a chain write replaces the whole chain, so a field this
    // mapper drops is a field the next save deletes from the operator's
    // configuration file.
    medium: t.medium
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
    window_unit: t.windowUnit ? t.windowUnit : undefined,
    medium: t.medium ? t.medium : undefined
  };
}

// The one projection of a declared storage destination onto this UI's
// shape, shared by GET /settings, the destinations list and every write's
// own response (G2.2, #594). One function rather than four literals: the
// field this shape must never grow is a credential, and four copies is
// four places somebody could add one.
function fromWireStorageMedium(m: WireStorageMediumSummary): StorageMedium {
  return {
    id: m.id,
    type: m.type,
    bucket: m.bucket,
    region: m.region,
    endpoint: m.endpoint,
    prefix: m.prefix,
    storageClass: m.storage_class,
    uploadVerification: m.upload_verification,
    readsRequireRestore: m.reads_require_restore,
    // H2.2's three (#622). is_local and is_default are required on the
    // wire and defaulted anyway, because this mapping also runs against
    // an engine older than the fields: a list that came back with no
    // default marked would render every row as "not the default", which
    // is a list no deployment can be in, and reading undefined as false
    // is the honest version of that rather than a guess at which row it
    // would have been.
    path: m.path,
    isLocal: m.is_local ?? false,
    isDefault: m.is_default ?? false,
    // Issue #636. Omitted on the wire when false, and an engine built
    // before this field omits it always, so ?? false is the right
    // default: absence means "nothing here says this destination's check
    // was skipped", never "this destination was proven".
    connectionUnverified: m.connection_unverified ?? false
  };
}

/**
 * The backend catalogue's wire form, camelCased (EPIC I, #664).
 *
 * Every optional string is passed through as-is rather than defaulted:
 * absent and empty mean different things for all three. An absent
 * `unset_means` says the field has no resolved-at-read-time value, and an
 * empty one would read as "it resolves to the empty string", which is a
 * different claim about the engine's behaviour.
 *
 * The one place a default IS applied is the arrays, because a manifest
 * with no enum choices omits `values` entirely and a caller iterating it
 * should not have to ask.
 */
function fromWireBackendManifest(m: WireBackendManifest): BackendManifest {
  return {
    id: m.id,
    label: m.label,
    summary: m.summary,
    role: m.role,
    fields: (m.fields ?? []).map((f) => ({
      id: f.id,
      label: f.label,
      help: f.help,
      kind: f.kind,
      required: f.required,
      values: f.values ? f.values.map((v) => ({ value: v.value, label: v.label })) : undefined,
      pattern: f.pattern,
      unsetMeans: f.unset_means
    })),
    probe: {
      steps: (m.probe?.steps ?? []).map((s) => ({ step: s.step, run: s.run, reason: s.reason }))
    }
  };
}

// toWireStorageMedium is the write direction, and the omissions are the
// interesting part. An absent credentials block is sent as an absent
// credentials block, never as an empty object: on an edit the backend
// reads "no credential named" as "keep the one already configured", and
// an empty object would be indistinguishable from a caller that meant to
// send one and lost it.
function toWireStorageMedium(spec: StorageMediumSpec): Record<string, unknown> {
  const body: Record<string, unknown> = {
    id: spec.id,
    type: spec.type,
    bucket: spec.bucket
  };
  if (spec.region) body.region = spec.region;
  if (spec.endpoint) body.endpoint = spec.endpoint;
  if (spec.prefix) body.prefix = spec.prefix;
  if (spec.storageClass) body.storage_class = spec.storageClass;
  if (spec.uploadVerification) body.upload_verification = spec.uploadVerification;
  // Sent only when it is asked for, so an ordinary save is byte for byte
  // the body it was before #636 and an engine older than the field is
  // never handed one it would reject. This UI never asks for it: the
  // wizard cannot save until its own check has passed.
  if (spec.skipConnectionCheck) body.skip_connection_check = true;
  const c = spec.credentials;
  if (c && (c.credentialsId || c.file || c.env || (c.command && c.command.length > 0))) {
    body.credentials = {
      ...(c.credentialsId ? { credentials_id: c.credentialsId } : {}),
      ...(c.file ? { file: c.file } : {}),
      ...(c.env ? { env: c.env } : {}),
      ...(c.command && c.command.length > 0 ? { command: c.command } : {})
    };
  }
  return body;
}

/**
 * The body both configuration operations take (issue #669).
 *
 * `values` is keyed by MANIFEST FIELD ID and carries the operator's
 * values as the strings the engine validates - a bool as "true" or
 * "false", which is what `backend.validateFieldValue`'s KindBool case
 * reads. There is no key here per S3 field and there is deliberately no
 * `bucket`: that is the difference between this body and
 * toWireStorageMedium above, which is a hand-transcribed copy of
 * s3.json's field ids and therefore cannot carry a local volume's
 * `path`.
 *
 * `credentials` stays its own reference object rather than an entry in
 * `values`, and that is the load-bearing part. A credential is not a
 * value: it is a reference this deployment minted, it is validated by a
 * different rule, and a value bag that could hold one is a value bag
 * something will eventually put material into. Keeping it separate is
 * what makes #665's C1-C5 true by construction on this path too.
 */
function toWireMediumConfiguration(config: StorageMediumConfiguration): Record<string, unknown> {
  // Sorted, and the sort is not tidiness: an unordered pair list makes
  // the request body and the echoed command line differ run to run for
  // no reason, and a test asserting on a body then depends on
  // object-key insertion order. The same three lines live in
  // toWireStorageMedium; they stay duplicated rather than extracted
  // because a two-call-site sort-and-map does not earn a frozen
  // signature (#668's own call).
  //
  // The key is always present, even when there is nothing in it. An
  // EMPTY list means "this instance carries no values" and an absent one
  // would mean "I have nothing to say about values", and those must not
  // share a spelling: this operation sends the whole declared field set,
  // so empty is a real instruction and not a shrug.
  const values = config.fields;
  const body: Record<string, unknown> = {
    fields: Object.keys(values)
      .sort()
      .map((field) => ({ field, value: values[field] }))
  };
  if (config.backend) body.backend = config.backend;
  const c = config.credentials;
  if (c && (c.credentialsId || c.file || c.env || (c.command && c.command.length > 0))) {
    body.credentials = {
      ...(c.credentialsId ? { credentials_id: c.credentialsId } : {}),
      ...(c.file ? { file: c.file } : {}),
      ...(c.env ? { env: c.env } : {}),
      ...(c.command && c.command.length > 0 ? { command: c.command } : {})
    };
  }
  return body;
}

// fromWireMediumPreflight is shared by the by-id preflight and the
// candidate one, so the two cannot render the same report differently.
// Every check is carried through, skipped ones included: a surface that
// dropped them would show a shorter list on a failure than on a success,
// which is the one moment the full list matters most.
function fromWireMediumPreflight(r: WireMediumPreflightResponse): MediumPreflight {
  return {
    medium: r.medium,
    ok: r.ok,
    checks: r.checks.map((c) => ({
      step: c.step,
      outcome: c.outcome,
      category: c.category ?? "",
      detail: c.detail
    }))
  };
}

function fromWireCapacitySettings(c: WireCapacitySettings): CapacitySettings {
  return {
    capBytes: c.cap_bytes,
    warningFreeBytes: c.warning_free_bytes,
    criticalFreeBytes: c.critical_free_bytes,
    safetyMarginBytes: c.safety_margin_bytes,
    backupRoot: c.backup_root,
    backupRootConfigured: c.backup_root_configured
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
    capacity: fromWireCapacitySettings(body.capacity),
    mediums: (body.mediums ?? []).map(fromWireStorageMedium),
    schema: {
      storage: {
        // `class` is a reserved word in the wire shape's own spelling, so
        // it becomes className here; nothing else is renamed.
        verificationClasses: (body.schema.storage.verification_classes ?? []).map((c) => ({
          className: c.class,
          proves: c.proves,
          requires: c.requires,
          downloadsObject: c.downloads_object
        })),
        mediumDisclosure: body.schema.storage.medium_disclosure,
        retrievalDisclosure: body.schema.storage.retrieval_disclosure
      },
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

/**
 * Issue #286. `binding_constraint` and `denominator` both carry the wire's
 * `""` alongside `"disk"`/`"cap"`; the type says so (StorageDenominator has
 * no empty member) so a caller cannot forget that `""` only ever shows up
 * when `known` is false.
 */
function fromWireManagerStorage(m: WireManagerStorage): ManagerStorage {
  return {
    known: m.known,
    unknownReason: m.unknown_reason,
    measuredPath: m.measured_path,
    totalBytes: m.total_bytes,
    freeBytes: m.free_bytes,
    availableBytes: m.available_bytes,
    catalogBytes: m.catalog_bytes,
    catalogBytesKnown: m.catalog_bytes_known,
    otherBytes: m.other_bytes,
    otherBytesKnown: m.other_bytes_known,
    capBytes: m.cap_bytes,
    denominator: m.denominator,
    limitBytes: m.limit_bytes,
    usedBytes: m.used_bytes,
    headroomBytes: m.headroom_bytes,
    bindingConstraint: m.binding_constraint,
    warningFreeBytes: m.warning_free_bytes,
    criticalFreeBytes: m.critical_free_bytes,
    level: m.level
  };
}

/** Builds the PATCH body, carrying "the caller did not name this field"
 *  through as an ABSENT key rather than a null or a zero. The backend
 *  reads an absent key as "leave this alone" and an explicitly empty
 *  tiers list as a refusable request, so collapsing the two here would
 *  silently turn one into the other. */
function wireUpdateSettings(req: UpdateSettingsRequest) {
  const body: Record<string, unknown> = {};

  if (req.retention) {
    const r = req.retention;
    const retention: Record<string, unknown> = {};
    if (r.timezone !== undefined) retention.timezone = r.timezone;
    if (r.weekStartsOn !== undefined) retention.week_starts_on = r.weekStartsOn;
    if (r.tiers !== undefined) retention.tiers = r.tiers.map(wireTier);
    if (r.protectLastKnownGood !== undefined) {
      retention.protect_last_known_good = r.protectLastKnownGood;
    }
    body.retention = retention;
  }

  if (req.capacity) {
    const c = req.capacity;
    const capacity: WireUpdateCapacitySettings = {};
    // Every field carries a meaning at 0 ("no cap", "no warning/critical
    // line"), so the check is `!== undefined`, never truthiness: a
    // capacity.capBytes of 0 must reach the wire as an explicit 0, not be
    // dropped the way an empty string or a falsy number would be.
    if (c.capBytes !== undefined) capacity.cap_bytes = c.capBytes;
    if (c.warningFreeBytes !== undefined) capacity.warning_free_bytes = c.warningFreeBytes;
    if (c.criticalFreeBytes !== undefined) capacity.critical_free_bytes = c.criticalFreeBytes;
    if (c.safetyMarginBytes !== undefined) capacity.safety_margin_bytes = c.safetyMarginBytes;
    body.capacity = capacity;
  }

  // Sent only when it is true. It is a consent, not a setting, and a
  // literal `false` on every write would read as an operator repeatedly
  // declining something nobody asked them.
  if (req.acknowledgeMediumDisclosure) {
    body.acknowledge_medium_disclosure = true;
  }

  return body;
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

/**
 * The wire's halt vocabulary, mapped onto this UI's own (issue #245).
 *
 * A reason this build does not recognise maps to undefined and renders
 * nothing, rather than being passed through as a value no banner knows
 * what to do with. A newer service reporting a reason this UI has never
 * heard of is a real possibility, and the honest thing to show for it is
 * what is shown for a set nothing is known about.
 */
const HALT_REASON: Record<string, BackupSet["haltReason"]> = {
  HOST_KEY_CHANGED: "host-key-changed",
  AUTHENTICATION_FAILED: "authentication-failed",
  KEY_PERMISSIONS: "key-permissions"
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
  let readOnlyRetained = 0;
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
    readOnlyRetained += set.read_only_retained_count;

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
    readOnlyRetainedCount: readOnlyRetained,
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
    // Carried, not dropped (#662). Until this line the mapper read both
    // verdict fields and threw the lifecycle state away, and the two
    // verdicts cannot express "this backup failed an attempt and stopped":
    // nothing on that path records a validation verdict, so `validation`
    // below is "pending", and a FAILED row is not quarantined, so
    // `quarantine` is null. The state is the only field that says it.
    state: a.state,
    validation:
      a.validation === "passed" ? "verified" : a.validation === "failed" ? "failed" : "pending",
    // The backend records which retention tier last selected an artifact,
    // as ONE tier name rather than the classification set this type
    // models. Reporting the one it actually has is the honest mapping;
    // inventing the others would be a claim about a policy this response
    // does not carry.
    retentionClasses: retentionClassesFor(a.retention_tier),
    remoteSourceRemovedAt: stampOrNull(a.remote_source_removed_at),
    retentionPolicy: retentionPolicyFor(a.retention_policy),
    placements: (a.placements ?? []).map(fromWirePlacement),
    quarantine: a.quarantined
      ? {
          reason: quarantineReasonFor(a),
          detail: a.quarantine_reason ?? "",
          detectedAt: a.updated_at,
          remoteSourceRetained: true
        }
      : null
  };
}

/**
 * Narrows the wire's retention_policy onto ArtifactRetentionPolicy.
 *
 * The parameter is widened to `string | undefined` rather than typed as
 * the wire union, because the whole job of this function is the case the
 * types say cannot happen: a response from a build that predates the
 * field. The contract makes retention_policy required, and a type is a
 * claim about a server this client did not compile.
 *
 * Anything the contract does not name becomes "unknown", never
 * "configured". A backup nothing will ever delete is exactly the row an
 * operator opens this screen to find, and resolving silence to the
 * reassuring value would hide it behind a healthy-looking row: the shape
 * of mapper bug this codebase has already shipped once (see
 * fromWirePlacement's own verification_class note for the same rule one
 * field over).
 */
function retentionPolicyFor(value: string | undefined): ArtifactRetentionPolicy {
  switch (value) {
    case "configured":
    case "none":
      return value;
    default:
      return "unknown";
  }
}

/**
 * Maps one durable copy.
 *
 * Every absence on the wire stays an absence here. `verification_class` is
 * omitted for a copy NOTHING has verified, and it becomes null rather than
 * being helpfully defaulted to the weakest rung: "existence" is a claim
 * that an object was seen at the recorded size, and for a copy nobody has
 * looked at, that claim is false. `size_bytes` is omitted when nobody
 * recorded a size and becomes null rather than 0, because a backup can
 * genuinely be zero bytes.
 */
function fromWirePlacement(p: WirePlacement): BackupPlacement {
  return {
    medium: p.medium,
    mediumType: p.medium_type,
    location: p.location,
    sizeBytes: p.size_bytes ?? null,
    storageClass: p.storage_class ?? "",
    verificationClass: p.verification_class ?? null,
    verifiedAt: stampOrNull(p.verified_at),
    access: p.access,
    status: p.status
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
 * A transition neither table names still appears in the feed, as an
 * "info" event captioned with the states themselves. Dropping it would be
 * worse than showing it plainly: an unexplained gap in an audit trail is
 * indistinguishable from nothing having happened.
 */
type ActivityCaption = { type: ActivityEventType; severity: Severity; text: string };

/**
 * The moves whose DESTINATION alone misdescribes them (issue #663).
 *
 * Keyed on the edge, and consulted before the destination table below,
 * because the origin is the discriminator: it is what the state machine
 * itself keys on ("the origin decides what may follow: QUARANTINED is
 * the clearest case", core/lifecycle machine.go), and it is already on
 * the wire, so nothing has to be fetched or joined to read it.
 *
 * Both entries here are one SUCCESSFUL in-place recovery (#662).
 * `completeIngestionInPlace` re-stamps the artifact where it stands once
 * the durable copy has been matched against the remote object, and then
 * passes through the quarantine waypoint that the reinstatement edge has
 * to start from, before committing. Read off the destination those are
 * amber "Attempt failed" and red "Quarantined for review": two failures
 * reported for a recovery that worked, and two rows an operator asking
 * for errors is handed.
 *
 * Every OTHER origin into QUARANTINED is deliberately absent, so it
 * keeps the red badge below. That is the `validate` origin, and there it
 * is a true statement about a record that really is broken.
 *
 * The captions and severities are agreed verbatim with
 * core/cmd/backup-manager/activity.go's own table, which derives the
 * same severity for `rbm activity --severity`; "ok" and "info" are the
 * one rank there, as they are to anyone filtering here. Issue #625 was
 * the last time those two drifted apart.
 *
 * The key type is a pattern rather than plain `string` so a key written
 * without the separator does not compile into a row that can never be
 * hit. It cannot check the STATE names: the wire types them as `string`
 * (generated/contract.ts), and this app declares no closed lifecycle
 * vocabulary to check them against.
 */
const ACTIVITY_BY_TRANSITION: Record<`${string}>${string}`, ActivityCaption> = {
  "FAILED>FAILED": { type: "verification-passed", severity: "ok", text: "Durable copy matched the remote object" },
  "FAILED>QUARANTINED": { type: "validation-failed", severity: "info", text: "Held for the reinstatement judgement" }
};

/** Where a transition ended, for the moves that origin does not change. */
const ACTIVITY_BY_STATE: Record<string, ActivityCaption> = {
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
  // Edge first, destination second, plain default third. The fallbacks
  // are the point as much as the lookup is: an edge nobody named still
  // reads as the state it reached, and a state nobody named still shows
  // up under its own name.
  const known = (e.from ? ACTIVITY_BY_TRANSITION[`${e.from}>${e.to}`] : undefined) ?? ACTIVITY_BY_STATE[e.to];
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
 * Maps the live activity feed off the wire (issue #573).
 *
 * Two shapes change here and both are deliberate. The wire's optional
 * fields become explicit nulls, because "absent" is a fact this feed
 * carries on purpose: a missing artifacts_total means a pass has not
 * counted its rows yet, and rendering it as zero would draw a bar as a
 * finished cycle. And the wire's ordered array of key/value pairs becomes
 * a record, because every reader here looks a field up by name;
 * JavaScript keeps insertion order for string keys, so the renderer that
 * prints an unknown event's fields still prints them in the order they
 * were logged.
 */
function fromWireLiveActivityEvent(e: WireLiveActivityEvent): SetActivityEvent {
  const fields: Record<string, string> = {};
  for (const f of e.fields) fields[f.key] = f.value;
  return {
    sequence: e.sequence,
    at: e.at,
    level: e.level,
    // Absent stays absent rather than becoming a null or an empty
    // string. A line that states no result has reported no operation,
    // which is what a start and every progress note do, and rendering
    // that as a value would make the field the thing nobody trusts.
    result: e.result,
    action: e.action,
    actionId: e.action_id,
    event: e.event,
    scope: e.scope,
    message: e.message,
    fields
  };
}

/** The actions a bucket says are still open (issue #625). */
function fromWireUnfinishedActions(actions: WireLiveActivityAction[]): UnfinishedAction[] {
  return actions.map((a) => ({
    action: a.action,
    actionId: a.action_id,
    startedAt: a.started_at,
    sequence: a.sequence
  }));
}

function fromWireLiveActivitySet(s: WireLiveActivitySet): SetActivity {
  return {
    setId: s.backup_set_id,
    active: s.active,
    stage: s.stage ?? null,
    artifact: s.artifact ?? null,
    artifactsCompleted: s.artifacts_completed,
    artifactsTotal: s.artifacts_total ?? null,
    progressBasis: s.progress_basis,
    bytesTransferred: s.bytes_transferred ?? null,
    bytesTotal: s.bytes_total ?? null,
    bytesPerSecond: s.bytes_per_second ?? null,
    failures: s.failures,
    outcome: s.outcome ?? null,
    startedAt: s.started_at ?? null,
    finishedAt: s.finished_at ?? null,
    events: s.events.map(fromWireLiveActivityEvent),
    unfinishedActions: fromWireUnfinishedActions(s.unfinished_actions ?? []),
    truncated: s.truncated,
    dropped: s.dropped,
    oldestSequence: s.oldest_sequence,
    latestSequence: s.latest_sequence
  };
}

function fromWireLiveActivity(r: WireLiveActivityResponse): LiveActivity {
  return {
    observedAt: r.observed_at,
    epoch: r.epoch,
    pollAfterMs: r.poll_after_ms,
    sets: r.sets.map(fromWireLiveActivitySet),
    // Absent means the reading was narrowed to one set, or came from a
    // service too old to have a deployment bucket at all. Null rather
    // than an empty bucket in both cases: an empty one is a claim that
    // the deployment has said nothing, and neither of those is that.
    deployment: r.deployment
      ? {
          events: r.deployment.events.map(fromWireLiveActivityEvent),
          unfinishedActions: fromWireUnfinishedActions(r.deployment.unfinished_actions ?? []),
          truncated: r.deployment.truncated,
          dropped: r.deployment.dropped,
          oldestSequence: r.deployment.oldest_sequence,
          latestSequence: r.deployment.latest_sequence
        }
      : null
  };
}

/**
 * Maps one operation off the wire onto the UI's model.
 *
 * The durable record and the live reading are two different things and
 * this keeps them apart: the record's own fields (status, action,
 * timestamps) always map, and `progress` maps only when the service
 * actually sent one. The service sends one only while the operation is
 * executing in its process, so a finished operation, a queued one, and one
 * that was running before a restart all arrive with no progress object,
 * and all three map to `progress: null` rather than to a percent of zero.
 */
function fromWireOperation(op: WireOperation): Operation {
  return {
    id: op.operation_id,
    setId: op.backup_set_id ?? "",
    setName: op.backup_set_id ?? "All backup sets",
    kind: "transfer",
    label: op.action ? op.action.replace(/_/g, " ") : "operation",
    status: toOperationStatus(op.status),
    progress: op.progress ? fromWireProgress(op.progress) : null,
    // A run cycle reads from every source and writes only to this
    // deployment's own storage; it releases a remote source only after a
    // durable local copy is committed and verified. It is not a
    // read-only pass, so this is false rather than a comforting default.
    nonDestructive: false,
    startedAt: op.started_at ?? op.created_at ?? "",
    // Absent for anything that is not a finished run cycle, and null
    // rather than a pair of zeroes: "walked nothing, got nothing through"
    // is the loudest thing this object can say, and saying it about a
    // cycle that is merely still running would send an operator hunting a
    // failure that has not happened.
    cycle: op.cycle
      ? {
          backupSetsProcessed: op.cycle.backup_sets_processed,
          artifactsWalked: op.cycle.artifacts_walked,
          artifactsThrough: op.cycle.artifacts_through,
          // Absent stays absent. The service omits this pair for a cycle
          // whose recorded summary predates it, and filling in zeroes
          // here would turn "nobody wrote it down" into "nothing moved".
          moves: op.cycle.moves
            ? { attempted: op.cycle.moves.attempted, landed: op.cycle.moves.landed }
            : null
        }
      : null
  };
}

/**
 * Maps one live reading.
 *
 * Every optional field passes through as-is, undefined included: the
 * service omits a field it did not measure, and this must not helpfully
 * fill in a zero on the way past. `bytes_transferred: 0` is a copy that
 * has started and moved nothing; an absent `bytes_transferred` is a copy
 * nobody is measuring. The renderer needs to be able to tell those apart,
 * so this preserves the difference rather than flattening it.
 */
function fromWireProgress(p: NonNullable<WireOperation["progress"]>): TransferProgress {
  return {
    observedAt: p.observed_at,
    sequence: p.sequence,
    stage: p.stage,
    backupSetId: p.backup_set_id,
    backupSetsDone: p.backup_sets_done,
    backupSetsTotal: p.backup_sets_total,
    artifact: p.artifact,
    artifactsDone: p.artifacts_done,
    bytesDone: p.bytes_transferred,
    bytesTotal: p.bytes_total,
    bytesPerSecond: p.bytes_per_second
  };
}

/** The contract types `status` as an open string; these four are what the
 *  service writes (core/internal/state's operations table). Anything else
 *  is reported as "running" rather than silently dropped: an operation the
 *  client cannot classify is still an operation the service knows about,
 *  and hiding it would be worse than showing it as in flight. */
function toOperationStatus(status: string): OperationStatus {
  switch (status) {
    case "queued":
    case "running":
    case "completed":
    case "failed":
      return status;
    default:
      return "running";
  }
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

/**
 * GET /system/health's per-set array, keyed by backup set id, for the join
 * fromWireBackupSet makes (issue #245).
 *
 * getHealth() next door reads the same endpoint and aggregates it down to
 * fleet counts, throwing the per-set array away. This keeps it, which is
 * what gives `haltReason` a producer at all and what replaces the sets
 * list's invented `state`/`stateNote` with the verdict the server actually
 * computed.
 *
 * A failed read resolves EMPTY rather than rejecting, and the reasoning is
 * worth stating because swallowing an error usually is not defensible.
 * This one is a secondary read behind a primary one: failing the whole
 * sets list because a second endpoint was unavailable would take away the
 * page an operator uses to find out what is configured. An empty map is
 * not a claim that anything is fine either, because every field it feeds
 * falls back to saying it is not known: `haltReason` stays absent and the
 * state note says the verdict is missing. The failure that matters, the
 * dashboard's own health call, still surfaces through getHealth().
 */
/**
 * Turns a BackupSetPatch into the PATCH body, dropping every key the
 * caller left undefined.
 *
 * The dropping is the point, and it is why this is not a field-by-field
 * object literal: an object literal with `port: patch.port` still carries
 * the key with an undefined value, JSON.stringify removes it, and that
 * happens to work, which is worse than either alternative because it
 * works by accident. Doing it explicitly means a future field added here
 * cannot be the one where it stops working.
 *
 * completionMethod is translated back to core's own strategy vocabulary
 * (rename/marker/stable) here, in the one place this boundary is
 * crossed, exactly as fromWireBackupSet translates it the other way.
 */
function wireBackupSetPatch(patch: BackupSetPatch): Record<string, unknown> {
  const body: Record<string, unknown> = {};
  const put = (key: string, value: unknown) => {
    if (value !== undefined) body[key] = value;
  };
  put("host", patch.host);
  put("port", patch.port);
  put("user", patch.username);
  put("remote_path", patch.remoteFolder);
  put("local_path", patch.destination);
  put("include", patch.includePatterns);
  put("completion_strategy", patch.completionMethod && COMPLETION_METHOD_TO_STRATEGY[patch.completionMethod]);
  put("stable_for_seconds", patch.stableForSeconds);
  put("stale_after_seconds", patch.staleAfterSeconds);
  put("ssh_key_id", patch.sshKeyId);
  put("known_hosts_line", patch.knownHostsLine);
  // Both sent only when the caller actually set them, like every key
  // above, so an ordinary save is never a pre-acknowledged one and never
  // a pre-granted re-trust.
  put("acknowledge_repoint", patch.acknowledgeRepoint);
  put("acknowledge_host_key_change", patch.acknowledgeHostKeyChange);
  return body;
}

/** The inverse of COMPLETION_STRATEGY_TO_METHOD, built from it rather
 *  than written out again, so the two can never disagree about which
 *  method means which strategy. */
const COMPLETION_METHOD_TO_STRATEGY: Record<CompletionMethod, string> = Object.fromEntries(
  Object.entries(COMPLETION_STRATEGY_TO_METHOD).map(([strategy, method]) => [method, strategy])
) as Record<CompletionMethod, string>;

/** null stays null: "nothing is running" and "something is running with
 *  no name yet" are different facts, and collapsing them onto a
 *  zero-valued object would make every Edit press warn. */
function fromWireRunningWork(w: WireRunningWork | null | undefined): RunningWork | null {
  if (!w) return null;
  return { artifact: w.artifact, stage: w.stage };
}

function perSetHealth(): Promise<Map<string, WireBackupSetHealth>> {
  return request<WireHealthResponse>("/system/health")
    .then((r) => new Map((r.backup_sets ?? []).map((set) => [set.backup_set_id, set])))
    .catch(() => new Map<string, WireBackupSetHealth>());
}

/** apps/common/webhost/router.go's `{source}/{set}` route params
 *  (model.BackupSetID's own composite shape), URL-encoded independently,
 *  which is also how api/v1/openapi.json publishes them.
 *  See BackupSet.source/BackupSet.set's own doc (types/backup.ts). */
const backupSetPath = (source: string, set: string) =>
  "/backup-sets/" + encodeURIComponent(source) + "/" + encodeURIComponent(set);

/** The half of the refusals below that is the same whichever rule was
 *  broken: why an id that cannot fill a path is refused here rather than
 *  sent. */
const MALFORMED_ID =
  " Building it anyway would request a path this API does not declare, and be answered 404," +
  " which reads as a deployment missing an endpoint rather than as an id that could never" +
  " have named anything.";

/** One segment of a composite id, URL-encoded on its own.
 *
 *  A backup set is "source/set" and a backup is "source/set/name", so both
 *  identities span path segments. A path parameter matches exactly one
 *  segment everywhere it is consumed, in chi and in any client that
 *  escapes what it is handed, so the paths below spell out the segments
 *  the id is made of rather than pasting the whole id in as one. The
 *  contract used to publish these as a single `{id}`, and that was the
 *  defect: the second client to be written from the document escaped its
 *  parameter, as it should, and asked for `production%2Fpostgres`.
 *
 *  A segment that is not there is a refusal, not an empty string. An id
 *  with the wrong number of parts is a caller error, and the only question
 *  is who gets blamed for it: core/internal/apiclient's fillPath refuses
 *  an empty path parameter for the same reason, and these are the same
 *  paths. `of` is how many parts the route needs, so the check is about
 *  the id rather than about the one segment being read.
 *
 *  "." and ".." are refused for the same reason fillPath refuses them:
 *  encodeURIComponent returns both unchanged, because they are legal path
 *  characters, so a part that is one of them would assemble a path that
 *  climbs out of the route the contract declares. As values, never as
 *  substrings, since a dot is an ordinary character in a backup's name. */
const idSegment = (id: string, n: number, of: number) => {
  const parts = id.split("/");
  if (parts.length !== of) {
    throw new Error(
      `this route is built from an id of ${of} parts, and "${id}" has ${parts.length}.` + MALFORMED_ID
    );
  }
  const segment = parts[n];
  if (!segment) {
    throw new Error(
      `this route is built from an id of ${of} parts, and part ${n + 1} of "${id}" is empty.` + MALFORMED_ID
    );
  }
  if (segment === "." || segment === "..") {
    throw new Error(
      `this route is built from an id of ${of} parts, and part ${n + 1} of "${id}" is "${segment}",` +
        ` a relative path segment rather than a name.` + MALFORMED_ID
    );
  }
  return encodeURIComponent(segment);
};

/** GET /backup-sets/{source}/{set}, from the composite id a caller holds. */
const backupSetIdPath = (id: string) => "/backup-sets/" + idSegment(id, 0, 2) + "/" + idSegment(id, 1, 2);

/** /backups/{source}/{set}/{name}, from the composite id a caller holds. */
const artifactPath = (id: string) =>
  "/backups/" + idSegment(id, 0, 3) + "/" + idSegment(id, 1, 3) + "/" + idSegment(id, 2, 3);

/** /quarantine/{source}/{set}/{name}: the same backup, under the three
 *  operator actions a quarantined one has. */
const quarantinedArtifactPath = (id: string) =>
  "/quarantine/" + idSegment(id, 0, 3) + "/" + idSegment(id, 1, 3) + "/" + idSegment(id, 2, 3);

const retentionPath = (source: string, set: string) => backupSetPath(source, set) + "/retention";

/**
 * The contract, implemented against a running service.
 *
 * Every method is one request and one mapping, and the object is flat on
 * purpose: the interesting decisions are per operation (which path, which
 * of the two error shapes, what an omitted field becomes) and each one
 * that has a reason carries it at the method rather than in a shared
 * helper that would hide the differences.
 */
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
    Promise.all([request<WireListBackupSetsResponse>("/backup-sets"), perSetHealth()]).then(
      ([r, health]) => r.backup_sets.map((bs) => fromWireBackupSet(bs, health.get(bs.id)))
    ),
  getSet: async (id) =>
    Promise.all([request<WireBackupSet>(backupSetIdPath(id)), perSetHealth()]).then(([bs, health]) =>
      fromWireBackupSet(bs, health.get(bs.id))
    ),
  // POST /operations with a run_cycle action, not a per-set run route.
  // There has never been one, and the reason is not an oversight: a run
  // cycle is deployment-wide (it walks every enabled backup set), which is
  // why the durable operation record carries no backup set id either. The
  // config_revision is what makes the submission optimistically
  // concurrent: a stale value is refused server-side rather than running
  // against a configuration the caller has not seen.
  runCycle: (configRevision, idempotencyKey) =>
    post(
      "/operations",
      { action: "run_cycle", config_revision: configRevision },
      { [IDEMPOTENCY_KEY_HEADER]: idempotencyKey }
    ),
  // The second run action on the same route (issue #597). Same route,
  // same gate, same tier: /operations is where durable, idempotency-keyed,
  // revision-checked long work is started, and the durable row has always
  // had a backup set id column that run_cycle correctly leaves empty.
  //
  // The engine half is not new either. `rbm fetch
  // --backup-set` has called internal/app.Service.Fetch since FR-1; what
  // was missing was a way to reach it in the SERVING process, so the work
  // takes the engine's single-flight lock and shows up in its feeds
  // instead of running in a second process against the same journal.
  runBackupSet: (backupSetId, configRevision, idempotencyKey) =>
    post(
      "/operations",
      { action: "run_backup_set", config_revision: configRevision, backup_set_id: backupSetId },
      { [IDEMPOTENCY_KEY_HEADER]: idempotencyKey }
    ),
  // The same route as runCycle above, with a different action and its own
  // parameter object. Not a route of its own, deliberately: a restore is
  // a durable, idempotency-keyed, configuration-revision-checked
  // operation whose row outlives the request, which is exactly what
  // /operations was built for, and a second route would give this
  // deployment two answers to "how does a long-running job get started".
  //
  // Nothing is read out of the response except the four fields below.
  // In particular nothing here reads or invents a percentage: a restore
  // has none, and the type it resolves to has nowhere to put one.
  restoreCopy: async (req) => {
    const r = await request<WireOperation>("/operations", {
      method: "POST",
      // The same required header the two run actions send. It was missing
      // here for the same reason and with the same result: this route
      // refuses without it, so every restore this client asked for was
      // answered 400 before it reached the service's own logic.
      headers: { [IDEMPOTENCY_KEY_HEADER]: req.idempotencyKey },
      body: JSON.stringify({
        action: "restore_placement",
        config_revision: req.configRevision,
        restore: {
          artifact_id: req.artifactId,
          medium: req.medium,
          window_days: req.windowDays,
          acknowledged: req.acknowledged
        }
      })
    });
    return {
      operationId: r.operation_id,
      status: r.status,
      windowDays: r.restore?.window_days ?? req.windowDays,
      wait: r.restore?.wait ?? "",
      billing: r.restore?.billing ?? ""
    };
  },
  // The persisted-set mode of the shared test-connection route. Sending
  // only the id is the point: this client neither knows nor should have to
  // echo back the key reference and trusted host line the set is
  // configured with.
  testConnection: (id) =>
    request<WireTestConnectionResponse>("/backup-sets/test-connection", {
      method: "POST",
      body: JSON.stringify({ backup_set_id: id })
    }).then(fromWireConnectionTestOutcome),
  setEnabled: (source, set, enabled) => post(backupSetPath(source, set) + "/enabled", { enabled }),
  setReadOnly: (source, set, readOnly) =>
    post(backupSetPath(source, set) + "/read-only", { read_only: readOnly }),

  // Issue #350. The body carries ONLY the keys the caller set, which is
  // what makes a per-box Save persist only that box: wireBackupSetPatch
  // below drops every undefined rather than sending a zero, because a
  // zero is a real answer for `port` and sending one for a field the
  // operator never touched is exactly the silent clobber this route's
  // sparse shape exists to prevent.
  //
  // It reads the whole set back, health included, the same pair getSet
  // fetches, so a caller can put the persisted truth on the graph rather
  // than the value it hoped it had written. Fetching health again is not
  // wasted work: the freshness verdict can genuinely change as a result
  // of the edit (a new stale_after, a moved local path), and a page that
  // kept the old verdict beside a new value would be showing two moments
  // at once.
  updateBackupSet: (source, set, patch) =>
    Promise.all([
      request<WireBackupSet>(backupSetPath(source, set), {
        method: "PATCH",
        body: JSON.stringify(wireBackupSetPatch(patch))
      }),
      perSetHealth()
    ]).then(([bs, health]) => fromWireBackupSet(bs, health.get(bs.id))),

  // Issue #391. DELETE on the set itself, which coexists with the PATCH
  // above on the same path because chi routes on the method too. The 204
  // is already handled by `request` (it returns undefined rather than
  // trying to parse an empty body), so there is nothing to map here.
  removeSet: (source, set) => request<void>(backupSetPath(source, set), { method: "DELETE" }),

  getEditHold: (source, set) =>
    request<WireBackupSetEditHoldState>(backupSetPath(source, set) + "/edit-hold").then((r) => ({
      held: r.held,
      running: fromWireRunningWork(r.running)
    })),
  takeEditHold: (source, set) =>
    request<WireBackupSetEditHold>(backupSetPath(source, set) + "/edit-hold", { method: "POST" }).then(
      (r) => ({ expiresAt: r.expires_at, stopped: fromWireRunningWork(r.stopped) })
    ),
  releaseEditHold: (source, set) => post(backupSetPath(source, set) + "/edit-hold/release"),

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
  // Issue #592's two reads. The listing carries no path by construction
  // (the server does not send one), so there is nothing to strip here;
  // the candidate scan does, and its paths are rendered as what they are,
  // a description of the operator's own machine.
  listSSHKeys: () =>
    request<WireListSSHKeysResponse>("/ssh-keys").then((r) => (r.keys ?? []).map(fromWireSSHKey)),
  listSSHKeyCandidates: () =>
    request<WireListSSHKeyCandidatesResponse>("/ssh/key-candidates").then((r) => ({
      // Both halves, always, and `?? []` on each: a deployment that
      // reports neither must render as "nothing was scanned", which the
      // wizard says out loud, rather than as "no keys found".
      locations: (r.locations ?? []).map((l) => ({
        path: l.path,
        kind: l.kind,
        found: l.found,
        ...(l.problem ? { problem: l.problem } : {})
      })),
      candidates: (r.candidates ?? []).map((c) => ({
        id: c.id,
        path: c.path,
        location: c.location,
        algorithm: c.algorithm,
        fingerprint: c.fingerprint,
        publicKey: c.public_key,
        mode: c.mode,
        inStore: c.in_store,
        selectable: c.selectable,
        ...(c.in_store_id ? { inStoreId: c.in_store_id } : {}),
        ...(c.reason ? { reason: c.reason } : {})
      }))
    })),
  // The other import, on its own route. It sends an id and nothing else:
  // the key material stays on the machine that already holds it, and the
  // server reads it once through the same validation a pasted key goes
  // through. Same result type as importSSHKey above, so a caller that
  // offers both ways in has one success path rather than two.
  importSSHKeyCandidate: (candidateId) =>
    request<SSHKeyImportResult>("/ssh-keys/from-candidate", {
      method: "POST",
      body: JSON.stringify({ candidate_id: candidateId })
    }),
  probeHostKey: (host, port) =>
    request<{ algorithm: string; fingerprint: string; known_hosts_line: string }>("/ssh/host-key-probe", {
      method: "POST",
      body: JSON.stringify({ host, port })
    }).then((r) => ({ algorithm: r.algorithm, fingerprint: r.fingerprint, knownHostsLine: r.known_hosts_line })),
  testCandidateConnection: (params) =>
    request<WireTestConnectionResponse>("/backup-sets/test-connection", {
      method: "POST",
      body: JSON.stringify(wireConnectionTestParams(params))
    }).then(fromWireConnectionTestOutcome),

  listArtifacts: (setId) =>
    request<WireListArtifactsResponse>(
      "/backups" + (setId ? "?setId=" + encodeURIComponent(setId) : "")
    ).then((r) => r.artifacts.map(fromWireArtifact)),
  getArtifact: async (id) => request<WireArtifact>(artifactPath(id)).then(fromWireArtifact),

  listOperations: () =>
    request<WireListOperationsResponse>("/operations").then((r) => r.operations.map(fromWireOperation)),
  // Bounded by construction: a caller that names no limit still gets the
  // service's default rather than the whole record, and the cursor it
  // hands back is how the next page is asked for.
  //
  // Same always-present "?" and per-parameter trailing separator as
  // getLiveActivity below, for the reason spelled out there: it is what
  // keeps every branch of this expression a path whose query begins in
  // the same place.
  listActivity: (query) =>
    request<WireListActivityResponse>(
      "/activity?" +
        (query?.limit ? "limit=" + query.limit + "&" : "") +
        (query?.before ? "before=" + encodeURIComponent(query.before) : "")
    ).then((r) => ({
      events: r.events.map(fromWireActivityEvent),
      // Absent stays absent: a client tests for the key to decide
      // whether there is a page behind this one, and an empty string
      // would answer that question wrongly in every truthy check.
      ...(r.next_cursor ? { nextCursor: r.next_cursor } : {})
    })),
  // Every parameter is optional and each one is appended with its own
  // trailing separator after a "?" that is always present. That is not
  // fussiness: it means every branch of this expression builds a path
  // whose query begins in the same place, so the path is the same string
  // no matter which parameters were passed, and a bare trailing "?" or
  // "&" is inert to every server and every proxy.
  getLiveActivity: (options) =>
    request<WireLiveActivityResponse>(
      "/activity/live?" +
        (options?.setId ? "backup_set=" + encodeURIComponent(options.setId) + "&" : "") +
        (options?.since ? "since=" + options.since + "&" : "") +
        (options?.limit ? "limit=" + options.limit : "")
    ).then(fromWireLiveActivity),
  listQuarantine: () =>
    request<WireListArtifactsResponse>("/quarantine").then((r) => r.artifacts.map(fromWireArtifact)),
  revalidate: async (id) => post(quarantinedArtifactPath(id) + "/revalidate"),
  retryIngestion: async (id) => post(quarantinedArtifactPath(id) + "/retry"),
  // Issue #419. A different path and a different refusal from the one
  // above, because FAILED and QUARANTINED are different facts about a
  // backup: one is not finished, the other is not trusted.
  retryFailedIngestion: async (id, note) =>
    request<void>(artifactPath(id) + "/retry", {
      method: "POST",
      body: JSON.stringify(note ? { note } : {})
    }).then(() => undefined),
  // Unlike its two siblings this one reads its response. A reinstate that
  // reaches the backend and comes back saying the copy is bad is a 200,
  // not a rejection, so a caller that ignored the body could not tell that
  // from a success.
  reinstate: async (id) =>
    request<WireArtifactReinstateResponse>(quarantinedArtifactPath(id) + "/reinstate", { method: "POST" }).then((r) => ({
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

  // Issue #333: three methods on one sub-resource. PUT replaces the whole
  // policy (never PATCH: an override replaces the deployment's chain and
  // is never merged with it), and DELETE is the only spelling of "go back
  // to inheriting", which cannot be a value on an update where an absent
  // field already means "leave this alone".
  getBackupSetRetention: (source, set) =>
    request<WireBackupSetRetention>(retentionPath(source, set)).then(fromWireBackupSetRetention),
  setBackupSetRetention: (source, set, policy) =>
    request<WireBackupSetRetention>(retentionPath(source, set), {
      method: "PUT",
      body: JSON.stringify(wireRetentionOverride(policy))
    }).then(fromWireBackupSetRetention),
  clearBackupSetRetention: (source, set) =>
    request<WireBackupSetRetention>(retentionPath(source, set), { method: "DELETE" }).then(
      fromWireBackupSetRetention
    ),

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

  // Issue #443. Like reinstate above, this one reads its response: a
  // preflight that reaches the backend and comes back saying the bucket
  // denies writes is a 200, and a caller that ignored the body could not
  // tell that from a medium that works.
  preflightStorageMedium: (mediumId) =>
    request<WireMediumPreflightResponse>(
      "/storage-mediums/" + encodeURIComponent(mediumId) + "/preflight",
      { method: "POST" }
    ).then(fromWireMediumPreflight),

  // G2.2 (issue #594). The import is the one call in this file that ever
  // carries an S3 secret, and it carries it in one direction: what comes
  // back is an id, and this method deliberately returns only that, so a
  // caller cannot accidentally hold on to anything else.
  importStorageCredentials: (accessKeyId, secretAccessKey, sessionToken) =>
    request<WireImportStorageCredentialsResponse>("/storage-credentials", {
      method: "POST",
      body: JSON.stringify({
        access_key_id: accessKeyId,
        secret_access_key: secretAccessKey,
        ...(sessionToken ? { session_token: sessionToken } : {})
      })
    }).then((r) => r.id),

  listStorageMediums: () =>
    request<WireListStorageMediumsResponse>("/storage-mediums").then((r) =>
      (r.mediums ?? []).map(fromWireStorageMedium)
    ),

  getStorageMedium: (mediumId) =>
    request<WireStorageMediumSummary>(
      "/storage-mediums/" + encodeURIComponent(mediumId)
    ).then(fromWireStorageMedium),

  getStorageMediumUsage: (mediumId) =>
    request<WireStorageMediumUsageResponse>(
      "/storage-mediums/" + encodeURIComponent(mediumId) + "/usage"
    ).then((r) => ({
      medium: r.medium,
      placements: r.placements,
      backupSets: (r.backup_sets ?? []).map((s) => ({
        set: s.set,
        placements: s.placements,
        onlyCopyHere: s.only_copy_here
      }))
    })),

  listBackends: () =>
    request<WireListBackendsResponse>("/backends").then((r) => ({
      registered: (r.backends ?? []).map(fromWireBackendManifest),
      unregistered: (r.unregistered ?? []).map((u) => ({ transport: u.transport })),
      instanceIdPattern: r.instance_id_pattern,
      reservedInstanceId: r.reserved_instance_id
    })),

  // Verify before save. It writes nothing whatever the report says, and
  // it resolves rather than rejects on a destination that does not work,
  // for the reason preflightStorageMedium does: a bucket that is not
  // there is what an operator did, not what broke.
  preflightStorageMediumCandidate: (spec) =>
    request<WireMediumPreflightResponse>("/storage-mediums/preflight", {
      method: "POST",
      body: JSON.stringify(toWireStorageMedium(spec))
    }).then(fromWireMediumPreflight),

  createStorageMedium: (spec) =>
    request<WireStorageMediumSummary>("/storage-mediums", {
      method: "POST",
      body: JSON.stringify(toWireStorageMedium(spec))
    }).then(fromWireStorageMedium),

  updateStorageMedium: (mediumId, spec) =>
    request<WireStorageMediumSummary>(
      "/storage-mediums/" + encodeURIComponent(mediumId),
      { method: "PUT", body: JSON.stringify(toWireStorageMedium(spec)) }
    ).then(fromWireStorageMedium),

  // Issue #669: the three operations that speak in manifest field ids.
  // The read exists because this flow sends the whole declared field
  // set, so a form cannot start empty: see StorageMediumConfigurationState.
  getStorageMediumConfiguration: (mediumId) =>
    request<WireMediumConfigurationResponse>(
      "/storage-mediums/" + encodeURIComponent(mediumId) + "/configuration"
    ).then((r) => ({
      fields: Object.fromEntries((r.fields ?? []).map((pair) => [pair.field, pair.value])),
      credentialConfigured: r.credential_configured
    })),

  // The write pair takes the id in the path and the collected values in
  // the body, and the preflight writes nothing whatever it answers - the
  // same property preflightStorageMediumCandidate above has, and for the
  // same reason: a destination that does not work is what an operator
  // configured, not a request that broke, so it resolves with `ok` false.
  preflightStorageMediumConfiguration: (mediumId, config) =>
    request<WireMediumPreflightResponse>(
      "/storage-mediums/" + encodeURIComponent(mediumId) + "/configuration/preflight",
      { method: "POST", body: JSON.stringify(toWireMediumConfiguration(config)) }
    ).then(fromWireMediumPreflight),

  configureStorageMedium: (mediumId, config) =>
    request<WireStorageMediumSummary>(
      "/storage-mediums/" + encodeURIComponent(mediumId) + "/configuration",
      { method: "PUT", body: JSON.stringify(toWireMediumConfiguration(config)) }
    ).then(fromWireStorageMedium),

  removeStorageMedium: (mediumId) =>
    request<void>("/storage-mediums/" + encodeURIComponent(mediumId), {
      method: "DELETE"
    }).then(() => undefined),

  // No body. The whole content of the request is which destination, and
  // that is in the path, so a body would be a second place for one fact
  // and a second thing for the engine to reconcile against the path.
  setDefaultStorageMedium: (mediumId) =>
    request<WireStorageMediumSummary>(
      "/storage-mediums/" + encodeURIComponent(mediumId) + "/default",
      { method: "PUT" }
    ).then(fromWireStorageMedium),

  // Issue #286. Reads GET /system/storage's `manager` object only: the
  // per-backup-set list beside it answers a different question (see
  // ManagerStorage's own doc), and nothing in this client needs it yet.
  getStorage: () =>
    request<WireListStorageStatusResponse>("/system/storage").then((r) => fromWireManagerStorage(r.manager)),

  scanCatalog: () =>
    request<WireCatalogReportResponse>("/catalog/scan", { method: "POST" }).then(fromWireCatalogReport),
  rebuildCatalog: () => post("/catalog/rebuild"),

  login: (username, password) => post("/auth/login", { username, password }),
  enrollAdministrator: (username, password) => post("/auth/enroll", { username, password }),
  rotatePassword: (currentPassword, newPassword) =>
    post("/auth/password", { currentPassword, newPassword }),
  logout: () => post("/auth/logout")
};
