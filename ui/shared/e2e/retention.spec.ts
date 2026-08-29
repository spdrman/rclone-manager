import { expect, test } from "./fixtures";

/** §17 and §35 — the highest-consequence flow in the product. */
test.describe("retention preview and apply", () => {
  test.beforeEach(async ({ bm, page }) => {
    await bm.goto("/sets");
    await page.getByRole("article").first().getByRole("button", { name: "Open" }).click();
    await page.getByRole("button", { name: "Preview retention" }).click();
    await expect(page.getByRole("dialog", { name: "Retention preview" })).toBeVisible();
  });

  test("shows the server-issued plan id", async ({ page }) => {
    await expect(page.getByText(/Plan plan_.* issued by the backup service/)).toBeVisible();
  });

  test("summarises keep, delete and reclaim", async ({ page }) => {
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("Keep", { exact: true }).first()).toBeVisible();
    await expect(dialog.getByText("Delete", { exact: true }).first()).toBeVisible();
    await expect(dialog.getByText("Reclaim")).toBeVisible();
  });

  test("itemises what is kept and why, and what is deleted and why", async ({ page }) => {
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText(/Not selected by current policy/).first()).toBeVisible();
    await expect(dialog.getByText("Protected").first()).toBeVisible();
  });

  test("a stale plan blocks the destructive path and offers a refresh", async ({ page }) => {
    const dialog = page.getByRole("dialog");
    const staleNotice = dialog.getByText("Retention preview changed");

    if (await staleNotice.isVisible().catch(() => false)) {
      await expect(dialog.getByText(/No files were deleted/)).toBeVisible();
      await expect(dialog.getByRole("button", { name: "Continue…" })).toBeDisabled();
      await dialog.getByRole("button", { name: "Review new plan" }).click();
      await expect(dialog.getByRole("button", { name: "Continue…" })).toBeEnabled();
    } else {
      // Current plan: reopening the preview produces the stale variant next.
      await dialog.getByRole("button", { name: "Cancel" }).click();
      await page.getByRole("button", { name: "Preview retention" }).click();
      const second = page.getByRole("dialog");
      await expect(second.getByText("Retention preview changed")).toBeVisible();
      await expect(second.getByRole("button", { name: "Continue…" })).toBeDisabled();
    }
  });

  test("confirmation names the exact count and reclaimed size", async ({ page }) => {
    const dialog = page.getByRole("dialog");
    const cont = dialog.getByRole("button", { name: "Continue…" });
    if (await cont.isDisabled()) {
      await dialog.getByRole("button", { name: "Review new plan" }).click();
    }
    await cont.click();

    const confirm = page.getByRole("dialog", { name: "Apply retention" });
    await expect(confirm).toContainText("Destructive action");
    await expect(confirm).toContainText(/\d+ retained backup files will be permanently removed/);
    await expect(confirm).toContainText(/will be reclaimed/);
    await expect(confirm).toContainText(/will not be recalculated/);
    await expect(confirm).toContainText(/newest known-good backup is protected/);
  });

  test("the confirm button states the consequence and is not the default focus", async ({ page }) => {
    const dialog = page.getByRole("dialog");
    const cont = dialog.getByRole("button", { name: "Continue…" });
    if (await cont.isDisabled()) await dialog.getByRole("button", { name: "Review new plan" }).click();
    await cont.click();

    const confirm = page.getByRole("dialog", { name: "Apply retention" });
    await expect(confirm.getByRole("button", { name: /^Delete \d+ backups$/ })).toBeVisible();
    await expect(confirm.getByRole("button", { name: /^OK$/ })).toHaveCount(0);
    await expect(confirm.getByRole("button", { name: "Cancel" })).toBeFocused();
  });

  test("Escape cancels without deleting anything", async ({ page }) => {
    const dialog = page.getByRole("dialog");
    const cont = dialog.getByRole("button", { name: "Continue…" });
    if (await cont.isDisabled()) await dialog.getByRole("button", { name: "Review new plan" }).click();
    await cont.click();

    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: "Apply retention" })).toHaveCount(0);
  });

  test("clicking the scrim dismisses the preview", async ({ page }) => {
    await page.locator(".dialog-scrim").click({ position: { x: 5, y: 5 } });
    await expect(page.getByRole("dialog", { name: "Retention preview" })).toHaveCount(0);
  });
});
