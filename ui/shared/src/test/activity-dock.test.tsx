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
import {
  ActivityDock,
  DOCK_BAR_HEIGHT,
  dockEntries,
  dockReservedHeight,
  dockPrefix,
  dockText,
  environmentPreamble,
  foldReading,
  restartRule
} from "@shared/components/ActivityDock";
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

  // A cycle belongs to no single backup set, so a cycle that announced
  // itself and went quiet has no strip to be reported on: this dock is
  // the only surface that can say so at all (issue #625).
  it("carries every bucket's unfinished actions, the deployment's included", () => {
    const held = foldReading(
      undefined,
      reading({
        sets: [
          {
            ...set("a/b", [event(2)]),
            unfinishedActions: [{ action: "connection_test", actionId: "ct-1", startedAt: "2026-09-07T14:02:00Z", sequence: 2 }]
          }
        ],
        deployment: {
          ...deployment([event(1, { scope: "deployment", event: "cycle_start" })]),
          unfinishedActions: [{ action: "cycle", actionId: "c-1", startedAt: "2026-09-07T14:01:00Z", sequence: 1 }]
        }
      })
    );
    expect((held.unfinishedActions ?? []).map((a) => a.action)).toEqual(["cycle", "connection_test"]);
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
            // Bare on the wire: the prompt is this panel's to draw.
            command: "backup-manager backup-set patch api-server/var-backups --stale-after 48h"
          }
        })
      },
      { kind: "restart", at: "2026-09-07T14:02:41Z" }
    ];

    const text = dockText(entries, "alice");
    const lines = text.split("\n");
    expect(lines[0]).toContain("[api-server/var-backups]");
    expect(lines[1]).toContain("[you]");
    expect(text).toContain("\n$ backup-manager backup-set patch api-server/var-backups --stale-after 48h");
    expect(lines[lines.length - 1]).toContain("engine restarted");
  });

  it("draws the prompt in front of a command the wire sends bare, and never twice", () => {
    // The `command` field is a command, not a screen: a script reading
    // the journal hands it to a shell as is. So the "$ " is drawn here,
    // once, the same way the "# " is drawn in front of a gap.
    const text = dockText(
      [
        {
          kind: "event",
          event: event(1, {
            event: "api_action",
            scope: "deployment",
            fields: { actor: "alice", route: "POST /api/v1/catalog/rebuild", command: "backup-manager catalog rebuild" }
          })
        }
      ],
      "alice"
    );
    expect(text).toContain("\n$ backup-manager catalog rebuild");
    expect(text).not.toContain("$ $");
  });

  it("says a command is not runnable as printed when the engine says so, from the field rather than the text", () => {
    const text = dockText(
      [
        {
          kind: "event",
          event: event(1, {
            event: "api_action",
            scope: "deployment",
            fields: {
              actor: "alice",
              route: "PUT /api/v1/backup-sets/{source}/{set}/retention",
              command: "backup-manager backup-set retention api-server/var-backups --policy-file <a file holding these tiers as a retention: block>",
              command_runnable: "false"
            }
          })
        }
      ],
      "alice"
    );
    expect(text).toContain("$ backup-manager backup-set retention");
    expect(text).toContain("#   not runnable as printed");
    // And the control: a runnable command does not carry the note.
    const runnable = dockText(
      [
        {
          kind: "event",
          event: event(2, {
            event: "api_action",
            scope: "deployment",
            fields: { actor: "alice", route: "POST /api/v1/catalog/rebuild", command: "backup-manager catalog rebuild" }
          })
        }
      ],
      "alice"
    );
    expect(runnable).not.toContain("not runnable");
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

  it("puts the environment the commands need first, said once, and never the password", () => {
    const preamble = environmentPreamble("http://nas.local:8080", "alice");
    expect(preamble).toBe(
      "export BACKUP_MANAGER_API_URL=http://nas.local:8080 BACKUP_MANAGER_API_USERNAME=alice BACKUP_MANAGER_API_PASSWORD=<your password>"
    );

    // Composed from the origin the page was loaded from rather than by
    // the engine, because the engine does not know the address a reader
    // would type at their own shell; behind a proxy it is not what the
    // engine listens on.
    expect(environmentPreamble("https://nas.example.com", null)).toBe(
      "export BACKUP_MANAGER_API_URL=https://nas.example.com BACKUP_MANAGER_API_USERNAME=<the administrator you sign in as> BACKUP_MANAGER_API_PASSWORD=<your password>"
    );
    // A name a shell would split is quoted, so the line is still one an
    // operator can paste.
    expect(environmentPreamble("http://nas.local:8080", "the admin")).toContain("BACKUP_MANAGER_API_USERNAME='the admin'");

    const text = dockText(
      [{ kind: "event", event: event(1, { event: "api_action", scope: "deployment", fields: { actor: "alice", command: "backup-manager catalog rebuild" } }) }],
      "alice",
      preamble
    );
    expect(text.split("\n")[0]).toBe(preamble);
    // Once, at the top, and not again beside the command.
    expect(text.split("BACKUP_MANAGER_API_URL").length).toBe(2);
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
              fields: { actor: "alice", route: "PATCH /api/v1/settings", command: "backup-manager settings patch --timezone Europe/Berlin" }
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
    const copied = writeText.mock.calls[0][0] as string;
    expect(copied).toContain("$ backup-manager settings patch --timezone Europe/Berlin");
    // The first line of what is copied is the environment the command
    // needs, built from this page's own origin, so what is pasted is
    // runnable under it.
    expect(copied.split("\n")[0]).toBe(environmentPreamble(window.location.origin, null));
    vi.unstubAllGlobals();
  });

  it("draws the environment header at the top of the scrollback, from this page's own origin", async () => {
    renderDock(dockApi([reading({ deployment: deployment([event(1, { scope: "deployment", event: "startup", message: "backup-manager starting" })]) })]));
    await screen.findByText(/backup-manager starting/);
    const header = screen.getByText(/^export BACKUP_MANAGER_API_URL=/);
    expect(header.textContent).toContain("BACKUP_MANAGER_API_URL=" + window.location.origin);
    expect(header.textContent).toContain("BACKUP_MANAGER_API_PASSWORD=<your password>");
  });
});

