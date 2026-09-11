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
import { useCallback, useMemo, useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import { useCausl } from "@shared/state/graph";
import { setsNode } from "@shared/state/appNodes";
import { PageHeader } from "@shared/components/PageHeader";
import { FieldHelp } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import { ActivityTimeline } from "@shared/components/ActivityTimeline";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { isNotConfigured, describeFailure } from "@shared/api/failure";
import { isDebugEnabled } from "@shared/api/debug";
import type { ApiError } from "@shared/api/contracts";
import type { ActivityEvent, Severity } from "@shared/types/operation";

/**
 * How many events one page asks for (issue #730).
 *
 * Named here rather than left to the service's own default so this page
 * asks for a page SIZE it renders and then pages, instead of asking for
 * "the feed" and receiving whatever a deployment has accumulated since
 * the day it was installed. The number matches the service's default, so
 * the request this page makes is the one every other reader already
 * made; what is new is that it follows the cursor afterwards.
 */
const ACTIVITY_PAGE_SIZE = 200;

/** Deliberately not overbuilt (§19): two filters, one list. It said four
 *  until #299 took the "Time range" select away, which had no handler and
 *  no endpoint parameter behind it to acquire one. */
export function ActivityPage() {
  const api = useApi();
  const page = useAsync(() => api.listActivity({ limit: ACTIVITY_PAGE_SIZE }), [api]);
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

  // Issue #730. The pages loaded BEHIND the first one, in load order,
  // with the cursor the last of them ended at.
  //
  // null means nobody has asked for an older page yet, and while it is
  // null the first page's own cursor is the authority. Once it is set, it
  // is: a cursor of null then means the record ended, which is a
  // different state from "not asked yet" and the reason this is one piece
  // of state rather than an events array beside an optional string. With
  // two, a page that came back empty with no cursor would fall back to
  // the first page's cursor and offer the control again forever.
  const [older, setOlder] = useState<{ events: ActivityEvent[]; cursor: string | null } | null>(null);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [olderFailure, setOlderFailure] = useState<string | null>(null);

  const cursor = older ? older.cursor : page.data?.nextCursor ?? null;

  const loadOlder = useCallback(() => {
    if (!cursor || loadingOlder) return;
    setLoadingOlder(true);
    setOlderFailure(null);
    api
      .listActivity({ limit: ACTIVITY_PAGE_SIZE, before: cursor })
      .then((next) => {
        setOlder((held) => ({
          events: [...(held?.events ?? []), ...next.events],
          cursor: next.nextCursor ?? null
        }));
      })
      .catch((e: unknown) => {
        // Reported beside the control rather than through ErrorState: the
        // events already on screen are still true, and replacing a
        // rendered timeline with an error panel because the page BEHIND
        // it could not be read would lose the thing the reader came for.
        setOlderFailure(describeFailure(e, "Backupd could not read older events.").message);
      })
      .finally(() => setLoadingOlder(false));
  }, [api, cursor, loadingOlder]);

  const filtered = useMemo(() => {
    const rank: Record<Severity, number> = { info: 0, ok: 0, warn: 1, error: 2 };
    // Every page loaded so far, filtered as one list: a filter that only
    // applied to the newest page would hide matches an operator has
    // already fetched.
    const loaded = [...(page.data?.events ?? []), ...(older?.events ?? [])];
    return loaded.filter((e) => {
      if (setId && e.setId !== setId) return false;
      if (minSeverity && rank[e.severity] < Number(minSeverity)) return false;
      return true;
    });
  }, [page.data, older, setId, minSeverity]);

  // #275: an empty timeline is the truth on an unconfigured instance, and
  // the filters above have nothing to filter.
  if (isNotConfigured(page.error))
    return (
      <>
        <PageHeader title="Activity" subtitle="Nothing has happened yet" />
        <EmptyState title="No activity yet">
          Backupd records what it does here. It has done nothing yet, because this
          instance has no configuration and no backup set to run.
        </EmptyState>
      </>
    );

  if (page.error)
    return (
      <>
        <PageHeader title="Activity" subtitle="Operational timeline across all backup sets" />
        <ErrorState
          {...page.error}
          retriedAt={retriedAt ?? undefined}
          onRetry={() => {
            setRetriedAt(new Date().toLocaleTimeString());
            // The pages loaded behind the first one are dropped with it:
            // they were read against a cursor the reload has no reason to
            // land on, and keeping them would splice two different reads
            // of a growing record into one list.
            setOlder(null);
            setOlderFailure(null);
            page.reload();
          }}
        />
        {isDebugEnabled() ? <DebugFailure error={page.error} /> : null}
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
            Removed rather than wired: there was no endpoint parameter
            behind it. #730 gave the route a cursor rather than a window,
            so what this page offers instead is the control below, which
            reads further back one page at a time. */}
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

      {/* Issue #730. Offered whenever the service said there is a page
          behind the one on screen, INCLUDING when the filters above match
          nothing in what is loaded: the events an operator is looking for
          may be in the next page, and hiding the only way to reach them
          under an empty state is the dead end that made this page load
          the whole record in the first place. */}
      {cursor ? (
        <div style={{ marginTop: 12, display: "flex", alignItems: "center", gap: 9, flexWrap: "wrap" }}>
          <button className="btn" type="button" disabled={loadingOlder} onClick={loadOlder}>
            {loadingOlder ? "Loading older events…" : "Load older events"}
          </button>
          {olderFailure ? (
            <span role="alert" style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
              {olderFailure}
            </span>
          ) : null}
        </div>
      ) : null}
    </>
  );
}

/**
 * Issue #730's on-screen half, shown only when diagnostics are on.
 *
 * The deployment this exists for reaches the operator as "Backupd
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
