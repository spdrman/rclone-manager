import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { BackupSetWizardPage } from "@shared/pages/BackupSetWizardPage";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";

function renderWizard() {
  return render(
    <MemoryRouter>
      <PlatformProvider bridge={genericBridge}>
        <BackupSetWizardPage readOnly={false} />
      </PlatformProvider>
    </MemoryRouter>
  );
}

describe("add backup set wizard", () => {
  it("has six grouped steps, not a dozen screens", () => {
    renderWizard();
    expect(screen.getAllByRole("listitem").length).toBe(6);
  });

  it("blocks saving until remote deletion is acknowledged", async () => {
    renderWizard();
    await userEvent.click(screen.getByRole("button", { name: "Review" }));

    const save = screen.getByRole("button", { name: /Save, enable & run/ });
    expect(save).toBeDisabled();

    await userEvent.click(screen.getByRole("checkbox", { name: /remote backup will be removed only after/i }));
    expect(save).toBeEnabled();
  });

  it("warns when stable-size completion is chosen", async () => {
    renderWizard();
    await userEvent.click(screen.getByRole("button", { name: "Discovery" }));
    await userEvent.click(screen.getByRole("radio", { name: /Stable file size/ }));
    expect(screen.getByText(/infers completion and provides less assurance/)).toBeTruthy();
  });

  it("does not offer a native storage picker on a platform without one", async () => {
    renderWizard();
    await userEvent.click(screen.getByRole("button", { name: "Storage & retention" }));
    expect(screen.getByRole("button", { name: "Validate path" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Browse volumes/ })).toBeNull();
  });

  it("never renders a private key", async () => {
    renderWizard();
    await userEvent.click(screen.getByRole("button", { name: "Authentication" }));
    expect(screen.getByText(/ssh-ed25519 AAAA/)).toBeTruthy();
    // Match key MATERIAL, not the words. The step deliberately says
    // "Private keys stay on this NAS and are never shown after
    // creation", which is the correct thing to tell an operator, and a
    // bare /PRIVATE KEY/i fired on that reassurance rather than on a
    // leak. A PEM header cannot appear by accident.
    expect(document.body.textContent).not.toMatch(/-----BEGIN [A-Z ]*PRIVATE KEY-----/);
  });
});
