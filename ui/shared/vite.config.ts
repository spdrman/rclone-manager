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
    // e2e/ holds Playwright specs. Without this, `vitest run` collects
    // them and every one fails with "Playwright Test did not expect
    // test.describe() to be called here", which is what made 13 of the
    // 16 test files fail on a clean checkout.
    exclude: ["e2e/**", "node_modules/**", "dist/**"]
  }
});
