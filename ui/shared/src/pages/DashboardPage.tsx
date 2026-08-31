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
import { HaltBanner } from "@shared/components/HaltBanner";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { bytes } from "@shared/utilities/format";

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
  // GET /api/v1/operations returns recent operations, newest first, not
  // only the live ones, so a panel headed "Active operations" that counted
  // the whole list would report every finished run of the day as running.
  // Filtering here rather than in the client keeps the node holding what
  // the endpoint actually returns.
  const active = operations.data?.filter((op) => op.status === "running" || op.status === "queued") ?? null;
  const activity = useAsync(() => api.listActivity(), [api]);

  if (health.error)
    return <ErrorState {...health.error} onRetry={health.reload} />;

  const h = health.data;
  // Any set the manager could not connect to, whatever the reason
  // (#245). Keying on the reason's presence rather than on one value
  // means a rejected login raises this too, under its own words: a set
  // whose credentials the host refuses backs up exactly as little as one
  // whose key changed.
  const haltedSet = sets.data?.find((s) => s.haltReason);
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

      {/* A set the manager cannot connect to is the highest-severity state
          in the product and is surfaced above everything else. The wording
          lives in HaltBanner so this and the set's own page cannot
          describe the same refusal differently. */}
      {haltedSet ? (
        <HaltBanner
          set={haltedSet}
          actions={
            <button className="btn btn--sm" onClick={() => navigate("/sets/" + haltedSet.id)}>
              Review fingerprint
            </button>
          }
        />
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
              label="Degraded or stale"
              value={String(h.setsDegraded + h.setsStale)}
              detail="sets needing attention"
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
            {active ? active.length + " running" : "…"}
          </span>
        </div>
        <div style={{ padding: 18, display: "flex", flexDirection: "column", gap: 12 }}>
          {/* operations.data is null until the first fetch resolves — that is
              "not known yet", not "zero", so it must not fall into the
              "Nothing running right now." branch below (mandatory review,
              PR #143). A failed fetch gets its own small inline notice
              rather than the page's full-page ErrorState, since operations
              is a secondary resource here (health owns that treatment via
              the early return above) and a stale-but-known operations list
              should stay visible under the notice rather than being
              replaced by it. */}
          {operations.error ? (
            <div className="banner banner--danger" style={{ fontSize: "var(--text-sm)" }}>
              Live operation status is unavailable ({operations.error.message}).
            </div>
          ) : null}
          {active === null
            ? (operations.error
                ? null
                : <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>Checking for active operations…</p>)
            : active.length
              ? (
                <div style={{ display: "flex", flexDirection: "column", gap: 20 }}>
                  {active.map((op, i) => (
                    <div
                      key={op.id}
                      style={{
                        paddingTop: i === 0 ? 0 : 18,
                        borderTop: i === 0 ? undefined : "1px solid var(--border)"
                      }}
                    >
                      <OperationProgress operation={op} />
                    </div>
                  ))}
                </div>
              )
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
