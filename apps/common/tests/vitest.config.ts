import { defineConfig } from "vitest/config";
import { resolve } from "node:path";

// This suite exercises apps/<provider>/frontend/platform.ts modules, which
// import ui/shared code through the same "@shared" alias ui/shared itself
// uses. Kept identical here so those files behave the same way they do
// when a real provider build pulls them in.
export default defineConfig({
  resolve: {
    alias: {
      "@shared": resolve(__dirname, "../../../ui/shared/src"),
      // Forces every "react"/"react-dom" import - including from
      // ui/shared/src files pulled in through the @shared alias above -
      // to resolve to THIS package's own copy, not whatever ui/shared's
      // own node_modules happens to have installed. Without this, a test
      // that renders a ui/shared component (PlatformProvider, say) ends
      // up with two separate React module instances in the same test:
      // this package's own (used by @testing-library/react's render())
      // and ui/shared's (used by the component's own `useRef`/etc. calls,
      // resolved relative to where that source file lives) - two
      // instances means two separate hooks-dispatcher singletons, which
      // fails with "Cannot read properties of null (reading 'useRef')"
      // the instant a component from one instance is rendered by the
      // other's ReactDOM.
      react: resolve(__dirname, "node_modules/react"),
      "react-dom": resolve(__dirname, "node_modules/react-dom")
    }
  },
  test: {
    // A local-account provider's getAuthContext() touches window.fetch;
    // ugosBridge's touches window.ugos. jsdom, not node, matches how
    // ui/shared's own vitest config ran this suite before it moved here.
    environment: "jsdom"
  }
});
