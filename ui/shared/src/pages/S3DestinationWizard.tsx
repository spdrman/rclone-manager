/**
 * The S3 destination wizard (G2.2, issue #594), following the shape agreed
 * in docs/design/s3-destination-wizard.html.
 *
 * Four panes, and nothing is written until the fourth.
 *
 *   1. Endpoint and bucket, behind a provider preset that only fills the
 *      endpoint field in.
 *   2. Credentials, in the four spellings the backend accepts.
 *   3. Test connection, which runs the engine's eight checks against the
 *      CANDIDATE and writes nothing whatever it answers. It is eight here
 *      and not nine: this pane only ever checks a bucket, and the ninth
 *      step #622 added belongs to the drive on this machine, which is not
 *      something this wizard declares.
 *   4. Save, showing the YAML that is about to be written before it is
 *      written.
 *
 * # Why the ordering is the feature
 *
 * The engine has been able to prove a storage medium works since #443, and
 * could only ever prove one that was already written into config.yaml. So
 * the supported way to find out whether a destination works was to declare
 * it first, which is exactly the ordering FR-30 exists to prevent: a
 * destination that fails verification should be one no backup was ever
 * pointed at. Step 3 checks a candidate, step 4 writes it, and Save stays
 * disabled until step 3 comes back OK.
 *
 * # The preset fills in a field, and decides nothing
 *
 * A provider preset writes an endpoint into the endpoint box and stops
 * there. The value still goes to rclone unexamined, because the set of
 * legal regions and endpoints belongs to the provider and changes without
 * this product being rebuilt: a list here would be a second, staler copy
 * that refuses a region that works, which is the argument
 * config.StorageMedium.Region's own doc makes.
 *
 * # The secret is held for one call and is never sent twice
 *
 * The pasted access key and secret go to POST /storage-credentials once,
 * in exchange for an opaque id, and are cleared from this component the
 * moment that call returns. Every later request in this flow carries the
 * id. That is the same thing the SSH wizard already does with a private
 * key (#98), and it is what lets step 3 check a candidate at all without a
 * secret, a host path or a variable name travelling on a request body.
 *
 * # Editing
 *
 * The same four panes, with step 2's credential OPTIONAL. It has to be:
 * this API reports nothing about a destination's credential, not even its
 * kind, so a form cannot pre-fill one and an edit that demanded one back
 * would mean re-importing an access key to change a region. An edit that
 * names no credential keeps the one already configured, and the wizard
 * says so rather than leaving it to be inferred from an empty box.
 */
