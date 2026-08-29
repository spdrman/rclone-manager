import { expect, test } from "@playwright/test";

/** §31 — desktop-first, but a small app window must stay fully usable. This
 *  project runs at 940x720; the regression it guards is a clipped table column. */
test.describe("small app window", () => {
  test("no data table hides a column", async ({ page }) => {
    for (const path of ["/backups", "/quarantine"]) {
      await page.goto(path);

      const scroller = page.locator(".table-scroll");
      await expect(scroller).toBeVisible();
      expect(await scroller.evaluate((el) => getComputedStyle(el).overflowX)).toBe("auto");

      // Every column header must be reachable by scrolling the container.
      const headers = page.getByRole("columnheader");
      const count = await headers.count();
      expect(count).toBeGreaterThan(0);
      await headers.nth(count - 1).scrollIntoViewIfNeeded();
      await expect(headers.nth(count - 1)).toBeVisible();
    }
  });

  test("the page itself never scrolls sideways", async ({ page }) => {
    for (const path of ["/", "/sets", "/backups", "/activity", "/settings"]) {
      await page.goto(path);
      const overflowing = await page.evaluate(
        () => document.documentElement.scrollWidth > document.documentElement.clientWidth + 1
      );
      expect(overflowing, "horizontal page scroll on " + path).toBe(false);
    }
  });

  test("navigation and primary actions stay reachable", async ({ page }) => {
    await page.goto("/");
    await expect(page.getByRole("navigation", { name: "Sections" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Add backup set" })).toBeVisible();
  });

  test("set cards reflow instead of overlapping", async ({ page }) => {
    await page.goto("/sets");
    const cards = page.getByRole("article");
    const count = await cards.count();

    const boxes = [];
    for (let i = 0; i < count; i++) boxes.push(await cards.nth(i).boundingBox());

    for (const box of boxes) {
      expect(box).not.toBeNull();
      expect(box!.width).toBeGreaterThan(240);
    }
  });

  test("dialogs remain fully visible", async ({ page }) => {
    await page.goto("/sets");
    await page.getByRole("article").first().getByRole("button", { name: "Open" }).click();
    await page.getByRole("button", { name: "Preview retention" }).click();

    const dialog = page.getByRole("dialog");
    const box = await dialog.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.y).toBeGreaterThanOrEqual(0);
    expect(box!.height).toBeLessThanOrEqual(720);
  });
});
