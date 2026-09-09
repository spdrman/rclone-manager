// Records the moving pictures on docs/site/storage.html.
//
// Run it from the repository root:
//
//     node docs/site/tools/capture-storage.mjs
//
// harness.mjs holds the arrangement: where the app comes from, why it is
// never a real deployment, why the clock and timezone are pinned, and why
// a clip is a list of held frames rather than a video.
//
// # What these three clips are about
//
// #622 and #636, which between them are most of what 0.3.3 added to
// storage. Before them the drive on this machine was not a destination
// you could name, a retention tier had no way to say where its copies
// go, and a destination could be written into the configuration without
// anything having been proven about it.
//
// The terminal is collapsed in all of them, on purpose. What these clips
// are about is the command line printed UNDER each control, which the
// form recomputes as you edit it, and a 240px log panel in shot competes
// with the thing being shown for no gain.

import { Clip, EXAMPLE, mb, openApp, screensTotal, settle, typeInto, VIEWPORT, withDevServer } from "./harness.mjs";

/** The standard viewport, not a window of this file's own. An earlier
 *  version of this used 1440 to get around the terminal's filter chips
 *  clipping out of their bar, which #650 fixed; a capture script picking
 *  its own window size for a reason that has gone is a picture that no
 *  longer frames what the browser suite frames. */
const WINDOW = VIEWPORT;
const WIDTH = 1100;

/** The card, by its own heading rather than by position, so a reordering
 *  of the settings page moves these clips instead of breaking them. */
const DESTINATIONS = 'section.card:has(h2:text-is("Storage destinations"))';
const RETENTION = 'section.card:has(h2:text-is("Retention policy"))';
const WIZARD = '[aria-label="Add a destination"]';

const clips = [];

async function fresh(app, path) {
  const { page } = await openApp(app, { path, viewport: WINDOW });
  await settle(page, 1200);
  await page.getByRole("button", { name: "Hide terminal" }).click();
  await settle(page, 300);
  return page;
}

