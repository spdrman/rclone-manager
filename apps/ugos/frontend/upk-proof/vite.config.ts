import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Deliberately standalone: no @shared alias, no @platform-entry alias, nothing tying
// this into ui/shared's provider-neutral build (see apps/ugos/docs/upk-proof-procedure.md
// for why). `base: "./"` because the backend serves this bundle from a bare filesystem
// root, not from a known absolute path.
export default defineConfig({
  base: "./",
  plugins: [react()],
  build: {
    outDir: "dist",
    emptyOutDir: true
  }
});
