/**
 * What a backup set, a backup and a retention decision are, as this
 * frontend understands them.
 *
 * Almost every declaration here carries a note, and reading a few of them
 * shows what the file is actually for. It is not a transcription of the
 * wire: client.ts does that, and the generated bindings hold the wire's
 * own shapes. What these types add is the meaning an absence has, which no
 * generated schema can carry.
 *
 * That is the thread running through the file. An optional field here is
 * optional because absence says something a value could not, and the doc
 * beside it says what: no halt reason on record is not "reachable", a copy
 * with no verification class is not a weakly verified copy, a verdict with
 * no medium is not a verdict about local storage. Several of these fields
 * were required booleans once, filled in by the mapper with a literal
 * false, which turned "nobody said" into a claim and put it on screen.
 *
 * The other recurring decision is which vocabularies are closed. Health
 * states and completion methods are closed because the product defines
 * them; retention tiers are open because an operator defines them, and
 * anything narrowing an open vocabulary onto a closed one refuses rather
 * than guessing.
 */
import type { RetentionSettings } from "@shared/api/contracts";
import type { WireArtifact, WirePlacement } from "@shared/api/generated/contract";

/** The service's verdict on one backup set, or on all of them together.
 *  Four values rather than a boolean because "stale" and "failing" call
 *  for different actions: one set has not run recently enough, the other
 *  ran and did not work. */
export type HealthState = "healthy" | "degraded" | "stale" | "failing";

/** How this manager decides a remote file has finished being written.
 *  The order here is the order of assurance: the first two are signals a
 *  producer sends deliberately, and the last one is an inference from a
 *  file that stopped changing, which is why choosing it makes the UI say
 *  so and makes the stable-size window a required companion. */
export type CompletionMethod =
  | "atomic-rename"
  | "completion-marker"
  | "stable-size";

/** The CLOSED badge vocabulary used to describe a backup. The tier chain
 *  itself is open and operator-defined, so this is deliberately not the
 *  same thing as a tier name: a tier this build has never heard of maps to
 *  no class at all rather than being forced into one, and "protected" in
 *  particular is a promise that retention will never delete this backup. */
export type RetentionClass = "daily" | "weekly" | "monthly" | "protected";

/** Which checks a backup set runs. They stack rather than replace each
 *  other: the transfer arriving intact, the bytes matching a digest, and
 *  the artifact being something the application would accept are three
 *  separate claims, and a set can make any subset of them. */
export type ValidationKind = "transfer" | "checksum" | "application";

/**
 * Issue #299's rule, applied to the field that used to sit here.
 *
 * This type carried a full per-set `RetentionPolicy` (daily/weekly/
 * monthly, timezone, week start, protection) that NOTHING computed:
 * client.ts's own mapper filled it with zeros and "UTC" for every set, so
 * the backup set card and the detail page both drew "0 / 0 / 0" and
 * "0 kept" against real deployments. A field the UI draws and nothing
 * reads is exactly what #299 removed from the wizard, and it was still
 * here.
 *
 * #333 replaced it with the answer that is actually computed. A backup
 * set carries whether it is retained under its own policy or the
 * deployment's (below); the policy itself is served by
 * `getBackupSetRetention`, on demand, on the page that can show a whole
 * chain rather than three numbers that only fit one shape of chain.
 */

