/**
 * Revalidate and Retry ingestion, when the request is rejected.
 *
 * Both resolve with nothing worth reading, so their only visible effect is
 * the list reloading. That made a rejection invisible: the button
 * un-disabled itself and the page sat there, which is indistinguishable
 * from a click that never registered. Each case therefore asserts two
 * things, that a failure is stated and that the list is NOT reloaded,
 * because reloading on a failure would put the old rows back and complete
 * the illusion.
 *
 * The success case is the positive control. Without it a page that always
 * showed an error and never reloaded would pass everything above.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QuarantinePage } from "@shared/pages/QuarantinePage";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import type { AsyncState } from "@shared/hooks/useAsync";
import type { BackupManagerApi } from "@shared/api/contracts";
import type { BackupArtifact } from "@shared/types/backup";

const ARTIFACT: BackupArtifact = {
  id: "art_a",
  setId: "set_test",
  setName: "Production PostgreSQL",
  filename: "pg-2026-08-28.dump.zst",
  remoteOriginalPath: "/backups/postgresql/pg-2026-08-28.dump.zst",
  localPath: "/data/backups/production/postgres/pg-2026-08-28.dump.zst",
  producedAt: "2026-08-28T02:00:00+02:00",
  receivedAt: "2026-08-28T02:05:00+02:00",
  sizeBytes: 1024,
  checksum: "deadbeef",
  checksumAlgorithm: "sha256",
  validation: "failed",
  retentionClasses: [],
  remoteSourceRemovedAt: null,
  quarantine: {
    reason: "checksum-mismatch",
    detail: "sha256 mismatch: local file hashes to deadbeef, remote reports feedface",
    detectedAt: "2026-08-28T02:06:00+02:00",
    remoteSourceRetained: true
  },
  placements: [
    {
      medium: "local", mediumType: "local",
      location: "/data/backups/production/postgres/pg-2026-08-28.dump.zst",
      sizeBytes: 1024, storageClass: "",
      verificationClass: null, verifiedAt: null,
      access: "immediate", status: "ACTIVE"
    }
  ]
};

function renderPage(api: BackupManagerApi, reload: () => void) {
  const quarantine: AsyncState<BackupArtifact[]> = {
    data: [ARTIFACT],
    error: null,
    loading: false,
    reload
  };
  return render(
    <ApiProvider api={api}>
      <QuarantinePage readOnly={false} quarantine={quarantine} />
    </ApiProvider>
  );
}

/** Mandatory review, PR #147 — Revalidate/Retry ingestion were wired as a
 *  bare `.then(reload)` with no `.catch()`. A rejection then left
 *  `reload` never called and surfaced only as an unhandled promise
 *  rejection: no reload, no error state, nothing visible at all. These
 *  tests pin the fix: a rejection is caught, produces a visible error,
 *  and never reaches the reload/success path. */
describe("QuarantinePage: revalidate/retry surface a failure instead of going silent", () => {
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("shows a visible error and never calls reload when Revalidate's request rejects", async () => {
    const reload = vi.fn();
    const revalidate = vi.fn(() => Promise.reject(new Error("network down")));
    const api = { ...createMockApi(), revalidate };

    renderPage(api, reload);

    const user = userEvent.setup();
    await act(async () => {
      await user.click(screen.getByRole("button", { name: "Revalidate" }));
    });
    // Let the rejection's .catch() microtask settle. If the component ever
    // regresses to a bare `.then(reload)` with no `.catch()`, this
    // rejection surfaces as an actual unhandled rejection instead — which
    // vitest treats as a failing test on its own.
    await act(async () => {});

    expect(revalidate).toHaveBeenCalledWith(ARTIFACT.id);
    expect(reload).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent(/could not revalidate/i);
  });

  it("shows a visible error and never calls reload when Retry ingestion's request rejects", async () => {
    const reload = vi.fn();
    const retryIngestion = vi.fn(() => Promise.reject(new Error("validation error")));
    const api = { ...createMockApi(), retryIngestion };

    renderPage(api, reload);

    const user = userEvent.setup();
    await act(async () => {
      await user.click(screen.getByRole("button", { name: "Retry ingestion" }));
    });
    await act(async () => {});

    expect(retryIngestion).toHaveBeenCalledWith(ARTIFACT.id);
    expect(reload).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent(/could not retry ingestion/i);
  });

  it("still reloads on success and shows no error banner", async () => {
    const reload = vi.fn();
    const revalidate = vi.fn(() => Promise.resolve());
    const api = { ...createMockApi(), revalidate };

    renderPage(api, reload);

    const user = userEvent.setup();
    await act(async () => {
      await user.click(screen.getByRole("button", { name: "Revalidate" }));
    });
    await act(async () => {});

    expect(reload).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
