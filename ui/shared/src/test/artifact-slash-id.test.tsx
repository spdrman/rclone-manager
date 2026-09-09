import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import { App } from "@shared/App";
import { ApiProvider } from "@shared/api/ApiContext";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { createMockApi } from "@shared/api/mock";
import type { BackupManagerApi } from "@shared/api/contracts";
import type { AuthContext, PlatformBridge } from "@shared/types/platform";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { resetGraphForTests } from "@shared/state/graph";
import type { BackupArtifact } from "@shared/types/backup";

/**
 * Issue #677, and the same lesson as #285 one route further down.
 *
 * model.ArtifactID.String() (core/internal/model/ids.go) is
 * BackupSetID.String() + "/" + name, so a real artifact id off the wire is
 * three parts — "production/api-server/notes.txt" — matching the API's own
 * /backups/{source}/{set}/{name} shape (router.go). The route declared one
 * segment, so a click on a backup row matched nothing, the catch-all took
 * it, and the operator landed on the Dashboard with no error and no 404.
 *
 * The reason five existing cases sat green over that is the part worth
 * copying from #285's file next door: they borrowed api/mock.ts's ids,
 * which were flat opaque tokens ("art_01J9C1XY7T09"), and a fixture that
 * is the wrong shape in the one dimension the bug lives in cannot show the
 * bug. So every id here is built to the real shape directly, the mock's
 * lookup is replaced by one that REFUSES an id it does not know (the
 * mock's own getArtifact falls back to the first artifact, which would let
 * a wrongly reassembled id render a page and pass), and the assertions are
 * on the id the API was asked for and the id the page printed — not merely
 * on a detail page having appeared.
 *
 * Both routes are covered here rather than just the broken one. This is
 * the second time the same defect has been fixed on this pair, and a guard
 * that watches one of two neighbours is how the first fix failed to reach
 * the second.
 */

const AUTHENTICATED: AuthContext = { authenticated: true, username: "bm-admin", mode: "local-account" };
const bridge: PlatformBridge = { ...genericBridge, getAuthContext: () => Promise.resolve(AUTHENTICATED) };

/** The address bar, readable from a test. The defect this file guards is
 *  a REDIRECT: nothing matches, App's catch-all navigates to "/", and the
 *  page an operator asked for is replaced by the Dashboard without a word.
 *  Asserting on the path as well as on the page says which of the two
 *  failures happened — a page that failed to load its data still leaves
 *  the URL alone. */
function LocationProbe() {
  return <span data-testid="pathname">{useLocation().pathname}</span>;
}

