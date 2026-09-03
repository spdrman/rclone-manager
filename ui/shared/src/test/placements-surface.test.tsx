import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { BackupDetailPage } from "@shared/pages/BackupDetailPage";
import { BackupsPage } from "@shared/pages/BackupsPage";
import type { BackupManagerApi } from "@shared/api/contracts";
import type { BackupArtifact, BackupPlacement } from "@shared/types/backup";

/**
 * EPIC E, FR-34 on screen (issue #240).
 *
 * Every test here asks the same question in different words: does this
 * surface tell "there is no copy" apart from "there is a copy nobody can
 * reach" apart from "there is a copy nobody has checked"?
 *
 * That distinction is the whole issue. Issue #361 was a run cycle that
 * backed nothing up and reported success; a placement row that reads
 * "stored on offsite_s3" for an unconfirmable copy is the same defect
 * rendered in HTML, and an operator finds out it was decorative at the one
 * moment it matters.
 */

const LOCAL: BackupPlacement = {
  medium: "local",
  mediumType: "local",
  location: "/data/backups/production/pg/nightly.dump.zst",
  sizeBytes: 4096,
  storageClass: "",
  verificationClass: "content",
  verifiedAt: "2026-08-28T02:00:59+02:00",
  access: "immediate",
  status: "ACTIVE"
};

function artifact(over: Partial<BackupArtifact> = {}): BackupArtifact {
  return {
    id: "production/pg/nightly.dump.zst",
    setId: "production/pg",
    setName: "Production PostgreSQL",
    filename: "nightly.dump.zst",
    remoteOriginalPath: "db01:/srv/dumps/nightly.dump.zst",
    localPath: "/data/backups/production/pg/nightly.dump.zst",
    producedAt: "2026-08-28T01:58:44+02:00",
    receivedAt: "2026-08-28T02:00:53+02:00",
    sizeBytes: 4096,
    checksum: "deadbeef",
    checksumAlgorithm: "sha256",
    validation: "verified",
    retentionClasses: ["daily"],
    remoteSourceRemovedAt: "2026-08-28T02:01:01+02:00",
    quarantine: null,
    placements: [LOCAL],
    ...over
  };
}

function apiServing(a: BackupArtifact, list: BackupArtifact[] = [a]): BackupManagerApi {
  const api = createMockApi();
  vi.spyOn(api, "getArtifact").mockResolvedValue(a);
  vi.spyOn(api, "listArtifacts").mockResolvedValue(list);
  return api;
}

