import { afterEach, describe, expect, it } from "vitest";
import { graph, resetGraphForTests } from "./graph";
import { operationsNode } from "./appNodes";
import type { Operation } from "@shared/types/operation";

const OPERATION: Operation = {
  id: "op_test_1",
  setId: "set_test",
  setName: "Production PostgreSQL",
  kind: "transfer",
  stage: "transferring",
  label: "Transferring backup",
  percent: 42,
  nonDestructive: false,
  startedAt: "2026-08-29T00:00:00+02:00"
};

/** B2.1 — operationsNode is the graph-backed replacement for every page's own
 *  `useAsync(() => api.listOperations())`. Its behavior is the plain
 *  ResourceState<T> resource-node contract (see resource.test.ts for the
 *  generic mechanics); this file only proves the specific node this issue
 *  adds exists, is readable/committable like its siblings, and is wired
 *  into resetGraphForTests() so component tests can isolate against it. */
describe("operationsNode", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("starts unresolved, like every resource node", () => {
    expect(graph.read(operationsNode)).toEqual({ data: null, error: null, loading: true });
  });

  it("is committed and read like any other resource node", () => {
    graph.commit("test/seed-operations", (tx) =>
      tx.set(operationsNode, { data: [OPERATION], error: null, loading: false })
    );

    expect(graph.read(operationsNode).data).toEqual([OPERATION]);
    expect(graph.read(operationsNode).loading).toBe(false);
  });

  it("resets back to unresolved via resetGraphForTests, so one test's committed operations cannot leak into the next", () => {
    graph.commit("test/seed-operations", (tx) =>
      tx.set(operationsNode, { data: [OPERATION], error: null, loading: false })
    );

    resetGraphForTests();

    expect(graph.read(operationsNode)).toEqual({ data: null, error: null, loading: true });
  });
});
