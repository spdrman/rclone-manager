import { createCausl } from "@causlts/core";
import { createCauslHook } from "./useCausl";

/**
 * The one causl graph for the whole app (EPIC B — "causl-ts, from the
 * ground up"). Created once, here, at module load. Every piece of
 * cross-component state in ui/shared lives on this graph as an `input` or
 * a `derived` node — nothing else in this tree should reach for
 * `createContext` + `useState` to hold state another component needs to
 * read; that pattern is exactly what this migration replaces (see
 * platformNodes.ts and appNodes.ts).
 *
 * @causlts/core is the plain TS-only reference engine (no WASM, no
 * preload step): `createCausl()` is synchronous and returns a ready graph.
 */
export const graph = createCausl();

/** The one shared binding hook the rest of ui/shared reads through. */
export const useCausl = createCauslHook(graph);
