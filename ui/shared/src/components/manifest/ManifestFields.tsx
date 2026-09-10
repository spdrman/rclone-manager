/**
 * The form a backend manifest describes, drawn (issue #669, EPIC I #664;
 * artboards `ConfigLocal` and `ConfigS3`).
 *
 * # There is no field set in this file
 *
 * That is the whole design and it is worth stating plainly, because the
 * file looks like it is missing something. A local volume renders a
 * directory and a subdirectory; S3 renders six fields and a credential
 * pair; neither list appears below. What appears below is one renderer
 * per declared KIND, and the manifest decides which kinds appear, in
 * what order, with which labels, which of them are required, and what
 * each one's rules are.
 *
 * The test that proves it (manifest-field-renderer.test.tsx) feeds this
 * component a manifest for a backend that does not exist. A `switch` on
 * backend id renders the two bundled manifests perfectly and that one
 * not at all, which is why the epic's own text says that if this
 * component grows one, #664 has failed. Adding SFTP later is meant to be
 * a JSON file, and this file is the reason it can be.
 *
 * # The kind switch is not that switch
 *
 * Switching on `field.kind` is switching on a closed vocabulary the
 * ENGINE owns (`backend.FieldKind`, seven members, refused at load if a
 * manifest invents an eighth). It has one case per kind and no case per
 * backend, so a new backend adds no code here and a new KIND is a
 * compile error in exactly the two places that have to answer for it -
 * this function and `fieldProblem`. That is the arrangement
 * MediumPreflightChecks.mark argues for: a value nobody wrote a case for
 * must not be able to render as one somebody did.
 *
 * # Credential material has its own channel, and that is structural
 *
 * A KindCredential field's inputs call `onCredentialChange` and never
 * `onChange`, so the value bag this component fills - the one the review
 * step renders and the one the save request is built from - has no
 * channel a secret could arrive on. #665's C1-C5 keep material out of
 * config.yaml, out of the echoed command line and out of every API
 * response, and none of them can see a React component: holding the
 * typed value in state that a later step renders is the one remaining
 * way to leak an access key, and it is prevented here by there being
 * nowhere to put it rather than by anybody remembering not to.
 */
import { useId } from "react";
import type { BackendManifest, BackendManifestField } from "@shared/api/contracts";
import { PasswordInput } from "@shared/components/PasswordInput";
import type { ManifestFieldValues } from "@shared/components/manifest/manifestFieldRules";

/**
 * The material for one credential, held only between typing it and
 * exchanging it for an opaque reference.
 *
 * It is a pair because the one credential this deployment can mint a
 * reference for is a pair: `importStorageCredentials(accessKeyId,
 * secretAccessKey)` is the only operation in the whole client that ever
 * carries material. So the shape here is keyed to the MINTING operation
 * and not to a backend - the day a second credential shape exists it is
 * that operation and this type that grow, and still not a branch on
 * which backend is being configured.
 */
export interface ManifestCredentialDraft {
  accessKeyId: string;
  secretAccessKey: string;
}

export function ManifestFields({
  manifest,
  values,
  problems,
  credential,
  credentialAlreadyStored = false,
  onChange,
  onCredentialChange
}: {
  manifest: BackendManifest;
  values: ManifestFieldValues;
  /** One sentence per field that is wrong, keyed by field id, from
   *  `manifestProblems`. Passed in rather than computed here so the
   *  caller decides WHEN a field starts being told it is wrong; a form
   *  that refuses a required field before it has been touched is a form
   *  that greets an operator with three errors. */
  problems: Record<string, string>;
  credential: ManifestCredentialDraft;
  /** True when this instance already has a credential configured, which
   *  makes the pair optional: leaving it empty keeps the one already
   *  there. It has to work that way, because nothing reports a
   *  destination's credential back - not even its kind - so a form
   *  cannot pre-fill what it never received, and demanding it back would
   *  mean re-importing an access key to change a region. */
  credentialAlreadyStored?: boolean;
  onChange(fieldId: string, value: string | boolean): void;
  onCredentialChange(draft: ManifestCredentialDraft): void;
}) {
  const prefix = useId();

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      {manifest.fields.map((field) => (
        <FieldRow
          key={field.id}
          field={field}
          controlId={`${prefix}-${field.id}`}
          problem={problems[field.id]}
        >
          {field.kind === "credential" ? (
            <CredentialPair
              field={field}
              controlId={`${prefix}-${field.id}`}
              draft={credential}
              alreadyStored={credentialAlreadyStored}
              onCredentialChange={onCredentialChange}
            />
          ) : (
            <ValueControl
              field={field}
              controlId={`${prefix}-${field.id}`}
              value={values[field.id] ?? ""}
              onChange={onChange}
            />
          )}
        </FieldRow>
      ))}
    </div>
  );
}

/**
 * The label, help and problem around one control.
 *
 * The label carries an id as well as htmlFor because PasswordInput names
 * its input with aria-labelledby and therefore needs a label ELEMENT to
 * point at; set on every row rather than only the credential one so the
 * two spellings cannot diverge (S3DestinationWizard's Field makes the
 * same argument).
 */
