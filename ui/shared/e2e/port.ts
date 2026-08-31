// Which port the suite serves itself on.
//
// This used to be a single hard-coded 5273 shared by every checkout on the
// machine, with `E2E_PORT` documented as the manual way out. That leaves a
// real window open, and it is the window #172 came through. Playwright's
// WebServerPlugin probes the URL once before spawning, and if nothing
// answers it spawns and then races an availability poll against the child
// exiting, and the poll runs immediately, before its first backoff. So two
// runs that both probe a free port, both spawn Vite, and race the bind can
// end with the loser's `--strictPort` Vite exiting while the availability
// poll is answered by the WINNER's server. The losing run then tests a
// foreign build and nothing notices, which is #172 exactly.
//
// Deriving the default from the checkout closes that by giving concurrent
// worktrees different ports in the first place, so they no longer contend
// at all. `--strictPort` and `reuseExistingServer: false` stay as the
// backstop for the residual (two checkouts hashing to the same slot), and
// playwright.config.ts prints the resolved port so a bind failure names it.
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";

/** Bottom of the derived range. Below Vite's own 5173 default is other
 *  people's territory; above 6999 is macOS's AirPlay receiver on 7000. */
export const PORT_FLOOR = 5200;
/** Slots in the derived range (5200-6999). Wide on purpose: this machine
 *  carries ~50 worktrees of this repo and runs several gates at once, and
 *  the birthday maths over a 300-slot range puts a colliding pair at
 *  roughly 1 in 6. A collision is not silent (see the backstop above), but
 *  it costs a run, so buy the slots. */
export const PORT_SLOTS = 1800;

/** Stable per checkout: the same worktree always gets the same port, so a
 *  URL stays bookmarkable and a trace stays reproducible, while a different
 *  worktree of the same repo gets a different one. */
export function derivePort(checkoutRoot: string): number {
  return PORT_FLOOR + (createHash("sha256").update(checkoutRoot).digest().readUInt32BE(0) % PORT_SLOTS);
}

/** The checkout this run belongs to. `git rev-parse --show-toplevel` rather
 *  than the config file's own directory, because Playwright loads this
 *  config as ESM or CJS depending on the package type and only one of those
 *  has `__dirname`; a worktree root is also the identity we actually mean.
 *  Falls back to the working directory outside a checkout (a tarball, a
 *  container image) where every run is the only run anyway. */
export function checkoutRoot(cwd: string = process.cwd()): string {
  try {
    const root = execFileSync("git", ["rev-parse", "--show-toplevel"], {
      cwd,
      encoding: "utf8",
      stdio: ["ignore", "pipe", "ignore"]
    }).trim();
    if (root) return root;
  } catch {
    // Not a checkout, or no git on PATH.
  }
  return cwd;
}

/**
 * E2E_PORT wins when it is set to something usable, and is rejected loudly
 * when it is not. `Number(process.env.E2E_PORT ?? …)` used to accept both
 * `""` (a shell wrapper's unset variable, which became port 0) and any
 * typo (which became NaN); both reached `--port` and `baseURL` and bought
 * a 60 second webServer timeout that named the server rather than the
 * variable. An empty value means "unset" and takes the derived default; a
 * non-empty one has to be a port.
 */
export function resolveE2EPort(
  env: Record<string, string | undefined> = process.env,
  root: string = checkoutRoot()
): number {
  const raw = (env.E2E_PORT ?? "").trim();
  if (raw === "") return derivePort(root);
  if (!/^\d+$/.test(raw) || Number(raw) < 1 || Number(raw) > 65535) {
    throw new Error("E2E_PORT must be a whole number between 1 and 65535; received " + JSON.stringify(env.E2E_PORT));
  }
  return Number(raw);
}
