/**
 * The interface between this frontend and any backend that can serve it:
 * `BackupdApi` at the bottom of the file, and every request and
 * response type its methods name.
 *
 * Two implementations satisfy it, `httpApi` in client.ts and the fixtures
 * in mock.ts, and the split matters more than it looks. Because pages
 * depend on this file and never on client.ts, the whole UI can be
 * exercised against fixtures without a service, and a page cannot reach
 * for a field the contract does not promise.
 *
 * Nothing here is a snake_case wire shape. These are the camelCase domain
 * types the app speaks; client.ts owns the translation, and the generated
 * bindings own the wire. What this file adds on top of the generated
 * module is the shape of a CONVERSATION rather than of a payload: which
 * request fields go together, which answers can be absent and what an
 * absence means, and which values are references rather than secrets. The
 * per-declaration notes carry that, and several of them record a refusal
 * the service will produce if a caller gets it wrong, because the type
 * alone cannot say "the server rejects this combination".
 */
import { API_ERROR_CODES as GENERATED_API_ERROR_CODES } from "./generated/contract";
import type { ApiErrorCode, WireConnectionCheck, WireMediumPreflightCheck } from "./generated/contract";
import type { BackupArtifact, BackupSet, CompletionMethod, RetentionPlan } from "@shared/types/backup";
import type {
  ActivityEvent,
  Operation,
  SystemHealth,
  VersionInfo
} from "@shared/types/operation";
import type { LiveActivity } from "@shared/types/activity";

/**
 * Every error code this frontend's backends can actually put on the wire,
 * re-exported from the generated bindings rather than restated here.
 *
 * Before issue #166 this file held the list itself, as a runtime array
 * transcribed by hand from two Go packages. That is exactly the shape the
 * API contract rule prohibits: a second source of truth that goes stale
 * silently, and had already done so once (issue #96's review, mandatory
 * finding M2 - the webhost half of the list was missing entirely, so the
 * one branch in this frontend that reads a code could never match).
 *
 * The list now lives in api/v1/openapi.json, is generated into
 * generated/contract.ts, and is checked from both ends: a Go handler that
 * emits an unregistered code fails apps/common/webhost's
 * TestContract_TheErrorCodeRegistryIsExactlyWhatTheHandlersEmit, and a
 * hand edit to the generated file fails
 * scripts/api/check-contract-drift.sh.
 *
 * Two naming conventions still live in the list on purpose, because two
 * Go packages do: the kebab-case values are this UI's own design-canvas
 * vocabulary (WIRE_ERROR_CODES vs UI_ERROR_CODES separates them in the
 * generated module), while apps/common/auth/local and
 * apps/common/webhost both emit UPPER_SNAKE_CASE and are listed verbatim
 * rather than translated.
 */
export {
  API_ERROR_CODES,
  UI_ERROR_CODES,
  WIRE_ERROR_CODES,
  API_ERROR_CLASSES,
  API_OPERATIONS,
  API_VERSION,
  API_BASE_PATH
} from "./generated/contract";
export type { ApiErrorCode, ContractOperation } from "./generated/contract";

/** Correlation id travels with every failure and is shown under "Advanced
 *  details". Raw stack traces are never rendered (§37). */
const KNOWN_API_ERROR_CODES: ReadonlySet<string> = new Set(GENERATED_API_ERROR_CODES);

/** Narrows a code read off the wire to ApiErrorCode, or "unknown" for
 *  anything this frontend does not know. The one place a network string
 *  becomes an ApiErrorCode: a caller comparing against a literal is then
 *  comparing against a value that really can appear, and an unrecognised
 *  code degrades to the generic error path instead of quietly matching
 *  nothing. */
export function toApiErrorCode(value: unknown): ApiErrorCode {
  return typeof value === "string" && KNOWN_API_ERROR_CODES.has(value)
    ? (value as ApiErrorCode)
    : "unknown";
}

/** A refusal, as the service states it. `message` is already written for
 *  an operator rather than for a log, which is why the default path in
 *  api/failure.ts shows it verbatim: a reason nobody anticipated still
 *  beats a reason this frontend invented. */
export interface ApiError {
  code: ApiErrorCode;
  /** Operator-facing sentence. Already human. */
  message: string;
  /** What to do next, if anything. */
  remediation?: string;
  /** The id the failing RESPONSE carried, and ABSENT when it carried
   *  none. It used to be required, which meant every caller that had no id
   *  had to write one down, and the one they all wrote was the literal
   *  `"unavailable"` (#598). That is worse than nothing: ErrorState only
   *  offers its Advanced details disclosure when a failure carried an id,
   *  so a literal one buys an operator a panel to open with a string in it
   *  that appears in no log anywhere. Optional so "no id" is expressible. */
  correlationId?: string;
  /** The HTTP status the refusal arrived with, and ABSENT when no
   *  response arrived at all. Carried because a gateway status is the one
   *  fact that separates "the service refused" from "something in front
   *  of the service answered for it": a bodyless 502 means serve-ui's
   *  reverse proxy could not reach the engine (#795), and without the
   *  status that is indistinguishable here from a service that answered
   *  something unreadable. Never rendered on its own — api/failure.ts is
   *  the only reader. */
  status?: number;
  /** The technical facts behind this failure, for the Advanced details
   *  panel and the copy button beside it: an exception's own name and
   *  message, the request path, the response status and content type where
   *  there was a response. Never a stack trace and never a source path
   *  (§37). Absent when the service named its own reason, which is already
   *  in `message` and says more than a class name would. */
  detail?: string;
}

/** The typed envelope, thrown. It extends Error so an unprepared caller
 *  still gets something with a readable message, and carries `api` so a
 *  prepared one can branch on the code and quote the correlation id. */
export class BackupdError extends Error {
  constructor(readonly api: ApiError) {
    super(api.message);
    this.name = "BackupdError";
  }
}

/**
 * The two failures a request can produce that are NOT refusals, labelled
 * at the one place that can tell them apart (#598).
 *
 * `request()` used to type only the third case, a response the service
 * refused with, and let the other two escape as whatever the browser
 * happened to throw. So every caller above caught an untyped exception and
 * had nothing to say about it but a sentence of its own invention, and
 * "the service never answered" and "the service answered and this build
 * could not read the answer" arrived on screen as the same eleven words.
 * They are completely different problems for whoever is fixing them.
 *
 * Only `request()` constructs one, and that is what makes the label worth
 * anything: an exception reaching a caller WITHOUT this wrapper did not
 * come out of the request at all, it came out of whatever the caller
 * chained onto it, which is a third thing again.
 */
export type RequestFailureKind =
  /** `fetch` itself rejected: nothing came back, so there is no status, no
   *  content type and no correlation id. Whether the request was carried
   *  out is unknown, and this deliberately does not claim otherwise. */
  | "no-response"
  /** A response arrived and its body could not be read as JSON. The status
   *  and content type are the two facts that separate a proxy's HTML error
   *  page from a truncated body, and the correlation id is present
   *  whenever the response carried the header. */
  | "unreadable-body";

export class RequestFailure extends Error {
  readonly kind: RequestFailureKind;
  /** The API path asked for, WITHOUT the base prefix, exactly as the
   *  caller named it ("/activity"). */
  readonly path: string;
  readonly status?: number;
  readonly contentType?: string;
  readonly correlationId?: string;
  /** The exception this wraps. `Error.cause` is not used for it because
   *  the field has to survive being read by code compiled for older
   *  targets, and because a named field is what a test asserts on. */
  readonly cause: unknown;

  constructor(init: {
    kind: RequestFailureKind;
    path: string;
    status?: number;
    contentType?: string;
    correlationId?: string;
    cause: unknown;
  }) {
    super(init.kind === "no-response" ? "the request got no reply" : "the response body could not be read");
    this.name = "RequestFailure";
    this.kind = init.kind;
    this.path = init.path;
    this.status = init.status;
    this.contentType = init.contentType;
    this.correlationId = init.correlationId;
    this.cause = init.cause;
  }
}

/** How an exception says what it is, for the Advanced details panel.
 *  `name: message` rather than String(e), which renders a bare Error as
 *  "Error: boom" and a plain thrown string as itself; both are worth
 *  showing and neither is worth a special case at every call site. */
export function describeException(e: unknown): string {
  if (e instanceof Error) return e.name + ": " + e.message;
  if (typeof e === "string") return e;
  return String(e);
}

/** What a read-only catalog scan found, before anything is written. The
 *  three counts are what the confirmation is built on: an operator agrees
 *  to a specific number of artifacts being adopted and a specific number
 *  needing a look, never to "recover the catalog". */
export interface CatalogScanPreview {
  discovered: number;
  valid: number;
  requiresReview: number;
}

/**
 * Issue #146 (B2.7): the add-backup-set wizard's (#98) real write path,
 * backed by apps/common/webhost's create-backup-set, SSH-key-import,
 * host-key-probe and connection-test endpoints.
 *
 * SSHKeyId/knownHostsLine carry a REFERENCE, never key material or an
 * unverified fingerprint directly — importSSHKey and probeHostKey are
 * what produce those references in the first place, mirroring core's own
 * config.Key (a backup set's config never carries raw key bytes, only
 * where to find them).
 */
/**
 * One entry in the registered application-validator catalog
 * (apps/common/webhost's GET /api/v1/validators, backed by
 * core/service's own RegisteredValidators).
 *
 * An id and a label, and deliberately nothing else. The wizard's step 5
 * picklist sends `id` back as CreateBackupSetRequest.validatorId; the
 * script it resolves to is a server-side path this frontend never learns
 * and could not use (docs/EPIC-B-multi-nas.md §26 Step 5: the API/UI
 * layer selects a validator by id, never by naming an executable).
 */
export interface ValidatorCatalogEntry {
  id: string;
  /** One operator-facing sentence: what this validator checks. */
  summary: string;
}

/**
 * Issue #350: a sparse edit of one already-persisted backup set. Every
 * field is optional, and an omitted one is left alone rather than
 * cleared, which is the property the detail page's per-box Save rests on.
 *
 * It carries no name and no source, deliberately: a backup set's identity
 * keys every journal row, artifact id and recovery manifest it has ever
 * produced, so renaming one is a migration rather than an edit
 * (core/service/backupsetupdate.go's own package doc).
 *
 * It does carry the key reference and the trusted host line, since issue
 * #572. Both are still produced by the import and probe steps rather than
 * typed here, and re-trusting a host is still a trust decision, which is
 * what acknowledgeHostKeyChange answers.
 */
