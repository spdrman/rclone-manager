/**
 * The activity strips on the dashboard: one per backup set, always
 * present, kept current by polling (issue #573).
 *
 * The cases here are about the three things that make a pinned strip
 * trustworthy rather than decorative. It appears for every configured set,
 * including the quiet ones, so its silence is never ambiguous between
 * "nothing is running" and "nothing is reporting". It comes back for more
 * on its own, at the cadence the SERVICE names, because the service is the
 * one that knows whether anything is moving. And it carries the cursor it
 * was given, so following a live cycle does not mean re-sending the whole
 * tail every second to a NAS.
 *
 * A failed fetch is the fourth case and it is the same discipline the rest
 * of this suite already enforces: an error is a stated notice, never a
 * confident empty panel.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { DashboardActivity } from "@shared/pages/DashboardActivity";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { BackupManagerError } from "@shared/api/contracts";
import type { BackupManagerApi } from "@shared/api/contracts";
import type { BackupSet } from "@shared/types/backup";
import type { LiveActivity, SetActivity } from "@shared/types/activity";

const BASE_SET: BackupSet = {
  connectionUnverified: false,
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
  trustedHostKeyRecordedAt: "2026-08-02T10:14:00+02:00",
  sshKeyId: "key_a1b2c3"
};

const SECOND_SET: BackupSet = {
  ...BASE_SET,
  id: "media/weekly-archive",
  source: "media",
  set: "weekly-archive",
  name: "Media archive"
};

function activityFor(setId: string, over: Partial<SetActivity> = {}): SetActivity {
  return {
    setId,
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
    events: [],
    truncated: false,
    dropped: false,
    oldestSequence: 0,
    latestSequence: 0,
    ...over
  };
}

/** One epoch throughout, because everything in this file is one process.
 *  A restart is its own case and lives in dashboard-activity-restart.tsx. */
function feed(sets: SetActivity[], pollAfterMs = 10_000): LiveActivity {
  return { observedAt: "2026-08-29T02:01:20+02:00", epoch: "one-process", pollAfterMs, sets, deployment: null };
}

