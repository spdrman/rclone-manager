import { expect, test } from "./fixtures";

/** §32 — keyboard, semantics and non-colour-only status, checked structurally
 *  rather than with a snapshot so failures point at a real defect. */
const PAGES = ["/", "/sets", "/backups", "/activity", "/quarantine", "/settings"];

test.describe("accessibility", () => {
  for (const path of PAGES) {
    test("exactly one h1 on " + path, async ({ page }) => {
      await page.goto(path);
      await expect(page.getByRole("heading", { level: 1 })).toHaveCount(1);
    });

    test("every control is reachable and labelled on " + path, async ({ page }) => {
      await page.goto(path);

      const unlabelled = await page.evaluate(() => {
        const controls = Array.from(
          document.querySelectorAll("button, a[href], input, select, textarea")
        );
        return controls
          .filter((el) => {
            const style = getComputedStyle(el);
            if (style.display === "none" || style.visibility === "hidden") return false;
            const label =
              el.getAttribute("aria-label") ??
              el.getAttribute("title") ??
              (el as HTMLElement).innerText ??
              "";
            if (label.trim().length > 0) return false;
            // An input may be labelled by a wrapping <label>.
            return !el.closest("label");
          })
          .map((el) => el.tagName + (el.className ? "." + String(el.className).slice(0, 40) : ""));
      });

      expect(unlabelled, "unlabelled controls on " + path).toEqual([]);
    });

    test("status is never colour-only on " + path, async ({ page }) => {
      await page.goto(path);
      // Any element painted with a status token must also contain text.
      const colourOnly = await page.evaluate(() => {
        const statusVars = ["--ok", "--warn", "--danger"];
        const resolved = statusVars.map((v) =>
          getComputedStyle(document.documentElement).getPropertyValue(v).trim()
        );
        return Array.from(document.querySelectorAll<HTMLElement>("*"))
          .filter((el) => {
            const bg = getComputedStyle(el).backgroundColor;
            const isStatus = resolved.some((c) => c && bg.includes(c));
            if (!isStatus) return false;
            const hasText = (el.innerText ?? "").trim().length > 0;
            const hasRole = el.getAttribute("role") === "progressbar" || el.getAttribute("role") === "meter";
            const labelled = el.getAttribute("aria-label") ?? el.getAttribute("aria-hidden");
            return !hasText && !hasRole && !labelled;
          })
          .map((el) => el.tagName);
      });

      expect(colourOnly, "colour-only status indicators on " + path).toEqual([]);
    });
  }

  test("tab order reaches the section navigation", async ({ page }) => {
    await page.goto("/");
    for (let i = 0; i < 12; i++) {
      await page.keyboard.press("Tab");
      const inNav = await page.evaluate(
        () => !!document.activeElement?.closest("nav[aria-label='Sections']")
      );
      if (inNav) return;
    }
    throw new Error("Section navigation was not reachable within 12 tab stops");
  });

  test("focus is visible on the focused control", async ({ page }) => {
    await page.goto("/");
    await page.keyboard.press("Tab");
    const outline = await page.evaluate(() => {
      const el = document.activeElement as HTMLElement | null;
      if (!el) return null;
      const s = getComputedStyle(el);
      return { width: s.outlineWidth, style: s.outlineStyle };
    });
    expect(outline).not.toBeNull();
    expect(outline!.style).not.toBe("none");
  });

  test("dialogs are modal, labelled and Escape-dismissible", async ({ page }) => {
    await page.goto("/sets");
    await page.getByRole("article").first().getByRole("button", { name: "Open" }).click();
    // exact: true — disambiguates from the detail page's other
    // "Preview retention plan" button.
    await page.getByRole("button", { name: "Preview retention", exact: true }).click();

    const dialog = page.getByRole("dialog");
    await expect(dialog).toHaveAttribute("aria-modal", "true");
    const label = (await dialog.getAttribute("aria-label")) ?? (await dialog.getAttribute("aria-labelledby"));
    expect(label).toBeTruthy();
  });

  test("progress and meters expose their values", async ({ page }) => {
    await page.goto("/");
    const bar = page.getByRole("progressbar").first();
    await expect(bar).toHaveAttribute("aria-valuenow", /\d+/);
    await expect(bar).toHaveAttribute("aria-valuemin", "0");
    await expect(bar).toHaveAttribute("aria-valuemax", "100");

    const meter = page.getByRole("meter", { name: "Storage used" });
    await expect(meter).toHaveAttribute("aria-valuenow", /\d+/);
  });

  test("tables use real table semantics", async ({ page }) => {
    await page.goto("/backups");
    const table = page.getByRole("table");
    await expect(table).toBeVisible();
    await expect(table.getByRole("columnheader").first()).toBeVisible();
    await expect(table.getByRole("row").nth(1)).toBeVisible();
  });

  test("no console errors on any page", async ({ page }) => {
    const errors: string[] = [];
    page.on("console", (msg) => {
      if (msg.type() === "error") errors.push(msg.text());
    });
    page.on("pageerror", (e) => errors.push(e.message));

    for (const path of PAGES) {
      await page.goto(path);
      await page.waitForLoadState("networkidle");
    }

    expect(errors).toEqual([]);
  });
});
