import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
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
 * Issue #677, and it is issue #285 a second time on the neighbouring route.
 *
 * model.ArtifactID.String() (core/internal/model/ids.go) is
 * `a.Set.String() + "/" + a.Name`, and BackupSetID.String() is itself
 * `source + "/" + set`, so a real artifact id off the wire has THREE
 * segments: "production/api-server/db.dump.zst". App.tsx declares
 * `/backups/:artifactId`, one segment, and BackupsPage navigates with
 * "/backups/" + a.id, so on any real deployment nothing matches and the
 * catch-all lands the operator on the Dashboard. The lifecycle timeline
 * and the whole FR-34 Copies card are unreachable in a browser.
 *
 * Two reasons this went unnoticed for so long, both worth keeping:
 *
 * api/mock.ts's artifact ids are slash-free ("art_01J9F4M2QK8Z"), so
 * every fixture-driven test navigates a URL a real id can never produce.
 * These cases therefore build the id shape directly rather than borrowing
 * the mock's, exactly as backup-set-slash-id.test.tsx does for #285.
 *
 * And backup-detail-page.test.tsx renders BackupDetailPage under a
 * `<Route path="/backups/:artifactId">` it declares itself, which
 * reproduces the broken route locally and can never disagree with
 * App.tsx. So these cases render the real <App /> and let the real route
 * table decide, which is the only arrangement that can fail.
 *
 * Three cases were folded in from a second suite opened for the same
 * issue before either of us saw the other's branch (#696 onto #680): an
 * id whose parts need escaping, round-tripped in both directions; the
 * address bar asserted alongside the page, so a redirect can be told
 * from a page that failed to load; and the neighbouring
 * /sets/:source/:set route under the same escaping, since #285 is this
 * same defect and a guard on one of two neighbours is how it reached
 * this one.
 */

const AUTHENTICATED: AuthContext = { authenticated: true, username: "bm-admin", mode: "local-account" };
const bridge: PlatformBridge = { ...genericBridge, getAuthContext: () => Promise.resolve(AUTHENTICATED) };

const SOURCE = "production";
const SET = "api-server";
const NAME = "db.dump.zst";
const SET_NAME = "Production API server";
const ARTIFACT_ID = `${SOURCE}/${SET}/${NAME}`;

/* Built from the mock's OWN artifact, with only the id and the names
 * overridden. Hand-writing the whole shape is how the first draft of this
 * file failed for the wrong reason: BackupsPage reads `placements` and
 * `retentionPolicy`, my fixture had neither, and the row-click case threw
 * inside the page instead of failing on the route. A test that goes red
 * for a reason other than the bug is worse than no test. */
async function apiWithRealId(
  source = SOURCE,
  set = SET,
  name = NAME
): Promise<{ api: BackupManagerApi; asked: string[]; id: string }> {
  const base = createMockApi();
  const template = (await createMockApi().listArtifacts())[0];
  const id = `${source}/${set}/${name}`;
  const only: BackupArtifact = {
    ...template,
    id,
    setId: `${source}/${set}`,
    setName: SET_NAME,
    filename: name,
  };
  const asked: string[] = [];
  const api = {
    ...base,
    listArtifacts: () => Promise.resolve([only]),
    getArtifact: (askedFor: string) => {
      asked.push(askedFor);
      return askedFor === id
        ? Promise.resolve(only)
        : Promise.reject(new Error(`no artifact ${askedFor}`));
    },
  } as BackupManagerApi;
  return { api, asked, id };
}

/* The address bar, readable from a test. The failure here is a REDIRECT,
 * and a page that merely failed to load its data would leave the URL
 * alone, so the two are worth telling apart: with this, the row-click
 * case fails as `expected '/' to be '/backups/production/api-server/
 * db.dump.zst'` — the catch-all named — instead of as an element that
 * could not be found for any of a dozen reasons. */
function LocationProbe() {
  return <span data-testid="pathname">{useLocation().pathname}</span>;
}

