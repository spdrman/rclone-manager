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
import { createRequire } from "node:module";
import { createServer } from "node:net";
import {
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  writeFileSync
} from "node:fs";
import { dirname, resolve } from "node:path";
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
  fakeKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nEXAMPLE-PLACEHOLDER-NOT-A-KEY\n-----END OPENSSH PRIVATE KEY-----\n",
  // For the storage-destination wizard. The access key is the literal
  // string the AWS documentation uses for an example and the secret is
  // typed into a password field that renders as dots, but both are also
  // never sent anywhere: the API on the other end is an in-memory object.
  s3: {
    name: "offsite-b2",
    endpoint: "https://s3.us-west-002.backblazeb2.com",
    region: "us-west-002",
    bucket: "nas-offsite-archive",
    accessKey: "EXAMPLE-ACCESS-KEY-ID",
    secretKey: "EXAMPLE-SECRET-not-a-real-one"
  }
};

function readPin() {
  const pin = readFileSync(resolve(REPO, "scripts/e2e/tests-repo.pin"), "utf8");
  const sha = /^TESTS_REPO_SHA=([0-9a-f]{40})$/m.exec(pin);
  if (!sha) throw new Error("scripts/e2e/tests-repo.pin carries no full TESTS_REPO_SHA");
  return sha[1];
}

export function loadPlaywright() {
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
  try {
    await waitForServer(base + "/", dev);
    browser = await chromium.launch();
    return await body({ base, browser });
  } finally {
    if (browser) await browser.close();
    dev.kill("SIGTERM");
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
    return {
      x: Math.max(0, Math.round(box.x - pad)),
      y: Math.max(0, Math.round(box.y - pad)),
      width: Math.min(viewport.width, Math.round(box.width + pad * 2)),
      height: Math.min(viewport.height, Math.round(box.height + pad * 2))
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
    const r = spawnSync("magick", [file, "-strip", "-dither", "None", "-colors", "256", "PNG8:" + file], { stdio: "inherit" });
    if (r.status !== 0) throw new Error("magick failed on " + file);
    after += statSync(file).size;
  }
  console.log(
    "quantised to 256 colours: " +
      (before / 1024 / 1024).toFixed(2) + " MB -> " + (after / 1024 / 1024).toFixed(2) + " MB"
  );
}

// ----------------------------------------------------------------- GIFs

const FFMPEG = process.env.FFMPEG ?? "/opt/homebrew/opt/ffmpeg-full/bin/ffmpeg";

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
    this.page = page;
    this.name = name;
    this.clipSel = clip;
    this.width = width;
    this.pad = pad;
    this.colors = colors;
    this.frames = [];
    this.dir = resolve(SCREENS, ".frames-" + name);
    // A frames directory left behind by a failed run would otherwise be
    // concatenated into the next one. Inside the repository's own output
    // directory, named by this clip, and created by this class.
    rmSync(this.dir, { recursive: true, force: true });
    mkdirSync(this.dir, { recursive: true });
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
    // Measured once and then locked for the rest of the clip. Every frame
    // of a GIF has to be the same size, and a card that grows when a
    // report lands in it does not: measuring per frame produced a clip
    // whose frames were 618px and 1326px tall, which ffmpeg squashed into
    // one canvas. Locking also means the crop cannot drift by a pixel
    // between two runs, which is half of what makes a re-record diffable.
    if (!this.rect) this.rect = await clipRect(this.page, this.clipSel, this.pad);
    const clip = this.rect;
    const file = resolve(this.dir, String(this.frames.length).padStart(4, "0") + ".png");
    await this.page.screenshot({ path: file, animations: "disabled", ...(clip ? { clip } : {}) });
    this.frames.push({ file, hold });
    return this;
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
