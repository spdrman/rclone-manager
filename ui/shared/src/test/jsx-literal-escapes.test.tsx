import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import ts from "typescript";

/**
 * Issue #257. A unicode escape written where JSX does not interpret it
 * reaches the operator as the six characters `\u2715` instead of the glyph.
 *
 * There are exactly two positions where that happens, and this file proves
 * both of them against React itself below, rather than asserting a style
 * rule nobody can check:
 *
 *   <span>\u2715</span>        JSX text        - renders "\u2715"
 *   <StatusBadge glyph="\u2715">  JSX attribute   - renders "\u2715"
 *   <span>{"\u2715"}</span>    braced literal  - renders the glyph
 *
 * JSX text and JSX attribute strings are HTML-ish, not JavaScript: entities
 * are decoded, backslash escapes are not. Only a braced string literal is
 * read by JavaScript before React ever sees it.
 *
 * The guard is deliberately the CLASS and not the sites #257 listed. #257
 * found four, all of them JSX text. Scanning for the class found four more
 * the same operator would have hit next: a "Timezone \u00b7 week start" label
 * on every backup set's detail page, and the three health badges on the
 * dashboard itself ("\u25b2 1 set stale", "\u2715 1 set halted"), all in the
 * attribute position nobody had thought to look at.
 *
 * It scans source rather than rendered output on purpose. A rendered sweep
 * only ever sees the branches some test happens to drive, and seven of
 * these eight sites are inside a conditional: one of them, ErrorState's,
 * renders only after a request has failed. A parse of the tree sees every
 * branch whether or not anything renders it.
 */

/**
 * Every .tsx that ships, read as raw source at transform time.
 *
 * import.meta.glob rather than a readdir walk because ui/shared's
 * tsconfig deliberately does not carry @types/node (its own comment says
 * why: putting "node" in "types" makes Node's globals visible to every
 * component under src/), and vite already has the file list.
 *
 * The provider shells are in here too: they are the other half of what
 * reaches an operator, and they are JSX written by the same hands.
 */
const SHIPPED_TSX: Record<string, string> = {
  ...import.meta.glob("../**/*.tsx", { query: "?raw", import: "default", eager: true }),
  ...import.meta.glob("../../../../apps/*/frontend/**/*.tsx", { query: "?raw", import: "default", eager: true })
};

/** This file, and the other tests, are not shipped UI: they are allowed to
 *  write the broken forms, and this one has to, to prove they are broken. */
function isShipped(path: string): boolean {
  return !path.includes(".test.") && !path.includes("/test/");
}

const UNICODE_ESCAPE = /\\u[0-9a-fA-F]{4}/;

interface Finding {
  file: string;
  line: number;
  where: "JSX text" | "JSX attribute";
  source: string;
}

/** Every place in one file where a unicode escape was written somewhere JSX
 *  will not interpret it. Parsed, not pattern-matched: the same characters
 *  inside a braced string literal, an object literal or a plain TypeScript
 *  string are correct, and a regex over raw source cannot tell those apart
 *  from the two that are not. */
export function findUninterpretedEscapes(fileName: string, source: string): Finding[] {
  const sourceFile = ts.createSourceFile(fileName, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const findings: Finding[] = [];

  const record = (node: ts.Node, where: Finding["where"]) => {
    findings.push({
      file: fileName,
      line: sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile)).line + 1,
      where,
      source: node.getText(sourceFile).trim()
    });
  };

  const visit = (node: ts.Node) => {
    if (ts.isJsxText(node) && UNICODE_ESCAPE.test(node.getText(sourceFile))) {
      record(node, "JSX text");
    }
    if (
      ts.isJsxAttribute(node) &&
      node.initializer !== undefined &&
      ts.isStringLiteral(node.initializer) &&
      UNICODE_ESCAPE.test(node.initializer.getText(sourceFile))
    ) {
      record(node, "JSX attribute");
    }
    ts.forEachChild(node, visit);
  };

  visit(sourceFile);
  return findings;
}

describe("what JSX does with a unicode escape", () => {
  it("does not interpret one written as JSX text", () => {
    const { container } = render(<span>\u2713</span>);
    expect(container.textContent).toBe("\\u2713");
  });

  it("does not interpret one written as a JSX attribute string", () => {
    const { container } = render(<span data-glyph="\u2713" />);
    expect(container.querySelector("span")?.getAttribute("data-glyph")).toBe("\\u2713");
  });

  it("does interpret one written as a braced string literal", () => {
    const { container } = render(<span>{"\u2713"}</span>);
    expect(container.textContent).toBe("✓");
  });
});

describe("the scanner", () => {
  it("reports an escape in JSX text", () => {
    const findings = findUninterpretedEscapes("probe.tsx", String.raw`const a = <span>\u2715</span>;`);
    expect(findings.map((f) => f.where)).toEqual(["JSX text"]);
  });

  it("reports an escape in a JSX attribute string", () => {
    const findings = findUninterpretedEscapes("probe.tsx", String.raw`const a = <Badge glyph="\u25b2">x</Badge>;`);
    expect(findings.map((f) => f.where)).toEqual(["JSX attribute"]);
  });

  it("does not report the three forms that are correct", () => {
    const correct = [
      String.raw`const a = <span>{"\u2715"}</span>;`,
      String.raw`const a = <Badge glyph={"\u25b2"}>x</Badge>;`,
      String.raw`const GLYPH = { ok: "\u2713" }; const a = <Badge glyph={GLYPH.ok}>x</Badge>;`
    ];
    for (const source of correct) {
      expect(findUninterpretedEscapes("probe.tsx", source)).toEqual([]);
    }
  });
});

describe("the shipped UI", () => {
  const files = Object.keys(SHIPPED_TSX).filter(isShipped).sort();

  /** An empty result has two explanations, and "the glob matched nothing"
   *  is the one that would make the assertion below pass for the wrong
   *  reason. */
  it("actually reads the files it claims to check", () => {
    expect(files.length).toBeGreaterThan(30);
    for (const expected of [
      "../components/EmptyState.tsx",
      "../components/HealthSummary.tsx",
      "../pages/BackupSetDetailPage.tsx",
      "../pages/CatalogRecoveryPage.tsx",
      "../pages/QuarantinePage.tsx",
      "../../../../apps/generic/frontend/bootstrap.tsx"
    ]) {
      expect(files).toContain(expected);
    }
  });

  it("never writes a unicode escape where JSX will not interpret it", () => {
    const findings = files.flatMap((file) => findUninterpretedEscapes(file, SHIPPED_TSX[file]));
    const readable = findings.map((f) => f.file + ":" + f.line + " (" + f.where + ") " + f.source);
    expect(readable).toEqual([]);
  });
});
