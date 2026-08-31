import type { Operation, TransferProgress } from "@shared/types/operation";
import { TRANSFER_STAGES, progressPercent } from "@shared/types/operation";
import { bytes, rate } from "@shared/utilities/format";

const STAGE_LABEL: Record<string, string> = {
  discovering: "Discovering",
  transferring: "Transferring",
  verifying: "Verifying",
  committing: "Committing",
  "cleaning-remote": "Cleaning remote source",
  complete: "Complete"
};

/** Why there is nothing to draw, in the operation's own terms. Each of
 *  these is a different situation and none of them is "0%": a bar sitting
 *  at zero would claim a transfer exists and has moved nothing, which is
 *  false in all four cases. */
const NO_PROGRESS_REASON: Record<Operation["status"], string> = {
  queued: "Queued. Nothing has started yet, so there is nothing to measure.",
  running:
    "No progress reading is available. Live progress comes from the service process running the cycle, so an operation left behind by a restart reports none.",
  completed: "Finished. Progress is reported only while an operation is running.",
  failed: "Stopped. Progress is reported only while an operation is running."
};

export function OperationProgress({ operation }: { operation: Operation }) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 11 }}>
      <div
        style={{
          display: "flex", alignItems: "baseline", justifyContent: "space-between",
          gap: 14, flexWrap: "wrap"
        }}
      >
        <div style={{ display: "flex", alignItems: "baseline", gap: 10 }}>
          <span style={{ fontWeight: 600, fontSize: 14 }}>{operation.setName}</span>
          <span style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>{operation.label}</span>
        </div>
        {operation.progress ? <Readings progress={operation.progress} /> : null}
      </div>

      {operation.progress ? (
        <Live progress={operation.progress} nonDestructive={operation.nonDestructive} label={operation.label} />
      ) : (
        <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
          {NO_PROGRESS_REASON[operation.status]}
        </p>
      )}

      <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
        {operation.nonDestructive
          ? "Read-only pass \u2014 no artifacts are deleted during this operation."
          : "The remote artifact is removed only after the NAS copy is verified and durably committed."}
      </p>
    </div>
  );
}

/** The measured numbers, each rendered only when the service measured it.
 *  Nothing here falls back to a zero: an absent reading shows no element
 *  at all, which is what "unknown" looks like. */
function Readings({ progress }: { progress: TransferProgress }) {
  const percent = progressPercent(progress);
  return (
    <div
      style={{
        display: "flex", gap: 16, fontFamily: "var(--font-mono)",
        fontSize: "var(--text-sm)", color: "var(--text-2)"
      }}
    >
      {progress.backupSetsTotal > 0 ? (
        <span>
          {"set " + (progress.backupSetsDone + 1) + " of " + progress.backupSetsTotal}
        </span>
      ) : null}
      <span>{progress.artifactsDone + " done"}</span>
      {progress.bytesDone !== undefined && progress.bytesTotal !== undefined ? (
        <span>{bytes(progress.bytesDone) + " / " + bytes(progress.bytesTotal)}</span>
      ) : null}
      {progress.bytesPerSecond !== undefined ? <span>{rate(progress.bytesPerSecond)}</span> : null}
      <span style={{ color: "var(--text)", fontWeight: 600 }}>
        {percent === null ? "\u2014" : percent + "%"}
      </span>
    </div>
  );
}

/** The bar and the stage checklist.
 *
 *  The bar measures the ONE artifact being copied, never the operation: a
 *  run cycle is a pass over every enabled backup set and what it will find
 *  is discovered as it goes, so there is no honest denominator for the
 *  whole of it. The caption under the bar says so, and the bar's
 *  accessible name carries the artifact's name for the same reason.
 *
 *  With no measurable fraction the bar is indeterminate: aria-valuenow is
 *  omitted, which is exactly what ARIA means by indeterminate, rather than
 *  set to 0, which would mean "measured, and nothing has moved". */
function Live({
  progress,
  nonDestructive,
  label
}: {
  progress: TransferProgress;
  nonDestructive: boolean;
  label: string;
}) {
  const stageIndex = TRANSFER_STAGES.indexOf(progress.stage);
  const percent = progressPercent(progress);
  const subject = progress.artifact ?? progress.backupSetId ?? "the current backup set";

  return (
    <>
      <div
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={100}
        {...(percent === null ? {} : { "aria-valuenow": percent })}
        aria-label={
          percent === null
            ? label + ": " + subject + ", progress not measurable"
            : label + ": copying " + subject
        }
        style={{ height: 6, borderRadius: 3, background: "var(--surface-3)", overflow: "hidden" }}
      >
        <div
          style={{
            width: (percent ?? 0) + "%", height: "100%",
            background: nonDestructive ? "var(--text-3)" : "var(--accent)"
          }}
        />
      </div>

      <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
        {percent === null
          ? "Working on " + subject + ". Its size is not known, so how far through it this is cannot be measured."
          : "The bar measures " + subject + ", not the whole run."}
      </p>

      {stageIndex >= 0 ? (
        <ol
          style={{
            margin: "2px 0 0", padding: 0, listStyle: "none",
            display: "flex", flexWrap: "wrap", gap: "6px 10px", fontSize: "var(--text-sm)"
          }}
        >
          {TRANSFER_STAGES.map((stage, i) => {
            const done = i < stageIndex;
            const current = i === stageIndex;
            return (
              <li
                key={stage}
                aria-current={current ? "step" : undefined}
                style={{
                  display: "flex", alignItems: "center", gap: 6,
                  color: current ? "var(--text)" : done ? "var(--text-2)" : "var(--text-3)",
                  fontWeight: current ? 600 : 400
                }}
              >
                <span aria-hidden="true" style={{ color: done ? "var(--ok)" : undefined }}>
                  {done ? "\u2713" : current ? "\u25cf" : "\u25cb"}
                </span>
                {STAGE_LABEL[stage]}
              </li>
            );
          })}
        </ol>
      ) : null}
    </>
  );
}
