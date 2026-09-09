/**
 * The typeface is served from the image now, and issue #631 (H2.5) is four
 * promises about that rather than one about how it looks.
 *
 * Until this, index.html asked fonts.googleapis.com for IBM Plex Sans and
 * IBM Plex Mono on every page load. That is the same defect #621 is about
 * one layer up, and it is quieter: a missing icon is an empty box somebody
 * reports, while a missing typeface is a page that renders in Helvetica
 * and looks fine. This product runs on a NAS that may have no route off
 * its own LAN, on purpose, so the request fails there, nothing errors, and
 * every measurement the design makes against Plex is off by whatever the
 * browser picked instead.
 *
 * The four promises, and where each one is checked below:
 *
 *   - nothing is FETCHED from a third party. Checked by refusing both
 *     Google font hosts by name and then, more usefully, by refusing any
 *     absolute URL in a subresource attribute at all, because the next CDN
 *     somebody adds will not be called fonts.googleapis.com.
 *   - the faces are DECLARED where the families are, and every one of them
 *     points at a file this repository actually ships. A src pointing at a
 *     path with nothing behind it fails exactly the way the CDN did.
 *   - the fallback stays REAL. A face can still fail to load (a truncated
 *     copy, a proxy that mangles woff2, a glyph outside the subset), and
 *     what happens then has to be a legible page rather than a page of
 *     tofu. That is two separate things: a non-Plex family after the Plex
 *     one in each stack, and font-display: swap so a face that never
 *     arrives does not hold the paint.
 *   - the weight set is a DECISION. Seven faces is what the retired link
 *     asked for, and the reason it is still seven is written on the test
 *     that checks it: every one of them is reached by something in the
 *     shipped sources. Both directions, so a weight added to a component
 *     with no face behind it is a red build and a face nothing asks for is
 *     one too.
 *
 * The scans read the sources as text, for the reason icon-artwork.test.tsx
 * gives about its own: what is being asserted is that something is ABSENT
 * everywhere, and a rendered sweep only sees the branches some test
 * happens to drive.
 *
 * What is NOT here: the proof that a browser with egress blocked renders
 * the page. That is scripts/offline-bundle.test.mjs, which builds the
 * bundle, serves it over loopback with every non-loopback connection
 * refused at the socket, and walks it. This file is about the source; that
 * one is about the artifact.
 */
import { describe, expect, it } from "vitest";
import indexHtml from "../../index.html?raw";
import typographyCss from "@shared/design-system/typography.css?raw";

/**
 * The vendored files, keyed by the path the built page asks for.
 *
 * import.meta.glob rather than a readdir walk because ui/shared's tsconfig
 * deliberately keeps @types/node out of this program, which is the same
 * reason icon-artwork.test.tsx and dark-mode-contrast.test.ts read their
 * sources this way. Not eager: the keys are the question, and eagerly
 * importing seven woff2 files would pull a couple of hundred KB of binary
 * through the transform for nothing.
 */
const VENDORED = Object.keys(import.meta.glob("../../public/fonts/*")).map((path) =>
  path.replace("../../public", "")
);

/** Every shipped source, read as raw text. The provider shells too. */
const SHIPPED: Record<string, string> = {
  ...import.meta.glob("../**/*.{ts,tsx,css}", { query: "?raw", import: "default", eager: true }),
  ...import.meta.glob("../../../../apps/*/frontend/**/*.{ts,tsx,css}", {
    query: "?raw",
    import: "default",
    eager: true
  })
};

const SHIPPED_PATHS = Object.keys(SHIPPED)
  .filter((path) => !path.includes(".test.") && !path.includes("/test/"))
  .sort();

/**
 * The faces this product ships, as (family, weight).
 *
 * This is the weight decision, and it is the same set the Google Fonts
 * link asked for, which is deliberate: this change is meant to be
 * invisible on a machine that had internet, so the shipped set is the set
 * that used to be downloaded. What made it worth re-deciding rather than
 * copying is that the image carries the bundle six times over (the one
 * compiled into the binary plus the five per-provider bundles at
 * /ui/bundles), so a face costs six times its own size against the gated
 * image budget.
 *
 * Each is here because something reaches it. Sans 700 is the one that
 * looks droppable from a grep and is not: three explicit declarations in
 * SSHAuthWizard, and every <strong> in the product, which the user agent
 * renders at bolder and which computes to 700 from body text at 400.
 */
