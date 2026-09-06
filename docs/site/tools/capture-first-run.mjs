// Re-shoots every screenshot on docs/site/first-run.html.
//
// Run it from the repository root:
//
//     node docs/site/tools/capture-first-run.mjs
//
// It writes PNGs into docs/site/screens/, overwriting whatever is there,
// and prints one line per screen so a diff of the output tells you which
// picture moved.
//
// # Where the app comes from, and why it is never a real deployment
//
// The flow this walks ends by writing a configuration file and claiming an
// administrator account, and the enrollment token that opens it is single
// use. Pointing this at a real instance would finish somebody's setup for
// them, so it never does: it starts `ui/shared`'s own Vite dev server,
// where `src/app/createApp.tsx` substitutes `createMockApi` under
// `import.meta.env.DEV`. Every screen below is the real component tree
// with real copy, rendering fixture data from `src/api/mock.ts`.
//
// `?scenario=first-run` is what puts the app in the unconfigured state:
// `createMockApi` starts with `configured = false` for that scenario only,
// so `App.tsx` renders `FirstRunPage` instead of the shell. It is a real
// scenario in the product's own fixtures, not something added here.
//
// # Why the auth stub exists
//
// The generic bridge (`apps/generic/frontend/platform.ts`) decides whether
// the app is signed in by fetching `/api/v1/auth/session`. The mock API is
// an in-memory object, not a network layer, so in dev nothing answers that
// request and Vite's own dev server replies with the SPA's index.html. The
// app reads that as "not authenticated", forever, and the login page is
// the only screen anyone can reach. Stubbing the one route is the same fix
// `rclone-manager-tests`' Suite B applies in its own fixtures, for the
// same reason.
//
// # Why Playwright is borrowed rather than installed
//
// Issue #158 moved the browser suite out of `ui/shared` into
// `spdrman/rclone-manager-tests`, and adding a Playwright dependency back
// here would undo that. So this resolves `playwright-core` out of the
// checkout `scripts/e2e/run-tests-repo-gate.sh` already maintains, keyed
// by the sha in `scripts/e2e/tests-repo.pin`. That file is read, never
// written. If the gate has not run yet on this machine there is no
// checkout to borrow from, and this says so rather than guessing.

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

/** The standard desktop NAS administration window the browser suite uses
 *  (§31), so a screenshot here and a failure trace there frame the same
 *  layout. deviceScaleFactor 2 because these are read on retina displays
 *  and a 1x capture of 13px UI text is unreadable when scaled up. */
const VIEWPORT = { width: 1280, height: 900 };
const SCALE = 2;

/** Never a real host, never a real port, never real key material. The port
 *  in particular is deliberately uncommitted in this project, so the
 *  screenshots carry a placeholder that cannot be mistaken for one. */
const EXAMPLE = {
  token: "EXAMPLE-TOKEN-not-a-real-one",
  adminUser: "nas-admin",
  adminPassword: "correct-horse-battery-staple",
  setName: "api-server-nightly",
  host: "api-server.example.net",
  port: "<your-ssh-port>",
  user: "backup-agent",
  remoteFolder: "/var/backups/",
  include: "*.tar.zst",
  destination: "/data/backups/api-server/",
  // Obviously not a key. It is never screenshotted: the paste field is
  // captured empty, and this only exists so the Import button can be
  // clicked and the imported-fingerprint state reached.
  fakeKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nEXAMPLE-PLACEHOLDER-NOT-A-KEY\n-----END OPENSSH PRIVATE KEY-----\n"
};

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


