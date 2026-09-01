import { afterEach, describe, expect, it } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { setsNode, versionNode } from "@shared/state/appNodes";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import type { FieldHelpCopy } from "@shared/components/fieldHelpCopy";
import { LoginPage } from "@shared/auth/LoginPage";
import { EnrollmentPage } from "@shared/auth/EnrollmentPage";
import { SettingsPage } from "@shared/pages/SettingsPage";
import { ActivityPage } from "@shared/pages/ActivityPage";
import { BackupsPage } from "@shared/pages/BackupsPage";
import { EditBackupSetDialog } from "@shared/pages/EditBackupSetDialog";
import type { BackupSet } from "@shared/types/backup";
import type { VersionInfo } from "@shared/types/operation";

/**
 * Issue #278: the page wiring, as distinct from the component.
 *
 * FieldHelp's own suite proves the pop-up behaves. This proves each field
 * is actually attached to one, and to the RIGHT one: the failure this
 * catches is a `help` prop dropped in a refactor, or an aria-describedby
 * left off a control so the copy is drawn on screen and never announced.
 * Both leave a page that looks finished, which is why they are asserted
 * rather than reviewed.
 *
 * The assertion goes through the control's own accessible description
 * rather than through the rendered pop-up, because that is the property
 * that has to hold: a description a screen reader reads, whether or not
 * anything is on screen.
 */

const VERSION: VersionInfo = {
  api: "v1", service: "1.3.0", buildCommit: "9f4c1ab", goVersion: "go1.27.0",
  engine: "1.68.2", configRevision: "cfg_9f4c1ab", ready: true, compatible: true
};

/** The control names its copy, all three parts of it, and nothing that is
 *  not copy. */
function expectHelp(control: HTMLElement, copy: FieldHelpCopy) {
  const describedBy = control.getAttribute("aria-describedby");
  expect(describedBy, "the control carries no aria-describedby").toBeTruthy();

  const described = document.getElementById(describedBy ?? "");
  expect(described, "aria-describedby points at nothing").not.toBeNull();
  expect(described?.textContent).toContain(copy.what);
  expect(described?.textContent).toContain(copy.example);
  expect(described?.textContent).toContain(copy.effect);
}

function seedSets(sets: BackupSet[]) {
  act(() => {
    graph.commit("test/seed-sets", (tx) => tx.set(setsNode, { data: sets, error: null, loading: false }));
  });
}

describe("every explained field is wired to its own copy", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  it("on the sign-in form", () => {
    render(
      <MemoryRouter>
        <ApiProvider api={createMockApi()}>
          <LoginPage onSignedIn={() => {}} />
        </ApiProvider>
      </MemoryRouter>
    );

    expectHelp(screen.getByLabelText("Username"), FIELD_HELP.loginUsername);
    expectHelp(screen.getByLabelText("Password"), FIELD_HELP.loginPassword);
  });

  it("on first-run enrolment", () => {
    render(
      <MemoryRouter>
        <ApiProvider api={createMockApi()}>
          <EnrollmentPage onEnrolled={() => {}} />
        </ApiProvider>
      </MemoryRouter>
    );

    expectHelp(screen.getByLabelText("Username"), FIELD_HELP.enrollUsername);
    expectHelp(screen.getByLabelText("Password"), FIELD_HELP.enrollPassword);
    expectHelp(screen.getByLabelText("Confirm password"), FIELD_HELP.enrollConfirm);
  });

  it("on the retention policy form, including the tier a chain is built from", async () => {
    await renderSettings();

    expectHelp(screen.getByLabelText("Timezone"), FIELD_HELP.retentionTimezone);
    expectHelp(screen.getByLabelText("Week starts on"), FIELD_HELP.weekStartsOn);
    expectHelp(
      screen.getByRole("checkbox", { name: /Protect the newest known-good backup/ }),
      FIELD_HELP.protectLastKnownGood
    );

    const tier = within(screen.getByRole("group", { name: "Tier 1" }));
    expectHelp(tier.getByLabelText("Name"), FIELD_HELP.tierName);
    expectHelp(tier.getByLabelText("Granularity"), FIELD_HELP.tierGranularity);
    expectHelp(tier.getByLabelText("Keep"), FIELD_HELP.tierKeep);
    expectHelp(tier.getByLabelText("Window unit"), FIELD_HELP.tierWindowUnit);

    // Period (days) exists only on a custom-period tier, so it has to be
    // reached the way an operator reaches it rather than asserted absent.
    fireEvent.change(tier.getByLabelText("Granularity"), { target: { value: "days" } });
    expectHelp(tier.getByLabelText("Period (days)"), FIELD_HELP.tierPeriodDays);
  });

  it("on the administrator password rotation form", async () => {
    await renderSettings();

    expectHelp(screen.getByLabelText("Current password"), FIELD_HELP.currentPassword);
    expectHelp(screen.getByLabelText("New password"), FIELD_HELP.newPassword);
    expectHelp(screen.getByLabelText("Confirm new password"), FIELD_HELP.confirmNewPassword);
  });

  it("on the activity filters", async () => {
    render(
      <ApiProvider api={createMockApi()}>
        <ActivityPage />
      </ApiProvider>
    );
    await act(async () => {});

    expectHelp(screen.getByLabelText("Backup set"), FIELD_HELP.activitySetFilter);
    expectHelp(screen.getByLabelText("Severity"), FIELD_HELP.activitySeverityFilter);
  });

  it("on the backups filter", async () => {
    const api = createMockApi();
    seedSets(await createMockApi().listSets());

    render(
      <MemoryRouter>
        <ApiProvider api={api}>
          <BackupsPage readOnly={false} />
        </ApiProvider>
      </MemoryRouter>
    );
    await act(async () => {});

    expectHelp(screen.getByLabelText("Filter by backup set"), FIELD_HELP.backupsSetFilter);
  });

  it("on the edit-backup-set form", async () => {
    const sets = await createMockApi().listSets();

    render(<EditBackupSetDialog set={sets[0]} open onClose={() => {}} />);

    expectHelp(screen.getByLabelText("Name"), FIELD_HELP.editSetName);
  });
});

async function renderSettings() {
  act(() => {
    graph.commit("test/seed-version", (tx) =>
      tx.set(versionNode, { data: VERSION, error: null, loading: false })
    );
  });

  render(
    <MemoryRouter>
      <ApiProvider api={createMockApi()}>
        <PlatformProvider bridge={genericBridge}>
          <SettingsPage readOnly={false} />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
  // The mock API answers with a deliberate latency, so the retention card
  // is still on its loading copy after one flush.
  await screen.findByLabelText("Timezone");
}
