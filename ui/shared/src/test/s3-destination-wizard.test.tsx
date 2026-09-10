import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { StorageDestinationsCard } from "@shared/pages/StorageDestinationsCard";
import { ApiProvider } from "@shared/api/ApiContext";
import { BackupManagerError } from "@shared/api/contracts";
import type {
  MediumPreflight,
  StorageMedium,
  StorageMediumSpec,
  StorageMediumUsage
} from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { resetGraphForTests } from "@shared/state/graph";

/**
 * G2.2 (#594): declaring an S3 destination from the browser, proving it
 * before it is written down, and what happens when a destination backups
 * already live on stops answering.
 *
 * Three properties are asserted here that nothing else in this repository
 * can assert, because they are about the ORDER things happen in and about
 * what a screen says rather than about what a function returns.
 *
 *   - Nothing is written until the destination has been proven. The whole
 *     reason this issue exists is that the engine could only ever prove a
 *     medium that was already in config.yaml, which made "declare it and
 *     find out" the supported flow.
 *   - The secret is submitted once and is gone from the page afterwards,
 *     and never appears in a later request. A canary proves it was in play
 *     and then proves it went nowhere else.
 *   - A failed re-verification changes no state and says so. FR-30's
 *     invariant is that at no instant may a backup have no confirmed
 *     readable copy, so the screen has to say "unreachable" and not
 *     anything that reads as "gone".
 */


const OFFSITE: StorageMedium = {
  id: "offsite_s3",
  type: "s3",
  bucket: "nas-backups",
  region: "us-east-1",
  prefix: "monthly",
  storageClass: "STANDARD_IA",
  uploadVerification: "readback",
  readsRequireRestore: false,
  isLocal: false,
  isDefault: false,
  connectionUnverified: false
};

function report(ok: boolean, failAt?: MediumPreflight["checks"][number]["step"]): MediumPreflight {
  const steps: Array<MediumPreflight["checks"][number]["step"]> = [
    "credentials",
    "reach",
    "deliverable",
    "write",
    "read_back",
    "storage_class",
    "verification",
    "delete"
  ];
  let seenFailure = false;
  return {
    medium: "offsite_s3",
    ok,
    checks: steps.map((step) => {
      if (step === failAt) {
        seenFailure = true;
        return { step, outcome: "failed", category: "configuration", detail: "the endpoint answered but does not hold this bucket" };
      }
      if (seenFailure) return { step, outcome: "skipped", category: "", detail: "this check did not run" };
      return { step, outcome: "passed", category: "", detail: "this check passed" };
    })
  };
}

const USAGE: StorageMediumUsage = {
  medium: "offsite_s3",
  placements: 148,
  backupSets: [
    { set: "api-server/var-backups", placements: 96, onlyCopyHere: 96 },
    { set: "nas-media/photos", placements: 52, onlyCopyHere: 52 }
  ]
};

function renderCard(overrides: Partial<ReturnType<typeof createMockApi>>) {
  const api = { ...createMockApi(), ...overrides } as ReturnType<typeof createMockApi>;
  render(
    <ApiProvider api={api}>
      <StorageDestinationsCard readOnly={false} />
    </ApiProvider>
  );
  return api;
}

/**
 * Reaches this wizard the way an operator now does (EPIC I, #668).
 *
 * "Add a destination" opens the add wizard — choose a backend, name the
 * instance, confirm — and the configure step this file tests is what
 * follows it. Only the NAVIGATION changed: every assertion below is
 * unchanged, because the property they pin is unchanged. This wizard is
 * still the one and only thing that writes a destination, and it still
 * writes nothing until the destination has been proven.
 *
 * The `role="group"` name is the same on both wizards, so the wait after
 * the last click is for a control only the configure step has.
 */
