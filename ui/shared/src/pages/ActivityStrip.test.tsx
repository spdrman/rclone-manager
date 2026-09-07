/**
 * The strip pinned at the foot of the dashboard, in the three states the
 * mockup draws (issue #573).
 *
 * Every assertion here is something an operator can see or hear: a word, a
 * count, a role, an accessible name. None of them reach for a class name
 * or an inline style, because a strip that renders the right DOM with the
 * wrong words is still a strip nobody can read.
 *
 * The three states are the whole point and they are the matrix. A set
 * mid-transfer, a set that stopped with failures, and a healthy idle one
 * have to be told apart at a glance, and the failing case's content is
 * real: it is the integrity failure and the REMOTE_RETAINED recording
 * failure from #570, which is exactly the pair this strip exists to
 * surface without anyone opening a log.
 */
import { describe, expect, it, vi } from "vitest";
import { render, screen, within, fireEvent } from "@testing-library/react";
import { ActivityStrip, activityLine, logText, artifactFraction } from "./ActivityStrip";
import type { BackupSet } from "@shared/types/backup";
import type { SetActivity, SetActivityEvent } from "@shared/types/activity";

const SET: BackupSet = {
  id: "api-server/var-backups",
  source: "api-server",
  set: "var-backups",
  name: "api-server / var-backups",
  host: "api-server.internal",
  port: 1209,
  username: "backup-agent",
  remoteFolder: "/var/backups",
  includePatterns: ["*"],
  excludePatterns: [],
  completionMethod: "completion-marker",
  stableForSeconds: 0,
  destination: "/data/backups/api-server/var-backups",
  retentionIsOverride: false,
  validations: ["transfer", "checksum"],
  state: "healthy",
  stateNote: "Verified nightly.",
  enabled: true,
  readOnly: true,
  readOnlyRetainedCount: 12,
  newestKnownGoodAt: "2026-09-07T00:04:44+02:00",
  lastRunAt: "2026-09-07T00:04:44+02:00",
  lastValidation: "passed",
  expectedIntervalHours: 1,
  retainedCount: 41,
  retainedBytes: 12 * 1024 ** 3,
  hostFingerprint: "SHA256:test-fingerprint",
  fingerprintTrustedAt: "2026-09-01T10:14:00+02:00"
};

function event(over: Partial<SetActivityEvent> & { sequence: number }): SetActivityEvent {
  return {
    at: "2026-09-07T00:16:29Z",
    level: "info",
    event: "cycle_start",
    scope: "deployment",
    message: "cycle starting",
    fields: {},
    ...over
  };
}

const IDLE: SetActivity = {
  setId: "api-server/var-backups",
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
  latestSequence: 0
};

const TRANSFERRING: SetActivity = {
  ...IDLE,
  active: true,
  stage: "transferring",
  artifact: "dpkg.status.2.gz",
  artifactsCompleted: 26,
  artifactsTotal: 41,
  progressBasis: "artifacts",
  bytesTransferred: 2 * 1024 * 1024,
  bytesTotal: 170 * 1024 * 1024,
  bytesPerSecond: 4.1 * 1024 ** 2,
  startedAt: "2026-09-07T00:16:29Z",
  events: [
    event({ sequence: 1, event: "discovery", scope: "set", message: "discovery pass complete", fields: { backup_set: "api-server/var-backups", discovered: "41", pending: "26" } }),
    event({ sequence: 2, event: "lifecycle_transition", scope: "set", message: "lifecycle transition", fields: { artifact: "api-server/var-backups/alternatives.tar.0", from: "DISCOVERED", to: "TRANSFERRING" } }),
    event({ sequence: 3, event: "lifecycle_transition", scope: "set", message: "lifecycle transition", fields: { artifact: "api-server/var-backups/alternatives.tar.0", from: "VERIFYING", to: "VERIFIED" } })
  ],
  oldestSequence: 1,
  latestSequence: 3
};

// The real thing from #570: an md5 mismatch against an empty destination,
// and the recording failure behind it.
const FAILING: SetActivity = {
  ...IDLE,
  artifactsCompleted: 26,
  artifactsTotal: 28,
  progressBasis: "artifacts",
  failures: 2,
  startedAt: "2026-09-07T00:16:00Z",
  finishedAt: "2026-09-07T00:16:55Z",
  events: [
    event({
      sequence: 1,
      level: "info",
      event: "lifecycle_transition",
      scope: "set",
      message: "lifecycle transition",
      fields: {
        artifact: "cicd-pipeline/var-backups/dpkg.status.1.gz",
        from: "VERIFYING",
        to: "FAILED",
        detail: "md5 differs: source a70969a2 destination d41d8cd9 (empty)"
      }
    }),
    event({
      sequence: 2,
      level: "warn",
      event: "error",
      scope: "deployment",
      message: "error",
      fields: { op: "record-failure", error: "could not record FAILED: artifact is REMOTE_RETAINED, not TRANSFERRING" }
    }),
    event({ sequence: 3, event: "cycle_end", scope: "deployment", message: "cycle finished with an error", fields: { cycle_id: "c1", error: "2 artifacts failed" } })
  ],
  oldestSequence: 1,
  latestSequence: 3
};

