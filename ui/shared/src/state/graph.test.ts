/**
 * The reset that every later component test leans on, proved before
 * anything leans on it.
 *
 * Small, and deliberately so: the mechanism is three lines, and what needs
 * proving is that it reaches derived nodes and more than one input, since
 * a reset that quietly covered only what the test author remembered would
 * fail as a silent leak between unrelated suites rather than as a red
 * test here.
 */
import { describe, expect, it } from "vitest";
import { graph, registerInput, resetGraphForTests } from "./graph";

/** Proves the isolation utility Phase 2's component tests will need
 *  (mandatory-review item 12): the app's graph is a true singleton, and
 *  the causl engine itself has no in-place reset, so this is the only
 *  mechanism available to keep one test's committed state from leaking
 *  into the next without a throwaway `createCausl()` graph (which is not
 *  an option for a test that renders against real nodes like bridgeNode
 *  or healthNode). */
describe("resetGraphForTests", () => {
  it("commits a registerInput node back to the value it was created with", () => {
    const node = registerInput("test.graph-reset.counter", 0);
    graph.commit("bump", (tx) => tx.set(node, 5));
    expect(graph.read(node)).toBe(5);

    resetGraphForTests();

    expect(graph.read(node)).toBe(0);
  });

  it("recomputes a derived node from the reset input, with no separate handling needed", () => {
    const node = registerInput("test.graph-reset.base", 1);
    const doubled = graph.derived("test.graph-reset.doubled", (get) => get(node) * 2);
    graph.commit("bump", (tx) => tx.set(node, 10));
    expect(graph.read(doubled)).toBe(20);

    resetGraphForTests();

    expect(graph.read(doubled)).toBe(2);
  });

  it("resets more than one registered input in the same call", () => {
    const a = registerInput("test.graph-reset.a", "a-initial");
    const b = registerInput("test.graph-reset.b", "b-initial");
    graph.commit("bump", (tx) => {
      tx.set(a, "a-changed");
      tx.set(b, "b-changed");
    });

    resetGraphForTests();

    expect(graph.read(a)).toBe("a-initial");
    expect(graph.read(b)).toBe("b-initial");
  });
});
