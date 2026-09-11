import { Fragment } from "react";
import type { TrustedHostKey } from "@shared/types/backup";

/**
 * The host keys a connection will actually check against, shown the way
 * they have to be read before anyone relies on them.
 *
 * # Every value here is one somebody read
 *
 * This panel used to state "Algorithm: ssh-ed25519" beside an empty
 * fingerprint, on every deployment. The algorithm was a literal written
 * into the detail page's JSX and the fingerprint was
 * BackupSet.hostFingerprint, which api/client.ts filled with a literal
 * empty string because nothing on the wire carried a host key at all. So a
 * deployment whose anchor was an RSA key was told it had trusted an
 * ed25519 one, with no digest beside it to check that against, and the
 * halt banner for a CHANGED host key, the most dangerous state in the app,
 * linked here so an operator could make exactly that comparison.
 *
 * The keys now come from GET /backup-sets, which reads them out of the
 * set's own known_hosts, or from the wizard's own probe. An empty list is
 * rendered as a sentence rather than as a blank row: "nothing has been
 * read yet" and "this is what is trusted" are different answers, and a
 * blank beside a confident label is neither.
 *
 * # Why a list
 *
 * A host answering with more than one key algorithm has a known_hosts line
 * for each, so a set pinning both an ed25519 and an RSA key is in ordinary
 * shape. Showing one of two would offer a fingerprint the server in front
 * of the operator may not present, which is the same failure as showing an
 * invented one, one step subtler.
 *
 * # What this cannot show, and why there is no "presented now" row
 *
 * A panel that could put the key a host just offered beside the key on
 * record would be the ideal thing to link a host-key halt to. Nothing in
 * this product produces that value: a halted set carries a haltReason and
 * no key material, and the transport does not record what it was offered.
 * A row for it existed here and was never passed one, which is the same
 * defect as the literal above with the fault the other way round, so it is
 * gone. What the halt banner links to is what IS on record, which is the
 * half of the comparison this product actually holds; the other half is
 * the server, which is where the operator has to look anyway.
 *
 * Fingerprints break on any character rather than wrapping on word
 * boundaries, because a base64 digest has no words and a line break in a
 * convenient-looking place is how two different keys come to look the same
 * at a glance.
 */
export function FingerprintDisplay({
  host,
  keys,
  emptyNote = "Backupd could not read a host key for this set, so none is shown here.",
  trustedAt
}: {
  host: string;
  /** What is trusted for this host. Empty renders emptyNote instead. */
  keys: TrustedHostKey[];
  /** What to say when there is no key to show. It differs by caller: the
   *  wizard has not asked the host yet, the detail page asked and got
   *  nothing back, and those are not the same sentence. */
  emptyNote?: string;
  trustedAt?: string | null;
}) {
  return (
    <dl
      style={{
        margin: 0, display: "grid", gridTemplateColumns: "132px 1fr",
        gap: "12px 16px", padding: "16px 18px",
        background: "var(--surface-2)", border: "1px solid var(--border)",
        borderRadius: "var(--radius-lg)", fontSize: 13
      }}
    >
      <dt style={{ color: "var(--text-2)" }}>Host</dt>
      <dd className="mono" style={{ margin: 0 }}>{host}</dd>

      {keys.length === 0 ? (
        <>
          <dt style={{ color: "var(--text-2)" }}>Fingerprint</dt>
          <dd style={{ margin: 0, color: "var(--text-3)" }}>{emptyNote}</dd>
        </>
      ) : (
        keys.map((k) => (
          <Fragment key={k.algorithm + " " + k.fingerprint}>
            <dt style={{ color: "var(--text-2)" }}>Algorithm</dt>
            <dd className="mono" style={{ margin: 0 }}>{k.algorithm}</dd>
            <dt style={{ color: "var(--text-2)" }}>Fingerprint</dt>
            <dd className="mono" style={{ margin: 0, wordBreak: "break-all", fontSize: 13.5 }}>
              {k.fingerprint}
            </dd>
          </Fragment>
        ))
      )}

      <dt style={{ color: "var(--text-2)" }}>Trusted</dt>
      <dd style={{ margin: 0 }}>
        {trustedAt ? new Date(trustedAt).toLocaleString() : "Not yet trusted"}
      </dd>
    </dl>
  );
}
