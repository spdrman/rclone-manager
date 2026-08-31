import { afterEach, describe, expect, it } from "vitest";
import { graph, resetGraphForTests } from "./graph";
import {
  commitRetentionRevisions,
  operationsNode,
  readOnlyNode,
  retentionPlanNode,
  retentionPlanStaleNode,
  retentionRevisionsNode,
  versionNode
} from "./appNodes";
import type { Operation, VersionInfo } from "@shared/types/operation";
import type { RetentionPlan } from "@shared/types/backup";

const OPERATION: Operation = {
  id: "op_test_1",
  setId: "set_test",
  setName: "Production PostgreSQL",
  kind: "transfer",
  label: "Transferring backup",
  status: "running",
  progress: {
    observedAt: "2026-08-29T00:00:05+02:00",
    sequence: 7,
    stage: "transferring",
    backupSetId: "set_test",
    backupSetsDone: 0,
    backupSetsTotal: 1,
    artifact: "nightly.dump",
    artifactsDone: 0,
    bytesDone: 42,
    bytesTotal: 100
  },
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

const PLAN: RetentionPlan = {
  planId: "retplan_test_1",
  backupSetId: "production/postgres-primary",
  inventoryRevision: "inv_1",
  configRevision: "cfg_1",
  expiresAt: "2026-08-29T06:09:48+02:00",
  keepCount: 1,
  deleteCount: 1,
  reclaimBytes: 1024,
  verdicts: [
    { artifact: "a.dump", action: "KEEP", reason: "GFS daily tier", tiers: [{ tier: "DAILY", selectedBy: "BOTH" }] },
    { artifact: "b.dump", action: "DELETE", reason: "Not selected by current retention policy", tiers: [] }
  ]
};

/** B3.1 (#96) — issue's own required TDD case: "is this plan stale"
 *  becomes a derived() node comparing the plan's captured
 *  inventory_revision/config_revision against the current committed
 *  values, not a boolean read off the wire (RetentionPlan carries no
 *  `stale` field at all — see its own doc, types/backup.ts). */
describe("retentionPlanStaleNode", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("is false before any plan has been read (nothing to compare)", () => {
    expect(graph.read(retentionPlanStaleNode)).toBe(false);
  });

  it("is false once a plan is read and its own revisions are committed as the baseline", () => {
    graph.commit("test/seed-plan", (tx) => tx.set(retentionPlanNode, { data: PLAN, error: null, loading: false }));
    commitRetentionRevisions({ inventoryRevision: PLAN.inventoryRevision, configRevision: PLAN.configRevision });

    expect(graph.read(retentionPlanStaleNode)).toBe(false);
  });

  it("flips true the moment the graph learns of an inventory change, from that commit alone — no re-fetch", () => {
    graph.commit("test/seed-plan", (tx) => tx.set(retentionPlanNode, { data: PLAN, error: null, loading: false }));
    commitRetentionRevisions({ inventoryRevision: PLAN.inventoryRevision, configRevision: PLAN.configRevision });
    expect(graph.read(retentionPlanStaleNode)).toBe(false);

    // GIVEN plan P was previewed, WHEN the backup set's inventory changes —
    // simulated here as a direct graph commit, standing in for whatever
    // later learns of the real change (a re-preview, a live poll/push).
    commitRetentionRevisions({ inventoryRevision: "inv_2", configRevision: PLAN.configRevision });

    expect(graph.read(retentionPlanStaleNode)).toBe(true);
  });

  it("also flips true on a config revision change alone, independent of inventory", () => {
    graph.commit("test/seed-plan", (tx) => tx.set(retentionPlanNode, { data: PLAN, error: null, loading: false }));
    commitRetentionRevisions({ inventoryRevision: PLAN.inventoryRevision, configRevision: PLAN.configRevision });

    commitRetentionRevisions({ inventoryRevision: PLAN.inventoryRevision, configRevision: "cfg_2" });

    expect(graph.read(retentionPlanStaleNode)).toBe(true);
  });

  it("resets retentionRevisionsNode back to null via resetGraphForTests", () => {
    commitRetentionRevisions({ inventoryRevision: "inv_1", configRevision: "cfg_1" });
    expect(graph.read(retentionRevisionsNode)).not.toBeNull();

    resetGraphForTests();

    expect(graph.read(retentionRevisionsNode)).toBeNull();
  });
});

const COMPATIBLE_VERSION: VersionInfo = {
  api: "v1", service: "1.3.0", buildCommit: "9f4c1ab", goVersion: "go1.27.0",
  engine: "1.68.2", configRevision: "cfg_9f4c1ab", ready: true, compatible: true
};

const INCOMPATIBLE_VERSION: VersionInfo = {
  ...COMPATIBLE_VERSION, service: "1.2.0", api: "v0", compatible: false
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
