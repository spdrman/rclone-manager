import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QuarantinePage } from "@shared/pages/QuarantinePage";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import type { AsyncState } from "@shared/hooks/useAsync";
import type { ArtifactReinstatement, BackupManagerApi } from "@shared/api/contracts";
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

const REINSTATED: ArtifactReinstatement = {
  reinstated: true,
  checked: true,
  passed: true,
  state: "COMMITTED",
  reason: "recomputed hash still matches the hash recorded at verification"
};

const REFUSED: ArtifactReinstatement = {
  reinstated: false,
  checked: true,
  passed: false,
  state: "",
  reason: "the durable local copy no longer matches the hash recorded at verification"
};

function renderPage(api: BackupManagerApi, reload: () => void, readOnly = false) {
  const quarantine: AsyncState<BackupArtifact[]> = {
    data: [ARTIFACT],
    error: null,
    loading: false,
    reload
  };
  return render(
    <ApiProvider api={api}>
      <QuarantinePage readOnly={readOnly} quarantine={quarantine} />
    </ApiProvider>
  );
}

function rowActions() {
  return within(screen.getAllByRole("row")[1]).getAllByRole("button");
}

async function openConfirmation() {
  const user = userEvent.setup();
  await act(async () => {
    await user.click(screen.getByRole("button", { name: "Reinstate…" }));
  });
  return user;
}

/**
 * Issue #229. #220 built the whole reinstatement path and left the operator
 * with no way to reach it, because reinstating is the one recovery action
 * that costs something permanently: FR-15's `DeleteRemote` refuses a
 * reinstated artifact from then on, so its remote source is preserved for
 * good (ADR 0004).
 *
 * These tests pin the two halves of that. The cost is disclosed BEFORE the
 * action is taken, through the same ConfirmationDialog tier this UI already
 * uses for "Remove set configuration" and "Rebuild catalog", with the trade
 * named in the confirm button (§35). And the three outcomes stay visibly
 * different: a rejected request is an error, a refusal is a verdict about
 * the backup, and only a real reinstatement reloads the list and reports
 * that the remote source is now kept for good.
 */
describe("QuarantinePage: Reinstate is offered, and never as a fourth ordinary button", () => {
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("offers Reinstate alongside the three existing actions, in last position", () => {
    renderPage(createMockApi(), vi.fn());

    expect(rowActions().map((b) => b.textContent)).toEqual([
      "Inspect",
      "Revalidate",
      "Retry ingestion",
      "▲Reinstate…"
    ]);
  });

  it("marks Reinstate as the one action that opens a confirmation and carries a caution glyph", () => {
    renderPage(createMockApi(), vi.fn());

    const reinstate = screen.getByRole("button", { name: "Reinstate…" });
    // The ellipsis is this UI's existing "this opens a confirmation" affordance
    // ("Apply retention now…", "Remove set configuration…"), and Reinstate is
    // the only row action that has one: the other three take effect on click.
    const withEllipsis = rowActions().filter((b) => /…$/.test(b.textContent ?? ""));
    expect(withEllipsis).toEqual([reinstate]);

    // A glyph and the caution tier, so it cannot be mistaken for another
    // Retry ingestion at a glance. Decorative only: the accessible name
    // stays "Reinstate…".
    expect(reinstate.className).toContain("btn--caution");
    expect(within(reinstate).getByText("▲")).toHaveAttribute("aria-hidden", "true");
    const decorated = rowActions().filter((b) => b.querySelector("[aria-hidden='true']") !== null);
    expect(decorated).toEqual([reinstate]);
  });

  it("states on the page itself that a reinstated backup keeps its remote source for good", () => {
    renderPage(createMockApi(), vi.fn());

    expect(
      screen.getByText(/reinstated backup keeps its remote source for good/i)
    ).toBeTruthy();
  });

  it("disables Reinstate on a read-only instance, like every other write action here", () => {
    renderPage(createMockApi(), vi.fn(), true);

    expect(screen.getByRole("button", { name: "Reinstate…" })).toBeDisabled();
  });
});

