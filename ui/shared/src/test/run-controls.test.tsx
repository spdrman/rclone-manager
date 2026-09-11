/**
 * What happens when an operator presses a run button.
 *
 * Nothing anywhere asserted that before this file. The only test that
 * touched the control asserted it exists once and carries a tooltip, and
 * the dashboard's copy had no handler at all, so "pressing it does
 * nothing" was not a failing test, it was no test.
 *
 * Every case here is one of the four defects issue #597 found stacked on
 * top of each other, held apart deliberately: a press that does nothing,
 * a press whose refusal is swallowed, a press that sends an empty
 * configuration revision, and a press with no idempotency key. Each of
 * them looked identical from an operator's chair, which is why each of
 * them gets its own assertion rather than one end-to-end case that would
 * pass as soon as any one of them was fixed.
 *
 * The refusals are asserted by the TYPED CODE the service sent, never by
 * a status: 403 is both the destructive gate and a stale CSRF token, and
 * 409 covers three refusals an operator resolves in three different
 * places.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { DashboardPage } from "@shared/pages/DashboardPage";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { BackupdError } from "@shared/api/contracts";
import type { BackupdApi } from "@shared/api/contracts";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { setsNode, versionNode } from "@shared/state/appNodes";
import type { AsyncState } from "@shared/hooks/useAsync";
import type { BackupSet } from "@shared/types/backup";
import type { SystemHealth, VersionInfo } from "@shared/types/operation";
import { backupSetPath } from "@shared/utilities/routes";

const noop = () => {};

const VERSION: VersionInfo = {
  api: "v1",
  service: "1.3.0",
  buildCommit: "9f4c1ab",
  goVersion: "go1.27.0",
  engine: "1.68.2",
  configRevision: "cfg_9f4c1ab",
  ready: true,
  compatible: true
};

/** A health report with nothing wrong, so the page renders its normal
 *  body rather than the not-configured or error state. */
const HEALTH: SystemHealth = {
  generatedAt: "2026-08-30T10:00:00Z",
  serviceRunning: true,
  backupHealth: "healthy",
  backupHealthReason: "every set has a fresh known-good backup",
  newestVerifiedBackupAt: "2026-08-30T09:40:00Z",
  lastCompletedBackupAt: "2026-08-30T09:40:00Z",
  oldestSetFreshnessHours: 1,
  setsHealthy: 1,
  setsDegraded: 0,
  setsStale: 0,
  setsFailing: 0,
  quarantinedCount: 0,
  readOnlyRetainedCount: 0,
  storageFreeBytes: 1,
  storageTotalBytes: 2,
  storageState: "nominal",
  storageReadingsUnavailable: 0
};

function seedVersion(version: VersionInfo | null) {
  act(() => {
    graph.commit("test/seed-version", (tx) =>
      tx.set(versionNode, { data: version, error: null, loading: version === null })
    );
  });
}

async function renderDashboard(api: ReturnType<typeof createMockApi>, sets: BackupSet[]) {
  const health: AsyncState<SystemHealth> = { data: HEALTH, error: null, loading: false, reload: noop };
  const setsState: AsyncState<BackupSet[]> = { data: sets, error: null, loading: false, reload: noop };
  const rendered = render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <DashboardPage health={health} sets={setsState} readOnly={false} />
      </ApiProvider>
    </MemoryRouter>
  );
  // The page fetches activity and storage on mount; let those settle so a
  // press below is not racing an unrelated state update.
  await screen.findByRole("button", { name: "Run all enabled sets" });
  return rendered;
}

function press(name: string) {
  act(() => {
    screen.getByRole("button", { name }).click();
  });
}

