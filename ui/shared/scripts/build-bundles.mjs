#!/usr/bin/env node
// Builds one UI bundle per provider into dist-bundles/<provider>/ (issue
// #180, owned by #167).
//
// `npm run build` produces exactly one bundle, for whichever provider
// VITE_PLATFORM named, and that single bundle is what
// apps/generic/webui embeds. That is still the right shape for the
// canonical image: it ships the generic bridge, and its size is gated.
//
// What this script adds is the other half of the runtime-selection
// mechanism. A provider PACKAGE (a .spk, a .UPK, a NAS app-store entry)
// can build its own bridge here and ship the directory beside the exact
// same core binary, then point the web host at it with --ui-dir, or lay
// the whole dist-bundles/ tree down and point --ui-root at it. The binary
// never changes, which is what section 3.7 requires and what
// apps/generic/tests/uibundle proves.
//
// Deliberately NOT wired into container/Dockerfile, and the arithmetic
// says over the budget, not inside it.
//
// Each bundle is about 352 KiB (360,448 bytes). The gate is 1.05x the
// recorded baseline image of 43,008,762 bytes, so the ceiling is
// 45,159,200. Which image the headroom is measured from is the whole
// question, and it moves every time the binary grows: against this
// change's own measured image of 43,074,298 (docs/runtime-contract.md's
// metrics table) the headroom is 2,084,902 bytes. The canonical image
// already carries the generic bundle, so shipping the rest means six
// more at 2,162,688 bytes, which is OVER the ceiling by about 78 KB. All
// seven would be over by about 438 KB. The older "inside by roughly
// 12 KB" reading came from measuring against the baseline image rather
// than the one this change actually produces.
//
// So this is not a coin toss on a gate, it is a gate that fails.
// Converting each adapter to ship its own bundle is #169's work, and the
// image-size question is worth re-measuring there rather than
// pre-empting here.
import { spawnSync } from "node:child_process";
import { rmSync, mkdirSync, cpSync, existsSync } from "node:fs";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = resolve(here, "..");

// Keep in sync with e2e/fixtures.ts's PROVIDERS export and with
// apps/common/platform/capabilities.PlatformID, duplicated here for the
// same reason e2e-all-providers.mjs duplicates it: no TypeScript loader.
const PROVIDERS = ["generic", "ugos", "synology", "truenas", "unraid", "openmediavault", "proxmox"];

const only = process.argv.slice(2).filter((a) => !a.startsWith("-"));
const targets = only.length > 0 ? only : PROVIDERS;

for (const provider of targets) {
  if (!PROVIDERS.includes(provider)) {
    console.error(`build-bundles: unknown provider ${provider}; known: ${PROVIDERS.join(", ")}`);
    process.exit(2);
  }
}

// Created once, and NEVER removed wholesale: `build:bundles synology`
// used to delete every bundle a previous full run had produced, because
// the removal was scoped to the tree rather than to the targets asked
// for. Each target's own directory is removed inside the loop below,
// after its build succeeds, so a subset build replaces exactly what it
// rebuilds and a failed build leaves the previous good bundle alone.
const outRoot = resolve(root, "dist-bundles");
mkdirSync(outRoot, { recursive: true });

const failed = [];
for (const provider of targets) {
  console.log(`\n==> bundle: ${provider}`);
  const result = spawnSync("npm", ["run", "build"], {
    cwd: root,
    stdio: "inherit",
    env: { ...process.env, VITE_PLATFORM: provider }
  });
  if (result.status !== 0) {
    failed.push(provider);
    continue;
  }
  const dist = resolve(root, "dist");
  if (!existsSync(resolve(dist, "index.html"))) {
    // A build that exits 0 without producing an app shell is the failure
    // that would otherwise ship an empty bundle directory and turn every
    // route into a 404 at the customer's end.
    console.error(`build-bundles: ${provider} built with no dist/index.html`);
    failed.push(provider);
    continue;
  }
  const out = resolve(outRoot, provider);
  rmSync(out, { recursive: true, force: true });
  cpSync(dist, out, { recursive: true });
}

if (failed.length > 0) {
  console.error(`\nbuild-bundles: FAILED for ${failed.length}/${targets.length}: ${failed.join(", ")}`);
  process.exit(1);
}

console.log(`\nbuild-bundles: wrote ${targets.length} bundle(s) to ${outRoot}`);
console.log("Serve one with:  backup-manager-web serve-ui --ui-dir <dir>");
console.log("Or the tree with: backup-manager-web serve-ui --ui-root <root> --profile <name>");
