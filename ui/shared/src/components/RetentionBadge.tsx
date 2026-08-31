import type { RetentionClass } from "@shared/types/backup";

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
 *  never collapsed into one "tier" (§13). */
export function RetentionBadge({ kind }: { kind: RetentionClass }) {
  const isProtected = kind === "protected";
  return (
    <Badge
      label={LABEL[kind]}
      accent={isProtected}
      title={isProtected ? "Newest known-good backup \u2014 never deleted by retention" : undefined}
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
 *  the confirm-before-delete dialog carries no reason text to contradict. */
export function RetentionTierBadges({ tiers }: { tiers: string[] }) {
  if (tiers.length === 0)
    return <span style={{ color: "var(--text-3)", fontSize: "var(--text-sm)" }}>unclassified</span>;
  return (
    <span style={{ display: "inline-flex", gap: 5, flexWrap: "wrap" }}>
      {tiers.map((tier) => {
        const known = TIER_TO_CLASS[tier];
        return known !== undefined
          ? <RetentionBadge key={tier} kind={known} />
          : <Badge key={tier} label={tierLabel(tier)} />;
      })}
    </span>
  );
}
