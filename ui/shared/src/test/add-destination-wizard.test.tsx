import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { AddDestinationWizard } from "@shared/pages/AddDestinationWizard";
import { StorageDestinationsCard } from "@shared/pages/StorageDestinationsCard";
import { ApiProvider } from "@shared/api/ApiContext";
import type {
  BackendCatalog,
  BackupManagerApi,
  StorageMedium
} from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { resetGraphForTests } from "@shared/state/graph";

/**
 * I2.1 (#668): adding a destination is choosing a REGISTERED BACKEND and
 * then naming an INSTANCE of it.
 *
 * Four properties are asserted here that nothing on the Go side can
 * assert, because they are about which screen asks which question and
 * about what a field refuses before a request is ever sent.
 *
 *   - The backend list is the registry's, not this component's. A
 *     hardcoded array of backend names, or a switch on backend type,
 *     would be EPIC I (#664) failing in the one surface the whole epic
 *     exists to change, so the fixture serves a backend that no bundled
 *     manifest declares and the screen has to render it anyway.
 *   - Choosing a backend and naming an instance are SEPARATE steps.
 *     Collapsed into one screen, the wizard has quietly re-asserted that
 *     a destination is a backend type, which is the assumption #664
 *     exists to remove. Several instances of one backend is the normal
 *     case; the first release shipping two backends makes the separation
 *     look like ceremony, and it is not.
 *   - A name that cannot be used is refused IN THE FIELD. A wizard that
 *     lets an operator type `local` and then fails at the API is a worse
 *     experience than one that says so where they typed it.
 *   - Every refusal is by SHAPE and never echoes what was typed, which is
 *     `mediumcreds.go`'s doctrine (#665's C1-C5) arriving on the browser
 *     side. An operator who pastes a secret into the name field by
 *     mistake must not see it rendered back at them.
 *
 * And the list itself: two instances of one backend, side by side,
 * distinguishable. That is the epic made visible, and a list that grouped
 * or labelled by backend type so two local volumes read as one thing
 * would be the defect rather than the feature.
 */

/** A name-field mistake that must never be rendered back. Not a real
 *  secret, and the point is that this surface cannot tell: it refuses by
 *  shape, so it never has to decide whether what it was handed was
 *  sensitive. */
const PASTED_BY_MISTAKE = "CANARY-668-pasted-into-the-name-field-9f2ab41c";

/**
 * The catalogue the fixture serves.
 *
 * `object_lake` is deliberately NOT one of the manifests this build
 * bundles. It is here so "the list came from the registry" is a claim a
 * test can fail: a component holding its own array of backend names
 * cannot render this row, and a component reading the response can render
 * nothing else.
 *
 * `sftp` is registered and reports `configurable: false`, which is
 * #731's shape: a backend this build describes in full and cannot yet
 * save one of. It is here so the picker's third state is a claim a test
 * can fail too — shown, searchable, and unchoosable.
 */
const CATALOG: BackendCatalog = {
  registered: [
    {
      id: "local_volume",
      label: "Local volume",
      summary: "A directory on a filesystem this host can see.",
      role: "local_volume",
      configurable: true,
      fields: [
        { id: "path", label: "Directory", kind: "path", required: true },
        { id: "prefix", label: "Key prefix", kind: "key_prefix", required: false }
      ],
      probe: { steps: [] }
    },
    {
      id: "s3",
      label: "S3 bucket",
      summary: "A remote object store addressed by a bucket and a key.",
      role: "object_store",
      configurable: true,
      fields: [
        { id: "bucket", label: "Bucket", kind: "string", required: true },
        { id: "credentials", label: "Credentials", kind: "credential", required: true }
      ],
      probe: { steps: [] }
    },
    {
      id: "object_lake",
      label: "Object lake",
      summary: "A backend this build does not bundle, served by the fixture.",
      role: "object_store",
      configurable: true,
      fields: [{ id: "bucket", label: "Bucket", kind: "string", required: true }],
      probe: { steps: [] }
    },
    {
      id: "sftp",
      label: "SSH server (SFTP)",
      summary: "A directory on another machine, reached over SSH.",
      role: "remote_filesystem",
      configurable: false,
      fields: [
        { id: "host", label: "Host", kind: "string", required: true },
        { id: "credentials", label: "SSH private key", kind: "credential", required: true }
      ],
      probe: { steps: [] }
    }
  ],
  // Disjoint from the registered list above, the way the server
  // computes it: `unregistered` is a subtraction, and a transport
  // cannot be both. sftp came off this list when #731 registered it.
  unregistered: [{ transport: "webdav" }],
  instanceIdPattern: "^[a-z][a-z0-9_]*$",
  reservedInstanceId: "local"
};

