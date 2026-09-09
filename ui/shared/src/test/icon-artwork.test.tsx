/**
 * The icons are drawn artwork now, and issue #621 is four separate promises
 * about how (offline, cheap, attributed, still accessible) rather than one
 * about what they look like.
 *
 * Until this, every icon in the product was a Unicode geometric character
 * rendered in whatever the operator's system font decided: a black
 * up-pointing triangle for a warning, a multiplication sign for a failure,
 * two squares that differ only in the direction of their hatching for two
 * different nav rows. That renders differently on every platform, several
 * of them are hard to tell apart at 13px, and a few carry no icon meaning
 * at all.
 *
 * The four promises, and where each one is checked below:
 *
 *   - the artwork is BUNDLED. This deployment runs on a NAS that may have
 *     no route off its own LAN, so an icon that is fetched at draw time is
 *     an empty box on exactly the machine this product is for. Checked by
 *     asserting the shape that cannot fetch (an inline <svg> with its own
 *     <path>) and by refusing every shape that can.
 *   - the geometric glyphs are GONE from the shipped UI, and the
 *     punctuation is untouched. Nineteen non-ASCII code points were in the
 *     source and only fourteen of them were ever icons; the middle dot, the
 *     ellipsis, the two dashes and the section sign are text doing text's
 *     job and replacing them with pictures would be a defect. Both
 *     directions are checked, because a sweep that took the punctuation too
 *     would pass a test that only looked for the icons.
 *   - the swap did not cost the accessibility the badges already had. The
 *     design system's rule is that glyphs and not colour carry meaning and
 *     that the decorative glyph is aria-hidden with the visible word doing
 *     the work for a screen reader. Swapping the artwork must not quietly
 *     turn a labelled badge into an unlabelled picture.
 *   - the licence is recorded. Font Awesome Free icons are CC BY 4.0,
 *     which is an attribution licence, so shipping the artwork obliges this
 *     project to say whose it is and under what terms.
 *
 * Several of these read the sources as text rather than through jsdom, for
 * the reason dark-mode-contrast.test.ts gives about its own scans: what is
 * being asserted is that something is ABSENT everywhere, and a rendered
 * sweep only ever sees the branches some test happens to drive. Most of
 * these glyphs are inside a conditional that fires on a state no unit test
 * arranges.
 */
import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import {
  FONT_AWESOME_ICON_LICENCE,
  FONT_AWESOME_LICENCE_URL,
  FONT_AWESOME_NOTICE,
  FONT_AWESOME_RELEASE,
  ICON_ARTWORK,
  ICON_NAMES,
  Icon,
  iconForLegacyGlyph
} from "@shared/design-system/icons";
import type { IconName } from "@shared/design-system/icons";
import { HEALTH_PRESENTATION, HealthBadge, StatusBadge } from "@shared/components/StatusBadge";
import { WarningBanner } from "@shared/components/WarningBanner";
import { PageHeader } from "@shared/components/PageHeader";
import iconModule from "@shared/design-system/icons.tsx?raw";
import indexHtml from "../../index.html?raw";
import activityStrip from "@shared/pages/ActivityStrip.tsx?raw";
import type { HealthState } from "@shared/types/backup";

/**
 * The fourteen code points that were icons, with the name each one is now
 * drawn under.
 *
 * The two warning glyphs collapse onto one entry on purpose. U+25B2 and
 * U+26A0 were both "something is wrong here" and were drawn differently
 * only because they were written at different times, which is exactly the
 * kind of drift a shared registry exists to stop.
 */
const WAS_AN_ICON: Record<string, IconName> = {
  "▲": "warning",
  "⚠": "warning",
  "✕": "failure",
  "✓": "success",
  "●": "status-active",
  "○": "status-idle",
  "⚙": "settings",
  "◇": "dashboard",
  "▤": "backup-sets",
  "▥": "backups",
  "⊘": "quarantine",
  "≡": "activity",
  "→": "arrow-right",
  "←": "arrow-left"
};