function strip(activity: SetActivity | null) {
  return render(<ActivityStrip set={SET} activity={activity} />);
}

describe("a set mid-transfer", () => {
  it("names the step, the artifact and where it is in the set", () => {
    strip(TRANSFERRING);
    const region = screen.getByRole("region", { name: /activity for api-server \/ var-backups/i });

    // The pill shouts the step and the sentence under the bar names it in
    // prose. Both are asserted, exactly, because "transferring" also
    // appears down in the log and a loose match would pass on that alone.
    expect(within(region).getByText("TRANSFERRING")).toBeInTheDocument();
    expect(within(region).getByText("Transferring")).toBeInTheDocument();
    expect(within(region).getByText("dpkg.status.2.gz")).toBeInTheDocument();
    // "26 of 41" is the whole claim: a percentage on its own leaves an
    // operator to guess what it counts.
    expect(within(region).getByText(/26 of 41 artifacts/i)).toBeInTheDocument();
    // "4 MB/s", not the mockup's "4.1 MB/s". The rate goes through the
    // design system's own formatter, which drops the decimal on purpose:
    // this number is re-read every second off a moving bar, and a
    // twitching tenth of a megabyte is noise an eye has to filter rather
    // than information. Disagreeing with it here would make this panel the
    // one screen in the product that renders a rate differently.
    expect(within(region).getByText(/4 MB\/s/)).toBeInTheDocument();
  });

  it("puts a bar behind the fraction and says what the bar counts", () => {
    strip(TRANSFERRING);
    const bar = screen.getByRole("progressbar");
    expect(bar).toHaveAttribute("aria-valuenow", "63");
    expect(bar.getAttribute("aria-label")).toMatch(/26 of 41 artifacts/i);
  });

  it("says the work is moving separately from how far it has got", () => {
    strip(TRANSFERRING);
    // A large artifact holds the numbers still for a long time while
    // everything is fine, so "is anything happening" has to be its own
    // statement rather than something inferred from the percentage.
    expect(screen.getByText(/in progress/i)).toBeInTheDocument();
  });

  it("shows the events with the newest last, rendered from their own fields", () => {
    strip(TRANSFERRING);
    const log = screen.getByRole("log");
    expect(within(log).getByText(/discovery complete: 41 artifacts, 26 pending/i)).toBeInTheDocument();
    expect(within(log).getByText(/transfer started: alternatives\.tar\.0/i)).toBeInTheDocument();
    expect(within(log).getByText(/verified alternatives\.tar\.0/i)).toBeInTheDocument();
  });
});

describe("a set that stopped with failures", () => {
  it("says how far it got, how many failed, and that nothing will retry it on its own", () => {
    strip(FAILING);
    const region = screen.getByRole("region", { name: /activity for api-server \/ var-backups/i });
    expect(within(region).getByText(/stopped after 26 of 28/i)).toBeInTheDocument();
    // The whole trail, not just the count: the count alone also appears in
    // the cycle_end line down in the log, and what this case is about is
    // the sentence telling an operator nothing will pick it up again.
    expect(within(region).getByText(/2 artifacts failed .* nothing will retry them on its own/i)).toBeInTheDocument();
    expect(within(region).getByText(/needs attention/i)).toBeInTheDocument();
  });

  it("carries the error's own detail, so nobody has to open a log to learn which fault this is", () => {
    strip(FAILING);
    const log = screen.getByRole("log");
    expect(within(log).getByText(/md5 differs: source a70969a2 destination d41d8cd9 \(empty\)/i)).toBeInTheDocument();
    expect(within(log).getByText(/could not record FAILED: artifact is REMOTE_RETAINED, not TRANSFERRING/i)).toBeInTheDocument();
  });

  it("is not reported as in progress", () => {
    strip(FAILING);
    expect(screen.queryByText(/in progress/i)).not.toBeInTheDocument();
  });
});