/** The drive backups land on (#622): present in every real listing, and
 *  the reason `local` is refused as a name. */
const LOCAL: StorageMedium = {
  id: "local",
  type: "local",
  bucket: "",
  storageClass: "",
  uploadVerification: "readback",
  readsRequireRestore: false,
  path: "/srv/backups",
  isLocal: true,
  isDefault: true,
  connectionUnverified: false
};

function instance(id: string, over: Partial<StorageMedium>): StorageMedium {
  return {
    id,
    type: "s3",
    bucket: "",
    storageClass: "STANDARD",
    uploadVerification: "readback",
    readsRequireRestore: false,
    isLocal: false,
    isDefault: false,
    connectionUnverified: false,
    ...over
  };
}

/** Renders the wizard and returns the api it was given plus the spy that
 *  receives the handoff, because under P2 the interesting thing about the
 *  confirm step is WHAT it hands on, not what it writes. */
function renderWizard(
  existing: StorageMedium[],
  overrides: Partial<BackupManagerApi> = {}
) {
  const api = {
    ...createMockApi(),
    listBackends: vi.fn(() => Promise.resolve(structuredClone(CATALOG))),
    // Spies, not the fixture's own implementations: the load-bearing
    // assertions in this file are that these were NEVER called, and a
    // plain function cannot answer that question.
    createStorageMedium: vi.fn(),
    updateStorageMedium: vi.fn(),
    preflightStorageMediumCandidate: vi.fn(),
    ...overrides
  } as unknown as BackupManagerApi;
  const confirmed = vi.fn<(backendId: string, instanceId: string) => void>();
  render(
    <ApiProvider api={api}>
      <AddDestinationWizard existing={existing} onClose={() => {}} onConfirmed={confirmed} />
    </ApiProvider>
  );
  return { api, confirmed };
}

function group() {
  return screen.getByRole("group", { name: "Add a destination" });
}

async function chooseBackend(label: string) {
  const radio = await screen.findByRole("radio", { name: new RegExp("^" + label) });
  fireEvent.click(radio);
  fireEvent.click(screen.getByRole("button", { name: "Next: name this instance" }));
  await screen.findByLabelText("Instance name");
}

function typeName(value: string) {
  fireEvent.change(screen.getByLabelText("Instance name"), { target: { value } });
}

