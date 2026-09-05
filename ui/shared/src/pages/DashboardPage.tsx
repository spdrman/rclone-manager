import { useNavigate } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import type { AsyncState } from "@shared/hooks/useAsync";
import { useCausl } from "@shared/state/graph";
import { operationsNode } from "@shared/state/appNodes";
import type { BackupSet } from "@shared/types/backup";
import type { CycleOutcome, SystemHealth } from "@shared/types/operation";
import { PageHeader } from "@shared/components/PageHeader";
import { HealthSummary } from "@shared/components/HealthSummary";
import { MetricCard } from "@shared/components/MetricCard";
import { StorageGauge } from "@shared/components/StorageGauge";
import { OperationProgress } from "@shared/components/OperationProgress";
import { ActivityTimeline } from "@shared/components/ActivityTimeline";
import { WarningBanner } from "@shared/components/WarningBanner";
import { StatusBadge } from "@shared/components/StatusBadge";
import { HaltBanner } from "@shared/components/HaltBanner";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { isNotConfigured } from "@shared/api/failure";
import { bytes } from "@shared/utilities/format";
import { backupSetPath } from "@shared/utilities/routes";

/**
 * The one evidence-navigating action each halt reason actually has on the
 * backup set's own detail page (issue #285's second defect). Host-key-
 * changed has a fingerprint panel there ("Connection", FingerprintDisplay)
 * to send an operator to; authentication-failed has no equivalent
 * evidence section, so it carries no entry here rather than borrowing the
 * fingerprint one. HaltBanner's own doc is the rule this map exists to
 * satisfy: actions are for navigating to the evidence, never a stand-in
 * for it, and a reason with no matching entry gets no action at all
 * rather than the wrong one.
 */
