import { afterEach, describe, expect, it } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { BackupSetWizardPage } from "@shared/pages/BackupSetWizardPage";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import { wizardHostKeyChangedNode } from "@shared/state/wizardNodes";

function renderWizard(readOnly = false) {
  return render(
    <MemoryRouter>
      <PlatformProvider bridge={genericBridge}>
        <BackupSetWizardPage readOnly={readOnly} />
      </PlatformProvider>
    </MemoryRouter>
  );
}

// wizard.hostKeyChanged lives on the shared causl graph (issue #98 —
// state/wizardNodes.ts), not in this component's own useState, precisely
// so something outside the wizard (a host-key re-probe, another test) can
// change it while the wizard is open. That means test isolation depends
// on resetting the graph between tests, the same as
// PlatformContext.test.tsx — even though the wizard's other answers
// (completion, host trust, acknowledgement) are plain component state and
// need no such reset.
afterEach(() => {
  cleanup();
  resetGraphForTests();
});

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

  describe("SSH key import (step 2)", () => {
    it("disables Import until a key is pasted, then clears the paste box and never re-displays it", async () => {
      renderWizard();
      await userEvent.click(screen.getByRole("button", { name: "Authentication" }));
      await userEvent.click(screen.getByRole("radio", { name: /Import key/ }));

      const textarea = screen.getByLabelText(/private key/i);
      const importBtn = screen.getByRole("button", { name: "Import key" });
      expect(importBtn).toBeDisabled();

      // Synthetic fixture only — this is not a real key, on this app's own
      // rule that nothing resembling real key material ever appears in a
      // test or a commit.
      const fixtureKey = "FAKE-TEST-KEY-MATERIAL-not-a-real-key-0123456789";
      await userEvent.type(textarea, fixtureKey);
      expect(importBtn).toBeEnabled();

      await userEvent.click(importBtn);

      expect(screen.queryByLabelText(/private key/i)).toBeNull();
      expect(screen.getByText(/key imported/i)).toBeTruthy();
      expect(document.body.textContent).not.toContain(fixtureKey);
    });

    it("guards the private-key textarea against cloud spellcheck / autofill leakage (M2, #98 PR #145 review)", async () => {
      renderWizard();
      await userEvent.click(screen.getByRole("button", { name: "Authentication" }));
      await userEvent.click(screen.getByRole("radio", { name: /Import key/ }));

      const textarea = screen.getByLabelText(/private key/i);
      expect(textarea).toHaveAttribute("spellcheck", "false");
      expect(textarea).toHaveAttribute("autocomplete", "off");
      expect(textarea).toHaveAttribute("autocorrect", "off");
      expect(textarea).toHaveAttribute("autocapitalize", "off");
    });

    it("does not claim a shape-validation step that doesn't run (M3, #98 PR #145 review)", async () => {
      renderWizard();
      await userEvent.click(screen.getByRole("button", { name: "Authentication" }));
      await userEvent.click(screen.getByRole("radio", { name: /Import key/ }));

      const textarea = screen.getByLabelText(/private key/i);
      await userEvent.type(textarea, "FAKE-TEST-KEY-MATERIAL-not-a-real-key-0123456789");

      expect(screen.queryByText(/validated locally/i)).toBeNull();
      expect(screen.queryByText(/shape only/i)).toBeNull();
    });

    it("shows that a managed key already in use cannot simply be deleted", async () => {
      renderWizard();
      await userEvent.click(screen.getByRole("button", { name: "Authentication" }));
      await userEvent.click(screen.getByRole("radio", { name: /Use managed key/ }));
      expect(screen.getByText(/other backup sets/i)).toBeTruthy();
    });
  });

  describe("source fields carry through to review (step 1 -> step 6)", () => {
    it("reflects an edited hostname in the review step's source summary, not the original example", async () => {
      renderWizard();
      const hostField = screen.getByLabelText("Server hostname");
      await userEvent.clear(hostField);
      await userEvent.type(hostField, "warehouse-nas.internal");

      await userEvent.click(screen.getByRole("button", { name: "Review" }));
      expect(screen.getByText("warehouse-nas.internal")).toBeTruthy();
      expect(screen.queryByText("prod-db-01.internal")).toBeNull();
    });

    it("keeps the typed hostname after navigating away from step 1 and back", async () => {
      renderWizard();
      const hostField = screen.getByLabelText("Server hostname");
      await userEvent.clear(hostField);
      await userEvent.type(hostField, "warehouse-nas.internal");

      await userEvent.click(screen.getByRole("button", { name: "Authentication" }));
      await userEvent.click(screen.getByRole("button", { name: "Source" }));
      expect(screen.getByLabelText("Server hostname")).toHaveValue("warehouse-nas.internal");
    });
  });

  describe("the review step reads step 2/4's real answers, not fixed example text (#98)", () => {
    it("reflects the completion method chosen on step 4, surviving the trip to review", async () => {
      renderWizard();
      await userEvent.click(screen.getByRole("button", { name: "Discovery" }));
      await userEvent.click(screen.getByRole("radio", { name: /Atomic rename/ }));

      await userEvent.click(screen.getByRole("button", { name: "Review" }));
      expect(screen.getByText(/atomic rename/i)).toBeTruthy();
      expect(screen.queryByText(/completion marker/i)).toBeNull();
    });

    it("reflects a trust decision made on step 3, surviving the trip to review", async () => {
      renderWizard();
      await userEvent.click(screen.getByRole("button", { name: "Verify server" }));
      await userEvent.click(screen.getByRole("button", { name: "Trust host" }));

      await userEvent.click(screen.getByRole("button", { name: "Review" }));
      expect(screen.queryByText(/not yet trusted/i)).toBeNull();
      expect(screen.getByText(/^trusted$/i)).toBeTruthy();
    });
  });

  describe("host trust does not survive a hostname edit (M1, #98 PR #145 review)", () => {
    it("resets host trust once the hostname changes after Trust host, but not while it still matches", async () => {
      renderWizard();

      await userEvent.click(screen.getByRole("button", { name: "Verify server" }));
      await userEvent.click(screen.getByRole("button", { name: "Trust host" }));
      expect(screen.getByRole("button", { name: "Host trusted" })).toBeDisabled();

      // Re-visiting the same step with the hostname unchanged must not
      // un-trust it — only an actual edit should.
      await userEvent.click(screen.getByRole("button", { name: "Source" }));
      await userEvent.click(screen.getByRole("button", { name: "Verify server" }));
      expect(screen.getByRole("button", { name: "Host trusted" })).toBeDisabled();

      await userEvent.click(screen.getByRole("button", { name: "Source" }));
      const hostField = screen.getByLabelText("Server hostname");
      await userEvent.clear(hostField);
      await userEvent.type(hostField, "a-different-server.internal");
      await userEvent.click(screen.getByRole("button", { name: "Verify server" }));

      expect(screen.queryByRole("button", { name: "Host trusted" })).toBeNull();
      expect(screen.getByRole("button", { name: "Trust host" })).toBeEnabled();
    });
  });

  describe("a changed host key blocks saving (WP 2.3 acceptance: 'changed host key blocks operation')", () => {
    it("disables both gated save actions the instant the host key changes, even though acknowledged is still checked", async () => {
      renderWizard();
      await userEvent.click(screen.getByRole("button", { name: "Review" }));
      await userEvent.click(screen.getByRole("checkbox", { name: /remote backup will be removed only after/i }));

      const runNow = screen.getByRole("button", { name: /Save, enable & run/ });
      const enable = screen.getByRole("button", { name: /^Save & enable$/ });
      expect(runNow).toBeEnabled();
      expect(enable).toBeEnabled();

      // Simulates the shared host-trust state changing while the wizard is
      // open on the review step — the same fact DashboardPage surfaces
      // from app.sets as haltReason === "host-key-changed" — landing here
      // as a direct graph commit because nothing re-probes the host yet.
      act(() => {
        graph.commit("test/wizard-host-key-changed", (tx) => tx.set(wizardHostKeyChangedNode, true));
      });

      expect(runNow).toBeDisabled();
      expect(enable).toBeDisabled();
      expect(screen.getAllByText(/host key changed/i).length).toBeGreaterThan(0);
    });
  });

  describe("read-only mode (management actions disabled, #106)", () => {
    it("keeps save blocked even after acknowledgement when the app-wide readOnly node is true", async () => {
      renderWizard();
      act(() => {
        graph.commit("test/version-incompatible", (tx) =>
          tx.set(versionNode, {
            data: {
              ui: "1.4.0",
              service: "1.3.0",
              core: "1.3.0",
              rclone: "1.65.0",
              schema: 3,
              architecture: "amd64",
              buildCommit: "0000000",
              compatible: false
            },
            error: null,
            loading: false
          })
        );
      });

      await userEvent.click(screen.getByRole("button", { name: "Review" }));
      await userEvent.click(screen.getByRole("checkbox", { name: /remote backup will be removed only after/i }));

      expect(screen.getByRole("button", { name: /Save, enable & run/ })).toBeDisabled();
      expect(screen.getByRole("button", { name: /^Save & enable$/ })).toBeDisabled();
    });
  });
});