describe("choosing a backend (#668 step 1)", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("renders the backends the registry serves, including one this build does not bundle", async () => {
    renderWizard([LOCAL]);

    // The load-bearing one. A component with its own array of backend
    // names cannot produce this row at all: no bundled manifest declares
    // `object_lake`, so it exists only in the response.
    expect(await screen.findByRole("radio", { name: /^Object lake/ })).toBeTruthy();
    expect(screen.getByRole("radio", { name: /^Local volume/ })).toBeTruthy();
    expect(screen.getByRole("radio", { name: /^S3 bucket/ })).toBeTruthy();
    // The manifest's own summary, rendered as served.
    expect(within(group()).getByText("A directory on a filesystem this host can see.")).toBeTruthy();
  });

  it("shows the wider rclone catalogue as understood and not registered, and refuses to select it", async () => {
    renderWizard([LOCAL]);
    await screen.findByRole("radio", { name: /^Local volume/ });

    // Somebody who came here for a shape this build understands learns
    // nothing from a menu that never mentions it. The row that says the
    // shape is understood and not registered is a real answer, and it
    // can never be submitted.
    const row = within(group()).getByRole("listitem", { name: "webdav" });
    expect(within(row).getByText("not registered")).toBeTruthy();
    expect(within(row).queryByRole("radio")).toBeNull();
  });

  it("shows a registered backend that cannot be configured yet, disabled and with the reason", async () => {
    renderWizard([LOCAL]);

    // #731's row. Registered, so it is in the list with its own label
    // and summary rather than as a transport name; not configurable, so
    // the radio is disabled and there is nothing to choose.
    const radio = (await screen.findByRole("radio", {
      name: /^SSH server \(SFTP\)/
    })) as HTMLInputElement;
    expect(radio.disabled).toBe(true);
    expect(within(group()).getByText(/Not yet configurable — tracked in #235/)).toBeTruthy();
  });

  it("cannot be walked past the picker on a backend that is not configurable", async () => {
    const { confirmed } = renderWizard([LOCAL]);
    const radio = await screen.findByRole("radio", { name: /^SSH server \(SFTP\)/ });

    // A disabled control is not a request the wizard may honour, however
    // it arrives: clicking it chooses nothing, so Next stays refused and
    // the steps that collect and hand on a destination are unreachable.
    fireEvent.click(radio);
    const next = screen.getByRole("button", { name: "Next: name this instance" }) as HTMLButtonElement;
    expect(next.disabled).toBe(true);

    fireEvent.click(next);
    expect(screen.queryByLabelText("Instance name")).toBeNull();
    expect(confirmed).not.toHaveBeenCalled();
  });

  it("keeps a backend that cannot be configured yet in the search results", async () => {
    renderWizard([LOCAL]);
    await screen.findByRole("radio", { name: /^Local volume/ });

    // The #731 milestone in one assertion: somebody searching for sftp
    // finds it. A picker that hid what it cannot offer would answer that
    // search with nothing at all.
    fireEvent.change(screen.getByLabelText("Search backends"), { target: { value: "sftp" } });
    expect(screen.getByRole("radio", { name: /^SSH server \(SFTP\)/ })).toBeTruthy();
    expect(screen.queryByRole("radio", { name: /^S3 bucket/ })).toBeNull();
  });

  it("filters the list without inventing entries", async () => {
    renderWizard([LOCAL]);
    await screen.findByRole("radio", { name: /^Local volume/ });

    fireEvent.change(screen.getByLabelText("Search backends"), { target: { value: "lake" } });
    expect(screen.getByRole("radio", { name: /^Object lake/ })).toBeTruthy();
    expect(screen.queryByRole("radio", { name: /^S3 bucket/ })).toBeNull();
  });
});

describe("naming the instance (#668 step 2)", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("is a step of its own: step 1 asks for no name and step 2 asks for no backend", async () => {
    renderWizard([LOCAL]);
    await screen.findByRole("radio", { name: /^S3 bucket/ });

    // Step 1 collects a backend and nothing else. If a name field lived
    // here, the wizard would be saying a destination IS a backend type.
    expect(screen.queryByLabelText("Instance name")).toBeNull();

    await chooseBackend("S3 bucket");

    // And step 2 collects a name and nothing else: the backend has been
    // decided and is reported, not re-asked.
    expect(screen.queryByRole("radio", { name: /^S3 bucket/ })).toBeNull();
    expect(screen.getByLabelText("Instance name")).toBeTruthy();
  });

  it("lists the instances that already exist on the chosen backend, so uniqueness is visible where it applies", async () => {
    renderWizard([
      LOCAL,
      instance("cold_archive", { type: "s3", bucket: "cold" }),
      instance("nas_mirror", { type: "local_volume", path: "/mnt/nas" })
    ]);
    await screen.findByRole("radio", { name: /^S3 bucket/ });
    await chooseBackend("S3 bucket");

    const already = within(group()).getByRole("list", { name: "Instances of S3 bucket that already exist" });
    expect(within(already).getByText("cold_archive")).toBeTruthy();
    expect(within(already).queryByText("nas_mirror")).toBeNull();
  });

  it("refuses a name already taken, in the form, before anything is sent", async () => {
    const { api } = renderWizard([LOCAL, instance("cold_archive", { bucket: "cold" })]);
    await screen.findByRole("radio", { name: /^S3 bucket/ });
    await chooseBackend("S3 bucket");

    typeName("cold_archive");

    expect(await screen.findByText(/already exists/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Next: confirm" })).toBeDisabled();
    // In the form, which is the point: the operator is told here rather
    // than by an API refusal arriving on a screen they have already left.
    expect(api.preflightStorageMediumCandidate).not.toHaveBeenCalled();
    expect(api.createStorageMedium).not.toHaveBeenCalled();
  });

  it("refuses a name taken on a DIFFERENT backend too, because instance names are one namespace", async () => {
    renderWizard([LOCAL, instance("nas_mirror", { type: "local_volume", path: "/mnt/nas" })]);
    await screen.findByRole("radio", { name: /^S3 bucket/ });
    await chooseBackend("S3 bucket");

    typeName("nas_mirror");

    expect(await screen.findByText(/already exists/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Next: confirm" })).toBeDisabled();
  });

  it("refuses `local` with the reason, rather than letting the API refuse it later", async () => {
    renderWizard([LOCAL]);
    await screen.findByRole("radio", { name: /^Local volume/ });
    await chooseBackend("Local volume");

    typeName("local");

    // The reserved word, and why. `local` is instance zero of the
    // local_volume backend, written by the first-boot seed (#670), and it
    // is refused on every operator-facing path.
    const said = within(group()).getByRole("alert").textContent ?? "";
    expect(said).toContain("reserved");
    expect(screen.getByRole("button", { name: "Next: confirm" })).toBeDisabled();
  });

  it("refuses a malformed name by shape, and never echoes what was typed", async () => {
    renderWizard([LOCAL]);
    await screen.findByRole("radio", { name: /^S3 bucket/ });
    await chooseBackend("S3 bucket");

    typeName(PASTED_BY_MISTAKE);

    const said = within(group()).getByRole("alert").textContent ?? "";
    expect(said).toContain("lower_snake_case");
    // The doctrine: refusals by SHAPE, never by content. An operator who
    // pastes the wrong clipboard entry into a name field must not see it
    // rendered back at them, and this surface cannot tell what it was
    // handed, which is exactly why it must never repeat it.
    expect(said).not.toContain(PASTED_BY_MISTAKE);
    expect(document.body.textContent).not.toContain(PASTED_BY_MISTAKE);
    expect(screen.getByRole("button", { name: "Next: confirm" })).toBeDisabled();
  });

  it("accepts a second instance of a backend that already has one", async () => {
    renderWizard([LOCAL, instance("nas_mirror", { type: "local_volume", path: "/mnt/nas" })]);
    await screen.findByRole("radio", { name: /^Local volume/ });
    await chooseBackend("Local volume");

    typeName("usb_dock");

    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Next: confirm" })).not.toBeDisabled()
    );
  });
});

describe("confirming (#668 step 3)", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  async function reachConfirm() {
    const rendered = renderWizard([LOCAL]);
    await screen.findByRole("radio", { name: /^Local volume/ });
    await chooseBackend("Local volume");
    typeName("usb_dock");
    fireEvent.click(await screen.findByRole("button", { name: "Next: confirm" }));
    await screen.findByRole("button", { name: "Next: configure it" });
    return rendered;
  }

  it("shows the backend and the instance name that will be created", async () => {
    await reachConfirm();

    const shown = group().textContent ?? "";
    expect(shown).toContain("Local volume");
    expect(shown).toContain("usb_dock");
    // And what it does NOT yet have. "not asked for yet" is a different
    // sentence from an empty value, and the difference is the whole of
    // what this step is confirming.
    expect(shown).toContain("not asked for yet");
  });

  it("names the fields configuring it will ask for, read off the manifest", async () => {
    await reachConfirm();

    // Read from the served manifest, not described in prose: a confirm
    // step for a destination whose values have not been collected can
    // only keep this promise by reading the same document the next step
    // renders.
    const shown = group().textContent ?? "";
    expect(shown).toContain("Directory");
    expect(shown).toContain("Key prefix (optional)");
  });

  it("says nothing has been written yet, and that a test must pass before a tier can select it", async () => {
    await reachConfirm();

    const shown = group().textContent ?? "";
    // #668's issue says this step CREATES an unusable destination. It
    // does not, because an instance carrying no values is refused by
    // backend.Registry.ValidateInstance (validate.go:228-233) — a
    // required field that is absent is "<field> is required", and both
    // bundled backends have required fields. So the destination is
    // written once, by the configure step, after a check passes; this
    // screen says that instead of claiming a create it cannot do.
    expect(shown).toContain("Nothing has been written yet");
    expect(shown).toContain("no retention tier can select it until it has been proved");
  });

  it("writes nothing itself, and hands the backend and the name to the configure step", async () => {
    const { api, confirmed } = await reachConfirm();

    fireEvent.click(screen.getByRole("button", { name: "Next: configure it" }));

    expect(confirmed).toHaveBeenCalledTimes(1);
    expect(confirmed).toHaveBeenCalledWith("local_volume", "usb_dock");
    // The load-bearing negative: nothing is declared by these three
    // steps. #594's property is that nothing is written until the
    // destination has been proven, and a create here would be a write in
    // front of any proof.
    expect(api.createStorageMedium).not.toHaveBeenCalled();
    expect(api.updateStorageMedium).not.toHaveBeenCalled();
  });

  it("names no equivalent command, because these steps run none", async () => {
    await reachConfirm();

    const shown = group().textContent ?? "";
    // This step used to print `rbm medium add usb_dock --backend
    // local_volume`, and `medium` has no --backend flag at all
    // (core/cmd/backup-manager/medium.go:183) — the line failed on
    // execution with "flag provided but not defined". `--type` would not
    // have fixed it either: there is no `medium add` equivalent to a step
    // that writes nothing, since an instance carrying no values is
    // refused by ValidateInstance. So the gap is NAMED, the way
    // core/cliecho names a route with no verb.
    //
    // Asserted as the absence of a runnable line AND the presence of the
    // reason, because either half alone is satisfiable by a defect:
    // printing nothing at all passes the first, and printing a broken
    // line beside the prose passes the second.
    expect(shown).not.toContain("--backend");
    expect(shown).not.toContain("rbm medium add usb_dock");
    expect(shown).toContain("These steps run none");
    expect(shown).toContain("printed by the configure step");
  });
});

describe("the destinations list (#668, the Main artboard)", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("renders two instances of one backend distinguishably", async () => {
    const api = {
      ...createMockApi(),
      listStorageMediums: vi.fn(() =>
        Promise.resolve([
          LOCAL,
          instance("nas_mirror", { type: "local_volume", path: "/mnt/nas" }),
          instance("usb_dock", { type: "local_volume", path: "/mnt/usb" }),
          instance("cold_archive", { type: "s3", bucket: "cold", region: "us-east-1" }),
          instance("warm_tier", { type: "s3", bucket: "warm", region: "eu-west-1" })
        ])
      )
    } as BackupManagerApi;
    render(
      <ApiProvider api={api}>
        <StorageDestinationsCard readOnly={false} />
      </ApiProvider>
    );

    // Four declared instances, two of each backend, each its own row.
    const mirror = await screen.findByRole("group", { name: "Storage destination nas_mirror" });
    const dock = screen.getByRole("group", { name: "Storage destination usb_dock" });
    screen.getByRole("group", { name: "Storage destination cold_archive" });
    screen.getByRole("group", { name: "Storage destination warm_tier" });

    // And the two on ONE backend are told apart by their configuration,
    // not by their type. A list that described both as "local_volume" and
    // stopped would make two destinations read as one thing, which is the
    // assumption #664 exists to remove.
    expect(within(mirror).getByText(/\/mnt\/nas/)).toBeTruthy();
    expect(within(dock).getByText(/\/mnt\/usb/)).toBeTruthy();
  });
});
