import { defineConfig, devices } from "@playwright/test";

// The suite serves itself on its own port, not Vite's default 5173, so a
// `npm run dev` a developer already has open is never what gets tested, and
// never has to be shut down to run the suite. Override with E2E_PORT when two
// checkouts of this repo need to run the suite at the same time.
const PORT = Number(process.env.E2E_PORT ?? 5273);
const BASE_URL = "http://localhost:" + PORT;

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
    baseURL: BASE_URL,
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
    // --strictPort: fail rather than quietly slide to the next free port,
    // which would leave the suite pointing at BASE_URL with nothing there.
    command: "npm run dev -- --port " + PORT + " --strictPort",
    url: BASE_URL,
    // Never adopt a server this run did not start. This used to be
    // `!process.env.CI`, which meant that locally, whatever happened to be
    // listening answered the suite — including a dev server started from a
    // different worktree of this repo, serving a different build. That is
    // what made #172 look like an ordering flake: "step 5 lists the three
    // validation layers" was green in isolation only because a pre-#164
    // server was still up on 5173 and still rendered the toggle #164 had
    // replaced. On a port nothing else is holding, it failed alone and in
    // company alike. scripts/e2e-all-providers.mjs hit the same bug from the
    // other direction and worked around it by forcing CI=1; it no longer has
    // to. A run that cannot start its own server now fails loudly instead of
    // reporting on someone else's code.
    reuseExistingServer: false,
    timeout: 60_000
  }
});
