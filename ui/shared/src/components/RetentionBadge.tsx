import type { RetentionClass } from "@shared/types/backup";

const LABEL: Record<RetentionClass, string> = {
  daily: "Daily", weekly: "Weekly", monthly: "Monthly", protected: "Protected"
};

/** A backup can hold several classifications at once — they are shown as a set,
 *  never collapsed into one "tier" (§13). */
export function RetentionBadge({ kind }: { kind: RetentionClass }) {
  const isProtected = kind === "protected";
  return (
    <span
      style={{
        padding: "2px 7px", borderRadius: "var(--radius-sm)",
        fontFamily: "var(--font-mono)", fontSize: "var(--text-xs)",
        border: "1px solid " + (isProtected ? "var(--accent)" : "var(--border-strong)"),
        background: isProtected ? "var(--accent-quiet)" : "transparent"
      }}
      title={isProtected ? "Newest known-good backup \u2014 never deleted by retention" : undefined}
    >
      {LABEL[kind]}
    </span>
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
