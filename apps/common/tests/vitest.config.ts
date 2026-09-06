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
      "react-dom": resolve(__dirname, "node_modules/react-dom"),
      // Same reasoning as react/react-dom above, for a different failure
      // mode: ui/shared/src/state/graph.ts imports the bare specifier
      // "@causlts/core", and Vite resolves a bare specifier relative to
      // the IMPORTING FILE's own location on disk (walking up
      // ui/shared/src/state -> ui/shared -> ui/shared/node_modules),
      // never relative to this config's root - adding @causlts/core to
      // THIS package's own package.json/node_modules alone does nothing
      // for that resolution. Declaring it in this package's own
      // package.json (see that file) still matters: it's what makes
      // `node_modules/@causlts/core` exist here at all for this alias to
      // point at, and what makes `npm ci` reproducible without relying
      // on ui/shared happening to have been installed first.
      "@causlts/core": resolve(__dirname, "node_modules/@causlts/core")
    }
  },
  test: {
    // A local-account provider's getAuthContext() touches window.fetch;
    // ugosBridge's touches window.ugos. jsdom, not node, matches how
    // ui/shared's own vitest config ran this suite before it moved here.
    environment: "jsdom"
  }
});
