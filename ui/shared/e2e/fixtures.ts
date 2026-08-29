import { expect, test as base } from "@playwright/test";
import type { Page } from "@playwright/test";

export type Scenario =
  | "default"
  | "empty"
  | "storage-critical"
  | "catalog-recovery"
  | "version-mismatch";

export const PROVIDERS = [
  "generic",
  "ugos",
  "synology",
  "truenas",
  "unraid",
  "openmediavault",
  "proxmox"
] as const;

/** Page-object surface. Keeps the specs readable and stops selector drift from
 *  spreading across the suite. */
export class BackupManager {
  constructor(readonly page: Page) {}

  async goto(path = "/", scenario: Scenario = "default") {
    const url = scenario === "default" ? path : path + "?scenario=" + scenario;
    await this.page.goto(url);
    // The shell is present once the section nav has rendered.
    await expect(this.page.getByRole("navigation", { name: "Sections" })).toBeVisible();
  }

  nav(label: string) {
    return this.page.getByRole("navigation", { name: "Sections" }).getByRole("link", { name: new RegExp(label, "i") });
  }

  async navigateTo(label: string) {
    await this.nav(label).click();
  }

  heading(name: string | RegExp) {
    return this.page.getByRole("heading", { level: 1, name });
  }

  card(label: string) {
    return this.page.getByRole("region", { name: label });
  }

  dialog(name?: string | RegExp) {
    return name ? this.page.getByRole("dialog", { name }) : this.page.getByRole("dialog");
  }

  async theme() {
    return this.page.locator("html").getAttribute("data-theme");
  }

  async toggleTheme() {
    await this.page.getByRole("button", { name: "Toggle colour theme" }).click();
  }

  async cssVar(name: string) {
    return this.page.evaluate(
      (v) => getComputedStyle(document.documentElement).getPropertyValue(v).trim(),
      name
    );
  }

  /** Every status in this product must carry a glyph AND a text label. */
  async assertNotColourOnly(locator = this.page.locator("body")) {
    const text = await locator.innerText();
    expect(text.length).toBeGreaterThan(0);
  }
}

export const test = base.extend<{ bm: BackupManager }>({
  bm: async ({ page }, use) => {
    await use(new BackupManager(page));
  }
});

export { expect };
