import { useState } from "react";
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
  // Neither call resolves with a body worth keeping — the reload of
  // `quarantine` is what actually updates the row. This state exists only
  // to give a rejected revalidate/retry a visible outcome instead of a
  // silent no-op (mandatory review, PR #147): without it, a backend
  // failure left the button's click looking like it did nothing at all.
  const [actionError, setActionError] = useState<string | null>(null);
  // Reinstate is the one action whose success is worth saying out loud.
  // The other two either change the row (and the reload speaks for itself)
  // or change nothing at all; this one tells an operator that a backup they
  // were about to give up on is a restore point again, and that its remote
  // source is now kept for good.
  const [actionNotice, setActionNotice] = useState<string | null>(null);

  const clearOutcome = () => {
    setActionError(null);
    setActionNotice(null);
  };

  /** Reinstating has three outcomes, not two, and an operator has to be
   *  able to tell them apart. The request can fail; it can succeed and
   *  report that the local copy is bad, which is a verdict about the
   *  backup rather than a failure of the request; or it can succeed and
   *  actually return the backup to service. Only the third reloads the
   *  list, because only the third changes it. */
  const reinstate = (a: BackupArtifact) => {
    clearOutcome();
    api
      .reinstate(a.id)
      .then((outcome) => {
        if (!outcome.reinstated) {
          setActionError(
            "\"" + a.filename + "\" was not reinstated. " + (outcome.reason || "The checks did not pass.")
          );
          return;
        }
        setActionNotice(
          "\"" + a.filename + "\" is trusted again (" + outcome.state + "). " + outcome.reason +
            ". Its remote source is kept from now on."
        );
        quarantine.reload();
      })
      .catch(() => setActionError("Could not reinstate \"" + a.filename + "\". Try again."));
  };

  const revalidate = (a: BackupArtifact) => {
    clearOutcome();
    api
      .revalidate(a.id)
      .then(quarantine.reload)
      .catch(() => setActionError("Could not revalidate \"" + a.filename + "\". Try again."));
  };

  const retryIngestion = (a: BackupArtifact) => {
    clearOutcome();
    api
      .retryIngestion(a.id)
      .then(quarantine.reload)
      .catch(() => setActionError("Could not retry ingestion for \"" + a.filename + "\". Try again."));
  };

  if (quarantine.error) return <ErrorState {...quarantine.error} onRetry={quarantine.reload} />;

  const data = quarantine.data ?? [];

  return (
    <>
      <PageHeader
        title="Quarantine"
        subtitle="Artifacts held back from the catalog. Their remote originals are retained until the issue is resolved."
      />

      {actionError ? (
        <div style={{ marginBottom: 14 }}>
          <ErrorState message={actionError} correlationId="cid_quarantine_action" />
        </div>
      ) : null}

      {actionNotice ? (
        <div className="card" role="status" style={{ marginBottom: 14 }}>
          {actionNotice}
        </div>
      ) : null}

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
                          <button className="btn btn--sm" disabled={readOnly} onClick={() => revalidate(a)}>
                            Revalidate
                          </button>
                          <button className="btn btn--sm" disabled={readOnly} onClick={() => retryIngestion(a)}>
                            Retry ingestion
                          </button>
                          <button className="btn btn--sm" disabled={readOnly} onClick={() => reinstate(a)}>
                            Reinstate
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
            remote deletion. Retry ingestion fetches the backup again from its remote;
            Reinstate keeps the local copy and trusts it again, which is the answer when
            the remote is gone or the quarantine was a mistake. A reinstated backup keeps
            its remote source for good: this manager will never delete it afterwards.
          </p>
        </>
      )}
    </>
  );
}
