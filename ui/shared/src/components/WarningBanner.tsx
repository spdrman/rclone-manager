/**
 * The full-width notice a page puts above its content.
 *
 * Only the warn and danger tones announce themselves as alerts. That is a
 * deliberate line rather than an oversight: an assistive technology
 * interrupting the reader is right for "this backup set is halted" and
 * wrong for the first-run hint, and a component that alerted on every tone
 * would train people to ignore the ones that matter.
 *
 * `actions` is a slot rather than a button prop because the banners that
 * carry one mostly navigate to evidence rather than resolving anything.
 * The halt banner is the case that fixes this rule: it will not offer to
 * dismiss, retry or re-trust, so whatever a page passes has to be its own
 * decision, made where the consequences are visible.
 */
import type { ReactNode } from "react";

export type BannerTone = "info" | "ok" | "warn" | "danger";

const GLYPH: Record<BannerTone, string> = {
  info: "i", ok: "\u2713", warn: "\u25b2", danger: "\u2715"
};

const COLOR: Record<BannerTone, string> = {
  info: "var(--text-3)", ok: "var(--ok)", warn: "var(--warn)", danger: "var(--danger)"
};

export function WarningBanner({
  tone = "warn",
  eyebrow,
  title,
  children,
  actions
}: {
  tone?: BannerTone;
  eyebrow?: string;
  title?: string;
  children?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <div className={"banner banner--" + tone} role={tone === "danger" || tone === "warn" ? "alert" : undefined}>
      <span aria-hidden="true" style={{ color: COLOR[tone], lineHeight: 1.5 }}>{GLYPH[tone]}</span>
      <div style={{ flex: 1, display: "flex", flexDirection: "column", gap: 6 }}>
        {eyebrow ? (
          <div
            className="eyebrow"
            style={{ color: COLOR[tone], fontSize: "var(--text-xs)", letterSpacing: "0.09em" }}
          >
            {eyebrow}
          </div>
        ) : null}
        {title ? <div style={{ fontWeight: 600, fontSize: 13.5 }}>{title}</div> : null}
        {children ? (
          <div style={{ fontSize: 13, color: "var(--text-2)", maxWidth: "76ch" }}>{children}</div>
        ) : null}
        {actions ? <div style={{ display: "flex", gap: 9, marginTop: 4 }}>{actions}</div> : null}
      </div>
    </div>
  );
}