export interface BackupSetPatch {
  host?: string;
  /** 0 selects the default port, so it is a meaningful value rather than
   *  an absent one; omit the key to leave the port alone. */
  port?: number;
  username?: string;
  remoteFolder?: string;
  destination?: string;
  includePatterns?: string[];
  /** This and `stableForSeconds` are one setting the server takes as two,
   *  and a patch moving to "stable-size" has to carry both: a set on that
   *  method with a zero window is refused, and there is no default the
   *  server can supply for it. `withCompanions` in backupSetEditFields is
   *  what keeps the pair together on the way out of the edit form. */
  completionMethod?: CompletionMethod;
  /** Only stored when the completion method in effect after this edit is
   *  "stable-size"; anything else clears it. A patch naming a window the
   *  result would therefore not hold is refused with INVALID_REQUEST
   *  rather than accepted and dropped, so a 200 here means the value was
   *  actually kept. */
  stableForSeconds?: number;
  staleAfterSeconds?: number;
  /** The id of a key POST /ssh-keys has already imported, replacing the
   *  one this set authenticates with. A reference, never key material.
   *  An id no import produced is refused with SSH_KEY_NOT_FOUND. */
  sshKeyId?: string;
  /** The exact known_hosts line this set should trust from now on, as
   *  probeHostKey returns it. It pins ONE plain host key for THIS set's
   *  own host: a marker such as @cert-authority, and a line naming some
   *  other host, are both refused with INVALID_REQUEST. One that changes
   *  what the set trusts for its own address is refused with
   *  BACKUP_SET_HOST_KEY_CHANGE_NOT_ACKNOWLEDGED unless
   *  `acknowledgeHostKeyChange` says otherwise, and that covers a key on
   *  record this one line would stop pinning as well as a new key being
   *  pinned. Re-sending the only line on record changes no trust and is
   *  never refused. */
  knownHostsLine?: string;
  /** Confirms an edit that moves this set to different data. Needed only
   *  when `host`, `remoteFolder` or `destination` actually change on a
   *  set that already has artifacts on record; without it the service
   *  refuses with BACKUP_SET_REPOINT_NOT_ACKNOWLEDGED and writes nothing.
   *  It is not a property of the backup set: it answers one refusal, for
   *  one request. */
  acknowledgeRepoint?: boolean;
  /** Confirms changing what host keys this set trusts for its own
   *  address: a different key being pinned, or a key on record that the
   *  one line sent would stop pinning. Changing `port` does not exempt an
   *  edit from it. A separate answer from `acknowledgeRepoint` on purpose:
   *  that one says "this is the same data at a new address" and this one
   *  says "this is the same host with a new key", and one flag for both
   *  would let an operator who meant one of them quietly grant the other.
   *
   *  It answers for the value it was shown alongside, so the retry after a
   *  refusal re-sends the body that was refused rather than re-reading the
   *  form (BackupSetDetailPage's `refusal` state). */
  acknowledgeHostKeyChange?: boolean;
}

/** What a run cycle is doing for one backup set right now: the content of
 *  the warning shown before edit mode opens. Discarding a partial
 *  transfer of a named artifact is a materially different cost from
 *  cancelling a tick that has not started work, which is why this names
 *  both rather than saying "something is running". */
export interface RunningWork {
  /** The artifact being worked on, or "" during discovery. */
  artifact: string;
  /** One of the cycle's own stage names ("discovering", "transferring",
   *  "verifying", "committing", "cleaning-remote"). */
  stage: string;
}

/** What GET /backup-sets/{source}/{set}/edit-hold answers. */
export interface EditHoldState {
  held: boolean;
  /** Null when no cycle is currently inside this set, which is what lets
   *  edit mode open with no prompt for a risk that does not exist. */
  running: RunningWork | null;
}

/** What taking the hold answers: `stopped` is null when nothing was
 *  running, so a caller never claims to have interrupted something. */
export interface EditHoldTaken {
  expiresAt: string;
  stopped: RunningWork | null;
}

/**
 * Everything needed to create a backup set, in one request.
 *
 * Two properties are worth knowing before adding a field. Nothing here is
 * key material: `sshKeyId` and `knownHostsLine` are references produced by
 * the import and probe steps, so a wizard that never holds a private key
 * cannot leak one. And the same body creates the FIRST configuration on an
 * unconfigured instance, because the operator answers the same questions
 * either way, which is why `runImmediately` has a documented refusal
 * rather than a second request shape.
 */
export interface CreateBackupSetRequest {
  sourceName?: string;
  name: string;
  host: string;
  port: number;
  user: string;
  sshKeyId: string;
  knownHostsLine: string;
  remotePath: string;
  localPath: string;
  include: string[];
  completionStrategy: "rename" | "marker" | "stable";
  stableForSeconds?: number;
  staleAfterSeconds?: number;
  /** The registered application validator to run against every artifact
   *  in this set (listValidators), or omitted for none — which is what
   *  every request before issue #162 meant, and still the default. */
  validatorId?: string;
  /** "Save disabled" — excludes the set from every run cycle until an
   *  operator re-enables it. */
  disabled?: boolean;
  /** Declares this backup set's remote source read-only from creation
   *  (issue #282): pull backups from here, but never delete the remote
   *  original. Omitted or false means exactly what every request meant
   *  before this field existed. Issue #316's wizard control for it. */
  readOnly?: boolean;
  /** "Save, enable & run" — submits a run_cycle operation immediately
   *  after this set is persisted. Ignored (never runs anything) when
   *  disabled is true. */
  runImmediately?: boolean;
  /** Confirms creating this set somewhere other than where the history
   *  already on its id came from (issue #411). Removing a set frees its
   *  id up, and a set created over an id that already has artifacts on
   *  record takes every one of them, so a different host, remote path or
   *  destination is the same move an edit makes. Without it such a create
   *  refuses with BACKUP_SET_HISTORY_REPOINT_NOT_ACKNOWLEDGED and writes
   *  nothing; re-creating a set exactly where it was removed from asks
   *  nothing. Sent only when the caller actually set it, so an ordinary
   *  create is never a pre-acknowledged one. */
  acknowledgeRepoint?: boolean;
}

/** What a submitted run_cycle operation looks like from
 *  createBackupSet's own response: the two fields that response is read
 *  for. Deliberately NOT the Operation type (types/operation.ts), which
 *  also carries the live progress reading a polling client reads (issue
 *  #221) and which a create response could never hold anyway, since the
 *  cycle it just submitted has not started
 *  (docs/EPIC-B-multi-nas.md §14). */
export interface RunCycleSubmission {
  operationId: string;
  status: string;
}

/** What an operator fills in to ask for one archived copy to be restored
 *  (EPIC E, FR-34). */
export interface RestoreCopyRequest {
  /** The backup, as "source/set/name". */
  artifactId: string;
  /** The id of the storage medium holding the copy to restore. */
  medium: string;
  /** How many days the restored copy should stay readable, 1 to 30.
   *  Zero is not a shorter restore, it is one that is billed and then
   *  immediately unavailable. */
  windowDays: number;
  /** The operator saying they know this is billed and takes hours.
   *  Required true; see BackupApi.restoreCopy. */
  acknowledged: boolean;
  /** The configuration revision the caller is displaying. */
  configRevision: string;
  /** One key per LOGICAL restore, reused on every retry of it. POST
   *  /operations declares the header required and refuses without one;
   *  see BackupdApi.runCycle for why the key belongs to the
   *  submission rather than to the attempt. */
  idempotencyKey: string;
}

/** What a restore looks like the instant it has been accepted.
 *
 *  There is no percent, no finishesAt and no cost, and there is nowhere
 *  to add one without editing this comment: the provider reports a
 *  restore as running or finished and nothing else, and this deployment
 *  has no price list. */
export interface RestoreSubmission {
  operationId: string;
  status: string;
  /** The window that was actually asked for, in days. */
  windowDays: number;
  /** The storage class's OWN published restore time, in plain words. A
   *  documented property of the class, and never an estimate for this
   *  particular restore, so a UI must not render it as a countdown. */
  wait: string;
  /** The statement that a bill exists, with no amount. Empty for a class
   *  the provider does not charge retrieval on. */
  billing: string;
}

/** The set as it was actually persisted, echoed back so the caller can
 *  render what it just saved without a second read. The last two fields
 *  are the reason this is not just an id: an immediate run can fail while
 *  the set is created, the response is a 201 either way, and dropping the
 *  error told an operator a backup was running when nothing started. */
export interface CreatedBackupSet {
  id: string;
  sourceName: string;
  name: string;
  host: string;
  port: number;
  user: string;
  remotePath: string;
  localPath: string;
  include: string[];
  completionStrategy: string;
  /** The registered validator this set was saved with, echoed back so a
   *  caller can render what it just persisted without a second fetch.
   *  Empty when none was chosen. */
  validatorId?: string;
  disabled: boolean;
  /** The read-only declaration this set was actually saved with (issue
   *  #282, #316), echoed back so a caller can render what it just
   *  persisted without a second fetch. */
  readOnly: boolean;
  /** Present only when the request's runImmediately was set AND
   *  honoured (never when disabled was also set — see
   *  CreateBackupSetRequest.runImmediately's own doc). */
  operation?: RunCycleSubmission;
  /** Why the requested immediate run did not start. The set itself was
   *  created either way (the response is 201 regardless), so this is the
   *  ONLY signal that "Save, enable & run" half-succeeded: at most one of
   *  operation or runError is ever set. Dropping it, which this mapper
   *  did until PR #194's review, tells an operator a backup is running
   *  when nothing ever started, and they find out at the next restore. */
  runError?: string;
}

/** What an imported key becomes: an id to refer to it by, and a
 *  fingerprint to show. Never the key itself, which is the whole point of
 *  importing through the service rather than carrying it in a form. */
export interface SSHKeyImportResult {
  id: string;
  algorithm: string;
  fingerprint: string;
}

/**
 * Issue #592: one key in this deployment's own store, as GET /ssh-keys
 * describes it.
 *
 * There is no path here and there will not be one. `SSHKeyRef.KeyFile` is
 * kept off the wire so a caller never learns the server's filesystem
 * layout, and an inventory is exactly the shape where a path column looks
 * helpful and is not: the file it would name lives inside a container the
 * operator has no shell in.
 *
 * `publicKey` is the exception and it has to be. It is public material by
 * definition, and it is the one string that turns a red "could not
 * authenticate" into something an operator can act on: paste it into the
 * remote account's authorized_keys.
 */
export interface SSHKeyListing {
  id: string;
  algorithm: string;
  fingerprint: string;
  /** The authorized_keys line. Public material, never the private half. */
  publicKey: string;
  /** RFC 3339, or "" when the deployment cannot report it. "" is a real
   *  answer and must not be rendered as a date. */
  importedAt: string;
  /** Listed, and not offerable for verification until its passphrase
   *  resolves, which is the same rule the import path already applies. */
  passphraseProtected: boolean;
  /** Why this row could not be described. Never names a path. */
  problem?: string;
  /** Every backup set id pointing at this key, sorted. An EMPTY list is a
   *  real and useful answer, and it is the column that turns a wall of
   *  ids into a decision: a key four sets depend on and a key nothing
   *  references are very different things to point a fifth set at. */
  usedBy: string[];
}

