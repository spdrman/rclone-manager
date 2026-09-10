import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ApiProvider } from "@shared/api/ApiContext";
import { BackupManagerError } from "@shared/api/contracts";
import type {
  BackendManifest,
  BackupManagerApi,
  MediumPreflight,
  StorageMedium,
  StorageMediumConfiguration
} from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { DestinationConfigureWizard } from "@shared/pages/DestinationConfigureWizard";

/**
 * The configure flow end to end (I2.2, issue #669): fill the fields the
 * manifest declares, test the connection, review.
 *
 * The manifest is synthetic here for the reason it is synthetic in
 * manifest-field-renderer.test.tsx, and the reason applies harder to
 * this file: driving the WHOLE flow with a backend nobody wrote code for
 * is what proves the flow is data-driven rather than only the form
 * inside it. `widget_locker` declares a path, a key namespace, an enum
 * and a credential, and nothing in this product knows any of them.
 *
 * Four properties are asserted here that no component test can reach,
 * because all four are about ORDER:
 *
 *   - the material goes to the credential import once and appears in no
 *     later request, with a canary proving it was in play;
 *   - a pass is invalidated by any edit to the fields (#636): a changed
 *     endpoint must not inherit an old pass;
 *   - a refusal writes nothing and says so, in the service's own words;
 *   - the default moves only when it was asked for, after the save, and
 *     never as a side effect of it.
 */

const CANARY_SECRET = "CANARY-669-configure-8b21f0ac-DO-NOT-SEND";
const PLACEHOLDER_KEY_ID = "EXAMPLE-NOT-A-REAL-KEY";

function syntheticManifest(): BackendManifest {
  return {
    id: "widget_locker",
    label: "Widget locker",
    summary: "A backend that does not exist, so nothing can know about it.",
    role: "object_store",
    rcloneBackend: "widgetlocker",
    fields: [
      { id: "path", label: "Spool directory", kind: "path", required: true },
      { id: "prefix", label: "Shelf namespace", kind: "key_prefix", required: false },
      {
        id: "upload_verification",
        label: "How a copy is proven",
        kind: "enum",
        required: false,
        unsetMeans: "readback",
        values: [
          { value: "readback", label: "Read the copy back" },
          { value: "attested", label: "Believe the locker" }
        ]
      },
      { id: "credentials", label: "Locker key", kind: "credential", required: true }
    ],
    probe: {
      steps: [
        { step: "credentials", run: true },
        { step: "reach", run: true },
        { step: "deliverable", run: true },
        { step: "write", run: true },
        { step: "read_back", run: true },
        { step: "storage_class", run: false, reason: "a widget locker has one shelf." },
        { step: "verification", run: true },
        { step: "delete", run: true }
      ]
    }
  };
}

const LOCKER: StorageMedium = {
  id: "locker_one",
  type: "widget_locker",
  bucket: "",
  storageClass: "STANDARD",
  uploadVerification: "readback",
  readsRequireRestore: false,
  isLocal: false,
  isDefault: false,
  connectionUnverified: true
};

function report(ok: boolean): MediumPreflight {
  const failing = "the destination refused the write; the credential and the container are right and the policy is not";
  return {
    medium: "locker_one",
    ok,
    checks: [
      { step: "credentials", outcome: "passed", detail: "the key was obtained" },
      { step: "reach", outcome: "passed", detail: "the locker answered" },
      { step: "deliverable", outcome: "passed", detail: "the shelf reads on demand" },
      ok
        ? { step: "write", outcome: "passed", detail: "an object was written" }
        : { step: "write", outcome: "failed", category: "permission", detail: failing },
      ok
        ? { step: "read_back", outcome: "passed", detail: "read back byte for byte" }
        : { step: "read_back", outcome: "skipped", detail: "the write did not happen" },
      { step: "storage_class", outcome: "skipped", detail: "one shelf" },
      ok
        ? { step: "verification", outcome: "passed", detail: "the content class was proven" }
        : { step: "verification", outcome: "skipped", detail: "nothing was written" },
      ok
        ? { step: "delete", outcome: "passed", detail: "the probe object was removed" }
        : { step: "delete", outcome: "skipped", detail: "there is no probe object" }
    ]
  };
}

/** A fresh instance: declared by #668, nothing configured yet, no
 *  credential. That is the state this flow is entered from. */
function unconfigured() {
  return vi.fn(() => Promise.resolve({ values: {}, credentialConfigured: false }));
}