describe("QuarantinePage: the confirmation discloses the permanent forfeiture", () => {
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("asks nothing of the backend until the confirmation is taken", async () => {
    const reload = vi.fn();
    const reinstate = vi.fn(() => Promise.resolve(REINSTATED));
    renderPage({ ...createMockApi(), reinstate }, reload);

    await openConfirmation();

    expect(reinstate).not.toHaveBeenCalled();
    expect(reload).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog")).toBeTruthy();
  });

  it("names the forfeiture in the dialog and the trade in the confirm button", async () => {
    renderPage(createMockApi(), vi.fn());

    await openConfirmation();
    const dialog = within(screen.getByRole("dialog"));

    expect(dialog.getByText(/permanently forfeits/i)).toBeTruthy();
    expect(dialog.getByText(/never delete it/i)).toBeTruthy();
    // §35: the consequence is in the confirm button, and "OK" is never
    // acceptable. Naming the button "Reinstate" alone would hide the half of
    // the deal the operator is actually paying.
    expect(dialog.getByRole("button", { name: /keep the remote source/i })).toBeTruthy();
    expect(dialog.queryByRole("button", { name: /^(OK|Yes|Confirm|Submit)$/ })).toBeNull();
  });

  it("cancelling takes no action at all", async () => {
    const reload = vi.fn();
    const reinstate = vi.fn(() => Promise.resolve(REINSTATED));
    renderPage({ ...createMockApi(), reinstate }, reload);

    const user = await openConfirmation();
    await act(async () => {
      await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Cancel" }));
    });

    expect(reinstate).not.toHaveBeenCalled();
    expect(reload).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("status")).toBeNull();
  });
});

describe("QuarantinePage: the three reinstatement outcomes stay visibly different", () => {
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  async function confirm(api: BackupManagerApi, reload: () => void) {
    renderPage(api, reload);
    const user = await openConfirmation();
    await act(async () => {
      await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: /keep the remote source/i }));
    });
    await act(async () => {});
  }

  it("reports a real reinstatement as a success that names the remote source it just bought", async () => {
    const reload = vi.fn();
    const reinstate = vi.fn(() => Promise.resolve(REINSTATED));

    await confirm({ ...createMockApi(), reinstate }, reload);

    expect(reinstate).toHaveBeenCalledWith(ARTIFACT.id);
    expect(reload).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("dialog")).toBeNull();

    const notice = within(screen.getByRole("status"));
    expect(notice.getByText(new RegExp(ARTIFACT.filename))).toBeTruthy();
    expect(notice.getByText(new RegExp(REINSTATED.reason))).toBeTruthy();
    expect(notice.getByText(/kept for good/i)).toBeTruthy();
    // A success is not an alert. The other two outcomes are.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("reports a refusal as a verdict about the backup, not as a failed request", async () => {
    const reload = vi.fn();
    const reinstate = vi.fn(() => Promise.resolve(REFUSED));

    await confirm({ ...createMockApi(), reinstate }, reload);

    // The call succeeded, so nothing here may read as a broken request, and
    // nothing moved, so the list is not re-read and no success is claimed.
    expect(reload).not.toHaveBeenCalled();
    expect(screen.queryByRole("status")).toBeNull();

    const verdict = within(screen.getByRole("alert"));
    expect(verdict.getByText(/stays in quarantine/i)).toBeTruthy();
    expect(verdict.getByText(new RegExp(REFUSED.reason))).toBeTruthy();
    expect(verdict.queryByText(/could not reinstate/i)).toBeNull();
  });

  it("reports a rejected request as a failure, and never as a verdict", async () => {
    const reload = vi.fn();
    const reinstate = vi.fn(() => Promise.reject(new Error("network down")));

    await confirm({ ...createMockApi(), reinstate }, reload);

    expect(reload).not.toHaveBeenCalled();
    expect(screen.queryByRole("status")).toBeNull();

    const failure = within(screen.getByRole("alert"));
    expect(failure.getByText(/could not reinstate/i)).toBeTruthy();
    // "the checks failed" is a claim about the backup, and a request that
    // never got an answer is not entitled to make it.
    expect(failure.queryByText(/stays in quarantine/i)).toBeNull();
  });

  it("clears a previous outcome when another action is taken", async () => {
    const reload = vi.fn();
    const reinstate = vi.fn(() => Promise.resolve(REFUSED));
    const api = { ...createMockApi(), reinstate };

    await confirm(api, reload);
    expect(screen.getByRole("alert")).toBeTruthy();

    const user = userEvent.setup();
    await act(async () => {
      await user.click(screen.getByRole("button", { name: "Revalidate" }));
    });
    await act(async () => {});

    expect(screen.queryByRole("alert")).toBeNull();
  });
});
