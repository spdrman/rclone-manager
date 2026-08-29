import { expect, test } from "./fixtures";

test.describe("settings", () => {
  test.beforeEach(async ({ bm }) => {
    await bm.goto("/settings");
  });

  test("exposes the five documented service settings", async ({ page }) => {
    for (const label of [
      "Polling interval", "Log level",
      "Storage warning threshold", "Storage critical threshold",
      "Default retention for new sets"
    ]) {
      await expect(page.getByText(label, { exact: true })).toBeVisible();
    }
  });

  test("makes the platform abstraction explicit", async ({ page }) => {
    for (const label of ["Platform", "Integration", "Authentication", "Deployment", "Storage mount"]) {
      await expect(page.getByText(label, { exact: true }).first()).toBeVisible();
    }
  });

  test("lists capabilities with an honest supported/unsupported marker", async ({ page }) => {
    for (const cap of [
      "Native authentication", "Native notifications", "Storage picker",
      "Embedded window", "App-store packaging"
    ]) {
      await expect(page.getByText(cap, { exact: true })).toBeVisible();
    }
  });

  test("does not present a missing capability as available", async ({ page }) => {
    // Generic has no native notifications, so the copy must say so.
    await expect(page.getByText(/Native NAS notifications are not available/)).toBeVisible();
    await expect(page.getByText(/webhook notifications are enabled/i)).toBeVisible();
  });

  test("reports full build provenance", async ({ page }) => {
    for (const label of [
      "Backup Manager version", "Service version", "Core version",
      "Embedded rclone", "Database schema", "Platform adapter",
      "Architecture", "Build commit"
    ]) {
      await expect(page.getByText(label, { exact: true })).toBeVisible();
    }
  });
});

test.describe("version mismatch", () => {
  test("disables management but keeps information visible", async ({ bm, page }) => {
    await bm.goto("/", "version-mismatch");

    await expect(page.getByText("Backup Manager update required")).toBeVisible();
    await expect(page.getByText(/Management actions have been disabled/)).toBeVisible();
    await expect(page.getByText(/UI 1\.3\.0/)).toBeVisible();
    await expect(page.getByText(/Service 1\.2\.0/)).toBeVisible();

    // Read-only information still renders.
    await expect(page.getByText(/^BACKUPS /)).toBeVisible();

    // Every mutating control is disabled.
    await expect(page.getByRole("button", { name: "Add backup set" })).toBeDisabled();
    await expect(page.getByRole("button", { name: "Run all due sets" })).toBeDisabled();
  });

  test("disables destructive controls on set detail", async ({ bm, page }) => {
    await bm.goto("/sets", "version-mismatch");
    await page.getByRole("article").first().getByRole("button", { name: "Open" }).click();

    await expect(page.getByRole("button", { name: /Apply retention now/ })).toBeDisabled();
    await expect(page.getByRole("button", { name: /Remove set configuration/ })).toBeDisabled();
    await expect(page.getByRole("button", { name: "Run now" })).toBeDisabled();
  });
});

test.describe("catalog recovery", () => {
  test("scan then rebuild, with no destructive step", async ({ bm, page }) => {
    await bm.goto("/settings", "catalog-recovery");

    await page.getByRole("button", { name: "Scan backup storage" }).click();
    await expect(bm.heading("Catalog recovery")).toBeVisible();
    await expect(page.getByText(/No files will be deleted, moved, or modified/)).toBeVisible();

    await page.getByRole("button", { name: "Scan backup storage" }).click();
    await expect(page.getByText("Catalog rebuild preview")).toBeVisible();
    await expect(page.getByText("Backup artifacts discovered")).toBeVisible();
    await expect(page.getByText("Require review")).toBeVisible();
    await expect(page.getByText(/placed in Quarantine, not deleted/)).toBeVisible();

    await page.getByRole("button", { name: "Rebuild catalog" }).click();
    const dialog = page.getByRole("dialog", { name: "Rebuild catalog" });
    await expect(dialog).toContainText(/This operation is additive/);
    await expect(dialog).toContainText(/No backup files are deleted/);
    await expect(dialog.getByRole("button", { name: /^Rebuild from \d+ artifacts$/ })).toBeVisible();

    await dialog.getByRole("button", { name: /Rebuild from/ }).click();
    await expect(bm.heading("Backups")).toBeVisible();
  });
});
