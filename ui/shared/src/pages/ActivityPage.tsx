/**
 * The event log, filtered by set and by severity, and nothing else.
 *
 * Deliberately not overbuilt: two filters and a list. The temptation on a
 * page like this is a search box, a date range and a saved-view mechanism,
 * and none of those is the thing an operator actually does here, which is
 * "show me what went wrong on this set".
 *
 * The severity filter ranks info and ok the same, because they are the
 * same thing to somebody filtering: neither is a problem. That collapse is
 * why the control offers a threshold rather than a set of checkboxes.
 */
import { useMemo, useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import { useCausl } from "@shared/state/graph";
import { setsNode } from "@shared/state/appNodes";
import { PageHeader } from "@shared/components/PageHeader";
import { FieldHelp } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import { ActivityTimeline } from "@shared/components/ActivityTimeline";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { isNotConfigured } from "@shared/api/failure";
import { isDebugEnabled } from "@shared/api/debug";
import type { ApiError } from "@shared/api/contracts";
import type { Severity } from "@shared/types/operation";

/** Deliberately not overbuilt (§19): two filters, one list. It said four
 *  until #299 took the "Time range" select away, which had no handler and
 *  no endpoint parameter behind it to acquire one. */
export function ActivityPage() {
  const api = useApi();
  const events = useAsync(() => api.listActivity(), [api]);
  // Reads the same shared node BackupSetsPage/DashboardPage/BackupsPage do
  // (#106) instead of running a fifth independent listSets() fetch just to
  // populate this filter dropdown (#103).
  const sets = useCausl(setsNode);

  const [setId, setSetId] = useState("");
  const [minSeverity, setMinSeverity] = useState("");
  // Issue #598: kept here rather than inside ErrorState, because that
  // component unmounts for as long as the retry is in flight (the page
  // renders its normal body while `error` is cleared), so a counter living
  // in it would be reset by the very act it is meant to record.
  const [retriedAt, setRetriedAt] = useState<string | null>(null);

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

  if (events.error)
    return (
      <>
        <PageHeader title="Activity" subtitle="Operational timeline across all backup sets" />
        <ErrorState
          {...events.error}
          retriedAt={retriedAt ?? undefined}
          onRetry={() => {
            setRetriedAt(new Date().toLocaleTimeString());
            events.reload();
          }}
        />
        {isDebugEnabled() ? <DebugFailure error={events.error} /> : null}
      </>
    );

  return (
    <>
      <PageHeader title="Activity" subtitle="Operational timeline across all backup sets" />

      <div style={{ display: "flex", gap: 9, flexWrap: "wrap" }}>
        {/* These two carry an aria-label rather than a visible one, so the
            help attaches through the low-level wrapper: HelpField would
            draw a second, visible label beside a filter row that is
            deliberately compact. */}
        <FieldHelp label="Backup set" help={FIELD_HELP.activitySetFilter}>
          {(helpId) => (
            <select
              className="select" style={{ height: 32 }} aria-label="Backup set"
              aria-describedby={helpId}
              value={setId} onChange={(e) => setSetId(e.target.value)}
            >
              <option value="">All backup sets</option>
              {(sets.data ?? []).map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
            </select>
          )}
        </FieldHelp>
        <FieldHelp label="Severity" help={FIELD_HELP.activitySeverityFilter}>
          {(helpId) => (
            <select
              className="select" style={{ height: 32 }} aria-label="Severity"
              aria-describedby={helpId}
              value={minSeverity} onChange={(e) => setMinSeverity(e.target.value)}
            >
              <option value="">All severities</option>
              <option value="1">Warning and above</option>
              <option value="2">Errors only</option>
            </select>
          )}
        </FieldHelp>
        {/* Issue #299: a "Time range" select used to sit here,
            `defaultValue="24"` with no `onChange` and nothing reading it.
            Removed rather than wired: listActivity() takes no window
            argument, and building server-side windowing is out of scope
            for that issue. */}
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

/**
 * Issue #730's on-screen half, shown only when diagnostics are on.
 *
 * The deployment this exists for reaches the operator as "Backup Manager
 * did not answer", and that is the right thing to SAY: the request got no
 * reply, so there is no status and no correlation id to quote. What it
 * does not do is get the facts off the screen and into the hands of
 * whoever is reading the server log. ErrorState folds them into a
 * collapsed disclosure, omits the code entirely, and says nothing at all
 * about an ABSENT correlation id — and "the response carried no id" is
 * itself the finding on this failure, because a proxy that never reached
 * the service cannot have produced one.
 *
 * So with `?debug=1` the same failure's three machine-facing facts are
 * put on screen open: the code the hooks branch on, the correlation id
 * when there was one (and the fact that there was not, when there was
 * not), and the technical detail failure.ts assembled. An operator reads
 * the id back to whoever is grepping the service log for
 * `activity_debug`.
 *
 * Deliberately a paragraph of monospace text rather than a panel: it is
 * for selecting and pasting, nobody who did not ask for it ever sees it,
 * and it must not look like part of the product.
 */
function DebugFailure({ error }: { error: ApiError }) {
  const lines = [
    "code " + error.code,
    "correlation id " + (error.correlationId ?? "(none: the failing response carried no X-Correlation-Id)"),
    error.detail
  ].filter((line): line is string => !!line);

  return (
    <div
      className="mono"
      style={{
        marginTop: 8,
        fontSize: "var(--text-sm)",
        color: "var(--text-3)",
        whiteSpace: "pre-wrap",
        userSelect: "text"
      }}
    >
      {"[rm-debug]\n" + lines.join("\n")}
    </div>
  );
}
