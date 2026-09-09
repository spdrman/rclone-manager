import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
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
async function apiWithRealId(): Promise<{ api: BackupManagerApi; asked: string[] }> {
  const base = createMockApi();
  const template = (await createMockApi().listArtifacts())[0];
  const only: BackupArtifact = {
    ...template,
    id: ARTIFACT_ID,
    setId: `${SOURCE}/${SET}`,
    setName: SET_NAME,
    filename: NAME,
  };
  const asked: string[] = [];
  const api = {
    ...base,
    listArtifacts: () => Promise.resolve([only]),
    getArtifact: (id: string) => {
      asked.push(id);
      return id === ARTIFACT_ID
        ? Promise.resolve(only)
        : Promise.reject(new Error(`no artifact ${id}`));
    },
  } as BackupManagerApi;
  return { api, asked };
}

function renderApp(api: BackupManagerApi, route: string) {
  return render(
    <MemoryRouter initialEntries={[route]}>
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
    // reads like a misclick, so the absence of the Dashboard is the claim.
    expect(screen.queryByRole("heading", { name: /dashboard/i })).not.toBeInTheDocument();
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

  /* The positive control, and the reason the four cases above are
   * evidence rather than a broken file. The ONLY difference here is an id
   * with no separators in it, which is the shape api/mock.ts happens to
   * use and the shape the current one-segment route can match. It passes
   * today. So when the four above go red, the route is what is wrong, not
   * the fixture, the render helper or the assertion. Delete this control
   * only if the route stops distinguishing the two, which is exactly what
   * fixing #677 should do. */
  it("already works for a slash-free id, which is why the failures above are the route", async () => {
    const base = createMockApi();
    const template = (await createMockApi().listArtifacts())[0];
    const flat: BackupArtifact = { ...template, id: "art_01J9F4M2QK8Z", filename: NAME, setName: SET_NAME };
    const api = {
      ...base,
      listArtifacts: () => Promise.resolve([flat]),
      getArtifact: () => Promise.resolve(flat),
    } as BackupManagerApi;

    renderApp(api, "/backups/art_01J9F4M2QK8Z");

    expect(await screen.findByText(NAME)).toBeInTheDocument();
  });
});
