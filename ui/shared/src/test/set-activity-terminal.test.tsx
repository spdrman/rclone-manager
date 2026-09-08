/**
 * The per-set activity terminal on the backup set detail page, and what
 * `Test Connection` actually did (issue #596).
 *
 * # Why these cases and not a snapshot of the panel
 *
 * The page already had an Activity section: six rows of the durable
 * lifecycle record. What it had nothing of was what the set is DOING, and
 * the button beside it was worse than useless. `onTest={() =>
 * api.testConnection(set.id)}` created a promise and dropped it, so
 * nothing read the outcome, nothing caught a rejection and nothing
 * rendered: in 0.3.2 there is no surface anywhere that draws a connection
 * test result, not even the verdict.
 *
 * So the cases below are written so that a verdict cannot satisfy them.
 * Asserting "something rendered after the click" would pass against a
 * one-line "connection ok", which is exactly the shape this issue exists
 * to replace. Each case asserts the INDIVIDUAL step results: six of them,
 * with their own outcomes, including the skipped ones, which are the
 * whole reason a failed test is legible at all.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { BackupManagerError } from "@shared/api/contracts";
import type { BackupManagerApi } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { resetGraphForTests } from "@shared/state/graph";
import { clearBrowserNoticesForTests } from "@shared/state/browserNotices";
import { backupSetPath } from "@shared/utilities/routes";
import type { LiveActivity, SetActivity, SetActivityEvent } from "@shared/types/activity";

function renderDetail(source: string, set: string, api: BackupManagerApi) {
  return render(
    <MemoryRouter initialEntries={[backupSetPath(source, set)]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={false} />} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

function emptyActivity(setId: string, events: SetActivityEvent[] = []): SetActivity {
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
    events,
    truncated: false,
    dropped: false,
    oldestSequence: events.length > 0 ? events[0].sequence : 0,
    latestSequence: events.length > 0 ? events[events.length - 1].sequence : 0
  };
}

function reading(setId: string, events: SetActivityEvent[] = []): LiveActivity {
  return {
    observedAt: "2026-09-07T09:41:02Z",
    epoch: "epoch-1",
    pollAfterMs: 1000,
    sets: [emptyActivity(setId, events)],
    // Narrowed readings carry no deployment bucket: that is the service's
    // own answer for "a caller that named a set asked about that set".
    deployment: null
  };
}

/** The six lines the engine emits for one connection test, exactly as
 *  core/service's recordConnectionTest shapes them: one event per step,
 *  each carrying its own step, outcome, category and detail. */
function connectionTestEvents(): SetActivityEvent[] {
  const steps: Array<[string, string, string, string]> = [
    ["credentials", "passed", "", "read the private key this backup set's configured key file names"],
    ["resolve", "passed", "", "cicd.example.net is 198.51.100.7 (A)"],
    ["connect", "passed", "", "TCP to 198.51.100.7:22 in 12ms"],
    ["host_key", "failed", "host_key", "the key this server offers is not the one this backup set trusts"],
    ["authenticate", "skipped", "", "the host key did not match, so nothing was offered to this server"],
    ["list", "skipped", "", "never attempted"]
  ];
  return steps.map(([step, outcome, category, detail], i) => ({
    sequence: 100 + i,
    at: "2026-09-07T09:41:0" + i + "Z",
    level: outcome === "passed" ? "info" : "warn",
    event: "connection_test",
    scope: "set",
    // The step states how it went in the engine's own four-value
    // vocabulary and keeps its finer word in step_outcome, which is the
    // shape recordConnectionTest emits since issue #625. The two field
    // names are not interchangeable: `outcome` is the reserved key the
    // record's own mark is written under, and a step supplying a second
    // value there is the duplicate-key bug that rule exists to stop.
    outcome: outcome === "passed" ? "success" : outcome === "skipped" ? "warning" : "error",
    message: "connection test: " + step + " " + outcome,
    fields: {
      backup_set: "production/postgres-primary",
      step,
      step_outcome: outcome,
      detail,
      ...(category ? { category } : {})
    }
  }));
}

async function firstSet() {
  const sets = await createMockApi().listSets();
  return sets[0];
}

