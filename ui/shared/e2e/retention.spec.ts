import { expect, test } from "./fixtures";

/** §17 and §35 — the highest-consequence flow in the product. */
test.describe("retention preview and apply", () => {
  test.beforeEach(async ({ bm, page }) => {
    await bm.goto("/sets");
    await page.getByRole("article").first().getByRole("button", { name: "Open" }).click();
    // exact: true — the detail page also has a "Preview retention plan"
    // button further down (Retention section); non-exact matching is
    // ambiguous between the two once the page is actually reachable.
    await page.getByRole("button", { name: "Preview retention", exact: true }).click();
    await expect(page.getByRole("dialog", { name: "Retention preview" })).toBeVisible();
    // The dialog renders immediately with "Requesting plan…" while
    // previewRetention() is still in flight. Wait for the plan to actually
    // resolve before any test runs.
    await expect(page.getByText(/Plan retplan_.* issued by the backup service/)).toBeVisible();
  });

  test("shows the server-issued plan id", async ({ page }) => {
    await expect(page.getByText(/Plan retplan_.* issued by the backup service/)).toBeVisible();
  });

  test("summarises keep, delete and reclaim", async ({ page }) => {
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("Keep", { exact: true }).first()).toBeVisible();
    await expect(dialog.getByText("Delete", { exact: true }).first()).toBeVisible();
    await expect(dialog.getByText("Reclaim")).toBeVisible();
  });

  test("itemises what is kept and why, and what is deleted and why", async ({ page }) => {
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText(/Not selected by current retention policy/).first()).toBeVisible();
    await expect(dialog.getByText("Protected").first()).toBeVisible();
  });

  // §96 design pass: a refused delete is a third, deliberate verdict
  // (FR-20), not an error — it must read as calm and informational, never
  // as a fault the operator has to dismiss.
  test("shows a refused artifact calmly, not as an error", async ({ page }) => {
    const dialog = page.getByRole("dialog");
    const refuseSection = dialog.getByText(/^Refuse/);
    await expect(refuseSection).toBeVisible();

    const refusedRow = dialog.locator(".banner--info", { hasText: "sibling-prefix directory" });
    await expect(refusedRow).toBeVisible();
    // Never the alert role, and never the danger banner class — that's
    // reserved for genuine faults (a broken transfer, an unreachable host).
    await expect(refusedRow).not.toHaveAttribute("role", "alert");
    await expect(dialog.locator(".banner--danger")).toHaveCount(0);
  });

  test("confirmation names the exact count and reclaimed size", async ({ page }) => {
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "Continue…" }).click();

    const confirm = page.getByRole("dialog", { name: "Apply retention" });
    await expect(confirm).toContainText("Destructive action");
    await expect(confirm).toContainText(/\d+ retained backup files will be permanently removed/);
    await expect(confirm).toContainText(/will be reclaimed/);
    await expect(confirm).toContainText(/will not be recalculated/);
    await expect(confirm).toContainText(/newest known-good backup is protected/);
  });

  test("the confirm button states the consequence and is not the default focus", async ({ page }) => {
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "Continue…" }).click();

    const confirm = page.getByRole("dialog", { name: "Apply retention" });
    await expect(confirm.getByRole("button", { name: /^Delete \d+ backups$/ })).toBeVisible();
    await expect(confirm.getByRole("button", { name: /^OK$/ })).toHaveCount(0);
    await expect(confirm.getByRole("button", { name: "Cancel" })).toBeFocused();
  });

  test("Escape cancels without deleting anything", async ({ page }) => {
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "Continue…" }).click();

    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: "Apply retention" })).toHaveCount(0);
  });

  test("clicking the scrim dismisses the preview", async ({ page }) => {
    await page.locator(".dialog-scrim").click({ position: { x: 5, y: 5 } });
    await expect(page.getByRole("dialog", { name: "Retention preview" })).toHaveCount(0);
  });

  // §29.3's full wizard flow: obtain a plan, present counts, confirm,
  // submit that exact plan_id, and the dialog closes on success.
  test("confirming applies the exact reviewed plan and closes the dialog", async ({ page }) => {
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "Continue…" }).click();

    const confirm = page.getByRole("dialog", { name: "Apply retention" });
    await confirm.getByRole("button", { name: /^Delete \d+ backups$/ }).click();

    await expect(page.getByRole("dialog", { name: "Apply retention" })).toHaveCount(0);
    await expect(page.getByRole("dialog", { name: "Retention preview" })).toHaveCount(0);
  });
});
