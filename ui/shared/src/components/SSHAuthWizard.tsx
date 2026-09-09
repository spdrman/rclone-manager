import { useEffect, useMemo, useState } from "react";
import type { CSSProperties, ReactNode } from "react";
import { useApi } from "@shared/api/ApiContext";
import { apiErrorOf, describeFailure } from "@shared/api/failure";
import type {
  ConnectionTestOutcome,
  ConnectionCheck,
  SSHKeyCandidate,
  SSHKeyDiscovery,
  SSHKeyListing
} from "@shared/api/contracts";
import type { BackupSet } from "@shared/types/backup";

/**
 * Issue #592: replacing a backup set's SSH authentication, guided.
 *
 * # What this replaces
 *
 * Two text boxes, both write-only. The SSH key box asked an operator to
 * "paste the id of an imported key", and a key id is a uuid the product
 * showed exactly once, in the response to the import that created it, so
 * the box was a blank you filled in with something you would have had to
 * write down months ago. The trusted host key box asked for a
 * known_hosts line, which is the same problem one layer down.
 *
 * The goal this is measured against is the owner's: make it very easy for
 * a brand new user to get rclone authenticated with their SSH server. So
 * nothing here asks anybody to paste a key FIRST. Pasting is the last
 * option on step 1, not the only one, and on a default install the first
 * option is already the right one: the installer generates
 * <prefix>/secrets/id_ed25519 and compose mounts it read-only, so there
 * is exactly one key sitting on the machine and this is the surface that
 * finally offers it.
 *
 * # Why step 2 goes through the refusal rather than around it
 *
 * A wizard is exactly the shape of thing that quietly acquires a
 * trust-on-first-use default in the name of being friendly. Doing that
 * here would undo #572: core/service/backupsethostkey.go compares what
 * this set's own known_hosts pins against the line being offered and
 * refuses when they differ, naming both fingerprints, because those two
 * strings are the entire content of the decision.
 *
 * This wizard adds nothing beside that. It probes, shows the comparison,
 * and the ONLY way past a mismatch is the operator ticking the
 * acknowledgement, which travels as acknowledgeHostKeyChange on the final
 * patch. The acknowledgement is scoped to the fingerprint that was shown:
 * `ackedFingerprint` records which one was ticked, and a probe that later
 * returns something else clears it, so ticking never grants "trust
 * whatever answers next".
 *
 * A wizard that trusted whatever answered would not be easier. It would
 * be a wizard that cannot tell an operator's rebuilt VPS from somebody
 * else's server.
 *
 * # Why step 3 has four results and not one
 *
 * "It connected" is the answer that hides the three failures an operator
 * actually hits. Failing to authenticate with the host key green means
 * the public half is not in the remote authorized_keys, and the fix is
 * one line to paste. Failing to LIST with authentication green means the
 * account works and cannot read the folder, which is a permissions
 * problem on the source and nothing to do with keys. One red sentence
 * makes those indistinguishable, which is how somebody ends up
 * regenerating a keypair to fix a chmod.
 *
 * Apply stays unavailable until the run comes back clean, and it is
 * disarmed by any change to what was verified: `verifiedAgainst` records
 * the exact key and host line the greens were about, so editing either
 * one after a pass turns Apply off rather than letting a green stand for
 * values nobody checked.
 *
 * "Clean" is the engine's own verdict and never a count of green rows.
 * The gate here used to be `every(step => passed)`, and a step is allowed
 * to be SKIPPED: a set whose key this deployment does not hold, or a
 * source with no host to reach, reports honest skips and could never have
 * been applied. Reading `ok` is also one fewer place that can disagree
 * with the engine about what a report means.
 *
 * # What never appears here
 *
 * A private key, in either direction. The store listing carries ids,
 * fingerprints and public halves and no paths; the candidate scan carries
 * paths because a candidate's path is its identity to an operator, and
 * the handle sent back is opaque. Selecting a candidate imports it
 * server-side, so key material never crosses the network at all.
 *
 * # Where the log goes
 *
 * Nowhere in here. The wizard owns no log surface: every request it makes
 * that changes something is recorded by the API's own action middleware
 * (#599) and lands in the terminal docked to the bottom of every page, so
 * a refusal that arrives after this closes is still somewhere an operator
 * can read it.
 */
export function SSHAuthWizard({
  set,
  open,
  onCancel,
  onApplied
}: {
  set: BackupSet;
  open: boolean;
  onCancel(): void;
  /** Called with the set as the server reports it after the patch. */
  onApplied(updated: BackupSet): void;
}) {
  // No hooks before this, which is what lets it return early. The wizard
  // is MOUNTED per opening, so a second opening starts from step 1 with
  // nothing selected and no stale probe by construction, rather than
  // through an effect that resets it one painted frame later.
  if (!open) return null;
  return <OpenWizard set={set} onCancel={onCancel} onApplied={onApplied} />;
}

type StepIndex = 0 | 1 | 2 | 3;

const STEP_TITLES = ["Method", "Server identity", "Verify", "Apply"];

/** What the operator has chosen on step 1. `keyId` is what the patch
 *  carries; everything else is what the review pane names it by. */
