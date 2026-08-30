import { useNavigate } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import type { AsyncState } from "@shared/hooks/useAsync";
import { useCausl } from "@shared/state/graph";
import { operationsNode } from "@shared/state/appNodes";
import type { BackupSet } from "@shared/types/backup";
import type { SystemHealth } from "@shared/types/operation";
import { PageHeader } from "@shared/components/PageHeader";
import { HealthSummary } from "@shared/components/HealthSummary";
import { MetricCard } from "@shared/components/MetricCard";
import { StorageGauge } from "@shared/components/StorageGauge";
import { OperationProgress } from "@shared/components/OperationProgress";
import { ActivityTimeline } from "@shared/components/ActivityTimeline";
import { WarningBanner } from "@shared/components/WarningBanner";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { bytes, percent } from "@shared/utilities/format";

export function DashboardPage({
  health,
  sets,
  readOnly
}: {
  health: AsyncState<SystemHealth>;
  sets: AsyncState<BackupSet[]>;
  readOnly: boolean;
}) {
  const api = useApi();
  const navigate = useNavigate();
  // Live operation progress (§52, #95) reads the shared graph node directly
  // — App.tsx owns the one fetch/poll of it, so this page never re-fetches
  // its own copy. BackupSetsPage (#97) reads the exact same node, so the
  // two can never disagree about what is currently running.
  const operations = useCausl(operationsNode);
  const activity = useAsync(() => api.listActivity(), [api]);

  if (health.error)
    return <ErrorState {...health.error} onRetry={health.reload} />;

  const h = health.data;
  const haltedSet = sets.data?.find((s) => s.haltReason === "host-key-changed");
  const staleSet = sets.data?.find((s) => s.state === "stale");

  if (sets.data && sets.data.length === 0)
    return (
      <>
        <PageHeader title="Dashboard" subtitle="No backup sets configured" />
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

  return (
    <>
      <PageHeader
        title="Dashboard"
        subtitle={
          sets.data
            ? sets.data.length + " backup sets \u00b7 polling every 30s"
            : "Loading…"
        }
        actions={
          <>
            <button className="btn" disabled={readOnly}>Run all due sets</button>
            <button className="btn btn--primary" disabled={readOnly} onClick={() => navigate("/sets/new")}>
              Add backup set
            </button>
          </>
        }
      />

      {/* A changed host key is the highest-severity state in the product and is
          surfaced above everything else. */}
      {haltedSet ? (
        <WarningBanner
          tone="danger"
          eyebrow="Security warning"
          title={"The SSH host key for " + haltedSet.host + " has changed"}
          actions={
            <button className="btn btn--sm" onClick={() => navigate("/sets/" + haltedSet.id)}>
              Review fingerprint
            </button>
          }
        >
          Backup operations for this set have been stopped until the new fingerprint
          is independently verified. No remote artifacts will be deleted while the
          set is halted.
        </WarningBanner>
      ) : null}

      {h ? <HealthSummary health={h} /> : null}

      {h ? (
        <section className="card" aria-label="Key metrics">
          <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(196px, 1fr))" }}>
            <MetricCard
              label="Backup sets"
              value={String(h.setsHealthy)}
              detail={h.setsStale + " stale \u00b7 " + h.setsFailing + " failing"}
            />
            <MetricCard
              label="Success rate \u00b7 7d"
              value={percent(h.successRate7d)}
              detail="verified ingestion cycles"
            />
            <MetricCard
              label="Quarantine"
              value={String(h.quarantinedCount)}
              detail="need review"
            />
            <MetricCard label="Storage" value={bytes(h.storageFreeBytes) + " free"}>
              <div style={{ marginTop: 8 }}>
                <StorageGauge
                  freeBytes={h.storageFreeBytes}
                  totalBytes={h.storageTotalBytes}
                  state={h.storageState}
                />
              </div>
            </MetricCard>
          </div>
        </section>
      ) : null}

      {staleSet ? (
        <WarningBanner tone="warn" title={"Stale \u00b7 " + staleSet.name}>
          {staleSet.stateNote}
        </WarningBanner>
      ) : null}

      <section className="card" aria-label="Active operations">
        <div className="card__header">
          <h2 className="eyebrow">Active operations</h2>
          <span style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
            {(operations.data?.length ?? 0) + " running"}
          </span>
        </div>
        <div style={{ padding: 18, display: "flex", flexDirection: "column", gap: 20 }}>
          {operations.data?.length
            ? operations.data.map((op, i) => (
                <div
                  key={op.id}
                  style={{
                    paddingTop: i === 0 ? 0 : 18,
                    borderTop: i === 0 ? undefined : "1px solid var(--border)"
                  }}
                >
                  <OperationProgress operation={op} />
                </div>
              ))
            : <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>Nothing running right now.</p>}
        </div>
      </section>

      <section className="card" aria-label="Recent activity">
        <div className="card__header">
          <h2 className="eyebrow">Recent activity</h2>
          <button
            className="btn btn--quiet"
            style={{ height: "auto", padding: 0, border: "none", background: "none", color: "var(--accent)", fontSize: "var(--text-sm)" }}
            onClick={() => navigate("/activity")}
          >
            View all
          </button>
        </div>
        <div style={{ padding: "14px 18px" }}>
          <ActivityTimeline events={(activity.data ?? []).slice(0, 6)} dense />
        </div>
      </section>
    </>
  );
}
