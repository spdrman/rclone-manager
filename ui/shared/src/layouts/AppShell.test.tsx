/**
 * The shell's own layout claim (issue #617).
 *
 * The dock is fixed to the browser window, which takes it out of flow, so
 * the content column has to leave room for it explicitly. Without that the
 * last row of a long table sits behind the terminal with no way to scroll
 * it clear, which is the objection the previous arrangement was built
 * around. The room is expressed as the custom property the dock publishes
 * rather than a constant, because the panel is resizable and collapsible
 * and a hardcoded number would be wrong the moment either happens.
 */
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ApiProvider } from "@shared/api/ApiContext";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { AppShell } from "@shared/layouts/AppShell";
import { DOCK_BAR_HEIGHT } from "@shared/components/ActivityDock";
import type { BackupManagerApi } from "@shared/api/contracts";

const quietApi = {
  getLiveActivity: () =>
    Promise.resolve({
      observedAt: "2026-09-07T14:02:41Z",
      epoch: "one-process",
      pollAfterMs: 10_000,
      sets: [],
      deployment: null
    })
} as unknown as BackupManagerApi;

function renderShell() {
  return render(
    <MemoryRouter>
      <PlatformProvider bridge={genericBridge}>
        <ApiProvider api={quietApi}>
          <AppShell
            health={null}
            version={null}
            counts={{}}
            theme="dark"
            onToggleTheme={() => {}}
            onSignOut={() => {}}
          >
            <p>a page</p>
          </AppShell>
        </ApiProvider>
      </PlatformProvider>
    </MemoryRouter>
  );
}

describe("the shell leaves room for the docked terminal", () => {
  it("reserves the height the dock publishes under the content column", () => {
    renderShell();
    const main = screen.getByRole("main");
    expect(main.style.paddingBottom).toBe(`var(--dock-height, ${DOCK_BAR_HEIGHT}px)`);
  });

  // The fallback matters on the first paint, before the dock's effect has
  // run: one line of room rather than none, so a collapsed bar never
  // covers content even for a frame.
  it("falls back to one line before the dock has published anything", () => {
    document.documentElement.style.removeProperty("--dock-height");
    renderShell();
    expect(screen.getByRole("main").style.paddingBottom).toContain(`${DOCK_BAR_HEIGHT}px`);
  });
});