/** These are flat UI screenshots: a few dozen distinct colours, large runs
 *  of one value, no photographic gradient anywhere. A 256-colour palette
 *  is visually identical and roughly a third of the size, which matters
 *  because every one of these lands in git history forever. Skipped, with
 *  a line saying so, on a machine with no ImageMagick, so a re-shoot there
 *  still produces correct (if larger) pictures. */
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

  // --strictPort so a lost race fails loudly rather than serving a
  // different port than the one this script then navigates to, and an
  // explicit --host because Vite's default `localhost` binds ::1 only on
  // macOS, which the 127.0.0.1 probe above cannot then reach.
  const dev = spawn("npm", ["run", "dev", "--", "--host", "127.0.0.1", "--port", String(port), "--strictPort"], {
    cwd: UI_DIR,
    stdio: ["ignore", "ignore", "inherit"]
  });

  let browser;
  try {
    await waitForServer(base + "/", dev);

    browser = await chromium.launch();
    const context = await browser.newContext({ viewport: VIEWPORT, deviceScaleFactor: SCALE });

    // The one route the mock API cannot answer. Flipped, not fixed: the
    // enrollment and sign-in screens only exist while this says 401.
    const session = { authenticated: false };
    await context.route("**/api/v1/auth/session", (route) =>
      route.fulfill(
        session.authenticated
          ? { status: 200, contentType: "application/json", body: JSON.stringify({ username: EXAMPLE.adminUser }) }
          : { status: 401, contentType: "application/json", body: JSON.stringify({ message: "not authenticated" }) }
      )
    );

    const page = await context.newPage();
    const shots = [];
    /** The sign-in and enrolment screens centre a 452px card in a full
     *  viewport, so a plain capture is nine parts empty background. Clip
     *  those to the card plus a margin instead. */
    const AUTH_CARD = "#root > div > div";

    const shot = async (name, clipSel) => {
      // Settle the fonts and any in-flight mock delay before the capture,
      // so a re-shoot of an unchanged screen produces an unchanged file.
      await page.evaluate(() => document.fonts.ready);
      await page.waitForTimeout(250);

      // Trim the dead space below a short page. Several of the wizard
      // steps only fill two thirds of a 900px window, and a screenshot
      // that is a third empty reads on the page as a rendering fault
      // rather than as a short form. Taller-than-viewport screens fall
      // through to a full-page capture instead of being cut off.
      let clip;
      if (clipSel) {
        const box = await page.locator(clipSel).first().boundingBox();
        if (!box) throw new Error("nothing matched " + clipSel + " for " + name);
        const pad = 28;
        clip = {
          x: Math.max(0, Math.round(box.x - pad)),
          y: Math.max(0, Math.round(box.y - pad)),
          width: Math.round(box.width + pad * 2),
          height: Math.round(box.height + pad * 2)
        };
      } else {
        const content = await page.evaluate(() => {
          const root = document.getElementById("root");
          return root ? Math.ceil(root.getBoundingClientRect().height) : 0;
        });
        const height = content + 32;
        if (content > 0 && height < VIEWPORT.height) {
          clip = { x: 0, y: 0, width: VIEWPORT.width, height };
        }
      }

      const file = resolve(OUT, name + ".png");
      await page.screenshot({
        path: file,
        animations: "disabled",
        ...(clip ? { clip } : { fullPage: true })
      });
      shots.push(name);
      console.log("  " + name + ".png");
    };

    // ------------------------------------------------------------- enrolment
    //
    // The real entry point. The engine prints an enrollment link on first
    // start (apps/common/auth/local/service.go, PrintBootstrapNotice) and
    // this is the page at the other end of it. The token in the URL is a
    // placeholder: the mock does not check it, and a real one must never
    // be committed.
    await page.goto(base + "/enroll?scenario=first-run&token=" + EXAMPLE.token);
    await page.getByRole("heading", { name: /Create Backup Manager administrator/ }).waitFor();
    await shot("01-enrolment-empty", AUTH_CARD);

    await page.getByLabel("Username").fill(EXAMPLE.adminUser);
    await page.getByLabel(/^Password/).fill("short");
    await page.getByText(/Minimum 12 characters/).first().waitFor();
    await shot("02-enrolment-password-too-short", AUTH_CARD);

    await page.getByLabel(/^Password/).fill(EXAMPLE.adminPassword);
    await page.getByLabel("Confirm password").fill(EXAMPLE.adminPassword);
    await page.getByRole("button", { name: "Create administrator" }).waitFor({ state: "visible" });
    await shot("03-enrolment-ready", AUTH_CARD);

    // Enrolment succeeds against the mock, and the app then asks the
    // bridge who is signed in. Flip the stub first so the answer is the
    // administrator that was just created.
    session.authenticated = true;
    await page.getByRole("button", { name: "Create administrator" }).click();

    // ------------------------------------------------------------- first run
    await page.getByRole("heading", { name: "Set up Backup Manager" }).waitFor();
    await shot("04-first-run-step-1-source");

    // A regex, not { exact: true }: a couple of these inputs sit inside a
    // <label> that also carries a hint and a button, so the accumulated
    // accessible name is the field name plus all of that. Anchoring at the
    // start still cannot collide with any other field on the same step.
    const fill = async (label, value) => {
      const field = page.getByLabel(new RegExp("^" + label));
      await field.fill(value);
      await field.blur();
    };
    await fill("Backup set name", EXAMPLE.setName);
    await fill("Server hostname", EXAMPLE.host);
    await fill("SSH port", EXAMPLE.port);
    await fill("Username", EXAMPLE.user);
    await shot("05-step-1-source-filled");

    // ------------------------------------------------------- authentication
    await page.getByRole("button", { name: "Authentication" }).click();
    await page.getByRole("heading", { name: "Authentication", level: 2 }).waitFor();
    await shot("06-step-2-authentication");

    await page.getByRole("radio", { name: /Import key/ }).check();
    await page.getByLabel(/Private key/).waitFor();
    await shot("07-step-2-import-key");

    await page.getByLabel(/Private key/).fill(EXAMPLE.fakeKey);
    await page.getByRole("button", { name: "Import key" }).click();
    await page.getByText("Key imported").waitFor();
    await shot("08-step-2-key-imported");

    // ------------------------------------------------------- verify server
    await page.getByRole("button", { name: "Verify server" }).click();
    await page.getByRole("heading", { name: "Verify server", level: 2 }).waitFor();
    await page.getByRole("button", { name: "Trust host" }).waitFor();
    await shot("09-step-3-verify-server");

    await page.getByRole("button", { name: "Trust host" }).click();
    await page.getByRole("button", { name: "Host trusted" }).waitFor();
    await shot("10-step-3-host-trusted");

    // ----------------------------------------------------------- discovery
    await page.getByRole("button", { name: "Discovery" }).click();
    await page.getByRole("heading", { name: "Backup discovery", level: 2 }).waitFor();
    await fill("Remote folder", EXAMPLE.remoteFolder);
    await fill("Include patterns", EXAMPLE.include);
    await shot("11-step-4-discovery");

    await page.getByRole("radio", { name: /Atomic rename/ }).check();
    await shot("12-step-4-completion-atomic-rename");

    // ------------------------------------------------- storage / retention
    await page.getByRole("button", { name: "Storage & retention" }).click();
    await page.getByRole("heading", { name: /Storage, retention and validation/, level: 2 }).waitFor();
    await fill("NAS destination", EXAMPLE.destination);
    await shot("13-step-5-storage-retention");

    // ------------------------------------------------------------- review
    await page.getByRole("button", { name: "Review" }).click();
    await page.getByRole("heading", { name: "Review", level: 2 }).waitFor();
    await shot("14-step-6-review");

    await page.getByRole("checkbox", { name: /I understand the remote backup will be removed/ }).check();
    await shot("15-step-6-acknowledged");

    // ------------------------------------------------------- configured app
    await page.getByRole("button", { name: "Finish setup" }).click();
    await page.getByRole("navigation", { name: "Sections" }).waitFor();
    await page.waitForTimeout(600);
    await shot("16-configured-dashboard");

    // ---------------------------------------------------------- sign in
    //
    // Last, not first. An operator meets this screen on the SECOND visit,
    // or on a first visit that did not go through the printed link. A
    // reload rebuilds the mock, so the app is unconfigured again and the
    // session stub decides what renders.
    session.authenticated = false;
    await page.goto(base + "/?scenario=first-run");
    await page.getByRole("heading", { name: "Sign in" }).waitFor();
    await shot("17-sign-in", AUTH_CARD);

    console.log("\n" + shots.length + " screens written to docs/site/screens/");
    optimise(shots);
    const total = readdirSync(OUT)
      .filter((f) => f.endsWith(".png"))
      .reduce((n, f) => n + statSync(resolve(OUT, f)).size, 0);
    console.log("total " + (total / 1024 / 1024).toFixed(2) + " MB in " + OUT);
  } finally {
    if (browser) await browser.close();
    dev.kill("SIGTERM");
  }
}

main().catch((e) => {
  console.error(String(e && e.stack ? e.stack : e));
  process.exit(1);
});
