import type { RetentionSettings } from "@shared/api/contracts";

export type HealthState = "healthy" | "degraded" | "stale" | "failing";

export type CompletionMethod =
  | "atomic-rename"
  | "completion-marker"
  | "stable-size";

export type RetentionClass = "daily" | "weekly" | "monthly" | "protected";

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
}

export type QuarantineReason =
  | "checksum-mismatch"
  | "validation-failed"
  | "unexpected-artifact"
  | "remote-identity-changed"
  | "incomplete-transfer";

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

export interface RetentionVerdict {
  /** The artifact's filename within its backup set, not an opaque id
   *  (service.RetentionArtifactVerdict.Artifact is v.Artifact.Name). */
  artifact: string;
  action: RetentionVerdictAction;
  reason: string;
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
