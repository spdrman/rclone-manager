/**
 * Every backup set as a row, with the per-set controls that used to mean
 * opening the set first.
 *
 * Putting enable, disable and remove on the row is the decision this page
 * turns on, and it is the one that needs care: a destructive action inside
 * a list is the classic way to act on the wrong item. The three answers
 * live in BackupSetCard's own doc, and what is here is the state behind
 * them. One removal target at a time, so two confirmations cannot be open
 * at once; a per-row busy set, so a row with something in flight offers
 * nothing else; and a ref beside that state, because React batches and the
 * second click of a double-click is dispatched before any re-render has
 * disabled anything.
 *
 * The deployment-wide run stays in the page header where its scope is
 * legible, and it submits against the configuration revision the screen is
 * currently showing rather than one fetched at the moment of the click,
 * because refusing a run based on a revision nobody has seen is the whole
 * point of that check.
 */
import { useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import type { AsyncState } from "@shared/hooks/useAsync";
import { useCausl } from "@shared/state/graph";
import { operationsNode, versionNode } from "@shared/state/appNodes";
import { progressPercent } from "@shared/types/operation";
import type { BackupSet } from "@shared/types/backup";
import { PageHeader } from "@shared/components/PageHeader";
import { BackupSetCard } from "@shared/components/BackupSetCard";
import { RemoveBackupSetDialog } from "@shared/components/RemoveBackupSetDialog";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { isNotConfigured } from "@shared/api/failure";
import { backupSetPath } from "@shared/utilities/routes";

export function BackupSetsPage({
  sets,
  readOnly
}: {
  sets: AsyncState<BackupSet[]>;
  readOnly: boolean;
}) {
  const api = useApi();
  const navigate = useNavigate();
  // Reads the same shared node DashboardPage does (#95) — previously this
  // page ran its own independent listOperations() poll, so the two could
  // disagree about what was currently running for a given set.
  const operations = useCausl(operationsNode);
  // The configuration revision the screen is CURRENTLY showing, not one
  // read fresh at submit time: a run submitted against a revision this
  // page has not seen is exactly what CONFIG_REVISION_STALE exists to
  // refuse (see BackupManagerApi.runCycle's own doc).
  const version = useCausl(versionNode);

  // ---------------------------------------------------------- per-row
  //
  // Enable, disable and remove act on ONE backup set and used to mean
  // opening that set first. They are here now, on the row, which puts a
  // destructive action in a list and makes "which row is this about"
  // the thing worth spending care on. BackupSetCard's own doc has the
  // three answers; this is the state behind them.
  //
  // `inFlight` is a ref AND `busy` is state on purpose. The ref is the
  // synchronous truth a second click has to be refused against: React
  // batches, so two clicks dispatched before the re-render both see a
  // button that is still enabled and a `busy` that is still empty, and
  // the guard that runs inside the handler is the only one that has
  // already been told about the first click. `busy` is the copy the
  // render reads, which is what turns the rest of that row off.
  const inFlight = useRef<Set<string>>(new Set());
  const [busy, setBusy] = useState<readonly string[]>([]);
  // The set whose removal is being confirmed, or null. Only one at a
  // time, so two removals cannot be in confirmation at once, and the
  // dialog is mounted per target so nothing typed for one row can
  // survive into another's.
  const [removeTarget, setRemoveTarget] = useState<BackupSet | null>(null);

  const publishBusy = () => setBusy([...inFlight.current]);

  function toggleEnabled(target: BackupSet) {
    if (inFlight.current.has(target.id)) return;
    inFlight.current.add(target.id);
    publishBusy();
    void api
      .setEnabled(target.source, target.set, !target.enabled)
      .finally(() => {
        inFlight.current.delete(target.id);
        publishBusy();
        // setEnabled resolves to nothing, so the only way this page can
        // show what is now true is to re-read the list. Same call
        // App.tsx wired into this prop, so the dashboard's copy of the
        // same node moves with it.
        sets.reload();
      });
  }

  // #275: the empty list an unconfigured instance should show, rather than
  // the refusal it answers with. Same shape as the genuinely-zero-sets case
  // below, different sentence, because "no configuration yet" and "a
  // configuration with no sets in it" are not the same fact.
  if (isNotConfigured(sets.error))
    return (
      <>
        <PageHeader title="Backup sets" subtitle="No backup sets configured" />
        <EmptyState
          title="No backup sets yet"
          action={
            <button className="btn btn--primary" onClick={() => navigate("/sets/new")}>
              Add backup set
            </button>
          }
        >
          This instance has no configuration yet. Adding your first backup set is what
          writes it.
        </EmptyState>
      </>
    );

  if (sets.error) return <ErrorState {...sets.error} onRetry={sets.reload} />;

  const data = sets.data ?? [];
  const healthy = data.filter((s) => s.state === "healthy").length;
  const stale = data.filter((s) => s.state === "stale").length;
  const failing = data.filter((s) => s.state === "failing").length;
  const disabled = data.filter((s) => !s.enabled).length;

  // Loading and "genuinely zero sets" are different states (sets.data is
  // null vs. an empty array \u2014 see ResourceState/AsyncState) and must not be
  // conflated: the header's own "Add backup set" action button is always
  // present below, so treating "still loading" as "empty" briefly rendered
  // the EmptyState's second copy of that same button while sets.data was
  // still null. Only render the EmptyState (with its own action button, no
  // header action button) once loading has actually finished with zero
  // sets, matching DashboardPage's equivalent early return.
  if (sets.data && data.length === 0) {
    return (
      <>
        <PageHeader title="Backup sets" subtitle="No backup sets configured" />
        <EmptyState
          title="No backup sets yet"
          action={
            <button className="btn btn--primary" onClick={() => navigate("/sets/new")}>
              Add backup set
            </button>
          }
        >
          Connect Backup Manager to your first server to begin collecting and
          retaining verified backups.
        </EmptyState>
      </>
    );
  }

  return (
    <>
      <PageHeader
        title="Backup sets"
        subtitle={
          data.length +
          " sets \u00b7 " + healthy + " healthy \u00b7 " + stale + " stale \u00b7 " + failing + " failing" +
          // Only when there are any. A permanent "0 disabled" is a line
          // an operator stops reading, and then stops seeing on the day
          // it says 1.
          (disabled === 0 ? "" : " \u00b7 " + disabled + " disabled")
        }
        actions={
          <>
            {/* The one run control on this page, and it is page-level
                rather than per-card because that is the scope it has: a
                run cycle walks every enabled backup set in one pass and
                there is no per-set run to call (#211, #214). Each card
                used to carry its own copy of exactly this action under
                the label "Run now", so clicking it on one set started
                all of them (#231). The tooltip is the same sentence
                BackupSetDetailPage's button carries, for the same
                reason: the label alone cannot say how wide the action
                reaches. */}
            <button
              className="btn"
              disabled={readOnly}
              title="Runs one pass over every enabled backup set, not only this one."
              onClick={() => api.runCycle(version.data?.configRevision ?? "").then(sets.reload)}
            >
              Run all due sets
            </button>
            <button className="btn btn--primary" disabled={readOnly} onClick={() => navigate("/sets/new")}>
              Add backup set
            </button>
          </>
        }
      />

      {/* operations.data is null until the first fetch resolves — that is
          "not known yet", not "nothing running for any set", so it must
          not be indistinguishable from a genuinely idle card below
          (mandatory review, PR #143). A failed fetch gets its own small
          inline notice here rather than a full-page ErrorState, since
          operations is secondary to sets on this page (sets.error owns
          that treatment via the early return above). */}
      {operations.error ? (
        <div className="banner banner--danger" style={{ fontSize: "var(--text-sm)" }}>
          Live operation status is unavailable ({operations.error.message}) — current-operation
          badges below may be stale.
        </div>
      ) : null}

      <div
        style={{
          display: "grid",
          gridTemplateColumns: "repeat(auto-fill, minmax(372px, 1fr))",
          gap: 14
        }}
      >
        {data.map((set) => {
          const op = operations.data?.find((o) => o.setId === set.id);
          // The percent is the one being copied artifact's, and it is null
          // when the service could not measure one. Null drops the number
          // entirely rather than printing "0%", which would read as a
          // stalled transfer instead of an unmeasured one.
          const pct = op?.progress ? progressPercent(op.progress) : null;
          const currentOperation = op
            ? op.label.toLowerCase() + (pct === null ? "" : " " + pct + "%")
            : operations.data === null
              ? "checking…"
              : undefined;
          return (
            <BackupSetCard
              key={set.id}
              set={set}
              currentOperation={currentOperation}
              onOpen={() => navigate(backupSetPath(set.source, set.set))}
              onTest={() => api.testConnection(set.id)}
              onToggleEnabled={() => toggleEnabled(set)}
              onRemove={() => setRemoveTarget(set)}
              // A row whose removal is being confirmed is as busy as one
              // with a request already out: leaving Disable live under an
              // open removal confirmation is the disable-racing-a-remove
              // shape, one dialog away.
              busy={busy.includes(set.id) || removeTarget?.id === set.id}
              actionsDisabled={readOnly}
            />
          );
        })}
      </div>

      {/* Mounted per target rather than kept open with a changing `set`,
          so the typed confirmation starts empty for every row by
          construction and not only by an effect.

          There is nowhere to navigate on success from here: the list IS
          where the detail page sends an operator after a removal. So this
          refreshes the list instead, through the same node App fetches,
          which is what the create path in BackupSetWizardPage does after
          a write and for the same reason. */}
      {removeTarget ? (
        <RemoveBackupSetDialog
          set={removeTarget}
          open
          onCancel={() => setRemoveTarget(null)}
          onRemoved={() => {
            setRemoveTarget(null);
            sets.reload();
          }}
        />
      ) : null}
    </>
  );
}