interface ChosenKey {
  keyId: string;
  algorithm: string;
  fingerprint: string;
  publicKey: string;
  origin: string;
}

interface ProbedHostKey {
  algorithm: string;
  fingerprint: string;
  knownHostsLine: string;
}

function OpenWizard({
  set,
  onCancel,
  onApplied
}: {
  set: BackupSet;
  onCancel(): void;
  onApplied(updated: BackupSet): void;
}) {
  const api = useApi();
  const [step, setStep] = useState<StepIndex>(0);

  const [keys, setKeys] = useState<SSHKeyListing[] | null>(null);
  const [scan, setScan] = useState<SSHKeyDiscovery | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const [chosen, setChosen] = useState<ChosenKey | null>(null);
  const [pastedKey, setPastedKey] = useState("");
  const [pastedPassphrase, setPastedPassphrase] = useState("");

  const [probed, setProbed] = useState<ProbedHostKey | null>(null);
  // The fingerprint the operator ticked, not a bare boolean. A boolean
  // would survive a re-probe that returned a DIFFERENT key, which is
  // exactly the "trust whatever answers next" this must not become.
  const [ackedFingerprint, setAckedFingerprint] = useState<string | null>(null);

  const [outcome, setOutcome] = useState<ConnectionTestOutcome | null>(null);
  // What the four greens were about. Apply reads this rather than the
  // current selection, so changing either value after a pass disarms it.
  const [verifiedAgainst, setVerifiedAgainst] = useState<string | null>(null);

  // A counter rather than a callback the effect calls. An import needs
  // the two lists re-read, and the obvious way to write that is a `load`
  // function invoked from both places, which puts a synchronous setState
  // inside the effect body and cascades a render before the first request
  // has started. Bumping this instead keeps every setState inside a
  // promise callback, which is the shape BackupSetWizardPage's own
  // catalog fetch already uses.
  const [reloadToken, setReloadToken] = useState(0);

  useEffect(() => {
    let cancelled = false;
    Promise.all([api.listSSHKeys(), api.listSSHKeyCandidates()])
      .then(([listed, found]) => {
        if (cancelled) return;
        setKeys(listed);
        setScan(found);
        // Cleared beside the values that replace it, never on the way in.
        // Blanking the reason before a retry leaves a wizard that says
        // nothing at all while the retry is in flight, and says nothing
        // again if it fails the same way.
        setLoadError(null);
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        // A failed listing is reported and the wizard stays on step 1
        // with nothing selected. It is never silently turned into an
        // empty list, which would read as "you have no keys" and is the
        // exact sentence this feature exists to stop the product saying.
        setLoadError(describeFailure(e, "The key store and the key scan could not be read.").message);
      });
    return () => {
      cancelled = true;
    };
  }, [api, reloadToken]);

  const onRecord = set.trustedHostKeys;
  const hostAddress = set.host + ":" + set.port;

  /** Whether the probed key is one this set already trusts. A match moves
   *  on with nothing to answer; anything else is the decision #572 built. */
  const probeMatches = useMemo(() => {
    if (!probed) return false;
    return onRecord.some((k) => k.fingerprint === probed.fingerprint);
  }, [probed, onRecord]);

  /** The set trusts nothing this deployment could read, which is a THIRD
   *  state and not a match. It is what a set pointing at an unreadable or
   *  hand-maintained known_hosts reports, and calling it a match would be
   *  treating "I could not check" as "it is fine". */
  const nothingOnRecord = onRecord.length === 0;

  const acknowledging =
    probed !== null && !probeMatches && ackedFingerprint !== null && ackedFingerprint === probed.fingerprint;
  const hostSettled = probed !== null && (probeMatches || acknowledging);

  const verificationSignature = chosen && probed ? chosen.keyId + " " + probed.knownHostsLine : null;
  const staleResults = outcome !== null && verifiedAgainst !== verificationSignature;
  /** Apply is armed by the ENGINE's own verdict, not by re-deriving one
   *  from the rows.
   *
   *  `outcome.ok` is sourcecheck.Report.OK, which is "no step failed".
   *  The old gate here was `every(step => passed)`, and that breaks the
   *  moment a step is legitimately skipped: a set whose key this
   *  deployment does not hold, or a local source with no host to reach,
   *  reports honest skips and would never have been appliable. Two
   *  places deciding the same thing is also how they end up disagreeing,
   *  and the one that would have won here is the one in the browser. */
  const allPassed = outcome !== null && outcome.ok && !staleResults;

  const selectStoredKey = (k: SSHKeyListing) => {
    setChosen({
      keyId: k.id,
      algorithm: k.algorithm,
      fingerprint: k.fingerprint,
      publicKey: k.publicKey,
      origin: "the key store"
    });
    setOutcome(null);
    setVerifiedAgainst(null);
  };

  const selectCandidate = async (c: SSHKeyCandidate) => {
    setError(null);
    setBusy("Importing " + c.path);
    try {
      // Server-side. The browser sends the opaque candidate id and gets
      // back an id and a fingerprint; the private half is read once, on
      // the machine that already holds it, and the original is left
      // alone. That is the same promise --ssh-key-file makes, and the
      // wizard must not answer it differently from the CLI.
      const ref = await api.importSSHKeyCandidate(c.id);
      setChosen({
        keyId: ref.id,
        algorithm: ref.algorithm,
        fingerprint: ref.fingerprint,
        publicKey: c.publicKey,
        origin: c.path
      });
      setOutcome(null);
      setVerifiedAgainst(null);
      setReloadToken((n) => n + 1);
    } catch (e) {
      setError(describeFailure(e, "That key could not be imported from where it was found.").message);
    } finally {
      setBusy(null);
    }
  };

  const importPasted = async () => {
    setError(null);
    setBusy("Importing the pasted key");
    try {
      const ref = await api.importSSHKey(pastedKey);
      setChosen({
        keyId: ref.id,
        algorithm: ref.algorithm,
        fingerprint: ref.fingerprint,
        // A pasted key's public half is not in the import response, and
        // deriving one here would mean parsing key material in a browser.
        // The listing that follows carries it, so the review pane fills
        // this in from there rather than computing it.
        publicKey: "",
        origin: "pasted just now"
      });
      // Discarded the instant the import resolves, per the step's own
      // promise. The passphrase goes with it: it belonged to material
      // this page no longer holds.
      setPastedKey("");
      setPastedPassphrase("");
      setOutcome(null);
      setVerifiedAgainst(null);
      setReloadToken((n) => n + 1);
    } catch (e) {
      setError(describeFailure(e, "That key could not be imported.").message);
    } finally {
      setBusy(null);
    }
  };

  const probe = async () => {
    setError(null);
    setBusy("Asking " + hostAddress + " for its host key");
    try {
      const result = await api.probeHostKey(set.host, set.port);
      setProbed(result);
      // A probe that returns a key other than the acknowledged one clears
      // the acknowledgement. The answer belonged to the fingerprint it
      // was given for, never to the address.
      setAckedFingerprint((current) => (current === result.fingerprint ? current : null));
      setOutcome(null);
      setVerifiedAgainst(null);
    } catch (e) {
      setError(describeFailure(e, "The server could not be asked for its host key.").message);
    } finally {
      setBusy(null);
    }
  };

  const verify = async () => {
    if (!chosen || !probed) return;
    setError(null);
    setOutcome(null);
    setBusy("Verifying");
    const signature = chosen.keyId + " " + probed.knownHostsLine;
    try {
      // Candidate mode, not backup-set mode. The set is persisted, but
      // what is being checked is a CANDIDATE: the key and host line the
      // operator has chosen and not yet saved. Re-checking the set would
      // be a green result for something they did not ask about.
      const result = await api.testCandidateConnection({
        host: set.host,
        port: set.port,
        user: set.username,
        sshKeyId: chosen.keyId,
        knownHostsLine: probed.knownHostsLine,
        remotePath: set.remoteFolder
      });
      setOutcome(result);
      setVerifiedAgainst(signature);
    } catch (e) {
      setError(describeFailure(e, "The verification could not be run.").message);
    } finally {
      setBusy(null);
    }
  };

  const apply = async () => {
    if (!chosen || !probed) return;
    setError(null);
    setBusy("Saving");
    try {
      // One patch, the same one the edit page sends, so there is one
      // write path and not two. The acknowledgement travels only when
      // step 2 actually needed it.
      const updated = await api.updateBackupSet(set.source, set.set, {
        sshKeyId: chosen.keyId,
        knownHostsLine: probed.knownHostsLine,
        ...(acknowledging ? { acknowledgeHostKeyChange: true } : {})
      });
      onApplied(updated);
    } catch (e) {
      const apiError = apiErrorOf(e);
      if (apiError?.code === "BACKUP_SET_HOST_KEY_CHANGE_NOT_ACKNOWLEDGED") {
        // The refusal #572 built, arriving here rather than being
        // pre-empted by this page's own opinion. It is shown and the
        // wizard goes BACK to step 2, because the answer to it is a
        // decision about a fingerprint rather than a retry, and that pane
        // is where the decision is made.
        setStep(1);
      }
      setError(describeFailure(e, "This backup set's authentication was not changed.").message);
    } finally {
      setBusy(null);
    }
  };

  const canLeaveStep: Record<StepIndex, boolean> = {
    0: chosen !== null,
    1: hostSettled,
    2: allPassed,
    3: false
  };

  return (
    <div style={backdrop} role="dialog" aria-modal="true" aria-label="SSH authentication">
      <div style={panel}>
        <header style={head}>
          <div>
            <p style={eyebrow}>SSH authentication</p>
            <h2 style={heading}>{"Replace the authentication on " + set.id}</h2>
          </div>
          <ol style={rail}>
            {STEP_TITLES.map((title, i) => (
              <li
                key={title}
                aria-current={i === step ? "step" : undefined}
                style={{
                  ...railStep,
                  color: i === step ? "var(--accent)" : i < step ? "var(--ok)" : "var(--text-3)",
                  fontWeight: i === step ? 600 : 400
                }}
              >
                {String(i + 1) + ". " + title}
              </li>
            ))}
          </ol>
        </header>

        <div style={body}>
          {loadError ? <Notice tone="danger">{loadError}</Notice> : null}
          {error ? <Notice tone="danger">{error}</Notice> : null}

          {step === 0 ? (
            <MethodStep
              set={set}
              keys={keys}
              scan={scan}
              chosen={chosen}
              pastedKey={pastedKey}
              pastedPassphrase={pastedPassphrase}
              onPastedKey={setPastedKey}
              onPastedPassphrase={setPastedPassphrase}
              onImportPasted={() => void importPasted()}
              onSelectStored={selectStoredKey}
              onSelectCandidate={(c) => void selectCandidate(c)}
              busy={busy}
            />
          ) : null}

          {step === 1 ? (
            <IdentityStep
              hostAddress={hostAddress}
              onRecord={onRecord}
              nothingOnRecord={nothingOnRecord}
              probed={probed}
              probeMatches={probeMatches}
              ackedFingerprint={ackedFingerprint}
              onProbe={() => void probe()}
              onAck={setAckedFingerprint}
              busy={busy}
            />
          ) : null}

          {step === 2 ? (
            <VerifyStep
              outcome={outcome}
              stale={staleResults}
              publicKey={chosen?.publicKey ?? ""}
              user={set.username}
              host={set.host}
              onVerify={() => void verify()}
              busy={busy}
            />
          ) : null}

          {step === 3 && chosen && probed ? (
            <ApplyStep
              set={set}
              chosen={chosen}
              probed={probed}
              onRecord={onRecord}
              acknowledging={acknowledging}
            />
          ) : null}
        </div>

        <footer style={foot}>
          <button type="button" onClick={onCancel} style={btn} disabled={busy !== null}>
            {step === 3 ? "Cancel" : "Close"}
          </button>
          <span style={{ flex: 1 }} />
          {busy ? <span style={hint}>{busy}</span> : null}
          {step > 0 ? (
            <button
              type="button"
              onClick={() => setStep((s) => (s - 1) as StepIndex)}
              style={btn}
              disabled={busy !== null}
            >
              Back
            </button>
          ) : null}
          {step < 3 ? (
            <button
              type="button"
              onClick={() => setStep((s) => (s + 1) as StepIndex)}
              style={{ ...btn, ...btnPrimary }}
              disabled={busy !== null || !canLeaveStep[step]}
            >
              {"Next: " + STEP_TITLES[step + 1].toLowerCase()}
            </button>
          ) : (
            <button
              type="button"
              onClick={() => void apply()}
              style={{ ...btn, ...btnPrimary }}
              disabled={busy !== null || !allPassed}
            >
              Apply and close
            </button>
          )}
        </footer>
      </div>
    </div>
  );
}

