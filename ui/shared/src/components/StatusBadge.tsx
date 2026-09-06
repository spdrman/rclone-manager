/**
 * The pill that states a status, and the one table that decides how each
 * health state looks anywhere in the app.
 *
 * Colour is never the message. Every badge carries a glyph and a word, and
 * the glyph is marked decorative so a screen reader hears the word alone,
 * which is the same reading a colour-blind operator gets from the shape.
 * A status that can only be told apart by hue is a status that is not
 * being communicated.
 *
 * `HEALTH_PRESENTATION` is exported because several surfaces need the
 * pieces rather than the finished badge: the summary headline wants the
 * label in capitals, the card wants the glyph on its own. Keeping the
 * mapping in one place is what stops "stale" from being amber on one
 * screen and red on the next.
 */
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

/** A health state as a badge, with no choices left to the caller. It
 *  exists so that no page picks its own tone, glyph or wording for a
 *  state: "stale" reads the same on the dashboard, on a card and on a
 *  detail page, because none of them decide. */
export function HealthBadge({ state }: { state: HealthState }) {
  const p = HEALTH_PRESENTATION[state];
  return <StatusBadge tone={p.tone} glyph={p.glyph}>{p.label}</StatusBadge>;
}
