/**
 * The cursor every surface following the live feed sends back.
 *
 * It is one number, it is the whole of what makes polling this feed
 * cheap, and it is the one thing a client can get wrong in a way that
 * nothing on screen shows. Both defects here are silent by
 * construction: the merge deduplicates by sequence, so a cursor that
 * asks for too much draws exactly the same panel as a correct one while
 * refetching the service's entire held tail on every tick, and a cursor
 * that asks for too little draws exactly the same panel while a page of
 * lines is never requested again.
 *
 * So these cases assert the REQUEST, not the picture. The picture cannot
 * tell them apart.
 */
import { describe, expect, it, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ApiProvider } from "@shared/api/ApiContext";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { ActivityDock } from "@shared/components/ActivityDock";
import { mergeActivity, nextCursor, useActivityFeed } from "@shared/pages/useActivityFeed";
import type { BackupdApi } from "@shared/api/contracts";
import type { DeploymentActivity, LiveActivity, SetActivity, SetActivityEvent } from "@shared/types/activity";

function event(sequence: number, over: Partial<SetActivityEvent> = {}): SetActivityEvent {
  return {
    sequence,
    at: "2026-09-07T14:02:41Z",
    level: "info",
    event: "commit",
    scope: "set",
    message: "durable commit complete " + sequence,
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

function set(setId: string, events: SetActivityEvent[], over: Partial<SetActivity> = {}): SetActivity {
  return {
    ...IDLE,
    setId,
    events,
    oldestSequence: events.length > 0 ? events[0].sequence : 0,
    latestSequence: events.length > 0 ? events[events.length - 1].sequence : 0,
    ...over
  };
}

function deployment(events: SetActivityEvent[], over: Partial<DeploymentActivity> = {}): DeploymentActivity {
  return {
    events,
    truncated: false,
    dropped: false,
    oldestSequence: events.length > 0 ? events[0].sequence : 0,
    latestSequence: events.length > 0 ? events[events.length - 1].sequence : 0,
    ...over
  };
}

function reading(over: Partial<LiveActivity> = {}): LiveActivity {
  return {
    observedAt: "2026-09-07T14:02:41Z",
    epoch: "one-process",
    pollAfterMs: 1000,
    sets: [],
    deployment: null,
    ...over
  };
}

function renderDock(api: BackupdApi) {
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

describe("where the cursor comes from", () => {
  it("takes it from what the service says it HOLDS, not from what happened to arrive", () => {
    // An idle poll: the service is holding 4200 lines and has nothing
    // above the cursor to hand over. A cursor built from the arriving
    // events is zero here, and zero means "send me your whole tail".
    const quiet = reading({
      sets: [set("a/b", [], { oldestSequence: 4000, latestSequence: 4200 })],
      deployment: deployment([], { oldestSequence: 4000, latestSequence: 4200 })
    });
    expect(nextCursor(4200, quiet)).toBe(4200);
  });

  it("never goes backwards, whatever a reading says", () => {
    // A cursor that rewinds re-requests everything above the point it
    // rewound to, on every tick, for ever.
    expect(nextCursor(4200, reading({ sets: [set("a/b", [], { latestSequence: 7 })] }))).toBe(4200);
    expect(nextCursor(0, reading())).toBe(0);
  });

  it("counts the deployment's bucket, which is the only bucket a fresh install has", () => {
    const fresh = reading({ sets: [], deployment: deployment([event(1), event(2)], { latestSequence: 2 }) });
    expect(nextCursor(0, fresh)).toBe(2);
  });

  it("holds back at a bucket that handed over one page and said there was more", () => {
    // The service answers a cursor with the OLDEST slice above it and
    // says when the limit cut that slice short, which is an invitation
    // to ask again from where it stopped. A cursor that jumped to the
    // highest sequence anywhere in the reading would step over 4..300:
    // still held, still inside the buffer, and never requested again by
    // anybody, with nothing in any later response saying so.
    const paged = reading({
      sets: [
        set("busy/set", [event(1), event(2), event(3)], { truncated: true, latestSequence: 300 }),
        set("quiet/set", [event(400)], { latestSequence: 400 })
      ]
    });
    expect(nextCursor(0, paged)).toBe(3);

    // The deployment's bucket pages the same way and is held back for
    // the same reason.
    const pagedDeployment = reading({
      sets: [set("quiet/set", [event(400)], { latestSequence: 400 })],
      deployment: deployment([event(1), event(2)], { truncated: true, latestSequence: 300 })
    });
    expect(nextCursor(0, pagedDeployment)).toBe(2);

    // And the control: without the flag the same reading advances to the
    // top, because there is nothing left to page.
    const whole = reading({
      sets: [
        set("busy/set", [event(1), event(2), event(3)], { latestSequence: 3 }),
        set("quiet/set", [event(400)], { latestSequence: 400 })
      ]
    });
    expect(nextCursor(0, whole)).toBe(400);
  });
});

describe("the cursor the dock actually sends", () => {
  it("keeps its place through an idle poll instead of asking for the whole tail again", async () => {
    vi.useFakeTimers();
    // A long-running deployment: the service holds 200 lines per bucket
    // and the dock has caught up with them.
    const busy = reading({
      sets: [set("a/b", [event(4199), event(4200)], { oldestSequence: 4000, latestSequence: 4200 })]
    });
    // Then nothing happens, which is what most polls look like.
    const idle = reading({
      sets: [set("a/b", [], { oldestSequence: 4000, latestSequence: 4200 })]
    });
    const getLiveActivity = vi.fn().mockResolvedValueOnce(busy).mockResolvedValue(idle);
    renderDock({ getLiveActivity } as unknown as BackupdApi);

    await act(async () => {});
    expect(getLiveActivity).toHaveBeenLastCalledWith({ since: 0, limit: 200 });

    await act(async () => {
      vi.advanceTimersByTime(1100);
    });
    expect(getLiveActivity).toHaveBeenLastCalledWith({ since: 4200, limit: 200 });

    // The poll after the idle one is the whole defect. A cursor rebuilt
    // from the events that arrived is zero here, and since=0 asks a
    // twenty-set deployment for roughly 4200 events, per tab, every
    // second, while the merge hides it by deduplicating them.
    await act(async () => {
      vi.advanceTimersByTime(1100);
    });
    expect(getLiveActivity).toHaveBeenLastCalledWith({ since: 4200, limit: 200 });
    vi.useRealTimers();
  });

  it("asks again from where a truncated bucket stopped rather than past it", async () => {
    vi.useFakeTimers();
    const paged = reading({
      sets: [
        set("busy/set", [event(1), event(2), event(3)], { truncated: true, latestSequence: 300 }),
        set("quiet/set", [event(400)], { latestSequence: 400 })
      ]
    });
    const getLiveActivity = vi.fn().mockResolvedValue(paged);
    renderDock({ getLiveActivity } as unknown as BackupdApi);

    await act(async () => {});
    await act(async () => {
      vi.advanceTimersByTime(1100);
    });
    expect(getLiveActivity).toHaveBeenLastCalledWith({ since: 3, limit: 200 });
    vi.useRealTimers();
  });

  it("renders the deployment's lines and advances past them on a deployment with no sets", async () => {
    vi.useFakeTimers();
    const first = reading({
      sets: [],
      deployment: deployment([event(1, { scope: "deployment", event: "startup", message: "backupd starting" })], {
        latestSequence: 1
      })
    });
    const idle = reading({ sets: [], deployment: deployment([], { oldestSequence: 1, latestSequence: 1 }) });
    const getLiveActivity = vi.fn().mockResolvedValueOnce(first).mockResolvedValue(idle);
    renderDock({ getLiveActivity } as unknown as BackupdApi);

    // findBy* polls on a timer, and the timers here are fake, so the
    // reading is awaited by flushing the microtask queue instead.
    await act(async () => {});
    expect(screen.getByText(/backupd starting/)).toBeInTheDocument();
    await act(async () => {
      vi.advanceTimersByTime(1100);
    });
    await act(async () => {
      vi.advanceTimersByTime(1100);
    });
    expect(getLiveActivity).toHaveBeenLastCalledWith({ since: 1, limit: 200 });
    vi.useRealTimers();
  });
});

/** A surface that follows one set's feed and nothing else, which is what
 *  the backup set detail page is. */
function OneSetFeed({ setId }: { setId: string }) {
  const feed = useActivityFeed(setId);
  return <span data-testid="lines">{feed.held.get(setId)?.events.length ?? 0}</span>;
}

describe("when a surface changes which set it is asking about", () => {
  it("does not carry the old set's cursor into the new set's question", async () => {
    vi.useFakeTimers();
    // The counter is one counter for the whole process, so a set that has
    // been running all night sits at 412 while a quiet one's own lines
    // stop at 5. Carrying the first cursor into the second question asks
    // for everything after 412 about a set whose lines stop at 5, and is
    // told, correctly and uselessly, that there is nothing.
    const busy = reading({ sets: [set("busy/set", [event(411), event(412)], { latestSequence: 412 })] });
    const quiet = reading({ sets: [set("quiet/set", [event(5)], { latestSequence: 5 })] });
    const getLiveActivity = vi.fn().mockImplementation((options: { setId?: string }) =>
      Promise.resolve(options.setId === "busy/set" ? busy : quiet)
    );
    const api = { getLiveActivity } as unknown as BackupdApi;

    const view = render(
      <ApiProvider api={api}>
        <OneSetFeed setId="busy/set" />
      </ApiProvider>
    );
    await act(async () => {});
    expect(screen.getByTestId("lines").textContent).toBe("2");

    // The route changes. The last reading the hook holds is still the
    // busy set's, because a fetch in flight does not blank what is on
    // screen, and it is an answer to a question nobody is asking any
    // more.
    view.rerender(
      <ApiProvider api={api}>
        <OneSetFeed setId="quiet/set" />
      </ApiProvider>
    );
    await act(async () => {});
    expect(getLiveActivity).toHaveBeenLastCalledWith({ since: 0, limit: 200, setId: "quiet/set" });

    await act(async () => {
      vi.advanceTimersByTime(1100);
    });
    expect(getLiveActivity).toHaveBeenLastCalledWith({ since: 5, limit: 200, setId: "quiet/set" });
    vi.useRealTimers();
  });
});

describe("saying that earlier lines are gone", () => {
  it("keeps saying it while the window starts above what the service still holds", () => {
    // The page holds from 200 up; the service still holds from 100. The
    // lines between are in the service's buffer and not in this window,
    // so what is on screen is not continuous and a later clean reading
    // does not make it so.
    const held = mergeActivity(undefined, set("a/b", [event(200)], { dropped: true, oldestSequence: 100 }));
    const next = mergeActivity(held, set("a/b", [event(201)], { dropped: false, oldestSequence: 100 }));
    expect(next.dropped).toBe(true);
  });

  it("stops saying it once the window holds everything the service does", () => {
    // The flag is sticky on purpose, and this is the one thing that
    // clears it: the page's own window now reaches back at least as far
    // as the service's buffer does, so every line anybody still has is
    // on screen. Left sticky, a single dropped=true (the service used to
    // send one to every first read) put "earlier lines are not held here
    // any more" on the panel for the life of the tab, and a gap warning
    // that fires when there is no gap teaches an operator to ignore the
    // real one.
    const held = mergeActivity(undefined, set("a/b", [event(100), event(101)], { dropped: true, oldestSequence: 100 }));
    expect(held.dropped).toBe(true);
    const next = mergeActivity(held, set("a/b", [event(102)], { dropped: false, oldestSequence: 100 }));
    expect(next.dropped).toBe(false);
  });

  it("still says it the moment the page's own window throws a line away", () => {
    const held = mergeActivity(undefined, set("a/b", [event(1), event(2), event(3)], { oldestSequence: 1 }), 2);
    expect(held.dropped).toBe(true);
  });
});