/**
 * One private key file the engine can actually see (GET
 * /ssh/key-candidates).
 *
 * Unlike SSHKeyListing this DOES carry a path, because a candidate's path
 * is its identity to an operator and there is no other way to say which
 * of several files is meant. What makes that safe is server-side: the
 * locations scanned are a closed, constant set, never caller-supplied and
 * never walked recursively. The handle that travels back is `id`, which
 * is opaque, so nothing this UI sends can name a file for the server to
 * read.
 *
 * `fingerprint` comes from the .pub beside the key and is "" when there
 * is none. A candidate with no fingerprint is shown, marked, and not
 * selectable: deriving one would mean the server read a private key in
 * order to put it in a list.
 */
export interface SSHKeyCandidate {
  id: string;
  path: string;
  location: string;
  algorithm: string;
  fingerprint: string;
  publicKey: string;
  /** Permission bits as an operator writes them, e.g. "0600". */
  mode: string;
  inStore: boolean;
  inStoreId?: string;
  selectable: boolean;
  /** Why it is not selectable. A row nobody can pick and nobody can
   *  explain is worse than no row. */
  reason?: string;
}

/** One place the scan looked, reported whether or not it held anything.
 *  Rendering these is not optional: an empty candidate list on a packaged
 *  install means the engine is a distroless container that cannot see the
 *  operator's home directory, not that they have no keys. */
export interface SSHKeyDiscoveryLocation {
  path: string;
  /** "configured-key-file" | "mount" | "home" | "discovery-dir". */
  kind: string;
  found: number;
  problem?: string;
}

/** One scan: what was found, and everywhere that was looked. Never one
 *  without the other. */
export interface SSHKeyDiscovery {
  locations: SSHKeyDiscoveryLocation[];
  candidates: SSHKeyCandidate[];
}

/** What a host presented, before anyone has decided to trust it. The
 *  fingerprint is for a human to compare against something they already
 *  know; the known-hosts line is what trust would actually be anchored to,
 *  and only travels once "Trust host" is pressed. */
export interface HostKeyProbeResult {
  algorithm: string;
  fingerprint: string;
  knownHostsLine: string;
}

/** Whether a pre-save connection worked, and the service's own words when
 *  it did not. Deliberately not an error: a failed test is an ordinary
 *  answer to a question the wizard asked on purpose, and throwing would
 *  make the failure path the exceptional one. */
export interface ConnectionTestOutcome {
  ok: boolean;
  message?: string;
  /**
   * What the test actually DID, one entry per step and always all of
   * them, in the order they run (issues #592 and #596).
   *
   * `ok` and `message` mean exactly what they always meant, so this is
   * additive in both directions: an older client reading only those two
   * keeps working, and a newer one against an older engine gets an empty
   * list rather than a wrong one. Empty is "this engine reports no
   * breakdown", which a render site has to draw as that and never as six
   * failures.
   *
   * ONE array for both modes. The persisted mode reads the key, the
   * known_hosts and the remote path off the set and the candidate mode
   * off the request, and they answer the same six questions, so nothing
   * here depends on which request was sent.
   */
  checks: ConnectionCheck[];
}

/**
 * One step of a connection test.
 *
 * `skipped` is a first-class outcome and never a quiet pass: a surface
 * that renders a skipped authentication as anything but "this was never
 * tried" has told an operator their credentials are fine on the strength
 * of a step that never ran. `category` is the machine-readable half a
 * surface branches on; `detail` is a sentence the engine composed and
 * never an underlying transport error's text.
 */
export interface ConnectionCheck {
  /** Both taken from the generated contract rather than restated (issue
   *  #633). The same interface's medium-preflight twin had already gone
   *  stale as a hand-written copy, and there is nothing about this pair
   *  that made it less likely to: a step added to a connection test would
   *  arrive here as a value this UI's types say cannot exist. */
  step: WireConnectionCheck["step"];
  outcome: WireConnectionCheck["outcome"];
  category?: string;
  detail: string;
  /**
   * How long this step took on its own, in milliseconds.
   *
   * OPTIONAL, and absent is not zero. Only credentials, resolve, connect
   * and host_key are measured separately; authenticate and list are
   * decided from a single call and have no timing of their own. A
   * surface that defaulted this to 0 would print "0 ms" next to a green
   * "Authenticated" on every successful run, which says the server
   * answered instantly when what happened is that nobody measured.
   */
  durationMs?: number;
}

/** The subset of CreateBackupSetRequest's SSH-facing fields a pre-save
 *  connection test needs — everything a subsequent createBackupSet call
 *  would carry, minus the fields that only matter once a set actually
 *  exists (name, paths, completion, ...). */
export interface ConnectionTestParams {
  host: string;
  port: number;
  user: string;
  sshKeyId: string;
  knownHostsLine: string;
  remotePath?: string;
}

/**
 * Issue #140 (B3.7): the server-side settings surface, backed by
 * apps/common/webhost's GET/PATCH /api/v1/settings.
 *
 * One retention tier, exactly as core/internal/config's RetentionTier
 * models it (FR-18's chain, generalized from three hardcoded tiers by
 * issue #156). `periodDays` is required by, and only legal on,
 * granularity "days"; `windowUnit` is optional and empty means "the same
 * as granularity", which is the ordinary case — but it is not decoration:
 * the default weekly tier buckets by week and looks back over calendar
 * MONTHS, so a form without it cannot express the default policy.
 */
export interface RetentionTierSetting {
  name: string;
  granularity: string;
  periodDays?: number;
  keep: number;
  windowUnit?: string;
  /** The storage destination this tier's backups live on, by id.
   *
   *  It is on the shape that is both READ and WRITTEN because a settings
   *  write replaces the whole chain: a field this UI could read but not
   *  send back is a field that editing one tier's keep would silently
   *  delete from another tier, moving somebody's backups back onto local
   *  disk without saying so.
   *
   *  Since #622 the backend always names one, LOCAL_DESTINATION_ID
   *  included, so nothing on this side has to know that an absent value
   *  used to mean the local backup root. It stays optional in the TYPE
   *  because this is also the WRITE shape and an older caller that omits
   *  it is still understood, and because the settings schema's default
   *  chain is the same type; a reader should use the value it gets and
   *  fall back to LOCAL_DESTINATION_ID rather than to "". */
  medium?: string;
}

/**
 * One configured storage medium, as GET /settings reports it: what it is
 * called, what kind of place it is, which bucket and region, and which
 * class backups are written with.
 *
 * There is no field for a credential and there is not going to be
 * (FR-33): a medium's credentials reach the backend as a reference to a
 * file, an environment variable or a command, and none of the three has a
 * spelling on this boundary at all.
 */
export interface StorageMedium {
  id: string;
  /** What kind of place this is. `s3` is a bucket an operator declared;
   *  `local` is the drive this deployment's backups land on. Branch on
   *  `isLocal` rather than on this word: the set grows by architecture
   *  decision, and a second local-ish backend added one day must not
   *  silently make an entry undeletable. */
  type: string;
  bucket: string;
  region?: string;
  /** An endpoint override for an S3-compatible service; absent means the
   *  provider's own endpoint for the region. */
  endpoint?: string;
  /** The key namespace inside the bucket; absent puts the key layout at
   *  the root. */
  prefix?: string;
  storageClass: string;
  /** How an upload is proven before the local copy is deleted, already
   *  resolved, so this UI never has to know what an unset value defaults
   *  to. */
  uploadVerification: string;
  /** True when this medium's class cannot be read on demand: a backup here
   *  needs an explicit restore, taking hours, before anything can read it.
   *  Computed by the backend, so this UI holds no list of its own of which
   *  classes count as archive. */
  readsRequireRestore: boolean;

  /** The drive the LOCAL destination writes to, resolved by the backend
   *  exactly as the capacity section resolves its backup root, so one
   *  deployment cannot show two mounts. Absent for a declared medium,
   *  because a bucket has no path on the manager's host, and absent for a
   *  local entry the configuration cannot place yet (no backup set, or
   *  sets on different volumes), which reads as "not known yet" rather
   *  than as a blank path. */
  path?: string;

  /** True for the one entry that is the drive this deployment's backups
   *  land on (#622). It is not declared in the configuration, so it has
   *  no Edit and no Remove, and a surface decides that from this rather
   *  than by comparing an id against a reserved string of its own. */
  isLocal: boolean;

  /** True for the destination a NEWLY CREATED retention tier starts on.
   *  Exactly one entry in a list carries it. It says nothing about where
   *  anything currently is: moving it moves no backup and rewrites no
   *  tier, which is why it needs no confirmation. */
  isDefault: boolean;

  /**
   * Issue #636: this destination was declared without ever having been
   * proven.
   *
   * True means the engine was told to skip the check it runs in front of
   * every create and every destination-changing edit (`--no-verify` on
   * the CLI, `skip_connection_check` on the API), and it is the engine's
   * own record of that: nothing a client sends sets the mark directly,
   * and the S3 wizard never skips, because it cannot save until its own
   * check has passed. A test connection that PASSES against it clears
   * the mark, and one that fails leaves it alone.
   *
   * False is not a claim that the destination works today, only that
   * nothing here says it was never proven. An engine built before this
   * field answers false, and so does every destination declared before
   * the mark existed, which is why a surface says "never proven" for true
   * and says nothing at all for false rather than drawing a green tick.
   *
   * Always false for the local hard drive, which is not declared and has
   * no bucket to prove.
   */
  connectionUnverified: boolean;
}

/**
 * The id of the drive this deployment's backups land on (#622).
 *
 * It is a constant rather than a literal at each call site because it is
 * the one string in this product that has to mean the same thing in a
 * placement record, in a retention tier, in an API response and in a
 * picker. The backend reserves it: no declared destination may claim it,
 * and a configuration file spells this destination by absence, which the
 * server translates in one place so nothing here has to.
 */
export const LOCAL_DESTINATION_ID = "local";

/**
 * Where one storage medium's credentials come from, in the four spellings
 * the backend accepts. Exactly one must be set, and none of them is
 * credential MATERIAL: this is a reference in all four.
 *
 * `credentialsId` is the one this UI uses, and the reason it exists at
 * all. It is opaque, minted by the backend, and names nothing about the
 * manager's host, so it is the only spelling that is safe to put on a
 * request body, into an echoed command line, and into a terminal
 * transcript an operator can export and paste into a support thread. The
 * other three are here because an operator who already has a credential
 * on the host, or in a secrets manager, should not have to hand this
 * product a secret to use it.
 */
