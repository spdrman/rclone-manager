import { useNavigate } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import type { AsyncState } from "@shared/hooks/useAsync";
import type { BackupSet } from "@shared/types/backup";
import { PageHeader } from "@shared/components/PageHeader";
import { BackupSetCard } from "@shared/components/BackupSetCard";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";

export function BackupSetsPage({
  sets,
  readOnly
}: {
  sets: AsyncState<BackupSet[]>;
  readOnly: boolean;
}) {
  const api = useApi();
  const navigate = useNavigate();
  const operations = useAsync(() => api.listOperations(), [api]);

  if (sets.error) return <ErrorState {...sets.error} onRetry={sets.reload} />;

  const data = sets.data ?? [];
  const healthy = data.filter((s) => s.state === "healthy").length;
  const stale = data.filter((s) => s.state === "stale").length;
  const failing = data.filter((s) => s.state === "failing").length;

  return (
    <>
      <PageHeader
        title="Backup sets"
        subtitle={
          data.length +
          " sets \u00b7 " + healthy + " healthy \u00b7 " + stale + " stale \u00b7 " + failing + " failing"
        }
        actions={
          <button className="btn btn--primary" disabled={readOnly} onClick={() => navigate("/sets/new")}>
            Add backup set
          </button>
        }
      />

      {data.length === 0 ? (
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
      ) : (
        <div
          style={{
            display: "grid",
            gridTemplateColumns: "repeat(auto-fill, minmax(372px, 1fr))",
            gap: 14
          }}
        >
          {data.map((set) => {
            const op = operations.data?.find((o) => o.setId === set.id);
            return (
              <BackupSetCard
                key={set.id}
                set={set}
                currentOperation={op ? op.label.toLowerCase() + " " + op.percent + "%" : undefined}
                onOpen={() => navigate("/sets/" + set.id)}
                onRun={() => api.runSet(set.id).then(sets.reload)}
                onTest={() => api.testConnection(set.id)}
                actionsDisabled={readOnly}
              />
            );
          })}
        </div>
      )}
    </>
  );
}