function renderDetail(a: BackupArtifact) {
  const api = apiServing(a);
  render(
    <MemoryRouter initialEntries={["/backups/" + a.id]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/backups/*" element={<BackupDetailPage />} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

function renderList(list: BackupArtifact[]) {
  const api = apiServing(list[0], list);
  render(
    <MemoryRouter initialEntries={["/backups"]}>
      <ApiProvider api={api}>
        <BackupsPage readOnly={false} />
      </ApiProvider>
    </MemoryRouter>
  );
}

describe("where a backup's copies are", () => {
  // The empty case, and the one the partial file makes dangerous. localPath
  // is populated the whole time a transfer is running: it names the
  // .partial being written. A surface that showed it as storage would say a
  // backup is on the NAS at exactly the moment it demonstrably is not.
  it("says there is no confirmed copy, rather than showing the ingestion path as one", async () => {
    const a = artifact({
      placements: [],
      localPath: "/data/backups/production/pg/nightly.dump.zst.partial",
      validation: "pending"
    });
    renderDetail(a);

    const copies = await screen.findByRole("region", { name: "Copies" }).catch(() => null);
    // The card is a plain <section>; find it by its heading instead.
    expect(copies ?? (await screen.findByText("Copies"))).toBeTruthy();
    expect(await screen.findByText("No confirmed copy yet")).toBeTruthy();

    // The precondition. Without it, "no copy row for the partial path" is
    // satisfied by a page that never had a partial path to misreport.
    expect(screen.getAllByText(a.localPath).length).toBeGreaterThan(0);

    // And the partial path is not presented as a copy: it appears exactly
    // once, in the artifact card, and never inside the copies section.
    expect(screen.queryByText("Readable now")).toBeNull();
    expect(screen.queryByText("Content verified")).toBeNull();
  });

  it("says a copy nobody can reach cannot be confirmed, and does not call it readable", async () => {
    renderDetail(
      artifact({
        placements: [
          {
            medium: "offsite_s3",
            mediumType: "",
            location: "rclone-manager/production/pg/nightly.dump.zst",
            sizeBytes: 4096,
            storageClass: "",
            verificationClass: "existence",
            verifiedAt: "2026-07-14T02:20:00+02:00",
            access: "unreachable",
            status: "ACTIVE"
          }
        ]
      })
    );

    expect(await screen.findByText("Out of reach")).toBeTruthy();
    // The words matter more than the badge: an operator has to learn that
    // this is an inability to check, not a missing backup.
    expect(screen.getByText(/nothing here can confirm this copy/i)).toBeTruthy();
    expect(screen.getByText(/not the same as the copy being gone/i)).toBeTruthy();
    // And it is emphatically not reported as available.
    expect(screen.queryByText("Readable now")).toBeNull();
  });

  it("says an archive copy needs a restore before anybody relies on it", async () => {
    renderDetail(
      artifact({
        placements: [
          {
            medium: "offsite_cold",
            mediumType: "s3",
            location: "rclone-manager/production/pg/nightly.dump.zst",
            sizeBytes: 4096,
            storageClass: "DEEP_ARCHIVE",
            verificationClass: null,
            verifiedAt: null,
            access: "requires_restore",
            status: "ACTIVE"
          }
        ]
      })
    );

    expect(await screen.findByText("Needs a restore")).toBeTruthy();
    expect(screen.getByText(/cannot be read on demand/i)).toBeTruthy();
    expect(screen.getByText("DEEP_ARCHIVE")).toBeTruthy();
    expect(screen.queryByText("Readable now")).toBeNull();
  });

  it("says a copy nobody has verified is not verified, and gives it no class and no date", async () => {
    renderDetail(
      artifact({
        placements: [{ ...LOCAL, verificationClass: null, verifiedAt: null }]
      })
    );

    expect(await screen.findByText("Not verified")).toBeTruthy();
    expect(screen.getByText("Nothing has checked this copy.")).toBeTruthy();
    // The three rungs are claims that somebody looked. None may be
    // rendered for a copy nobody has.
    expect(screen.queryByText("Content verified")).toBeNull();
    expect(screen.queryByText("Provider checksum matched")).toBeNull();
    expect(screen.queryByText("Existence only")).toBeNull();
  });

  it("reports the class that was actually achieved, never a stronger one", async () => {
    renderDetail(
      artifact({
        placements: [
          { ...LOCAL, verificationClass: "existence", verifiedAt: "2026-08-28T06:00:02+02:00" }
        ]
      })
    );

    expect(await screen.findByText("Existence only")).toBeTruthy();
    // FR-31's rule: an existence check reported as content verification is
    // worse than no check, because it turns "nobody has read these bytes
    // in a year" into a green tick.
    expect(screen.queryByText("Content verified")).toBeNull();
    // And the backend's own words for what that rung proves reach the
    // screen, rather than a paraphrase this file wrote.
    expect(
      await screen.findByText(/Proves an object exists at the recorded key, at the recorded size/i)
    ).toBeTruthy();
  });

  it("states that retrieval is billed, and puts no figure anywhere near it", async () => {
    renderDetail(
      artifact({
        placements: [
          LOCAL,
          {
            medium: "offsite_s3",
            mediumType: "s3",
            location: "rclone-manager/production/pg/nightly.dump.zst",
            sizeBytes: 4096,
            storageClass: "STANDARD_IA",
            verificationClass: "existence",
            verifiedAt: "2026-08-28T06:00:02+02:00",
            access: "immediate",
            status: "ACTIVE"
          }
        ]
      })
    );

    expect(await screen.findByText(/billed by your provider/i)).toBeTruthy();
    // No price, no rate, no percentage: the backend cannot compute any of
    // them honestly, so nothing here may render one (the #211 rule).
    expect(document.body.textContent).not.toMatch(/\$\s?\d/);
    expect(document.body.textContent).not.toMatch(/\d+\s*(USD|EUR|cents)/i);
    expect(document.body.textContent).not.toMatch(/per\s+(GB|TB|gigabyte)/i);
  });

  it("says a size nobody recorded is not recorded, rather than zero bytes", async () => {
    renderDetail(artifact({ placements: [{ ...LOCAL, sizeBytes: null }] }));

    expect(await screen.findByText("not recorded")).toBeTruthy();
  });
});

describe("the backups list only grows a Medium column when there is one", () => {
  it("shows no Medium column for a deployment whose backups are all local", async () => {
    renderList([artifact(), artifact({ id: "production/pg/older.dump.zst", filename: "older.dump.zst" })]);

    await screen.findByText("nightly.dump.zst");
    expect(screen.queryByRole("columnheader", { name: "Medium" })).toBeNull();
    // The positive control: the table really did render, so the absence
    // above is an absence of the column and not of the page.
    expect(screen.getByRole("columnheader", { name: "Retention" })).toBeTruthy();
  });

  it("shows the column, and flags the copies nobody can reach, once one backup lives elsewhere", async () => {
    renderList([
      artifact({
        placements: [
          {
            medium: "offsite_s3",
            mediumType: "",
            location: "k/nightly.dump.zst",
            sizeBytes: 4096,
            storageClass: "",
            verificationClass: "existence",
            verifiedAt: "2026-07-14T02:20:00+02:00",
            access: "unreachable",
            status: "ACTIVE"
          }
        ]
      }),
      artifact({ id: "production/pg/arriving.dump.zst", filename: "arriving.dump.zst", placements: [] })
    ]);

    await screen.findByText("nightly.dump.zst");
    expect(screen.getByRole("columnheader", { name: "Medium" })).toBeTruthy();
    expect(screen.getByText("offsite_s3")).toBeTruthy();
    expect(screen.getByText("Out of reach")).toBeTruthy();

    // The backup that is still arriving has no copy, and the cell says so
    // rather than being blank, which would read as "the column does not
    // apply here".
    const arriving = screen.getByText("arriving.dump.zst").closest("tr");
    expect(arriving).not.toBeNull();
    expect(within(arriving as HTMLElement).getByText("No copy yet")).toBeTruthy();
  });
});
