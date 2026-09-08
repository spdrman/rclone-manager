/**
 * Every icon this product draws, as the artwork rather than as a character
 * the operator's font happened to have (issue #621).
 *
 * # What was here before
 *
 * Icons were Unicode geometric code points written into JSX: U+25B2 for a
 * warning, U+2715 for a failure, U+2713 for a success, U+25A4 and U+25A5
 * for two different nav rows. There were fourteen of them and every one
 * had the same three problems. It rendered in whatever the operator's
 * system font decided, so the product looked different on a Mac, on a
 * Windows machine and inside a NAS vendor's embedded browser. Several were
 * near-identical at the 13px they were drawn at, which is where a tick and
 * a cross being one filled shape apart stops being a distinction. And a
 * few carried no icon meaning at all: a hollow diamond for the dashboard
 * and two squares hatched in different directions for backup sets and
 * backups are not pictures of anything.
 *
 * # Why the paths are in here rather than in a package
 *
 * Three constraints, and together they pick the shape rather than leaving
 * it to taste.
 *
 * This runs on a NAS. Not "usually has internet", not "behind a proxy": an
 * appliance on a LAN that may have no route out at all, which is a
 * deployment this product supports on purpose. Anything fetched to draw an
 * icon (a CDN stylesheet, a webfont, a sprite sheet, an <img>) is an empty
 * box there, and it fails silently, with the layout intact and nothing on
 * screen saying a thing is missing.
 *
 * There is a bundle budget and the image size is gated at 1.05x its
 * baseline. Font Awesome's React packages would spend a slice of that on
 * an icon registry, a tree-shaking story and a runtime, to draw fourteen
 * shapes. What is actually needed is fourteen `d` attributes, which is
 * about 4.7 KB of path data before compression, and that is what is below.
 *
 * Font Awesome Free icons are CC BY 4.0, which is an attribution licence
 * rather than a permissive one. So the artwork has to be recorded rather
 * than merely used, and it is:
 * docs/compliance/bundled-icon-artwork.md carries the attribution CC BY
 * 4.0 section 3(a)(1) asks for, icon-artwork.test.tsx checks that record
 * against this registry in both directions, and the licence's own request
 * that its embedded comments not be stripped out of the files is what
 * FONT_AWESOME_NOTICE below is.
 *
 * Nothing here is modified. Each `path` is the exact `d` attribute out of
 * the SVG that release ships, so the artwork a recipient gets is the
 * artwork Font Awesome published.
 *
 * # The accessibility contract, which did not change
 *
 * The design system's rule is that glyphs and not colour carry meaning,
 * and that the glyph itself is decorative: it is aria-hidden and the
 * visible word beside it is what a screen reader reads. Swapping a
 * character for a picture is exactly the change that quietly breaks that,
 * so Icon sets aria-hidden itself rather than leaving it to thirty call
 * sites, and there is no prop to turn it off. An icon that needs a name is
 * an icon that should have a word beside it.
 *
 * Sizing is 1em by default, so an icon is the size of the text it sits in
 * and every existing font-size on every existing wrapper keeps meaning
 * what it meant. Colour is currentColor, so the `var(--warn)` and
 * `var(--ok)` a badge already set on its wrapper reach the artwork with no
 * second colour system and no literal hex anywhere near a component, which
 * is the rule dark-mode-contrast.test.ts enforces for text.
 */
import type { CSSProperties } from "react";

/** The Font Awesome Free release the artwork below was taken from. Pinned
 *  rather than described, because the attribution has to name a version:
 *  a later release is a different upload with its own paths. */
export const FONT_AWESOME_RELEASE = "6.7.2";

/** The licence the icons are under. Font Awesome Free is three licences at
 *  once (icons, fonts, code) and only this one applies here, because only
 *  the icons are used: no webfont and no Font Awesome code ships. */
export const FONT_AWESOME_ICON_LICENCE = "CC BY 4.0";

/** The URI of the licence text, which is a thing CC BY 4.0 section
 *  3(a)(1)(D) asks an attribution to carry rather than a convenience. */
export const FONT_AWESOME_LICENCE_URL = "https://creativecommons.org/licenses/by/4.0/";

/**
 * The comment Font Awesome embeds in every SVG file it ships.
 *
 * Reproduced verbatim, because the licence asks in as many words that
 * these not be actively removed from files, and lifting a path out of the
 * file it came from is the easiest possible way to remove one by accident.
 *
 * It is a constant rather than a comment so that a build cannot strip it:
 * minification removes comments, and this is the one line of this module
 * that is a licence obligation rather than an explanation.
 */
