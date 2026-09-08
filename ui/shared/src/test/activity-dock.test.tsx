/**
 * The docked terminal (issue #599).
 *
 * The cases here are the four claims that are structural rather than
 * cosmetic, plus the two that are about not lying. A restart keeps the
 * lines leading up to it and draws a rule, because those are the evidence
 * of why a process died. A set's own line and the deployment's are
 * distinguishable, because that distinction is what the split in #593
 * bought and nothing rendered it before. A request line names WHO made it
 * rather than saying "you" to everybody. And a command echoed for an
 * action goes into the export, because "send me what the terminal said"
 * is half the reason the echo exists.
 */
import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ApiProvider } from "@shared/api/ApiContext";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { ActivityDock, dockEntries, dockPrefix, dockText, foldReading, restartRule } from "@shared/components/ActivityDock";
import type { DockEntry } from "@shared/components/ActivityDock";
import type { BackupManagerApi } from "@shared/api/contracts";
import type { DeploymentActivity, LiveActivity, SetActivity, SetActivityEvent } from "@shared/types/activity";

function event(sequence: number, over: Partial<SetActivityEvent> = {}): SetActivityEvent {
  return {
    sequence,
    at: "2026-09-07T14:02:0" + (sequence % 10) + "Z",
    level: "info",
    event: "commit",
    scope: "set",
    message: "durable commit complete",
    fields: {},
    ...over
  };
}

const IDLE = {
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
} satisfies Omit<SetActivity, "setId">;

function set(setId: string, events: SetActivityEvent[]): SetActivity {
  return {
    ...IDLE,
    setId,
    events,
    oldestSequence: events.length > 0 ? events[0].sequence : 0,
    latestSequence: events.length > 0 ? events[events.length - 1].sequence : 0
  };
}

function deployment(events: SetActivityEvent[]): DeploymentActivity {
  return {
    events,
    truncated: false,
    dropped: false,
    oldestSequence: events.length > 0 ? events[0].sequence : 0,
    latestSequence: events.length > 0 ? events[events.length - 1].sequence : 0
  };
}

function reading(over: Partial<LiveActivity> = {}): LiveActivity {
  return {
    observedAt: "2026-09-07T14:02:41Z",
    epoch: "one-process",
    pollAfterMs: 10_000,
    sets: [],
    deployment: null,
    ...over
  };
}

describe("what the dock holds", () => {
  it("carries a set's own lines and the deployment's in one window, ordered by sequence", () => {
    const held = foldReading(
      undefined,
      reading({
        sets: [set("api-server/var-backups", [event(2), event(4)])],
        deployment: deployment([event(1, { scope: "deployment", event: "cycle_start" }), event(3, { scope: "deployment", event: "disk_pressure" })])
      })
    );
    expect(held.events.map((e) => e.sequence)).toEqual([1, 2, 3, 4]);
  });

  it("fills in the set a line came from, so a line knows which strip it belongs to", () => {
    const held = foldReading(undefined, reading({ sets: [set("api-server/var-backups", [event(1)])] }));
    expect(held.events[0].fields.backup_set).toBe("api-server/var-backups");
    // A line that already named its set keeps its own answer rather than
    // being overwritten by the bucket it arrived in.
    const named = foldReading(
      undefined,
      reading({ sets: [set("api-server/var-backups", [event(1, { fields: { backup_set: "other/set" } })])] })
    );
    expect(named.events[0].fields.backup_set).toBe("other/set");
  });

  it("deduplicates by sequence, so a repeated or overlapping poll is harmless", () => {
    const first = foldReading(undefined, reading({ sets: [set("a/b", [event(1), event(2)])] }));
    const second = foldReading(first, reading({ sets: [set("a/b", [event(2), event(3)])] }));
    expect(second.events.map((e) => e.sequence)).toEqual([1, 2, 3]);
  });
});

