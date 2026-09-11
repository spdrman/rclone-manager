// The stack's own proof, run from the client machine.
//
// This is not a spec and it is not trying to be one: the Playwright suite
// over in spdrman/rclone-manager-tests is what asserts product behaviour.
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
import { chromium, expect } from "@playwright/test";

const baseURL = req("RM_BASE_URL");
const username = req("RM_ADMIN_USERNAME");
const password = req("RM_ADMIN_PASSWORD");
const backupSet = process.env.RM_BACKUP_SET ?? "";
const artifacts = process.env.RM_ARTIFACTS_DIR ?? "/artifacts";

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
// Everything else counts, and an uncaught exception counts unconditionally.
const complaints = [];
let signedIn = false;

function record(kind, text, { alwaysCounts = false } = {}) {
  const abort = text.includes("net::ERR_ABORTED");
  const unauthorisedWhileSignedOut = !signedIn && /\b40[13]\b/.test(text);
  complaints.push({
    kind,
    text,
    counts: alwaysCounts || !(abort || unauthorisedWhileSignedOut)
  });
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
  // with `rbm-web auth create-admin`. Nothing here is mocked, so a wrong
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
