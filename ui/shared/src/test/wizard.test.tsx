import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { BackupSetWizardPage } from "@shared/pages/BackupSetWizardPage";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { BackupManagerError } from "@shared/api/contracts";
import type { BackupManagerApi } from "@shared/api/contracts";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import { wizardHostKeyChangedNode } from "@shared/state/wizardNodes";

// Issue #146 (B2.7): the wizard now reads useApi() (step 2's import, step
// 3's host-key probe, step 6's Save buttons all call through it), so
// every render needs an ApiProvider — createMockApi() is the same
// deterministic fixture ui/shared/e2e's own Playwright suite runs
// against (playwright.config.ts's own comment), reused here for the
// same reason.
function renderWizard(readOnly = false, api: BackupManagerApi = createMockApi()) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <PlatformProvider bridge={genericBridge}>
          <BackupSetWizardPage readOnly={readOnly} />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

/** Same providers as renderWizard, but with an actual route table
 *  (rather than a bare MemoryRouter) so navigate("/sets") on a
 *  successful save (issue #146) is observable — renderWizard alone has
 *  nowhere for that navigation to land. */
function renderWizardWithRoutes(api: BackupManagerApi) {
  return render(
    <MemoryRouter initialEntries={["/sets/new"]}>
      <ApiProvider api={api}>
        <PlatformProvider bridge={genericBridge}>
          <Routes>
            <Route path="/sets/new" element={<BackupSetWizardPage readOnly={false} />} />
            <Route path="/sets" element={<div>SETS LIST PAGE</div>} />
          </Routes>
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

/** Drives the wizard through Authentication (import a key), Verify
 *  server (wait for the probe, trust it), and on to Review — everything
 *  the Save buttons need to have a real sshKeyId/knownHostsLine to send
 *  (issue #146), EXCEPT acknowledgement, deliberately left to the
 *  caller: several tests below need to isolate "acknowledged" from the
 *  key-import/host-trust preconditions (M7, #146 review) rather than
 *  flip all three at once. completeWizardUpToReview (below) is this plus
 *  the acknowledgement click, for tests that just need every
 *  precondition met. */
async function advanceToReviewReady() {
  await userEvent.click(screen.getByRole("button", { name: "Authentication" }));
  await userEvent.click(screen.getByRole("radio", { name: /Import key/ }));
  await userEvent.type(screen.getByLabelText(/private key/i), "FAKE-TEST-KEY-MATERIAL-not-a-real-key-0123456789");
  await userEvent.click(screen.getByRole("button", { name: "Import key" }));
  await screen.findByText(/key imported/i);

  await userEvent.click(screen.getByRole("button", { name: "Verify server" }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Trust host" })).toBeEnabled());
  await userEvent.click(screen.getByRole("button", { name: "Trust host" }));

  await userEvent.click(screen.getByRole("button", { name: "Review" }));
}

async function completeWizardUpToReview() {
  await advanceToReviewReady();
  await userEvent.click(screen.getByRole("checkbox", { name: /remote backup will be removed only after/i }));
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
    // Every OTHER save precondition (imported key, trusted host) is
    // satisfied first, so this isolates acknowledgement as the one
    // variable under test — see M7 (#146 review) on why those two now
    // also gate the button.
    await advanceToReviewReady();

    const save = screen.getByRole("button", { name: /Save, enable & run/ });
    expect(save).toBeDisabled();

    await userEvent.click(screen.getByRole("checkbox", { name: /remote backup will be removed only after/i }));
    expect(save).toBeEnabled();
  });

  // M7 (#146 review): the wizard's own save-preconditions gap. Save used
  // to stay clickable with no key imported and no host trusted -
  // clicking it fired handleSave, which rejected the request via its own
  // ad hoc guard rather than the button ever refusing to be clicked.
  it("keeps Save disabled without an imported key or a trusted host, even after acknowledging (M7, #146 review)", async () => {
    renderWizard();
    await userEvent.click(screen.getByRole("button", { name: "Review" }));
    await userEvent.click(screen.getByRole("checkbox", { name: /remote backup will be removed only after/i }));

    expect(screen.getByRole("button", { name: /Save, enable & run/ })).toBeDisabled();
    expect(screen.getByRole("button", { name: /^Save & enable$/ })).toBeDisabled();
  });

  it("warns when stable-size completion is chosen", async () => {
    renderWizard();
    await userEvent.click(screen.getByRole("button", { name: "Discovery" }));
    await userEvent.click(screen.getByRole("radio", { name: /Stable file size/ }));
    expect(screen.getByText(/infers completion and provides less assurance/)).toBeTruthy();
  });

  it("does not offer a native storage picker on a platform without one", async () => {
    renderWizard();
    await userEvent.click(screen.getByRole("button", { name: "Storage & validation" }));
    expect(screen.getByRole("button", { name: "Validate path" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Browse volumes/ })).toBeNull();
  });

  it("never renders a private key", async () => {
    renderWizard();
    await userEvent.click(screen.getByRole("button", { name: "Authentication" }));
    // Default key source is "Generate", whose panel (#299) says plainly
    // that this path can't be saved yet rather than showing a fixed
    // sample key, so this is the positive control that we're on the
    // right panel at all.
    expect(screen.getByText(/Generating a key on save isn.t available yet/)).toBeTruthy();
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

      // Import now goes through api.importSSHKey (issue #146), a real
      // (mocked) async call — findByText waits for that to resolve
      // instead of asserting the instant before it has.
      expect(await screen.findByText(/key imported/i)).toBeTruthy();
      expect(screen.queryByLabelText(/private key/i)).toBeNull();
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

    // #299: this used to assert a fabricated "Already installed on 2
    // other backup sets" fact with no managed-key store behind it. The
    // panel now says plainly that this path can't be saved yet, same as
    // "Generate" above.
    it("says a managed key can't be reused on save yet, rather than showing a fabricated in-use count", async () => {
      renderWizard();
      await userEvent.click(screen.getByRole("button", { name: "Authentication" }));
      await userEvent.click(screen.getByRole("radio", { name: /Use managed key/ }));
      expect(screen.getByText(/Reusing a managed key on save isn.t available yet/)).toBeTruthy();
      expect(screen.queryByText(/other backup sets/i)).toBeNull();
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
      // Issue #146: "Trust host" stays disabled until the real (mocked)
      // host-key probe resolves — see BackupSetWizardPage's probeHost.
      await waitFor(() => expect(screen.getByRole("button", { name: "Trust host" })).toBeEnabled());
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
      await waitFor(() => expect(screen.getByRole("button", { name: "Trust host" })).toBeEnabled());
      await userEvent.click(screen.getByRole("button", { name: "Trust host" }));
      expect(screen.getByRole("button", { name: "Host trusted" })).toBeDisabled();

      // Re-visiting the same step with the hostname unchanged must not
      // un-trust it — only an actual edit should. No new probe fires
      // either (same host:port already probed), so no wait is needed
      // here.
      await userEvent.click(screen.getByRole("button", { name: "Source" }));
      await userEvent.click(screen.getByRole("button", { name: "Verify server" }));
      expect(screen.getByRole("button", { name: "Host trusted" })).toBeDisabled();

      await userEvent.click(screen.getByRole("button", { name: "Source" }));
      const hostField = screen.getByLabelText("Server hostname");
      await userEvent.clear(hostField);
      await userEvent.type(hostField, "a-different-server.internal");
      await userEvent.click(screen.getByRole("button", { name: "Verify server" }));

      expect(screen.queryByRole("button", { name: "Host trusted" })).toBeNull();
      // A new host means a new probe: "Trust host" only becomes
      // enabled again once that (mocked) probe resolves.
      await waitFor(() => expect(screen.getByRole("button", { name: "Trust host" })).toBeEnabled());
    });
  });

  describe("a changed host key blocks saving (WP 2.3 acceptance: 'changed host key blocks operation')", () => {
    it("disables both gated save actions the instant the host key changes, even though acknowledged is still checked", async () => {
      renderWizard();
      // M7 (#146 review): the key-import/host-trust preconditions are
      // satisfied first (advanceToReviewReady), same as the
      // acknowledgement test above, so this isolates the host-key-change
      // effect as the one variable under test.
      await advanceToReviewReady();
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
              api: "v0",
              service: "1.3.0",
              buildCommit: "0000000",
              goVersion: "go1.27.0",
              engine: "1.65.0",
              configRevision: "cfg_0000000",
              ready: true,
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

  // Issue #146 (B2.7): RED plan's "the wizard's Save buttons actually
  // call the create endpoint and handle its response (success ->
  // navigate/confirm, failure -> surface the error, not a silent
  // no-op)".
  describe("the Save buttons persist a backup set for real (issue #146)", () => {
    it("Save & enable calls createBackupSet with disabled:false, runImmediately:false and navigates to the sets list on success", async () => {
      const api = createMockApi();
      const spy = vi.spyOn(api, "createBackupSet");
      renderWizardWithRoutes(api);

      await completeWizardUpToReview();
      await userEvent.click(screen.getByRole("button", { name: /^Save & enable$/ }));

      expect(await screen.findByText("SETS LIST PAGE")).toBeTruthy();
      expect(spy).toHaveBeenCalledTimes(1);
      const req = spy.mock.calls[0][0];
      expect(req.disabled).toBe(false);
      expect(req.runImmediately).toBe(false);
      expect(req.sshKeyId).toBeTruthy();
      expect(req.knownHostsLine).toBeTruthy();
    });

    it("Save, enable & run calls createBackupSet with runImmediately:true", async () => {
      const api = createMockApi();
      const spy = vi.spyOn(api, "createBackupSet");
      renderWizardWithRoutes(api);

      await completeWizardUpToReview();
      await userEvent.click(screen.getByRole("button", { name: /Save, enable & run/ }));

      await screen.findByText("SETS LIST PAGE");
      const req = spy.mock.calls[0][0];
      expect(req.disabled).toBe(false);
      expect(req.runImmediately).toBe(true);
    });

    it("Save disabled calls createBackupSet with disabled:true and needs no acknowledgement", async () => {
      const api = createMockApi();
      const spy = vi.spyOn(api, "createBackupSet");
      renderWizardWithRoutes(api);

      await userEvent.click(screen.getByRole("button", { name: "Authentication" }));
      await userEvent.click(screen.getByRole("radio", { name: /Import key/ }));
      await userEvent.type(screen.getByLabelText(/private key/i), "FAKE-TEST-KEY-MATERIAL-not-a-real-key-0123456789");
      await userEvent.click(screen.getByRole("button", { name: "Import key" }));
      await screen.findByText(/key imported/i);
      await userEvent.click(screen.getByRole("button", { name: "Verify server" }));
      await waitFor(() => expect(screen.getByRole("button", { name: "Trust host" })).toBeEnabled());
      await userEvent.click(screen.getByRole("button", { name: "Trust host" }));
      await userEvent.click(screen.getByRole("button", { name: "Review" }));
      // Deliberately no acknowledgement checkbox click — this is the
      // whole point of the "Save disabled" escape hatch.

      await userEvent.click(screen.getByRole("button", { name: "Save disabled" }));

      await screen.findByText("SETS LIST PAGE");
      const req = spy.mock.calls[0][0];
      expect(req.disabled).toBe(true);
      expect(req.runImmediately).toBe(false);
    });

    it("sends the chosen application validator's id, and nothing that could name an executable (issue #162)", async () => {
      const api = createMockApi();
      const spy = vi.spyOn(api, "createBackupSet");
      renderWizardWithRoutes(api);

      await userEvent.click(screen.getByRole("button", { name: "Storage & validation" }));
      const picker = await screen.findByLabelText(/application validation/i);
      // A real picklist, not the decorative toggle #98 shipped: the
      // options come from the backend's own registered catalog.
      await waitFor(() => expect(within(picker as HTMLSelectElement).getAllByRole("option").length).toBeGreaterThan(1));
      await userEvent.selectOptions(picker, "trailer-marker");

      await completeWizardUpToReview();
      await userEvent.click(screen.getByRole("button", { name: /^Save & enable$/ }));

      await screen.findByText("SETS LIST PAGE");
      const req = spy.mock.calls[0][0];
      expect(req.validatorId).toBe("trailer-marker");

      const banned = ["command", "executable", "argv", "script", "shell", "binary", "exec"];
      const offending = Object.keys(req).filter((k) => banned.some((w) => k.toLowerCase().includes(w)));
      expect(offending).toEqual([]);
    });

    it("sends no validator at all when the operator leaves the picklist on its default (issue #162)", async () => {
      const api = createMockApi();
      const spy = vi.spyOn(api, "createBackupSet");
      renderWizardWithRoutes(api);

      await completeWizardUpToReview();
      await userEvent.click(screen.getByRole("button", { name: /^Save & enable$/ }));

      await screen.findByText("SETS LIST PAGE");
      expect(spy.mock.calls[0][0].validatorId).toBeUndefined();
    });

    it("says so when the validator catalog cannot be loaded, rather than showing an empty picklist (issue #162)", async () => {
      const api = createMockApi();
      vi.spyOn(api, "listValidators").mockRejectedValue(
        new BackupManagerError({ code: "INTERNAL", message: "nope", correlationId: "cid_2" })
      );
      renderWizard(false, api);

      await userEvent.click(screen.getByRole("button", { name: "Storage & validation" }));
      expect(await screen.findByText(/could not load the available validators/i)).toBeTruthy();
    });

    // M4 (#194 review): the create succeeded, the requested run did not
    // start, and the response says so in run_error. Every assertion above
    // is the positive control for this one — with no run_error the wizard
    // navigates away, so "does not navigate" here is a behaviour that
    // depends on the field rather than on the wizard never navigating.
    it("says the run did not start, rather than reporting a plain success, when the response carries run_error", async () => {
      const api = createMockApi();
      vi.spyOn(api, "createBackupSet").mockResolvedValue({
        id: "api/x", sourceName: "api", name: "x", host: "h", port: 22, user: "u",
        remotePath: "/r", localPath: "/l", include: [], completionStrategy: "rename",
        disabled: false, readOnly: false,
        runError: "the destructive gate is closed, so the run was not submitted"
      });
      renderWizardWithRoutes(api);

      await completeWizardUpToReview();
      await userEvent.click(screen.getByRole("button", { name: /Save, enable & run/ }));

      expect(await screen.findByText(/Saved, but the run did not start/i)).toBeTruthy();
      expect(
        screen.getByText(/the destructive gate is closed, so the run was not submitted/i)
      ).toBeTruthy();
      // Not navigated away: the sets list showing the new set is exactly
      // the "it worked" reading this response contradicts.
      expect(screen.queryByText("SETS LIST PAGE")).toBeNull();
      // And no second set can be created by pressing Save again.
      expect(screen.getByRole("button", { name: /Save, enable & run/ })).toBeDisabled();
    });

    it("surfaces a failed save inline instead of navigating or silently doing nothing", async () => {
      const api = createMockApi();
      vi.spyOn(api, "createBackupSet").mockRejectedValue(
        new BackupManagerError({ code: "INVALID_REQUEST", message: "remote_path is required", correlationId: "cid_1" })
      );
      renderWizardWithRoutes(api);

      await completeWizardUpToReview();
      await userEvent.click(screen.getByRole("button", { name: /^Save & enable$/ }));

      expect(await screen.findByText("remote_path is required")).toBeTruthy();
      expect(screen.queryByText("SETS LIST PAGE")).toBeNull();
    });

    // Before M7 (#146 review), this scenario was reachable by clicking
    // Save: the button stayed enabled with no key imported, and
    // handleSave's own ad hoc guard rejected the request after the
    // click. Save is now structurally disabled for this same
    // combination instead — proven here by confirming the button itself
    // is disabled and createBackupSet is never called, rather than by
    // clicking a button that no longer accepts clicks.
    it("keeps Save disabled when the key source isn't the wired 'import' path, instead of allowing a doomed request", async () => {
      const api = createMockApi();
      const spy = vi.spyOn(api, "createBackupSet");
      renderWizardWithRoutes(api);

      // Default keySource is "generate" — never touch Authentication.
      await userEvent.click(screen.getByRole("button", { name: "Verify server" }));
      await waitFor(() => expect(screen.getByRole("button", { name: "Trust host" })).toBeEnabled());
      await userEvent.click(screen.getByRole("button", { name: "Trust host" }));
      await userEvent.click(screen.getByRole("button", { name: "Review" }));
      await userEvent.click(screen.getByRole("checkbox", { name: /remote backup will be removed only after/i }));

      expect(screen.getByRole("button", { name: /^Save & enable$/ })).toBeDisabled();
      expect(spy).not.toHaveBeenCalled();
      expect(screen.queryByText("SETS LIST PAGE")).toBeNull();
    });
  });
});