const HALT_ACTION_LABEL: Partial<Record<NonNullable<BackupSet["haltReason"]>, string>> = {
  "host-key-changed": "Review fingerprint"
};

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
  // The most recent FINISHED run cycle that carries counts, newest first
  // (GET /api/v1/operations returns recent operations in that order).
  //
  // A cycle "completed" when it ran to the end, which is much narrower
  // than it reads: a backup's own quarantine is a business outcome rather
  // than an operation failure, so a cycle that backed nothing up finishes
  // with exactly the same status as one that backed everything up. Issue
  // #361 was that lie told to a cron job; #368 recorded the two counts
  // that tell them apart, and nothing rendered them until this.
  //
  // `cycle` is null for an operation still running, and that is why the
  // filter is on the counts and not on the status: a pair of zeroes drawn
  // for a cycle nobody has measured yet is the loudest possible wrong
  // answer.
  const lastCycle = operations.data?.find((op) => op.cycle !== null) ?? null;
  const activity = useAsync(() => api.listActivity(), [api]);
  // Issue #286: a separate fetch, not derived from `health` above. GET
  // /system/storage's `manager` object answers a different question than
  // GET /system/health's per-set list (see ManagerStorage's own doc for
  // why summing that list cannot answer it), and it is the one this
  // panel is meant to show.
  const storage = useAsync(() => api.getStorage(), [api]);

  // #275: an instance with no configuration refuses every read here, and
  // that is not a fault to report, it is a setup step nobody has taken.
  if (isNotConfigured(health.error))
    return (
      <>
        <PageHeader title="Dashboard" subtitle="Not configured yet" />
        <EmptyState
          title="Nothing is being backed up yet"
          action={
            <button className="btn btn--primary" onClick={() => navigate("/sets/new")}>
              Add backup set
            </button>
          }
        >
          This instance has no configuration. Adding your first backup set is what writes
          it, and this page starts reporting the moment that set runs.
        </EmptyState>
      </>
    );

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
            haltedSet.haltReason && HALT_ACTION_LABEL[haltedSet.haltReason] ? (
              <button
                className="btn btn--sm"
                onClick={() => navigate(backupSetPath(haltedSet.source, haltedSet.set))}
              >
                {HALT_ACTION_LABEL[haltedSet.haltReason]}
              </button>
            ) : null
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
            <MetricCard
              label="Storage"
              value={
                storage.data
                  ? storage.data.known
                    ? bytes(storage.data.freeBytes) + " free"
                    : "Not known yet"
                  : "…"
              }
            >
              <div style={{ marginTop: 8 }}>
                {storage.data ? (
                  <StorageGauge storage={storage.data} />
                ) : storage.error ? (
                  <div className="banner banner--danger" style={{ fontSize: "var(--text-sm)" }}>
                    {"Storage capacity is unavailable (" + storage.error.message + ")."}
                  </div>
                ) : (
                  <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>Checking storage…</p>
                )}
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

      {lastCycle && lastCycle.cycle ? (
        <LastCycleOutcome outcome={lastCycle.cycle} />
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

/**
 * What the last finished run cycle actually got done.
 *
 * Two numbers, and a sentence that says what they mean. There is
 * deliberately no percentage and no rate: a cycle discovers what it will
 * find as it goes, so there is no honest denominator for the whole until
 * it ends, and a "success rate" is exactly the kind of confident figure
 * issue #211 removed from this UI once already.
 */
function LastCycleOutcome({ outcome }: { outcome: CycleOutcome }) {
  // Walked something and got none of it through. That is the reading the
  // counts exist for, and it is the only one this panel raises its voice
  // about: zero walked is a quiet cycle with nothing to do, which is not
  // a problem.
  const barren = outcome.artifactsWalked > 0 && outcome.artifactsThrough === 0;
  const short = !barren && outcome.artifactsThrough < outcome.artifactsWalked;

  // FR-30's half. A move pass is only worth a word when there was
  // something to move: a deployment that declares no storage medium
  // records a real zero here, every cycle, and a permanent "0 of 0 moved"
  // row on its dashboard would be a new thing to explain that says
  // nothing. A null pair is a cycle recorded before these counts existed
  // and is not a cycle that moved nothing, so it is silent too.
  const moves = outcome.moves && outcome.moves.attempted > 0 ? outcome.moves : null;
  const barrenMoves = moves !== null && moves.landed === 0;
  const shortMoves = moves !== null && !barrenMoves && moves.landed < moves.attempted;

  return (
    <section
      className="card"
      aria-label="Last run cycle"
      style={barren || barrenMoves ? { borderColor: "var(--warn)" } : undefined}
    >
      <div
        className="card__header"
        style={barren || barrenMoves ? { borderBottomColor: "var(--warn)" } : undefined}
      >
        <h2 className="eyebrow">Last run cycle</h2>
        {barren ? (
          <StatusBadge tone="warn" glyph={"\u25b2"}>Nothing got through</StatusBadge>
        ) : barrenMoves ? (
          // A cycle can back everything up perfectly and put none of it
          // where the chain says it belongs, and this is the badge for
          // exactly that: the backups happened, the moves did not.
          <StatusBadge tone="warn" glyph={"\u25b2"}>Nothing moved</StatusBadge>
        ) : short ? (
          <StatusBadge tone="warn" glyph={"\u25b2"}>Some did not get through</StatusBadge>
        ) : (
          <StatusBadge tone="ok" glyph={"\u25cf"}>All through</StatusBadge>
        )}
      </div>
      <div style={{ padding: 18, display: "flex", flexDirection: "column", gap: 12 }}>
        <div style={{ display: "flex", gap: 28, alignItems: "flex-end", flexWrap: "wrap" }}>
          <div>
            <div className="mono" style={{ fontSize: "var(--text-2xl)", fontWeight: 500 }}>
              {outcome.artifactsWalked}
            </div>
            <div className="eyebrow">walked</div>
          </div>
          <div>
            <div
              className="mono"
              style={{
                fontSize: "var(--text-2xl)", fontWeight: 500,
                color: barren || short ? "var(--warn)" : "var(--ok)"
              }}
            >
              {outcome.artifactsThrough}
            </div>
            <div className="eyebrow">got through</div>
          </div>
          <div style={{ marginLeft: "auto", textAlign: "right" }}>
            <div className="mono" style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
              {outcome.backupSetsProcessed + (outcome.backupSetsProcessed === 1 ? " backup set" : " backup sets")}
            </div>
          </div>
        </div>
        <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-2)", maxWidth: "72ch" }}>
          {barren
            ? "This cycle had a reason to touch " + outcome.artifactsWalked +
              " backups and none of them ended it with their bytes on durable storage. It is recorded as completed because it ran to the end."
            : short
              ? "This cycle walked " + outcome.artifactsWalked + " backups and " +
                outcome.artifactsThrough + " of them ended it with their bytes on durable storage."
              : "Every backup this cycle had a reason to touch ended it with its bytes on durable storage."}
        </p>
        {moves !== null && (
          <p
            style={{
              margin: 0, fontSize: "var(--text-sm)", maxWidth: "72ch",
              color: barrenMoves || shortMoves ? "var(--warn)" : "var(--text-2)"
            }}
          >
            {barrenMoves
              ? moves.attempted + " backups were due to move to the medium their retention tier names, and none arrived. " +
                "They are still on the medium they were on, and nothing was deleted."
              : shortMoves
                ? moves.attempted + " backups were due to move to the medium their retention tier names, and " +
                  moves.landed + " of them arrived."
                : moves.attempted + " backups were due to move to the medium their retention tier names, and all " +
                  moves.landed + " arrived."}
          </p>
        )}
      </div>
    </section>
  );
}
