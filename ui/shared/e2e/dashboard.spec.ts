import { expect, test } from "./fixtures";

test.describe("dashboard", () => {
  test("states the BACKUP verdict, not the daemon's", async ({ bm, page }) => {
    await bm.goto("/");
    // §8 — the headline is about backups; the daemon is a supporting chip.
    await expect(page.getByText(/^BACKUPS /)).toBeVisible();
    // Scoped to the header — the dashboard body's own health summary widget
    // repeats "Service running" with a different suffix (uptime, not
    // version), so an unscoped match is ambiguous.
    await expect(page.getByRole("banner").getByText(/Service running/)).toBeVisible();
    await expect(page.getByText(/a healthy daemon is not a healthy backup|not produced a verified backup|halted/i).first()).toBeVisible();
  });

  test("every status chip pairs a glyph with a text label", async ({ bm, page }) => {
    await bm.goto("/");
    const summary = page.getByRole("region", { name: "Backup health" });
    await expect(summary).toBeVisible();
    await expect(summary).toContainText(/Service running/);
    await expect(summary).toContainText(/Storage/);
  });

  test("reports freshness figures", async ({ bm, page }) => {
    await bm.goto("/");
    await expect(page.getByText("Last successful cycle")).toBeVisible();
    await expect(page.getByText("Newest verified backup")).toBeVisible();
    await expect(page.getByText("Oldest set freshness")).toBeVisible();
  });

  test("surfaces the host-key change above the metrics", async ({ bm, page }) => {
    await bm.goto("/");
    const alert = page.getByRole("alert").filter({ hasText: /host key/i });
    await expect(alert).toBeVisible();
    await expect(alert).toContainText(/Security warning/i);
    await expect(alert).toContainText(/No remote artifacts will be deleted/i);
  });

  test("host-key alert links through to the halted set", async ({ bm, page }) => {
    await bm.goto("/");
    await page.getByRole("button", { name: "Review fingerprint" }).click();
    await expect(page.getByText(/Fingerprint|Presented now/).first()).toBeVisible();
  });

  test("shows the four metric cards with a storage meter", async ({ bm, page }) => {
    await bm.goto("/");
    const metrics = page.getByRole("region", { name: "Key metrics" });
    await expect(metrics).toContainText("Backup sets");
    await expect(metrics).toContainText("Success rate");
    await expect(metrics).toContainText("Quarantine");
    await expect(metrics).toContainText("Storage");
    await expect(page.getByRole("meter", { name: "Storage used" })).toBeVisible();
  });

  test("renders an active transfer with staged progress", async ({ bm, page }) => {
    await bm.goto("/");
    const ops = page.getByRole("region", { name: "Active operations" });
    await expect(ops).toBeVisible();

    const bar = ops.getByRole("progressbar").first();
    await expect(bar).toBeVisible();
    const value = Number(await bar.getAttribute("aria-valuenow"));
    expect(value).toBeGreaterThan(0);
    expect(value).toBeLessThanOrEqual(100);

    // getByText(exact: true) matches an element's whole DOM text content,
    // which here includes the stage glyph (aria-hidden span rendered right
    // before the label, e.g. "✓Discovering") — an exact match on the label
    // alone never resolves. "listitem" has no ARIA name-from-content (name
    // is author-only per the ARIA role table), so getByRole(…, {name}) can't
    // find it either — a plain substring getByText is the fix, scoped to the
    // stage list itself (not the whole region) since "Transferring" is also
    // a substring of the op's own label, "Transferring backup", rendered
    // above the list.
    const stageList = ops.getByRole("list");
    for (const stage of ["Discovering", "Transferring", "Verifying", "Committing", "Cleaning remote source", "Complete"]) {
      await expect(stageList.getByText(stage)).toBeVisible();
    }
    await expect(ops.getByRole("listitem").filter({ has: page.locator("[aria-current='step']") })).toHaveCount(0);
  });

  test("marks the current transfer stage exactly once", async ({ bm, page }) => {
    await bm.goto("/");
    const current = page.getByRole("region", { name: "Active operations" }).locator("li[aria-current='step']");
    await expect(current).toHaveCount(1);
    await expect(current).toContainText("Transferring");
  });

  test("labels a read-only pass as non-destructive", async ({ bm, page }) => {
    await bm.goto("/");
    await expect(page.getByText(/Read-only pass .* no artifacts are deleted/)).toBeVisible();
  });

  test("explains remote deletion ordering under the transfer", async ({ bm, page }) => {
    await bm.goto("/");
    await expect(
      page.getByText(/removed only after the NAS copy is verified and durably committed/)
    ).toBeVisible();
  });

  test("lists recent activity and links to the full timeline", async ({ bm, page }) => {
    await bm.goto("/");
    const recent = page.getByRole("region", { name: "Recent activity" });
    await expect(recent.getByRole("listitem").first()).toBeVisible();
    await recent.getByRole("button", { name: "View all" }).click();
    await expect(bm.heading("Activity")).toBeVisible();
  });

  test("empty installation shows an actionable empty state", async ({ bm, page }) => {
    await bm.goto("/", "empty");
    await expect(page.getByText("No backup sets yet")).toBeVisible();
    await expect(page.getByText(/Connect Backup Manager to your first server/)).toBeVisible();
    await page.getByRole("button", { name: "Add backup set" }).click();
    await expect(bm.heading("Add backup set")).toBeVisible();
  });

  test("critical storage escalates the headline", async ({ bm, page }) => {
    await bm.goto("/", "storage-critical");
    await expect(page.getByText("BACKUPS FAILING")).toBeVisible();
    await expect(page.getByText(/Storage critical/)).toBeVisible();
  });
});