function renderApp(api: BackupManagerApi, route: string) {
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

afterEach(() => {
  cleanup();
  resetGraphForTests();
});

describe("an artifact id with separators in it reaches its own page", () => {
  it("opens the detail page when the URL is typed or followed directly", async () => {
    const { api } = await apiWithRealId();
    renderApp(api, `/backups/${ARTIFACT_ID}`);

    // The filename is the detail page's own heading. The Dashboard, which
    // is where the catch-all sends an unmatched route, never renders it.
    expect(await screen.findByText(NAME)).toBeInTheDocument();
  });

  it("does not silently land on the Dashboard instead", async () => {
    const { api } = await apiWithRealId();
    renderApp(api, `/backups/${ARTIFACT_ID}`);
    await screen.findByText(NAME);

    // The failure this guards is not an error page, it is a redirect that
    // reads like a misclick, so the absence of the Dashboard is the claim
    // — and the address bar says the same thing from the other side: a
    // redirect replaced the URL, a page that failed to load would not
    // have.
    expect(screen.queryByRole("heading", { name: /dashboard/i })).not.toBeInTheDocument();
    expect(screen.getByTestId("pathname").textContent).toBe(`/backups/${ARTIFACT_ID}`);
  });

  it("opens the detail page when a row in Backups is clicked", async () => {
    const user = userEvent.setup();
    const { api } = await apiWithRealId();
    renderApp(api, "/backups");

    const row = await screen.findByText(SET_NAME);
    await user.click(row);

    expect(await screen.findByText(NAME)).toBeInTheDocument();
  });

  it("asks the API for the id it was given, separators and all", async () => {
    const { api, asked } = await apiWithRealId();
    renderApp(api, `/backups/${ARTIFACT_ID}`);
    await screen.findByText(NAME);

    // A route that matched only the last segment would look right on
    // screen and fetch the wrong thing, so the id is checked whole.
    expect(asked).toContain(ARTIFACT_ID);
  });

  /* Escaping is the other half of the round trip, and it is a half that
   * cannot be got right by concatenation. A space and a "#" in a backup's
   * name are ordinary — a "#" pasted into a path raw starts a fragment
   * and takes the rest of the id out of the URL entirely — so the parts
   * are escaped one at a time on the way out (artifactPath, utilities/
   * routes.ts) and unescaped one at a time on the way back in (React
   * Router's own param decoding, read by BackupDetailPage). Both
   * directions are asserted here, because either one alone is a half
   * that can be right while the pair is wrong. */
  it("round-trips an id whose parts need escaping, from the row click to the API", async () => {
    const user = userEvent.setup();
    const { api, asked, id } = await apiWithRealId("prod site", "nightly #2", "db dump #7.sql.gz");
    renderApp(api, "/backups");

    await user.click(await screen.findByText(SET_NAME));

    expect(await screen.findByText("db dump #7.sql.gz")).toBeInTheDocument();
    // The URL the list built, spelled out. This is the same string the
    // case below arrives with, which is what makes the two halves one
    // round trip rather than two conventions that happen to agree.
    expect(screen.getByTestId("pathname").textContent).toBe(
      "/backups/prod%20site/nightly%20%232/db%20dump%20%237.sql.gz"
    );
    // And the API asked for the id whole, unescaped, character for
    // character what the list held.
    expect(asked).toContain(id);
  });

  it("opens that escaped id from a deep link, spelled out as the URL it must be", async () => {
    // The encoding pinned as a literal on purpose. A bookmark, an address
    // bar and a link in a ticket carry this string and nothing else, so
    // it is the contract rather than an implementation detail of whatever
    // built it.
    const { api, asked, id } = await apiWithRealId("prod site", "nightly #2", "db dump #7.sql.gz");
    renderApp(api, "/backups/prod%20site/nightly%20%232/db%20dump%20%237.sql.gz");

    expect(await screen.findByText("db dump #7.sql.gz")).toBeInTheDocument();
    expect(asked).toContain(id);
  });

  /* The neighbour, and the reason it is in this file: /sets/:source/:set
   * had this exact defect in #285, its explanation sits four lines above
   * the line that still had it, and a guard watching one of two
   * neighbours is how that fix failed to reach this one. The plain case
   * lives in backup-set-slash-id.test.tsx; the escaped one belongs beside
   * the artifact route's, because both routes now get their escaping from
   * the same place. */
  it("does the same for a backup set id whose parts need escaping (#285's route)", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    const template = (await createMockApi().listSets())[0];
    const only = { ...template, id: "prod site/nightly #2", source: "prod site", set: "nightly #2" };
    const asked: string[] = [];
    const served = {
      ...api,
      listSets: () => Promise.resolve([only]),
      getSet: (id: string) => {
        asked.push(id);
        return id === only.id ? Promise.resolve(only) : Promise.reject(new Error(`no set ${id}`));
      },
    } as BackupManagerApi;

    renderApp(served, "/sets");
    await user.click(await screen.findByRole("button", { name: "Open" }));

    expect(await screen.findByRole("heading", { name: "Connection" })).toBeInTheDocument();
    expect(screen.getByTestId("pathname").textContent).toBe("/sets/prod%20site/nightly%20%232");
    expect(asked).toContain("prod site/nightly #2");
  });

  /* The control that replaces #680's, and it inherits its whole job.
   *
   * #680's control was "a slash-free id already works", which made the
   * four red cases evidence about the ROUTE rather than about the
   * fixture or the helper. Its own comment said to delete it once the
   * route stops distinguishing the two shapes, and that is precisely
   * what the fix does, so it is retired here rather than re-pinned (the
   * quote and its post-fix failure are recorded on the PR).
   *
   * What must not be retired with it is the coverage. The route now
   * declares exactly three segments, and the cheap wrong way to make
   * every case above pass is a greedy `/backups/*`, which swallows any
   * number of segments and cannot be told from the correct route by any
   * three-segment test in this file. So the control is inverted: a URL
   * that is NOT three segments must not open a detail page. It falls to
   * the catch-all, which is the same behaviour any other unknown URL
   * gets, and an id of one segment is a thing no deployment can produce
   * — model.ArtifactID.String() always joins three parts, and the API's
   * own route is /backups/{source}/{set}/{name}.
   *
   * Under `/backups/*` this case fails and nothing else in the file
   * does, which is the property that makes it worth keeping. */
  it("does not open a detail page for a URL that is not three segments, which a greedy route would", async () => {
    const { api, asked } = await apiWithRealId();

    renderApp(api, "/backups/art_01J9F4M2QK8Z");

    expect(await screen.findByRole("heading", { name: /dashboard/i })).toBeInTheDocument();
    expect(screen.getByTestId("pathname").textContent).toBe("/");
    // And nothing was fetched under a half-id on the way past. A greedy
    // route would have asked for something here.
    expect(asked).toEqual([]);
  });
});
