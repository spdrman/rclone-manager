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
    detectedAt: "2026-08-28T02:06:00+02:00",
    remoteSourceRetained: true
  }
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

/** Issue #220. "Retry ingestion" re-fetches from the remote, which is
 *  useless when the remote is gone or when the quarantine was the mistake.
 *  "Reinstate" is the other answer: keep the local copy and trust it again.
 *
 *  The outcome of a reinstate is never a plain success/failure, because a
 *  request that reaches the backend and comes back saying "the copy is
 *  bad" is not an error, and an operator who saw only a spinner stop would
 *  be left guessing. These tests pin all three outcomes as visibly
 *  different. */
describe("QuarantinePage: reinstating a backup whose local copy is intact", () => {
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("reloads the list and says so when the backup is trusted again", async () => {
    const reload = vi.fn();
    const reinstate = vi.fn(() =>
      Promise.resolve({
        reinstated: true,
        checked: true,
        passed: true,
        state: "COMMITTED",
        reason: "recomputed hash still matches the hash recorded at verification"
      })
    );
    const api = { ...createMockApi(), reinstate };

    renderPage(api, reload);

    const user = userEvent.setup();
    await act(async () => {
      await user.click(screen.getByRole("button", { name: "Reinstate" }));
    });
    await act(async () => {});

    expect(reinstate).toHaveBeenCalledWith(ARTIFACT.id);
    expect(reload).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("status")).toHaveTextContent(/trusted again/i);
  });

  it("reports the verdict, and does not claim success, when the checks do not pass", async () => {
    const reload = vi.fn();
    const reinstate = vi.fn(() =>
      Promise.resolve({
        reinstated: false,
        checked: true,
        passed: false,
        state: "",
        reason: "local final file now hashes to abc, but the sha256 hash recorded at verification was def"
      })
    );
    const api = { ...createMockApi(), reinstate };

    renderPage(api, reload);

    const user = userEvent.setup();
    await act(async () => {
      await user.click(screen.getByRole("button", { name: "Reinstate" }));
    });
    await act(async () => {});

    expect(reload).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent(/hashes to abc/i);
  });

  it("shows a visible error and never calls reload when the request rejects", async () => {
    const reload = vi.fn();
    const reinstate = vi.fn(() => Promise.reject(new Error("network down")));
    const api = { ...createMockApi(), reinstate };

    renderPage(api, reload);

    const user = userEvent.setup();
    await act(async () => {
      await user.click(screen.getByRole("button", { name: "Reinstate" }));
    });
    await act(async () => {});

    expect(reinstate).toHaveBeenCalledWith(ARTIFACT.id);
    expect(reload).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent(/could not reinstate/i);
  });

  it("says on the page that a reinstated backup never releases its remote source", () => {
    renderPage(createMockApi(), vi.fn());
    expect(screen.getByText(/never trigger remote deletion/i)).toBeInTheDocument();
    expect(screen.getByText(/reinstated backup keeps its remote source/i)).toBeInTheDocument();
  });
});
