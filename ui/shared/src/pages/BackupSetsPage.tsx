import { useNavigate } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import type { AsyncState } from "@shared/hooks/useAsync";
import { useCausl } from "@shared/state/graph";
import { operationsNode, versionNode } from "@shared/state/appNodes";
import { progressPercent } from "@shared/types/operation";
import type { BackupSet } from "@shared/types/backup";
import { PageHeader } from "@shared/components/PageHeader";
import { BackupSetCard } from "@shared/components/BackupSetCard";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { isNotConfigured } from "@shared/api/failure";

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
          " sets \u00b7 " + healthy + " healthy \u00b7 " + stale + " stale \u00b7 " + failing + " failing"
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
              onOpen={() => navigate("/sets/" + set.id)}
              onTest={() => api.testConnection(set.id)}
              actionsDisabled={readOnly}
            />
          );
        })}
      </div>
    </>
  );
}
