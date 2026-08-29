import { expect, test } from "@playwright/test";

/** §46 — provider treatment is 5-10%: accent token, window chrome, native
 *  capabilities. This spec proves the shared product is identical across
 *  providers and that no provider claims a capability it lacks (§45).
 *
 *  Run per provider:  VITE_PLATFORM=<id> npx playwright test --project=providers
 */
const PLATFORM = process.env.VITE_PLATFORM ?? "generic";

const EXPECTED: Record<
  string,
  { name: string; integration: string; nativeAuth: boolean; nativeNotify: boolean; picker: boolean; chrome: boolean; mount: string }
> = {
  generic: { name: "Generic Docker / Linux", integration: "Standalone web app", nativeAuth: false, nativeNotify: false, picker: false, chrome: false, mount: "/data/backups" },
  ugos: { name: "UGREEN UGOS Pro", integration: "Native app", nativeAuth: true, nativeNotify: true, picker: true, chrome: true, mount: "/volume1/backup-manager" },
  synology: { name: "Synology DSM", integration: "Embedded web app", nativeAuth: false, nativeNotify: false, picker: false, chrome: true, mount: "/volume1/backup-manager" },
  truenas: { name: "TrueNAS", integration: "Container app", nativeAuth: false, nativeNotify: false, picker: false, chrome: false, mount: "/mnt/tank/backup-manager" },
  unraid: { name: "Unraid", integration: "Container app", nativeAuth: false, nativeNotify: false, picker: false, chrome: false, mount: "/mnt/user/backups" },
  openmediavault: { name: "OpenMediaVault", integration: "Compose plugin container", nativeAuth: false, nativeNotify: false, picker: false, chrome: false, mount: "/srv/dev-disk-by-uuid/backups" },
  proxmox: { name: "Proxmox VE", integration: "Standalone web app", nativeAuth: false, nativeNotify: false, picker: false, chrome: false, mount: "/mnt/backup-manager" }
};

const expected = EXPECTED[PLATFORM];

test.describe("provider: " + PLATFORM, () => {
  test("identifies itself in the sidebar and settings", async ({ page }) => {
    await page.goto("/settings");
    await expect(page.getByText(expected.name).first()).toBeVisible();
    await expect(page.getByText(expected.mount).first()).toBeVisible();
  });

  test("keeps the shared information architecture", async ({ page }) => {
    await page.goto("/");
    const nav = page.getByRole("navigation", { name: "Sections" });
    for (const label of ["Dashboard", "Backup sets", "Backups", "Activity", "Quarantine", "Settings"]) {
      await expect(nav.getByRole("link", { name: new RegExp(label, "i") })).toBeVisible();
    }
  });

  test("retains Backup Manager product identity, not provider branding", async ({ page }) => {
    await page.goto("/");
    await expect(page.getByText("rclone-manager")).toBeVisible();
    const title = await page.title();
    expect(title).toMatch(/Backup Manager/);
  });

  test("declares an accent token", async ({ page }) => {
    await page.goto("/");
    const accent = await page.evaluate(() =>
      getComputedStyle(document.documentElement).getPropertyValue("--accent").trim()
    );
    expect(accent).not.toBe("");
  });

  test("draws window chrome only when the platform is embedded", async ({ page }) => {
    await page.goto("/");
    const titlebar = page.getByText(new RegExp("Backup Manager .* " + expected.name.replace(/[.*+?^\${}()|[\]\\]/g, "\\$&")));
    if (expected.chrome) {
      await expect(titlebar.first()).toBeVisible();
    } else {
      await expect(titlebar).toHaveCount(0);
    }
  });

  test("offers a native storage picker only when supported", async ({ page }) => {
    await page.goto("/sets/new");
    await page.getByRole("button", { name: "Storage & retention" }).click();

    if (expected.picker) {
      await expect(page.getByRole("button", { name: /Browse volumes/ })).toBeVisible();
    } else {
      await expect(page.getByRole("button", { name: /Browse volumes/ })).toHaveCount(0);
      await expect(page.getByRole("button", { name: "Validate path" })).toBeVisible();
      await expect(page.getByText(/no native storage picker/)).toBeVisible();
    }
  });

  test("describes notifications honestly", async ({ page }) => {
    await page.goto("/settings");
    if (expected.nativeNotify) {
      await expect(page.getByText(/Native .* notifications are available and enabled/)).toBeVisible();
    } else {
      await expect(page.getByText(/Native NAS notifications are not available/)).toBeVisible();
    }
  });

  test("reports the correct authentication mode", async ({ page }) => {
    await page.goto("/settings");
    if (expected.nativeAuth) {
      await expect(page.getByText(new RegExp(expected.name + " session"))).toBeVisible();
    } else {
      await expect(page.getByText("Backup Manager local account").first()).toBeVisible();
    }
  });

  test("contains no provider-specific lifecycle UI", async ({ page }) => {
    for (const path of ["/", "/sets", "/backups", "/quarantine"]) {
      await page.goto(path);
      const text = await page.locator("body").innerText();
      // Lifecycle vocabulary is shared; a provider name must never qualify it.
      expect(text).not.toMatch(new RegExp(expected.name + " (transfer|commit|retention)", "i"));
    }
  });
});