function MethodStep({
  set,
  keys,
  scan,
  chosen,
  pastedKey,
  pastedPassphrase,
  onPastedKey,
  onPastedPassphrase,
  onImportPasted,
  onSelectStored,
  onSelectCandidate,
  busy
}: {
  set: BackupSet;
  keys: SSHKeyListing[] | null;
  scan: SSHKeyDiscovery | null;
  chosen: ChosenKey | null;
  pastedKey: string;
  pastedPassphrase: string;
  onPastedKey(next: string): void;
  onPastedPassphrase(next: string): void;
  onImportPasted(): void;
  onSelectStored(k: SSHKeyListing): void;
  onSelectCandidate(c: SSHKeyCandidate): void;
  busy: string | null;
}) {
  return (
    <>
      <p style={lede}>
        {"How should " +
          set.id +
          " authenticate to " +
          set.host +
          "? Nothing changes until the last step, and the method this set uses now keeps working until then."}
      </p>

      <Group title="Keys this deployment already holds">
        {keys === null ? (
          <p style={quiet}>Reading the key store...</p>
        ) : keys.length === 0 ? (
          <p style={quiet}>
            This deployment has imported no keys yet. The two groups below are where a
            first one comes from.
          </p>
        ) : (
          keys.map((k) => (
            <Option
              key={k.id}
              selected={chosen?.keyId === k.id}
              disabled={busy !== null || k.passphraseProtected || k.fingerprint === ""}
              onSelect={() => onSelectStored(k)}
              title={(k.algorithm || "key") + (k.importedAt ? " imported " + k.importedAt.slice(0, 10) : "")}
              fingerprint={k.fingerprint}
              tags={[
                "KEY STORE",
                ...(k.passphraseProtected ? ["PASSPHRASE-PROTECTED"] : []),
                ...(k.usedBy.length > 0
                  ? ["USED BY " + k.usedBy.length + " SET" + (k.usedBy.length === 1 ? "" : "S")]
                  : [])
              ]}
              note={
                k.problem
                  ? k.problem
                  : k.passphraseProtected
                    ? "Needs its passphrase to be resolvable before it can be verified."
                    : k.usedBy.length > 0
                      ? "Currently used by " + k.usedBy.join(", ")
                      : "Not used by any backup set"
              }
            />
          ))
        )}
      </Group>

      <Group title="Keys found on this machine">
        {/* The locations come first and are never conditional on there
            being candidates. An empty list has two readings, "you have no
            keys" and "I could not look where your keys are", and on a
            packaged install the engine is a distroless container with
            five mounts and no home directory, so the second is the true
            one. */}
        {scan === null ? (
          <p style={quiet}>Scanning...</p>
        ) : (
          <>
            <p style={quiet}>
              {"Searched " +
                scan.locations.length +
                " location" +
                (scan.locations.length === 1 ? "" : "s") +
                " this engine can actually reach:"}
            </p>
            <ul style={locationList}>
              {scan.locations.map((l) => (
                <li key={l.path} style={locationRow}>
                  <code style={mono}>{l.path}</code>
                  <span style={quietInline}>
                    {l.problem ? ", " + l.problem : ", " + l.found + " key" + (l.found === 1 ? "" : "s")}
                  </span>
                </li>
              ))}
            </ul>
            {scan.candidates.length === 0 ? (
              <p style={quiet}>
                Nothing in those locations. On a packaged install this engine is a
                container with a fixed set of mounts, so a key in your own home directory
                on the NAS is not visible to it unless that directory is mounted.
              </p>
            ) : (
              scan.candidates.map((c) => (
                <Option
                  key={c.id}
                  selected={chosen?.origin === c.path}
                  disabled={busy !== null || !c.selectable}
                  onSelect={() => onSelectCandidate(c)}
                  title={c.path}
                  fingerprint={c.fingerprint}
                  tags={["ON THIS MACHINE", ...(c.mode ? ["MODE " + c.mode] : [])]}
                  note={
                    c.reason
                      ? c.reason
                      : "Selecting this reads the key once on the NAS and copies it into the key store. The original is left alone."
                  }
                />
              ))
            )}
          </>
        )}
      </Group>

      <Group title="Other methods this product supports">
        <p style={quiet}>
          A key resolved by a command or an environment variable (for a secrets manager
          such as OpenBao, Vault, SOPS or 1Password) is configured in this deployment&rsquo;s
          own config.yaml under <code style={mono}>key.command</code> or{" "}
          <code style={mono}>key.env</code>, and is not something this wizard writes. There
          is no password and no ssh-agent option here because the transport cannot use
          either: it never sets <code style={mono}>pass</code>,{" "}
          <code style={mono}>ask_password</code> or <code style={mono}>key_use_agent</code>.
        </p>
        <label style={label}>
          Import a new private key
          <textarea
            value={pastedKey}
            onChange={(e) => onPastedKey(e.target.value)}
            rows={4}
            spellCheck={false}
            placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
            style={textarea}
          />
        </label>
        <label style={label}>
          Passphrase, if this key has one
          <input
            type="password"
            value={pastedPassphrase}
            onChange={(e) => onPastedPassphrase(e.target.value)}
            style={input}
          />
        </label>
        <p style={quiet}>
          Sent once, validated, written into the key store with 0600 permissions, and never
          displayed again. This page discards its own copy the instant the import returns.
        </p>
        <button
          type="button"
          onClick={onImportPasted}
          style={btn}
          disabled={busy !== null || pastedKey.trim() === ""}
        >
          Import and use this key
        </button>
      </Group>

      {chosen ? (
        <Notice tone="ok">
          {"Selected " +
            (chosen.algorithm || "key") +
            " " +
            chosen.fingerprint +
            " from " +
            chosen.origin +
            ". Nothing is written yet."}
        </Notice>
      ) : null}
    </>
  );
}

