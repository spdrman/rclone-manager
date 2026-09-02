import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
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

/**
 * Issue #275. A freshly installed instance used to be a dead end: App.tsx
 * returned the first-run surface INSTEAD of the routed application, so the
 * shell, the navigation and every page were unreachable until a backup set
 * existed. The wizard's own "Cancel and return to backup sets" changed the
 * URL to /sets and nothing happened, because which surface rendered was
 * gated on whether a configuration existed rather than on the route: the
 * operator saw no change and the address bar started lying.
 *
 * These tests drive the thing that was actually wrong, at the level it was
 * wrong: an instance with no configuration, walked the way an operator
 * walks it. Asserting page by page would not have caught it, because every
 * page was individually fine and none of them were reachable.
 *
 * The fixture refuses exactly what the real unconfigured router refuses
 * (503 NOT_CONFIGURED for everything outside its own small route table, see
 * apps/common/webhost's newUnconfiguredRouter and mock.ts's own
 * SERVED_WHILE_UNCONFIGURED), because a fixture that answers happily is a
 * fixture that cannot show this.
 */

const AUTHENTICATED: AuthContext = { authenticated: true, username: "bm-admin", mode: "local-account" };
const bridge: PlatformBridge = { ...genericBridge, getAuthContext: () => Promise.resolve(AUTHENTICATED) };

