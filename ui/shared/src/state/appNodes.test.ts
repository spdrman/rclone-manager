import { afterEach, describe, expect, it } from "vitest";
import { graph, resetGraphForTests } from "./graph";
import { operationsNode, readOnlyNode, versionNode } from "./appNodes";
import type { Operation, VersionInfo } from "@shared/types/operation";

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

const COMPATIBLE_VERSION: VersionInfo = {
  ui: "1.3.0", service: "1.3.0", core: "1.3.0", rclone: "1.68.2",
  schema: 41, architecture: "linux/arm64", buildCommit: "9f4c1ab", compatible: true
};

const INCOMPATIBLE_VERSION: VersionInfo = {
  ...COMPATIBLE_VERSION, service: "1.2.0", core: "1.2.0", compatible: false
};

/** B2.6 (#103) — readOnlyNode is `derived()` purely from versionNode (see
 *  appNodes.ts). SettingsPage displays versionNode.data directly, and
 *  App.tsx's `readOnly` flag IS this node — both are therefore, structurally,
 *  reads of the same commit, not two independent `getVersion()` answers that
 *  happen to usually agree. This is the pure-logic half of that guarantee;
 *  version-settings-activity.test.tsx proves it at the component level. */
describe("readOnlyNode: derived purely from versionNode, never its own fetch", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("is false before any version has resolved (never a premature read-only lock)", () => {
    expect(graph.read(readOnlyNode)).toBe(false);
  });

  it("flips to true in the SAME commit that makes versionNode incompatible — no separate step can observe one without the other", () => {
    const commit = graph.commit("test/seed-version-incompatible", (tx) =>
      tx.set(versionNode, { data: INCOMPATIBLE_VERSION, error: null, loading: false })
    );

    expect(graph.read(readOnlyNode)).toBe(true);
    // readOnlyNode is one of the nodes this exact commit changed — proving it
    // recomputed AT this commit, not lazily on some later read.
    expect(commit.changedNodes).toContain(readOnlyNode.id);
  });

  it("returns to false when versionNode is committed back to a compatible version", () => {
    graph.commit("test/seed-version-incompatible", (tx) =>
      tx.set(versionNode, { data: INCOMPATIBLE_VERSION, error: null, loading: false })
    );
    expect(graph.read(readOnlyNode)).toBe(true);

    graph.commit("test/seed-version-compatible", (tx) =>
      tx.set(versionNode, { data: COMPATIBLE_VERSION, error: null, loading: false })
    );
    expect(graph.read(readOnlyNode)).toBe(false);
  });
});
