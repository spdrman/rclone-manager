import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { usePlatform } from "@shared/platform/PlatformContext";
import { useApi } from "@shared/api/ApiContext";
import { BackupManagerError } from "@shared/api/contracts";
import type { ValidatorCatalogEntry } from "@shared/api/contracts";
import { PageHeader } from "@shared/components/PageHeader";
import { WarningBanner } from "@shared/components/WarningBanner";
import { FingerprintDisplay } from "@shared/components/FingerprintDisplay";
import { FieldHelp, HelpField } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import type { FieldHelpCopy } from "@shared/components/fieldHelpCopy";
import type { CompletionMethod } from "@shared/types/backup";
import { graph, useCausl } from "@shared/state/graph";
import { setsNode } from "@shared/state/appNodes";
import { fetchResource } from "@shared/state/resource";
import { resetWizardAnswers, wizardCanSaveNode, wizardHostKeyChangedNode } from "@shared/state/wizardNodes";

const STEPS = [
  "Source",
  "Authentication",
  "Verify server",
  "Discovery",
  "Storage & validation",
  "Review"
] as const;

/** Shown only until the real probe (issue #146) resolves for the first
 *  time — see the "Verify server" step below — so step 3 never renders
 *  a completely blank fingerprint while that request is in flight.
 *  Never what "Trust host" actually trusts: that always reads the real
 *  probedKnownHostsLine state, never this constant. */
const FINGERPRINT_PLACEHOLDER = "SHA256:…probing…";

function errorMessage(e: unknown, fallback: string): string {
  return e instanceof BackupManagerError ? e.api.message : fallback;
}

function completionStrategyFor(method: CompletionMethod): "rename" | "marker" | "stable" {
  if (method === "atomic-rename") return "rename";
  if (method === "stable-size") return "stable";
  return "marker";
}

function completionSummaryLabel(method: CompletionMethod): string {
  if (method === "atomic-rename") return "atomic rename";
  if (method === "stable-size") return "stable size";
  return "completion marker";
}

/** Issue #176: the same wizard, run on an instance that has no
 *  configuration yet.
 *
 *  Only two things change, and both are consequences of there being
 *  nothing on disk rather than of a different flow. The save goes to POST
 *  /api/v1/system/first-run instead of POST /api/v1/backup-sets, because
 *  there is no configuration to fold a set into. And "Save, enable & run"
 *  is not offered: an unconfigured instance has no service to submit a
 *  run to until the configuration it is about to write has been opened,
 *  so a button promising an immediate run would be promising something
 *  the backend deliberately does not do (core/service's
 *  CreateInitialConfig ignores run_immediately, and says why). */
export interface BackupSetWizardPageProps {
  readOnly: boolean;
  firstRun?: boolean;
  /** Called with the wizard's own persisted result instead of navigating
   *  to /sets, which does not exist yet on an unconfigured instance. */
  onFirstRunComplete?: (restartRequired: boolean) => void;
}