describe("the dashboard's run control", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  // Defect 1. `<button className="btn" disabled={readOnly}>Run all due
  // sets</button>` was the whole of it: no onClick, on the copy an
  // operator presses first.
  it("submits a run cycle when it is pressed", async () => {
    // The signature is spelled out rather than inferred: `vi.fn(() =>
    // ...)` infers a zero-argument mock, and reading calls[0][1] off one
    // type-checks under `tsc --noEmit` and fails the real build.
    //
    // It goes on vi.fn's own type parameter rather than on a stub's
    // parameter list, because a parameter list is the one shape that
    // cannot say "two arguments, neither of which this stub looks at":
    // eslint's no-unused-vars is on for the whole workspace with no
    // argsIgnorePattern, so an unused `_key` is an error however it is
    // spelled. Taking the type from the contract is better than either,
    // since the mock now moves when BackupdApi does.
    const runCycle = vi.fn<BackupdApi["runCycle"]>(() => Promise.resolve());
    const api = { ...createMockApi(), runCycle };
    const sets = await createMockApi().listSets();
    seedVersion(VERSION);
    await renderDashboard(api, sets);

    press("Run all enabled sets");

    await waitFor(() => expect(runCycle).toHaveBeenCalledTimes(1));
    // The revision the SCREEN is showing, not an empty string and not one
    // read fresh at submit time.
    expect(runCycle.mock.calls[0][0]).toBe("cfg_9f4c1ab");
    // Defect 4: an idempotency key, which no build of this client has
    // ever sent, because `post` had nowhere to put a header.
    expect(typeof runCycle.mock.calls[0][1]).toBe("string");
    expect(runCycle.mock.calls[0][1].length).toBeGreaterThan(15);
  });

  // Defect 2, and the sharpest one: every refusal was an unhandled
  // rejection, so a 403 was pixel-identical to the button above having no
  // handler at all.
  it("renders the gate's refusal by its typed code rather than swallowing it", async () => {
    const runCycle = vi.fn(() =>
      Promise.reject(
        new BackupdError({
          code: "DESTRUCTIVE_OPERATIONS_DISABLED",
          message: "destructive operations are disabled until the trusted-proxy authentication gate has been verified",
          correlationId: "cid_gate_1"
        })
      )
    );
    const api = { ...createMockApi(), runCycle };
    const sets = await createMockApi().listSets();
    seedVersion(VERSION);
    await renderDashboard(api, sets);

    press("Run all enabled sets");

    await screen.findByText("This deployment will not start a backup run.");
    // The gate's refusal is not transient and retrying will not help, so
    // the one thing that still works has to be on screen: the command
    // this button is equivalent to, copy-pasteable exactly as shown. The
    // pre block specifically, not just the words somewhere on the page,
    // because the remediation sentence names the command too and a match
    // on that would pass with nothing to copy.
    const command = document.querySelector("pre");
    expect(command?.textContent).toContain("backupd run");
    // The id somebody copies into a support message, behind the sentence
    // rather than inside it.
    expect(screen.getByText("cid_gate_1")).toBeTruthy();
  });

  // A different refusal has to read differently. Without this, a single
  // hardcoded "could not run" sentence would satisfy the case above.
  it("tells a run already in progress apart from the gate", async () => {
    const runCycle = vi.fn(() =>
      Promise.reject(
        new BackupdError({
          code: "OPERATION_ALREADY_RUNNING",
          message: "rejected: another run is already in progress",
          correlationId: "cid_busy_1"
        })
      )
    );
    const api = { ...createMockApi(), runCycle };
    const sets = await createMockApi().listSets();
    seedVersion(VERSION);
    await renderDashboard(api, sets);

    press("Run all enabled sets");

    await screen.findByText("A backup run is already in progress.");
    expect(screen.queryByText("This deployment will not start a backup run.")).toBeNull();
  });

  // Defect 3. `version.data?.configRevision ?? ""` sent an empty revision
  // whenever the press beat the version fetch, and the service refuses an
  // empty one. Refusing here, out loud, is the difference between "this
  // page is not ready" and a 400 nobody rendered.
  it("refuses to submit before the configuration revision has loaded, and says why", async () => {
    const runCycle = vi.fn(() => Promise.resolve());
    const api = { ...createMockApi(), runCycle };
    const sets = await createMockApi().listSets();
    seedVersion(null);
    await renderDashboard(api, sets);

    press("Run all enabled sets");

    await screen.findByText("This page has not finished loading.");
    expect(runCycle).not.toHaveBeenCalled();
  });

  // The whole point of the header: one key per logical submission, reused
  // when the operator asks for the same thing again after a refusal. A
  // fresh key per attempt would turn a dropped response into a second
  // backup run, which is worse than sending no key at all.
  it("reuses one idempotency key across a retry, and mints a new one after a success", async () => {
    const keys: string[] = [];
    let refuse = true;
    const runCycle = vi.fn((_revision: string, key: string) => {
      keys.push(key);
      return refuse
        ? Promise.reject(
            new BackupdError({ code: "INTERNAL", message: "boom", correlationId: "cid_1" })
          )
        : Promise.resolve();
    });
    const api = { ...createMockApi(), runCycle };
    const sets = await createMockApi().listSets();
    seedVersion(VERSION);
    await renderDashboard(api, sets);

    press("Run all enabled sets");
    await waitFor(() => expect(keys).toHaveLength(1));

    // Same submission, asked for again.
    press("Run all enabled sets");
    await waitFor(() => expect(keys).toHaveLength(2));
    expect(keys[1]).toBe(keys[0]);

    // It goes through this time, which ENDS that submission.
    refuse = false;
    press("Run all enabled sets");
    await waitFor(() => expect(keys).toHaveLength(3));
    expect(keys[2]).toBe(keys[0]);

    // A new ask is a new submission and must not replay the finished one:
    // the service would answer the replay with the old operation and the
    // second run an operator asked for would never happen.
    press("Run all enabled sets");
    await waitFor(() => expect(keys).toHaveLength(4));
    expect(keys[3]).not.toBe(keys[0]);
  });
});