describe("the backup set detail page has a terminal about that set", () => {
  afterEach(() => {
    clearBrowserNoticesForTests();
    resetGraphForTests();
    vi.useRealTimers();
  });

  it("follows the live feed narrowed to this set, not the whole deployment", async () => {
    const target = await firstSet();
    const api = createMockApi();
    const live = vi.spyOn(api, "getLiveActivity").mockResolvedValue(reading(target.id));

    renderDetail(target.source, target.set, api);
    await screen.findByLabelText("Activity for " + target.name);

    await waitFor(() => expect(live).toHaveBeenCalled());
    // An empty feed for a set that does not exist reads exactly like a
    // quiet set, which is why the route takes the id and refuses an
    // unknown one. A page that asked for the whole deployment and
    // filtered in the browser would lose that refusal and would carry
    // every other set's traffic across the wire to draw one set.
    expect(live.mock.calls.every(([options]) => options?.setId === target.id)).toBe(true);
  });

  it("starts a fresh cursor when the route moves to another set, so B's log is not read with A's cursor", async () => {
    const sets = await createMockApi().listSets();
    const [a, b] = sets;
    const api = createMockApi();
    const live = vi.spyOn(api, "getLiveActivity").mockImplementation(async (options) =>
      reading(options?.setId ?? a.id, [
        {
          sequence: 412,
          at: "2026-09-07T09:00:00Z",
          level: "info",
          event: "discovery",
          scope: "set",
          message: "discovery",
          fields: { backup_set: options?.setId ?? a.id, discovered: "9", pending: "9" }
        }
      ])
    );

    // A real in-router navigation, the shape this page's other tests
    // already use: React Router keeps the component MOUNTED across it,
    // which is the whole point.
    function Harness() {
      const navigate = useNavigate();
      return (
        <>
          <button onClick={() => navigate(backupSetPath(b.source, b.set))}>go to the second set</button>
          <Routes>
            <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={false} />} />
          </Routes>
        </>
      );
    }

    render(
      <MemoryRouter initialEntries={[backupSetPath(a.source, a.set)]}>
        <ApiProvider api={api}>
          <Harness />
        </ApiProvider>
      </MemoryRouter>
    );
    await screen.findByLabelText("Activity for " + a.name);
    await waitFor(() => expect(live.mock.calls.some(([o]) => o?.setId === a.id)).toBe(true));

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "go to the second set" }));
    });

    // The sequence counter is one counter across every bucket in the
    // process, so carrying A's cursor into a poll about B asks for
    // everything after 412 about a set whose own lines stop lower and is
    // told, correctly, that there is nothing. That reads as a set that
    // has never done anything.
    await waitFor(() => expect(live.mock.calls.some(([o]) => o?.setId === b.id)).toBe(true));
    const forB = live.mock.calls.filter(([o]) => o?.setId === b.id);
    expect(forB[0][0]?.since).toBe(0);
  });

  it("renders the set's own live log, so the page says what the set is doing", async () => {
    const target = await firstSet();
    const api = createMockApi();
    vi.spyOn(api, "getLiveActivity").mockResolvedValue(
      reading(target.id, [
        {
          sequence: 41,
          at: "2026-09-07T09:40:00Z",
          level: "info",
          event: "discovery",
          scope: "set",
          message: "discovery",
          fields: { backup_set: target.id, discovered: "41", pending: "26" }
        }
      ])
    );

    renderDetail(target.source, target.set, api);
    const strip = await screen.findByLabelText("Activity for " + target.name);
    await within(strip).findByText(/discovery complete: 41 artifacts, 26 pending/);
  });
});

