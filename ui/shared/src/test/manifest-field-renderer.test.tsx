import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BACKEND_FIELD_KINDS } from "@shared/api/contracts";
import type { BackendManifest, BackendManifestField } from "@shared/api/contracts";
import { ManifestFields } from "@shared/components/manifest/ManifestFields";
import manifestFieldsSource from "@shared/components/manifest/ManifestFields.tsx?raw";
import manifestFieldRulesSource from "@shared/components/manifest/manifestFieldRules.ts?raw";
import manifestProbeStepsSource from "@shared/components/manifest/ManifestProbeSteps.tsx?raw";
import manifestReviewSource from "@shared/components/manifest/ManifestReview.tsx?raw";
import { fieldProblem, manifestProblems, emptyValues } from "@shared/components/manifest/manifestFieldRules";
import type { ManifestFieldValues } from "@shared/components/manifest/manifestFieldRules";

/**
 * The one test in #669 that can tell whether the epic worked (issue #669,
 * EPIC I #664).
 *
 * The manifest it feeds the renderer is for a backend that does not
 * exist and never will: `widget_locker`, dialed through an rclone backend
 * this binary does not register, with one field of every declared kind.
 * Nothing in the product knows anything about it, so every control that
 * appears on screen got there because the manifest said so.
 *
 * That is the assertion, and it is why the manifest is synthetic rather
 * than `s3.json`. A component with a `switch` on backend type renders the
 * two bundled manifests perfectly and this one not at all, so testing
 * against a real backend proves the renderer works for the backends
 * somebody already wrote code for, which is exactly the thing in doubt.
 * If adding SFTP is ever to be a manifest rather than a fourth wizard,
 * this file is what says so.
 */

/** One field of every kind in BACKEND_FIELD_KINDS, in that order. */
const SYNTHETIC_FIELDS: BackendManifestField[] = [
  {
    id: "locker_name",
    label: "Locker name",
    help: "Whatever the operator of a widget locker calls one.",
    kind: "string",
    required: true,
    pattern: "^[a-z][a-z0-9-]*$"
  },
  {
    id: "spool_directory",
    label: "Spool directory",
    kind: "path",
    required: true
  },
  {
    id: "control_url",
    label: "Control URL",
    kind: "url",
    required: false
  },
  {
    id: "shelf_class",
    label: "Shelf class",
    kind: "enum",
    required: false,
    unsetMeans: "OPEN_SHELF",
    values: [
      { value: "OPEN_SHELF", label: "Open shelf" },
      { value: "SEALED_CRATE", label: "Sealed crate" }
    ]
  },
  {
    id: "warm_the_locker",
    label: "Warm the locker first",
    kind: "bool",
    required: false
  },
  {
    id: "locker_key",
    label: "Locker key",
    help: "Stored once, in a file only this service can read.",
    kind: "credential",
    required: true
  },
  {
    id: "shelf_prefix",
    label: "Shelf namespace",
    kind: "key_prefix",
    required: false
  }
];

