/**
 * What the strips do when the service they are following restarts
 * (issue #573, and the review of the panel that shipped it).
 *
 * The cursor is a sequence number and the sequence counter is per
 * process: it starts again at zero every time the engine starts. So a tab
 * that had reached 412 asks for everything after 412, a fresh process
 * answers correctly that it has nothing above 412, and the panel goes on
 * showing lines from a cycle that no longer exists, for as long as the
 * tab is open. Restarts are routine here (this ships under container
 * restart policies, and "restart it" is a remedy we hand operators), and
 * the panel's whole stated purpose is telling "nothing is running" apart
 * from "nothing is reporting". After a restart it was doing neither.
 *
 * The reading already carries everything needed to notice: an epoch that
 * names the process, and a latest sequence that sits below the cursor it
 * handed out. Both are checked, because a client talking to a service too
 * old to send an epoch still has the second.
 */
import { describe, expect, it, vi } from "vitest";
import { act, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { DashboardActivity, feedRestarted, mergeActivity } from "@shared/pages/DashboardActivity";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import type { BackupManagerApi } from "@shared/api/contracts";
import type { BackupSet } from "@shared/types/backup";
import type { LiveActivity, SetActivity, SetActivityEvent } from "@shared/types/activity";

const SET: BackupSet = {
  id: "production/postgres-primary",
  source: "production",
  set: "postgres-primary",
  name: "Production PostgreSQL",
  host: "prod-db-01.internal",
  port: 22,
  username: "backup-agent",
  remoteFolder: "/backups/postgresql/",
  includePatterns: ["*.dump.zst"],
  excludePatterns: [],
  completionMethod: "completion-marker",
  stableForSeconds: 0,
  destination: "/data/backups/production/postgres/",
  retentionIsOverride: false,
  validations: ["transfer", "checksum"],
  state: "healthy",
  stateNote: "Verified nightly dump.",
  enabled: true,
  readOnly: false,
  readOnlyRetainedCount: 0,
  newestKnownGoodAt: "2026-08-29T02:01:01+02:00",
  lastRunAt: "2026-08-29T02:01:01+02:00",
  lastValidation: "passed",
  expectedIntervalHours: 24,
  retainedCount: 32,
  retainedBytes: 421 * 1024 ** 3,
  trustedHostKeys: [{ algorithm: "ssh-ed25519", fingerprint: "SHA256:test-fingerprint" }],
  trustedHostKeyRecordedAt: "2026-08-02T10:14:00+02:00"
};

function line(sequence: number, message: string): SetActivityEvent {
  return { sequence, at: "2026-09-07T00:00:00Z", level: "info", event: "an_event_of_its_own", scope: "set", message, fields: {} };
}

function activity(events: SetActivityEvent[], latestSequence: number, over: Partial<SetActivity> = {}): SetActivity {
  return {
    setId: SET.id,
    active: false,
    stage: null,
    artifact: null,
    artifactsCompleted: 0,
    artifactsTotal: null,
    progressBasis: "unknown",
    bytesTransferred: null,
    bytesTotal: null,
    bytesPerSecond: null,
    failures: 0,
    outcome: null,
    startedAt: null,
    finishedAt: null,
    events,
    truncated: false,
    dropped: false,
    oldestSequence: events.length > 0 ? events[0].sequence : 0,
    latestSequence,
    ...over
  };
}

function feed(epoch: string, sets: SetActivity[]): LiveActivity {
  return { observedAt: "2026-09-07T00:00:20Z", epoch, pollAfterMs: 1000, sets, deployment: null };
}

describe("noticing that the service restarted", () => {
  it("sees a different epoch for what it is", () => {
    const next = feed("second-process", [activity([], 7)]);
    expect(feedRestarted({ epoch: "first-process", cursor: 412 }, next)).toBe(true);
    expect(feedRestarted({ epoch: "first-process", cursor: 412 }, feed("first-process", [activity([], 500)]))).toBe(false);
  });

  it("falls back to the sequence when there is no epoch to compare", () => {
    // A fresh process's own highest sequence sits far below a cursor the
    // dead one handed out, which is a rewind no live feed can produce.
    expect(feedRestarted({ epoch: null, cursor: 412 }, feed("", [activity([], 7)]))).toBe(true);
    expect(feedRestarted({ epoch: null, cursor: 412 }, feed("", [activity([], 413)]))).toBe(false);
    // A page that has never held anything cannot have been rewound.
    expect(feedRestarted({ epoch: null, cursor: 0 }, feed("", [activity([], 0)]))).toBe(false);
  });

  it("drops the dead process's lines instead of showing them forever", async () => {
    vi.useFakeTimers();
    const before = feed("first-process", [
      activity([line(411, "transferring old-artifact"), line(412, "cycle finished")], 412)
    ]);
    // The restart. The new process has emitted seven lines of its own and
    // none of them are above the cursor, so it answers with nothing.
    const afterRestart = feed("second-process", [activity([], 7)]);
    // And once the page has dropped its cursor, the next poll gets them.
    const caughtUp = feed("second-process", [activity([line(1, "cycle starting"), line(2, "discovery complete")], 2)]);

    const getLiveActivity = vi
      .fn()
      .mockResolvedValueOnce(before)
      .mockResolvedValueOnce(afterRestart)
      .mockResolvedValue(caughtUp);
    const api: BackupManagerApi = { ...createMockApi(), getLiveActivity };
    render(
      <MemoryRouter>
        <ApiProvider api={api}>
          <DashboardActivity sets={[SET]} />
        </ApiProvider>
      </MemoryRouter>
    );
    await act(async () => {});
    expect(within(screen.getAllByRole("log")[0]).getByText(/transferring old-artifact/)).toBeInTheDocument();

    await act(async () => {
      vi.advanceTimersByTime(1100);
    });
    // The dead process's lines are gone the moment the restart is
    // noticed, rather than sitting there under a bar that says the work
    // is still in flight.
    expect(screen.queryByText(/transferring old-artifact/)).not.toBeInTheDocument();

    await act(async () => {
      vi.advanceTimersByTime(1100);
    });
    // The cursor went back to the beginning with them, so the next ask is
    // for everything the live process holds rather than for what is above
    // a number it will not reach for hours.
    expect(getLiveActivity).toHaveBeenLastCalledWith({ since: 0, limit: 200 });
    expect(within(screen.getAllByRole("log")[0]).getByText(/cycle starting/)).toBeInTheDocument();
    vi.useRealTimers();
  });

  it("remembers a gap once one has happened", () => {
    // The page's own buffer is not re-continuous just because a later
    // reading answered a caught-up cursor cleanly.
    const held = mergeActivity(undefined, activity([line(1, "one")], 1, { dropped: true }));
    const next = mergeActivity(held, activity([line(2, "two")], 2, { dropped: false }));
    expect(next.dropped).toBe(true);
  });
});