function IdentityStep({
  hostAddress,
  onRecord,
  nothingOnRecord,
  probed,
  probeMatches,
  ackedFingerprint,
  onProbe,
  onAck,
  busy
}: {
  hostAddress: string;
  onRecord: BackupSet["trustedHostKeys"];
  nothingOnRecord: boolean;
  probed: ProbedHostKey | null;
  probeMatches: boolean;
  ackedFingerprint: string | null;
  onProbe(): void;
  onAck(fingerprint: string | null): void;
  busy: string | null;
}) {
  return (
    <>
      <p style={lede}>
        {"This asks " +
          hostAddress +
          " what host key it offers right now and compares it against what this backup set already trusts. It trusts nothing on your behalf: a key that does not match is the same refusal the edit page raises, and the only way past it is your answer."}
      </p>

      <div style={compare}>
        <div style={compareBox}>
          <p style={compareKey}>On record for this set</p>
          {nothingOnRecord ? (
            <p style={quiet}>
              This deployment could not report what this set trusts. That covers an anchor
              it cannot read and a file that pins nothing for this address, and it is not
              the same answer as &ldquo;nothing is trusted&rdquo;.
            </p>
          ) : (
            onRecord.map((k) => (
              <p key={k.fingerprint} style={mono}>
                {k.algorithm + " " + k.fingerprint}
              </p>
            ))
          )}
        </div>
        <div style={compareBox}>
          <p style={compareKey}>Offered just now</p>
          {probed ? (
            <p style={mono}>{probed.algorithm + " " + probed.fingerprint}</p>
          ) : (
            <p style={quiet}>Not asked yet.</p>
          )}
        </div>
      </div>

      <button type="button" onClick={onProbe} style={btn} disabled={busy !== null}>
        {probed ? "Ask again" : "Ask the server"}
      </button>

      {probed && probeMatches ? (
        <Notice tone="ok">
          {"The key " + hostAddress + " offered is one this set already trusts. Nothing to decide."}
        </Notice>
      ) : null}

      {probed && !probeMatches ? (
        <Notice tone="warn">
          <strong>A rebuilt server and a machine in the middle look identical from here.</strong>
          <p style={{ margin: "6px 0 0" }}>
            {nothingOnRecord
              ? "There is nothing on record to compare this against, so trusting it is establishing trust rather than confirming it."
              : "The key on record and the key offered are different keys, and nothing here can tell those two situations apart."}{" "}
            Check the offered fingerprint against the server itself before you answer:{" "}
            <code style={mono}>ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub</code> on the
            box prints exactly this string.
          </p>
          <label style={{ ...label, flexDirection: "row", alignItems: "flex-start", gap: 8, marginTop: 12 }}>
            <input
              type="checkbox"
              checked={ackedFingerprint === probed.fingerprint}
              onChange={(e) => onAck(e.target.checked ? probed.fingerprint : null)}
            />
            <span>
              {"I compared " +
                probed.fingerprint +
                " against the server itself and it is the right server."}
            </span>
          </label>
          <p style={{ ...quiet, marginTop: 6 }}>
            This is recorded against that fingerprint and nothing else. A different key
            offered later asks again.
          </p>
        </Notice>
      ) : null}
    </>
  );
}