export function BackupSetWizardPage({ readOnly, firstRun = false, onFirstRunComplete }: BackupSetWizardPageProps) {
  const navigate = useNavigate();
  const { bridge } = usePlatform();
  const caps = bridge.capabilities();
  const api = useApi();

  const [step, setStep] = useState(1);

  // Source/authentication field values are local, in-progress wizard
  // state (nothing outside this component reads them while the wizard is
  // open) — but they must be CONTROLLED, not `defaultValue`, or they are
  // lost the moment another step's panel is shown (each step's subtree
  // unmounts while it isn't the active one) and the review step below
  // has nothing real to read back.
  const [source, setSource] = useState({
    name: "Production PostgreSQL",
    host: "prod-db-01.internal",
    port: "22",
    username: "backup-agent"
  });
  const updateSource = (field: keyof typeof source, value: string) =>
    setSource((s) => ({ ...s, [field]: value }));

  const [keySource, setKeySource] = useState<"generate" | "managed" | "import">("generate");
  const [importPasted, setImportPasted] = useState("");
  const [importedFingerprint, setImportedFingerprint] = useState<string | null>(null);
  // The backend reference the import step's fingerprint stands for
  // (issue #146): CreateBackupSetRequest.sshKeyId, never the key
  // material itself, which never lives in this component's state at
  // all past the one importSSHKey call below.
  const [importedKeyId, setImportedKeyId] = useState<string | null>(null);
  const [importing, setImporting] = useState(false);
  const [importError, setImportError] = useState<string | null>(null);

  // Discovery/storage fields a valid create-backup-set request needs
  // (issue #146): promoted from #98's `defaultValue`-only placeholders
  // to real controlled state, same reasoning as `source` above — the
  // Save buttons need to read back whatever an operator actually typed
  // here, not the built-in example text.
  const [remoteFolder, setRemoteFolder] = useState("/backups/postgresql/");
  const [includePatterns, setIncludePatterns] = useState("*.dump.zst");
  const [localDestination, setLocalDestination] = useState(() => bridgeDefaultPath(bridge.deployment.storageMount));

  // The wizard's other answers — completion method and the deletion
  // acknowledgement — are local too, same tier as `source` above: nothing
  // outside this component reads them while the wizard is open (see
  // state/wizardNodes.ts's module comment for the actual local-vs-graph
  // rule).
  const [completion, setCompletion] = useState<CompletionMethod>("completion-marker");
  const [acknowledged, setAcknowledged] = useState(false);
  // Issue #316: "pull from here, never delete here" (#282), declared at
  // save time rather than only by hand-editing config.yaml afterward.
  // Read at Review below to swap the remote-source-handling copy, and in
  // saveDisabled below to waive the deletion acknowledgement — there is
  // nothing to acknowledge deleting when this is checked, the same
  // reasoning "Save disabled" already gets for free by being its own
  // button (see saveDisabled's own comment).
  const [readOnlySource, setReadOnlySource] = useState(false);

  // Host trust is local too, but it needs to remember WHICH host/port it
  // was granted for, not just whether it was granted: editing the
  // hostname after trusting host A must not leave host B showing
  // "trusted" (see revalidateHostTrust below, wired to the field's
  // onBlur).
  const [hostTrusted, setHostTrusted] = useState(false);
  const [trustedHostKey, setTrustedHostKey] = useState<string | null>(null);
  // The known_hosts line CreateBackupSetRequest.knownHostsLine carries
  // once "Trust host" is actually clicked — the real trust anchor a
  // subsequent connection is checked against, not merely display text
  // (issue #146).
  const [trustedKnownHostsLine, setTrustedKnownHostsLine] = useState<string | null>(null);

  // Real host-key probe (issue #146), replacing #98's hardcoded
  // fingerprint constants: probedFor is the "host:port" the CURRENT
  // probe results are for, so the effect below re-probes automatically
  // whenever source.host/source.port changes while step 3 is open,
  // without needing revalidateHostTrust to also manage this state.
  const [probing, setProbing] = useState(false);
  const [probeError, setProbeError] = useState<string | null>(null);
  const [probedFor, setProbedFor] = useState<string | null>(null);
  const [probedFingerprint, setProbedFingerprint] = useState<string | null>(null);
  const [probedAlgorithm, setProbedAlgorithm] = useState<string | null>(null);
  const [probedKnownHostsLine, setProbedKnownHostsLine] = useState<string | null>(null);

  // wizard.hostKeyChanged lives on the shared graph — see
  // state/wizardNodes.ts for why this one node earns that spot when the
  // rest of the wizard's answers don't. Resetting it on mount means
  // opening "Add backup set" a second time never inherits a previous
  // session's stale value.
  //
  // Committed synchronously during render, not in an effect — an effect
  // runs after the first paint has already happened, so a freshly opened
  // wizard would flash whatever a previous session last left on the
  // graph before self-correcting on the next render. Same pattern (and
  // same reasoning) as PlatformContext.tsx's bridge-mounted commit.
  // Step 5's application-validator picklist (issue #162). Local
  // useState, not a graph node: nothing outside this component reads
  // either the catalog or the operator's choice while the wizard is
  // open, which is the same bar state/wizardNodes.ts holds every other
  // wizard answer to.
  //
  // validatorCatalog is null until the fetch settles, "unavailable" if
  // it failed. The distinction matters: an empty picklist and a picklist
  // that could not be loaded look identical on screen and mean opposite
  // things, and this step's whole history is a control that looked real
  // and did nothing.
  const [validatorCatalog, setValidatorCatalog] = useState<ValidatorCatalogEntry[] | null>(null);
  const [validatorCatalogFailed, setValidatorCatalogFailed] = useState(false);
  const [validatorId, setValidatorId] = useState("");

  useEffect(() => {
    let cancelled = false;
    api
      .listValidators()
      .then((catalog) => {
        if (!cancelled) setValidatorCatalog(catalog);
      })
      .catch(() => {
        if (!cancelled) setValidatorCatalogFailed(true);
      });
    return () => {
      cancelled = true;
    };
  }, [api]);

  const selectedValidator = (validatorCatalog ?? []).find((v) => v.id === validatorId);

  const [hasResetOnMount, setHasResetOnMount] = useState(false);
  if (!hasResetOnMount) {
    resetWizardAnswers();
    setHasResetOnMount(true);
  }

  const hostKeyChanged = useCausl(wizardHostKeyChangedNode);
  // §12 — the graph answers "is saving structurally possible at all"
  // (not blocked by the app-wide readOnly node (#106) or a changed host
  // key (WP 2.3's "changed host key blocks operation")); combined here
  // with the session's own acknowledgement to get the actual gate. This
  // is the only place `readOnly` is consulted for gating — the `readOnly`
  // prop below is read only to pick which hint text to show.
  const canSave = useCausl(wizardCanSaveNode);
  // M7 (#146 review): folds in the two preconditions this issue itself
  // added — an imported key and a trusted host — that handleSave already
  // refuses to save without (see its own early-return guards below).
  // Before this, only !canSave || !acknowledged gated the button, so
  // clicking Save with no key imported or no host trusted was not
  // disabled at all: it fired handleSave, which then rejected the
  // request via its own ad hoc check and a freshly-set saveError string,
  // instead of the button structurally refusing to be clicked in the
  // first place — the exact clickable-then-rejected shape this
  // safety-tool's own review flags everywhere else it appears.
  // readOnlySource waives the acknowledgement, not canSave/keySource/
  // trustedKnownHostsLine: a read-only set still needs a real, trusted
  // connection to pull backups FROM — declaring it read-only only takes
  // away the one thing there would otherwise be to acknowledge deleting.
  const saveDisabled =
    !canSave ||
    (!acknowledged && !readOnlySource) ||
    keySource !== "import" ||
    !importedKeyId ||
    !trustedKnownHostsLine;

  const trustHost = () => {
    setHostTrusted(true);
    setTrustedHostKey(source.host + ":" + source.port);
    setTrustedKnownHostsLine(probedKnownHostsLine);
    graph.commit("wizard/trustHost", (tx) => tx.set(wizardHostKeyChangedNode, false));
  };

  // M1 fix (#98 PR #145 review): a host trusted on step 3 must not still
  // read as trusted once the operator goes back and points step 1's
  // hostname/port at a different server — that would defeat the whole
  // point of a fingerprint-pinning UI. Scoped to blur (a field losing
  // focus with a changed value), not every keystroke, so retyping the
  // same hostname mid-edit doesn't re-prompt trust on every character.
  const revalidateHostTrust = () => {
    if (!hostTrusted || trustedHostKey === null) return;
    if (trustedHostKey !== source.host + ":" + source.port) {
      setHostTrusted(false);
      setTrustedHostKey(null);
      setTrustedKnownHostsLine(null);
    }
  };

  // probeHost is issue #146's real replacement for #98's hardcoded
  // TRUSTED_FINGERPRINT/CHANGED_FINGERPRINT constants: it fetches
  // host:port's actual current host key, trusting nothing itself (see
  // core's ProbeHostKey doc) — only "Trust host" (above) turns a probed
  // result into something a later connection is actually checked
  // against.
  async function probeHost() {
    const key = source.host + ":" + source.port;
    setProbing(true);
    setProbeError(null);
    // Cleared up front, not just overwritten on success: while a new
    // probe (for a just-edited host) is in flight, "Trust host" must
    // not stay enabled against the PREVIOUS host's stale fingerprint —
    // see !probedFingerprint in the button's own disabled condition
    // below.
    setProbedFingerprint(null);
    setProbedAlgorithm(null);
    setProbedKnownHostsLine(null);
    try {
      const result = await api.probeHostKey(source.host, Number(source.port) || 22);
      setProbedFingerprint(result.fingerprint);
      setProbedAlgorithm(result.algorithm);
      setProbedKnownHostsLine(result.knownHostsLine);
    } catch (e) {
      setProbeError(errorMessage(e, "Could not reach this server to fetch its host key."));
    } finally {
      setProbedFor(key);
      setProbing(false);
    }
  }

  // Probes automatically the first time step 3 is opened, and again
  // whenever source.host/source.port changes while it stays open —
  // "Re-fetch fingerprint" (below) calls probeHost() directly for an
  // explicit re-check of the SAME host.
  useEffect(() => {
    const key = source.host + ":" + source.port;
    if (step === 3 && probedFor !== key && !probing) {
      // probeHost's first statement is setProbing(true), a genuine,
      // deliberate "start loading" state update synchronous with this
      // effect running — the canonical fetch-on-mount shape, not the
      // unbounded render cascade this rule otherwise guards against
      // (there is nothing recursive here: probedFor is set at the end
      // of probeHost, which is what stops this same effect from
      // re-triggering itself once the fetch resolves).
      // eslint-disable-next-line react-hooks/set-state-in-effect
      void probeHost();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, source.host, source.port]);

  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  // The set was created and the immediate run was not started (issue
  // #194's review, M4). It is neither a save failure nor a plain success,
  // so it gets its own state rather than being folded into saveError:
  // there is nothing left to retry here, and the operator has to be told
  // that no backup is running before they walk away believing one is.
  const [runNotStarted, setRunNotStarted] = useState<string | null>(null);
  // The refusal a create over an id that already has artifacts on record
  // gets when it points somewhere other than where they came from (issue
  // #411). It carries the two Save arguments as well as the message,
  // because the way out of it is to re-send THIS save with the
  // acknowledgement, not some default one: "Save disabled" that came back
  // refused has to stay a disabled save when it is confirmed.
  const [repointRefusal, setRepointRefusal] =
    useState<{ message: string; disabled: boolean; runImmediately: boolean } | null>(null);

  // handleSave is every one of the wizard's three Save buttons' onClick
  // (issue #146): "Save, enable & run" and "Save & enable" pass
  // disabled: false, differing only in runImmediately; "Save disabled"
  // passes disabled: true. A failure surfaces inline via saveError
  // (never a silent no-op); a success navigates back to the sets list,
  // after refreshing the shared setsNode so BackupSetsPage shows the
  // new set with no manual refetch of its own (appNodes.ts's setsNode +
  // resource.ts's fetchResource, exactly as that module's own doc
  // describes for a mutation elsewhere).
  async function handleSave(disabled: boolean, runImmediately: boolean, acknowledgeRepoint = false) {
    if (keySource !== "import" || !importedKeyId) {
      setSaveError(
        keySource === "generate"
          ? "Generating a key on save isn't available yet — import a key on the Authentication step instead."
          : "Reusing a managed key on save isn't available yet — import a key on the Authentication step instead."
      );
      return;
    }
    if (!trustedKnownHostsLine) {
      setSaveError("Trust the host's fingerprint on the Verify server step before saving.");
      return;
    }

    setSaving(true);
    setSaveError(null);
    setRepointRefusal(null);
    try {
      const request = {
        name: source.name,
        host: source.host,
        port: Number(source.port) || 22,
        user: source.username,
        sshKeyId: importedKeyId,
        knownHostsLine: trustedKnownHostsLine,
        remotePath: remoteFolder,
        localPath: localDestination,
        include: includePatterns
          .split(",")
          .map((p) => p.trim())
          .filter(Boolean),
        completionStrategy: completionStrategyFor(completion),
        // Omitted, never sent as "", when no validator was chosen: the
        // backend reads an empty id as "no validator", but leaving the
        // key out says the same thing without asking it to.
        validatorId: validatorId || undefined,
        stableForSeconds: completion === "stable-size" ? 3600 : undefined,
        disabled,
        readOnly: readOnlySource,
        runImmediately: firstRun ? false : runImmediately,
        // Sent only when the operator actually answered the refusal, so
        // an ordinary save is never a pre-acknowledged one.
        acknowledgeRepoint: acknowledgeRepoint || undefined
      };
      if (firstRun) {
        const result = await api.completeFirstRun(request);
        onFirstRunComplete?.(result.restartRequired);
        return;
      }
      const created = await api.createBackupSet(request);
      fetchResource(setsNode, () => api.listSets());
      if (created.runError) {
        // Deliberately no navigate: the sets list shows the set, which is
        // exactly the "it worked" reading this response contradicts. Stay
        // here and say what did not happen, with the Save buttons off,
        // because the set already exists and pressing one again would
        // create a second.
        setRunNotStarted(created.runError);
        return;
      }
      navigate("/sets");
    } catch (e) {
      const message = errorMessage(
        e,
        firstRun ? "Could not save this configuration." : "Could not save this backup set."
      );
      if (
        e instanceof BackupManagerError &&
        e.api.code === "BACKUP_SET_HISTORY_REPOINT_NOT_ACKNOWLEDGED"
      ) {
        // Not a save error under the buttons. The service is not saying
        // anything on this form is wrong: it is saying this id already
        // has backups on record and this form would create the set
        // somewhere else, which is a decision with its own two answers.
        setRepointRefusal({ message, disabled, runImmediately });
        return;
      }
      setSaveError(message);
    } finally {
      setSaving(false);
    }
  }

  // M7 (#146 review): each new precondition gets its own hint, in the
  // same words handleSave's own early-return guards already use, rather
  // than every disabled reason falling through to the acknowledgement
  // hint below regardless of which precondition actually failed.
  let saveHint = "";
  if (hostKeyChanged) {
    saveHint = "The host key changed since it was trusted — resolve that on the Verify server step before saving.";
  } else if (keySource !== "import" || !importedKeyId) {
    saveHint =
      keySource === "generate"
        ? "Generating a key on save isn't available yet — import a key on the Authentication step instead."
        : keySource === "managed"
          ? "Reusing a managed key on save isn't available yet — import a key on the Authentication step instead."
          : "Import an SSH key on the Authentication step before saving.";
  } else if (!trustedKnownHostsLine) {
    saveHint = "Trust the host's fingerprint on the Verify server step before saving.";
  } else if (saveDisabled && !readOnly) {
    saveHint = "Acknowledge remote-source handling to enable saving.";
  } else if (saveError) {
    saveHint = saveError;
  }

  return (
    <div style={{ maxWidth: 900, width: "100%", margin: "0 auto", display: "flex", flexDirection: "column", gap: 16 }}>
      <PageHeader
        back={{ label: "Cancel and return to backup sets", onClick: () => navigate("/sets") }}
        title="Add backup set"
      />

      <ol
        style={{
          margin: 0, padding: 0, listStyle: "none",
          display: "grid", gridTemplateColumns: "repeat(6, 1fr)", gap: 8
        }}
      >
        {STEPS.map((label, i) => {
          const n = i + 1;
          const active = step === n;
          const done = step > n;
          return (
            <li key={label}>
              <button
                onClick={() => setStep(n)}
                // Without this the accessible name is "01 Authentication",
                // because the step-number span is part of the button. A
                // screen reader should hear the step, not the numeral glued
                // to the label.
                aria-label={label}
                aria-current={active ? "step" : undefined}
                style={{
                  display: "flex", flexDirection: "column", gap: 5, width: "100%",
                  padding: "9px 10px", borderRadius: "var(--radius-lg)", textAlign: "left",
                  border: "1px solid " + (active ? "var(--accent)" : "var(--border)"),
                  background: active ? "var(--accent-quiet)" : "var(--surface)",
                  color: active ? "var(--text)" : "var(--text-2)",
                  font: "inherit", cursor: "pointer"
                }}
              >
                <span className="mono" style={{ display: "flex", alignItems: "center", gap: 7, fontSize: "var(--text-xs)" }}>
                  {"0" + n}
                  <span aria-hidden="true" style={{ color: "var(--ok)", opacity: done ? 1 : 0 }}>✓</span>
                </span>
                <span style={{ fontSize: "var(--text-sm)", fontWeight: 500 }}>{label}</span>
              </button>
            </li>
          );
        })}
      </ol>

      <section className="card">
        <div style={{ padding: "20px 22px 22px" }}>
          {step === 1 ? (
            <StepBody
              title="Source"
              lede="The remote server that produces the backup artifacts. Backup Manager pulls — it is never given write access to your data."
            >
              <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(228px, 1fr))", gap: "15px 18px" }}>
                <Field
                  label="Backup set name" value={source.name} onChange={(v) => updateSource("name", v)} span
                  help={FIELD_HELP.wizardSetName}
                />
                <Field
                  label="Server hostname" value={source.host} onChange={(v) => updateSource("host", v)}
                  onBlur={revalidateHostTrust} mono help={FIELD_HELP.wizardHostname}
                />
                <Field
                  label="SSH port" value={source.port} onChange={(v) => updateSource("port", v)}
                  onBlur={revalidateHostTrust} mono help={FIELD_HELP.wizardSshPort}
                />
                <Field
                  label="Username" value={source.username} onChange={(v) => updateSource("username", v)} mono
                  help={FIELD_HELP.wizardUsername}
                />
              </div>
            </StepBody>
          ) : null}

          {step === 2 ? (
            <StepBody
              title="Authentication"
              lede="Install the public key on the remote server. Private keys stay on this NAS and are never shown after creation."
            >
              {/* One tooltip for the whole group, on the radiogroup container
                  itself: aria-describedby is valid there, and there is no
                  single control the way HelpField's .field shape assumes.
                  The copy is honest about a fact split across three radios,
                  that only one of them actually lets you finish this
                  wizard, which is exactly the kind of thing no single
                  radio's own label says. */}
              <FieldHelp label="Key source" help={FIELD_HELP.wizardKeySource}>
                {(helpId) => (
                  <div
                    role="radiogroup" aria-label="Key source" aria-describedby={helpId}
                    style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(216px, 1fr))", gap: 10 }}
                  >
                    <Choice
                      name="keysrc" title="Generate dedicated SSH key" detail="Recommended · scoped to this set only"
                      checked={keySource === "generate"} onChange={() => setKeySource("generate")}
                    />
                    <Choice
                      name="keysrc" title="Use managed key" detail="Reuse an existing Backup Manager key"
                      checked={keySource === "managed"} onChange={() => setKeySource("managed")}
                    />
                    <Choice
                      name="keysrc" title="Import key" detail="Paste once · stored encrypted, never displayed"
                      checked={keySource === "import"} onChange={() => setKeySource("import")}
                    />
                  </div>
                )}
              </FieldHelp>

              {/* Issue #299: "Generate" used to show a fixed sample public
                  key (never actually generated per set) with a "Copy
                  public key" button, plus an authorized_keys instruction
                  that always named "backup-agent" regardless of the
                  username actually entered on the Source step —
                  fabricated specifics, the same class of problem as the
                  webhook line the Settings page used to show. This path
                  is already refused at save (see handleSave/saveHint
                  below), exactly like "Use managed key", so both panels
                  now say the same honest thing instead of inventing
                  detail for a path that cannot be saved. */}
              {keySource === "generate" ? (
                <div className="banner banner--info" style={{ marginTop: 18, fontSize: "var(--text-sm)" }}>
                  <span aria-hidden="true">i</span>
                  <span>
                    Generating a key on save isn&rsquo;t available yet — import a key on the Authentication
                    step instead.
                  </span>
                </div>
              ) : null}

              {/* Issue #299: "Use managed key" used to show a picklist of
                  two hardcoded key names and a fabricated "Already
                  installed on 2 other backup sets" count — there is no
                  managed-key store behind either. Same treatment as
                  "Generate" above. */}
              {keySource === "managed" ? (
                <div className="banner banner--info" style={{ marginTop: 18, fontSize: "var(--text-sm)" }}>
                  <span aria-hidden="true">i</span>
                  <span>
                    Reusing a managed key on save isn&rsquo;t available yet — import a key on the
                    Authentication step instead.
                  </span>
                </div>
              ) : null}

              {keySource === "import" ? (
                <div style={{ marginTop: 18, display: "flex", flexDirection: "column", gap: 10 }}>
                  {importedFingerprint ? (
                    <div className="banner banner--ok" style={{ alignItems: "flex-start" }}>
                      <span aria-hidden="true" style={{ color: "var(--ok)" }}>✓</span>
                      <span style={{ flex: 1 }}>
                        <span style={{ display: "block", fontWeight: 600, fontSize: "var(--text-base)" }}>Key imported</span>
                        <span className="mono" style={{ display: "block", marginTop: 3, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                          {importedFingerprint}
                        </span>
                        <span style={{ display: "block", marginTop: 5, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                          The pasted key material has already been discarded from this screen. It cannot be shown again.
                        </span>
                      </span>
                      <button
                        className="btn btn--sm"
                        onClick={() => {
                          setImportedFingerprint(null);
                          setImportPasted("");
                        }}
                      >
                        Replace
                      </button>
                    </div>
                  ) : (
                    <>
                      <HelpField label="Private key (OpenSSH or PEM)" help={FIELD_HELP.wizardPrivateKey}>
                        {(helpId) => (
                          <textarea
                            className="input input--mono"
                            aria-describedby={helpId}
                            rows={5}
                            value={importPasted}
                            onChange={(e) => setImportPasted(e.target.value)}
                            placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
                            style={{ height: "auto", padding: "10px 11px", resize: "vertical" }}
                            // M2 (#98 PR #145 review): key material must never
                            // leave this screen unhashed, and cloud/"enhanced"
                            // spellcheck (on by default in some browsers) sends
                            // the full contents of an unmasked text field to a
                            // third-party service as the user types or pastes.
                            spellCheck={false}
                            autoComplete="off"
                            autoCorrect="off"
                            autoCapitalize="off"
                          />
                        )}
                      </HelpField>
                      <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
                        <button
                          className="btn btn--primary"
                          disabled={importPasted.trim().length === 0 || importing}
                          onClick={async () => {
                            setImporting(true);
                            setImportError(null);
                            try {
                              const result = await api.importSSHKey(importPasted);
                              setImportedFingerprint(result.algorithm + " · " + result.fingerprint);
                              setImportedKeyId(result.id);
                              setImportPasted("");
                            } catch (e) {
                              setImportError(errorMessage(e, "Could not import this key."));
                            } finally {
                              setImporting(false);
                            }
                          }}
                        >
                          {importing ? "Importing…" : "Import key"}
                        </button>
                        <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                          {importPasted.trim().length === 0
                            ? "Paste a private key to enable Import."
                            : "Nothing checks this on this screen — the backend validates it against the server on import."}
                        </span>
                      </div>
                      {importError ? (
                        <div className="banner banner--danger" style={{ fontSize: "var(--text-sm)" }}>
                          <span aria-hidden="true">!</span>
                          <span>{importError}</span>
                        </div>
                      ) : null}
                      <div className="banner banner--info" style={{ fontSize: "var(--text-sm)" }}>
                        <span aria-hidden="true">i</span>
                        <span>
                          Sent once, straight to the backend. Never written to this page&rsquo;s own logs, never
                          included in a config export, never echoed back after import.
                        </span>
                      </div>
                    </>
                  )}
                </div>
              ) : null}
            </StepBody>
          ) : null}

          {step === 3 ? (
            <StepBody
              title="Verify server"
              lede="Confirm this fingerprint through a channel other than this connection before trusting the host."
            >
              {hostKeyChanged ? (
                <div style={{ marginBottom: 16 }}>
                  <WarningBanner tone="danger" eyebrow="Host key changed">
                    The host key for {source.host || "this server"} has changed since it was trusted. This can
                    mean the server was rebuilt, or that something is intercepting the connection — verify the
                    new fingerprint independently before trusting it.
                  </WarningBanner>
                </div>
              ) : null}
              {probeError ? (
                <div style={{ marginBottom: 16 }}>
                  <WarningBanner tone="danger" eyebrow="Could not fetch the host key">
                    {probeError}
                  </WarningBanner>
                </div>
              ) : null}
              <FingerprintDisplay
                host={source.host + ":" + source.port}
                algorithm={probedAlgorithm ?? "ssh-ed25519"}
                fingerprint={probedFingerprint ?? FINGERPRINT_PLACEHOLDER}
                trustedAt={hostTrusted && !hostKeyChanged ? new Date().toISOString() : null}
              />
              <div style={{ display: "flex", gap: 10, marginTop: 16, flexWrap: "wrap", alignItems: "center" }}>
                <button
                  className={"btn " + (hostKeyChanged ? "btn--destructive-confirm" : "btn--primary")}
                  disabled={(hostTrusted && !hostKeyChanged) || probing || !probedFingerprint}
                  onClick={trustHost}
                >
                  {hostKeyChanged ? "Trust new fingerprint" : hostTrusted ? "Host trusted" : "Trust host"}
                </button>
                <button className="btn" disabled={probing} onClick={() => void probeHost()}>
                  {probing ? "Fetching…" : "Re-fetch fingerprint"}
                </button>
              </div>
              <div style={{ marginTop: 16 }}>
                <WarningBanner tone="danger" eyebrow="If this ever changes">
                  A changed host key stops all backup operations for the set and blocks
                  remote artifact deletion until an administrator verifies the new
                  fingerprint independently.
                </WarningBanner>
              </div>
            </StepBody>
          ) : null}

          {step === 4 ? (
            <StepBody
              title="Backup discovery"
              lede="Where artifacts appear, and how Backup Manager knows one is finished being written."
            >
              <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(228px, 1fr))", gap: "15px 18px" }}>
                <Field
                  label="Remote folder" value={remoteFolder} onChange={setRemoteFolder} mono span
                  help={FIELD_HELP.wizardRemoteFolder}
                />
                <Field
                  label="Include patterns" value={includePatterns} onChange={setIncludePatterns} mono
                  help={FIELD_HELP.wizardIncludePatterns}
                />
              </div>
              {/* Issue #299 (was #98's display-only placeholder before
                  that): an "Exclude patterns" field used to sit here,
                  `defaultValue`-only. Removed rather than wired — core's
                  config.BackupSet still has no exclude field, only
                  Include (core/internal/config/config.go), so there is
                  nowhere real for this to be sent. */}

              <FieldHelp label="Completion method" help={FIELD_HELP.wizardCompletionMethod}>
                {(helpId) => (
                  <fieldset aria-describedby={helpId} style={{ margin: "18px 0 0", padding: 0, border: "none" }}>
                    <legend style={{ padding: "0 0 9px", fontSize: "var(--text-sm)", fontWeight: 500, color: "var(--text-2)" }}>
                      Completion method
                    </legend>
                    <div style={{ display: "flex", flexDirection: "column", gap: 9 }}>
                      <div className="eyebrow" style={{ fontSize: "var(--text-xs)" }}>Recommended</div>
                      <Choice
                        name="cm" title="Atomic rename"
                        detail="Producer writes to a temporary name, then renames into place."
                        checked={completion === "atomic-rename"}
                        onChange={() => setCompletion("atomic-rename")}
                      />
                      <Choice
                        name="cm" title="Completion marker / manifest"
                        detail="Producer writes a sidecar manifest when the artifact is complete."
                        checked={completion === "completion-marker"}
                        onChange={() => setCompletion("completion-marker")}
                      />
                      <div className="eyebrow" style={{ fontSize: "var(--text-xs)", marginTop: 4 }}>Advanced</div>
                      <Choice
                        name="cm" title="Stable file size / timestamp"
                        detail="Use only when the producer cannot signal completion."
                        checked={completion === "stable-size"}
                        onChange={() => setCompletion("stable-size")}
                      >
                        {completion === "stable-size" ? (
                          <div style={{ marginTop: 9 }}>
                            <WarningBanner tone="warn">
                              This method infers completion and provides less assurance than
                              a producer-provided completion marker.
                            </WarningBanner>
                          </div>
                        ) : null}
                      </Choice>
                    </div>
                  </fieldset>
                )}
              </FieldHelp>
            </StepBody>
          ) : null}

          {step === 5 ? (
            <StepBody
              title="Storage and validation"
              lede="Where the NAS copy lives, and how it is proven good."
            >
              <div className="eyebrow" style={{ fontSize: "var(--text-xs)", marginBottom: 10 }}>Storage</div>
              <HelpField
                label="NAS destination" help={FIELD_HELP.wizardNasDestination}
                labelStyle={{ maxWidth: 560 }}
              >
                {(helpId) => (
                  <>
                    <span style={{ display: "flex", gap: 8 }}>
                      <input
                        className="input input--mono"
                        aria-describedby={helpId}
                        style={{ flex: 1 }}
                        value={localDestination}
                        onChange={(e) => setLocalDestination(e.target.value)}
                      />
                      {/* Do not fake a native picker the platform does not have (§22). */}
                      <button className="btn" style={{ whiteSpace: "nowrap" }}>
                        {caps.storagePicker ? "Browse volumes…" : "Validate path"}
                      </button>
                    </span>
                    <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                      {caps.storagePicker
                        ? "Uses the native " + bridge.name + " storage picker."
                        : "This platform integration has no native storage picker — enter the mounted path directly (" + bridge.deployment.storageMount + ")."}
                    </span>
                  </>
                )}
              </HelpField>

              {/* Issue #299 (per #111 before it): this step used to draw
                  its own Daily/Weekly/Monthly/Week-starts fields plus an
                  always-checked "protect newest known-good" toggle, none
                  of them wired to anything. #111 already decided GFS
                  retention is one global policy, configured once on the
                  Settings page (RetentionPolicyCard, #140), and
                  specifically warned that this wizard's own per-set shape
                  "must not be mistaken for a capability." Removed here
                  rather than reopening that decision. */}

              <div className="eyebrow" style={{ fontSize: "var(--text-xs)", margin: "20px 0 10px" }}>Validation</div>
              <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
                {/* Transfer verification is unconditional server-side —
                    there is no field anywhere that could turn it off — so
                    unlike Checksum verification below this stays, but
                    disabled: an honest status, not a control. */}
                <Toggle label="Transfer verification" note="always on" defaultChecked disabled />
                {/* Issue #299: "Checksum verification" used to sit here as
                    a live-looking, always-checked toggle. newBackupSetFor
                    (core/service/backupsets.go) sets Hash: "" on every
                    created set, unconditionally — there is no field for
                    this toggle to write to. That "" is deliberate, not a
                    gap: the recommended deployment is a chrooted,
                    forced-command internal-sftp account with no shell,
                    against which hash computation is proven not to work
                    (see backupsets.go's own comment and
                    core/tests/sftpintegration.TestSFTPHashCapability), so
                    wiring this toggle would offer a choice that fails
                    every artifact in the account shape this product
                    recommends. Removed rather than wired. */}

                {/* Issue #162: a real picklist over the backend's own
                    registered catalog (GET /api/v1/validators), replacing
                    the decorative toggle #98 shipped. The operator picks
                    an id; there is deliberately no field here, or
                    anywhere else in this app, for naming a command
                    (docs/EPIC-B-multi-nas.md §26 Step 5). */}
                <HelpField label="Application validation" help={FIELD_HELP.wizardValidatorId}>
                  {(helpId) => (
                    <>
                      <select
                        className="select"
                        aria-describedby={helpId}
                        value={validatorId}
                        disabled={validatorCatalogFailed || validatorCatalog === null}
                        onChange={(e) => setValidatorId(e.target.value)}
                      >
                        <option value="">None (transfer verification only)</option>
                        {(validatorCatalog ?? []).map((v) => (
                          <option key={v.id} value={v.id}>
                            {v.id}
                          </option>
                        ))}
                      </select>
                      {/* The option labels are the ids themselves, since an
                          id is what this actually sends and what an operator
                          will see again in config.yaml. The chosen entry's
                          own sentence goes here instead, where there is room
                          for it. */}
                      <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                        {validatorCatalogFailed
                          ? "Could not load the available validators. Save without one, or retry after reloading."
                          : validatorCatalog === null
                            ? "Loading the available validators…"
                            : (selectedValidator?.summary ??
                              "No application validator: transfer verification only.")}
                      </span>
                      <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                        A validator runs against every artifact once it is transferred. Rejecting one
                        quarantines it and leaves the remote copy in place.
                      </span>
                    </>
                  )}
                </HelpField>
              </div>
            </StepBody>
          ) : null}

          {step === 6 ? (
            <StepBody title="Review" lede="Confirm the configuration and the remote-source handling policy.">
              <div
                style={{
                  display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(224px, 1fr))",
                  gap: 1, background: "var(--border)", border: "1px solid var(--border)",
                  borderRadius: "var(--radius-lg)", overflow: "hidden"
                }}
              >
                <Summary label="Source" lines={[source.host, remoteFolder]} />
                <Summary label="Destination" lines={[localDestination]} />
                {/* Issue #299: this card used to show a hardcoded
                    "7 daily" / "13 weekly" / "12 monthly" that summarized
                    fields removed above — retention is one global policy
                    now (see Settings), not something this wizard's Review
                    step reports per set. "SHA-256" below is gone for the
                    same reason as the Checksum verification toggle: this
                    product never actually sets a hash algorithm. */}
                <Summary
                  label="Validation"
                  lines={[
                    "transfer verify",
                    validatorId || "no application validator",
                    completionSummaryLabel(completion)
                  ]}
                />
                <Summary
                  label="Host trust"
                  lines={[hostKeyChanged ? "Host key changed — blocked" : hostTrusted ? "Trusted" : "Not yet trusted"]}
                />
              </div>

              <div style={{ border: "1.5px solid var(--warn)", borderRadius: 9, overflow: "hidden", marginTop: 18 }}>
                <div
                  style={{
                    padding: "12px 16px", background: "var(--warn-quiet)",
                    borderBottom: "1px solid var(--warn)", display: "flex", alignItems: "center", gap: 9
                  }}
                >
                  <span aria-hidden="true" style={{ color: "var(--warn)" }}>▲</span>
                  <span className="eyebrow" style={{ fontSize: "var(--text-xs)", color: "var(--text)", fontWeight: 600 }}>
                    Remote source handling
                  </span>
                </div>
                <div style={{ padding: 16, display: "flex", flexDirection: "column", gap: 14 }}>
                  {/* Issue #316: declared here, at the point this page
                      already explains what deleting the remote source
                      means, rather than as an unexplained toggle earlier
                      in the flow. Checking it changes what the rest of
                      this box says, because there is no deletion left to
                      walk through or acknowledge once it is checked. */}
                  <label
                    style={{
                      display: "flex", gap: 10, padding: "13px 14px",
                      border: "1px solid var(--border-strong)", borderRadius: "var(--radius-lg)",
                      background: "var(--surface-2)", fontSize: 13, cursor: "pointer"
                    }}
                  >
                    <input
                      type="checkbox"
                      checked={readOnlySource}
                      onChange={(e) => setReadOnlySource(e.target.checked)}
                      style={{ marginTop: 2, accentColor: "var(--accent)" }}
                    />
                    <span>
                      This source is read-only — pull backups from here, but never delete
                      the remote original.
                    </span>
                  </label>

                  {readOnlySource ? (
                    <p style={{ margin: 0, fontSize: 13.5, maxWidth: "78ch" }}>
                      Backup Manager will keep every backup from this source's remote
                      copy for good, however completely it passes transfer, verification
                      and commit. Releasing that storage, if it is ever wanted, is a
                      decision made outside this manager.
                    </p>
                  ) : (
                    <>
                      <p style={{ margin: 0, fontSize: 13.5, maxWidth: "78ch" }}>
                        After a backup has been successfully transferred, verified, durably
                        committed to this NAS, and recorded as safe, Backup Manager will
                        delete the original backup artifact from the remote server.
                      </p>
                      <ol
                        className="mono"
                        style={{
                          margin: 0, padding: 0, listStyle: "none", display: "flex",
                          flexWrap: "wrap", alignItems: "center", gap: 8,
                          fontSize: "var(--text-xs)", color: "var(--text-2)"
                        }}
                      >
                        {["Discovered", "Transferred", "Verified", "Committed", "Safe state persisted"].map((p) => (
                          <li key={p} style={{ display: "flex", gap: 8 }}>
                            <span>{p}</span>
                            <span aria-hidden="true">→</span>
                          </li>
                        ))}
                        <li style={{ color: "var(--warn)", fontWeight: 600 }}>Remote artifact deleted</li>
                      </ol>
                      <FieldHelp label="Acknowledgement" help={FIELD_HELP.wizardAcknowledge}>
                        {(helpId) => (
                          <label
                            style={{
                              display: "flex", gap: 10, padding: "13px 14px",
                              border: "1px solid var(--border-strong)", borderRadius: "var(--radius-lg)",
                              background: "var(--surface-2)", fontSize: 13, cursor: "pointer"
                            }}
                          >
                            <input
                              type="checkbox"
                              aria-describedby={helpId}
                              checked={acknowledged}
                              onChange={(e) => setAcknowledged(e.target.checked)}
                              style={{ marginTop: 2, accentColor: "var(--accent)" }}
                            />
                            <span>
                              I understand the remote backup will be removed only after the NAS
                              copy has been safely committed.
                            </span>
                          </label>
                        )}
                      </FieldHelp>
                    </>
                  )}
                </div>
              </div>

              {runNotStarted ? (
                <div style={{ marginTop: 18 }}>
                  <WarningBanner
                    tone="warn"
                    eyebrow="Saved, but the run did not start"
                    actions={
                      <button className="btn" onClick={() => navigate("/sets")}>
                        Go to backup sets
                      </button>
                    }
                  >
                    The backup set was created and is enabled. The immediate run did not
                    start: {runNotStarted}. Nothing is backing up yet — start a run from
                    the backup sets list once that is resolved.
                  </WarningBanner>
                </div>
              ) : null}

              {repointRefusal ? (
                <div style={{ marginTop: 18 }}>
                  <WarningBanner
                    tone="warn"
                    eyebrow="This id already has backups on record"
                    actions={
                      <>
                        <button
                          className="btn btn--primary"
                          disabled={saving}
                          onClick={() =>
                            void handleSave(repointRefusal.disabled, repointRefusal.runImmediately, true)
                          }
                        >
                          Create anyway
                        </button>
                        <button className="btn" disabled={saving} onClick={() => setRepointRefusal(null)}>
                          Go back and change it
                        </button>
                      </>
                    }
                  >
                    {repointRefusal.message}
                  </WarningBanner>
                </div>
              ) : null}

              <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap", marginTop: 18 }}>
                {firstRun ? null : (
                  <button
                    className="btn btn--primary"
                    disabled={saveDisabled || saving || runNotStarted !== null}
                    onClick={() => void handleSave(false, true)}
                  >
                    {saving ? "Saving…" : "Save, enable & run"}
                  </button>
                )}
                <button
                  className={firstRun ? "btn btn--primary" : "btn"}
                  disabled={saveDisabled || saving || runNotStarted !== null}
                  onClick={() => void handleSave(false, false)}
                >
                  {saving ? "Saving…" : firstRun ? "Finish setup" : "Save & enable"}
                </button>
                <button
                  className="btn btn--quiet"
                  disabled={saving || runNotStarted !== null}
                  onClick={() => void handleSave(true, false)}
                >
                  {saving ? "Saving…" : "Save disabled"}
                </button>
                {saveHint ? (
                  <span
                    style={{
                      fontSize: "var(--text-sm)",
                      color: hostKeyChanged || saveError ? "var(--danger)" : "var(--text-3)"
                    }}
                  >
                    {saveHint}
                  </span>
                ) : null}
              </div>
            </StepBody>
          ) : null}
        </div>

        <div className="card__footer" style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 12 }}>
          <button className="btn" onClick={() => setStep(Math.max(1, step - 1))} disabled={step === 1}>Back</button>
          <span className="mono" style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
            {"Step " + step + " of 6"}
          </span>
          <button className="btn btn--primary" onClick={() => setStep(Math.min(6, step + 1))} disabled={step === 6}>
            Continue
          </button>
        </div>
      </section>
    </div>
  );
}

