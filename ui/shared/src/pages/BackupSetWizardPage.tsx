import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { usePlatform } from "@shared/platform/PlatformContext";
import { useApi } from "@shared/api/ApiContext";
import { BackupManagerError } from "@shared/api/contracts";
import { PageHeader } from "@shared/components/PageHeader";
import { WarningBanner } from "@shared/components/WarningBanner";
import { FingerprintDisplay } from "@shared/components/FingerprintDisplay";
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
  "Storage & retention",
  "Review"
] as const;

const PUBLIC_KEY =
  "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIL4pQ7mXvR2tYc8nJ0dKeW1sBfHgZaTqOo9UiKrEu backup-manager@nas-01";

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

export function BackupSetWizardPage({ readOnly }: { readOnly: boolean }) {
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
  const saveDisabled = !canSave || !acknowledged;

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

  // handleSave is every one of the wizard's three Save buttons' onClick
  // (issue #146): "Save, enable & run" and "Save & enable" pass
  // disabled: false, differing only in runImmediately; "Save disabled"
  // passes disabled: true. A failure surfaces inline via saveError
  // (never a silent no-op); a success navigates back to the sets list,
  // after refreshing the shared setsNode so BackupSetsPage shows the
  // new set with no manual refetch of its own (appNodes.ts's setsNode +
  // resource.ts's fetchResource, exactly as that module's own doc
  // describes for a mutation elsewhere).
  async function handleSave(disabled: boolean, runImmediately: boolean) {
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
    try {
      await api.createBackupSet({
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
        stableForSeconds: completion === "stable-size" ? 3600 : undefined,
        disabled,
        runImmediately
      });
      fetchResource(setsNode, () => api.listSets());
      navigate("/sets");
    } catch (e) {
      setSaveError(errorMessage(e, "Could not save this backup set."));
    } finally {
      setSaving(false);
    }
  }

  let saveHint = "";
  if (hostKeyChanged) {
    saveHint = "The host key changed since it was trusted — resolve that on the Verify server step before saving.";
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
                <Field label="Backup set name" value={source.name} onChange={(v) => updateSource("name", v)} span />
                <Field
                  label="Server hostname" value={source.host} onChange={(v) => updateSource("host", v)}
                  onBlur={revalidateHostTrust} mono
                />
                <Field
                  label="SSH port" value={source.port} onChange={(v) => updateSource("port", v)}
                  onBlur={revalidateHostTrust} mono
                />
                <Field label="Username" value={source.username} onChange={(v) => updateSource("username", v)} mono />
              </div>
            </StepBody>
          ) : null}

          {step === 2 ? (
            <StepBody
              title="Authentication"
              lede="Install the public key on the remote server. Private keys stay on this NAS and are never shown after creation."
            >
              <div role="radiogroup" aria-label="Key source" style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(216px, 1fr))", gap: 10 }}>
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

              {keySource === "generate" ? (
                <div style={{ border: "1px solid var(--border)", borderRadius: "var(--radius-lg)", overflow: "hidden", marginTop: 18 }}>
                  <div className="card__header" style={{ background: "var(--surface-2)" }}>
                    <span className="eyebrow" style={{ fontSize: "var(--text-xs)" }}>Public key · ed25519</span>
                    <button className="btn btn--sm" onClick={() => navigator.clipboard?.writeText(PUBLIC_KEY)}>
                      Copy public key
                    </button>
                  </div>
                  <div className="mono" style={{ padding: "13px 14px", fontSize: "var(--text-sm)", lineHeight: 1.6, wordBreak: "break-all" }}>
                    {PUBLIC_KEY}
                  </div>
                  <p style={{ margin: 0, padding: "0 14px 13px", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                    Add to <span className="mono">/home/backup-agent/.ssh/authorized_keys</span> on the remote server.
                  </p>
                </div>
              ) : null}

              {keySource === "managed" ? (
                <div style={{ marginTop: 18, display: "flex", flexDirection: "column", gap: 10 }}>
                  <label className="field">
                    <span className="field__label">Managed key</span>
                    <select className="select">
                      <option>nas-01-postgres · ed25519 · SHA256:9kQ2m…</option>
                      <option>nas-01-billing · ed25519 · SHA256:7bTmQ…</option>
                    </select>
                  </label>
                  <div className="banner banner--info" style={{ fontSize: "var(--text-sm)" }}>
                    <span aria-hidden="true">i</span>
                    <span>
                      Already installed on 2 other backup sets. A key in use can&rsquo;t be deleted from
                      Settings until every set referencing it is reassigned or disabled.
                    </span>
                  </div>
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
                      <label className="field">
                        <span className="field__label">Private key (OpenSSH or PEM)</span>
                        <textarea
                          className="input input--mono"
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
                      </label>
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
                <Field label="Remote folder" value={remoteFolder} onChange={setRemoteFolder} mono span />
                <Field label="Include patterns" value={includePatterns} onChange={setIncludePatterns} mono />
                {/* Exclude patterns stays display-only (#98): core's
                    config.BackupSet has no exclude field yet (only
                    Include, see core/internal/config/config.go), so
                    there is nowhere real for this to be sent. */}
                <Field label="Exclude patterns" defaultValue="*.tmp, *.part" mono />
              </div>

              <fieldset style={{ margin: "18px 0 0", padding: 0, border: "none" }}>
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
            </StepBody>
          ) : null}

          {step === 5 ? (
            <StepBody
              title="Storage, retention and validation"
              lede="Where the NAS copy lives, how long it is kept, and how it is proven good."
            >
              <div className="eyebrow" style={{ fontSize: "var(--text-xs)", marginBottom: 10 }}>Storage</div>
              <label className="field" style={{ maxWidth: 560 }}>
                <span className="field__label">NAS destination</span>
                <span style={{ display: "flex", gap: 8 }}>
                  <input
                    className="input input--mono"
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
              </label>

              <div className="eyebrow" style={{ fontSize: "var(--text-xs)", margin: "20px 0 10px" }}>Retention</div>
              <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(164px, 1fr))", gap: 14 }}>
                <Field label="Daily" defaultValue="7 days" mono />
                <Field label="Weekly" defaultValue="3 months" mono />
                <Field label="Monthly" defaultValue="12 months" mono />
                <label className="field">
                  <span className="field__label">Week starts</span>
                  <select className="select" defaultValue="Monday">
                    <option>Monday</option>
                    <option>Sunday</option>
                  </select>
                </label>
              </div>
              <label className="banner banner--ok" style={{ marginTop: 12, alignItems: "center", fontSize: "var(--text-sm)", cursor: "pointer" }}>
                <input type="checkbox" defaultChecked style={{ accentColor: "var(--ok)" }} />
                <span>Protect newest known-good backup — never deleted by retention</span>
              </label>

              <div className="eyebrow" style={{ fontSize: "var(--text-xs)", margin: "20px 0 10px" }}>Validation</div>
              <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
                <Toggle label="Transfer verification" note="always on" defaultChecked />
                <Toggle label="Checksum verification" note="SHA-256" defaultChecked mono />
                <Toggle label="Application validation" note="pg_restore --list" />
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
                <Summary label="Retention" lines={["7 daily", "13 weekly", "12 monthly"]} />
                <Summary label="Validation" lines={["SHA-256", "transfer verify", completionSummaryLabel(completion)]} />
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
                  <label
                    style={{
                      display: "flex", gap: 10, padding: "13px 14px",
                      border: "1px solid var(--border-strong)", borderRadius: "var(--radius-lg)",
                      background: "var(--surface-2)", fontSize: 13, cursor: "pointer"
                    }}
                  >
                    <input
                      type="checkbox"
                      checked={acknowledged}
                      onChange={(e) => setAcknowledged(e.target.checked)}
                      style={{ marginTop: 2, accentColor: "var(--accent)" }}
                    />
                    <span>
                      I understand the remote backup will be removed only after the NAS
                      copy has been safely committed.
                    </span>
                  </label>
                </div>
              </div>

              <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap", marginTop: 18 }}>
                <button
                  className="btn btn--primary"
                  disabled={saveDisabled || saving}
                  onClick={() => void handleSave(false, true)}
                >
                  {saving ? "Saving…" : "Save, enable & run"}
                </button>
                <button
                  className="btn"
                  disabled={saveDisabled || saving}
                  onClick={() => void handleSave(false, false)}
                >
                  {saving ? "Saving…" : "Save & enable"}
                </button>
                <button className="btn btn--quiet" disabled={saving} onClick={() => void handleSave(true, false)}>
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

function Field(
  props:
    | {
        label: string; value: string; onChange: (v: string) => void; onBlur?: () => void;
        mono?: boolean; span?: boolean; defaultValue?: undefined;
      }
    | { label: string; defaultValue: string; mono?: boolean; span?: boolean; value?: undefined; onChange?: undefined; onBlur?: undefined }
) {
  const { label, mono, span } = props;
  const style = span ? { gridColumn: "1 / -1", maxWidth: 420 } : undefined;
  const className = "input" + (mono ? " input--mono" : "");
  return (
    <label className="field" style={style}>
      <span className="field__label">{label}</span>
      {"onChange" in props && props.onChange ? (
        <input className={className} value={props.value} onChange={(e) => props.onChange(e.target.value)} onBlur={props.onBlur} />
      ) : (
        <input className={className} defaultValue={props.defaultValue} />
      )}
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

function Toggle({ label, note, defaultChecked, mono }: { label: string; note: string; defaultChecked?: boolean; mono?: boolean }) {
  return (
    <label
      style={{
        display: "flex", alignItems: "center", gap: 10, padding: "11px 13px",
        border: "1px solid var(--border)", borderRadius: 7,
        background: "var(--surface-2)", fontSize: 13, cursor: "pointer"
      }}
    >
      <input type="checkbox" defaultChecked={defaultChecked} style={{ accentColor: "var(--accent)" }} />
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