function renderApp(api: BackupManagerApi, route = "/") {
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

/** The section nav is the shell, and the shell rendering at all is the
 *  thing #275 is about. */
function nav() {
  return screen.getByRole("navigation", { name: "Sections" });
}

async function shellIsUp() {
  await screen.findByRole("navigation", { name: "Sections" }, { timeout: 4000 });
}

async function go(label: string) {
  await userEvent.click(within(nav()).getByRole("link", { name: new RegExp(label, "i") }));
}

/** Drives the wizard to a state where every save precondition is met:
 *  an imported key, a trusted host key, and the remote-deletion
 *  acknowledgement. Unchanged from the first-run test this replaces. */
async function completeSetupForm() {
  await userEvent.click(screen.getByRole("button", { name: "Authentication" }));
  await userEvent.click(screen.getByRole("radio", { name: /Import key/ }));
  await userEvent.type(screen.getByLabelText(/private key/i), "FAKE-TEST-KEY-MATERIAL-not-a-real-key-0123456789");
  await userEvent.click(screen.getByRole("button", { name: "Import key" }));
  await screen.findByText(/key imported/i);

  await userEvent.click(screen.getByRole("button", { name: "Verify server" }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Trust host" })).toBeEnabled());
  await userEvent.click(screen.getByRole("button", { name: "Trust host" }));

  await userEvent.click(screen.getByRole("button", { name: "Review" }));
  await userEvent.click(screen.getByRole("checkbox", { name: /remote backup will be removed only after/i }));
}

afterEach(() => {
  cleanup();
  resetGraphForTests();
  vi.restoreAllMocks();
});

describe("an instance with no configuration is an application, not a wall", () => {
  it("renders the shell and its navigation on the very first visit", async () => {
    renderApp(createMockApi("first-run"));
    await shellIsUp();

    for (const section of ["Dashboard", "Backup sets", "Backups", "Activity", "Quarantine", "Settings"]) {
      expect(within(nav()).getByRole("link", { name: new RegExp(section, "i") })).toBeInTheDocument();
    }
    // And it says, once, that setup has not been done, with the way to do it.
    expect(await screen.findByText(/has no configuration yet/i)).toBeInTheDocument();
  });

  it("shows every section something true about itself, and never a refusal", async () => {
    renderApp(createMockApi("first-run"));
    await shellIsUp();
    await screen.findByText(/Nothing is being backed up yet/i);

    const expected: [string, RegExp][] = [
      ["Backup sets", /No backup sets yet/i],
      ["Backups", /No backups yet/i],
      ["Activity", /No activity yet/i],
      ["Quarantine", /Nothing in quarantine/i]
    ];
    for (const [section, copy] of expected) {
      await go(section);
      expect(await screen.findByText(copy)).toBeInTheDocument();
      // What the service actually answered was a 503 quoting a route path.
      // None of that belongs in front of an operator, and neither does a
      // Try again button for a request that will refuse every time.
      expect(document.body.textContent).not.toContain("/api/v1/");
      expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    }

    await go("Settings");
    // Settings mostly works with no configuration: the platform, the build
    // and the administrator password have nothing to do with backup sets.
    expect(await screen.findByRole("heading", { name: "Settings" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Change password" })).toBeInTheDocument();
    // The one card that reads configuration says so plainly.
    expect(screen.getByText(/Retention is part of the configuration/i)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("/api/v1/");
    // And it stops claiming files were found in a storage location that
    // does not exist yet.
    expect(screen.queryByText(/Existing backup data detected/i)).toBeNull();
  });

  it("lets the operator enter the wizard from the empty list and leave it again", async () => {
    renderApp(createMockApi("first-run"));
    await shellIsUp();

    await go("Backup sets");
    await userEvent.click(await screen.findByRole("button", { name: "Add backup set" }));
    expect(await screen.findByRole("heading", { name: "Add backup set" })).toBeInTheDocument();

    // #275 itself: this control changed the URL and did nothing, because
    // there was no sets list to return to.
    await userEvent.click(screen.getByRole("button", { name: /Cancel and return to backup sets/ }));
    expect(await screen.findByText(/No backup sets yet/i)).toBeInTheDocument();

    // Still explorable afterwards, and still able to come back.
    await go("Settings");
    expect(await screen.findByRole("heading", { name: "Settings" })).toBeInTheDocument();
    await go("Backup sets");
    expect(await screen.findByRole("button", { name: "Add backup set" })).toBeInTheDocument();
  });

  it("writes the first configuration through completeFirstRun and lands back on the list", async () => {
    const api = createMockApi("first-run");
    const completeFirstRun = vi.spyOn(api, "completeFirstRun");
    const createBackupSet = vi.spyOn(api, "createBackupSet");

    renderApp(api, "/sets/new");
    await shellIsUp();
    await completeSetupForm();
    await userEvent.click(screen.getByRole("button", { name: /^Finish setup$/ }));

    await waitFor(() => expect(completeFirstRun).toHaveBeenCalledTimes(1));
    // There is no configuration to fold a set into, so the ordinary create
    // route must not be what this writes through.
    expect(createBackupSet).not.toHaveBeenCalled();

    // The same destination the configured path reaches by the same route.
    expect(await screen.findByRole("heading", { name: "Backup sets" }, { timeout: 4000 })).toBeInTheDocument();
    // And the instance has stopped saying it has no configuration.
    await waitFor(() => expect(screen.queryByText(/has no configuration yet/i)).toBeNull());
  });

  it("does not offer an immediate run, because an unconfigured instance has no service to run one", async () => {
    renderApp(createMockApi("first-run"), "/sets/new");
    await shellIsUp();
    await completeSetupForm();

    expect(screen.getByRole("button", { name: /^Finish setup$/ })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Save, enable & run/ })).toBeNull();
  });

  it("never asks for run_immediately even though the wizard's own state can carry it", async () => {
    const api = createMockApi("first-run");
    const completeFirstRun = vi.spyOn(api, "completeFirstRun");

    renderApp(api, "/sets/new");
    await shellIsUp();
    await completeSetupForm();
    await userEvent.click(screen.getByRole("button", { name: /^Finish setup$/ }));

    await waitFor(() => expect(completeFirstRun).toHaveBeenCalledTimes(1));
    expect(completeFirstRun.mock.calls[0][0].runImmediately).toBe(false);
  });

  it("tells the operator their configuration is safe when the instance cannot activate in place", async () => {
    const api = createMockApi("first-run");
    vi.spyOn(api, "completeFirstRun").mockResolvedValue({
      backupSet: {
        id: "set_x", sourceName: "api", name: "x", host: "h", port: 22, user: "u",
        remotePath: "/r", localPath: "/l", include: [], completionStrategy: "marker",
        validatorId: undefined, disabled: false, readOnly: false
      },
      restartRequired: true
    });

    renderApp(api, "/sets/new");
    await shellIsUp();
    await completeSetupForm();
    await userEvent.click(screen.getByRole("button", { name: /^Finish setup$/ }));

    expect(await screen.findByRole("heading", { name: /Configuration saved/i })).toBeInTheDocument();
    expect(screen.getByText(/restart the Backup Manager container or service/i)).toBeInTheDocument();
    // This one state really is a dead end, and offers no navigation it
    // cannot honour.
    expect(screen.queryByRole("navigation", { name: "Sections" })).toBeNull();
  });

  it("reports a refused setup without claiming it succeeded", async () => {
    const api = createMockApi("first-run");
    vi.spyOn(api, "completeFirstRun").mockRejectedValue(new Error("boom"));

    renderApp(api, "/sets/new");
    await shellIsUp();
    await completeSetupForm();
    await userEvent.click(screen.getByRole("button", { name: /^Finish setup$/ }));

    await screen.findByText(/Could not save this configuration/i);
    expect(screen.queryByRole("heading", { name: /Configuration saved/i })).toBeNull();
    expect(screen.getByRole("heading", { name: "Add backup set" })).toBeInTheDocument();
  });
});

/** The control. Every empty state above has to be caused by the refusal,
 *  not rendered unconditionally: an assertion that something is absent
 *  passes just as happily when the thing is absent everywhere. */
describe("a configured instance is unaffected", () => {
  it("shows real data and none of the not-configured copy", async () => {
    renderApp(createMockApi());
    await shellIsUp();

    expect(await screen.findByRole("heading", { name: "Dashboard" })).toBeInTheDocument();
    expect(screen.queryByText(/has no configuration yet/i)).toBeNull();
    expect(screen.queryByText(/Nothing is being backed up yet/i)).toBeNull();

    await go("Backup sets");
    expect(await screen.findByRole("heading", { name: "Backup sets" })).toBeInTheDocument();
    expect(screen.queryByText(/No backup sets yet/i)).toBeNull();

    await go("Settings");
    expect(await screen.findByRole("heading", { name: "Settings" })).toBeInTheDocument();
    expect(screen.queryByText(/Retention is part of the configuration/i)).toBeNull();
    expect(screen.getByText(/Existing backup data detected/i)).toBeInTheDocument();
  });
});
