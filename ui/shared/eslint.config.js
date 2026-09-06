// The ESLint rules for the whole ui/shared workspace, which is a wider reach
// than it sounds and is the point of the file.
//
// TypeScript already runs over this project, so a second checker has to earn
// its place by catching what the first structurally cannot. That is the line
// this config is drawn along: React's hook rules, Fast Refresh boundaries the
// dev server depends on, and code nothing uses. None of those are type errors,
// so no amount of tsc finds them.
//
// The reach matters as much as the rules. tsc only sees the files
// tsconfig.json includes, which is src, while `eslint .` sweeps the workspace,
// so this is the only check that reads the build configuration beside it. The
// no-unused-vars rule below is here because of exactly that gap, and it says
// so at some length: the belief that tsc already covered it was tested and
// turned out to be wrong.
//
// Everything else is an exception, and each one names the thing it is making
// room for rather than just switching a rule off.

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
  { ignores: ["dist/**", "dist-bundles/**", "coverage/**"] },
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
      // no-unused-vars: on here, and deliberately not delegated to tsc's
      // noUnusedLocals/noUnusedParameters. Those two only ever see the
      // files tsconfig.json's "include" lists, which is `src` (plus
      // apps/*/frontend through tsconfig.providers.json), whereas
      // `eslint .` sweeps the whole workspace. So vite.config.ts, and
      // anything anyone adds outside src/ later, is covered by this rule
      // and by nothing else. It was off until issue #219 on the claim
      // that tsc "already covers this project-wide"; it did not, and an
      // unused binding planted in vite.config.ts passed both `npm run
      // lint` and `npm run eslint`. Inside src/ the two checks do
      // genuinely overlap, and that duplicate is the price of a rule
      // whose reach does not depend on an include list kept in a
      // different file.
      "@typescript-eslint/no-unused-vars": "error",
    },
  },
  {
    files: ["**/*.test.{ts,tsx}", "src/test/**/*.{ts,tsx}"],
    rules: {
      // Test files legitimately export helpers/fixtures alongside
      // components (e.g. src/test/bridges.ts-style fixtures); Fast
      // Refresh only matters for files Vite's dev server actually serves.
      "react-refresh/only-export-components": "off",
    },
  },
  {
    // A Node CLI script (scripts/build-bundles.mjs), not browser code —
    // needs Node's globals, not the DOM ones the rest of this config
    // implicitly assumes.
    files: ["scripts/**/*.mjs"],
    languageOptions: {
      globals: { process: "readonly", console: "readonly" },
    },
  },
);
