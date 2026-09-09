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
// Everything about how the app is started, why it is never pointed at a
// real deployment, why there is an auth stub and where Playwright comes
// from lives in harness.mjs, which this and every other capture script
// here share. Read that file first; this one is just the walk.
//
// `?scenario=first-run` is what puts the app in the unconfigured state:
// `createMockApi` starts with `configured = false` for that scenario only,
// so the app renders the unconfigured shell instead of a dashboard with
// data behind it. It is a real scenario in the product's own fixtures,
// not something added here.
//
// # The shape of this flow changed under the old version of this file
//
// It used to walk a dedicated `FirstRunPage` whose heading was "Set up
// Backup Manager". That page is gone. #275 replaced it with the ordinary
// add-backup-set wizard running with `firstRun` set, reached the way an
// operator reaches it: enrol, land on a dashboard that says there is no
// configuration yet, and press Add backup set. Everything below follows
// the flow that exists rather than the one the old script described, and
// the step names moved with it ("Storage & validation", not "Storage &
// retention": retention is a deployment-wide policy now and is not asked
// for while a first set is being created).
//
// # Why the viewport is 1800 tall, and why there is no terminal in these
//
// #617 pinned the global terminal to the browser window. A fixed element
// and a full-page screenshot do not mix: Chromium paints the fixed panel
// where it sits, so a tall page captured full-page gets the terminal
// stamped across it with a field of empty background underneath, which is
// a picture of nothing that happens. A viewport taller than the longest
// screen here means every capture is a real viewport and no full-page
// stitch is ever needed.
//
// The cost is that the terminal, being pinned to the bottom of a window
// that is now taller than any of these pages, falls outside the clip. It
// is genuinely there on every one of these screens and it is genuinely
// not in these pictures, which first-run.html says in one line rather
// than leaving a reader to wonder. It has a page of its own, with moving
// pictures, because a docked log is a thing you watch rather than a thing
// you look at.

import { EXAMPLE, mb, openApp, optimise, screensTotal, shot, withDevServer } from "./harness.mjs";

/** The sign-in and enrolment screens centre a 452px card in a full
 *  viewport, so a plain capture is nine parts empty background. Clip
 *  those to the card plus a margin instead. */
const AUTH_CARD = "#root > div > div";

const VIEWPORT = { width: 1280, height: 1800 };

/** From the wizard's own heading to the bottom of its card, which has no
 *  wrapper element of its own, so it is asked for as a union. Clipping to
 *  <main> instead does not work: the shell gives it a minimum of a full
 *  viewport, so on a window made tall on purpose a main-shaped picture is
 *  two thirds empty background. */
const MAIN = ["main h1", "main section.card"];

/** The whole application window, at the size somebody actually has one.
 *  A crop of a viewport made tall on purpose, and used only for the two
 *  pictures that are about the shell rather than about a form. */
const WINDOW = { x: 0, y: 0, width: 1280, height: 900 };

