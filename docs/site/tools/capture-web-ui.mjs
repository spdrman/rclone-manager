// Records the moving pictures on docs/site/web-ui.html.
//
// Run it from the repository root:
//
//     node docs/site/tools/capture-web-ui.mjs
//
// It writes GIFs into docs/site/screens/ and prints one line per clip with
// its frame count, duration and size, so a diff of the output tells you
// which picture moved and what it cost.
//
// harness.mjs holds the arrangement: where the app comes from, why it is
// never a real deployment, why the clock is pinned and why a clip is a
// list of held frames rather than a video. Read that first.
//
// # What is on this page and what is not
//
// Eight clips, and each one is a capability that shipped in 0.3.2 or
// 0.3.3 which a still cannot show. The terminal being pinned is a claim
// about what happens when you scroll. A filter is a claim about what
// disappears. A theme toggle is a claim about a whole window at once. A
// run control is a claim about what a button does and what it says it
// did.
//
// What is NOT here, and this one is a finding rather than a choice: a
// clip of a browser action arriving in the GLOBAL terminal. It cannot,
// because ActivityDock does not read state/browserNotices at all, only
// SetActivityPanel does. So a run started from a button lands in the
// banner and in that set's own panel, both of which are below, and never
// in the docked terminal, whose "This browser" chip therefore draws an
// empty log every time. The global terminal's own lines are the engine's,
// including the api_action lines that carry the equivalent command, and
// that is what the filter clip shows.
//
// # Why the window is 1440 wide
//
// Because at 1120 the terminal's filter row wraps to a second line and
// the 32px bar clips it, so half the chips are unreachable. That is a
// real defect and it is reported, but a documentation page should show
// the control working: a reader who cannot see the chip cannot learn what
// it does. With enough backup sets that row wraps at any width, so this
// is a wider window rather than a fix.

import { Clip, mb, openApp, screensTotal, settle, withDevServer } from "./harness.mjs";

/** A browser window, not a desktop. The GIF is written narrower than the
 *  window so a 2x capture downsamples into it, which is what keeps 13px
 *  UI text readable at documentation size. */
const WINDOW = { width: 1440, height: 860 };
const WIDTH = 1100;

const clips = [];