export interface BackupSet {
  id: string;
  /**
   * The two halves of core's own model.BackupSetID (core/internal/model/
   * ids.go): `source` names the configured remote source this set backs up
   * from, `set` names this particular backup set under that source.
   * apps/common/webhost's retention routes are the first to key a URL by
   * this composite shape directly (router.go: `/backup-sets/{source}/{set}/
   * retention/...`) rather than by `id` alone, so client.ts's
   * previewRetention/applyRetention take these two fields rather than
   * guessing how to split a flat `id` back into them.
   */
  source: string;
  set: string;
  name: string;
  host: string;
  port: number;
  username: string;
  remoteFolder: string;
  includePatterns: string[];
  excludePatterns: string[];
  completionMethod: CompletionMethod;
  /**
   * The window `stable-size` waits for before it treats a file as
   * finished, in seconds, and 0 for every other completion method.
   *
   * It is here (issue #350) because the inline editor offers the
   * completion method, and offering that method without its window would
   * be offering a Save that can only fail: core refuses a backup set
   * whose strategy is "stable" and whose window is zero, exactly as it
   * refuses one at creation.
   */
  stableForSeconds: number;
  destination: string;
  /** Whether this set declares its own retention policy rather than being
   *  retained under the deployment's (issue #333, config.BackupSet's own
   *  RetentionIsOverride). The chain itself is not here: see the note
   *  above this interface. */
  retentionIsOverride: boolean;
  validations: ValidationKind[];
  state: HealthState;
  /** Human sentence explaining the state. Never rely on colour alone. */
  stateNote: string;
  enabled: boolean;
  /**
   * Issue #282's read-only declaration, resolved (config.BackupSet.ReadOnly,
   * never the per-set-override/source-default split): pull backups from
   * this source, but never delete the remote original. Set through the
   * wizard or the "declare read-only" control on this set's own detail
   * page (issue #316), not only by hand-editing config.yaml.
   */
  readOnly: boolean;
  /**
   * How many backups in THIS set currently hold a remote source kept only
   * because `readOnly` above is true, not because any one of them was
   * individually reinstated out of quarantine (issue #227's own count,
   * which this type has no field for at all: see QuarantinePage.tsx's own
   * doc for why that population has never had a UI home). Comes from this
   * set's own entry in GET /system/health, the same join haltReason above
   * already depends on; 0 when that report could not be read for this set,
   * which is indistinguishable here from a genuine zero — the aggregate
   * across every set (SystemHealth.readOnlyRetainedCount) carries the
   * same caveat for the identical reason.
   */
  readOnlyRetainedCount: number;
  /**
   * Why the manager could not connect to this backup set the last time it
   * tried, so nothing was backed up. Absent when no refusal is on record
   * (#245).
   *
   * Absent is not "this set is reachable". It is "no refusal has been
   * observed", which a set that has never been cycled also produces, and
   * that asymmetry is the reason this is an optional reason rather than a
   * boolean. A required `halted: boolean` sat beside it until #231, and a
   * required field has to be filled in by every mapper: api/client.ts
   * filled it with a literal `false` on every set, which is a claim
   * rather than a gap, and it drove a per-card button's disabled state
   * and the word a card printed for its current activity. Two fields for
   * one concept could also disagree; this one carries it alone.
   *
   * All three values have a producer. api/client.ts reads them from GET
   * /system/health's per-set `halt_reason`, which core writes to a
   * durable per-backup-set record when a cycle's transport refuses the
   * connection and removes when a later cycle runs that set to
   * completion. A reason a newer service reports that this build does not
   * recognise maps to absent rather than through, so a banner never
   * renders for a word it cannot explain.
   *
   * `key-permissions` (#293) is the one of the three that never reaches
   * the host at all: the configured key's on-disk mode no longer matches
   * what it was imported with, caught before a connection is even
   * attempted. It is kept distinct from `authentication-failed` on
   * purpose, the same way that one is kept distinct from
   * `host-key-changed`: a rejected login is a question for the remote
   * account, a permission drift is a question for this filesystem, and
   * collapsing the two would put an operator on the wrong page.
   *
   * Nothing keyed on this may offer to resume the set. §77 invariant 5
   * makes re-trusting a changed host key an explicit administrator action
   * taken outside this manager, so these banners report and link; they
   * never dismiss, retry or re-trust.
   */
  haltReason?: "host-key-changed" | "authentication-failed" | "key-permissions";
  newestKnownGoodAt: string | null;
  lastRunAt: string | null;
  lastValidation: "passed" | "failed" | "not-run";
  expectedIntervalHours: number;
  retainedCount: number;
  retainedBytes: number;
  hostFingerprint: string;
  fingerprintTrustedAt: string | null;
}