export const FONT_AWESOME_NOTICE =
  "Font Awesome Free 6.7.2 by @fontawesome - https://fontawesome.com " +
  "License - https://fontawesome.com/license/free " +
  "(Icons: CC BY 4.0, Fonts: SIL OFL 1.1, Code: MIT License) " +
  "Copyright 2024 Fonticons, Inc.";

/** The name a surface asks for an icon by. Intentionally the ROLE and not
 *  the drawing: "warning" rather than "triangle", so changing which
 *  artwork a warning uses is one line here and not a search through the
 *  call sites. */
export type IconName =
  | "warning"
  | "failure"
  | "success"
  | "status-active"
  | "status-idle"
  | "info"
  | "dashboard"
  | "backup-sets"
  | "backups"
  | "activity"
  | "quarantine"
  | "settings"
  | "arrow-left"
  | "arrow-right";

/** One icon's artwork, and the Font Awesome name it came from. The name is
 *  kept rather than dropped because the attribution record has to list it
 *  and a test compares the two lists. */
export interface IconArtwork {
  /** The style and name of the Font Awesome Free icon, as that release
   *  files it: `solid/triangle-exclamation`. */
  fontAwesome: string;
  viewBox: string;
  /** The `d` attribute, verbatim. */
  path: string;
}

/**
 * The registry.
 *
 * Every entry says which retired code point it replaces, because that is
 * the fact a reader coming to this file after #621 wants and it is not
 * recoverable from the artwork.
 */