describe("what is written in front of a line", () => {
  it("tells a set's own work from the deployment's, which is the whole point of the split", () => {
    expect(dockPrefix(event(1, { scope: "deployment" }), "alice").label).toBe("engine");
    expect(dockPrefix(event(2, { fields: { backup_set: "api-server/var-backups" } }), "alice").label).toBe(
      "api-server/var-backups"
    );
  });

  it("names WHO made a request rather than saying you to everybody", () => {
    const mine = event(3, { event: "api_action", scope: "deployment", fields: { actor: "alice" } });
    const theirs = event(4, { event: "api_action", scope: "deployment", fields: { actor: "bob" } });
    expect(dockPrefix(mine, "alice").label).toBe("you");
    // The case that matters: two people administering one NAS have to be
    // able to see that the other one just did something. A panel that
    // said "you" to both would tell them the opposite.
    expect(dockPrefix(theirs, "alice").label).toBe("bob");
    expect(dockPrefix(mine, "bob").label).toBe("alice");
  });
});

describe("an engine restart", () => {
  it("keeps the lines leading up to it and draws a rule, rather than dropping them", () => {
    const before = foldReading(undefined, reading({ sets: [set("a/b", [event(1), event(2)])] }));
    const history: DockEntry[] = [
      ...dockEntries([], before),
      { kind: "restart", at: "2026-09-07T14:02:41Z" }
    ];
    // The new process starts its sequence counter again at 1, which is
    // exactly why the dead process's lines have to be frozen rather than
    // merged: 1 would land on top of 1.
    const after = foldReading(undefined, reading({ epoch: "second-process", sets: [set("a/b", [event(1, { event: "startup" })])] }));
    const all = dockEntries(history, after);

    expect(all).toHaveLength(4);
    expect(all[2]).toEqual({ kind: "restart", at: "2026-09-07T14:02:41Z" });
    expect(all[0].kind === "event" && all[0].event.event).toBe("commit");
    expect(all[3].kind === "event" && all[3].event.event).toBe("startup");
  });

  it("says what the rule means rather than drawing a bare line", () => {
    expect(restartRule("2026-09-07T14:02:41Z")).toContain("engine restarted");
    expect(restartRule("2026-09-07T14:02:41Z")).toContain("a process that has gone");
  });
});

describe("the text an operator takes away", () => {
  it("carries the prefix, the echoed command and the restart rule, in the order on screen", () => {
    const entries: DockEntry[] = [
      { kind: "event", event: event(1, { fields: { backup_set: "api-server/var-backups" } }) },
      {
        kind: "event",
        event: event(2, {
          event: "api_action",
          scope: "deployment",
          level: "info",
          message: "patch /backup-sets/{source}/{set}",
          fields: {
            actor: "alice",
            route: "PATCH /api/v1/backup-sets/{source}/{set}",
            command: "$ backup-manager backup-set patch api-server/var-backups --stale-after 48h"
          }
        })
      },
      { kind: "restart", at: "2026-09-07T14:02:41Z" }
    ];

    const text = dockText(entries, "alice");
    const lines = text.split("\n");
    expect(lines[0]).toContain("[api-server/var-backups]");
    expect(lines[1]).toContain("[you]");
    expect(text).toContain("$ backup-manager backup-set patch api-server/var-backups --stale-after 48h");
    expect(lines[lines.length - 1]).toContain("engine restarted");
  });

  it("prints the gap for an action with no command rather than nothing at all", () => {
    const text = dockText(
      [
        {
          kind: "event",
          event: event(1, {
            event: "api_action",
            scope: "deployment",
            message: "post /operations refused: DESTRUCTIVE_OPERATIONS_DISABLED",
            fields: {
              actor: "alice",
              route: "POST /api/v1/operations",
              command_gap: "no backup-manager equivalent yet",
              command_gap_detail: "`backup-manager run` starts a cycle in your own shell, not in this engine"
            }
          })
        }
      ],
      "alice"
    );
    expect(text).toContain("# no backup-manager equivalent yet · POST /api/v1/operations");
    expect(text).toContain("not in this engine");
  });
});

/** An api that answers one reading and then nothing new, which is what a
 *  quiet deployment looks like. */
function dockApi(readings: LiveActivity[]): BackupManagerApi {
  let i = 0;
  return {
    getLiveActivity: () => {
      const next = readings[Math.min(i, readings.length - 1)];
      i++;
      return Promise.resolve(next);
    }
  } as unknown as BackupManagerApi;
}