describe("the per-set run control", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  async function renderDetail(api: ReturnType<typeof createMockApi>, set: BackupSet) {
    const rendered = render(
      <MemoryRouter initialEntries={[backupSetPath(set.source, set.set)]}>
        <ApiProvider api={api}>
          <Routes>
            <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={false} />} />
          </Routes>
        </ApiProvider>
      </MemoryRouter>
    );
    await screen.findByRole("button", { name: "Run this backup set" });
    return rendered;
  }

  // The control this page has never had. The engine half has existed
  // since FR-1 behind `backupd fetch --backup-set`; what was
  // missing was a way to reach it from a browser.
  it("runs exactly the set on screen, by its full source/backup-set id", async () => {
    const runBackupSet = vi.fn<BackupdApi["runBackupSet"]>(() => Promise.resolve());
    const sets = await createMockApi().listSets();
    const target = sets.find((s) => s.enabled) ?? sets[0];
    const api = { ...createMockApi(), runBackupSet };
    seedVersion(VERSION);
    await renderDetail(api, target);

    press("Run this backup set");

    await waitFor(() => expect(runBackupSet).toHaveBeenCalledTimes(1));
    expect(runBackupSet.mock.calls[0][0]).toBe(target.id);
    expect(runBackupSet.mock.calls[0][1]).toBe("cfg_9f4c1ab");
    expect(runBackupSet.mock.calls[0][2].length).toBeGreaterThan(15);
  });

  // The refusal that only exists once a request names one set, and the
  // reason it is not folded into OPERATION_ALREADY_RUNNING: one says wait
  // for the run in flight, the other says leave edit mode, and an
  // operator sent to the wrong one waits forever.
  it("says a set held for editing is being edited, not that a run is in progress", async () => {
    const runBackupSet = vi.fn(() =>
      Promise.reject(
        new BackupdError({
          code: "BACKUP_SET_HELD_FOR_EDITING",
          message: "service: this backup set is held for editing",
          correlationId: "cid_held_1"
        })
      )
    );
    const sets = await createMockApi().listSets();
    const target = sets.find((s) => s.enabled) ?? sets[0];
    const api = { ...createMockApi(), runBackupSet };
    seedVersion(VERSION);
    await renderDetail(api, target);

    press("Run this backup set");

    await screen.findByText("This backup set is being edited.");
    expect(screen.queryByText("A backup run is already in progress.")).toBeNull();
    // The command an operator can still run, naming the set by the full
    // id `--backup-set` has taken since #569, so it pastes into a shell
    // without being split by hand.
    const command = document.querySelector("pre");
    expect(command?.textContent).toContain("backupd fetch --backup-set " + target.id);
  });

  // The owner's rule, explicitly: when a deployment-wide run is refused,
  // the reason lands in the terminal of EVERY associated backup set,
  // because every one of them is a set the operator just asked to have
  // backed up and did not. A refusal that only reached the dashboard
  // would be invisible to somebody looking at the set they care about.
  it("shows a deployment-wide refusal on the set's own page", async () => {
    const runCycle = vi.fn(() =>
      Promise.reject(
        new BackupdError({
          code: "DESTRUCTIVE_OPERATIONS_DISABLED",
          message: "destructive operations are disabled",
          correlationId: "cid_gate_2"
        })
      )
    );
    const sets = await createMockApi().listSets();
    const target = sets.find((s) => s.enabled) ?? sets[0];
    const api = { ...createMockApi(), runCycle };
    seedVersion(VERSION);
    // The hook reads the shared sets node to learn which sets a
    // deployment-wide run would visit, which is what decides whose
    // terminal the refusal reaches.
    act(() => {
      graph.commit("test/seed-sets", (tx) =>
        tx.set(setsNode, { data: sets, error: null, loading: false })
      );
    });
    await renderDetail(api, target);

    press("Run all enabled sets");

    await screen.findByText("This deployment will not start a backup run.");
  });
});