/**
 * Whether one copy of a backup can be READ right now
 * (core/internal/placement.Access, FR-34), narrowed from the generated
 * wire union so this UI cannot invent a fifth value or misspell one of
 * the four.
 *
 * "unreachable" is the value this whole feature exists for: it means this
 * deployment cannot currently get to the place the copy is in, so it can
 * neither confirm the copy nor deny it. It is NOT "the copy is gone", and
 * a surface that renders the two the same way has told an operator
 * something false about the only thing they will care about.
 */
export type PlacementAccess = WirePlacement["access"];

/**
 * Which rung of the verification ladder a copy has ACHIEVED (FR-31),
 * or `null` when NOTHING has verified it.
 *
 * Null is the load-bearing value. The wire omits the field entirely for an
 * unverified copy, and this type keeps that distinguishable rather than
 * defaulting it to the weakest rung: "existence" is a claim that an object
 * was seen at the recorded size, and for a copy nobody has looked at, that
 * claim is simply untrue.
 */
export type VerificationClass = NonNullable<WirePlacement["verification_class"]>;

/**
 * One DURABLE copy of one backup, and where it actually is (FR-29).
 *
 * A value of this type exists because the backend recorded a finished
 * copy. An artifact with no copies has an EMPTY array, which is an
 * ordinary answer for one still transferring: the partial file on disk is
 * not a copy and deliberately has no entry here. So a surface reads the
 * three cases apart rather than collapsing them:
 *
 *   - `placements` empty            -> there is no copy anywhere yet
 *   - `access === "unreachable"`    -> there is a copy nobody can confirm
 *   - `verificationClass === null`  -> there is a copy nobody has checked
 */
export interface BackupPlacement {
  /** "local", or the id of a configured storage medium. */
  medium: string;
  /** What kind of place holds it, or "" when the configuration no longer
   *  describes the medium at all. Served rather than derived from
   *  `medium === "local"`, so this UI holds no copy of a reserved id. */
  mediumType: string;
  /** An absolute path for a local copy, an object key for a medium copy.
   *  Never a credential and never a signed URL. */
  location: string;
  /** What this copy measures, or null when nobody recorded it. Null is
   *  not zero: a backup can genuinely be empty. */
  sizeBytes: number | null;
  /** The medium's storage class, or "" for a local copy. */
  storageClass: string;
  /** The strongest class ACHIEVED, or null when nothing has verified this
   *  copy. See VerificationClass. */
  verificationClass: VerificationClass | null;
  /** When that class was last achieved, or null. Null exactly when
   *  verificationClass is. */
  verifiedAt: string | null;
  access: PlacementAccess;
  /** "ACTIVE", or "DELETE_PENDING" for a copy whose removal is recorded
   *  and may not have happened yet. A copy the backend knows is gone is
   *  not served at all, so there is no third value to render. */
  status: WirePlacement["status"];
}

/**
 * What decides when one backup is deleted (issue #523).
 *
 * "configured" and "none" are the wire's own two values, taken from the
 * generated type so this cannot drift from the contract. The third is
 * this UI's and is the reason the type is not just the wire union:
 *
 *   - "configured" -> a retention chain still selects this backup and
 *     will age it out in its own time;
 *   - "none"       -> the backup set's configuration was removed while
 *     the backup stayed on storage, so NO chain will ever select it,
 *     nothing will ever delete it, and it holds its space until somebody
 *     removes it by hand;
 *   - "unknown"    -> the response did not say.
 *
 * "unknown" exists so that a response which did not answer cannot be read
 * as an answer. The field is required on the wire, so this is the older
 * build talking, and the tempting resolution, treating a missing field as
 * "configured", is the one that is actively wrong: it would render the
 * backups nothing will ever delete as ordinary healthy rows, which is the
 * exact failure the field was added to end. So a surface renders
 * "unknown" as a question it cannot answer, never as reassurance.
 */
export type ArtifactRetentionPolicy = WireArtifact["retention_policy"] | "unknown";

