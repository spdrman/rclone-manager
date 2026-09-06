/**
 * The bundle entry, and the only file in the shared tree that knows a
 * provider shell exists.
 *
 * `@platform-entry` is an alias the build resolves per provider (see
 * scripts/build-bundles.mjs), so the seven bundles differ from each other
 * by what this one import points at and by nothing else in this directory.
 */
import bootstrap from "@platform-entry";

// Every provider shell default-exports a bootstrap function. The shared entry
// knows nothing about which one it is.
bootstrap(document.getElementById("root")!);
