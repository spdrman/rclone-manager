import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { httpApi } from "@shared/api/client";
import { BackupDetailPage } from "@shared/pages/BackupDetailPage";
import { BackupsPage } from "@shared/pages/BackupsPage";
import type { BackupManagerApi } from "@shared/api/contracts";
import type { BackupArtifact } from "@shared/types/backup";
import type { WireArtifact } from "@shared/api/generated/contract";

/**
 * Issue #523 on screen and on the way in: which of these backups is
 * nothing ever going to delete?
 *
 * Removing a backup set is config-only, so its backups stay on storage and
 * stay on this list. What they lose on the way out is every retention
 * chain, because retention walks the configuration and the configuration
 * no longer names the set. Rendered plainly they are indistinguishable
 * from the healthy rows beside them, and the disk fills without anybody
 * being told.
 *
 * Every test here checks BOTH answers on the same render. A page that
 * marked every row and a page that marked none both pass a one-sided
 * test, and both are useless in opposite directions: one hides the rows
 * that matter, the other cries wolf until nobody reads the marker.
 */

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
    retentionPolicy: "configured",
    remoteSourceRemovedAt: "2026-08-28T02:01:01+02:00",
    quarantine: null,
    placements: [],
    ...over
  };
}

const GOVERNED = artifact();

const UNGOVERNED = artifact({
  id: "production/retired/old.dump.zst",
  setId: "production/retired",
  setName: "Legacy Redis (removed)",
  filename: "old.dump.zst",
  // The tier the journal still remembers, on a backup no chain will ever
  // look at again. It is here so the assertions below are about the page
  // choosing what to say, not about an artifact that had nothing to say.
  retentionClasses: ["daily", "monthly"],
  retentionPolicy: "none"
});

function renderList(list: BackupArtifact[]) {
  const api: BackupManagerApi = createMockApi();
  vi.spyOn(api, "listArtifacts").mockResolvedValue(list);
  render(
    <MemoryRouter initialEntries={["/backups"]}>
      <ApiProvider api={api}>
        <BackupsPage readOnly={false} />
      </ApiProvider>
    </MemoryRouter>
  );
}

/** The row a filename appears in, so an assertion is about ONE backup
 *  rather than about the page as a whole. */
async function rowFor(a: BackupArtifact): Promise<HTMLElement> {
  const cell = await screen.findByText(a.filename);
  const row = cell.closest("tr");
  if (!row) throw new Error("no row for " + a.filename);
  return row;
}

describe("the Backups list says which backups nothing will ever delete", () => {
  it("marks the row whose backup set was removed, and leaves the governed row alone", async () => {
    renderList([GOVERNED, UNGOVERNED]);

    const ungoverned = await rowFor(UNGOVERNED);
    expect(within(ungoverned).getByText("Nothing will delete this")).toBeTruthy();

    // The other half of the same claim, and the reason both rows are
    // rendered together: a page that marked everything would pass the
    // assertion above on its own.
    const governed = await rowFor(GOVERNED);
    expect(within(governed).queryByText("Nothing will delete this")).toBeNull();
    expect(within(governed).getByText("Daily")).toBeTruthy();
  });

  it("says the consequence rather than naming the state", async () => {
    renderList([GOVERNED, UNGOVERNED]);

    const row = await rowFor(UNGOVERNED);
    // "Unconfigured" and "No retention policy" are both true and both
    // leave the operator to work out what follows. What follows is that
    // this file is never going away on its own, which is the sentence
    // `backup-manager artifacts` prints and the one that gets read on a
    // page of four hundred rows.
    expect(within(row).queryByText(/unconfigured/i)).toBeNull();
    expect(within(row).getByText(/nothing will delete/i)).toBeTruthy();
  });

  it("does not badge a stale tier on a backup no chain will ever select again", async () => {
    renderList([GOVERNED, UNGOVERNED]);

    const ungoverned = await rowFor(UNGOVERNED);
    expect(within(ungoverned).queryByText("Daily")).toBeNull();
    expect(within(ungoverned).queryByText("Monthly")).toBeNull();

    // The precondition: this artifact really does carry those tiers, so
    // the absence above is the page's decision rather than an empty
    // fixture. The governed row proves the same badges render at all.
    expect(UNGOVERNED.retentionClasses).toEqual(["daily", "monthly"]);
    expect(within(await rowFor(GOVERNED)).getByText("Daily")).toBeTruthy();
  });

  it("explains under the list what the marked rows mean", async () => {
    renderList([GOVERNED, UNGOVERNED]);
    expect(await screen.findByText(/nothing here will ever delete them/i)).toBeTruthy();
    expect(screen.getByText(/create the backup set again/i)).toBeTruthy();
  });

  // The other half of the footnote's rule: a deployment that has never
  // removed a backup set reads exactly the page it read before, which is
  // the same restraint the CLI's own footnote shows.
  it("says nothing about removed sets when every listed backup is governed", async () => {
    renderList([GOVERNED]);

    await screen.findByText(GOVERNED.filename);
    expect(screen.queryByText(/nothing here will ever delete them/i)).toBeNull();
    expect(screen.queryByText("Nothing will delete this")).toBeNull();
  });
});