function VerifyStep({
  outcome,
  stale,
  publicKey,
  user,
  host,
  onVerify,
  busy
}: {
  outcome: ConnectionTestOutcome | null;
  stale: boolean;
  publicKey: string;
  user: string;
  host: string;
  onVerify(): void;
  busy: string | null;
}) {
  const failed = outcome?.checks.find((c) => c.outcome === "failed");
  return (
    <>
      <p style={lede}>
        This runs the selected key against the settled host key and lists the folder this
        set pulls from. Nothing is written to the server and nothing is persisted here.
      </p>
      <button
        type="button"
        onClick={onVerify}
        style={{ ...btn, ...btnPrimary }}
        disabled={busy !== null}
      >
        {outcome ? "Verify again" : "Verify"}
      </button>

      {outcome === null ? null : outcome.checks.length === 0 ? (
        <Notice tone="warn">
          {"This engine reported " +
            (outcome.ok ? "success" : "a failure") +
            " without a per-step breakdown, so there are no individual results to show. " +
            (outcome.message ?? "")}
        </Notice>
      ) : (
        <ol style={checkList}>
          {outcome.checks.map((c) => (
            <li key={c.step} style={checkRow}>
              <span style={{ ...checkGlyph, color: glyphColour(c.outcome) }}>{glyph(c.outcome)}</span>
              <span style={{ flex: 1, minWidth: 0 }}>
                <span style={{ fontWeight: 600 }}>{stepTitle(c, user)}</span>
                {c.detail ? (
                  <span style={{ ...mono, display: "block", color: "var(--text-3)" }}>{c.detail}</span>
                ) : null}
              </span>
              <span style={quietInline}>{timing(c)}</span>
            </li>
          ))}
        </ol>
      )}

      {stale ? (
        <Notice tone="warn">
          These results are about a different key or host key from the one selected now.
          Verify again before applying.
        </Notice>
      ) : null}

      {failed?.step === "authenticate" && publicKey ? (
        <Notice tone="warn">
          <strong>{"This key's public half is not authorised on " + host + "."}</strong>
          <p style={{ margin: "6px 0" }}>
            {"Put this line in that account's ~/.ssh/authorized_keys on " +
              host +
              ", then verify again."}
          </p>
          <code style={{ ...mono, display: "block" }}>{publicKey}</code>
        </Notice>
      ) : null}

      {failed?.step === "list" ? (
        <Notice tone="warn">
          {user +
            " authenticated, so the key is fine. What failed is reading the folder, which is a permissions problem on the source and has nothing to do with the key."}
        </Notice>
      ) : null}

      <p style={quiet}>
        Apply stays unavailable until this run comes back clean. A green &ldquo;reached the
        server&rdquo; on its own proves a socket opened; the line that matters to a backup is
        the last one.
      </p>
    </>
  );
}

