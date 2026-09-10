// Records the moving pictures on docs/site/ssh.html.
//
// Run it from the repository root:
//
//     node docs/site/tools/capture-ssh.mjs
//
// harness.mjs holds the arrangement: where the app comes from, why it is
// never a real deployment, why the clock and timezone are pinned, and why
// a clip is a list of held frames rather than a video.
//
// # The two clips, and what they are each about
//
// #592 replaced two write-only text boxes with a wizard. The boxes asked
// an operator to paste "the id of an imported key", and a key id is a
// uuid the product showed exactly once, in the response to the import
// that created it. The wizard's first step is the answer to that: it
// lists what this deployment already holds and what it can find on the
// machine, and on a default install the key the installer generated is
// already sitting there waiting to be picked.
//
// #624 is the other half. Trusting a host key settles which machine
// answered and settles nothing about whether the key authenticates or
// whether the account can read the folder, so a wizard that stopped at
// the host key was letting a set be saved on a connection nobody had
// proven. The second clip is the check that closes that, on the step
// where it now blocks the save.
//
// # No key material is ever in frame
//
// The paste box is photographed empty and the candidate that gets picked
// is one the fixture puts on the machine, selected by its path. Selecting
// a candidate imports it server side, so key material never crosses the
// network at all, and there is none here to cross it.

import { Clip, EXAMPLE, mb, openApp, screensTotal, settle, VIEWPORT, withDevServer } from "./harness.mjs";

/** The standard viewport rather than a copy of its numbers, so this
 *  cannot drift away from it the way a literal would. */
const WINDOW = VIEWPORT;
const WIDTH = 1040;

const clips = [];

await withDevServer(async (app) => {
  // ------------------------------------- the wizard, all four of its steps
  {
    const { page } = await openApp(app, { path: "/sets/production/postgres-primary", viewport: WINDOW });
    await settle(page, 1200);
    await page.getByRole("button", { name: "Change SSH authentication" }).click();
    await page.getByRole("dialog", { name: "SSH authentication" }).waitFor();
    await settle(page, 600);

    const clip = new Clip(page, "ssh-key-wizard", { width: WIDTH });
    // Step 1, top: the keys this deployment already holds, each with its
    // fingerprint and what is currently using it. Nothing here asks
    // anybody to remember an id.
    await clip.frame(3.0);

    // Down to the half that is the point of the issue: the keys found on
    // the machine, with the locations that were actually searched and the
    // ones that are not present said out loud.
    await page.mouse.move(640, 500);
    await page.mouse.wheel(0, 620);
    await clip.frame(3.0);

    await page.getByRole("button", { name: /id_ed25519/ }).click();
    await settle(page, 400);
    await clip.frame(2.4);

    // Step 2. The probe, and the comparison against what this set's own
    // known_hosts already pins. The only way past a mismatch is an
    // acknowledgement scoped to the fingerprint that was shown.
    await page.getByRole("button", { name: "Next: server identity" }).click();
    await settle(page, 500);
    await clip.frame(2.2);
    await page.getByRole("button", { name: "Ask the server" }).click();
    await settle(page, 900);
    await clip.frame(3.2);

    // Step 3, and the reason there are four results rather than one.
    // Failing to authenticate with the host key green means the public
    // half is not in the remote authorized_keys. Failing to LIST with
    // authentication green is a permissions problem on the source and
    // nothing to do with keys. One red sentence makes those two
    // indistinguishable, which is how somebody ends up regenerating a
    // keypair to fix a chmod.
    await page.getByRole("button", { name: "Next: verify" }).click();
    await settle(page, 500);
    await clip.frame(2.0);
    await page.getByRole("button", { name: /^Verify/ }).click();
    await settle(page, 1400);
    await clip.frame(4.0);
    clips.push(await clip.write());
  }

  // ------------------------- a source is proven before a set relies on it
  //
  // #624, on the step where it bites. Before the check runs the panel
  // says nothing has been proven and all three save buttons are off;
  // after it comes back clean the same panel says the source has been
  // proven and saving is enabled.
  {
    const { page } = await openApp(app, { path: "/sets/new", viewport: WINDOW });
    await settle(page, 1000);
    await page.getByRole("button", { name: "Hide terminal" }).click();

    const fill = async (label, value) => {
      const field = page.getByLabel(new RegExp("^" + label));
      await field.fill(value);
      await field.blur();
    };
    await fill("Backup set name", EXAMPLE.setName);
    await fill("Server hostname", EXAMPLE.host);
    await fill("SSH port", EXAMPLE.port);
    await fill("Username", EXAMPLE.user);

    await page.getByRole("button", { name: "Authentication" }).click();
    await page.getByRole("radio", { name: /Import key/ }).check();
    await page.getByLabel(/Private key/).fill(EXAMPLE.fakeKey);
    await page.getByRole("button", { name: "Import key" }).click();
    await page.getByText("Key imported").waitFor();

    await page.getByRole("button", { name: "Verify server" }).click();
    await page.getByRole("button", { name: "Trust host" }).click();
    await page.getByRole("button", { name: "Host trusted" }).waitFor();

    await page.getByRole("button", { name: "Discovery" }).click();
    await fill("Remote folder", EXAMPLE.remoteFolder);
    await fill("Include patterns", EXAMPLE.include);

    await page.getByRole("button", { name: "Storage & validation" }).click();
    await fill("NAS destination", EXAMPLE.destination);

    await page.getByRole("button", { name: "Review" }).click();
    await page.getByRole("heading", { name: "Review", level: 2 }).waitFor();
    await page.getByRole("button", { name: "Test connection" }).scrollIntoViewIfNeeded();
    await settle(page, 700);

    const clip = new Clip(page, "ssh-source-proof", { width: WIDTH });
    await clip.frame(3.4);
    await page.getByRole("button", { name: "Test connection" }).hover();
    await clip.frame(1.2);
    await page.getByRole("button", { name: "Test connection" }).click();
    await page.getByText("This source has been proven").waitFor();
    await settle(page, 600);
    await clip.frame(4.4);
    clips.push(await clip.write());
  }
});

const total = clips.reduce((n, c) => n + c.bytes, 0);
console.log("\n" + clips.length + " clips, " + mb(total));
const { count, bytes } = screensTotal();
console.log("screens/ now holds " + count + " files, " + mb(bytes));