/**
 * One backup: the file itself, where it came from, what has been proved
 * about it, and where its copies are.
 *
 * The timestamps are the spine and they are recorded rather than derived.
 * `producedAt` is when the remote signalled the file was finished,
 * `receivedAt` is when this manager had it, and `remoteSourceRemovedAt` is
 * the only one that can be null on an otherwise-healthy backup, because
 * the original outlives the copy until the copy is verified and durably
 * committed. A surface that computed any of these from another would be
 * able to draw a deletion that has not happened.
 */
export interface BackupArtifact {
  id: string;
  setId: string;
  setName: string;
  filename: string;
  remoteOriginalPath: string;
  localPath: string;
  producedAt: string;
  receivedAt: string;
  sizeBytes: number;
  checksum: string;
  /** Whatever algorithm produced `checksum`, reported rather than assumed.
   *  It is sha256 for everything core records today (internal/transport's
   *  own SHA256, and the operator-triggered validate refuses any other
   *  recorded algorithm outright), but a literal type here would mean the
   *  UI silently relabelled a hash it did not recognise. Empty when
   *  nothing has been hashed. */
  checksumAlgorithm: string;
  validation: "verified" | "failed" | "pending";
  retentionClasses: RetentionClass[];
  /** Remote deletion is a lifecycle FACT, never a user action. */
  remoteSourceRemovedAt: string | null;
  quarantine: QuarantineRecord | null;
  /** Every durable copy this backup currently has. Empty means there is
   *  no copy anywhere yet; see BackupPlacement. `localPath` above is the
   *  path ingestion landed on and is not evidence that a readable file is
   *  sitting there. */
  placements: BackupPlacement[];
  /** What will eventually delete this backup, or that nothing will.
   *  Required, and deliberately not optional: an absent field would be a
   *  fourth state every surface would have to decide about on its own,
   *  and "unknown" is already that decision, made once. See
   *  ArtifactRetentionPolicy. */
  retentionPolicy: ArtifactRetentionPolicy;
}

/** The closed bucket a quarantined backup falls into, derived from the
 *  backend's own diagnostic sentence for badging and filtering. The
 *  sentence itself is kept beside it (QuarantineRecord.detail): the bucket
 *  is what a list can group by, and the sentence is what actually tells an
 *  operator what was found. */
export type QuarantineReason =
  | "checksum-mismatch"
  | "validation-failed"
  | "unexpected-artifact"
  | "remote-identity-changed"
  | "incomplete-transfer";

/** Why one backup was set aside, in both forms: the category a surface
 *  can badge, and the words the backend recorded. Present only on an
 *  artifact that is actually quarantined, so its absence is the ordinary
 *  case rather than a missing field. */
export interface QuarantineRecord {
  reason: QuarantineReason;
  /**
   * The literal diagnostic sentence the backend recorded at the moment
   * this backup was quarantined (core/service.Artifact.QuarantineReason,
   * wire field quarantine_reason), verbatim. `reason` above is a closed
   * category derived FROM this text for badging/filtering (see
   * client.ts's quarantineReasonFor); this is the actual words, for an
   * operator who needs to know exactly what was found, not just which
   * bucket it fell into (issue #308). Empty when the backend has none to
   * report, which the UI renders as absent rather than as a blank line.
   */
  detail: string;
  detectedAt: string;
  /** Quarantined artifacts never trigger remote deletion. */
  remoteSourceRetained: true;
}

/**
 * core/service.RetentionArtifactVerdict (core/service/retention.go),
 * translated by client.ts from apps/common/webhost's snake_case wire shape
 * (handlers_retention.go's retentionVerdictResponse). "KEEP", "DELETE" or
 * "REFUSE" — internal/retention.PruneAction's own three values (FR-20):
 * REFUSE is a third, deliberate outcome distinct from KEEP, not an error —
 * an artifact policy did not select AND that fails a safety check.
 */
export type RetentionVerdictAction = "KEEP" | "DELETE" | "REFUSE";