export const ICON_ARTWORK: Record<IconName, IconArtwork> = {
  /** Replaces U+25B2 (22 uses) and U+26A0 (1). Both meant "something is
   *  wrong here" and were drawn differently only because they were
   *  written at different times. */
  "warning": {
    fontAwesome: "solid/triangle-exclamation",
    viewBox: "0 0 512 512",
    path:
      "M256 32c14.2 0 27.3 7.5 34.5 19.8l216 368c7.3 12.4 7.3 27.7 .2 40.1S486.3 480 472 480L40 480c-14.3 0-27.6-7.7-34.7-20.1s-7-27.8 .2-40.1l216-368C228.7 39.5 241.8 32 256 32zm0 128c-13.3 0-24 10.7-24 24l0 112c0 13.3 10.7 24 24 24s24-10.7 24-24l0-112c0-13.3-10.7-24-24-24zm32 224a32 32 0 1 0 -64 0 32 32 0 1 0 64 0z"
  },
  /** Replaces U+2715 (18). The bare cross was the glyph closest to the
   *  success tick at 13px, and a circle around each is what makes them
   *  tell apart at a glance rather than on inspection. */
  "failure": {
    fontAwesome: "solid/circle-xmark",
    viewBox: "0 0 512 512",
    path:
      "M256 512A256 256 0 1 0 256 0a256 256 0 1 0 0 512zM175 175c9.4-9.4 24.6-9.4 33.9 0l47 47 47-47c9.4-9.4 24.6-9.4 33.9 0s9.4 24.6 0 33.9l-47 47 47 47c9.4 9.4 9.4 24.6 0 33.9s-24.6 9.4-33.9 0l-47-47-47 47c-9.4 9.4-24.6 9.4-33.9 0s-9.4-24.6 0-33.9l47-47-47-47c-9.4-9.4-9.4-24.6 0-33.9z"
  },
  /** Replaces U+2713 (15). */
  "success": {
    fontAwesome: "solid/circle-check",
    viewBox: "0 0 512 512",
    path:
      "M256 512A256 256 0 1 0 256 0a256 256 0 1 0 0 512zM369 209L241 337c-9.4 9.4-24.6 9.4-33.9 0l-64-64c-9.4-9.4-9.4-24.6 0-33.9s24.6-9.4 33.9 0l47 47L335 175c9.4-9.4 24.6-9.4 33.9 0s9.4 24.6 0 33.9z"
  },
  /** Replaces U+25CF (9): the filled dot that means a thing is on, live
   *  or reachable. */
  "status-active": {
    fontAwesome: "solid/circle",
    viewBox: "0 0 512 512",
    path:
      "M256 512A256 256 0 1 0 256 0a256 256 0 1 0 0 512z"
  },
  /** Replaces U+25CB (2): the hollow dot for a step not started and for a
   *  backup set that is disabled. The regular weight rather than the
   *  solid one, so it stays the negative of status-active the way the
   *  two dots were. */
  "status-idle": {
    fontAwesome: "regular/circle",
    viewBox: "0 0 512 512",
    path:
      "M464 256A208 208 0 1 0 48 256a208 208 0 1 0 416 0zM0 256a256 256 0 1 1 512 0A256 256 0 1 1 0 256z"
  },
  /** Not one of the fourteen. WarningBanner drew its info tone as a
   *  lower-case letter i and ActivityTimeline drew the info severity as a
   *  middle dot, so the one tone that was not a picture in either place is
   *  a picture in both now. That middle dot is the only one #621 takes,
   *  and it is taken because it was filling an icon slot rather than
   *  separating anything. */
  "info": {
    fontAwesome: "solid/circle-info",
    viewBox: "0 0 512 512",
    path:
      "M256 512A256 256 0 1 0 256 0a256 256 0 1 0 0 512zM216 336l24 0 0-64-24 0c-13.3 0-24-10.7-24-24s10.7-24 24-24l48 0c13.3 0 24 10.7 24 24l0 88 8 0c13.3 0 24 10.7 24 24s-10.7 24-24 24l-80 0c-13.3 0-24-10.7-24-24s10.7-24 24-24zm40-208a32 32 0 1 1 0 64 32 32 0 1 1 0-64z"
  },
  /** Replaces U+25C7, a hollow diamond that was not a picture of
   *  anything. */
  "dashboard": {
    fontAwesome: "solid/gauge-high",
    viewBox: "0 0 512 512",
    path:
      "M0 256a256 256 0 1 1 512 0A256 256 0 1 1 0 256zM288 96a32 32 0 1 0 -64 0 32 32 0 1 0 64 0zM256 416c35.3 0 64-28.7 64-64c0-17.4-6.9-33.1-18.1-44.6L366 161.7c5.3-12.1-.2-26.3-12.3-31.6s-26.3 .2-31.6 12.3L257.9 288c-.6 0-1.3 0-1.9 0c-35.3 0-64 28.7-64 64s28.7 64 64 64zM176 144a32 32 0 1 0 -64 0 32 32 0 1 0 64 0zM96 288a32 32 0 1 0 0-64 32 32 0 1 0 0 64zm352-32a32 32 0 1 0 -64 0 32 32 0 1 0 64 0z"
  },
  /** Replaces U+25A4. That one and the next were a square hatched
   *  horizontally and a square hatched vertically: a distinction nobody
   *  can make at 13px, and one that says nothing when they can. */
  "backup-sets": {
    fontAwesome: "solid/layer-group",
    viewBox: "0 0 576 512",
    path:
      "M264.5 5.2c14.9-6.9 32.1-6.9 47 0l218.6 101c8.5 3.9 13.9 12.4 13.9 21.8s-5.4 17.9-13.9 21.8l-218.6 101c-14.9 6.9-32.1 6.9-47 0L45.9 149.8C37.4 145.8 32 137.3 32 128s5.4-17.9 13.9-21.8L264.5 5.2zM476.9 209.6l53.2 24.6c8.5 3.9 13.9 12.4 13.9 21.8s-5.4 17.9-13.9 21.8l-218.6 101c-14.9 6.9-32.1 6.9-47 0L45.9 277.8C37.4 273.8 32 265.3 32 256s5.4-17.9 13.9-21.8l53.2-24.6 152 70.2c23.4 10.8 50.4 10.8 73.8 0l152-70.2zm-152 198.2l152-70.2 53.2 24.6c8.5 3.9 13.9 12.4 13.9 21.8s-5.4 17.9-13.9 21.8l-218.6 101c-14.9 6.9-32.1 6.9-47 0L45.9 405.8C37.4 401.8 32 393.3 32 384s5.4-17.9 13.9-21.8l53.2-24.6 152 70.2c23.4 10.8 50.4 10.8 73.8 0z"
  },
  /** Replaces U+25A5. */
  "backups": {
    fontAwesome: "solid/box-archive",
    viewBox: "0 0 512 512",
    path:
      "M32 32l448 0c17.7 0 32 14.3 32 32l0 32c0 17.7-14.3 32-32 32L32 128C14.3 128 0 113.7 0 96L0 64C0 46.3 14.3 32 32 32zm0 128l448 0 0 256c0 35.3-28.7 64-64 64L96 480c-35.3 0-64-28.7-64-64l0-256zm128 80c0 8.8 7.2 16 16 16l160 0c8.8 0 16-7.2 16-16s-7.2-16-16-16l-160 0c-8.8 0-16 7.2-16 16z"
  },
  /** Replaces U+2261. The Activity page is a reverse-chronological event
   *  log, so a clock turning back is what it actually is. */
  "activity": {
    fontAwesome: "solid/clock-rotate-left",
    viewBox: "0 0 512 512",
    path:
      "M75 75L41 41C25.9 25.9 0 36.6 0 57.9L0 168c0 13.3 10.7 24 24 24l110.1 0c21.4 0 32.1-25.9 17-41l-30.8-30.8C155 85.5 203 64 256 64c106 0 192 86 192 192s-86 192-192 192c-40.8 0-78.6-12.7-109.7-34.4c-14.5-10.1-34.4-6.6-44.6 7.9s-6.6 34.4 7.9 44.6C151.2 495 201.7 512 256 512c141.4 0 256-114.6 256-256S397.4 0 256 0C185.3 0 121.3 28.7 75 75zm181 53c-13.3 0-24 10.7-24 24l0 104c0 6.4 2.5 12.5 7 17l72 72c9.4 9.4 24.6 9.4 33.9 0s9.4-24.6 0-33.9l-65-65 0-94.1c0-13.3-10.7-24-24-24z"
  },
  /** Replaces U+2298, which was already this shape where a font had it. */
  "quarantine": {
    fontAwesome: "solid/ban",
    viewBox: "0 0 512 512",
    path:
      "M367.2 412.5L99.5 144.8C77.1 176.1 64 214.5 64 256c0 106 86 192 192 192c41.5 0 79.9-13.1 111.2-35.5zm45.3-45.3C434.9 335.9 448 297.5 448 256c0-106-86-192-192-192c-41.5 0-79.9 13.1-111.2 35.5L412.5 367.2zM0 256a256 256 0 1 1 512 0A256 256 0 1 1 0 256z"
  },
  /** Replaces U+2699, which was already a gear where a font had one. */
  "settings": {
    fontAwesome: "solid/gear",
    viewBox: "0 0 512 512",
    path:
      "M495.9 166.6c3.2 8.7 .5 18.4-6.4 24.6l-43.3 39.4c1.1 8.3 1.7 16.8 1.7 25.4s-.6 17.1-1.7 25.4l43.3 39.4c6.9 6.2 9.6 15.9 6.4 24.6c-4.4 11.9-9.7 23.3-15.8 34.3l-4.7 8.1c-6.6 11-14 21.4-22.1 31.2c-5.9 7.2-15.7 9.6-24.5 6.8l-55.7-17.7c-13.4 10.3-28.2 18.9-44 25.4l-12.5 57.1c-2 9.1-9 16.3-18.2 17.8c-13.8 2.3-28 3.5-42.5 3.5s-28.7-1.2-42.5-3.5c-9.2-1.5-16.2-8.7-18.2-17.8l-12.5-57.1c-15.8-6.5-30.6-15.1-44-25.4L83.1 425.9c-8.8 2.8-18.6 .3-24.5-6.8c-8.1-9.8-15.5-20.2-22.1-31.2l-4.7-8.1c-6.1-11-11.4-22.4-15.8-34.3c-3.2-8.7-.5-18.4 6.4-24.6l43.3-39.4C64.6 273.1 64 264.6 64 256s.6-17.1 1.7-25.4L22.4 191.2c-6.9-6.2-9.6-15.9-6.4-24.6c4.4-11.9 9.7-23.3 15.8-34.3l4.7-8.1c6.6-11 14-21.4 22.1-31.2c5.9-7.2 15.7-9.6 24.5-6.8l55.7 17.7c13.4-10.3 28.2-18.9 44-25.4l12.5-57.1c2-9.1 9-16.3 18.2-17.8C227.3 1.2 241.5 0 256 0s28.7 1.2 42.5 3.5c9.2 1.5 16.2 8.7 18.2 17.8l12.5 57.1c15.8 6.5 30.6 15.1 44 25.4l55.7-17.7c8.8-2.8 18.6-.3 24.5 6.8c8.1 9.8 15.5 20.2 22.1 31.2l4.7 8.1c6.1 11 11.4 22.4 15.8 34.3zM256 336a80 80 0 1 0 0-160 80 80 0 1 0 0 160z"
  },
  /** Replaces U+2190: the way back, in every page header that has one. */
  "arrow-left": {
    fontAwesome: "solid/arrow-left",
    viewBox: "0 0 448 512",
    path:
      "M9.4 233.4c-12.5 12.5-12.5 32.8 0 45.3l160 160c12.5 12.5 32.8 12.5 45.3 0s12.5-32.8 0-45.3L109.2 288 416 288c17.7 0 32-14.3 32-32s-14.3-32-32-32l-306.7 0L214.6 118.6c12.5-12.5 12.5-32.8 0-45.3s-32.8-12.5-45.3 0l-160 160z"
  },
  /** Replaces U+2192. */
  "arrow-right": {
    fontAwesome: "solid/arrow-right",
    viewBox: "0 0 448 512",
    path:
      "M438.6 278.6c12.5-12.5 12.5-32.8 0-45.3l-160-160c-12.5-12.5-32.8-12.5-45.3 0s-12.5 32.8 0 45.3L338.8 224 32 224c-17.7 0-32 14.3-32 32s14.3 32 32 32l306.7 0L233.4 393.4c-12.5 12.5-12.5 32.8 0 45.3s32.8 12.5 45.3 0l160-160z"
  }
};

