import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ApiProvider } from "@shared/api/ApiContext";
import type { MediumPreflight, StorageMediumSpec } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { StorageDestinationsCard } from "@shared/pages/StorageDestinationsCard";
import { resetGraphForTests } from "@shared/state/graph";

/**
 * Issue #633: the fixture's candidate check answered the archive question
 * upside down, so against `createMockApi` a candidate on STANDARD failed
 * at `deliverable` and a candidate on DEEP_ARCHIVE passed.
 *
 * Both halves of that matter, and a test that asserted only one of them
 * would be worth very little: a helper hardcoded to "always green" passes
 * the STANDARD case, and a helper hardcoded to "always red" passes the
 * archive case. Only asserting the two together says the fixture is
 * reading the storage class at all, which is the property that broke.
 *
 * The two describes below are the same defect at the two altitudes it was
 * felt at. The first asks the fixture directly, because the fixture is
 * what was wrong. The second drives the real wizard against it with
 * nothing stubbed, because the consequence was not "a fixture disagrees
 * with the engine" but "no destination can be saved in the browser":
 * `S3DestinationWizard` gates Save on `report !== null && report.ok`, and
 * an inverted report turns that gate into a wall for every ordinary class
 * and a door for the one class `config.Validate` refuses a retention tier
 * (#442). Nothing caught it because `s3-destination-wizard.test.tsx`
 * drives the component against its own stub, and the browser suite in
 * spdrman/rclone-manager-tests had never driven the wizard past the
 * credentials pane, so the mock's projection of a submitted spec was on
 * nobody's path.
 */

const PLACEHOLDER_KEY_ID = "EXAMPLE-NOT-A-REAL-KEY";
const PLACEHOLDER_SECRET = "EXAMPLE-NOT-A-REAL-SECRET";

function candidateSpec(storageClass: string): StorageMediumSpec {
  return {
    id: "candidate_s3",
    type: "s3",
    bucket: "nas-backups",
    region: "us-east-1",
    storageClass,
    uploadVerification: "readback",
    credentials: { credentialsId: "cred-1" }
  };
}

function stepOf(report: MediumPreflight, step: string): MediumPreflight["checks"][number] {
  const found = report.checks.find((c) => c.step === step);
  if (!found) throw new Error(`the report carries no ${step} step: ${JSON.stringify(report.checks)}`);
  return found;
}

describe("the fixture's candidate check reads the storage class it was handed", () => {
  it("passes a candidate on an ordinary class, and says why at the deliverable step", async () => {
    const report = await createMockApi().preflightStorageMediumCandidate(candidateSpec("STANDARD"));

    expect(report.ok).toBe(true);
    expect(stepOf(report, "deliverable").outcome).toBe("passed");
    // The five steps after `deliverable` are the ones a refusal skips, so
    // asserting them here is what tells a pass apart from a report that
    // merely stopped short without saying so.
    for (const step of ["write", "read_back", "storage_class", "verification", "delete"]) {
      expect(stepOf(report, step).outcome).toBe("passed");
    }
  });

  it("refuses a candidate on an archive class at the deliverable step, and skips the five after it", async () => {
    for (const archiveClass of ["GLACIER", "DEEP_ARCHIVE"]) {
      const report = await createMockApi().preflightStorageMediumCandidate(candidateSpec(archiveClass));

      expect(report.ok).toBe(false);
      const deliverable = stepOf(report, "deliverable");
      expect(deliverable.outcome).toBe("failed");
      expect(deliverable.category).toBe("configuration");
      expect(deliverable.detail).toContain(archiveClass);
      for (const step of ["write", "read_back", "storage_class", "verification", "delete"]) {
        expect(stepOf(report, step).outcome).toBe("skipped");
      }
    }
  });

  /**
   * The property the fix is actually for. Two call sites deriving one fact
   * independently is what let them diverge, so this asserts they agree
   * rather than asserting each one's answer separately: whatever the
   * fixture decides about a class, it has to decide the same thing about
   * a declared medium of that class and about a candidate for it.
   *
   * `offsite_s3` is STANDARD_IA and `offsite_cold` is DEEP_ARCHIVE in the
   * default fixture, so this covers a class on each side of the line
   * without either case being one the helper could special-case its way
   * through.
   */
  it("gives a candidate the same verdict the by-id check gives a declared medium of that class", async () => {
    for (const [mediumId, storageClass] of [
      ["offsite_s3", "STANDARD_IA"],
      ["offsite_cold", "DEEP_ARCHIVE"]
    ]) {
      const api = createMockApi();
      const declared = await api.preflightStorageMedium(mediumId);
      const candidate = await api.preflightStorageMediumCandidate(candidateSpec(storageClass));

      expect(candidate.ok).toBe(declared.ok);
      expect(stepOf(candidate, "deliverable").outcome).toBe(stepOf(declared, "deliverable").outcome);
    }
  });
});

