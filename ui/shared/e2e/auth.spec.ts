import { expect, test } from "@playwright/test";

/** §30 — the login must not resemble a NAS system login, and must say so. */
test.describe("local authentication", () => {
  test("sign-out lands on the Backup Manager login", async ({ page }) => {
    await page.goto("/");
    await page.getByRole("button", { name: "Sign out" }).click();

    await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
    await expect(page.getByText(/not.*your NAS operating-system login/i)).toBeVisible();
    await expect(page.getByRole("img", { name: "Backup Manager" })).toBeVisible();
  });

  test("login requires both fields", async ({ page }) => {
    await page.goto("/");
    await page.getByRole("button", { name: "Sign out" }).click();

    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();

    const username = page.getByLabel("Username");
    await expect(username).toHaveAttribute("required", "");
    await expect(page.getByLabel("Password")).toHaveAttribute("type", "password");
  });

  test("uses correct autocomplete hints for password managers", async ({ page }) => {
    await page.goto("/");
    await page.getByRole("button", { name: "Sign out" }).click();
    await expect(page.getByLabel("Username")).toHaveAttribute("autocomplete", "username");
    await expect(page.getByLabel("Password")).toHaveAttribute("autocomplete", "current-password");
  });

  test("first-run enrolment explains account scope", async ({ page }) => {
    await page.goto("/");
    await page.getByRole("button", { name: "Sign out" }).click();
    await page.getByRole("link", { name: /Create the administrator account/ }).click();

    await expect(page.getByRole("heading", { name: /Create Backup Manager administrator/ })).toBeVisible();
    await expect(
      page.getByText(/manages Backup Manager only.*separate from your NAS operating-system account/s)
    ).toBeVisible();
  });

  test("enrolment enforces length and confirmation before enabling submit", async ({ page }) => {
    await page.goto("/enroll");

    const submit = page.getByRole("button", { name: "Create administrator" });
    await expect(submit).toBeDisabled();

    await page.getByLabel("Username").fill("bm-admin");
    await page.getByLabel("Password", { exact: true }).fill("short");
    await expect(page.getByText(/Minimum 12 characters/).first()).toBeVisible();
    await expect(submit).toBeDisabled();

    await page.getByLabel("Password", { exact: true }).fill("a-long-enough-passphrase");
    await page.getByLabel("Confirm password").fill("different-passphrase");
    await expect(page.getByText("Passwords do not match")).toBeVisible();
    await expect(submit).toBeDisabled();

    await page.getByLabel("Confirm password").fill("a-long-enough-passphrase");
    await expect(page.getByText("Passwords do not match")).toHaveCount(0);
    await expect(submit).toBeEnabled();
  });

  test("never echoes a password in the DOM as plain text", async ({ page }) => {
    await page.goto("/enroll");
    await page.getByLabel("Password", { exact: true }).fill("super-secret-passphrase");
    expect(await page.locator("body").innerText()).not.toContain("super-secret-passphrase");
  });
});