function bridgeDefaultPath(mount: string) {
  return mount.replace(/\/$/, "") + "/production/postgres/";
}

function StepBody({ title, lede, children }: { title: string; lede: string; children: React.ReactNode }) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 18 }}>
      <div>
        <h2>{title}</h2>
        <p style={{ margin: "5px 0 0", fontSize: 13, color: "var(--text-2)" }}>{lede}</p>
      </div>
      <div>{children}</div>
    </div>
  );
}

/**
 * One labelled text input, controlled or (for the wizard's own still-
 * decorative fields, see fieldHelpCopy.ts) `defaultValue`. `help` is
 * optional and deliberately so: passing it is what turns this into an
 * explained field (#278); the decorative call sites below never pass it,
 * so they keep rendering exactly as before, with no pop-up and no
 * aria-describedby for a claim this file can't stand behind.
 *
 * `help`'s presence, not a separate boolean, decides which shape renders.
 * When it's set, `style` (the grid-span object `span` builds) is forwarded
 * to HelpField's own `style`, not to the `<label>` inside it: `gridColumn`
 * only affects a DIRECT grid child, and once HelpField wraps the label,
 * its own outer div is that direct child instead.
 */
function Field(
  props:
    | {
        label: string; value: string; onChange: (v: string) => void; onBlur?: () => void;
        mono?: boolean; span?: boolean; help?: FieldHelpCopy; defaultValue?: undefined;
      }
    | {
        label: string; defaultValue: string; mono?: boolean; span?: boolean; help?: FieldHelpCopy;
        value?: undefined; onChange?: undefined; onBlur?: undefined;
      }
) {
  const { label, mono, span, help } = props;
  const style = span ? { gridColumn: "1 / -1", maxWidth: 420 } : undefined;
  const className = "input" + (mono ? " input--mono" : "");
  const input = (helpId?: string) =>
    "onChange" in props && props.onChange ? (
      <input
        className={className}
        aria-describedby={helpId}
        value={props.value}
        onChange={(e) => props.onChange(e.target.value)}
        onBlur={props.onBlur}
      />
    ) : (
      <input className={className} aria-describedby={helpId} defaultValue={props.defaultValue} />
    );

  if (help) {
    return (
      <FieldHelp label={label} help={help} style={style}>
        {(helpId) => (
          <label className="field">
            <span className="field__label">{label}</span>
            {input(helpId)}
          </label>
        )}
      </FieldHelp>
    );
  }

  return (
    <label className="field" style={style}>
      <span className="field__label">{label}</span>
      {input()}
    </label>
  );
}

