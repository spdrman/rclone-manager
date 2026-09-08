/**
 * Issue #598. The Activity page failing on a real NAS with nothing but
 * "Backup Manager could not complete that request." and "correlation id
 * unavailable" under it.
 *
 * Every case here drives the same shape: `listActivity` rejects with an
 * exception that is NOT a `BackupManagerError`, which is what a dropped
 * connection, a truncated body and a mapper that threw all look like from
 * a page's side. What is asserted is never that the page failed. It is
 * that what reaches the operator names something: the exception's own
 * words are on screen, they can be copied off it, and no correlation id is
 * offered, because a failure that carried none has no id anybody could
 * look up.
 *
 * The dashboard cases exist because that surface is the quieter half of
 * the same bug. It runs the identical `useAsync(() => api.listActivity())`
 * and never reads `.error`, so on the deployment this issue was reported
 * from the Recent activity panel is drawing empty right now, which is
 * indistinguishable from a NAS where nothing has ever happened.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { ActivityPage } from "@shared/pages/ActivityPage";
import { DashboardPage } from "@shared/pages/DashboardPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { resetGraphForTests } from "@shared/state/graph";
import type { AsyncState } from "@shared/hooks/useAsync";
import type { BackupSet } from "@shared/types/backup";
import type { SystemHealth } from "@shared/types/operation";

/** The exact shape the NAS produced: a 2xx whose body was not the JSON
 *  this build expected, so `await res.json()` threw before any typed
 *  envelope existed. */
function unreadableBody() {
  return new SyntaxError("Unexpected token '<', \"<!doctype \"... is not valid JSON");
}

const HEALTH: AsyncState<SystemHealth> = {
  data: null,
  error: null,
  loading: false,
  reload: () => {}
};

/** One configured backup set, because a dashboard with none renders the
 *  "No backup sets yet" empty state instead of its panels, and the panel
 *  under test is one of the ones it would not draw. */
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
  excludePatterns: ["*.tmp"],
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
  sshKeyId: "key_fixture_1"
};

const SETS: AsyncState<BackupSet[]> = {
  data: [SET],
  error: null,
  loading: false,
  reload: () => {}
};

