import type { BackupSet } from "@shared/types/backup";
import { HealthBadge, HEALTH_PRESENTATION } from "./StatusBadge";
import { bytes, relativeAge } from "@shared/utilities/format";

/**
 * One backup set, as a card.
 *
 * # There is no run control here, deliberately (#231)
 *
 * This card carried a "Run now" button whose handler was
 * `api.runCycle(configRevision)`: a DEPLOYMENT-WIDE pass over every
 * enabled backup set, not a run of the set on the card. #214 rewired the
 * handler (core has no per-set run and never has) and renamed the button
 * everywhere it appears in a page header, but not here, so one card's
 * button silently started all of them.
 *
 * Renaming it would have been half a fix. Placement carries scope on its
 * own: a control inside a card reads as acting on that card whatever its
 * label says, and a list of twelve sets would have rendered twelve
 * identical copies of one deployment-wide action. So the button is gone
 * from the card and lives once in the page header, beside the equivalent
 * controls DashboardPage and BackupSetDetailPage already put there.
 */
export function BackupSetCard({
  set,
  currentOperation,
  onOpen,
  onTest,
  actionsDisabled = false
}: {
  set: BackupSet;
  currentOperation?: string;
  onOpen(): void;
  onTest(): void;
  actionsDisabled?: boolean;
}) {
  const tone = HEALTH_PRESENTATION[set.state].tone;
  const borderColor =
    set.state === "healthy"
      ? "var(--border)"
      : tone === "warn"
        ? "var(--warn)"
        : "var(--danger)";

  return (
    <article
      className="card"
      style={{ borderColor, display: "flex", flexDirection: "column" }}
    >
      <div
        style={{
          display: "flex", alignItems: "flex-start", justifyContent: "space-between",
          gap: 12, padding: "15px 17px 13px"
        }}
      >
        <div>
          <div style={{ fontSize: 15, fontWeight: 600, letterSpacing: "-0.01em" }}>{set.name}</div>
          <div className="mono" style={{ marginTop: 3, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
            {set.host + ":" + set.port}
          </div>
        </div>
        <HealthBadge state={set.state} />
      </div>

      <p
        style={{
          margin: 0, padding: "0 17px 13px", fontSize: "var(--text-sm)",
          color: "var(--text-2)", minHeight: 34
        }}
      >
        {set.stateNote}
      </p>

      <dl
        style={{
          margin: 0, padding: "13px 17px", borderTop: "1px solid var(--border)",
          display: "grid", gridTemplateColumns: "1fr 1fr", gap: "11px 14px",
          fontSize: "var(--text-sm)"
        }}
      >
        <Field label="Newest known-good" value={relativeAge(set.newestKnownGoodAt)} mono />
        <Field label="Last run" value={relativeAge(set.lastRunAt)} mono />
        {/* Issue #333. This used to read "0 / 0 / 0" on every card in
            every real deployment, because the daily/weekly/monthly numbers
            behind it were a hardcoded placeholder nothing computed. It
            now names WHICH policy retains this set, which is the thing
            the server actually answers, and the chain itself lives on the
            set's own page where a chain of any shape can be rendered. */}
        <Field
          label="Retention"
          value={set.retentionIsOverride ? "This set's own policy" : "Deployment policy"}
        />
        <Field
          label="Last validation"
          value={
            set.lastValidation === "passed"
              ? "Passed"
              : set.lastValidation === "failed"
                ? "Failed"
                : "Not run"
          }
        />
        <Field label="Retained" value={set.retainedCount + " \u00b7 " + bytes(set.retainedBytes)} mono />
        <Field label="Expected every" value={set.expectedIntervalHours + "h"} mono />
      </dl>

      <div
        className="card__footer"
        style={{ display: "flex", alignItems: "center", gap: 8 }}
      >
        <button className="btn btn--sm" onClick={onOpen}>Open</button>
        <button className="btn btn--sm btn--quiet" onClick={onTest} disabled={actionsDisabled}>
          Test connection
        </button>
        <div style={{ flex: 1 }} />
        {/* What is running for this set right now, and nothing more. The
            fallback used to be `set.halted ? "halted" : "idle"`, and
            `halted` was a field nothing ever computed: the HTTP client
            filled it with a literal `false` on every set, so the branch
            could not fire against a real service and a set the manager
            had refused to connect to read as "idle". The word now says
            only what operationsNode actually reports (#231). Why the set
            is not running, when the manager knows, is stateNote's job and
            the detail page's host-key banner's. */}
        <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
          {currentOperation ?? "idle"}
        </span>
      </div>
    </article>
  );
}

function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div>
      <dt
        className="eyebrow"
        style={{ fontSize: 10.5, letterSpacing: "0.06em" }}
      >
        {label}
      </dt>
      <dd style={{ margin: "3px 0 0", fontFamily: mono ? "var(--font-mono)" : undefined }}>
        {value}
      </dd>
    </div>
  );
}
