/**
 * The backup detail page, and the one bug its state ownership exists to
 * prevent.
 *
 * The case worth reading first is the stale-flash one. React Router keeps
 * this component mounted when only the `:artifactId` changes, so a page
 * that renders whatever data it already has while the new fetch is in
 * flight shows one artifact's checksum, size and lifecycle under a
 * different artifact's URL. On a page whose whole purpose is telling an
 * operator whether THIS backup is safe, that is a correctness bug and not
 * a flicker, and it is why the fetch here stayed page-local.
 *
 * The remaining cases are the ordinary contract: every documented field
 * reaches the screen, and a failure produces a stated error rather than a
 * blank page.
 */
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import { artifactPath } from "@shared/utilities/routes";
import { BackupDetailPage } from "@shared/pages/BackupDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupManagerApi } from "@shared/api/contracts";
import { BackupManagerError } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import type { BackupArtifact } from "@shared/types/backup";

/* The route and the URL are both built the way the application builds
 * them (App.tsx's /backups/:source/:set/:name, and artifactPath for the
 * URL), because a test that declares its own route shape cannot disagree
 * with the real one — and while it declared a single :artifactId it did
 * not disagree with a route no real id could match, which is how #677
 * survived five cases in this file. The path is spelled out rather than
 * imported because it IS the assertion: change App.tsx and these fail. */
function renderDetail(artifactId: string, api: BackupManagerApi) {
  return render(
    <MemoryRouter initialEntries={[artifactPath(artifactId)]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/backups/:source/:set/:name" element={<BackupDetailPage />} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

// B2.4 mandatory-review fix: this page is page-local `useAsync` state
// (matching the sibling BackupSetDetailPage), not a shared graph node —
// nothing else reads this particular artifact, and the shared-node version
// let one artifact's fields render under a different artifact's URL while
// the new fetch was in flight (see the "no stale flash" test below).
describe("backup detail page reads the artifact", () => {
  it("fetches the artifact for the given id", async () => {
    const api = createMockApi();
    const artifacts = await createMockApi().listArtifacts();
    const target = artifacts[0];

    renderDetail(target.id, api);

    await screen.findByText(target.filename);
  });

  it("re-fetches when navigating to a different artifact", async () => {
    const api = createMockApi();
    const getArtifactSpy = vi.spyOn(api, "getArtifact");
    const artifacts = await createMockApi().listArtifacts();
    const [first, second] = artifacts;

    const { unmount } = renderDetail(first.id, api);
    await screen.findByText(first.filename);
    unmount();

    renderDetail(second.id, api);
    await screen.findByText(second.filename);
    expect(getArtifactSpy).toHaveBeenCalledTimes(2);
  });

  // The bug the mandatory review flagged: unmounting between renders (the
  // test above) never exercises the same-component-instance path, which is
  // what actually happens on a browser back/forward between two previously
  // visited artifact URLs, or list -> artifact A -> back -> artifact B.
  // React Router does not remount BackupDetailPage for a route-param change
  // alone, so the fetch for B starts while A's fields are still on screen.
  it("does not render artifact A's fields under artifact B's url while B is still loading (no unmount)", async () => {
    const api = createMockApi();
    const artifacts = await createMockApi().listArtifacts();
    const [first, second] = artifacts;
    const resolvers: Record<string, () => void> = {};
    vi.spyOn(api, "getArtifact").mockImplementation(
      (id) =>
        new Promise<BackupArtifact>((resolve) => {
          resolvers[id] = () => resolve(artifacts.find((a) => a.id === id) ?? first);
        })
    );

    function Harness() {
      const navigate = useNavigate();
      return (
        <>
          <button onClick={() => navigate(artifactPath(second.id))}>go to second</button>
          <Routes>
            <Route path="/backups/:source/:set/:name" element={<BackupDetailPage />} />
          </Routes>
        </>
      );
    }

    render(
      <MemoryRouter initialEntries={[artifactPath(first.id)]}>
        <ApiProvider api={api}>
          <Harness />
        </ApiProvider>
      </MemoryRouter>
    );

    resolvers[first.id]();
    await screen.findByText(first.filename);

    fireEvent.click(screen.getByText("go to second"));

    // The second fetch is in flight and unresolved: artifact A's fields
    // must not still be on screen under artifact B's url.
    expect(screen.queryByText(first.filename)).toBeNull();
    expect(screen.queryByText(first.id)).toBeNull();

    resolvers[second.id]();
    await screen.findByText(second.filename);
  });

  it("shows every documented artifact field once loaded", async () => {
    const api = createMockApi();
    const artifacts = await createMockApi().listArtifacts();
    const target = artifacts[0];

    renderDetail(target.id, api);
    await screen.findByText(target.filename);

    // "Ingestion path" was "Local path" until issue #240. The old label
    // read as "this is where your backup is", which the field has never
    // meant: it is where ingestion landed, and it stays populated for an
    // artifact still transferring (a partial file) and for one whose local
    // copy has since been moved to a storage medium. Where the bytes
    // actually are is the Copies card's question now.
    for (const label of [
      "Artifact ID", "Backup set", "Remote original", "Ingestion path",
      "Producer timestamp", "Received timestamp", "Size", "Checksum",
      "Validation result", "Retention classes", "Remote source removed"
    ]) {
      expect(screen.getByText(label)).toBeTruthy();
    }
  });

  it("shows an error state when the fetch fails, never a blank page", async () => {
    const api = createMockApi();
    vi.spyOn(api, "getArtifact").mockRejectedValue(
      new BackupManagerError({ code: "unknown", message: "That artifact no longer exists.", correlationId: "cid_test" })
    );

    // Three parts, because a URL that is not three parts no longer
    // reaches this page at all — the route would not match and there
    // would be no error state to find. The id names a backup the fixture
    // does not have, which is the case under test.
    renderDetail("production/api-server/does-not-exist.dump", api);

    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByText("That artifact no longer exists.")).toBeTruthy();
  });
});
