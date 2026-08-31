import { expect, test } from "./fixtures";

test.describe("add backup set wizard", () => {
  test.beforeEach(async ({ bm, page }) => {
    await bm.goto("/sets");
    await page.getByRole("button", { name: "Add backup set" }).click();
    await expect(bm.heading("Add backup set")).toBeVisible();
  });

  test("groups the flow into exactly six steps", async ({ page }) => {
    const steps = page.getByRole("listitem");
    await expect(steps).toHaveCount(6);
    for (const label of ["Source", "Authentication", "Verify server", "Discovery", "Storage & retention", "Review"]) {
      await expect(page.getByRole("button", { name: label })).toBeVisible();
    }
  });

  test("marks the current step and advances with Continue", async ({ page }) => {
    await expect(page.getByRole("button", { name: "Source" })).toHaveAttribute("aria-current", "step");
    await expect(page.getByText("Step 1 of 6")).toBeVisible();
    await page.getByRole("button", { name: "Continue" }).click();
    await expect(page.getByText("Step 2 of 6")).toBeVisible();
    // exact: true — "← Cancel and return to backup sets" also contains
    // "back" (as a substring of "backup"), so a non-exact match is ambiguous
    // once that cancel link and the step-nav "Back" button are both present.
    await page.getByRole("button", { name: "Back", exact: true }).click();
    await expect(page.getByText("Step 1 of 6")).toBeVisible();
  });

  test("Back is disabled on the first step and Continue on the last", async ({ page }) => {
    // exact: true — see the comment in the previous test.
    await expect(page.getByRole("button", { name: "Back", exact: true })).toBeDisabled();
    await page.getByRole("button", { name: "Review" }).click();
    await expect(page.getByRole("button", { name: "Continue" })).toBeDisabled();
  });

  test("step 1 collects source connection details", async ({ page }) => {
    for (const label of ["Backup set name", "Server hostname", "SSH port", "Username"]) {
      await expect(page.getByText(label, { exact: true })).toBeVisible();
    }
  });

  test("step 2 offers three key sources and shows only the public key", async ({ page }) => {
    await page.getByRole("button", { name: "Authentication" }).click();
    await expect(page.getByRole("radio")).toHaveCount(3);
    await expect(page.getByText("Generate dedicated SSH key")).toBeVisible();
    await expect(page.getByText(/never displayed/)).toBeVisible();
    await expect(page.getByText(/^ssh-ed25519 AAAA/)).toBeVisible();
    // The step's own copy legitimately says "Private keys stay on this NAS
    // and are never shown" — a bare /PRIVATE KEY/i match flags that
    // sentence as a false positive. Match the actual PEM marker instead
    // (same pattern safety-invariants.spec.ts uses for a real key leak).
    expect(await page.locator("body").innerText()).not.toMatch(/BEGIN (OPENSSH|RSA|EC) PRIVATE KEY/i);
  });

  test("step 2 copy button is present for the public key", async ({ page }) => {
    await page.getByRole("button", { name: "Authentication" }).click();
    await expect(page.getByRole("button", { name: "Copy public key" })).toBeEnabled();
  });

  test("step 3 requires an explicit trust decision", async ({ page }) => {
    await page.getByRole("button", { name: "Verify server" }).click();
    await expect(page.getByText("Algorithm")).toBeVisible();
    await expect(page.getByText(/SHA256:/)).toBeVisible();
    await expect(page.getByText("Not yet trusted")).toBeVisible();

    const trust = page.getByRole("button", { name: "Trust host" });
    await expect(trust).toBeEnabled();
    await trust.click();
    await expect(page.getByRole("button", { name: "Host trusted" })).toBeDisabled();
  });

  test("step 3 warns what a future host-key change will do", async ({ page }) => {
    await page.getByRole("button", { name: "Verify server" }).click();
    await expect(page.getByText(/stops all backup operations for the set/)).toBeVisible();
    await expect(page.getByText(/blocks remote artifact deletion/)).toBeVisible();
  });

  test("step 4 separates recommended from advanced completion methods", async ({ page }) => {
    await page.getByRole("button", { name: "Discovery" }).click();
    await expect(page.getByText("Recommended")).toBeVisible();
    await expect(page.getByText("Advanced")).toBeVisible();
    await expect(page.getByRole("radio", { name: /Atomic rename/ })).toBeVisible();
    await expect(page.getByRole("radio", { name: /Completion marker/ })).toBeChecked();
  });

  test("step 4 warns when completion is inferred", async ({ page }) => {
    await page.getByRole("button", { name: "Discovery" }).click();
    await expect(page.getByText(/infers completion/)).toHaveCount(0);

    await page.getByRole("radio", { name: /Stable file size/ }).click();
    await expect(
      page.getByText(/infers completion and provides less assurance than a producer-provided completion marker/)
    ).toBeVisible();
  });

  test("step 5 defaults retention and protects the known-good backup", async ({ page }) => {
    await page.getByRole("button", { name: "Storage & retention" }).click();
    // getByDisplayValue is a Testing Library method, not one Playwright's
    // Locator API exposes — these are plain <input defaultValue=…> fields
    // (BackupSetWizardPage's Field()), matched by their <label> text instead.
    await expect(page.getByLabel("Daily")).toHaveValue("7 days");
    await expect(page.getByLabel("Weekly")).toHaveValue("3 months");
    await expect(page.getByLabel("Monthly")).toHaveValue("12 months");
    await expect(page.getByRole("checkbox", { name: /Protect newest known-good/ })).toBeChecked();
  });

  test("step 5 lists the three validation layers", async ({ page }) => {
    await page.getByRole("button", { name: "Storage & retention" }).click();
    await expect(page.getByRole("checkbox", { name: /Transfer verification/ })).toBeChecked();
    await expect(page.getByRole("checkbox", { name: /Checksum verification/ })).toBeChecked();

    // The third layer stopped being a checkbox in #164, which replaced the
    // decorative "Application validation" toggle with a real picklist over
    // the backend's registered catalog (GET /api/v1/validators). It is a
    // combobox now, so the old getByRole("checkbox") matched nothing and
    // this assertion could only ever time out.
    const validator = page.getByRole("combobox", { name: /Application validation/ });
    // Enabled is the real signal that the catalog fetch settled:
    // BackupSetWizardPage disables the picklist while validatorCatalog is
    // null and leaves it disabled if the fetch failed. Waiting on that
    // instead of a sleep also means a catalog that never loads fails here
    // rather than passing on an empty list.
    await expect(validator).toBeEnabled();
    // Off by default, same contract the removed toggle carried: transfer and
    // checksum verification are on, application validation is opt-in.
    await expect(validator).toHaveValue("");
    const options = validator.getByRole("option");
    await expect(options.first()).toHaveText("None (transfer and checksum verification only)");
    // ...and the choices below it come from the backend's catalog rather than
    // a hardcoded list. Counted, not enumerated, so adding a validator to the
    // mock fixture does not turn this red. Safe to read without retrying:
    // toBeEnabled() above already proved the fetch settled.
    expect(await options.count()).toBeGreaterThan(1);
  });

  test("step 6 summarises source, destination, retention and validation", async ({ page }) => {
    await page.getByRole("button", { name: "Review" }).click();
    for (const label of ["Source", "Destination", "Retention", "Validation"]) {
      await expect(page.getByText(label, { exact: true }).first()).toBeVisible();
    }
  });

  test("step 6 discloses remote-source handling in full", async ({ page }) => {
    await page.getByRole("button", { name: "Review" }).click();
    await expect(page.getByText("Remote source handling")).toBeVisible();
    await expect(
      page.getByText(/transferred, verified, durably committed to this NAS, and recorded as safe/)
    ).toBeVisible();
    await expect(page.getByText("Remote artifact deleted")).toBeVisible();
  });

  test("saving is blocked until the acknowledgement is checked", async ({ page }) => {
    // Every OTHER save precondition is satisfied first — import a key on
    // step 2 and trust the host on step 3 — so this isolates the
    // acknowledgement checkbox as the one variable under test. Since M7
    // (#146 review) those two also gate the Save buttons, so reaching
    // Review cold would leave Save disabled for reasons this test isn't
    // about (mirrors wizard.test.tsx's advanceToReviewReady).
    await page.getByRole("button", { name: "Authentication" }).click();
    await page.getByRole("radio", { name: /Import key/ }).click();
    // Synthetic fixture only — never a real key.
    await page.getByLabel(/private key/i).fill("FAKE-TEST-KEY-MATERIAL-not-a-real-key-0123456789");
    await page.getByRole("button", { name: "Import key" }).click();
    await expect(page.getByText(/key imported/i)).toBeVisible();

    await page.getByRole("button", { name: "Verify server" }).click();
    const trust = page.getByRole("button", { name: "Trust host" });
    await expect(trust).toBeEnabled();
    await trust.click();
    await expect(page.getByRole("button", { name: "Host trusted" })).toBeDisabled();

    await page.getByRole("button", { name: "Review" }).click();

    const runNow = page.getByRole("button", { name: /Save, enable & run/ });
    const enable = page.getByRole("button", { name: /^Save & enable$/ });
    await expect(runNow).toBeDisabled();
    await expect(enable).toBeDisabled();
    await expect(page.getByText(/Acknowledge remote-source handling/)).toBeVisible();

    await page.getByRole("checkbox", { name: /remote backup will be removed only after/i }).check();
    await expect(runNow).toBeEnabled();
    await expect(enable).toBeEnabled();

    // Unchecking must re-arm the guard, not leave it latched open.
    await page.getByRole("checkbox", { name: /remote backup will be removed only after/i }).uncheck();
    await expect(runNow).toBeDisabled();
  });

  test("offers a save-disabled escape hatch that needs no acknowledgement", async ({ page }) => {
    await page.getByRole("button", { name: "Review" }).click();
    await expect(page.getByRole("button", { name: "Save disabled" })).toBeEnabled();
  });

  test("cancel returns to the sets list", async ({ bm, page }) => {
    await page.getByRole("button", { name: /Cancel and return to backup sets/ }).click();
    await expect(bm.heading("Backup sets")).toBeVisible();
  });

  test("a hostname typed in step 1 shows up in the review step, not the built-in example", async ({ page }) => {
    const host = page.getByLabel("Server hostname");
    await host.fill("warehouse-nas.internal");

    await page.getByRole("button", { name: "Review" }).click();
    await expect(page.getByText("warehouse-nas.internal")).toBeVisible();
    await expect(page.getByText("prod-db-01.internal")).toHaveCount(0);
  });

  test("importing a key never leaves the pasted material on screen", async ({ page }) => {
    await page.getByRole("button", { name: "Authentication" }).click();
    await page.getByRole("radio", { name: /Import key/ }).click();

    // Synthetic fixture only — never a real key.
    const fixtureKey = "FAKE-TEST-KEY-MATERIAL-not-a-real-key-0123456789";
    const textarea = page.getByLabel(/private key/i);
    await textarea.fill(fixtureKey);

    const importBtn = page.getByRole("button", { name: "Import key" });
    await expect(importBtn).toBeEnabled();
    await importBtn.click();

    await expect(page.getByText(/key imported/i)).toBeVisible();
    await expect(page.getByLabel(/private key/i)).toHaveCount(0);
    expect(await page.locator("body").innerText()).not.toContain(fixtureKey);
  });

  // Issue #146 (B2.7): the wizard's Save buttons previously had no
  // onClick at all. This drives the full wizard-to-save flow through the
  // real running app (against the mock API this whole suite runs
  // against — see playwright.config.ts's own comment on why) and
  // confirms a backup set actually exists afterward, visible on the
  // sets list, not just a success toast.
  test("completing the wizard and clicking Save & enable persists a new backup set, visible on the sets list", async ({ page, bm }) => {
    const uniqueName = "E2E Wizard Set " + Date.now();
    await page.getByLabel("Backup set name").fill(uniqueName);

    await page.getByRole("button", { name: "Authentication" }).click();
    await page.getByRole("radio", { name: /Import key/ }).click();
    await page.getByLabel(/private key/i).fill("FAKE-TEST-KEY-MATERIAL-not-a-real-key-0123456789");
    await page.getByRole("button", { name: "Import key" }).click();
    await expect(page.getByText(/key imported/i)).toBeVisible();

    await page.getByRole("button", { name: "Verify server" }).click();
    const trust = page.getByRole("button", { name: "Trust host" });
    await expect(trust).toBeEnabled();
    await trust.click();
    await expect(page.getByRole("button", { name: "Host trusted" })).toBeDisabled();

    await page.getByRole("button", { name: "Review" }).click();
    await page.getByRole("checkbox", { name: /remote backup will be removed only after/i }).check();

    await page.getByRole("button", { name: /^Save & enable$/ }).click();

    // Saving navigates back to the sets list (BackupSetWizardPage's
    // handleSave), and the newly created set is there without a manual
    // page reload — the shared setsNode refresh (appNodes.ts/
    // resource.ts's fetchResource) this wizard triggers on success.
    await expect(bm.heading("Backup sets")).toBeVisible();
    await expect(page.getByText(uniqueName)).toBeVisible();
  });

  test("Save stays disabled without an imported key or a trusted host, even after acknowledging", async ({ page }) => {
    // Before M7 (#146 review) this scenario was reachable by clicking
    // Save: the button stayed enabled with no key imported and no host
    // trusted, and handleSave's own ad hoc guard rejected the request
    // after the click, surfacing an inline error. Save is now
    // structurally disabled for this same combination instead — proven
    // here by the buttons refusing to be clicked at all, mirroring
    // wizard.test.tsx's "keeps Save disabled without an imported key or
    // a trusted host" unit test.
    await page.getByRole("button", { name: "Review" }).click();
    await page.getByRole("checkbox", { name: /remote backup will be removed only after/i }).check();

    await expect(page.getByRole("button", { name: /Save, enable & run/ })).toBeDisabled();
    await expect(page.getByRole("button", { name: /^Save & enable$/ })).toBeDisabled();
    await expect(page.getByRole("heading", { level: 1, name: "Add backup set" })).toBeVisible();
  });
});