import { useId, useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import type {
  ApiError,
  MediumPreflight,
  StorageMedium,
  StorageMediumCredentialsReference,
  StorageMediumSpec
} from "@shared/api/contracts";
import { apiErrorOf } from "@shared/api/failure";
import { Banner } from "@shared/components/Banner";
import { PasswordInput } from "@shared/components/PasswordInput";
import { MediumPreflightChecks } from "@shared/pages/MediumPreflightChecks";
import { CommandEcho } from "@shared/pages/CommandEcho";
import {
  addCommand,
  editCommand,
  importCredentialsCommand,
  testConnectionCandidateCommand
} from "@shared/pages/storageDestinationCommands";

/**
 * The providers the first pane offers, and the endpoint each one fills in.
 *
 * `endpoint: null` means "leave the endpoint field alone", which is what
 * Amazon S3 and "other" both want: AWS has no endpoint override, and an
 * unknown S3-compatible service is exactly the case this product has no
 * value for. The two placeholders that carry a region token are shown as
 * they are rather than interpolated, because guessing an operator's region
 * into an endpoint is the same mistake as holding a region list.
 */
const PROVIDERS: Array<{ id: string; label: string; endpoint: string | null }> = [
  { id: "aws", label: "Amazon S3", endpoint: null },
  { id: "minio", label: "MinIO", endpoint: "http://minio.internal:9000" },
  { id: "backblaze", label: "Backblaze B2", endpoint: "https://s3.us-west-002.backblazeb2.com" },
  { id: "wasabi", label: "Wasabi", endpoint: "https://s3.wasabisys.com" },
  { id: "other", label: "Other S3-compatible", endpoint: null }
];

const STORAGE_CLASSES = [
  "STANDARD",
  "STANDARD_IA",
  "ONEZONE_IA",
  "INTELLIGENT_TIERING",
  "GLACIER_IR",
  "GLACIER",
  "DEEP_ARCHIVE"
];

/** Which of the four credential spellings the operator is using. */
type CredentialMode = "import" | "file" | "env" | "command" | "unchanged";

interface Draft {
  id: string;
  region: string;
  endpoint: string;
  bucket: string;
  prefix: string;
  storageClass: string;
  uploadVerification: string;
}

export function S3DestinationWizard({
  editing,
  onClose,
  onSaved
}: {
  editing?: StorageMedium;
  onClose(): void;
  /** Called once the destination is written, with the destination the
   *  engine answered with.
   *
   *  It carries the saved medium since #622, because the wizard is now
   *  opened from INSIDE a retention tier's picker as well as from the
   *  settings list, and the tier has to be able to select what was just
   *  created. Handing back the engine's own answer rather than the draft
   *  is the point: an id the engine resolved differently, or a class it
   *  defaulted, is what the tier should end up pointing at. Callers that
   *  only reload a list ignore the argument. */
  onSaved(saved?: StorageMedium): void;
}) {
  const api = useApi();
  const fieldId = useId();
  const [step, setStep] = useState(1);
  const [draft, setDraft] = useState<Draft>(() => draftOf(editing));

  // "unchanged" is only reachable on an edit, and it is the DEFAULT there
  // for the reason this file's doc gives: the credential is the one field
  // that cannot be read back, so it is the one field an edit leaves alone
  // unless it is told otherwise.
  const [mode, setMode] = useState<CredentialMode>(editing ? "unchanged" : "import");
  const [accessKeyId, setAccessKeyId] = useState("");
  const [secretAccessKey, setSecretAccessKey] = useState("");
  const [file, setFile] = useState("");
  const [env, setEnv] = useState("");
  const [command, setCommand] = useState("");

  // credentialsId is what an import exchanged the material for. Once it is
  // set the material above is cleared, so from step 3 onward this
  // component holds a reference and nothing else.
  const [credentialsId, setCredentialsId] = useState("");

  const [report, setReport] = useState<MediumPreflight | null>(null);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<ApiError | null>(null);

  const credentials = credentialsOf(mode, { credentialsId, file, env, command });
  const spec: StorageMediumSpec = {
    id: draft.id,
    type: "s3",
    region: draft.region || undefined,
    endpoint: draft.endpoint || undefined,
    bucket: draft.bucket,
    prefix: draft.prefix || undefined,
    storageClass: draft.storageClass || undefined,
    uploadVerification: draft.uploadVerification || undefined,
    credentials
  };

  async function toVerify() {
    setBusy(true);
    setFailure(null);
    try {
      let ref = credentialsId;
      if (mode === "import" && !ref) {
        ref = await api.importStorageCredentials(accessKeyId, secretAccessKey);
        setCredentialsId(ref);
        // Cleared the instant the id comes back. From here on this
        // component holds a reference and the material is gone from the
        // page, which is the same discipline the SSH wizard applies to a
        // pasted private key.
        setAccessKeyId("");
        setSecretAccessKey("");
      }
      setStep(3);
      const checked = await api.preflightStorageMediumCandidate({
        ...spec,
        credentials: mode === "import" ? { credentialsId: ref } : credentials
      });
      setReport(checked);
    } catch (e) {
      setFailure(apiErrorOf(e));
      setStep(2);
    } finally {
      setBusy(false);
    }
  }

  async function verifyAgain() {
    setBusy(true);
    setFailure(null);
    setReport(null);
    try {
      setReport(await api.preflightStorageMediumCandidate(spec));
    } catch (e) {
      setFailure(apiErrorOf(e));
    } finally {
      setBusy(false);
    }
  }

  async function save() {
    setBusy(true);
    setFailure(null);
    try {
      const saved = editing
        ? await api.updateStorageMedium(editing.id, spec)
        : await api.createStorageMedium(spec);
      onSaved(saved);
    } catch (e) {
      setFailure(apiErrorOf(e));
    } finally {
      setBusy(false);
    }
  }

  // An edit that keeps its existing credential cannot be verified as a
  // candidate: a candidate carries its own reference by definition, and
  // this deployment does not report the one already configured, so there
  // is nothing to check with. Saving is allowed, and the wizard says what
  // was and was not proven rather than pretending a check ran.
  const cannotVerify = mode === "unchanged";
  const saveEnabled = cannotVerify || (report !== null && report.ok);

  return (
    <div
      role="group"
      aria-label={editing ? `Edit destination ${editing.id}` : "Add a destination"}
      style={{
        border: "1px solid var(--border-strong)",
        borderRadius: "var(--radius-xl)",
        padding: 14,
        display: "flex",
        flexDirection: "column",
        gap: 14,
        background: "var(--surface-2)"
      }}
    >
      <div style={{ display: "flex", alignItems: "baseline", gap: 10, flexWrap: "wrap" }}>
        <strong style={{ fontSize: 14 }}>
          {editing ? `Edit destination · ${editing.id}` : "Add a destination"}
        </strong>
        <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
          {step === 4 ? "what gets written, shown before it is written" : "nothing is saved until step 4"}
        </span>
        <span style={{ marginLeft: "auto", fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
          Step {step} of 4
        </span>
      </div>

      {failure ? (
        <Banner tone="warn" style={{ fontSize: 13 }} dismissKey={failure.message}>
          {failure.message}
        </Banner>
      ) : null}

      {step === 1 ? (
        <EndpointPane
          fieldId={fieldId}
          draft={draft}
          lockId={Boolean(editing)}
          onChange={setDraft}
          onNext={() => setStep(2)}
          onCancel={onClose}
        />
      ) : null}

      {step === 2 ? (
        <CredentialsPane
          fieldId={fieldId}
          editing={Boolean(editing)}
          mode={mode}
          onMode={setMode}
          accessKeyId={accessKeyId}
          onAccessKeyId={setAccessKeyId}
          secretAccessKey={secretAccessKey}
          onSecretAccessKey={setSecretAccessKey}
          file={file}
          onFile={setFile}
          env={env}
          onEnv={setEnv}
          command={command}
          onCommand={setCommand}
          busy={busy}
          onBack={() => setStep(1)}
          onNext={cannotVerify ? () => setStep(4) : toVerify}
          nextLabel={cannotVerify ? "Next: save" : "Next: test connection"}
        />
      ) : null}

      {step === 3 ? (
        <VerifyPane
          spec={spec}
          report={report}
          busy={busy}
          onBack={() => setStep(2)}
          onVerifyAgain={verifyAgain}
          onNext={() => setStep(4)}
        />
      ) : null}

      {step === 4 ? (
        <SavePane
          spec={spec}
          editing={Boolean(editing)}
          verified={report !== null && report.ok}
          cannotVerify={cannotVerify}
          busy={busy}
          saveEnabled={saveEnabled}
          onBack={() => setStep(cannotVerify ? 2 : 3)}
          onSave={save}
        />
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Step 1: endpoint and bucket
// ---------------------------------------------------------------------------

function EndpointPane({
  fieldId,
  draft,
  lockId,
  onChange,
  onNext,
  onCancel
}: {
  fieldId: string;
  draft: Draft;
  lockId: boolean;
  onChange(d: Draft): void;
  onNext(): void;
  onCancel(): void;
}) {
  const set = (patch: Partial<Draft>) => onChange({ ...draft, ...patch });
  const ready = draft.id.trim() !== "" && draft.bucket.trim() !== "";

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
        {PROVIDERS.map((p) => (
          <button
            key={p.id}
            className="btn"
            type="button"
            onClick={() => set({ endpoint: p.endpoint ?? "" })}
          >
            {p.label}
          </button>
        ))}
      </div>
      <p style={{ margin: 0, fontSize: 12.5, color: "var(--text-2)", maxWidth: "74ch" }}>
        A preset only fills the endpoint field in. The value is handed to rclone unexamined, because
        the list of legal regions and endpoints belongs to the provider, not to this product.
      </p>

      <Field id={`${fieldId}-id`} label="Destination id" hint="lower_snake_case; this is the name a retention tier will use">
        <input
          id={`${fieldId}-id`}
          className="input"
          value={draft.id}
          disabled={lockId}
          placeholder="offsite_s3"
          onChange={(e) => set({ id: e.target.value })}
        />
      </Field>
      <Field id={`${fieldId}-region`} label="Region">
        <input
          id={`${fieldId}-region`}
          className="input"
          value={draft.region}
          placeholder="us-east-1"
          onChange={(e) => set({ region: e.target.value })}
        />
      </Field>
      <Field
        id={`${fieldId}-endpoint`}
        label="Endpoint (optional)"
        hint="leave empty for the provider's own endpoint for this region"
      >
        <input
          id={`${fieldId}-endpoint`}
          className="input"
          value={draft.endpoint}
          onChange={(e) => set({ endpoint: e.target.value })}
        />
      </Field>
      <Field id={`${fieldId}-bucket`} label="Bucket">
        <input
          id={`${fieldId}-bucket`}
          className="input"
          value={draft.bucket}
          placeholder="nas-backups"
          onChange={(e) => set({ bucket: e.target.value })}
        />
      </Field>
      <Field id={`${fieldId}-prefix`} label="Prefix (optional)">
        <input
          id={`${fieldId}-prefix`}
          className="input"
          value={draft.prefix}
          placeholder="monthly"
          onChange={(e) => set({ prefix: e.target.value })}
        />
      </Field>
      <Field id={`${fieldId}-class`} label="Storage class">
        <select
          id={`${fieldId}-class`}
          className="input"
          value={draft.storageClass}
          onChange={(e) => set({ storageClass: e.target.value })}
        >
          {STORAGE_CLASSES.map((c) => (
            <option key={c} value={c}>
              {c}
            </option>
          ))}
        </select>
      </Field>
      <Field
        id={`${fieldId}-verification`}
        label="Upload verification"
        hint="readback re-downloads and re-hashes before the local copy goes"
      >
        <select
          id={`${fieldId}-verification`}
          className="input"
          value={draft.uploadVerification}
          onChange={(e) => set({ uploadVerification: e.target.value })}
        >
          <option value="readback">readback</option>
          <option value="attested">attested</option>
        </select>
      </Field>

      <div style={{ display: "flex", gap: 8 }}>
        <button className="btn" type="button" onClick={onCancel}>
          Cancel
        </button>
        <button className="btn btn--primary" type="button" disabled={!ready} onClick={onNext}>
          Next: credentials
        </button>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Step 2: credentials
// ---------------------------------------------------------------------------

function CredentialsPane(props: {
  fieldId: string;
  editing: boolean;
  mode: CredentialMode;
  onMode(m: CredentialMode): void;
  accessKeyId: string;
  onAccessKeyId(v: string): void;
  secretAccessKey: string;
  onSecretAccessKey(v: string): void;
  file: string;
  onFile(v: string): void;
  env: string;
  onEnv(v: string): void;
  command: string;
  onCommand(v: string): void;
  busy: boolean;
  onBack(): void;
  onNext(): void;
  nextLabel: string;
}) {
  const { fieldId, mode } = props;
  const options: Array<{ id: CredentialMode; label: string; note: string }> = [
    ...(props.editing
      ? [
          {
            id: "unchanged" as CredentialMode,
            label: "Keep the credential already configured",
            note:
              "This product never reports a destination's credential back, not even which kind it is, so there is nothing to pre-fill and nothing to resubmit. Leaving it alone is what lets you change a region without re-importing a key."
          }
        ]
      : []),
    {
      id: "import",
      label: "Paste a key and let me store it",
      note:
        "I write it to a 0600 file beside config.yaml and keep only the id, the same thing importing an SSH private key already does."
    },
    {
      id: "file",
      label: "A credentials file already on this machine",
      note: "AWS shared-credentials format. rclone opens it, so the secret never enters this process at all."
    },
    {
      id: "env",
      label: "An environment variable",
      note: "Named here, read at connection time. Nothing about its value is stored."
    },
    {
      id: "command",
      label: "A command that prints them",
      note:
        "An argv array, run directly and never through a shell. This is how a secrets manager gets adopted without this product depending on one."
    }
  ];

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      <p style={{ margin: 0, fontSize: 12.5, color: "var(--text-2)", maxWidth: "74ch" }}>
        Whichever of these you pick, what lands in the configuration file is a{" "}
        <strong>reference</strong>: a path, a variable name, or a command. config.yaml has no field a
        secret fits in, and never will.
      </p>

      {options.map((o) => (
        <label key={o.id} style={{ display: "flex", gap: 9, alignItems: "flex-start", fontSize: 13 }}>
          <input
            type="radio"
            name={`${fieldId}-credential-mode`}
            checked={mode === o.id}
            onChange={() => props.onMode(o.id)}
          />
          <span>
            <span style={{ fontWeight: 600 }}>{o.label}</span>
            <span style={{ display: "block", color: "var(--text-2)", fontSize: 12.5, maxWidth: "70ch" }}>
              {o.note}
            </span>
          </span>
        </label>
      ))}

      {mode === "import" ? (
        <>
          <Field id={`${fieldId}-akid`} label="Access key id">
            <input
              id={`${fieldId}-akid`}
              className="input"
              autoComplete="off"
              value={props.accessKeyId}
              onChange={(e) => props.onAccessKeyId(e.target.value)}
            />
          </Field>
          <Field
            id={`${fieldId}-secret`}
            label="Secret access key"
            hint="Discarded from this page the moment step 3 answers. It is never logged, never echoed back, and never returned by any read of the configuration."
          >
            <PasswordInput
              label="Secret access key"
              labelledBy={`${fieldId}-secret-label`}
              value={props.secretAccessKey}
              onChange={props.onSecretAccessKey}
              autoComplete="off"
            />
          </Field>
        </>
      ) : null}

      {mode === "file" ? (
        <Field id={`${fieldId}-file`} label="Path to the credentials file">
          <input
            id={`${fieldId}-file`}
            className="input"
            value={props.file}
            placeholder="/etc/rclone-manager/s3.creds"
            onChange={(e) => props.onFile(e.target.value)}
          />
        </Field>
      ) : null}

      {mode === "env" ? (
        <Field id={`${fieldId}-env`} label="Environment variable name">
          <input
            id={`${fieldId}-env`}
            className="input"
            value={props.env}
            placeholder="BACKUP_S3_OFFSITE"
            onChange={(e) => props.onEnv(e.target.value)}
          />
        </Field>
      ) : null}

      {mode === "command" ? (
        <Field id={`${fieldId}-command`} label="Command" hint="split on spaces, run directly, never through a shell">
          <input
            id={`${fieldId}-command`}
            className="input"
            value={props.command}
            placeholder="op read op://vault/s3/creds"
            onChange={(e) => props.onCommand(e.target.value)}
          />
        </Field>
      ) : null}

      {mode === "import" ? (
        <CommandEcho label="the same thing from a terminal" commands={[importCredentialsCommand()]} />
      ) : null}

      <div style={{ display: "flex", gap: 8 }}>
        <button className="btn" type="button" onClick={props.onBack}>
          Back
        </button>
        <button className="btn btn--primary" type="button" disabled={props.busy} onClick={props.onNext}>
          {props.busy ? "Working…" : props.nextLabel}
        </button>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Step 3: verify
// ---------------------------------------------------------------------------

function VerifyPane({
  spec,
  report,
  busy,
  onBack,
  onVerifyAgain,
  onNext
}: {
  spec: StorageMediumSpec;
  report: MediumPreflight | null;
  busy: boolean;
  onBack(): void;
  onVerifyAgain(): void;
  onNext(): void;
}) {
  const failed = report !== null && !report.ok;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      <div style={{ fontSize: 12.5, color: "var(--text-2)" }}>
        A real object, written and read back and deleted. Nothing is saved by checking.
      </div>

      {failed ? (
        // Not dismissible (#620). It is the only thing on screen saying
        // why Save is off, and its middle sentence is a claim about the
        // check list BELOW it: steps after the failing one were never
        // tried. Put this away and that list reads as a complete report
        // on a destination, which is the misunderstanding it exists to
        // prevent.
        <Banner tone="warn" dismissible={false} style={{ display: "block", fontSize: 13 }}>
          <div style={{ fontWeight: 600, marginBottom: 4 }}>Not saved.</div>
          <p style={{ margin: 0, maxWidth: "74ch" }}>
            Steps after the failing one were never tried, so nothing below claims anything about
            them. Fix the destination or the key policy and test the connection again. Save stays
            disabled.
          </p>
        </Banner>
      ) : null}

      {busy ? (
        <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>Checking the destination…</p>
      ) : report ? (
        <MediumPreflightChecks report={report} />
      ) : null}

      <CommandEcho label="the same thing from a terminal" commands={[testConnectionCandidateCommand(spec)]} />

      <div style={{ display: "flex", gap: 8 }}>
        <button className="btn" type="button" onClick={onBack}>
          Back
        </button>
        <button className="btn" type="button" disabled={busy} onClick={onVerifyAgain}>
          Test connection again
        </button>
        <button
          className="btn btn--primary"
          type="button"
          disabled={busy || report === null || !report.ok}
          onClick={onNext}
        >
          Next: save
        </button>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Step 4: save
// ---------------------------------------------------------------------------

function SavePane({
  spec,
  editing,
  verified,
  cannotVerify,
  busy,
  saveEnabled,
  onBack,
  onSave
}: {
  spec: StorageMediumSpec;
  editing: boolean;
  verified: boolean;
  cannotVerify: boolean;
  busy: boolean;
  saveEnabled: boolean;
  onBack(): void;
  onSave(): void;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      <p style={{ margin: 0, fontSize: 12.5, color: "var(--text-2)", maxWidth: "74ch" }}>
        Declaring a destination moves nothing on its own. Backups arrive here only once a retention
        tier names it, which is a separate decision with a disclosure of its own.
      </p>

      {cannotVerify ? (
        // Not dismissible (#620). This is the sentence that says which
        // half of the check is happening, sitting directly above Save.
        // Its verified counterpart below can be put away, and that
        // asymmetry is the point: the reassuring half is disposable and
        // the one about an unproven destination is not.
        //
        // It used to say nothing would be proven at all, and #636 made
        // that half wrong in the same way it made `medium edit`'s own
        // line wrong: this page cannot check the destination, because it
        // cannot read the credential already configured, and the engine
        // can, because it resolves that credential out of the file it is
        // about to write. So the save is checked and can be refused, and
        // the honest sentence is the one that says who is doing it.
        <Banner tone="info" dismissible={false} style={{ fontSize: 12.5 }}>
          Not checked from this page: this edit keeps the credential already configured, which this
          page cannot read. The manager tests the destination before the edit lands and refuses when
          it cannot, naming the step that failed.
        </Banner>
      ) : verified ? (
        <Banner tone="info" style={{ fontSize: 12.5 }}>
          Verified: an object was written to this destination, read back byte for byte, and deleted.
        </Banner>
      ) : null}

      <pre
        className="mono"
        style={{
          margin: 0,
          fontSize: "var(--text-xs)",
          background: "var(--surface-1)",
          border: "1px solid var(--border)",
          borderRadius: "var(--radius-md)",
          padding: 10,
          overflowX: "auto"
        }}
      >
        {yamlPreview(spec)}
      </pre>

      <p style={{ margin: 0, fontSize: 12, color: "var(--text-2)", maxWidth: "74ch" }}>
        There is no <span className="mono">access_key_id</span> key here and there is no{" "}
        <span className="mono">secret_access_key</span> key here. A configuration file carrying
        either is a parse error, before validation even runs.
      </p>

      <CommandEcho
        label="the same thing from a terminal"
        commands={[editing ? editCommand(spec) : addCommand(spec)]}
      />

      <div style={{ display: "flex", gap: 8 }}>
        <button className="btn" type="button" onClick={onBack}>
          Back
        </button>
        <button
          className="btn btn--primary"
          type="button"
          disabled={busy || !saveEnabled}
          onClick={onSave}
        >
          {busy ? "Saving…" : editing ? "Save changes" : "Save destination"}
        </button>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function draftOf(editing?: StorageMedium): Draft {
  return {
    id: editing?.id ?? "",
    region: editing?.region ?? "",
    endpoint: editing?.endpoint ?? "",
    bucket: editing?.bucket ?? "",
    prefix: editing?.prefix ?? "",
    storageClass: editing?.storageClass ?? "STANDARD",
    uploadVerification: editing?.uploadVerification ?? "readback"
  };
}

/**
 * The credential reference this wizard submits, from whichever of the four
 * spellings is selected.
 *
 * "unchanged" produces `undefined` rather than an empty object, and the
 * difference is what the backend reads: an absent credentials block means
 * "keep the one already configured", and an empty one would be
 * indistinguishable from a form that meant to send a reference and lost
 * it.
 */
function credentialsOf(
  mode: CredentialMode,
  v: { credentialsId: string; file: string; env: string; command: string }
): StorageMediumCredentialsReference | undefined {
  switch (mode) {
    case "unchanged":
      return undefined;
    case "import":
      return v.credentialsId ? { credentialsId: v.credentialsId } : undefined;
    case "file":
      return { file: v.file };
    case "env":
      return { env: v.env };
    default:
      return { command: v.command.split(/\s+/).filter(Boolean) };
  }
}

/**
 * The YAML the save is about to write, shown before it is written.
 *
 * It is a rendering for a person and not the bytes the server will
 * actually produce, which is why it says so on screen: the engine encodes
 * the whole configuration, and this describes the one entry being added to
 * it. What it is exact about is the part that matters, which is that the
 * credential appears as a REFERENCE and that there is no key here a secret
 * could be in.
 */
function yamlPreview(spec: StorageMediumSpec): string {
  const lines = [
    "# config.yaml, storage_mediums[]",
    `- id: ${spec.id || "<destination id>"}`,
    `  type: ${spec.type}`
  ];
  if (spec.region) lines.push(`  region: ${spec.region}`);
  if (spec.endpoint) lines.push(`  endpoint: ${spec.endpoint}`);
  lines.push(`  bucket: ${spec.bucket || "<bucket>"}`);
  if (spec.prefix) lines.push(`  prefix: ${spec.prefix}`);
  if (spec.storageClass) lines.push(`  storage_class: ${spec.storageClass}`);
  if (spec.uploadVerification) lines.push(`  upload_verification: ${spec.uploadVerification}`);

  const c = spec.credentials;
  if (!c) {
    lines.push("  credentials: <unchanged>");
  } else if (c.credentialsId) {
    lines.push("  credentials:");
    lines.push(`    file: <config dir>/s3_credentials/${c.credentialsId}   # 0600, written by step 2`);
  } else if (c.file) {
    lines.push("  credentials:");
    lines.push(`    file: ${c.file}`);
  } else if (c.env) {
    lines.push("  credentials:");
    lines.push(`    env: ${c.env}`);
  } else if (c.command && c.command.length > 0) {
    lines.push("  credentials:");
    lines.push("    command:");
    for (const part of c.command) lines.push(`      - ${part}`);
  }
  return lines.join("\n");
}

function Field({
  id,
  label,
  hint,
  children
}: {
  id: string;
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
      {/* The label carries an id as well as htmlFor, because PasswordInput
          names its input with aria-labelledby (its own doc says why) and
          therefore needs a label ELEMENT to point at, not just a for/id
          pair. Setting it on every field rather than only that one keeps
          the two spellings from diverging. */}
      <label id={`${id}-label`} htmlFor={id} style={{ fontSize: 12.5, fontWeight: 600 }}>
        {label}
      </label>
      {children}
      {hint ? (
        <span style={{ fontSize: 12, color: "var(--text-3)", maxWidth: "74ch" }}>{hint}</span>
      ) : null}
    </div>
  );
}
