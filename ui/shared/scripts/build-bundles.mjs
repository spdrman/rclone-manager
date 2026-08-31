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
// Deliberately NOT wired into container/Dockerfile. Each bundle is
// roughly 350 KB, so embedding all seven would add about 2.1 MB to a
// 43 MB image, against a gated budget of 5% (about 2.15 MB). That is
// inside the budget by roughly 12 KB, which is not a margin, it is a
// coin toss on a gate. Converting each adapter to ship its own bundle is
// #169's work, and the image-size question is worth arguing there with a
// real measurement rather than pre-empting here.
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

const outRoot = resolve(root, "dist-bundles");
rmSync(outRoot, { recursive: true, force: true });
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
  cpSync(dist, resolve(outRoot, provider), { recursive: true });
}

if (failed.length > 0) {
  console.error(`\nbuild-bundles: FAILED for ${failed.length}/${targets.length}: ${failed.join(", ")}`);
  process.exit(1);
}

console.log(`\nbuild-bundles: wrote ${targets.length} bundle(s) to ${outRoot}`);
console.log("Serve one with:  backup-manager-web serve-ui --ui-dir <dir>");
console.log("Or the tree with: backup-manager-web serve-ui --ui-root <root> --profile <name>");
