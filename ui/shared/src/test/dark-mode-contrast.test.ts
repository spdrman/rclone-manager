/**
 * Dark mode never draws black text (issue #618).
 *
 * These read the sources as text rather than through jsdom, on purpose.
 * jsdom does not implement the cascade for user-agent defaults and has no
 * notion of `color-scheme` at all, so a rendered assertion would pass
 * against exactly the bug it is meant to catch. What went wrong is a
 * declaration that is absent, and absence is a thing the source can be
 * asked about directly.
 *
 * They come in through Vite's `?raw` rather than node:fs because
 * tsconfig.json deliberately keeps @types/node out of this program, so
 * that Node's globals are not type-visible to every component under src/.
 * `?raw` and import.meta.glob are typed by vite/client, which is already
 * the one entry in "types".
 *
 * The defect they pin: nothing declared `color-scheme`, so every native
 * control took the user agent's light default (black on white) whatever
 * `data-theme` said, and any component that styled a control without
 * naming a colour inherited it. It reads correctly in light mode, which is
 * why it survived review.
 */
import { describe, expect, it } from "vitest";
import tokens from "@shared/design-system/tokens.css?raw";
import components from "@shared/design-system/components.css?raw";
import wizard from "@shared/components/SSHAuthWizard.tsx?raw";

/** Every component source under src/, test files excluded. */
const SOURCES = Object.entries(
  import.meta.glob("../**/*.{ts,tsx}", { query: "?raw", import: "default", eager: true }) as Record<string, string>
).filter(([path]) => !path.includes(".test."));

/** The body of a top-level rule, by its selector. */
function block(css: string, selector: string): string {
  const at = css.indexOf(selector + " {");
  if (at < 0) return "";
  return css.slice(at, css.indexOf("}", at));
}

describe("the themes tell the browser which one they are", () => {
  it("declares a light color-scheme on the default theme", () => {
    expect(block(tokens, ":root")).toMatch(/color-scheme:\s*light/);
  });

  it("declares a dark color-scheme on the dark theme", () => {
    expect(block(tokens, '[data-theme="dark"]')).toMatch(/color-scheme:\s*dark/);
  });
});

describe("a native control cannot land on the user-agent default", () => {
  it("gives bare inputs, selects, textareas and buttons the theme's text colour", () => {
    // Low specificity on purpose: one element selector, so every existing
    // class rule and every inline style still wins. A floor, not an
    // override.
    expect(block(components, "input, select, textarea, button")).toMatch(/color:\s*var\(--text\)/);
  });
});

describe("the wizard the report named", () => {
  // The SSH auth wizard styles its own controls instead of using the
  // .input/.btn classes, and set a border and a background without ever
  // naming a colour. The floor above catches that now, but this is the
  // surface the bug was reported against and it should say what it draws
  // rather than depending on a rule one file over.
  function styleObject(name: string): string {
    const at = wizard.indexOf("const " + name + ": CSSProperties = {");
    expect(at).toBeGreaterThan(-1);
    return wizard.slice(at, wizard.indexOf("};", at));
  }

  it("names a colour and a background on its text fields", () => {
    const input = styleObject("input");
    expect(input).toMatch(/color:\s*"var\(--text\)"/);
    expect(input).toMatch(/background:\s*"var\(--surface-2\)"/);
  });

  it("names a colour on its buttons, which otherwise take the UA buttontext", () => {
    expect(styleObject("btn")).toMatch(/color:\s*"var\(--text\)"/);
  });
});

describe("no component hardcodes a text colour", () => {
  // A literal colour cannot invert with the theme, which is the whole
  // failure mode here. The accent is the sharpest case: it is dark in
  // light mode and LIGHT in dark mode (0.52 against 0.70 lightness), so
  // text on it has to move, and a hardcoded white silently stops being
  // readable in exactly one of the two themes.
  it("uses tokens rather than literal hex or named colours for text", () => {
    const offenders: string[] = [];
    for (const [path, text] of SOURCES) {
      for (const m of text.matchAll(/color:\s*"(#[0-9a-fA-F]{3,8}|white|black)"/g)) {
        offenders.push(path + ": " + m[0]);
      }
    }
    // A scan that walked nothing would report the same empty list as a
    // clean tree, so the reach is asserted too. It was 85 files when this
    // was written and the floor is deliberately well under that.
    expect(SOURCES.length).toBeGreaterThan(60);
    expect(offenders).toEqual([]);
  });
});