describe("a healthy idle set", () => {
  it("is still there, and says when the last cycle finished rather than nothing at all", () => {
    strip({ ...IDLE, artifactsCompleted: 18, artifactsTotal: 18, progressBasis: "artifacts", finishedAt: "2026-09-07T00:04:44Z" });
    const region = screen.getByRole("region", { name: /activity for api-server \/ var-backups/i });
    expect(within(region).getByText(/idle/i)).toBeInTheDocument();
    expect(within(region).getByText(/last cycle finished/i)).toBeInTheDocument();
  });

  it("draws no bar at all before anything has been discovered, rather than a full one", () => {
    strip(IDLE);
    const bar = screen.getByRole("progressbar");
    // No denominator means no reading. Zero of zero would draw as a
    // finished cycle, which is the one thing it must not say.
    expect(bar).not.toHaveAttribute("aria-valuenow");
    expect(bar.getAttribute("aria-label")).toMatch(/not measurable|nothing discovered/i);
  });

  it("reads as still checking, never as idle, while nothing has loaded", () => {
    strip(null);
    expect(screen.getByText(/checking/i)).toBeInTheDocument();
    expect(screen.queryByText(/idle/i)).not.toBeInTheDocument();
  });
});

describe("the log's own controls", () => {
  it("counts the lines and can be collapsed away", () => {
    strip(TRANSFERRING);
    expect(screen.getByText(/3 lines/i)).toBeInTheDocument();
    expect(screen.getByRole("log")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /hide log/i }));
    expect(screen.queryByRole("log")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /show log/i }));
    expect(screen.getByRole("log")).toBeInTheDocument();
  });

  it("copies the same text it shows", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    strip(TRANSFERRING);

    fireEvent.click(screen.getByRole("button", { name: /copy/i }));
    expect(writeText).toHaveBeenCalledTimes(1);
    expect(writeText.mock.calls[0][0]).toBe(logText(TRANSFERRING.events));
    vi.unstubAllGlobals();
  });

  it("says when the buffer dropped lines rather than presenting a gap as continuity", () => {
    strip({ ...TRANSFERRING, oldestSequence: 51, latestSequence: 250 });
    expect(screen.getByText(/earlier lines are not held here/i)).toBeInTheDocument();
  });
});

describe("the line each event renders as", () => {
  it("uses the event's own fields, and falls back to the message for a name it does not know", () => {
    expect(activityLine(event({ sequence: 1, event: "cycle_start", message: "cycle starting" })).text).toMatch(/cycle started/i);
    expect(
      activityLine(event({ sequence: 2, event: "transfer_stats", fields: { artifact: "a/b/one.dump", bytes_transferred: "1048576" } })).text
    ).toMatch(/one\.dump/);
    expect(
      activityLine(event({ sequence: 3, event: "a_name_this_build_does_not_know", message: "something happened", fields: { note: "42" } })).text
    ).toMatch(/something happened/);
  });

  it("colours a completed step and a failure differently from a plain note", () => {
    expect(activityLine(event({ sequence: 1, event: "commit", fields: { artifact: "a/b/one.dump" } })).tone).toBe("ok");
    expect(activityLine(event({ sequence: 2, level: "error", event: "error", fields: { op: "verify", error: "boom" } })).tone).toBe("error");
    expect(activityLine(event({ sequence: 3, level: "warn", event: "retry", fields: { op: "copy_to_local", attempt: "2", category: "transient", error: "reset" } })).tone).toBe("warn");
    expect(activityLine(event({ sequence: 4, event: "discovery", fields: { discovered: "3", pending: "1" } })).tone).toBe("info");
  });
});

describe("the fraction", () => {
  it("is null with no denominator and never zero", () => {
    expect(artifactFraction(IDLE)).toBeNull();
    expect(artifactFraction({ ...IDLE, artifactsTotal: 0, progressBasis: "artifacts" })).toBeNull();
  });

  it("rounds the way the mockup does", () => {
    expect(artifactFraction(TRANSFERRING)).toBe(63);
    expect(artifactFraction({ ...FAILING })).toBe(93);
  });
});

/**
 * A reading nobody can refresh (issue #573 review, defect 3).
 *
 * Keeping the last good reading on screen is right: blanking a panel an
 * operator is reading is a worse answer than an old one. What was wrong
 * is that the old one did not read as old WHERE the reading is. The pill
 * pulsed, the bar swept, a byte rate stood beside a time estimate, and
 * every one of those is a claim that the process is alive right now,
 * which is the exact thing nobody knows while the poll is failing. The
 * fraction stays, because it is still the last true thing measured.
 */
