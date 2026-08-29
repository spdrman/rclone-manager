export function FingerprintDisplay({
  host,
  algorithm,
  fingerprint,
  changedFrom,
  trustedAt
}: {
  host: string;
  algorithm: string;
  fingerprint: string;
  /** When present, this host key CHANGED — the most dangerous state in the app. */
  changedFrom?: string;
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

      <dt style={{ color: "var(--text-2)" }}>Algorithm</dt>
      <dd className="mono" style={{ margin: 0 }}>{algorithm}</dd>

      {changedFrom ? (
        <>
          <dt style={{ color: "var(--text-2)" }}>Previously trusted</dt>
          <dd className="mono" style={{ margin: 0, wordBreak: "break-all", color: "var(--text-2)" }}>
            {changedFrom}
          </dd>
          <dt style={{ color: "var(--danger)" }}>Presented now</dt>
          <dd className="mono" style={{ margin: 0, wordBreak: "break-all", color: "var(--danger)" }}>
            {fingerprint}
          </dd>
        </>
      ) : (
        <>
          <dt style={{ color: "var(--text-2)" }}>Fingerprint</dt>
          <dd className="mono" style={{ margin: 0, wordBreak: "break-all", fontSize: 13.5 }}>
            {fingerprint}
          </dd>
        </>
      )}

      <dt style={{ color: "var(--text-2)" }}>Trusted</dt>
      <dd style={{ margin: 0 }}>
        {trustedAt ? new Date(trustedAt).toLocaleString() : "Not yet trusted"}
      </dd>
    </dl>
  );
}
