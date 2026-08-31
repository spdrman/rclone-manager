// A subset build must not destroy the bundles it was not asked to build.
//
// build-bundles.mjs used to remove the whole dist-bundles tree before its
// loop, so `npm run build:bundles synology` deleted the six bundles a
// previous full run had produced. That is an unconditional recursive
// delete on an argument-dependent path, which is the shape worth a test
// of its own.
//
// The script is exercised as a real subprocess against a fixture copy of
// itself: it resolves its own root from import.meta.url, so a copy under
// a temp directory reads and writes there and never touches this
// checkout. `npm run build` is answered by a stub on PATH that writes the
// dist/index.html the script insists on, which keeps this a test about
// the delete rather than about Vite.
import { describe, it, expect } from "vitest";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, writeFileSync, existsSync, copyFileSync, chmodSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));

function fixture() {
  const root = mkdtempSync(resolve(tmpdir(), "build-bundles-"));

  mkdirSync(resolve(root, "scripts"), { recursive: true });
  copyFileSync(resolve(here, "build-bundles.mjs"), resolve(root, "scripts", "build-bundles.mjs"));

  // The stub `npm`: writes the app shell the script checks for, into the
  // shared dist/ the real Vite build would write to.
  const bin = resolve(root, "bin");
  mkdirSync(bin, { recursive: true });
  const npm = resolve(bin, "npm");
  writeFileSync(npm, '#!/bin/sh\nmkdir -p "$(pwd)/dist"\necho "$VITE_PLATFORM" > "$(pwd)/dist/index.html"\n');
  chmodSync(npm, 0o755);

  return { root, bin };
}

function run(fx, args) {
  return spawnSync(process.execPath, [resolve(fx.root, "scripts", "build-bundles.mjs"), ...args], {
    cwd: fx.root,
    encoding: "utf8",
    env: { ...process.env, PATH: `${fx.bin}:${process.env.PATH}` }
  });
}

describe("build-bundles", () => {
  it("leaves the bundles a subset build was not asked for alone, and still replaces the one it was", () => {
    const fx = fixture();

    // What a previous full run left behind: one bundle this build does
    // not name, and stale output for the one it does.
    mkdirSync(resolve(fx.root, "dist-bundles", "generic"), { recursive: true });
    writeFileSync(resolve(fx.root, "dist-bundles", "generic", "index.html"), "generic");
    mkdirSync(resolve(fx.root, "dist-bundles", "synology"), { recursive: true });
    writeFileSync(resolve(fx.root, "dist-bundles", "synology", "stale-chunk.js"), "stale");

    const result = run(fx, ["synology"]);
    expect(result.status, result.stderr).toBe(0);

    expect(
      existsSync(resolve(fx.root, "dist-bundles", "generic", "index.html")),
      "a subset build deleted a bundle it was never asked to build"
    ).toBe(true);

    // The control, and the half that keeps the assertion above honest:
    // dropping the removal entirely would satisfy it while leaving stale
    // output from an older build mixed into the new bundle.
    expect(existsSync(resolve(fx.root, "dist-bundles", "synology", "index.html"))).toBe(true);
    expect(
      existsSync(resolve(fx.root, "dist-bundles", "synology", "stale-chunk.js")),
      "the rebuilt bundle still carries a file from the previous build, so the removal no longer happens at all"
    ).toBe(false);
  }, 60_000);

  it("refuses an unknown provider before deleting anything", () => {
    const fx = fixture();
    mkdirSync(resolve(fx.root, "dist-bundles", "generic"), { recursive: true });
    writeFileSync(resolve(fx.root, "dist-bundles", "generic", "index.html"), "generic");

    const result = run(fx, ["nosuchprovider"]);
    expect(result.status).toBe(2);
    expect(existsSync(resolve(fx.root, "dist-bundles", "generic", "index.html"))).toBe(true);
  }, 60_000);
});