async function openWizard(overrides: Partial<BackupManagerApi>) {
  const api = {
    ...createMockApi(),
    getStorageMediumConfiguration: unconfigured(),
    ...overrides
  } as BackupManagerApi;
  const onSaved = vi.fn();
  render(
    <ApiProvider api={api}>
      <DestinationConfigureWizard
        manifest={syntheticManifest()}
        destination={LOCKER}
        currentDefault="local"
        onClose={vi.fn()}
        onSaved={onSaved}
      />
    </ApiProvider>
  );
  // The form is not rendered until the current configuration has been
  // read: an empty form is not a neutral starting point when the save
  // sends the whole declared field set.
  await screen.findByTestId("manifest-field-row-path");
  return { api, onSaved };
}

function fill(fieldId: string, value: string) {
  const control = within(screen.getByTestId(`manifest-field-row-${fieldId}`)).getByTestId(
    "manifest-field-control"
  );
  fireEvent.change(control, { target: { value } });
}

function press(name: string | RegExp) {
  fireEvent.click(screen.getByRole("button", { name }));
}

/** Everything the manifest declares, filled in legally. */
async function describeTheLocker() {
  fill("path", "/mnt/lockers");
  fill("prefix", "widgets/2026");
  fill("credentials", PLACEHOLDER_KEY_ID);
  fireEvent.change(screen.getByLabelText("Locker key — secret"), {
    target: { value: CANARY_SECRET }
  });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Next: test connection" })).toBeEnabled()
  );
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("the fields step", () => {
  it("will not go on to the check while a declared field is wrong", async () => {
    await openWizard({});

    // Required and empty, straight from the manifest: nothing in this
    // component knows that a widget locker needs a spool directory.
    expect(screen.getByRole("button", { name: "Next: test connection" })).toBeDisabled();

    fill("path", "lockers");
    fill("credentials", PLACEHOLDER_KEY_ID);
    // Now filled in, and still refused: `path` is a KindPath field, and
    // a relative path is what backend.validateFieldValue refuses.
    expect(screen.getByRole("button", { name: "Next: test connection" })).toBeDisabled();
    expect(
      within(screen.getByTestId("manifest-field-row-path")).getByTestId("manifest-field-problem")
    ).toBeInTheDocument();

    fill("path", "/mnt/lockers");
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Next: test connection" })).toBeEnabled()
    );
  });
});

describe("the check", () => {
  it("sends the values keyed by manifest field id, and the secret nowhere but the import", async () => {
    const importCredentials = vi.fn(() => Promise.resolve("cred-669"));
    const preflight = vi.fn<(id: string, config: StorageMediumConfiguration) => Promise<MediumPreflight>>(
      () => Promise.resolve(report(true))
    );
    await openWizard({
      importStorageCredentials: importCredentials,
      preflightStorageMediumConfiguration: preflight
    });

    await describeTheLocker();
    press("Next: test connection");
    await waitFor(() => expect(preflight).toHaveBeenCalled());

    expect(importCredentials).toHaveBeenCalledWith(PLACEHOLDER_KEY_ID, CANARY_SECRET);
    expect(preflight).toHaveBeenCalledWith("locker_one", {
      // Keyed by field id, with the unset optional enum ABSENT rather
      // than sent as its unset_means value: a default resolved at read
      // time must not travel as though somebody chose it (#294).
      values: { path: "/mnt/lockers", prefix: "widgets/2026" },
      credentials: { credentialsId: "cred-669" }
    });

    // The canary was in play - the assertion above proves it reached the
    // import - and it is in no other request and nowhere on screen.
    const everySentPayload = JSON.stringify(preflight.mock.calls);
    expect(everySentPayload).not.toContain(CANARY_SECRET);
    expect(document.body.textContent).not.toContain(CANARY_SECRET);
  });

  it("renders the manifest's declared skip and its reason, not just a verdict", async () => {
    await openWizard({
      importStorageCredentials: vi.fn(() => Promise.resolve("cred-669")),
      preflightStorageMediumConfiguration: vi.fn(() => Promise.resolve(report(true)))
    });

    await describeTheLocker();
    press("Next: test connection");

    await waitFor(() =>
      expect(
        within(screen.getByTestId("probe-step-row-write")).getByTestId("probe-step-outcome")
      ).toHaveTextContent("passed")
    );
    const skipped = screen.getByTestId("probe-step-row-storage_class");
    expect(within(skipped).getByTestId("probe-step-outcome")).toHaveTextContent("skipped");
    expect(within(skipped).getByText("a widget locker has one shelf.")).toBeInTheDocument();
  });

  it("writes nothing on a refusal, and says so in the manager's own words", async () => {
    const configure = vi.fn();
    await openWizard({
      importStorageCredentials: vi.fn(() => Promise.resolve("cred-669")),
      preflightStorageMediumConfiguration: vi.fn(() => Promise.resolve(report(false))),
      configureStorageMedium: configure
    });

    await describeTheLocker();
    press("Next: test connection");

    await waitFor(() =>
      expect(
        within(screen.getByTestId("probe-step-row-write")).getByTestId("probe-step-outcome")
      ).toHaveTextContent("failed")
    );

    // The failing step's reason is the service's sentence, verbatim.
    expect(within(screen.getByTestId("probe-step-row-write")).getByTestId("probe-step-detail"))
      .toHaveTextContent(
        "the destination refused the write; the credential and the container are right and the policy is not"
      );
    // And nothing was written, which is the other half of #669's line:
    // a refusal saves nothing and leaves the destination unverified.
    expect(configure).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Next: review" })).toBeDisabled();
  });

  it("does not echo what the operator typed into the refusal", async () => {
    await openWizard({
      importStorageCredentials: vi.fn(() => Promise.resolve("cred-669")),
      preflightStorageMediumConfiguration: vi.fn(() => Promise.resolve(report(false)))
    });

    await describeTheLocker();
    press("Next: test connection");
    await waitFor(() => expect(screen.getByTestId("probe-step-row-write")).toBeInTheDocument());

    // Refusals by shape, never by content (mediumcreds.go). The banner
    // above the list explains what a refusal means and names no value
    // from the form; the only place a typed value may appear on this
    // step is inside a sentence the manager itself sent, and it sent
    // none containing one.
    const banner = screen.getByText(/Steps after the failing one were never tried/);
    expect(banner.textContent).not.toContain("/mnt/lockers");
    expect(banner.textContent).not.toContain("widgets/2026");
  });
});

