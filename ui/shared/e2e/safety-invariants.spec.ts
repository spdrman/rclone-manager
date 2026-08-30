import { expect, test } from "./fixtures";

/** The product's non-negotiables, asserted across the whole UI rather than in
 *  one component. If any of these fail the design has regressed, not the code. */
const ALL_PAGES = [
  "/", "/sets", "/sets/new", "/backups", "/activity", "/quarantine", "/settings", "/catalog-recovery"
];

test.describe("safety invariants", () => {
  test("no page exposes an arbitrary remote or file delete", async ({ page }) => {
    for (const path of ALL_PAGES) {
      await page.goto(path);
      await expect(page.getByRole("button", { name: /delete remote/i })).toHaveCount(0);
      await expect(page.getByRole("button", { name: /delete file/i })).toHaveCount(0);
      await expect(page.getByRole("button", { name: /force delete/i })).toHaveCount(0);
      await expect(page.getByRole("button", { name: /delete anyway/i })).toHaveCount(0);
    }
  });

  test("no page ever renders a private key", async ({ page }) => {
    for (const path of ALL_PAGES) {
      await page.goto(path);
      const text = await page.locator("body").innerText();
      expect(text, "private key material on " + path).not.toMatch(/BEGIN (OPENSSH|RSA|EC) PRIVATE KEY/i);
      expect(text).not.toMatch(/private key:/i);
    }
  });

  test("the product never calls a retained backup a restore point", async ({ page }) => {
    for (const path of ALL_PAGES) {
      await page.goto(path);
      expect(await page.locator("body").innerText()).not.toMatch(/restore point/i);
    }
  });

  test("no page offers restore execution", async ({ page }) => {
    for (const path of ALL_PAGES) {
      await page.goto(path);
      await expect(page.getByRole("button", { name: /^restore/i })).toHaveCount(0);
    }
  });

  test("no raw stack trace is ever displayed", async ({ page }) => {
    for (const path of ALL_PAGES) {
      await page.goto(path);
      const text = await page.locator("body").innerText();
      expect(text).not.toMatch(/at [\w$.]+ \(.*:\d+:\d+\)/);
      expect(text).not.toMatch(/Traceback \(most recent call last\)/);
    }
  });

  test("destructive buttons never use vague labels", async ({ page }) => {
    for (const path of ALL_PAGES) {
      await page.goto(path);
      await expect(page.getByRole("button", { name: /^(OK|Yes|Confirm|Submit)$/ })).toHaveCount(0);
    }
  });

  test("destructive actions are never the visual primary action", async ({ page }) => {
    await page.goto("/sets");
    await page.getByRole("article").first().getByRole("button", { name: "Open" }).click();

    const destructive = page.getByRole("button", { name: /Apply retention now|Remove set configuration/ });
    const count = await destructive.count();
    expect(count).toBeGreaterThan(0);

    for (let i = 0; i < count; i++) {
      const bg = await destructive.nth(i).evaluate((el) => getComputedStyle(el).backgroundColor);
      // Outline treatment only — a filled destructive button in-page is a defect.
      expect(bg === "rgba(0, 0, 0, 0)" || bg === "transparent").toBe(true);
    }
  });

  test("remote deletion is always described as post-commit", async ({ page }) => {
    await page.goto("/");
    await expect(
      page.getByText(/removed only after the NAS copy is verified and durably committed/)
    ).toBeVisible();

    await page.goto("/sets/new");
    await page.getByRole("button", { name: "Review" }).click();
    await expect(
      page.getByText(/transferred, verified, durably committed to this NAS, and recorded as safe/)
    ).toBeVisible();
  });

  test("freshness is stated as an age, not a bare date", async ({ page }) => {
    await page.goto("/");
    await expect(page.getByText(/(minutes|hours|days) ago/).first()).toBeVisible();
  });
});