describe("where the dock sits (issue #617)", () => {
  beforeEach(() => {
    try {
      window.localStorage.clear();
    } catch {
      /* a browser with site data blocked */
    }
    document.documentElement.style.removeProperty("--dock-height");
  });

  // The requirement EPIC G actually asked for: attached to the browser
  // window, not to the end of the document. As a flex sibling it rode the
  // bottom of the page, so on anything longer than a screen it scrolled
  // away and the always-on panel was only there when you were already at
  // the bottom.
  it("pins itself to the browser window rather than riding the end of the page", async () => {
    renderDock(dockApi([reading({ deployment: deployment([event(1, { scope: "deployment" })]) })]));
    const panel = await screen.findByLabelText("Terminal");
    expect(panel.style.position).toBe("fixed");
    expect(panel.style.bottom).toBe("0px");
    expect(panel.style.left).toBe("0px");
    expect(panel.style.right).toBe("0px");
  });

  // Fixed positioning takes the panel out of flow, so whatever is under it
  // has to be told how much room to leave or the last row of a long table
  // sits behind the terminal permanently. That is the exact objection the
  // old comments in this file and in AppShell raised against going fixed,
  // and it is answered by reserving rather than by staying in flow.
  it("reserves one line collapsed and its whole height expanded", () => {
    expect(dockReservedHeight(false, 240)).toBe(DOCK_BAR_HEIGHT);
    expect(dockReservedHeight(false, 999)).toBe(DOCK_BAR_HEIGHT);
    expect(dockReservedHeight(true, 240)).toBe(240 + DOCK_BAR_HEIGHT + 8);
    expect(dockReservedHeight(true, 400)).toBe(400 + DOCK_BAR_HEIGHT + 8);
  });

  it("publishes the height it reserves, and moves it when the viewer collapses the panel", async () => {
    renderDock(dockApi([reading({ deployment: deployment([event(1, { scope: "deployment" })]) })]));
    await screen.findByLabelText("Terminal");

    await waitFor(() =>
      expect(document.documentElement.style.getPropertyValue("--dock-height")).toBe(
        dockReservedHeight(true, 240) + "px"
      )
    );

    await act(async () => {
      screen.getByRole("button", { name: "Hide terminal" }).click();
    });

    await waitFor(() =>
      expect(document.documentElement.style.getPropertyValue("--dock-height")).toBe(DOCK_BAR_HEIGHT + "px")
    );
  });
});
