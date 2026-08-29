import { defineConfig, devices } from "@playwright/test";

/** The suite runs against the mock API (`npm run dev`), which is deterministic
 *  and covers every documented state. Provider projects re-run the shell specs
 *  against each platform bridge so provider treatment cannot regress. */
export default defineConfig({
  testDir: "./e2e",
  timeout: 30_000,
  expect: { timeout: 5_000 },
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["github"], ["html", { open: "never" }]] : [["list"]],

  use: {
    baseURL: "http://localhost:5173",
    trace: "on-first-retry",
    screenshot: "only-on-failure",
    // Standard desktop NAS administration window (§31).
    viewport: { width: 1440, height: 900 }
  },

  projects: [
    {
      name: "generic",
      use: { ...devices["Desktop Chrome"], viewport: { width: 1440, height: 900 } }
    },
    {
      // Only the provider-treatment spec runs per provider; shared product
      // behaviour is proven once, not seven times (§44).
      name: "providers",
      testMatch: /provider-treatment\.spec\.ts/,
      use: { ...devices["Desktop Chrome"] }
    },
    {
      name: "small-window",
      testMatch: /responsive\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], viewport: { width: 940, height: 720 } }
    }
  ],

  webServer: {
    command: "npm run dev",
    url: "http://localhost:5173",
    reuseExistingServer: !process.env.CI,
    timeout: 60_000
  }
});