describe("Test connection says what it actually did", () => {
  afterEach(() => {
    clearBrowserNoticesForTests();
    resetGraphForTests();
    vi.useRealTimers();
  });

  it("puts every individual step, with its own outcome, in this set's terminal", async () => {
    const target = await firstSet();
    const api = createMockApi();

    // Nothing on the feed until the test has run. The engine records
    // every step BEFORE it answers the request, so the reading taken
    // after the button is pressed is the one that carries them.
    let tested = false;
    vi.spyOn(api, "getLiveActivity").mockImplementation(async () =>
      reading(target.id, tested ? connectionTestEvents() : [])
    );
    vi.spyOn(api, "testConnection").mockImplementation(async () => {
      tested = true;
      // checks is [] and not absent: the shape is one array on both
      // modes of this route now, so a stub that left it off would be a
      // shape no engine answers with.
      return { ok: false, message: "the key this server offers is not the one this backup set trusts", checks: [] };
    });

    renderDetail(target.source, target.set, api);
    const strip = await screen.findByLabelText("Activity for " + target.name);

    fireEvent.click(screen.getByRole("button", { name: "Test connection" }));

    // Six steps, each with its own outcome, and NOT one verdict. This is
    // the assertion a verdict cannot satisfy, which is the point: "the
    // connection test failed" is what 0.3.2 could already have rendered
    // and it is what sends an operator to the wrong afternoon.
    for (const [step, outcome] of [
      ["credentials", "passed"],
      ["resolve", "passed"],
      ["connect", "passed"],
      ["host_key", "failed"],
      ["authenticate", "skipped"],
      ["list", "skipped"]
    ]) {
      await within(strip).findByText(new RegExp("^" + step + "\\s+" + outcome));
    }

    // And the details, because a step name with no sentence under it is
    // a status light. The skipped ones carry theirs too: a surface that
    // draws an unattempted authentication as anything but "this was
    // never tried" has told an operator their credentials are fine on
    // the strength of a step that never ran.
    await within(strip).findByText(/the key this server offers is not the one this backup set trusts/);
    await within(strip).findByText(/the host key did not match, so nothing was offered to this server/);
  });

  it("asks the feed for the steps as soon as the request answers, not on the next tick", async () => {
    const target = await firstSet();
    const api = createMockApi();
    const live = vi.spyOn(api, "getLiveActivity").mockResolvedValue(reading(target.id));
    vi.spyOn(api, "testConnection").mockResolvedValue({ ok: true, checks: [] });

    renderDetail(target.source, target.set, api);
    await screen.findByLabelText("Activity for " + target.name);
    await waitFor(() => expect(live).toHaveBeenCalled());

    const before = live.mock.calls.length;
    fireEvent.click(screen.getByRole("button", { name: "Test connection" }));

    // No timer is advanced anywhere in this test. A panel that waited for
    // the poll interval would leave an operator looking at an unchanged
    // screen for up to ten seconds after pressing a button, which reads
    // exactly like the button that did nothing.
    await waitFor(() => expect(live.mock.calls.length).toBeGreaterThan(before));
  });

  it("writes a refusal that never reached the engine into the same terminal, marked as the browser's", async () => {
    const target = await firstSet();
    const api = createMockApi();
    vi.spyOn(api, "getLiveActivity").mockResolvedValue(reading(target.id));
    // A stale double-submit pair is refused by middleware before any
    // handler runs, so the engine's own event stream knows nothing about
    // it. There is nothing on the server that could have logged this.
    vi.spyOn(api, "testConnection").mockRejectedValue(
      new BackupManagerError({
        code: "CSRF_TOKEN_MISMATCH",
        message: "This request could not be verified.",
        correlationId: "cid_596"
      })
    );

    renderDetail(target.source, target.set, api);
    const strip = await screen.findByLabelText("Activity for " + target.name);

    fireEvent.click(screen.getByRole("button", { name: "Test connection" }));

    // It lands in the log, it is marked as coming from this browser
    // rather than from the engine, and it is not swallowed the way
    // `onClick={() => api.testConnection(s.id)}` swallowed it.
    const line = await within(strip).findByText(/\[browser\]/);
    // The reason, the remediation and the correlation id all reach the
    // log. The correlation id in particular is what makes "send me what
    // the terminal said" a support request that actually helps.
    expect(line.textContent).toMatch(/security token is missing or out of date/);
    expect(line.textContent).toMatch(/Reload the page/);
    expect(line.textContent).toMatch(/cid_596/);
  });

  it("leaves the button usable again once the test has answered", async () => {
    const target = await firstSet();
    const api = createMockApi();
    vi.spyOn(api, "getLiveActivity").mockResolvedValue(reading(target.id));
    vi.spyOn(api, "testConnection").mockResolvedValue({ ok: true, checks: [] });

    renderDetail(target.source, target.set, api);
    await screen.findByLabelText("Activity for " + target.name);

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Test connection" }));
    });
    await waitFor(() => expect((screen.getByRole("button", { name: "Test connection" }) as HTMLButtonElement).disabled).toBe(false));
  });
});