function renderDock(api: BackupManagerApi) {
  return render(
    <MemoryRouter>
      <PlatformProvider bridge={genericBridge}>
        <ApiProvider api={api}>
          <ActivityDock />
        </ApiProvider>
      </PlatformProvider>
    </MemoryRouter>
  );
}

describe("the panel itself", () => {
  // The chrome is per viewer and it is in localStorage, so one case
  // collapsing the panel would otherwise leave it collapsed for the next.
  beforeEach(() => {
    try {
      window.localStorage.clear();
    } catch {
      /* a browser with site data blocked */
    }
  });

  it("renders the deployment's own lines on a deployment with no backup sets at all", async () => {
    // The case the feed could not answer before #599: an operator
    // clicking through a wizard on a fresh install, with nothing
    // configured and therefore no strip anywhere to read.
    renderDock(
      dockApi([
        reading({
          sets: [],
          deployment: deployment([event(1, { scope: "deployment", event: "startup", message: "backup-manager starting" })])
        })
      ])
    );
    expect(await screen.findByText(/backup-manager starting/)).toBeInTheDocument();
    expect(screen.getByText("[engine]")).toBeInTheDocument();
  });

  it("says the reading is not refreshing rather than drawing something that claims the process is alive", async () => {
    const api = {
      getLiveActivity: () => Promise.reject(new Error("the engine is not answering"))
    } as unknown as BackupManagerApi;
    renderDock(api);
    expect(await screen.findByText("not refreshing")).toBeInTheDocument();
  });

  it("collapses to the newest line and a count of what went wrong, never to nothing", async () => {
    const user = (await import("@testing-library/user-event")).default;
    renderDock(
      dockApi([
        reading({
          deployment: deployment([
            event(1, { scope: "deployment", event: "startup", message: "backup-manager starting" }),
            event(2, { scope: "deployment", event: "error", level: "error", message: "error", fields: { error: "the source refused the connection" } })
          ])
        })
      ])
    );
    await screen.findByText(/the source refused the connection/);
    await user.click(screen.getByRole("button", { name: /hide terminal/i }));

    // A terminal that collapses to nothing teaches an operator to stop
    // opening it.
    expect(screen.getByRole("button", { name: /show terminal/i })).toBeInTheDocument();
    expect(screen.getByText(/the source refused the connection/)).toBeInTheDocument();
    expect(screen.getByText("1 error")).toBeInTheDocument();
  });

  it("filters without dropping lines from the buffer, so clearing it brings them back", async () => {
    const user = (await import("@testing-library/user-event")).default;
    renderDock(
      dockApi([
        reading({
          sets: [set("api-server/var-backups", [event(2, { message: "durable commit complete" })])],
          deployment: deployment([event(1, { scope: "deployment", event: "cycle_start", message: "cycle starting" })])
        })
      ])
    );
    await screen.findByText("cycle started");
    await user.click(screen.getByRole("button", { name: "api-server/var-backups" }));
    await waitFor(() => expect(screen.queryByText("cycle started")).not.toBeInTheDocument());
    expect(screen.getByText(/committed/)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Everything" }));
    expect(await screen.findByText("cycle started")).toBeInTheDocument();
  });

  it("copies what it is showing, including the echoed command", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    const user = (await import("@testing-library/user-event")).default;
    renderDock(
      dockApi([
        reading({
          deployment: deployment([
            event(1, {
              scope: "deployment",
              event: "api_action",
              message: "patch /settings",
              fields: { actor: "alice", route: "PATCH /api/v1/settings", command: "$ backup-manager settings patch --timezone Europe/Berlin" }
            })
          ])
        })
      ])
    );
    await screen.findByText(/backup-manager settings patch/);
    await act(async () => {
      await user.click(screen.getByRole("button", { name: /^copy$/i }));
    });
    expect(writeText).toHaveBeenCalledTimes(1);
    expect(writeText.mock.calls[0][0]).toContain("$ backup-manager settings patch --timezone Europe/Berlin");
    vi.unstubAllGlobals();
  });
});
