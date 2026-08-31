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