/** Every registered name, in registry order. Derived rather than written
 *  out again, so the sweep in icon-artwork.test.tsx cannot go on passing
 *  over an icon somebody forgot to add to a second list. */
export const ICON_NAMES = Object.keys(ICON_ARTWORK) as IconName[];

/**
 * The characters a `glyph` string can still carry, and the artwork each
 * one becomes.
 *
 * This exists for one caller. ActivityStrip's status pill hands
 * StatusBadge a character, and that file belongs to #625 this cycle, so
 * reaching into it to change one string is how two branches end up
 * conflicting over a line neither of them is about. Translating instead
 * means its badges are artwork today, without the file being touched.
 *
 * It is a Map rather than an object literal so a lookup cannot answer with
 * something off Object.prototype: `iconForLegacyGlyph("constructor")`
 * returning a function would be a strange way to draw a badge.
 *
 * It goes when the last caller does. icon-artwork.test.tsx pins which
 * files those are and fails when one of them stops needing this, which is
 * what stops a shim outliving its reason.
 */
const LEGACY_GLYPHS = new Map<string, IconName>([
  ["✓", "success"],
  ["✕", "failure"],
  ["▲", "warning"],
  ["⚠", "warning"],
  ["●", "status-active"],
  ["○", "status-idle"]
]);

