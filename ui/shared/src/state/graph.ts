/**
 * The app's one reactive graph, and the two pieces of machinery that keep
 * it usable from a test.
 *
 * A module-level singleton is an awkward thing to test, and everything
 * here except `graph` itself is about paying that bill honestly rather
 * than pretending it is not owed. `registerInput` exists so a reset can
 * know what to reset, and `resetGraphForTests` exists because the engine
 * offers no reset of its own and a fresh graph would not reach the nodes
 * that were bound to the old one at import time. Both notes below spell
 * that out, because the obvious alternative (build a new graph per test)
 * looks like it should work and does not.
 */
import { createCausl } from "@causlts/core";
import type { InputNode } from "@causlts/core";
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
 *
 * Node-id convention: `graph.input`/`graph.derived` throw on a repeated
 * id, and node ids are plain top-level string literals — a collision
 * crashes the whole app at import time with no indication of which two
 * modules collided. The two prefixes that exist today are `app.*`
 * (App.tsx's own state, appNodes.ts) and `platform.*` (PlatformContext's
 * state, platformNodes.ts). Phase 2 adds six more provider-specific
 * frontends registering nodes onto this same shared singleton: every node
 * a provider app adds MUST be prefixed `<providerId>.*` (e.g. `ugos.*`,
 * `synology.*`, matching ui/shared's PlatformId —
 * ui/shared/src/types/platform.ts), so a collision is traceable to the
 * two colliding providers from the id alone.
 */
export const graph = createCausl();

/** The one shared binding hook the rest of ui/shared reads through. */
export const useCausl = createCauslHook(graph);

/**
 * Every input node registered through `registerInput`, paired with the
 * value it was first created with. Backs `resetGraphForTests()` only —
 * nothing in production code ever reads this list.
 */
const resettableInputs: { node: InputNode<unknown>; initial: unknown }[] = [];

/**
 * Registers an input node on the app's shared graph AND remembers its
 * initial value for `resetGraphForTests()`. Every input node meant to be
 * reset between tests — which, in practice, means every input node
 * created on this shared singleton (appNodes.ts, platformNodes.ts,
 * resource.ts) — MUST be created through this function rather than
 * calling `graph.input` directly, or `resetGraphForTests()` silently
 * skips it.
 */
export function registerInput<T>(id: string, initial: T): InputNode<T> {
  const node = graph.input<T>(id, initial);
  resettableInputs.push({ node: node as InputNode<unknown>, initial });
  return node;
}

/**
 * Test-only: commits every input node registered via `registerInput` back
 * to the value it was first created with, in one transaction. Derived
 * nodes need no separate treatment — they recompute automatically as pure
 * functions of the inputs above (appNodes.ts's countsNode/readOnlyNode,
 * platformNodes.ts's capabilityCopyNode).
 *
 * Why this exists, and why it works this way: the causl engine has no
 * in-place "reset" operation — `createCausl()` always returns a brand-new,
 * independent universe (see its own docs) — and building a fresh graph
 * here would not help anyway, since bridgeNode/healthNode/etc. are
 * module-level consts evaluated ONCE, at import time, against whatever
 * graph instance was live then; swapping this module's exported `graph`
 * binding afterward would not move them to the new instance. Resetting
 * values in place on the one real graph instance is therefore the only
 * isolation primitive available without restructuring how nodes are
 * declared (see graph.test.ts for the mechanics).
 *
 * Call this from an `afterEach` in any test that renders a component
 * against real graph-backed state (bridgeNode, healthNode, ...) instead
 * of a throwaway `createCausl()` graph, so one test's committed state
 * cannot leak into the next. Tests that only exercise the graph mechanics
 * themselves should keep building throwaway graphs instead (see
 * useCausl.test.tsx) — this helper is for Phase 2's component tests
 * against the real app singleton.
 */
export function resetGraphForTests(): void {
  graph.commit("test/reset", (tx) => {
    for (const { node, initial } of resettableInputs) {
      tx.set(node, initial);
    }
  });
}
