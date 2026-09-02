import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { DashboardPage } from "@shared/pages/DashboardPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupManagerApi } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import type { BackupSet } from "@shared/types/backup";
import type { SystemHealth } from "@shared/types/operation";
import { resetGraphForTests } from "@shared/state/graph";
import { backupSetPath } from "@shared/utilities/routes";

/**
 * Issue #245. These two banners existed before this and could not fire:
 * `haltReason` had no producer anywhere in the running system, so a set
 * the manager refuses to connect to rendered as merely stale. They fire
 * now, from a fact core observed and persisted, and each case here has a
 * matching negative so the reason drives which banner appears rather than
 * whether any banner appears.
 */

async function setFixture(overrides: Partial<BackupSet> = {}): Promise<BackupSet> {
  const sets = await createMockApi().listSets();
  // Healthy by default so the only banner on screen is the one under
  // test: the dashboard raises its own role="alert" for a stale set, and
  // two alerts would make findByRole("alert") ambiguous rather than
  // wrong.
  const base = { ...sets[0], state: "healthy" as const };
  const next: BackupSet = { ...base, ...overrides };
  if (!("haltReason" in overrides)) delete next.haltReason;
  return next;
}

function renderDetail(set: BackupSet) {
  const api = createMockApi();
  vi.spyOn(api, "getSet").mockResolvedValue(set);
  return render(
    <MemoryRouter initialEntries={[backupSetPath(set.source, set.set)]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={false} />} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

const idleHealth: SystemHealth = {
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

function renderDashboard(sets: BackupSet[], api: BackupManagerApi = createMockApi()) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <DashboardPage
          health={{ data: idleHealth, error: null, loading: false, reload: () => {} }}
          sets={{ data: sets, error: null, loading: false, reload: () => {} }}
          readOnly={false}
        />
      </ApiProvider>
    </MemoryRouter>
  );
}

describe("the halt banners fire from a reason the service reported (issue #245)", () => {
  afterEach(() => {
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("the detail page names a changed host key when that is the reason", async () => {
    renderDetail(await setFixture({ haltReason: "host-key-changed" }));

    const banner = await screen.findByRole("alert");
    expect(banner.textContent).toMatch(/SSH host key/i);
    expect(banner.textContent).toMatch(/Security warning/i);
    expect(banner.textContent).toMatch(/No remote artifacts will be deleted/i);
  });

  it("the detail page names a rejected login when that is the reason, not a host key", async () => {
    renderDetail(await setFixture({ haltReason: "authentication-failed" }));

    const banner = await screen.findByRole("alert");
    expect(banner.textContent).toMatch(/could not (log in|sign in|authenticate)|rejected/i);
    expect(banner.textContent).not.toMatch(/SSH host key/i);
  });

  it("the detail page names a key-permission problem when that is the reason, not a rejected login (#293)", async () => {
    renderDetail(await setFixture({ haltReason: "key-permissions" }));

    const banner = await screen.findByRole("alert");
    expect(banner.textContent).toMatch(/permission/i);
    expect(banner.textContent).not.toMatch(/SSH host key/i);
    expect(banner.textContent).not.toMatch(/rejected|log in|sign in/i);
  });

  it("the detail page shows no halt banner for a set with no reason", async () => {
    renderDetail(await setFixture());

    // The page has rendered, so a missing banner is a missing banner.
    expect(await screen.findByText("Overview")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("the dashboard raises the host-key alert above the metrics", async () => {
    renderDashboard([await setFixture({ haltReason: "host-key-changed" })]);

    const banner = await screen.findByRole("alert");
    expect(banner.textContent).toMatch(/SSH host key/i);
    expect(screen.getByRole("button", { name: "Review fingerprint" })).toBeTruthy();
  });

  it("the dashboard raises a rejected login under its own words", async () => {
    renderDashboard([await setFixture({ haltReason: "authentication-failed" })]);

    const banner = await screen.findByRole("alert");
    expect(banner.textContent).toMatch(/could not (log in|sign in|authenticate)|rejected/i);
    expect(banner.textContent).not.toMatch(/SSH host key/i);
  });

  // Second defect in the same report (issue #285): DashboardPage.tsx used
  // to pass one hardcoded action, "Review fingerprint", regardless of
  // haltReason, so a rejected-credential halt offered a button for a
  // problem it did not have. A fingerprint has nothing to do with a
  // rejected login, and HaltBanner's own doc is the rule this breaks:
  // actions are for navigating to the evidence, and there is no
  // fingerprint evidence to navigate to here. The right fix is no
  // action, not a relabelled one — an action naming the wrong remedy
  // sends an operator to check the wrong thing.
  it("offers no action for a rejected login, rather than the host-key one", async () => {
    renderDashboard([await setFixture({ haltReason: "authentication-failed" })]);

    await screen.findByRole("alert");
    expect(screen.queryByRole("button", { name: "Review fingerprint" })).toBeNull();
  });

  it("the dashboard raises a key-permission problem under its own words (#293)", async () => {
    renderDashboard([await setFixture({ haltReason: "key-permissions" })]);

    const banner = await screen.findByRole("alert");
    expect(banner.textContent).toMatch(/permission/i);
    expect(banner.textContent).not.toMatch(/SSH host key/i);
  });

  it("the dashboard raises nothing when no set carries a reason", async () => {
    renderDashboard([await setFixture()]);

    expect(await screen.findByRole("region", { name: "Key metrics" })).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
