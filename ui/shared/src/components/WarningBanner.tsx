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
 *
 * That sentence is also what settled `dismissible` when #620 gave every
 * banner a close control. The control is on by default, because a notice
 * an operator has read should go away, and the halt banner passes false:
 * a close control there is the same defect as the "Keep set halted" button
 * that used to sit on it and do nothing, one step further along. It looks
 * like a decision an operator is allowed to make about the halt, and it is
 * not. Banner's own doc carries the rule for the rest of the opt-outs.
 *
 * The dismissal is scoped to what the banner is SAYING rather than to this
 * element's position in the tree, which is why the key below is derived
 * from what it says. DashboardPage renders one halt banner for whichever
 * set is halted and one stale banner for whichever set is stale, so
 * without that a dismissal would carry over from one set's refusal to a
 * different set's.
 *
 * The body counts, and leaving it out was a real defect rather than a
 * theoretical one. The derived key was tone, eyebrow and title alone,
 * which holds until a call site keeps a fixed title over a body that
 * moves; the dashboard's stale banner is exactly that, titled "Stale ·
 * <name>" with every word an operator acts on in the note underneath. The
 * same set staying stale unmounts nothing, so one dismissal swallowed
 * every later note for it.
 *
 * Only a SCALAR body joins the key. A ReactNode has no cheap identity and
 * hashing one would be a guess that fails silently in whichever direction
 * it got wrong, so a caller rendering elements passes `dismissKey`
 * explicitly instead. Every banner here whose body is prose gets the
 * property for free.
 */
import type { ReactNode } from "react";
import { Banner } from "./Banner";
import type { BannerTone } from "./Banner";
import { Icon } from "@shared/design-system/icons";
import type { IconName } from "@shared/design-system/icons";

export type { BannerTone };

/** Issue #621. The info tone used to be drawn as a lower-case letter i,
 *  which is not an icon, it is a letter that looks like one at 13px and
 *  looks like a typo at any other size. */
const ICON: Record<BannerTone, IconName> = {
  info: "info", ok: "success", warn: "warning", danger: "failure"
};

const COLOR: Record<BannerTone, string> = {
  info: "var(--text-3)", ok: "var(--ok)", warn: "var(--warn)", danger: "var(--danger)"
};

/** The body as a key fragment, when it is prose. Anything else reads as
 *  the empty string rather than as a guess: a node that renders differently
 *  on every pass would make the key change constantly and put a dismissed
 *  banner straight back, which is the opposite failure and a worse one,
 *  because it looks like the control is broken. */
function scalarBody(children: ReactNode): string {
  return typeof children === "string" || typeof children === "number" ? String(children) : "";
}

export function WarningBanner({
  tone = "warn",
  eyebrow,
  title,
  children,
  actions,
  dismissible = true,
  dismissKey
}: {
  tone?: BannerTone;
  eyebrow?: string;
  title?: string;
  children?: ReactNode;
  actions?: ReactNode;
  /** False for a banner that must stay on screen (#620). Banner's own doc
   *  carries the rule for when that is right; HaltBanner and App's two
   *  root banners are the callers that use it. */
  dismissible?: boolean;
  /** What this banner is reporting, when the caller knows it better than
   *  the words do. Overrides the derived key below, and is the way a
   *  caller whose body is elements rather than prose gets the same
   *  property the derived key gives everyone else. */
  dismissKey?: string;
}) {
  return (
    <Banner
      tone={tone}
      role={tone === "danger" || tone === "warn" ? "alert" : undefined}
      dismissible={dismissible}
      // The unit separator rather than a printable one, so a title that
      // happens to contain the separator cannot make two different
      // reports look like the same one.
      dismissKey={dismissKey ?? [tone, eyebrow ?? "", title ?? "", scalarBody(children)].join("\u001f")}
    >
      <span aria-hidden="true" style={{ color: COLOR[tone], lineHeight: 1.5 }}>
        <Icon name={ICON[tone]} />
      </span>
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
    </Banner>
  );
}