/**
 * Which of FR-18's two placements selected an artifact for one tier
 * (internal/retention.GFSSelectedBy, via the wire's `selected_by`).
 *
 * FR-18 places every artifact twice, once by the timestamp this manager
 * discovered it and once by the producer's own timestamp on the remote
 * object, and KEEP is the union of the two passes. FR-8 requires the
 * second to be treated as untrusted, so "DISCOVERY" and "PRODUCER" are
 * materially different answers to "why is this being kept" and the
 * confirm-before-delete dialog has to be able to tell them apart.
 *
 * "PROTECTION" is not a placement: it is FR-19's last-known-good term,
 * which is not a bucket selection at all and so names neither timestamp.
 */
export type RetentionTierPlacement = "DISCOVERY" | "PRODUCER" | "BOTH" | "PROTECTION";

/**
 * One tier's claim on an artifact, paired with the placement that made it
 * (internal/retention.GFSTierSelection).
 *
 * The pairing is per tier and not per verdict on purpose: a single
 * artifact can be selected by DAILY through one placement and by MONTHLY
 * through the other, so a single attribution on the verdict would be
 * wrong in exactly the case an operator opened this dialog to understand.
 */
export interface RetentionTierSelection {
  /**
   * The tier's name. The value set is OPEN, not the three legacy names.
   * FR-18's retention policy is a chain of operator-defined tiers
   * (core/internal/config's Retention.Tiers), and this is one tier's
   * configured `name` upper-cased, so "SEMI_ANNUAL", "ANNUAL" or
   * "FORTNIGHTLY" are all ordinary values, as is "LAST_KNOWN_GOOD"
   * (internal/retention.TierLastKnownGood) when FR-19's protection is
   * what kept the artifact. Config constrains a name to ^[a-z][a-z0-9_]*$,
   * so the string is bounded, but nothing in this UI may treat an
   * unrecognised tier as "no tier": see RetentionTierBadges, which badges
   * an unknown tier under its own name rather than dropping it.
   */
  tier: string;
  selectedBy: RetentionTierPlacement;
}

/** What retention decided about one backup, and why. Every verdict in a
 *  plan is shown, keep and delete alike, because a plan is confirmed as a
 *  whole and an operator noticing that the wrong backup is on the delete
 *  side is the entire point of showing it before it runs. */
export interface RetentionVerdict {
  /** The artifact's filename within its backup set, not an opaque id
   *  (service.RetentionArtifactVerdict.Artifact is v.Artifact.Name). */
  artifact: string;
  action: RetentionVerdictAction;
  reason: string;
  /**
   * Where the copy this verdict is about lives, when that is a configured
   * storage medium (EPIC E FR-30, issue #430). The wire's `medium`,
   * carried through exactly as it arrives, undefined included.
   *
   * UNDEFINED MEANS LOCAL, with one documented exception, and the
   * exception is why this is not resolved to "local" here. The two REFUSE
   * shapes that establish nothing at all, a location the journal records
   * twice and an artifact whose local path could not be resolved, also
   * arrive undefined, and defaulting them would put a place on a verdict
   * that deliberately names none.
   *
   * A DELETE always establishes its medium, so for the question FR-30
   * asks, "where would this deletion happen", `medium ?? "local"` is the
   * whole answer and is safe to render. "Delete 40 backups" means
   * something very different when half of them are objects in a bucket
   * somebody else pays for.
   */
  medium?: string;
  /**
   * Populated only for a KEEP verdict: which retention tier(s) selected
   * it, and which placement selected it for each. Empty for
   * DELETE/REFUSE.
   *
   * This is the wire's `tier_selections` rather than its `tiers`. The
   * wire carries both, because a client that only wants the names should
   * not have to walk objects for them; this UI always wants the
   * placement, so keeping the bare list too would be a second copy
   * nothing here reads.
   */
  tiers: RetentionTierSelection[];
}