describe("a reading that is no longer refreshing", () => {
  it("stops asserting the process is alive, and keeps what was actually measured", () => {
    render(<ActivityStrip set={SET} activity={TRANSFERRING} stale />);
    const region = screen.getByRole("region", { name: /activity for api-server \/ var-backups/i });

    // The rate and the estimate are both claims about now.
    expect(within(region).queryByText(/MB\/s/)).not.toBeInTheDocument();
    expect(within(region).queryByText(/left$/)).not.toBeInTheDocument();
    expect(within(region).queryByText(/in progress/i)).not.toBeInTheDocument();

    // The headline says which of the two things this panel exists to
    // tell apart is actually true.
    expect(within(region).getByText(/not reporting/i)).toBeInTheDocument();

    // And the fraction, which was measured and has not stopped being
    // what was measured.
    expect(within(region).getByText(/26 of 41 artifacts/i)).toBeInTheDocument();
    expect(screen.getByRole("progressbar")).toHaveAttribute("aria-valuenow", "63");
  });

  it("says the same thing to a reader who cannot see the animation", () => {
    const { container } = render(<ActivityStrip set={SET} activity={TRANSFERRING} stale />);
    // The sweep and the spinner are the two "it is alive" signals that
    // carry no words at all, so they are the two a stale reading must
    // not draw.
    expect(container.querySelector(".activity-bar__fill--busy")).toBeNull();
    expect(container.querySelector(".activity-spinner")).toBeNull();
  });

  it("draws all of it while the reading is current", () => {
    const { container } = render(<ActivityStrip set={SET} activity={TRANSFERRING} />);
    expect(screen.getByText(/4 MB\/s/)).toBeInTheDocument();
    expect(container.querySelector(".activity-bar__fill--busy")).not.toBeNull();
    expect(screen.getByText("TRANSFERRING")).toBeInTheDocument();
  });
});

/**
 * A pass that ended badly with nothing to count (issue #573 review,
 * defect 4).
 *
 * The headline was drawn from the failure count and the fraction, and
 * both are zero and absent for the pass that goes wrong earliest: a set
 * whose reconcile or discovery failed never reaches an artifact. So the
 * worst outcome the engine can produce drew as an em-dash, an ok-toned
 * bar and "Idle, last cycle finished 2m ago". The error was in the log
 * the whole time, which is not where an eye lands.
 */
describe("a pass that ended badly", () => {
  const FAILED_AT_RECONCILE: SetActivity = {
    ...IDLE,
    outcome: "failed",
    finishedAt: "2026-09-07T00:16:55Z",
    events: [
      event({ sequence: 1, level: "error", event: "error", scope: "set", message: "error", fields: { op: "reconcile", error: "the source refused the connection" } })
    ],
    oldestSequence: 1,
    latestSequence: 1
  };

  it("does not read as an idle set that had nothing to do", () => {
    strip(FAILED_AT_RECONCILE);
    const region = screen.getByRole("region", { name: /activity for api-server \/ var-backups/i });
    expect(within(region).queryByText(/^Idle$/)).not.toBeInTheDocument();
    expect(within(region).getByText(/needs attention/i)).toBeInTheDocument();
    expect(within(region).getByText(/did not finish/i)).toBeInTheDocument();
  });

  it("colours the bar from the outcome rather than from a failure count of zero", () => {
    const { container } = strip(FAILED_AT_RECONCILE);
    expect(container.querySelector(".activity-bar__fill--ok")).toBeNull();
    expect(container.querySelector(".activity-bar__fill--danger")).not.toBeNull();
  });

  it("tells a pass somebody stopped apart from a pass that broke", () => {
    strip({ ...IDLE, outcome: "stopped", artifactsCompleted: 26, artifactsTotal: 41, progressBasis: "artifacts", finishedAt: "2026-09-07T00:16:55Z" });
    const region = screen.getByRole("region", { name: /activity for api-server \/ var-backups/i });
    expect(within(region).getByText("STOPPED")).toBeInTheDocument();
    expect(within(region).getByText(/was stopped before it finished/i)).toBeInTheDocument();
    // Not an alarm: an operator taking an edit hold is this manager
    // doing what it was asked, and it must not be spelled the way an
    // unreachable source is.
    expect(within(region).queryByText(/needs attention/i)).not.toBeInTheDocument();
    // And fifteen artifacts it never attempted are not a green 63%.
    expect(within(region).getByText(/26 of 41 artifacts/i)).toBeInTheDocument();
  });

  it("shows failures accumulating in the headline while the pass is still running", () => {
    strip({ ...TRANSFERRING, failures: 2 });
    const region = screen.getByRole("region", { name: /activity for api-server \/ var-backups/i });
    // The step is still the most specific true thing, so it stays named.
    expect(within(region).getByText("TRANSFERRING")).toBeInTheDocument();
    // But a whole cycle in which failures are invisible in the headline
    // is a whole cycle an operator has no reason to look closer at.
    expect(within(region).getByText(/2 artifacts failed/i)).toBeInTheDocument();
  });

  it("says lines are missing when the service says its buffer lost them", () => {
    strip({ ...TRANSFERRING, dropped: true });
    expect(screen.getByText(/earlier lines are not held here/i)).toBeInTheDocument();
  });
});