function ApplyStep({
  set,
  chosen,
  probed,
  onRecord,
  acknowledging
}: {
  set: BackupSet;
  chosen: ChosenKey;
  probed: ProbedHostKey;
  onRecord: BackupSet["trustedHostKeys"];
  acknowledging: boolean;
}) {
  return (
    <>
      <p style={lede}>
        One patch, the same one the edit page sends. The method being replaced is named
        beside the one replacing it, because &ldquo;replace the existing method&rdquo; is
        meaningless if the thing being replaced is never shown.
      </p>
      <dl style={ledger}>
        <LedgerRow
          label="Key it uses now"
          value={set.sshKeyId === "" ? "a key this deployment does not manage" : set.sshKeyId}
        />
        <LedgerRow
          label="Key it will use"
          value={(chosen.algorithm || "key") + " " + chosen.fingerprint}
        />
        <LedgerRow
          label="Host key it trusts now"
          value={
            onRecord.length === 0
              ? "not reported by this deployment"
              : onRecord.map((k) => k.algorithm + " " + k.fingerprint).join(", ")
          }
        />
        <LedgerRow
          label="Host key it will trust"
          value={probed.algorithm + " " + probed.fingerprint}
        />
        <LedgerRow
          label="Everything else"
          value="unchanged: host, port, user, folders, retention, validation"
        />
      </dl>
      {acknowledging ? (
        <Notice tone="warn">
          {"This carries your acknowledgement for " +
            probed.fingerprint +
            ", and for that fingerprint only. It grants nothing else, and a different key offered later asks again."}
        </Notice>
      ) : null}
      <Notice tone="ok">
        The private key never left this NAS and was never displayed. What is written is a
        reference to it and a public host key line.
      </Notice>
    </>
  );
}

