/**
 * Configure a destination that already exists (I2.2, issue #669).
 *
 * A separate flow from adding, and entered after it: #668 declares the
 * instance and names it, this fills in the fields its backend's manifest
 * declares, proves them, and shows what is about to be written.
 *
 * # Three steps, and the first one holds no field list
 *
 * Everything about which fields appear lives in ManifestFields and its
 * rules module, driven by the manifest. This file decides the ORDER -
 * fill, test, review - and nothing about the form. There is no branch
 * anywhere below on which backend is being configured, and the test that
 * proves it drives all of this against a manifest for a backend that
 * does not exist.
 *
 * # The mark is cleared by any edit to the fields
 *
 * #636's guarantee, and the reason `provenFor` holds a key derived from
 * the values rather than a boolean: a changed endpoint must not inherit
 * an old pass. Editing anything makes the pass stale by construction,
 * because the key no longer matches, so nothing has to remember to reset
 * a flag. Save is offered only while the key matches, which is the same
 * ordering FR-30 exists to enforce and the same one #594's wizard has:
 * what was proven is what gets written.
 *
 * # The secret is held for one call and never appears again
 *
 * The typed pair goes to `importStorageCredentials` once, in exchange
 * for an opaque id, and is cleared from this component the moment that
 * call returns. Every later request in this flow carries the id. That is
 * what lets step 2 prove a configuration at all without material on a
 * request body, and it is the same thing the SSH wizard does with a
 * private key (#98) and #594's wizard does with an access key.
 *
 * # Making it the default is offered, never taken
 *
 * A checkbox on the review step with the consequence written beside it,
 * and a separate call after the save. Never a side effect of saving
 * (#669), and never bundled into the configuration request: the two are
 * different acts and the engine keeps them separate for the reason #622
 * gives.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import type {
  ApiError,
  BackendManifest,
  MediumPreflight,
  StorageMedium
} from "@shared/api/contracts";
import { apiErrorOf } from "@shared/api/failure";
import { Banner } from "@shared/components/Banner";
import { CommandEcho } from "@shared/pages/CommandEcho";
import { ManifestFields } from "@shared/components/manifest/ManifestFields";
import type { ManifestCredentialDraft } from "@shared/components/manifest/ManifestFields";
import { ManifestProbeSteps } from "@shared/components/manifest/ManifestProbeSteps";
import { ManifestReview } from "@shared/components/manifest/ManifestReview";
import {
  configurationValues,
  credentialField,
  emptyValues,
  manifestProblems
} from "@shared/components/manifest/manifestFieldRules";
import type { ManifestFieldValues } from "@shared/components/manifest/manifestFieldRules";
import {
  editConfigurationCommand,
  importCredentialsCommand,
  testConnectionCommand
} from "@shared/pages/destinationConfigureCommands";

type Step = "fields" | "test" | "review";

const NO_CREDENTIAL: ManifestCredentialDraft = { accessKeyId: "", secretAccessKey: "" };

export function DestinationConfigureWizard({
  manifest,
  destination,
  currentDefault,
  onClose,
  onSaved
}: {
  manifest: BackendManifest;
  destination: StorageMedium;
  currentDefault: string | null;
  onClose(): void;
  onSaved(medium: StorageMedium): void;
}) {
  const api = useApi();

  const [step, setStep] = useState<Step>("fields");
  // null until the destination's current configuration has been read.
  // Starting from `emptyValues` and correcting later would render one
  // frame of a form claiming this destination has nothing configured,
  // and a save from that frame would unset fields the operator never
  // touched: this flow sends the whole declared set, so an empty form is
  // not a neutral starting point.
  const [values, setValues] = useState<ManifestFieldValues | null>(null);
  const [credentialConfigured, setCredentialConfigured] = useState(false);
  const [credential, setCredential] = useState<ManifestCredentialDraft>(NO_CREDENTIAL);
  const [credentialsId, setCredentialsId] = useState<string | null>(null);
  const [report, setReport] = useState<MediumPreflight | null>(null);
  const [provenFor, setProvenFor] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [elapsedMs, setElapsedMs] = useState<number | null>(null);
  const [makeDefault, setMakeDefault] = useState(false);
  const [failure, setFailure] = useState<ApiError | null>(null);
  const startedAt = useRef<number | null>(null);

  useEffect(() => {
    let live = true;
    api
      .getStorageMediumConfiguration(destination.id)
      .then((current) => {
        if (!live) return;
        setValues({ ...emptyValues(manifest), ...current.fields });
        setCredentialConfigured(current.credentialConfigured);
      })
      .catch((e: unknown) => live && setFailure(apiErrorOf(e)));
    return () => {
      live = false;
    };
  }, [api, destination.id, manifest]);

  // The elapsed counter, and the only reason this component owns a
  // timer. Nothing streams step progress - the manager answers a probe
  // with one whole report - so this is what says the request is alive
  // during a round trip long enough to look hung.
  useEffect(() => {
    if (!busy) return;
    const tick = setInterval(() => {
      if (startedAt.current !== null) setElapsedMs(Date.now() - startedAt.current);
    }, 100);
    return () => clearInterval(tick);
  }, [busy]);

  const problems = useMemo(
    () => (values === null ? {} : manifestProblems(manifest, values)),
    [manifest, values]
  );
  const credentialDeclared = credentialField(manifest);
  const credentialTyped = credential.accessKeyId !== "" || credential.secretAccessKey !== "";
  const credentialSatisfied =
    credentialDeclared === undefined ||
    !credentialDeclared.required ||
    credentialConfigured ||
    credentialsId !== null ||
    credentialTyped;
  const describable =
    values !== null && Object.keys(problems).length === 0 && credentialSatisfied;

  const proofKey = useMemo(
    () => (values === null ? "" : proofKeyOf(values, credentialsId, credentialTyped)),
    [credentialTyped, credentialsId, values]
  );
  const verified = provenFor !== null && provenFor === proofKey;

  const editField = useCallback((fieldId: string, value: string | boolean) => {
    // The report goes with the edit: a step list describing an endpoint
    // that is no longer in the form is worse than no step list, because
    // it reads as a report about what is on screen. The PASS needs no
    // clearing here - see proofKeyOf - and deliberately is not cleared,
    // so that the screen can say the pass has gone stale rather than
    // silently having none.
    setValues((previous) => (previous === null ? previous : { ...previous, [fieldId]: value }));
    setReport(null);
    setElapsedMs(null);
    setFailure(null);
  }, []);

  const runProbe = useCallback(async () => {
    if (values === null) return;
    setBusy(true);
    setFailure(null);
    setReport(null);
    startedAt.current = Date.now();
    setElapsedMs(0);
    try {
      // The material is exchanged for an opaque id first, and dropped
      // from this component before anything else is sent. From here on
      // the flow carries a reference that names no path and no variable
      // on the manager's host.
      let reference = credentialsId;
      if (credentialTyped) {
        reference = await api.importStorageCredentials(
          credential.accessKeyId,
          credential.secretAccessKey
        );
        setCredential(NO_CREDENTIAL);
        setCredentialsId(reference);
        setCredentialConfigured(true);
      }

      const answer = await api.preflightStorageMediumConfiguration(destination.id, {
        fields: configurationValues(manifest, values),
        ...(reference ? { credentials: { credentialsId: reference } } : {})
      });
      setReport(answer);
      setElapsedMs(Date.now() - (startedAt.current ?? Date.now()));
      // Recorded against the configuration that was actually proven,
      // with the material already exchanged: `credentialTyped` is false
      // from here, which is the state the form is in once the pair has
      // been cleared.
      if (answer.ok) setProvenFor(proofKeyOf(values, reference, false));
    } catch (e: unknown) {
      setFailure(apiErrorOf(e));
      setElapsedMs(null);
    } finally {
      setBusy(false);
    }
  }, [api, credential, credentialTyped, credentialsId, destination.id, manifest, values]);

  const toTestStep = useCallback(() => {
    setStep("test");
    void runProbe();
  }, [runProbe]);

  const save = useCallback(async () => {
    if (values === null) return;
    setBusy(true);
    setFailure(null);
    try {
      let saved = await api.configureStorageMedium(destination.id, {
        fields: configurationValues(manifest, values),
        ...(credentialsId ? { credentials: { credentialsId } } : {})
      });
      // After the write and never inside it. A save that also moved the
      // default would be doing a second thing the operator asked for in
      // a different sentence, and the engine answers each with the
      // destination as it now stands so the caller renders what was
      // persisted.
      if (makeDefault) saved = await api.setDefaultStorageMedium(destination.id);
      onSaved(saved);
    } catch (e: unknown) {
      setFailure(apiErrorOf(e));
    } finally {
      setBusy(false);
    }
  }, [api, credentialsId, destination.id, makeDefault, manifest, onSaved, values]);

  const echo = useMemo(() => {
    if (values === null) return { command: "", fieldsWithNoFlag: [] as string[] };
    return editConfigurationCommand(manifest, destination.id, configurationValues(manifest, values));
  }, [destination.id, manifest, values]);

  return (
    <div
      role="group"
      aria-label={`Configure ${destination.id}`}
      style={{ display: "flex", flexDirection: "column", gap: 14 }}
    >
      <div style={{ fontSize: 12.5, color: "var(--text-2)" }}>
        {manifest.label} — {manifest.summary}
      </div>

      {failure ? (
        <Banner tone="warn" dismissible={false} style={{ display: "block", fontSize: 13 }}>
          <div style={{ fontWeight: 600, marginBottom: 4 }}>Nothing was written.</div>
          {/* The service's sentence, repeated. Not reworded and not
              interpolated with anything from the form: refusals by
              shape, never by content (mediumcreds.go). */}
          <p style={{ margin: 0, maxWidth: "74ch" }} data-testid="configure-failure">
            {failure.message}
          </p>
        </Banner>
      ) : null}

      {values === null ? (
        <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
          Reading this destination&rsquo;s configuration…
        </p>
      ) : step === "fields" ? (
        <>
          <ManifestFields
            manifest={manifest}
            values={values}
            problems={problems}
            credential={credential}
            credentialAlreadyStored={credentialConfigured}
            onChange={editField}
            onCredentialChange={(next) => {
              setCredential(next);
              setReport(null);
            }}
          />
          {provenFor !== null && !verified ? (
            // The mark, cleared, said out loud (#636). It would be
            // enough for correctness to drop the pass silently - Save is
            // gated on it either way - but then the only evidence of the
            // rule would be a button an operator finds disabled two
            // steps later, and "a changed endpoint must not inherit an
            // old pass" is worth saying where the change was made. It is
            // also what makes the rule observable, and therefore
            // testable, at the moment it applies.
            <p
              data-testid="configure-mark-stale"
              style={{ margin: 0, fontSize: 12.5, color: "var(--text-2)", maxWidth: "74ch" }}
            >
              The check that passed was for different values. This destination has to be tested again
              before the change can be saved.
            </p>
          ) : null}
          {credentialDeclared ? (
            <CommandEcho label="the same thing from a terminal" commands={[importCredentialsCommand()]} />
          ) : null}
          <Buttons
            onCancel={onClose}
            primaryLabel="Next: test connection"
            primaryEnabled={describable}
            onPrimary={toTestStep}
          />
        </>
      ) : step === "test" ? (
        <>
          <div style={{ fontSize: 12.5, color: "var(--text-2)", maxWidth: "74ch" }}>
            A real object, written to this destination and read back and removed. Reachable is not the
            same as writable. Nothing is saved by checking, and a refusal leaves this destination
            exactly as unverified as it is now.
          </div>
          <ManifestProbeSteps
            manifest={manifest}
            report={report}
            running={busy}
            elapsedMs={elapsedMs}
          />
          {report !== null && !report.ok ? (
            // Not dismissible (#620). It is the only thing on screen
            // saying why the review step is out of reach, and its second
            // sentence is a claim about the list above it.
            <Banner tone="warn" dismissible={false} style={{ display: "block", fontSize: 13 }}>
              <div style={{ fontWeight: 600, marginBottom: 4 }}>Not saved, and not verified.</div>
              <p style={{ margin: 0, maxWidth: "74ch" }}>
                Steps after the failing one were never tried, so nothing above claims anything about
                them. The reason beside the failing step is the manager&rsquo;s own; fix the
                destination or the policy and test again.
              </p>
            </Banner>
          ) : null}
          <CommandEcho
            label="the engine's own check, against what is already saved"
            commands={[testConnectionCommand(destination.id)]}
          />
          <Buttons
            onCancel={() => setStep("fields")}
            cancelLabel="Back"
            secondaryLabel={busy ? "Testing…" : "Test connection again"}
            secondaryEnabled={!busy}
            onSecondary={() => void runProbe()}
            primaryLabel="Next: review"
            primaryEnabled={!busy && verified}
            onPrimary={() => setStep("review")}
          />
        </>
      ) : (
        <>
          <ManifestReview
            manifest={manifest}
            destinationId={destination.id}
            values={values}
            credentialStored={credentialConfigured}
            verified={verified}
            makeDefault={makeDefault}
            currentDefault={currentDefault}
            onMakeDefaultChange={setMakeDefault}
          />
          <CommandEcho label="the same thing from a terminal" commands={[echo.command]} />
          {echo.fieldsWithNoFlag.length > 0 ? (
            // A parity gap printed as a gap. `medium edit` has no flag
            // for these field ids, so the line above configures less
            // than this screen is about to save, and saying so is the
            // whole point of printing commands at all (EPIC G's rule,
            // argued in storageDestinationCommands.ts).
            <p
              data-testid="configure-command-gap"
              style={{ margin: 0, fontSize: 12, color: "var(--text-3)", maxWidth: "74ch" }}
            >
              That command cannot carry {echo.fieldsWithNoFlag.join(", ")}: this CLI has no flag for
              those fields yet, so the line above is not the whole of what this save writes.
            </p>
          ) : null}
          <Buttons
            onCancel={() => setStep("test")}
            cancelLabel="Back"
            primaryLabel={busy ? "Saving…" : "Save configuration"}
            primaryEnabled={!busy && verified}
            onPrimary={() => void save()}
          />
        </>
      )}
    </div>
  );
}