function syntheticManifest(): BackendManifest {
  return {
    id: "widget_locker",
    label: "Widget locker",
    summary: "A backend that does not exist, so nothing can know about it.",
    role: "object_store",
    fields: SYNTHETIC_FIELDS,
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

function field(id: string): BackendManifestField {
  const found = SYNTHETIC_FIELDS.find((f) => f.id === id);
  if (!found) throw new Error("no synthetic field " + id);
  return found;
}

function renderFields(overrides?: {
  values?: ManifestFieldValues;
  problems?: Record<string, string>;
}) {
  const manifest = syntheticManifest();
  const onChange = vi.fn();
  const onCredentialChange = vi.fn();
  render(
    <ManifestFields
      manifest={manifest}
      values={overrides?.values ?? emptyValues(manifest)}
      problems={overrides?.problems ?? {}}
      credential={{ accessKeyId: "", secretAccessKey: "" }}
      onChange={onChange}
      onCredentialChange={onCredentialChange}
    />
  );
  return { manifest, onChange, onCredentialChange };
}

/** The block for one declared field. One testid answers both questions
 *  this file asks of it - which fields rendered, in what order, and what
 *  is inside a named one - which is why it carries the id rather than
 *  there being a second wrapper to hang a list query on. */
function row(fieldId: string): HTMLElement {
  return screen.getByTestId(`manifest-field-row-${fieldId}`);
}

afterEach(cleanup);

describe("a manifest for a backend that does not exist", () => {
  it("renders one control per declared field, in the manifest's own order", () => {
    renderFields();

    const labels = screen
      .getAllByTestId(/^manifest-field-row-/)
      .map((r) => within(r).getByTestId("manifest-field-label").textContent);

    // Every label the manifest declared, none it did not, and in the
    // order it declared them: a renderer that sorted, grouped or dropped
    // one would be deciding something the manifest already decided.
    expect(labels).toEqual([
      "Locker name",
      "Spool directory",
      "Control URL",
      "Shelf class",
      "Warm the locker first",
      "Locker key",
      "Shelf namespace"
    ]);
  });

  it("renders a control for every kind in BACKEND_FIELD_KINDS", () => {
    renderFields();

    // Read off the exported vocabulary rather than a list written here,
    // so the day an eighth kind is declared this fails instead of that
    // kind rendering as an empty row nobody notices. That is the failure
    // mode worth catching: a field an operator cannot see is a field
    // they cannot fill in, and a required one makes the destination
    // unconfigurable with no error anywhere.
    for (const kind of BACKEND_FIELD_KINDS) {
      const declared = SYNTHETIC_FIELDS.find((f) => f.kind === kind);
      expect(declared, `no synthetic field declares kind ${kind}`).toBeDefined();
      expect(
        within(row(declared!.id)).getByTestId("manifest-field-control"),
        `kind ${kind} rendered no control`
      ).toBeInTheDocument();
    }
  });

  it("offers an enum's declared members and nothing else", () => {
    renderFields();

    const options = within(row("shelf_class"))
      .getAllByRole("option")
      .map((o) => ({ value: (o as HTMLOptionElement).value, label: o.textContent }));

    // The empty option first, then exactly the two the manifest
    // declared. A hardcoded storage-class list is the thing this
    // assertion refuses: those seven values live in s3.json and a
    // renderer holding a copy of them would offer them here, to a
    // backend that has no storage classes at all.
    expect(options).toEqual([
      { value: "", label: "Leave unset (OPEN_SHELF)" },
      { value: "OPEN_SHELF", label: "Open shelf" },
      { value: "SEALED_CRATE", label: "Sealed crate" }
    ]);
  });

  it("leaves an optional enum unset rather than pre-selecting what unset means", () => {
    renderFields();

    const select = within(row("shelf_class")).getByTestId(
      "manifest-field-control"
    ) as HTMLSelectElement;

    // #294's argument, on screen. `unset_means` is what an ACCESSOR
    // resolves an empty field to, not a value to write into the record:
    // pre-selecting it turns a default the product can change into a
    // value frozen into the operator's file by the next save. So the
    // control is empty and the words say what empty will mean.
    expect(select.value).toBe("");
  });

  it("marks required fields as required and optional ones as optional", () => {
    renderFields();

    const required = within(row("locker_name")).getByTestId("manifest-field-control");
    const optional = within(row("control_url")).getByTestId("manifest-field-control");

    expect(required).toBeRequired();
    expect(optional).not.toBeRequired();
  });

  it("renders a declared help sentence and nothing where none is declared", () => {
    renderFields();

    expect(
      within(row("locker_name")).getByText("Whatever the operator of a widget locker calls one.")
    ).toBeInTheDocument();
    expect(within(row("spool_directory")).queryByTestId("manifest-field-help")).toBeNull();
  });

  it("masks a credential field and never routes it through the value bag", async () => {
    const { onChange, onCredentialChange } = renderFields();
    const user = userEvent.setup();

    // Queried by its accessible name rather than by a testid, because
    // the secret box is PasswordInput (#344) and reusing that is the
    // point: its masking, its reveal toggle, and its spellcheck and
    // autocapitalize refusals are the protections a hand-rolled
    // type=password here would silently drop.
    const secret = within(row("locker_key")).getByLabelText("Locker key — secret");
    expect(secret).toHaveAttribute("type", "password");

    await user.type(secret, "hunter2");

    // The separation is structural and not a convention anyone has to
    // remember: credential material reaches its own callback, and the
    // values object the review step later renders has no channel it
    // could arrive on. C1-C5 in #665 keep it out of config.yaml, the
    // terminal and every API response; this keeps it out of the one
    // place the browser could still have leaked it.
    expect(onCredentialChange).toHaveBeenCalled();
    expect(onChange).not.toHaveBeenCalled();
  });

  it("has no entry in the value bag for a credential field at all", () => {
    const manifest = syntheticManifest();
    expect(Object.keys(emptyValues(manifest))).toEqual([
      "locker_name",
      "spool_directory",
      "control_url",
      "shelf_class",
      "warm_the_locker",
      "shelf_prefix"
    ]);
  });

  it("shows a problem the manifest's own rules produced, against the field it belongs to", () => {
    renderFields({ problems: { spool_directory: "an absolute path, starting with /" } });

    expect(
      within(row("spool_directory")).getByText("an absolute path, starting with /")
    ).toBeInTheDocument();
    expect(within(row("locker_name")).queryByTestId("manifest-field-problem")).toBeNull();
  });
});

/**
 * Each kind validates by what its own declaration says, and by nothing
 * else. The rules are the browser's half of backend.ValidateInstance:
 * they exist so an operator is told which box is wrong before a request
 * is sent, and they are deliberately the same rules, not stricter ones.
 */
describe("validating a value against its declared kind", () => {
  it("refuses an empty required field and accepts an empty optional one", () => {
    expect(fieldProblem(field("locker_name"), "")).not.toBeNull();
    expect(fieldProblem(field("control_url"), "")).toBeNull();
  });

  it("holds a string to the pattern the manifest declared", () => {
    expect(fieldProblem(field("locker_name"), "row-14")).toBeNull();
    expect(fieldProblem(field("locker_name"), "Row 14")).not.toBeNull();
  });

  it("accepts no pattern as no constraint", () => {
    const unpatterned: BackendManifestField = { id: "x", label: "X", kind: "string", required: false };
    expect(fieldProblem(unpatterned, "anything at all, / and . included")).toBeNull();
  });

  it("requires a path to be absolute", () => {
    expect(fieldProblem(field("spool_directory"), "/mnt/lockers")).toBeNull();
    expect(fieldProblem(field("spool_directory"), "lockers")).not.toBeNull();
    expect(fieldProblem(field("spool_directory"), "./lockers")).not.toBeNull();
  });

  it("requires a URL to have a scheme and a host", () => {
    expect(fieldProblem(field("control_url"), "https://lockers.internal:9000")).toBeNull();
    expect(fieldProblem(field("control_url"), "lockers.internal:9000")).not.toBeNull();
    expect(fieldProblem(field("control_url"), "https://")).not.toBeNull();
  });

  it("refuses an enum value the manifest did not declare", () => {
    expect(fieldProblem(field("shelf_class"), "SEALED_CRATE")).toBeNull();
    expect(fieldProblem(field("shelf_class"), "GLACIER")).not.toBeNull();
  });

  it("refuses the four key-prefix shapes a key namespace may not have", () => {
    const prefix = field("shelf_prefix");
    expect(fieldProblem(prefix, "widgets/2026")).toBeNull();
    expect(fieldProblem(prefix, "/widgets")).not.toBeNull();
    expect(fieldProblem(prefix, "widgets/")).not.toBeNull();
    expect(fieldProblem(prefix, "widgets//2026")).not.toBeNull();
    // Not tidiness: restoring an artifact writes it to a local path
    // derived from the key, so a ".." segment in a key namespace is a
    // path escape on this host (config.validateMediumPrefix's own doc,
    // quoted by KindKeyPrefix in manifest.go).
    expect(fieldProblem(prefix, "widgets/../etc")).not.toBeNull();
    expect(fieldProblem(prefix, "widgets/./etc")).not.toBeNull();
  });

  it("treats a bool as always answered, because a checkbox cannot be empty", () => {
    expect(fieldProblem(field("warm_the_locker"), false)).toBeNull();
    expect(fieldProblem(field("warm_the_locker"), true)).toBeNull();
  });

  it("collects one problem per offending field and leaves the rest out", () => {
    const manifest = syntheticManifest();
    const problems = manifestProblems(manifest, {
      ...emptyValues(manifest),
      locker_name: "Row 14",
      spool_directory: "/mnt/lockers",
      shelf_prefix: "/widgets"
    });

    expect(Object.keys(problems).sort()).toEqual(["locker_name", "shelf_prefix"]);
  });

  it("never reports a problem for a credential field, because it holds no value to check", () => {
    const manifest = syntheticManifest();
    const problems = manifestProblems(manifest, emptyValues(manifest));
    expect(problems).not.toHaveProperty("locker_key");
  });
});

/**
 * The structural half of the same claim.
 *
 * The synthetic manifest above proves the renderer can draw a backend
 * nobody wrote code for. It cannot prove the renderer draws the two
 * BUNDLED backends by the same route: a `switch` with a case for `s3`, a
 * case for `local_volume` and a generic default passes every assertion
 * above and has failed the epic anyway. So this reads the source and
 * refuses a backend id in it.
 *
 * A source scan is a weak instrument and this repository uses one only
 * where behaviour cannot answer, which is here. It comes with its
 * positive control, as every absence assertion in this project does
 * (plan-665-registry.md 8, after TestTheETagScanCanActuallyFail): the
 * control proves the scan can still fail, so the day the pattern stops
 * matching anything the test says so instead of passing quietly.
 */
// Through Vite's `?raw` rather than node:fs, for the reason
// dark-mode-contrast.test.ts gives: `?raw` is typed by vite/client,
// which this workspace already has, and node's own types are not a
// dependency of the frontend.
const RENDERER_SOURCES: Record<string, string> = {
  "ManifestFields.tsx": manifestFieldsSource,
  "manifestFieldRules.ts": manifestFieldRulesSource,
  "ManifestProbeSteps.tsx": manifestProbeStepsSource,
  "ManifestReview.tsx": manifestReviewSource
};

/** A backend id, or an rclone backend name, used as a value to compare
 *  against. Deliberately not a search for the WORD: `s3.json` is named
 *  in prose in these files and should be, and `local_volume` appears in
 *  BackendRole's own union. What is refused is a comparison. */
function backendComparisons(source: string): string[] {
  const pattern =
    /(?:===?|!==?|case)\s*["'`](?:s3|local_volume|local|sftp|widget_locker)["'`]|["'`](?:s3|local_volume|local|sftp)["'`]\s*(?:===?|!==?)/g;
  return source.match(pattern) ?? [];
}

describe("the renderer names no backend", () => {
  it("compares nothing against a backend id", () => {
    for (const [name, source] of Object.entries(RENDERER_SOURCES)) {
      expect(backendComparisons(source), `${name} compares against a backend id`).toEqual([]);
    }
  });

  it("can actually fail", () => {
    // The control. Without it, a pattern that stopped matching would
    // make the assertion above pass for every source file forever,
    // including one that had grown the switch.
    expect(backendComparisons('if (manifest.id === "s3") renderBucketField();')).toHaveLength(1);
    expect(backendComparisons('switch (m.id) { case "local_volume": return <PathField />; }')).toHaveLength(1);
  });
});
