// The build and test configuration for the shared UI, and the one place the
// provider shell is chosen.
//
// This workspace holds a UI that has to become seven slightly different
// applications without seven copies of itself, and the direction of the
// dependency is what makes that work: the shared UI never imports a provider,
// the provider imports the shared UI. That inversion has to be resolved
// somewhere, and here is the only place it can be, because a bundler alias is
// the one mechanism that can answer "which provider" at build time without a
// runtime switch in shipped code. VITE_PLATFORM picks the shell,
// @platform-entry resolves to that provider's bootstrap, and __PLATFORM_ID__
// bakes the name in so the running bundle can say which one it is. Every
// bundle scripts/build-bundles.mjs produces is one pass through here with a
// different value.
//
// The test block underneath is the same file wearing its other hat, because
// vitest reads its configuration from this one. Two of its settings are
// decisions rather than defaults and are argued where they sit.

import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { resolve } from "node:path";

// The provider shell is chosen at build time. The shared UI never imports a
// provider; the provider imports the shared UI.
const platform = process.env.VITE_PLATFORM ?? "generic";

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      "@shared": resolve(__dirname, "src"),
      "@platform-entry": resolve(__dirname, "../../apps", platform, "frontend/bootstrap.tsx")
    }
  },
  define: { __PLATFORM_ID__: JSON.stringify(platform) },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: "./src/test/setup.ts",
    // Process real stylesheets instead of stubbing them to an empty
    // module, so a test that imports one is asserting against the CSS this
    // project actually ships. It is off by default in vitest, and that
    // default hid a real bug: components.css set `display` on the field
    // help pop-up, which beats the user-agent [hidden] rule outright, so
    // every pop-up in a real browser was permanently open while every
    // jsdom visibility assertion still passed. Only the test files that
    // import a stylesheet pay for this.
    css: true,
    // e2e/ used to hold the Playwright suite, and this exclusion kept
    // `vitest run` from collecting its specs. The suite left in #158: it
    // is Suite B of spdrman/rclone-manager-tests now, and rclone-manager's
    // own gate runs it from there on every commit (#197). The pattern
    // stays because nothing costs less than an exclusion for a directory
    // that does not exist, and because it is the one line that would have
    // to come back the day anyone reintroduces browser specs here.
    exclude: ["e2e/**/*.spec.ts", "node_modules/**", "dist/**"]
  }
});
