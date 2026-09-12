// The stack's own proof, run from the client machine.
//
// This is not a spec and it is not trying to be one: the Playwright suite
// over in backupdproject/backupd-tests is what asserts product behaviour.
// This file answers a narrower question, and it is the question a topology
// script has to answer before anyone builds on it. Is there a real browser
// in this container, can it reach the UI container over the private
// network, does the reverse proxy carry its API calls to the engine, and
// does the engine answer them out of a SQLite database with real rows in
// it. Four hops, and every one of them is missing from a suite that runs
// `npm run dev` against createMockApi.
//
// # What it looks at, and why the Activity page specifically
//
// The failure this whole stack exists for was on real hardware: the Activity
// page errored in the browser while the server returned HTTP 200 with
// valid JSON in 20 milliseconds. Nothing that watches the server can see
// that, and nothing that mocks the client can either, because the mock
// answers with the shape the client already expects. So this drives the
// real page against the real answer and treats a console error or an
// unhandled rejection as a failure, which is the only place that class of
// bug is visible at all.
//
// # No test runner here
//
// One file, `node smoke.mjs`, using the browser and the assertion library
// out of @playwright/test but not its runner. A runner would want a
// config, a config would want a baseURL, and this file would then be
// asserting things about the configuration it was handed rather than about
// the stack. It reads four environment variables and nothing else.
import { existsSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { setTimeout as sleep } from "node:timers/promises";
import { chromium, expect } from "@playwright/test";

const baseURL = req("RM_BASE_URL");
const username = req("RM_ADMIN_USERNAME");
const password = req("RM_ADMIN_PASSWORD");
const backupSet = process.env.RM_BACKUP_SET ?? "";
const artifacts = process.env.RM_ARTIFACTS_DIR ?? "/artifacts";
// Issue #795. Set by three-machine-web-ui.sh --break-engine, together
// with the directory the break is driven from. Unset, everything below
// that reads them is skipped and this file is the check it was.
const engineControl = process.env.RM_ENGINE_UNREACHABLE === "1" ? req("RM_ENGINE_CONTROL") : null;

function req(name) {
  const v = process.env[name];
  if (!v) {
    console.error(`smoke: ${name} is not set, and this check cannot invent one.`);
    process.exit(2);
  }
  return v;
}

function log(msg) {
  console.log(`    smoke: ${msg}`);
}

// Everything the page complained about while this ran, collected from the
// first navigation rather than per step, because a render error thrown on
// the dashboard and only noticed on Activity is still this run's finding.
//
// Two things are recorded and then not counted, and both are the product
// behaving correctly rather than a filter loosened until it went green.
//
//   A 401 WHILE SIGNED OUT. The app asks whether this browser has a session
//   before it decides what to render, and for a browser that does not the
//   correct answer is 401. Chromium logs every non-2xx resource load as a
//   console error, so counting those would mean the login page could never
//   pass, which is not a bar this product can clear or should have to. After
//   sign-in a 401 is counted, because then it means something genuinely lost
//   the session.
//
//   net::ERR_ABORTED, at any point. An aborted request is one the PAGE
//   cancelled: React tearing down an effect, or a navigation superseding a
//   fetch that is still in flight. That is a decision the client made, not a
//   failure the server caused, and React's own StrictMode produces them by
//   design. The guard against this hiding a real problem is not in here, it
//   is the content assertions below: a page that aborted the requests it
//   actually needed would render empty, and every step already requires
//   something specific to be on the screen.
//
//   THE FAILURES A DELIBERATE OUTAGE CAUSES (#795). --break-engine takes
//   the engine away on purpose, so the 502s, the failed requests and the
//   console errors during that window are the fault this run ASKED for.
//   Counting them would make the mode unable to pass; not counting them is
//   safe because the window is bounded by two explicit calls and because
//   the assertions inside it are stricter than the ones outside: a page
//   that went blank instead of complaining fails there.
//
//   What the window does NOT excuse is an uncaught exception (#795's
//   review). The guard used to read `!engineDown && (alwaysCounts || ...)`,
//   so `engineDown` overrode the one kind that is never acceptable, and a
//   React render crashing during the outage — the precise defect this
//   whole reproduction exists to find, one surface along — was recorded and
//   then excused. An unreachable engine is a reason for a 502; it is not a
//   reason for this bundle to throw.
//
// Everything else counts, and an uncaught exception counts unconditionally.
const complaints = [];
let signedIn = false;
let engineDown = false;

function record(kind, text, { alwaysCounts = false } = {}) {
  const abort = text.includes("net::ERR_ABORTED");
  const unauthorisedWhileSignedOut = !signedIn && /\b40[13]\b/.test(text);
  complaints.push({
    kind,
    text,
    // Recorded, so a complaint that counts can say WHEN it happened: an
    // uncaught exception thrown during the outage and one thrown after
    // the heal are different findings and get different sentences.
    engineDown,
    counts: alwaysCounts || !(engineDown || abort || unauthorisedWhileSignedOut)
  });
}

/**
 * Issue #795's control channel, the client half.
 *
 * This container has no Docker socket on purpose — a browser that can
 * stop containers is not the browser under test — so the engine is taken
 * away by asking the harness on the host for it. Files in a directory,
 * and the success ack is only written once the work has finished and been
 * VERIFIED (for "start", once the engine's own healthcheck passes), which
 * is the whole reason to wait for one rather than sleeping.
 *
 * Both acks are removed BEFORE the request goes out. Otherwise the
 * leavings of the previous cycle answer this one instantly and the run
 * continues against a stack in the opposite state to the one it believes
 * it is in.
 *
 * "stop-failed" / "start-failed" is the harness saying it could NOT reach
 * the state (#795's review). It matters that this is louder than the
 * timeout it replaces: a rig that cannot stop the engine leaves the
 * browser asserting an outage against a healthy stack, and a start whose
 * healthcheck never came good leaves every assertion after it reading a
 * half-open engine. Both used to arrive here as a success ack.
 */
async function engine(action, budgetMs) {
  const ack = `${engineControl}/${action === "stop" ? "stopped" : "started"}`;
  const failedAck = `${engineControl}/${action}-failed`;
  rmSync(ack, { force: true });
  rmSync(failedAck, { force: true });
  writeFileSync(`${engineControl}/${action}`, "");
  const deadline = Date.now() + budgetMs;
  for (;;) {
    if (existsSync(ack)) break;
    if (existsSync(failedAck)) {
      // The reason the harness wrote, quoted rather than summarised: it
      // is docker's own words about the container, and this file is not
      // in a position to improve on them.
      const reason = readFileSync(failedAck, "utf8").trim();
      throw new Error(
        `the harness could not "${action}" the engine: ${reason || "it gave no reason"}. ` +
          `That is a failure of the rig, not of the product: nothing below this line ran.`
      );
    }
    if (Date.now() > deadline) {
      throw new Error(
        `the harness did not acknowledge "${action}" within ${budgetMs}ms: ` +
          `nothing appeared at ${ack} and nothing at ${failedAck}. ` +
          `The watcher in scripts/e2e/three-machine-web-ui.sh is what writes both.`
      );
    }
    await sleep(200);
  }
  engineDown = action === "stop";
  log(`the engine is ${engineDown ? "stopped, and serve-ui is still up in front of it" : "running again"}`);
}

const browser = await chromium.launch({
  // --no-sandbox, and the harness is what decides. Chromium's own sandbox
  // wants either a setuid helper it can exec or a user namespace it can
  // clone into, and which of those is available depends on the uid this
  // container was given and on the host's seccomp profile. The harness
  // knows both; this file does not, so it does what it is told rather than
  // guessing and dying with "Running as root without --no-sandbox is not
  // supported", which is the message everyone meets and nobody enjoys.
  args: process.env.RM_CHROMIUM_NO_SANDBOX === "1" ? ["--no-sandbox"] : []
});
const context = await browser.newContext({
  viewport: { width: 1440, height: 900 },
  // The #730 reproduction (three-machine-web-ui.sh --front-proxy-tls) puts a
  // TLS + HTTP/2 reverse proxy with a self-signed leaf in front of serve-ui,
  // so the browser negotiates h2 the way a real NAS's front door does rather
  // than the plain HTTP/1.1 this rig otherwise uses. Trust the leaf: the
  // point is the transport, not certificate provenance. Off unless the
  // harness sets it, so the default plain-HTTP run is unchanged.
  ignoreHTTPSErrors: process.env.RM_IGNORE_HTTPS === "1"
});
const page = await context.newPage();

page.on("console", (m) => {
  if (m.type() === "error") record("console.error", m.text());
});
// Unconditional: nothing a correctly working page does throws an exception
// nobody caught, signed in or out.
page.on("pageerror", (e) => record("uncaught", e.message, { alwaysCounts: true }));
page.on("requestfailed", (r) => {
  record(
    "request failed",
    `${r.method()} ${r.url()} (${r.failure()?.errorText ?? "no reason given"})`
  );
});

let failure = null;
try {
  // ------------------------------------------------ the page is served
  const response = await page.goto(baseURL + "/", { waitUntil: "domcontentloaded" });
  if (!response) throw new Error(`no response at all from ${baseURL}/`);
  if (response.status() !== 200) {
    throw new Error(`${baseURL}/ answered ${response.status()}, want 200`);
  }
  log(`GET / answered ${response.status()} from ${response.url()}`);

  // ------------------------------------------------------- signed out
  //
  // The heading, not the URL. A router that rendered nothing at all would
  // still be sitting at "/", and an empty page passing a login check is
  // how a suite ends up proving that a server is listening.
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible({ timeout: 20_000 });
  log("the login page rendered");

  // --------------------------------------------------------- sign in
  //
  // A real POST through serve-ui's reverse proxy to the engine's local
  // auth, answered out of the administrator record the harness created
  // with `backupd-web auth create-admin`. Nothing here is mocked, so a wrong
  // password fails exactly the way a wrong password fails in production.
  await page.getByLabel("Username").fill(username);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();

  await expect(page.getByRole("navigation", { name: "Sections" })).toBeVisible({ timeout: 20_000 });
  signedIn = true;
  log("the generated administrator signed in and the shell rendered");

  // ------------------------------------------------- the seeded state
  if (backupSet) {
    await page.getByRole("navigation", { name: "Sections" })
      .getByRole("link", { name: /Backup sets/i })
      .click();
    await expect(page.getByRole("heading", { level: 1, name: "Backup sets" })).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText(backupSet, { exact: false }).first()).toBeVisible({ timeout: 20_000 });
    log(`the seeded backup set ${backupSet} is listed, read out of the engine's own database`);
  }

  // --------------------------------------------------- the Activity page
  await page.getByRole("navigation", { name: "Sections" })
    .getByRole("link", { name: /Activity/i })
    .click();
  await expect(page.getByRole("heading", { level: 1, name: "Activity" })).toBeVisible({ timeout: 20_000 });
  log("the Activity page rendered against a real lifecycle journal");

  // A render that threw and recovered still rendered, so the heading above
  // is not the whole answer. This is.
  const counted = complaints.filter((c) => c.counts);
  if (counted.length > 0) {
    throw new Error(
      "the browser complained while these pages rendered, which is the exact shape of the bug this stack exists for:\n" +
        counted.map((c) => `      ${c.kind}: ${c.text}`).join("\n")
    );
  }
  const excused = complaints.filter((c) => !c.counts);
  log(
    "and nothing was logged to the console, thrown, or failed to load" +
      (excused.length > 0
        ? ` (${excused.length} recorded and not counted, see this file's header for which two shapes those are)`
        : "")
  );
  for (const c of excused) log(`  not counted: ${c.kind}: ${c.text}`);

  // ------------------------------- the engine goes away underneath it
  //
  // Issue #795, and only when the harness asked for it. Everything above
  // ran against a working stack; this takes the engine out from under a
  // browser that is already signed in and holding the app, which is the
  // shape the fault was reported in: the container was renamed under a
  // loaded SPA, so serve-ui stayed up and answered every /api/v1 call 502
  // on behalf of a service it could no longer reach.
  //
  // What is asserted is not that the page fails. It is that the failure
  // ARRIVES: the page that showed nothing on a real NAS has to put
  // something on screen that names what went wrong, or this mode is
  // measuring a blank page and calling it a pass.
  if (engineControl) {
    const nav = page.getByRole("navigation", { name: "Sections" });
    const surfaced = page.getByRole("alert").filter({ hasText: /did not answer|could not reach/i });

    await engine("stop", 60_000);

    // Leave and come back rather than reloading. A reload would re-run the
    // session check against the same broken hop and land on the
    // service-unreachable screen, which is a different (also correct)
    // surface; the one #795 is about is the page re-fetching inside a
    // session that is already established.
    await nav.getByRole("link", { name: /Dashboard/i }).click();
    await nav.getByRole("link", { name: /Activity/i }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Activity" })).toBeVisible({ timeout: 20_000 });

    await expect(surfaced.first()).toBeVisible({ timeout: 30_000 });
    const said = await surfaced.first().innerText();
    log(`with the engine gone, the Activity page says: ${said.split("\n")[0]}`);

    // The two wordings are different problems for whoever is fixing them
    // (#598), and this is the one that is NOT "the answer could not be
    // read": nothing the browser received came from the engine at all.
    if (/could not read the answer/i.test(said)) {
      throw new Error(
        "the Activity page called an unreachable engine an unreadable answer, which sends whoever reads it looking at the wrong machine:\n      " +
          said.replace(/\n/g, "\n      ")
      );
    }
    // The literal string #598 removed, checked on the whole page rather
    // than the banner: an id that appears in no log is worse than no id,
    // and this is the one place it could come back.
    const pageText = await page.locator("body").innerText();
    if (/correlation id unavailable/i.test(pageText)) {
      throw new Error("the failure offered the literal correlation id \"unavailable\", which matches nothing in any log (#598).");
    }
    // And it is a FAILURE on screen, not an empty feed. "Nothing has
    // happened in this window" on a NAS whose engine is unreachable is
    // the exact lie this whole reproduction exists to catch.
    await expect(page.getByText("No matching events")).toHaveCount(0);

    // Try again, against a service that is still down. The button has to
    // really re-issue: one that looks inert is one people press harder.
    await page.getByRole("button", { name: /try again/i }).first().click();
    await expect(page.getByText(/Tried again at /)).toBeVisible({ timeout: 30_000 });
    await expect(surfaced.first()).toBeVisible();
    log("Try again re-issued the request and the page said so rather than going quiet");

    // The dashboard's own panel is the quieter half of the same bug: it
    // ran the identical fetch and used to draw empty, which is
    // indistinguishable from a NAS where nothing has ever happened.
    await nav.getByRole("link", { name: /Dashboard/i }).click();
    await expect(surfaced.first()).toBeVisible({ timeout: 30_000 });
    log("and the dashboard surfaces it too rather than drawing an empty panel");

    // ------------------------------------------------ and it recovers
    await engine("start", 180_000);

    // The restart ENDS the session, sometimes. The engine keeps its
    // sessions in its own process (apps/common/auth/local), so the
    // container that went away took them with it, and whether this
    // browser is still signed in afterwards depends on nothing this file
    // controls. BOTH outcomes are correct, and the recovery is only
    // proven by following whichever one happened.
    //
    // Which is the bug in what this block used to do (#795's review). It
    // waited for the Activity heading and for the banner to clear, and a
    // dead session satisfies both by rendering the LOGIN page: no
    // Activity feed, no error alert, and a green recovery with the
    // operator sitting on a sign-in form. So the transition is followed
    // here, and what is asserted at the end of it is a real feed.
    const signInHeading = page.getByRole("heading", { name: "Sign in" });
    const activityHeading = page.getByRole("heading", { level: 1, name: "Activity" });
    if (await nav.isVisible()) {
      await nav.getByRole("link", { name: /Activity/i }).click();
    }
    // Whichever arrives first. `.or` is a locator, not a race between two
    // awaits, so neither outcome is a timeout that has to be caught.
    await expect(activityHeading.or(signInHeading).first()).toBeVisible({ timeout: 60_000 });

    if (await signInHeading.isVisible()) {
      // The session died with the container, which is the common case and
      // the one the container run found this product handling badly: what
      // the operator got was an in-panel "authentication required" with a
      // Try again beside it, and re-issuing a read cannot mend a session
      // that no longer exists. Reaching this form IS the fix working.
      log("the restart ended the session, and the app offered the sign-in form rather than a dead retry");
      // Back to signed-out for the recorder's own rule about 401s: the app
      // is about to ask /auth/session again with no session to show.
      signedIn = false;
      await page.getByLabel("Username").fill(username);
      await page.getByLabel("Password", { exact: true }).fill(password);
      await page.getByRole("button", { name: "Sign in" }).click();
      await expect(page.getByRole("navigation", { name: "Sections" })).toBeVisible({ timeout: 30_000 });
      signedIn = true;
      await nav.getByRole("link", { name: /Activity/i }).click();
    } else {
      log("the session survived the restart, and the app went straight back to the feed");
    }

    await expect(activityHeading).toBeVisible({ timeout: 30_000 });
    // A banner that never goes away is its own defect: an operator who
    // fixed the deployment has to be able to see that they fixed it.
    await expect(surfaced).toHaveCount(0, { timeout: 30_000 });
    // And no error alert of ANY wording, which is the stronger statement:
    // `surfaced` only matches the two #795 sentences, so a page that
    // recovered into a different failure (an UNAUTHENTICATED read, a
    // half-open database) would have passed the line above.
    await expect(page.getByRole("main").getByRole("alert")).toHaveCount(0, { timeout: 30_000 });

    // The feed itself, because "no error" is not the same fact as
    // "reading again". Either rows out of the journal — the seeded cycle
    // put some there and the restart did not empty the database, it is on
    // a volume — or the healthy empty state that says so in words. Never
    // neither, which is what a page that rendered nothing at all looks
    // like, and that is the shape of the original report.
    const rows = page.getByRole("main").getByRole("listitem");
    const emptyFeed = page.getByText("No matching events");
    if ((await rows.count()) === 0 && (await emptyFeed.count()) === 0) {
      throw new Error(
        "the Activity page recovered into neither a feed nor its empty state, so nothing " +
          "on screen says the read worked: this is the blank page #795 was reported as, " +
          "arrived at from the other direction."
      );
    }
    log(
      `the engine came back and the Activity page read the journal again (${await rows.count()} events on screen)`
    );

    // Nothing may have gone wrong AFTER the break was healed, and nothing
    // may have THROWN during it: complaints recorded inside the window
    // were excused as they were recorded, except the uncaught ones, which
    // are never excused (see record above).
    const counting = complaints.filter((c) => c.counts);
    if (counting.length > 0) {
      const duringOutage = counting.filter((c) => c.engineDown);
      throw new Error(
        (duringOutage.length > 0
          ? "the browser threw while the engine was deliberately down, which this mode does not excuse: " +
            "a 502 is the fault it asked for and an uncaught exception is not:\n"
          : "the browser complained on a stack that was working again, so the recovery is not clean:\n") +
          counting.map((c) => `      ${c.kind}: ${c.text}`).join("\n")
      );
    }
  }
} catch (err) {
  failure = err;
}

// The screenshot on the way out either way, because a green run whose
// evidence is a line of text is a green run nobody can check.
try {
  await page.screenshot({ path: `${artifacts}/smoke-${failure ? "failed" : "passed"}.png`, fullPage: true });
  log(`screenshot written to ${artifacts}/smoke-${failure ? "failed" : "passed"}.png`);
} catch (e) {
  log(`could not write the screenshot: ${e.message}`);
}

await context.close();
await browser.close();

if (failure) {
  console.error("");
  console.error("==> smoke: FAILED. " + failure.message);
  process.exit(1);
}