function renderActivity(rejectWith: unknown) {
  const api = createMockApi();
  vi.spyOn(api, "listActivity").mockRejectedValue(rejectWith);
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

function renderDashboard(rejectWith: unknown) {
  const api = createMockApi();
  vi.spyOn(api, "listActivity").mockRejectedValue(rejectWith);
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <PlatformProvider bridge={genericBridge}>
          <DashboardPage health={HEALTH} sets={SETS} readOnly={false} />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

describe("the Activity page says what actually failed", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("never renders the literal correlation id 'unavailable'", async () => {
    renderActivity(unreadableBody());
    await act(async () => {});

    expect(screen.getByRole("alert")).toBeTruthy();
    // The whole of what an operator got on 0.3.2, and the reason this
    // issue exists: an Advanced details disclosure with a string behind it
    // that appears in no log anywhere.
    expect(document.body.textContent).not.toContain("correlation id unavailable");
    expect(document.body.textContent).not.toContain("unavailable");
  });

  it("puts the exception's own words where they can be read", async () => {
    renderActivity(unreadableBody());
    await act(async () => {});

    const alert = screen.getByRole("alert");
    expect(within(alert).getByText(/Advanced details/)).toBeTruthy();
    expect(alert.textContent).toContain("SyntaxError");
    expect(alert.textContent).toContain("is not valid JSON");
  });

  it("does not substitute one fixed sentence for every untyped failure", async () => {
    renderActivity(unreadableBody());
    await act(async () => {});
    const unreadable = screen.getByRole("alert").textContent ?? "";

    cleanup();
    renderActivity(new TypeError("Failed to fetch"));
    await act(async () => {});
    const noAnswer = screen.getByRole("alert").textContent ?? "";

    // "the service did not answer" and "this build could not read what it
    // answered" are different problems for whoever is fixing them, and
    // they used to render as the same eleven words.
    expect(unreadable).not.toBe(noAnswer);
    expect(noAnswer).toContain("Failed to fetch");
  });

  it("offers a copy button, and says so when the browser has no clipboard", async () => {
    renderActivity(unreadableBody());
    await act(async () => {});

    const copy = screen.getByRole("button", { name: /copy details/i });
    // Every NAS deployment on a plain http:// origin: navigator.clipboard
    // is not defined at all, so a button whose entire job is getting a
    // diagnostic off the screen has to say it could not rather than look
    // like it worked.
    await userEvent.click(copy);
    await act(async () => {});
    expect(screen.getByText(/could not copy/i)).toBeTruthy();
  });

  it("still keeps a typed refusal's own message and correlation id", async () => {
    const { BackupManagerError } = await import("@shared/api/contracts");
    renderActivity(
      new BackupManagerError({ code: "INTERNAL", message: "failed to list activity", correlationId: "cid_real42" })
    );
    await act(async () => {});

    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("failed to list activity");
    expect(alert.textContent).toContain("cid_real42");
  });
});

describe("the dashboard's Recent activity panel does not swallow the same failure", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("distinguishes a panel that could not load from a panel with nothing in it", async () => {
    renderDashboard(unreadableBody());
    await act(async () => {});

    const panel = screen.getByRole("region", { name: "Recent activity" });
    expect(within(panel).getByRole("alert")).toBeTruthy();
    expect(panel.textContent).toContain("SyntaxError");
    expect(panel.textContent).not.toContain("unavailable");
  });

  it("draws no alert at all when the feed simply has nothing in it", async () => {
    const api = createMockApi();
    vi.spyOn(api, "listActivity").mockResolvedValue([]);
    render(
      <MemoryRouter>
        <ApiProvider api={api}>
          <PlatformProvider bridge={genericBridge}>
            <DashboardPage health={HEALTH} sets={SETS} readOnly={false} />
          </PlatformProvider>
        </ApiProvider>
      </MemoryRouter>
    );
    await act(async () => {});

    const panel = screen.getByRole("region", { name: "Recent activity" });
    expect(within(panel).queryByRole("alert")).toBeNull();
  });
});

/**
 * The backstop, driven directly.
 *
 * `ApiError.correlationId` used to be a required field, so every caller
 * with no id to give wrote one down, and they all wrote the same literal.
 * The constructors this issue owns are fixed. Four more live in retention
 * and settings files another lane is working in, and the rule belongs
 * where the id is rendered anyway: a surface written next month will have
 * the same temptation, and this is what stops it reaching an operator.
 */
describe("ErrorState refuses an id that is not an id", () => {
  afterEach(cleanup);

  it("offers no advanced details for the literal 'unavailable'", async () => {
    const { ErrorState } = await import("@shared/components/EmptyState");
    render(<ErrorState message="Something failed." correlationId="unavailable" />);

    expect(screen.queryByText("Advanced details")).toBeNull();
    expect(document.body.textContent).not.toContain("unavailable");
  });

  it("shows a real id, which is the whole point of the check above", async () => {
    const { ErrorState } = await import("@shared/components/EmptyState");
    render(<ErrorState message="Something failed." correlationId="cid_realOne" />);

    expect(screen.getByText("Advanced details")).toBeTruthy();
    expect(document.body.textContent).toContain("cid_realOne");
  });

  it("offers a disclosure for a detail even when there is no id at all", async () => {
    const { ErrorState } = await import("@shared/components/EmptyState");
    render(<ErrorState message="Something failed." detail="TypeError: Failed to fetch" />);

    expect(screen.getByText("Advanced details")).toBeTruthy();
    expect(document.body.textContent).toContain("TypeError: Failed to fetch");
    expect(document.body.textContent).not.toContain("correlation id");
  });

  it("offers no disclosure when the failure carried neither", async () => {
    const { ErrorState } = await import("@shared/components/EmptyState");
    render(<ErrorState message="Something failed." />);

    expect(screen.queryByText("Advanced details")).toBeNull();
  });
});
