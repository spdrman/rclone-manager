import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { BackupSetWizardPage } from "@shared/pages/BackupSetWizardPage";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { resetGraphForTests } from "@shared/state/graph";

/**
 * Issue #299 — proves each of the wizard's decorative fields is actually
 * GONE from the rendered page, not merely still there and unread. Every
 * one of these used to be a real DOM control (uncontrolled, no onChange,
 * or a fabricated static fact) with nothing on the create-backup-set path
 * ever reading it back; see fieldHelpCopy.ts's module doc and this file's
 * companion comments in BackupSetWizardPage.tsx for the per-field reasoning.
 */

function renderWizard() {
  render(
    <MemoryRouter>
      <ApiProvider api={createMockApi()}>
        <PlatformProvider bridge={genericBridge}>
          <BackupSetWizardPage readOnly={false} />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

describe("the wizard no longer renders the decorative fields #299 removed", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  it("has no Exclude patterns field on the Discovery step", async () => {
    const user = userEvent.setup();
    renderWizard();

    await user.click(screen.getByRole("button", { name: "Discovery" }));

    expect(screen.queryByLabelText("Exclude patterns")).toBeNull();
    expect(screen.queryByText("Exclude patterns")).toBeNull();
  });

  it("has no per-set retention controls on the Storage & validation step", async () => {
    const user = userEvent.setup();
    renderWizard();

    await user.click(screen.getByRole("button", { name: "Storage & validation" }));

    expect(screen.queryByLabelText("Daily")).toBeNull();
    expect(screen.queryByLabelText("Weekly")).toBeNull();
    expect(screen.queryByLabelText("Monthly")).toBeNull();
    expect(screen.queryByLabelText("Week starts")).toBeNull();
    expect(screen.queryByText("Protect newest known-good backup — never deleted by retention")).toBeNull();
    expect(screen.queryByText("Retention")).toBeNull();
  });

  it("has no Checksum verification toggle, and a Transfer verification indicator that cannot be unchecked", async () => {
    const user = userEvent.setup();
    renderWizard();

    await user.click(screen.getByRole("button", { name: "Storage & validation" }));

    expect(screen.queryByText("Checksum verification")).toBeNull();
    expect(screen.queryByText("SHA-256")).toBeNull();

    const transferVerification = screen.getByRole("checkbox", { name: /Transfer verification/ });
    expect(transferVerification).toBeChecked();
    expect(transferVerification).toBeDisabled();
  });

  it("has no fabricated sample public key or authorized_keys instruction on the default Generate key-source panel", async () => {
    const user = userEvent.setup();
    renderWizard();

    await user.click(screen.getByRole("button", { name: "Authentication" }));

    // Default key source is "Generate" — confirmed by the panel below
    // rendering without clicking any radio first.
    expect(screen.queryByText(/ssh-ed25519 AAAA/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Copy public key" })).toBeNull();
    expect(screen.queryByText(/authorized_keys/)).toBeNull();
    expect(screen.getByText(/Generating a key on save isn.t available yet/)).toBeTruthy();
  });

  it("has no hardcoded managed-key picklist or fabricated in-use count on the Use managed key panel", async () => {
    const user = userEvent.setup();
    renderWizard();

    await user.click(screen.getByRole("button", { name: "Authentication" }));
    await user.click(screen.getByRole("radio", { name: /Use managed key/ }));

    expect(screen.queryByLabelText("Managed key")).toBeNull();
    expect(screen.queryByText(/nas-01-postgres/)).toBeNull();
    expect(screen.queryByText(/other backup sets/i)).toBeNull();
    expect(screen.getByText(/Reusing a managed key on save isn.t available yet/)).toBeTruthy();
  });

  it("has no Retention summary and no SHA-256 claim on the Review step", async () => {
    const user = userEvent.setup();
    renderWizard();

    await user.click(screen.getByRole("button", { name: "Review" }));

    expect(screen.queryByText("Retention")).toBeNull();
    expect(screen.queryByText("7 daily")).toBeNull();
    expect(screen.queryByText("13 weekly")).toBeNull();
    expect(screen.queryByText("12 monthly")).toBeNull();
    expect(screen.queryByText("SHA-256")).toBeNull();
  });
});
