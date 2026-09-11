// The shared machinery every capture script on this site runs on.
//
// Nothing in here is run directly. `capture-first-run.mjs` and the
// `capture-*.mjs` scripts beside it each import this, drive one flow, and
// leave the arrangement below to be the same arrangement every time.
//
// # Where the app comes from, and why it is never a real deployment
//
// This is the constraint the whole file is built around and it predates
// the GIFs: the first-run flow ends by writing a configuration file and
// claiming an administrator account, and the enrollment token that opens
// it is single use. Pointing a capture at a real instance would finish
// somebody's setup for them. So none of them ever does. Every script
// starts `ui/shared`'s own Vite dev server, where `src/app/createApp.tsx`
// substitutes `createMockApi` under `import.meta.env.DEV`, and every
// picture on the site is the real component tree with the real copy
// rendering fixture data from `src/api/mock.ts`.
//
// That is not a compromise made for the first-run page's sake. It is the
// same reason the rest of them do it: `medium remove` really removes a
// destination, a retention apply really deletes artifacts, and a capture
// script is exactly the kind of thing that gets pointed at the wrong
// address at two in the morning. There is no address here to point it at.
//
// # Why the auth stub exists
//
// The generic bridge (`apps/generic/frontend/platform.ts`) decides whether
// the app is signed in by fetching `/api/v1/auth/session`. The mock API is
// an in-memory object, not a network layer, so in dev nothing answers that
// request and Vite's own dev server replies with the SPA's index.html. The
// app reads that as "not authenticated", forever, and the login page is
// the only screen anyone can reach. Stubbing the one route is the same fix
// `backupd-tests`' Suite B applies in its own fixtures, for the
// same reason.
//
// # Why Playwright is borrowed rather than installed
//
// Issue #158 moved the browser suite out of `ui/shared` into
// `backupdproject/backupd-tests`, and adding a Playwright dependency back
// here would undo that. So this resolves `playwright-core` out of the
// checkout `scripts/e2e/run-tests-repo-gate.sh` already maintains, keyed
// by the sha in `scripts/e2e/tests-repo.pin`. That file is read, never
// written. If the gate has not run yet on this machine there is no
// checkout to borrow from, and this says so rather than guessing.
//
// # Why the clock is pinned
//
// `Recorder` below writes GIFs, and a GIF of a terminal is mostly
// timestamps. Left alone, every re-record would differ from the last in
// every frame that has a clock in it, which destroys the one property
// these outputs are supposed to have: that a diff of them tells you which
// picture moved. `page.clock.setFixedTime` freezes `Date.now()` and
// `new Date()` at a fixed instant while leaving the timers running, so
// the mock's own `setTimeout` delays still resolve and the app still
// polls. The instant is the same one the fixtures are written around.

import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { createRequire } from "node:module";
import { createServer } from "node:net";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));

export const SITE = resolve(HERE, "..");
export const REPO = resolve(SITE, "../..");
export const UI_DIR = resolve(REPO, "ui/shared");
export const SCREENS = resolve(SITE, "screens");

/** The standard desktop NAS administration window the browser suite uses
 *  (§31), so a screenshot here and a failure trace there frame the same
 *  layout. deviceScaleFactor 2 because these are read on retina displays
 *  and a 1x capture of 13px UI text is unreadable when scaled up. */
export const VIEWPORT = { width: 1280, height: 900 };
export const SCALE = 2;

/** The instant every capture pretends it is. Inside the window the
 *  fixtures describe, so a "3 minutes ago" reads as three minutes and not
 *  as eight months. */
export const FROZEN_TIME = new Date("2026-08-29T06:15:00+02:00");

/** Never a real host, never a real port, never real key material, never a
 *  real credential. The SSH port in particular is deliberately
 *  uncommitted in this project, so the pictures carry a placeholder that
 *  cannot be mistaken for one. */
