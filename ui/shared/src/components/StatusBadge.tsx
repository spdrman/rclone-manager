/**
 * The pill that states a status, and the one table that decides how each
 * health state looks anywhere in the app.
 *
 * Colour is never the message. Every badge carries an icon and a word, and
 * the icon is marked decorative so a screen reader hears the word alone,
 * which is the same reading a colour-blind operator gets from the shape.
 * A status that can only be told apart by hue is a status that is not
 * being communicated.
 *
 * `HEALTH_PRESENTATION` is exported because several surfaces need the
 * pieces rather than the finished badge: the summary headline wants the
 * label in capitals, the card wants the icon on its own. Keeping the
 * mapping in one place is what stops "stale" from being amber on one
 * screen and red on the next.
 *
 * Until issue #621 the artwork here was a Unicode geometric character and
 * this file called it a glyph. It is a name in the shared icon registry
 * now, and the registry's own doc says why that is a picture rather than a
 * code point. What did not change is the rule above it.
 */
import type { HealthState } from "@shared/types/backup";
import { Icon, iconForLegacyGlyph } from "@shared/design-system/icons";
import type { IconName } from "@shared/design-system/icons";

export type StatusTone = "ok" | "warn" | "danger" | "neutral" | "accent";

const TONE: Record<StatusTone, { color: string; quiet: string }> = {
  ok: { color: "var(--ok)", quiet: "var(--ok-quiet)" },
  warn: { color: "var(--warn)", quiet: "var(--warn-quiet)" },
  danger: { color: "var(--danger)", quiet: "var(--danger-quiet)" },
  neutral: { color: "var(--border-strong)", quiet: "var(--surface-2)" },
  accent: { color: "var(--accent)", quiet: "var(--accent-quiet)" }
};

/** Icons, not colour, carry the meaning. Screen readers get the label text;
 *  the icon is decorative. §9 forbids colour-only status. */
export const HEALTH_PRESENTATION: Record<
  HealthState,
  { tone: StatusTone; icon: IconName; label: string }
> = {
  healthy: { tone: "ok", icon: "status-active", label: "Healthy" },
  degraded: { tone: "warn", icon: "warning", label: "Degraded" },
  stale: { tone: "warn", icon: "warning", label: "Stale" },
  failing: { tone: "danger", icon: "failure", label: "Failing" }
};

export function StatusBadge({
  tone = "neutral",
  icon,
  glyph,
  children
}: {
  tone?: StatusTone;
  icon?: IconName;
  /**
   * A retired code point, for a caller that still hands this component a
   * character instead of a name.
   *
   * There is exactly one, ActivityStrip's status pill, and #625 owns that
   * file this cycle, so #621 translates rather than reaching into it: the
   * character is looked up in the registry's compatibility table and the
   * same artwork is drawn. A string the table does not know falls through
   * to being rendered as itself, which is what it did before this change
   * and is a better answer than nothing on screen.
   *
   * It goes when that call site does. icon-artwork.test.tsx names the
   * files still in this position and fails when one of them leaves, so
   * the shim cannot quietly outlive its reason.
   */
  glyph?: string;
  children: React.ReactNode;
}) {
  const t = TONE[tone];
  const resolved = icon ?? (glyph === undefined ? undefined : iconForLegacyGlyph(glyph));
  return (
    <span
      style={{
        display: "inline-flex", alignItems: "center", gap: 6,
        padding: "3px 9px", borderRadius: "var(--radius-pill)",
        border: "1px solid " + t.color, background: t.quiet,
        fontSize: "var(--text-sm)", fontWeight: 500, whiteSpace: "nowrap"
      }}
    >
      {resolved ? (
        <span aria-hidden="true" style={{ color: t.color, display: "inline-flex" }}>
          <Icon name={resolved} />
        </span>
      ) : glyph ? (
        <span aria-hidden="true" style={{ color: t.color }}>{glyph}</span>
      ) : null}
      {children}
    </span>
  );
}

/** A health state as a badge, with no choices left to the caller. It
 *  exists so that no page picks its own tone, icon or wording for a
 *  state: "stale" reads the same on the dashboard, on a card and on a
 *  detail page, because none of them decide. */
export function HealthBadge({ state }: { state: HealthState }) {
  const p = HEALTH_PRESENTATION[state];
  return <StatusBadge tone={p.tone} icon={p.icon}>{p.label}</StatusBadge>;
}
