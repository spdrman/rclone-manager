import type { BackupSet } from "@shared/types/backup";
import { HealthBadge, HEALTH_PRESENTATION } from "./StatusBadge";
import { bytes, relativeAge } from "@shared/utilities/format";

export function BackupSetCard({
  set,
  currentOperation,
  onOpen,
  onRun,
  onTest,
  actionsDisabled = false
}: {
  set: BackupSet;
  currentOperation?: string;
  onOpen(): void;
  onRun(): void;
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
        <Field
          label="Retention"
          value={set.retention.daily + " / " + set.retention.weekly + " / " + set.retention.monthly}
          mono
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
        <button className="btn btn--sm" onClick={onRun} disabled={actionsDisabled || set.halted}>
          Run now
        </button>
        <button className="btn btn--sm btn--quiet" onClick={onTest} disabled={actionsDisabled}>
          Test connection
        </button>
        <div style={{ flex: 1 }} />
        <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
          {currentOperation ?? (set.halted ? "halted" : "idle")}
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