describe("the verification mark", () => {
  it("does not survive an edit to a field", async () => {
    const preflight = vi.fn(() => Promise.resolve(report(true)));
    await openWizard({
      importStorageCredentials: vi.fn(() => Promise.resolve("cred-669")),
      preflightStorageMediumConfiguration: preflight
    });

    await describeTheLocker();
    press("Next: test connection");
    await waitFor(() => expect(screen.getByRole("button", { name: "Next: review" })).toBeEnabled());

    press("Back");
    expect(screen.queryByTestId("configure-mark-stale")).toBeNull();

    fill("prefix", "widgets/2027");

    // #636: a changed key namespace must not inherit the old pass, and
    // the screen says so where the change was made rather than leaving
    // an operator to find a disabled button two steps later.
    expect(screen.getByTestId("configure-mark-stale")).toHaveTextContent(
      "The check that passed was for different values"
    );
    expect(preflight).toHaveBeenCalledTimes(1);
  });

  it("comes back when the edit is undone, because it is about the values and not about editing", async () => {
    // The other half of the same mechanism, and the reason the mark is a
    // key rather than a flag: retyping the value that was proven leaves
    // the operator with exactly the configuration that passed, so
    // demanding a second identical check would be theatre.
    await openWizard({
      importStorageCredentials: vi.fn(() => Promise.resolve("cred-669")),
      preflightStorageMediumConfiguration: vi.fn(() => Promise.resolve(report(true)))
    });

    await describeTheLocker();
    press("Next: test connection");
    await waitFor(() => expect(screen.getByRole("button", { name: "Next: review" })).toBeEnabled());
    press("Back");

    fill("prefix", "widgets/2027");
    expect(screen.getByTestId("configure-mark-stale")).toBeInTheDocument();
    fill("prefix", "widgets/2026");
    expect(screen.queryByTestId("configure-mark-stale")).toBeNull();
  });

  it("does not survive a newly typed credential either", async () => {
    // A key that has been retyped is a different credential, and a pass
    // proven with the old one says nothing about it. This is the case a
    // reference-only mark cannot see: material is exchanged for an
    // opaque id and dropped, so nothing the id can compare has changed.
    await openWizard({
      importStorageCredentials: vi.fn(() => Promise.resolve("cred-669")),
      preflightStorageMediumConfiguration: vi.fn(() => Promise.resolve(report(true)))
    });

    await describeTheLocker();
    press("Next: test connection");
    await waitFor(() => expect(screen.getByRole("button", { name: "Next: review" })).toBeEnabled());
    press("Back");

    fireEvent.change(screen.getByLabelText("Locker key — secret"), {
      target: { value: "a-different-key-entirely" }
    });
    expect(screen.getByTestId("configure-mark-stale")).toBeInTheDocument();
  });
});