export interface StorageMediumCredentialsReference {
  credentialsId?: string;
  file?: string;
  env?: string;
  command?: string[];
}

/**
 * One storage destination as this UI describes it, for declaring it,
 * replacing it, or having it checked before either.
 *
 * The same shape for all three deliberately. What is proven and what is
 * saved must be the same destination, and the interesting bug in a
 * destination wizard is one that verifies green and then saves as
 * something slightly different.
 *
 * `credentials` is optional only on an EDIT, where omitting it keeps the
 * credential already configured. It has to be optional there, because
 * StorageMedium above deliberately reports nothing about the credential,
 * not even its kind, so a form cannot resubmit what it never received.
 */
export interface StorageMediumSpec {
  id: string;
  type: string;
  region?: string;
  endpoint?: string;
  bucket: string;
  prefix?: string;
  storageClass?: string;
  uploadVerification?: string;
  credentials?: StorageMediumCredentialsReference;

  /**
   * Declare this destination without proving it first (issue #636).
   *
   * The engine runs the same eight-step check `preflightStorageMediumCandidate`
   * answers, in front of the write, and rejects with
   * MEDIUM_CONNECTION_NOT_PROVEN when it fails; this is the deliberate
   * opt-out, and a destination written under it is marked
   * `connectionUnverified` until a check passes.
   *
   * This UI does not send it. The wizard keeps Save disabled until its
   * own check has come back green, so there is never a moment where it
   * would have anything to skip; the field is here because the shape it
   * lives on is the API's shape, and a client that could not express the
   * flag could not be told what the refusal it might meet is about.
   */
  skipConnectionCheck?: boolean;
}

/**
 * One destination's configuration, in the vocabulary its backend's
 * manifest declares (issue #669, EPIC I #664).
 *
 * This is the shape StorageMediumSpec above cannot be. That one
 * enumerates S3's fields - `bucket` is required and there is no `path` -
 * because it was written when S3 was the only destination anybody could
 * declare, so it is a hand-transcribed copy of s3.json's field ids and
 * cannot carry a local volume at all. #667 deletes that assumption from
 * the engine; this is the wire's half of the same deletion, and it is
 * additive rather than a rewrite so that #594's S3 wizard keeps working
 * until it is retired.
 *
 * `values` is keyed by manifest field id, with every value as the string
 * the engine validates: a bool is "true" or "false", which is what
 * `backend.validateFieldValue` reads, and an omitted or empty entry
 * means UNSET - never a default filled in by the caller, because a
 * default written back into the record is a default frozen into the
 * operator's file by the next save (#294).
 *
 * `credentials` is separate, and that separation is load-bearing rather
 * than tidy. A credential is not a value: it is a reference this
 * deployment minted, it is checked by a different rule, and a bag that
 * COULD hold one is a bag something will eventually put material into.
 * Keeping it out is what makes #665's C1-C5 hold on this path by
 * construction. Absent means "keep the credential already configured",
 * for StorageMediumSpec's reason: nothing reports a destination's
 * credential back, not even its kind, so a form cannot resubmit what it
 * never received.
 */
export interface StorageMediumConfiguration {
  /**
   * The backend this instance is an instance of, required when the id is
   * not declared yet.
   *
   * It is here because a destination cannot exist unconfigured (P2,
   * #669): `Registry.ValidateInstance` refuses an absent required field
   * (core/internal/backend/validate.go:228-233), `config.Validate`
   * delegates every per-field rule to it, and both bundled manifests
   * have required fields. So there is no record to read a backend id
   * off before the values exist, and the create and the configure are
   * one write with one check in front of it.
   */
  backend?: string;
  fields: Record<string, string>;
  credentials?: StorageMediumCredentialsReference;
}

/**
 * What one destination has configured right now, in its backend's
 * vocabulary (issue #669).
 *
 * A form has to have this before it can offer an edit. This flow sends
 * the WHOLE declared field set, so a form that started empty and saved
 * would unset every field the operator did not retype - an empty form is
 * not a neutral starting point here, and inferring the current values
 * from StorageMedium's named fields in the browser would be a second,
 * hand-transcribed copy of the field-id mapping the engine already owns
 * (config's `storageMediumFieldValue`).
 *
 * An unset optional field is ABSENT rather than present as its
 * `unset_means` value, which is the same #294 rule the write side obeys:
 * a default resolved at read time must not travel as though somebody
 * chose it, because then the next save freezes it into their file.
 *
 * `credentialConfigured` is a boolean and never the reference. Whether a
 * credential exists is what a form needs - it decides whether the pair
 * may be left empty - and it is not material, not a path, and not a
 * variable name, so it is the most this may say (FR-33, #665's C3).
 */
export interface StorageMediumConfigurationState {
  fields: Record<string, string>;
  credentialConfigured: boolean;
}

/**
 * What the journal says is currently on one storage destination, per
 * backup set (FR-30).
 *
 * The list is the point. "148 copies affected" with nothing named is a
 * number an operator cannot act on, which is why this carries the sets
 * and not only the total, and why `onlyCopyHere` is separate: "52 copies
 * live here" and "52 backups have their only confirmed copy here" are
 * different sentences and call for different colours.
 */
export interface StorageMediumUsage {
  medium: string;
  placements: number;
  backupSets: Array<{
    set: string;
    placements: number;
    onlyCopyHere: number;
  }>;
}

/**
 * What one value a backend collects IS (EPIC I, #664).
 *
 * The set is closed in the engine and a manifest may not add to it, so
 * this union is a real closed set rather than a hint: a surface renders
 * one control per kind, and a kind it does not know is a version
 * mismatch to say out loud, not free text to fall back to.
 *
 * `credential` is the marker that routes an input to the credential
 * import instead of into the instance body. It is a kind rather than a
 * flag on a string field because a credential is not a value: the
 * material never travels on the same path a value does, and a form that
 * treated it as "a string with a secret flag" would be one refactor away
 * from putting it there.
 */
export type BackendFieldKind =
  | "string"
  | "path"
  | "url"
  | "enum"
  | "bool"
  | "credential"
  | "key_prefix";

/**
 * The same seven kinds at runtime.
 *
 * It exists so a renderer's own test can iterate the set and fail when a
 * kind has no control, instead of a new kind rendering as nothing at all
 * on a screen nobody re-opened. A union alone cannot be enumerated, and a
 * second hand-written list beside it would be the drift this pair exists
 * to prevent — so the type is derived from nothing and the array is
 * checked against it by the compiler.
 */
export const BACKEND_FIELD_KINDS: readonly BackendFieldKind[] = [
  "string",
  "path",
  "url",
  "enum",
  "bool",
  "credential",
  "key_prefix"
];

/**
 * What a backend IS to this engine, as opposed to which rclone backend it
 * dials. Closed in the engine, for BackendFieldKind's reason.
 */
export type BackendRole = "object_store" | "local_volume" | "remote_filesystem";

/** One choice an `enum`-kind field offers: the value that is stored, and
 *  the words to render for it. */
export interface BackendEnumValue {
  value: string;
  label: string;
}

/**
 * One thing an operator is asked for when they configure an instance of a
 * backend.
 *
 * This is SHAPE and never a value. A `credential` field says a credential
 * is needed here and says nothing about what one is, which is what makes
 * the whole catalogue safe to hold in a browser at all.
 */
export interface BackendManifestField {
  /** The key this value is stored under, and deliberately the same
   *  spelling the configuration file uses for the same fact. It is what a
   *  create request keys its values by, so nothing here has to translate
   *  a label back into a field. */
  id: string;
  label: string;
  help?: string;
  kind: BackendFieldKind;
  required: boolean;
  /** The closed choice set, for `enum` fields and only for those. */
  values?: BackendEnumValue[];
  /** Anchored regular expression source, for `string` fields and only
   *  for those; safe to pass to `new RegExp`. */
  pattern?: string;
  /** What the engine resolves this field to when an instance leaves it
   *  empty.
   *
   *  Render it as "leave empty for X" and send NOTHING. It is not a
   *  default to pre-fill: a default written into the request is a default
   *  frozen into the operator's own file by the next settings save
   *  (issue #294), which is why the engine resolves it with an accessor
   *  rather than storing it. */
  unsetMeans?: string;
}

/** One step of the verification vocabulary and whether this backend runs
 *  it. `reason` is present exactly when `run` is false: a skipped step is
 *  a first-class outcome and not a quiet pass, so it has to say why in
 *  words an operator reads. */
export interface BackendProbeStep {
  step: string;
  run: boolean;
  reason?: string;
}

/**
 * One registered backend, as data.
 *
 * A destination is an INSTANCE of one of these. That is the whole of EPIC
 * I: several instances of one backend is the normal case rather than an
 * edge, so a manifest carries what a picker needs to offer the backend
 * and says nothing whatever about any particular destination.
 */
export interface BackendManifest {
  /** The backend's name, what a create request names, and the value the
   *  CLI's `medium add --type` takes. */
  id: string;
  label: string;
  summary: string;
  role: BackendRole;
  /** Whether an instance of this backend can be authored today.
   *
   *  False is a backend this build registers, describes and serves, and
   *  cannot yet store or dial: `sftp` (#731) is the first one. Render it
   *  and refuse it — somebody who came looking for it deserves the real
   *  shape and the reason rather than silence — and never submit it: the
   *  configure step has nowhere to save what it would collect, so
   *  offering it collects eight values and fails on the last screen.
   *  Tracked in #235. */
  configurable: boolean;
  fields: BackendManifestField[];
  probe: { steps: BackendProbeStep[] };
}

/** A storage shape the engine understands and no manifest declares, so
 *  no instance of one can exist.
 *
 *  Rendered, dimmed, rather than hidden. Somebody who came looking for
 *  SFTP learns nothing from a menu that never mentions it and asks again
 *  next month; a row saying the shape is understood and is not registered
 *  is a real answer. Nothing about it can be submitted. */
export interface UnregisteredBackend {
  /** How bytes would reach a destination of this shape - a protocol
   *  name such as `sftp`. A registered manifest reports no transport
   *  (#81 keeps implementation names off /api/v1, and `role` is the
   *  product answer); this one has to be named by something, and having
   *  no manifest is precisely what it means, so there is no id. */
  transport: string;
}

/**
 * Every backend an instance may be declared on, and the rules an instance
 * id follows.
 *
 * The rules travel with the catalogue rather than being restated in a
 * form, for RetentionSchema's stated reason: a form has to refuse exactly
 * what a hand-edited configuration file would be refused for, and a
 * client holding its own copy of the rule goes stale in one direction
 * only — silently accepting a name the engine then rejects.
 */
