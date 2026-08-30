import { expect, test } from "./fixtures";

/** §30 — the login must not resemble a NAS system login, and must say so.
 *
 *  Every other spec in this suite starts authenticated (the fixture handles
 *  it — see fixtures.ts). This file is the one place that deliberately drops
 *  back to signed-out, using bm.signOut()/bm.setAuthenticated(false) rather
 *  than assuming a fresh page load is unauthenticated. */
test.describe("local authentication", () => {
  test("sign-out lands on the Backup Manager login", async ({ page, bm }) => {
    await bm.goto("/");
    await bm.signOut();

    await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
    await expect(page.getByText(/not.*your NAS operating-system login/i)).toBeVisible();
    await expect(page.getByRole("img", { name: "Backup Manager" })).toBeVisible();
  });

  test("login requires both fields", async ({ page, bm }) => {
    await bm.goto("/");
    await bm.signOut();

    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();

    const username = page.getByLabel("Username");
    await expect(username).toHaveAttribute("required", "");
    await expect(page.getByLabel("Password")).toHaveAttribute("type", "password");
  });

  test("uses correct autocomplete hints for password managers", async ({ page, bm }) => {
    await bm.goto("/");
    await bm.signOut();
    await expect(page.getByLabel("Username")).toHaveAttribute("autocomplete", "username");
    await expect(page.getByLabel("Password")).toHaveAttribute("autocomplete", "current-password");
  });

  test("first-run enrolment explains account scope", async ({ page, bm }) => {
    await bm.goto("/");
    await bm.signOut();
    await page.getByRole("link", { name: /Create the administrator account/ }).click();

    await expect(page.getByRole("heading", { name: /Create Backup Manager administrator/ })).toBeVisible();
    await expect(
      page.getByText(/manages Backup Manager only.*separate from your NAS operating-system account/s)
    ).toBeVisible();
  });

  test("enrolment enforces length and confirmation before enabling submit", async ({ page, bm }) => {
    // /enroll is only served while signed out (App.tsx routes it away once
    // authenticated), and there is no shell to wait for here, so this goes
    // straight through page.goto rather than bm.goto.
    bm.setAuthenticated(false);
    await page.goto("/enroll");

    const submit = page.getByRole("button", { name: "Create administrator" });
    await expect(submit).toBeDisabled();

    // Prefix regex, not exact: true — the "Minimum 12 characters." warning
    // renders inside this field's own <label>, so its accessible name grows
    // from "Password" to "PasswordMinimum 12 characters." (Playwright's own
    // label-text accumulation, unlike a screen reader's accessibility tree,
    // does not insert a separator between sibling text nodes) the moment a
    // too-short value is typed. A leading-anchored prefix still can't
    // collide with "Confirm password", which starts with "Confirm".
    const password = page.getByLabel(/^Password/);
    await page.getByLabel("Username").fill("bm-admin");
    await password.fill("short");
    await expect(page.getByText(/Minimum 12 characters/).first()).toBeVisible();
    await expect(submit).toBeDisabled();

    await password.fill("a-long-enough-passphrase");
    await page.getByLabel("Confirm password").fill("different-passphrase");
    await expect(page.getByText("Passwords do not match")).toBeVisible();
    await expect(submit).toBeDisabled();

    await page.getByLabel("Confirm password").fill("a-long-enough-passphrase");
    await expect(page.getByText("Passwords do not match")).toHaveCount(0);
    await expect(submit).toBeEnabled();
  });

  test("never echoes a password in the DOM as plain text", async ({ page, bm }) => {
    bm.setAuthenticated(false);
    await page.goto("/enroll");
    await page.getByLabel("Password", { exact: true }).fill("super-secret-passphrase");
    expect(await page.locator("body").innerText()).not.toContain("super-secret-passphrase");
  });
});