/**
 * The code points that were never icons and must survive untouched.
 *
 * The middle dot separates a value from its unit and one chip from the
 * next, the ellipsis marks a control that opens a confirmation, the two
 * dashes are dashes, and the section sign cites the specification. None of
 * them is a picture of anything.
 */
const IS_PUNCTUATION = ["·", "…", "–", "—", "§"];

/**
 * Every shipped source, read as raw text.
 *
 * import.meta.glob rather than a readdir walk because ui/shared's tsconfig
 * deliberately keeps @types/node out of this program, which is the same
 * reason jsx-literal-escapes.test.tsx and dark-mode-contrast.test.ts read
 * their sources this way. The provider shells are in here too: they are
 * the other half of what an operator sees.
 */
const SHIPPED: Record<string, string> = {
  ...import.meta.glob("../**/*.{ts,tsx}", { query: "?raw", import: "default", eager: true }),
  ...import.meta.glob("../../../../apps/*/frontend/**/*.{ts,tsx}", { query: "?raw", import: "default", eager: true })
};

const SHIPPED_PATHS = Object.keys(SHIPPED)
  .filter((path) => !path.includes(".test.") && !path.includes("/test/"))
  .sort();

/**
 * The one file allowed to write a retired code point, because writing them
 * down is how it retires them: the compatibility table below maps the
 * characters a `glyph` string can still carry onto the artwork that
 * replaces them.
 */
const ICON_REGISTRY = "../design-system/icons.tsx";

/**
 * The files that still carry a retired glyph, each with the reason it is
 * not this change's to fix.
 *
 * Every one of them is owned by another change in EPIC H #623 that is in
 * flight at the same time as this one, and reaching into somebody else's
 * file to swap a character is how two branches collide over a line neither
 * of them is really about.
 *
 * The list is matched EXACTLY rather than treated as a ceiling, in both
 * directions. A file that acquires a glyph fails the sweep, and a file that
 * loses its last one fails this entry, which is what stops the exemption
 * outliving the reason for it: when #622, #624 or #625 lands, deleting a
 * line here is the whole of the follow-up.
 *
 * ActivityStrip is the interesting one, because its badge is drawn by
 * StatusBadge and StatusBadge no longer renders a character. Its glyphs
 * come out as artwork today, through the compatibility table, without the
 * file being touched: see the group at the bottom of this file.
 */
const NOT_THIS_CHANGE_TO_FIX: Record<string, string> = {
  "../pages/ActivityStrip.tsx": "#625 owns the terminal's level vocabulary and this file with it",
  "../pages/BackupSetWizardPage.tsx": "#624 owns the SSH source verification this file is being rebuilt around",
  "../pages/RetentionPolicyCard.tsx": "#622 owns retention's storage destinations and this card with them",
  "../pages/SettingsPage.tsx": "#622 owns the settings surface that has to list the local disk"
};

/** One character as the regex that matches the way TypeScript spells it in
 *  a string literal: a backslash, a `u`, and four hex digits. */
function escapedFormOf(character: string): string {
  return "\\\\u" + character.codePointAt(0)!.toString(16).padStart(4, "0");
}

/** Which retired code points one source still carries, in either spelling. */
export function findRetiredGlyphs(source: string): string[] {
  const found = new Set<string>();
  for (const glyph of Object.keys(WAS_AN_ICON)) {
    if (source.includes(glyph)) found.add(glyph);
    // The same character spelled as an escape. The shipped UI writes them
    // that way far more often than it writes the character itself, and a
    // scan that only looked for the character would have called this
    // change finished with nineteen of its twenty-two warning triangles
    // still in the tree.
    //
    // Four backslashes because the pattern has to reach the regex engine
    // as \\u25b2 and mean "a backslash, then u25b2". Two would reach it as
    // ▲, which the engine reads as the character itself, and the
    // check would silently collapse into the one above it.
    if (new RegExp(escapedFormOf(glyph), "i").test(source)) found.add(glyph);
  }
  return [...found].sort();
}