export interface BackendCatalog {
  registered: BackendManifest[];
  unregistered: UnregisteredBackend[];
  /** Anchored regular expression source; safe to pass to `new RegExp`. */
  instanceIdPattern: string;
  /** The one instance id no operator may choose. It names the drive this
   *  deployment's backups land on, written by the first-boot seed as
   *  instance zero of the local volume backend, and it is refused on
   *  every operator-facing path. A form that offers it and then meets the
   *  refusal is a worse experience than one that says so in the field. */
  reservedInstanceId: string;
}

/**
 * One step of a storage-medium preflight (issue #443).
 *
 * There is no field here for a credential and there is not going to be
 * (FR-33). `detail` is one of the engine's own sentences and never the
 * text of what actually came back, because that names a path on the host
 * or the name of an environment variable; the classified cause goes to the
 * manager's log instead.
 */
export interface MediumPreflightCheck {
  /**
   * What this step proves. `credentials` is whether the credential the
   * medium declares can be obtained at all, which is a question for the
   * host; `reach` is whether the endpoint answers and holds the bucket
   * with that credential, which is a question for the provider.
   *
   * Taken from the generated contract rather than spelled out again here
   * (issue #633, found while fixing the mock's candidate check). It was
   * spelled out again, and it had already gone stale: #622 gave the drive
   * on this machine a report of its own with a ninth step, `space`, which
   * the engine emits (mediumcheck.LocalSteps) and the contract carries,
   * and this copy never got it. So the one report shape this UI could not
   * describe was the one every deployment has, and the renderer's own doc
   * saying it draws "however many the engine sent" was making a promise
   * the types here could not keep. Consuming the wire union is the same
   * argument the error-code registry above makes, and
   * contract.conformance.test.ts holds both ends of it.
   */
  step: WireMediumPreflightCheck["step"];
  /** `skipped` is a real answer and not a quiet pass: an earlier step
   *  failed in a way that makes this one meaningless. Rendering a skipped
   *  write as anything but "this was never tried" tells an operator their
   *  bucket is writable on the strength of a credential nobody obtained.
   *
   *  From the contract for the same reason `step` above is: it sat five
   *  lines under the union that had already drifted, restated the same
   *  way, and the only thing keeping it honest was that nobody had added
   *  an outcome yet. */
  outcome: WireMediumPreflightCheck["outcome"];
  /** The transport category a failure classified as, absent when the step
   *  did not fail. Branch on this, never on `detail`. */
  category?: string;
  detail: string;
}

/**
 * The result of proving one storage medium works.
 *
 * A medium that does not work RESOLVES with `ok` false rather than
 * rejecting: a bucket that is not there is what an operator did, not what
 * broke, exactly as a failed connection test reports itself.
 */
export interface MediumPreflight {
  medium: string;
  ok: boolean;
  /** One entry per step, always the full list, so a surface renders a
   *  fixed set of rows rather than discovering which steps happened to
   *  run. */
  checks: MediumPreflightCheck[];
}

/**
 * One rung of the verification ladder (FR-31), with the backend's own
 * words for what it proves and what achieving it takes.
 *
 * Served rather than written here for the reason RetentionSchema is
 * served: a frontend that keeps its own copy of what "existence" proves
 * eventually tells an operator something the engine does not, and the
 * sentence somebody reads while deciding whether a backup is safe is the
 * worst place in the product for a stale paraphrase.
 */
export interface VerificationClassInfo {
  className: string;
  proves: string;
  /** What achieving this class requires, in words. Deliberately words,
   *  and deliberately not a field called "cost": the backend has no price
   *  list, so a number here would be invented, and a field named for one
   *  is one release away from holding one. */
  requires: string;
  /** True when achieving this class downloads the object's bytes, which
   *  the provider bills for. The same predicate the engine refuses
   *  automatic medium revalidation on, read rather than restated. */
  downloadsObject: boolean;
}

/** The vocabulary and the consent text a storage-medium mapping is written
 *  against. */
export interface StorageSchema {
  /** The ladder, strongest first. */
  verificationClasses: VerificationClassInfo[];
  /** The words an operator has to be shown before the first save that
   *  sends a tier's backups off local disk (FR-27). The backend refuses
   *  such a write without an acknowledgment and its refusal carries this
   *  same text, so what the form shows and what the server enforces cannot
   *  come apart. */
  mediumDisclosure: string;
  /** The plain statement about reading a copy back off a medium. It
   *  carries no figure, and never will. */
  retrievalDisclosure: string;
}

/** The FR-18/FR-19 policy as it is actually deciding. `tiers` is always
 *  the RESOLVED chain: a config file written with the legacy
 *  daily_days/weekly_months/monthly_months sugar reports the three tiers
 *  those keys stand for, so this UI renders one shape for one policy and
 *  never has to know the sugar exists. */
export interface RetentionSettings {
  timezone: string;
  weekStartsOn: string;
  tiers: RetentionTierSetting[];
  /** FR-19. Turning this off is what core/internal/retention calls a
   *  materially more dangerous configuration, and SettingsPage confirms
   *  it before the write. */
  protectLastKnownGood: boolean;
}

/**
 * Issue #333: one backup set's OWN retention policy, unresolved, exactly
 * as its configuration file carries it.
 *
 * Every field is optional and an omitted one INHERITS from the
 * deployment's resolved policy rather than falling back to a product
 * default. That is the whole difference between this type and
 * RetentionSettings above, which is always fully resolved: this one
 * answers "what does the file say", and a form that resolved it would
 * turn every inherited field into an explicit one the moment somebody
 * re-saved a policy they had not edited.
 *
 * A policy has to name the WHOLE chain. Half of one is refused by the
 * server, in the same words a hand-edited config.yaml is refused with,
 * because completing the missing half from the product defaults is how a
 * set silently ends up retaining less than the operator who wrote the
 * deployment's policy believes. Nothing in this UI ever builds a partial
 * one: the editor starts from a whole resolved chain and every edit is on
 * top of that.
 */
export interface RetentionOverride {
  timezone?: string;
  weekStartsOn?: string;
  /** FR-18's original three-scalar chain. All three or none. This UI
   *  never sends them (it edits the chain, exactly as the deployment's
   *  own retention form does), and the type carries them because the
   *  server round-trips a policy an operator wrote by hand. */
  dailyDays?: number;
  weeklyMonths?: number;
  monthlyMonths?: number;
  tiers?: RetentionTierSetting[];
  protectLastKnownGood?: boolean;
  /** The operator's acknowledgment of `schema.storage.mediumDisclosure`,
   *  on this write for the reason UpdateSettingsRequest carries it: a
   *  set's own chain can send a tier's backups off local disk exactly as
   *  the deployment's policy can, and the backend refuses the first such
   *  mapping with MEDIUM_DISCLOSURE_REQUIRED without it. A consent, not
   *  part of the policy: the backend never serves it back, and this UI
   *  sends it only on a save that introduces a mapping. */
  acknowledgeMediumDisclosure?: boolean;
}

/**
 * Which retention policy one backup set is retained under, and where that
 * policy came from (issue #333).
 *
 * `isOverride` is served rather than derived by comparing `effective`
 * against `deployment`: a set that deliberately pinned a chain identical
 * to the deployment's is NOT inheriting, and the whole point of pinning
 * it is that a later edit to the deployment's policy will not move it.
 *
 * `deployment` travels with every answer, including for a set that is
 * inheriting. It is what "this is what clearing would return you to"
 * means for a set that overrides, and it is the honest starting point for
 * a form about to create one, which is what stops a first submission
 * being half a policy.
 */
export interface BackupSetRetention {
  backupSetId: string;
  isOverride: boolean;
  /** The policy actually deciding for this set, resolved. */
  effective: RetentionSettings;
  /** The deployment's own policy, resolved, whether or not this set is
   *  currently retained under it. */
  deployment: RetentionSettings;
  /** The raw policy this set declared, or undefined when it inherits. */
  override?: RetentionOverride;
}

/**
 * The closed value sets and bounds the backend validates a retention
 * chain against, served alongside the values themselves.
 *
 * This is read from the server rather than hardcoded here on purpose: the
 * lists come from core/internal/config's own constants, so a granularity
 * added there reaches this form without a second copy in this file
 * silently going stale.
 */
export interface RetentionSchema {
  granularities: string[];
  /** Every granularity except the custom period, which can never measure
   *  a window (config.RetentionTier.windowUnit's own rule). */
  windowUnits: string[];
  /** Anchored regular expression source; safe to pass to `new RegExp`. */
  tierNamePattern: string;
  /** The one name a configured tier may not claim, because FR-19's
   *  protected term already occupies it. */
  reservedTierName: string;
  keepMax: number;
  periodDaysMax: number;
  /** The chain a configuration that spells neither the explicit tier list
   *  nor the legacy scalars resolves to, straight from
   *  core/internal/config.DefaultRetentionTiers.
   *
   *  Served rather than written into this UI because "restore the default
   *  chain" is not a display string: saving it writes an explicit tiers
   *  list, which clears the legacy scalars and permanently migrates a
   *  config that would have tracked the product's default onto a frozen
   *  copy of it. A stale copy here could therefore narrow a real retention
   *  window, silently and in the dangerous direction. */
  defaultTiers: RetentionTierSetting[];
}

/**
 * FR-21's capacity configuration as it is actually deciding (issue #286):
 * the operator's storage cap, the two levels a reading is weighed
 * against, the safety margin held back before every transfer, and the
 * filesystem all of that is measured on.
 *
 * Every number is BYTES. The MB/GB picker beside the field in the
 * Settings form is display only and converts at the edge, so nothing on
 * this boundary, and nothing in the config file underneath it, ever
 * carries a unit — a number whose meaning depends on a second field
 * getting out of step with it is exactly the kind of mistake a stray
 * factor of 1024 makes invisible in a diff.
 */
export interface CapacitySettings {
  /** The ceiling on how much space this manager may occupy.
   *
   * ZERO MEANS NO CAP, never a zero-byte ceiling: the sentinel this
   * product's default rests on, and nothing may resolve it to a number.
   * Enforced, not merely displayed — a transfer that would push usage
   * over this number is refused before it starts, the same way one the
   * disk cannot hold already is. */
  capBytes: number;
  /** The headroom level, measured against whichever of the disk's free
   *  space and the cap's remaining allowance is smaller, at or below
   *  which a reading is reported as a warning. 0 means no warning line.
   *  Never refuses a transfer by itself. */
  warningFreeBytes: number;
  /** The headroom floor at or below which a transfer is refused outright.
   *  0 means no critical line. Must not exceed warningFreeBytes: headroom
   *  is expected to cross the warning line before the critical one. */
  criticalFreeBytes: number;
  /** Held back on top of every incoming artifact's own size before a
   *  transfer is admitted. Not exposed on the Settings form; configured,
   *  if at all, by editing the config file directly. */
  safetyMarginBytes: number;
  /** The directory whose filesystem the manager-wide storage reading
   *  (GET /system/storage's `manager` object) is taken from, ALREADY
   *  RESOLVED: an operator's explicit choice when there is one, otherwise
   *  the directory every backup set's destination has in common. Empty
   *  means this configuration cannot say, which the dashboard renders as
   *  "not known yet" rather than as a blank path. */
  backupRoot: string;
  /** Whether backupRoot was named by an operator rather than derived. A
   *  form must never put a derived value in an editable box: saving it
   *  back would pin today's derivation into the file as an explicit
   *  choice, including on a later release that would have derived it
   *  better. */
  backupRootConfigured: boolean;
}

