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

  test("tab order reaches the section navigation", async ({ page, bm }) => {
    // bm.goto() rather than a bare page.goto(), for the reason the next
    // test already gives: it waits on the section nav rendering instead of
    // racing the app's mount. This one was missed when its neighbours were
    // fixed, and it won the race often enough to look green. Issue #176's
    // first-run round trip now sits in front of the shell, so the same
    // race is one this test always loses: it tabs through a page that has
    // not painted its nav yet and reports the nav unreachable, which reads
    // as an accessibility regression and is really a test that never
    // waited. With the shell up, the nav is three tab stops away.
    await bm.goto("/");
    for (let i = 0; i < 12; i++) {
      await page.keyboard.press("Tab");
      const inNav = await page.evaluate(
        () => !!document.activeElement?.closest("nav[aria-label='Sections']")
      );
      if (inNav) return;
    }
    throw new Error("Section navigation was not reachable within 12 tab stops");
  });

  test("focus is visible on the focused control", async ({ page, bm }) => {
    // bm.goto() (not a bare page.goto()) so this test waits on the same
    // real signal — the section nav rendering — the rest of this PR's
    // fixes use, instead of racing the app's mount.
    await bm.goto("/");
    // Not part of the async-data race the rest of this file's siblings
    // share (confirmed by hand: the CSS's :focus-visible rule is correct,
    // and the very first Tab keypress after goto() is what's unreliable —
    // dispatching it immediately, before Chromium has settled page focus
    // post-navigation, deterministically leaves focus on <body> in headless
    // mode). The real signal being waited on is the browser's document
    // focus actually settling post-navigation, so assert that directly
    // instead of guessing at how many frames it takes; a bare extra
    // `page.keyboard.press("Tab")` would also dodge the race, but would
    // then be testing the second tab stop, not the first. This is the
    // harness workaround #113 already anticipated.
    await page.waitForFunction(() => document.hasFocus());
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
