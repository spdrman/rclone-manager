import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ApiProvider } from "@shared/api/ApiContext";
import type { MediumPreflight, MediumPreflightCheck, StorageMediumSpec } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { S3DestinationWizard } from "@shared/pages/S3DestinationWizard";
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
 * backupdproject/backupd-tests had never driven the wizard past the
 * credentials pane, so the mock's projection of a submitted spec was on
 * nobody's path.
 */

/** The contract's step names, so a list of them below cannot name one
 *  the engine does not have. */
type Step = MediumPreflightCheck["step"];

const PLACEHOLDER_KEY_ID = "EXAMPLE-NOT-A-REAL-KEY";
const PLACEHOLDER_SECRET = "EXAMPLE-NOT-A-REAL-SECRET";

/** The five steps a refusal at `deliverable` skips, in the engine's order. */
const AFTER_DELIVERABLE: Step[] = ["write", "read_back", "storage_class", "verification", "delete"];

/** Everything ahead of `verification`, which all passes on the one report
 *  in this suite whose failure is in the middle rather than at the front. */
const BEFORE_VERIFICATION: Step[] = ["credentials", "reach", "deliverable", "write", "read_back", "storage_class"];

function candidateSpec(storageClass: string, uploadVerification = "readback"): StorageMediumSpec {
  return {
    id: "candidate_s3",
    type: "s3",
    bucket: "nas-backups",
    region: "us-east-1",
    storageClass,
    uploadVerification,
    credentials: { credentialsId: "cred-1" }
  };
}

/**
 * One step out of a report, by name.
 *
 * `step` is typed as the contract's own union rather than as `string`,
 * and in this file of all files that is the point: it is the exhibit for
 * consuming the union instead of restating it. Typed this way, the
 * missing `space` member the sibling fix found would have been a compile
 * error the moment anybody asserted on that step, instead of a drift
 * nothing could see. The `Step` alias below is what keeps the literal
 * lists that follow under the same rule.
 */