/** One heading per step, all six of them.
 *
 *  Exhaustive over the union on purpose: a step with no case would render
 *  a blank heading beside a coloured glyph, which is a row a reader
 *  scores by its colour alone. */
function stepTitle(c: ConnectionCheck, user: string): string {
  switch (c.step) {
    case "credentials":
      return c.outcome === "failed" ? "This key cannot be used on this NAS" : "Key is usable on this NAS";
    case "resolve":
      return "Hostname resolves";
    case "connect":
      return "Reached the server";
    case "host_key":
      return "Host key matched what this set trusts";
    case "authenticate":
      return c.outcome === "failed" ? "Could not authenticate as " + user : "Authenticated as " + user;
    case "list":
      return "Listed the folder this set pulls from";
  }
}

/** The right-hand column.
 *
 *  Three states, not two. A skipped step says it was never tried; a
 *  measured one says how long it took; and a step with NO durationMs
 *  says nothing at all, because authenticate and list are decided from a
 *  single call and have no timing of their own. Printing "0 ms" there is
 *  what this used to do, and it told an operator the server answered
 *  instantly on every successful run. */
function timing(c: ConnectionCheck): string {
  if (c.outcome === "skipped") return "not attempted";
  if (c.durationMs === undefined) return "";
  return c.durationMs + " ms";
}

function glyph(outcome: ConnectionCheck["outcome"]): string {
  return outcome === "passed" ? "PASS" : outcome === "failed" ? "FAIL" : "SKIP";
}

function glyphColour(outcome: ConnectionCheck["outcome"]): string {
  return outcome === "passed" ? "var(--ok)" : outcome === "failed" ? "var(--danger)" : "var(--text-3)";
}

function Group({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section style={{ marginBottom: 18 }}>
      <h3 style={groupHead}>{title}</h3>
      <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>{children}</div>
    </section>
  );
}

function Option({
  selected,
  disabled,
  onSelect,
  title,
  fingerprint,
  tags,
  note
}: {
  selected: boolean;
  disabled: boolean;
  onSelect(): void;
  title: string;
  fingerprint: string;
  tags: string[];
  note: string;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      disabled={disabled}
      aria-pressed={selected}
      style={{
        ...option,
        borderColor: selected ? "var(--accent)" : "var(--border)",
        opacity: disabled ? 0.62 : 1,
        cursor: disabled ? "default" : "pointer"
      }}
    >
      <span style={{ fontWeight: 600, display: "block" }}>
        {title}
        {tags.map((t) => (
          <span key={t} style={chip}>
            {t}
          </span>
        ))}
      </span>
      {/* An empty fingerprint is said, never left blank. A blank under a
          confident heading is the defect this whole listing exists to
          end, and here it also means something specific: the private key
          was not read in order to invent one. */}
      <span style={{ ...mono, display: "block", color: "var(--text-2)" }}>
        {fingerprint === "" ? "no fingerprint available" : fingerprint}
      </span>
      <span style={{ ...quiet, display: "block", margin: "3px 0 0" }}>{note}</span>
    </button>
  );
}

function LedgerRow({ label, value }: { label: string; value: string }) {
  return (
    <>
      <dt style={{ color: "var(--text-2)" }}>{label}</dt>
      <dd style={{ ...mono, margin: 0 }}>{value}</dd>
    </>
  );
}

function Notice({ tone, children }: { tone: "ok" | "warn" | "danger"; children: ReactNode }) {
  const colour = tone === "ok" ? "var(--ok)" : tone === "warn" ? "var(--warn)" : "var(--danger)";
  return (
    <div
      role={tone === "danger" ? "alert" : undefined}
      style={{ ...notice, borderColor: colour, color: "var(--text)" }}
    >
      {children}
    </div>
  );
}

