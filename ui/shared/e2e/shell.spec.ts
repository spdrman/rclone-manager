import { expect, test } from "./fixtures";

test.describe("application shell", () => {
  test("renders the lockup, service chip and every section link", async ({ bm, page }) => {
    await bm.goto("/");

    await expect(page.getByRole("img", { name: "Backup Manager" })).toBeVisible();
    await expect(page.getByText("rclone-manager")).toBeVisible();
    await expect(page.getByText(/Service running/)).toBeVisible();

    for (const label of ["Dashboard", "Backup sets", "Backups", "Activity", "Quarantine", "Settings"]) {
      await expect(bm.nav(label)).toBeVisible();
    }
  });

  test("navigates to every section and marks the active link", async ({ bm, page }) => {
    await bm.goto("/");

    const routes: Array<[string, string | RegExp]> = [
      ["Backup sets", "Backup sets"],
      ["Backups", "Backups"],
      ["Activity", "Activity"],
      ["Quarantine", "Quarantine"],
      ["Settings", "Settings"],
      ["Dashboard", "Dashboard"]
    ];

    for (const [link, heading] of routes) {
      await bm.navigateTo(link);
      await expect(bm.heading(heading)).toBeVisible();
      await expect(bm.nav(link)).toHaveAttribute("aria-current", "page");
    }
  });

  test("shows counts on sets, backups and quarantine", async ({ bm }) => {
    await bm.goto("/");
    await expect(bm.nav("Backup sets")).toContainText(/\d/);
    await expect(bm.nav("Backups")).toContainText(/\d/);
    await expect(bm.nav("Quarantine")).toContainText(/\d/);
  });

  test("names the platform it is running on", async ({ bm, page }) => {
    await bm.goto("/");
    await expect(page.getByText("Running on")).toBeVisible();
  });

  test("theme toggle flips and survives reload", async ({ bm }) => {
    await bm.goto("/");
    expect(await bm.theme()).toBe("light");

    await bm.toggleTheme();
    expect(await bm.theme()).toBe("dark");

    await bm.page.reload();
    expect(await bm.theme()).toBe("dark");

    await bm.toggleTheme();
    expect(await bm.theme()).toBe("light");
  });

  test("surfaces text on a dark surface with a real foreground colour", async ({ bm }) => {
    await bm.goto("/");
    await bm.toggleTheme();
    const bg = await bm.cssVar("--bg");
    const text = await bm.cssVar("--text");
    expect(bg).not.toBe("");
    expect(text).not.toBe("");
    expect(bg).not.toBe(text);
  });
});