/** The icon a retired character is drawn as, or undefined for anything
 *  that was never an icon. Punctuation answers undefined, which is the
 *  right answer: a middle dot separating a value from its unit is text. */
export function iconForLegacyGlyph(glyph: string): IconName | undefined {
  return LEGACY_GLYPHS.get(glyph);
}

/**
 * One icon.
 *
 * Decorative, always. There is no prop to give it a name, because every
 * place this is used has a word beside it doing that job and an icon that
 * needs its own label is an icon in the wrong place. aria-hidden is set
 * here rather than at the call sites for the same reason the tone table
 * lives in StatusBadge: a contract that each caller has to remember is a
 * contract that holds until somebody forgets.
 *
 * focusable="false" is not decoration on that. Internet Explorer and some
 * embedded webviews put an inline SVG in the tab order by default, and a
 * page whose tab order runs through fourteen invisible pictures is a page
 * a keyboard cannot be used on.
 *
 * The box is square whatever the artwork's aspect ratio is, and the viewBox
 * scales into it centred (SVG's default preserveAspectRatio), so a 448-wide
 * arrow gets a little air either side rather than being stretched. That is
 * wanted rather than tolerated: the nav rows line up because every icon
 * occupies the same square.
 *
 * `1em` by default, so an icon is the size of the text it sits in and every
 * font-size already set on every wrapper in this app goes on meaning what
 * it meant.
 */
export function Icon({
  name,
  size,
  className,
  style
}: {
  name: IconName;
  /** Anything CSS accepts for a length. Defaults to 1em, which is almost
   *  always what a surface wants: the icon tracks its own text. */
  size?: number | string;
  className?: string;
  style?: CSSProperties;
}) {
  const artwork = ICON_ARTWORK[name];
  const box = size ?? "1em";
  return (
    <svg
      viewBox={artwork.viewBox}
      width={box}
      height={box}
      // currentColor and never a colour of its own, so the token the
      // caller already set (var(--warn), var(--ok), var(--danger)) reaches
      // the artwork. A literal here could not invert with the theme, which
      // is the rule dark-mode-contrast.test.ts enforces for text and the
      // same rule for the same reason.
      fill="currentColor"
      aria-hidden="true"
      focusable="false"
      className={className}
      style={{ display: "inline-block", verticalAlign: "-0.125em", flex: "none", ...style }}
    >
      <path d={artwork.path} />
    </svg>
  );
}