function fill(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

/**
 * The five add-path tests that were here have MOVED, not gone (I2.2,
 * issue #669), to destination-configure-wizard.test.tsx's
 * "the assertions #594 made about the add path, on the manifest
 * renderer".
 *
 * #668 repointed the card's "Add a destination" button at the
 * manifest-driven add wizard, and #669 repointed its configure step at
 * the manifest renderer. So this file's create-mode preamble reached a
 * pane the card can no longer open, and - the part that made a
 * byte-identical port impossible - the five asserted against
 * `preflightStorageMediumCandidate` and `createStorageMedium`, which the
 * new flow does not call. Their assertions therefore live against
 * `preflightStorageMediumConfiguration` and `configureStorageMedium`
 * instead, made against a backend that does not exist, which is
 * strictly stronger than making them against the one backend the old
 * form knew.
 *
 * Every one of them was proven to still fail against the defect it was
 * written for before this deletion was made, #594's canary included:
 * material on the probe, a save enabled without a pass, only the failed
 * step rendered, and the secret interpolated into the echoed command.
 * The last one found a real gap in the ported draft.
 *
 * What stays below is what is still reachable: the FR-30 re-test of a
 * destination backups already live on, its removal refusal, and the
 * edit pane, all of which S3DestinationWizard still serves.
 */

describe("re-testing a destination backups already live on (FR-30)", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("changes no state, names the count, and lists the sets", async () => {
    renderCard({
      listStorageMediums: vi.fn(() => Promise.resolve([OFFSITE])),
      preflightStorageMedium: vi.fn(() => Promise.resolve(report(false, "reach"))),
      getStorageMediumUsage: vi.fn(() => Promise.resolve(USAGE))
    });

    // "Test connection", not "Verify": the same idea had three names in
    // this product and this surface carried the odd one out (#622).
    fireEvent.click(await screen.findByRole("button", { name: "Test connection" }));
    await screen.findByText(/148 copies are recorded there/);

    const shown = document.body.textContent ?? "";
    // The words are the feature. "Unreachable" means this deployment
    // cannot ask, and it is emphatically not "the copy is gone"; an
    // operator who reads a red check as the second one does something
    // drastic.
    expect(shown).toContain("Nothing has been deleted and nothing will be");
    expect(shown).toContain("unreachable");
    expect(shown).toContain("retention will not prune against them");
    // Listed rather than counted: a number with nothing named is not
    // something anybody can act on.
    expect(shown).toContain("api-server/var-backups");
    expect(shown).toContain("nas-media/photos");
    // Editing stays available while it is failing, because a credential
    // that expired is exactly what an operator needs to be able to fix.
    expect(screen.getByRole("button", { name: "Edit" })).not.toBeDisabled();
  });

  it("renders the removal refusal with what is on the destination behind it", async () => {
    const remove = vi.fn(() =>
      Promise.reject(
        new BackupManagerError({
          code: "MEDIUM_IN_USE",
          message: "service: storage medium still holds copies: 148 copies on storage medium \"offsite_s3\"",
          correlationId: "cid_test"
        })
      )
    );
    renderCard({
      listStorageMediums: vi.fn(() => Promise.resolve([OFFSITE])),
      removeStorageMedium: remove,
      getStorageMediumUsage: vi.fn(() => Promise.resolve(USAGE))
    });

    fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
    await waitFor(() => expect(remove).toHaveBeenCalled());
    await screen.findByText(/still holds copies/);

    const shown = document.body.textContent ?? "";
    expect(shown).toContain("api-server/var-backups");
    expect(shown).toContain("only copy is here");
    // The destination is still on screen: a refused removal changes
    // nothing, and a row that vanished would say otherwise.
    expect(screen.getByText("offsite_s3")).toBeTruthy();
  });
});

describe("editing a destination", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("keeps the credential when the edit does not name a new one", async () => {
    const update = vi.fn<(id: string, spec: StorageMediumSpec) => Promise<StorageMedium>>(() =>
      Promise.resolve(OFFSITE)
    );
    renderCard({
      listStorageMediums: vi.fn(() => Promise.resolve([OFFSITE])),
      updateStorageMedium: update
    });

    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    await screen.findByRole("group", { name: "Edit destination offsite_s3" });

    fill("Region", "eu-west-1");
    fireEvent.click(screen.getByRole("button", { name: "Next: credentials" }));
    // The default on an edit is to keep the credential, because this API
    // reports nothing about it and a form cannot resubmit what it never
    // received.
    fireEvent.click(await screen.findByRole("button", { name: "Next: save" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(update).toHaveBeenCalled());
    const call = update.mock.calls[0];
    expect(call?.[0]).toBe("offsite_s3");
    expect(call?.[1].region).toBe("eu-west-1");
    // Undefined and not an empty object: an absent credentials block means
    // "keep the one configured", and an empty one would be
    // indistinguishable from a form that lost its reference.
    expect(call?.[1].credentials).toBeUndefined();
  });
});