/**
 * One backup a retention plan would relocate, and both ends of the move
 * (EPIC E FR-27, issue #430; service.RetentionMove, via the wire's
 * `moves`).
 *
 * A move is a statement about PLACEMENT and nothing else. Planning one
 * never adds a backup to the keep set and never removes one, which is why
 * these travel beside the verdicts rather than inside them, and why a
 * dialog that renders both must not present a move as a third kind of
 * verdict.
 *
 * Both mediums are spelled out, "local" included, unlike
 * RetentionVerdict.medium above. A verdict answers "where would this
 * happen", which has an implicit default; a move answers "from where to
 * where", which has none.
 */
export interface RetentionMove {
  /** The backup's own filename, scoped to the plan's backup set, exactly
   *  as RetentionVerdict.artifact is. */
  artifact: string;
  /** Where the one confirmed copy is today. */
  fromMedium: string;
  /** The medium the first tier that selects this backup names as its
   *  home. Always different from fromMedium: a backup already at home is
   *  not a move. */
  toMedium: string;
}

/**
 * docs/EPIC-B-multi-nas.md §15.6's own preview/apply response shape
 * (apps/common/webhost's retentionPlanResponse, translated to camelCase by
 * client.ts). GET .../retention/preview and POST .../retention/apply both
 * return exactly this shape — a caller never has to reconcile "what would
 * happen" against a differently-shaped "what happened".
 *
 * There is deliberately no `stale` field here: whether this plan is stale
 * is derived client-side (state/appNodes.ts's retentionPlanStaleNode) by
 * comparing inventoryRevision/configRevision against what the graph has
 * itself most recently observed, not trusted as a boolean the wire hands
 * over (issue #96's own "causl-ts for staleness, not a boolean parsed off
 * the response").
 */
export interface RetentionPlan {
  /** Server-issued, immutable, single-use. The UI applies exactly this
   *  plan_id or nothing (§17). */
  planId: string;
  backupSetId: string;
  inventoryRevision: string;
  configRevision: string;
  /** RFC3339Nano. After this instant ApplyRetentionPlan always answers
   *  RETENTION_PLAN_STALE, even if nothing else changed. */
  expiresAt: string;
  keepCount: number;
  deleteCount: number;
  reclaimBytes: number;
  /** The durable operation this apply was recorded under. Empty on a plan
   *  a preview returned — a preview creates no operation. */
  operationId?: string;
  verdicts: RetentionVerdict[];

  /**
   * Every backup this plan would relocate, in verdict order (EPIC E
   * FR-27, issue #430).
   *
   * ALWAYS an array. The wire omits the field entirely for a deployment
   * that declares no storage medium, and this normalises that to [] for
   * the reason `tiers` is normalised: an optional array has three
   * readings and only two of them are ever true, so nothing downstream
   * should have to tell "no moves" from "the server did not say".
   */
  moves: RetentionMove[];

  /**
   * Every kept backup whose current location could not be established, in
   * verdict order: the journal holds no confirmed copy of it, or holds
   * more than one, which is a move already in flight.
   *
   * No move is planned for one, and that is exactly why this list exists
   * rather than the backup being quietly skipped. "I could not confirm
   * where this is" and "this is already where it belongs" produce the
   * same silence and are not the same claim, and only one of them is
   * something an operator acts on.
   *
   * ALWAYS an array, for `moves`' reason.
   */
  unconfirmedPlacements: string[];

  /** The policy these verdicts were decided under, resolved (issue #333).
   *
   *  It travels with the plan rather than being fetched beside it because
   *  a plan is pinned to the configuration revision it was computed
   *  against and a second read is not: a dialog that fetched the policy
   *  on its own could show a chain that did not decide the verdicts
   *  underneath it. */
  retention: RetentionSettings;

  /** Whether `retention` above is this backup set's OWN policy rather
   *  than the deployment's.
   *
   *  "Why is this backup about to be deleted" has a different answer, and
   *  a different place to go and change it, depending on which one was in
   *  force, and that is the question this dialog exists to answer before
   *  an operator authorises a deletion. */
  retentionIsOverride: boolean;
}
