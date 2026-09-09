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
// At #167 this was deliberately NOT wired into container/Dockerfile,
// because the arithmetic said over the budget rather than inside it. Each
// bundle was about 352 KiB (360,448 bytes), the gate was 1.05x a recorded
// baseline image of 43,008,762 bytes so the ceiling was 45,159,200, and
// against that change's own measured image of 43,074,298 the headroom was
// 2,084,902 bytes. The canonical image already carried the generic
// bundle, so shipping the rest meant six more at 2,162,688 bytes, over
// the ceiling by about 78 KB, and all seven over by about 438 KB. (The
// older "inside by roughly 12 KB" reading came from measuring against the
// baseline image rather than the one that change actually produced, which
// is the trap: the headroom moves every time the binary grows.)
//
// #169 and #180 then did wire it in, and this comment claimed otherwise
// for long enough for #635 to find it. container/Dockerfile's
// frontend-build stage runs `npm run build:bundles truenas unraid
// openmediavault proxmox synology`, and the runtime stage copies
// dist-bundles/ to /ui/bundles. Five, not seven: generic is compiled into
// the binary and ugos ships in EPIC D's UPK, so those two have a carrier
// already and a directory for either would be bytes nobody serves.
//
// Note which half of the paragraph above decides that, because the
// sentence right before it is the trap it describes. "Which image the
// headroom is measured from is the whole question" was the right
// observation, and it applies to the answer as much as to the estimate:
// once the five bundles are INSIDE the recorded baseline, they are not
// spending the headroom any more, and any sum that charges them to it is
// counting them twice. The baseline was re-captured at 69,704,266 with
// them in it on 2026-09-08, so that is now the case.
//
// Re-derived there, the old arithmetic contradicts itself, which is the
// clearest sign it has stopped deciding anything. 5% of 69,704,266 is
// 3,485,213; the five measure 3,503,996 (about 700,799 each, twice the
// 352 KiB above, because #632 put 139,744 bytes of woff2 in every one and
// EPIC F, G and H grew the JS chunk). Charge them and FIVE is over by
// 18,783. Do not charge them, which is correct, and the two missing
// bundles want 1,401,598 against 3,485,213, so SEVEN fits with 2,083,615
// spare. The count is a duplication argument, not a size one. docs/perf/
// carries what the image measures, and is the only place worth trusting
// for it.
import { spawnSync } from "node:child_process";
import { rmSync, mkdirSync, cpSync, existsSync, writeFileSync } from "node:fs";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = resolve(here, "..");

// Keep in sync with e2e/fixtures.ts's PROVIDERS export and with
// apps/common/platform/capabilities.PlatformID, duplicated here for the
// same reason e2e-all-providers.mjs duplicates it: no TypeScript loader.
const PROVIDERS = ["generic", "ugos", "synology", "truenas", "unraid", "openmediavault", "proxmox"];

// The marker file written into every bundle directory. Kept in sync with
// distribution/packaging's UIBundleMarkerName and with
// apps/synology/spk's own reader.
const BUNDLE_MARKER = "bundle.json";

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
  const target = resolve(outRoot, provider);
  rmSync(target, { recursive: true, force: true });
  cpSync(dist, target, { recursive: true });

  // A bundle directory has to be able to say which provider it is for.
  // Without this, "ship the Synology bundle in the .spk" is a build-time
  // convention nothing can check, and the failure mode is silent: the
  // package installs, the UI loads, and it is the wrong bridge. The
  // Synology package builder refuses a bundle whose marker names another
  // provider, and distribution/packaging reads the same file.
  writeFileSync(
    resolve(target, BUNDLE_MARKER),
    JSON.stringify({ schema: "rclone-manager/ui-bundle/1", platform: provider }, null, 2) + "\n"
  );
}

if (failed.length > 0) {
  console.error(`\nbuild-bundles: FAILED for ${failed.length}/${targets.length}: ${failed.join(", ")}`);
  process.exit(1);
}

console.log(`\nbuild-bundles: wrote ${targets.length} bundle(s) to ${outRoot}`);
// `rbm-web` is the only name the web host answers to since 0.3.3 renamed
// the CLI, and it is spelled once, as cliecho.WebBinary in
// core/cliecho/cliname.go. 0.3.3 is a clean cut, so there is no older
// spelling still resolving beside it. The Go package directory is still
// apps/generic/cmd/backup-manager-web and stays that way, because a
// package path is not something an operator types: a developer building
// from a checkout gets a binary named after the directory and would run
// that instead.
console.log("Serve one with:  rbm-web serve-ui --ui-dir <dir>");
console.log("Or the tree with: rbm-web serve-ui --ui-root <root> --profile <name>");
