import { useState } from "react";
import type { CSSProperties, ReactNode } from "react";

/** The four tones a notice can be drawn in. */
export type BannerTone = "info" | "ok" | "warn" | "danger";

export interface BannerProps {
  tone: BannerTone;
  /** The element to draw. `li` is here for the one caller that uses the
   *  banner box as the skin of a list row inside its own `<ul>`, where a
   *  div would be invalid markup. */
  as?: "div" | "li";
  role?: "alert" | "status" | "group";
  /** Only for a banner whose `role` needs a name of its own. Spelled out
   *  rather than spreading arbitrary props, so the component's surface
   *  stays something a reader can enumerate. */
  ariaLabel?: string;
  className?: string;
  style?: CSSProperties;
  /**
   * Set false for a banner that has to stay on screen. Default true, so
   * the control is opt-OUT rather than opt-in: #620 asks for a way out of
   * every banner, and a default of false would leave that to be remembered
   * at thirty call sites.
   *
   * Two shapes earn the opt-out, and both are about what goes away WITH
   * the banner rather than about how serious the banner is:
   *
   *   - the banner IS the surface, not a notice over it. ErrorState and
   *     ErrorBoundary are rendered INSTEAD of the content that failed and
   *     carry the only way back (Try again, Reload); MediumDisclosure
   *     carries the checkbox that arms Save. Dismissing one of those
   *     leaves a blank panel and a control that is off for no visible
   *     reason.
   *   - the words are the only thing standing between an operator and a
   *     misunderstanding. The halt banner is the case WarningBanner's own
   *     doc had already settled ("it will not offer to dismiss, retry or
   *     re-trust", §77 invariant 5). StorageDestinationsCard's failed
   *     verification pane is the same shape: every sentence in it is a
   *     claim about what has NOT happened, and it exists to stop an
   *     operator reading a red check as "my backups are gone".
   *
   * Anything else takes the control. A banner that is merely serious is
   * still a banner somebody has finished reading.
   */
  dismissible?: boolean;
  /**
   * What this banner is currently reporting, when the caller has a value
   * that says so (an error message, usually).
   *
   * A dismissal is about the sentence that was on screen, so when this
   * changes the banner comes back without waiting for a remount. That is
   * the half of #620 that is a safety property rather than a convenience:
   * without it, an operator who put away "Backup Manager could not log in
   * to nas-01" would never see "the SSH host key for nas-01 has changed"
   * arrive in its place, because the second condition would render into a
   * box that is already dismissed.
   *
   * Optional, because most banners say one fixed thing and have no such
   * value. WarningBanner derives one from its own tone, eyebrow, title and
   * scalar body, which covers every notice that goes through it. The tone
   * is folded in here for every caller either way, see `reportKey` below.
   */
  dismissKey?: string;
  /** The close control's accessible name. Worth setting on a surface that
   *  carries more than one banner at once, where the default leaves a
   *  reader, and a locator, with two controls of the same name. */
  dismissLabel?: string;
  children?: ReactNode;
}