const backdrop: CSSProperties = {
  position: "fixed",
  inset: 0,
  background: "rgba(20,20,18,.42)",
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
  padding: 24,
  zIndex: 60
};
const panel: CSSProperties = {
  background: "var(--surface)",
  border: "1px solid var(--border)",
  borderRadius: 10,
  width: "min(880px, 100%)",
  maxHeight: "100%",
  display: "flex",
  flexDirection: "column",
  overflow: "hidden"
};
const head: CSSProperties = {
  padding: "16px 20px",
  borderBottom: "1px solid var(--border)",
  display: "flex",
  flexWrap: "wrap",
  gap: 12,
  alignItems: "baseline",
  justifyContent: "space-between"
};
const eyebrow: CSSProperties = {
  margin: 0,
  fontSize: 11,
  letterSpacing: ".06em",
  textTransform: "uppercase",
  color: "var(--text-3)"
};
const heading: CSSProperties = { margin: "3px 0 0", fontSize: 16 };
const rail: CSSProperties = { display: "flex", gap: 12, listStyle: "none", margin: 0, padding: 0, fontSize: 12 };
const railStep: CSSProperties = { whiteSpace: "nowrap" };
const body: CSSProperties = {
  padding: 20,
  overflowY: "auto",
  display: "flex",
  flexDirection: "column",
  gap: 12
};
const foot: CSSProperties = {
  padding: "12px 20px",
  borderTop: "1px solid var(--border)",
  display: "flex",
  gap: 8,
  alignItems: "center"
};
const lede: CSSProperties = { margin: 0, fontSize: 13, color: "var(--text-2)" };
const quiet: CSSProperties = { margin: 0, fontSize: 12, color: "var(--text-3)" };
const quietInline: CSSProperties = { fontSize: 12, color: "var(--text-3)" };
const groupHead: CSSProperties = {
  margin: "0 0 8px",
  fontSize: 11,
  letterSpacing: ".06em",
  textTransform: "uppercase",
  color: "var(--text-3)"
};
const option: CSSProperties = {
  textAlign: "left",
  border: "1px solid var(--border)",
  borderRadius: 8,
  padding: "10px 14px",
  background: "var(--surface)",
  font: "inherit",
  fontSize: 13
};
const chip: CSSProperties = {
  marginLeft: 8,
  fontSize: 9.5,
  fontWeight: 700,
  letterSpacing: ".05em",
  borderRadius: 4,
  padding: "1px 6px",
  background: "var(--surface-3)",
  color: "var(--text-2)"
};
const mono: CSSProperties = {
  fontFamily: "var(--font-mono, ui-monospace, monospace)",
  fontSize: 11.5,
  margin: 0,
  wordBreak: "break-all"
};
const notice: CSSProperties = { border: "1px solid", borderRadius: 8, padding: "10px 14px", fontSize: 12.5 };
const compare: CSSProperties = { display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 };
const compareBox: CSSProperties = { border: "1px solid var(--border)", borderRadius: 8, padding: "10px 14px" };
const compareKey: CSSProperties = {
  margin: "0 0 5px",
  fontSize: 10.5,
  fontWeight: 700,
  letterSpacing: ".06em",
  textTransform: "uppercase",
  color: "var(--text-3)"
};
const checkList: CSSProperties = {
  listStyle: "none",
  margin: 0,
  padding: 0,
  border: "1px solid var(--border)",
  borderRadius: 8,
  overflow: "hidden"
};
const checkRow: CSSProperties = {
  display: "flex",
  gap: 11,
  alignItems: "center",
  padding: "11px 16px",
  borderTop: "1px solid var(--border)",
  fontSize: 13
};
const checkGlyph: CSSProperties = { width: 42, fontWeight: 700, fontSize: 11 };
const ledger: CSSProperties = {
  margin: 0,
  display: "grid",
  gridTemplateColumns: "180px 1fr",
  gap: "9px 16px",
  fontSize: 12.5
};
const label: CSSProperties = { display: "flex", flexDirection: "column", gap: 4, fontSize: 12.5 };
// Background and colour are named rather than left to the user agent
// (#618). color-scheme now makes the UA's own default follow the theme, so
// this is no longer load-bearing on its own, but a control on a themed
// surface should say what it is instead of depending on the browser and
// the token layer agreeing about which grey.
const input: CSSProperties = {
  font: "inherit",
  fontSize: 13,
  padding: "6px 8px",
  background: "var(--surface-2)",
  color: "var(--text)",
  border: "1px solid var(--border-strong, var(--border))",
  borderRadius: 6
};
const textarea: CSSProperties = {
  ...input,
  fontFamily: "var(--font-mono, ui-monospace, monospace)",
  fontSize: 11.5
};
const locationList: CSSProperties = {
  listStyle: "none",
  margin: "6px 0",
  padding: 0,
  display: "flex",
  flexDirection: "column",
  gap: 3
};
const locationRow: CSSProperties = { fontSize: 12 };
const btn: CSSProperties = {
  height: 30,
  padding: "0 12px",
  border: "1px solid var(--border-strong, var(--border))",
  borderRadius: 6,
  background: "var(--surface)",
  color: "var(--text)",
  font: "inherit",
  fontSize: 12.5,
  cursor: "pointer",
  alignSelf: "flex-start"
};
// var(--surface) rather than a literal white (#618). The accent is not one
// colour: it is dark in light mode and LIGHT in dark mode (0.52 vs 0.70
// lightness), so the text on it has to invert with the theme. A hardcoded
// white is right in light mode and about 1.9:1 in dark mode, which is the
// same shape of mistake as the black-on-dark this issue is named for, one
// layer up. --surface inverts with the theme and is what .btn--primary in
// the design system already uses.
const btnPrimary: CSSProperties = {
  background: "var(--accent)",
  borderColor: "var(--accent)",
  color: "var(--surface)"
};
const hint: CSSProperties = { fontSize: 11.5, color: "var(--text-3)" };
