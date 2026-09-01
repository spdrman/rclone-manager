import { useMemo, useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import { useCausl } from "@shared/state/graph";
import { setsNode } from "@shared/state/appNodes";
import { PageHeader } from "@shared/components/PageHeader";
import { ActivityTimeline } from "@shared/components/ActivityTimeline";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { isNotConfigured } from "@shared/api/failure";
import type { Severity } from "@shared/types/operation";

/** Deliberately not overbuilt (§19): four filters, one list. */
export function ActivityPage() {
  const api = useApi();
  const events = useAsync(() => api.listActivity(), [api]);
  // Reads the same shared node BackupSetsPage/DashboardPage/BackupsPage do
  // (#106) instead of running a fifth independent listSets() fetch just to
  // populate this filter dropdown (#103).
  const sets = useCausl(setsNode);

  const [setId, setSetId] = useState("");
  const [minSeverity, setMinSeverity] = useState("");

  const filtered = useMemo(() => {
    const rank: Record<Severity, number> = { info: 0, ok: 0, warn: 1, error: 2 };
    return (events.data ?? []).filter((e) => {
      if (setId && e.setId !== setId) return false;
      if (minSeverity && rank[e.severity] < Number(minSeverity)) return false;
      return true;
    });
  }, [events.data, setId, minSeverity]);

  // #275: an empty timeline is the truth on an unconfigured instance, and
  // the filters above have nothing to filter.
  if (isNotConfigured(events.error))
    return (
      <>
        <PageHeader title="Activity" subtitle="Nothing has happened yet" />
        <EmptyState title="No activity yet">
          Backup Manager records what it does here. It has done nothing yet, because this
          instance has no configuration and no backup set to run.
        </EmptyState>
      </>
    );

  if (events.error) return <ErrorState {...events.error} onRetry={events.reload} />;

  return (
    <>
      <PageHeader title="Activity" subtitle="Operational timeline across all backup sets" />

      <div style={{ display: "flex", gap: 9, flexWrap: "wrap" }}>
        <select className="select" style={{ height: 32 }} aria-label="Backup set" value={setId} onChange={(e) => setSetId(e.target.value)}>
          <option value="">All backup sets</option>
          {(sets.data ?? []).map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
        </select>
        <select className="select" style={{ height: 32 }} aria-label="Severity" value={minSeverity} onChange={(e) => setMinSeverity(e.target.value)}>
          <option value="">All severities</option>
          <option value="1">Warning and above</option>
          <option value="2">Errors only</option>
        </select>
        <select className="select" style={{ height: 32 }} aria-label="Time range" defaultValue="24">
          <option value="24">Last 24 hours</option>
          <option value="168">Last 7 days</option>
        </select>
      </div>

      {filtered.length === 0 ? (
        <EmptyState title="No matching events">
          Nothing has happened in this window for the selected filters.
        </EmptyState>
      ) : (
        <div className="card">
          <ActivityTimeline events={filtered} />
        </div>
      )}
    </>
  );
}
