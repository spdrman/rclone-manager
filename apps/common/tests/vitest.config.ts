import { defineConfig } from "vitest/config";
import { resolve } from "node:path";

// This suite exercises apps/<provider>/frontend/platform.ts modules, which
// import ui/shared code through the same "@shared" alias ui/shared itself
// uses. Kept identical here so those files behave the same way they do
// when a real provider build pulls them in.
export default defineConfig({
  resolve: {
    alias: {
      "@shared": resolve(__dirname, "../../../ui/shared/src")
    }
  },
  test: {
    // A local-account provider's getAuthContext() touches window.fetch;
    // ugosBridge's touches window.ugos. jsdom, not node, matches how
    // ui/shared's own vitest config ran this suite before it moved here.
    environment: "jsdom"
  }
});
