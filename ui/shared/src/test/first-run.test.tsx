import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { FirstRunPage } from "@shared/pages/FirstRunPage";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import type { BackupManagerApi } from "@shared/api/contracts";
import { resetGraphForTests } from "@shared/state/graph";

// Issue #176: the screen a fresh app-store install shows. Before this,
// there was no such screen and no process to serve it — `serve` validated
// config.yaml before the listener started, so a freshly installed
// container had no web UI at all until an operator SSHed in and wrote
// YAML by hand.

function renderFirstRun(api: BackupManagerApi, onConfigured = () => {}) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <PlatformProvider bridge={genericBridge}>
          <FirstRunPage onConfigured={onConfigured} />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

/** Drives the wizard to a state where every save precondition is met,
 *  identical to wizard.test.tsx's own helper: an imported key, a trusted
 *  host key, and the remote-deletion acknowledgement. */
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
});

describe("first run", () => {
  it("explains that nothing is configured yet and offers the setup form", () => {
    renderFirstRun(createMockApi("first-run"));
    expect(screen.getByRole("heading", { name: /set up backup manager/i })).toBeInTheDocument();
    expect(screen.getByText(/no configuration yet/i)).toBeInTheDocument();
    // The ordinary wizard, not a second parallel form.
    expect(screen.getAllByRole("listitem").length).toBe(6);
  });

  it("writes the first configuration through completeFirstRun, never through createBackupSet", async () => {
    const api = createMockApi("first-run");
    const completeFirstRun = vi.spyOn(api, "completeFirstRun");
    const createBackupSet = vi.spyOn(api, "createBackupSet");
    const onConfigured = vi.fn();

    renderFirstRun(api, onConfigured);
    await completeSetupForm();
    await userEvent.click(screen.getByRole("button", { name: /^Finish setup$/ }));

    await waitFor(() => expect(completeFirstRun).toHaveBeenCalledTimes(1));
    // There is no configuration to fold a set into, so the ordinary
    // create route must not be what this screen calls.
    expect(createBackupSet).not.toHaveBeenCalled();
    await waitFor(() => expect(onConfigured).toHaveBeenCalledTimes(1));
  });

  it("does not offer an immediate run, because an unconfigured instance has no service to run one", async () => {
    renderFirstRun(createMockApi("first-run"));
    await completeSetupForm();

    expect(screen.getByRole("button", { name: /^Finish setup$/ })).toBeEnabled();
    expect(screen.queryByRole("button", { name: /Save, enable & run/ })).not.toBeInTheDocument();
  });

  it("never asks for run_immediately even though the wizard's own state can carry it", async () => {
    const api = createMockApi("first-run");
    const completeFirstRun = vi.spyOn(api, "completeFirstRun");

    renderFirstRun(api);
    await completeSetupForm();
    await userEvent.click(screen.getByRole("button", { name: /^Finish setup$/ }));

    await waitFor(() => expect(completeFirstRun).toHaveBeenCalled());
    expect(completeFirstRun.mock.calls[0][0].runImmediately).toBe(false);
  });

  it("tells the operator their configuration is safe when the instance cannot activate in place", async () => {
    const api = createMockApi("first-run");
    vi.spyOn(api, "completeFirstRun").mockResolvedValue({
      backupSet: {
        id: "api/nightly",
        sourceName: "api",
        name: "nightly",
        host: "prod-db-01.internal",
        port: 22,
        user: "backup-agent",
        remotePath: "/backups",
        localPath: "/data/backups",
        include: [],
        completionStrategy: "marker",
        disabled: false
      },
      restartRequired: true
    });
    const onConfigured = vi.fn();

    renderFirstRun(api, onConfigured);
    await completeSetupForm();
    await userEvent.click(screen.getByRole("button", { name: /^Finish setup$/ }));

    expect(await screen.findByRole("heading", { name: /configuration saved/i })).toBeInTheDocument();
    expect(screen.getByText(/restart/i)).toBeInTheDocument();
    // A restart is needed, but setup is NOT unfinished: the operator must
    // not be sent back through the form to retype everything.
    expect(onConfigured).not.toHaveBeenCalled();
  });

  it("reports a refused setup without claiming it succeeded", async () => {
    const api = createMockApi("first-run");
    vi.spyOn(api, "completeFirstRun").mockRejectedValue(new Error("boom"));
    const onConfigured = vi.fn();

    renderFirstRun(api, onConfigured);
    await completeSetupForm();
    await userEvent.click(screen.getByRole("button", { name: /^Finish setup$/ }));

    expect(await screen.findByText(/could not save this configuration/i)).toBeInTheDocument();
    expect(onConfigured).not.toHaveBeenCalled();
    // Positive control: the same form, same clicks, against an API that
    // accepts the write, does reach onConfigured — so the assertion above
    // is about the rejection and not about the form never submitting.
    cleanup();
    const working = createMockApi("first-run");
    const secondOnConfigured = vi.fn();
    renderFirstRun(working, secondOnConfigured);
    await completeSetupForm();
    await userEvent.click(screen.getByRole("button", { name: /^Finish setup$/ }));
    await waitFor(() => expect(secondOnConfigured).toHaveBeenCalledTimes(1));
  });
});
