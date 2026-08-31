import js from "@eslint/js";
import tseslint from "typescript-eslint";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";

// Real ESLint, alongside the existing `npm run lint` (tsc --noEmit,
// misleadingly named but left as-is to avoid an unrelated rename — see
// package.json's `eslint` script for this one instead). tsc catches type
// errors; this catches everything tsc structurally cannot: React hook
// rule violations (the exact class of bug the causl migration was
// specifically written to avoid — see src/state/useCausl.ts), unused
// code, and Fast Refresh boundary violations Vite's dev server depends on.
export default tseslint.config(
  // dist-bundles/ is `npm run build:bundles`'s output, one built bundle
  // per provider (issue #180). Same kind of artifact as dist/ and ignored
  // for the same reason: linting minified vendor output produces a
  // thousand errors about code nobody in this repository wrote, which is
  // how a lint step stops being run at all.
  { ignores: ["dist/**", "dist-bundles/**", "coverage/**", "playwright-report/**", "test-results/**"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ["**/*.{ts,tsx}"],
    plugins: {
      "react-hooks": reactHooks,
      "react-refresh": reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      "react-refresh/only-export-components": ["warn", { allowConstantExport: true }],
      // no-unused-vars: TS's own noUnused* compiler options already cover
      // this project-wide (tsconfig.json), so the ESLint duplicate is
      // redundant noise on top of a tsc error that already blocks `lint`.
      "@typescript-eslint/no-unused-vars": "off",
    },
  },
  {
    files: ["**/*.test.{ts,tsx}", "e2e/**/*.ts", "src/test/**/*.{ts,tsx}"],
    rules: {
      // Test files legitimately export helpers/fixtures alongside
      // components (e.g. src/test/bridges.ts-style fixtures); Fast
      // Refresh only matters for files Vite's dev server actually serves.
      "react-refresh/only-export-components": "off",
    },
  },
  {
    // Playwright specs/fixtures, not React component code. Playwright's
    // own fixture API has a callback parameter literally named `use`
    // (test.extend({ bm: async ({ page }, use) => { await use(...) } })),
    // which collides with the react-hooks plugin's name-based heuristic
    // for detecting React's use() hook — a false positive, not a real
    // rules-of-hooks violation, since nothing under e2e/ is a component.
    files: ["e2e/**/*.ts"],
    rules: {
      "react-hooks/rules-of-hooks": "off",
    },
  },
  {
    // A Node CLI script (scripts/e2e-all-providers.mjs), not browser code —
    // needs Node's globals, not the DOM ones the rest of this config
    // implicitly assumes.
    files: ["scripts/**/*.mjs"],
    languageOptions: {
      globals: { process: "readonly", console: "readonly" },
    },
  },
);