/** A PARTIAL capacity update: only the fields named here change. Every
 *  field is optional rather than a plain number because, on this block,
 *  ZERO IS A MEANING ("no cap", "no warning line", "no critical line"),
 *  not the absence of one — a plain number could not tell "set this to
 *  zero" apart from "I did not mention this field", and those are
 *  opposite requests. */
export interface UpdateCapacitySettings {
  capBytes?: number;
  warningFreeBytes?: number;
  criticalFreeBytes?: number;
  safetyMarginBytes?: number;
}

/** Everything GET /settings answers with: the current values, plus the
 *  SCHEMA describing what values are legal. The schema travels with the
 *  settings so a form can enforce the server's rules without holding a
 *  second copy of them, which is the rule the retention and capacity
 *  cards are both written to. */
export interface AppSettings {
  retention: RetentionSettings;
  capacity: CapacitySettings;
  /** Every storage medium the configuration declares, in declaration
   *  order. Empty for every deployment that has configured none, which is
   *  the case the Medium column and the medium picker both disappear
   *  for. */
  mediums: StorageMedium[];
  schema: { retention: RetentionSchema; storage: StorageSchema };
}

/** A PARTIAL update: only the fields named here change, everything else
 *  keeps whatever the config file currently says. Omitting `tiers`
 *  deliberately leaves the chain (and a legacy file's own spelling of it)
 *  untouched, which is what lets a caller flip one toggle without
 *  rewriting a policy it never edited. */
export interface UpdateRetentionSettings {
  timezone?: string;
  weekStartsOn?: string;
  tiers?: RetentionTierSetting[];
  protectLastKnownGood?: boolean;
}

/** A sparse settings write: only the blocks named here are touched, and
 *  within them only the fields named. Every layer of that is deliberate,
 *  because a caller editing one toggle must not rewrite a retention chain
 *  it never read. */
export interface UpdateSettingsRequest {
  retention?: UpdateRetentionSettings;
  capacity?: UpdateCapacitySettings;
  /** The operator's acknowledgment of `schema.storage.mediumDisclosure`,
   *  required by the backend on a write that first sends a tier's backups
   *  to a non-local medium (FR-27).
   *
   *  Sending it is not what makes the write safe; the backend decides
   *  whether it is needed and refuses the write with
   *  MEDIUM_DISCLOSURE_REQUIRED when it is missing. Disabling a Save
   *  button is a courtesy to the operator, never the gate. */
  acknowledgeMediumDisclosure?: boolean;
}

/** Which question a storage gauge is a fraction OF (issue #286): the
 *  whole disk, or an operator's configured cap. The two are
 *  indistinguishable once reduced to a percentage — 80% of a 2 TB volume
 *  and 80% of a 100 GB allowance draw the same bar — so this is carried
 *  rather than inferred at the display layer. */
export type StorageDenominator = "disk" | "cap";

/** Why a manager-wide storage reading could not be taken. `""` is not a
 *  real reason; it is what a `known: true` reading carries in that slot.
 *  See ManagerStorage.known. */
export type StorageUnknownReason = "" | "no_backup_root" | "not_created" | "unreadable" | "misconfigured";

/**
 * GET /api/v1/system/storage's manager-wide reading (issue #286): what
 * the backup root's filesystem holds, what this manager itself accounts
 * for, and which of the two a gauge should be a fraction of.
 *
 * `known` is false whenever no reading could be taken — no backup root
 * yet, one that has not been created, one that could not be read, or a
 * capacity configuration that cannot produce a verdict — and every
 * numeric field is then 0. That 0 must never be rendered as a
 * measurement: this is the type StorageGauge's own unknown-state branch
 * exists for, and the shape "0 B of 0 B used · NaN%" reached a screen on
 * an unconfigured instance is exactly the defect issue #286 was opened
 * on.
 */
export interface ManagerStorage {
  known: boolean;
  unknownReason: StorageUnknownReason;
  /** The directory actually statted, present whether or not the reading
   *  succeeded. The engine runs in a container: this is what lets an
   *  operator confirm the reading is of the bind-mounted backup volume
   *  and not of the container's own root filesystem. */
  measuredPath: string;

  totalBytes: number;
  /** Every free block, including any only a privileged process could
   *  allocate into. Observability only. */
  freeBytes: number;
  /** Free space this process may actually use (df's Avail); the figure
   *  every verdict below is decided from. */
  availableBytes: number;

  /** This manager's own consumption, summed from the state database's
   *  own record of artifact sizes rather than walked off the backup
   *  root, so it counts only what this manager put there. */
  catalogBytes: number;
  /** False means the catalog could not be summed, which is a different
   *  thing from a genuine zero on a deployment that has transferred
   *  nothing yet. */
  catalogBytesKnown: boolean;
  /** How much of what the filesystem reports as used this manager does
   *  NOT account for. A large gap means something else is writing into
   *  the backup root. */
  otherBytes: number;
  otherBytesKnown: boolean;

  /** The configured ceiling; 0 means none. */
  capBytes: number;
  /** What limitBytes/usedBytes below answer a fraction OF. */
  denominator: StorageDenominator;
  limitBytes: number;
  usedBytes: number;

  /** How much room is left before the binding constraint refuses a
   *  transfer: the smaller of availableBytes and the cap's remaining
   *  allowance. */
  headroomBytes: number;
  /** Which of the two actually produced headroomBytes. A genuinely
   *  different fact from `denominator`: a capped deployment whose volume
   *  is nearly full is bound by the disk, which an operator watching
   *  their allowance fill has no reason to expect. `""` when `known` is
   *  false. */
  bindingConstraint: "" | StorageDenominator;

  warningFreeBytes: number;
  criticalFreeBytes: number;
  /** `""` when `known` is false: an unread disk is not OK. */
  level: "" | "OK" | "WARNING" | "CRITICAL";
}

/** GET /api/v1/system/first-run's answer (issue #176). `configured` is
 *  false on an instance that is listening with no config.yaml on disk at
 *  all: it serves the setup flow, not the application, and every backup,
 *  retention, quarantine and settings route refuses with NOT_CONFIGURED
 *  until setup completes. */
export interface FirstRunStatus {
  configured: boolean;
}

/** POST /api/v1/system/first-run's answer. It is `CreatedBackupSet` plus
 *  the one thing only a first run can report: the configuration is
 *  durably written, but this process could not open a service against it
 *  in place, so it needs restarting before it serves the application.
 *  That is deliberately not modelled as an error — the setup itself
 *  succeeded. */
export interface FirstRunResult {
  backupSet: CreatedBackupSet;
  restartRequired: boolean;
}

/** The outcome of {@link BackupdApi.reinstate}. */
export interface ArtifactReinstatement {
  /** Whether the backup was actually returned to a trusted state. */
  reinstated: boolean;
  /** Whether there was anything to check at all. False is never a pass. */
  checked: boolean;
  /** The verdict of the checks that ran. */
  passed: boolean;
  /** The lifecycle state the backup was returned to, empty when nothing moved. */
  state: string;
  /** What was checked and what it found, already a sentence. */
  reason: string;
}

/** Which slice of the durable activity feed to read (issue #730). */
export interface ActivityQuery {
  /** How many events to ask for. The service has a default and a
   *  maximum of its own, so this is a request rather than a promise; a
   *  caller that sends nothing gets the default. */
  limit?: number;
  /** A `nextCursor` from an earlier page, which asks for the events
   *  OLDER than that page ended at. Opaque: it is the service's own
   *  ordering key and nothing here may parse it. */
  before?: string;
}

/** One page of {@link BackupdApi.listActivity}. */
export interface ActivityFeedPage {
  /** The page itself, newest first. */
  events: ActivityEvent[];
  /** Where to continue from, absent when this page reached the end of
   *  the record. Absent is what a surface must test to decide whether to
   *  offer "load older" at all: offering it for a record that has ended
   *  is a control that does nothing. */
  nextCursor?: string;
}

/**
 * Everything this frontend can ask a backend to do.
 *
 * Two implementations satisfy it and both are real: httpApi talks to a
 * service, createMockApi answers from memory. Pages depend on this
 * interface and on neither of them, which is what makes the whole UI
 * runnable without a backend and keeps a page from reaching for a field
 * nothing promised.
 *
 * The method docs below carry what a signature cannot: which calls an
 * unconfigured instance refuses, which ones the service will reject
 * without an accompanying acknowledgement, and which answers are echoes of
 * a write rather than fresh reads. Those are the things a caller gets
 * wrong, and none of them are visible in the types.
 */
export interface BackupdApi {
  getVersion(): Promise<VersionInfo>;
  getHealth(): Promise<SystemHealth>;

  /** Issue #176: which mode this instance is in. Read before anything
   *  else, because on an unconfigured instance every other call below
   *  refuses. */
  getFirstRunStatus(): Promise<FirstRunStatus>;
  /** Issue #176: writes this deployment's FIRST configuration. Same
   *  request body as createBackupSet, because the operator answers the
   *  same questions in the same wizard; what differs is that there is no
   *  configuration to fold it into yet. */
  completeFirstRun(req: CreateBackupSetRequest): Promise<FirstRunResult>;

