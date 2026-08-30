import { useApi } from "@shared/api/ApiContext";
import type { AsyncState } from "@shared/hooks/useAsync";
import { PageHeader } from "@shared/components/PageHeader";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { stamp } from "@shared/utilities/format";
import type { BackupArtifact, QuarantineReason } from "@shared/types/backup";

const REASON: Record<QuarantineReason, string> = {
  "checksum-mismatch": "Checksum mismatch",
  "validation-failed": "Validation failed",
  "unexpected-artifact": "Unexpected artifact",
  "remote-identity-changed": "Remote identity changed",
  "incomplete-transfer": "Incomplete transfer"
};

/** No "delete remote anyway" action exists here, by design (§18).
 *
 *  `quarantine` is the SAME `quarantineNode`-backed resource App.tsx
 *  fetches to compute the sidebar's `counts.quarantine` badge (see
 *  appNodes.ts, `useResource(quarantineNode, ...)` in App.tsx) — passed
 *  down exactly like `sets`/`health` already are. This page used to run
 *  its own independent `useAsync(() => api.listQuarantine())`, so the
 *  badge and this list were two separate reads of the same resource that
 *  could disagree (#101). Reading the shared node here instead means both
 *  can only ever show what was last committed to that one node. */
export function QuarantinePage({
  readOnly,
  quarantine
}: {
  readOnly: boolean;
  quarantine: AsyncState<BackupArtifact[]>;
}) {
  const api = useApi();
  const rows = quarantine;

  if (rows.error) return <ErrorState {...rows.error} onRetry={rows.reload} />;

  const data = rows.data ?? [];

  return (
    <>
      <PageHeader
        title="Quarantine"
        subtitle="Artifacts held back from the catalog. Their remote originals are retained until the issue is resolved."
      />

      {data.length === 0 ? (
        <EmptyState title="No quarantined backups">
          No backup artifacts currently require attention.
        </EmptyState>
      ) : (
        <>
          <div className="card">
            <div className="table-scroll">
              <table className="table" style={{ minWidth: 900 }}>
                <thead>
                  <tr>
                    <th scope="col">Backup</th>
                    <th scope="col">Backup set</th>
                    <th scope="col">Reason</th>
                    <th scope="col">Detected</th>
                    <th scope="col">Remote source</th>
                    <th scope="col" style={{ textAlign: "right" }}>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {data.map((a) => (
                    <tr key={a.id}>
                      <td className="mono" style={{ fontSize: "var(--text-sm)" }}>{a.filename}</td>
                      <td>{a.setName}</td>
                      <td>
                        <span style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
                          <span aria-hidden="true" style={{ color: "var(--danger)" }}>\u2715</span>
                          {a.quarantine ? REASON[a.quarantine.reason] : "\u2014"}
                        </span>
                      </td>
                      <td className="mono" style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                        {a.quarantine ? stamp(a.quarantine.detectedAt) : "\u2014"}
                      </td>
                      <td style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>Retained on remote</td>
                      <td>
                        <span style={{ display: "flex", gap: 7, justifyContent: "flex-end" }}>
                          <button className="btn btn--sm">Inspect</button>
                          <button className="btn btn--sm" disabled={readOnly} onClick={() => api.revalidate(a.id).then(rows.reload)}>
                            Revalidate
                          </button>
                          <button className="btn btn--sm" disabled={readOnly} onClick={() => api.retryIngestion(a.id).then(rows.reload)}>
                            Retry ingestion
                          </button>
                        </span>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
          <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
            Quarantined artifacts are never counted as known-good, and never trigger
            remote deletion.
          </p>
        </>
      )}
    </>
  );
}
