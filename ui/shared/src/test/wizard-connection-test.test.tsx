/**
 * Issue #624 (H2.3): the add-backup-set wizard will not save a source
 * whose connection has not been proven.
 *
 * What the wizard already had was host IDENTITY. "Trust host" pins a
 * known_hosts line, which settles that the machine answering is the
 * machine whose fingerprint an operator compared, and settles nothing
 * else: not that the imported key authenticates, not that the account can
 * read the remote folder, not that anything the pipeline needs works. A
 * set could be saved, relied on, and only THEN tested, which is the order
 * this issue turns around.
 *
 * The S3 destination wizard has kept Save disabled until its candidate
 * check comes back ok since #594. These cases hold the source wizard to
 * the same shape, against the same mock api the rest of this suite runs
 * on, so the gate is proven by driving the screen rather than by reading
 * the component.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { BackupSetWizardPage } from "@shared/pages/BackupSetWizardPage";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import type { BackupManagerApi } from "@shared/api/contracts";
import { resetGraphForTests } from "@shared/state/graph";

function renderWizard(api: BackupManagerApi = createMockApi()) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <PlatformProvider bridge={genericBridge}>
          <BackupSetWizardPage readOnly={false} />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

/** Everything the Save buttons needed BEFORE this issue: a key imported,
 *  a host trusted, and remote deletion acknowledged. Deliberately stops
 *  short of the connection test, so each case below isolates that one
 *  precondition rather than flipping all four at once. */
async function everythingExceptTheConnectionTest() {
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

describe("the wizard will not save an unproven connection", () => {
  it("keeps Save disabled while every other precondition is met", async () => {
    renderWizard();
    await everythingExceptTheConnectionTest();

    expect(screen.getByRole("button", { name: "Save & enable" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Save, enable & run" })).toBeDisabled();
    // The hint says which precondition, in the same words as the
    // control that satisfies it. A disabled button with no reason is
    // the shape an operator reads as "this app is broken". Matched on
    // the whole sentence rather than on the control's name, which by
    // design appears twice on this step.
    expect(screen.getByText(/Test connection before saving/i)).toBeInTheDocument();
  });

  it("enables Save once the connection test passes", async () => {
    renderWizard();
    await everythingExceptTheConnectionTest();

    await userEvent.click(screen.getByRole("button", { name: /^Test connection$/ }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Save & enable" })).toBeEnabled());
  });

  it("leaves Save disabled when the connection test comes back not ok", async () => {
    const api = createMockApi();
    vi.spyOn(api, "testCandidateConnection").mockResolvedValue({
      ok: false,
      message: "the remote path could not be listed",
      checks: [
        { step: "list", outcome: "failed", category: "remote_path", detail: "/backups/postgresql/ could not be listed" }
      ]
    });
    renderWizard(api);
    await everythingExceptTheConnectionTest();

    await userEvent.click(screen.getByRole("button", { name: /^Test connection$/ }));
    // Twice on purpose: once as the failing step's own detail, once as
    // the banner that says what it means for saving.
    expect((await screen.findAllByText(/could not be listed/i)).length).toBeGreaterThan(0);
    expect(screen.getByRole("button", { name: "Save & enable" })).toBeDisabled();
  });

  it("checks the values on the form, not a default candidate", async () => {
    const api = createMockApi();
    const spy = vi.spyOn(api, "testCandidateConnection");
    renderWizard(api);
    await everythingExceptTheConnectionTest();

    await userEvent.click(screen.getByRole("button", { name: /^Test connection$/ }));
    await waitFor(() => expect(spy).toHaveBeenCalled());
    // The known_hosts line the operator actually trusted, and the key
    // they actually imported. A check run against anything else would be
    // proving a connection this wizard is not about to save.
    const sent = spy.mock.calls[0][0];
    expect(sent.knownHostsLine).toBe("mock-host.internal ssh-ed25519 AAAAC3NzaC1lZDI1NTE5mock");
    expect(sent.sshKeyId).toMatch(/^key_mock_/);
    expect(sent.remotePath).toBe("/backups/postgresql/");
  });

  it("makes an edited host undo a passing test", async () => {
    renderWizard();
    await everythingExceptTheConnectionTest();
    await userEvent.click(screen.getByRole("button", { name: /^Test connection$/ }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Save & enable" })).toBeEnabled());

    // Going back and pointing the wizard at a different machine has to
    // take the proof away with it, for exactly the reason trusting a
    // host does: a result that outlived the values it was about would be
    // a green tick standing for a connection nobody ever made.
    await userEvent.click(screen.getByRole("button", { name: "Source" }));
    const host = screen.getByLabelText(/host/i);
    await userEvent.clear(host);
    await userEvent.type(host, "other-machine.internal");

    await userEvent.click(screen.getByRole("button", { name: "Review" }));
    expect(screen.getByRole("button", { name: "Save & enable" })).toBeDisabled();
  });
});