describe("the sweep itself", () => {
  // An empty result has two explanations and "the scan walked nothing" is
  // the one that would make every assertion below pass for the wrong
  // reason.
  it("reads the shipped sources it claims to check", () => {
    expect(SHIPPED_PATHS.length).toBeGreaterThan(60);
    for (const expected of [
      "../components/StatusBadge.tsx",
      "../components/WarningBanner.tsx",
      "../layouts/AppShell.tsx",
      "../pages/DashboardPage.tsx",
      "../../../../apps/generic/frontend/bootstrap.tsx"
    ]) {
      expect(SHIPPED_PATHS).toContain(expected);
    }
  });

  it("finds a retired glyph written as a character and written as an escape", () => {
    expect(findRetiredGlyphs('const a = "▲";')).toEqual(["▲"]);
    expect(findRetiredGlyphs('const a = "\\u25b2";')).toEqual(["▲"]);
    expect(findRetiredGlyphs('const a = "\\u25B2";')).toEqual(["▲"]);
    expect(findRetiredGlyphs('const a = "nothing to see";')).toEqual([]);
  });

  it("does not report punctuation as a retired glyph", () => {
    for (const mark of IS_PUNCTUATION) {
      expect(findRetiredGlyphs('const a = "' + mark + '";')).toEqual([]);
    }
  });

  it("names the fourteen the issue named, and lands every one of them on real artwork", () => {
    // The count is the issue's own, and it is asserted rather than
    // described so that this table cannot quietly become a different
    // table. The second half is what stops the right-hand column being
    // decorative text: every name here has to be one the registry
    // actually draws.
    expect(Object.keys(WAS_AN_ICON)).toHaveLength(14);
    for (const [glyph, name] of Object.entries(WAS_AN_ICON)) {
      expect(ICON_NAMES, glyph).toContain(name);
    }
  });
});

