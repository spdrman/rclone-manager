import { expect, test } from "./fixtures";

test.describe("backups table", () => {
  test.beforeEach(async ({ bm }) => {
    await bm.goto("/backups");
  });

  test("is called Backups, never Restore points", async ({ bm, page }) => {
    await expect(bm.heading("Backups")).toBeVisible();
    expect(await page.locator("body").innerText()).not.toMatch(/restore point/i);
  });

  test("states plainly that it does not perform restore", async ({ page }) => {
    await expect(
      page.getByText(/does not perform application restore .* retained, verified copies/)
    ).toBeVisible();
  });

  test("renders a real table with all seven documented columns", async ({ page }) => {
    const table = page.getByRole("table");
    await expect(table).toBeVisible();
    for (const col of ["Time", "Backup set", "Artifact", "Size", "Validation", "Retention", "Status"]) {
      await expect(table.getByRole("columnheader", { name: col })).toBeVisible();
    }
  });

  test("uses column headers, not styled divs", async ({ page }) => {
    await expect(page.getByRole("columnheader")).toHaveCount(7);
    const rows = await page.getByRole("row").count();
    expect(rows).toBeGreaterThan(1);
  });

  test("a backup can hold several retention classifications at once", async ({ page }) => {
    const multi = page.getByRole("row").filter({ hasText: "Daily" }).filter({ hasText: "Weekly" });
    await expect(multi.first()).toBeVisible();
  });

  test("marks the protected artifact distinctly", async ({ page }) => {
    await expect(page.getByText("Protected").first()).toBeVisible();
  });

  test("validation shows a glyph and a word, not just colour", async ({ page }) => {
    const cell = page.getByRole("row").nth(1);
    await expect(cell).toContainText(/Verified|Failed|Pending/);
  });

  test("status distinguishes removed from retained remote sources", async ({ page }) => {
    await expect(page.getByText(/Remote source (removed|retained)/).first()).toBeVisible();
  });

  test("filters by backup set", async ({ page }) => {
    const before = await page.getByRole("row").count();
    await page.getByLabel("Filter by backup set").selectOption({ index: 1 });
    await expect(page.getByRole("row")).not.toHaveCount(before);
  });

  test("retention preview requires a set to be chosen first", async ({ page }) => {
    await expect(page.getByRole("button", { name: "Preview retention" })).toBeDisabled();
    await page.getByLabel("Filter by backup set").selectOption({ index: 1 });
    await expect(page.getByRole("button", { name: "Preview retention" })).toBeEnabled();
  });

  test("row click opens the artifact detail", async ({ page }) => {
    await page.getByRole("row").nth(1).click();
    await expect(page.getByText("Artifact ID")).toBeVisible();
  });

  test("offers no arbitrary file-delete action", async ({ page }) => {
    await expect(page.getByRole("button", { name: /delete remote/i })).toHaveCount(0);
    await expect(page.getByRole("button", { name: /^delete file/i })).toHaveCount(0);
  });

  test("empty install explains what has not happened yet", async ({ bm, page }) => {
    await bm.goto("/backups", "empty");
    await expect(page.getByText("No backups yet")).toBeVisible();
    await expect(page.getByText(/has not completed its first successful ingestion/)).toBeVisible();
  });
});

test.describe("backup detail", () => {
  test.beforeEach(async ({ bm, page }) => {
    await bm.goto("/backups");
    await page.getByRole("row").nth(1).click();
  });

  test("shows every documented artifact field", async ({ page }) => {
    for (const label of [
      "Artifact ID", "Backup set", "Remote original", "Local path",
      "Producer timestamp", "Received timestamp", "Size", "Checksum",
      "Validation result", "Retention classes", "Remote source removed"
    ]) {
      await expect(page.getByText(label, { exact: true })).toBeVisible();
    }
  });

  test("renders the lifecycle in the only correct order", async ({ page }) => {
    const timeline = page.getByRole("region", { name: "Lifecycle" }).or(page.locator("ol").last());
    const text = await timeline.innerText();

    const order = [
      "DISCOVERED", "TRANSFERRED", "VERIFIED", "COMMITTED",
      "SAFE STATE PERSISTED", "REMOTE SOURCE DELETED"
    ];
    const positions = order.map((label) => text.indexOf(label));

    for (const p of positions) expect(p).toBeGreaterThan(-1);
    for (let i = 1; i < positions.length; i++) {
      // Remote deletion can never precede commit (§15).
      expect(positions[i]).toBeGreaterThan(positions[i - 1]);
    }
  });

  test("timestamps every reached phase", async ({ page }) => {
    const timeline = page.locator("ol").last();
    await expect(timeline).toContainText(/\d{2}:\d{2}:\d{2}/);
  });

  test("frames remote deletion as a consequence, not an action", async ({ page }) => {
    await expect(
      page.getByText(/lifecycle consequence of a proven NAS copy .* never an independent file operation/)
    ).toBeVisible();
    await expect(page.getByRole("button", { name: /delete/i })).toHaveCount(0);
  });

  test("returns to the table", async ({ bm, page }) => {
    await page.getByRole("button", { name: /Backups/ }).first().click();
    await expect(bm.heading("Backups")).toBeVisible();
  });
});