/**
 * The browser-shaped half, with the API deliberately NOT stubbed. Every
 * other case in this repository that drives the wizard hands it a
 * `preflightStorageMediumCandidate` of its own, which is how a fixture
 * that answered backwards survived: the surface was proven against a
 * report the test wrote and never against the report dev and e2e see.
 */
describe("the S3 destination wizard driven against the unmodified fixture", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  async function wizardOnStorageClass(storageClass: string) {
    const api = createMockApi();
    render(
      <ApiProvider api={api}>
        <StorageDestinationsCard readOnly={false} />
      </ApiProvider>
    );

    fireEvent.click(await screen.findByRole("button", { name: "Add a destination" }));
    await screen.findByRole("group", { name: "Add a destination" });

    fireEvent.change(screen.getByLabelText("Destination id"), { target: { value: "candidate_s3" } });
    fireEvent.change(screen.getByLabelText("Region"), { target: { value: "us-east-1" } });
    fireEvent.change(screen.getByLabelText("Bucket"), { target: { value: "nas-backups" } });
    fireEvent.change(screen.getByLabelText("Storage class"), { target: { value: storageClass } });
    fireEvent.click(screen.getByRole("button", { name: "Next: credentials" }));

    fireEvent.change(await screen.findByLabelText("Access key id"), { target: { value: PLACEHOLDER_KEY_ID } });
    fireEvent.change(screen.getByLabelText("Secret access key"), { target: { value: PLACEHOLDER_SECRET } });
    fireEvent.click(screen.getByRole("button", { name: "Next: test connection" }));

    const next = await screen.findByRole("button", { name: "Next: save" }, { timeout: 5000 });
    const group = screen.getByRole("group", { name: "Add a destination" });

    // Waiting for the report itself and not only for the pane. Save is
    // disabled while the check is in flight too, so an assertion made
    // before the report lands would read as "the archive class was
    // refused" on a fixture that had not answered yet. The fixture's
    // import is 400ms and its preflight another 700, both well outside
    // testing-library's default one second, so the window is wide.
    await waitFor(() => expect(within(group).getByText("delete")).toBeTruthy(), { timeout: 5000 });
    return { api, next, group };
  }

  it("lets an ordinary destination be saved, which is the flow the fixture exists to serve", async () => {
    const { api, next, group } = await wizardOnStorageClass("STANDARD");

    await waitFor(() => expect(next).toBeEnabled(), { timeout: 5000 });
    // All eight, and no skips. A report that stopped short would still
    // leave Save reachable if `ok` were true, and that combination is
    // exactly what a fixture drifting from the engine looks like.
    expect(within(group).getAllByText("passed").length).toBe(8);
    expect(within(group).queryAllByText("skipped").length).toBe(0);

    fireEvent.click(next);
    fireEvent.click(await screen.findByRole("button", { name: "Save destination" }));

    // Saved means saved: the fixture keeps what it is given (#594), so the
    // destination has to come back out of the list rather than only having
    // been accepted by a call that returned.
    await waitFor(async () => {
      const mediums = await api.listStorageMediums();
      expect(mediums.map((m) => m.id)).toContain("candidate_s3");
    }, { timeout: 5000 });
  }, 15000);

  it("keeps Save out of reach for an archive destination, and names the class in the refusal", async () => {
    const { next, group } = await wizardOnStorageClass("DEEP_ARCHIVE");

    await waitFor(() => expect(next).toBeDisabled(), { timeout: 5000 });
    expect(within(group).getByText("failed(configuration)")).toBeTruthy();
    // The one sentence an operator has to act on. A retention tier cannot
    // deliver to an archive class, and the wizard is where they find that
    // out rather than at the save that config.Validate refuses (#442).
    expect(group.textContent ?? "").toContain("cannot be read until an explicit restore has finished");
  }, 15000);
});