describe("the geometric glyphs are gone from the shipped UI", () => {
  it("leaves none behind except in the files another change owns", () => {
    const offenders: Record<string, string[]> = {};
    for (const path of SHIPPED_PATHS) {
      if (path === ICON_REGISTRY) continue;
      const found = findRetiredGlyphs(SHIPPED[path]);
      if (found.length > 0) offenders[path] = found;
    }
    expect(Object.keys(offenders).sort()).toEqual(Object.keys(NOT_THIS_CHANGE_TO_FIX).sort());
  });

  it("names a live reason for every file it excuses", () => {
    for (const [path, reason] of Object.entries(NOT_THIS_CHANGE_TO_FIX)) {
      expect(SHIPPED_PATHS).toContain(path);
      expect(reason).toMatch(/#\d+/);
    }
  });
});

describe("the punctuation is untouched", () => {
  // Replacing these would be the other way to fail #621. A sweep that
  // caught the middle dot separating "used · 41%" would have turned a unit
  // into a picture, and no assertion about the icons would have noticed.
  it("leaves the separators, the ellipsis, the dashes and the section sign in the tree", () => {
    for (const mark of IS_PUNCTUATION) {
      const carriers = SHIPPED_PATHS.filter(
        (path) =>
          SHIPPED[path].includes(mark) ||
          new RegExp("\\\\u" + mark.codePointAt(0)!.toString(16).padStart(4, "0"), "i").test(SHIPPED[path])
      );
      expect(carriers.length).toBeGreaterThan(0);
    }
  });

  it("never registers a punctuation mark as something to draw", () => {
    for (const mark of IS_PUNCTUATION) {
      expect(Object.keys(WAS_AN_ICON)).not.toContain(mark);
      expect(iconForLegacyGlyph(mark)).toBeUndefined();
    }
  });
});

describe("nothing is fetched to draw an icon", () => {
  // The deployment this product is for is a NAS on a network that may have
  // no route out of itself. A CDN link, a webfont, a sprite sheet and an
  // <img> all fail the same way there, and all four fail SILENTLY: the
  // layout is intact and the icon is an empty box, which is worse than a
  // visible error because nothing says anything is missing.
  it("draws every icon as an inline path this bundle already carries", () => {
    for (const name of ICON_NAMES) {
      const { container } = render(<Icon name={name} />);
      const svg = container.querySelector("svg");
      expect(svg, name).not.toBeNull();
      expect(svg!.querySelectorAll("path").length, name).toBeGreaterThan(0);
      expect(svg!.querySelector("path")!.getAttribute("d"), name).toBe(ICON_ARTWORK[name].path);
      // A <use> is a reference, and a reference to a sprite file is a
      // fetch wearing SVG's clothes.
      expect(svg!.querySelector("use"), name).toBeNull();
      expect(container.querySelector("img"), name).toBeNull();
    }
  });

  it("carries no address for anything an icon is drawn from", () => {
    expect(iconModule).not.toMatch(/https?:\/\/(?!fontawesome\.com|creativecommons\.org)/);
    expect(iconModule).not.toMatch(/url\(/);
    expect(iconModule).not.toMatch(/@font-face/);
    expect(iconModule).not.toMatch(/\bfetch\(/);
  });

  it("keeps every icon's artwork in the module rather than in a file to load", () => {
    for (const name of ICON_NAMES) {
      expect(iconModule, name).toContain(ICON_ARTWORK[name].path);
    }
  });
});

describe("the icons are decorative and the words carry the meaning", () => {
  it("hides every icon from assistive technology", () => {
    for (const name of ICON_NAMES) {
      const { container } = render(<Icon name={name} />);
      const svg = container.querySelector("svg")!;
      expect(svg.getAttribute("aria-hidden"), name).toBe("true");
      // Without this an SVG is a tab stop in some browsers, so the way out
      // of a page would run through fourteen pictures.
      expect(svg.getAttribute("focusable"), name).toBe("false");
    }
  });

  it("keeps a health badge's word as its whole accessible text", () => {
    for (const state of Object.keys(HEALTH_PRESENTATION) as HealthState[]) {
      const { container } = render(<HealthBadge state={state} />);
      expect(container.querySelector("svg"), state).not.toBeNull();
      expect(container.textContent, state).toBe(HEALTH_PRESENTATION[state].label);
    }
  });

  it("keeps a banner's words when the tone glyph becomes a picture", () => {
    const { container } = render(
      <WarningBanner tone="danger" eyebrow="HALTED" title="Nothing got through">
        <p>Every transfer in the last pass failed.</p>
      </WarningBanner>
    );
    expect(container.querySelector("svg[aria-hidden='true']")).not.toBeNull();
    expect(container.textContent).toContain("HALTED");
    expect(container.textContent).toContain("Nothing got through");
  });

  it("leaves a back link named by its words alone", () => {
    // It used to be named "← Backups", because the arrow was text
    // inside the button and a button's accessible name is its text. A
    // screen reader read the arrow out.
    const { getByRole, container } = render(
      <PageHeader title="A backup" back={{ label: "Backups", onClick: () => {} }} />
    );
    expect(getByRole("button", { name: "Backups" })).toBeTruthy();
    expect(container.querySelector("button svg[aria-hidden='true']")).not.toBeNull();
  });

  it("takes its colour from whatever token the caller set rather than naming one", () => {
    // The rule dark-mode-contrast.test.ts enforces for text applies to
    // artwork for the same reason: a literal colour cannot invert with the
    // theme. currentColor is how an icon inherits the token its badge
    // already chose.
    for (const name of ICON_NAMES) {
      const { container } = render(<Icon name={name} />);
      expect(container.querySelector("svg")!.getAttribute("fill"), name).toBe("currentColor");
    }
    expect(iconModule).not.toMatch(/fill:\s*"?#[0-9a-fA-F]{3,8}/);
  });
});

describe("the badge in the file this change may not touch", () => {
  // ActivityStrip belongs to #625 this cycle, so its pill still passes a
  // character to StatusBadge. StatusBadge translates it, which is what
  // makes that file's badges artwork today rather than after the follow-up.
  //
  // Worth pinning rather than leaving to be noticed: the day that file is
  // rewritten the translation stops being exercised, and this group is
  // what says so.
  it("translates every character its pill can pass into artwork", () => {
    const passed = findRetiredGlyphs(activityStrip);
    expect(passed.length).toBeGreaterThan(0);
    for (const glyph of passed) {
      expect(iconForLegacyGlyph(glyph), glyph).toBeDefined();
    }
  });

  it("draws artwork for a badge given a character rather than a name", () => {
    const { container } = render(<StatusBadge tone="warn" glyph="▲">NOT REPORTING</StatusBadge>);
    const svg = container.querySelector("svg");
    expect(svg).not.toBeNull();
    expect(svg!.querySelector("path")!.getAttribute("d")).toBe(ICON_ARTWORK.warning.path);
    expect(container.textContent).toBe("NOT REPORTING");
  });
});

describe("the licence the artwork ships under is recorded", () => {
  const COMPLIANCE = import.meta.glob("../../../../docs/compliance/*.md", {
    query: "?raw",
    import: "default",
    eager: true
  }) as Record<string, string>;
  const ATTRIBUTION =
    COMPLIANCE[
      Object.keys(COMPLIANCE).find((path) => path.endsWith("/bundled-icon-artwork.md")) ?? ""
    ] ?? "";

  it("has a file to record it in", () => {
    // A missing file reads back as "" here rather than throwing, and an
    // empty reading is a refusal: without this the four assertions below
    // would each fail for their own reason and none of them would say the
    // record is simply absent.
    expect(Object.keys(COMPLIANCE).length).toBeGreaterThan(1);
    expect(ATTRIBUTION.length).toBeGreaterThan(400);
  });

  it("names the licence, its text and whose work this is", () => {
    // CC BY 4.0 section 3(a)(1) asks for the creator, the licence, a URI to
    // its text and a statement of whether the material was modified. An
    // SPDX id on its own is not that.
    expect(ATTRIBUTION).toContain(FONT_AWESOME_ICON_LICENCE);
    expect(ATTRIBUTION).toContain(FONT_AWESOME_LICENCE_URL);
    expect(ATTRIBUTION).toContain("Fonticons");
    expect(ATTRIBUTION).toContain(FONT_AWESOME_RELEASE);
    expect(ATTRIBUTION).toMatch(/unmodified|not modified|verbatim/i);
  });

  it("lists exactly the artwork this product actually ships", () => {
    // Both directions. A record that names icons the registry dropped is
    // as wrong as one that misses icons it gained, and only one of those
    // is a licensing problem, so the check cannot be a one-way one.
    const listed = [...ATTRIBUTION.matchAll(/^\s*[-|]\s*`([a-z-]+\/[a-z-]+)`/gm)].map((m) => m[1]).sort();
    const shipped = ICON_NAMES.map((name) => ICON_ARTWORK[name].fontAwesome).sort();
    expect(listed).toEqual(shipped);
  });

  it("keeps Font Awesome's own attribution comment beside the artwork", () => {
    // The licence asks for the embedded comments not to be stripped out of
    // the files, in as many words. This module is where those files' path
    // data ended up, so this is where the comment belongs.
    expect(FONT_AWESOME_NOTICE).toContain("Font Awesome Free " + FONT_AWESOME_RELEASE);
    expect(FONT_AWESOME_NOTICE).toContain("Fonticons, Inc.");
    expect(iconModule).toContain(FONT_AWESOME_LICENCE_URL);
  });

  it("carries the attribution into the page a recipient is served", () => {
    // The one that is not about the source tree. Everything else in this
    // group is a file somebody reads in the repository; this is the
    // attribution reaching whoever has the built artifact and nothing
    // else, which is who CC BY 4.0 section 3(a) is about. A JavaScript
    // comment could not do this job: minification removes them, and an
    // exported constant nothing imports is dropped by the bundler.
    expect(indexHtml).toContain("Font Awesome Free " + FONT_AWESOME_RELEASE);
    expect(indexHtml).toContain("Fonticons, Inc.");
    expect(indexHtml).toContain(FONT_AWESOME_LICENCE_URL);
    expect(indexHtml).toMatch(/unmodified/i);
  });
});
