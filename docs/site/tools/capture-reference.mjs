// Re-shoots every screenshot on docs/site/reference.html.
//
// Run it from the repository root:
//
//     node docs/site/tools/capture-reference.mjs
//
// It writes PNGs into docs/site/screens/ under the `ref-` prefix,
// overwriting whatever is there, and prints one line per screen so a diff
// of the output tells you which picture moved. The `first-run` set that
// capture-first-run.mjs owns is left alone: the two tools share a
// directory and nothing else, and the prefix is what keeps a re-shoot of
// one from quietly deleting the other's work.
//
// Everything about where the app comes from, why it is never a real
// deployment, why the auth route is stubbed and why Playwright is
// borrowed rather than installed is explained at the top of
// capture-first-run.mjs. This is the same harness pointed at the
// signed-in application instead of at the setup flow.
//
// The one difference worth stating here: that tool walks a FLOW, so its
// order is the order an operator meets the screens in. This one walks a
// SURFACE, so its order is the navigation's, and each screen is reached
// by its own URL rather than by clicking through the previous one. A
// screen that fails to render therefore takes down its own capture and
// nothing else, which is what makes it safe to shoot ten of them in
// one run.

import { spawn, spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import { createServer } from "node:net";
import { existsSync, mkdirSync, readFileSync, readdirSync, statSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const SITE = resolve(HERE, "..");
const REPO = resolve(SITE, "../..");
const UI_DIR = resolve(REPO, "ui/shared");
const OUT = resolve(SITE, "screens");

const VIEWPORT = { width: 1280, height: 900 };
const SCALE = 2;

/** The administrator the session stub reports. Never a real account. */
const ADMIN = "nas-admin";

function readPin() {
  const pin = readFileSync(resolve(REPO, "scripts/e2e/tests-repo.pin"), "utf8");
  const sha = /^TESTS_REPO_SHA=([0-9a-f]{40})$/m.exec(pin);
  if (!sha) throw new Error("scripts/e2e/tests-repo.pin carries no full TESTS_REPO_SHA");
  return sha[1];
}

function loadPlaywright() {
  const cacheRoot = resolve(
    process.env.XDG_CACHE_HOME ?? resolve(process.env.HOME ?? "", ".cache"),
    "rclone-manager-tests-gate"
  );
  const suite = resolve(cacheRoot, readPin(), "suites/web-ui");
  if (!existsSync(resolve(suite, "node_modules/playwright-core"))) {
    throw new Error(
      "no Playwright to borrow: " + suite + " has no node_modules.\n" +
        "Run the e2e gate once (scripts/e2e/run-tests-repo-gate.sh) so it populates that checkout, then re-run this."
    );
  }
  return createRequire(resolve(suite, "package.json"))("playwright-core");
}

function freePort() {
  return new Promise((ok, fail) => {
    const s = createServer();
    s.once("error", fail);
    s.listen({ host: "127.0.0.1", port: 0, exclusive: true }, () => {
      const { port } = s.address();
      s.close(() => ok(port));
    });
  });
}

async function waitForServer(url, child) {
  const deadline = Date.now() + 60_000;
  for (;;) {
    if (child.exitCode !== null) throw new Error("the dev server exited with code " + child.exitCode);
    try {
      const res = await fetch(url, { redirect: "manual" });
      if (res.status < 500) return;
    } catch {
      // not listening yet
    }
    if (Date.now() > deadline) throw new Error("the dev server did not answer " + url + " within 60s");
    await new Promise((r) => setTimeout(r, 250));
  }
}

/** Same reasoning as capture-first-run.mjs: flat UI, a few dozen colours,
 *  and every one of these lands in git history forever. */
function optimise(names) {
  const magick = spawnSync("magick", ["-version"], { stdio: "ignore" });
  if (magick.error || magick.status !== 0) {
    console.log("ImageMagick not found, leaving the PNGs at full colour depth");
    return;
  }
  let before = 0;
  let after = 0;
  for (const name of names) {
    const file = resolve(OUT, name + ".png");
    before += statSync(file).size;
    const r = spawnSync("magick", [file, "-strip", "-colors", "256", "PNG8:" + file], { stdio: "inherit" });
    if (r.status !== 0) throw new Error("magick failed on " + file);
    after += statSync(file).size;
  }
  console.log(
    "quantised to 256 colours: " +
      (before / 1024 / 1024).toFixed(2) + " MB -> " + (after / 1024 / 1024).toFixed(2) + " MB"
  );
}

async function main() {
  if (!existsSync(resolve(UI_DIR, "node_modules"))) {
    throw new Error(UI_DIR + " has no installed dependencies. Fix it with: cd " + UI_DIR + " && npm ci");
  }
  mkdirSync(OUT, { recursive: true });

  const { chromium } = loadPlaywright();
  const port = await freePort();
  const base = "http://127.0.0.1:" + port;

  const dev = spawn("npm", ["run", "dev", "--", "--host", "127.0.0.1", "--port", String(port), "--strictPort"], {
    cwd: UI_DIR,
    stdio: ["ignore", "ignore", "inherit"]
  });

  let browser;
  try {
    await waitForServer(base + "/", dev);

    browser = await chromium.launch();
    const context = await browser.newContext({ viewport: VIEWPORT, deviceScaleFactor: SCALE });

    // Signed in for the whole run, unlike the first-run tool: every screen
    // below is behind authentication and the mock API is an in-memory
    // object with no network layer to answer this one route.
    await context.route("**/api/v1/auth/session", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ username: ADMIN })
      })
    );

    const page = await context.newPage();
    const shots = [];

    const shot = async (name) => {
      await page.evaluate(() => document.fonts.ready);
      await page.waitForTimeout(300);
      const content = await page.evaluate(() => {
        const root = document.getElementById("root");
        return root ? Math.ceil(root.getBoundingClientRect().height) : 0;
      });
      const height = content + 32;
      const clip = content > 0 && height < VIEWPORT.height
        ? { x: 0, y: 0, width: VIEWPORT.width, height }
        : null;
      await page.screenshot({
        path: resolve(OUT, name + ".png"),
        animations: "disabled",
        ...(clip ? { clip } : { fullPage: true })
      });
      shots.push(name);
      console.log("  " + name + ".png");
    };

    /** Navigate, then wait for something only THAT screen renders.
     *
     *  A settle timer alone is not enough and the failure it produces is
     *  the worst kind: the previous screen is still on the page when the
     *  shutter opens, so the capture succeeds and the picture is of the
     *  wrong page. Waiting for a marker turns that into a timeout that
     *  names the screen instead. */
    const visit = async (path, marker) => {
      await page.goto(base + path);
      await marker(page).waitFor({ state: "visible", timeout: 20_000 });
    };

    const text = (re) => (p) => p.getByText(re).first();
    const heading = (re) => (p) => p.getByRole("heading", { name: re }).first();

    /** Collapse the docked terminal before the page captures, and shoot it
     *  on its own afterwards.
     *
     *  The dock is `position: fixed`, so in a full-page capture it renders
     *  once, at the bottom of the FIRST viewport, which on a long page is
     *  the middle of the picture. Expanded that is 170px of a screen
     *  covered by a panel that is not covering it on the real thing.
     *  Collapsed it is the one-line bar every page genuinely carries, so
     *  leaving that in is documentation rather than an artefact. Its open
     *  state is in localStorage (ActivityDock), so this is clicked once
     *  and holds for the rest of the run. */
    const collapseDock = async () => {
      const hide = page.getByRole("button", { name: "Hide terminal" });
      if (await hide.count()) await hide.first().click();
    };

    const SCREENS = [
      ["ref-01-dashboard", "/", text(/Recent activity/i)],
      ["ref-02-backup-sets", "/sets", text(/Backup sets|backup set/i)],
      ["ref-09-set-detail", "/sets/production/postgres-primary", heading(/Production PostgreSQL/i)],
      ["ref-03-backups", "/backups", text(/Backups|artifact/i)],
      ["ref-10-backup-detail", "/backups/art_01J9F4M2QK8Z", text(/postgres-prod-20260828\.dump\.zst/)],
      ["ref-04-activity", "/activity", text(/Activity/i)],
      ["ref-05-quarantine", "/quarantine", text(/Quarantine/i)],
      ["ref-06-settings", "/settings", text(/Notifications/i)],
      ["ref-07-catalog-recovery", "/catalog-recovery", text(/Existing backup data detected/i)]
    ];

    // Once, before the first capture: goto the dashboard, collapse, and
    // let localStorage carry it through every navigation below.
    await visit("/", text(/Recent activity/i));
    await collapseDock();

    for (const [name, path, marker] of SCREENS) {
      await visit(path, marker);
      await shot(name);
    }

    // ------------------------------------------------------- the terminal
    //
    // Its own screen, because it is its own surface: every command the
    // interface runs on the operator's behalf is echoed here, with the
    // engine's own live feed beside it, and none of that is visible in a
    // picture of the bar it collapses to.
    await visit("/", text(/Recent activity/i));
    const show = page.getByRole("button", { name: "Show terminal" });
    if (await show.count()) await show.first().click();
    await page.getByRole("button", { name: "Hide terminal" }).first().waitFor();
    await page.evaluate(() => document.fonts.ready);
    await page.waitForTimeout(400);
    {
      const dock = await page.locator("section").filter({ has: page.getByRole("button", { name: "Hide terminal" }) }).first().boundingBox();
      if (!dock) throw new Error("the docked terminal did not lay out");
      await page.screenshot({
        path: resolve(OUT, "ref-08-terminal.png"),
        animations: "disabled",
        clip: {
          x: Math.round(dock.x),
          y: Math.round(dock.y),
          width: Math.round(dock.width),
          height: Math.round(dock.height)
        }
      });
      shots.push("ref-08-terminal");
      console.log("  ref-08-terminal.png");
    }

    console.log("\n" + shots.length + " screens written to docs/site/screens/");
    optimise(shots);
    const total = readdirSync(OUT)
      .filter((f) => f.startsWith("ref-") && f.endsWith(".png"))
      .reduce((n, f) => n + statSync(resolve(OUT, f)).size, 0);
    console.log("total " + (total / 1024 / 1024).toFixed(2) + " MB of ref- screens in " + OUT);
  } finally {
    if (browser) await browser.close();
    dev.kill("SIGTERM");
  }
}

main().catch((e) => {
  console.error(String(e && e.stack ? e.stack : e));
  process.exit(1);
});
