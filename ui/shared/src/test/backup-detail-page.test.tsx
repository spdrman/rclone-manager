import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { BackupDetailPage } from "@shared/pages/BackupDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupManagerApi } from "@shared/api/contracts";
import { BackupManagerError } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { artifactDetailNode } from "@shared/state/appNodes";

function renderDetail(artifactId: string, api: BackupManagerApi) {
  return render(
    <MemoryRouter initialEntries={["/backups/" + artifactId]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/backups/:artifactId" element={<BackupDetailPage />} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

// #106 put App.tsx's four resources on the shared causl graph as
// `createResourceNode` inputs. B2.4 moves BackupDetailPage's single-artifact
// read onto that same mechanism (its own node — nothing else fetches this
// particular artifact today) rather than leaving it as page-local
// useAsync state: see docs/EPIC-B-multi-nas.md, B2.4.
describe("backup detail page reads the artifact through the graph", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("fetches the artifact into the shared artifactDetailNode resource", async () => {
    const api = createMockApi();
    const artifacts = await createMockApi().listArtifacts();
    const target = artifacts[0];

    renderDetail(target.id, api);

    await screen.findByText(target.filename);

    expect(graph.read(artifactDetailNode).data?.id).toBe(target.id);
  });

  it("re-fetches into the same node when navigating to a different artifact", async () => {
    const api = createMockApi();
    const getArtifactSpy = vi.spyOn(api, "getArtifact");
    const artifacts = await createMockApi().listArtifacts();
    const [first, second] = artifacts;

    const { unmount } = renderDetail(first.id, api);
    await screen.findByText(first.filename);
    expect(graph.read(artifactDetailNode).data?.id).toBe(first.id);
    unmount();

    renderDetail(second.id, api);
    await screen.findByText(second.filename);
    expect(graph.read(artifactDetailNode).data?.id).toBe(second.id);
    expect(getArtifactSpy).toHaveBeenCalledTimes(2);
  });

  it("shows every documented artifact field once loaded", async () => {
    const api = createMockApi();
    const artifacts = await createMockApi().listArtifacts();
    const target = artifacts[0];

    renderDetail(target.id, api);
    await screen.findByText(target.filename);

    for (const label of [
      "Artifact ID", "Backup set", "Remote original", "Local path",
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

    renderDetail("does-not-exist", api);

    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByText("That artifact no longer exists.")).toBeTruthy();
  });
});
