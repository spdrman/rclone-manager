#!/usr/bin/env node
// Runs the "providers" Playwright project once per provider. This used to
// be a shell loop inline in package.json's "e2e:all-providers" string.
// Two bugs with that shape, both found while fixing #113:
//
// 1. Run directly (`sh -c "for p in ...; do ...; done"`) it covered all
//    seven providers in ~220s. Run through `npm run` it finished in ~30s
//    having tested only "generic", and still exited 0 — a seven-provider
//    matrix silently covering one seventh and reporting green is worse
//    than no matrix. This script is a real loop with a real per-provider
//    result, and it reports every failing provider rather than stopping
//    at the first (`process.exitCode`, not a `|| exit 1` that would abort
//    the whole run on the first red provider and hide the rest).
//
// 2. playwright.config.ts used to set `reuseExistingServer: !process.env.CI`,
//    so locally, the second and subsequent providers were reusing a dev
//    server that Vite had already built with the FIRST provider's
//    VITE_PLATFORM baked in (see vite.config.ts's @platform-entry alias).
//    Whatever a later provider's spec asserted, it was asserting it
//    against the wrong build. This script worked around that by forcing
//    CI=1 on every invocation. The hazard is fixed at the source now
//    (#172): `reuseExistingServer` is an unconditional `false`, so every
//    provider starts and tears down its own server whatever CI says.
//
//    The flag is gone rather than kept as a belt-and-braces, because it
//    was never free: it also bought `retries: 1` from the same config,
//    and this matrix was the ONLY path in the suite carrying a retry. A
//    retry is what turns a deterministic red into something that reads
//    as a flake, which is how #172's real failure got dismissed twice.
//    `--forbid-only` is passed explicitly below instead, since it is the
//    half of CI=1 this matrix does want: a stray `test.only` here would
//    silently shrink the matrix, which is bug 1 all over again.
import { spawnSync } from "node:child_process";

// Keep in sync with e2e/fixtures.ts's PROVIDERS export — duplicated here
// rather than imported so this script has no TypeScript-loader dependency.
const PROVIDERS = ["generic", "ugos", "synology", "truenas", "unraid", "openmediavault", "proxmox"];

const failed = [];

for (const provider of PROVIDERS) {
  console.log("\n==> provider: " + provider);
  const result = spawnSync("npx", ["playwright", "test", "--project=providers", "--forbid-only"], {
    stdio: "inherit",
    env: {
      ...process.env,
      VITE_PLATFORM: provider
    }
  });
  if (result.status !== 0) failed.push(provider);
}

if (failed.length > 0) {
  console.error("\ne2e:all-providers: FAILED for " + failed.length + "/" + PROVIDERS.length + " provider(s): " + failed.join(", "));
  process.exit(1);
}

console.log("\ne2e:all-providers: all " + PROVIDERS.length + " providers passed.");