  listSets(): Promise<BackupSet[]>;
  getSet(id: string): Promise<BackupSet>;
  /**
   * Submits a run cycle: one pass over every enabled backup set.
   *
   * It takes no backup set id, because a run cycle is deployment-wide in
   * core (internal/app's RunCycle walks every enabled set), which is why
   * the durable operation record it produces carries none either. That
   * used to read "because there is no such operation", which was true of
   * the HTTP surface and never of the engine: runBackupSet below is the
   * operation, on this same route with its own action. The shared UI also
   * used to call `POST /backup-sets/{id}/run`, which no runtime has ever
   * served.
   *
   * configRevision is the revision the CALLER is currently displaying,
   * not one read fresh at submit time. That is the whole point of the
   * token: a screen that has been open while somebody edited the
   * configuration is refused (CONFIG_REVISION_STALE) instead of running
   * against a setup nobody looking at it has seen.
   */
  /**
   * `idempotencyKey` is one key per LOGICAL submission, reused on every
   * retry of that submission and never regenerated per attempt.
   *
   * That is the entire point of the header, and getting it backwards is
   * worse than omitting it: a client that minted a fresh key per attempt
   * would turn one dropped response into two backup runs, and the service
   * would have no way to know the two requests were the same intent. The
   * key belongs to whoever knows what a retry is, which is the caller,
   * not this client (useRunControls owns that lifetime today).
   *
   * It is a required parameter rather than a defaulted one because the
   * route refuses without it: for every build this project has shipped,
   * `post` had no way to send a header at all, so every run submitted
   * from a browser was answered 400 by a handler that never saw the
   * body (issue #597).
   */
  runCycle(configRevision: string, idempotencyKey: string): Promise<void>;
  /**
   * Submits a run of exactly ONE backup set: the same reconcile, discover
   * and walk `runCycle` performs for every enabled set, narrowed to this
   * one (issue #597, EPIC G's G1.4).
   *
   * `backupSetId` is the full "source/backup-set" id, which is the id
   * every surface in this product prints and the one `backupd
   * fetch --backup-set` has taken since #569.
   *
   * It shares runCycle's route, gate and single-flight lock, so a per-set
   * run and a deployment-wide one cannot overlap and the loser is refused
   * with OPERATION_ALREADY_RUNNING rather than queued. Backing up
   * whatever is on disk now is not made more correct by having been asked
   * for twice.
   */
  runBackupSet(backupSetId: string, configRevision: string, idempotencyKey: string): Promise<void>;
  /**
   * Asks for one archived copy of one backup to be made readable again
   * (EPIC E, FR-34).
   *
   * `acknowledged` is a required true rather than a defaulted one, and
   * that is the whole mechanism behind "make an accidental restore hard":
   * a caller that forgot to ask a human gets a refusal, not a bill,
   * because the value that costs nothing is the one you get by leaving
   * the field alone. Compare a `force` flag, where the forgetful caller
   * is the one who spends the money.
   *
   * `configRevision` is the revision the CALLER is displaying, for the
   * reason runCycle's own doc gives, plus one that is sharper here: this
   * request names a medium by id, and a configuration that moved while
   * the screen was open may have repointed that id at a different bucket.
   *
   * What comes back says how long the storage class publishes a restore
   * as taking and that a bill exists. It never says a percentage, a
   * finishing time or an amount, and there is nowhere in the type to put
   * one: S3 reports a restore as running or finished and nothing else,
   * and this product holds no price list.
   */
  restoreCopy(req: RestoreCopyRequest): Promise<RestoreSubmission>;
  /** Re-checks an ALREADY persisted backup set's connection, by id. The
   *  connection details come from the configuration, so nothing about the
   *  key or the trusted host line travels from here. */
  testConnection(id: string): Promise<ConnectionTestOutcome>;
  /**
   * Turns one backup set on or off.
   *
   * `source`/`set` are BackupSet's own two-part identity, the same pair
   * previewRetention and applyRetention take, because the route keys on
   * exactly those two path segments rather than on the flat `id`.
   */
  setEnabled(source: string, set: string, enabled: boolean): Promise<void>;
  /**
   * Declares, or withdraws, one backup set's read-only status (issue
   * #282, #316), through the API/detail-page control rather than by
   * hand-editing config.yaml. Turning it on only prevents a FUTURE
   * deletion; turning it back off does not retroactively authorise
   * deleting anything this manager already retained under it.
   *
   * `source`/`set` are BackupSet's own two-part identity, the same pair
   * setEnabled above takes.
   */
  setReadOnly(source: string, set: string, readOnly: boolean): Promise<void>;

  /**
   * Issue #350: changes one already-persisted backup set. Sparse, and
   * that is the contract rather than a convenience: a key this patch
   * omits is left exactly as it is, which is what lets the detail page's
   * per-box Save persist only the box it belongs to. A Save that wrote
   * every field would be lying about its scope, and an operator who
   * changed two boxes and saved one would silently ship both.
   *
   * It resolves to the whole backup set as it now stands, so a caller can
   * put the persisted truth back on the graph rather than the value it
   * hoped it had written.
   *
   * `source`/`set` are BackupSet's own two-part identity, the same pair
   * setEnabled and setReadOnly take.
   */
  updateBackupSet(source: string, set: string, patch: BackupSetPatch): Promise<BackupSet>;

  /**
   * Issue #391: removes one backup set's configuration, so nothing is
   * collected for it from here on.
   *
   * Configuration only. Every backup the set already took stays on
   * storage and stays listed under Backups, which is what the
   * confirmation the operator accepted promises, and this call is why
   * that confirmation now means something: it used to close the dialog
   * and call nothing at all.
   *
   * Not reversible as a call. The undo is creating a set with the same
   * source and name again, which re-adopts every artifact the removed one
   * produced, because an artifact is identified by source/set/name rather
   * than by a surrogate key.
   *
   * It resolves to nothing, because there is nothing left to resolve to.
   * A caller showing the removed set has to navigate away rather than
   * re-read it: the next `getSet` for this id is a 404.
   *
   * `source`/`set` are BackupSet's own two-part identity, the same pair
   * setEnabled, setReadOnly and updateBackupSet take.
   */
  removeSet(source: string, set: string): Promise<void>;

  /**
   * Issue #350's edit hold. A backup set being edited while a cycle runs
   * against it is two writers on one definition, so entering edit mode
   * holds that one set: the pass currently running against it stops, and
   * the scheduler starts no new one until the hold is released or its
   * lease lapses.
   *
   * The read is separate from the take on purpose, and that split is what
   * makes declining possible: a caller reads first, shows the operator
   * what pressing Edit would interrupt, and only takes the hold once they
   * accept. `running` is null when nothing is in flight for this set,
   * which is what lets edit mode open with no prompt at all.
   */
  getEditHold(source: string, set: string): Promise<EditHoldState>;
  /** Takes the hold, or renews one already held (the same call: see the
   *  route's own doc for why a late heartbeat must not be refused).
   *  Resolves with what it interrupted, or null when nothing was
   *  running, so a caller never claims to have stopped something. */
  takeEditHold(source: string, set: string): Promise<EditHoldTaken>;
  /** Leaves edit mode. Every route out of edit mode calls it, and
   *  releasing a hold that is not held is a success. */
  releaseEditHold(source: string, set: string): Promise<void>;

  /** Issue #146 (B2.7): the wizard's three Save buttons. */
  createBackupSet(req: CreateBackupSetRequest): Promise<CreatedBackupSet>;
  /** Issue #162: the registered application-validator catalog, read by
   *  the wizard's step 5 picklist. Read-only — there is no route that
   *  adds to it, by design. */
  listValidators(): Promise<ValidatorCatalogEntry[]>;
  /** The wizard's "Import key" step (#98 step 2). Sent once; the
   *  caller discards its own copy of privateKeyPem the instant this
   *  resolves, per that step's own on-screen copy. */
  importSSHKey(privateKeyPem: string): Promise<SSHKeyImportResult>;
  /** Issue #592: every key in this deployment's store. The read that
   *  makes `sshKeyId` a value an operator can actually choose, instead of
   *  one they had to write down at import time months ago. */
  listSSHKeys(): Promise<SSHKeyListing[]>;
  /** Issue #592: private keys the engine can see, and every location it
   *  looked in. Both halves, always: the locations are what make an empty
   *  list mean something. */
  listSSHKeyCandidates(): Promise<SSHKeyDiscovery>;
  /** Issue #592: import a key this machine already holds, by the opaque
   *  id `listSSHKeyCandidates` gave it. No key material crosses the
   *  network in either direction, and the original file is left alone.
   *  Its own operation rather than a mode of `importSSHKey`, because
   *  pasting material this host has never seen and choosing a key it
   *  already found are different acts; both answer with the same
   *  `SSHKeyImportResult`. */
  importSSHKeyCandidate(candidateId: string): Promise<SSHKeyImportResult>;
  /** The wizard's "Verify server" step (#98 step 3): fetches a real
   *  fingerprint for host:port, trusting nothing yet. */
  probeHostKey(host: string, port: number): Promise<HostKeyProbeResult>;
  /** A pre-save reachability/auth check, run before createBackupSet —
   *  distinct from testConnection(id) above, which checks an ALREADY
   *  persisted set. */
  testCandidateConnection(params: ConnectionTestParams): Promise<ConnectionTestOutcome>;

  /**
   * Every backup this deployment holds, optionally narrowed to one backup
   * set by its two-part "source/set" id.
   *
   * A setId naming no configured backup set REJECTS with
   * BACKUP_SET_NOT_FOUND rather than resolving to an empty list. An empty
   * list has to keep meaning "this backup set exists and holds no backups
   * yet"; if it also meant "there is no such backup set", a bookmarked
   * filter that outlived a rename would read as "your backups are gone".
   */
  listArtifacts(setId?: string): Promise<BackupArtifact[]>;
  getArtifact(id: string): Promise<BackupArtifact>;

  listOperations(): Promise<Operation[]>;
  /**
   * One page of the durable lifecycle record, newest first (issue #730).
   *
   * A page rather than "the feed" because the record is append-only and
   * nothing prunes it: a deployment that has been running a year holds
   * more events than any screen can render, and a caller that asked for
   * all of them would make the page that opens on the worst deployment
   * the slowest one. Read the first page with a `limit`, then follow
   * `nextCursor` for what is behind it.
   */
  listActivity(query?: ActivityQuery): Promise<ActivityFeedPage>;

  /**
   * What every configured backup set is doing right now, plus a bounded
   * tail of the events behind it (issue #573).
   *
   * A different question from `listActivity` above and not a filter over
   * it. That one reads the durable lifecycle record and cannot know a
   * transfer is at 4 MB/s; this one reads what the serving process is
   * holding in memory and has forgotten last Tuesday.
   *
   * It is polled rather than streamed, and the service names the cadence
   * in `pollAfterMs` because the service is the one that knows whether
   * anything is moving. `since` is the highest sequence already seen, so
   * a repeat call carries back only what is new.
   *
   * Every configured set comes back whether or not anything has happened
   * to it: a panel that appears only during activity teaches an operator
   * to hunt for it, and its absence then means either nothing is running
   * or nothing is reporting, with no way to tell which.
   */
  getLiveActivity(options?: { setId?: string; since?: number; limit?: number }): Promise<LiveActivity>;
  listQuarantine(): Promise<BackupArtifact[]>;
  revalidate(artifactId: string): Promise<void>;
  retryIngestion(artifactId: string): Promise<void>;