const FACES: ReadonlyArray<{ family: string; weight: number }> = [
  { family: "IBM Plex Sans", weight: 400 },
  { family: "IBM Plex Sans", weight: 500 },
  { family: "IBM Plex Sans", weight: 600 },
  { family: "IBM Plex Sans", weight: 700 },
  { family: "IBM Plex Mono", weight: 400 },
  { family: "IBM Plex Mono", weight: 500 },
  { family: "IBM Plex Mono", weight: 600 }
];

/** One parsed @font-face block. */
type Face = { family: string; weight: number; src: string; display: string; range: string; style: string };

function declaredFaces(css: string): Face[] {
  const out: Face[] = [];
  for (const block of css.matchAll(/@font-face\s*\{([^}]*)\}/g)) {
    const body = block[1];
    const read = (property: string) => {
      const m = body.match(new RegExp(`(?:^|;)\\s*${property}\\s*:([^;]*)`));
      return m ? m[1].trim() : "";
    };
    out.push({
      family: read("font-family").replace(/["']/g, ""),
      weight: Number(read("font-weight")),
      src: read("src"),
      display: read("font-display"),
      range: read("unicode-range"),
      style: read("font-style")
    });
  }
  return out;
}

const DECLARED = declaredFaces(typographyCss);

describe("the page asks nobody for a typeface", () => {
  it("names neither Google font host anywhere in the document", () => {
    for (const host of ["fonts.googleapis.com", "fonts.gstatic.com"]) {
      expect(
        indexHtml.includes(host),
        `index.html still reaches ${host}, which is a request that fails on an isolated deployment and takes the typeface with it`
      ).toBe(false);
    }
  });

  it("warms up no connection to anywhere", () => {
    // preconnect and dns-prefetch exist only to make a third-party fetch
    // faster. One left behind after the fetch it belonged to is gone is
    // still an outbound request on every page load, and it is the kind of
    // line a sweep for the host name misses.
    expect(
      /rel=["'](?:preconnect|dns-prefetch)["']/.test(indexHtml),
      "index.html still preconnects somewhere, which is an outbound request on every page load for a product that runs on private networks on purpose"
    ).toBe(false);
  });

  it("loads every subresource from its own origin", () => {
    // The general rule behind the two above: the next CDN somebody adds
    // will not be called fonts.googleapis.com.
    const remote: string[] = [];
    for (const m of indexHtml.matchAll(/(?:href|src)=["']([^"']+)["']/g)) {
      if (/^(?:[a-z][a-z0-9+.-]*:)?\/\//i.test(m[1])) remote.push(m[1]);
    }
    expect(remote, `index.html loads ${remote.join(", ")} from off this deployment`).toEqual([]);
  });

  it("says in the document who the typeface belongs to and where its licence is", () => {
    // The same argument #621's icon comment makes: minification strips
    // comments out of the JavaScript, so the HTML is the one place an
    // attribution reaches somebody holding the built artifact rather than
    // the source.
    for (const phrase of ["IBM Plex", "SIL Open Font License", "/fonts/LICENSE.txt", "#631"]) {
      expect(
        indexHtml.includes(phrase),
        `index.html's attribution never mentions ${phrase}, and the built page is where that attribution has to reach a recipient`
      ).toBe(true);
    }
  });
});

describe("the faces are declared where the families are", () => {
  it("declares one face per family and weight, and no others", () => {
    const want = FACES.map((f) => `${f.family} ${f.weight}`).sort();
    const got = DECLARED.map((f) => `${f.family} ${f.weight}`).sort();
    expect(got).toEqual(want);
  });

  it("points every face at a file this repository ships", () => {
    expect(DECLARED.length, "typography.css declares no @font-face at all, so this check read nothing").toBeGreaterThan(0);
    for (const face of DECLARED) {
      const url = face.src.match(/url\(["']?([^"')]+)["']?\)/)?.[1] ?? "";
      expect(url, `${face.family} ${face.weight} declares a src this test cannot read: ${face.src}`).not.toBe("");
      expect(
        /^(?:[a-z][a-z0-9+.-]*:)?\/\//i.test(url),
        `${face.family} ${face.weight} is fetched from ${url}, which is the defect this issue is about`
      ).toBe(false);
      expect(
        VENDORED.includes(url),
        `${face.family} ${face.weight} points at ${url} and this repository ships ${VENDORED.join(", ") || "no font file at all"}`
      ).toBe(true);
    }
  });

  it("ships the licence text beside the fonts", () => {
    // OFL-1.1 §2 lets the fonts be redistributed "provided that each copy
    // contains the above copyright notice and this license". Each copy
    // includes the ones inside the image, so the licence sits in the
    // public directory with the woff2 files and is copied into every
    // bundle by the same build step rather than by a Dockerfile line
    // somebody has to remember.
    expect(
      VENDORED.includes("/fonts/LICENSE.txt"),
      "the fonts ship without the licence text beside them, and OFL-1.1 §2 asks for it in every copy"
    ).toBe(true);
  });
});

describe("a face that does not arrive still leaves a legible page", () => {
  it("keeps a real fallback behind each Plex family", () => {
    for (const [token, plex] of [
      ["--font-sans", "IBM Plex Sans"],
      ["--font-mono", "IBM Plex Mono"]
    ]) {
      const stack = typographyCss.match(new RegExp(`${token}\\s*:([^;]*)`))?.[1] ?? "";
      expect(stack, `typography.css declares no ${token}`).not.toBe("");
      const families = stack.split(",").map((f) => f.trim().replace(/["']/g, ""));
      expect(families[0], `${token} does not lead with ${plex}`).toBe(plex);
      expect(
        families.slice(1).length,
        `${token} is ${stack.trim()}, so a face that fails to load leaves the browser to pick anything`
      ).toBeGreaterThan(0);
    }
  });

  it("lets a face that never arrives be overtaken rather than waited for", () => {
    for (const face of DECLARED) {
      expect(
        face.display,
        `${face.family} ${face.weight} declares no font-display, so the default block period holds the first paint on a file that may not be coming`
      ).toBe("swap");
    }
  });

  it("says which characters each face actually covers", () => {
    // These are the Latin1 subsets, not the complete faces, and the
    // subsetting is what keeps the cost payable. unicode-range is how the
    // browser knows that: without it a page whose text is inside the
    // subset behaves the same, and one that reaches outside it gets a
    // download that cannot help.
    for (const face of DECLARED) {
      expect(
        face.range,
        `${face.family} ${face.weight} is a subset and declares no unicode-range, so nothing says what it is a subset of`
      ).not.toBe("");
    }
  });
});

describe("the weight set is a decision and not an inheritance", () => {
  /** Every numeric font-weight the shipped sources ask for. */
  function weightsAskedFor(): Map<number, string[]> {
    const found = new Map<number, string[]>();
    const note = (weight: number, where: string) => {
      found.set(weight, [...(found.get(weight) ?? []), where]);
    };
    for (const path of SHIPPED_PATHS) {
      // The @font-face blocks are stripped before the sweep. They carry a
      // font-weight each, and counting those would make the second case
      // below circular: every shipped face would prove itself asked for by
      // its own declaration, and a face nothing draws with would pass.
      const source = SHIPPED[path].replace(/@font-face\s*\{[^}]*\}/g, "");
      for (const m of source.matchAll(/font-weight\s*:([^;{}]*)/g)) {
        for (const n of m[1].matchAll(/\d{3}/g)) note(Number(n[0]), path);
      }
      for (const m of source.matchAll(/fontWeight\s*:([^,}\n]*)/g)) {
        for (const n of m[1].matchAll(/\d{3}/g)) note(Number(n[0]), path);
      }
      // <strong> and <b> carry no declaration and are not weightless: the
      // user agent stylesheet renders them at `bolder`, which computes to
      // 700 against body text at 400. This is the reason sans 700 is
      // shipped, and it is invisible to a grep for the number.
      if (/<(?:strong|b)[\s>]/.test(source)) note(700, path);
    }
    return found;
  }

  it("uses no weight this product has no face for", () => {
    const shipped = new Set(FACES.map((f) => f.weight));
    for (const [weight, where] of weightsAskedFor()) {
      expect(
        shipped.has(weight),
        `${where.join(", ")} asks for weight ${weight} and no face is shipped at it, so the browser picks the nearest face it has and the text renders at a weight nobody chose`
      ).toBe(true);
    }
  });

  it("ships no weight nothing asks for", () => {
    const asked = new Set(weightsAskedFor().keys());
    for (const weight of new Set(FACES.map((f) => f.weight))) {
      expect(
        asked.has(weight),
        `a face is shipped at weight ${weight} and nothing in the shipped sources asks for it; that is six copies of a file nobody draws with, because the image carries the bundle once per provider`
      ).toBe(true);
    }
  });

  it("reads something at all", () => {
    // The positive control for the two above. Both sweep a set derived
    // from a regexp over globbed sources, and both pass cheerfully over an
    // empty set.
    expect(SHIPPED_PATHS.length).toBeGreaterThan(50);
    expect(weightsAskedFor().size).toBeGreaterThan(1);
  });
});