await withDevServer(async (app) => {
  // ------------------------------------------- the drive on this machine
  //
  // #622's first half. Settings used to list only the destinations the
  // configuration declared, so the place every backup actually lands was
  // the one place that was never named, and an operator reading that list
  // could reasonably conclude their backups were going nowhere. It is
  // first in the list now, carries the default mark, says what the mark
  // does and does not mean, and answers a test connection like any other
  // destination, in the same fixed order of named steps.
  //
  // One thing in this clip is the fixture rather than the product: the
  // mock answers the local drive's preflight with the bucket-shaped
  // report it gives an S3 destination, so the detail column says the
  // endpoint "holds bucket" and names an empty one. The engine's own
  // local report is a different set of steps. Nothing on the page claims
  // otherwise, and it is reported.
  {
    const page = await fresh(app, "/settings");
    const card = page.locator(DESTINATIONS);
    await card.scrollIntoViewIfNeeded();
    await settle(page, 400);
    const clip = new Clip(page, "storage-local-drive", { width: WIDTH });
    await clip.frame(2.6);
    await card.getByRole("button", { name: "Test connection" }).first().hover();
    await clip.frame(1.2);
    await card.getByRole("button", { name: "Test connection" }).first().click();
    await card.getByText("credentials").first().waitFor();
    await settle(page, 500);
    await clip.frame(4.0);
    clips.push(await clip.write());
  }

  // ------------------------------------------------ declaring a new one
  //
  // Four steps, and the fourth is the one that matters: #636 means the
  // wizard will not write a destination into the configuration that has
  // not answered. The report is a fixed order of named steps rather than
  // one verdict, because knowing that the write failed after credentials
  // and reach both passed is most of the diagnosis.
  //
  // Nothing here is a real credential. The access key is a placeholder
  // and the secret is typed into a password field, and both go to an
  // in-memory object rather than over a network.
  {
    const page = await fresh(app, "/settings");
    const card = page.locator(DESTINATIONS);
    await card.scrollIntoViewIfNeeded();
    await card.getByRole("button", { name: "Add a destination" }).click();
    const wizard = page.locator(WIZARD);
    await wizard.scrollIntoViewIfNeeded();
    await settle(page, 500);

    // The whole window rather than the wizard panel, because this panel
    // grows by several hundred pixels when the check list lands in it and
    // a GIF cannot change size halfway through. The window is a fixed
    // frame that the growing panel happens inside, which is also what an
    // operator sees.
    const clip = new Clip(page, "storage-add-destination", { width: WIDTH });
    await clip.frame(2.2);
    await page.getByRole("button", { name: "Backblaze B2" }).click();
    await clip.frame(1.6);

    await typeInto(clip, page.getByLabel("Destination id"), EXAMPLE.s3.name, { chunks: 2 });
    await typeInto(clip, page.getByLabel("Bucket"), EXAMPLE.s3.bucket, { chunks: 2 });
    await clip.frame(1.8);

    await page.getByRole("button", { name: "Next: credentials" }).click();
    await settle(page, 400);
    // Step 2 offers four ways to give the destination a credential and
    // only the first of them puts a secret on this page. Whichever is
    // picked, what reaches config.yaml is a reference: a key id, a path, a
    // variable name or a command. There is no field in that file a secret
    // fits in.
    await clip.frame(2.6);
    await page.getByRole("radio", { name: /Paste a key and let me store it/ }).check();
    await clip.frame(1.4);
    await typeInto(clip, page.getByLabel("Access key id"), EXAMPLE.s3.accessKey, { chunks: 1 });
    // A password field, so what is photographed is a row of dots. The
    // value is a placeholder either way and the API it reaches is an
    // in-memory object.
    await typeInto(clip, page.getByRole("textbox", { name: "Secret access key" }), EXAMPLE.s3.secretKey, { chunks: 1 });
    await clip.frame(1.6);

    await page.getByRole("button", { name: "Next: test connection" }).click();
    // The report, not the spinner: "Next: save" is enabled by the engine's
    // own `ok`, so waiting for it to become clickable is waiting for the
    // thing the step is about. #636 is exactly this gate.
    await page.getByRole("button", { name: "Next: save" }).waitFor({ state: "visible" });
    await page.waitForFunction(() => {
      const b = [...document.querySelectorAll("button")].find((e) => e.textContent.trim() === "Next: save");
      return b && !b.disabled;
    });
    await wizard.scrollIntoViewIfNeeded();
    await settle(page, 700);
    await clip.frame(4.4);
    clips.push(await clip.write());
  }

  // ----------------------------------------- a tier picks where copies go
  //
  // #622's second half, and the reason it is a separate decision from
  // declaring the destination: declaring one moves nothing, and a tier
  // naming one is what actually sends copies off this machine. The
  // command under the picker is recomputed as the value changes, which is
  // the parity rule working live rather than as a caption.
  {
    const page = await fresh(app, "/settings");
    const card = page.locator(RETENTION);
    await card.scrollIntoViewIfNeeded();
    await settle(page, 400);
    const clip = new Clip(page, "storage-tier-destination", { width: WIDTH });
    await clip.frame(2.6);
    const picker = page.getByLabel("Storage medium for tier 1");
    await picker.scrollIntoViewIfNeeded();
    await settle(page, 300);
    await clip.frame(1.4);
    await picker.selectOption("offsite_s3");
    await settle(page, 400);
    await clip.frame(4.0);
    await picker.selectOption("local");
    await settle(page, 400);
    await clip.frame(2.4);
    clips.push(await clip.write());
  }
});

const total = clips.reduce((n, c) => n + c.bytes, 0);
console.log("\n" + clips.length + " clips, " + mb(total));
const { count, bytes } = screensTotal();
console.log("screens/ now holds " + count + " files, " + mb(bytes));