function renderApp(api: BackupManagerApi, route = "/") {
  return render(
    <MemoryRouter initialEntries={[route]}>
      <LocationProbe />
      <ApiProvider api={api}>
        <PlatformProvider bridge={bridge}>
          <App />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

async function shellIsUp() {
  await screen.findByRole("navigation", { name: "Sections" }, { timeout: 4000 });
}

function nav() {
  return screen.getByRole("navigation", { name: "Sections" });
}

async function go(label: string) {
  await userEvent.click(within(nav()).getByRole("link", { name: new RegExp(label, "i") }));
}

/** A backup as core actually identifies one: `source/set/name`, with the
 *  three halves also present separately so the fixture cannot disagree
 *  with itself the way a hand-written flat id would. */
function artifact(source: string, set: string, name: string): BackupArtifact {
  return {
    id: source + "/" + set + "/" + name,
    setId: source + "/" + set,
    setName: "API server",
    filename: name,
    remoteOriginalPath: "api-01.internal:/srv/backups/" + name,
    localPath: "/data/backups/" + source + "/" + set + "/" + name,
    producedAt: "2026-08-28T01:58:44+02:00",
    receivedAt: "2026-08-28T02:00:53+02:00",
    sizeBytes: 4096,
    checksum: "4f2a9c1e7b6d0835ae91cf4d2b7801e6c35a9f18d4b27e60ac139f5b8e2d7a04",
    checksumAlgorithm: "sha256",
    validation: "verified",
    retentionClasses: ["daily"],
    retentionPolicy: "configured",
    remoteSourceRemovedAt: "2026-08-28T02:01:01+02:00",
    quarantine: null,
    placements: [
      {
        medium: "local", mediumType: "local",
        location: "/data/backups/" + source + "/" + set + "/" + name,
        sizeBytes: 4096, storageClass: "",
        verificationClass: "content", verifiedAt: "2026-08-28T02:00:59+02:00",
        access: "immediate", status: "ACTIVE"
      }
    ]
  };
}

/** The mock, with the artifact lookup made strict. `createMockApi`'s own
 *  `getArtifact` answers an unknown id with `artifacts[0]`, so a page
 *  reached by an id that lost or gained a part would still render somebody
 *  else's backup and every assertion below would pass. A real deployment
 *  answers 404. */
function apiWithArtifacts(artifacts: BackupArtifact[]): BackupManagerApi {
  const api = createMockApi();
  vi.spyOn(api, "listArtifacts").mockResolvedValue(artifacts);
  vi.spyOn(api, "getArtifact").mockImplementation((id) => {
    const found = artifacts.find((a) => a.id === id);
    return found
      ? Promise.resolve(found)
      : Promise.reject(new Error("mock getArtifact: no such artifact id " + JSON.stringify(id)));
  });
  return api;
}

afterEach(() => {
  cleanup();
  resetGraphForTests();
  vi.restoreAllMocks();
});

describe("a backup whose id spans three segments is reachable (issue #677)", () => {
  it("opens from the backups list", async () => {
    const a = artifact("production", "api-server", "notes.txt");
    renderApp(apiWithArtifacts([a]));
    await shellIsUp();

    await go("Backups");
    await userEvent.click(await screen.findByText("notes.txt"));

    // The URL first: on the defect this is "/", because the click built a
    // path no route declared and the catch-all sent it to the Dashboard.
    expect(screen.getByTestId("pathname").textContent).toBe("/backups/production/api-server/notes.txt");
    // The Lifecycle card is on the detail page and nowhere else, so
    // finding it is proof this navigated rather than that the list's own
    // filename cell satisfied a laxer query.
    expect(await screen.findByRole("heading", { name: "Lifecycle" })).toBeInTheDocument();
    // The whole id, printed by the page, is the round trip closing: it
    // went out as three URL segments and came back reassembled.
    expect(screen.getByText("production/api-server/notes.txt")).toBeInTheDocument();
  });

  it("opens from a deep link, the way a reload or a bookmark arrives", async () => {
    const a = artifact("production", "api-server", "notes.txt");
    renderApp(apiWithArtifacts([a]), "/backups/production/api-server/notes.txt");

    expect(await screen.findByRole("heading", { name: "Lifecycle" })).toBeInTheDocument();
    // Checked after the page has settled, because the redirect this
    // defect performs is an effect and not synchronous with the render:
    // asked-for URL still in the address bar, no silent bounce home.
    expect(screen.getByTestId("pathname").textContent).toBe("/backups/production/api-server/notes.txt");
    expect(screen.getByText("production/api-server/notes.txt")).toBeInTheDocument();
  });

  it("round-trips an id whose parts contain characters a URL must escape", async () => {
    // A space and a "#" in a filename are ordinary on a real deployment
    // and neither survives being pasted into a path raw: the "#" would
    // start a fragment and the rest of the id would never reach the
    // router at all. Escaping is therefore part of the id round-tripping,
    // not a separate nicety.
    const a = artifact("prod site", "nightly #2", "db dump #7.sql.gz");
    const api = apiWithArtifacts([a]);
    renderApp(api);
    await shellIsUp();

    await go("Backups");
    await userEvent.click(await screen.findByText("db dump #7.sql.gz"));

    // The URL the list built, spelled out: this is the same string the
    // deep-link case below arrives with, which is what makes the two
    // halves of the round trip one round trip.
    expect(screen.getByTestId("pathname").textContent).toBe(
      "/backups/prod%20site/nightly%20%232/db%20dump%20%237.sql.gz"
    );
    expect(await screen.findByRole("heading", { name: "Lifecycle" })).toBeInTheDocument();
    // Asked for by the id the list held, character for character. The
    // strict lookup above means a lost escape is a rejected fetch and an
    // error state, but this says which id was asked for rather than only
    // that some fetch worked.
    expect(api.getArtifact).toHaveBeenCalledWith("prod site/nightly #2/db dump #7.sql.gz");
  });

  it("opens the same escaped id from a deep link, spelled out as the URL it must be", async () => {
    // The encoding pinned as a literal on purpose. Anything that reads
    // this URL — the browser's address bar, a bookmark, a link in a
    // ticket — sees this string and nothing else, so it is the contract
    // rather than an implementation detail of whatever builds it.
    const a = artifact("prod site", "nightly #2", "db dump #7.sql.gz");
    const api = apiWithArtifacts([a]);
    renderApp(api, "/backups/prod%20site/nightly%20%232/db%20dump%20%237.sql.gz");

    expect(await screen.findByRole("heading", { name: "Lifecycle" })).toBeInTheDocument();
    expect(api.getArtifact).toHaveBeenCalledWith("prod site/nightly #2/db dump #7.sql.gz");
  });

  // The neighbour, for the reason in this file's header: /sets/:source/:set
  // has had this bug once already (#285). backup-set-slash-id.test.tsx
  // covers the plain case; the escaped one belongs beside the artifact
  // route's, because both routes get their escaping from the same place.
  it("does the same for a backup set id whose parts need escaping (issue #285's route)", async () => {
    const api = createMockApi();
    const sets = await api.listSets();
    const target = { ...sets[0], id: "prod site/nightly #2", source: "prod site", set: "nightly #2", name: "Nightly archive" };
    vi.spyOn(api, "listSets").mockResolvedValue([target]);
    const getSet = vi.spyOn(api, "getSet").mockImplementation((id) =>
      id === target.id
        ? Promise.resolve(target)
        : Promise.reject(new Error("mock getSet: no such set id " + JSON.stringify(id)))
    );

    renderApp(api);
    await shellIsUp();

    await go("Backup sets");
    await userEvent.click(await screen.findByRole("button", { name: "Open" }));

    expect(await screen.findByRole("heading", { name: "Connection" })).toBeInTheDocument();
    expect(getSet).toHaveBeenCalledWith("prod site/nightly #2");
  });
});
