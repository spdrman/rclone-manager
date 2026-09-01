import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { resetGraphForTests } from "@shared/state/graph";
import { BackupSetWizardPage } from "@shared/pages/BackupSetWizardPage";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import type { FieldHelpCopy } from "@shared/components/fieldHelpCopy";

/**
 * Issue #278's wizard follow-up (held back from the original PR while
 * #275/#288 restructured App.tsx, and cleared once #288 merged): the
 * wizard's honest fields are wired to their own copy, the same wiring
 * check field-help-pages.test.tsx runs for every other page. FieldHelp's
 * own suite proves the interaction works; this proves the RIGHT copy
 * reaches the RIGHT control on a page assembled across six steps rather
 * than one screen, and that the still-decorative controls (#299) were
 * left alone rather than gaining a plausible-looking pop-up.
 *
 * Two of the honest fields (key source, completion method) explain a
 * GROUP of radios rather than one control, so their copy is asserted
 * against the group container's own aria-describedby (a radiogroup div,
 * a fieldset), not against any one radio inside it.
 */

function expectHelp(control: HTMLElement, copy: FieldHelpCopy) {
  const describedBy = control.getAttribute("aria-describedby");
  expect(describedBy, "the control carries no aria-describedby").toBeTruthy();

  const described = document.getElementById(describedBy ?? "");
  expect(described, "aria-describedby points at nothing").not.toBeNull();
  expect(described?.textContent).toContain(copy.what);
  expect(described?.textContent).toContain(copy.example);
  expect(described?.textContent).toContain(copy.effect);
}

function expectNoHelp(control: HTMLElement) {
  expect(control.getAttribute("aria-describedby")).toBeNull();
}

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

describe("the wizard's honest fields are wired to their own copy", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  it("on the Source step", () => {
    renderWizard();

    expectHelp(screen.getByLabelText("Backup set name"), FIELD_HELP.wizardSetName);
    expectHelp(screen.getByLabelText("Server hostname"), FIELD_HELP.wizardHostname);
    expectHelp(screen.getByLabelText("SSH port"), FIELD_HELP.wizardSshPort);
    expectHelp(screen.getByLabelText("Username"), FIELD_HELP.wizardUsername);
  });

  it("on the Authentication step, including the group tooltip on the key-source radios", async () => {
    const user = userEvent.setup();
    renderWizard();

    await user.click(screen.getByRole("button", { name: "Authentication" }));

    // One tooltip for all three radios: the copy is a fact about the
    // GROUP ("only one of the three lets you finish this wizard"), which
    // no single radio's own label states.
    expectHelp(screen.getByRole("radiogroup", { name: "Key source" }), FIELD_HELP.wizardKeySource);

    // The private-key field only renders once "Import key" is selected;
    // the default choice is "Generate", whose own panel is decorative
    // and deliberately left unexplained (#299).
    await user.click(screen.getByRole("radio", { name: /Import key/ }));
    expectHelp(screen.getByLabelText(/private key/i), FIELD_HELP.wizardPrivateKey);
  });

  it("on the Discovery step, including the group tooltip on the completion-method fieldset", async () => {
    const user = userEvent.setup();
    renderWizard();

    await user.click(screen.getByRole("button", { name: "Discovery" }));

    expectHelp(screen.getByLabelText("Remote folder"), FIELD_HELP.wizardRemoteFolder);
    expectHelp(screen.getByLabelText("Include patterns"), FIELD_HELP.wizardIncludePatterns);
    // A <fieldset> is role "group", named by its own <legend>.
    expectHelp(screen.getByRole("group", { name: "Completion method" }), FIELD_HELP.wizardCompletionMethod);

    // Exclude patterns sits in the same grid, defaultValue-only and
    // unwired to anything the server reads (config.BackupSet has no
    // exclude field): no honest sentence exists for it, so it gets none.
    expectNoHelp(screen.getByLabelText("Exclude patterns"));
  });

  it("on the Storage & retention step, and not on its per-set retention controls", async () => {
    const user = userEvent.setup();
    renderWizard();

    await user.click(screen.getByRole("button", { name: "Storage & retention" }));

    // Both labels also wrap a caption sentence (and, for NAS destination,
    // a button) besides their own field name, so testing-library's exact
    // label-text match sees the whole concatenated content rather than
    // just the name; { exact: false } is a substring match against that
    // same content, which the field name still uniquely picks out here.
    expectHelp(screen.getByLabelText("NAS destination", { exact: false }), FIELD_HELP.wizardNasDestination);
    expectHelp(screen.getByLabelText("Application validation", { exact: false }), FIELD_HELP.wizardValidatorId);

    // #111 settled retention as one global policy; these three still draw
    // the per-set shape it warned against, so they stay unexplained.
    expectNoHelp(screen.getByLabelText("Daily"));
    expectNoHelp(screen.getByLabelText("Weekly"));
    expectNoHelp(screen.getByLabelText("Monthly"));
  });

  it("on the Review step's acknowledgement checkbox", async () => {
    const user = userEvent.setup();
    renderWizard();

    await user.click(screen.getByRole("button", { name: "Review" }));

    expectHelp(
      screen.getByRole("checkbox", { name: /remote backup will be removed only after/i }),
      FIELD_HELP.wizardAcknowledge
    );
  });
});