await withDevServer(async (app) => {
  // -------------------------------------------------- the terminal is pinned
  //
  // #617, and the one thing about it a still cannot say. Before this the
  // dock sat in flow at the end of the document, so on any page longer
  // than a screen the always-on panel had scrolled out of view and was
  // only there once you had already got to the bottom, which is the
  // opposite of what it is for.
  {
    const { page } = await openApp(app, { path: "/sets/production/postgres-primary", viewport: WINDOW });
    await settle(page, 1200);
    const clip = new Clip(page, "ui-terminal-pinned", { width: WIDTH });
    await clip.frame(1.6);
    for (const _ of [0, 1, 2, 3]) {
      await page.mouse.wheel(0, 560);
      await clip.frame(0.5);
    }
    await page.mouse.wheel(0, 1400);
    await clip.frame(2.2);
    await page.mouse.wheel(0, -5000);
    await clip.frame(1.6);
    clips.push(await clip.write());
  }

  // ------------------------------------------------------ filtering the log
  //
  // One deployment, several backup sets, one buffer. The chips answer
  // "which of these lines is about the thing I am looking at", and
  // "Commands only" is the one that turns the panel into a transcript of
  // what this browser asked the engine to do, with the command line each
  // request was equivalent to.
  {
    const { page } = await openApp(app, { path: "/settings", viewport: WINDOW });
    await settle(page, 1200);
    const clip = new Clip(page, "ui-terminal-filters", { clip: "section[aria-label='Terminal']", width: WIDTH });
    await clip.frame(2.0);
    await page.getByRole("button", { name: "production/billing-mysql" }).click();
    await clip.frame(2.0);
    await page.getByRole("button", { name: "Engine", exact: true }).click();
    await clip.frame(2.0);
    await page.getByRole("button", { name: "Commands only" }).click();
    await clip.frame(2.6);
    await page.getByRole("button", { name: "Everything" }).click();
    await clip.frame(1.4);
    clips.push(await clip.write());
  }

  // ----------------------------------------------------------- collapse it
  //
  // The bar it collapses to is not nothing: it keeps the newest line and
  // a count of what went wrong, because a terminal that collapses to
  // nothing teaches an operator to stop opening it. The state is this
  // browser's, in localStorage, and no navigation resets it.
  {
    const { page } = await openApp(app, { path: "/backups", viewport: WINDOW });
    await settle(page, 1200);
    const clip = new Clip(page, "ui-terminal-collapse", { width: WIDTH });
    await clip.frame(1.8);
    await page.getByRole("button", { name: "Hide terminal" }).click();
    await clip.frame(2.6);
    await page.getByRole("button", { name: "Show terminal" }).click();
    await clip.frame(1.8);
    clips.push(await clip.write());
  }

  // ------------------------------------------------------------- dark mode
  //
  // #618. The defect was not that dark mode looked bad, it was that every
  // native control kept the browser's own light rendering, so a select
  // and a number input drew black text on a dark field and the settings
  // form was the worst of it. The whole window is in frame here because
  // that is the only way to see a theme.
  {
    const { page } = await openApp(app, { path: "/settings", viewport: WINDOW });
    await settle(page, 1200);
    await page.mouse.wheel(0, 260);
    const clip = new Clip(page, "ui-dark-mode", { width: WIDTH });
    await clip.frame(2.2);
    await page.getByRole("button", { name: "Toggle colour theme" }).click();
    await clip.frame(3.0);
    await page.mouse.wheel(0, 460);
    await clip.frame(2.8);
    await page.getByRole("button", { name: "Toggle colour theme" }).click();
    await clip.frame(1.8);
    clips.push(await clip.write());
  }

  // -------------------------- run controls, the command, and closing the notice
  //
  // Four of 0.3.3's rules in one interaction. #597 made the run buttons
  // real and gave their answers somewhere to go. The standing parity rule
  // means the notice names the command the press was equivalent to,
  // copy-pasteable exactly as shown. #625 means it states an outcome
  // rather than leaving one to be inferred. #620 means the notice has a
  // way to close it. And the same line lands in the docked terminal, on
  // the "This browser" scope, which is the whole argument for the
  // terminal being there: the answer to what just happened is in one
  // place whatever page you are on.
  {
    const { page } = await openApp(app, { path: "/sets/production/postgres-primary", viewport: WINDOW });
    await settle(page, 1200);
    // Collapsed first, and not to tidy the frame. Leaving the terminal
    // open under this would put a panel in shot that does NOT receive the
    // line the banner is about, which is a picture that teaches the wrong
    // thing about where a run announces itself.
    await page.getByRole("button", { name: "Hide terminal" }).click();
    await settle(page, 400);
    const clip = new Clip(page, "ui-run-controls", { clip: "main", width: WIDTH, pad: 10 });
    await clip.frame(1.8);
    await page.getByRole("button", { name: "Run this backup set" }).hover();
    await clip.frame(1.6);
    await page.getByRole("button", { name: "Run this backup set" }).click();
    await page.getByText("Started a run of this backup set.").first().waitFor();
    await clip.frame(3.4);
    const close = page.getByRole("button", { name: /Dismiss this notice/ }).first();
    await close.hover();
    await clip.frame(1.0);
    await close.click();
    await clip.frame(2.0);
    clips.push(await clip.write());
  }

  // -------------------------------------------- one backup set's own activity
  //
  // #596. The dashboard answers "what is this deployment doing". This
  // answers "what is THIS set doing", with its own bar, its own step
  // sentence and its own log, on the page about that set. It is also the
  // surface where the equivalent command really does show up on its own:
  // press Run and the line this browser wrote lands in this set's log,
  // marked [browser] because "the engine said this" and "your browser
  // asked for this" are different facts.
  {
    const { page } = await openApp(app, { path: "/sets/production/postgres-primary", viewport: WINDOW });
    await settle(page, 1200);
    await page.getByRole("button", { name: "Hide terminal" }).click();
    const panel = page.locator("section[aria-label^=\'Activity for\']").first();
    await panel.scrollIntoViewIfNeeded();
    await settle(page, 600);
    const clip = new Clip(page, "ui-set-activity", { clip: "section[aria-label^=\'Activity for\']", width: WIDTH, pad: 14 });
    await clip.frame(3.0);
    await page.getByRole("button", { name: "Run this backup set" }).click();
    await page.getByText("[browser] Started a run of").waitFor();
    await settle(page, 400);
    await panel.scrollIntoViewIfNeeded();
    await clip.frame(3.6);
    await page.getByRole("button", { name: "Hide log" }).click();
    await clip.frame(2.0);
    await page.getByRole("button", { name: "Show log" }).click();
    await clip.frame(2.2);
    clips.push(await clip.write());
  }

  // --------------------------------------------- what retention would do
  //
  // A preview is a read. It lists every artifact the policy has an
  // opinion about, on both sides, with the reason beside each one, and
  // the reasons are the engine's own words rather than a re-derivation:
  // "protected as the newest known-good", "sibling-prefix directory found
  // at the computed path; refusing to delete". Nothing is deleted by
  // looking, and the destructive step behind it is a separate press with
  // a gate of its own.
  {
    const { page } = await openApp(app, { path: "/sets/production/postgres-primary", viewport: WINDOW });
    await settle(page, 1200);
    await page.getByRole("button", { name: "Hide terminal" }).click();
    const clip = new Clip(page, "ui-retention-preview", { width: WIDTH });
    await clip.frame(1.8);
    await page.getByRole("button", { name: "Preview retention", exact: true }).click();
    await page.getByText("DELETE", { exact: false }).first().waitFor();
    await settle(page, 800);
    await clip.frame(4.2);
    await page.mouse.move(720, 520);
    await page.mouse.wheel(0, 420);
    await clip.frame(3.4);
    clips.push(await clip.write());
  }

  // ------------------------------------------------- editing a backup set
  //
  // #591. Opening edit mode takes a hold on the set, which is a real
  // thing on the server with a command of its own, and leaving without
  // saving has to give that hold back. Before this there was a back link
  // and no way to discard what you had typed, so an operator who opened
  // the form to look at it left it held.
  {
    const { page } = await openApp(app, { path: "/sets/production/postgres-primary", viewport: WINDOW });
    await settle(page, 1200);
    await page.getByRole("button", { name: "Hide terminal" }).click();
    const clip = new Clip(page, "ui-edit-mode", { width: WIDTH });
    await clip.frame(1.8);
    await page.getByRole("button", { name: "Edit", exact: true }).click();
    await settle(page, 900);
    await clip.frame(3.4);
    await page.getByRole("button", { name: /CANCEL & EXIT EDIT MODE/i }).hover();
    await clip.frame(1.4);
    await page.getByRole("button", { name: /CANCEL & EXIT EDIT MODE/i }).click();
    await settle(page, 900);
    await clip.frame(2.4);
    clips.push(await clip.write());
  }
});

const total = clips.reduce((n, c) => n + c.bytes, 0);
console.log("\n" + clips.length + " clips, " + mb(total));
const { count, bytes } = screensTotal();
console.log("screens/ now holds " + count + " files, " + mb(bytes));