function Choice({
  name, title, detail, defaultChecked, checked, onChange, children
}: {
  name: string; title: string; detail: string;
  defaultChecked?: boolean; checked?: boolean; onChange?(): void;
  children?: React.ReactNode;
}) {
  const selected = checked ?? defaultChecked ?? false;
  return (
    <label
      style={{
        display: "flex", gap: 10, padding: "13px 14px",
        border: (selected ? "1.5px solid var(--accent)" : "1px solid var(--border-strong)"),
        borderRadius: "var(--radius-lg)",
        background: selected ? "var(--accent-quiet)" : "var(--surface-2)",
        cursor: "pointer"
      }}
    >
      <input
        type="radio" name={name}
        checked={checked} defaultChecked={checked === undefined ? defaultChecked : undefined}
        onChange={onChange}
        style={{ marginTop: 2, accentColor: "var(--accent)" }}
      />
      <span style={{ flex: 1 }}>
        <span style={{ display: "block", fontSize: 13, fontWeight: 600 }}>{title}</span>
        <span style={{ display: "block", marginTop: 3, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
          {detail}
        </span>
        {children}
      </span>
    </label>
  );
}

function Toggle({
  label, note, defaultChecked, mono, disabled
}: {
  label: string; note: string; defaultChecked?: boolean; mono?: boolean; disabled?: boolean;
}) {
  return (
    <label
      style={{
        display: "flex", alignItems: "center", gap: 10, padding: "11px 13px",
        border: "1px solid var(--border)", borderRadius: 7,
        background: "var(--surface-2)", fontSize: 13, cursor: disabled ? "default" : "pointer"
      }}
    >
      <input
        type="checkbox" defaultChecked={defaultChecked} disabled={disabled}
        style={{ accentColor: "var(--accent)" }}
      />
      <span style={{ flex: 1 }}>{label}</span>
      <span
        className={mono ? "mono" : undefined}
        style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}
      >
        {note}
      </span>
    </label>
  );
}

function Summary({ label, lines }: { label: string; lines: string[] }) {
  return (
    <div style={{ background: "var(--surface)", padding: "14px 16px" }}>
      <div className="eyebrow" style={{ fontSize: "var(--text-xs)" }}>{label}</div>
      <div className="mono" style={{ marginTop: 7, fontSize: "var(--text-sm)", lineHeight: 1.7 }}>
        {lines.map((l) => <div key={l}>{l}</div>)}
      </div>
    </div>
  );
}
