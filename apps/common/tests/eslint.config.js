import js from "@eslint/js";
import tseslint from "typescript-eslint";

// This package only consumes provider bridges/fixtures, it doesn't define
// React components, so no react-hooks/react-refresh plugins here — see
// ui/shared/eslint.config.js for the fuller frontend config.
export default tseslint.config(
  { ignores: ["coverage/**"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
);