describe("the review step", () => {
  it("never renders the credential, and saves what was proven", async () => {
    const configure = vi.fn(() => Promise.resolve({ ...LOCKER, connectionUnverified: false }));
    const setDefault = vi.fn();
    const { onSaved } = await openWizard({
      importStorageCredentials: vi.fn(() => Promise.resolve("cred-669")),
      preflightStorageMediumConfiguration: vi.fn(() => Promise.resolve(report(true))),
      configureStorageMedium: configure,
      setDefaultStorageMedium: setDefault
    });

    await describeTheLocker();
    press("Next: test connection");
    await waitFor(() => expect(screen.getByRole("button", { name: "Next: review" })).toBeEnabled());
    press("Next: review");

    expect(screen.getByTestId("review-row-credentials")).toHaveTextContent(
      "stored, and never shown again"
    );
    expect(document.body.textContent).not.toContain(CANARY_SECRET);
    expect(within(screen.getByTestId("review-row-upload_verification")).getByTestId("review-value"))
      .toHaveTextContent("unset — read as readback");

    press("Save configuration");
    await waitFor(() => expect(configure).toHaveBeenCalled());

    expect(configure).toHaveBeenCalledWith("locker_one", {
      values: { path: "/mnt/lockers", prefix: "widgets/2026" },
      credentials: { credentialsId: "cred-669" }
    });
    // Not asked for, so not done. Never a side effect of saving (#669).
    expect(setDefault).not.toHaveBeenCalled();
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
  });

  it("moves the default only when it was asked for, and after the save", async () => {
    const order: string[] = [];
    const configure = vi.fn(() => {
      order.push("configure");
      return Promise.resolve({ ...LOCKER, connectionUnverified: false });
    });
    const setDefault = vi.fn(() => {
      order.push("default");
      return Promise.resolve({ ...LOCKER, isDefault: true, connectionUnverified: false });
    });
    await openWizard({
      importStorageCredentials: vi.fn(() => Promise.resolve("cred-669")),
      preflightStorageMediumConfiguration: vi.fn(() => Promise.resolve(report(true))),
      configureStorageMedium: configure,
      setDefaultStorageMedium: setDefault
    });

    await describeTheLocker();
    press("Next: test connection");
    await waitFor(() => expect(screen.getByRole("button", { name: "Next: review" })).toBeEnabled());
    press("Next: review");

    fireEvent.click(screen.getByRole("checkbox", { name: /default/i }));
    press("Save configuration");

    await waitFor(() => expect(setDefault).toHaveBeenCalledWith("locker_one"));
    expect(order).toEqual(["configure", "default"]);
  });

  it("prints the terminal line and names the fields it cannot carry", async () => {
    await openWizard({
      importStorageCredentials: vi.fn(() => Promise.resolve("cred-669")),
      preflightStorageMediumConfiguration: vi.fn(() => Promise.resolve(report(true)))
    });

    await describeTheLocker();
    press("Next: test connection");
    await waitFor(() => expect(screen.getByRole("button", { name: "Next: review" })).toBeEnabled());
    press("Next: review");

    // `medium edit` has --prefix and has no --path, measured from its own
    // flag switch. The gap is printed as a gap rather than papered over
    // with an invented flag, which is what makes EPIC G's parity rule
    // fail visibly instead of silently.
    expect(screen.getByText("rbm medium edit locker_one --prefix widgets/2026")).toBeInTheDocument();
    expect(screen.getByTestId("configure-command-gap")).toHaveTextContent("cannot carry path");
  });

  it("repeats the manager's refusal when the save itself is refused", async () => {
    const configure = vi.fn(() =>
      Promise.reject(
        new BackupManagerError({
          code: "MEDIUM_CONNECTION_NOT_PROVEN",
          message: "service: the destination refused the write before this configuration was written",
          correlationId: "cid_669save"
        })
      )
    );
    const { onSaved } = await openWizard({
      importStorageCredentials: vi.fn(() => Promise.resolve("cred-669")),
      preflightStorageMediumConfiguration: vi.fn(() => Promise.resolve(report(true))),
      configureStorageMedium: configure
    });

    await describeTheLocker();
    press("Next: test connection");
    await waitFor(() => expect(screen.getByRole("button", { name: "Next: review" })).toBeEnabled());
    press("Next: review");
    press("Save configuration");

    // The engine checks again in front of the write (#636) and can
    // refuse after this page's own check passed - a bucket policy can
    // change between the two. Its sentence is repeated, not reworded.
    await waitFor(() =>
      expect(screen.getByTestId("configure-failure")).toHaveTextContent(
        "service: the destination refused the write before this configuration was written"
      )
    );
    expect(onSaved).not.toHaveBeenCalled();
  });
});
