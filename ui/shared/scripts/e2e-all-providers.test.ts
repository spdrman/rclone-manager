// What e2e-all-providers.mjs actually hands each provider run.
//
// The script is a spawner, so the only way to check it without running
// seven real browser matrices is to put a fake `npx` first on PATH and read
// back what it was called with. That is what this does.
//
// The assertion that matters is a negative one (#172): the script must NOT
// force CI=1 any more, because playwright.config.ts turns CI into
// `retries: 1`, and this matrix was the only path in the suite carrying a
// retry. A negative assertion is worthless unless the harness can see the
// thing it says is absent, so the last test is the positive control: with
// CI set in the parent, the capture file shows it.
import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, writeFileSync, chmodSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

const SCRIPT = join(dirname(fileURLToPath(import.meta.url)), "e2e-all-providers.mjs");
const PROVIDERS = ["generic", "ugos", "synology", "truenas", "unraid", "openmediavault", "proxmox"];

const workdir = mkdtempSync(join(tmpdir(), "e2e-all-providers-"));

type Invocation = { args: string; vitePlatform: string; ci: string };

/** Runs the real script with a fake `npx` on PATH, and returns one record
 *  per provider invocation. `extraEnv` seeds the PARENT environment, which
 *  is what the CI control varies. */
function runScript(extraEnv: Record<string, string> = {}): { status: number | null; calls: Invocation[] } {
  const bin = mkdtempSync(join(workdir, "bin-"));
  const capture = join(bin, "calls.txt");
  writeFileSync(
    join(bin, "npx"),
    [
      "#!/bin/sh",
      '{ printf "ARGS:%s\\n" "$*"',
      '  printf "VITE_PLATFORM:%s\\n" "${VITE_PLATFORM-<unset>}"',
      '  printf "CI:%s\\n" "${CI-<unset>}"',
      '} >> "$E2E_TEST_CAPTURE"',
      "exit 0",
      ""
    ].join("\n")
  );
  chmodSync(join(bin, "npx"), 0o755);

  const env: Record<string, string> = {};
  for (const [k, v] of Object.entries(process.env)) if (v !== undefined) env[k] = v;
  // vitest itself may well have been started under CI; the script's own
  // behaviour is what is under test, not this machine's.
  delete env.CI;
  Object.assign(env, extraEnv, { PATH: bin + ":" + env.PATH, E2E_TEST_CAPTURE: capture });

  const result = spawnSync(process.execPath, [SCRIPT], { env, encoding: "utf8" });
  const lines = existsSync(capture) ? readFileSync(capture, "utf8").trim().split("\n") : [];
  const calls: Invocation[] = [];
  for (let i = 0; i + 2 < lines.length; i += 3) {
    calls.push({
      args: lines[i].replace("ARGS:", ""),
      vitePlatform: lines[i + 1].replace("VITE_PLATFORM:", ""),
      ci: lines[i + 2].replace("CI:", "")
    });
  }
  return { status: result.status, calls };
}

describe("e2e-all-providers", () => {
  const run = runScript();

  it("runs the providers project once per provider, in order", () => {
    expect(run.status).toBe(0);
    expect(run.calls.map((c) => c.vitePlatform)).toEqual(PROVIDERS);
    for (const call of run.calls) expect(call.args).toContain("playwright test --project=providers");
  });

  it("does not force CI=1, which would buy this matrix a retry nothing else has", () => {
    // playwright.config.ts: `retries: process.env.CI ? 1 : 0`. A retry is
    // what let #172's deterministic red read as a flake.
    expect(run.calls.map((c) => c.ci)).toEqual(PROVIDERS.map(() => "<unset>"));
  });

  it("still forbids test.only, the half of CI=1 this matrix wants", () => {
    // Passed at the call site rather than inherited from a flag that also
    // buys retries. A stray .only here silently shrinks a seven-provider
    // matrix to one test, which is the bug this script was written for.
    for (const call of run.calls) expect(call.args).toContain("--forbid-only");
  });

  it("positive control: the capture sees CI when the environment has one", () => {
    // Without this, the assertion above would pass just as happily against
    // a harness that never observed CI at all.
    const control = runScript({ CI: "1" });
    expect(control.calls.map((c) => c.ci)).toEqual(PROVIDERS.map(() => "1"));
  });
});
