import { API_ERROR_CODES as GENERATED_API_ERROR_CODES } from "./generated/contract";
import type { ApiErrorCode } from "./generated/contract";
import type { BackupArtifact, BackupSet, CompletionMethod, RetentionPlan } from "@shared/types/backup";
import type {
  ActivityEvent,
  Operation,
  SystemHealth,
  VersionInfo
} from "@shared/types/operation";

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

export interface ApiError {
  code: ApiErrorCode;
  /** Operator-facing sentence. Already human. */
  message: string;
  /** What to do next, if anything. */
  remediation?: string;
  correlationId: string;
}

export class BackupManagerError extends Error {
  constructor(readonly api: ApiError) {
    super(api.message);
    this.name = "BackupManagerError";
  }
}

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
 * (core/service/backupsetupdate.go's own package doc). It carries no key
 * reference and no trusted host line either: those are the results of the
 * wizard's import and probe steps, and re-trusting a host is a trust
 * decision rather than a field.
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
  completionMethod?: CompletionMethod;
  /** Only meaningful when the completion method in effect after this edit
   *  is "stable-size". */
  stableForSeconds?: number;
  staleAfterSeconds?: number;
  /** Confirms an edit that moves this set to different data. Needed only
   *  when `host`, `remoteFolder` or `destination` actually change on a
   *  set that already has artifacts on record; without it the service
   *  refuses with BACKUP_SET_REPOINT_NOT_ACKNOWLEDGED and writes nothing.
   *  It is not a property of the backup set: it answers one refusal, for
   *  one request. */
  acknowledgeRepoint?: boolean;
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

export interface SSHKeyImportResult {
  id: string;
  algorithm: string;
  fingerprint: string;
}

export interface HostKeyProbeResult {
  algorithm: string;
  fingerprint: string;
  knownHostsLine: string;
}

export interface ConnectionTestOutcome {
  ok: boolean;
  message?: string;
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
  /** The storage medium this tier's artifacts live on (EPIC E, FR-27),
   *  or undefined for the backup set's own local path. Carried in both
   *  directions rather than read-only: a retention write REPLACES the
   *  whole chain, so a field this type could not hold would be a field
   *  the next settings save deleted from the operator's file, and
   *  editing daily's keep would quietly move monthly's artifacts back
   *  onto local disk. */
  medium?: string;
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
 * Issue #333. One backup set's own retention policy, as it is written.
 *
 * `tiers` is the WHOLE chain and is required. The three legacy
 * daily_days/weekly_months/monthly_months scalars are deliberately not
 * writable per set: naming two of the three would be half a chain, and
 * half a chain one level down resolves its missing half to the PRODUCT
 * default rather than to the deployment's policy, which silently
 * shortens retention. Everything else is optional and inherits from the
 * deployment's resolved policy when omitted, because the timezone and
 * the week start decide how any chain is reckoned rather than what it
 * says.
 */
export interface BackupSetRetentionOverride {
  tiers: RetentionTierSetting[];
  /** Omit to inherit the deployment's. */
  timezone?: string;
  /** Omit to inherit the deployment's. */
  weekStartsOn?: string;
  /** Omit to inherit the deployment's posture. An explicit false widens
   *  what a later retention apply may delete, for this one set. */
  protectLastKnownGood?: boolean;
}

/**
 * Issue #333. What one backup set is retained under, and whether it says
 * so itself.
 *
 * `policy` is always the RESOLVED chain in force, so nothing here has to
 * redo inheritance. `isOverride` reads whether the operator wrote a
 * block on this set, never whether the two chains happen to match: a set
 * override and a deployment policy that agree want opposite advice ("edit
 * the set" versus "edit the deployment").
 */
export interface BackupSetRetention {
  backupSetId: string;
  isOverride: boolean;
  policy: RetentionSettings;
  /** What clearing the override would return this set to. Equal to
   *  `policy` exactly when `isOverride` is false. */
  deploymentPolicy: RetentionSettings;
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

export interface AppSettings {
  retention: RetentionSettings;
  capacity: CapacitySettings;
  schema: { retention: RetentionSchema };
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

export interface UpdateSettingsRequest {
  retention?: UpdateRetentionSettings;
  capacity?: UpdateCapacitySettings;
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

/** The outcome of {@link BackupManagerApi.reinstate}. */
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

export interface BackupManagerApi {
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
   * It takes no backup set id, because there is no such operation. A run
   * cycle is deployment-wide in core (internal/app's RunCycle walks every
   * enabled set), which is why the durable operation record it produces
   * carries no backup set id either. The shared UI used to call
   * `POST /backup-sets/{id}/run`, which no runtime has ever served.
   *
   * configRevision is the revision the CALLER is currently displaying,
   * not one read fresh at submit time. That is the whole point of the
   * token: a screen that has been open while somebody edited the
   * configuration is refused (CONFIG_REVISION_STALE) instead of running
   * against a setup nobody looking at it has seen.
   */
  runCycle(configRevision: string): Promise<void>;
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
  listActivity(): Promise<ActivityEvent[]>;
  listQuarantine(): Promise<BackupArtifact[]>;
  revalidate(artifactId: string): Promise<void>;
  retryIngestion(artifactId: string): Promise<void>;

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

  /** Issue #140 (B3.7): the settings surface. getSettings reads the
   *  policy in effect plus the schema it is validated against;
   *  updateSettings applies only the fields the request names and returns
   *  the settings that are now running, so a caller renders what was
   *  actually persisted rather than echoing its own request back. */
  getSettings(): Promise<AppSettings>;
  updateSettings(req: UpdateSettingsRequest): Promise<AppSettings>;

  /**
   * Issue #333: one backup set's own retention policy. Retention used to
   * be global only, so an operator wanting a database dump kept on a
   * different chain from a media share had to run a second deployment.
   *
   * All three reach the same service method the CLI's own
   * `backup-set retention show|set|clear` does, so the two surfaces
   * cannot answer differently. `setBackupSetRetention` REPLACES any
   * override already there rather than merging with it, and
   * `clearBackupSetRetention` on a set that has none is a success that
   * writes nothing.
   */
  getBackupSetRetention(source: string, set: string): Promise<BackupSetRetention>;
  setBackupSetRetention(
    source: string,
    set: string,
    override: BackupSetRetentionOverride
  ): Promise<BackupSetRetention>;
  clearBackupSetRetention(source: string, set: string): Promise<BackupSetRetention>;

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
