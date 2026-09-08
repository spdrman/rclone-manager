/**
 * The activity strips at the foot of the dashboard: one per backup set,
 * kept current (issue #573).
 *
 * The polling loop underneath moved to pages/useActivityFeed.ts when a
 * SECOND surface started reading the same feed (the backup set detail
 * page, issue #596). The reason it is a hook rather than a graph node is
 * there: both surfaces poll at the same cadence, but they ask different
 * questions, and a shared node holds one answer.
 *
 * mergeActivity and feedRestarted are re-exported from here because the
 * global terminal (ActivityDock, issue #599) imports them from this
 * module. They live in the hook now; this keeps the one import path
 * rather than making a move of the loop a churn across three files.
 */
import { ErrorState } from "@shared/components/EmptyState";
import { ActivityStrip } from "@shared/pages/ActivityStrip";
import { useActivityFeed } from "@shared/pages/useActivityFeed";
import type { BackupSet } from "@shared/types/backup";

export { feedRestarted, mergeActivity } from "@shared/pages/useActivityFeed";

export function DashboardActivity({ sets }: { sets: BackupSet[] | null }) {
  const feed = useActivityFeed();

  if (!sets || sets.length === 0) return null;

  return (
    <section className="card" aria-label="Activity">
      <div className="card__header">
        <h2 className="eyebrow">Activity</h2>
        <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
          {feed.error ? "not refreshing" : "refreshing every " + Math.round(feed.pollAfterMs / 1000) + "s"}
        </span>
      </div>
      {/* A failed poll states itself and leaves the strips where they are.
          The last good reading stays on screen under the notice, because
          blanking the panel an operator is reading is a worse answer than
          an old one that says it is old. */}
      {feed.error ? (
        <div style={{ padding: "var(--space-3) var(--space-5) 0" }}>
          <ErrorState {...feed.error} onRetry={feed.refresh} />
        </div>
      ) : null}
      {/* A failed poll makes every reading below it a reading from the
          past, and the strips are told so rather than left drawing a
          pulsing bar and a byte rate that both assert the process is
          alive. That is the one thing nobody knows while the poll is
          failing, and it is the whole distinction this panel exists to
          draw. */}
      {sets.map((set, i) => (
        <div key={set.id} style={{ borderTop: i === 0 ? undefined : "1px solid var(--border)" }}>
          <ActivityStrip set={set} activity={feed.held.get(set.id) ?? null} stale={feed.error !== null} />
        </div>
      ))}
    </section>
  );
}