function FieldRow({
  field,
  controlId,
  problem,
  children
}: {
  field: BackendManifestField;
  controlId: string;
  problem?: string;
  children: React.ReactNode;
}) {
  return (
    <div
      data-testid={`manifest-field-row-${field.id}`}
      data-field={field.id}
      style={{ display: "flex", flexDirection: "column", gap: 4 }}
    >
      <label
        id={`${controlId}-label`}
        htmlFor={controlId}
        data-testid="manifest-field-label"
        style={{ fontSize: 12.5, fontWeight: 600 }}
      >
        {field.label}
      </label>
      {children}
      {field.help ? (
        <span
          id={`${controlId}-help`}
          data-testid="manifest-field-help"
          style={{ fontSize: 12, color: "var(--text-3)", maxWidth: "74ch" }}
        >
          {field.help}
        </span>
      ) : null}
      {problem ? (
        <span
          data-testid="manifest-field-problem"
          role="alert"
          style={{ fontSize: 12, color: "var(--danger)", maxWidth: "74ch" }}
        >
          {problem}
        </span>
      ) : null}
    </div>
  );
}

/** The six kinds that hold a value. KindCredential is the seventh and is
 *  not here: it holds material, which never enters the value bag. */
function ValueControl({
  field,
  controlId,
  value,
  onChange
}: {
  field: BackendManifestField;
  controlId: string;
  value: string | boolean;
  onChange(fieldId: string, value: string | boolean): void;
}) {
  const described = field.help ? `${controlId}-help` : undefined;

  switch (field.kind) {
    case "bool":
      return (
        <input
          id={controlId}
          data-testid="manifest-field-control"
          type="checkbox"
          aria-describedby={described}
          checked={value === true}
          onChange={(e) => onChange(field.id, e.target.checked)}
        />
      );

    case "enum":
      return (
        <select
          id={controlId}
          data-testid="manifest-field-control"
          className="input"
          aria-describedby={described}
          required={field.required}
          value={typeof value === "string" ? value : ""}
          onChange={(e) => onChange(field.id, e.target.value)}
        >
          {/* The empty choice stays selectable and stays selected, and
              its words say what leaving it empty will MEAN. Selecting
              `unset_means` for the operator would turn a default the
              product resolves at read time into a value frozen into
              their config.yaml by the next save, which is #294's
              argument and the reason UnsetMeans is documented as
              something an accessor resolves and nothing writes. */}
          <option value="">
            {field.required
              ? "Choose one"
              : field.unsetMeans
                ? `Leave unset (${field.unsetMeans})`
                : "Leave unset"}
          </option>
          {(field.values ?? []).map((choice) => (
            <option key={choice.value} value={choice.value}>
              {choice.label}
            </option>
          ))}
        </select>
      );

    case "string":
    case "path":
    case "url":
    case "key_prefix":
      return (
        <input
          id={controlId}
          data-testid="manifest-field-control"
          className="input"
          // `type="text"` for a url as well, deliberately. `type="url"`
          // brings the browser's own validity rule, which accepts any
          // scheme and would let a field pass in the browser that the
          // manifest's own KindURL rule refuses - two definitions of a
          // legal endpoint, with the one nobody wrote deciding.
          type="text"
          inputMode={field.kind === "url" ? "url" : undefined}
          aria-describedby={described}
          required={field.required}
          value={typeof value === "string" ? value : ""}
          onChange={(e) => onChange(field.id, e.target.value)}
        />
      );

    case "credential":
      throw new Error("a credential field is rendered by CredentialPair, never as a value control");
  }

  const unhandled: never = field.kind;
  throw new Error("unhandled manifest field kind: " + String(unhandled));
}

/**
 * A credential, typed once and exchanged for an opaque reference.
 *
 * The secret box is PasswordInput (#344) rather than a hand-rolled
 * `type="password"`, and that is a security decision and not a styling
 * one: it masks, it re-masks on submit, and it turns off spellcheck and
 * autocapitalize, because Chrome's and Edge's enhanced spellcheck
 * transmit the contents of a `type=text` field to a remote service and
 * exempt `type=password` explicitly. Its own docblock makes that
 * argument in full.
 *
 * The access key id is not a secret and is not masked. Masking it would
 * be theatre: it is an identifier that appears in the provider's own
 * console and in bucket policies, and a field an operator cannot read
 * back is a field they cannot check for a typo.
 */
function CredentialPair({
  field,
  controlId,
  draft,
  alreadyStored,
  onCredentialChange
}: {
  field: BackendManifestField;
  controlId: string;
  draft: ManifestCredentialDraft;
  alreadyStored: boolean;
  onCredentialChange(draft: ManifestCredentialDraft): void;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      <input
        id={controlId}
        data-testid="manifest-field-control"
        className="input"
        type="text"
        autoComplete="off"
        aria-labelledby={`${controlId}-label`}
        placeholder="key id"
        required={field.required && !alreadyStored}
        value={draft.accessKeyId}
        onChange={(e) => onCredentialChange({ ...draft, accessKeyId: e.target.value })}
      />
      <label id={`${controlId}-secret-label`} htmlFor={`${controlId}-secret`} className="sr-only">
        {field.label} — secret
      </label>
      <PasswordInput
        label={`${field.label} — secret`}
        labelledBy={`${controlId}-secret-label`}
        value={draft.secretAccessKey}
        autoComplete="off"
        required={field.required && !alreadyStored}
        onChange={(secretAccessKey) => onCredentialChange({ ...draft, secretAccessKey })}
      />
      {alreadyStored ? (
        <span style={{ fontSize: 12, color: "var(--text-3)", maxWidth: "74ch" }}>
          Leave both empty to keep the credential already configured. Nothing reports it back, not
          even its kind, so this form cannot show you which one it is.
        </span>
      ) : null}
    </div>
  );
}
