import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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

const CANARY_SECRET = "CANARY-594-wizard-4c81de60fa93-DO-NOT-SEND";
const PLACEHOLDER_KEY_ID = "EXAMPLE-NOT-A-REAL-KEY";

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

async function openTheWizard(api: Partial<ReturnType<typeof createMockApi>>) {
  const built = renderCard({ listStorageMediums: vi.fn(() => Promise.resolve([])), ...api });
  const add = await screen.findByRole("button", { name: "Add a destination" });
  fireEvent.click(add);
  await screen.findByRole("group", { name: "Add a destination" });
  return built;
}

function fill(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

async function describeDestinationThroughStepOne() {
  fill("Destination id", "offsite_s3");
  fill("Region", "us-east-1");
  fill("Bucket", "nas-backups");
  fill("Prefix (optional)", "monthly");
  fireEvent.change(screen.getByLabelText("Storage class"), { target: { value: "STANDARD_IA" } });
  fireEvent.click(screen.getByRole("button", { name: "Next: credentials" }));
  await screen.findByLabelText("Access key id");
}

describe("the S3 destination wizard", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("writes nothing until the destination has been proven", async () => {
    const importCredentials = vi.fn<(id: string, secret: string) => Promise<string>>(() =>
      Promise.resolve("cred-1")
    );
    const preflight = vi.fn<(spec: StorageMediumSpec) => Promise<MediumPreflight>>(() =>
      Promise.resolve(report(true))
    );
    const create = vi.fn(() => Promise.resolve(OFFSITE));
    const api = await openTheWizard({
      importStorageCredentials: importCredentials,
      preflightStorageMediumCandidate: preflight,
      createStorageMedium: create
    });

    await describeDestinationThroughStepOne();
    fill("Access key id", PLACEHOLDER_KEY_ID);
    fireEvent.change(screen.getByLabelText("Secret access key"), { target: { value: CANARY_SECRET } });
    fireEvent.click(screen.getByRole("button", { name: "Next: test connection" }));

    await waitFor(() => expect(preflight).toHaveBeenCalled());
    // The load-bearing assertion: the candidate was checked and NOTHING was
    // declared. Before this issue there was no way to reach this state at
    // all, because the only preflight took an id out of config.yaml.
    expect(create).not.toHaveBeenCalled();

    const candidate = preflight.mock.calls[0]?.[0];
    expect(candidate?.bucket).toBe("nas-backups");
    expect(candidate?.credentials).toEqual({ credentialsId: "cred-1" });

    fireEvent.click(await screen.findByRole("button", { name: "Next: save" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save destination" }));
    await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
    expect(api.createStorageMedium).toHaveBeenCalledTimes(1);
  });

  it("submits the secret once and never sends it again", async () => {
    const importCredentials = vi.fn<(id: string, secret: string) => Promise<string>>(() =>
      Promise.resolve("cred-1")
    );
    const preflight = vi.fn<(spec: StorageMediumSpec) => Promise<MediumPreflight>>(() =>
      Promise.resolve(report(true))
    );
    const create = vi.fn(() => Promise.resolve(OFFSITE));
    await openTheWizard({
      importStorageCredentials: importCredentials,
      preflightStorageMediumCandidate: preflight,
      createStorageMedium: create
    });

    await describeDestinationThroughStepOne();
    fill("Access key id", PLACEHOLDER_KEY_ID);
    fireEvent.change(screen.getByLabelText("Secret access key"), { target: { value: CANARY_SECRET } });
    fireEvent.click(screen.getByRole("button", { name: "Next: test connection" }));
    await waitFor(() => expect(preflight).toHaveBeenCalled());
    fireEvent.click(await screen.findByRole("button", { name: "Next: save" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save destination" }));
    await waitFor(() => expect(create).toHaveBeenCalled());

    // The positive control: the canary really did reach the import, so
    // "it is nowhere else" is a statement about a code path rather than
    // about a form that carried nothing.
    expect(importCredentials).toHaveBeenCalledTimes(1);
    expect(importCredentials.mock.calls[0]?.[1]).toBe(CANARY_SECRET);

    const later = JSON.stringify([preflight.mock.calls, create.mock.calls]);
    expect(later).not.toContain(CANARY_SECRET);
    expect(later).not.toContain(PLACEHOLDER_KEY_ID);
  });

  it("keeps Save disabled while the destination cannot be proven, and renders every skipped step", async () => {
    const preflight = vi.fn<(spec: StorageMediumSpec) => Promise<MediumPreflight>>(() =>
      Promise.resolve(report(false, "reach"))
    );
    const create = vi.fn(() => Promise.resolve(OFFSITE));
    await openTheWizard({
      importStorageCredentials: vi.fn(() => Promise.resolve("cred-1")),
      preflightStorageMediumCandidate: preflight,
      createStorageMedium: create
    });

    await describeDestinationThroughStepOne();
    fill("Access key id", PLACEHOLDER_KEY_ID);
    fireEvent.change(screen.getByLabelText("Secret access key"), { target: { value: CANARY_SECRET } });
    fireEvent.click(screen.getByRole("button", { name: "Next: test connection" }));

    const next = await screen.findByRole("button", { name: "Next: save" });
    await waitFor(() => expect(next).toBeDisabled());
    expect(create).not.toHaveBeenCalled();

    // Every step, always, including the six that never ran. A surface that
    // dropped them would show an operator a shorter list on a failure than
    // on a success, which is the one moment the full list matters most.
    const group = screen.getByRole("group", { name: "Add a destination" });
    for (const step of ["credentials", "reach", "deliverable", "write", "read_back", "storage_class", "verification", "delete"]) {
      expect(within(group).getByText(step)).toBeTruthy();
    }
    expect(within(group).getAllByText("skipped").length).toBe(6);
    // The category is the machine-readable half and belongs beside the
    // outcome, because an operator scanning that column is deciding whose
    // problem it is.
    expect(within(group).getByText("failed(configuration)")).toBeTruthy();
  });

  it("shows the YAML that is about to be written, with a reference and no secret in it", async () => {
    await openTheWizard({
      importStorageCredentials: vi.fn(() => Promise.resolve("cred-1")),
      preflightStorageMediumCandidate: vi.fn(() => Promise.resolve(report(true))),
      createStorageMedium: vi.fn(() => Promise.resolve(OFFSITE))
    });

    await describeDestinationThroughStepOne();
    fill("Access key id", PLACEHOLDER_KEY_ID);
    fireEvent.change(screen.getByLabelText("Secret access key"), { target: { value: CANARY_SECRET } });
    fireEvent.click(screen.getByRole("button", { name: "Next: test connection" }));
    fireEvent.click(await screen.findByRole("button", { name: "Next: save" }));

    const group = await screen.findByRole("group", { name: "Add a destination" });
    const shown = group.textContent ?? "";
    expect(shown).toContain("id: offsite_s3");
    expect(shown).toContain("s3_credentials/cred-1");
    expect(shown).not.toContain(CANARY_SECRET);
    expect(shown).not.toContain(PLACEHOLDER_KEY_ID);
    expect(shown).not.toContain("access_key_id: ");
  });

  it("prints the equivalent backup-manager command, and it carries no secret", async () => {
    await openTheWizard({
      importStorageCredentials: vi.fn(() => Promise.resolve("cred-1")),
      preflightStorageMediumCandidate: vi.fn(() => Promise.resolve(report(true))),
      createStorageMedium: vi.fn(() => Promise.resolve(OFFSITE))
    });

    await describeDestinationThroughStepOne();
    expect(screen.getByText("backup-manager medium import-credentials --stdin")).toBeTruthy();

    fill("Access key id", PLACEHOLDER_KEY_ID);
    fireEvent.change(screen.getByLabelText("Secret access key"), { target: { value: CANARY_SECRET } });
    fireEvent.click(screen.getByRole("button", { name: "Next: test connection" }));
    fireEvent.click(await screen.findByRole("button", { name: "Next: save" }));

    const group = await screen.findByRole("group", { name: "Add a destination" });
    const shown = group.textContent ?? "";
    expect(shown).toContain("backup-manager medium add offsite_s3");
    expect(shown).toContain("--credentials-id cred-1");
    expect(shown).not.toContain("--secret-access-key");
    expect(shown).not.toContain(CANARY_SECRET);
  });
});

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