await withDevServer(async (app) => {
  const { page, session } = await openApp(app, { path: null, authenticated: false, viewport: VIEWPORT });
  const shots = [];
  // 16px of margin rather than the default 28: the union below ends at
  // the bottom of the wizard card and the shell's footer line sits just
  // under it, so a wider pad catches half a sentence of it.
  const take = async (name, sel) => shots.push(await shot(page, name, sel, { pad: 16 }));

  // --------------------------------------------------------------- enrolment
  //
  // The real entry point. The engine prints an enrollment link on first
  // start (apps/common/auth/local/service.go, PrintBootstrapNotice) and
  // this is the page at the other end of it. The token in the URL is a
  // placeholder: the mock does not check it, and a real one must never
  // be committed.
  await page.goto(app.base + "/enroll?scenario=first-run&token=" + EXAMPLE.token);
  await page.getByRole("heading", { name: /Create Backup Manager administrator/ }).waitFor();
  await take("01-enrolment-empty", AUTH_CARD);

  await page.getByLabel("Username").fill(EXAMPLE.adminUser);
  await page.getByLabel(/^Password/).fill("short");
  await page.getByText(/Minimum 12 characters/).first().waitFor();
  await take("02-enrolment-password-too-short", AUTH_CARD);

  await page.getByLabel(/^Password/).fill(EXAMPLE.adminPassword);
  // Not getByLabel: PasswordInput puts a "Show confirm password" toggle
  // inside the same label, so the accessible name matches a button as
  // well as the field and a label lookup is ambiguous. The role pins
  // which of the two is meant.
  await page.getByRole("textbox", { name: "Confirm password" }).fill(EXAMPLE.adminPassword);
  await page.getByRole("button", { name: "Create administrator" }).waitFor({ state: "visible" });
  await take("03-enrolment-ready", AUTH_CARD);

  // Enrolment succeeds against the mock, and the app then asks the
  // bridge who is signed in. Flip the stub first so the answer is the
  // administrator that was just created.
  session.authenticated = true;
  await page.getByRole("button", { name: "Create administrator" }).click();

  // ------------------------------------------------- the unconfigured shell
  //
  // Not a setup page. The ordinary application, with nothing behind it
  // and a banner saying so. The banner deliberately carries no button of
  // its own, because the two pages that can act on it already offer Add
  // backup set and a banner repeating it would put the same primary
  // action on one page twice.
  await page.getByRole("navigation", { name: "Sections" }).waitFor();
  await page.getByText("Backup Manager has no configuration yet").waitFor();
  await take("04-first-run-dashboard", WINDOW);

  // ---------------------------------------------------------------- step 1
  await page.getByRole("button", { name: "Add backup set" }).first().click();
  await page.getByRole("heading", { name: "Add backup set", level: 1 }).waitFor();
  await take("05-wizard-step-1-source", MAIN);

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
  await take("06-wizard-step-1-filled", MAIN);

  // --------------------------------------------------------- authentication
  await page.getByRole("button", { name: "Authentication" }).click();
  await page.getByRole("heading", { name: "Authentication", level: 2 }).waitFor();
  await take("07-wizard-step-2-authentication", MAIN);

  await page.getByRole("radio", { name: /Import key/ }).check();
  await page.getByLabel(/Private key/).waitFor();
  await take("08-wizard-step-2-import-key", MAIN);

  await page.getByLabel(/Private key/).fill(EXAMPLE.fakeKey);
  await page.getByRole("button", { name: "Import key" }).click();
  await page.getByText("Key imported").waitFor();
  await take("09-wizard-step-2-key-imported", MAIN);

  // --------------------------------------------------------- verify server
  await page.getByRole("button", { name: "Verify server" }).click();
  await page.getByRole("heading", { name: "Verify server", level: 2 }).waitFor();
  await page.getByRole("button", { name: "Trust host" }).waitFor();
  await take("10-wizard-step-3-verify-server", MAIN);

  await page.getByRole("button", { name: "Trust host" }).click();
  await page.getByRole("button", { name: "Host trusted" }).waitFor();
  await take("11-wizard-step-3-host-trusted", MAIN);

  // ------------------------------------------------------------- discovery
  await page.getByRole("button", { name: "Discovery" }).click();
  await page.getByRole("heading", { name: "Backup discovery", level: 2 }).waitFor();
  await fill("Remote folder", EXAMPLE.remoteFolder);
  await fill("Include patterns", EXAMPLE.include);
  await take("12-wizard-step-4-discovery", MAIN);

  await page.getByRole("radio", { name: /Atomic rename/ }).check();
  await take("13-wizard-step-4-completion-atomic-rename", MAIN);

  // --------------------------------------------------- storage / validation
  await page.getByRole("button", { name: "Storage & validation" }).click();
  await page.getByRole("heading", { name: /Storage and validation/, level: 2 }).waitFor();
  await fill("NAS destination", EXAMPLE.destination);
  await take("14-wizard-step-5-storage-validation", MAIN);

  // ----------------------------------------------------------------- review
  await page.getByRole("button", { name: "Review" }).click();
  await page.getByRole("heading", { name: "Review", level: 2 }).waitFor();
  await take("15-wizard-step-6-review", MAIN);

  // #624. Nothing has been proven until this runs, and none of the three
  // save buttons is available until it comes back clean.
  await page.getByRole("button", { name: "Test connection" }).click();
  await page.getByText(/list/i).first().waitFor();
  await page.waitForTimeout(1200);
  await take("16-wizard-step-6-connection-proven", MAIN);

  await page.getByRole("checkbox", { name: /I understand the remote backup will be removed/ }).check();
  await take("17-wizard-step-6-acknowledged", MAIN);

  // ----------------------------------------------------------- configured app
  //
  // "Finish setup", not "Save & enable": during first run the same button
  // is labelled for the thing it actually finishes, and the third option
  // ("Save disabled") stays, because writing the configuration and
  // starting to back up are two decisions.
  await page.getByRole("button", { name: "Finish setup" }).click();
  await page.getByRole("heading", { name: "Backup sets", level: 1 }).waitFor();
  await page.waitForTimeout(800);
  await take("18-configured-backup-sets", WINDOW);

  // -------------------------------------------------------------- sign in
  //
  // Last, not first. An operator meets this screen on the SECOND visit,
  // or on a first visit that did not go through the printed link. A
  // reload rebuilds the mock, so the app is unconfigured again and the
  // session stub decides what renders.
  session.authenticated = false;
  await page.goto(app.base + "/?scenario=first-run");
  await page.getByRole("heading", { name: "Sign in" }).waitFor();
  await take("19-sign-in", AUTH_CARD);

  console.log("\n" + shots.length + " screens written to docs/site/screens/");
  optimise(shots);
  const { count, bytes } = screensTotal();
  console.log("screens/ now holds " + count + " files, " + mb(bytes));
});
