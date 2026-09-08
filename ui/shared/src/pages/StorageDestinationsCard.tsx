/**
 * The storage destinations an operator owns: the list, the verify button,
 * the removal, and the FR-30 report when a destination artifacts already
 * live on stops answering (G2.2, issue #594).
 *
 * The wizard behind "Add a destination" is S3DestinationWizard.tsx; this
 * is the page it opens from and returns to.
 *
 * # What FR-30 makes this card responsible for
 *
 * FR-30's invariant is that at no instant may an artifact have no
 * confirmed readable copy. Two things on this card could break it by
 * accident, and both are handled here rather than left to the backend to
 * refuse quietly.
 *
 * A failed re-verification is a REPORT and never a state change. Nothing
 * is deleted and nothing is marked lost. The affected copies read as
 * unreachable, which is the word the engine already produces for a medium
 * the configuration cannot reach and which means "this deployment cannot
 * confirm or deny the copy", emphatically not "the copy is gone". No prune
 * runs against them, and no source copy anywhere is reclaimed on the
 * strength of a placement that could not be confirmed. The engine already
 * declines to delete a source it cannot confirm; this card's job is to say
 * so on screen instead of letting it be silent.
 *
 * And the banner names the count AND lists the backup sets, because "148
 * copies affected" with no list is not something an operator can act on.
 *
 * Editing a destination stays available while it is failing, deliberately:
 * a credential that expired is exactly the thing an operator needs to be
 * able to fix. Removing it is refused while any placement names it, and
 * the refusal says how many.
 *
 * # Every action names its CLI equivalent
 *
 * EPIC G's standing rule. The command lines come from
 * storageDestinationCommands.ts, which is also where the argument for why
 * none of them can carry a secret lives. G1.2's global terminal will print
 * them; until it exists they are shown beside the control that produces
 * them, which is the same string either way.
 */
import { useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import type {
  ApiError,
  MediumPreflight,
  StorageMedium,
  StorageMediumUsage
} from "@shared/api/contracts";
import { useAsync } from "@shared/hooks/useAsync";
import { ErrorState } from "@shared/components/EmptyState";
import { apiErrorOf, isNotConfigured } from "@shared/api/failure";
import { S3DestinationWizard } from "@shared/pages/S3DestinationWizard";
import { MediumPreflightChecks } from "@shared/pages/MediumPreflightChecks";
import { CommandEcho } from "@shared/pages/CommandEcho";
import { preflightCommand, removeCommand, showCommand } from "@shared/pages/storageDestinationCommands";

export function StorageDestinationsCard({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const mediums = useAsync<StorageMedium[]>(() => api.listStorageMediums(), [api]);
  const [editing, setEditing] = useState<StorageMedium | null>(null);
  const [adding, setAdding] = useState(false);

  return (
    <section className="card">
      <div className="card__header">
        <h2 className="eyebrow">Storage destinations</h2>
      </div>
      <div className="card__body" style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        {isNotConfigured(mediums.error) ? (
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)" }}>
            Storage destinations are part of a configuration this instance has not been given
            yet. They become editable once the first backup set has been added.
          </p>
        ) : mediums.error ? (
          <ErrorState
            message={mediums.error.message}
            remediation="The storage destinations could not be read, so they cannot be edited here yet."
            correlationId={mediums.error.correlationId}
            onRetry={mediums.reload}
          />
        ) : mediums.data ? (
          <>
            <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "74ch" }}>
              The places a retention tier can send a backup to, besides each backup set&rsquo;s own
              local path. Declaring one moves nothing on its own: backups arrive here only once a
              retention tier names it, which is a separate decision with a disclosure of its own.
            </p>

            {mediums.data.length === 0 ? (
              <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
                No storage destinations are declared. Every backup stays on this machine.
              </p>
            ) : (
              <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
                {mediums.data.map((m) => (
                  <DestinationRow
                    key={m.id}
                    medium={m}
                    readOnly={readOnly}
                    onEdit={() => setEditing(m)}
                    onChanged={mediums.reload}
                  />
                ))}
              </div>
            )}

            <div>
              <button className="btn btn--primary" disabled={readOnly} onClick={() => setAdding(true)}>
                Add a destination
              </button>
              <span style={{ marginLeft: 10, fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
                writes storage_mediums[] in config.yaml
              </span>
            </div>
          </>
        ) : (
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>Loading storage destinations…</p>
        )}

        {adding ? (
          <S3DestinationWizard
            onClose={() => setAdding(false)}
            onSaved={() => {
              setAdding(false);
              mediums.reload();
            }}
          />
        ) : null}
        {editing ? (
          <S3DestinationWizard
            editing={editing}
            onClose={() => setEditing(null)}
            onSaved={() => {
              setEditing(null);
              mediums.reload();
            }}
          />
        ) : null}
      </div>
    </section>
  );
}

/**
 * One destination, with the three things that can be done to it.
 *
 * Verify and Remove both keep their result on this row rather than
 * navigating anywhere, because both answers are about this row and because
 * a failed verification has to be able to sit beside the destination it is
 * about while the operator reads what is affected.
 */
function DestinationRow({
  medium,
  readOnly,
  onEdit,
  onChanged
}: {
  medium: StorageMedium;
  readOnly: boolean;
  onEdit(): void;
  onChanged(): void;
}) {
  const api = useApi();
  const [report, setReport] = useState<MediumPreflight | null>(null);
  const [usage, setUsage] = useState<StorageMediumUsage | null>(null);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<ApiError | null>(null);

  async function verify() {
    setBusy(true);
    setFailure(null);
    try {
      const result = await api.preflightStorageMedium(medium.id);
      setReport(result);
      // The usage report is fetched WHEN THE CHECK FAILS and not before,
      // because that is the only moment it means anything: FR-30's
      // question is "what is affected", and asking it about a destination
      // that just answered would put a count on screen with nothing to
      // act on. Nothing about this fetch changes any state; it reads the
      // journal.
      setUsage(result.ok ? null : await api.getStorageMediumUsage(medium.id));
    } catch (e) {
      setFailure(apiErrorOf(e));
    } finally {
      setBusy(false);
    }
  }

  async function remove() {
    setBusy(true);
    setFailure(null);
    try {
      await api.removeStorageMedium(medium.id);
      onChanged();
    } catch (e) {
      setFailure(apiErrorOf(e));
      // The refusal names a count; this puts the list behind it on screen,
      // because "148 copies affected" with nothing named is a number
      // rather than something an operator can act on.
      try {
        setUsage(await api.getStorageMediumUsage(medium.id));
      } catch {
        // A usage read that fails leaves the refusal standing on its own.
        // It is strictly extra detail, and swallowing its own failure is
        // better than replacing a precise refusal with a vaguer one.
      }
    } finally {
      setBusy(false);
    }
  }

  const failedVerification = report !== null && !report.ok;

  return (
    <div
      style={{
        border: "1px solid var(--border)",
        borderRadius: "var(--radius-lg)",
        padding: 12,
        display: "flex",
        flexDirection: "column",
        gap: 10
      }}
    >
      <div style={{ display: "flex", alignItems: "baseline", gap: 10, flexWrap: "wrap" }}>
        <span style={{ fontWeight: 600, fontSize: 14 }}>{medium.id}</span>
        <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
          {describeDestination(medium)}
        </span>
        {medium.readsRequireRestore ? (
          <span className="badge" style={{ color: "var(--warn)" }}>
            reads need a restore
          </span>
        ) : null}
      </div>

      <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
        <button className="btn" disabled={busy} onClick={verify}>
          {busy ? "Working…" : "Verify"}
        </button>
        <button className="btn" disabled={readOnly || busy} onClick={onEdit}>
          Edit
        </button>
        <button className="btn" disabled={readOnly || busy} onClick={remove}>
          Remove
        </button>
      </div>

      <CommandEcho
        label="the same thing from a terminal"
        commands={[showCommand(medium.id), preflightCommand(medium.id), removeCommand(medium.id)]}
      />

      {failure ? (
        <div className="banner banner--warn" style={{ fontSize: 13 }}>
          {failure.message}
        </div>
      ) : null}

      {failedVerification ? (
        <FailedVerificationBanner medium={medium} usage={usage} />
      ) : null}

      {report ? <MediumPreflightChecks report={report} /> : null}

      {usage && !failedVerification && failure ? <AffectedSets usage={usage} /> : null}
    </div>
  );
}

/**
 * The FR-30 case, and the one pane on this page whose wording is
 * load-bearing.
 *
 * Everything it says is a claim about what has NOT happened, because that
 * is the whole content of FR-30 at this moment: a destination this
 * deployment cannot reach is a destination it cannot ask about, and the
 * failure mode this banner exists to prevent is an operator reading a red
 * verification as "my backups are gone" and doing something drastic.
 */
function FailedVerificationBanner({
  medium,
  usage
}: {
  medium: StorageMedium;
  usage: StorageMediumUsage | null;
}) {
  const affected = usage?.placements ?? 0;
  const onlyCopy = (usage?.backupSets ?? []).reduce((n, s) => n + s.onlyCopyHere, 0);

  return (
    <div className="banner banner--warn" style={{ display: "block", fontSize: 13 }}>
      <div style={{ fontWeight: 600, marginBottom: 6 }}>
        {affected > 0
          ? `I cannot reach ${medium.id}, and ${affected} ${affected === 1 ? "copy is" : "copies are"} recorded there.`
          : `I cannot reach ${medium.id}. Nothing on record is stored there.`}
      </div>
      {affected > 0 ? (
        <>
          <p style={{ margin: "0 0 8px", maxWidth: "74ch" }}>
            Nothing has been deleted and nothing will be. A copy I cannot confirm is not a copy I
            treat as gone: those backups read as <strong>unreachable</strong> until this destination
            answers again, retention will not prune against them, and no source copy anywhere gets
            reclaimed on the strength of a placement I could not verify. Editing this destination is
            allowed. Removing it is refused while a copy names it.
            {onlyCopy > 0
              ? ` ${onlyCopy} of them ${onlyCopy === 1 ? "is" : "are"} the only confirmed copy of their backup anywhere.`
              : ""}
          </p>
          {usage ? <AffectedSets usage={usage} /> : null}
        </>
      ) : (
        <p style={{ margin: 0, maxWidth: "74ch" }}>
          Nothing has changed. No backup references this destination yet, so a failing check here
          costs nothing but the next move that would have used it.
        </p>
      )}
    </div>
  );
}

/** The list behind the count. A count on its own is a number; this is the
 *  part an operator can act on. */
function AffectedSets({ usage }: { usage: StorageMediumUsage }) {
  if (usage.backupSets.length === 0) return null;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      <div className="eyebrow" style={{ fontSize: 10.5 }}>
        What is affected, listed rather than counted
      </div>
      {usage.backupSets.map((s) => (
        <div key={s.set} style={{ display: "flex", gap: 10, fontSize: 12.5, flexWrap: "wrap" }}>
          <span className="mono">{s.set}</span>
          <span style={{ color: "var(--text-2)" }}>
            {s.placements} {s.placements === 1 ? "backup" : "backups"}
            {s.onlyCopyHere > 0 ? " · only copy is here" : ""}
          </span>
          <span className="badge">unreachable</span>
        </div>
      ))}
      <p style={{ margin: 0, fontSize: 12, color: "var(--text-3)", maxWidth: "74ch" }}>
        Nothing on this list is reported as lost. Unreachable means this deployment currently has no
        way to ask, which is a different sentence from &ldquo;the copy is gone&rdquo;.
      </p>
    </div>
  );
}

/** The one-line description a row shows: everything about the place, and
 *  nothing about the credential, because the API reports nothing about the
 *  credential (see StorageMedium's own doc). */
function describeDestination(m: StorageMedium): string {
  const where = m.prefix ? `${m.bucket}/${m.prefix}` : m.bucket;
  return [m.type, where, m.region, m.storageClass].filter(Boolean).join(" · ");
}