function Buttons({
  onCancel,
  cancelLabel = "Cancel",
  secondaryLabel,
  secondaryEnabled,
  onSecondary,
  primaryLabel,
  primaryEnabled,
  onPrimary
}: {
  onCancel(): void;
  cancelLabel?: string;
  secondaryLabel?: string;
  secondaryEnabled?: boolean;
  onSecondary?(): void;
  primaryLabel: string;
  primaryEnabled: boolean;
  onPrimary(): void;
}) {
  return (
    <div style={{ display: "flex", gap: 8 }}>
      <button className="btn" type="button" onClick={onCancel}>
        {cancelLabel}
      </button>
      {secondaryLabel ? (
        <button className="btn" type="button" disabled={!secondaryEnabled} onClick={onSecondary}>
          {secondaryLabel}
        </button>
      ) : null}
      <button className="btn btn--primary" type="button" disabled={!primaryEnabled} onClick={onPrimary}>
        {primaryLabel}
      </button>
    </div>
  );
}

/**
 * Which configuration a pass belongs to (#636).
 *
 * The mark is not a boolean, and that is the whole of the mechanism.
 * "Verified" means a check passed FOR THESE VALUES, so the pass is
 * recorded as a key derived from them and compared against the key the
 * form currently has: an edit makes the two differ by construction, and
 * nothing anywhere has to remember to reset a flag. A boolean would have
 * needed a reset at every mutation site, and the site somebody forgets
 * is the one where a changed endpoint inherits an old pass.
 *
 * Freshly typed credential material counts, and has to. The reference
 * cannot: material is exchanged for an opaque id and dropped, so a pair
 * typed after a pass changes nothing the id can see, and the pass would
 * stand for a key that is no longer the one being offered.
 */
function proofKeyOf(
  values: ManifestFieldValues,
  credentialsId: string | null,
  credentialTyped: boolean
): string {
  return JSON.stringify([values, credentialsId, credentialTyped]);
}
