/**
 * How a backup's retention shows up on a row: which tiers keep it, and
 * what selected it for each of them.
 *
 * The file holds two badge families and the difference between them is the
 * thing to understand before changing either. `RetentionBadges` takes the
 * CLOSED four-value vocabulary that describes a backup, and
 * `RetentionTierBadges` takes a verdict's own tiers, whose value set is
 * open because an operator defines the chain. An unrecognised tier is
 * therefore badged under its own name rather than dropped, since dropping
 * it would leave a kept artifact looking unclassified, which is the
 * wording for the opposite situation.
 *
 * Placement is named per badge rather than per row on purpose. One
 * artifact can be selected for one tier by the timestamp this manager
 * recorded and for another by the producer's own, and those differ in how
 * much they can be trusted, so a single answer for the row would be wrong
 * for exactly the artifact where it matters.
 */
import type {
  RetentionClass,
  RetentionTierPlacement,
  RetentionTierSelection
} from "@shared/types/backup";

const LABEL: Record<RetentionClass, string> = {
  daily: "Daily", weekly: "Weekly", monthly: "Monthly", protected: "Protected"
};

/** internal/retention.GFSTier / TierLastKnownGood's own string values, mapped
 *  to RetentionBadge's vocabulary.
 *
 *  This table is closed and the wire's tier set is not: FR-18's chain is
 *  operator-defined (core/internal/config's Retention.Tiers), so SEMI_ANNUAL,
 *  ANNUAL, FORTNIGHTLY or anything else a config file spells arrives here.
 *  The table therefore only decides which tiers get bespoke styling — chiefly
 *  LAST_KNOWN_GOOD, which is not a GFS selection at all but FR-19's
 *  protection and has to keep reading differently. Everything else is badged
 *  under its own name by RetentionTierBadges. */
const TIER_TO_CLASS: Record<string, RetentionClass> = {
  DAILY: "daily",
  WEEKLY: "weekly",
  MONTHLY: "monthly",
  LAST_KNOWN_GOOD: "protected"
};

/** "SEMI_ANNUAL" reads as "Semi Annual". config.Validate constrains a tier
 *  name to ^[a-z][a-z0-9_]*$ before internal/retention upper-cases it, so the
 *  input here is bounded to letters, digits and underscores. */
function tierLabel(tier: string): string {
  return tier
    .toLowerCase()
    .split("_")
    .filter((word) => word.length > 0)
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
    .join(" ");
}

/** How each placement reads on a badge, and what the badge's tooltip says
 *  it means (issue #218).
 *
 *  The word is the same one the CLI's per-artifact line prints, so an
 *  operator moving between `backup-manager retention --dry-run` and this
 *  dialog reads one vocabulary rather than two. PROTECTION is absent on
 *  purpose: FR-19's term is not a placement, and a parenthesised word
 *  after "Protected" would read as one. An unrecognised value still
 *  renders verbatim (see placementSuffix), because silently dropping to
 *  bare would make it indistinguishable from FR-19's protection. */
const PLACEMENT: Record<string, { word: string; title: string }> = {
  DISCOVERY: {
    word: "discovery",
    title: "Selected by the time Backup Manager discovered this artifact. Nothing outside this manager can move that timestamp."
  },
  PRODUCER: {
    word: "producer",
    title: "Selected by the producer's own timestamp on the remote object, which Backup Manager treats as untrusted input. Only this pass keeps it in this tier."
  },
  BOTH: {
    word: "both",
    title: "Selected by both the discovery timestamp and the producer's own timestamp, so this tier does not rest on untrusted input."
  }
};

/** The "(discovery)" half of a tier badge's label, or "" when there is no
 *  placement to name. */
function placementSuffix(by: RetentionTierPlacement | undefined): string {
  if (by === undefined || by === "PROTECTION") return "";
  return " (" + (PLACEMENT[by]?.word ?? by.toLowerCase()) + ")";
}

function Badge({ label, accent, title }: { label: string; accent?: boolean; title?: string }) {
  return (
    <span
      style={{
        padding: "2px 7px", borderRadius: "var(--radius-sm)",
        fontFamily: "var(--font-mono)", fontSize: "var(--text-xs)",
        border: "1px solid " + (accent ? "var(--accent)" : "var(--border-strong)"),
        background: accent ? "var(--accent-quiet)" : "transparent"
      }}
      title={title}
    >
      {label}
    </span>
  );
}

/** A backup can hold several classifications at once — they are shown as a set,
 *  never collapsed into one "tier" (§13).
 *
 *  `by` is which of FR-18's two placements selected this artifact for this
 *  tier, when the caller knows it (RetentionTierBadges always does; the
 *  RetentionClass callers, which describe a backup rather than one
 *  verdict's tier, do not). */
export function RetentionBadge({ kind, by }: { kind: RetentionClass; by?: RetentionTierPlacement }) {
  const isProtected = kind === "protected";
  const placement = by !== undefined ? PLACEMENT[by] : undefined;
  return (
    <Badge
      label={LABEL[kind] + placementSuffix(by)}
      accent={isProtected}
      title={
        isProtected
          ? "Newest known-good backup \u2014 never deleted by retention"
          : placement?.title
      }
    />
  );
}

export function RetentionBadges({ classes }: { classes: RetentionClass[] }) {
  if (classes.length === 0)
    return <span style={{ color: "var(--text-3)", fontSize: "var(--text-sm)" }}>unclassified</span>;
  return (
    <span style={{ display: "inline-flex", gap: 5, flexWrap: "wrap" }}>
      {classes.map((c) => <RetentionBadge key={c} kind={c} />)}
    </span>
  );
}

/** Badges a retention verdict's own `tiers` (types/backup.ts's
 *  RetentionVerdict.tiers), whose value set is open.
 *
 *  Every tier that selected the artifact is shown: the four with a bespoke
 *  class in their own styling, and any operator-defined tier under its own
 *  name. An artifact kept by a tier this build has never heard of must never
 *  fall through to "unclassified", which is the wording for the opposite
 *  condition (no tier claims this — the DELETE case) and which the Keep row of
 *  the confirm-before-delete dialog carries no reason text to contradict.
 *
 *  Each badge also names the placement that selected the artifact for that
 *  tier (issue #218). It is per badge and not once per row because one
 *  artifact can be selected by DAILY through the discovery timestamp and
 *  by MONTHLY through the producer's own, and a row-level answer would be
 *  wrong for exactly that artifact. */
export function RetentionTierBadges({ tiers }: { tiers: RetentionTierSelection[] }) {
  if (tiers.length === 0)
    return <span style={{ color: "var(--text-3)", fontSize: "var(--text-sm)" }}>unclassified</span>;
  return (
    <span style={{ display: "inline-flex", gap: 5, flexWrap: "wrap" }}>
      {tiers.map((sel) => {
        const known = TIER_TO_CLASS[sel.tier];
        return known !== undefined
          ? <RetentionBadge key={sel.tier} kind={known} by={sel.selectedBy} />
          : <Badge
              key={sel.tier}
              label={tierLabel(sel.tier) + placementSuffix(sel.selectedBy)}
              title={PLACEMENT[sel.selectedBy]?.title}
            />;
      })}
    </span>
  );
}