describe("a response that does not say is not read as a response that said yes", () => {
  it("marks a row the server reported no policy for, rather than passing it off as governed", async () => {
    // What an older build serves: the field is required by the contract,
    // so this is a server that predates it.
    const silent = artifact({
      id: "production/pg/older.dump.zst",
      filename: "older.dump.zst",
      retentionPolicy: "unknown"
    });
    renderList([GOVERNED, silent]);

    const row = await rowFor(silent);
    expect(within(row).getByText("Retention not reported")).toBeTruthy();
    expect(within(row).queryByText("Daily")).toBeNull();

    expect(await screen.findByText(/did not say which retention policy/i)).toBeTruthy();

    // And a governed row beside it still reads as governed, so this is a
    // statement about the silent row rather than about the whole page.
    expect(within(await rowFor(GOVERNED)).getByText("Daily")).toBeTruthy();
  });
});

function renderDetail(a: BackupArtifact) {
  const api: BackupManagerApi = createMockApi();
  vi.spyOn(api, "getArtifact").mockResolvedValue(a);
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

// The detail page is where an operator lands after clicking the row the
// list flagged. It reads the same field, so the two have to agree: a page
// that answered "Retention classes: daily, monthly" under a row marked
// "nothing will delete this" would leave the operator trusting whichever
// screen they happened to read second.
describe("the backup detail page agrees with the list it was reached from", () => {
  it("says nothing will delete a backup whose set was removed, and drops the stale tiers", async () => {
    renderDetail(UNGOVERNED);

    expect(await screen.findByText("Nothing will delete this")).toBeTruthy();
    expect(await screen.findByText(/no retention chain selects or expires this backup/i)).toBeTruthy();
    // The header badges are the stale claim this replaces; the field list
    // still records what the journal remembers, which is a record rather
    // than a promise.
    expect(screen.queryByText("Daily")).toBeNull();
    expect(screen.queryByText("Monthly")).toBeNull();
  });

  it("leaves a governed backup reading exactly as it did", async () => {
    renderDetail(GOVERNED);

    expect(await screen.findByText("Daily")).toBeTruthy();
    expect(screen.queryByText("Nothing will delete this")).toBeNull();
    expect(screen.getByText(/retention chain decides when this is deleted/i)).toBeTruthy();
  });

  it("says a silent server left the question open, rather than answering it", async () => {
    renderDetail(artifact({ retentionPolicy: "unknown" }));

    expect(await screen.findByText("Retention not reported")).toBeTruthy();
    expect(screen.getByText(/did not say, so this page cannot tell you/i)).toBeTruthy();
    expect(screen.queryByText(/retention chain decides when this is deleted/i)).toBeNull();
  });
});

/** The wire shape of one governed backup, as the client's mapper sees it. */
function wireArtifact(over: Partial<WireArtifact> = {}): WireArtifact {
  return {
    id: "production/pg/nightly.dump.zst",
    backup_set_id: "production/pg",
    source_name: "production",
    set_name: "pg",
    name: "nightly.dump.zst",
    remote_path: "db01:/srv/dumps/nightly.dump.zst",
    local_path: "/data/backups/production/pg/nightly.dump.zst",
    placements: [],
    state: "COMPLETE",
    discovered_at: "2026-08-28T01:58:44+02:00",
    updated_at: "2026-08-28T02:00:53+02:00",
    size_bytes: 4096,
    validation: "passed",
    quarantined: false,
    quarantine_irrecoverable: false,
    retention_policy: "configured",
    ...over
  };
}

/** Serves one artifacts list over fetch and reads back what httpApi made
 *  of it, so the mapper is exercised through the real client rather than
 *  called directly. */
async function mappedFrom(wire: unknown): Promise<BackupArtifact> {
  const response = new Response(JSON.stringify({ artifacts: [wire] }), {
    status: 200,
    headers: { "Content-Type": "application/json" }
  });
  vi.spyOn(globalThis, "fetch").mockResolvedValue(response);
  try {
    const [mapped] = await httpApi.listArtifacts();
    return mapped;
  } finally {
    vi.restoreAllMocks();
  }
}

describe("the client maps retention_policy without inventing an answer", () => {
  it("carries both of the contract's values through", async () => {
    expect((await mappedFrom(wireArtifact())).retentionPolicy).toBe("configured");
    expect((await mappedFrom(wireArtifact({ retention_policy: "none" }))).retentionPolicy).toBe("none");
  });

  it("reads a missing retention_policy as unknown, never as configured", async () => {
    // The older-build response. The cast is the point of the test: the
    // generated type says this cannot happen, and a type is a claim about
    // a server this client did not compile.
    const older = wireArtifact();
    delete (older as { retention_policy?: string }).retention_policy;

    const mapped = await mappedFrom(older);
    expect(mapped.retentionPolicy).toBe("unknown");
    expect(mapped.retentionPolicy).not.toBe("configured");
  });

  it("reads a value the contract does not name as unknown, in either direction", async () => {
    const mapped = await mappedFrom(wireArtifact({ retention_policy: "yes" as never }));
    expect(mapped.retentionPolicy).toBe("unknown");
  });
});