  /**
   * Put one FAILED backup back into the pipeline so it is attempted again
   * (issue #419).
   *
   * FAILED is not quarantine and this is not `retryIngestion` with a wider
   * mouth. Quarantine means somebody has to decide whether a backup is
   * trustworthy; FAILED means an attempt did not finish, and until this
   * existed a backup that reached it stopped being worked on permanently,
   * because nothing in the product ever took either of the exits the
   * lifecycle graph declares for it.
   *
   * Nothing does this automatically and that is deliberate: a blind
   * re-transfer of gigabytes for a cause nothing has classified is a cost
   * the backend refuses to take on its own, so an operator asking IS the
   * eligibility rule.
   *
   * `note` is recorded alongside the transition so a later failure of the
   * same backup carries what was tried last time. Rejects with
   * ARTIFACT_NOT_FAILED when the backup is not stuck, which is what a
   * stale screen produces.
   */
  retryFailedIngestion(artifactId: string, note?: string): Promise<void>;

  /**
   * Re-check a quarantined backup's durable local copy and, when what is
   * found is enough, trust it again (issue #220).
   *
   * `retryIngestion` throws the local copy away and re-fetches from the
   * remote, which is the wrong answer twice over: when the local copy is
   * fine and the quarantine was the mistake, and when the remote source is
   * gone and there is nothing left to fetch. This is the other answer.
   *
   * It resolves rather than rejects when the checks do not pass, because
   * "your backup is bad" is a verdict about the backup and not a failed
   * request. Read `reinstated` for whether the backup moved and `reason`
   * for what was found; the two together are what an operator needs, and
   * either alone is misleading.
   *
   * A reinstated backup never releases its remote source afterwards. That
   * is permanent, and it is what makes the action safe to offer.
   */
  reinstate(artifactId: string): Promise<ArtifactReinstatement>;

  /**
   * Server computes and owns the plan. The UI may only apply it by id.
   * `source`/`set` are BackupSet's own two-part identity (core's
   * model.BackupSetID) — apps/common/webhost/router.go's
   * `/backup-sets/{source}/{set}/retention/...` routes key on exactly
   * these, not on BackupSet.id. applyRetention still takes `source`/`set`
   * to build the same URL, even though `planId` alone is what the backend
   * actually resolves the plan by (service.ApplyRetentionPlan's own doc).
   */
  previewRetention(source: string, set: string): Promise<RetentionPlan>;
  applyRetention(source: string, set: string, planId: string): Promise<RetentionPlan>;

  /**
   * Issue #333: one backup set's OWN retention policy, as three
   * operations on one sub-resource rather than as fields on the backup
   * set.
   *
   * `setBackupSetRetention` replaces the set's whole policy and never
   * merges with anything, and `clearBackupSetRetention` is the only way
   * to say "go back to inheriting the deployment's policy": that cannot
   * be a value on an update where an absent field already means "leave
   * this alone", since those are opposite requests.
   *
   * All three answer with the same shape, so a caller re-renders from
   * what the server says is now deciding rather than from its own
   * request.
   */
  getBackupSetRetention(source: string, set: string): Promise<BackupSetRetention>;
  setBackupSetRetention(
    source: string,
    set: string,
    policy: RetentionOverride
  ): Promise<BackupSetRetention>;
  clearBackupSetRetention(source: string, set: string): Promise<BackupSetRetention>;

  /** Issue #140 (B3.7): the settings surface. getSettings reads the
   *  policy in effect plus the schema it is validated against;
   *  updateSettings applies only the fields the request names and returns
   *  the settings that are now running, so a caller renders what was
   *  actually persisted rather than echoing its own request back. */
  getSettings(): Promise<AppSettings>;
  updateSettings(req: UpdateSettingsRequest): Promise<AppSettings>;

  /**
   * Prove one declared storage medium actually works, before a cycle
   * carrying a real backup finds out for the operator (issue #443).
   *
   * This is the medium's equivalent of testCandidateConnection, and
   * deliberately a stronger check: it writes a probe object, reads it back
   * byte for byte, compares the storage class it landed in against the one
   * the configuration claims, asks whether the verification class the
   * medium declares can actually be achieved there, and deletes the probe.
   * A wrong region, a policy that denies PutObject and an endpoint that
   * silently ignores storage_class all answer a reachability ping
   * perfectly well and then fail a move, in the middle of a cycle, after a
   * backup has already been chosen to leave local disk.
   *
   * It takes an ID and never a candidate, unlike testCandidateConnection:
   * a medium is declared in the configuration file and nowhere else, and
   * the only fields that would make a candidate one meaningful are its
   * three credential references, so a request body for one would make a
   * path on the host into something this UI sends.
   *
   * It resolves rather than rejects when the medium does not work. Read
   * `ok` for the verdict and `checks` for which step failed and how; a
   * caller that only rendered "failed" has dropped the only part an
   * operator can act on. Rejects with MEDIUM_NOT_FOUND for an id the
   * configuration does not declare.
   */
  preflightStorageMedium(mediumId: string): Promise<MediumPreflight>;

  /**
   * G2.2 (issue #594): the storage-destination surface a wizard drives.
   *
   * importStorageCredentials is the only call in this whole client that
   * ever holds an S3 secret. It sends the material once and answers with
   * an opaque id; nothing here reads it back, and the caller discards its
   * own copy the moment the id arrives.
   *
   * preflightStorageMediumCandidate is why that id exists in this shape.
   * preflightStorageMedium above can only check a destination already
   * written into the operator's configuration, so without this the only
   * way to find out whether a destination works is to save it first. The
   * candidate carries `credentialsId`, which names no path and no
   * variable on the manager's host, so the check is possible with nothing
   * host-shaped on a request body. It writes nothing whatever the report
   * says, and resolves rather than rejects when the destination does not
   * work, exactly as the by-id preflight does.
   *
   * removeStorageMedium rejects with MEDIUM_IN_USE while any copy names
   * the destination (FR-30). That is not a nicety: un-declaring it would
   * leave this deployment with no bucket, no endpoint and no credential
   * to reach those copies with, so they would read as unreachable and no
   * prune could run against them. getStorageMediumUsage is what a caller
   * renders instead of the bare refusal.
   */
  importStorageCredentials(accessKeyId: string, secretAccessKey: string, sessionToken?: string): Promise<string>;
  listStorageMediums(): Promise<StorageMedium[]>;
  getStorageMedium(mediumId: string): Promise<StorageMedium>;
  getStorageMediumUsage(mediumId: string): Promise<StorageMediumUsage>;
  preflightStorageMediumCandidate(spec: StorageMediumSpec): Promise<MediumPreflight>;
  createStorageMedium(spec: StorageMediumSpec): Promise<StorageMedium>;
  updateStorageMedium(mediumId: string, spec: StorageMediumSpec): Promise<StorageMedium>;
  removeStorageMedium(mediumId: string): Promise<void>;

  /**
   * EPIC I (#664): every backend a destination can be an instance of,
   * and the rules an instance id follows.
   *
   * The add-a-destination picker reads this and holds no list of its
   * own. A component with an array of backend names in it, or a switch
   * on backend type, is the assumption this epic exists to remove — that
   * a destination IS a backend type rather than an instance of one —
   * re-asserted in the one surface the epic is about.
   *
   * Read-only: there is no route that adds to it, by design. A manifest
   * decides what a destination may BE, including which rclone backend it
   * dials, so a client-extensible catalogue would put FR-4's gate on the
   * far side of the network from the binary it constrains.
   */
  listBackends(): Promise<BackendCatalog>;

  /**
   * Configure a destination in its backend's own vocabulary (issue
   * #669, EPIC I #664), and prove it first.
   *
   * A read and two writes, and the writes are separate for the reason
   * preflightStorageMediumCandidate exists beside createStorageMedium:
   * a check that can only run against what is already written makes
   * "declare it and find out" the supported flow, which is the ordering
   * FR-30 exists to prevent. The preflight writes nothing whatever it
   * answers and resolves with `ok` false when the destination does not
   * work - a bucket that is not there is what an operator configured,
   * not a request that broke.
   *
   * `configureStorageMedium` is create AND replace, which is what PUT
   * means and which P2 makes necessary rather than convenient: a
   * destination cannot exist unconfigured, because
   * `Registry.ValidateInstance` refuses an absent required field
   * (core/internal/backend/validate.go:228-233), `config.Validate`
   * delegates every per-field rule to it, and both bundled manifests
   * have required fields. So #668's confirm step names a backend and an
   * instance and writes nothing, and this is the single create, after
   * the check. `backend` on the request is required exactly then,
   * because there is no record to read it off yet.
   *
   * `getStorageMediumConfiguration` exists because these writes replace
   * the WHOLE declared field set: a form that started empty and saved
   * would unset every field the operator did not retype.
   *
   * The engine runs the same check in front of the write and refuses
   * with MEDIUM_CONNECTION_NOT_PROVEN when it fails (#636), so a
   * destination cannot become configured-and-unproven by way of this
   * path either - and a configuration naming no credential is checked
   * with the one already stored, which is what lets an operator change
   * a prefix on a destination whose access key they do not have.
   */
  getStorageMediumConfiguration(mediumId: string): Promise<StorageMediumConfigurationState>;
  preflightStorageMediumConfiguration(
    mediumId: string,
    config: StorageMediumConfiguration
  ): Promise<MediumPreflight>;
  configureStorageMedium(
    mediumId: string,
    config: StorageMediumConfiguration
  ): Promise<StorageMedium>;

  /** Make this the destination a NEWLY CREATED retention tier starts on
   *  (#622). It moves that and nothing else: no existing tier is
   *  rewritten and no backup is relocated, which is why it needs no
   *  acknowledgment in front of it. It answers with the destination that
   *  is now the default, so a caller re-renders from the answer rather
   *  than from its own optimistic guess. */
  setDefaultStorageMedium(mediumId: string): Promise<StorageMedium>;

  /** Issue #286: the one manager-wide storage reading. Deliberately not
   *  derived from anything else this client already fetches — see
   *  ManagerStorage's own doc for why summing the per-set list cannot
   *  answer this question (a fresh instance sums to zero, two sets
   *  sharing a volume sum to twice a disk that exists once, and a
   *  manager-wide cap has no per-set entry to live on). Requires
   *  configuration, exactly like getSettings. */
  getStorage(): Promise<ManagerStorage>;

  scanCatalog(): Promise<CatalogScanPreview>;
  rebuildCatalog(): Promise<void>;

  login(username: string, password: string): Promise<void>;
  enrollAdministrator(username: string, password: string): Promise<void>;
  /** apps/common/auth/local's POST /password (issue #128). Requires an
   *  already-authenticated session; rotates the stored password hash and
   *  revokes every other live session for this administrator. */
  rotatePassword(currentPassword: string, newPassword: string): Promise<void>;
  logout(): Promise<void>;
}