/**
 * The box every notice in this app is drawn in, and the close control that
 * lets an operator put one away (issue #620).
 *
 * Until now there was no component here at all. WarningBanner drew the
 * notices that wanted an icon, an eyebrow and a title, and thirty-four
 * other places set the `banner banner--info` classes on a div of their
 * own, each with its own layout inside. Nothing was wrong with those divs;
 * what was wrong is that a class name cannot carry behaviour, so there was
 * nowhere to put a close control and no way to answer #620 without giving
 * the box one owner.
 *
 * So this takes the same `style`, `role` and children those call sites
 * already passed and draws exactly what they drew. The conversion was a
 * rename plus a tone prop rather than a redesign of thirty-four layouts,
 * which is what kept it reviewable.
 *
 * # Dismissing is a viewer-side act and nothing else
 *
 * This app refuses through banners, so what matters about the close
 * control is what it must NOT do. It calls nothing, sends nothing and
 * stores nothing: the whole mechanism is one `useState` in this component,
 * so a dismissal cannot outlive the render tree it happened in. A
 * condition that is still true draws its banner again the next time the
 * surface is rendered, and an operator who put one away by accident gets
 * it back by reloading.
 *
 * That is deliberately narrower than persisting the dismissal. Somewhere
 * durable, localStorage or the service, would mean a banner an operator
 * can silence for good, and a silenced refusal reads exactly like no
 * refusal at all.
 *
 * # Why a re-render alone does not bring it back
 *
 * #620 asks that re-rendering the same condition bring the banner back.
 * Read as "any re-render" that cannot be built, because it contradicts the
 * acceptance criterion beside it: DashboardPage re-renders on every poll
 * and on every activity frame, so the banner would be back within a second
 * or two and the control would read as broken. The reading here keeps both
 * halves true. The dismissal dies with the render tree, so rendering the
 * surface again from the condition (a navigation, a reload, a remount)
 * shows the banner, and `dismissKey` brings it back mid-render the moment
 * it starts reporting something else.
 *
 * # The control itself
 *
 * A real `<button type="button">`, so Tab reaches it and Enter and Space
 * operate it with no keyboard handling written here. The type is explicit
 * because banners sit inside forms (the wizard, the settings pages) and a
 * button with no type inside a form submits it.
 *
 * Its name is visually hidden TEXT rather than an aria-label, following
 * FieldHelp's close control and the reasoning its module doc gives: an
 * aria-label makes a button answer to that name in every label-based
 * lookup, and text inside the button is what survives translation and
 * find-in-page. PasswordInput diverges from that rule because its toggle
 * sits inside a `<label>` whose textContent is the field's own name. A
 * banner is not inside one, so the rule applies here unchanged.
 *
 * It is put in the corner by CSS rather than by DOM order, which is what
 * lets it sit at the top right of a banner whose content is a column, a
 * row or a list item. Being last in the DOM makes it last in the tab
 * order, so a keyboard reads the notice and its actions before it reaches
 * the way out, which is the order somebody would want them in anyway.
 */
export function Banner({
  tone,
  as: Tag = "div",
  role,
  ariaLabel,
  className,
  style,
  dismissible = true,
  dismissKey,
  dismissLabel = "Dismiss this notice",
  children
}: BannerProps) {
  const [dismissed, setDismissed] = useState(false);

  // The tone joins the key without any caller asking. Two banners in the
  // arms of one ternary are one element in one position, so React keeps
  // the state across the swap, and a banner that changed colour under a
  // dismissal is reporting something else by definition. It costs a caller
  // nothing and it is the half of "which report is this" that is visible
  // on screen.
  const reportKey = tone + "\u001f" + (dismissKey ?? "");

  // Adjusting state during render rather than in an effect, which is the
  // pattern React documents for "reset when a prop changes" and the one
  // PasswordInput already uses here for the same reason: an effect would
  // paint one frame in which the new condition's banner is still hidden
  // by the previous condition's dismissal.
  const [lastKey, setLastKey] = useState(reportKey);
  if (reportKey !== lastKey) {
    setLastKey(reportKey);
    if (dismissed) setDismissed(false);
  }

  if (dismissible && dismissed) return null;

  const classes = ["banner", "banner--" + tone];
  // The modifier is what reserves the room the control sits in, so a
  // banner that opted out keeps the symmetric padding it always had.
  if (dismissible) classes.push("banner--dismissible");
  if (className) classes.push(className);

  return (
    <Tag className={classes.join(" ")} role={role} aria-label={ariaLabel} style={style}>
      {children}
      {dismissible ? (
        <button type="button" className="banner__close" onClick={() => setDismissed(true)}>
          {/* A literal in a braced expression rather than JSX text or a
              unicode escape, which is the bug #257 exists for: an escape
              written as element content reaches the operator as the six
              characters it is spelled with. */}
          <span aria-hidden="true">{"×"}</span>
          <span className="visually-hidden">{dismissLabel}</span>
        </button>
      ) : null}
    </Tag>
  );
}
