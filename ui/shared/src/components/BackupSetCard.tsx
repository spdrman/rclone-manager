import type { BackupSet } from "@shared/types/backup";
import { HealthBadge, HEALTH_PRESENTATION, StatusBadge } from "./StatusBadge";
import { removalPhrase } from "./RemoveBackupSetDialog";
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
 *
 * # There ARE enable, disable and remove controls here, deliberately
 *
 * They are the opposite case, and the same reasoning settles both. Those
 * three act on ONE backup set, they take that set's identity, and until
 * now every one of them meant opening the set first. An operator with a
 * dozen sets who wants to pause one had to navigate into it, act, and
 * navigate back. Placement carries scope, so a control that acts on one
 * set belongs on that set's row.
 *
 * That places a destructive action in a list, which is the arrangement
 * that gets the wrong row acted on. Three things answer it:
 *
 *   - the row prints its own `source/set` identity, so the operator can
 *     see which set they are on without opening it
 *   - removal is confirmed by retyping that identity, and the phrase is
 *     the row's, so confirming the set below the one you meant does not
 *     match (RemoveBackupSetDialog and ConfirmationDialog's own docs)
 *   - both controls carry the set's name in their accessible name, so a
 *     screen reader hears "Disable Production PostgreSQL" rather than the
 *     fourth identical "Disable" on the page
 *
 * `busy` is the third: a row with something in flight offers nothing
 * else, so a disable cannot race a removal on the same set.
 */
export function BackupSetCard({
  set,
  currentOperation,
  onOpen,
  onTest,
  onToggleEnabled,
  onRemove,
  busy = false,
  actionsDisabled = false
}: {
  set: BackupSet;
  currentOperation?: string;
  onOpen(): void;
  onTest(): void;
  onToggleEnabled(): void;
  onRemove(): void;
  /** Something is already in flight for THIS set, or its removal is being
   *  confirmed. Every control that would send a second request for the
   *  same set is off while it is true. */
  busy?: boolean;
  actionsDisabled?: boolean;
}) {
  const rowDisabled = actionsDisabled || busy;
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
        <div style={{ minWidth: 0 }}>
          <div style={{ fontSize: 15, fontWeight: 600, letterSpacing: "-0.01em" }}>{set.name}</div>
          {/* The set's canonical identity, on the row. `name` is a display
              label two sets under different sources may share; this is the
              pair the API takes, the pair the set's own URL carries, and
              the exact string the removal confirmation asks to be
              retyped. Printing it here is what makes that retyping a
              recognition rather than a copy out of the dialog doing the
              asking. */}
          <div className="mono" style={{ marginTop: 3, fontSize: "var(--text-sm)", color: "var(--text-2)", overflowWrap: "anywhere" }}>
            {removalPhrase(set)}
          </div>
          <div className="mono" style={{ marginTop: 2, fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
            {set.host + ":" + set.port}
          </div>
        </div>
        <div style={{ display: "flex", flexDirection: "column", alignItems: "flex-end", gap: 6 }}>
          <HealthBadge state={set.state} />
          {/* State, not just an action. An operator scanning this page can
              see which sets are paused without pressing anything, which
              the enable/disable button's own label cannot do on its own:
              a button reading "Enable" is equally consistent with "this
              set is off" and with "this button turns sets on". */}
          {set.enabled ? null : (
            <StatusBadge tone="neutral" glyph={"\u25cb"}>Disabled</StatusBadge>
          )}
        </div>
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
        style={{ display: "flex", alignItems: "center", flexWrap: "wrap", gap: 8 }}
      >
        <button className="btn btn--sm" onClick={onOpen}>Open</button>
        <button className="btn btn--sm btn--quiet" onClick={onTest} disabled={rowDisabled}>
          Test connection
        </button>
        {/* Caution tier, same as the detail page's copy of this control,
            and no typed confirmation: pausing a backup set is reversible
            by pressing the same button again, and making an operator type
            a name for that would be ceremony without a risk behind it. */}
        <button
          className="btn btn--sm btn--caution"
          onClick={onToggleEnabled}
          disabled={rowDisabled}
        >
          {set.enabled ? "Disable" : "Enable"}
          <span className="visually-hidden">{" " + set.name}</span>
        </button>
        <button
          className="btn btn--sm btn--destructive"
          onClick={onRemove}
          disabled={rowDisabled}
        >
          {"Remove\u2026"}
          <span className="visually-hidden">{" " + set.name}</span>
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