export const EXAMPLE = {
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
  // Obviously not a key. It is never photographed: the paste field is
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

/** `page.clock.setFixedTime`, which every capture depends on for a stable
 *  timestamp, arrived in Playwright 1.45. An older borrowed copy fails
 *  with "cannot read properties of undefined", a hundred lines into a
 *  recording, which is not a sentence anybody can act on. */
const PLAYWRIGHT_MIN = [1, 45];

/**
 * Where Playwright comes from, in the order it is looked for.
 *
 * The pinned gate checkout first, because that is the copy this repository
 * already maintains and the one the browser suite runs against. Then any
 * other populated gate checkout, newest first: `scripts/e2e/tests-repo.pin`
 * moves whenever the specs move, and on the day it does every cached
 * checkout on the machine is suddenly at the wrong sha through no fault of
 * the person trying to re-record a picture. Then `ui/shared`'s own
 * node_modules, which has Playwright on any machine that has run the unit
 * tests.
 *
 * None of that changes what issue #158 decided. Nothing here adds a
 * dependency to anything; every candidate is a copy that already exists
 * for its own reasons, and this only declines to be the one script that
 * breaks because a pin moved.
 */
function playwrightCandidates() {
  const cacheRoot = resolve(
    process.env.XDG_CACHE_HOME ?? resolve(process.env.HOME ?? "", ".cache"),
    "backupd-tests-gate"
  );
  const pinned = readPin();
  const out = [{ why: "the pinned gate checkout " + pinned.slice(0, 12), dir: resolve(cacheRoot, pinned, "suites/web-ui") }];

  if (existsSync(cacheRoot)) {
    const others = readdirSync(cacheRoot)
      .filter((sha) => sha !== pinned && /^[0-9a-f]{40}$/.test(sha))
      .map((sha) => ({ sha, dir: resolve(cacheRoot, sha, "suites/web-ui") }))
      .filter((c) => existsSync(resolve(c.dir, "node_modules/playwright-core")))
      .map((c) => ({ ...c, at: statSync(resolve(c.dir, "node_modules/playwright-core")).mtimeMs }))
      .sort((a, b) => b.at - a.at);
    for (const c of others) {
      out.push({ why: "an unpinned gate checkout " + c.sha.slice(0, 12) + ", because the pinned one is not populated", dir: c.dir });
    }
  }

  out.push({ why: "ui/shared's own node_modules", dir: UI_DIR });
  return out;
}

export function loadPlaywright() {
  const tried = [];
  for (const candidate of playwrightCandidates()) {
    const pkg = resolve(candidate.dir, "package.json");
    const mod = resolve(candidate.dir, "node_modules/playwright-core");
    if (!existsSync(mod) || !existsSync(pkg)) {
      tried.push(candidate.dir);
      continue;
    }
    const require = createRequire(pkg);
    const version = require("playwright-core/package.json").version;
    const [major, minor] = version.split(".").map(Number);
    if (major < PLAYWRIGHT_MIN[0] || (major === PLAYWRIGHT_MIN[0] && minor < PLAYWRIGHT_MIN[1])) {
      throw new Error(
        "Playwright " + version + " in " + candidate.dir + " is too old.\n" +
          "These captures pin the clock with page.clock.setFixedTime, which needs " +
          PLAYWRIGHT_MIN.join(".") + " or newer. Without it every recording carries a different time."
      );
    }
    console.log("using Playwright " + version + " from " + candidate.why);
    return require("playwright-core");
  }
  throw new Error(
    "no Playwright to borrow. Looked in:\n  " + tried.join("\n  ") + "\n" +
      "Run the e2e gate once (scripts/e2e/run-tests-repo-gate.sh) so it populates the pinned checkout,\n" +
      "or `cd " + UI_DIR + " && npm ci`, then re-run this."
  );
}

/** A launch failure is almost always a missing browser BINARY rather than
 *  a missing package, and the two are different installs: playwright-core
 *  is the one package that deliberately does not manage them. Saying which
 *  command fixes it turns a stack trace into an instruction. */
async function launchChromium(chromium) {
  try {
    return await chromium.launch();
  } catch (e) {
    throw new Error(
      "Chromium would not launch, which usually means the browser binary is not installed.\n" +
        "playwright-core does not download one. Install it with:\n" +
        "  npx playwright install chromium\n\n" +
        String(e && e.message ? e.message : e)
    );
  }
}

/**
 * The port the dev server is always started on.
 *
 * Fixed rather than allocated, and that is a determinism decision rather
 * than a convenience. The global terminal prints an environment preamble
 * built from `window.location.origin`, so with an ephemeral port every
 * recording of the terminal carries a different four-digit number in the
 * first line, and a re-record would differ from the last in a frame where
 * nothing about the product had changed. It is also the wrong thing to
 * photograph: a reader should see a stable placeholder address, not
 * whatever the kernel handed out that afternoon.
 *
 * 8080 rather than something high and unlikely, because it is the port
 * the shipped Compose file publishes and the one the engine's own
 * enrollment notice prints. A reader looking at the terminal's first line
 * should see the address they will actually type. If it is busy the run
 * stops and says so, because falling back to a free one would silently
 * give up both properties this is here for.
 */
export const DEV_PORT = Number(process.env.CAPTURE_PORT ?? 8080);

function assertPortFree(port) {
  return new Promise((ok, fail) => {
    const s = createServer();
    s.once("error", () =>
      fail(
        new Error(
          "port " + port + " is busy, and these captures use a fixed port on purpose (see DEV_PORT).\n" +
            "Stop whatever is holding it, or set CAPTURE_PORT to another one and re-record everything so the pictures agree."
        )
      )
    );
    s.listen({ host: "127.0.0.1", port, exclusive: true }, () => s.close(() => ok(port)));
  });
}

async function waitForServer(url, child) {
  const deadline = Date.now() + 90_000;
  for (;;) {
    // Both, because a child killed by a signal leaves exitCode null and a
    // check on exitCode alone spins to the deadline saying the server did
    // not answer, which is true and is not the reason.
    if (child.signalCode !== null) throw new Error("the dev server was killed by " + child.signalCode);
    if (child.exitCode !== null) throw new Error("the dev server exited with code " + child.exitCode);
    try {
      const res = await fetch(url, { redirect: "manual" });
      if (res.status < 500) return;
    } catch {
      // not listening yet
    }
    if (Date.now() > deadline) throw new Error("the dev server did not answer " + url + " within 90s");
    await new Promise((r) => setTimeout(r, 250));
  }
}

/**
 * Starts the dev server once, hands it to `body`, and stops it afterwards
 * whatever happens.
 *
 * The server is the expensive part of every one of these scripts (a cold
 * Vite start plus a browser launch is most of the wall clock), so a script
 * that captures six flows starts one server and opens six contexts inside
 * this, rather than paying for six.
 *
 * The process is killed by the pid this holds and never by name. There is
 * no `pkill` anywhere near this file: the one thing worse than a leaked
 * dev server is a pattern kill that also takes out the editor's language
 * server.
 *
 * # Ctrl-C has to reach the teardown too
 *
 * A `finally` covers a throw and covers nothing else. A SIGTERM or a
 * Ctrl-C kills this process where it stands, `finally` never runs, and
 * the dev server it spawned goes on holding port 8080 after the script
 * that started it is gone, which is exactly the state the next run
 * refuses to start in. So the teardown is registered as a signal handler
 * as well, and the handler re-raises the signal afterwards so this process
 * still dies of what killed it rather than reporting a tidy exit code
 * nobody asked for.
 */
export async function withDevServer(body) {
  if (!existsSync(resolve(UI_DIR, "node_modules"))) {
    throw new Error(UI_DIR + " has no installed dependencies. Fix it with: cd " + UI_DIR + " && npm ci");
  }
  const port = await assertPortFree(DEV_PORT);
  const base = "http://127.0.0.1:" + port;

  // --strictPort so a lost race fails loudly rather than serving a
  // different port than the one this script then navigates to, and an
  // explicit --host because Vite's default `localhost` binds ::1 only on
  // macOS, which the 127.0.0.1 probe above cannot then reach.
  const dev = spawn("npm", ["run", "dev", "--", "--host", "127.0.0.1", "--port", String(port), "--strictPort"], {
    cwd: UI_DIR,
    stdio: ["ignore", "ignore", "inherit"]
  });

  const { chromium } = loadPlaywright();
  let browser;

  /** Kills the server FIRST and closes the browser second, each on its
   *  own, because the previous order let a throwing browser.close() skip
   *  the kill and leak the port. Idempotent, because both the finally
   *  below and a signal handler can reach it. */
  let torn = false;
  const teardown = async () => {
    if (torn) return;
    torn = true;
    try {
      if (dev.exitCode === null && dev.signalCode === null) {
        dev.kill("SIGTERM");
        // Wait for it to actually go. Returning while the child is still
        // shutting down leaves the port held for a moment, and the next
        // run's assertPortFree is what trips over it.
        await Promise.race([once(dev, "exit"), new Promise((r) => setTimeout(r, 5000))]);
      }
    } catch {
      // Nothing useful to do about a kill that failed, and it must not
      // stop the browser being closed.
    }
    try {
      if (browser) await browser.close();
    } catch {
      // Same.
    }
  };

  const signals = ["SIGINT", "SIGTERM", "SIGHUP"];
  const onSignal = (signal) => {
    void teardown().then(() => {
      for (const s of signals) process.removeListener(s, onSignal);
      process.kill(process.pid, signal);
    });
  };
  for (const s of signals) process.once(s, onSignal);

  try {
    await waitForServer(base + "/", dev);
    browser = await launchChromium(chromium);
    return await body({ base, browser });
  } finally {
    for (const s of signals) process.removeListener(s, onSignal);
    await teardown();
  }
}

/**
 * One browser context, one page, pointed at the app and signed in.
 *
 * `session` is returned rather than baked in because two flows need to
 * change their mind about it mid-capture: the enrollment screens only
 * exist while it says 401, and the app only renders while it says 200.
 */
export async function openApp(
  { base, browser },
  { viewport = VIEWPORT, scale = SCALE, authenticated = true, path = "/", scenario = "default" } = {}
) {
  // Locale and timezone are pinned for the same reason the clock is. Every
  // time in this app is rendered with toLocaleTimeString, so on an
  // unpinned context the fixtures read as 17:01 on one machine and 02:01
  // on another and a re-record differs everywhere a stamp appears. Berlin
  // because the fixtures' own retention policy is written in it, so the
  // times on screen agree with the timezone the settings page reports.
  const context = await browser.newContext({
    viewport,
    deviceScaleFactor: scale,
    locale: "en-GB",
    timezoneId: "Europe/Berlin"
  });
  const session = { authenticated };

  // The one route the mock API cannot answer.
  await context.route("**/api/v1/auth/session", (route) =>
    route.fulfill(
      session.authenticated
        ? { status: 200, contentType: "application/json", body: JSON.stringify({ username: EXAMPLE.adminUser }) }
        : { status: 401, contentType: "application/json", body: JSON.stringify({ message: "not authenticated" }) }
    )
  );

  const page = await context.newPage();
  // Before the first navigation, or the app has already read the clock.
  await page.clock.setFixedTime(FROZEN_TIME);

  if (path !== null) {
    const q = path.includes("?") ? "&" : "?";
    await page.goto(base + path + q + "scenario=" + scenario);
  }
  return { context, page, session };
}

/** Settles the fonts and any in-flight mock delay, so a re-shoot of an
 *  unchanged screen produces an unchanged file. */
export async function settle(page, ms = 250) {
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(ms);
}

// --------------------------------------------------------------- stills

/**
 * A still, clipped to a selector or to the content, written to
 * docs/site/screens/.
 *
 * The clip is the interesting half. Several screens only fill two thirds
 * of a 900px window, and a screenshot that is a third empty reads on the
 * page as a rendering fault rather than as a short form. Taller-than-
 * viewport screens fall through to a full-page capture instead of being
 * cut off.
 */
export async function shot(page, name, clipSel, { pad = 28, log = true } = {}) {
  await settle(page);
  const clip = await clipRect(page, clipSel, pad);
  const file = resolve(SCREENS, name + ".png");
  mkdirSync(SCREENS, { recursive: true });
  await page.screenshot({ path: file, animations: "disabled", ...(clip ? { clip } : { fullPage: true }) });
  if (log) console.log("  " + name + ".png");
  return name;
}

async function clipRect(page, clipSel, pad) {
  // An explicit rectangle wins over everything. It is how a capture asks
  // for "the top 900px of this window", which is the only honest way to
  // photograph the whole application shell out of a viewport made tall on
  // purpose so nothing needs a full-page stitch.
  if (clipSel && !Array.isArray(clipSel) && typeof clipSel === "object") return clipSel;
  if (clipSel) {
    // An array is the union of everything it matches, which is how a
    // capture says "from the heading to the bottom of the card" without a
    // wrapper element existing for it. The shell gives <main> a minimum of
    // a full viewport, so clipping to main on a tall window is most of a
    // field of empty background; clipping to what is actually drawn is
    // not.
    const selectors = Array.isArray(clipSel) ? clipSel : [clipSel];
    let box = null;
    for (const sel of selectors) {
      for (const el of await page.locator(sel).all()) {
        // Into view before measuring, because a bounding box is viewport
        // relative and a clip outside the viewport is rejected outright.
        // It is a no-op when the element is already on screen, so a clip
        // that takes twenty frames of one card does not jitter.
        await el.scrollIntoViewIfNeeded();
        const b = await el.boundingBox();
        if (!b) continue;
        box = box === null
          ? b
          : {
              x: Math.min(box.x, b.x),
              y: Math.min(box.y, b.y),
              width: Math.max(box.x + box.width, b.x + b.width) - Math.min(box.x, b.x),
              height: Math.max(box.y + box.height, b.y + b.height) - Math.min(box.y, b.y)
            };
      }
    }
    if (!box) throw new Error("nothing matched " + selectors.join(", "));
    const viewport = page.viewportSize();
    const x = Math.max(0, Math.round(box.x - pad));
    const y = Math.max(0, Math.round(box.y - pad));
    // Clamped against what is left of the viewport from x and y, not
    // against its full width and height. A box near the right edge with a
    // pad on it overhangs, and Playwright truncates the capture without
    // saying so, which is a picture quietly missing a strip of itself.
    return {
      x,
      y,
      width: Math.min(viewport.width - x, Math.round(box.width + pad * 2)),
      height: Math.min(viewport.height - y, Math.round(box.height + pad * 2))
    };
  }
  const viewport = page.viewportSize();
  const content = await page.evaluate(() => {
    const root = document.getElementById("root");
    return root ? Math.ceil(root.getBoundingClientRect().height) : 0;
  });
  const height = content + 32;
  if (content > 0 && height < viewport.height) {
    return { x: 0, y: 0, width: viewport.width, height };
  }
  return null;
}

/**
 * These are flat UI screenshots: a few dozen distinct colours, large runs
 * of one value, no photographic gradient anywhere. A 256-colour palette
 * is visually identical and roughly a third of the size, which matters
 * because every one of these lands in git history forever. Skipped, with
 * a line saying so, on a machine with no ImageMagick, so a re-shoot there
 * still produces correct (if larger) pictures.
 */
export function optimise(names) {
  const magick = spawnSync("magick", ["-version"], { stdio: "ignore" });
  if (magick.error || magick.status !== 0) {
    console.log("ImageMagick not found, leaving the PNGs at full colour depth");
    return;
  }
  let before = 0;
  let after = 0;
  for (const name of names) {
    const file = resolve(SCREENS, name + ".png");
    before += statSync(file).size;
    // Through a temporary and then a rename, rather than reading and
    // writing the same path. These are committed binaries, and a crash
    // halfway through an in-place rewrite leaves a corrupt one in the
    // working tree looking like a change somebody made on purpose.
    const tmp = file + ".tmp";
    const r = spawnSync("magick", [file, "-strip", "-dither", "None", "-colors", "256", "PNG8:" + tmp], { stdio: "inherit" });
    if (r.status !== 0) {
      rmSync(tmp, { force: true });
      throw new Error("magick failed on " + file);
    }
    renameSync(tmp, file);
    after += statSync(file).size;
  }
  console.log(
    "quantised to 256 colours: " +
      (before / 1024 / 1024).toFixed(2) + " MB -> " + (after / 1024 / 1024).toFixed(2) + " MB"
  );
}

// ----------------------------------------------------------------- GIFs

/**
 * Where ffmpeg is, decided by asking rather than by assuming.
 *
 * This used to be one hard-coded Apple Silicon Homebrew path, which is a
 * statement about the machine it was written on and about nothing else:
 * on a Linux box, an Intel Mac, or a Mac where ffmpeg came from anywhere
 * but that formula, every capture script failed at the encode with a path
 * that had never existed there.
 *
 * PATH first, because a machine that has ffmpeg on PATH has the one its
 * owner chose. The Homebrew paths are a fallback for a shell that has not
 * picked them up, and FFMPEG overrides both. Which one was taken is
 * printed with the first clip, so a surprising encode is one line away
 * from being explained.
 */
function findFfmpeg() {
  const candidates = process.env.FFMPEG
    ? [process.env.FFMPEG]
    : [
        "ffmpeg",
        "/opt/homebrew/opt/ffmpeg-full/bin/ffmpeg",
        "/opt/homebrew/bin/ffmpeg",
        "/usr/local/bin/ffmpeg",
        "/usr/bin/ffmpeg"
      ];
  for (const c of candidates) {
    const r = spawnSync(c, ["-version"], { stdio: "ignore" });
    if (!r.error && r.status === 0) return c;
  }
  throw new Error(
    "no ffmpeg. Looked for: " + candidates.join(", ") + "\n" +
      "Install it, or set FFMPEG to its path and re-run."
  );
}

let ffmpegPath = null;
function ffmpeg() {
  if (ffmpegPath === null) {
    ffmpegPath = findFfmpeg();
    console.log("encoding with " + ffmpegPath);
  }
  return ffmpegPath;
}

/**
 * A GIF, recorded as a scripted sequence of held frames rather than as a
 * video of the browser.
 *
 * # Why not `recordVideo`
 *
 * Playwright will record the context to webm and that webm converts to a
 * GIF, and the result is wrong in three ways at once. It samples at a
 * fixed rate, so how many frames a step gets depends on how fast the
 * machine ran it, which means the same script produces a different file
 * every time and a diff of the output stops meaning anything. It spends
 * most of its frames on nothing happening, which is most of the bytes. And
 * it gives a reader no time on the frame that matters, because the
 * interaction it is a video of is one an operator does at typing speed
 * while a reader is trying to see what changed.
 *
 * So a clip here is a list of (frame, how long to hold it). The script
 * drives the real app, really clicks the real controls, and asks for a
 * frame at the moments worth seeing. Nothing is staged and no frame is
 * drawn by hand: every one of them is a screenshot of the running
 * application, taken between two genuine interactions with it.
 *
 * The result is deterministic in the way that matters. The same script
 * produces the same frame count with the same durations on any machine,
 * so a re-record differs only where the application's own rendering
 * differs.
 *
 * "Only where the rendering differs" is not "never", and it is worth
 * saying which cases those are rather than claiming a determinism this
 * does not have. A surface the application animates on its own, such as
 * the per-set activity panel's spinner and progress bar, is caught at
 * whatever phase it was in, so those clips move by a few kilobytes
 * between runs. What is pinned is everything that was drifting for no
 * reason: the frame count, the hold times, the crop, the clock, the
 * timezone, the locale and the port.
 *
 * # Size
 *
 * Two things keep these small enough to put a dozen on one page. The
 * frames are held, so a fifteen-second clip is twenty pictures rather than
 * three hundred; and the palette is generated across the whole clip with
 * `stats_mode=diff`, which spends its colours on the parts that change.
 * Dithering is off: this is flat UI with hard text edges, and a dither
 * pattern on a solid panel is both uglier and several times larger.
 */
export class Clip {
  /**
   * @param {import("playwright-core").Page} page
   * @param {string} name output basename, written to docs/site/screens/
   * @param {object} opts
   * @param {string=} opts.clip selector to crop every frame to
   * @param {number=} opts.width output width in pixels
   * @param {number=} opts.colors palette size
   * @param {number=} opts.pad padding around the crop selector
   */
  constructor(page, name, { clip = null, width = 900, pad = 0, colors = 64 } = {}) {
    // Validated, because `name` decides a path. It used to be concatenated
    // into a directory under docs/site/screens/ which the constructor then
    // deleted, so `new Clip(page, "../..")` resolved to docs/site and
    // removed the site. It is a file name, it is only ever a file name,
    // and now it has to look like one.
    if (!/^[a-z0-9][a-z0-9-]*$/.test(name)) {
      throw new Error(
        "clip name " + JSON.stringify(name) + " is not a file name. " +
          "Lower case letters, digits and hyphens, because it becomes docs/site/screens/<name>.gif."
      );
    }
    this.page = page;
    this.name = name;
    this.clipSel = clip;
    this.width = width;
    this.pad = pad;
    this.colors = colors;
    this.frames = [];
    // Frames go to a fresh temporary directory OUTSIDE the repository.
    // They used to be written to a dot-directory inside
    // docs/site/screens/, which is a tracked directory of committed
    // binaries that ignores nothing by that name, and every failure path
    // in write() throws before the cleanup, so a bad run left loose PNGs
    // in it looking like something somebody meant to add.
    this.dir = mkdtempSync(join(tmpdir(), "rcm-site-clip-" + name + "-"));
  }

  /**
   * Capture one frame and hold it for `hold` seconds.
   *
   * The default is a beat: long enough to register, short enough that a
   * sequence of them reads as one motion. Pass more for the frame the clip
   * exists to show.
   */
  async frame(hold = 0.55, { settleFor = 120 } = {}) {
    await this.page.evaluate(() => document.fonts.ready);
    await this.page.waitForTimeout(settleFor);
    const clip = await this.rectFor();
    const file = resolve(this.dir, String(this.frames.length).padStart(4, "0") + ".png");
    await this.page.screenshot({ path: file, animations: "disabled", ...(clip ? { clip } : {}) });
    this.frames.push({ file, hold });
    return this;
  }

  /**
   * The rectangle to photograph, the same size every frame and in the
   * right place on this one.
   *
   * The SIZE is measured once and locked. Every frame of a GIF has to be
   * the same size, and a card that grows when a report lands in it is
   * not: measuring per frame produced a clip whose frames were 618px and
   * 1326px tall, which ffmpeg squashed onto one canvas.
   *
   * The POSITION is not locked, and that is the correction to the first
   * version of this. A bounding box is relative to the viewport, so a
   * rectangle locked at frame 0 and reused after the page has scrolled
   * photographs a different part of the document while looking exactly as
   * intended. Two clips here scroll mid-recording. So the anchor is kept
   * in document coordinates and converted back per frame, which means the
   * crop follows the thing it is a crop of.
   *
   * A region that has left the viewport entirely is a failure and not a
   * clamp: a picture of the wrong place is worse than a script that
   * stops.
   */
  async rectFor() {
    if (this.clipSel === null || this.clipSel === undefined) {
      if (!this.rect) this.rect = await clipRect(this.page, this.clipSel, this.pad);
      return this.rect;
    }
    if (typeof this.clipSel === "object" && !Array.isArray(this.clipSel)) return this.clipSel;

    const here = await clipRect(this.page, this.clipSel, this.pad);
    const scrollY = await this.page.evaluate(() => window.scrollY);
    if (!this.anchor) {
      this.anchor = { x: here.x, docY: here.y + scrollY, width: here.width, height: here.height };
      return { x: here.x, y: here.y, width: here.width, height: here.height };
    }

    const y = Math.round(this.anchor.docY - scrollY);
    const viewport = this.page.viewportSize();
    if (y < 0 || y + this.anchor.height > viewport.height) {
      throw new Error(
        this.name + " frame " + this.frames.length + ": the region this clip is of has scrolled " +
          (y < 0 ? "above" : "below") + " the window (y " + y + ", height " + this.anchor.height +
          ", viewport " + viewport.height + "). Scroll it back into view before this frame, or " +
          "start a second clip, rather than photographing whatever is there instead."
      );
    }
    return { x: this.anchor.x, y, width: this.anchor.width, height: this.anchor.height };
  }

  /** Hold the frame already captured for longer, without taking another
   *  picture of it. Cheaper than a second screenshot and, more to the
   *  point, guaranteed identical to the one before it. */
  hold(seconds) {
    if (this.frames.length === 0) throw new Error("hold() before the first frame of " + this.name);
    this.frames[this.frames.length - 1].hold += seconds;
    return this;
  }

  /** Encode, report, and remove the frames. Returns { name, width,
   *  height, bytes } so the caller can print one table at the end. */
  async write() {
    if (this.frames.length < 2) throw new Error(this.name + " has " + this.frames.length + " frames");

    // The concat demuxer's own format. The last entry is repeated because
    // its duration is otherwise ignored, which is a documented quirk of
    // the demuxer and not a workaround for anything here.
    const list = ["ffconcat version 1.0"];
    for (const f of this.frames) {
      list.push("file '" + f.file + "'");
      list.push("duration " + f.hold.toFixed(3));
    }
    list.push("file '" + this.frames[this.frames.length - 1].file + "'");
    const listFile = resolve(this.dir, "frames.txt");
    writeFileSync(listFile, list.join("\n") + "\n");

    const out = resolve(SCREENS, this.name + ".gif");
    const filter =
      "scale=" + this.width + ":-2:flags=lanczos,split[a][b];" +
      "[a]palettegen=max_colors=" + this.colors + ":stats_mode=diff[p];" +
      "[b][p]paletteuse=dither=none:diff_mode=rectangle";

    const r = spawnSync(
      FFMPEG,
      [
        "-y", "-hide_banner", "-loglevel", "error",
        "-f", "concat", "-safe", "0", "-i", listFile,
        "-filter_complex", filter,
        "-fps_mode", "passthrough",
        "-loop", "0",
        out
      ],
      { stdio: "inherit" }
    );
    if (r.error) {
      throw new Error(
        "could not run ffmpeg at " + FFMPEG + ". Set FFMPEG to its path and re-run.\n" + r.error.message
      );
    }
    if (r.status !== 0) throw new Error("ffmpeg failed on " + this.name);

    const bytes = statSync(out).size;
    const height = Math.round((this.width * this.frameHeight()) / this.frameWidth());
    rmSync(this.dir, { recursive: true, force: true });
    const seconds = this.frames.reduce((n, f) => n + f.hold, 0);
    console.log(
      "  " + this.name + ".gif  " + this.width + "px  " + this.frames.length + " frames  " +
        seconds.toFixed(1) + "s  " + (bytes / 1024).toFixed(0) + " KB"
    );
    return { name: this.name, width: this.width, height, bytes, frames: this.frames.length, seconds };
  }

  frameWidth() {
    return this._size().width;
  }
  frameHeight() {
    return this._size().height;
  }
  _size() {
    if (!this._cached) {
      // PNG's IHDR is at a fixed offset, which beats shelling out to
      // ImageMagick for two integers.
      const buf = readFileSync(this.frames[0].file);
      this._cached = { width: buf.readUInt32BE(16), height: buf.readUInt32BE(20) };
    }
    return this._cached;
  }
}

/** Types into a field a chunk at a time, taking a frame per chunk, so a
 *  filled form reads as somebody filling it rather than as a value that
 *  teleported in. */
export async function typeInto(clip, locator, value, { chunks = 3, hold = 0.32 } = {}) {
  const size = Math.ceil(value.length / chunks);
  await locator.click();
  for (let i = 0; i < value.length; i += size) {
    await locator.fill(value.slice(0, Math.min(value.length, i + size)));
    await clip.frame(hold);
  }
  await locator.blur();
}

/** Every GIF and PNG currently in screens/, with its size, for the line
 *  every script prints at the end. */
export function screensTotal() {
  const files = readdirSync(SCREENS).filter((f) => f.endsWith(".png") || f.endsWith(".gif"));
  const bytes = files.reduce((n, f) => n + statSync(resolve(SCREENS, f)).size, 0);
  return { count: files.length, bytes };
}

export function mb(bytes) {
  return (bytes / 1024 / 1024).toFixed(2) + " MB";
}
