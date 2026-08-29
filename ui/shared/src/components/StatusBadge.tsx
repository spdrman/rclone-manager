import type { HealthState } from "@shared/types/backup";

export type StatusTone = "ok" | "warn" | "danger" | "neutral" | "accent";

const TONE: Record<StatusTone, { color: string; quiet: string }> = {
  ok: { color: "var(--ok)", quiet: "var(--ok-quiet)" },
  warn: { color: "var(--warn)", quiet: "var(--warn-quiet)" },
  danger: { color: "var(--danger)", quiet: "var(--danger-quiet)" },
  neutral: { color: "var(--border-strong)", quiet: "var(--surface-2)" },
  accent: { color: "var(--accent)", quiet: "var(--accent-quiet)" }
};

/** Glyphs, not colour, carry the meaning. Screen readers get the label text;
 *  the glyph is decorative. §9 forbids colour-only status. */
export const HEALTH_PRESENTATION: Record<
  HealthState,
  { tone: StatusTone; glyph: string; label: string }
> = {
  healthy: { tone: "ok", glyph: "\u25cf", label: "Healthy" },
  degraded: { tone: "warn", glyph: "\u25b2", label: "Degraded" },
  stale: { tone: "warn", glyph: "\u25b2", label: "Stale" },
  failing: { tone: "danger", glyph: "\u2715", label: "Failing" }
};

export function StatusBadge({
  tone = "neutral",
  glyph,
  children
}: {
  tone?: StatusTone;
  glyph?: string;
  children: React.ReactNode;
}) {
  const t = TONE[tone];
  return (
    <span
      style={{
        display: "inline-flex", alignItems: "center", gap: 6,
        padding: "3px 9px", borderRadius: "var(--radius-pill)",
        border: "1px solid " + t.color, background: t.quiet,
        fontSize: "var(--text-sm)", fontWeight: 500, whiteSpace: "nowrap"
      }}
    >
      {glyph ? (
        <span aria-hidden="true" style={{ color: t.color }}>{glyph}</span>
      ) : null}
      {children}
    </span>
  );
}

export function HealthBadge({ state }: { state: HealthState }) {
  const p = HEALTH_PRESENTATION[state];
  return <StatusBadge tone={p.tone} glyph={p.glyph}>{p.label}</StatusBadge>;
}