function renderStrips(api: BackupManagerApi, sets: BackupSet[] | null = [BASE_SET, SECOND_SET]) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <DashboardActivity sets={sets} />
      </ApiProvider>
    </MemoryRouter>
  );
}

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("the strips on the dashboard", () => {
  it("draws one per configured set, including the ones with nothing to say", async () => {
    const getLiveActivity = vi.fn().mockResolvedValue(
      // Only one set has anything. The other still gets a strip: a panel
      // that appears only during activity teaches an operator to hunt for
      // it, and its absence then means either nothing is running or
      // nothing is reporting.
      feed([activityFor("production/postgres-primary", { finishedAt: "2026-08-29T01:00:00+02:00" })])
    );
    renderStrips({ ...createMockApi(), getLiveActivity });
    await act(async () => {});

    expect(screen.getByRole("region", { name: /activity for production postgresql/i })).toBeInTheDocument();
    expect(screen.getByRole("region", { name: /activity for media archive/i })).toBeInTheDocument();
  });

  it("asks again at the cadence the service named, not one it picked itself", async () => {
    vi.useFakeTimers();
    const getLiveActivity = vi.fn().mockResolvedValue(feed([activityFor("production/postgres-primary")], 1000));
    renderStrips({ ...createMockApi(), getLiveActivity });
    await act(async () => {});
    expect(getLiveActivity).toHaveBeenCalledTimes(1);

    // The service said 1s because something was moving. Half of it is not
    // yet due; the whole of it is.
    await act(async () => {
      vi.advanceTimersByTime(500);
    });
    expect(getLiveActivity).toHaveBeenCalledTimes(1);

    await act(async () => {
      vi.advanceTimersByTime(600);
    });
    expect(getLiveActivity).toHaveBeenCalledTimes(2);
  });

  it("does not keep polling once it has been taken off the screen", async () => {
    vi.useFakeTimers();
    const getLiveActivity = vi.fn().mockResolvedValue(feed([activityFor("production/postgres-primary")], 1000));
    const view = renderStrips({ ...createMockApi(), getLiveActivity });
    await act(async () => {});
    view.unmount();

    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    expect(getLiveActivity).toHaveBeenCalledTimes(1);
  });

  it("says what went wrong rather than showing an empty panel", async () => {
    const getLiveActivity = vi.fn().mockRejectedValue(
      new BackupManagerError({
        code: "INTERNAL",
        message: "The backup service could not read live activity.",
        correlationId: "cid_test"
      })
    );
    renderStrips({ ...createMockApi(), getLiveActivity });
    await act(async () => {});

    expect(screen.getByText(/could not read live activity/i)).toBeInTheDocument();
  });

  it("reads as still checking while the first request is in flight", async () => {
    const getLiveActivity = vi.fn().mockReturnValue(new Promise<LiveActivity>(() => {}));
    renderStrips({ ...createMockApi(), getLiveActivity });

    const region = screen.getByRole("region", { name: /activity for production postgresql/i });
    expect(within(region).getByText(/checking/i)).toBeInTheDocument();
  });

  it("carries the cursor it was given, so a second look is not the whole tail again", async () => {
    vi.useFakeTimers();
    const getLiveActivity = vi.fn().mockResolvedValue(
      feed(
        [
          activityFor("production/postgres-primary", {
            events: [
              { sequence: 41, at: "2026-08-29T02:01:11+02:00", level: "info", event: "cycle_start", scope: "deployment", message: "cycle starting", fields: {} },
              { sequence: 42, at: "2026-08-29T02:01:12+02:00", level: "info", event: "discovery", scope: "set", message: "discovery pass complete", fields: { discovered: "3", pending: "1" } }
            ],
            oldestSequence: 41,
            latestSequence: 42
          })
        ],
        1000
      )
    );
    renderStrips({ ...createMockApi(), getLiveActivity });
    await act(async () => {});

    // The first look asks for everything the service still holds, and
    // for as much of it as the contract allows in one answer: the
    // service hands back the OLDEST slice above the cursor, so a smaller
    // limit is safe but means catching up over several polls.
    expect(getLiveActivity).toHaveBeenCalledWith({ since: 0, limit: 200 });

    await act(async () => {
      vi.advanceTimersByTime(1100);
    });
    expect(getLiveActivity).toHaveBeenLastCalledWith({ since: 42, limit: 200 });
  });

  it("keeps the lines it already had when a later reading only carries what is new", async () => {
    const first = feed(
      [
        activityFor("production/postgres-primary", {
          events: [{ sequence: 1, at: "2026-08-29T02:01:11+02:00", level: "info", event: "cycle_start", scope: "deployment", message: "cycle starting", fields: {} }],
          oldestSequence: 1,
          latestSequence: 1
        })
      ],
      1000
    );
    const second = feed(
      [
        activityFor("production/postgres-primary", {
          events: [{ sequence: 2, at: "2026-08-29T02:01:12+02:00", level: "info", event: "commit", scope: "set", message: "durable commit complete", fields: { artifact: "production/postgres-primary/one.dump" } }],
          oldestSequence: 1,
          latestSequence: 2
        })
      ],
      1000
    );
    vi.useFakeTimers();
    const getLiveActivity = vi.fn().mockResolvedValueOnce(first).mockResolvedValue(second);
    renderStrips({ ...createMockApi(), getLiveActivity });
    await act(async () => {});
    await act(async () => {
      vi.advanceTimersByTime(1100);
    });

    // Both lines, in order. A page that replaced its buffer with each
    // cursored slice would show only the newest, and one that appended
    // without deduplicating would show a line twice on any retried request.
    const log = screen.getAllByRole("log")[0];
    expect(within(log).getByText(/cycle started/i)).toBeInTheDocument();
    expect(within(log).getByText(/committed one\.dump/i)).toBeInTheDocument();
    expect(screen.getByText(/^2 lines$/i)).toBeInTheDocument();
  });

  it("renders nothing at all before the set list has arrived, rather than a strip for no set", async () => {
    const getLiveActivity = vi.fn().mockResolvedValue(feed([]));
    renderStrips({ ...createMockApi(), getLiveActivity }, null);
    await act(async () => {});

    expect(screen.queryByRole("region", { name: /activity for/i })).not.toBeInTheDocument();
  });
});
