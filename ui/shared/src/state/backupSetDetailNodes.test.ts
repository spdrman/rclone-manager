import { afterEach, describe, expect, it } from "vitest";
import { graph, resetGraphForTests } from "./graph";
import { quarantineNode } from "./appNodes";
import {
  captureSetEditSnapshot,
  currentSetActivityNode,
  currentSetDetailNode,
  isSetEditStale
} from "./backupSetDetailNodes";
import type { BackupSet } from "@shared/types/backup";
import type { ActivityEvent } from "@shared/types/operation";

const RETENTION = {
  daily: 7,
  weekly: 13,
  monthly: 12,
  timezone: "Europe/Berlin",
  weekStartsOn: "monday" as const,
  protectLastKnownGood: true
};

const SET_V1: BackupSet = {
  id: "set_test", source: "production", set: "postgres-primary",
  name: "Production PostgreSQL",
  host: "prod-db-01.internal", port: 22, username: "backup-agent",
  remoteFolder: "/backups/postgresql/", includePatterns: ["*.dump.zst"],
  excludePatterns: ["*.tmp"], completionMethod: "completion-marker",
  destination: "/data/backups/production/postgres/", retention: RETENTION,
  validations: ["transfer", "checksum"],
  state: "healthy",
  stateNote: "Verified nightly dump.",
  enabled: true, halted: false,
  newestKnownGoodAt: "2026-08-29T02:01:01+02:00",
  lastRunAt: "2026-08-29T02:01:01+02:00",
  lastValidation: "passed", expectedIntervalHours: 24,
  retainedCount: 32, retainedBytes: 421,
  hostFingerprint: "SHA256:9kQ2mVv+Rt4hLc0pXeN1sJfB7yUwZaGdQ8oT3iKrEuM",
  fingerprintTrustedAt: "2026-08-02T10:14:00+02:00"
};

/** Someone else's commit, landed after the form opened against SET_V1 —
 *  same id, a real field changed. */
const SET_V2: BackupSet = { ...SET_V1, name: "Production PostgreSQL (renamed)" };

const ACTIVITY: ActivityEvent[] = [
  {
    id: "evt_test_1", at: "2026-08-29T02:01:01+02:00", type: "backup-committed",
    severity: "ok", setId: "set_test", setName: "Production PostgreSQL",
    text: "Backup completed", detail: "", correlationId: "cid_test"
  }
];

function seedSet(set: BackupSet) {
  graph.commit("test/seed-set-detail", (tx) =>
    tx.set(currentSetDetailNode, { data: set, error: null, loading: false })
  );
}

/** B2.2 (#97) — currentSetDetailNode/currentSetActivityNode are the
 *  graph-backed replacement for BackupSetDetailPage's own
 *  useAsync(() => api.getSet(setId)) / useAsync(() => api.listActivity()).
 *  Their generic behavior is the shared ResourceState<T> contract (see
 *  resource.test.ts / appNodes.test.ts); this file only proves the two
 *  specific nodes this issue adds exist and are wired into
 *  resetGraphForTests(), plus the stale-edit-rejection logic that reading
 *  a BackupSet off the graph (rather than a plain useAsync `data` field)
 *  makes possible. */
describe("currentSetDetailNode / currentSetActivityNode", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("start unresolved, like every resource node", () => {
    expect(graph.read(currentSetDetailNode)).toEqual({ data: null, error: null, loading: true });
    expect(graph.read(currentSetActivityNode)).toEqual({ data: null, error: null, loading: true });
  });

  it("are committed and read like any other resource node", () => {
    seedSet(SET_V1);
    graph.commit("test/seed-activity", (tx) =>
      tx.set(currentSetActivityNode, { data: ACTIVITY, error: null, loading: false })
    );

    expect(graph.read(currentSetDetailNode).data).toEqual(SET_V1);
    expect(graph.read(currentSetActivityNode).data).toEqual(ACTIVITY);
  });

  it("reset back to unresolved via resetGraphForTests, so one test's committed set cannot leak into the next", () => {
    seedSet(SET_V1);
    resetGraphForTests();

    expect(graph.read(currentSetDetailNode)).toEqual({ data: null, error: null, loading: true });
  });
});

describe("stale-edit rejection (#97 — B2.2 acceptance: 'stale edits are rejected')", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("captureSetEditSnapshot returns null before any set has loaded onto the graph", () => {
    expect(captureSetEditSnapshot()).toBeNull();
  });

  it("a snapshot captured right after the set loads is not stale", () => {
    seedSet(SET_V1);

    const snapshot = captureSetEditSnapshot();

    expect(snapshot).not.toBeNull();
    expect(isSetEditStale(snapshot!)).toBe(false);
  });

  // The GIVEN/WHEN/THEN from the issue: a set-edit form opened against the
  // set as committed at time T; another commit updates that SAME set
  // before the form submits; the submit is rejected as stale rather than
  // silently overwriting. Driven off the graph's own per-node commit
  // counter (graph.stats().nodeVersion), never a bespoke revision field
  // bolted onto BackupSet.
  it("GIVEN a set-edit form opened at time T, WHEN another commit updates that same set before submit, THEN it is rejected as stale", () => {
    seedSet(SET_V1);
    const snapshot = captureSetEditSnapshot()!;

    // Someone else's commit lands before this form submits.
    seedSet(SET_V2);

    expect(isSetEditStale(snapshot)).toBe(true);
  });

  it("does not silently let a stale snapshot's captured set differ from what is now on the graph", () => {
    seedSet(SET_V1);
    const snapshot = captureSetEditSnapshot()!;
    seedSet(SET_V2);

    expect(snapshot.set.name).toBe(SET_V1.name);
    expect(graph.read(currentSetDetailNode).data?.name).toBe(SET_V2.name);
  });

  // Negative case, same shape as the destructive-safety invariant (§4C):
  // a commit to a DIFFERENT node must never be mistaken for "that same
  // set changed" — otherwise every backup set's edit form would spuriously
  // reject as stale whenever anything else on the app changed.
  it("does not flag the edit as stale when an unrelated node commits, not this one", () => {
    seedSet(SET_V1);
    const snapshot = captureSetEditSnapshot()!;

    graph.commit("test/seed-quarantine", (tx) =>
      tx.set(quarantineNode, { data: [], error: null, loading: false })
    );

    expect(isSetEditStale(snapshot)).toBe(false);
  });

  it("treats a snapshot as stale once the graph has no set loaded any more (e.g. a failed refetch)", () => {
    seedSet(SET_V1);
    const snapshot = captureSetEditSnapshot()!;

    graph.commit("test/set-detail-failed", (tx) =>
      tx.set(currentSetDetailNode, { data: null, error: { code: "unknown", message: "gone", correlationId: "cid" }, loading: false })
    );

    expect(isSetEditStale(snapshot)).toBe(true);
  });
});
