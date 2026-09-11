/**
 * Issue #730. The Activity page used to call `listActivity()` with no
 * bound at all and render whatever came back, which on a deployment that
 * has been running a year is the entire append-only transition log in one
 * response — the payload half of the read that failed on the reporter's
 * NAS.
 *
 * So the page now asks for a page and follows the cursor. What is
 * asserted here is the observable half of that: the first request is
 * bounded, the control that appears is the only way to reach older events
 * and it sends the cursor the service handed over, an appended page does
 * not replace the one already on screen, the control goes away when the
 * record ends, and the filters above the list still apply to everything
 * loaded rather than only to the newest page.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { ActivityPage } from "@shared/pages/ActivityPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { resetGraphForTests } from "@shared/state/graph";
import type { ActivityQuery, BackupManagerApi } from "@shared/api/contracts";
import type { ActivityEvent } from "@shared/types/operation";

function event(id: string, text: string, severity: ActivityEvent["severity"]): ActivityEvent {
  return {
    id,
    at: "2026-08-29T04:12:08+02:00",
    type: "backup-discovered",
    severity,
    setId: "production/postgres-primary",
    setName: "Production PostgreSQL",
    text,
    detail: "",
    correlationId: ""
  };
}

/** A four-event record served two at a time, keyed on the cursor exactly
 *  as the service keys on it: the id of the last event handed over. The
 *  page size is the FIXTURE's, not the page's, which is the point — a
 *  client must page on what the response says and not on the number it
 *  asked for. */
const RECORD = [
  event("ev_1", "Newest event", "info"),
  event("ev_2", "Second event", "info"),
  event("ev_3", "Older error", "error"),
  event("ev_4", "Oldest event", "info")
];

function pagingApi(): { api: BackupManagerApi; asked: ActivityQuery[] } {
  const api = createMockApi();
  const asked: ActivityQuery[] = [];
  vi.spyOn(api, "listActivity").mockImplementation((query?: ActivityQuery) => {
    asked.push(query ?? {});
    const from = query?.before ? RECORD.findIndex((e) => e.id === query.before) + 1 : 0;
    const events = RECORD.slice(from, from + 2);
    const last = events[events.length - 1];
    return Promise.resolve(
      events.length === 2 && from + 2 < RECORD.length
        ? { events, nextCursor: last.id }
        : { events }
    );
  });
  return { api, asked };
}

function renderPage(api: BackupManagerApi) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <PlatformProvider bridge={genericBridge}>
          <ActivityPage />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

describe("the Activity page reads the durable record one page at a time", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("asks for a bounded first page and offers the cursor as the way to reach older events", async () => {
    const { api, asked } = pagingApi();
    renderPage(api);
    await act(async () => {});

    // Bounded: the request names a count. An unbounded first read is the
    // defect, and it looks identical on screen.
    expect(asked[0].limit).toBeGreaterThan(0);
    expect(asked[0].before).toBeUndefined();
    expect(screen.getByText("Newest event")).toBeTruthy();
    expect(screen.queryByText("Older error")).toBeNull();

    await act(async () => {
      await userEvent.click(screen.getByRole("button", { name: /load older events/i }));
    });

    // The cursor the first response carried, sent back verbatim.
    expect(asked[1].before).toBe("ev_2");
    // Appended, not replaced: the page an operator was already reading
    // must still be there under the one they just asked for.
    expect(screen.getByText("Newest event")).toBeTruthy();
    expect(screen.getByText("Older error")).toBeTruthy();
  });

  it("stops offering the control once the record has ended", async () => {
    const { api } = pagingApi();
    renderPage(api);
    await act(async () => {});

    await act(async () => {
      await userEvent.click(screen.getByRole("button", { name: /load older events/i }));
    });

    // The second page is the last one, so it carried no cursor: a control
    // still offered here would fetch nothing and say nothing about why.
    expect(screen.getByText("Oldest event")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /load older events/i })).toBeNull();
  });

  it("filters across every page loaded, not only the newest one", async () => {
    const { api } = pagingApi();
    renderPage(api);
    await act(async () => {});

    await act(async () => {
      await userEvent.click(screen.getByRole("button", { name: /load older events/i }));
    });
    await act(async () => {
      await userEvent.selectOptions(screen.getByLabelText("Severity"), "2");
    });

    // The only error in the record is on the second page. A filter
    // applied to the first page alone would blank the list an operator
    // just fetched specifically to search.
    expect(screen.getByText("Older error")).toBeTruthy();
    expect(screen.queryByText("Newest event")).toBeNull();
  });
});
