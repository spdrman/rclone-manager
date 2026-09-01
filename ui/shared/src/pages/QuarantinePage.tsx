import { useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import type { AsyncState } from "@shared/hooks/useAsync";
import { PageHeader } from "@shared/components/PageHeader";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
import { WarningBanner } from "@shared/components/WarningBanner";
import { describeFailure } from "@shared/api/failure";
import { stamp } from "@shared/utilities/format";
import type { BackupArtifact, QuarantineReason } from "@shared/types/backup";

const REASON: Record<QuarantineReason, string> = {
  "checksum-mismatch": "Checksum mismatch",
  "validation-failed": "Validation failed",
  "unexpected-artifact": "Unexpected artifact",
  "remote-identity-changed": "Remote identity changed",
  "incomplete-transfer": "Incomplete transfer"
};

/** What the last action on this page did, in the operator's terms. The three
 *  reinstatement outcomes are three different things and are rendered three
 *  different ways (issue #229):
 *
 *  - `failed`   the request never got an answer. An error, `role="alert"`.
 *  - `refused`  the request was answered and the answer was "no". That is a
 *               verdict about the backup, not a broken request, so it reads
 *               as a warning and carries the reason the checks gave.
 *  - `restored` the backup is back in service. The only outcome that reloads
 *               the list, and the only one that is `role="status"` rather
 *               than an alert, because nothing is wrong.
 *
 *  Revalidate and Retry ingestion only ever produce `failed`: neither
 *  resolves with a body worth reading. */
type Outcome =
  | { kind: "failed"; message: string; remediation?: string; correlationId?: string }
  | { kind: "refused"; filename: string; reason: string }
  | { kind: "restored"; filename: string; reason: string };

/** #274: these three actions used to report "Try again." whatever the
 *  service said, under the literal `cid_quarantine_action`. Reinstatement
 *  in particular refuses for reasons retrying cannot fix
 *  (ARTIFACT_IRRECOVERABLE, ARTIFACT_NOT_QUARANTINED), and the service
 *  names them. "Try again" is kept only for the case where nothing else
 *  is known. */
function actionFailed(verb: string, filename: string, e: unknown): Outcome {
  const headline = verb + " \"" + filename + "\".";
  const failure = describeFailure(e, headline);
  const reason = failure.remediation ?? (failure.message === headline ? undefined : failure.message);
  return {
    kind: "failed",
    message: headline,
    // "Try again" only where nothing better is known, instead of as the
    // standing advice it used to be.
    remediation: reason ?? "Try again.",
    correlationId: failure.correlationId
  };
}

/** No "delete remote anyway" action exists here, by design (§18).
 *
 *  `quarantine` is the SAME `quarantineNode`-backed resource App.tsx
 *  fetches to compute the sidebar's `counts.quarantine` badge (see
 *  appNodes.ts, `useResource(quarantineNode, ...)` in App.tsx) — passed
 *  down exactly like `sets`/`health` already are. This page used to run
 *  its own independent `useAsync(() => api.listQuarantine())`, so the
 *  badge and this list were two separate reads of the same resource that
 *  could disagree (#101). Reading the shared node here instead means both
 *  can only ever show what was last committed to that one node.
 *
 *  Nothing on this page describes an artifact AFTER it has been reinstated,
 *  and that is not an omission. A reinstated artifact is COMMITTED or
 *  COMPLETE again, so `listQuarantine` stops returning it and its row leaves
 *  this table on the reload the success notice follows. The count of
 *  reinstated artifacts still holding a remote source (#227,
 *  `ReinstatedRemoteRetainedCount`) is a per-backup-set health figure and is
 *  not on this page's read path: App.tsx hands this page `quarantine` and
 *  nothing else, and the client does not map that field into any UI type at
 *  all. It belongs where the rest of a set's health is shown, not fetched
 *  separately to decorate a row here. */
export function QuarantinePage({
  readOnly,
  quarantine
}: {
  readOnly: boolean;
  quarantine: AsyncState<BackupArtifact[]>;
}) {
  const api = useApi();
  // Revalidate and Retry ingestion resolve with nothing worth keeping, so
  // the reload of `quarantine` is what updates the row. This state
  // exists so that a rejected call has a visible outcome instead of a
  // silent no-op (mandatory review, PR #147): without it, a backend failure
  // left the button's click looking like it did nothing at all. Reinstate
  // then gave it two more outcomes to tell apart; see Outcome.
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  // Non-null while the confirmation is open, and it holds the artifact
  // rather than a boolean so the dialog can name the file it is about.
  const [confirming, setConfirming] = useState<BackupArtifact | null>(null);
  // Reinstating re-runs the backup set's configured validator, which is a
  // real amount of work, so the dialog stays up with its confirm disabled
  // until the answer arrives rather than closing onto a page that shows
  // nothing yet.
  const [running, setRunning] = useState(false);

  const revalidate = (a: BackupArtifact) => {
    setOutcome(null);
    api
      .revalidate(a.id)
      .then(quarantine.reload)
      .catch((e: unknown) => setOutcome(actionFailed("Could not revalidate", a.filename, e)));
  };

  const retryIngestion = (a: BackupArtifact) => {
    setOutcome(null);
    api
      .retryIngestion(a.id)
      .then(quarantine.reload)
      .catch((e: unknown) => setOutcome(actionFailed("Could not retry ingestion for", a.filename, e)));
  };

  const reinstate = (a: BackupArtifact) => {
    setOutcome(null);
    setRunning(true);
    api
      .reinstate(a.id)
      .then((result) => {
        setConfirming(null);
        if (!result.reinstated) {
          setOutcome({ kind: "refused", filename: a.filename, reason: result.reason });
          return;
        }
        setOutcome({ kind: "restored", filename: a.filename, reason: result.reason });
        quarantine.reload();
      })
      .catch((e: unknown) => {
        setConfirming(null);
        setOutcome(actionFailed("Could not reinstate", a.filename, e));
      })
      .finally(() => setRunning(false));
  };

  if (quarantine.error) return <ErrorState {...quarantine.error} onRetry={quarantine.reload} />;

  const data = quarantine.data ?? [];

  return (
    <>
      <PageHeader
        title="Quarantine"
        subtitle="Artifacts held back from the catalog. Their remote originals are retained until the issue is resolved."
      />

      {outcome?.kind === "failed" ? (
        <div style={{ marginBottom: 14 }}>
          <ErrorState
            message={outcome.message}
            remediation={outcome.remediation}
            correlationId={outcome.correlationId}
          />
        </div>
      ) : null}

      {outcome?.kind === "refused" ? (
        <div style={{ marginBottom: 14 }}>
          <WarningBanner
            tone="warn"
            eyebrow="Not reinstated"
            title={"\"" + outcome.filename + "\" stays in quarantine."}
          >
            <p style={{ margin: 0 }}>{"The checks ran and did not carry it: " + outcome.reason + "."}</p>
            <p style={{ margin: "6px 0 0" }}>
              Nothing moved, and nothing was forfeited. Its remote source is still there to
              re-ingest from.
            </p>
          </WarningBanner>
        </div>
      ) : null}

      {outcome?.kind === "restored" ? (
        // role="status", not "alert": a reinstatement that worked is not a
        // problem, and the polite live region is what announces it.
        <div style={{ marginBottom: 14 }} role="status">
          <WarningBanner
            tone="ok"
            eyebrow="Reinstated"
            title={"\"" + outcome.filename + "\" is back in service."}
          >
            <p style={{ margin: 0 }}>{"The checks carried it: " + outcome.reason + "."}</p>
            <p style={{ margin: "6px 0 0" }}>
              Its remote source is kept for good from now on. Backup Manager will never delete
              it, however completely this backup passes every later check.
            </p>
          </WarningBanner>
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
                          <span aria-hidden="true" style={{ color: "var(--danger)" }}>{"\u2715"}</span>
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
                          {/* Last, in the caution tier, glyphed, and with the
                              trailing ellipsis this UI already uses for "this
                              opens a confirmation" ("Apply retention now…",
                              "Remove set configuration…"). Four differences
                              from its neighbours, because it is the only one
                              of the four that cannot be undone. */}
                          <button
                            className="btn btn--sm btn--caution"
                            disabled={readOnly}
                            onClick={() => {
                              setOutcome(null);
                              setConfirming(a);
                            }}
                          >
                            <span aria-hidden="true" style={{ color: "var(--warn)" }}>▲</span>
                            Reinstate…
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
            remote deletion. A reinstated backup keeps its remote source for good: Backup
            Manager gives up deleting the original of any backup it has reinstated, and
            that decision is permanent.
          </p>
        </>
      )}

      {confirming ? (
        <ConfirmationDialog
          open
          eyebrow="Permanent, and not reversible"
          title="Reinstate this backup"
          confirmLabel="Reinstate and keep the remote source"
          disabled={running}
          onCancel={() => {
            if (!running) setConfirming(null);
          }}
          onConfirm={() => reinstate(confirming)}
        >
          <p style={{ margin: 0 }}>
            {"Backup Manager re-checks the durable local copy of \"" + confirming.filename +
              "\" now, and returns it to service only if what it finds is enough on its own."}
          </p>
          <p style={{ margin: 0, color: "var(--text-2)" }}>
            Reinstating permanently forfeits this backup's remote deletion. Its remote
            source stays where it is for good: Backup Manager will never delete it,
            however completely the backup passes every later check.
          </p>
          <p style={{ margin: 0, color: "var(--text-2)" }}>
            That trade is what makes this safe to offer. The evidence behind a
            reinstatement is a local re-check, which is a weaker thing than the full
            verification the backup passed on its way in, and not enough to authorise
            destroying the last remaining copy of the source.
          </p>
        </ConfirmationDialog>
      ) : null}
    </>
  );
}
