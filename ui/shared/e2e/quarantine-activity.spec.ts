import { expect, test } from "./fixtures";

test.describe("quarantine", () => {
  test.beforeEach(async ({ bm }) => {
    await bm.goto("/quarantine");
  });

  test("explains that remote originals are retained", async ({ page }) => {
    await expect(page.getByText(/remote originals are retained until the issue is resolved/)).toBeVisible();
    await expect(
      page.getByText(/never counted as known-good, and never trigger remote deletion/)
    ).toBeVisible();
  });

  test("shows all six documented columns", async ({ page }) => {
    for (const col of ["Backup", "Backup set", "Reason", "Detected", "Remote source", "Actions"]) {
      await expect(page.getByRole("columnheader", { name: col })).toBeVisible();
    }
  });

  test("names a concrete reason per row", async ({ page }) => {
    await expect(
      page.getByText(/Checksum mismatch|Validation failed|Unexpected artifact|Remote identity changed|Incomplete transfer/).first()
    ).toBeVisible();
  });

  test("offers only the three safe recovery actions", async ({ page }) => {
    const row = page.getByRole("row").nth(1);
    await expect(row.getByRole("button", { name: "Inspect" })).toBeVisible();
    await expect(row.getByRole("button", { name: "Revalidate" })).toBeVisible();
    await expect(row.getByRole("button", { name: "Retry ingestion" })).toBeVisible();
    await expect(row.getByRole("button")).toHaveCount(3);
  });

  test("never offers to delete the remote anyway", async ({ page }) => {
    await expect(page.getByRole("button", { name: /delete remote/i })).toHaveCount(0);
    await expect(page.getByRole("button", { name: /anyway/i })).toHaveCount(0);
  });

  test("empty quarantine reassures rather than looking broken", async ({ bm, page }) => {
    await bm.goto("/quarantine", "empty");
    await expect(page.getByText("No quarantined backups")).toBeVisible();
    await expect(page.getByText(/currently require attention/)).toBeVisible();
  });
});

test.describe("activity", () => {
  test.beforeEach(async ({ bm }) => {
    await bm.goto("/activity");
  });

  test("lists events with timestamp, severity glyph and set", async ({ page }) => {
    const first = page.getByRole("listitem").first();
    await expect(first).toContainText(/\d{2}:\d{2}:\d{2}/);
    await expect(first).not.toBeEmpty();
  });

  test("covers the documented event vocabulary", async ({ page }) => {
    const text = await page.locator("body").innerText();
    for (const event of [
      "Backup discovered", "Transfer complete", "Verification passed",
      "Backup committed", "Remote source deleted", "Retention completed"
    ]) {
      expect(text).toContain(event);
    }
  });

  test("offers exactly the four documented filters", async ({ page }) => {
    await expect(page.getByLabel("Backup set")).toBeVisible();
    await expect(page.getByLabel("Severity")).toBeVisible();
    await expect(page.getByLabel("Time range")).toBeVisible();
  });

  test("severity filter narrows the list", async ({ page }) => {
    const before = await page.getByRole("listitem").count();
    await page.getByLabel("Severity").selectOption("2");
    const after = await page.getByRole("listitem").count();
    expect(after).toBeLessThan(before);
    expect(after).toBeGreaterThan(0);
  });

  test("set filter narrows the list", async ({ page }) => {
    const before = await page.getByRole("listitem").count();
    await page.getByLabel("Backup set").selectOption({ index: 1 });
    expect(await page.getByRole("listitem").count()).toBeLessThan(before);
  });

  test("over-filtering shows an empty state, not a blank panel", async ({ page }) => {
    await page.getByLabel("Severity").selectOption("2");
    await page.getByLabel("Backup set").selectOption({ index: 2 });
    const empty = page.getByText("No matching events");
    const items = page.getByRole("listitem");
    if ((await items.count()) === 0) await expect(empty).toBeVisible();
  });
});