function stepOf(report: MediumPreflight, step: Step): MediumPreflight["checks"][number] {
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
    for (const step of AFTER_DELIVERABLE) {
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
      for (const step of AFTER_DELIVERABLE) {
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
 * The same defect as the storage class above, on the axis the first audit
 * could not see.
 *
 * I went looking for the MECHANISM that produced #633, a fact derived
 * twice and negated once, and that search came back clean. It could not
 * have found this: `verification` was an unconditional pass with a
 * hardcoded sentence, so `uploadVerification` was not read at all and
 * there was no expression to have backwards.
 *
 * What it costs is exactly what #633 costs. `S3DestinationWizard` offers
 * `attested` in its own dropdown, `config.Validate` refuses an s3 medium
 * that declares it (validateUploadVerificationIsAchievable), and the
 * engine's preflight fails it at this very step because the embedded
 * rclone's s3 backend serves no full-object digest
 * (mediumcheck.run.verification). So an operator picked it, saw eight
 * green steps and a Save button, and the gate that exists to teach them
 * the rule taught them its opposite.
 *
 * The engine's own comment on that step is the argument for this suite:
 * "The whole purpose of a preflight is that an operator learns that here
 * instead."
 */
describe("the fixture's candidate check reads the upload verification it was handed", () => {
  it("passes readback at the verification step, naming the step that proved it", async () => {
    const report = await createMockApi().preflightStorageMediumCandidate(
      candidateSpec("STANDARD", "readback")
    );

    expect(report.ok).toBe(true);
    const verification = stepOf(report, "verification");
    expect(verification.outcome).toBe("passed");
    // The engine credits the read-back step rather than re-reading the
    // object, and says so. A fixture that passed without that clause
    // would be claiming a proof nothing performed.
    expect(verification.detail).toContain("read-back");
  });

  it("refuses attested at the verification step, and still reports the delete after it", async () => {
    const report = await createMockApi().preflightStorageMediumCandidate(
      candidateSpec("STANDARD", "attested")
    );

    expect(report.ok).toBe(false);
    const verification = stepOf(report, "verification");
    expect(verification.outcome).toBe("failed");
    // The engine classifies it as a capability the endpoint does not have,
    // not as a configuration mistake, and a surface deciding whose problem
    // this is branches on the category.
    expect(verification.category).toBe("unsupported_capability");
    expect(verification.detail).toContain("attested");
    expect(verification.detail).toContain("readback");

    // The shape, which is the half a fixture that only ever fails at
    // `deliverable` cannot produce: everything BEFORE the failure passed,
    // and the probe is still rolled back afterwards, so `delete` passes
    // after a failed step rather than being skipped by it. A renderer
    // that assumed a failure ends the report draws this one wrong.
    for (const step of BEFORE_VERIFICATION) {
      expect(stepOf(report, step).outcome).toBe("passed");
    }
    expect(stepOf(report, "delete").outcome).toBe("passed");
  });

  it("skips the verification question entirely when the class cannot take delivery", async () => {
    // Both things wrong at once, and the earlier one wins. Nothing was
    // written, so there is no probe to attest to, and reporting a
    // capability refusal about an upload that never happened would be a
    // verdict on a step nobody ran.
    const report = await createMockApi().preflightStorageMediumCandidate(
      candidateSpec("DEEP_ARCHIVE", "attested")
    );

    expect(report.ok).toBe(false);
    expect(stepOf(report, "deliverable").outcome).toBe("failed");
    expect(stepOf(report, "verification").outcome).toBe("skipped");
  });
});

/**
 * The browser-shaped half, with the API deliberately NOT stubbed. Every
 * other case in this repository that drives the wizard hands it a
 * `preflightStorageMediumCandidate` of its own, which is how a fixture
 * that answered backwards survived: the surface was proven against a
 * report the test wrote and never against the report dev and e2e see.
 */
/**
 * Driven through the EDIT pane since #669 (I2.2).
 *
 * These three used to reach S3DestinationWizard's create mode through
 * the card's "Add a destination" button. That button now opens the
 * manifest-driven add wizard and hands off to the manifest-driven
 * configure step, so the create mode is not reachable from the card at
 * all - and #669's own tests cover that flow, against a backend that
 * does not exist.
 *
 * What these three are about is not the create: it is that THE FIXTURE
 * answers a candidate check the way a real endpoint would, and that Save
 * stays out of reach when it refuses. The edit pane runs the same
 * candidate check against the same fixture and gates Save on the same
 * report, so every assertion below is unchanged and only the preamble
 * moved. Repointed rather than deleted for exactly that reason: the
 * property is still true and still worth a test.
 */
describe("the S3 destination wizard driven against the unmodified fixture", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  async function wizardOnStorageClass(storageClass: string, uploadVerification = "readback") {
    const api = createMockApi();
    // A destination with a BUCKET, which is what this pane edits and what
    // a candidate check needs a class and a credential for. It used to be
    // `!m.isLocal`, and that stopped meaning "a bucket" when EPIC I
    // (#664) made a declared local volume an ordinary destination: those
    // are not the reserved entry either, so the first match became a
    // directory and this pane was handed one to type a bucket into.
    // Branching on the place a destination carries rather than on which
    // one is reserved is the same rule describeDestination follows.
    const existing = (await api.listStorageMediums()).find((m) => m.bucket);
    if (!existing) throw new Error("the fixture declares no destination with a bucket to edit");
    render(
      <ApiProvider api={api}>
        <S3DestinationWizard editing={existing} onClose={() => {}} onSaved={() => {}} />
      </ApiProvider>
    );

    await screen.findByRole("group", { name: `Edit destination ${existing.id}` });

    fireEvent.change(screen.getByLabelText("Region"), { target: { value: "us-east-1" } });
    fireEvent.change(screen.getByLabelText("Bucket"), { target: { value: "nas-backups" } });
    fireEvent.change(screen.getByLabelText("Storage class"), { target: { value: storageClass } });
    fireEvent.change(screen.getByLabelText("Upload verification"), { target: { value: uploadVerification } });
    fireEvent.click(screen.getByRole("button", { name: "Next: credentials" }));

    // The edit pane defaults to keeping the credential already
    // configured, which is right for an edit and is the one thing these
    // three do not want: what they are checking is a candidate report,
    // and that needs a credential this page can actually name.
    fireEvent.click(await screen.findByRole("radio", { name: /Paste a key/ }));
    fireEvent.change(await screen.findByLabelText("Access key id"), { target: { value: PLACEHOLDER_KEY_ID } });
    fireEvent.change(screen.getByLabelText("Secret access key"), { target: { value: PLACEHOLDER_SECRET } });
    fireEvent.click(screen.getByRole("button", { name: "Next: test connection" }));

    const next = await screen.findByRole("button", { name: "Next: save" }, { timeout: 5000 });
    const group = screen.getByRole("group", { name: `Edit destination ${existing.id}` });

    // Waiting for the report itself and not only for the pane. Save is
    // disabled while the check is in flight too, so an assertion made
    // before the report lands would read as "the archive class was
    // refused" on a fixture that had not answered yet. The fixture's
    // import is 400ms and its preflight another 700, both well outside
    // testing-library's default one second, so the window is wide.
    await waitFor(() => expect(within(group).getByText("delete")).toBeTruthy(), { timeout: 5000 });
    return { api, next, group, edited: existing.id };
  }

  it("lets an ordinary destination be saved, which is the flow the fixture exists to serve", async () => {
    const { api, next, group, edited } = await wizardOnStorageClass("STANDARD");

    await waitFor(() => expect(next).toBeEnabled(), { timeout: 5000 });
    // All eight, and no skips. A report that stopped short would still
    // leave Save reachable if `ok` were true, and that combination is
    // exactly what a fixture drifting from the engine looks like.
    expect(within(group).getAllByText("passed").length).toBe(8);
    expect(within(group).queryAllByText("skipped").length).toBe(0);

    fireEvent.click(next);
    fireEvent.click(await screen.findByRole("button", { name: "Save changes" }));

    // Saved means saved: the fixture keeps what it is given (#594), so
    // what was submitted has to come back out of the LIST rather than
    // only having been accepted by a call that returned. Through the
    // edit pane that is the class this pass set, on the destination it
    // was set on - the same assertion about the same property, keyed on
    // a destination that exists rather than on one this pane no longer
    // creates.
    await waitFor(async () => {
      const mediums = await api.listStorageMediums();
      expect(mediums.find((m) => m.id === edited)?.storageClass).toBe("STANDARD");
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

  it("keeps Save out of reach for a destination declaring a verification class it cannot achieve", async () => {
    // The class the wizard's own dropdown offers and this build cannot
    // deliver. Everything about this destination is ordinary except the
    // one field, so a Save that stayed enabled here would be enabled for
    // the exact configuration config.Validate rejects at load.
    const { next, group } = await wizardOnStorageClass("STANDARD", "attested");

    await waitFor(() => expect(next).toBeDisabled(), { timeout: 5000 });
    expect(within(group).getByText("failed(unsupported_capability)")).toBeTruthy();
    expect(group.textContent ?? "").toContain("Declare readback instead");

    // Six passes before the failure and one after it, which is the shape
    // an archive refusal never produces. It is the case worth having in a
    // browser test rather than only at the fixture: the renderer draws
    // every step in the engine's order, and this is the only report in
    // this suite where a pass FOLLOWS a failure.
    expect(within(group).getAllByText("passed").length).toBe(7);
    expect(within(group).queryAllByText("skipped").length).toBe(0);
  }, 15000);
});
